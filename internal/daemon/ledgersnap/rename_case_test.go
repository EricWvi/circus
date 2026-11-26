package ledgersnap_test

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/daemon/ledgersnap"
	"github.com/EricWvi/circus/internal/ledger/snapshot"
)

const rebased = circusv1.RebaseLedgerResult_OUTCOME_REBASED

// TestCaseOnlyRename: a rebase that renames a file to a name differing
// only in case, as a deletion and a write, in either order and with the
// same or new content, leaves exactly the new name with its content; so
// does the same rebase sent again after its edits were applied. On a
// folding file system (macOS) both names reach one file: the write must
// not take the old file for its own target, nor the deletion the new file
// for the old one, whatever the old file's size (INDEX.md may exceed what
// any other path may hold).
func TestCaseOnlyRename(t *testing.T) {
	ledgersnap.SetFoldsNames(t, true)
	big := strings.Repeat("i", snapshot.MaxFileBytes+64<<10)
	for _, tc := range []struct {
		name, from, to, old, content string
		writeFirst, applied          bool
	}{
		{name: "same content", from: "A.md", to: "a.md", old: "a", content: "a"},
		{name: "new content", from: "A.md", to: "a.md", old: "a", content: "b"},
		{name: "same content, write first", from: "A.md", to: "a.md", old: "a", content: "a", writeFirst: true},
		{name: "new content, write first", from: "A.md", to: "a.md", old: "a", content: "b", writeFirst: true},
		{name: "same content, sent again", from: "A.md", to: "a.md", old: "a", content: "a", applied: true},
		{name: "new content, sent again", from: "A.md", to: "a.md", old: "a", content: "b", applied: true},
		{name: "from a large index", from: snapshot.IndexPath, to: "index.md", old: big, content: "short"},
		{name: "to a large index, sent again", from: "index.md", to: snapshot.IndexPath, old: "short", content: big, applied: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, ledgersnap.Options{})
			path := f.ready(base1, tc.from, tc.old, "keep.md", "k")
			edits := []*circusv1.RebaseLedgerRequest_Edit{rebaseEdit(tc.from, tc.old, nil), rebaseEdit(tc.to, "", ptr(tc.content))}
			if tc.writeFirst {
				slices.Reverse(edits)
			}
			if tc.applied {
				// An earlier attempt applied the edits; its answer was lost
				// before the manifest was written.
				if err := os.Remove(filepath.Join(path, tc.from)); err != nil {
					t.Fatal(err)
				}
				write(t, filepath.Join(path, tc.to), tc.content)
			}
			if r := f.rebase(nil, edits); r.Outcome != rebased {
				t.Fatalf("rebase: %v", r)
			}
			want := []string{tc.to, "keep.md"}
			slices.Sort(want)
			if got := entries(t, path); !slices.Equal(got, want) {
				t.Fatalf("snapshot: %v", got)
			}
			if got := read(t, filepath.Join(path, tc.to)); got != tc.content {
				t.Fatalf("%s holds %d bytes, want %d", tc.to, len(got), len(tc.content))
			}
			if c := changes(f.collect()); c != "" {
				t.Fatalf("changes after the rebase: %s", c)
			}
		})
	}
}

// Where names fold, a file the agent creates under exactly an edit's path
// while the rebase runs, with the edit's target content, counts as that
// edit applied, as everywhere else.
func TestFoldingRebaseFindsAConcurrentExactCreation(t *testing.T) {
	ledgersnap.SetFoldsNames(t, true)
	f := newFixture(t, ledgersnap.Options{})
	path := f.ready(base1, "a.md", "a")
	ledgersnap.SetBeforeEdit(f.svc, func(p string) {
		if p == "a.md" {
			write(t, filepath.Join(path, "b.md"), "b")
		}
	})
	r := f.rebase(nil, []*circusv1.RebaseLedgerRequest_Edit{rebaseEdit("a.md", "a", ptr("a2")), rebaseEdit("b.md", "", ptr("b"))})
	if r.Outcome != rebased {
		t.Fatalf("rebase: %v", r)
	}
	if got := entries(t, path); !slices.Equal(got, []string{"a.md", "b.md"}) {
		t.Fatalf("snapshot: %v", got)
	}
}

// Where names fold, an agent that renames A.md to a.md while a case-only
// rename of the same file waits keeps its rename: the deletion finds no
// A.md listed, and the write finds the agent's a.md where it expected
// nothing.
func TestFoldingRebaseKeepsAConcurrentRename(t *testing.T) {
	ledgersnap.SetFoldsNames(t, true)
	f := newFixture(t, ledgersnap.Options{})
	path := f.ready(base1, "A.md", "a", "keep.md", "k")
	ledgersnap.SetBeforeEdit(f.svc, func(p string) {
		if p == "keep.md" {
			if err := os.Rename(filepath.Join(path, "A.md"), filepath.Join(path, "a.md")); err != nil {
				t.Error(err)
			}
		}
	})
	r := f.rebase(nil, []*circusv1.RebaseLedgerRequest_Edit{
		rebaseEdit("keep.md", "k", ptr("k2")), rebaseEdit("A.md", "a", nil), rebaseEdit("a.md", "", ptr("b")),
	})
	if r.Outcome != circusv1.RebaseLedgerResult_OUTCOME_FAILED || r.Error != stale || !strings.Contains(r.ErrorDetail, "a.md changed while the rebase ran") {
		t.Fatalf("rebase: %v", r)
	}
	if got := entries(t, path); !slices.Equal(got, []string{"a.md", "keep.md"}) {
		t.Fatalf("snapshot: %v", got)
	}
	if got := read(t, filepath.Join(path, "a.md")); got != "a" {
		t.Fatalf("the agent's a.md holds %q", got)
	}
}

