package ledger

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/EricWvi/circus/internal/store"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// doc renders a decision file the way an agent writes one.
func doc(number, title, body string) []byte {
	return []byte("---\nnumber: " + number + "\ntitle: " + title + "\n---\n\n" + body)
}

func edit(p string, content []byte) Change { return Change{Path: p, Content: content} }

func remove(p string) Change { return Change{Path: p, Deleted: true} }

var changesetSeq int

func openCS(t *testing.T, m *Manager, project string) store.LedgerChangeset {
	t.Helper()
	changesetSeq++
	c, err := m.OpenChangeset(t.Context(), project, fmt.Sprintf("cs-%03d", changesetSeq), "worker-1")
	check(t, err)
	return c
}

func checkCS(t *testing.T, m *Manager, id string, changes ...Change) CheckResult {
	t.Helper()
	res, err := m.CheckChangeset(t.Context(), id, "opener", changes)
	check(t, err)
	return res
}

func allow(context.Context, bool) error { return nil }

var errNotIntegrator = errors.New("integrator required")

// opener lets only fast-forward merges through, as for a worker without
// the integrator grant.
func opener(_ context.Context, fastForward bool) error {
	if !fastForward {
		return errNotIntegrator
	}
	return nil
}

func mergeCS(t *testing.T, m *Manager, id string, o MergeOptions) store.LedgerChangeset {
	t.Helper()
	if o.Author == "" {
		o.Author = "merger"
	}
	if o.Authorize == nil {
		o.Authorize = allow
	}
	c, err := m.MergeChangeset(t.Context(), id, o)
	check(t, err)
	if c.State != store.LedgerChangesetMerged || !store.IsLedgerCommitID(c.Merge.Commit) {
		t.Fatalf("merged changeset = %+v", c)
	}
	return c
}

func mainFiles(t *testing.T, m *Manager, project string) map[string]string {
	t.Helper()
	r, err := m.open(m.repoPath(project))
	check(t, err)
	defer r.storage.Close()
	head, err := r.head()
	check(t, err)
	c, err := r.commit(head)
	check(t, err)
	out := make(map[string]string)
	check(t, r.walkFiles(t.Context(), c.TreeHash, "", func(f fileObject) error {
		data, err := r.blob(f.hash, maxIndexBytes, nil)
		out[f.path] = string(data)
		return err
	}))
	return out
}

func preconditions(t *testing.T, err error) []Violation {
	t.Helper()
	var pe *PreconditionError
	if !errors.As(err, &pe) {
		t.Fatalf("error = %v, want a precondition failure", err)
	}
	return pe.Violations
}

func hasProblem(problems []Problem, p, fragment string) bool {
	return slices.ContainsFunc(problems, func(x Problem) bool { return x.Path == p && strings.Contains(x.Message, fragment) })
}

func TestChangesetFastForwardMergeOfAnEmptyLedger(t *testing.T) {
	ctx := t.Context()
	m, db := openLedger(t, func() time.Time { return baseTime }, "project")
	c := openCS(t, m, "project")
	if c.BaseCommit != "" || c.State != store.LedgerChangesetOpen {
		t.Fatalf("opened changeset = %+v", c)
	}
	first := doc("D?a", "First choice", "Builds on D?b, not D?zz or xD?b.\n")
	second := doc(`"D?b"`, "Second D?a", "See D?a and circus#D?a.\n")
	pathA, pathB := "D?a-First-choice.md", "core/D?b-Second-D-a.md"
	res := checkCS(t, m, c.ID, edit(pathA, first), edit(pathB, second))
	if len(res.Problems) != 0 || !res.FastForward || res.MainCommit != "" || len(res.Conflicts) != 0 {
		t.Fatalf("check = %+v", res)
	}
	code := strings.Repeat("c", 40)
	merged := mergeCS(t, m, c.ID, MergeOptions{CodeCommit: code, Authorize: opener})
	if !maps.Equal(merged.Merge.Numbers, map[string]uint32{"a": 0, "b": 1}) || merged.Merge.CodeCommit != code || merged.Merge.Parent != "" {
		t.Fatalf("merge = %+v", merged.Merge)
	}
	files := mainFiles(t, m, "project")
	want := map[string]string{
		"D0-First-choice.md":   "---\nnumber: D0\ntitle: \"First choice\"\n---\n\nBuilds on D1, not D?zz or xD?b.\n",
		"core/D1-Second-D0.md": "---\nnumber: D1\ntitle: \"Second D0\"\n---\n\nSee D0 and circus#D0.\n",
		"INDEX.md":             string(renderIndex([]store.LedgerEntry{{Number: 0, Title: "First choice"}, {Number: 1, Dir: "core", Title: "Second D0"}})),
	}
	if !maps.Equal(files, want) {
		t.Fatalf("main files = %q", files)
	}
	index, err := db.GetLedgerIndex(ctx, "project")
	check(t, err)
	if index.Head != merged.Merge.Commit || len(index.Entries) != 2 || index.NextNumber != 2 {
		t.Fatalf("index = %+v", index)
	}
	history, _, err := db.ListLedgerHistory(ctx, "project", 1, "", 10)
	check(t, err)
	if len(history) != 1 || history[0].CodeCommit != code || history[0].Author != "merger" || history[0].Summary != "Merge changeset "+c.ID {
		t.Fatalf("history = %+v", history)
	}
	// Merging again returns the same result and changes nothing.
	again := mergeCS(t, m, c.ID, MergeOptions{CodeCommit: strings.Repeat("d", 40)})
	if !maps.Equal(again.Merge.Numbers, merged.Merge.Numbers) || again.Merge.Commit != merged.Merge.Commit || again.Merge.CodeCommit != code {
		t.Fatalf("second merge = %+v, first %+v", again, merged)
	}
	if head, err := db.GetLedgerHead(ctx, "project"); err != nil || head != merged.Merge.Commit {
		t.Fatalf("head after second merge = %q, %v", head, err)
	}
	if _, err := m.CheckChangeset(ctx, c.ID, "opener", nil); preconditions(t, err)[0].Type != ViolationState {
		t.Fatalf("check of a merged changeset = %v", err)
	}
}

// seedLedger commits decisions D1 (root) and D2 (core) on main.
func seedLedger(t *testing.T, m *Manager, project string) {
	t.Helper()
	commit(t, m, project, []Decision{
		{Number: 1, Title: "One", Body: []byte("one\n")},
		{Number: 2, Dir: "core", Title: "Two", Body: []byte("two\n")},
	}, CommitOptions{Author: "import", Message: "Seed"})
}

