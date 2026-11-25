package snapshot

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func openDir(t *testing.T, path string) *os.File {
	t.Helper()
	d, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func put(t *testing.T, root, p, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(p))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// settle waits until the file system clock of d has moved past everything
// written so far, so a manifest taken next finds no racy file.
func settle(t *testing.T, d *os.File) {
	t.Helper()
	start, err := fsNow(Clock{Dir: d})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		now, err := fsNow(Clock{Dir: d})
		if err != nil {
			t.Fatal(err)
		}
		if now > start {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the file system clock did not move")
		}
		time.Sleep(time.Millisecond)
	}
}

// testClock is a clock directory beside the test's snapshots, on their
// file system.
func testClock(t *testing.T) Clock { return Clock{Dir: openDir(t, t.TempDir())} }

// base writes files into a new directory and returns it with a manifest
// in which no file is racy.
func base(t *testing.T, files map[string]string) (string, *os.File, *Manifest) {
	t.Helper()
	root := t.TempDir()
	for p, c := range files {
		put(t, root, p, c)
	}
	d := openDir(t, root)
	settle(t, d)
	m, problems, err := Rebuild(t.Context(), d, testClock(t), strings.Repeat("a", 40))
	if err != nil || len(problems) != 0 {
		t.Fatalf("rebuild: %v %v", problems, err)
	}
	return root, d, m
}

