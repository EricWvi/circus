package ledger

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	snapshotfiles "github.com/EricWvi/circus/internal/ledger/snapshot"
	"github.com/EricWvi/circus/internal/store"
	"github.com/go-git/go-git/v5/plumbing"
)

func content(p string, data []byte) SnapshotChange { return SnapshotChange{Path: p, Content: data} }

func known(p string, data []byte) SnapshotChange {
	return SnapshotChange{Path: p, Known: true, SHA256: sha256Hex(data)}
}

func sessionCheck(t *testing.T, m *Manager, id, view, base string, changes ...SnapshotChange) SessionCheckResult {
	t.Helper()
	changesetSeq++
	res, err := m.CheckSessionChangeset(t.Context(), SessionCheck{ProjectID: "project", SessionID: id,
		NewChangesetID: fmt.Sprintf("scs-%03d", changesetSeq), ViewBase: view, SnapshotBase: base, Changes: changes})
	check(t, err)
	return res
}

func entryNames(entries []SessionEntry) []string {
	var out []string
	for _, e := range entries {
		out = append(out, e.Path)
	}
	return out
}

// TestFastForwardByTreeEquality: a changeset whose base differs from the
// main line's head but holds the same tree fast-forwards, for a worker
// without the integrator grant too.
func TestFastForwardByTreeEquality(t *testing.T) {
	m, _ := openLedger(t, nil, "project")
	first := []Decision{{Number: 0, Title: "Storage", Body: []byte("SQLite.\n")}}
	commit(t, m, "project", first, CommitOptions{Author: "a", Message: "one"})
	c := openCS(t, m, "project")
	commit(t, m, "project", []Decision{{Number: 0, Title: "Storage", Body: []byte("Postgres.\n")}}, CommitOptions{Author: "a", Message: "two"})
	head := commit(t, m, "project", first, CommitOptions{Author: "a", Message: "three"})
	if head == c.BaseCommit {
		t.Fatal("the main line did not move")
	}
	res := checkCS(t, m, c.ID, edit("D?a-Cache.md", doc("D?a", "Cache", "Cache reads.\n")))
	if !res.FastForward || len(res.Conflicts) != 0 {
		t.Fatalf("check = %+v", res)
	}
	page, err := m.ReadChangesetConflicts(t.Context(), c.ID, head, "", "", 1<<20)
	check(t, err)
	if len(page.Conflicts) != 0 {
		t.Fatalf("conflicts = %+v", page.Conflicts)
	}
	merged := mergeCS(t, m, c.ID, MergeOptions{Authorize: opener})
	if merged.Merge.Parent != head || merged.Merge.Numbers["a"] != 1 {
		t.Fatalf("merge = %+v", merged.Merge)
	}
}

