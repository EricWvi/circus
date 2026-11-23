package dirlist

import (
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/clock"
	"google.golang.org/protobuf/proto"
)

func require(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func list(t *testing.T, path string, hidden bool) *circusv1.ListDirectoryResult {
	t.Helper()
	clk := clock.New()
	lister := New(nil, slog.Default())
	result := lister.Read(t.Context(), path, hidden)
	lister.Mark(t.Context(), clk, clk.Now().Add(2*time.Second), result)
	return result
}

func TestResolve(t *testing.T) {
	home := t.TempDir()
	root := filepath.VolumeName(home) + string(filepath.Separator)
	for _, tc := range []struct {
		name, input, path, parent string
		code                      circusv1.DirectoryError
	}{
		{name: "empty", path: home, parent: filepath.Dir(home)},
		{name: "tilde", input: "~", path: home, parent: filepath.Dir(home)},
		{name: "subdirectory", input: "~/repo/../src/./", path: filepath.Join(home, "src"), parent: home},
		{name: "root", input: root, path: root},
		{name: "relative", input: "folder", code: circusv1.DirectoryError_DIRECTORY_ERROR_INVALID_PATH},
		{name: "other home", input: "~user", code: circusv1.DirectoryError_DIRECTORY_ERROR_INVALID_PATH},
		{name: "NUL", input: home + "\x00", code: circusv1.DirectoryError_DIRECTORY_ERROR_INVALID_PATH},
		{name: "too long", input: root + strings.Repeat("a", 4096), code: circusv1.DirectoryError_DIRECTORY_ERROR_INVALID_PATH},
		{name: "invalid UTF-8 path", input: home + "\xff", code: circusv1.DirectoryError_DIRECTORY_ERROR_INVALID_PATH},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := resolve(tc.input, func() (string, error) {
				return home + string(filepath.Separator) + "." + string(filepath.Separator), nil
			})
			want := &circusv1.ListDirectoryResult{Path: tc.path, Parent: tc.parent, Home: home, Error: tc.code}
			if !proto.Equal(got, want) {
				t.Fatalf("result = %v, want %v", got, want)
			}
		})
	}
}

func TestUnknownHome(t *testing.T) {
	path := t.TempDir()
	for name, value := range map[string]string{"non-UTF-8": path + "\xff", "relative": "relative-home"} {
		t.Run(name, func(t *testing.T) {
			home := func() (string, error) { return value, nil }
			for _, input := range []string{"", "~", "~/repo"} {
				got := resolve(input, home)
				want := &circusv1.ListDirectoryResult{Error: circusv1.DirectoryError_DIRECTORY_ERROR_INVALID_PATH}
				if !proto.Equal(got, want) {
					t.Fatalf("resolve %q = %v, want %v", input, got, want)
				}
			}
			got := resolve(path, home)
			want := &circusv1.ListDirectoryResult{Path: path, Parent: filepath.Dir(path)}
			if !proto.Equal(got, want) {
				t.Fatalf("absolute path = %v, want %v", got, want)
			}
		})
	}
}

func TestListDirectoryEntries(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"z", "b-worktree", "A-repository", ".hidden"} {
		require(t, os.Mkdir(filepath.Join(dir, name), 0700))
	}
	require(t, os.Mkdir(filepath.Join(dir, "A-repository", ".git"), 0700))
	require(t, os.WriteFile(filepath.Join(dir, "b-worktree", ".git"), nil, 0600))
	require(t, os.WriteFile(filepath.Join(dir, "file"), nil, 0600))
	if err := os.Symlink("A-repository", filepath.Join(dir, "link")); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("creating symlinks: %v", err)
		}
		t.Fatal(err)
	}
	require(t, os.Symlink("file", filepath.Join(dir, "link-to-file")))
	require(t, os.Symlink("missing", filepath.Join(dir, "broken")))

	path := dir + string(filepath.Separator) + "."
	want := &circusv1.ListDirectoryResult{Path: dir, Parent: filepath.Dir(dir), Home: Resolve(dir).Home,
		Entries: []*circusv1.DirectoryEntry{
			{Name: "A-repository", GitRepository: true},
			{Name: "b-worktree", GitRepository: true},
			{Name: "link", GitRepository: true, Symlink: true},
			{Name: "z"},
		}}
	if got := list(t, path, false); !proto.Equal(got, want) {
		t.Fatalf("directory entries = %v, want %v", got, want)
	}
	want.Entries = append([]*circusv1.DirectoryEntry{{Name: ".hidden"}}, want.Entries...)
	if got := list(t, path, true); !proto.Equal(got, want) {
		t.Fatalf("including hidden = %v, want %v", got, want)
	}
	link := filepath.Join(dir, "link")
	if got := list(t, link, false); got.Path != link || got.Parent != dir || got.Error != 0 {
		t.Fatalf("listing a symlink should preserve the lexical path: %v", got)
	}
}

