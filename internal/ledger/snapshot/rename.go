package snapshot

import (
	"errors"

	"golang.org/x/sys/unix"
)

var errUnsupported = errors.New("exclusive rename is not supported")

// exclusiveRename and hardLink are replaced by tests to run the fallbacks
// on file systems that support both.
var (
	exclusiveRename = renameExclusive
	hardLink        = unix.Linkat
)

// renameNoReplace renames from (in fromFd) to to (in toFd) unless
// something exists at to, in which case it fails with EEXIST and changes
// nothing. It uses the platform's exclusive rename; where the file system
// does not support it, a hard link (which fails on an existing name) and
// an unlink; where hard links are not supported either, a check followed
// by a plain rename, which a file created at to between the two would
// lose. The three are tried in that order on every call.
func renameNoReplace(fromFd int, from string, toFd int, to string) error {
	err := exclusiveRename(fromFd, from, toFd, to)
	if !errors.Is(err, errUnsupported) {
		return err
	}
	err = hardLink(fromFd, from, toFd, to, 0)
	switch {
	case err == nil:
		return unix.Unlinkat(fromFd, from, 0)
	case errors.Is(err, unix.EEXIST):
		return err
	case !errors.Is(err, unix.EPERM) && !errors.Is(err, unix.ENOTSUP) && !errors.Is(err, unix.EOPNOTSUPP) && !errors.Is(err, unix.EMLINK):
		return err
	}
	var st unix.Stat_t
	if err := unix.Fstatat(toFd, to, &st, unix.AT_SYMLINK_NOFOLLOW); err == nil {
		return unix.EEXIST
	} else if !errors.Is(err, unix.ENOENT) {
		return err
	}
	return unix.Renameat(fromFd, from, toFd, to)
}