// TestSessionChangesets runs a session's changesets: opened by a
// collection with changes, checked again with known changes, merged, the
// view after the merge with the edits that rename the new decision, a
// next changeset that fast-forwards on the view until someone else merges.
func TestSessionChangesets(t *testing.T) {
	ctx := t.Context()
	m, _ := openLedger(t, nil, "project")
	head := commit(t, m, "project", []Decision{{Number: 0, Title: "Storage", Body: []byte("SQLite.\n")}}, CommitOptions{Author: "a", Message: "one"})
	files := mainFiles(t, m, "project")
	d0 := []byte(files["D0-Storage.md"])

	// No changes: nothing is opened.
	res := sessionCheck(t, m, "s1", head, head)
	if res.Changeset.ID != "" || res.Opened || len(res.Known) != 0 {
		t.Fatalf("empty check = %+v", res)
	}
	// Changes open the session's changeset on the view.
	newDoc := doc("D?a", "Backups", "Back up nightly; see D0 and D?a.\n")
	d0b := []byte(strings.Replace(string(d0), "SQLite.", "SQLite in WAL mode.", 1))
	res = sessionCheck(t, m, "s1", head, head, content("D?a-Backups.md", newDoc), content("D0-Storage.md", d0b), content("notes.txt", []byte("x\n")))
	if !res.Opened || res.Changeset.SessionID != "s1" || res.Changeset.BaseCommit != head || res.Changeset.OpenerWorkerID != "" || !res.FastForward {
		t.Fatalf("opened = %+v", res)
	}
	if !hasProblem(res.Problems, "notes.txt", "unexpected file") || len(res.Known) != 3 {
		t.Fatalf("problems %v, known %v", res.Problems, res.Known)
	}
	if got := entryNames(res.Entries); !slices.Equal(got, []string{"D0-Storage.md", "D?a-Backups.md", "notes.txt"}) {
		t.Fatalf("entries %v", got)
	}
	if e := res.Entries[1]; !e.Added || e.Number != "D?a" || e.Title != "Backups" {
		t.Fatalf("new entry %+v", e)
	}
	if e := res.Entries[0]; !e.ContentChanged || e.Number != "D0" || e.Added || e.Retitled {
		t.Fatalf("changed entry %+v", e)
	}
	id := res.Changeset.ID
	// Known changes come from the changeset; a wrong hash is refused.
	res = sessionCheck(t, m, "s1", head, head, known("D?a-Backups.md", newDoc), known("D0-Storage.md", d0b))
	if res.Changeset.ID != id || res.Opened || len(res.Entries) != 2 || len(res.Problems) != 0 {
		t.Fatalf("check with known changes = %+v", res)
	}
	_, err := m.CheckSessionChangeset(ctx, SessionCheck{ProjectID: "project", SessionID: "s1", ViewBase: head, SnapshotBase: head,
		Changes: []SnapshotChange{known("D?a-Backups.md", []byte("other"))}})
	if !errors.Is(err, ErrKnownChanged) {
		t.Fatalf("wrong known hash: %v", err)
	}
	read, err := m.ReadSessionChangeset(ctx, id)
	check(t, err)
	if !read.FastForward || !slices.Equal(entryNames(read.Entries), []string{"D0-Storage.md", "D?a-Backups.md"}) {
		t.Fatalf("read = %+v", read)
	}
	d, err := m.ReadSessionDiff(ctx, id, "D0-Storage.md")
	check(t, err)
	if string(d.Base.Content) != string(d0) || string(d.Changeset.Content) != string(d0b) {
		t.Fatalf("diff = %+v", d)
	}
	if _, err := m.ReadSessionDiff(ctx, id, "INDEX.md"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("diff of an unchanged path: %v", err)
	}

	// Merging it, the view renames the new decision as the merge did; the
	// snapshot, unchanged since the collection, is rebased onto it.
	merged := mergeCS(t, m, id, MergeOptions{Author: SessionAuthor("s1"), Authorize: opener})
	unchanged := RebaseInput{ChangesetID: id, SnapshotBase: head, Changes: []SnapshotChange{known("D?a-Backups.md", newDoc), known("D0-Storage.md", d0b)}}
	plan, err := m.PlanSessionRebase(ctx, unchanged)
	check(t, err)
	again, err := m.PlanSessionRebase(ctx, unchanged)
	check(t, err)
	viewOnly, err := m.SessionView(ctx, id)
	check(t, err)
	if again.View != plan.View || viewOnly != plan.View || plan.Target != plan.View {
		t.Fatalf("view built twice: %s, %s, %s; target %s", plan.View, again.View, viewOnly, plan.Target)
	}
	view := struct{ Commit string }{plan.View}
	if len(plan.Renamed) != 1 || plan.Renamed["D?a-Backups.md"] != "D1-Backups.md" || plan.Numbers["a"] != 1 {
		t.Fatalf("renamed %v, numbers %v", plan.Renamed, plan.Numbers)
	}
	var edits []string
	for _, e := range plan.Edits {
		edits = append(edits, e.Path)
	}
	// D0 holds the merged content already: the agent kept main's rendering.
	if !slices.Equal(edits, []string{"D1-Backups.md", "D?a-Backups.md", "INDEX.md"}) {
		t.Fatalf("edits %v", edits)
	}
	for _, e := range plan.Edits {
		switch e.Path {
		case "D?a-Backups.md":
			if !e.Deleted || e.ExpectedSHA256 != sha256Hex(newDoc) {
				t.Fatalf("edit %+v", e)
			}
		case "D1-Backups.md":
			if e.ExpectedSHA256 != "" || !strings.Contains(string(e.Content), "number: D1") || !strings.Contains(string(e.Content), "see D0 and D1.") {
				t.Fatalf("edit %+v", e)
			}
		}
	}
	if len(plan.Known) != 2 {
		t.Fatalf("known %v", plan.Known)
	}
	r, err := m.open(m.repoPath("project"))
	check(t, err)
	vc, err := r.commit(plumbing.NewHash(view.Commit))
	check(t, err)
	mc, err := r.commit(plumbing.NewHash(merged.Merge.Commit))
	check(t, err)
	ref, err := r.readRef(sessionRef("s1"))
	check(t, err)
	r.storage.Close()
	if len(vc.ParentHashes) != 1 || vc.ParentHashes[0].String() != merged.Merge.Commit || vc.TreeHash != mc.TreeHash || ref.String() != view.Commit {
		t.Fatalf("view %s: parents %v, tree %s (merge tree %s), ref %s", view.Commit, vc.ParentHashes, vc.TreeHash, mc.TreeHash, ref)
	}

	// The next changeset starts from the view and fast-forwards.
	viewFiles := mainFiles(t, m, "project")
	d1 := []byte(viewFiles["D1-Backups.md"])
	res = sessionCheck(t, m, "s1", view.Commit, view.Commit, content("D1-Backups.md", []byte(strings.Replace(string(d1), "nightly", "hourly", 1))))
	if !res.Opened || res.Changeset.BaseCommit != view.Commit || !res.FastForward {
		t.Fatalf("next changeset = %+v", res)
	}
	next := res.Changeset.ID
	// Someone else merges: it no longer fast-forwards, and a file both
	// changed conflicts.
	other := openCS(t, m, "project")
	checkCS(t, m, other.ID, edit("D1-Backups.md", []byte(strings.Replace(string(d1), "nightly", "weekly", 1))))
	mergeCS(t, m, other.ID, MergeOptions{Authorize: opener})
	read, err = m.ReadSessionChangeset(ctx, next)
	check(t, err)
	if read.FastForward {
		t.Fatal("fast-forward after another merge")
	}
	res = sessionCheck(t, m, "s1", view.Commit, view.Commit, content("D1-Backups.md", []byte(strings.Replace(string(d1), "nightly", "hourly", 1))))
	if res.FastForward || len(res.Conflicts) != 1 || res.Conflicts[0].Path != "D1-Backups.md" {
		t.Fatalf("check after another merge = %+v", res)
	}
	if _, err := m.MergeChangeset(ctx, next, MergeOptions{Author: "x", Authorize: opener}); !errors.Is(err, errNotIntegrator) {
		t.Fatalf("merge without the grant: %v", err)
	}

	check(t, m.RemoveSession(ctx, "project", "s1"))
	r, err = m.open(m.repoPath("project"))
	check(t, err)
	ref, err = r.readRef(sessionRef("s1"))
	check(t, err)
	r.storage.Close()
	if !ref.IsZero() {
		t.Fatalf("session reference after removal: %s", ref)
	}
}

