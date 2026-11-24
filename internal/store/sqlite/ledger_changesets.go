package sqlite

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/EricWvi/circus/internal/store"
)

func (o *ops) GetLedgerNextNumber(ctx context.Context, projectID string) (uint32, error) {
	if err := store.CheckID("project id", projectID); err != nil {
		return 0, err
	}
	var next uint32
	err := o.q.QueryRow(ctx, "SELECT COALESCE((SELECT next_number FROM ledger_heads WHERE project_id = ?), 0)", projectID).Scan(&next)
	return next, mapErr(err)
}

const changesetCols = `id, project_id, state, base_commit, opener_worker_id, created_at, updated_at,
	merge_commit, merge_parent, code_commit, numbers, discard_reason, archived`

func scanChangeset(s scanner) (store.LedgerChangeset, error) {
	var c store.LedgerChangeset
	var state, numbers string
	err := s.Scan(&c.ID, &c.ProjectID, &state, &c.BaseCommit, &c.OpenerWorkerID, &c.CreatedAt, &c.UpdatedAt,
		&c.Merge.Commit, &c.Merge.Parent, &c.Merge.CodeCommit, &numbers, &c.DiscardReason, &c.Archived)
	if err != nil {
		return c, mapErr(err)
	}
	c.State = store.LedgerChangesetState(state)
	c.Merge.Numbers, err = decodeLedgerNumbers(numbers)
	return c, err
}

// encodeLedgerNumbers writes the letters in order: "a=12,b=13".
func encodeLedgerNumbers(numbers map[string]uint32) string {
	var b strings.Builder
	for i, letters := range slices.Sorted(maps.Keys(numbers)) {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(letters)
		b.WriteByte('=')
		b.WriteString(strconv.FormatUint(uint64(numbers[letters]), 10))
	}
	return b.String()
}

func decodeLedgerNumbers(s string) (map[string]uint32, error) {
	if s == "" {
		return nil, nil
	}
	out := make(map[string]uint32)
	for pair := range strings.SplitSeq(s, ",") {
		letters, number, ok := strings.Cut(pair, "=")
		n, err := strconv.ParseUint(number, 10, 32)
		if !ok || err != nil || !store.ValidTemporaryLetters(letters) {
			return nil, errors.New("sqlite: corrupt ledger changeset numbers")
		}
		out[letters] = uint32(n)
	}
	return out, nil
}

func checkLedgerMerge(m store.LedgerMerge) error {
	if !store.IsLedgerCommitID(m.Commit) || m.Parent != "" && !store.IsLedgerCommitID(m.Parent) ||
		m.CodeCommit != "" && !store.IsLedgerCommitID(m.CodeCommit) || len(m.Numbers) > store.MaxLedgerDecisions {
		return invalid("invalid ledger merge")
	}
	for letters, n := range m.Numbers {
		if !store.ValidTemporaryLetters(letters) || n > store.MaxLedgerNumber {
			return invalid("invalid ledger merge number")
		}
	}
	return nil
}

func (o *ops) CreateLedgerChangeset(ctx context.Context, c store.LedgerChangeset) error {
	if err := store.CheckID("changeset id", c.ID); err != nil {
		return err
	}
	if err := store.CheckID("project id", c.ProjectID); err != nil {
		return err
	}
	if c.State != store.LedgerChangesetOpen || c.BaseCommit != "" && !store.IsLedgerCommitID(c.BaseCommit) ||
		c.Merge.Commit != "" || c.DiscardReason != "" || c.CreatedAt <= 0 || c.UpdatedAt < c.CreatedAt {
		return invalid("a new ledger changeset is open, without merge or discard reason, and has its times")
	}
	if c.OpenerWorkerID != "" {
		if err := store.CheckID("opener worker id", c.OpenerWorkerID); err != nil {
			return err
		}
	}
	return o.unit(ctx, func(tx *ops) error {
		var exists bool
		if err := tx.q.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM projects WHERE id = ?)", c.ProjectID).Scan(&exists); err != nil {
			return mapErr(err)
		}
		if !exists {
			return store.ErrNotFound
		}
		open, err := tx.count(ctx, "SELECT COUNT(*) FROM ledger_changesets WHERE project_id = ? AND state = 'open'", c.ProjectID)
		if err != nil {
			return err
		}
		if open >= store.MaxOpenLedgerChangesets {
			return store.ErrLimit
		}
		_, err = tx.exec(ctx, `INSERT INTO ledger_changesets (id, project_id, state, base_commit, opener_worker_id, created_at, updated_at)
			VALUES (?,?,'open',?,?,?,?)`, c.ID, c.ProjectID, c.BaseCommit, c.OpenerWorkerID, c.CreatedAt, c.UpdatedAt)
		return err
	})
}

