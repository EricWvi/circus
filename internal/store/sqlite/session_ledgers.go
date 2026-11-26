package sqlite

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/EricWvi/circus/internal/store"
)

const sessionLedgerCols = `l.session_id, l.user_id, l.project_id, l.device_id, l.state, l.path, l.error_code, l.error_detail,
	l.base_commit, l.view_commit, l.collected_at, l.due_at, l.known, l.problems, l.warnings, l.change_count, l.problem_count,
	l.noted_hash, l.pending, l.pending_changeset, l.pending_reason, l.rebase_target, l.attempts, l.retry_at, l.created_at, l.updated_at,
	COALESCE((SELECT c.id FROM ledger_changesets c WHERE c.session_id = l.session_id AND c.state = 'open'), '')`

func scanSessionLedger(s scanner) (store.SessionLedger, error) {
	var l store.SessionLedger
	var state, pending, known, problems, warnings string
	err := s.Scan(&l.SessionID, &l.UserID, &l.ProjectID, &l.DeviceID, &state, &l.Path, &l.ErrorCode, &l.ErrorDetail,
		&l.BaseCommit, &l.ViewCommit, &l.CollectedAt, &l.DueAt, &known, &problems, &warnings, &l.ChangeCount, &l.ProblemCount,
		&l.NotedHash, &pending, &l.PendingChangeset, &l.PendingReason, &l.RebaseTarget, &l.Attempts, &l.RetryAt, &l.CreatedAt, &l.UpdatedAt,
		&l.ChangesetID)
	if err != nil {
		return l, mapErr(err)
	}
	l.State, l.Pending = store.SessionLedgerState(state), store.SessionLedgerWork(pending)
	if err := errors.Join(json.Unmarshal([]byte(known), &l.Known), json.Unmarshal([]byte(problems), &l.Problems),
		json.Unmarshal([]byte(warnings), &l.Warnings)); err != nil {
		return l, fmt.Errorf("sqlite: corrupt session ledger %s: %w", l.SessionID, err)
	}
	// Empty lists read as nil, as they are given.
	if len(l.Known) == 0 {
		l.Known = nil
	}
	if len(l.Problems) == 0 {
		l.Problems = nil
	}
	if len(l.Warnings) == 0 {
		l.Warnings = nil
	}
	return l, nil
}

// encodeList encodes a list as JSON, "[]" when empty.
func encodeList[T any](list []T) (string, error) {
	if len(list) == 0 {
		return "[]", nil
	}
	b, err := json.Marshal(list)
	return string(b), err
}

func checkSessionLedger(l store.SessionLedger) error {
	for _, c := range []struct{ what, v string }{
		{"session id", l.SessionID}, {"user id", l.UserID}, {"project id", l.ProjectID}, {"device id", l.DeviceID},
	} {
		if err := store.CheckID(c.what, c.v); err != nil {
			return err
		}
	}
	switch {
	case !l.State.Valid():
		return invalid("unknown session ledger state %q", l.State)
	case l.State != store.SessionLedgerReady && l.Path != "":
		return invalid("only a ready snapshot has a path")
	case l.State == store.SessionLedgerReady && (l.Path == "" || len(l.Path) > 4096 || !utf8.ValidString(l.Path) || strings.ContainsRune(l.Path, 0)):
		return invalid("a ready snapshot has a path of at most 4096 bytes")
	case l.ErrorCode < 0 || len(l.ErrorDetail) > store.MaxSessionLedgerMessageBytes || !utf8.ValidString(l.ErrorDetail):
		return invalid("invalid session ledger error")
	case l.BaseCommit != "" && !store.IsLedgerCommitID(l.BaseCommit), l.ViewCommit != "" && !store.IsLedgerCommitID(l.ViewCommit),
		l.RebaseTarget != "" && !store.IsLedgerCommitID(l.RebaseTarget):
		return invalid("invalid session ledger commit")
	case l.DueAt < 0 || l.CollectedAt < 0 || l.Attempts < 0 || l.CreatedAt <= 0 || l.UpdatedAt < l.CreatedAt:
		return invalid("invalid session ledger times")
	case l.Pending != store.SessionLedgerNoWork && l.Pending != store.SessionLedgerRebase && l.Pending != store.SessionLedgerReset:
		return invalid("unknown session ledger work %q", l.Pending)
	case (l.Pending == store.SessionLedgerNoWork) != (l.PendingChangeset == ""):
		return invalid("pending work names its changeset")
	case len(l.PendingReason) > store.MaxLedgerDiscardReasonBytes || !utf8.ValidString(l.PendingReason):
		return invalid("invalid pending reason")
	case len(l.NotedHash) > 128:
		return invalid("noted hash is too long")
	}
	if err := store.CheckLedgerKnown(l.Known); err != nil {
		return err
	}
	if err := store.CheckLedgerProblems("problems", l.Problems); err != nil {
		return err
	}
	return store.CheckLedgerProblems("warnings", l.Warnings)
}