// rebasePlan plans the rebase of s1's snapshot of head after the merge
// of id, from a collection that changed the snapshot from the merged one
// by later (relative to the merged files).
func rebasePlan(t *testing.T, m *Manager, id, head string, merged map[string][]byte, later ...SnapshotChange) RebasePlan {
	t.Helper()
	byPath := map[string]SnapshotChange{}
	for p, data := range merged {
		byPath[p] = known(p, data)
	}
	for _, c := range later {
		if c.Deleted && merged[c.Path] == nil {
			delete(byPath, c.Path)
			continue
		}
		byPath[c.Path] = c
	}
	var changes []SnapshotChange
	for _, p := range slices.Sorted(maps.Keys(byPath)) {
		changes = append(changes, byPath[p])
	}
	plan, err := m.PlanSessionRebase(t.Context(), RebaseInput{ChangesetID: id, SnapshotBase: head, Changes: changes})
	check(t, err)
	return plan
}

// snapshotAfter applies a plan's edits to the snapshot's files.
func snapshotAfter(files map[string][]byte, plan RebasePlan) map[string][]byte {
	out := maps.Clone(files)
	for _, e := range plan.Edits {
		if e.Deleted {
			delete(out, e.Path)
		} else {
			out[e.Path] = e.Content
		}
	}
	return out
}