func compare(t *testing.T, d *os.File, m *Manifest) *Result {
	t.Helper()
	r, err := Compare(t.Context(), d, m, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func summary(r *Result) string {
	var parts []string
	for _, c := range r.Changes {
		switch {
		case c.Deleted:
			parts = append(parts, "-"+c.Path)
		case c.Content == "":
			parts = append(parts, fmt.Sprintf("~%s(%d)", c.Path, c.Size))
		default:
			parts = append(parts, fmt.Sprintf("+%s=%s", c.Path, c.Content))
		}
	}
	for _, p := range r.Problems {
		parts = append(parts, "!"+p.Path)
	}
	return strings.Join(parts, " ")
}

func TestCompareFindsChanges(t *testing.T) {
	root, d, m := base(t, map[string]string{"a.md": "a", "core/b.md": "b", "core/c.md": "c", ".hidden/x": "x"})
	if got := summary(compare(t, d, m)); got != "" {
		t.Fatalf("unchanged snapshot: %s", got)
	}
	put(t, root, "core/b.md", "bb")
	put(t, root, "new.md", "n")
	put(t, root, ".swp", "editor")
	if err := os.Remove(filepath.Join(root, "core", "c.md")); err != nil {
		t.Fatal(err)
	}
	r := compare(t, d, m)
	if got := summary(r); got != "+.swp=editor +core/b.md=bb -core/c.md +new.md=n" {
		t.Fatalf("changes: %s", got)
	}
	if c := r.Changes[1]; c.SHA256 != Hash("bb") || c.Size != 2 {
		t.Fatalf("changed file: %+v", c)
	}
	if !r.Complete || r.TooLarge {
		t.Fatalf("result: %+v", r)
	}
}

// countReads counts the files Compare and Rebuild read until the test
// ends.
func countReads(t *testing.T) *[]string {
	var read []string
	beforeRead = func(name string) { read = append(read, name) }
	t.Cleanup(func() { beforeRead = nil })
	return &read
}

// TestCompareStatHitReadsNothing: a file whose stat matches is not opened;
// any change of its stat, a chmod included (it moves ctime), makes it
// read.
func TestCompareStatHitReadsNothing(t *testing.T) {
	root, d, m := base(t, map[string]string{"a.md": "a", "b.md": "b"})
	read := countReads(t)
	if got := summary(compare(t, d, m)); got != "" || len(*read) != 0 {
		t.Fatalf("stat hit: %s, read %v", got, *read)
	}
	if err := os.Chmod(filepath.Join(root, "a.md"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := summary(compare(t, d, m)); got != "" || !slices.Equal(*read, []string{"a.md"}) {
		t.Fatalf("after chmod: %s, read %v", got, *read)
	}
}

// TestCompareStatFields: each of size, modification time, inode change
// time and inode alone makes a file read.
func TestCompareStatFields(t *testing.T) {
	_, d, m := base(t, map[string]string{"a.md": "a"})
	for _, tc := range []struct {
		name   string
		change func(e *Entry)
	}{
		{"size", func(e *Entry) { e.Size++ }},
		{"mtime", func(e *Entry) { e.MtimeNs-- }},
		{"ctime", func(e *Entry) { e.CtimeNs-- }},
		{"inode", func(e *Entry) { e.Inode++ }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			other := *m
			other.Files = slices.Clone(m.Files)
			tc.change(&other.Files[0])
			read := countReads(t)
			if got := summary(compare(t, d, &other)); got != "" || len(*read) != 1 {
				t.Fatalf("%s differs: %s, read %v", tc.name, got, *read)
			}
		})
	}
}

// TestCompareRenameOntoSameStat: a file renamed onto another of the same
// size and modification time is told apart by its inode.
func TestCompareRenameOntoSameStat(t *testing.T) {
	root := t.TempDir()
	put(t, root, "a.md", "aaaa")
	put(t, root, "b.md", "bbbb")
	old := time.Unix(1700000000, 123456789)
	for _, p := range []string{"a.md", "b.md"} {
		if err := os.Chtimes(filepath.Join(root, p), old, old); err != nil {
			t.Fatal(err)
		}
	}
	d := openDir(t, root)
	m, _, err := Rebuild(t.Context(), d, testClock(t), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, "b.md"), filepath.Join(root, "a.md")); err != nil {
		t.Fatal(err)
	}
	if got := summary(compare(t, d, m)); got != "+a.md=bbbb -b.md" {
		t.Fatalf("rename onto a same-stat file: %s", got)
	}
}

// TestCompareMtimeSetBack: a same-size rewrite whose modification time is
// set back (touch -d, cp -p) is found by its inode change time, as git
// finds it.
func TestCompareMtimeSetBack(t *testing.T) {
	root, d, m := base(t, map[string]string{"a.md": "one"})
	path := filepath.Join(root, "a.md")
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("two"), 0); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	mtime := time.Unix(0, m.Lookup("a.md").MtimeNs)
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	if got := summary(compare(t, d, m)); got != "+a.md=two" {
		t.Fatalf("mtime set back: %s", got)
	}
}

// TestCompareRacyMtime: a file changed within the timestamp tick of the
// manifest keeps size, inode and mtime, and is still found, because its
// mtime is not earlier than the manifest's.
func TestCompareRacyMtime(t *testing.T) {
	root, d, m := base(t, map[string]string{"a.md": "aaaa"})
	e := m.Lookup("a.md")
	path := filepath.Join(root, "a.md")
	rewriteSameStat := func(content string) {
		t.Helper()
		put(t, root, "a.md", content)
		ts := unix.NsecToTimespec(e.MtimeNs)
		if err := unix.UtimesNanoAt(unix.AT_FDCWD, path, []unix.Timespec{ts, ts}, 0); err != nil {
			t.Fatal(err)
		}
		var st unix.Stat_t
		if err := unix.Stat(path, &st); err != nil || mtimeNs(&st) != e.MtimeNs || st.Size != e.Size || uint64(st.Ino) != e.Inode { //nolint:unconvert // uint32 on some platforms
			t.Fatalf("stat not restored: %+v %v", st, err)
		}
	}
	// The rewrite moved the inode change time, which nobody can set
	// back; these manifests take it as recorded, as if the rewrite had
	// happened within the tick ctime was recorded in.
	withCtime := func(writtenAt int64) *Manifest {
		var st unix.Stat_t
		if err := unix.Stat(path, &st); err != nil {
			t.Fatal(err)
		}
		other := *m
		other.Files = slices.Clone(m.Files)
		other.Files[0].CtimeNs = ctimeNs(&st)
		other.WrittenAtNs = writtenAt
		return &other
	}
	// Outside the racy window, an identical stat is trusted, as in git.
	rewriteSameStat("bbbb")
	if got := summary(compare(t, d, withCtime(m.WrittenAtNs))); got != "" {
		t.Fatalf("non-racy identical stat: %s", got)
	}
	// The manifest taken within the file's tick: the same edit is found.
	rewriteSameStat("cccc")
	if got := summary(compare(t, d, withCtime(e.MtimeNs))); got != "+a.md=cccc" {
		t.Fatalf("racy file: %s", got)
	}
	// Back to the base content within the tick: no change.
	rewriteSameStat("aaaa")
	if got := summary(compare(t, d, withCtime(e.MtimeNs))); got != "" {
		t.Fatalf("racy file back to base: %s", got)
	}
}

func TestCompareChangedBack(t *testing.T) {
	root, d, m := base(t, map[string]string{"a.md": "same"})
	put(t, root, "a.md", "other content")
	if got := summary(compare(t, d, m)); got != "+a.md=other content" {
		t.Fatalf("changed: %s", got)
	}
	put(t, root, "a.md", "same")
	if got := summary(compare(t, d, m)); got != "" {
		t.Fatalf("changed back: %s", got)
	}
}

// TestCompareRenameAndOrder: a rename is a deletion and an addition, and
// changes come in byte order of the whole path.
func TestCompareRenameAndOrder(t *testing.T) {
	root, d, m := base(t, map[string]string{"a/b.md": "x"})
	if err := os.Rename(filepath.Join(root, "a", "b.md"), filepath.Join(root, "a-c.md")); err != nil {
		t.Fatal(err)
	}
	put(t, root, "a/z.md", "z")
	if got := summary(compare(t, d, m)); got != "+a-c.md=x -a/b.md +a/z.md=z" {
		t.Fatalf("rename: %s", got)
	}
}

// TestCompareNonRegularFiles: symlinks, FIFOs and sockets are problems,
// never followed or opened; a FIFO does not block the scan.
func TestCompareNonRegularFiles(t *testing.T) {
	outside := t.TempDir()
	put(t, outside, "secret.md", "secret")
	put(t, outside, "dir/inner.md", "inner")
	root, d, m := base(t, map[string]string{"a.md": "a", "core/b.md": "b", "keep.md": "k"})
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.Symlink(filepath.Join(outside, "secret.md"), filepath.Join(root, "link.md")))
	must(os.Symlink(filepath.Join(outside, "dir"), filepath.Join(root, "linkdir")))
	must(os.Symlink("missing", filepath.Join(root, "dangling")))
	must(unix.Mkfifo(filepath.Join(root, "fifo"), 0o644))
	// A base file replaced by a symlink, and a base directory by one.
	must(os.Remove(filepath.Join(root, "a.md")))
	must(os.Symlink(filepath.Join(outside, "secret.md"), filepath.Join(root, "a.md")))
	must(os.RemoveAll(filepath.Join(root, "core")))
	must(os.Symlink(filepath.Join(outside, "dir"), filepath.Join(root, "core")))
	want := "!a.md !core !dangling !fifo !link.md !linkdir"
	if l, err := net.Listen("unix", filepath.Join(root, "sock")); err == nil {
		defer l.Close()
		want += " !sock"
	}
	done := make(chan *Result, 1)
	go func() {
		r, err := Compare(context.Background(), d, m, Limits{})
		if err != nil {
			t.Error(err)
		}
		done <- r
	}()
	var r *Result
	select {
	case r = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the scan blocked")
	}
	if got := summary(r); got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
	for _, p := range r.Problems {
		if strings.Contains(p.Message, "secret") || strings.Contains(p.Message, "inner") {
			t.Fatalf("followed: %v", p)
		}
	}
	// Base files at or below a problem are not reported deleted.
	if !r.Unknown("a.md") || !r.Unknown("core/b.md") || r.Unknown("keep.md") {
		t.Fatal("unknown paths")
	}
}

