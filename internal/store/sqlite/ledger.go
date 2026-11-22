package sqlite

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"slices"
	"strings"
	"time"

	"github.com/EricWvi/circus/internal/store"
)

func (o *ops) LedgerProjectName(ctx context.Context, projectID string) (string, bool, error) {
	if err := store.CheckID("project id", projectID); err != nil {
		return "", false, err
	}
	var name string
	err := mapErr(o.q.QueryRow(ctx, "SELECT name FROM projects WHERE id = ?", projectID).Scan(&name))
	if errors.Is(err, store.ErrNotFound) {
		return "", false, nil
	}
	return name, err == nil, err
}

func (o *ops) ListLedgerProjects(ctx context.Context, after string) ([]string, error) {
	if err := store.CheckOptionalID("project cursor", after); err != nil {
		return nil, err
	}
	rows, err := o.q.QueryContext(ctx, "SELECT project_id FROM ledger_heads WHERE project_id > ? AND head <> '' ORDER BY project_id LIMIT ?", after, store.LedgerProjectPage)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, mapErr(err)
		}
		ids = append(ids, id)
	}
	return ids, mapErr(rows.Err())
}

func (o *ops) GetLedgerHead(ctx context.Context, projectID string) (string, error) {
	if err := store.CheckID("project id", projectID); err != nil {
		return "", err
	}
	var head string
	err := o.q.QueryRow(ctx, "SELECT COALESCE((SELECT head FROM ledger_heads WHERE project_id = ?), '')", projectID).Scan(&head)
	return head, mapErr(err)
}

func (o *ops) GetLedgerIndex(ctx context.Context, projectID string) (store.LedgerIndex, error) {
	var out store.LedgerIndex
	if err := store.CheckID("project id", projectID); err != nil {
		return out, err
	}
	err := o.atomic(ctx, func(tx *ops) error {
		var generation int64
		err := mapErr(tx.q.QueryRow(ctx, "SELECT head, next_number, generation FROM ledger_heads WHERE project_id = ?", projectID).
			Scan(&out.Head, &out.NextNumber, &generation))
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		rows, err := tx.q.QueryContext(ctx, `SELECT number, dir, title, path, last_commit, committed_at
			FROM ledger_entries WHERE project_id = ? AND generation = ? ORDER BY dir, path`, projectID, generation)
		if err != nil {
			return mapErr(err)
		}
		defer rows.Close()
		for rows.Next() {
			var e store.LedgerEntry
			if err := rows.Scan(&e.Number, &e.Dir, &e.Title, &e.Path, &e.Commit, &e.CommittedAt); err != nil {
				return mapErr(err)
			}
			out.Entries = append(out.Entries, e)
		}
		return mapErr(rows.Err())
	})
	return out, err
}

func (o *ops) ReplaceLedgerIndex(ctx context.Context, projectID, expected, head string, retainPrefix bool, entries []store.LedgerEntry, versions iter.Seq2[store.LedgerVersion, error]) error {
	index := store.LedgerIndex{Head: head, Entries: entries}
	if err := checkLedgerIndex(projectID, index); err != nil {
		return err
	}
	if expected != "" && !store.IsLedgerCommitID(expected) || head == "" && versions != nil ||
		retainPrefix && (expected == "" || head == "" || head == expected) {
		return invalid("invalid ledger replacement")
	}
	if retainPrefix {
		stale := false
		err := o.unit(ctx, func(tx *ops) error {
			generation, err := tx.ledgerGeneration(ctx, projectID, expected)
			if err != nil {
				return err
			}
			if err := tx.putLedgerHead(ctx, projectID, index); err != nil {
				return err
			}
			if err := tx.writeLedgerEntries(ctx, projectID, generation, entries); err != nil {
				return err
			}
			if versions == nil {
				return nil
			}
			batch := make([]store.LedgerVersion, 0, ledgerBatchRows)
			rows, next := 0, int64(0)
			for v, err := range versions {
				if err != nil {
					return err
				}
				if rows++; rows > store.MaxLedgerDecisions {
					return invalid("a ledger suffix holds at most %d versions", store.MaxLedgerDecisions)
				}
				next = max(next, v.Position+1)
				batch = append(batch, v)
				if len(batch) == cap(batch) {
					if err := tx.insertLedgerVersions(ctx, projectID, generation, batch, nil); err != nil {
						return err
					}
					batch = batch[:0]
				}
			}
			if err := tx.insertLedgerVersions(ctx, projectID, generation, batch, nil); err != nil {
				return err
			}
			if _, err := tx.exec(ctx, "UPDATE ledger_heads SET next_position = MAX(next_position, ?) WHERE project_id = ?", next, projectID); err != nil {
				return err
			}
			stale, err = tx.supersedeLedgerInstall(ctx, projectID)
			return err
		})
		if err == nil && stale {
			o.cleanLedgerIndex(ctx, projectID)
		}
		return err
	}
	return o.installLedgerIndex(ctx, projectID, expected, index, versions)
}

