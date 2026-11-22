package sqlite

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/EricWvi/circus/internal/store"
)

// Rows staged by a full index replacement that could not clean up after
// itself, as after a crash, are invisible and are deleted by the next one.
func TestLedgerReplacementPurgesAbandonedRows(t *testing.T) {
	d := openAt(t, t.TempDir(), Options{})
	defer d.Close()
	ctx := t.Context()
	if err := d.CreateUser(ctx, store.User{ID: "owner", Name: "Owner", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.CreateProject(ctx, store.Project{ID: "p", UserID: "owner", Name: "P", CreatedAt: 1, UpdatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	install := func(ctx context.Context, expected, head string, numbers int, stop func(int)) error {
		entries := make([]store.LedgerEntry, 0, numbers)
		for n := range uint32(numbers) {
			entries = append(entries, store.LedgerEntry{Number: n, Title: "T", Path: fmt.Sprintf("D%d-T.md", n), Commit: head})
		}
		return d.ReplaceLedgerIndex(ctx, "p", expected, head, false, entries, func(yield func(store.LedgerVersion, error) bool) {
			for i, e := range entries {
				if stop != nil {
					stop(i)
				}
				v := store.LedgerVersion{Number: e.Number, Commit: head, Title: e.Title, Path: e.Path, Added: true}
				if !yield(v, nil) {
					return
				}
			}
		})
	}
	count := func(table, where string) int {
		t.Helper()
		var n int
		err := d.sqldb.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table+` WHERE project_id = 'p' AND `+where+
			` (SELECT generation FROM ledger_heads WHERE project_id = 'p')`).Scan(&n)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	first, second := strings.Repeat("a", 40), strings.Repeat("b", 40)
	if err := install(ctx, "", first, 3, nil); err != nil {
		t.Fatal(err)
	}
	// A failed replacement deletes the rows it staged.
	injected := errors.New("replay failed")
	err := d.ReplaceLedgerIndex(ctx, "p", first, second, false, nil, func(yield func(store.LedgerVersion, error) bool) {
		for n := range uint32(2 * ledgerInstallRows) {
			if !yield(store.LedgerVersion{Number: n, Commit: second, Title: "T", Path: fmt.Sprintf("D%d-T.md", n), Added: true}, nil) {
				return
			}
		}
		yield(store.LedgerVersion{}, injected)
	})
	if !errors.Is(err, injected) {
		t.Fatalf("failed replacement = %v", err)
	}
	if n := count("ledger_versions", "generation <>"); n != 0 {
		t.Fatalf("%d staged versions left after a failure", n)
	}
	// A cancelled replacement still cleans up after itself.
	cancelled, cancel := context.WithCancel(ctx)
	rows := 3 * ledgerInstallRows
	err = install(cancelled, first, second, rows, func(i int) {
		if i == rows/2 {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled replacement = %v", err)
	}
	for _, table := range []string{"ledger_entries", "ledger_versions"} {
		if n := count(table, "generation <>"); n != 0 {
			t.Fatalf("%d staged %s rows left after a cancel", n, table)
		}
	}
	if index, err := d.GetLedgerIndex(ctx, "p"); err != nil || index.Head != first || len(index.Entries) != 3 {
		t.Fatalf("index after a cancelled replacement = %+v, %v", index, err)
	}

	// Rows staged by a process that crashed stay until the next write
	// that moves the head: here an advance.
	stage := func() {
		t.Helper()
		var generation int64
		if err := d.q.QueryRow(ctx, "UPDATE ledger_heads SET reserved = reserved + 1 WHERE project_id = 'p' RETURNING reserved").Scan(&generation); err != nil {
			t.Fatal(err)
		}
		entries := make([]store.LedgerEntry, 0, rows)
		versions := make([]store.LedgerVersion, 0, rows)
		for n := range uint32(rows) {
			e := store.LedgerEntry{Number: n, Title: "T", Path: fmt.Sprintf("D%d-T.md", n), Commit: second}
			entries = append(entries, e)
			versions = append(versions, store.LedgerVersion{Number: n, Commit: second, Title: "T", Path: e.Path, Added: true})
		}
		if err := d.writeLedgerEntries(ctx, "p", generation, entries); err != nil {
			t.Fatal(err)
		}
		for chunk := range slices.Chunk(versions, ledgerBatchRows) {
			if err := d.insertLedgerVersions(ctx, "p", generation, chunk, nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	stage()
	if n := count("ledger_versions", "generation <>"); n != rows {
		t.Fatalf("%d crashed versions, want %d", n, rows)
	}
	third := strings.Repeat("c", 40)
	if err := d.AdvanceLedgerIndex(ctx, "p", first, third, nil, nil); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"ledger_entries", "ledger_versions"} {
		if n := count(table, "generation <>"); n != 0 {
			t.Fatalf("%d crashed %s rows left after an advance", n, table)
		}
	}
	// And until the next full replacement.
	stage()
	if err := install(ctx, third, second, 5, nil); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"ledger_entries", "ledger_versions"} {
		if stale, live := count(table, "generation <>"), count(table, "generation ="); stale != 0 || live != 5 {
			t.Fatalf("%s: %d stale and %d live rows", table, stale, live)
		}
	}
}

// seedLedgerRows writes an index of n version rows for the project with
// one statement, which is far quicker than an install.
func seedLedgerRows(t *testing.T, d *DB, projectID string, n int) {
	t.Helper()
	ctx := t.Context()
	head := strings.Repeat("a", 40)
	if err := d.ReplaceLedgerIndex(ctx, projectID, "", head, false, []store.LedgerEntry{{Number: 0, Title: "T", Path: "D0-T.md", Commit: head}},
		func(yield func(store.LedgerVersion, error) bool) {
			yield(store.LedgerVersion{Number: 0, Commit: head, Title: "T", Path: "D0-T.md", Added: true}, nil)
		}); err != nil {
		t.Fatal(err)
	}
	_, err := d.sqldb.ExecContext(ctx, `WITH RECURSIVE i(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM i WHERE n < ?)
		INSERT INTO ledger_versions (project_id, generation, number, position, git_commit, committed_at, author, summary,
			dir, path, title, code_commit, added, content_changed, moved, retitled)
		SELECT ?, (SELECT generation FROM ledger_heads WHERE project_id = ?), n % 4096, n / 4096 + 1, printf('%040x', n), 0,
			'import', 'Seed', '', 'D0-T.md', 'T', '', 0, 1, 0, 0 FROM i`, n-1, projectID, projectID)
	if err != nil {
		t.Fatal(err)
	}
}

func ledgerRows(t *testing.T, d *DB, projectID string) int {
	t.Helper()
	var n int
	err := d.sqldb.QueryRowContext(t.Context(), `SELECT (SELECT COUNT(*) FROM ledger_versions WHERE project_id = ?1)
		+ (SELECT COUNT(*) FROM ledger_entries WHERE project_id = ?1)`, projectID).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// Deleting a project with a long ledger history deletes it in bounded
// transactions, so other store calls keep running. A crash between them
// leaves the project with an empty index, and the retry finishes.
func TestDeleteProjectWithLargeLedger(t *testing.T) {
	d := openAt(t, t.TempDir(), Options{})
	defer d.Close()
	ctx := t.Context()
	if err := d.CreateUser(ctx, store.User{ID: "owner", Name: "Owner", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.CreateProject(ctx, store.Project{ID: "p", UserID: "owner", Name: "P", CreatedAt: 1, UpdatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	rows := 200_000
	if raceEnabled {
		rows = 50_000
	}
	seedLedgerRows(t, d, "p", rows)

	crashed := errors.New("crashed")
	chunks := 0
	d.afterLedgerChunk = func() error {
		if chunks++; chunks == 3 {
			return crashed
		}
		return nil
	}
	if err := d.DeleteProject(ctx, "owner", "p", 2); !errors.Is(err, crashed) {
		t.Fatalf("interrupted delete = %v", err)
	}
	d.afterLedgerChunk = nil
	if _, err := d.GetProject(ctx, "owner", "p"); err != nil {
		t.Fatalf("project after an interrupted delete: %v", err)
	}
	index, err := d.GetLedgerIndex(ctx, "p")
	if err != nil || index.Head != "" || len(index.Entries) != 0 {
		t.Fatalf("index after an interrupted delete = %+v, %v", index, err)
	}
	if page, _, err := d.ListLedgerHistory(ctx, "p", 0, "", 10); err != nil || len(page) != 0 {
		t.Fatalf("history after an interrupted delete = %+v, %v", page, err)
	}

	const maxLatency = 250 * time.Millisecond
	stop, done := make(chan struct{}), make(chan error, 1)
	var worst time.Duration
	var reads int
	go func() {
		for {
			select {
			case <-stop:
				done <- nil
				return
			default:
			}
			call, cancel := context.WithTimeout(ctx, maxLatency)
			start := time.Now()
			_, err := d.GetUser(call, "owner")
			cancel()
			worst, reads = max(worst, time.Since(start)), reads+1
			if err != nil {
				done <- err
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	start := time.Now()
	err = d.DeleteProject(ctx, "owner", "p", 2)
	took := time.Since(start)
	close(stop)
	if readErr := <-done; readErr != nil || worst > maxLatency || reads < 2 {
		t.Fatalf("concurrent GetUser: %d reads, worst %s, %v", reads, worst, readErr)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("deleted %d ledger rows in %s: %d concurrent reads, worst latency %s", rows, took, reads, worst)
	if n := ledgerRows(t, d, "p"); n != 0 {
		t.Fatalf("%d ledger rows left", n)
	}
}

// Deleting a user drops the ledgers of its projects the same way.
func TestDeleteUserWithLedgers(t *testing.T) {
	d := openAt(t, t.TempDir(), Options{})
	defer d.Close()
	ctx := t.Context()
	if err := d.CreateUser(ctx, store.User{ID: "owner", Name: "Owner", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"p", "q"} {
		if _, err := d.CreateProject(ctx, store.Project{ID: id, UserID: "owner", Name: id, CreatedAt: 1, UpdatedAt: 1}); err != nil {
			t.Fatal(err)
		}
		seedLedgerRows(t, d, id, 3*ledgerInstallRows)
	}
	chunks := 0
	d.afterLedgerChunk = func() error { chunks++; return nil }
	if err := d.DeleteUser(ctx, "owner"); err != nil {
		t.Fatal(err)
	}
	if chunks < 4 {
		t.Fatalf("ledger rows deleted in %d bounded transactions", chunks)
	}
	for _, id := range []string{"p", "q"} {
		if n := ledgerRows(t, d, id); n != 0 {
			t.Fatalf("%s: %d ledger rows left", id, n)
		}
	}
}

// deleteFixture opens a store with a user, a device and a project whose
// ledger index holds rows version rows.
func deleteFixture(t *testing.T, rows int) *DB {
	t.Helper()
	d := openAt(t, t.TempDir(), Options{})
	t.Cleanup(func() { _ = d.Close() })
	ctx := t.Context()
	if err := d.CreateUser(ctx, store.User{ID: "owner", Name: "Owner", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := d.CreateIdentity(ctx, store.Identity{Kind: store.KindDevice, ID: "d1", UserID: "owner", Name: "d", MachineID: "m", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.CreateProject(ctx, store.Project{ID: "p", UserID: "owner", Name: "P", CreatedAt: 1, UpdatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	seedLedgerRows(t, d, "p", rows)
	return d
}

func addSession(t *testing.T, d *DB) {
	t.Helper()
	if _, err := d.CreateSession(t.Context(), store.Session{ID: "s1", ClientRequestID: "s1", UserID: "owner", ProjectID: "p",
		ClaudeSessionID: "c1", DefaultDeviceID: "d1", CreatedAt: 10}, nil, 10); err != nil {
		t.Fatal(err)
	}
}

// A deletion refused before it starts leaves the ledger index alone.
func TestDeleteProjectRefusedKeepsLedger(t *testing.T) {
	d := deleteFixture(t, 3*ledgerInstallRows)
	ctx := t.Context()
	before, err := d.GetLedgerIndex(ctx, "p")
	if err != nil {
		t.Fatal(err)
	}
	addSession(t, d)
	if err := d.DeleteProject(ctx, "owner", "p", 2); !errors.Is(err, store.ErrHasSessions) {
		t.Fatalf("delete with a session = %v", err)
	}
	if err := d.DeleteUser(ctx, "owner"); !errors.Is(err, store.ErrHasSessions) {
		t.Fatalf("user delete with a session = %v", err)
	}
	after, err := d.GetLedgerIndex(ctx, "p")
	if err != nil || after.Head != before.Head || !slices.Equal(after.Entries, before.Entries) {
		t.Fatalf("index after refused deletes = %+v, %v; before %+v", after, err, before)
	}
}

// A session created while the ledger rows are being deleted makes the
// final transaction refuse with ErrHasSessions, which callers retry.
func TestDeleteProjectRefusedAtTheEnd(t *testing.T) {
	d := deleteFixture(t, 3*ledgerInstallRows)
	ctx := t.Context()
	d.afterLedgerChunk = func() error {
		d.afterLedgerChunk = nil
		addSession(t, d)
		return nil
	}
	if err := d.DeleteProject(ctx, "owner", "p", 2); !errors.Is(err, store.ErrHasSessions) {
		t.Fatalf("delete with a session created meanwhile = %v", err)
	}
	if _, err := d.GetProject(ctx, "owner", "p"); err != nil {
		t.Fatalf("project after a refused delete: %v", err)
	}
}

// A caller that goes away during the deletion does not stop it halfway.
func TestDeleteProjectOutlivesItsCaller(t *testing.T) {
	d := deleteFixture(t, 4*ledgerInstallRows)
	caller, cancel := context.WithCancel(t.Context())
	defer cancel()
	d.afterLedgerChunk = func() error {
		cancel()
		return nil
	}
	if err := d.DeleteProject(caller, "owner", "p", 2); err != nil {
		t.Fatalf("delete whose caller went away = %v", err)
	}
	if _, err := d.GetProject(t.Context(), "owner", "p"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("project after the delete: %v", err)
	}
	if n := ledgerRows(t, d, "p"); n != 0 {
		t.Fatalf("%d ledger rows left", n)
	}
}

// Rows written by a racing commit between the purge and the final
// transaction are purged again instead of cascading in one transaction.
func TestDeleteProjectPurgesRowsWrittenMeanwhile(t *testing.T) {
	d := deleteFixture(t, 2*ledgerInstallRows)
	ctx := t.Context()
	head := strings.Repeat("b", 40)
	rewrites := 0
	var hook func() error
	hook = func() error {
		if rewrites < 1 {
			rewrites++
			// A rebuild that installed while the delete was purging. Its
			// own cleanup runs without the hook.
			d.afterLedgerChunk = nil
			defer func() { d.afterLedgerChunk = hook }()
			return d.ReplaceLedgerIndex(context.WithoutCancel(ctx), "p", "", head, false,
				[]store.LedgerEntry{{Number: 0, Title: "T", Path: "D0-T.md", Commit: head}},
				func(yield func(store.LedgerVersion, error) bool) {
					yield(store.LedgerVersion{Number: 0, Commit: head, Title: "T", Path: "D0-T.md", Added: true}, nil)
				})
		}
		return nil
	}
	// Count the ledger rows that the final transaction cascades, which it
	// does only after the project row is gone.
	if _, err := d.sqldb.ExecContext(ctx, `CREATE TABLE cascaded (n INTEGER NOT NULL);
		INSERT INTO cascaded VALUES (0);
		CREATE TRIGGER count_cascade AFTER DELETE ON ledger_versions
		WHEN NOT EXISTS (SELECT 1 FROM projects WHERE id = OLD.project_id)
		BEGIN UPDATE cascaded SET n = n + 1; END`); err != nil {
		t.Fatal(err)
	}
	d.afterLedgerChunk = hook
	if err := d.DeleteProject(ctx, "owner", "p", 2); err != nil {
		t.Fatal(err)
	}
	if rewrites != 1 {
		t.Fatalf("%d racing rewrites", rewrites)
	}
	var cascaded int
	if err := d.sqldb.QueryRowContext(ctx, "SELECT n FROM cascaded").Scan(&cascaded); err != nil || cascaded != 0 {
		t.Fatalf("the final transaction cascaded %d ledger versions, %v", cascaded, err)
	}
}

// The purge after a full replacement's switch continues when the caller's
// ctx ends.
func TestReplacementCleanupOutlivesItsCaller(t *testing.T) {
	d := deleteFixture(t, 4*ledgerInstallRows)
	caller, cancel := context.WithCancel(t.Context())
	defer cancel()
	d.afterLedgerChunk = func() error {
		cancel()
		return nil
	}
	head := strings.Repeat("b", 40)
	err := d.ReplaceLedgerIndex(caller, "p", strings.Repeat("a", 40), head, false,
		[]store.LedgerEntry{{Number: 0, Title: "T", Path: "D0-T.md", Commit: head}},
		func(yield func(store.LedgerVersion, error) bool) {
			yield(store.LedgerVersion{Number: 0, Commit: head, Title: "T", Path: "D0-T.md", Added: true}, nil)
		})
	if err != nil {
		t.Fatal(err)
	}
	d.afterLedgerChunk = nil
	if n := ledgerRows(t, d, "p"); n != 2 {
		t.Fatalf("%d ledger rows after the replacement, want the new entry and version", n)
	}
}

// A deletion whose ledger rows keep coming back gives up after a bounded
// number of attempts with ErrBusy, which callers retry.
func TestDeleteProjectGivesUpWhenRowsKeepComingBack(t *testing.T) {
	d := deleteFixture(t, 2*ledgerInstallRows)
	ctx := t.Context()
	rewrites := 0
	var hook func() error
	hook = func() error {
		if rewrites == 10 {
			return nil
		}
		rewrites++
		d.afterLedgerChunk = nil
		defer func() { d.afterLedgerChunk = hook }()
		// A racing rebuild that installs enough rows to be purged in
		// more than one chunk again.
		head := fmt.Sprintf("%040x", rewrites)
		indexed, err := d.GetLedgerHead(ctx, "p")
		if err != nil {
			return err
		}
		return d.ReplaceLedgerIndex(context.WithoutCancel(ctx), "p", indexed, head, false, nil,
			func(yield func(store.LedgerVersion, error) bool) {
				for n := range uint32(2 * ledgerInstallRows) {
					if !yield(store.LedgerVersion{Number: n, Commit: head, Title: "T", Path: fmt.Sprintf("D%d-T.md", n), Added: true}, nil) {
						return
					}
				}
			})
	}
	d.afterLedgerChunk = hook
	err := d.DeleteProject(ctx, "owner", "p", 2)
	if !errors.Is(err, store.ErrBusy) {
		t.Fatalf("delete with rows coming back = %v", err)
	}
	if rewrites != ledgerDeleteAttempts {
		t.Fatalf("%d attempts, want %d", rewrites, ledgerDeleteAttempts)
	}
	d.afterLedgerChunk = nil
	if err := d.DeleteProject(ctx, "owner", "p", 2); err != nil {
		t.Fatalf("retried delete = %v", err)
	}
}
