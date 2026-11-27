package snapshot

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

// Exporter writes a ledger export, the files of one main-line commit, into
// a fresh, empty directory that only it writes, for `circus ledger export`,
// which then renames that directory into place. Nothing is followed:
// directories are made and opened with O_NOFOLLOW relative to their
// parent, and files are created with O_EXCL, so a file never replaces
// anything. Files and directories take their modes from the umask.
//
// Paths are checked as Writer.Add checks them (ValidPath, MaxBytes, valid
// UTF-8). They must also come in strictly increasing order, at most
// MaxEntries of them, and no two names, files or directories, may be one
// name on a case-insensitive, normalization-insensitive file system
// (FoldKey) or one exact name used as both a file and a directory, so no
// file system can merge two paths of the export.
type Exporter struct {
	dir *os.File
	// names maps the FoldKey of every file and directory path written to
	// its exact path and whether it is a directory.
	names map[string]exportName
	// dirs lists the directories made, in the order made.
	dirs  []string
	files int
	last  string
}

type exportName struct {
	path string
	dir  bool
}

// NewExporter writes into dir, a fresh empty directory, which the caller
// keeps open until the Exporter is done.
func NewExporter(dir *os.File) *Exporter {
	return &Exporter{dir: dir, names: make(map[string]exportName)}
}

// Add writes content to p, creating missing directories, and syncs it.
func (e *Exporter) Add(p, content string) error {
	if !ValidPath(p) {
		return fmt.Errorf("%w: %q", ErrInvalidPath, p)
	}
	if len(content) > MaxBytes(p) || !utf8.ValidString(content) {
		return fmt.Errorf("%s: content must be valid UTF-8 of at most %d bytes", p, MaxBytes(p))
	}
	if p <= e.last {
		return fmt.Errorf("%w: %q is out of order", ErrInvalidPath, p)
	}
	if e.files >= MaxEntries {
		return fmt.Errorf("the export has more than %d files", MaxEntries)
	}
	var made []string
	for i := strings.IndexByte(p, '/'); i >= 0; i = nextSlash(p, i) {
		isNew, err := e.claim(p[:i], true)
		if err != nil {
			return err
		}
		if isNew {
			made = append(made, p[:i])
		}
	}
	if _, err := e.claim(p, false); err != nil {
		return err
	}
	e.last = p
	e.files++
	dirFd, name, err := e.openParent(p)
	if err != nil {
		return err
	}
	defer unix.Close(dirFd)
	e.dirs = append(e.dirs, made...)
	fd, err := unix.Openat(dirFd, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o666)
	if err != nil {
		return &os.PathError{Op: "create", Path: p, Err: err}
	}
	f := os.NewFile(uintptr(fd), p)
	_, err = io.WriteString(f, content)
	return errors.Join(err, f.Sync(), f.Close())
}

// claim records p as a file or a directory. It reports whether p is new,
// and refuses p when another path, or p as the other kind, has its
// FoldKey.
func (e *Exporter) claim(p string, dir bool) (bool, error) {
	key := FoldKey(p)
	have, ok := e.names[key]
	switch {
	case !ok:
		e.names[key] = exportName{path: p, dir: dir}
		return true, nil
	case have.path != p:
		return false, fmt.Errorf("%w: %q and %q are one name on a case-insensitive file system", ErrInvalidPath, have.path, p)
	case have.dir != dir || !dir:
		return false, fmt.Errorf("%w: %q is both a file and a directory", ErrInvalidPath, p)
	}
	return false, nil
}

func nextSlash(p string, i int) int {
	j := strings.IndexByte(p[i+1:], '/')
	if j < 0 {
		return -1
	}
	return i + 1 + j
}

// openParent opens the directory holding p, making the missing ones.
func (e *Exporter) openParent(p string) (int, string, error) {
	segments := strings.Split(p, "/")
	fd, err := unix.Openat(int(e.dir.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, "", &os.PathError{Op: "open", Path: e.dir.Name(), Err: err}
	}
	for i, seg := range segments[:len(segments)-1] {
		next, err := unix.Openat(fd, seg, dirFlags, 0)
		if errors.Is(err, unix.ENOENT) {
			if err = unix.Mkdirat(fd, seg, 0o777); err == nil {
				next, err = unix.Openat(fd, seg, dirFlags, 0)
			}
		}
		_ = unix.Close(fd)
		if err != nil {
			return -1, "", &os.PathError{Op: "open", Path: strings.Join(segments[:i+1], "/"), Err: err}
		}
		fd = next
	}
	return fd, segments[len(segments)-1], nil
}

// Sync syncs every directory made, deepest first, and the export's own
// directory, so the entries of the files written are durable.
func (e *Exporter) Sync() error {
	for i := len(e.dirs) - 1; i >= 0; i-- {
		fd, _, err := e.openParent(e.dirs[i] + "/.")
		if err != nil {
			return err
		}
		err = syncFd(fd)
		_ = unix.Close(fd)
		if err != nil {
			return &os.PathError{Op: "sync", Path: e.dirs[i], Err: err}
		}
	}
	return e.dir.Sync()
}

// RenameDirNoReplace renames from in fromFd to to in toFd unless something
// exists at to, which fails with EEXIST and changes nothing. It needs the
// platform's exclusive rename (renameat2 RENAME_NOREPLACE, renameatx_np
// RENAME_EXCL) and fails where the file system lacks it: the fallbacks of
// renameNoReplace do not apply to directories.
func RenameDirNoReplace(fromFd int, from string, toFd int, to string) error {
	err := exclusiveRename(fromFd, from, toFd, to)
	if errors.Is(err, errUnsupported) {
		return errors.New("the file system cannot rename a directory without replacing what is there")
	}
	return err
}

// RemoveAllAt removes name inside dirFd and, for a directory, everything
// in it, relative to the open directories and without following a symlink
// at any level.
func RemoveAllAt(dirFd int, name string) error {
	return removeAllAt(dirFd, name, 0)
}

func removeAllAt(dirFd int, name string, depth int) error {
	var st unix.Stat_t
	if err := unix.Fstatat(dirFd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return &os.PathError{Op: "stat", Path: name, Err: err}
	}
	if !isDir(&st) {
		if err := unix.Unlinkat(dirFd, name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
			return &os.PathError{Op: "remove", Path: name, Err: err}
		}
		return nil
	}
	// An export's path of MaxPathBytes has at most half as many levels.
	if depth > MaxPathBytes/2+1 {
		return fmt.Errorf("%s: nested more than %d directories deep", name, MaxPathBytes/2+1)
	}
	child, err := unix.Openat(dirFd, name, dirFlags, 0)
	if err != nil {
		return &os.PathError{Op: "open", Path: name, Err: err}
	}
	d := os.NewFile(uintptr(child), name)
	names, err := d.Readdirnames(-1)
	if err == nil {
		for _, n := range names {
			if err = removeAllAt(child, n, depth+1); err != nil {
				break
			}
		}
	}
	_ = d.Close()
	if err != nil {
		return err
	}
	if err := unix.Unlinkat(dirFd, name, unix.AT_REMOVEDIR); err != nil && !errors.Is(err, unix.ENOENT) {
		return &os.PathError{Op: "remove", Path: name, Err: err}
	}
	return nil
}
