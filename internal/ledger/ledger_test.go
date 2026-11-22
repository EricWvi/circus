package ledger

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/store/sqlite"
	"github.com/EricWvi/circus/internal/store/sqlite/sqlitetest"
	"github.com/EricWvi/circus/internal/testutil/wait"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

var baseTime = time.Date(2026, 11, 20, 1, 2, 3, 0, time.UTC)

func openLedger(t *testing.T, now func() time.Time, projects ...string) (*Manager, *sqlite.DB) {
	t.Helper()
	path := sqlitetest.Path(t)
	db, err := sqlite.Open(t.Context(), path, sqlite.Options{})
	check(t, err)
	t.Cleanup(func() { check(t, db.Close()) })
	at := baseTime.UnixMilli()
	check(t, db.CreateUser(t.Context(), store.User{ID: "owner", Name: "Owner", CreatedAt: at}))
	for _, id := range projects {
		_, err := db.CreateProject(t.Context(), store.Project{ID: id, UserID: "owner", Name: id, CreatedAt: at, UpdatedAt: at})
		check(t, err)
	}
	manager, err := New(path, db, now, nil)
	check(t, err)
	t.Cleanup(func() { check(t, manager.Close(context.Background())) })
	return manager, db
}

func check(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func commit(t *testing.T, m *Manager, projectID string, decisions []Decision, options CommitOptions) string {
	t.Helper()
	head, err := m.CommitMain(t.Context(), projectID, decisions, options)
	check(t, err)
	if !store.IsLedgerCommitID(head) {
		t.Fatalf("invalid commit ID %q", head)
	}
	return head
}

func TestCommitMainAndRead(t *testing.T) {
	ctx := t.Context()
	now := baseTime
	m, db := openLedger(t, func() time.Time { return now }, "project")
	info, err := os.Stat(m.root)
	check(t, err)
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("ledger root mode = %s", info.Mode())
	}
	if _, err := m.GetDecision(ctx, "project", 0, ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("empty ledger read = %v", err)
	}
	if _, err := os.Stat(m.repoPath("project")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("uninitialized repository stat = %v", err)
	}

	initial := Decision{Title: "First", Body: []byte("first\r\n正文\n")}
	other := Decision{Number: 5, Dir: "nested/sub", Title: "Other"}
	options := CommitOptions{Author: "import", Message: "Seed\n\nDetails", CodeCommit: strings.Repeat("a", 40)}
	h1 := commit(t, m, "project", []Decision{initial}, options)
	edited := initial
	edited.Body = []byte("changed\r\n正文\n")
	now = now.Add(time.Second)
	h2 := commit(t, m, "project", []Decision{other, edited}, CommitOptions{Author: "worker", Message: "Edit"})
	edited.Dir = "nested"
	now = now.Add(time.Second)
	h3 := commit(t, m, "project", []Decision{edited, other}, CommitOptions{Author: "worker", Message: "Move"})
	edited.Title = "Retitled: yes"
	now = now.Add(time.Second)
	h4 := commit(t, m, "project", []Decision{edited, other}, CommitOptions{Author: "worker", Message: "Retitle"})
	now = now.Add(time.Second)
	h5 := commit(t, m, "project", []Decision{edited, other}, CommitOptions{Author: "worker", Message: "No change"})

	old, err := m.GetDecision(ctx, "project", 0, h1)
	check(t, err)
	if !bytes.Equal(old.Body, initial.Body) || old.Title != initial.Title || old.Commit != h1 || old.CommittedAt != baseTime.UnixMilli() {
		t.Fatalf("initial decision = %+v", old)
	}
	got, err := m.GetDecision(ctx, "project", 0, "")
	check(t, err)
	if !bytes.Equal(got.Body, edited.Body) || got.Path != decisionPath(edited) || got.Title != edited.Title || got.Commit != h5 ||
		got.CommittedAt != now.UnixMilli() {
		t.Fatalf("current decision = %+v", got)
	}
	if _, err := m.GetDecision(ctx, "project", other.Number, h1); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("decision absent at root = %v", err)
	}
	index, err := db.GetLedgerIndex(ctx, "project")
	check(t, err)
	if index.Head != h5 || index.NextNumber != 6 || len(index.Entries) != 2 || index.Entries[0].Commit != h4 ||
		index.Entries[0].CommittedAt != now.Add(-time.Second).UnixMilli() || index.Entries[1].Commit != h2 {
		t.Fatalf("index = %+v", index)
	}
	history, more, err := m.ListDecisionHistory(ctx, "project", 0, "", 10)
	check(t, err)
	if more || len(history) != 4 {
		t.Fatalf("history length = %d, more %v", len(history), more)
	}
	for i, want := range []struct {
		commit string
		flags  [4]bool
	}{
		{h4, [4]bool{false, false, false, true}},
		{h3, [4]bool{false, false, true, false}},
		{h2, [4]bool{false, true, false, false}},
		{h1, [4]bool{true, false, false, false}},
	} {
		v := history[i]
		if v.Commit != want.commit || [4]bool{v.Added, v.ContentChanged, v.Moved, v.Retitled} != want.flags {
			t.Errorf("history at %d = %+v", i, v)
		}
	}
	last := history[len(history)-1]
	if last.Author != options.Author || last.Summary != "Seed" || last.CodeCommit != options.CodeCommit {
		t.Fatalf("initial version metadata = %+v", last)
	}
	if _, err := m.CommitMain(ctx, "project", []Decision{other}, CommitOptions{Author: "worker", Message: "Attempt delete"}); err == nil {
		t.Fatal("deleted an existing decision")
	} else {
		var invalid *ValidationError
		if !errors.As(err, &invalid) {
			t.Fatal(err)
		}
		expectProblem(t, invalid.Problems, decisionPath(edited), "cannot be removed")
	}

	r, err := openRepository(m.repoPath("project"))
	check(t, err)
	defer r.storage.Close()
	treeCommit, err := r.commit(plumbing.NewHash(h5))
	check(t, err)
	var files []string
	check(t, r.walkFiles(ctx, treeCommit.TreeHash, "", func(f fileObject) error {
		files = append(files, f.path)
		return nil
	}))
	slices.Sort(files)
	if !slices.Equal(files, []string{indexName, decisionPath(edited), decisionPath(other)}) {
		t.Fatalf("tree contains %v", files)
	}
}