// Where names fold, edits are still applied in the request's order: an
// edit that finds the agent's change stops the rebase before any edit
// after it, a deletion of an unrelated file included.
func TestFoldingRebaseStopsAtTheFirstChangedFile(t *testing.T) {
	ledgersnap.SetFoldsNames(t, true)
	f := newFixture(t, ledgersnap.Options{})
	path := f.ready(base1, "a.md", "a", "z.md", "z")
	ledgersnap.SetBeforeEdit(f.svc, func(p string) {
		if p == "a.md" {
			write(t, filepath.Join(path, "a.md"), "agent")
		}
	})
	r := f.rebase(nil, []*circusv1.RebaseLedgerRequest_Edit{rebaseEdit("a.md", "a", ptr("a2")), rebaseEdit("z.md", "z", nil)})
	if r.Error != stale || !strings.Contains(r.ErrorDetail, "a.md changed while the rebase ran") {
		t.Fatalf("rebase: %v", r)
	}
	if got := read(t, filepath.Join(path, "z.md")); got != "z" {
		t.Fatalf("z.md holds %q", got)
	}
}

// Many case-only renames in one rebase, every write sent before every
// deletion, each end with its new name (TestOrderIsLinear covers the cost
// of ordering long paths; on macOS each edit's F_FULLFSYNC dominates here).
func TestManyCaseOnlyRenames(t *testing.T) {
	ledgersnap.SetFoldsNames(t, true)
	f := newFixture(t, ledgersnap.Options{})
	const pairs = 32
	dir := strings.Repeat(strings.Repeat("d", 40)+"/", 4)
	var kv []string
	var edits []*circusv1.RebaseLedgerRequest_Edit
	for i := range pairs {
		from, to := fmt.Sprintf("%sA%03d.md", dir, i), fmt.Sprintf("%sa%03d.md", dir, i)
		kv = append(kv, from, "x")
		edits = append(edits, rebaseEdit(to, "", ptr("x")), rebaseEdit(from, "x", nil))
	}
	// All writes first, then all deletions: each write moves its deletion
	// forward.
	slices.SortStableFunc(edits, func(a, b *circusv1.RebaseLedgerRequest_Edit) int {
		return boolOrder(a.GetDeleted(), b.GetDeleted())
	})
	path := f.ready(base1, kv...)
	start := time.Now()
	if r := f.rebase(nil, edits); r.Outcome != rebased {
		t.Fatalf("rebase: %v", r)
	}
	t.Logf("%d case-only renames of %d-byte paths in %s", pairs, len(dir)+7, time.Since(start))
	names := entries(t, filepath.Join(path, strings.TrimSuffix(dir, "/")))
	if len(names) != pairs || names[0] != "a000.md" || names[pairs-1] != fmt.Sprintf("a%03d.md", pairs-1) {
		t.Fatalf("the directory holds %d names, from %q to %q", len(names), names[0], names[len(names)-1])
	}
}

func boolOrder(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return 1
	}
	return -1
}

// Ordering a rebase's edits computes each fold key once: 128 case-only
// renames of long non-ASCII paths, whose fold keys are costly, are ordered
// in a fraction of a second (comparing every write with every deletion
// took seven seconds),
// each deletion right before its write and everything else in order.
func TestOrderIsLinear(t *testing.T) {
	const pairs = 128
	dir := strings.Repeat(strings.Repeat("ä", 100)+"/", 8)
	var edits []*circusv1.RebaseLedgerRequest_Edit
	for i := range pairs {
		edits = append(edits, rebaseEdit(fmt.Sprintf("%sa%03d.md", dir, i), "", ptr("x")))
	}
	for i := range pairs {
		edits = append(edits, rebaseEdit(fmt.Sprintf("%sA%03d.md", dir, i), "x", nil))
	}
	edits = append(edits, rebaseEdit("unrelated.md", "u", nil))
	start := time.Now()
	ordered := ledgersnap.OrderEdits(edits)
	t.Logf("ordered %d edits in %s", len(edits), time.Since(start))
	if len(ordered) != len(edits) {
		t.Fatalf("%d edits ordered, want %d", len(ordered), len(edits))
	}
	for i := range pairs {
		del, write := ordered[2*i], ordered[2*i+1]
		if !del.GetDeleted() || del.Path != fmt.Sprintf("%sA%03d.md", dir, i) || write.Path != fmt.Sprintf("%sa%03d.md", dir, i) {
			t.Fatalf("edits %d and %d: %s, %s", 2*i, 2*i+1, del.Path[len(dir):], write.Path[len(dir):])
		}
	}
	if last := ordered[len(ordered)-1]; last.Path != "unrelated.md" {
		t.Fatalf("last edit %s, want the unrelated deletion where the request put it", last.Path)
	}
}