func (o *ops) CreateSessionLedger(ctx context.Context, l store.SessionLedger) error {
	if err := checkSessionLedger(l); err != nil {
		return err
	}
	known, err := encodeList(l.Known)
	if err != nil {
		return err
	}
	problems, err := encodeList(l.Problems)
	if err != nil {
		return err
	}
	warnings, err := encodeList(l.Warnings)
	if err != nil {
		return err
	}
	return o.unit(ctx, func(tx *ops) error {
		var exists bool
		if err := tx.q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sessions WHERE session_id = ? AND user_id = ?)`,
			l.SessionID, l.UserID).Scan(&exists); err != nil {
			return mapErr(err)
		}
		if !exists {
			return store.ErrNotFound
		}
		_, err := tx.exec(ctx, `INSERT INTO session_ledgers (session_id, user_id, project_id, device_id, state, path, error_code,
			error_detail, base_commit, view_commit, collected_at, due_at, known, problems, warnings, change_count, problem_count,
			noted_hash, pending, pending_changeset, pending_reason, rebase_target, attempts, retry_at, created_at, updated_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			l.SessionID, l.UserID, l.ProjectID, l.DeviceID, string(l.State), l.Path, l.ErrorCode, l.ErrorDetail,
			l.BaseCommit, l.ViewCommit, l.CollectedAt, l.DueAt, known, problems, warnings, l.ChangeCount, l.ProblemCount,
			l.NotedHash, string(l.Pending), l.PendingChangeset, l.PendingReason, l.RebaseTarget, l.Attempts, l.RetryAt, l.CreatedAt, l.UpdatedAt)
		return err
	})
}

func (o *ops) GetSessionLedger(ctx context.Context, sessionID string) (store.SessionLedger, error) {
	if err := store.CheckID("session id", sessionID); err != nil {
		return store.SessionLedger{}, err
	}
	return scanSessionLedger(o.q.QueryRow(ctx, `SELECT `+sessionLedgerCols+` FROM session_ledgers l WHERE l.session_id = ?`, sessionID))
}