func TestMainFirstParent(t *testing.T) {
	m, db := openLedger(t, nil, "project")
	d := Decision{Title: "Root"}
	opts := CommitOptions{Author: "import", Message: "Root"}
	first := commit(t, m, "project", []Decision{d}, opts)
	d.Body = []byte("main")
	main := commit(t, m, "project", []Decision{d}, opts)

	r, err := openRepository(m.repoPath("project"))
	check(t, err)
	defer r.storage.Close()
	ctx := t.Context()
	side := Decision{Title: "Side"}
	prepared, _, err := prepareSnapshot(ctx, []Decision{side})
	check(t, err)
	tree, _, err := r.putSnapshot(ctx, prepared, snapshot{})
	check(t, err)
	parent, err := r.commit(plumbing.NewHash(main))
	check(t, err)
	sig := object.Signature{Name: "other", Email: "other@circus.invalid", When: baseTime}
	writeCommit := func(tree plumbing.Hash, parents ...plumbing.Hash) plumbing.Hash {
		t.Helper()
		o := r.storage.NewEncodedObject()
		check(t, (&object.Commit{Author: sig, Committer: sig, Message: "Merge\n", TreeHash: tree, ParentHashes: parents}).Encode(o))
		h, err := r.putObject(o)
		check(t, err)
		return h
	}
	sideID := writeCommit(tree, plumbing.NewHash(first))
	ref := plumbing.ReferenceName("refs/heads/side")
	check(t, r.storage.SetReference(plumbing.NewHashReference(ref, sideID)))
	merge := writeCommit(parent.TreeHash, parent.Hash, sideID)
	_, err = r.setMain(parent.Hash, merge)
	check(t, err)
	check(t, m.Reconcile(ctx))
	if _, err := m.GetDecision(ctx, "project", 0, sideID.String()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second-parent commit was accepted: %v", err)
	}
	got, err := m.GetDecision(ctx, "project", 0, merge.String())
	check(t, err)
	if string(got.Body) != "main" || got.Commit != merge.String() {
		t.Fatalf("merge snapshot = %+v", got)
	}
	versions, _, err := db.ListLedgerHistory(ctx, "project", 0, "", 10)
	check(t, err)
	if len(versions) != 2 || versions[0].Commit != main || versions[1].Commit != first {
		t.Fatalf("history includes non-changing merge or side branch: %+v", versions)
	}
	d.Body = []byte("new main")
	commit(t, m, "project", []Decision{d}, opts)
	otherRef, err := r.Reference(ref, false)
	check(t, err)
	if otherRef.Hash() != sideID {
		t.Fatalf("side branch moved to %s", otherRef.Hash())
	}
}

type failAdvance struct {
	store.LedgerOps
	err error
}

func (f failAdvance) AdvanceLedgerIndex(context.Context, string, string, string, []store.LedgerEntry, []store.LedgerVersion) error {
	return f.err
}

