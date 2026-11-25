package snapshot

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"golang.org/x/sys/unix"
)

// applier returns an Applier on a snapshot of files and its staging
// directory's path.
func applier(t *testing.T, files map[string]string) (*Applier, string, string) {
	t.Helper()
	root, d, _ := base(t, files)
	staging := t.TempDir()
	return NewApplier(d, openDir(t, staging), "id.tmp", "id.aside"), root, staging
}

func names(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func content(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return "<" + err.Error() + ">"
	}
	return string(data)
}

func applierReplace(t *testing.T) {
	a, root, staging := applier(t, map[string]string{"a.md": "a", "b.md": "b", "c.md": "c"})
	for _, tc := range []struct {
		path, expected, content string
		err                     error
		want                    string
	}{
		{"new/dir/n.md", "", "n", nil, "n"},
		{"a.md", Hash("a"), "a2", nil, "a2"},
		// The file holds something else: put back as it was.
		{"b.md", Hash("other"), "b2", ErrChanged, "b"},
		// A file exists where none was expected: left alone.
		{"c.md", "", "c2", ErrChanged, "c"},
		// A file expected but gone: its deletion stays.
		{"gone.md", Hash("g"), "g2", ErrChanged, "<open " + filepath.Join(root, "gone.md") + ": no such file or directory>"},
	} {
		if err := a.Replace(tc.path, tc.expected, tc.content); !errors.Is(err, tc.err) || err != nil && tc.err == nil {
			t.Errorf("Replace(%s) = %v, want %v", tc.path, err, tc.err)
		}
		if got := content(t, filepath.Join(root, tc.path)); got != tc.want {
			t.Errorf("%s holds %q, want %q", tc.path, got, tc.want)
		}
		if got := names(t, staging); len(got) != 0 {
			t.Fatalf("staging after %s: %v", tc.path, got)
		}
	}
	if err := a.Sync(); err != nil {
		t.Fatal(err)
	}
}