func TestChangesetValidation(t *testing.T) {
	m, _ := openLedger(t, nil, "project")
	seedLedger(t, m, "project")
	for _, tc := range []struct {
		name    string
		changes []Change
		path    string
		problem string
	}{
		{"edit in place", []Change{edit("D1-One.md", doc("D1", "One", "edited\n"))}, "", ""},
		{"move", []Change{remove("core/D2-Two.md"), edit("other/D2-Two.md", doc("D2", "Two", "two\n"))}, "", ""},
		{"retitle", []Change{remove("D1-One.md"), edit("D1-Uno.md", doc("D1", "Uno", "one\n"))}, "", ""},
		{"extra front matter", []Change{edit("D1-One.md", []byte("---\nnumber: D1\ntitle: One\nstatus: done\n---\n\nx\n"))}, "D1-One.md", "closing front matter"},
		{"missing title", []Change{edit("D1-One.md", []byte("---\nnumber: D1\n---\n\nx\n"))}, "D1-One.md", "title"},
		{"number changed", []Change{edit("D1-One.md", doc("D3", "One", "x\n"))}, "D1-One.md", "cannot change; it is D1"},
		{"existing to temporary", []Change{edit("D1-One.md", doc("D?a", "One", "x\n"))}, "D1-One.md", "cannot change; it is D1"},
		{"removed", []Change{remove("core/D2-Two.md")}, "core/D2-Two.md", "cannot be removed"},
		{"index", []Change{edit("INDEX.md", []byte("x"))}, "INDEX.md", "generated"},
		{"index removed", []Change{remove("INDEX.md")}, "INDEX.md", "generated"},
		{"other file", []Change{edit("notes.txt", []byte("x"))}, "notes.txt", "unexpected file"},
		{"new with a number", []Change{edit("D7-New.md", doc("D7", "New", "x\n"))}, "D7-New.md", "temporary number"},
		{"copied number", []Change{edit("x/D1-One.md", doc("D1", "One", "x\n"))}, "x/D1-One.md", "also occurs at D1-One.md"},
		{"moved twice", []Change{remove("D1-One.md"), edit("a/D1-One.md", doc("D1", "One", "x\n")), edit("b/D1-One.md", doc("D1", "One", "x\n"))}, "b/D1-One.md", "also occurs at a/D1-One.md"},
		{"temporary twice", []Change{edit("D?a-New.md", doc("D?a", "New", "x\n")), edit("x/D?a-New.md", doc("D?a", "New", "x\n"))}, "x/D?a-New.md", "also occurs at D?a-New.md"},
		{"temporary too long", []Change{edit("D?abcde-New.md", doc("D?abcde", "New", "x\n"))}, "D?abcde-New.md", "one to four lowercase letters"},
		{"path mismatch", []Change{edit("D?a-Old.md", doc("D?a", "New", "x\n"))}, "D?a-Old.md", "expected D?a-New.md"},
		{"bad directory", []Change{edit(".hidden/D?a-New.md", doc("D?a", "New", "x\n"))}, ".hidden/D?a-New.md", "directory segments"},
		{"too large", []Change{edit("D1-One.md", doc("D1", "One", strings.Repeat("x", MaxDecisionFileBytes)))}, "D1-One.md", "exceeds 256 KiB"},
		{"multi-line title", []Change{edit("D?a-New.md", doc("D?a", `"New\nline"`, "x\n"))}, "D?a-New.md", "single UTF-8 line"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := openCS(t, m, "project")
			res := checkCS(t, m, c.ID, tc.changes...)
			if tc.problem == "" {
				if len(res.Problems) != 0 {
					t.Fatalf("problems = %+v", res.Problems)
				}
				return
			}
			if !hasProblem(res.Problems, tc.path, tc.problem) {
				t.Fatalf("problems = %+v, want %q at %s", res.Problems, tc.problem, tc.path)
			}
			// The merge validates again and refuses.
			_, err := m.MergeChangeset(t.Context(), c.ID, MergeOptions{Author: "merger", Authorize: allow})
			if v := preconditions(t, err); !slices.ContainsFunc(v, func(v Violation) bool {
				return v.Type == ViolationProblem && v.Subject == tc.path && strings.Contains(v.Description, tc.problem)
			}) {
				t.Fatalf("merge violations = %+v", v)
			}
			_, err = m.DiscardChangeset(t.Context(), c.ID, "invalid", "opener")
			check(t, err)
		})
	}
}

func TestChangesetRequestErrors(t *testing.T) {
	m, _ := openLedger(t, nil, "project")
	seedLedger(t, m, "project")
	c := openCS(t, m, "project")
	for _, tc := range []struct {
		name    string
		changes []Change
		field   string
	}{
		{"absolute", []Change{edit("/D1-One.md", nil)}, "changes[0].path"},
		{"dot dot", []Change{edit("a/../D1-One.md", nil)}, "changes[0].path"},
		{"empty segment", []Change{edit("a//D1-One.md", nil)}, "changes[0].path"},
		{"control", []Change{edit("a\x01.md", nil)}, "changes[0].path"},
		{"twice", []Change{edit("D1-One.md", nil), remove("D1-One.md")}, "changes[1].path"},
		{"remove a missing file", []Change{remove("D9-Nine.md")}, "changes[0].path"},
		{"file and directory", []Change{edit("D1-One.md/D?a-x.md", doc("D?a", "x", ""))}, "changes"},
		{"deleted with content", []Change{{Path: "D1-One.md", Deleted: true, Content: []byte("x")}}, "changes[0]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := m.CheckChangeset(t.Context(), c.ID, "opener", tc.changes)
			var re *RequestError
			if !errors.As(err, &re) || re.Field != tc.field {
				t.Fatalf("error = %v, want a request error for %s", err, tc.field)
			}
		})
	}
	var large []Change
	for i := range 33 {
		large = append(large, edit(fmt.Sprintf("D?%c-x.md", 'a'+i%26)+strings.Repeat("y", i), make([]byte, 256<<10)))
	}
	var le *LimitError
	if _, err := m.CheckChangeset(t.Context(), c.ID, "opener", large); !errors.As(err, &le) {
		t.Fatalf("oversized changeset = %v", err)
	}
	many := make([]Change, MaxChanges+1)
	for i := range many {
		many[i] = edit(fmt.Sprintf("f%d", i), nil)
	}
	if _, err := m.CheckChangeset(t.Context(), c.ID, "opener", many); !errors.As(err, &le) {
		t.Fatalf("too many changes = %v", err)
	}
	if _, err := m.CheckChangeset(t.Context(), "missing", "opener", nil); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown changeset = %v", err)
	}
}

func branchHead(t *testing.T, m *Manager, project, id string) plumbing.Hash {
	t.Helper()
	r, err := m.open(m.repoPath(project))
	check(t, err)
	defer r.storage.Close()
	h, err := r.readRef(branchRef(id))
	check(t, err)
	return h
}

func TestChangesetCheckReplacesContent(t *testing.T) {
	var clock atomic.Int64
	clock.Store(baseTime.UnixMilli())
	m, _ := openLedger(t, func() time.Time { return time.UnixMilli(clock.Load()) }, "project")
	seedLedger(t, m, "project")
	c := openCS(t, m, "project")
	one := edit("D1-One.md", doc("D1", "One", "edited\n"))
	checkCS(t, m, c.ID, one)
	first := branchHead(t, m, "project", c.ID)
	// A new commit would differ by its time.
	clock.Add(time.Hour.Milliseconds())
	checkCS(t, m, c.ID, one)
	if again := branchHead(t, m, "project", c.ID); again != first {
		t.Fatalf("the same changes moved the branch from %s to %s", first, again)
	}
	// A new check replaces the content: the edit of D1 is gone.
	checkCS(t, m, c.ID, edit("core/D2-Two.md", doc("D2", "Two", "edited\n")))
	merged := mergeCS(t, m, c.ID, MergeOptions{})
	files := mainFiles(t, m, "project")
	if !strings.HasSuffix(files["D1-One.md"], "\none\n") || !strings.HasSuffix(files["core/D2-Two.md"], "\nedited\n") {
		t.Fatalf("main after a replaced check = %q", files)
	}
	if len(merged.Merge.Numbers) != 0 {
		t.Fatalf("numbers = %v", merged.Merge.Numbers)
	}
	// Checking no changes drops the branch, and nothing is left to merge.
	d := openCS(t, m, "project")
	checkCS(t, m, d.ID, one)
	checkCS(t, m, d.ID, edit("D1-One.md", []byte(files["D1-One.md"])))
	if h := branchHead(t, m, "project", d.ID); !h.IsZero() {
		t.Fatalf("branch of an unchanged changeset = %s", h)
	}
	_, err := m.MergeChangeset(t.Context(), d.ID, MergeOptions{Author: "merger", Authorize: allow})
	if v := preconditions(t, err); v[0].Subject != "." || !strings.Contains(v[0].Description, "no changes") {
		t.Fatalf("merge of no changes = %+v", v)
	}
}