func (o *ops) UpdateSessionLedger(ctx context.Context, l store.SessionLedger, expect int64) (bool, error) {
	if err := checkSessionLedger(l); err != nil {
		return false, err
	}
	if l.UpdatedAt <= expect {
		return false, invalid("UpdateSessionLedger needs a later time")
	}
	known, err := encodeList(l.Known)
	if err != nil {
		return false, err
	}
	problems, err := encodeList(l.Problems)
	if err != nil {
		return false, err
	}
	warnings, err := encodeList(l.Warnings)
	if err != nil {
		return false, err
	}
	var ok bool
	err = o.unit(ctx, func(tx *ops) error {
		n, err := tx.exec(ctx, `UPDATE session_ledgers SET state = ?, path = ?, error_code = ?, error_detail = ?, base_commit = ?,
			view_commit = ?, collected_at = ?, due_at = ?, known = ?, problems = ?, warnings = ?, change_count = ?, problem_count = ?,
			noted_hash = ?, pending = ?, pending_changeset = ?, pending_reason = ?, rebase_target = ?, attempts = ?, retry_at = ?,
			updated_at = ? WHERE session_id = ? AND updated_at = ?`,
			string(l.State), l.Path, l.ErrorCode, l.ErrorDetail, l.BaseCommit, l.ViewCommit, l.CollectedAt, l.DueAt, known, problems,
			warnings, l.ChangeCount, l.ProblemCount, l.NotedHash, string(l.Pending), l.PendingChangeset, l.PendingReason, l.RebaseTarget,
			l.Attempts, l.RetryAt, l.UpdatedAt, l.SessionID, expect)
		if err != nil {
			return err
		}
		if n > 0 {
			ok = true
			return nil
		}
		var exists bool
		if err := tx.q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM session_ledgers WHERE session_id = ?)`, l.SessionID).Scan(&exists); err != nil {
			return mapErr(err)
		}
		if !exists {
			return store.ErrNotFound
		}
		return nil
	})
	return ok, err
}

const sessionLedgerWork = `(l.state IN ('pending', 'preparing') OR (l.state = 'ready' AND (l.due_at > 0 OR l.pending <> '')))`

func (o *ops) ListSessionLedgers(ctx context.Context, f store.SessionLedgerFilter) ([]store.SessionLedger, error) {
	if err := store.CheckLimit(f.Limit); err != nil {
		return nil, err
	}
	if err := store.CheckOptionalID("device id", f.DeviceID); err != nil {
		return nil, err
	}
	query := `SELECT ` + sessionLedgerCols + ` FROM session_ledgers l WHERE ` + sessionLedgerWork + ` AND l.session_id > ?`
	args := []any{f.After}
	if f.DeviceID != "" {
		query += ` AND l.device_id = ?`
		args = append(args, f.DeviceID)
	}
	rows, err := o.q.QueryContext(ctx, query+` ORDER BY l.session_id LIMIT ?`, append(args, f.Limit)...)
	if err != nil {
		return nil, mapErr(err)
	}
	return collect(rows, scanSessionLedger)
}

func (o *ops) ListSessionLedgerStatuses(ctx context.Context, sessionIDs []string) (map[string]store.SessionLedger, error) {
	if len(sessionIDs) > store.MaxPageSize {
		return nil, invalid("too many session ids")
	}
	if err := store.CheckIDs("session id", sessionIDs); err != nil {
		return nil, err
	}
	out := make(map[string]store.SessionLedger, len(sessionIDs))
	if len(sessionIDs) == 0 {
		return out, nil
	}
	rows, err := o.q.QueryContext(ctx, `SELECT `+sessionLedgerCols+` FROM session_ledgers l WHERE l.session_id IN (`+
		placeholders(len(sessionIDs))+`)`, anys(sessionIDs)...)
	if err != nil {
		return nil, mapErr(err)
	}
	list, err := collect(rows, scanSessionLedger)
	if err != nil {
		return nil, err
	}
	for _, l := range list {
		out[l.SessionID] = l
	}
	return out, nil
}

func (o *ops) GetOpenSessionChangeset(ctx context.Context, sessionID string) (store.LedgerChangeset, error) {
	if err := store.CheckID("session id", sessionID); err != nil {
		return store.LedgerChangeset{}, err
	}
	return scanChangeset(o.q.QueryRow(ctx, `SELECT `+changesetCols+` FROM ledger_changesets
		WHERE session_id = ? AND state = 'open'`, sessionID))
}

func (o *ops) ListLedgerSnapshotRemovals(ctx context.Context, deviceID, after string, limit int) ([]store.LedgerSnapshotRemoval, error) {
	if err := store.CheckLimit(limit); err != nil {
		return nil, err
	}
	if err := store.CheckOptionalID("device id", deviceID); err != nil {
		return nil, err
	}
	query := `SELECT session_id, user_id, device_id, created_at FROM ledger_snapshot_removals WHERE session_id > ?`
	args := []any{after}
	if deviceID != "" {
		query += ` AND device_id = ?`
		args = append(args, deviceID)
	}
	rows, err := o.q.QueryContext(ctx, query+` ORDER BY session_id LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, mapErr(err)
	}
	return collect(rows, func(s scanner) (store.LedgerSnapshotRemoval, error) {
		var r store.LedgerSnapshotRemoval
		return r, mapErr(s.Scan(&r.SessionID, &r.UserID, &r.DeviceID, &r.CreatedAt))
	})
}