// TestPlanSessionRebase: changes the agent made after the collection a
// merge used are kept; a new decision whose number the merge assigned
// takes it where the agent left the decision, so the next changeset
// holds the change as one of that decision.
func TestPlanSessionRebase(t *testing.T) {
	ctx := t.Context()
	type want struct {
		// file is the new decision's file after the rebase, with a line.
		file, line string
		// entries is the next changeset, relative to the view; added are
		// those of new decisions.
		entries, added []string
		renamed        map[string]string
		restored       []string
		// problems are the paths the plan asks the agent to check;
		// nextProblems those of the next changeset.
		problems, nextProblems []string
	}
	newDoc := doc("D?a", "Cache", "Cache reads.\n")
	cases := []struct {
		name  string
		later func(d0 []byte) []SnapshotChange
		want  want
	}{
		{name: "unchanged", later: func([]byte) []SnapshotChange { return nil },
			want: want{file: "D1-Cache.md", line: "Cache reads.", renamed: map[string]string{"D?a-Cache.md": "D1-Cache.md"}}},
		{name: "retitle", later: func([]byte) []SnapshotChange {
			return []SnapshotChange{{Path: "D?a-Cache.md", Deleted: true}, content("D?a-Caching.md", doc("D?a", "Caching", "Cache reads.\n"))}
		}, want: want{file: "D1-Caching.md", line: "Caching", entries: []string{"D1-Caching.md"},
			renamed: map[string]string{"D?a-Caching.md": "D1-Caching.md"}}},
		{name: "move", later: func([]byte) []SnapshotChange {
			return []SnapshotChange{{Path: "D?a-Cache.md", Deleted: true}, content("infra/D?a-Cache.md", newDoc)}
		}, want: want{file: "infra/D1-Cache.md", line: "Cache reads.", entries: []string{"infra/D1-Cache.md"},
			renamed: map[string]string{"infra/D?a-Cache.md": "infra/D1-Cache.md"}}},
		{name: "revise", later: func([]byte) []SnapshotChange {
			return []SnapshotChange{content("D?a-Cache.md", doc("D?a", "Cache", "Cache reads and writes; see D?a.\n"))}
		}, want: want{file: "D1-Cache.md", line: "Cache reads and writes; see D1.", entries: []string{"D1-Cache.md"},
			renamed: map[string]string{"D?a-Cache.md": "D1-Cache.md"}}},
		{name: "rewrite", later: func([]byte) []SnapshotChange {
			return []SnapshotChange{{Path: "D?a-Cache.md", Deleted: true}, content("ops/D?a-Eviction.md", doc("D?a", "Eviction", "Evict.\n"))}
		}, want: want{file: "ops/D1-Eviction.md", line: "Evict.", entries: []string{"ops/D1-Eviction.md"},
			renamed: map[string]string{"ops/D?a-Eviction.md": "ops/D1-Eviction.md"}}},
		{name: "remove", later: func([]byte) []SnapshotChange {
			return []SnapshotChange{{Path: "D?a-Cache.md", Deleted: true}}
		}, want: want{file: "D1-Cache.md", line: "Cache reads.", renamed: map[string]string{}, restored: []string{"D1-Cache.md"}}},
		// The merged path is gone and one file carries the letters: taken
		// for the decision, as a retitle and rewrite would be.
		{name: "letters reused, merged path gone", later: func([]byte) []SnapshotChange {
			return []SnapshotChange{{Path: "D?a-Cache.md", Deleted: true}, content("D?a-Queue.md", doc("D?a", "Queue", "Use a queue.\n"))}
		}, want: want{file: "D1-Queue.md", line: "Use a queue.", entries: []string{"D1-Queue.md"},
			renamed: map[string]string{"D?a-Queue.md": "D1-Queue.md"}}},
		{name: "letters reused, merged path kept", later: func([]byte) []SnapshotChange {
			return []SnapshotChange{content("D?a-Queue.md", doc("D?a", "Queue", "Use a queue, not D?a.\n"))}
		}, want: want{file: "D1-Cache.md", line: "Cache reads.", entries: []string{"D?b-Queue.md"}, added: []string{"D?b-Queue.md"},
			renamed: map[string]string{"D?a-Cache.md": "D1-Cache.md", "D?a-Queue.md": "D?b-Queue.md"}, problems: []string{"D?b-Queue.md"},
			// Its reference to the reused letters stays, for the agent
			// to resolve before the changeset is merged.
			nextProblems: []string{"D?b-Queue.md: refers to D?a, which no decision of this changeset has; check it"}}},
		{name: "letters reused twice, merged path gone", later: func([]byte) []SnapshotChange {
			return []SnapshotChange{{Path: "D?a-Cache.md", Deleted: true}, content("D?a-Queue.md", doc("D?a", "Queue", "Use a queue.\n")),
				content("ops/D?a-Stream.md", doc("D?a", "Stream", "Stream it.\n"))}
		}, want: want{file: "D1-Cache.md", line: "Cache reads.", entries: []string{"D?b-Queue.md", "ops/D?c-Stream.md"},
			added: []string{"D?b-Queue.md", "ops/D?c-Stream.md"}, restored: []string{"D1-Cache.md"},
			renamed:  map[string]string{"D?a-Queue.md": "D?b-Queue.md", "ops/D?a-Stream.md": "ops/D?c-Stream.md"},
			problems: []string{"D?b-Queue.md", "ops/D?c-Stream.md"}}},
		{name: "other file", later: func(d0 []byte) []SnapshotChange {
			return []SnapshotChange{content("D0-Storage.md", []byte(strings.Replace(string(d0), "SQLite.", "SQLite, cached per D?a.", 1)))}
		}, want: want{file: "D1-Cache.md", line: "Cache reads.", entries: []string{"D0-Storage.md"},
			renamed: map[string]string{"D?a-Cache.md": "D1-Cache.md"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, _ := openLedger(t, nil, "project")
			head := commit(t, m, "project", []Decision{{Number: 0, Title: "Storage", Body: []byte("SQLite.\n")}}, CommitOptions{Author: "a", Message: "one"})
			files := map[string][]byte{}
			for p, data := range mainFiles(t, m, "project") {
				files[p] = []byte(data)
			}
			res := sessionCheck(t, m, "s1", head, head, content("D?a-Cache.md", newDoc))
			mergeCS(t, m, res.Changeset.ID, MergeOptions{Authorize: opener})
			later := c.later(files["D0-Storage.md"])
			plan := rebasePlan(t, m, res.Changeset.ID, head, map[string][]byte{"D?a-Cache.md": newDoc}, later...)
			if !maps.Equal(plan.Renamed, c.want.renamed) {
				t.Fatalf("renamed %v, want %v", plan.Renamed, c.want.renamed)
			}
			if !slices.Equal(plan.Restored, c.want.restored) {
				t.Fatalf("restored %v, want %v", plan.Restored, c.want.restored)
			}
			var problems []string
			for _, p := range plan.Problems {
				problems = append(problems, p.Path)
				if !strings.Contains(p.Message, "reused the temporary number D?a") || !strings.Contains(p.Message, "D1-Cache.md") {
					t.Fatalf("problem %+v", p)
				}
			}
			if !slices.Equal(problems, c.want.problems) {
				t.Fatalf("problems %v, want %v", problems, c.want.problems)
			}
			// The snapshot the edits leave.
			snap := maps.Clone(files)
			snap["D?a-Cache.md"] = newDoc
			for _, ch := range later {
				if ch.Deleted {
					delete(snap, ch.Path)
				} else {
					snap[ch.Path] = ch.Content
				}
			}
			for _, e := range plan.Edits {
				have, ok := snap[e.Path]
				if e.ExpectedSHA256 == "" && ok || e.ExpectedSHA256 != "" && (!ok || sha256Hex(have) != e.ExpectedSHA256) {
					t.Fatalf("edit %s expects %q, the snapshot holds %v", e.Path, e.ExpectedSHA256, ok)
				}
			}
			snap = snapshotAfter(snap, plan)
			for p := range snap {
				if strings.Contains(p, "D?a") {
					t.Fatalf("temporary file %s kept: %v", p, slices.Sorted(maps.Keys(snap)))
				}
			}
			if c.want.file != "" && !strings.Contains(string(snap[c.want.file]), c.want.line) {
				t.Fatalf("%s %q (snapshot %v)", c.want.file, snap[c.want.file], slices.Sorted(maps.Keys(snap)))
			}
			if c.want.file != "" && !strings.Contains(string(snap[c.want.file]), "number: D1\n") {
				t.Fatalf("%s %q", c.want.file, snap[c.want.file])
			}
			// The snapshot holds the target's files; collected with
			// nothing new, the next changeset is the later change relative
			// to the view.
			target := map[string]string{}
			page, err := m.ReadSnapshotFiles(ctx, "project", plan.Target, "", 1<<20, 100)
			check(t, err)
			for _, f := range page.Files {
				target[f.Path] = string(f.Content)
			}
			if len(target) != len(snap) {
				t.Fatalf("target %v, snapshot %v", slices.Sorted(maps.Keys(target)), slices.Sorted(maps.Keys(snap)))
			}
			for p, data := range snap {
				if target[p] != string(data) {
					t.Fatalf("target %s %q, snapshot %q", p, target[p], data)
				}
			}
			if (plan.Target == plan.View) != (len(c.want.entries) == 0) {
				t.Fatalf("target %s, view %s", plan.Target, plan.View)
			}
			next := sessionCheck(t, m, "s1", plan.View, plan.Target)
			if got := entryNames(next.Entries); !slices.Equal(got, c.want.entries) {
				t.Fatalf("next changeset %v, want %v", got, c.want.entries)
			}
			var added []string
			for _, e := range next.Entries {
				if e.Added {
					added = append(added, e.Path)
				} else if e.Number != "D0" && e.Number != "D1" {
					t.Fatalf("next entry %+v", e)
				}
			}
			if !slices.Equal(added, c.want.added) {
				t.Fatalf("added %v, want %v", added, c.want.added)
			}
			var nextProblems []string
			for _, p := range next.Problems {
				nextProblems = append(nextProblems, p.Path+": "+p.Message)
			}
			if !slices.Equal(nextProblems, c.want.nextProblems) {
				t.Fatalf("next problems %v, want %v", nextProblems, c.want.nextProblems)
			}
			for _, w := range next.Warnings {
				if strings.Contains(w.Message, "D?a") {
					t.Fatalf("a stale reference is also a warning: %v", next.Warnings)
				}
			}
		})
	}
}

// TestPlanSessionRebaseOtherFiles: a file that is not a decision keeps its
// path, its references to the merged temporary numbers replaced.
func TestPlanSessionRebaseOtherFiles(t *testing.T) {
	m, _ := openLedger(t, nil, "project")
	head := commit(t, m, "project", []Decision{{Number: 0, Title: "Storage", Body: []byte("SQLite.\n")}}, CommitOptions{Author: "a", Message: "one"})
	newDoc := doc("D?a", "Cache", "Cache reads.\n")
	res := sessionCheck(t, m, "s1", head, head, content("D?a-Cache.md", newDoc))
	mergeCS(t, m, res.Changeset.ID, MergeOptions{Authorize: opener})
	plan := rebasePlan(t, m, res.Changeset.ID, head, map[string][]byte{"D?a-Cache.md": newDoc}, content("notes.md", []byte("See D?a.\n")))
	page, err := m.ReadSnapshotFiles(t.Context(), "project", plan.Target, "", 1<<20, 100)
	check(t, err)
	for _, f := range page.Files {
		if f.Path == "notes.md" {
			if string(f.Content) != "See D1.\n" {
				t.Fatalf("notes.md %q", f.Content)
			}
			return
		}
	}
	t.Fatal("notes.md not in the target")
}

// TestPlanSessionRebaseNameTaken: a later file takes the name a new
// decision renumbers to; the decision stays as the agent left it, for the
// agent to sort out, and is not reported renamed.
func TestPlanSessionRebaseNameTaken(t *testing.T) {
	m, _ := openLedger(t, nil, "project")
	head := commit(t, m, "project", []Decision{{Number: 0, Title: "Storage", Body: []byte("SQLite.\n")}}, CommitOptions{Author: "a", Message: "one"})
	newDoc := doc("D?a", "Cache", "Cache reads.\n")
	res := sessionCheck(t, m, "s1", head, head, content("D?a-Cache.md", newDoc))
	mergeCS(t, m, res.Changeset.ID, MergeOptions{Authorize: opener})
	mine, revised := doc("D1", "Cache", "Mine.\n"), doc("D?a", "Cache", "Revised.\n")
	plan := rebasePlan(t, m, res.Changeset.ID, head, map[string][]byte{"D?a-Cache.md": newDoc},
		content("D1-Cache.md", mine), content("D?a-Cache.md", revised))
	page, err := m.ReadSnapshotFiles(t.Context(), "project", plan.Target, "", 1<<20, 100)
	check(t, err)
	target := map[string]string{}
	for _, f := range page.Files {
		target[f.Path] = string(f.Content)
	}
	if target["D1-Cache.md"] != string(mine) || target["D?a-Cache.md"] != string(revised) || len(plan.Renamed) != 0 {
		t.Fatalf("target %v, renamed %v", target, plan.Renamed)
	}
}

// TestSessionEntries describes moves, renames, deletions and other files.
func TestSessionEntries(t *testing.T) {
	m, _ := openLedger(t, nil, "project")
	head := commit(t, m, "project", []Decision{
		{Number: 0, Title: "Storage", Body: []byte("SQLite.\n")},
		{Number: 1, Dir: "api", Title: "Transport", Body: []byte("Connect.\n")},
		{Number: 2, Title: "Old", Body: []byte("Gone.\n")},
	}, CommitOptions{Author: "a", Message: "one"})
	files := mainFiles(t, m, "project")
	res := sessionCheck(t, m, "s1", head, head,
		SnapshotChange{Path: "api/D1-Transport.md", Deleted: true},
		content("net/D1-Transport.md", []byte(files["api/D1-Transport.md"])),
		SnapshotChange{Path: "D0-Storage.md", Deleted: true},
		content("D0-Data.md", doc("D0", "Data", "SQLite.\n")),
		SnapshotChange{Path: "D2-Old.md", Deleted: true},
		content("INDEX.md", []byte("edited\n")))
	byPath := map[string]SessionEntry{}
	for _, e := range res.Entries {
		byPath[e.Path] = e
	}
	if e := byPath["net/D1-Transport.md"]; e.BasePath != "api/D1-Transport.md" || !e.Moved || e.Retitled || e.ContentChanged || e.Number != "D1" {
		t.Fatalf("moved %+v", e)
	}
	if e := byPath["D0-Data.md"]; e.BasePath != "D0-Storage.md" || e.Moved || !e.Retitled || e.BaseTitle != "Storage" || e.ContentChanged {
		t.Fatalf("retitled %+v", e)
	}
	if e := byPath["D2-Old.md"]; !e.Deleted || e.Number != "D2" || e.Title != "Old" {
		t.Fatalf("deleted %+v", e)
	}
	if e := byPath["INDEX.md"]; !e.ContentChanged || e.Number != "" {
		t.Fatalf("index %+v", e)
	}
	if len(res.Entries) != 4 {
		t.Fatalf("entries %+v", res.Entries)
	}
	if !hasProblem(res.Problems, "D2-Old.md", "cannot be removed") || !hasProblem(res.Problems, "INDEX.md", "generated") {
		t.Fatalf("problems %v", res.Problems)
	}
}

// TestSessionChangesetOfAnEmptyLedger: a snapshot of an empty ledger has
// no base commit and holds the generated index alone; its changeset merges
// and gets a view, and the rebase replaces the empty index.
func TestSessionChangesetOfAnEmptyLedger(t *testing.T) {
	ctx := t.Context()
	m, _ := openLedger(t, nil, "project")
	page, err := m.ReadSnapshotFiles(ctx, "project", "", "", 1<<20, 10)
	check(t, err)
	if len(page.Files) != 1 || page.Files[0].Path != indexName || page.More ||
		!bytes.Equal(page.Files[0].Content, RenderIndex(nil)) || !bytes.Contains(page.Files[0].Content, []byte("no decisions yet")) {
		t.Fatalf("empty ledger page = %+v", page)
	}
	if page, err = m.ReadSnapshotFiles(ctx, "project", "", indexName, 1<<20, 10); err != nil || len(page.Files) != 0 || page.More {
		t.Fatalf("empty ledger page after its index = %+v, %v", page, err)
	}
	// The unchanged snapshot, without a repository, has no changes; a
	// changed index is a problem, as for any ledger.
	if res := sessionCheck(t, m, "s0", "", ""); res.Opened || res.Changeset.ID != "" || len(res.Problems) != 0 || len(res.Known) != 0 {
		t.Fatalf("unchanged empty snapshot = %+v", res)
	}
	removed := sessionCheck(t, m, "s0", "", "", SnapshotChange{Path: indexName, Deleted: true})
	if !removed.Opened || !hasProblem(removed.Problems, indexName, "generated") {
		t.Fatalf("removed index = %+v", removed)
	}
	newDoc := doc("D?a", "First", "Start.\n")
	res := sessionCheck(t, m, "s1", "", "", content("D?a-First.md", newDoc))
	if !res.Opened || res.Changeset.BaseCommit != "" || !res.FastForward {
		t.Fatalf("check = %+v", res)
	}
	mergeCS(t, m, res.Changeset.ID, MergeOptions{Authorize: opener})
	plan, err := m.PlanSessionRebase(ctx, RebaseInput{ChangesetID: res.Changeset.ID, Changes: []SnapshotChange{known("D?a-First.md", newDoc)}})
	check(t, err)
	if plan.Renamed["D?a-First.md"] != "D0-First.md" || len(plan.Known) != 1 || plan.Target != plan.View {
		t.Fatalf("plan = %+v", plan)
	}
	// The snapshot holds the empty index, which the rebase replaces.
	i := slices.IndexFunc(plan.Edits, func(e SnapshotEdit) bool { return e.Path == indexName })
	if i < 0 || plan.Edits[i].Deleted || plan.Edits[i].ExpectedSHA256 != sha256Hex(RenderIndex(nil)) || !bytes.Contains(plan.Edits[i].Content, []byte("| D0 |  | First |")) {
		t.Fatalf("edits = %+v", plan.Edits)
	}
	view := struct{ Commit string }{plan.View}
	page, err = m.ReadSnapshotFiles(ctx, "project", view.Commit, "", 1<<20, 1)
	check(t, err)
	if len(page.Files) != 1 || page.Files[0].Path != "D0-First.md" || !page.More {
		t.Fatalf("first page = %+v", page)
	}
	page, err = m.ReadSnapshotFiles(ctx, "project", view.Commit, page.Files[0].Path, 1<<20, 10)
	check(t, err)
	if len(page.Files) != 1 || page.Files[0].Path != "INDEX.md" || page.More {
		t.Fatalf("second page = %+v", page)
	}
}

// TestFoldCollisions: a changed path that is one name with another path of
// the resulting ledger on macOS (case, or Unicode normalization, of a
// directory or a file) is a problem naming both; a case-only move is not.
func TestFoldCollisions(t *testing.T) {
	m, _ := openLedger(t, nil, "project")
	commit(t, m, "project", []Decision{{Number: 0, Dir: "core", Title: "Storage", Body: []byte("x\n")},
		{Number: 1, Dir: "café", Title: "Menu", Body: []byte("y\n")}}, CommitOptions{Author: "a", Message: "one"})
	files := mainFiles(t, m, "project")
	c := openCS(t, m, "project")
	res := checkCS(t, m, c.ID,
		edit("Core/D?a-New.md", doc("D?a", "New", "z\n")),
		edit("café/D?b-Other.md", doc("D?b", "Other", "w\n")))
	if !hasProblem(res.Problems, "Core/D?a-New.md", `"Core" and "core"`) ||
		!hasProblem(res.Problems, "café/D?b-Other.md", `"café" and "café"`) {
		t.Fatalf("problems %v", res.Problems)
	}
	// Moving a directory to another case removes the old name.
	res = checkCS(t, m, c.ID, remove("core/D0-Storage.md"), edit("Core/D0-Storage.md", []byte(files["core/D0-Storage.md"])))
	if len(res.Problems) != 0 {
		t.Fatalf("case-only move: %v", res.Problems)
	}
}

// TestIndexBound: the generated index of a ledger at its bounds, every
// title made of characters the index escapes, fits snapshots' bound for
// it.
func TestIndexBound(t *testing.T) {
	dir := strings.Repeat(strings.Repeat("d", 63)+"/", 4)[:255]
	entries := make([]store.LedgerEntry, store.MaxLedgerDecisions)
	for i := range entries {
		entries[i] = store.LedgerEntry{Number: store.MaxLedgerNumber - uint32(i), Dir: dir, Title: strings.Repeat("<", store.MaxLedgerTitleBytes)}
	}
	if n := len(renderIndex(entries)); n > snapshotfiles.MaxIndexBytes {
		t.Fatalf("index of %d bytes, over %d", n, snapshotfiles.MaxIndexBytes)
	}
}

// TestEmptyIndexObjects: a changeset of the empty ledger that keeps its
// index stores the index's blob, so its branch is a complete Git object
// graph; a changeset of the empty ledger that changed the index, once
// the main line has moved, reads the empty index as its conflict's base
// although no commit stored it.
func TestEmptyIndexObjects(t *testing.T) {
	m, _ := openLedger(t, nil, "project", "other")
	c := openCS(t, m, "project")
	checkCS(t, m, c.ID, edit("D?a-First.md", doc("D?a", "First", "x\n")))
	cmd := exec.Command("git", "fsck", "--full", "--strict")
	cmd.Env = append(os.Environ(), "GIT_DIR="+m.repoPath("project"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git fsck: %v\n%s", err, out)
	}

	stale := openCS(t, m, "other")
	commit(t, m, "other", []Decision{{Number: 0, Title: "Seed", Body: []byte("x\n")}}, CommitOptions{Author: "a", Message: "seed"})
	res := checkCS(t, m, stale.ID, edit("INDEX.md", []byte("changed\n")))
	if len(res.Conflicts) != 1 || res.Conflicts[0].Path != indexName || !res.Conflicts[0].Base.Exists ||
		!bytes.Equal(res.Conflicts[0].Base.Content, RenderIndex(nil)) || !hasProblem(res.Problems, indexName, "generated") {
		t.Fatalf("check = %+v", res)
	}
}