func TestChangesetConflicts(t *testing.T) {
	ctx := t.Context()
	m, _ := openLedger(t, nil, "project")
	seedLedger(t, m, "project")
	a, b := openCS(t, m, "project"), openCS(t, m, "project")
	checkCS(t, m, a.ID, edit("D1-One.md", doc("D1", "One", "from a\n")), edit("D?a-From-a.md", doc("D?a", "From a", "a\n")))
	resB := checkCS(t, m, b.ID, edit("D1-One.md", doc("D1", "One", "from b\n")), edit("core/D2-Two.md", doc("D2", "Two", "b only\n")),
		edit("D?a-From-b.md", doc("D?a", "From b", "refers to D?a\n")))
	if !resB.FastForward || len(resB.Conflicts) != 0 {
		t.Fatalf("check before the first merge = %+v", resB)
	}
	mergedA := mergeCS(t, m, a.ID, MergeOptions{Authorize: opener})
	if mergedA.Merge.Numbers["a"] != 3 {
		t.Fatalf("numbers of a = %v", mergedA.Merge.Numbers)
	}

	resB = checkCS(t, m, b.ID, edit("D1-One.md", doc("D1", "One", "from b\n")), edit("core/D2-Two.md", doc("D2", "Two", "b only\n")),
		edit("D?a-From-b.md", doc("D?a", "From b", "refers to D?a\n")))
	if resB.FastForward || resB.MainCommit != mergedA.Merge.Commit || len(resB.Conflicts) != 1 {
		t.Fatalf("check after the first merge = %+v", resB)
	}
	cf := resB.Conflicts[0]
	if cf.Path != "D1-One.md" || !cf.Base.Exists || !cf.Changeset.Exists || !cf.Main.Exists ||
		!strings.HasSuffix(string(cf.Base.Content), "\none\n") || string(cf.Changeset.Content) != string(doc("D1", "One", "from b\n")) ||
		!strings.HasSuffix(string(cf.Main.Content), "\nfrom a\n") {
		t.Fatalf("conflict = %+v", cf)
	}

	// Only an integrator may merge it, and only with the conflict resolved.
	if _, err := m.MergeChangeset(ctx, b.ID, MergeOptions{Author: "b", Authorize: opener}); !errors.Is(err, errNotIntegrator) {
		t.Fatalf("merge without the grant = %v", err)
	}
	_, err := m.MergeChangeset(ctx, b.ID, MergeOptions{Author: "integrator", Authorize: allow})
	if v := preconditions(t, err); len(v) != 1 || v[0].Type != ViolationConflict || v[0].Subject != "D1-One.md" {
		t.Fatalf("unresolved merge = %+v", v)
	}
	_, err = m.MergeChangeset(ctx, b.ID, MergeOptions{Author: "integrator", Authorize: allow,
		Resolved: []Change{edit("D1-One.md", doc("D1", "One", "from a and b\n")), edit("core/D2-Two.md", nil)}})
	if v := preconditions(t, err); len(v) != 1 || v[0].Subject != "core/D2-Two.md" || !strings.Contains(v[0].Description, "not a conflicting file") {
		t.Fatalf("resolution of a file without a conflict = %+v", v)
	}
	mergedB := mergeCS(t, m, b.ID, MergeOptions{Author: "integrator",
		Resolved: []Change{edit("D1-One.md", doc("D1", "One", "from a and b\n"))}})
	if mergedB.Merge.Numbers["a"] != 4 || mergedB.Merge.Parent != mergedA.Merge.Commit {
		t.Fatalf("merge of b = %+v", mergedB.Merge)
	}
	files := mainFiles(t, m, "project")
	if !strings.HasSuffix(files["D1-One.md"], "\nfrom a and b\n") || !strings.HasSuffix(files["core/D2-Two.md"], "\nb only\n") ||
		files["D3-From-a.md"] == "" || !strings.HasSuffix(files["D4-From-b.md"], "\nrefers to D4\n") {
		t.Fatalf("main after both merges = %q", files)
	}
}

func TestChangesetBehindMainWithDisjointFilesNeedsIntegrator(t *testing.T) {
	m, _ := openLedger(t, nil, "project")
	seedLedger(t, m, "project")
	a, b := openCS(t, m, "project"), openCS(t, m, "project")
	checkCS(t, m, a.ID, edit("D1-One.md", doc("D1", "One", "a\n")))
	resB := checkCS(t, m, b.ID, edit("core/D2-Two.md", doc("D2", "Two", "b\n")))
	mergeCS(t, m, a.ID, MergeOptions{Authorize: opener})
	resB = checkCS(t, m, b.ID, edit("core/D2-Two.md", doc("D2", "Two", "b\n")))
	if resB.FastForward || len(resB.Conflicts) != 0 || len(resB.Problems) != 0 {
		t.Fatalf("check of a changeset behind main = %+v", resB)
	}
	var sawFastForward []bool
	_, err := m.MergeChangeset(t.Context(), b.ID, MergeOptions{Author: "b", Authorize: func(ctx context.Context, ff bool) error {
		sawFastForward = append(sawFastForward, ff)
		return opener(ctx, ff)
	}})
	if !errors.Is(err, errNotIntegrator) || !slices.Equal(sawFastForward, []bool{false}) {
		t.Fatalf("merge without the grant = %v, fast-forward %v", err, sawFastForward)
	}
	mergeCS(t, m, b.ID, MergeOptions{Author: "integrator"})
	files := mainFiles(t, m, "project")
	if !strings.HasSuffix(files["D1-One.md"], "\na\n") || !strings.HasSuffix(files["core/D2-Two.md"], "\nb\n") {
		t.Fatalf("main = %q", files)
	}
}

func TestChangesetMergeRechecksAgainstMain(t *testing.T) {
	m, _ := openLedger(t, nil, "project")
	seedLedger(t, m, "project")
	a, b := openCS(t, m, "project"), openCS(t, m, "project")
	// Both move D2; after the first, the second's resolution of the old
	// path cannot also keep D2 at its own new path.
	checkCS(t, m, a.ID, remove("core/D2-Two.md"), edit("x/D2-Two.md", doc("D2", "Two", "two\n")))
	checkCS(t, m, b.ID, remove("core/D2-Two.md"), edit("y/D2-Two.md", doc("D2", "Two", "two\n")))
	mergeCS(t, m, a.ID, MergeOptions{})
	_, err := m.MergeChangeset(t.Context(), b.ID, MergeOptions{Author: "integrator", Authorize: allow, Resolved: []Change{remove("core/D2-Two.md")}})
	if v := preconditions(t, err); !slices.ContainsFunc(v, func(v Violation) bool {
		return v.Subject == "y/D2-Two.md" && strings.Contains(v.Description, "also occurs at x/D2-Two.md")
	}) {
		t.Fatalf("merge of a second move = %+v", v)
	}
}

