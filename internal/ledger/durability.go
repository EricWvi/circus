package ledger

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/EricWvi/circus/internal/store"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/objfile"
	"golang.org/x/sys/unix"
)

// The startup sweep takes an exclusive lock on the directory before
// removing abandoned staging directories, spools and reference locks.
// Writers take a shared lock so different projects can still write in parallel.
func (m *Manager) lockRoot(mode int) (func(), error) {
	dir, err := os.Open(m.root)
	if err != nil {
		return nil, err
	}
	err = unix.Flock(int(dir.Fd()), mode|unix.LOCK_NB)
	if err != nil {
		_ = dir.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: ledger directory is in use", store.ErrBusy)
		}
		return nil, err
	}
	return func() { _ = dir.Close() }, nil
}

func (r *repository) mainLockPath() string {
	return filepath.Join(r.dir, mainRef.String()+".lock")
}

func (r *repository) lockMain() (func(), error) {
	name := r.mainLockPath()
	if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("%w: ledger main is locked", store.ErrBusy)
	}
	if err != nil {
		return nil, err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(name)
		return nil, err
	}
	// The repository may have been renamed out of its staging directory.
	return func() { _ = os.Remove(r.mainLockPath()) }, nil
}

func syncPath(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	if err := errors.Join(f.Sync(), f.Close()); err != nil {
		return fmt.Errorf("sync %s: %w", path, err)
	}
	return nil
}

// Loose objects are written under this prefix in objects/ and renamed
// into place. The exclusive startup sweep removes abandoned ones.
const objectTempPrefix = ".object-"

var errCorruptObject = errors.New("ledger: corrupt object")

func (r *repository) loosePath(hash plumbing.Hash) string {
	id := hash.String()
	return filepath.Join(r.dir, "objects", id[:2], id[2:])
}

// storeObject makes the object available and durable for the next setMain.
// An object already present was possibly left by a crash, or written
// before objects were synced, so it is only reused after it inflates to
// the expected type, size and hash. A broken loose copy is replaced.
func (r *repository) storeObject(kind plumbing.ObjectType, hash plumbing.Hash, parts ...[]byte) error {
	var size int64
	for _, p := range parts {
		size += int64(len(p))
	}
	var err error
	switch err = r.verifyObject(kind, hash, size); {
	case err == nil:
		err = r.syncObject(hash)
	case errors.Is(err, plumbing.ErrObjectNotFound) || errors.Is(err, errCorruptObject):
		err = r.writeObject(kind, hash, size, parts)
	}
	if err == nil && r.faults != nil {
		r.faults.remove(hash)
	}
	return err
}

// suspectBlob is the cheap guard for a blob the parent commit already
// refers to: a loose copy must be nonempty, and a blob whose read failed
// earlier gets full verification.
func (r *repository) suspectBlob(hash plumbing.Hash) bool {
	if r.faults != nil && r.faults.has(hash) {
		return true
	}
	info, err := os.Stat(r.loosePath(hash))
	if errors.Is(err, os.ErrNotExist) {
		return r.storage.HasEncodedObject(hash) != nil
	}
	return err != nil || info.Size() == 0
}