func TestListDirectoryErrors(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	require(t, os.WriteFile(file, nil, 0600))
	for _, tc := range []struct {
		path string
		code circusv1.DirectoryError
	}{
		{filepath.Join(dir, "missing"), circusv1.DirectoryError_DIRECTORY_ERROR_NOT_FOUND},
		{file, circusv1.DirectoryError_DIRECTORY_ERROR_NOT_DIRECTORY},
		{filepath.Join(file, "child"), circusv1.DirectoryError_DIRECTORY_ERROR_NOT_DIRECTORY},
	} {
		if got := list(t, tc.path, true); got.Error != tc.code || got.Path != tc.path || got.Parent != filepath.Dir(tc.path) || len(got.Entries) != 0 {
			t.Errorf("list %q = %v, want %v", tc.path, got, tc.code)
		}
	}
	t.Run("permission denied", func(t *testing.T) {
		if os.Geteuid() == 0 || runtime.GOOS == "windows" {
			t.Skip("Unix directory permissions required")
		}
		require(t, os.Chmod(dir, 0))
		defer func() { require(t, os.Chmod(dir, 0700)) }()
		if got := list(t, dir, false); got.Error != circusv1.DirectoryError_DIRECTORY_ERROR_PERMISSION_DENIED {
			t.Fatalf("list inaccessible directory = %v", got)
		}
	})
}

func TestListDirectoryCaps(t *testing.T) {
	t.Run("kept entries", func(t *testing.T) {
		dir := t.TempDir()
		for i := maxEntries; i >= 0; i-- {
			require(t, os.Mkdir(filepath.Join(dir, fmt.Sprintf("dir-%04d", i)), 0700))
		}
		got := list(t, dir, true)
		if !got.Truncated || got.Error != 0 || len(got.Entries) != maxEntries {
			t.Fatalf("truncated = %v, error = %v, count = %d", got.Truncated, got.Error, len(got.Entries))
		}
		for i, entry := range got.Entries {
			if want := fmt.Sprintf("dir-%04d", i); entry.Name != want {
				t.Fatalf("entry %d = %q, want %q", i, entry.Name, want)
			}
		}
	})
}

type enumeratedFS struct {
	OSFileSystem
	dir *countedDirectory
}

func (f enumeratedFS) Open(string) (fs.ReadDirFile, error) { return f.dir, nil }

type countedDirectory struct {
	fs.ReadDirFile
	entry     fs.DirEntry
	remaining int
	requested int
}

func (*countedDirectory) Close() error { return nil }

func (d *countedDirectory) ReadDir(n int) ([]fs.DirEntry, error) {
	if n < 1 || n > readBatch {
		return nil, fmt.Errorf("invalid read size %d", n)
	}
	d.requested += n
	if d.remaining == 0 {
		return nil, io.EOF
	}
	batch := make([]fs.DirEntry, min(n, d.remaining))
	for i := range batch {
		batch[i] = d.entry
	}
	d.remaining -= len(batch)
	return batch, nil
}

func TestListDirectoryRawCap(t *testing.T) {
	dir := t.TempDir()
	require(t, os.WriteFile(filepath.Join(dir, "file"), nil, 0600))
	entries, err := os.ReadDir(dir)
	require(t, err)
	for _, count := range []int{maxRawEntries, maxRawEntries + 1} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			file := &countedDirectory{entry: entries[0], remaining: count}
			got := New(enumeratedFS{dir: file}, slog.Default()).Read(t.Context(), dir, true)
			if got.Truncated != (count > maxRawEntries) || got.Error != 0 || len(got.Entries) != 0 {
				t.Fatalf("list with %d raw entries = %v", count, got)
			}
			if file.requested != maxRawEntries+1 {
				t.Fatalf("requested %d raw records, want one lookahead beyond %d", file.requested, maxRawEntries)
			}
		})
	}
}