func applierNeverFollows(t *testing.T) {
	a, root, staging := applier(t, map[string]string{"a.md": "a"})
	outside := t.TempDir()
	put(t, outside, "target.md", "outside")
	if err := os.Symlink(filepath.Join(outside, "target.md"), filepath.Join(root, "link.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "dirlink")); err != nil {
		t.Fatal(err)
	}
	// A symlink where a file was expected is moved aside, found not to be
	// it and put back.
	if err := a.Replace("link.md", Hash("outside"), "x"); !errors.Is(err, ErrChanged) {
		t.Fatalf("Replace over a symlink: %v", err)
	}
	if target, err := os.Readlink(filepath.Join(root, "link.md")); err != nil || target != filepath.Join(outside, "target.md") {
		t.Fatalf("symlink after Replace: %q %v", target, err)
	}
	if err := a.Delete("link.md", Hash("outside")); !errors.Is(err, ErrChanged) {
		t.Fatalf("Delete of a symlink: %v", err)
	}
	if err := a.Replace("dirlink/target.md", Hash("outside"), "x"); err == nil {
		t.Fatal("wrote through a symlinked directory")
	}
	if err := a.Delete("dirlink/target.md", Hash("outside")); err == nil {
		t.Fatal("removed through a symlinked directory")
	}
	if content(t, filepath.Join(outside, "target.md")) != "outside" || len(names(t, staging)) != 0 {
		t.Fatal("the outside file or staging changed")
	}
}

func applierDelete(t *testing.T) {
	a, root, staging := applier(t, map[string]string{"a.md": "a", "b.md": "b"})
	if err := a.Delete("a.md", Hash("a")); err != nil {
		t.Fatal(err)
	}
	if err := a.Delete("b.md", Hash("other")); !errors.Is(err, ErrChanged) {
		t.Fatalf("Delete of a changed file: %v", err)
	}
	if err := a.Delete("none.md", Hash("x")); err != nil {
		t.Fatalf("Delete of nothing: %v", err)
	}
	if got := names(t, root); !slices.Equal(got, []string{"b.md"}) || content(t, filepath.Join(root, "b.md")) != "b" {
		t.Fatalf("snapshot: %v", got)
	}
	if got := names(t, staging); len(got) != 0 {
		t.Fatalf("staging: %v", got)
	}
}

// journal writes what a killed Applier leaves for p: the aside holding
// content and the journal naming p and the expected content.
func journal(t *testing.T, staging, p, content, expected string) {
	t.Helper()
	put(t, staging, "id.aside", content)
	put(t, staging, "id.aside.path", `{"path":"`+p+`","expected":"`+Hash(expected)+`"}`)
}

// applierRecover: what a killed Applier left aside goes back when its path
// is free; when the path is taken, expected content is dropped and
// anything else is kept beside it under a conflict name.
func applierRecover(t *testing.T) {
	a, root, staging := applier(t, map[string]string{"taken.md": "newer", "both.md": "newer"})
	if recovered, err := a.Recover(); recovered || err != nil {
		t.Fatalf("nothing to recover: %v %v", recovered, err)
	}
	conflict := func(name, content string) string { return conflictName(name, Hash(content)[:12]) }
	for _, tc := range []struct {
		path, content, expected string
		want                    map[string]string
	}{
		{"empty.md", "agent's write", "collected", map[string]string{"empty.md": "agent's write"}},
		{"taken.md", "collected", "collected", map[string]string{"taken.md": "newer"}},
		{"both.md", "agent's write", "collected", map[string]string{"both.md": "newer", conflict("both.md", "agent's write"): "agent's write"}},
	} {
		journal(t, staging, tc.path, tc.content, tc.expected)
		if recovered, err := a.Recover(); !recovered || err != nil {
			t.Fatalf("recover %s: %v %v", tc.path, recovered, err)
		}
		for p, want := range tc.want {
			if got := content(t, filepath.Join(root, p)); got != want {
				t.Fatalf("%s holds %q, want %q", p, got, want)
			}
		}
		if got := names(t, staging); len(got) != 0 {
			t.Fatalf("staging after recovering %s: %v", tc.path, got)
		}
	}
	// A journal without a file aside: killed before the move.
	put(t, staging, "id.aside.path", `{"path":"x.md","expected":"`+Hash("x")+`"}`)
	if recovered, err := a.Recover(); !recovered || err != nil || len(names(t, staging)) != 0 {
		t.Fatalf("journal only: %v %v %v", recovered, err, names(t, staging))
	}
}

func renameNoReplaceCase(t *testing.T) {
	dir := t.TempDir()
	put(t, dir, "from", "from")
	put(t, dir, "to", "to")
	d := openDir(t, dir)
	fd := int(d.Fd())
	if err := renameNoReplace(fd, "from", fd, "to"); !errors.Is(err, unix.EEXIST) {
		t.Fatalf("onto an existing file: %v", err)
	}
	if content(t, filepath.Join(dir, "from")) != "from" || content(t, filepath.Join(dir, "to")) != "to" {
		t.Fatal("a refused rename changed something")
	}
	if err := renameNoReplace(fd, "from", fd, "new"); err != nil {
		t.Fatal(err)
	}
	if got := names(t, dir); !slices.Equal(got, []string{"new", "to"}) {
		t.Fatalf("after the rename: %v", got)
	}
}

// renameModes runs fn with the exclusive rename, with the hard-link
// fallback, and with the check-and-rename fallback.
func renameModes(t *testing.T, fn func(t *testing.T)) {
	unsupported := func(int, string, int, string) error { return errUnsupported }
	noLinks := func(int, string, int, string, int) error { return unix.EPERM }
	for _, mode := range []struct {
		name      string
		exclusive func(int, string, int, string) error
		link      func(int, string, int, string, int) error
	}{
		{"exclusive rename", renameExclusive, unix.Linkat},
		{"hard link", unsupported, unix.Linkat},
		{"check and rename", unsupported, noLinks},
	} {
		t.Run(mode.name, func(t *testing.T) {
			exclusiveRename, hardLink = mode.exclusive, mode.link
			defer func() { exclusiveRename, hardLink = renameExclusive, unix.Linkat }()
			fn(t)
		})
	}
}

func TestApplier(t *testing.T) {
	for name, fn := range map[string]func(*testing.T){
		"replace": applierReplace, "never follows": applierNeverFollows, "delete": applierDelete,
		"recover": applierRecover, "rename without replacing": renameNoReplaceCase,
		"killed after moving aside": applierKilledAfterMove, "recovery without recreating": applierRecoverPaths,
	} {
		t.Run(name, func(t *testing.T) { renameModes(t, fn) })
	}
}

// killAfterMove makes the next move aside fail as a kill right after it
// would: the aside file and the journal the Applier wrote stay.
func killAfterMove(t *testing.T) {
	afterMoveAside = func() error { return errors.New("killed") }
	t.Cleanup(func() { afterMoveAside = nil })
}

// applierKilledAfterMove: a process killed right after moving a file aside
// (in a Replace or a Delete) leaves a journal from which Recover puts the
// file back; until then no other file is moved aside over it.
func applierKilledAfterMove(t *testing.T) {
	for _, op := range []string{"replace", "delete"} {
		t.Run(op, func(t *testing.T) {
			a, root, staging := applier(t, map[string]string{"a.md": "a", "b.md": "b"})
			killAfterMove(t)
			var err error
			if op == "replace" {
				err = a.Replace("a.md", Hash("a"), "a2")
			} else {
				err = a.Delete("a.md", Hash("a"))
			}
			if err == nil || errors.Is(err, ErrChanged) {
				t.Fatalf("%s: %v", op, err)
			}
			afterMoveAside = nil
			if got := names(t, root); !slices.Equal(got, []string{"b.md"}) {
				t.Fatalf("a.md was not moved aside: %v", got)
			}
			if err := a.Delete("b.md", Hash("b")); err == nil || content(t, filepath.Join(root, "b.md")) != "b" {
				t.Fatalf("a second move aside before recovery: %v", err)
			}
			if recovered, err := a.Recover(); !recovered || err != nil {
				t.Fatalf("recover: %v %v", recovered, err)
			}
			if content(t, filepath.Join(root, "a.md")) != "a" || len(names(t, staging)) != 0 {
				t.Fatalf("after recovery: %q, staging %v", content(t, filepath.Join(root, "a.md")), names(t, staging))
			}
		})
	}
}

// applierRecoverPaths: recovery never recreates a directory the writer
// removed or replaced; content nobody collected goes under a conflict
// name in the nearest directory still there, expected content is dropped;
// a journal that cannot be parsed restores the aside at the top.
func applierRecoverPaths(t *testing.T) {
	for _, tc := range []struct {
		name, content string
		writer        func(t *testing.T, root string)
		want          map[string]string
		gone          []string
	}{
		{"directory removed, uncollected", "agent's write", func(t *testing.T, root string) { mustRemoveAll(t, filepath.Join(root, "sub")) },
			map[string]string{conflictName("a.md", Hash("agent's write")[:12]): "agent's write"}, []string{"sub"}},
		{"directory removed, collected", "a", func(t *testing.T, root string) { mustRemoveAll(t, filepath.Join(root, "sub")) },
			nil, []string{"sub"}},
		{"directory replaced by a file", "agent's write", func(t *testing.T, root string) {
			mustRemoveAll(t, filepath.Join(root, "sub"))
			put(t, root, "sub", "a file now")
		}, map[string]string{"sub": "a file now", conflictName("a.md", Hash("agent's write")[:12]): "agent's write"}, nil},
		{"path is a directory now", "agent's write", func(t *testing.T, root string) {
			if err := os.Mkdir(filepath.Join(root, "sub", "a.md"), 0o755); err != nil {
				t.Fatal(err)
			}
		}, map[string]string{"sub/" + conflictName("a.md", Hash("agent's write")[:12]): "agent's write"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, root, staging := applier(t, map[string]string{"sub/a.md": tc.content, "sub/b.md": "b"})
			killAfterMove(t)
			// The edit expects "a"; the file holds tc.content.
			if err := a.Delete("sub/a.md", Hash("a")); err == nil {
				t.Fatal("the kill was not injected")
			}
			afterMoveAside = nil
			tc.writer(t, root)
			if recovered, err := a.Recover(); !recovered || err != nil {
				t.Fatalf("recover: %v %v", recovered, err)
			}
			for p, want := range tc.want {
				if got := content(t, filepath.Join(root, p)); got != want {
					t.Errorf("%s holds %q, want %q", p, got, want)
				}
			}
			for _, p := range tc.gone {
				if _, err := os.Lstat(filepath.Join(root, p)); !os.IsNotExist(err) {
					t.Errorf("%s was recreated", p)
				}
			}
			if got := names(t, staging); len(got) != 0 {
				t.Fatalf("staging: %v", got)
			}
		})
	}
	a, root, staging := applier(t, nil)
	put(t, staging, "id.aside", "kept")
	put(t, staging, "id.aside.path", "not a journal")
	if recovered, err := a.Recover(); !recovered || err != nil {
		t.Fatalf("malformed journal: %v %v", recovered, err)
	}
	if got := content(t, filepath.Join(root, conflictName("recovered", Hash("kept")[:12]))); got != "kept" || len(names(t, staging)) != 0 {
		t.Fatalf("malformed journal: %q, staging %v", got, names(t, staging))
	}
}

func mustRemoveAll(t *testing.T, p string) {
	t.Helper()
	if err := os.RemoveAll(p); err != nil {
		t.Fatal(err)
	}
}

// TestWriterClockFallthrough: a clock whose probe cannot be made is
// skipped for the next, and without any the newest file's time is used.
func TestWriterClockFallthrough(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into read-only directories")
	}
	readOnly := t.TempDir()
	if err := os.Chmod(readOnly, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(readOnly, 0o755) })
	good := testClock(t)
	d := openDir(t, t.TempDir())
	m, err := Write(d, []Clock{{Dir: openDir(t, readOnly)}, good}, "", []File{{"a.md", "a"}})
	if err != nil || m.WrittenAtNs <= m.Files[0].MtimeNs {
		t.Fatalf("fall through to the second clock: %+v %v", m, err)
	}
	m, err = Write(d, []Clock{{Dir: openDir(t, readOnly)}}, "", []File{{"b.md", "b"}})
	if err != nil || m.WrittenAtNs != m.Lookup("b.md").MtimeNs {
		t.Fatalf("fall back to the newest file: %+v %v", m, err)
	}
	if len(names(t, readOnly)) != 0 || len(names(t, good.Dir.Name())) != 0 {
		t.Fatal("a probe was left")
	}
}