// ledgerGeneration returns the generation readers see, after checking
// that the indexed head is still expected.
func (o *ops) ledgerGeneration(ctx context.Context, projectID, expected string) (int64, error) {
	var head string
	var generation int64
	err := mapErr(o.q.QueryRow(ctx, "SELECT head, generation FROM ledger_heads WHERE project_id = ?", projectID).Scan(&head, &generation))
	if errors.Is(err, store.ErrNotFound) {
		err = nil
	}
	if err != nil {
		return 0, err
	}
	if head != expected {
		return 0, store.ErrBusy
	}
	return generation, nil
}

// installLedgerIndex replaces the whole index without holding the store
// for a time proportional to the history. It reserves a new generation,
// stages the entries and versions under it in transactions of at most
// ledgerInstallRows rows, and then switches readers over by updating the
// head row. Until then, readers keep the previous complete index. Rows of
// the old generation and of abandoned installs are deleted afterwards,
// again in bounded transactions. A newer reservation supersedes an install
// in progress, which then fails with ErrBusy.
func (o *ops) installLedgerIndex(ctx context.Context, projectID, expected string, index store.LedgerIndex, versions iter.Seq2[store.LedgerVersion, error]) error {
	var generation int64
	err := o.unit(ctx, func(tx *ops) error {
		if _, err := tx.ledgerGeneration(ctx, projectID, expected); err != nil {
			return err
		}
		if _, err := tx.exec(ctx, `INSERT INTO ledger_heads (project_id, head, next_number) VALUES (?, '', 0)
			ON CONFLICT (project_id) DO NOTHING`, projectID); err != nil {
			return err
		}
		return mapErr(tx.q.QueryRow(ctx, `UPDATE ledger_heads SET reserved = reserved + 1 WHERE project_id = ?
			RETURNING reserved`, projectID).Scan(&generation))
	})
	if err != nil {
		return err
	}
	// A failed install gives up its reservation, so that its staged rows
	// can be deleted now or, when that fails too, by the next replacement.
	// It runs even when the caller's ctx has ended, within its own budget.
	abandon := func(err error) error {
		cleanup, cancel := ledgerCleanupContext(ctx)
		defer cancel()
		_, giveUp := o.exec(cleanup, "UPDATE ledger_heads SET reserved = reserved + 1 WHERE project_id = ? AND reserved = ?", projectID, generation)
		if giveUp == nil {
			giveUp = o.purgeLedgerIndex(cleanup, projectID)
		}
		return errors.Join(err, giveUp)
	}
	if err := o.purgeLedgerIndex(ctx, projectID); err != nil {
		return abandon(err)
	}
	stage := func(write func(tx *ops) error) error {
		return o.unit(ctx, func(tx *ops) error {
			if err := tx.checkLedgerReservation(ctx, projectID, generation); err != nil {
				return err
			}
			return write(tx)
		})
	}
	for batch := range slices.Chunk(index.Entries, ledgerInstallRows) {
		if err := stage(func(tx *ops) error { return tx.writeLedgerEntries(ctx, projectID, generation, batch) }); err != nil {
			return abandon(err)
		}
	}
	var next int64
	if versions != nil {
		batch := make([]store.LedgerVersion, 0, ledgerInstallRows)
		insert := func(tx *ops) error {
			for chunk := range slices.Chunk(batch, ledgerBatchRows) {
				if err := tx.insertLedgerVersions(ctx, projectID, generation, chunk, nil); err != nil {
					return err
				}
			}
			return nil
		}
		for v, err := range versions {
			if err == nil && len(batch) == cap(batch) {
				err = stage(insert)
				batch = batch[:0]
			}
			if err != nil {
				return abandon(err)
			}
			next = max(next, v.Position+1)
			batch = append(batch, v)
		}
		if err := stage(insert); err != nil {
			return abandon(err)
		}
	}
	err = stage(func(tx *ops) error {
		if _, err := tx.ledgerGeneration(ctx, projectID, expected); err != nil {
			return err
		}
		if err := tx.putLedgerHead(ctx, projectID, index); err != nil {
			return err
		}
		_, err := tx.exec(ctx, "UPDATE ledger_heads SET generation = ?, next_position = ? WHERE project_id = ?", generation, next, projectID)
		return err
	})
	if err != nil {
		return abandon(err)
	}
	// The new index is live. Rows left behind by a failure here are
	// invisible and are deleted by the next replacement.
	o.cleanLedgerIndex(ctx, projectID)
	return nil
}

