package storetest

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"iter"
	"slices"
	"strings"
	"testing"

	"github.com/EricWvi/circus/internal/store"
)

func testLedger(t *testing.T, b Backend) {
	s, _ := open(t, b)
	f := seed(t, s)
	ctx := t.Context()
	projectID := "session-project"
	first, second, third := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	name, found, err := s.LedgerProjectName(ctx, projectID)
	must(t, err)
	mustEqual(t, "project exists", found, true)
	mustEqual(t, "project name", name, "Sessions")
	_, found, err = s.LedgerProjectName(ctx, "missing")
	must(t, err)
	mustEqual(t, "unknown project exists", found, false)
	index, err := s.GetLedgerIndex(ctx, projectID)
	must(t, err)
	mustEqual(t, "initial index", index, store.LedgerIndex{})
	mustIs(t, s.ReplaceLedgerIndex(ctx, "missing", "", first, false, nil, nil), store.ErrNotFound)

	added := store.LedgerVersion{Number: 12, Commit: first, Added: true, Author: "import", Summary: "Seed",
		Title: "First", Path: "team/D12-First.md", Dir: "team", CommittedAt: t0}
	root := store.LedgerVersion{Number: 0, Commit: first, Added: true, Title: "Root", Path: "D0-Root.md", CommittedAt: t0}
	edited := added
	edited.Position, edited.Commit, edited.CommittedAt = 1, second, t0-1000
	edited.Added, edited.ContentChanged = false, true
	must(t, s.ReplaceLedgerIndex(ctx, projectID, "", second, false, []store.LedgerEntry{ledgerEntry(root), ledgerEntry(edited)}, func(yield func(store.LedgerVersion, error) bool) {
		for _, v := range []store.LedgerVersion{added, root, edited} {
			if !yield(v, nil) {
				return
			}
		}
	}))
	projects, err := s.ListLedgerProjects(ctx, "")
	must(t, err)
	mustEqual(t, "indexed projects", projects, []string{projectID})
	projects, err = s.ListLedgerProjects(ctx, projectID)
	must(t, err)
	mustEqual(t, "exclusive index cursor", len(projects), 0)
	index, err = s.GetLedgerIndex(ctx, projectID)
	must(t, err)
	mustEqual(t, "head", index.Head, second)
	mustEqual(t, "inferred counter", index.NextNumber, uint32(13))
	mustEqual(t, "entries ordered by directory", index.Entries[0].Number, uint32(0))
	mustEqual(t, "last changed commit", index.Entries[1].Commit, second)
	mustEqual(t, "last changed time", index.Entries[1].CommittedAt, edited.CommittedAt)

	// Install a prepared suffix and keep the unchanged prefix intact.
	suffix := edited
	suffix.Position, suffix.Commit = 2, third
	must(t, s.ReplaceLedgerIndex(ctx, projectID, second, third, true, []store.LedgerEntry{ledgerEntry(suffix)},
		func(yield func(store.LedgerVersion, error) bool) { yield(suffix, nil) }))
	withSuffix, err := s.GetLedgerIndex(ctx, projectID)
	must(t, err)
	mustEqual(t, "suffix updates the changed entry", withSuffix.Entries[1], ledgerEntry(suffix))
	mustEqual(t, "suffix keeps the other entry", withSuffix.Entries[0], ledgerEntry(root))
	pageWithSuffix, _, err := s.ListLedgerHistory(ctx, projectID, 12, "", 10)
	must(t, err)
	mustEqual(t, "suffix keeps prior versions", pageWithSuffix, []store.LedgerVersion{suffix, edited, added})
	must(t, s.ReplaceLedgerIndex(ctx, projectID, third, second, false, index.Entries,
		func(yield func(store.LedgerVersion, error) bool) {
			if yield(added, nil) && yield(root, nil) {
				yield(edited, nil)
			}
		}))

	page, more, err := s.ListLedgerHistory(ctx, projectID, 12, "", 1)
	must(t, err)
	mustEqual(t, "top version", page[0].Commit, second)
	mustEqual(t, "has earlier page", more, true)
	page, more, err = s.ListLedgerHistory(ctx, projectID, 12, second, 1)
	must(t, err)
	mustEqual(t, "older version", page, []store.LedgerVersion{added})
	mustEqual(t, "last page", more, false)
	_, _, err = s.ListLedgerHistory(ctx, projectID, 0, second, 1)
	mustIs(t, err, store.ErrLedgerCursor)

	advanced := index
	advanced.Head, advanced.NextNumber = third, 100
	high := store.LedgerEntry{Number: 99, Dir: "zzz", Title: "High", Path: "zzz/D99-High.md", Commit: third, CommittedAt: t0}
	advanced.Entries = append(advanced.Entries, high)
	newVersions := []store.LedgerVersion{{Number: high.Number, Title: high.Title, Dir: high.Dir,
		Path: high.Path, Commit: third, CommittedAt: t0, Added: true}}
	mustIs(t, s.AdvanceLedgerIndex(ctx, projectID, first, third, []store.LedgerEntry{high}, newVersions), store.ErrBusy)
	must(t, s.AdvanceLedgerIndex(ctx, projectID, second, third, []store.LedgerEntry{high}, newVersions))
	index, err = s.GetLedgerIndex(ctx, projectID)
	must(t, err)
	mustEqual(t, "advanced index", index, advanced)

	injected := errors.New("snapshot read failed")
	mustIs(t, s.ReplaceLedgerIndex(ctx, projectID, third, first, false, []store.LedgerEntry{ledgerEntry(added)}, func(yield func(store.LedgerVersion, error) bool) {
		if yield(added, nil) {
			yield(store.LedgerVersion{}, injected)
		}
	}), injected)
	index, err = s.GetLedgerIndex(ctx, projectID)
	must(t, err)
	mustEqual(t, "failed rebuild rolls back", index, advanced)
	must(t, s.ReplaceLedgerIndex(ctx, projectID, third, first, false, []store.LedgerEntry{ledgerEntry(added)}, func(yield func(store.LedgerVersion, error) bool) {
		yield(added, nil)
	}))
	index, err = s.GetLedgerIndex(ctx, projectID)
	must(t, err)
	mustEqual(t, "rebuild preserves counter", index.NextNumber, uint32(100))
	page, more, err = s.ListLedgerHistory(ctx, projectID, 12, "", 100)
	must(t, err)
	mustEqual(t, "rebuild replaces versions", page, []store.LedgerVersion{added})
	mustEqual(t, "only one version", more, false)

	mustIs(t, s.ReplaceLedgerIndex(ctx, projectID, third, "", false, nil, nil), store.ErrBusy)
	must(t, s.ReplaceLedgerIndex(ctx, projectID, first, "", false, nil, nil))
	index, err = s.GetLedgerIndex(ctx, projectID)
	must(t, err)
	mustEqual(t, "repository loss preserves the counter", index, store.LedgerIndex{NextNumber: 100})
	page, _, err = s.ListLedgerHistory(ctx, projectID, 12, "", 1)
	must(t, err)
	mustEqual(t, "repository loss clears versions", len(page), 0)

	must(t, s.DeleteProject(ctx, f.user.ID, projectID, t0))
	projects, err = s.ListLedgerProjects(ctx, "")
	must(t, err)
	mustEqual(t, "cascade index rows", len(projects), 0)
	index, err = s.GetLedgerIndex(ctx, projectID)
	must(t, err)
	mustEqual(t, "cascade entries and counter", index, store.LedgerIndex{})
	page, _, err = s.ListLedgerHistory(ctx, projectID, 12, "", 1)
	must(t, err)
	if len(page) != 0 {
		t.Fatal("project deletion left ledger rows")
	}
}

