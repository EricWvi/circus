package snapshot

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

func mtimeNs(st *unix.Stat_t) int64 {
	return int64(st.Mtim.Sec)*1e9 + int64(st.Mtim.Nsec) //nolint:unconvert // int32 on some platforms
}

func ctimeNs(st *unix.Stat_t) int64 {
	return int64(st.Ctim.Sec)*1e9 + int64(st.Ctim.Nsec) //nolint:unconvert // int32 on some platforms
}

// entryOf records st for path p with content hash sum.
func entryOf(p string, st *unix.Stat_t, sum string) Entry {
	return Entry{Path: p, Size: st.Size, MtimeNs: mtimeNs(st), CtimeNs: ctimeNs(st), Inode: uint64(st.Ino), SHA256: sum} //nolint:unconvert // uint32 on some platforms
}

func isRegular(st *unix.Stat_t) bool { return st.Mode&unix.S_IFMT == unix.S_IFREG }
func isDir(st *unix.Stat_t) bool     { return st.Mode&unix.S_IFMT == unix.S_IFDIR }

// kindName names a non-regular, non-directory file type.
func kindName(st *unix.Stat_t) string {
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFLNK:
		return "a symbolic link"
	case unix.S_IFIFO:
		return "a named pipe"
	case unix.S_IFSOCK:
		return "a socket"
	case unix.S_IFCHR, unix.S_IFBLK:
		return "a device"
	}
	return "not a regular file"
}

const (
	dirFlags  = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	readFlags = unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_NONBLOCK | unix.O_CLOEXEC
)