// Cleanup of invisible rows continues after the caller's ctx ends, for at
// most this long. Each of its transactions is bounded like an install's.
const ledgerCleanupTimeout = time.Minute

func ledgerCleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), ledgerCleanupTimeout)
}

// cleanLedgerIndex deletes invisible rows after a write that made them
// unreachable. A failure leaves them to a later replacement, advance or
// deletion, so it is not reported.
func (o *ops) cleanLedgerIndex(ctx context.Context, projectID string) {
	cleanup, cancel := ledgerCleanupContext(ctx)
	defer cancel()
	_ = o.purgeLedgerIndex(cleanup, projectID)
}

// supersedeLedgerInstall runs in a transaction that moves the head. Any
// full replacement still staging would fail its final head check anyway,
// so it is superseded at once, and the caller cleans up after the
// transaction when this reports rows of other generations.
func (o *ops) supersedeLedgerInstall(ctx context.Context, projectID string) (bool, error) {
	n, err := o.exec(ctx, "UPDATE ledger_heads SET reserved = reserved + 1 WHERE project_id = ? AND reserved > generation", projectID)
	return n > 0, err
}

func (o *ops) checkLedgerReservation(ctx context.Context, projectID string, generation int64) error {
	var reserved int64
	if err := mapErr(o.q.QueryRow(ctx, "SELECT reserved FROM ledger_heads WHERE project_id = ?", projectID).Scan(&reserved)); err != nil {
		return err
	}
	if reserved != generation {
		return fmt.Errorf("%w: a newer ledger index replacement started", store.ErrBusy)
	}
	return nil
}

// purgeLedgerIndex deletes the entries and versions of every generation
// other than the live and the most recently reserved one, at most
// ledgerInstallRows rows per transaction.
func (o *ops) purgeLedgerIndex(ctx context.Context, projectID string) error {
	for {
		deleted := 0
		err := o.unit(ctx, func(tx *ops) error {
			var live, reserved int64
			err := mapErr(tx.q.QueryRow(ctx, "SELECT generation, reserved FROM ledger_heads WHERE project_id = ?", projectID).Scan(&live, &reserved))
			if errors.Is(err, store.ErrNotFound) {
				return nil
			}
			if err != nil {
				return err
			}
			for _, table := range []struct{ name, key string }{
				{"ledger_entries", "project_id, generation, number"},
				{"ledger_versions", "project_id, generation, number, position"},
			} {
				for _, r := range [][2]int64{{0, live}, {live + 1, reserved}} {
					n, err := tx.exec(ctx, `DELETE FROM `+table.name+` WHERE (`+table.key+`) IN (
						SELECT `+table.key+` FROM `+table.name+`
						WHERE project_id = ? AND generation >= ? AND generation < ? LIMIT ?)`,
						projectID, r[0], r[1], ledgerInstallRows-deleted)
					if err != nil {
						return err
					}
					if deleted += n; deleted == ledgerInstallRows {
						return nil
					}
				}
			}
			return nil
		})
		if err != nil || deleted < ledgerInstallRows {
			return err
		}
		if o.db != nil && o.db.afterLedgerChunk != nil {
			if err := o.db.afterLedgerChunk(); err != nil {
				return err
			}
		}
	}
}

