package storetest

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/EricWvi/circus/internal/store"
)

func newChangeset(id, project string, at int64) store.LedgerChangeset {
	return store.LedgerChangeset{ID: id, ProjectID: project, State: store.LedgerChangesetOpen, OpenerWorkerID: "w1",
		CreatedAt: at, UpdatedAt: at, Archived: true}
}

// testLedgerChangesets: a changeset moves from open to merged, through a
// merge in progress, or to discarded; every transition checks the state it
// leaves, and pages run newest first.
func testLedgerChangesets(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	seed(t, s)
	const project = "session-project"
	base, merge, parent, code := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40), strings.Repeat("d", 40)

	c := newChangeset("cs1", project, t0)
	c.BaseCommit = base
	must(t, s.CreateLedgerChangeset(ctx, c))
	mustIs(t, s.CreateLedgerChangeset(ctx, c), store.ErrExists)
	mustIs(t, s.CreateLedgerChangeset(ctx, newChangeset("cs-missing", "missing", t0)), store.ErrNotFound)
	bad := newChangeset("cs-bad", project, t0)
	bad.State = store.LedgerChangesetMerged
	mustIs(t, s.CreateLedgerChangeset(ctx, bad), store.ErrInvalid)
	bad = newChangeset("cs-bad", project, t0)
	bad.BaseCommit = "abc"
	mustIs(t, s.CreateLedgerChangeset(ctx, bad), store.ErrInvalid)
	got, err := s.GetLedgerChangeset(ctx, "cs1")
	must(t, err)
	mustEqual(t, "created changeset", got, c)
	_, err = s.GetLedgerChangeset(ctx, "missing")
	mustIs(t, err, store.ErrNotFound)

	ok, err := s.TouchLedgerChangeset(ctx, "cs1", t0+5)
	must(t, err)
	mustEqual(t, "touch open", ok, true)
	_, err = s.TouchLedgerChangeset(ctx, "missing", t0+5)
	mustIs(t, err, store.ErrNotFound)

	// A merge in progress is recorded once, then finished or cleared by
	// its commit only.
	m := store.LedgerMerge{Commit: merge, Parent: parent, CodeCommit: code, Numbers: map[string]uint32{"a": 12, "zz": 13}}
	mustIs(t, func() error {
		_, err := s.PrepareLedgerMerge(ctx, "cs1", store.LedgerMerge{Commit: merge, Numbers: map[string]uint32{"A": 1}}, t0+6)
		return err
	}(), store.ErrInvalid)
	ok, err = s.PrepareLedgerMerge(ctx, "cs1", m, t0+6)
	must(t, err)
	mustEqual(t, "prepare", ok, true)
	ok, err = s.PrepareLedgerMerge(ctx, "cs1", m, t0+7)
	must(t, err)
	mustEqual(t, "prepare twice", ok, false)
	ok, err = s.DiscardLedgerChangeset(ctx, "cs1", "gone", false, t0+7)
	must(t, err)
	mustEqual(t, "discard during a merge", ok, false)
	got, err = s.GetLedgerChangeset(ctx, "cs1")
	must(t, err)
	mustEqual(t, "pending", got.Pending(), true)
	mustEqual(t, "pending merge", got.Merge, m)
	repairs, err := s.ListLedgerRepairs(ctx, project)
	must(t, err)
	mustEqual(t, "repairs", len(repairs), 1)
	ok, err = s.ClearLedgerMerge(ctx, "cs1", code)
	must(t, err)
	mustEqual(t, "clear another commit", ok, false)
	ok, err = s.ClearLedgerMerge(ctx, "cs1", merge)
	must(t, err)
	mustEqual(t, "clear", ok, true)
	got, err = s.GetLedgerChangeset(ctx, "cs1")
	must(t, err)
	mustEqual(t, "cleared merge", got.Merge, store.LedgerMerge{})
	ok, err = s.FinishLedgerMerge(ctx, "cs1", merge, t0+8)
	must(t, err)
	mustEqual(t, "finish without a merge", ok, false)
	ok, err = s.PrepareLedgerMerge(ctx, "cs1", m, t0+8)
	must(t, err)
	mustEqual(t, "prepare again", ok, true)
	ok, err = s.FinishLedgerMerge(ctx, "cs1", code, t0+9)
	must(t, err)
	mustEqual(t, "finish of another commit", ok, false)
	ok, err = s.FinishLedgerMerge(ctx, "cs1", merge, t0+9)
	must(t, err)
	mustEqual(t, "finish", ok, true)
	got, err = s.GetLedgerChangeset(ctx, "cs1")
	must(t, err)
	want := c
	want.State, want.UpdatedAt, want.Merge = store.LedgerChangesetMerged, t0+9, m
	mustEqual(t, "merged changeset", got, want)
	repairs, err = s.ListLedgerRepairs(ctx, project)
	must(t, err)
	mustEqual(t, "no repairs after merge", len(repairs), 0)
	for name, call := range map[string]func() (bool, error){
		"touch":   func() (bool, error) { return s.TouchLedgerChangeset(ctx, "cs1", t0+10) },
		"prepare": func() (bool, error) { return s.PrepareLedgerMerge(ctx, "cs1", m, t0+10) },
		"finish":  func() (bool, error) { return s.FinishLedgerMerge(ctx, "cs1", merge, t0+10) },
		"clear":   func() (bool, error) { return s.ClearLedgerMerge(ctx, "cs1", merge) },
		"discard": func() (bool, error) { return s.DiscardLedgerChangeset(ctx, "cs1", "late", true, t0+10) },
	} {
		ok, err := call()
		must(t, err)
		mustEqual(t, name+" of a merged changeset", ok, false)
	}

	// A discard records its reason; the branch may still be archived.
	must(t, s.CreateLedgerChangeset(ctx, newChangeset("cs2", project, t0+1)))
	mustIs(t, func() error { _, err := s.DiscardLedgerChangeset(ctx, "cs2", "", true, t0+2); return err }(), store.ErrInvalid)
	mustIs(t, func() error {
		_, err := s.DiscardLedgerChangeset(ctx, "cs2", strings.Repeat("x", store.MaxLedgerDiscardReasonBytes+1), true, t0+2)
		return err
	}(), store.ErrInvalid)
	ok, err = s.DiscardLedgerChangeset(ctx, "cs2", "not needed", false, t0+2)
	must(t, err)
	mustEqual(t, "discard", ok, true)
	repairs, err = s.ListLedgerRepairs(ctx, project)
	must(t, err)
	mustEqual(t, "unarchived discard", len(repairs), 1)
	must(t, s.ArchiveLedgerChangeset(ctx, "cs2"))
	must(t, s.ArchiveLedgerChangeset(ctx, "cs2"))
	mustIs(t, s.ArchiveLedgerChangeset(ctx, "missing"), store.ErrNotFound)
	got, err = s.GetLedgerChangeset(ctx, "cs2")
	must(t, err)
	want = newChangeset("cs2", project, t0+1)
	want.State, want.DiscardReason, want.UpdatedAt = store.LedgerChangesetDiscarded, "not needed", t0+2
	mustEqual(t, "discarded changeset", got, want)
	ok, err = s.DiscardLedgerChangeset(ctx, "cs2", "again", true, t0+3)
	must(t, err)
	mustEqual(t, "discard twice", ok, false)

	// Pages run newest first, by state when asked, with a cursor.
	must(t, s.CreateLedgerChangeset(ctx, newChangeset("cs3", project, t0+1)))
	page, more, err := s.ListLedgerChangesets(ctx, store.LedgerChangesetFilter{ProjectID: project, Limit: 2})
	must(t, err)
	mustEqual(t, "first page", ids(page), []string{"cs3", "cs2"})
	mustEqual(t, "more", more, true)
	page, more, err = s.ListLedgerChangesets(ctx, store.LedgerChangesetFilter{ProjectID: project, Limit: 2,
		After: &store.LedgerChangesetCursor{CreatedAt: page[1].CreatedAt, ID: page[1].ID}})
	must(t, err)
	mustEqual(t, "second page", ids(page), []string{"cs1"})
	mustEqual(t, "no more", more, false)
	for state, want := range map[store.LedgerChangesetState][]string{
		store.LedgerChangesetOpen: {"cs3"}, store.LedgerChangesetMerged: {"cs1"}, store.LedgerChangesetDiscarded: {"cs2"},
	} {
		page, _, err := s.ListLedgerChangesets(ctx, store.LedgerChangesetFilter{ProjectID: project, State: state, Limit: 10})
		must(t, err)
		mustEqual(t, string(state)+" page", ids(page), want)
	}
	for _, f := range []store.LedgerChangesetFilter{
		{ProjectID: project}, {ProjectID: project, Limit: store.MaxLedgerChangesetPage + 1},
		{ProjectID: project, Limit: 1, State: "closed"}, {Limit: 1},
	} {
		_, _, err := s.ListLedgerChangesets(ctx, f)
		mustIs(t, err, store.ErrInvalid)
	}
}

