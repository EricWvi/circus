//go:build !darwin

package snapshot

import "golang.org/x/sys/unix"

// syncFd flushes fd as os.File.Sync does: fsync.
func syncFd(fd int) error { return unix.Fsync(fd) }