func TestChangesetDiscard(t *testing.T) {
	ctx := t.Context()
	m, db := openLedger(t, nil, "project", "empty")
	seedLedger(t, m, "project")
	c := openCS(t, m, "project")
	checkCS(t, m, c.ID, edit("D1-One.md", doc("D1", "One", "x\n")))
	branch := branchHead(t, m, "project", c.ID)
	discarded, err := m.DiscardChangeset(ctx, c.ID, "not needed\nafter all", "discarder")
	check(t, err)
	if discarded.State != store.LedgerChangesetDiscarded || discarded.DiscardReason != "not needed\nafter all" || !discarded.Archived {
		t.Fatalf("discarded = %+v", discarded)
	}
	r, err := m.open(m.repoPath("project"))
	check(t, err)
	archive, err := r.readRef(discardedRef(c.ID))
	check(t, err)
	ac, err := r.commit(archive)
	check(t, err)
	if ac.Message != "Discard changeset "+c.ID+"\n\nnot needed\nafter all\n" || !slices.Equal(ac.ParentHashes, []plumbing.Hash{branch}) || ac.Author.Name != "discarder" {
		t.Fatalf("archive commit = %+v", ac)
	}
	if h, err := r.readRef(branchRef(c.ID)); err != nil || !h.IsZero() {
		t.Fatalf("branch after discard = %s, %v", h, err)
	}
	_ = r.storage.Close()
	again, err := m.DiscardChangeset(ctx, c.ID, "other", "discarder")
	check(t, err)
	if again.DiscardReason != discarded.DiscardReason {
		t.Fatalf("second discard = %+v", again)
	}
	if _, err := m.MergeChangeset(ctx, c.ID, MergeOptions{Author: "m", Authorize: allow}); preconditions(t, err)[0].Type != ViolationState {
		t.Fatalf("merge of a discarded changeset = %v", err)
	}
	merged := openCS(t, m, "project")
	checkCS(t, m, merged.ID, edit("D1-One.md", doc("D1", "One", "y\n")))
	mergeCS(t, m, merged.ID, MergeOptions{})
	if _, err := m.DiscardChangeset(ctx, merged.ID, "late", "d"); preconditions(t, err)[0].Type != ViolationState {
		t.Fatalf("discard of a merged changeset = %v", err)
	}
	// A changeset of an empty ledger with no repository has nothing to
	// archive.
	e := openCS(t, m, "empty")
	discarded, err = m.DiscardChangeset(ctx, e.ID, "nothing", "d")
	check(t, err)
	if !discarded.Archived {
		t.Fatalf("discard without a repository = %+v", discarded)
	}
	if got, err := db.GetLedgerChangeset(ctx, e.ID); err != nil || got.State != store.LedgerChangesetDiscarded {
		t.Fatalf("stored discard = %+v, %v", got, err)
	}
}

func TestReadChangesetBasePages(t *testing.T) {
	m, _ := openLedger(t, nil, "project", "empty")
	var ds []Decision
	for i := range 20 {
		ds = append(ds, Decision{Number: uint32(i), Dir: []string{"", "a", "a/b", "a-b"}[i%4], Title: fmt.Sprintf("T%d", i), Body: []byte(strings.Repeat("x", 1000))})
	}
	commit(t, m, "project", ds, CommitOptions{Author: "import", Message: "Seed"})
	c := openCS(t, m, "project")
	var paths []string
	after := ""
	for pages := 0; ; pages++ {
		files, more, err := m.ReadChangesetBase(t.Context(), c, after, 3000)
		check(t, err)
		if len(files) == 0 || pages > 30 {
			t.Fatalf("page %d = %d files", pages, len(files))
		}
		for _, f := range files {
			paths = append(paths, f.Path)
		}
		after = files[len(files)-1].Path
		if !more {
			break
		}
	}
	if len(paths) != 21 || !slices.IsSorted(paths) || !slices.Contains(paths, indexName) {
		t.Fatalf("base paths = %q", paths)
	}
	files, more, err := m.ReadChangesetBase(t.Context(), openCS(t, m, "empty"), "", 3000)
	if err != nil || more || len(files) != 0 {
		t.Fatalf("empty base = %v, %v, %v", files, more, err)
	}
}

func TestRewriteTemporary(t *testing.T) {
	numbers := map[string]uint32{"a": 7, "ab": 8, "zzzz": 9}
	for in, want := range map[string]string{
		"D?a":                 "D7",
		"(D?a), D?ab; D?zzzz": "(D7), D8; D9",
		"D?abc D?zzzzz D?b":   "D?abc D?zzzzz D?b",
		"xD?a D?a1 D?a_ D?aB": "xD?a D?a1 D?a_ D?aB",
		"p#D?a.":              "p#D7.",
		"D?":                  "D?",
		"中D?a文":               "中D7文",
	} {
		if got := string(rewriteTemporary([]byte(in), numbers)); got != want {
			t.Errorf("rewrite %q = %q, want %q", in, got, want)
		}
	}
	got, err := assignNumbers(map[string]string{"b": "", "a": "", "aa": "", "z": ""}, 10)
	check(t, err)
	if !maps.Equal(got, map[string]uint32{"a": 10, "b": 11, "z": 12, "aa": 13}) {
		t.Fatalf("assigned = %v", got)
	}
	if _, err := assignNumbers(map[string]string{"a": "", "b": ""}, store.MaxLedgerNumber); err == nil {
		t.Fatal("numbers past the last were assigned")
	}
}

// TestConcurrentChangesetMerges merges two changesets of one project at
// once: for workers without the grant only the first succeeds; for
// integrators both do, one after the other on a linear main line.
func TestConcurrentChangesetMerges(t *testing.T) {
	for _, integrator := range []bool{false, true} {
		t.Run(fmt.Sprintf("integrator=%t", integrator), func(t *testing.T) {
			ctx := t.Context()
			m, db := openLedger(t, nil, "project")
			seedLedger(t, m, "project")
			ids := make([]string, 4)
			for i := range ids {
				c := openCS(t, m, "project")
				ids[i] = c.ID
				checkCS(t, m, c.ID, edit(fmt.Sprintf("d%d/D?a-New.md", i), doc("D?a", "New", fmt.Sprintf("from %d\n", i))))
			}
			authorize := opener
			if integrator {
				authorize = allow
			}
			var wg sync.WaitGroup
			errs := make([]error, len(ids))
			for i, id := range ids {
				wg.Go(func() {
					_, errs[i] = m.MergeChangeset(ctx, id, MergeOptions{Author: "w", Authorize: authorize})
				})
			}
			wg.Wait()
			merged := 0
			for _, err := range errs {
				switch {
				case err == nil:
					merged++
				case !integrator && errors.Is(err, errNotIntegrator):
				default:
					t.Fatalf("merge: %v", err)
				}
			}
			if want := map[bool]int{false: 1, true: len(ids)}[integrator]; merged != want {
				t.Fatalf("%d merges succeeded, want %d: %v", merged, want, errs)
			}
			index, err := db.GetLedgerIndex(ctx, "project")
			check(t, err)
			if len(index.Entries) != 2+merged || index.NextNumber != uint32(3+merged) {
				t.Fatalf("index = %+v", index)
			}
			numbers := map[uint32]bool{}
			for _, id := range ids {
				c, err := db.GetLedgerChangeset(ctx, id)
				check(t, err)
				if c.State == store.LedgerChangesetMerged {
					numbers[c.Merge.Numbers["a"]] = true
				}
			}
			if len(numbers) != merged {
				t.Fatalf("numbers %v for %d merges", numbers, merged)
			}
			checkLinear(t, m, "project", 1+merged)
		})
	}
}

// checkLinear checks that main is a single first-parent line of n commits.
func checkLinear(t *testing.T, m *Manager, project string, n int) {
	t.Helper()
	r, err := m.open(m.repoPath(project))
	check(t, err)
	defer r.storage.Close()
	head, err := r.head()
	check(t, err)
	count := 0
	for h := head; !h.IsZero(); count++ {
		c, err := r.commit(h)
		check(t, err)
		if len(c.ParentHashes) > 1 {
			t.Fatalf("merge commit %s on main", h)
		}
		h = plumbing.ZeroHash
		if len(c.ParentHashes) == 1 {
			h = c.ParentHashes[0]
		}
	}
	if count != n {
		t.Fatalf("main has %d commits, want %d", count, n)
	}
}