func ids(page []store.LedgerChangeset) []string {
	out := make([]string, 0, len(page))
	for _, c := range page {
		out = append(out, c.ID)
	}
	return out
}

// testLedgerChangesetOpenLimit: concurrent creations never pass the open
// limit, and a merged or discarded changeset frees its place.
func testLedgerChangesetOpenLimit(t *testing.T, b Backend) {
	s, dir := open(t, b)
	ctx := deadline(t)
	seed(t, s)
	handles := []store.Store{s, reopen(t, b, dir)}
	const project = "session-project"
	var wg sync.WaitGroup
	results := make(chan error, store.MaxOpenLedgerChangesets+8)
	for i := range store.MaxOpenLedgerChangesets + 8 {
		wg.Go(func() {
			results <- handles[i%2].CreateLedgerChangeset(ctx, newChangeset(fmt.Sprintf("cs%03d", i), project, t0+int64(i)))
		})
	}
	wg.Wait()
	close(results)
	created := 0
	for err := range results {
		switch {
		case err == nil:
			created++
		case errors.Is(err, store.ErrBusy):
			// A backend may refuse a contended transaction; it left nothing.
		default:
			mustIs(t, err, store.ErrLimit)
		}
	}
	if created > store.MaxOpenLedgerChangesets {
		t.Fatalf("created %d open changesets, limit %d", created, store.MaxOpenLedgerChangesets)
	}
	for i := 0; created < store.MaxOpenLedgerChangesets; i++ {
		err := s.CreateLedgerChangeset(ctx, newChangeset(fmt.Sprintf("late%03d", i), project, t0+100+int64(i)))
		if err == nil {
			created++
		} else {
			mustIs(t, err, store.ErrExists)
		}
	}
	mustIs(t, s.CreateLedgerChangeset(ctx, newChangeset("over", project, t0+200)), store.ErrLimit)
	page, _, err := s.ListLedgerChangesets(ctx, store.LedgerChangesetFilter{ProjectID: project, Limit: 1})
	must(t, err)
	ok, err := s.DiscardLedgerChangeset(ctx, page[0].ID, "make room", true, t0+300)
	must(t, err)
	mustEqual(t, "discard", ok, true)
	must(t, s.CreateLedgerChangeset(ctx, newChangeset("over", project, t0+200)))
	mustIs(t, s.CreateLedgerChangeset(ctx, newChangeset("over2", project, t0+201)), store.ErrLimit)
}