func TestCompareInvalidContentAndNames(t *testing.T) {
	root, d, m := base(t, nil)
	put(t, root, "bad.md", "\xff\xfe")
	put(t, root, "bad\xffname.md", "x")
	put(t, root, "tab\tname.md", "x")
	r := compare(t, d, m)
	if len(r.Changes) != 0 || len(r.Problems) != 3 {
		t.Fatalf("problems: %s %v", summary(r), r.Problems)
	}
	for _, p := range r.Problems {
		if !strings.Contains(p.Message, "UTF-8") && !strings.Contains(p.Message, "control character") {
			t.Fatalf("problem: %v", p)
		}
	}
}

func TestCompareFileSizeLimit(t *testing.T) {
	root, d, m := base(t, nil)
	put(t, root, "at.md", strings.Repeat("x", MaxFileBytes))
	put(t, root, "over.md", strings.Repeat("x", MaxFileBytes+1))
	r := compare(t, d, m)
	if len(r.Changes) != 1 || r.Changes[0].Path != "at.md" || len(r.Changes[0].Content) != MaxFileBytes {
		t.Fatalf("changes: %s", summary(r))
	}
	if len(r.Problems) != 1 || r.Problems[0].Path != "over.md" || !strings.Contains(r.Problems[0].Message, fmt.Sprint(MaxFileBytes+1)) {
		t.Fatalf("problems: %v", r.Problems)
	}
	// A file that grew after its stat is caught by the bounded read.
	_, data, err := readRegular(int(d.Fd()), "over.md", nil, MaxFileBytes)
	if err != nil || len(data) != MaxFileBytes+1 {
		t.Fatalf("bounded read: %d %v", len(data), err)
	}
}