// crashAfter fails the named store calls, as a crash right before them
// would leave the store.
type crashAfter struct {
	store.LedgerOps
	fail map[string]bool
}

var errCrash = errors.New("crash")

func (s crashAfter) PrepareLedgerMerge(ctx context.Context, id string, mg store.LedgerMerge, at int64) (bool, error) {
	if s.fail["prepare"] {
		return false, errCrash
	}
	return s.LedgerOps.PrepareLedgerMerge(ctx, id, mg, at)
}

func (s crashAfter) ClearLedgerMerge(ctx context.Context, id, commit string) (bool, error) {
	if s.fail["clear"] {
		return false, errCrash
	}
	return s.LedgerOps.ClearLedgerMerge(ctx, id, commit)
}

func (s crashAfter) FinishLedgerMerge(ctx context.Context, id, commit string, at int64) (bool, error) {
	if s.fail["finish"] {
		return false, errCrash
	}
	return s.LedgerOps.FinishLedgerMerge(ctx, id, commit, at)
}

func (s crashAfter) AdvanceLedgerIndex(ctx context.Context, id, parent, head string, e []store.LedgerEntry, v []store.LedgerVersion) error {
	if s.fail["advance"] {
		return errCrash
	}
	return s.LedgerOps.AdvanceLedgerIndex(ctx, id, parent, head, e, v)
}

func (s crashAfter) ArchiveLedgerChangeset(ctx context.Context, id string) error {
	if s.fail["archive"] {
		return errCrash
	}
	return s.LedgerOps.ArchiveLedgerChangeset(ctx, id)
}

// failPublishAfterPrepare records the merge and then fails, before main
// moves, like a crash between the two.
type failPublishAfterPrepare struct{ store.LedgerOps }

func (s failPublishAfterPrepare) PrepareLedgerMerge(ctx context.Context, id string, mg store.LedgerMerge, at int64) (bool, error) {
	if _, err := s.LedgerOps.PrepareLedgerMerge(ctx, id, mg, at); err != nil {
		return false, err
	}
	return false, errCrash
}

func (s failPublishAfterPrepare) ClearLedgerMerge(context.Context, string, string) (bool, error) {
	return false, errCrash
}

func restart(t *testing.T, m *Manager, st store.LedgerOps) *Manager {
	t.Helper()
	n, err := New(filepath.Join(filepath.Dir(m.root), "db"), st, m.now, nil)
	check(t, err)
	t.Cleanup(func() { check(t, n.Close(context.Background())) })
	check(t, n.Reconcile(t.Context()))
	return n
}

func TestChangesetMergeCrashPoints(t *testing.T) {
	for _, tc := range []struct {
		name string
		// st wraps the store for the interrupted merge.
		st func(store.LedgerOps) store.LedgerOps
		// onMain reports whether the interrupted merge reached main.
		onMain bool
	}{
		{"before main moves", func(st store.LedgerOps) store.LedgerOps { return failPublishAfterPrepare{st} }, false},
		{"after main moves", func(st store.LedgerOps) store.LedgerOps {
			return crashAfter{st, map[string]bool{"finish": true}}
		}, true},
		{"after main moves, before the index", func(st store.LedgerOps) store.LedgerOps {
			return crashAfter{st, map[string]bool{"advance": true, "finish": true}}
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			m, db := openLedger(t, nil, "project")
			seedLedger(t, m, "project")
			c := openCS(t, m, "project")
			checkCS(t, m, c.ID, edit("D?a-New.md", doc("D?a", "New", "new\n")), edit("D1-One.md", doc("D1", "One", "see D?a\n")))
			m.st = tc.st(db)
			if _, err := m.MergeChangeset(ctx, c.ID, MergeOptions{Author: "w", Authorize: allow}); err == nil {
				t.Fatal("interrupted merge succeeded")
			}
			pending, err := db.GetLedgerChangeset(ctx, c.ID)
			check(t, err)
			if !pending.Pending() {
				t.Fatalf("interrupted merge left %+v", pending)
			}
			m.st = db
			n := restart(t, m, db)
			got, err := db.GetLedgerChangeset(ctx, c.ID)
			check(t, err)
			index, err := db.GetLedgerIndex(ctx, "project")
			check(t, err)
			if tc.onMain {
				if got.State != store.LedgerChangesetMerged || got.Merge.Commit != pending.Merge.Commit || index.Head != got.Merge.Commit || len(index.Entries) != 3 {
					t.Fatalf("after restart: %+v, index %+v", got, index)
				}
				again := mergeCS(t, n, c.ID, MergeOptions{})
				if again.Merge.Commit != pending.Merge.Commit || !maps.Equal(again.Merge.Numbers, pending.Merge.Numbers) {
					t.Fatalf("merge after restart = %+v", again)
				}
			} else {
				if got.State != store.LedgerChangesetOpen || got.Pending() || len(index.Entries) != 2 {
					t.Fatalf("after restart: %+v, index %+v", got, index)
				}
				merged := mergeCS(t, n, c.ID, MergeOptions{})
				if merged.Merge.Numbers["a"] != 3 {
					t.Fatalf("merge after restart = %+v", merged)
				}
			}
			files := mainFiles(t, n, "project")
			if !strings.HasSuffix(files["D1-One.md"], "\nsee D3\n") || files["D3-New.md"] == "" {
				t.Fatalf("main = %q", files)
			}
			checkLinear(t, n, "project", 2)
		})
	}
}

// TestChangesetMergeRepairedByNextOperation finishes an interrupted merge
// without a restart, when the project is next used.
func TestChangesetMergeRepairedByNextOperation(t *testing.T) {
	ctx := t.Context()
	m, db := openLedger(t, nil, "project")
	seedLedger(t, m, "project")
	c := openCS(t, m, "project")
	checkCS(t, m, c.ID, edit("D?a-New.md", doc("D?a", "New", "new\n")))
	m.st = crashAfter{db, map[string]bool{"finish": true}}
	if _, err := m.MergeChangeset(ctx, c.ID, MergeOptions{Author: "w", Authorize: allow}); !errors.Is(err, errCrash) {
		t.Fatalf("interrupted merge = %v", err)
	}
	m.st = db
	other := openCS(t, m, "project")
	if got, err := db.GetLedgerChangeset(ctx, c.ID); err != nil || got.State != store.LedgerChangesetMerged {
		t.Fatalf("after the next open: %+v, %v", got, err)
	}
	if other.BaseCommit == "" {
		t.Fatal("next changeset has no base")
	}
}

func TestChangesetArchiveRepaired(t *testing.T) {
	ctx := t.Context()
	m, db := openLedger(t, nil, "project")
	seedLedger(t, m, "project")
	c := openCS(t, m, "project")
	checkCS(t, m, c.ID, edit("D1-One.md", doc("D1", "One", "x\n")))
	m.st = crashAfter{db, map[string]bool{"archive": true}}
	discarded, err := m.DiscardChangeset(ctx, c.ID, "gone", "d")
	check(t, err)
	if discarded.State != store.LedgerChangesetDiscarded || discarded.Archived {
		t.Fatalf("discard with a failed archive = %+v", discarded)
	}
	m.st = db
	restart(t, m, db)
	got, err := db.GetLedgerChangeset(ctx, c.ID)
	check(t, err)
	if !got.Archived {
		t.Fatalf("after restart: %+v", got)
	}
	if h := branchHead(t, m, "project", c.ID); !h.IsZero() {
		t.Fatalf("branch after the repaired archive = %s", h)
	}
}