func (o *ops) GetLedgerChangeset(ctx context.Context, id string) (store.LedgerChangeset, error) {
	if err := store.CheckID("changeset id", id); err != nil {
		return store.LedgerChangeset{}, err
	}
	return scanChangeset(o.q.QueryRow(ctx, `SELECT `+changesetCols+` FROM ledger_changesets WHERE id = ?`, id))
}

func (o *ops) ListLedgerChangesets(ctx context.Context, f store.LedgerChangesetFilter) ([]store.LedgerChangeset, bool, error) {
	if err := store.CheckID("project id", f.ProjectID); err != nil {
		return nil, false, err
	}
	if f.State != "" && !f.State.Valid() || f.Limit < 1 || f.Limit > store.MaxLedgerChangesetPage {
		return nil, false, invalid("invalid ledger changeset filter")
	}
	query := `SELECT ` + changesetCols + ` FROM ledger_changesets WHERE project_id = ?`
	args := []any{f.ProjectID}
	if f.State != "" {
		query += ` AND state = ?`
		args = append(args, string(f.State))
	}
	if f.After != nil {
		if err := store.CheckID("changeset cursor", f.After.ID); err != nil {
			return nil, false, err
		}
		query += ` AND (created_at, id) < (?, ?)`
		args = append(args, f.After.CreatedAt, f.After.ID)
	}
	rows, err := o.q.QueryContext(ctx, query+` ORDER BY created_at DESC, id DESC LIMIT ?`, append(args, f.Limit+1)...)
	if err != nil {
		return nil, false, mapErr(err)
	}
	out, err := collect(rows, scanChangeset)
	if err != nil {
		return nil, false, err
	}
	if len(out) > f.Limit {
		return out[:f.Limit], true, nil
	}
	return out, false, nil
}

func (o *ops) ListLedgerRepairs(ctx context.Context, projectID string) ([]store.LedgerChangeset, error) {
	if err := store.CheckID("project id", projectID); err != nil {
		return nil, err
	}
	rows, err := o.q.QueryContext(ctx, `SELECT `+changesetCols+` FROM ledger_changesets
		WHERE project_id = ? AND ((state = 'open' AND merge_commit <> '') OR archived = 0) ORDER BY id LIMIT ?`,
		projectID, store.MaxLedgerRepairs)
	if err != nil {
		return nil, mapErr(err)
	}
	return collect(rows, scanChangeset)
}

// changesetUpdate runs an update of one changeset and reports whether it
// matched; a changeset that does not exist is ErrNotFound.
func (o *ops) changesetUpdate(ctx context.Context, id, query string, args ...any) (bool, error) {
	if err := store.CheckID("changeset id", id); err != nil {
		return false, err
	}
	var matched bool
	err := o.unit(ctx, func(tx *ops) error {
		n, err := tx.exec(ctx, query, args...)
		if err != nil {
			return err
		}
		if n > 0 {
			matched = true
			return nil
		}
		var exists bool
		if err := tx.q.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM ledger_changesets WHERE id = ?)", id).Scan(&exists); err != nil {
			return mapErr(err)
		}
		if !exists {
			return store.ErrNotFound
		}
		return nil
	})
	return matched, err
}

func (o *ops) TouchLedgerChangeset(ctx context.Context, id string, at int64) (bool, error) {
	if at <= 0 {
		return false, invalid("TouchLedgerChangeset needs a time")
	}
	return o.changesetUpdate(ctx, id, `UPDATE ledger_changesets SET updated_at = MAX(updated_at, ?) WHERE id = ? AND state = 'open'`, at, id)
}

