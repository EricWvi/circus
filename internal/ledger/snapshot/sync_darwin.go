package snapshot

import (
	"errors"

	"golang.org/x/sys/unix"
)

// syncFd flushes fd as os.File.Sync does on macOS: F_FULLFSYNC, which
// also flushes the drive's cache, falling back to fsync where the file
// system does not support it.
func syncFd(fd int) error {
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_FULLFSYNC, 0); err == nil || !errors.Is(err, unix.ENOTSUP) && !errors.Is(err, unix.ENOTTY) {
		return err
	}
	return unix.Fsync(fd)
}
