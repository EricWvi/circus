package workspace

import (
	"errors"
	"sync"

	"golang.org/x/sys/unix"
)

// openChildDir opens the directory name inside parentFd with openat2:
// RESOLVE_NO_SYMLINKS rejects a symlink (ELOOP) and RESOLVE_NO_XDEV
// rejects stepping onto any mount, bind mounts of the same filesystem
// included (EXDEV). The check belongs to the open itself, so the
// descriptor returned is the one that was checked. Without a usable
// openat2 (see mountCheckAvailable) a strict caller gets errNoMountCheck;
// others fall back to openSameDevice.
func openChildDir(parentFd int, name string, strict bool) (int, error) {
	if !mountCheckAvailable() {
		if strict {
			return -1, errNoMountCheck
		}
		return openSameDevice(parentFd, name)
	}
	fd, err := openat2(parentFd, name, &unix.OpenHow{
		Flags:   unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_XDEV,
	})
	switch {
	case err == nil:
		return fd, nil
	case errors.Is(err, unix.EXDEV):
		return -1, errMountPoint
	case errors.Is(err, unix.ENOSYS):
		if strict {
			return -1, errNoMountCheck
		}
		return openSameDevice(parentFd, name)
	}
	return -1, err
}

// openat2 is replaced by tests to simulate a kernel without it.
var openat2 = unix.Openat2

// mountCheckAvailable reports whether openat2 can be used: probed once and
// cached. Tests replace it.
var mountCheckAvailable = sync.OnceValue(probeOpenat2)

// probeOpenat2 opens "/" with O_PATH and the walk's resolve flags. That
// open needs no permission on any path (O_PATH reads nothing, and "/" has
// no parent to search), so any failure means the call itself is
// unavailable: ENOSYS on kernels before 5.6, or EPERM/EACCES from a
// seccomp profile that blocks it.
func probeOpenat2() bool {
	fd, err := openat2(unix.AT_FDCWD, "/", &unix.OpenHow{
		Flags:   unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_XDEV,
	})
	if err != nil {
		return false
	}
	_ = unix.Close(fd)
	return true
}
