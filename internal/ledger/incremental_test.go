package ledger

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/store/sqlite"
	"github.com/go-git/go-git/v5/plumbing"
)

// Each commit reuses the parent's subtrees, blobs and index rows. After
// random adds, moves, retitles, edits and unchanged commits, with some
// index updates failing, every commit's tree must equal one built from
// scratch, and the index and history must equal a full rebuild.
func TestIncrementalCommitsMatchFullBuilds(t *testing.T) {
	m, db := openLedger(t, nil, "project")
	ctx := t.Context()
	rng := rand.New(rand.NewPCG(1, 2))
	// Nested and sibling directories whose names sort around "/".
	dirs := []string{"", "a", "a/b", "a/b/c", "b", "c", "a-b", "a.b", "ab"}
	var ds []Decision
	steps := 300
	if raceEnabled {
		steps = 100
	}
	for step := range steps {
		switch op := rng.IntN(5); {
		case op == 0 || len(ds) == 0:
			ds = append(ds, Decision{Number: uint32(len(ds)), Dir: dirs[rng.IntN(len(dirs))], Title: fmt.Sprintf("t%d", rng.IntN(5)), Body: []byte("b")})
		case op == 1:
			ds[rng.IntN(len(ds))].Dir = dirs[rng.IntN(len(dirs))]
		case op == 2:
			ds[rng.IntN(len(ds))].Title = fmt.Sprintf("t%d", rng.IntN(5))
		case op == 3:
			ds[rng.IntN(len(ds))].Body = fmt.Appendf(nil, "%d", rng.IntN(4))
		}
		// A failed index update leaves the next commit a stale index, which
		// that commit rebuilds instead of advancing.
		failing := rng.IntN(8) == 0
		if failing {
			m.st = failAdvance{LedgerOps: db, err: store.ErrBusy}
		}
		head, err := m.CommitMain(ctx, "project", ds, CommitOptions{Author: "a", Message: fmt.Sprint("step ", step)})
		m.st = db
		if head == "" || err != nil && (!failing || !errors.Is(err, store.ErrBusy)) {
			t.Fatalf("step %d: commit = %q, %v", step, head, err)
		}
		r, err := openRepository(m.repoPath("project"))
		check(t, err)
		c, err := r.commit(plumbing.NewHash(head))
		check(t, err)
		prepared, _, err := prepareSnapshot(ctx, ds)
		check(t, err)
		full, _, err := r.putSnapshot(ctx, prepared, snapshot{})
		check(t, r.storage.Close())
		check(t, err)
		if c.TreeHash != full {
			t.Fatalf("step %d: incremental tree %s, full tree %s", step, c.TreeHash, full)
		}
		if step%100 == 99 {
			checkIndexMatchesRebuild(t, m, db, "project", ds)
		}
	}
}

func checkIndexMatchesRebuild(t *testing.T, m *Manager, db *sqlite.DB, projectID string, ds []Decision) {
	t.Helper()
	ctx := t.Context()
	// Commit once more so that the index is current before comparing.
	if _, err := m.CommitMain(ctx, projectID, ds, CommitOptions{Author: "a", Message: "compare"}); err != nil {
		t.Fatal(err)
	}
	incremental, history := ledgerState(t, db, projectID, ds)
	check(t, db.ReplaceLedgerIndex(ctx, projectID, incremental.Head, "", false, nil, nil))
	check(t, m.Reconcile(ctx))
	rebuilt, rebuiltHistory := ledgerState(t, db, projectID, ds)
	if fmt.Sprint(incremental) != fmt.Sprint(rebuilt) {
		t.Fatalf("incremental index differs from a rebuild:\n%+v\n%+v", incremental, rebuilt)
	}
	for number, versions := range history {
		if !slices.Equal(versions, rebuiltHistory[number]) {
			t.Fatalf("D%d: incremental history differs from a rebuild:\n%+v\n%+v", number, versions, rebuiltHistory[number])
		}
	}
}

// ledgerState reads the index and every decision's history. Positions are
// cleared: appended versions count index updates, a rebuild counts commits.
func ledgerState(t *testing.T, db *sqlite.DB, projectID string, ds []Decision) (store.LedgerIndex, map[uint32][]store.LedgerVersion) {
	t.Helper()
	index, err := db.GetLedgerIndex(t.Context(), projectID)
	check(t, err)
	history := make(map[uint32][]store.LedgerVersion, len(ds))
	for _, d := range ds {
		var all []store.LedgerVersion
		for after, more := "", true; more; {
			page, next, err := db.ListLedgerHistory(t.Context(), projectID, d.Number, after, store.MaxLedgerHistoryPage)
			check(t, err)
			for _, v := range page {
				after = v.Commit
				v.Position = 0
				all = append(all, v)
			}
			more = next
		}
		history[d.Number] = all
	}
	return index, history
}

// moveMainOnRead stands for a writer that ignores main.lock: it moves main
// right after the startup check has read the indexed head.
type moveMainOnRead struct {
	store.LedgerOps
	ref  string
	head plumbing.Hash
	done bool
}

func (s *moveMainOnRead) GetLedgerHead(ctx context.Context, id string) (string, error) {
	head, err := s.LedgerOps.GetLedgerHead(ctx, id)
	if !s.done {
		s.done = true
		err = errors.Join(err, os.WriteFile(s.ref, []byte(s.head.String()+"\n"), 0o600))
	}
	return head, err
}

// A rebuild must not install an index for a head that main has left.
func TestRebuildRechecksMain(t *testing.T) {
	m, db := openLedger(t, nil, "project")
	ctx := t.Context()
	d := Decision{Title: "first"}
	opts := CommitOptions{Author: "import", Message: "Seed"}
	first := commit(t, m, "project", []Decision{d}, opts)
	d.Title = "second"
	m.st = failAdvance{LedgerOps: db, err: store.ErrBusy}
	second, err := m.CommitMain(ctx, "project", []Decision{d}, opts)
	if second == "" || !errors.Is(err, store.ErrBusy) {
		t.Fatalf("stale index: %q, %v", second, err)
	}
	r, err := openRepository(m.repoPath("project"))
	check(t, err)
	defer r.storage.Close()
	third := gitCommit(t, r, plumbing.NewHash(second), mustTree(t, r, second))
	m.st = &moveMainOnRead{LedgerOps: db, ref: filepath.Join(r.dir, mainRef.String()), head: third}
	check(t, m.Reconcile(ctx))
	if indexed, err := db.GetLedgerHead(ctx, "project"); err != nil || indexed != first {
		t.Fatalf("indexed head after main moved during the rebuild = %s, %v; want %s", indexed, err, first)
	}
	m.st = db
	check(t, m.Reconcile(ctx))
	if indexed, err := db.GetLedgerHead(ctx, "project"); err != nil || indexed != third.String() {
		t.Fatalf("indexed head after the next check = %s, %v; want %s", indexed, err, third)
	}
}

func mustTree(t *testing.T, r *repository, commit string) plumbing.Hash {
	t.Helper()
	c, err := r.commit(plumbing.NewHash(commit))
	check(t, err)
	return c.TreeHash
}
