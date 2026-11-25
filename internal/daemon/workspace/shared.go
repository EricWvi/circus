package workspace

import (
	"context"
	"errors"
	"os"
)

// ErrLockBusy reports a DirLock held by another caller, in this daemon or
// another one on the device.
var ErrLockBusy = errBusy

// DirLock is the lock beside a directory that the workspace tools take,
// for other daemon tools that manage directories of their own.
type DirLock struct{ l *dirLock }

// LockBeside takes the non-blocking lock file ".<name>.circus-lock" inside
// dir, never following a symlink; ErrLockBusy when it is held. The kernel
// releases it when the process dies.
func LockBeside(dir *os.File, name string) (*DirLock, error) {
	l, err := lockIn(dir, name)
	if err != nil {
		return nil, err
	}
	return &DirLock{l: l}, nil
}

// Release removes the lock file and unlocks it.
func (l *DirLock) Release() { l.l.release() }

// RemoveTree removes the directory name inside parentFd and everything in
// it as Remove does: never following a symlink, never entering another
// mount, stopping when ctx ends. Nothing at name is not an error; a
// symlink or anything but a directory at name is refused.
func RemoveTree(ctx context.Context, parentFd int, name string) error {
	return newRemoval(ctx, false).removeDir(parentFd, name, 0, true)
}

// IsNotRemovable reports the error of RemoveTree for something at name
// that is not a directory.
func IsNotRemovable(err error) bool { return errors.Is(err, errNotRemovable) }