func (o *ops) AdvanceLedgerIndex(ctx context.Context, projectID, parent, head string, entries []store.LedgerEntry, versions []store.LedgerVersion) error {
	index := store.LedgerIndex{Head: head, Entries: entries}
	if err := checkLedgerIndex(projectID, index); err != nil {
		return err
	}
	if index.Head == "" || parent != "" && !store.IsLedgerCommitID(parent) || len(versions) > store.MaxLedgerDecisions {
		return invalid("invalid ledger advance")
	}
	stale := false
	err := o.unit(ctx, func(tx *ops) error {
		generation, err := tx.ledgerGeneration(ctx, projectID, parent)
		if err != nil {
			return err
		}
		if err := tx.putLedgerHead(ctx, projectID, index); err != nil {
			return err
		}
		if err := tx.writeLedgerEntries(ctx, projectID, generation, entries); err != nil {
			return err
		}
		if stale, err = tx.supersedeLedgerInstall(ctx, projectID); err != nil {
			return err
		}
		if len(versions) == 0 {
			return nil
		}
		var position int64
		err = tx.q.QueryRow(ctx, "SELECT next_position FROM ledger_heads WHERE project_id = ?", projectID).Scan(&position)
		if err != nil {
			return mapErr(err)
		}
		for _, v := range versions {
			if v.Commit != index.Head {
				return invalid("ledger version must name the new head")
			}
		}
		for batch := range slices.Chunk(versions, ledgerBatchRows) {
			if err := tx.insertLedgerVersions(ctx, projectID, generation, batch, &position); err != nil {
				return err
			}
		}
		_, err = tx.exec(ctx, "UPDATE ledger_heads SET next_position = ? WHERE project_id = ?", position+1, projectID)
		return err
	})
	if err == nil && stale {
		o.cleanLedgerIndex(ctx, projectID)
	}
	return err
}

func checkLedgerIndex(projectID string, index store.LedgerIndex) error {
	if err := store.CheckID("project id", projectID); err != nil {
		return err
	}
	if len(index.Entries) > store.MaxLedgerDecisions || index.Head == "" && (len(index.Entries) != 0 || index.NextNumber != 0) ||
		index.Head != "" && !store.IsLedgerCommitID(index.Head) {
		return invalid("invalid ledger index")
	}
	for _, e := range index.Entries {
		if e.Number > store.MaxLedgerNumber || !store.IsLedgerCommitID(e.Commit) || !validLedgerEntry(e.Title, e.Path) || len(e.Dir) > store.MaxLedgerDirBytes {
			return invalid("invalid ledger entry")
		}
	}
	return nil
}

func validLedgerEntry(title, path string) bool {
	return len(title) > 0 && len(title) <= store.MaxLedgerTitleBytes && len(path) > 0 && len(path) <= store.MaxLedgerPathBytes
}

func (o *ops) putLedgerHead(ctx context.Context, projectID string, index store.LedgerIndex) error {
	var next uint32
	for _, e := range index.Entries {
		next = max(next, e.Number+1)
	}
	_, err := o.exec(ctx, `INSERT INTO ledger_heads (project_id, head, next_number) VALUES (?,?,?)
		ON CONFLICT (project_id) DO UPDATE SET head = excluded.head, next_number = MAX(ledger_heads.next_number, excluded.next_number)`, projectID, index.Head, next)
	return err
}

// Batch statements bound both SQLite's bind parameters and the amount of
// per-statement work. Versions use the most parameters at 16 × 64 = 1024.
const ledgerBatchRows = 64

// A full index replacement stages and deletes versions in transactions of
// at most this many rows, so other store calls wait for one batch at most.
const ledgerInstallRows = 64

func (o *ops) writeLedgerEntries(ctx context.Context, projectID string, generation int64, entries []store.LedgerEntry) error {
	for batch := range slices.Chunk(entries, ledgerBatchRows) {
		args := make([]any, 0, len(batch)*8)
		for _, e := range batch {
			args = append(args, projectID, generation, e.Number, e.Dir, e.Title, e.Path, e.Commit, e.CommittedAt)
		}
		query := `INSERT INTO ledger_entries (project_id, generation, number, dir, title, path, last_commit, committed_at) VALUES ` +
			strings.TrimSuffix(strings.Repeat("(?,?,?,?,?,?,?,?),", len(batch)), ",") +
			` ON CONFLICT (project_id, generation, number) DO UPDATE SET dir = excluded.dir, title = excluded.title,
				path = excluded.path, last_commit = excluded.last_commit, committed_at = excluded.committed_at`
		if _, err := o.exec(ctx, query, args...); err != nil {
			return err
		}
	}
	return nil
}

