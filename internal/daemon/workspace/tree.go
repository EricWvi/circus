package workspace

import (
	"context"
	"errors"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// maxDepth bounds the descriptors a removal holds, one per level.
const maxDepth = 512

// readBatch is how many names a removal reads before deleting them and
// reopening the directory, which may reorder after deletions.
const readBatch = 1024

// maxRefills bounds how often a directory is emptied again when writers
// keep adding entries while it is being removed.
const maxRefills = 3

var (
	errMountPoint   = errors.New("is a mount point; nothing below it was removed")
	errTooDeep      = errors.New("is nested more than 512 levels deep")
	errNotRemovable = errors.New("is a symlink or not a directory")
	errRefilled     = errors.New("is not empty: new entries kept appearing while it was being removed")
	// errNoMountCheck reports a kernel that cannot open a directory
	// without crossing mounts (openat2 needs Linux 5.6).
	errNoMountCheck = errors.New("cannot be removed safely: this kernel lacks openat2 (Linux 5.6 or later), which keeps removal off other mounts")
)

// removal deletes a directory tree through directory descriptors. Every
// directory is opened relative to its already-open parent by openChildDir,
// which refuses symlinks and mount crossings (bind mounts included) for the
// very descriptor that is then read, so no check can be raced. Entries
// that are not directories are unlinked, never followed. It checks ctx
// before every entry and stops at the first error, restoring the modes it
// widened on directories that remain.
type removal struct {
	ctx context.Context
	uid int
	// strict refuses removal when the kernel cannot check mount crossings;
	// otherwise the walk falls back to comparing st_dev.
	strict  bool
	removed int
	// afterRead, for tests, runs after each batch of names is read.
	afterRead func(dir string)
	// beforeRound, for tests, runs before each round over a directory.
	beforeRound func(dir string)
}

func newRemoval(ctx context.Context, strict bool) *removal {
	return &removal{ctx: ctx, uid: os.Getuid(), strict: strict}
}

// removeAt removes name inside the directory dirFd: anything but a
// directory is unlinked; a directory is emptied and removed.
func (r *removal) removeAt(dirFd int, name string, depth int) error {
	if err := r.ctx.Err(); err != nil {
		return err
	}
	var st unix.Stat_t
	if err := unix.Fstatat(dirFd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return &os.PathError{Op: "stat", Path: name, Err: err}
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return r.unlink(dirFd, name, 0)
	}
	return r.removeDir(dirFd, name, depth, false)
}

// removeDir empties and removes the directory name inside parentFd. For
// the top of a removal (top set), a name that is no longer a directory is
// errNotRemovable instead of being unlinked.
func (r *removal) removeDir(parentFd int, name string, depth int, top bool) error {
	if depth >= maxDepth {
		return &os.PathError{Op: "remove", Path: name, Err: errTooDeep}
	}
	widened, original := false, uint32(0)
	// restore puts back the mode of a directory that stays, when the error
	// comes between two rounds, after its descriptor was closed.
	restore := func() {
		if !widened {
			return
		}
		if fd, err := openChildDir(parentFd, name, r.strict); err == nil {
			_ = unix.Fchmod(fd, original)
			_ = unix.Close(fd)
		}
	}
	refills := 0
	for {
		if r.beforeRound != nil {
			r.beforeRound(name)
		}
		if err := r.ctx.Err(); err != nil {
			restore()
			return err
		}
		fd, err := openChildDir(parentFd, name, r.strict)
		if err != nil {
			restore()
			switch {
			case errors.Is(err, unix.ENOENT):
				return nil
			case errors.Is(err, unix.ELOOP), errors.Is(err, unix.ENOTDIR):
				if top {
					return &os.PathError{Op: "open", Path: name, Err: errNotRemovable}
				}
				// Replaced by a symlink or file since the stat: unlink it.
				return r.unlink(parentFd, name, 0)
			}
			return &os.PathError{Op: "open", Path: name, Err: err}
		}
		dir := os.NewFile(uintptr(fd), name)
		if !widened {
			// A read-only directory of ours (such as a Go module cache)
			// cannot be emptied; the mode is restored if it stays.
			var st unix.Stat_t
			if err := unix.Fstat(fd, &st); err != nil {
				_ = dir.Close()
				return &os.PathError{Op: "stat", Path: name, Err: err}
			}
			if int(st.Uid) == r.uid && st.Mode&0o700 != 0o700 {
				original = uint32(st.Mode & 0o7777)
				widened = unix.Fchmod(fd, original|0o700) == nil
			}
		}
		more, err := r.removeEntries(dir, name, depth)
		if err == nil && !more {
			err = r.unlink(parentFd, name, unix.AT_REMOVEDIR)
			if errors.Is(err, unix.ENOTEMPTY) || errors.Is(err, unix.EEXIST) {
				if refills < maxRefills {
					refills++
					_ = dir.Close()
					continue
				}
				err = &os.PathError{Op: "remove", Path: name, Err: errRefilled}
			}
			if err == nil {
				_ = dir.Close()
				return nil
			}
		}
		if err != nil {
			if widened {
				_ = unix.Fchmod(fd, original)
			}
			_ = dir.Close()
			return err
		}
		_ = dir.Close()
	}
}

// removeEntries deletes up to readBatch entries of the open directory dir
// and reports whether more may remain.
func (r *removal) removeEntries(dir *os.File, name string, depth int) (bool, error) {
	names, err := dir.Readdirnames(readBatch)
	if err != nil && !errors.Is(err, io.EOF) {
		return false, &os.PathError{Op: "read", Path: name, Err: err}
	}
	if r.afterRead != nil {
		r.afterRead(name)
	}
	fd := int(dir.Fd())
	for _, entry := range names {
		if err := r.removeAt(fd, entry, depth+1); err != nil {
			var pathErr *os.PathError
			if errors.As(err, &pathErr) {
				pathErr.Path = name + "/" + pathErr.Path
			}
			return false, err
		}
	}
	return len(names) == readBatch, nil
}

func (r *removal) unlink(dirFd int, name string, flags int) error {
	err := unix.Unlinkat(dirFd, name, flags)
	switch {
	case err == nil:
		r.removed++
		return nil
	case errors.Is(err, unix.ENOENT):
		return nil
	}
	return &os.PathError{Op: "remove", Path: name, Err: err}
}

// openSameDevice opens the directory name inside parentFd without
// following a symlink and refuses it when its st_dev differs from the
// parent's. Both identities come from the descriptors themselves. This
// catches mounts of other filesystems but not a bind mount of the same
// filesystem.
func openSameDevice(parentFd int, name string) (int, error) {
	fd, err := unix.Openat(parentFd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	var parent, child unix.Stat_t
	if err := unix.Fstat(parentFd, &parent); err != nil {
		_ = unix.Close(fd)
		return -1, err
	}
	if err := unix.Fstat(fd, &child); err != nil {
		_ = unix.Close(fd)
		return -1, err
	}
	if parent.Dev != child.Dev {
		_ = unix.Close(fd)
		return -1, errMountPoint
	}
	return fd, nil
}