func (o *ops) PrepareLedgerMerge(ctx context.Context, id string, m store.LedgerMerge, at int64) (bool, error) {
	if err := checkLedgerMerge(m); err != nil {
		return false, err
	}
	if at <= 0 {
		return false, invalid("PrepareLedgerMerge needs a time")
	}
	return o.changesetUpdate(ctx, id, `UPDATE ledger_changesets SET merge_commit = ?, merge_parent = ?, code_commit = ?, numbers = ?,
		updated_at = MAX(updated_at, ?) WHERE id = ? AND state = 'open' AND merge_commit = ''`,
		m.Commit, m.Parent, m.CodeCommit, encodeLedgerNumbers(m.Numbers), at, id)
}

func (o *ops) ClearLedgerMerge(ctx context.Context, id, commit string) (bool, error) {
	if !store.IsLedgerCommitID(commit) {
		return false, invalid("invalid ledger merge commit")
	}
	return o.changesetUpdate(ctx, id, `UPDATE ledger_changesets SET merge_commit = '', merge_parent = '', code_commit = '', numbers = ''
		WHERE id = ? AND state = 'open' AND merge_commit = ?`, id, commit)
}

func (o *ops) FinishLedgerMerge(ctx context.Context, id, commit string, at int64) (bool, error) {
	if !store.IsLedgerCommitID(commit) || at <= 0 {
		return false, invalid("FinishLedgerMerge needs a commit and a time")
	}
	return o.changesetUpdate(ctx, id, `UPDATE ledger_changesets SET state = 'merged', updated_at = MAX(updated_at, ?)
		WHERE id = ? AND state = 'open' AND merge_commit = ?`, at, id, commit)
}

func (o *ops) DiscardLedgerChangeset(ctx context.Context, id, reason string, archived bool, at int64) (bool, error) {
	if reason == "" || len(reason) > store.MaxLedgerDiscardReasonBytes || !utf8.ValidString(reason) || strings.ContainsRune(reason, 0) || at <= 0 {
		return false, invalid("a discard needs a reason of 1 to %d bytes of UTF-8 and a time", store.MaxLedgerDiscardReasonBytes)
	}
	return o.changesetUpdate(ctx, id, `UPDATE ledger_changesets SET state = 'discarded', discard_reason = ?, archived = ?,
		updated_at = MAX(updated_at, ?) WHERE id = ? AND state = 'open' AND merge_commit = ''`, reason, archived, at, id)
}

func (o *ops) ArchiveLedgerChangeset(ctx context.Context, id string) error {
	_, err := o.changesetUpdate(ctx, id, `UPDATE ledger_changesets SET archived = 1 WHERE id = ? AND archived = 0`, id)
	return err
}

// purgeLedgerChangesets deletes the changesets of a deleted project, at
// most ledgerInstallRows rows per transaction. It continues when ctx ends,
// within the ledger cleanup budget.
func (o *ops) purgeLedgerChangesets(ctx context.Context, projectID string) error {
	ctx, cancel := ledgerCleanupContext(ctx)
	defer cancel()
	for {
		var n int
		err := o.unit(ctx, func(tx *ops) error {
			var exists bool
			if err := tx.q.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM projects WHERE id = ?)", projectID).Scan(&exists); err != nil {
				return mapErr(err)
			}
			if exists {
				return nil
			}
			var err error
			n, err = tx.exec(ctx, `DELETE FROM ledger_changesets WHERE id IN (
				SELECT id FROM ledger_changesets WHERE project_id = ? LIMIT ?)`, projectID, ledgerInstallRows)
			return err
		})
		if err == nil && o.db != nil && o.db.afterChangesetChunk != nil {
			o.db.afterChangesetChunk(n)
		}
		if err != nil || n < ledgerInstallRows {
			return err
		}
	}
}

func (o *ops) PurgeOrphanLedgerChangesets(ctx context.Context) error {
	for after := ""; ; {
		rows, err := o.q.QueryContext(ctx, `SELECT DISTINCT c.project_id FROM ledger_changesets c
			WHERE c.project_id > ? AND NOT EXISTS (SELECT 1 FROM projects p WHERE p.id = c.project_id)
			ORDER BY c.project_id LIMIT ?`, after, store.LedgerProjectPage)
		if err != nil {
			return mapErr(err)
		}
		ids, err := collect(rows, func(sc scanner) (string, error) {
			var id string
			return id, mapErr(sc.Scan(&id))
		})
		if err != nil || len(ids) == 0 {
			return err
		}
		for _, id := range ids {
			if err := o.purgeLedgerChangesets(ctx, id); err != nil {
				return err
			}
		}
		after = ids[len(ids)-1]
	}
}