// testLedgerChangesetDeletion: deleting a project, or its user, deletes
// its changesets, also more than one transaction's worth, and a project
// that survives keeps them.
func testLedgerChangesetDeletion(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seedProjects(t, s)
	_, err := s.CreateProject(ctx, newProject("p1", f.alice.ID, "one"))
	must(t, err)
	_, err = s.CreateProject(ctx, newProject("p2", f.alice.ID, "two"))
	must(t, err)
	_, err = s.CreateProject(ctx, newProject("p3", f.bob.ID, "three"))
	must(t, err)
	for i := range 150 {
		for _, p := range []string{"p1", "p2", "p3"} {
			id := fmt.Sprintf("%s-%03d", p, i)
			must(t, s.CreateLedgerChangeset(ctx, newChangeset(id, p, t0+int64(i))))
			if _, err := s.DiscardLedgerChangeset(ctx, id, "history", true, t0+int64(i)); err != nil {
				t.Fatal(err)
			}
		}
	}
	must(t, s.DeleteProject(ctx, f.alice.ID, "p1", t0+1000))
	_, err = s.GetLedgerChangeset(ctx, "p1-000")
	mustIs(t, err, store.ErrNotFound)
	page, _, err := s.ListLedgerChangesets(ctx, store.LedgerChangesetFilter{ProjectID: "p1", Limit: 1})
	must(t, err)
	mustEqual(t, "changesets of a deleted project", len(page), 0)
	page, _, err = s.ListLedgerChangesets(ctx, store.LedgerChangesetFilter{ProjectID: "p2", Limit: 1})
	must(t, err)
	mustEqual(t, "changesets of a surviving project", len(page), 1)

	// A user's deletion takes its projects' changesets.
	must(t, s.CreateUser(ctx, store.User{ID: "carol", Email: "carol@example.com", Role: store.RoleUser, CreatedAt: t0}))
	_, err = s.CreateProject(ctx, newProject("p4", "carol", "four"))
	must(t, err)
	for i := range 70 {
		must(t, s.CreateLedgerChangeset(ctx, newChangeset(fmt.Sprintf("p4-%03d", i), "p4", t0+int64(i))))
		if i >= store.MaxOpenLedgerChangesets-1 {
			_, err := s.DiscardLedgerChangeset(ctx, fmt.Sprintf("p4-%03d", i), "history", true, t0)
			must(t, err)
		}
	}
	must(t, s.DeleteUser(ctx, "carol"))
	_, err = s.GetLedgerChangeset(ctx, "p4-000")
	mustIs(t, err, store.ErrNotFound)

	// Orphans, as a crash leaves them, are purged; other rows stay.
	must(t, s.PurgeOrphanLedgerChangesets(ctx))
	_, err = s.GetLedgerChangeset(ctx, "p3-149")
	must(t, err)
}