// verifyObject returns ErrObjectNotFound, errCorruptObject for a loose
// object that does not inflate to exactly the expected object, or another
// error when the object cannot be checked or repaired.
func (r *repository) verifyObject(kind plumbing.ObjectType, hash plumbing.Hash, size int64) error {
	f, err := os.Open(r.loosePath(hash))
	if errors.Is(err, os.ErrNotExist) {
		if err := r.storage.HasEncodedObject(hash); err != nil {
			return err
		}
		// Only Git itself writes packs, so a packed copy cannot be
		// replaced by a loose one that go-git would prefer to read.
		o, err := r.storage.EncodedObject(kind, hash)
		if err != nil {
			return err
		}
		reader, err := o.Reader()
		if err != nil {
			return err
		}
		defer reader.Close()
		hasher := plumbing.NewHasher(kind, size)
		n, err := io.Copy(hasher, io.LimitReader(reader, size+1))
		if err != nil {
			return err
		}
		if o.Type() != kind || n != size || hasher.Sum() != hash {
			return fmt.Errorf("ledger: packed object %s is corrupt", hash)
		}
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	if err := checkLooseObject(f, kind, hash, size); err != nil {
		return fmt.Errorf("%w %s: %w", errCorruptObject, hash, err)
	}
	return nil
}

func checkLooseObject(f io.Reader, kind plumbing.ObjectType, hash plumbing.Hash, size int64) error {
	reader, err := objfile.NewReader(f)
	if err != nil {
		return err
	}
	defer reader.Close()
	t, n, err := reader.Header()
	if err != nil {
		return err
	}
	if t != kind || n != size {
		return fmt.Errorf("found %s of %d bytes, want %s of %d bytes", t, n, kind, size)
	}
	// Reading to the end also checks zlib's own checksum.
	read, err := io.Copy(io.Discard, io.LimitReader(reader, size+1))
	if err != nil {
		return err
	}
	if read != size || reader.Hash() != hash {
		return errors.New("content does not match its name")
	}
	return nil
}

// writeObject writes and fsyncs the compressed object in a temporary file
// before renaming it to its final name, so that name never refers to an
// incomplete file. setMain fsyncs the fan-out directory before main can
// refer to the object.
func (r *repository) writeObject(kind plumbing.ObjectType, hash plumbing.Hash, size int64, parts [][]byte) error {
	objects := filepath.Join(r.dir, "objects")
	f, err := os.CreateTemp(objects, objectTempPrefix)
	if err != nil {
		return err
	}
	renamed := false
	defer func() {
		if !renamed {
			_ = os.Remove(f.Name())
		}
	}()
	w := objfile.NewWriter(f)
	err = w.WriteHeader(kind, size)
	for _, p := range parts {
		if err == nil {
			_, err = w.Write(p)
		}
	}
	err = errors.Join(err, w.Close())
	if err == nil && w.Hash() != hash {
		err = fmt.Errorf("ledger: object content does not match %s", hash)
	}
	if err := errors.Join(err, f.Chmod(0o444), f.Sync(), f.Close()); err != nil {
		return err
	}
	id := hash.String()
	if err := os.Mkdir(filepath.Join(objects, id[:2]), 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	if err := os.Rename(f.Name(), r.loosePath(hash)); err != nil {
		return err
	}
	renamed = true
	r.markObjectDir(id[:2])
	return nil
}

func (r *repository) markObjectDir(dir string) {
	if r.dirtyObjects == nil {
		r.dirtyObjects = make(map[string]struct{})
	}
	r.dirtyObjects[dir] = struct{}{}
}

// syncObject makes an existing object durable. Losing power before this
// fsync, or the directory fsyncs in setMain, cannot be simulated without
// fault injection below the file system, so tests do not cover them.
func (r *repository) syncObject(hash plumbing.Hash) error {
	id := hash.String()
	dir := id[:2]
	name := filepath.Join(r.dir, "objects", dir, id[2:])
	if err := syncPath(name); errors.Is(err, os.ErrNotExist) {
		// An existing packed object requires no new loose object or directory.
		return r.storage.HasEncodedObject(hash)
	} else if err != nil {
		return err
	}
	r.markObjectDir(dir)
	return nil
}

// setMain compares against the old ref while the caller holds main.lock.
// The returned boolean reports whether the ref rename took effect. As with
// syncObject, a power loss between these fsyncs cannot be simulated without
// fault injection, so tests do not cover them.
func (r *repository) setMain(parent, hash plumbing.Hash) (bool, error) {
	return r.setRef(mainRef, parent, hash)
}

// setRef moves a reference from old to hash, both zero for an absent
// reference, after making the objects written so far durable. The caller
// holds main.lock, which guards every reference of the repository. A zero
// hash deletes the reference. The returned boolean reports whether the
// change took effect.
func (r *repository) setRef(name plumbing.ReferenceName, old, hash plumbing.Hash) (bool, error) {
	objects := filepath.Join(r.dir, "objects")
	for dir := range r.dirtyObjects {
		if err := syncPath(filepath.Join(objects, dir)); err != nil {
			return false, err
		}
	}
	if len(r.dirtyObjects) != 0 {
		if err := syncPath(objects); err != nil {
			return false, err
		}
	}
	current, err := r.readRef(name)
	if err != nil {
		return false, err
	}
	if current != old {
		if name == mainRef {
			return false, fmt.Errorf("%w: ledger main changed since it was read", store.ErrBusy)
		}
		return false, fmt.Errorf("%w: ledger reference %s changed since it was read", store.ErrBusy, name)
	}
	dst := filepath.Join(r.dir, name.String())
	if hash.IsZero() {
		if current.IsZero() {
			return false, nil
		}
		if err := os.Remove(dst); err != nil {
			return false, err
		}
		if err := syncPath(filepath.Dir(dst)); err != nil {
			return true, err
		}
		clear(r.dirtyObjects)
		return true, nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return false, err
	}
	f, err := os.CreateTemp(r.dir, refPrefix)
	if err != nil {
		return false, err
	}
	defer os.Remove(f.Name())
	_, writeErr := fmt.Fprintln(f, hash.String())
	if err := errors.Join(writeErr, f.Sync(), f.Close()); err != nil {
		return false, err
	}
	if err := os.Rename(f.Name(), dst); err != nil {
		return false, err
	}
	// Sync the reference's directory and every directory up to the
	// repository, which also covers directories created just now.
	for dir := filepath.Dir(dst); ; dir = filepath.Dir(dir) {
		if err := syncPath(dir); err != nil {
			return true, err
		}
		if dir == r.dir || len(dir) <= len(r.dir) {
			break
		}
	}
	clear(r.dirtyObjects)
	return true, nil
}
