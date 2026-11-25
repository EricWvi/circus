package snapshot

import (
	"errors"

	"golang.org/x/sys/unix"
)

// renameExclusive renames as renameNoReplace does, with
// renameatx_np(RENAME_EXCL); errUnsupported where the file system does
// not support the flag.
func renameExclusive(fromFd int, from string, toFd int, to string) error {
	err := unix.RenameatxNp(fromFd, from, toFd, to, unix.RENAME_EXCL)
	if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOTSUP) {
		return errUnsupported
	}
	return err
}
