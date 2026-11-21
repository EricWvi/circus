package workspace

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// errBusy reports a lock held by another preparation or removal, in this
// daemon or another one on the device.
var errBusy = errors.New("workspace is busy")

func lockName(name string) string      { return "." + name + ".circus-lock" }
func partialPrefix(name string) string { return "." + name + ".circus-partial-" }

// partialSuffixLen is the length of the random hex suffix of a partial
// clone's name.
const partialSuffixLen = 16

// isPartial reports whether entry is exactly a partial clone of name: the
// prefix followed by 16 lowercase hex digits. Anything else, such as a
// lock file or the partial clone of a workspace whose own name starts
// with "<name>.circus-partial-", is not.
func isPartial(entry, name string) bool {
	suffix, ok := strings.CutPrefix(entry, partialPrefix(name))
	if !ok || len(suffix) != partialSuffixLen {
		return false
	}
	for i := 0; i < len(suffix); i++ {
		if c := suffix[i]; !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// beforeFlock, for tests, runs between opening and locking a lock file.
var beforeFlock func()

// dirLock is a non-blocking flock on ".<name>.circus-lock" beside a
// workspace. The kernel releases it when the daemon dies. Releasing it
// removes the file; a locker that finds the file it locked removed or
// replaced retries on the current one, so two holders never coexist.
type dirLock struct {
	dir  *os.File
	name string
	fd   int
}

func lockIn(dir *os.File, name string) (*dirLock, error) {
	dirFd, file := int(dir.Fd()), lockName(name)
	for range 3 {
		fd, err := unix.Openat(dirFd, file, unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
		if err != nil {
			return nil, &os.PathError{Op: "open", Path: file, Err: err}
		}
		if beforeFlock != nil {
			beforeFlock()
		}
		if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
			_ = unix.Close(fd)
			if errors.Is(err, unix.EWOULDBLOCK) {
				return nil, errBusy
			}
			return nil, &os.PathError{Op: "lock", Path: file, Err: err}
		}
		var held, current unix.Stat_t
		if unix.Fstat(fd, &held) == nil && unix.Fstatat(dirFd, file, &current, unix.AT_SYMLINK_NOFOLLOW) == nil &&
			held.Dev == current.Dev && held.Ino == current.Ino {
			return &dirLock{dir: dir, name: file, fd: fd}, nil
		}
		// The previous holder removed the file while we were opening it.
		_ = unix.Flock(fd, unix.LOCK_UN)
		_ = unix.Close(fd)
	}
	return nil, errBusy
}

// release removes the lock file while still holding it, then unlocks. The
// explicit unlock matters: a child forked concurrently shares the open file
// description until it execs, and closing alone would keep the lock.
func (l *dirLock) release() {
	_ = unix.Unlinkat(int(l.dir.Fd()), l.name, 0)
	_ = unix.Flock(l.fd, unix.LOCK_UN)
	_ = unix.Close(l.fd)
}

// maxScannedEntries bounds the directory entries a leftover scan reads.
const maxScannedEntries = 100000

// removeLeftovers deletes ".<name>.circus-partial-<16 hex>" entries in dir, the
// hidden clones of earlier attempts that died. The caller holds the lock;
// nothing else in dir is touched.
func removeLeftovers(ctx context.Context, dir *os.File, name string) error {
	dirFd := int(dir.Fd())
	// Read through a separate description so dir's offset stays untouched.
	fd, err := unix.Openat(dirFd, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	list := os.NewFile(uintptr(fd), dir.Name())
	defer list.Close()
	var found []string
	for scanned := 0; scanned < maxScannedEntries; {
		if err := ctx.Err(); err != nil {
			return err
		}
		names, err := list.Readdirnames(readBatch)
		scanned += len(names)
		for _, entry := range names {
			if isPartial(entry, name) {
				found = append(found, entry)
			}
		}
		if errors.Is(err, io.EOF) || len(names) == 0 {
			break
		}
		if err != nil {
			return err
		}
	}
	// Partial clones are ours: on a kernel without openat2 the walk may
	// fall back to comparing devices.
	r := newRemoval(ctx, false)
	for _, entry := range found {
		if err := r.removeAt(dirFd, entry, 0); err != nil {
			return err
		}
	}
	return nil
}