// openParent opens the directory holding p inside dir without following a
// symlink at any level; with create, missing directories are made, each
// followed by a sync of its parent so the new entry is durable. It returns
// the descriptor, which the caller closes, and the last segment.
func openParent(dir *os.File, p string, create bool) (int, string, error) {
	segments := strings.Split(p, "/")
	fd, err := unix.Openat(int(dir.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, "", &os.PathError{Op: "open", Path: dir.Name(), Err: err}
	}
	for i, seg := range segments[:len(segments)-1] {
		next, err := unix.Openat(fd, seg, dirFlags, 0)
		if errors.Is(err, unix.ENOENT) && create {
			if err = unix.Mkdirat(fd, seg, 0o755); err == nil {
				err = syncFd(fd)
			}
			if err == nil || errors.Is(err, unix.EEXIST) {
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

// readRegular reads the regular file name inside dirFd without following a
// symlink or blocking on a FIFO: at most limit+1 bytes into buf, which it
// may grow. It returns the stat taken on the descriptor before reading,
// the bytes read and, for anything but a regular file, errNotRegular.
func readRegular(dirFd int, name string, buf []byte, limit int) (unix.Stat_t, []byte, error) {
	var st unix.Stat_t
	fd, err := unix.Openat(dirFd, name, readFlags, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENXIO) || errors.Is(err, unix.EOPNOTSUPP) {
			err = errNotRegular
		}
		return st, buf, err
	}
	defer unix.Close(fd)
	if err := unix.Fstat(fd, &st); err != nil {
		return st, buf, err
	}
	if !isRegular(&st) {
		return st, buf, errNotRegular
	}
	if beforeRead != nil {
		beforeRead(name)
	}
	buf = buf[:0]
	for len(buf) <= limit {
		if len(buf) == cap(buf) {
			buf = slices.Grow(buf, min(max(4096, len(buf)), limit+1-len(buf)))
		}
		n, err := unix.Read(fd, buf[len(buf):min(cap(buf), limit+1)])
		if errors.Is(err, unix.EINTR) || errors.Is(err, unix.EAGAIN) {
			continue
		}
		if err != nil {
			return st, buf, err
		}
		if n == 0 {
			break
		}
		buf = buf[:len(buf)+n]
	}
	return st, buf, nil
}

var errNotRegular = errors.New("not a regular file")

// beforeRead, for tests, runs after a file is opened and checked and
// before it is read.
var beforeRead func(name string)

// State is what a path inside a snapshot holds now.
type State struct {
	// Exists is false when nothing is at the path.
	Exists bool
	// SHA256 of the content; empty for anything that is not a regular file
	// of at most MaxBytes(path) bytes.
	SHA256 string
}

// Listed reports whether the directory that holds p (inside dir, reached
// without following a symlink) lists p's last element under exactly that
// spelling now. Where the file system folds names, p may reach a file
// listed under another spelling (a.md opening A.md); this tells them apart.
// A missing parent, or one that is not a directory, lists nothing.
func Listed(dir *os.File, p string) (bool, error) {
	if !ValidPath(p) {
		return false, ErrInvalidPath
	}
	fd, name, err := openParent(dir, p, false)
	if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	parent := os.NewFile(uintptr(fd), parentPath(p))
	defer parent.Close()
	for {
		names, err := parent.Readdirnames(readBatch)
		if slices.Contains(names, name) {
			return true, nil
		}
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		if err != nil {
			return false, &os.PathError{Op: "read", Path: parentPath(p), Err: err}
		}
	}
}

// ReadState reports what p holds inside dir, never following a symlink.
func ReadState(dir *os.File, p string) (State, error) {
	if !ValidPath(p) {
		return State{}, ErrInvalidPath
	}
	fd, name, err := openParent(dir, p, false)
	if errors.Is(err, unix.ENOENT) {
		return State{}, nil
	}
	if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
		// A parent is a symlink or a file: something else is in the way.
		return State{Exists: true}, nil
	}
	if err != nil {
		return State{}, err
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstatat(fd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return State{}, nil
		}
		return State{}, &os.PathError{Op: "stat", Path: p, Err: err}
	}
	limit := MaxBytes(p)
	if !isRegular(&st) || st.Size > int64(limit) {
		return State{Exists: true}, nil
	}
	_, data, err := readRegular(fd, name, nil, limit)
	switch {
	case errors.Is(err, errNotRegular):
		return State{Exists: true}, nil
	case errors.Is(err, unix.ENOENT):
		return State{}, nil
	case err != nil:
		return State{}, &os.PathError{Op: "read", Path: p, Err: err}
	case len(data) > limit:
		return State{Exists: true}, nil
	}
	return State{Exists: true, SHA256: Hash(data)}, nil
}

// File is one file to write into a snapshot.
type File struct {
	Path    string
	Content string
}

// Writer writes files into a directory and records their stats for a
// manifest.
type Writer struct {
	dir *os.File
	// Sync fsyncs every file written.
	Sync bool
	// Clocks are where Manifest may take its time, in order of preference:
	// the first on the directory's file system whose probe can be made.
	// Without one, the time is the newest modification time written, so
	// the files written within that last tick stay racy.
	Clocks  []Clock
	entries map[string]Entry
}

// NewWriter writes into dir, which the caller keeps open until the
// Writer is done.
func NewWriter(dir *os.File) *Writer {
	return &Writer{dir: dir, entries: make(map[string]Entry)}
}

// Add writes content to p, creating missing directories and replacing an
// earlier file at p, and records the file's stat taken after writing. It
// refuses a symlink at any level and anything but a regular file at p.
func (w *Writer) Add(p, content string) error {
	if !ValidPath(p) {
		return fmt.Errorf("%w: %q", ErrInvalidPath, p)
	}
	if len(content) > MaxBytes(p) || !utf8.ValidString(content) {
		return fmt.Errorf("%s: content must be valid UTF-8 of at most %d bytes", p, MaxBytes(p))
	}
	dirFd, name, err := openParent(w.dir, p, true)
	if err != nil {
		return err
	}
	defer unix.Close(dirFd)
	fd, err := unix.Openat(dirFd, name, unix.O_WRONLY|unix.O_CREAT|unix.O_TRUNC|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0o644)
	if err != nil {
		return &os.PathError{Op: "create", Path: p, Err: err}
	}
	f := os.NewFile(uintptr(fd), p)
	defer f.Close()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return &os.PathError{Op: "stat", Path: p, Err: err}
	}
	if !isRegular(&st) {
		return &os.PathError{Op: "create", Path: p, Err: errNotRegular}
	}
	if _, err := io.WriteString(f, content); err != nil {
		return err
	}
	if w.Sync {
		if err := f.Sync(); err != nil {
			return err
		}
	}
	if err := unix.Fstat(fd, &st); err != nil {
		return &os.PathError{Op: "stat", Path: p, Err: err}
	}
	w.entries[p] = entryOf(p, &st, Hash(content))
	return f.Close()
}

// Manifest returns the manifest of the files written, taking its time from
// the directory's file system after them (see Clock).
func (w *Writer) Manifest(baseCommit string) (*Manifest, error) {
	now, found := int64(0), false
	for _, c := range w.Clocks {
		if !SameFileSystem(w.dir, c.Dir) {
			continue
		}
		var err error
		if now, err = nextTick(c); err == nil {
			found = true
			break
		}
	}
	if !found {
		now = 0
		for _, e := range w.entries {
			now = max(now, e.MtimeNs)
		}
	}
	m := &Manifest{Version: manifestVersion, BaseCommit: baseCommit, WrittenAtNs: now, Files: make([]Entry, 0, len(w.entries))}
	for _, e := range w.entries {
		m.Files = append(m.Files, e)
	}
	slices.SortFunc(m.Files, func(a, b Entry) int { return strings.Compare(a.Path, b.Path) })
	return m, nil
}

// Write writes files into dir and returns their manifest, its time taken
// as Writer.Clocks says.
func Write(dir *os.File, clocks []Clock, baseCommit string, files []File) (*Manifest, error) {
	w := NewWriter(dir)
	w.Clocks = clocks
	for _, f := range files {
		if err := w.Add(f.Path, f.Content); err != nil {
			return nil, err
		}
	}
	return w.Manifest(baseCommit)
}

// Clock is where a manifest's time is taken: a directory on the
// snapshot's own file system but outside the snapshot, in which a probe
// file is created and removed. Outside, so that a process killed while
// the probe exists leaves it where no scan of the snapshot sees it.
type Clock struct {
	Dir *os.File
	// Name is the probe's name, removed first if a killed run left it;
	// empty for a random ".circus-clock-<hex>", for a directory several
	// processes may share.
	Name string
}

// SameFileSystem reports whether a and b are on the same file system
// (st_dev); false when either cannot be examined.
func SameFileSystem(a, b *os.File) bool {
	var sa, sb unix.Stat_t
	return unix.Fstat(int(a.Fd()), &sa) == nil && unix.Fstat(int(b.Fd()), &sb) == nil && sa.Dev == sb.Dev
}

// maxTickWait bounds how long nextTick waits for the clock to move.
const maxTickWait = 50 * time.Millisecond

// nextTick returns a time of the clock's file system later than every
// timestamp it gave before the call, when its clock moves within
// maxTickWait (a coarse Linux tick is at most 10 ms), else its current
// time. Files written before the call then are not racy.
func nextTick(c Clock) (int64, error) {
	first, err := fsNow(c)
	if err != nil {
		return 0, err
	}
	deadline := time.Now().Add(maxTickWait)
	for {
		now, err := fsNow(c)
		if err != nil || now > first || time.Now().After(deadline) {
			return now, err
		}
		time.Sleep(time.Millisecond)
	}
}

// afterProbe, for tests, runs while the probe file exists; an error
// returns at once, leaving the probe behind as a killed process would.
var afterProbe func() error

// fsNow returns the current time of the clock's file system, at its
// timestamp granularity, from a probe file created and removed in it.
func fsNow(c Clock) (int64, error) {
	name := c.Name
	dirFd := int(c.Dir.Fd())
	if name == "" {
		var b [8]byte
		_, _ = rand.Read(b[:])
		name = ".circus-clock-" + hex.EncodeToString(b[:])
	} else if err := unix.Unlinkat(dirFd, name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return 0, &os.PathError{Op: "remove", Path: name, Err: err}
	}
	fd, err := unix.Openat(dirFd, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return 0, &os.PathError{Op: "create", Path: name, Err: err}
	}
	var st unix.Stat_t
	err = unix.Fstat(fd, &st)
	_ = unix.Close(fd)
	if afterProbe != nil {
		if err := afterProbe(); err != nil {
			return 0, err
		}
	}
	if rmErr := unix.Unlinkat(dirFd, name, 0); err == nil && rmErr != nil {
		err = rmErr
	}
	if err != nil {
		return 0, &os.PathError{Op: "stat", Path: name, Err: err}
	}
	return mtimeNs(&st), nil
}