func TestChangesetsOfAnEmptyLedgerKeepReadsQuiet(t *testing.T) {
	ctx := t.Context()
	m, db := openLedger(t, nil, "project")
	c := openCS(t, m, "project")
	checkCS(t, m, c.ID, edit("D?a-New.md", doc("D?a", "New", "x\n")))
	// The repository exists for the branch but has no main line: reads
	// find no stale index to rebuild.
	check(t, m.CheckProject(ctx, "project"))
	m.mu.Lock()
	broken := len(m.broken)
	m.mu.Unlock()
	if broken != 0 {
		t.Fatalf("%d projects marked for a check", broken)
	}
	if head, err := db.GetLedgerHead(ctx, "project"); err != nil || head != "" {
		t.Fatalf("head = %q, %v", head, err)
	}
	check(t, m.Reconcile(ctx))
	mergeCS(t, m, c.ID, MergeOptions{Authorize: opener})
	if files := mainFiles(t, m, "project"); files["D0-New.md"] == "" {
		t.Fatalf("main = %q", files)
	}
}

func TestChangesetOpenLimit(t *testing.T) {
	m, _ := openLedger(t, nil, "project")
	for range store.MaxOpenLedgerChangesets {
		openCS(t, m, "project")
	}
	if _, err := m.OpenChangeset(t.Context(), "project", "one-more", "w"); !errors.Is(err, store.ErrLimit) {
		t.Fatalf("open beyond the limit = %v", err)
	}
	if _, err := m.OpenChangeset(t.Context(), "missing", "x", "w"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("open of a missing project = %v", err)
	}
}

// TestChangesetConflictContentsAreBounded reports conflicts past
// MaxConflictBytes without their contents.
func TestChangesetConflictContentsAreBounded(t *testing.T) {
	m, _ := openLedger(t, nil, "project")
	const files = 12
	body := strings.Repeat("x", 240<<10)
	var ds []Decision
	for i := range files {
		ds = append(ds, Decision{Number: uint32(i), Title: fmt.Sprintf("T%d", i), Body: []byte(body)})
	}
	commit(t, m, "project", ds, CommitOptions{Author: "import", Message: "Seed"})
	edits := func(text string) []Change {
		var out []Change
		for i := range files {
			out = append(out, edit(fmt.Sprintf("D%d-T%d.md", i, i), doc(fmt.Sprintf("D%d", i), fmt.Sprintf("T%d", i), text+body)))
		}
		return out
	}
	a, b := openCS(t, m, "project"), openCS(t, m, "project")
	checkCS(t, m, a.ID, edits("a")...)
	mergeCS(t, m, a.ID, MergeOptions{})
	res := checkCS(t, m, b.ID, edits("b")...)
	if len(res.Conflicts) != files {
		t.Fatalf("%d conflicts", len(res.Conflicts))
	}
	used, shown := 0, 0
	for i, cf := range res.Conflicts {
		if !cf.Omitted {
			if shown != i || cf.Base.Content == nil || cf.Main.Content == nil || cf.Changeset.Content == nil {
				t.Fatalf("conflict %d = %+v", i, cf.Path)
			}
			shown++
			used += EncodedBytes(cf.Base.Content) + EncodedBytes(cf.Changeset.Content) + EncodedBytes(cf.Main.Content)
			continue
		}
		if cf.Base.Content != nil || cf.Main.Content != nil || cf.Changeset.Content != nil || !cf.Base.Exists || !cf.Main.Exists {
			t.Fatalf("omitted conflict %d = %+v", i, cf.Path)
		}
	}
	if shown == 0 || shown == files || used > MaxConflictBytes {
		t.Fatalf("%d of %d conflicts shown with %d bytes", shown, files, used)
	}
}

// behindOnD1 merges one edit of D1 and leaves a second, from the same
// base, behind the main line with D1 in conflict.
func behindOnD1(t *testing.T, m *Manager) (merged, behind store.LedgerChangeset) {
	t.Helper()
	seedLedger(t, m, "project")
	a, b := openCS(t, m, "project"), openCS(t, m, "project")
	checkCS(t, m, a.ID, edit("D1-One.md", doc("D1", "One", "a\n")))
	checkCS(t, m, b.ID, edit("D1-One.md", doc("D1", "One", "b\n")))
	return mergeCS(t, m, a.ID, MergeOptions{}), b
}

func TestChangesetInvalidResolutions(t *testing.T) {
	for name, tc := range map[string]struct {
		resolution Change
		subject    string
		problem    string
	}{
		"garbage":           {edit("D1-One.md", []byte("garbage\n")), "D1-One.md", "front matter"},
		"removed":           {remove("D1-One.md"), "D1-One.md", "cannot be removed"},
		"other number":      {edit("D1-One.md", doc("D9", "One", "x\n")), "D1-One.md", "cannot change; it is D1"},
		"retitled":          {edit("D1-One.md", doc("D1", "Another", "x\n")), "D1-One.md", "expected D1-Another.md"},
		"too large":         {edit("D1-One.md", doc("D1", "One", strings.Repeat("x", 300<<10))), "D1-One.md", "exceeds 256 KiB"},
		"temporary":         {edit("D1-One.md", doc("D?q", "One", "x\n")), "D1-One.md", "cannot change; it is D1"},
		"control character": {edit("D1-One.md", doc("D1", `"One\u0001"`, "x\n")), "D1-One.md", "without control characters"},
	} {
		t.Run(name, func(t *testing.T) {
			m, _ := openLedger(t, nil, "project")
			merged, behind := behindOnD1(t, m)
			_, err := m.MergeChangeset(t.Context(), behind.ID, MergeOptions{Author: "i", Authorize: allow, Resolved: []Change{tc.resolution}})
			if v := preconditions(t, err); !slices.ContainsFunc(v, func(v Violation) bool {
				return v.Type == ViolationProblem && v.Subject == tc.subject && strings.Contains(v.Description, tc.problem)
			}) {
				t.Fatalf("violations = %+v, want %q at %s", v, tc.problem, tc.subject)
			}
			if files := mainFiles(t, m, "project"); !strings.HasSuffix(files["D1-One.md"], "\na\n") || len(files) != 3 {
				t.Fatalf("main after a refused resolution = %q", files)
			}
			if c, err := m.st.GetLedgerChangeset(t.Context(), behind.ID); err != nil || c.State != store.LedgerChangesetOpen || c.Pending() {
				t.Fatalf("refused changeset = %+v, %v", c, err)
			}
			_ = merged
		})
	}
}

func TestChangesetNothingToMerge(t *testing.T) {
	m, _ := openLedger(t, nil, "project")
	merged, behind := behindOnD1(t, m)
	_, err := m.MergeChangeset(t.Context(), behind.ID, MergeOptions{Author: "i", Authorize: allow,
		Resolved: []Change{edit("D1-One.md", doc("D1", "One", "a\n"))}})
	if v := preconditions(t, err); len(v) != 1 || v[0].Subject != "." || !strings.Contains(v[0].Description, "nothing to merge") {
		t.Fatalf("merge of nothing = %+v", v)
	}
	checkLinear(t, m, "project", 2)
	if head, err := m.st.GetLedgerHead(t.Context(), "project"); err != nil || head != merged.Merge.Commit {
		t.Fatalf("head = %s, %v", head, err)
	}
}

