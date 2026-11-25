package snapshot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

// Limits bound what Compare collects.
type Limits struct {
	// MaxChanges bounds the changes, deletions included; more sets
	// TooLarge and leaves Changes empty. Zero means MaxChanges.
	MaxChanges int
	// MaxChangedBytes bounds the content of the added and changed files;
	// more sets TooLarge and leaves every content out. Zero means
	// MaxChangedBytes. Ignored with HashesOnly.
	MaxChangedBytes int
	// HashesOnly leaves every content out: changes carry paths, hashes
	// and sizes, and the comparison holds no file beyond its read buffer.
	HashesOnly bool
}

func (l Limits) withDefaults() Limits {
	if l.MaxChanges <= 0 {
		l.MaxChanges = MaxChanges
	}
	if l.MaxChangedBytes <= 0 {
		l.MaxChangedBytes = MaxChangedBytes
	}
	return l
}

// Change is a file that differs from the base.
type Change struct {
	Path    string
	Deleted bool
	// Content of an added or changed file; empty when Result.TooLarge.
	Content string
	// SHA256 and Size of the current content; unset when Deleted.
	SHA256 string
	Size   int64
}

// Problem is something a scan could not use. An empty Path is a problem
// of the whole snapshot.
type Problem struct {
	Path    string
	Message string
}

// Result is the outcome of Compare.
type Result struct {
	// Changes relative to the base, in path order.
	Changes []Change
	// Problems in path order, the whole snapshot's first.
	Problems []Problem
	// TooLarge: more changes or changed content than the Limits allow.
	// With too many changes, or a scan stopped by a problem of the whole
	// snapshot, Changes is empty; with too much content, the changes come
	// without content.
	TooLarge bool
	// Complete is false when a problem of the whole snapshot stopped the
	// scan; nothing of the result describes the directory then.
	Complete bool
	// opaque holds paths whose content could not be determined: problems
	// and the directories that could not be read.
	opaque map[string]bool
}

// Unknown reports whether the state of p could not be determined: the
// scan was incomplete, or p or one of its parent directories is a problem.
// A base file whose state is unknown is not reported as deleted.
func (r *Result) Unknown(p string) bool {
	if !r.Complete {
		return true
	}
	for {
		if r.opaque[p] {
			return true
		}
		i := strings.LastIndexByte(p, '/')
		if i < 0 {
			return false
		}
		p = p[:i]
	}
}

// errOverflow stops a scan that met a problem of the whole snapshot.
var errOverflow = errors.New("snapshot scan stopped")

// scan walks a snapshot directory, calling visit for every regular file and
// recording problems for everything else.
type scan struct {
	ctx      context.Context
	entries  int
	problems []Problem
	opaque   map[string]bool
	// whole is the problem of the whole snapshot that stopped the scan.
	whole string
	buf   []byte
	visit func(dirFd int, name, path string, st *unix.Stat_t) error
}

func newScan(ctx context.Context) *scan {
	return &scan{ctx: ctx, opaque: make(map[string]bool)}
}

func (s *scan) problem(path, format string, args ...any) {
	path = strings.ToValidUTF8(path, "�")
	s.opaque[path] = true
	s.problems = append(s.problems, Problem{Path: path, Message: clip(fmt.Sprintf(format, args...))})
}