// testWorkerIntegrator: the grant is set on a live worker of the user
// only, read with the identity, and ends with the worker's deletion or the
// revocation of its credentials.
func testWorkerIntegrator(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	w, err := s.SetWorkerIntegrator(ctx, "u1", "w1", true)
	must(t, err)
	want := f.worker
	want.Integrator = true
	mustEqual(t, "granted worker", w, want)
	got, err := s.GetIdentity(ctx, store.KindWorker, "w1")
	must(t, err)
	mustEqual(t, "read grant", got.Integrator, true)
	list, err := s.ListIdentities(ctx, store.KindWorker, "u1")
	must(t, err)
	mustEqual(t, "listed grant", list[0].Integrator, true)
	byName, err := s.GetIdentityByName(ctx, store.KindWorker, "u1", "laptop")
	must(t, err)
	mustEqual(t, "grant by name", byName.Integrator, true)
	device, err := s.GetIdentity(ctx, store.KindDevice, "d1")
	must(t, err)
	mustEqual(t, "a device has no grant", device.Integrator, false)

	must(t, s.CreateUser(ctx, store.User{ID: "u2", Name: "other", CreatedAt: t0}))
	_, err = s.SetWorkerIntegrator(ctx, "u2", "w1", false)
	mustIs(t, err, store.ErrNotFound)
	_, err = s.SetWorkerIntegrator(ctx, "u1", "d1", true)
	mustIs(t, err, store.ErrNotFound)
	_, err = s.SetWorkerIntegrator(ctx, "", "w1", true)
	mustIs(t, err, store.ErrInvalid)

	// Revoking the credentials ends the grant.
	must(t, s.CreateCredential(ctx, store.Credential{ID: "c-w", Kind: store.KindWorker, PrincipalID: "w1", TokenHash: hash("worker"), CreatedAt: t0}))
	_, err = s.RevokeCredentials(ctx, store.KindWorker, "w1", t0+1)
	must(t, err)
	got, err = s.GetIdentity(ctx, store.KindWorker, "w1")
	must(t, err)
	mustEqual(t, "grant after revocation", got.Integrator, false)

	// So does deleting the worker, after which it cannot be granted.
	_, err = s.SetWorkerIntegrator(ctx, "u1", "w1", true)
	must(t, err)
	_, err = s.DeleteIdentity(ctx, store.KindWorker, "w1", t0+2)
	must(t, err)
	got, err = s.GetIdentity(ctx, store.KindWorker, "w1")
	must(t, err)
	mustEqual(t, "grant after deletion", got.Integrator, false)
	_, err = s.SetWorkerIntegrator(ctx, "u1", "w1", true)
	mustIs(t, err, store.ErrNotFound)
}