func ledgerEntry(v store.LedgerVersion) store.LedgerEntry {
	return store.LedgerEntry{Number: v.Number, Dir: v.Dir, Title: v.Title, Path: v.Path, Commit: v.Commit, CommittedAt: v.CommittedAt}
}

// ledgerAdditions returns one added entry and version per number, at
// position 0 of commit.
func ledgerAdditions(commit string, numbers int) ([]store.LedgerEntry, []store.LedgerVersion) {
	entries := make([]store.LedgerEntry, 0, numbers)
	versions := make([]store.LedgerVersion, 0, numbers)
	for n := range uint32(numbers) {
		title := fmt.Sprintf("Entry %d", n)
		v := store.LedgerVersion{Number: n, Commit: commit, Added: true, Author: "import", Summary: "Seed",
			Title: title, Path: fmt.Sprintf("D%d-Entry-%d.md", n, n), CommittedAt: t0}
		entries = append(entries, ledgerEntry(v))
		versions = append(versions, v)
	}
	return entries, versions
}

// A full replacement keeps the store available while it streams, and
// readers see the previous complete index until it finishes. A newer
// replacement supersedes one still in progress.
func testLedgerStagedReplacement(t *testing.T, b Backend) {
	s, _ := open(t, b)
	seed(t, s)
	ctx := t.Context()
	projectID := "session-project"
	first, second, third := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	oldEntries, oldVersions := ledgerAdditions(first, 3)
	must(t, s.ReplaceLedgerIndex(ctx, projectID, "", first, false, oldEntries, slicesSeq(oldVersions)))
	old, err := s.GetLedgerIndex(ctx, projectID)
	must(t, err)
	oldPage, _, err := s.ListLedgerHistory(ctx, projectID, 0, "", 10)
	must(t, err)

	// Far more rows than one bounded transaction holds.
	newEntries, newVersions := ledgerAdditions(second, store.MaxLedgerDecisions)
	halfway, resume := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- s.ReplaceLedgerIndex(ctx, projectID, first, second, false, newEntries, func(yield func(store.LedgerVersion, error) bool) {
			for i, v := range newVersions {
				if i == len(newVersions)/2 {
					close(halfway)
					<-resume
				}
				if !yield(v, nil) {
					return
				}
			}
		})
	}()
	<-halfway
	must(t, within(t, func(ctx context.Context) error {
		index, err := s.GetLedgerIndex(ctx, projectID)
		if err != nil {
			return err
		}
		mustEqual(t, "index during a replacement", index, old)
		page, _, err := s.ListLedgerHistory(ctx, projectID, store.MaxLedgerDecisions-1, "", 10)
		if err != nil {
			return err
		}
		mustEqual(t, "staged versions are invisible", len(page), 0)
		page, _, err = s.ListLedgerHistory(ctx, projectID, 0, "", 10)
		mustEqual(t, "history during a replacement", page, oldPage)
		return err
	}))
	// A newer replacement wins over the one in progress, even when it
	// leaves the head that one expects.
	newer, newerVersions := ledgerAdditions(first, 2)
	must(t, within(t, func(ctx context.Context) error {
		return s.ReplaceLedgerIndex(ctx, projectID, first, first, false, newer, slicesSeq(newerVersions))
	}))
	close(resume)
	mustIs(t, <-done, store.ErrBusy)
	index, err := s.GetLedgerIndex(ctx, projectID)
	must(t, err)
	mustEqual(t, "newer replacement", index, store.LedgerIndex{Head: first, NextNumber: 3, Entries: newer})

	// A failed replacement leaves the index as it was, and the next one
	// is not disturbed by the rows the failed one staged.
	injected := errors.New("replay failed")
	mustIs(t, s.ReplaceLedgerIndex(ctx, projectID, first, second, false, newEntries, func(yield func(store.LedgerVersion, error) bool) {
		for _, v := range newVersions[:len(newVersions)-1] {
			if !yield(v, nil) {
				return
			}
		}
		yield(store.LedgerVersion{}, injected)
	}), injected)
	index, err = s.GetLedgerIndex(ctx, projectID)
	must(t, err)
	mustEqual(t, "failed replacement keeps the index", index.Head, first)
	must(t, s.ReplaceLedgerIndex(ctx, projectID, first, second, false, newEntries, slicesSeq(newVersions)))
	index, err = s.GetLedgerIndex(ctx, projectID)
	must(t, err)
	mustEqual(t, "replacement after a failure", index, store.LedgerIndex{Head: second, NextNumber: store.MaxLedgerDecisions, Entries: sortedLedgerEntries(newEntries)})
	for _, number := range []uint32{0, store.MaxLedgerDecisions - 1} {
		page, more, err := s.ListLedgerHistory(ctx, projectID, number, "", 10)
		must(t, err)
		mustEqual(t, "one version each", page, []store.LedgerVersion{newVersions[number]})
		mustEqual(t, "no further versions", more, false)
	}

	// An advance during a full replacement wins: the replacement's final
	// transaction checks the expected head again.
	halfway, resume = make(chan struct{}), make(chan struct{})
	go func() {
		done <- s.ReplaceLedgerIndex(ctx, projectID, second, first, false, newEntries, func(yield func(store.LedgerVersion, error) bool) {
			for i, v := range newVersions {
				if i == len(newVersions)/2 {
					close(halfway)
					<-resume
				}
				v.Commit = first
				if !yield(v, nil) {
					return
				}
			}
		})
	}()
	<-halfway
	advanced := store.LedgerVersion{Number: 0, Position: 1, Commit: third, Title: "Advanced", Path: "D0-Advanced.md", ContentChanged: true, Retitled: true}
	must(t, within(t, func(ctx context.Context) error {
		return s.AdvanceLedgerIndex(ctx, projectID, second, third, []store.LedgerEntry{ledgerEntry(advanced)}, []store.LedgerVersion{advanced})
	}))
	close(resume)
	mustIs(t, <-done, store.ErrBusy)
	index, err = s.GetLedgerIndex(ctx, projectID)
	must(t, err)
	mustEqual(t, "advance during a replacement", index.Head, third)
	page, _, err := s.ListLedgerHistory(ctx, projectID, 0, "", 10)
	must(t, err)
	mustEqual(t, "advanced history", page, []store.LedgerVersion{advanced, newVersions[0]})

	// A suffix is bounded like the versions of one commit.
	mustIs(t, s.ReplaceLedgerIndex(ctx, projectID, third, first, true, nil, func(yield func(store.LedgerVersion, error) bool) {
		for i := range store.MaxLedgerDecisions + 1 {
			v := newVersions[i%len(newVersions)]
			v.Commit, v.Position, v.Added, v.ContentChanged = first, int64(2+i/len(newVersions)), false, true
			if !yield(v, nil) {
				return
			}
		}
	}), store.ErrInvalid)
}

func slicesSeq(versions []store.LedgerVersion) iter.Seq2[store.LedgerVersion, error] {
	return func(yield func(store.LedgerVersion, error) bool) {
		for _, v := range versions {
			if !yield(v, nil) {
				return
			}
		}
	}
}

// sortedLedgerEntries orders entries as GetLedgerIndex does: by directory,
// then path.
func sortedLedgerEntries(entries []store.LedgerEntry) []store.LedgerEntry {
	out := slices.Clone(entries)
	slices.SortFunc(out, func(a, b store.LedgerEntry) int {
		return cmp.Or(strings.Compare(a.Dir, b.Dir), strings.Compare(a.Path, b.Path))
	})
	return out
}
