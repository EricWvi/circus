// Package credfile reads and writes the local credential file that `circus
// join` creates and long-running roles (`circus daemon`, later the worker) read.
//
// One file per machine-role principal (D43 item 8): daemon.json and
// worker.json under $XDG_CONFIG_HOME/circus (default ~/.config/circus). The
// file holds a long-term bearer token, so it is written 0600 inside a 0700
// directory and replaced atomically.
package credfile

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Role names a machine-role principal and selects its file name.
type Role string

// Roles with their own credential file.
const (
	RoleDaemon Role = "daemon"
	RoleWorker Role = "worker"
)

// File is the on-disk credential for one machine-role principal.
type File struct {
	// ControlPlaneURL is the https:// (or loopback-only http://) base URL.
	ControlPlaneURL string `json:"control_plane_url"`
	// Role is the principal role this credential belongs to.
	Role Role `json:"role"`
	// PrincipalID is the device or worker identity ID from EnrollResponse.
	PrincipalID string `json:"principal_id"`
	// Token is the long-term bearer token. Secret.
	Token string `json:"token"`
	// CAFile is an optional absolute path to a PEM bundle that verifies the
	// control plane instead of the system roots (D43 item 7, --ca-file).
	CAFile string `json:"ca_file,omitempty"`
}

// ErrNotFound reports that no credential file exists at the path.
var ErrNotFound = errors.New("credfile: no credential file; run `circus join` first")

// Dir returns the credential directory: $XDG_CONFIG_HOME/circus, falling back
// to ~/.config/circus.
func Dir() (string, error) {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" && filepath.IsAbs(x) {
		return filepath.Join(x, "circus"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("credfile: locate home: %w", err)
	}
	return filepath.Join(home, ".config", "circus"), nil
}

// Path returns the default file path for role.
func Path(role Role) (string, error) {
	if err := role.validate(); err != nil {
		return "", err
	}
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, string(role)+".json"), nil
}

// Load reads and validates the file at path. A missing file is ErrNotFound.
// Files readable by group or others are rejected because they hold a secret.
func Load(path string) (File, error) {
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return File{}, ErrNotFound
	}
	if err != nil {
		return File{}, fmt.Errorf("credfile: %w", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return File{}, fmt.Errorf("credfile: %s has mode %v; it must not be accessible by group or others (chmod 600)", path, info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return File{}, fmt.Errorf("credfile: %w", err)
	}
	var f File
	if err := json.Unmarshal(data, &f); err != nil {
		return File{}, fmt.Errorf("credfile: parse %s: %w", path, err)
	}
	if err := f.validate(); err != nil {
		return File{}, err
	}
	return f, nil
}

// Save writes f to path with mode 0600, creating the parent directory with
// mode 0700. The write is atomic: a temp file in the same directory is synced
// and renamed over path.
func Save(path string, f File) error {
	if err := f.validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("credfile: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("credfile: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".credfile-*")
	if err != nil {
		return fmt.Errorf("credfile: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("credfile: %w", err)
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return fmt.Errorf("credfile: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("credfile: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("credfile: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("credfile: %w", err)
	}
	return nil
}

func (r Role) validate() error {
	if r != RoleDaemon && r != RoleWorker {
		return fmt.Errorf("credfile: unknown role %q", string(r))
	}
	return nil
}

func (f File) validate() error {
	if err := f.Role.validate(); err != nil {
		return err
	}
	switch {
	case f.ControlPlaneURL == "":
		return errors.New("credfile: control_plane_url is empty")
	case f.PrincipalID == "":
		return errors.New("credfile: principal_id is empty")
	case f.Token == "":
		return errors.New("credfile: token is empty")
	case f.CAFile != "" && !filepath.IsAbs(f.CAFile):
		return fmt.Errorf("credfile: ca_file %q must be an absolute path", f.CAFile)
	}
	return nil
}
