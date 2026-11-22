package providers

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// KeyFileName is the key file's name beside the database by default.
const KeyFileName = "provider.key"

// KeyPath returns the key file to use: override when set, else
// KeyFileName in the directory of the database file dbPath.
func KeyPath(dbPath, override string) string {
	if override != "" {
		return override
	}
	return filepath.Join(filepath.Dir(dbPath), KeyFileName)
}

// LoadOrCreateKey reads the KeyBytes key in path, creating the file with a
// new random key (mode 0600) when it does not exist. The file appears
// complete or not at all: the key is written to a temporary file in the
// same directory, synced, and linked into place, which fails if another
// start created path first (that key is then read). An existing file must
// be a regular file of exactly KeyBytes bytes without group or other
// permissions.
func LoadOrCreateKey(path string) ([]byte, error) {
	key, err := readKey(path)
	if !errors.Is(err, fs.ErrNotExist) {
		return key, err
	}
	if err := createKey(path); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, err
	}
	return readKey(path)
}

func readKey(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	switch {
	case !info.Mode().IsRegular():
		return nil, fmt.Errorf("provider key file %s is not a regular file", path)
	case info.Mode().Perm()&0o077 != 0:
		return nil, fmt.Errorf("provider key file %s is accessible to group or others (mode %04o); chmod 600 it", path, info.Mode().Perm())
	case info.Size() != KeyBytes:
		return nil, fmt.Errorf("provider key file %s must hold exactly %d bytes", path, KeyBytes)
	}
	key := make([]byte, KeyBytes)
	if _, err := io.ReadFull(f, key); err != nil {
		return nil, fmt.Errorf("read provider key file %s: %w", path, err)
	}
	return key, nil
}

func createKey(path string) error {
	key := make([]byte, KeyBytes)
	if _, err := rand.Read(key); err != nil {
		return err
	}
	defer clear(key)
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".provider-key-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	err = tmp.Chmod(0o600)
	if err == nil {
		_, err = tmp.Write(key)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("write provider key file: %w", err)
	}
	if err := os.Link(name, path); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
