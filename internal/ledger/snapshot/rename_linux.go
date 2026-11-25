package snapshot

import (
	"errors"

	"golang.org/x/sys/unix"
)

// renameExclusive renames as renameNoReplace does, with
// renameat2(RENAME_NOREPLACE); errUnsupported where the file system does
// not support the flag.
func renameExclusive(fromFd int, from string, toFd int, to string) error {
	err := unix.Renameat2(fromFd, from, toFd, to, unix.RENAME_NOREPLACE)
	if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.ENOTSUP) {
		return errUnsupported
	}
	return err
}