func (o *ops) GetLedgerSnapshotRemoval(ctx context.Context, sessionID string) (store.LedgerSnapshotRemoval, error) {
	if err := store.CheckID("session id", sessionID); err != nil {
		return store.LedgerSnapshotRemoval{}, err
	}
	var r store.LedgerSnapshotRemoval
	err := o.q.QueryRow(ctx, `SELECT session_id, user_id, device_id, created_at FROM ledger_snapshot_removals WHERE session_id = ?`,
		sessionID).Scan(&r.SessionID, &r.UserID, &r.DeviceID, &r.CreatedAt)
	return r, mapErr(err)
}

func (o *ops) DeleteLedgerSnapshotRemoval(ctx context.Context, sessionID string) error {
	if err := store.CheckID("session id", sessionID); err != nil {
		return err
	}
	_, err := o.exec(ctx, `DELETE FROM ledger_snapshot_removals WHERE session_id = ?`, sessionID)
	return err
}

func (o *ops) PruneLedgerSnapshotRemovals(ctx context.Context, before int64, limit int) (int, error) {
	if err := store.CheckLimit(limit); err != nil {
		return 0, err
	}
	return o.exec(ctx, `DELETE FROM ledger_snapshot_removals WHERE session_id IN (
		SELECT session_id FROM ledger_snapshot_removals WHERE created_at < ? ORDER BY created_at LIMIT ?)`, before, limit)
}

func (o *ops) CountNotesSinceUserInput(ctx context.Context, sessionID string) (int, error) {
	if err := store.CheckID("session id", sessionID); err != nil {
		return 0, err
	}
	return o.count(ctx, `SELECT COUNT(*) FROM session_inputs WHERE session_id = ?1 AND kind = 'note' AND deferred = 0
		AND created_at > COALESCE((SELECT MAX(created_at) FROM session_inputs WHERE session_id = ?1 AND kind IN ('user', 'answer')), 0)`, sessionID)
}

func (o *ops) ListOrphanSessionChangesets(ctx context.Context, after string, limit int) ([]store.LedgerChangeset, error) {
	if err := store.CheckLimit(limit); err != nil {
		return nil, err
	}
	rows, err := o.q.QueryContext(ctx, `SELECT `+changesetCols+` FROM ledger_changesets c
		WHERE c.session_id <> '' AND c.state = 'open' AND c.id > ? AND NOT EXISTS (SELECT 1 FROM sessions s WHERE s.session_id = c.session_id)
		ORDER BY c.id LIMIT ?`, after, limit)
	if err != nil {
		return nil, mapErr(err)
	}
	return collect(rows, scanChangeset)
}

func (o *ops) PruneLedgerCalls(ctx context.Context, limit int) (int, error) {
	if err := store.CheckLimit(limit); err != nil {
		return 0, err
	}
	return o.exec(ctx, `DELETE FROM calls WHERE call_id IN (SELECT c.call_id FROM calls c
		WHERE c.session_id GLOB 'ledger-*' AND c.call_id GLOB 'lg-*'
		AND c.state IN ('completed', 'offline', 'cancelled', 'rejected', 'lost')
		ORDER BY c.session_id, c.call_id LIMIT ?)`, limit)
}