// TestReadChangesetBaseLargeFile: a file larger than the page budget comes
// alone in its page, which still leads on to the rest.
func TestReadChangesetBaseLargeFile(t *testing.T) {
	m, _ := openLedger(t, nil, "project")
	commit(t, m, "project", []Decision{
		{Number: 0, Title: "Big", Body: []byte(strings.Repeat("x", 5000))},
		{Number: 1, Title: "Small", Body: []byte("y")},
	}, CommitOptions{Author: "import", Message: "Seed"})
	c := openCS(t, m, "project")
	var pages [][]string
	for after := ""; ; {
		files, more, err := m.ReadChangesetBase(t.Context(), c, after, 1000)
		check(t, err)
		var page []string
		for _, f := range files {
			page = append(page, f.Path)
		}
		pages = append(pages, page)
		if !more {
			break
		}
		if len(files) == 0 || len(pages) > 5 {
			t.Fatalf("pages %q", pages)
		}
		after = files[len(files)-1].Path
	}
	if fmt.Sprint(pages) != "[[D0-Big.md] [D1-Small.md INDEX.md]]" {
		t.Fatalf("pages %q", pages)
	}
}

// TestEncodedBytes matches the size protojson gives a string, which the
// response budgets rely on, for every ASCII byte and some UTF-8.
func TestEncodedBytes(t *testing.T) {
	var all []byte
	for c := range 128 {
		all = append(all, byte(c))
	}
	for _, s := range []string{"", "plain", string(all), "中文 \"\\\x00\x1f\x7f", strings.Repeat("\x01", 100)} {
		encoded, err := protojson.Marshal(wrapperspb.String(s))
		check(t, err)
		if got := EncodedBytes([]byte(s)); got != len(encoded) {
			t.Errorf("EncodedBytes(%q) = %d, protojson %d", s, got, len(encoded))
		}
	}
}

// numberStore answers a fixed index counter.
type numberStore struct {
	store.LedgerOps
	next uint32
}

func (s numberStore) GetLedgerNextNumber(context.Context, string) (uint32, error) { return s.next, nil }

// TestChangesetNextNumber assigns numbers from the larger of the index's
// counter and the main line's highest number plus one.
func TestChangesetNextNumber(t *testing.T) {
	for _, tc := range []struct {
		counter, want uint32
	}{{0, 3}, {2, 3}, {40, 40}} {
		t.Run(fmt.Sprint(tc.counter), func(t *testing.T) {
			m, db := openLedger(t, nil, "project")
			seedLedger(t, m, "project")
			m.st = numberStore{db, tc.counter}
			c := openCS(t, m, "project")
			checkCS(t, m, c.ID, edit("D?a-New.md", doc("D?a", "New", "x\n")))
			if merged := mergeCS(t, m, c.ID, MergeOptions{}); merged.Merge.Numbers["a"] != tc.want {
				t.Fatalf("numbers %v, want D%d", merged.Merge.Numbers, tc.want)
			}
		})
	}
}

// stuckMerge cannot record the outcome of the named changeset's merge.
type stuckMerge struct {
	store.LedgerOps
	id string
}

func (s stuckMerge) FinishLedgerMerge(ctx context.Context, id, commit string, at int64) (bool, error) {
	if id == s.id {
		return false, errCrash
	}
	return s.LedgerOps.FinishLedgerMerge(ctx, id, commit, at)
}

// TestChangesetRepairFailureIsolated: a changeset whose interrupted merge
// cannot be repaired refuses its own operations only.
func TestChangesetRepairFailureIsolated(t *testing.T) {
	ctx := t.Context()
	m, db := openLedger(t, nil, "project")
	seedLedger(t, m, "project")
	stuck := openCS(t, m, "project")
	checkCS(t, m, stuck.ID, edit("D?a-Stuck.md", doc("D?a", "Stuck", "x\n")))
	m.st = stuckMerge{db, stuck.ID}
	if _, err := m.MergeChangeset(ctx, stuck.ID, MergeOptions{Author: "w", Authorize: allow}); !errors.Is(err, errCrash) {
		t.Fatalf("interrupted merge = %v", err)
	}
	// Another changeset opens, checks and merges on top of it.
	other := openCS(t, m, "project")
	checkCS(t, m, other.ID, edit("D?a-Other.md", doc("D?a", "Other", "y\n")))
	merged := mergeCS(t, m, other.ID, MergeOptions{})
	if merged.Merge.Numbers["a"] != 4 {
		t.Fatalf("numbers after the stuck merge = %v", merged.Merge.Numbers)
	}
	for name, err := range map[string]error{
		"merge": func() error {
			_, err := m.MergeChangeset(ctx, stuck.ID, MergeOptions{Author: "w", Authorize: allow})
			return err
		}(),
		"check":   func() error { _, err := m.CheckChangeset(ctx, stuck.ID, "w", nil); return err }(),
		"discard": func() error { _, err := m.DiscardChangeset(ctx, stuck.ID, "x", "w"); return err }(),
		"repair": func() error {
			failed, err := m.RepairChangesets(ctx, "project")
			check(t, err)
			if len(failed) != 1 {
				t.Errorf("failed repairs %v", failed)
			}
			return failed[stuck.ID]
		}(),
	} {
		var repairErr *RepairError
		if !errors.Is(err, errCrash) || !errors.As(err, &repairErr) || repairErr.ID != stuck.ID {
			t.Errorf("%s of the stuck changeset = %v", name, err)
		}
	}
	// Once its store recovers, the stuck merge is found on main.
	m.st = db
	if failed, err := m.RepairChangesets(ctx, "project"); err != nil || len(failed) != 0 {
		t.Fatalf("repair after recovery = %v, %v", failed, err)
	}
	if c, err := db.GetLedgerChangeset(ctx, stuck.ID); err != nil || c.State != store.LedgerChangesetMerged || c.Merge.Numbers["a"] != 3 {
		t.Fatalf("repaired = %+v, %v", c, err)
	}
	checkLinear(t, m, "project", 3)
}

func TestChangesetWarnings(t *testing.T) {
	m, _ := openLedger(t, nil, "project")
	seedLedger(t, m, "project")
	c := openCS(t, m, "project")
	res := checkCS(t, m, c.ID, edit("D1-One.md", doc("D1", "One", "See D?a, D?b and D?b again; not xD?c.\n")),
		edit("D?a-New.md", doc("D?a", "New", "x\n")))
	if len(res.Problems) != 0 || len(res.Warnings) != 1 || res.Warnings[0].Path != "D1-One.md" || !strings.Contains(res.Warnings[0].Message, "D?b") {
		t.Fatalf("check = %+v", res)
	}
	mergeCS(t, m, c.ID, MergeOptions{})
	if files := mainFiles(t, m, "project"); !strings.HasSuffix(files["D1-One.md"], "\nSee D3, D?b and D?b again; not xD?c.\n") {
		t.Fatalf("main = %q", files)
	}
}

func countChangesets(t *testing.T, m *Manager, project string) int {
	t.Helper()
	page, _, err := m.st.ListLedgerChangesets(t.Context(), store.LedgerChangesetFilter{ProjectID: project, Limit: store.MaxLedgerChangesetPage})
	check(t, err)
	return len(page)
}