func TestListDirectoryOmitsInvalidUTF8(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires a filesystem with byte-oriented names")
	}
	dir := t.TempDir()
	for _, name := range []string{"a-\xff", "b", "c"} {
		require(t, os.Mkdir(filepath.Join(dir, name), 0700))
	}
	got := list(t, dir, true)
	var names []string
	for _, entry := range got.Entries {
		names = append(names, entry.Name)
	}
	if !slices.Equal(names, []string{"b", "c"}) || got.Truncated || got.Error != 0 {
		t.Fatalf("list with a non-UTF-8 name = %v", got)
	}
	if _, err := proto.Marshal(got); err != nil {
		t.Fatal(err)
	}
}

// statCountingFS fails every probe immediately and counts symlink Stat calls.
type statCountingFS struct {
	OSFileSystem
	stats int
}

func (f *statCountingFS) Stat(string) (fs.FileInfo, error)  { f.stats++; return nil, fs.ErrNotExist }
func (f *statCountingFS) Lstat(string) (fs.FileInfo, error) { return nil, fs.ErrNotExist }

func TestMarkSkipsProbesPastEntryCap(t *testing.T) {
	files := &statCountingFS{}
	entries := make([]*circusv1.DirectoryEntry, 0, maxEntries+1)
	for i := range maxEntries {
		entries = append(entries, &circusv1.DirectoryEntry{Name: fmt.Sprintf("d%04d", i)})
	}
	entries = append(entries, &circusv1.DirectoryEntry{Name: "zz-link", Symlink: true})
	result := &circusv1.ListDirectoryResult{Path: "/x", Entries: entries}
	clk := clock.New()
	New(files, slog.Default()).Mark(t.Context(), clk, clk.Now().Add(time.Hour), result)
	if files.stats != 0 {
		t.Fatalf("symlink beyond the entry cap was probed %d times", files.stats)
	}
	if len(result.Entries) != maxEntries || !result.Truncated {
		t.Fatalf("got %d entries, truncated=%v; want %d, true", len(result.Entries), result.Truncated, maxEntries)
	}
}

// hangingFS blocks every Lstat until release is closed, after signalling
// entered.
type hangingFS struct {
	OSFileSystem
	entered chan struct{}
	release chan struct{}
}

func (f *hangingFS) Lstat(string) (fs.FileInfo, error) {
	f.entered <- struct{}{}
	<-f.release
	return nil, fs.ErrNotExist
}

// racyClock is a fake clock that another goroutine advances by skew
// whenever a timer is armed with After, just before it is armed: the
// moment between reading Now and calling After.
type racyClock struct {
	*clock.Fake
	skew time.Duration
}

func (c racyClock) After(d time.Duration) <-chan time.Time {
	c.Advance(c.skew)
	return c.Fake.After(d)
}

// TestMarkStopsAtDeadlineDespiteRacingAdvance: Mark gives up on a hung
// probe once the clock reaches the deadline, even when the clock moved
// between reading the time and arming the alarm.
func TestMarkStopsAtDeadlineDespiteRacingAdvance(t *testing.T) {
	files := &hangingFS{entered: make(chan struct{}, 1), release: make(chan struct{})}
	defer close(files.release)
	clk := racyClock{Fake: clock.New(), skew: time.Second}
	deadline := clk.Now().Add(2 * time.Second)
	result := &circusv1.ListDirectoryResult{Path: "/x", Entries: []*circusv1.DirectoryEntry{{Name: "a"}}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		New(files, slog.Default()).Mark(t.Context(), clk, deadline, result)
	}()
	select {
	case <-files.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("probe never started")
	}
	clk.Advance(deadline.Sub(clk.Now()))
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Mark still waiting after the clock reached its deadline")
	}
	if len(result.Entries) != 1 || result.Entries[0].GitRepository {
		t.Fatalf("entries %v", result.Entries)
	}
}