func TestCompareEntryLimit(t *testing.T) {
	for _, n := range []int{MaxEntries, MaxEntries + 1} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			root, d, m := base(t, map[string]string{"base.md": "b"})
			for i := 1; i < n; i++ {
				put(t, root, fmt.Sprintf("f%05d", i), "")
			}
			// Every entry is a change: the change limit is not under test.
			r, err := Compare(t.Context(), d, m, Limits{MaxChanges: MaxEntries})
			if err != nil {
				t.Fatal(err)
			}
			if n == MaxEntries {
				if !r.Complete || r.TooLarge || len(r.Changes) != n-1 || len(r.Problems) != 0 {
					t.Fatalf("at the limit: complete %v, %d changes, %v", r.Complete, len(r.Changes), r.Problems)
				}
				return
			}
			if r.Complete || !r.TooLarge || len(r.Changes) != 0 || len(r.Problems) != 1 || r.Problems[0].Path != "" ||
				!strings.Contains(r.Problems[0].Message, "8192 entries") || !r.Unknown("base.md") {
				t.Fatalf("over the limit: complete %v, %d changes, %v", r.Complete, len(r.Changes), r.Problems)
			}
		})
	}
}

func TestCompareChangeCountLimit(t *testing.T) {
	for _, n := range []int{MaxChanges, MaxChanges + 1} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			// One deletion and n-1 additions.
			root, d, m := base(t, map[string]string{"gone.md": "g"})
			if err := os.Remove(filepath.Join(root, "gone.md")); err != nil {
				t.Fatal(err)
			}
			for i := 1; i < n; i++ {
				put(t, root, fmt.Sprintf("f%05d", i), "x")
			}
			r := compare(t, d, m)
			if n == MaxChanges {
				if r.TooLarge || len(r.Changes) != n || r.Changes[n-1].Path != "gone.md" || !r.Changes[n-1].Deleted || r.Changes[0].Content != "x" {
					t.Fatalf("at the limit: too large %v, %d changes", r.TooLarge, len(r.Changes))
				}
				return
			}
			if !r.TooLarge || len(r.Changes) != 0 || len(r.Problems) != 1 || !strings.Contains(r.Problems[0].Message, "more than 4096 files changed") {
				t.Fatalf("over the limit: too large %v, %d changes, %v", r.TooLarge, len(r.Changes), r.Problems)
			}
		})
	}
}