// clip bounds a message to 1024 bytes of valid UTF-8.
func clip(s string) string {
	s = strings.ToValidUTF8(s, "�")
	if len(s) <= 1024 {
		return s
	}
	cut := 1024
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// run walks the directory dir.
func (s *scan) run(dir *os.File) error {
	fd, err := unix.Openat(int(dir.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return &os.PathError{Op: "open", Path: dir.Name(), Err: err}
	}
	err = s.walk(fd, "", 0)
	if errors.Is(err, errOverflow) {
		return nil
	}
	return err
}

// readBatch is how many names one directory read returns.
const readBatch = 256

// walk reads the open directory fd, whose path inside the snapshot is
// prefix ("" or ending in '/'), and closes fd.
func (s *scan) walk(fd int, prefix string, depth int) error {
	dir := os.NewFile(uintptr(fd), prefix)
	defer dir.Close()
	for {
		names, err := dir.Readdirnames(readBatch)
		if err != nil && !errors.Is(err, io.EOF) {
			if prefix == "" {
				return &os.PathError{Op: "read", Path: dir.Name(), Err: err}
			}
			s.problem(strings.TrimSuffix(prefix, "/"), "could not be read: %v", errnoText(err))
			return nil
		}
		for _, name := range names {
			if err := s.entry(fd, prefix, name, depth); err != nil {
				return err
			}
		}
		if len(names) == 0 || errors.Is(err, io.EOF) {
			return nil
		}
	}
}

func (s *scan) entry(dirFd int, prefix, name string, depth int) error {
	if err := s.ctx.Err(); err != nil {
		return err
	}
	if s.entries++; s.entries > MaxEntries {
		s.whole = fmt.Sprintf("the snapshot has more than %d entries; nothing was collected", MaxEntries)
		return errOverflow
	}
	path := prefix + name
	switch {
	case !utf8.ValidString(name):
		s.problem(path, "name not valid UTF-8")
		return nil
	case hasControl(name):
		s.problem(path, "name with a control character")
		return nil
	case len(path) > MaxPathBytes || len(name) > maxSegmentBytes:
		s.problem(clipPath(path), "path longer than %d bytes", MaxPathBytes)
		return nil
	}
	var st unix.Stat_t
	if err := unix.Fstatat(dirFd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if !errors.Is(err, unix.ENOENT) {
			s.problem(path, "could not be examined: %v", errnoText(err))
		}
		return nil
	}
	switch {
	case isDir(&st):
		if depth+1 >= MaxDepth {
			s.problem(path, "nested more than %d directories deep", MaxDepth)
			return nil
		}
		child, err := unix.Openat(dirFd, name, dirFlags, 0)
		if err != nil {
			if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
				s.problem(path, "replaced while it was being read")
			} else if !errors.Is(err, unix.ENOENT) {
				s.problem(path, "could not be read: %v", errnoText(err))
			}
			return nil
		}
		return s.walk(child, path+"/", depth+1)
	case isRegular(&st):
		return s.visit(dirFd, name, path, &st)
	default:
		s.problem(path, "%s; the ledger holds only regular files", kindName(&st))
		return nil
	}
}

// read reads a regular file for visit. It returns false, after recording
// a problem, when the file cannot be used; ok with nil data means it
// vanished.
func (s *scan) read(dirFd int, name, path string, st *unix.Stat_t) (*unix.Stat_t, []byte, bool) {
	limit := MaxBytes(path)
	if st.Size > int64(limit) {
		s.problem(path, "%d bytes, over the %d-byte limit of a ledger file", st.Size, limit)
		return nil, nil, false
	}
	fst, data, err := readRegular(dirFd, name, s.buf, limit)
	s.buf = data
	switch {
	case errors.Is(err, unix.ENOENT):
		return nil, nil, true
	case errors.Is(err, errNotRegular):
		s.problem(path, "replaced while it was being read")
		return nil, nil, false
	case err != nil:
		s.problem(path, "could not be read: %v", errnoText(err))
		return nil, nil, false
	case len(data) > limit:
		s.problem(path, "%d bytes, over the %d-byte limit of a ledger file", max(fst.Size, int64(len(data))), limit)
		return nil, nil, false
	case !utf8.Valid(data):
		s.problem(path, "not valid UTF-8")
		return nil, nil, false
	}
	return &fst, data, true
}

func (s *scan) sortedProblems() []Problem {
	slices.SortFunc(s.problems, func(a, b Problem) int { return strings.Compare(a.Path, b.Path) })
	if s.whole != "" {
		s.problems = append([]Problem{{Message: s.whole}}, s.problems...)
	}
	return s.problems
}

// clipPath shortens an over-long path to MaxPathBytes of valid UTF-8.
func clipPath(p string) string {
	if len(p) <= MaxPathBytes {
		return p
	}
	cut := MaxPathBytes
	for cut > 0 && !utf8.RuneStart(p[cut]) {
		cut--
	}
	return p[:cut]
}

func errnoText(err error) string {
	var pe *os.PathError
	if errors.As(err, &pe) {
		err = pe.Err
	}
	return err.Error()
}

// Compare compares the directory dir with its manifest m: see the package
// documentation. It returns an error only when dir itself cannot be read
// or ctx ends.
func Compare(ctx context.Context, dir *os.File, m *Manifest, limits Limits) (*Result, error) {
	limits = limits.withDefaults()
	s := newScan(ctx)
	seen := make([]bool, len(m.Files))
	var changes []Change
	count, bytes, tooLarge := 0, 0, false
	add := func(c Change) {
		if count++; count > limits.MaxChanges {
			// Too many: keep counting, keep nothing.
			changes, tooLarge = nil, true
			return
		}
		if !c.Deleted && !limits.HashesOnly {
			if bytes += int(c.Size); bytes > limits.MaxChangedBytes && !tooLarge {
				tooLarge = true
				for i := range changes {
					changes[i].Content = ""
				}
			}
			if tooLarge {
				c.Content = ""
			}
		}
		changes = append(changes, c)
	}
	s.visit = func(dirFd int, name, path string, st *unix.Stat_t) error {
		var base *Entry
		i, found := slices.BinarySearchFunc(m.Files, path, func(e Entry, p string) int { return strings.Compare(e.Path, p) })
		if found {
			base, seen[i] = &m.Files[i], true
		}
		mtime := mtimeNs(st)
		if base != nil && base.Size == st.Size && base.MtimeNs == mtime && base.CtimeNs == ctimeNs(st) && base.Inode == uint64(st.Ino) && mtime < m.WrittenAtNs { //nolint:unconvert // uint32 on some platforms
			return nil
		}
		_, data, ok := s.read(dirFd, name, path, st)
		if !ok || data == nil {
			if ok && found {
				// Removed while being scanned.
				seen[i] = false
			}
			return nil
		}
		sum := Hash(data)
		if base != nil && base.SHA256 == sum {
			return nil
		}
		content := ""
		if !tooLarge && !limits.HashesOnly && count < limits.MaxChanges {
			content = string(data)
		}
		add(Change{Path: path, Content: content, SHA256: sum, Size: int64(len(data))})
		return nil
	}
	if err := s.run(dir); err != nil {
		return nil, err
	}
	r := &Result{opaque: s.opaque, Complete: s.whole == "", Problems: s.sortedProblems()}
	if !r.Complete {
		r.TooLarge = true
		return r, nil
	}
	for i, e := range m.Files {
		if !seen[i] && !r.Unknown(e.Path) {
			add(Change{Path: e.Path, Deleted: true})
		}
	}
	if count > limits.MaxChanges {
		r.TooLarge = true
		r.Problems = append([]Problem{{Message: fmt.Sprintf("more than %d files changed; nothing was collected", limits.MaxChanges)}}, r.Problems...)
		return r, nil
	}
	slices.SortFunc(changes, func(a, b Change) int { return strings.Compare(a.Path, b.Path) })
	r.Changes, r.TooLarge = changes, tooLarge
	return r, nil
}

// Rebuild makes the whole current content of dir a new base: it takes the
// manifest's time first, then reads and hashes every regular file,
// recording the stat taken before reading. Files that are problems are
// left out of the manifest and returned as problems; a problem of the
// whole snapshot (too many entries) is an error.
func Rebuild(ctx context.Context, dir *os.File, clock Clock, baseCommit string) (*Manifest, []Problem, error) {
	if !SameFileSystem(dir, clock.Dir) {
		return nil, nil, fmt.Errorf("the clock directory %s is not on the file system of %s", clock.Dir.Name(), dir.Name())
	}
	now, err := nextTick(clock)
	if err != nil {
		return nil, nil, err
	}
	s := newScan(ctx)
	m := &Manifest{Version: manifestVersion, BaseCommit: baseCommit, WrittenAtNs: now}
	s.visit = func(dirFd int, name, path string, st *unix.Stat_t) error {
		fst, data, ok := s.read(dirFd, name, path, st)
		if ok && data != nil {
			m.Files = append(m.Files, entryOf(path, fst, Hash(data)))
		}
		return nil
	}
	if err := s.run(dir); err != nil {
		return nil, nil, err
	}
	if s.whole != "" {
		return nil, nil, errors.New(s.whole)
	}
	slices.SortFunc(m.Files, func(a, b Entry) int { return strings.Compare(a.Path, b.Path) })
	return m, s.sortedProblems(), nil
}
