package sqlite

import (
	"context"
	"fmt"
	"testing"

	"github.com/EricWvi/circus/internal/store"
)

// TestChangesetDeletionChunks: a deleted project's changesets go in
// transactions of at most ledgerInstallRows rows each.
func TestChangesetDeletionChunks(t *testing.T) {
	d := openSeeded(t, t.TempDir(), Options{})
	defer d.Close()
	ctx := context.Background()
	const now = int64(1_700_000_000_000)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(d.CreateUser(ctx, store.User{ID: "u1", Name: "u", CreatedAt: now}))
	for _, p := range []string{"p1", "keep"} {
		_, err := d.CreateProject(ctx, store.Project{ID: p, UserID: "u1", Name: p, CreatedAt: now, UpdatedAt: now})
		must(err)
	}
	const rows = 2*ledgerInstallRows + 22
	for i := range rows {
		for _, p := range []string{"p1", "keep"} {
			id := fmt.Sprintf("%s-%03d", p, i)
			must(d.CreateLedgerChangeset(ctx, store.LedgerChangeset{ID: id, ProjectID: p, State: store.LedgerChangesetOpen, CreatedAt: now, UpdatedAt: now}))
			_, err := d.DiscardLedgerChangeset(ctx, id, "history", true, now)
			must(err)
		}
	}
	var chunks []int
	d.afterChangesetChunk = func(n int) { chunks = append(chunks, n) }
	must(d.DeleteProject(ctx, "u1", "p1", now))
	d.afterChangesetChunk = nil
	total := 0
	for _, n := range chunks {
		if n > ledgerInstallRows {
			t.Fatalf("a transaction deleted %d changesets: %v", n, chunks)
		}
		total += n
	}
	if total != rows || len(chunks) != 3 {
		t.Fatalf("chunks %v, want %d rows in 3", chunks, rows)
	}
	page, _, err := d.ListLedgerChangesets(ctx, store.LedgerChangesetFilter{ProjectID: "keep", Limit: 1})
	must(err)
	if len(page) != 1 {
		t.Fatal("the other project lost its changesets")
	}
}