// removeSessionLedger turns a deleted session's snapshot into a removal;
// the caller deletes the session in the same transaction.
func (o *ops) removeSessionLedger(ctx context.Context, sessionID string, now int64) error {
	_, err := o.exec(ctx, `INSERT OR IGNORE INTO ledger_snapshot_removals (session_id, user_id, device_id, created_at)
		SELECT session_id, user_id, device_id, ? FROM session_ledgers WHERE session_id = ?`, now, sessionID)
	return err
}

// maxLedgerEventBytes bounds a LEDGER event's payload.
const maxLedgerEventBytes = 4 << 10

// ledgerEventPayload is a LEDGER event's payload: numbers are dropped and
// the reason shortened when the whole would exceed maxLedgerEventBytes.
func ledgerEventPayload(event, changesetID string, numbers map[string]uint32, reason string) ([]byte, error) {
	type payload struct {
		Event       string            `json:"event"`
		ChangesetID string            `json:"changeset_id,omitempty"`
		Numbers     map[string]uint32 `json:"numbers,omitempty"`
		Reason      string            `json:"reason,omitempty"`
	}
	p := payload{Event: event, ChangesetID: changesetID, Reason: reason}
	if len(numbers) > 0 {
		p.Numbers = make(map[string]uint32, len(numbers))
		for letters, n := range numbers {
			p.Numbers["D?"+letters] = n
		}
	}
	encode := func() ([]byte, error) {
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		err := enc.Encode(p)
		return bytes.TrimSuffix(b.Bytes(), []byte("\n")), err
	}
	b, err := encode()
	for err == nil && len(b) > maxLedgerEventBytes {
		switch {
		case p.Numbers != nil:
			p.Numbers = nil
		default:
			n := len(p.Reason) / 2
			for n > 0 && !utf8.RuneStart(p.Reason[n]) {
				n--
			}
			p.Reason = p.Reason[:n]
		}
		b, err = encode()
	}
	return b, err
}

// sessionChangesetClosed records on the session of a merged or discarded
// changeset the work its snapshot owes, when it is ready (a failed one owes
// nothing), and a durable LEDGER event. A changeset of no session, or of a
// session without a snapshot, changes nothing.
func (o *ops) sessionChangesetClosed(ctx context.Context, changesetID string, work store.SessionLedgerWork, at int64) error {
	c, err := scanChangeset(o.q.QueryRow(ctx, `SELECT `+changesetCols+` FROM ledger_changesets WHERE id = ?`, changesetID))
	if err != nil || c.SessionID == "" {
		return err
	}
	var updated int64
	err = o.q.QueryRow(ctx, `SELECT updated_at FROM session_ledgers WHERE session_id = ?`, c.SessionID).Scan(&updated)
	if err != nil {
		if isNotFound(mapErr(err)) {
			return nil
		}
		return mapErr(err)
	}
	now := max(at, updated+1)
	event, reason := "merged", ""
	if work == store.SessionLedgerReset {
		event, reason = "discarded", c.DiscardReason
	}
	if _, err := o.exec(ctx, `UPDATE session_ledgers SET pending = ?, pending_changeset = ?, pending_reason = ?, rebase_target = '',
		attempts = 0, retry_at = 0, change_count = 0, problem_count = 0, problems = '[]', warnings = '[]', updated_at = ?
		WHERE session_id = ? AND state = 'ready'`,
		string(work), changesetID, reason, now, c.SessionID); err != nil {
		return err
	}
	body, err := ledgerEventPayload(event, changesetID, c.Merge.Numbers, reason)
	if err != nil {
		return err
	}
	_, err = o.AppendEvents(ctx, c.SessionID, nil, []store.EventInput{{Kind: store.EventKindLedger, Payload: body, At: now, Boundary: true}},
		store.DefaultMaxViewBytes)
	return err
}