func TestCompareChangedBytesLimit(t *testing.T) {
	const files = MaxChangedBytes / MaxFileBytes
	for _, extra := range []int{0, 1} {
		t.Run(fmt.Sprint(extra), func(t *testing.T) {
			root, d, m := base(t, nil)
			for i := range files {
				put(t, root, fmt.Sprintf("f%02d", i), strings.Repeat("x", MaxFileBytes))
			}
			if extra > 0 {
				put(t, root, "g", "y")
			}
			r := compare(t, d, m)
			if len(r.Changes) != files+extra || len(r.Problems) != 0 {
				t.Fatalf("%d changes, %v", len(r.Changes), r.Problems)
			}
			for _, c := range r.Changes {
				if c.SHA256 == "" || c.Size == 0 || (c.Content == "") != (extra > 0) {
					t.Fatalf("change %s: %d bytes of content, size %d", c.Path, len(c.Content), c.Size)
				}
			}
			if r.TooLarge != (extra > 0) {
				t.Fatalf("too large: %v", r.TooLarge)
			}
		})
	}
}

// mkdeep creates segs as nested directories inside root through
// descriptors, which allows paths longer than PATH_MAX, and a file in the
// innermost.
func mkdeep(t *testing.T, root *os.File, segs []string, file string) {
	t.Helper()
	fd, err := unix.Openat(int(root.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range segs {
		if err := unix.Mkdirat(fd, s, 0o755); err != nil && !errors.Is(err, unix.EEXIST) {
			t.Fatal(err)
		}
		next, err := unix.Openat(fd, s, unix.O_RDONLY|unix.O_DIRECTORY, 0)
		_ = unix.Close(fd)
		if err != nil {
			t.Fatal(err)
		}
		fd = next
	}
	defer unix.Close(fd)
	f, err := unix.Openat(fd, file, unix.O_WRONLY|unix.O_CREAT, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_ = unix.Close(f)
	t.Cleanup(func() {
		// t.TempDir's removal cannot reach so deep a path.
		_ = os.RemoveAll(filepath.Join(root.Name(), segs[0]))
		_ = unix.Unlinkat(int(root.Fd()), segs[0], unix.AT_REMOVEDIR)
	})
}

func TestComparePathAndDepthLimits(t *testing.T) {
	_, d, m := base(t, nil)
	segs := make([]string, 16)
	for i := range segs {
		segs[i] = fmt.Sprintf("%c%s", 'a'+i, strings.Repeat("d", 253))
	}
	prefix := strings.Join(segs, "/") + "/"
	mkdeep(t, d, segs, strings.Repeat("f", MaxPathBytes-len(prefix)))
	mkdeep(t, d, segs, strings.Repeat("g", MaxPathBytes-len(prefix)+1))
	deep := make([]string, MaxDepth)
	for i := range deep {
		deep[i] = "n"
	}
	mkdeep(t, d, deep[:MaxDepth-1], "ok.md")
	mkdeep(t, d, deep, "deep.md")
	r := compare(t, d, m)
	var paths []string
	for _, c := range r.Changes {
		paths = append(paths, fmt.Sprint(len(c.Path)))
	}
	if len(r.Changes) != 2 || len(r.Changes[0].Path) != MaxPathBytes || r.Changes[1].Path != strings.Repeat("n/", MaxDepth-1)+"ok.md" {
		t.Fatalf("changes (path lengths %v)", paths)
	}
	if len(r.Problems) != 2 || !strings.Contains(r.Problems[0].Message, "longer than 4096") ||
		r.Problems[1].Path != strings.Repeat("n/", MaxDepth-1)+"n" || !strings.Contains(r.Problems[1].Message, "nested more than 128") {
		t.Fatalf("problems: %v", r.Problems)
	}
}

func TestRebuildThenCompareIsClean(t *testing.T) {
	root, d, _ := base(t, map[string]string{"a.md": "a"})
	put(t, root, "b.md", "b")
	if err := os.Symlink("a.md", filepath.Join(root, "l")); err != nil {
		t.Fatal(err)
	}
	m, problems, err := Rebuild(t.Context(), d, testClock(t), strings.Repeat("b", 40))
	if err != nil || len(problems) != 1 || problems[0].Path != "l" {
		t.Fatalf("rebuild: %v %v", problems, err)
	}
	if len(m.Files) != 2 || m.Lookup("b.md").SHA256 != Hash("b") || m.BaseCommit != strings.Repeat("b", 40) {
		t.Fatalf("manifest: %+v", m)
	}
	r := compare(t, d, m)
	if len(r.Changes) != 0 || len(r.Problems) != 1 {
		t.Fatalf("after rebuild: %s", summary(r))
	}
}

func TestManifestFile(t *testing.T) {
	_, _, m := base(t, map[string]string{"a.md": "a", "z/b.md": "b"})
	dir := openDir(t, t.TempDir())
	if _, err := ReadManifest(dir, "m.json"); !errors.Is(err, ErrNoManifest) {
		t.Fatalf("missing manifest: %v", err)
	}
	if err := WriteManifest(dir, "m.json", m); err != nil {
		t.Fatal(err)
	}
	got, err := ReadManifest(dir, "m.json")
	if err != nil || !slices.Equal(got.Files, m.Files) || got.WrittenAtNs != m.WrittenAtNs || got.BaseCommit != m.BaseCommit {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	if names, _ := os.ReadDir(dir.Name()); len(names) != 1 {
		t.Fatalf("left behind: %v", names)
	}
	bad := *m
	bad.Files = []Entry{m.Files[1], m.Files[0]}
	if err := WriteManifest(dir, "bad.json", &bad); err == nil {
		t.Fatal("unsorted manifest written")
	}
	put(t, dir.Name(), "corrupt.json", `{"version":1,"files":[{"path":"../x","sha256":"`+Hash("")+`"}]}`)
	if _, err := ReadManifest(dir, "corrupt.json"); err == nil {
		t.Fatal("corrupt manifest read")
	}
	if err := os.Symlink("m.json", filepath.Join(dir.Name(), "link.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadManifest(dir, "link.json"); err == nil {
		t.Fatal("symlinked manifest read")
	}
	if removed, err := RemoveManifest(dir, "m.json"); !removed || err != nil {
		t.Fatalf("remove: %v %v", removed, err)
	}
}

func TestWriterAndManifest(t *testing.T) {
	root := t.TempDir()
	d := openDir(t, root)
	w := NewWriter(d)
	w.Sync = true
	clock := testClock(t)
	w.Clocks = []Clock{clock}
	for _, f := range []File{{"b.md", "b"}, {"core/a.md", "a"}, {"b.md", "bb"}} {
		if err := w.Add(f.Path, f.Content); err != nil {
			t.Fatal(err)
		}
	}
	m, err := w.Manifest(strings.Repeat("c", 64))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Files) != 2 || m.Files[0].Path != "b.md" || m.Files[0].SHA256 != Hash("bb") || m.Files[0].Size != 2 {
		t.Fatalf("manifest: %+v", m.Files)
	}
	if entries, _ := os.ReadDir(root); len(entries) != 2 {
		t.Fatalf("probe left behind: %v", entries)
	}
	// The manifest's time waits for the clock to move: fresh files are
	// not racy.
	for _, e := range m.Files {
		if e.MtimeNs >= m.WrittenAtNs {
			t.Fatalf("%s is racy: %d >= %d", e.Path, e.MtimeNs, m.WrittenAtNs)
		}
	}
	if r := compare(t, d, m); len(r.Changes) != 0 {
		t.Fatalf("after writing: %s", summary(r))
	}
	for _, p := range []string{"", "/abs", "a/../b", "./a", "a//b", "a\x00b", "tab\tx"} {
		if err := w.Add(p, "x"); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("Add(%q) = %v", p, err)
		}
	}
	if err := w.Add("x.md", "\xff"); err == nil {
		t.Error("invalid UTF-8 written")
	}
	// A symlinked directory on the way is refused, not followed.
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "out")); err != nil {
		t.Fatal(err)
	}
	if err := w.Add("out/x.md", "x"); !errors.Is(err, unix.ELOOP) && !errors.Is(err, unix.ENOTDIR) {
		t.Fatalf("through a symlink: %v", err)
	}
	if err := os.Symlink(filepath.Join(outside, "target"), filepath.Join(root, "link.md")); err != nil {
		t.Fatal(err)
	}
	if err := w.Add("link.md", "x"); err == nil {
		t.Fatal("wrote through a symlink")
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("wrote outside: %v", entries)
	}
}

// TestWriterWithoutClock: without a clock on the copy's file system the
// manifest's time is the newest file's, so only the files of that last
// tick stay racy, and no probe is ever created in the copy.
func TestWriterWithoutClock(t *testing.T) {
	root := t.TempDir()
	d := openDir(t, root)
	m, err := Write(d, nil, "", []File{{"a.md", "a"}, {"b.md", "b"}})
	if err != nil {
		t.Fatal(err)
	}
	newest := max(m.Files[0].MtimeNs, m.Files[1].MtimeNs)
	if m.WrittenAtNs != newest {
		t.Fatalf("time %d, newest file %d", m.WrittenAtNs, newest)
	}
	// A clock on another file system is not used either.
	cwd, err := os.MkdirTemp(".", ".clock-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(cwd)
	other := openDir(t, cwd)
	if SameFileSystem(d, other) {
		t.Skip("the working directory and the temporary directory are on one file system")
	}
	m, err = Write(d, []Clock{{Dir: other}}, "", []File{{"c.md", "c"}})
	if err != nil || m.WrittenAtNs != m.Lookup("c.md").MtimeNs {
		t.Fatalf("clock on another file system: %+v %v", m, err)
	}
	if entries, _ := os.ReadDir(cwd); len(entries) != 0 {
		t.Fatalf("probe in the other file system: %v", entries)
	}
	if _, _, err := Rebuild(t.Context(), d, Clock{Dir: other}, ""); err == nil {
		t.Fatal("Rebuild took the time of another file system")
	}
}

// TestClockProbeOutsideSnapshot: a probe left by a process killed while it
// existed is in the clock directory, never in the snapshot; a named probe
// left there is removed by the next use.
func TestClockProbeOutsideSnapshot(t *testing.T) {
	root, d, m := base(t, map[string]string{"a.md": "a"})
	clock := testClock(t)
	clock.Name = "id.clock"
	afterProbe = func() error { return errors.New("killed") }
	_, _, err := Rebuild(t.Context(), d, clock, "")
	afterProbe = nil
	if err == nil {
		t.Fatal("the injected failure was not seen")
	}
	if entries, _ := os.ReadDir(root); len(entries) != 1 {
		t.Fatalf("snapshot after the failure: %v", entries)
	}
	if got := summary(compare(t, d, m)); got != "" {
		t.Fatalf("collect after the failure: %s", got)
	}
	if entries, _ := os.ReadDir(clock.Dir.Name()); len(entries) != 1 || entries[0].Name() != "id.clock" {
		t.Fatalf("clock directory after the failure: %v", entries)
	}
	if _, _, err := Rebuild(t.Context(), d, clock, ""); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(clock.Dir.Name()); len(entries) != 0 {
		t.Fatalf("clock directory after the next use: %v", entries)
	}
}

// TestCompareFileGrowsWhileRead: a file within the limit at its stat that
// grows before it is read is caught by the bounded read, never collected
// cut to the limit.
func TestCompareFileGrowsWhileRead(t *testing.T) {
	root, d, m := base(t, nil)
	put(t, root, "grow.md", strings.Repeat("x", MaxFileBytes))
	beforeRead = func(name string) {
		f, err := os.OpenFile(filepath.Join(root, name), os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			t.Error(err)
			return
		}
		_, _ = f.WriteString("y")
		_ = f.Close()
	}
	defer func() { beforeRead = nil }()
	r := compare(t, d, m)
	if len(r.Changes) != 0 || len(r.Problems) != 1 || !strings.Contains(r.Problems[0].Message, fmt.Sprint(MaxFileBytes+1)) {
		t.Fatalf("grown file: %s %v", summary(r), r.Problems)
	}
}

func TestReadState(t *testing.T) {
	root, d, _ := base(t, map[string]string{"a.md": "a"})
	outside := t.TempDir()
	put(t, outside, "target.md", "outside")
	if err := os.Symlink(filepath.Join(outside, "target.md"), filepath.Join(root, "link.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "dirlink")); err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]State{"a.md": {true, Hash("a")}, "none.md": {}, "x/none.md": {}, "link.md": {Exists: true}, "dirlink/target.md": {Exists: true}} {
		if got, err := ReadState(d, p); err != nil || got != want {
			t.Errorf("ReadState(%s) = %+v, %v; want %+v", p, got, err, want)
		}
	}
}

func TestValidPath(t *testing.T) {
	long := strings.Repeat(strings.Repeat("a", 254)+"/", 16)
	for p, want := range map[string]bool{
		"D1-x.md": true, "core/D?a-y.md": true, ".hidden": true, "a/b/c": true,
		"": false, "/a": false, "a/": false, "a//b": false, "./a": false, "a/..": false, "a\\b": true,
		"\xff": false, "a\x7fb": false, strings.Repeat("a", 256): false,
		long + strings.Repeat("a", 16): true, long + strings.Repeat("a", 17): false, strings.Repeat("a", 255): true,
	} {
		if got := ValidPath(p); got != want {
			t.Errorf("ValidPath(%q) = %v", p[:min(len(p), 40)], got)
		}
	}
}

// TestCompareHashesOnly: no content is kept, so no amount of it is too
// large; hashes and sizes still identify every change.
func TestCompareHashesOnly(t *testing.T) {
	root, d, m := base(t, map[string]string{"gone.md": "g"})
	if err := os.Remove(filepath.Join(root, "gone.md")); err != nil {
		t.Fatal(err)
	}
	for i := range MaxChangedBytes/MaxFileBytes + 1 {
		put(t, root, fmt.Sprintf("f%02d", i), strings.Repeat("x", MaxFileBytes))
	}
	r, err := Compare(t.Context(), d, m, Limits{HashesOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if r.TooLarge || len(r.Changes) != MaxChangedBytes/MaxFileBytes+2 {
		t.Fatalf("too large %v, %d changes", r.TooLarge, len(r.Changes))
	}
	for _, c := range r.Changes {
		if c.Content != "" || !c.Deleted && (c.SHA256 != Hash(strings.Repeat("x", MaxFileBytes)) || c.Size != MaxFileBytes) {
			t.Fatalf("change %s: %d bytes of content, size %d", c.Path, len(c.Content), c.Size)
		}
	}
}

// TestIndexFileBound: the generated index at the top may exceed the
// per-file limit up to its own bound, read, written and hashed like any
// file; an INDEX.md elsewhere, or a larger one, is a problem as any file.
func TestIndexFileBound(t *testing.T) {
	large := strings.Repeat("| D1 | dir | title |\n", (MaxFileBytes/20)+100)
	if len(large) <= MaxFileBytes || len(large) > MaxIndexBytes {
		t.Fatalf("index size %d", len(large))
	}
	root, d, m := base(t, map[string]string{IndexPath: large})
	if e := m.Lookup(IndexPath); e == nil || e.SHA256 != Hash([]byte(large)) {
		t.Fatalf("manifest entry %+v", e)
	}
	st, err := ReadState(d, IndexPath)
	if err != nil || st.SHA256 != Hash([]byte(large)) {
		t.Fatalf("state %+v %v", st, err)
	}
	put(t, root, IndexPath, large+"x")
	put(t, root, "sub/"+IndexPath, large)
	put(t, root, "over.md", strings.Repeat("x", MaxFileBytes+1))
	r := compare(t, d, m)
	if got := summary(r); got != "+INDEX.md="+large+"x !over.md !sub/INDEX.md" {
		t.Fatalf("compare: %.200s", got)
	}
	w := NewWriter(openDir(t, t.TempDir()))
	if err := w.Add(IndexPath, large); err != nil {
		t.Fatal(err)
	}
	if err := w.Add("sub/"+IndexPath, large); err == nil {
		t.Fatal("a large INDEX.md below the top was written")
	}
	if MaxBytes("x.md") != MaxFileBytes || MaxBytes(IndexPath) != MaxIndexBytes {
		t.Fatal("bounds")
	}
}