func TestReconcileRepairsAndCleans(t *testing.T) {
	m, db := openLedger(t, nil, "stale", "missing", "orphan")
	ctx := t.Context()
	d := Decision{Title: "Original"}
	options := CommitOptions{Author: "import", Message: "Seed"}
	root := commit(t, m, "stale", []Decision{d}, options)
	commit(t, m, "missing", []Decision{d}, options)
	commit(t, m, "orphan", []Decision{d}, options)
	check(t, db.DeleteProject(ctx, "owner", "orphan", baseTime.UnixMilli()))
	check(t, m.Remove(ctx, "missing"))
	trash := filepath.Join(m.root, trashName, "abandoned")
	check(t, os.MkdirAll(trash, 0o700))
	check(t, os.WriteFile(filepath.Join(trash, "sentinel"), []byte("leftover"), 0o600))
	stage, err := os.MkdirTemp(m.root, initPrefix)
	check(t, err)

	failure := errors.New("stopped after moving Git main")
	m.st = failAdvance{LedgerOps: db, err: failure}
	d.Title = "Changed"
	tip, err := m.CommitMain(ctx, "stale", []Decision{d}, options)
	if tip == "" || !errors.Is(err, failure) {
		t.Fatalf("partial commit = %q, %v", tip, err)
	}
	head, err := db.GetLedgerHead(ctx, "stale")
	check(t, err)
	if head != root {
		t.Fatalf("indexed head advanced to %s", head)
	}

	var logs bytes.Buffer
	path := filepath.Join(filepath.Dir(m.root), "circus.db")
	restarted, err := New(path, db, nil, slog.New(slog.NewTextHandler(&logs, nil)))
	check(t, err)
	check(t, restarted.Reconcile(ctx))
	index, err := db.GetLedgerIndex(ctx, "stale")
	check(t, err)
	if index.Head != tip || index.NextNumber != 1 || len(index.Entries) != 1 || index.Entries[0].Title != d.Title {
		t.Fatalf("repaired index = %+v", index)
	}
	history, _, err := restarted.ListDecisionHistory(ctx, "stale", 0, "", 10)
	check(t, err)
	if len(history) != 2 || history[0].Commit != tip || !history[0].Retitled || !history[1].Added {
		t.Fatalf("repaired history = %+v", history)
	}
	index, err = db.GetLedgerIndex(ctx, "missing")
	check(t, err)
	if index.Head != "" || index.NextNumber != 1 || len(index.Entries) != 0 {
		t.Fatalf("cleared index = %+v", index)
	}
	for _, path := range []string{restarted.repoPath("orphan"), filepath.Dir(trash), stage} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s remains: %v", path, err)
		}
	}
	for _, message := range []string{"rebuilt ledger index", "cleared index", "removed orphan", "cleared ledger trash"} {
		if !strings.Contains(logs.String(), message) {
			t.Errorf("missing log %q in %q", message, &logs)
		}
	}
	check(t, restarted.Reconcile(ctx))
}

func TestConcurrentCommitMainSameProject(t *testing.T) {
	m, db := openLedger(t, nil, "project")
	const writes = 6
	ctx, cancel := context.WithTimeout(t.Context(), wait.Timeout)
	defer cancel()
	run := make(chan struct{})
	errs := make(chan error, writes)
	var group sync.WaitGroup
	for i := range writes {
		group.Go(func() {
			<-run
			_, err := m.CommitMain(ctx, "project", []Decision{{Title: "Concurrent", Body: fmt.Append(nil, i)}},
				CommitOptions{Author: "worker", Message: "Concurrent update"})
			errs <- err
		})
	}
	close(run)
	group.Wait()
	close(errs)
	for err := range errs {
		check(t, err)
	}
	index, err := db.GetLedgerIndex(ctx, "project")
	check(t, err)
	history, _, err := m.ListDecisionHistory(ctx, "project", 0, "", writes)
	check(t, err)
	if len(history) != writes || history[0].Commit != index.Head || !history[writes-1].Added {
		t.Fatalf("lost or unordered commits: index %+v, history %+v", index, history)
	}
	for i, v := range history {
		if v.Position != int64(writes-i-1) || v.ContentChanged != (i < writes-1) {
			t.Errorf("unexpected history version %+v", v)
		}
	}
}

func TestConcurrentCommitMainDifferentProjects(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), wait.Timeout)
	defer cancel()
	atCommit := make(chan struct{}, 2)
	release := make(chan struct{})
	m, db := openLedger(t, func() time.Time {
		atCommit <- struct{}{}
		select {
		case <-ctx.Done():
		case <-release:
		}
		return baseTime
	}, "left", "right")
	errs := make(chan error, 2)
	var group sync.WaitGroup
	for _, id := range []string{"left", "right"} {
		group.Go(func() {
			_, err := m.CommitMain(ctx, id, []Decision{{Title: id}}, CommitOptions{Author: "worker", Message: "Add"})
			errs <- err
		})
	}
	for range 2 {
		select {
		case <-ctx.Done():
			close(release)
			group.Wait()
			t.Fatal("commits on separate projects could not proceed together")
		case <-atCommit:
		}
	}
	close(release)
	group.Wait()
	close(errs)
	for err := range errs {
		check(t, err)
	}
	for _, id := range []string{"left", "right"} {
		index, err := db.GetLedgerIndex(ctx, id)
		check(t, err)
		if len(index.Entries) != 1 || index.Entries[0].Title != id {
			t.Errorf("%q index = %+v", id, index)
		}
	}
}