// TestReadChangesetConflicts pages through the conflicts of the last
// check with their contents, from the branch and the main line the check
// compared with, and refuses once the main line moved.
func TestReadChangesetConflicts(t *testing.T) {
	ctx := t.Context()
	m, _ := openLedger(t, nil, "project")
	seedLedger(t, m, "project")
	a, b := openCS(t, m, "project"), openCS(t, m, "project")
	checkCS(t, m, a.ID, edit("D1-One.md", doc("D1", "One", "a\n")), edit("core/D2-Two.md", doc("D2", "Two", "a\n")))
	res := checkCS(t, m, b.ID, edit("D1-One.md", doc("D1", "One", "b\n")), remove("core/D2-Two.md"),
		edit("x/D2-Two.md", doc("D2", "Two", "moved\n")), edit("D?a-New.md", doc("D?a", "New", "new\n")))
	if page, err := m.ReadChangesetConflicts(ctx, b.ID, res.MainCommit, "", "", 1<<20); err != nil || page.More || len(page.Conflicts) != 0 {
		t.Fatalf("conflicts of a fast-forward = %+v, %v", page, err)
	}
	merged := mergeCS(t, m, a.ID, MergeOptions{})
	// The local copy may change after the check: the branch is what counts.
	res = checkCS(t, m, b.ID, edit("D1-One.md", doc("D1", "One", "b\n")), remove("core/D2-Two.md"),
		edit("x/D2-Two.md", doc("D2", "Two", "moved\n")), edit("D?a-New.md", doc("D?a", "New", "new\n")))
	if res.MainCommit != merged.Merge.Commit || len(res.Conflicts) != 2 {
		t.Fatalf("check = %+v", res)
	}
	before := countChangesets(t, m, "project")
	var got []Conflict
	var branch string
	for after, pages := "", 0; ; pages++ {
		page, err := m.ReadChangesetConflicts(ctx, b.ID, res.MainCommit, branch, after, 10)
		check(t, err)
		if len(page.Conflicts) != 1 || pages > 2 || branch != "" && page.Branch != branch {
			t.Fatalf("page %d = %+v", pages, page)
		}
		got = append(got, page.Conflicts...)
		after, branch = page.Conflicts[0].Path, page.Branch
		if !page.More {
			break
		}
	}
	if len(got) != 2 || got[0].Path != "D1-One.md" || got[1].Path != "core/D2-Two.md" {
		t.Fatalf("conflicts %+v", got)
	}
	d1, d2 := got[0], got[1]
	if !strings.HasSuffix(string(d1.Base.Content), "\none\n") || string(d1.Changeset.Content) != string(doc("D1", "One", "b\n")) ||
		!strings.HasSuffix(string(d1.Main.Content), "\na\n") || d1.Omitted {
		t.Fatalf("D1 conflict %+v", d1)
	}
	if !d2.Base.Exists || d2.Changeset.Exists || d2.Changeset.Content != nil || !strings.HasSuffix(string(d2.Main.Content), "\na\n") {
		t.Fatalf("D2 conflict %+v", d2)
	}
	if after := countChangesets(t, m, "project"); after != before {
		t.Fatalf("reading conflicts changed the changesets: %d, then %d", before, after)
	}
	// A page read after the changeset was checked again is refused.
	first, err := m.ReadChangesetConflicts(ctx, b.ID, res.MainCommit, "", "", 10)
	check(t, err)
	checkCS(t, m, b.ID, edit("D1-One.md", doc("D1", "One", "b again\n")), remove("core/D2-Two.md"),
		edit("x/D2-Two.md", doc("D2", "Two", "moved\n")))
	_, err = m.ReadChangesetConflicts(ctx, b.ID, res.MainCommit, first.Branch, first.Conflicts[0].Path, 10)
	if v := preconditions(t, err); len(v) != 1 || v[0].Type != ViolationChangesetChanged {
		t.Fatalf("read after another check = %+v", v)
	}
	// Once the main line moves, the check must run again.
	c := openCS(t, m, "project")
	checkCS(t, m, c.ID, edit("D?a-Other.md", doc("D?a", "Other", "x\n")))
	mergeCS(t, m, c.ID, MergeOptions{})
	_, err = m.ReadChangesetConflicts(ctx, b.ID, res.MainCommit, "", "", 1<<20)
	if v := preconditions(t, err); len(v) != 1 || v[0].Type != ViolationMainMoved {
		t.Fatalf("read after the main line moved = %+v", v)
	}
	var request *RequestError
	if _, err := m.ReadChangesetConflicts(ctx, b.ID, "HEAD", "", "", 1<<20); !errors.As(err, &request) || request.Field != "main_commit" {
		t.Fatalf("invalid main commit = %v", err)
	}
	_, err = m.DiscardChangeset(ctx, b.ID, "done", "w")
	check(t, err)
	if _, err := m.ReadChangesetConflicts(ctx, b.ID, res.MainCommit, "", "", 1<<20); preconditions(t, err)[0].Type != ViolationState {
		t.Fatalf("read of a discarded changeset = %v", err)
	}
}

// TestChangesetRetitleKeepingItsPath merges a new title that leaves the
// truncated file name as it was.
func TestChangesetRetitleKeepingItsPath(t *testing.T) {
	m, _ := openLedger(t, nil, "project")
	long := strings.Repeat("Long title ", 7)
	commit(t, m, "project", []Decision{{Number: 1, Title: long + "one", Body: []byte("x\n")}}, CommitOptions{Author: "import", Message: "Seed"})
	p := FileName(1, long+"one")
	if FileName(1, long+"two") != p {
		t.Fatal("the titles do not share a file name")
	}
	c := openCS(t, m, "project")
	checkCS(t, m, c.ID, edit(p, doc("D1", long+"two", "x\n")))
	mergeCS(t, m, c.ID, MergeOptions{})
	if files := mainFiles(t, m, "project"); !strings.Contains(files[p], "title: \""+long+"two\"") {
		t.Fatalf("main = %q", files[p])
	}
}

// TestChangesetNothingToMergeAcrossSpellings: a resolution that differs
// from a main-line file only in how its front matter is spelled changes
// nothing.
func TestChangesetNothingToMergeAcrossSpellings(t *testing.T) {
	ctx := t.Context()
	m, _ := openLedger(t, nil, "project")
	commit(t, m, "project", []Decision{{Number: 1, Title: "One", Body: []byte("one\n")}}, CommitOptions{Author: "import", Message: "Seed"})
	b := openCS(t, m, "project")
	checkCS(t, m, b.ID, edit("D1-One.md", doc("D1", "One", "b\n")))
	// The main line moves to a file spelled as an older import wrote it,
	// with a plain title.
	r, err := m.open(m.repoPath("project"))
	check(t, err)
	parent, err := r.head()
	check(t, err)
	legacy := []byte("---\nnumber: D1\ntitle: One\n---\n\nlegacy\n")
	writeRawSnapshot(t, r, parent, "D1-One.md", filemode.Regular, legacy, RenderIndex([]Decision{{Number: 1, Title: "One"}}))
	check(t, r.storage.Close())
	_, err = m.MergeChangeset(ctx, b.ID, MergeOptions{Author: "i", Authorize: allow,
		Resolved: []Change{edit("D1-One.md", doc("D1", `"One"`, "legacy\n"))}})
	if v := preconditions(t, err); len(v) != 1 || !strings.Contains(v[0].Description, "nothing to merge") {
		t.Fatalf("merge of another spelling = %+v", v)
	}
}

func TestChangesetWarningsAreBounded(t *testing.T) {
	m, _ := openLedger(t, nil, "project")
	seedLedger(t, m, "project")
	var body strings.Builder
	for i := range 150 {
		fmt.Fprintf(&body, "D?%c%c ", 'a'+i/26, 'a'+i%26)
	}
	c := openCS(t, m, "project")
	res := checkCS(t, m, c.ID, edit("D1-One.md", doc("D1", "One", body.String()+"\n")))
	if len(res.Warnings) != MaxWarnings || res.Warnings[MaxWarnings-1].Path != "." || !strings.Contains(res.Warnings[MaxWarnings-1].Message, "51 more") {
		t.Fatalf("%d warnings, last %+v", len(res.Warnings), res.Warnings[len(res.Warnings)-1])
	}
}