// position overrides the input positions when appending versions for a
// single main-line commit. Rebuilds preserve each prepared row's position.
func (o *ops) insertLedgerVersions(ctx context.Context, projectID string, generation int64, versions []store.LedgerVersion, position *int64) error {
	if len(versions) == 0 {
		return nil
	}
	args := make([]any, 0, len(versions)*16)
	for _, v := range versions {
		if v.Number > store.MaxLedgerNumber || !store.IsLedgerCommitID(v.Commit) || !validLedgerEntry(v.Title, v.Path) ||
			len(v.Dir) > store.MaxLedgerDirBytes || len(v.Author) > store.MaxLedgerAuthorBytes || len(v.Summary) > store.MaxLedgerSummaryBytes ||
			v.CodeCommit != "" && !store.IsLedgerCommitID(v.CodeCommit) ||
			v.Added && (v.ContentChanged || v.Moved || v.Retitled) || !(v.Added || v.ContentChanged || v.Moved || v.Retitled) {
			return invalid("invalid ledger version")
		}
		if position != nil {
			v.Position = *position
		}
		args = append(args, projectID, generation, v.Number, v.Position, v.Commit, v.CommittedAt, v.Author, v.Summary, v.Dir,
			v.Path, v.Title, v.CodeCommit, v.Added, v.ContentChanged, v.Moved, v.Retitled)
	}
	query := `INSERT INTO ledger_versions (project_id, generation, number, position, git_commit, committed_at, author, summary,
		dir, path, title, code_commit, added, content_changed, moved, retitled) VALUES ` +
		strings.TrimSuffix(strings.Repeat("(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?),", len(versions)), ",")
	_, err := o.exec(ctx, query, args...)
	return err
}

func (o *ops) ListLedgerHistory(ctx context.Context, projectID string, number uint32, after string, limit int) ([]store.LedgerVersion, bool, error) {
	if err := store.CheckID("project id", projectID); err != nil {
		return nil, false, err
	}
	if number > store.MaxLedgerNumber || limit < 1 || limit > store.MaxLedgerHistoryPage || after != "" && !store.IsLedgerCommitID(after) {
		return nil, false, invalid("invalid ledger history filter")
	}
	var out []store.LedgerVersion
	err := o.atomic(ctx, func(tx *ops) error {
		var generation int64
		err := mapErr(tx.q.QueryRow(ctx, "SELECT generation FROM ledger_heads WHERE project_id = ?", projectID).Scan(&generation))
		if errors.Is(err, store.ErrNotFound) && after == "" {
			return nil
		}
		if errors.Is(err, store.ErrNotFound) {
			return store.ErrLedgerCursor
		}
		if err != nil {
			return err
		}
		query := `SELECT position, git_commit, committed_at, author, summary, dir, path, title, code_commit,
			added, content_changed, moved, retitled FROM ledger_versions WHERE project_id = ? AND generation = ? AND number = ?`
		args := []any{projectID, generation, number}
		if after != "" {
			var position int64
			err := mapErr(tx.q.QueryRow(ctx, "SELECT position FROM ledger_versions WHERE project_id = ? AND generation = ? AND number = ? AND git_commit = ?",
				projectID, generation, number, after).Scan(&position))
			if errors.Is(err, store.ErrNotFound) {
				return store.ErrLedgerCursor
			}
			if err != nil {
				return err
			}
			query += " AND position < ?"
			args = append(args, position)
		}
		rows, err := tx.q.QueryContext(ctx, query+" ORDER BY position DESC LIMIT ?", append(args, limit+1)...)
		if err != nil {
			return mapErr(err)
		}
		defer rows.Close()
		for rows.Next() {
			v := store.LedgerVersion{Number: number}
			if err := rows.Scan(&v.Position, &v.Commit, &v.CommittedAt, &v.Author, &v.Summary, &v.Dir, &v.Path, &v.Title, &v.CodeCommit,
				&v.Added, &v.ContentChanged, &v.Moved, &v.Retitled); err != nil {
				return mapErr(err)
			}
			out = append(out, v)
		}
		return mapErr(rows.Err())
	})
	if err != nil {
		return nil, false, err
	}
	if len(out) > limit {
		return out[:limit], true, nil
	}
	return out, false, nil
}
