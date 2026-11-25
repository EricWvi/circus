package sqlite

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/EricWvi/circus/internal/store"
)

// savepoints numbers the savepoints unit opens; names only need to be
// unique within one connection's open transaction.
var savepoints atomic.Uint64

// unit runs fn all or nothing: in a new transaction when o is the pool, in
// a savepoint when o is already a transaction, so a session method that
// fails half way leaves nothing behind even when the caller's fn ignores
// the error and commits. The rollback runs without the caller's
// cancellation, so a cancelled ctx cannot leave the method's partial
// writes in the outer transaction.
func (o *ops) unit(ctx context.Context, fn func(*ops) error) error {
	if o.db != nil {
		return o.atomic(ctx, fn)
	}
	name := fmt.Sprintf("sp%d", savepoints.Add(1))
	if _, err := o.q.ExecContext(ctx, "SAVEPOINT "+name); err != nil {
		return mapErr(err)
	}
	if err := fn(o); err != nil {
		bg := context.WithoutCancel(ctx)
		_, _ = o.q.ExecContext(bg, "ROLLBACK TO "+name)
		_, _ = o.q.ExecContext(bg, "RELEASE "+name)
		return err
	}
	_, err := o.q.ExecContext(ctx, "RELEASE "+name)
	return mapErr(err)
}

// activeCallStates are the call states that may still execute.
const activeCallStates = `('accepted', 'dispatched', 'running', 'unknown')`

// Sessions.

// workspaceBlocked holds a session's turns until its workspace is ready
// and while its ledger snapshot, when it has one, holds them
// (store.SessionLedger.HoldsTurns): waiting for its device, prepared, or
// with a merge or discard still to apply that no problem stops.
const workspaceBlocked = `((sessions.workspace = 1 AND NOT EXISTS (SELECT 1 FROM workspaces w
	WHERE w.project_id = sessions.project_id AND w.device_id = sessions.default_device_id AND w.state = 'ready'))
	OR EXISTS (SELECT 1 FROM session_ledgers sl WHERE sl.session_id = sessions.session_id
		AND (sl.state IN ('pending', 'preparing') OR (sl.state = 'ready' AND sl.pending <> '' AND sl.problem_count = 0))))`

const sessionCols = `session_id, user_id, claude_session_id, state, default_device_id, model, permission_mode,
	pinned_worker_id, turn_count, last_turn_outcome, interrupt_turn_no, previous_turn_interrupted, transcript_gap_turn_no, input_tokens, output_tokens,
	cache_read_tokens, cache_create_tokens, cost_micro_usd, duration_ms, transcript_bytes, view_bytes,
	view_truncated, last_event_seq, dropped_event_batches, reason, created_at, updated_at, stopped_at,
	project_id, client_request_id, location_index, title, last_activity_at, workspace, local, pin_derived, provider_id, ` + workspaceBlocked + `,
	(SELECT COUNT(*) FROM session_approvals a WHERE a.session_id = sessions.session_id
		AND a.state = 'pending' AND a.expires_at > ?1)`

func scanSession(s scanner) (store.Session, error) {
	var x store.Session
	var state, outcome string
	var truncated int64
	err := s.Scan(&x.ID, &x.UserID, &x.ClaudeSessionID, &state, &x.DefaultDeviceID, nullStr{&x.Model},
		nullStr{&x.PermissionMode}, nullStr{&x.PinnedWorkerID}, &x.TurnCount, nullStr{&outcome}, &x.InterruptTurnNo, &x.PreviousTurnInterrupted, &x.TranscriptGapTurnNo,
		&x.Usage.InputTokens, &x.Usage.OutputTokens, &x.Usage.CacheReadInputTokens,
		&x.Usage.CacheCreationInputTokens, &x.Usage.CostMicroUSD, &x.Usage.DurationMs,
		&x.TranscriptBytes, &x.ViewBytes, &truncated, &x.LastEventSeq, &x.DroppedEventBatches, nullStr{&x.Reason},
		&x.CreatedAt, &x.UpdatedAt, nullInt{&x.StoppedAt},
		&x.ProjectID, nullStr{&x.ClientRequestID}, &x.LocationIndex, &x.Title, &x.LastActivityAt, &x.Workspace, &x.Local, &x.PinDerived, nullStr{&x.ProviderID}, &x.WorkspaceBlocked, &x.PendingApprovalCount)
	x.State = store.SessionState(state)
	x.LastTurnOutcome = store.TurnOutcome(outcome)
	x.ViewTruncated = truncated != 0
	return x, mapErr(err)
}

func (o *ops) getSession(ctx context.Context, id string, now int64) (store.Session, error) {
	return scanSession(o.q.QueryRow(ctx, `SELECT `+sessionCols+` FROM sessions WHERE session_id = ?`, now, id))
}

func (o *ops) setState(ctx context.Context, id string, state store.SessionState, now int64) error {
	_, err := o.exec(ctx, `UPDATE sessions SET state = ?, updated_at = ? WHERE session_id = ?`, string(state), now, id)
	return err
}

const sessionByClientRequestIDQuery = `SELECT ` + sessionCols + ` FROM sessions
	WHERE project_id = ? AND client_request_id = ?`

func (o *ops) GetSessionByClientRequestID(ctx context.Context, projectID, clientRequestID string, now int64) (store.Session, error) {
	if err := store.CheckID("project id", projectID); err != nil {
		return store.Session{}, err
	}
	if err := store.CheckID("client request id", clientRequestID); err != nil {
		return store.Session{}, err
	}
	if now == 0 {
		return store.Session{}, invalid("GetSessionByClientRequestID needs a time")
	}
	return scanSession(o.q.QueryRow(ctx, sessionByClientRequestIDQuery, now, projectID, clientRequestID))
}

func (o *ops) CreateSession(ctx context.Context, s store.Session, cwds []store.SessionCwd, maxSessions int) (store.SessionCreated, error) {
	var out store.SessionCreated
	for _, c := range []struct{ what, v string }{
		{"session id", s.ID}, {"user id", s.UserID}, {"project id", s.ProjectID}, {"claude session id", s.ClaudeSessionID}, {"default device id", s.DefaultDeviceID},
		{"client request id", s.ClientRequestID},
	} {
		if err := store.CheckID(c.what, c.v); err != nil {
			return out, err
		}
	}
	for _, c := range []struct{ what, v string }{
		{"model", s.Model}, {"permission mode", s.PermissionMode}, {"pinned worker id", s.PinnedWorkerID}, {"provider id", s.ProviderID},
	} {
		if err := store.CheckOptionalID(c.what, c.v); err != nil {
			return out, err
		}
	}
	if s.CreatedAt == 0 {
		return out, invalid("session needs a creation time")
	}
	if s.Local && s.PinnedWorkerID == "" {
		return out, invalid("a local session needs a pinned worker")
	}
	if s.PinDerived && !s.Local {
		return out, invalid("only a local session has a derived pin")
	}
	if s.State != "" && s.State != store.SessionIdle {
		return out, invalid("a new session starts idle, not %q", s.State)
	}
	if maxSessions < 1 {
		return out, invalid("maxSessions must be positive")
	}
	if len(cwds) > store.MaxSessionCwds {
		return out, invalid("at most %d working directories", store.MaxSessionCwds)
	}
	for _, c := range cwds {
		if err := checkCwd(c); err != nil {
			return out, err
		}
	}
	err := o.unit(ctx, func(tx *ops) error {
		var exists int
		if err := tx.q.QueryRow(ctx, `SELECT 1 FROM projects WHERE id = ? AND user_id = ?`, s.ProjectID, s.UserID).Scan(&exists); err != nil {
			return mapErr(err)
		}
		old, err := tx.GetSessionByClientRequestID(ctx, s.ProjectID, s.ClientRequestID, s.CreatedAt)
		switch {
		case err == nil:
			if !old.MatchesCreate(&s.LocationIndex, s.RequestedWorkerID(), s.Model, s.PermissionMode, &s.ProviderID) {
				return fmt.Errorf("%w: client request %q was used for a different session", store.ErrExists, s.ClientRequestID)
			}
			out.Session = old
			return nil
		case !isNotFound(err):
			return err
		}
		var n int
		if err := tx.q.QueryRow(ctx, `SELECT COUNT(*) FROM sessions WHERE user_id = ?`, s.UserID).Scan(&n); err != nil {
			return mapErr(err)
		}
		if n >= maxSessions {
			return fmt.Errorf("%w: user %q has %d sessions", store.ErrLimit, s.UserID, n)
		}
		if err := tx.checkProviderRef(ctx, s.UserID, s.ProviderID); err != nil {
			return err
		}
		if _, err := tx.exec(ctx, `INSERT INTO sessions (session_id, user_id, claude_session_id, state, default_device_id,
			model, permission_mode, pinned_worker_id, created_at, updated_at, project_id, last_activity_at,
			client_request_id, location_index, workspace, local, pin_derived, provider_id) VALUES (?,?,?,'idle',?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			s.ID, s.UserID, s.ClaudeSessionID, s.DefaultDeviceID, nstr(s.Model), nstr(s.PermissionMode),
			nstr(s.PinnedWorkerID), s.CreatedAt, s.CreatedAt, s.ProjectID, s.CreatedAt, s.ClientRequestID, s.LocationIndex, b2i(s.Workspace),
			b2i(s.Local), b2i(s.PinDerived), nstr(s.ProviderID)); err != nil {
			return err
		}
		if _, err := tx.exec(ctx, `INSERT INTO session_leases (session_id) VALUES (?)`, s.ID); err != nil {
			return err
		}
		for _, c := range cwds {
			if _, err := tx.exec(ctx, `INSERT INTO session_cwds (session_id, agent_id, device_id, cwd, updated_at)
				VALUES (?,?,?,?,?) ON CONFLICT DO UPDATE SET cwd = excluded.cwd`,
				s.ID, c.AgentID, c.DeviceID, c.Cwd, s.CreatedAt); err != nil {
				return err
			}
		}
		out.Session, err = tx.getSession(ctx, s.ID, s.CreatedAt)
		out.Created = err == nil
		return err
	})
	if err != nil {
		return store.SessionCreated{}, err
	}
	return out, nil
}

func checkCwd(c store.SessionCwd) error {
	if err := store.CheckOptionalID("agent id", c.AgentID); err != nil {
		return err
	}
	if err := store.CheckID("device id", c.DeviceID); err != nil {
		return err
	}
	return store.CheckText("working directory", c.Cwd, store.MaxPathBytes, false)
}

func (o *ops) GetSession(ctx context.Context, sessionID string, now int64) (store.Session, error) {
	if now == 0 {
		return store.Session{}, invalid("GetSession needs a time")
	}
	return o.getSession(ctx, sessionID, now)
}

func (o *ops) SetSessionTitle(ctx context.Context, sessionID, title string) error {
	if err := store.CheckID("session id", sessionID); err != nil {
		return err
	}
	if title == "" || len(title) > store.MaxSessionTitleBytes {
		return invalid("title must be 1..%d bytes", store.MaxSessionTitleBytes)
	}
	n, err := o.exec(ctx, `UPDATE sessions SET title = ? WHERE session_id = ?`, title, sessionID)
	if err == nil && n == 0 {
		return store.ErrNotFound
	}
	return err
}

func (o *ops) ListSessions(ctx context.Context, f store.SessionFilter) ([]store.Session, error) {
	if err := store.CheckLimit(f.Limit); err != nil {
		return nil, err
	}
	if err := store.CheckOptionalID("user id", f.UserID); err != nil {
		return nil, err
	}
	if err := store.CheckOptionalID("project id", f.ProjectID); err != nil {
		return nil, err
	}
	if err := store.CheckOptionalID("web user id", f.WebUserID); err != nil {
		return nil, err
	}
	if err := store.CheckOptionalID("provider id", f.ProviderID); err != nil {
		return nil, err
	}
	if f.Now == 0 {
		return nil, invalid("ListSessions needs a time")
	}
	var where []string
	args := []any{f.Now}
	if f.UserID != "" {
		where, args = append(where, "user_id = ?"), append(args, f.UserID)
	}
	if f.ProjectID != "" {
		where, args = append(where, "project_id = ?"), append(args, f.ProjectID)
	}
	if f.WebUserID != "" {
		where, args = append(where, "project_id IN (SELECT id FROM projects WHERE user_id = ?)"), append(args, f.WebUserID)
	}
	if f.ProviderID != "" {
		where, args = append(where, "provider_id = ?"), append(args, f.ProviderID)
	}
	if f.Runnable {
		where = append(where, "NOT "+workspaceBlocked)
	}
	if f.After != nil {
		where, args = append(where, "(last_activity_at, session_id) < (?, ?)"), append(args, f.After.LastActivityAt, f.After.ID)
	}
	if len(f.States) > 5 {
		return nil, invalid("too many session states")
	}
	if len(f.States) > 0 {
		for _, st := range f.States {
			if !st.Valid() {
				return nil, invalid("unknown session state %q", st)
			}
		}
		where, args = append(where, "state IN ("+placeholders(len(f.States))+")"), append(args, anys(f.States)...)
	}
	q := `SELECT ` + sessionCols + ` FROM sessions`
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, " AND ")
	}
	rows, err := o.q.QueryContext(ctx, q+` ORDER BY last_activity_at DESC, session_id DESC LIMIT ?`, append(args, f.Limit)...)
	if err != nil {
		return nil, mapErr(err)
	}
	return collect(rows, scanSession)
}

func (o *ops) ListSessionCwds(ctx context.Context, sessionID string) ([]store.SessionCwd, error) {
	if err := store.CheckID("session id", sessionID); err != nil {
		return nil, err
	}
	var out []store.SessionCwd
	err := o.unit(ctx, func(tx *ops) error {
		var one int
		if err := tx.q.QueryRow(ctx, `SELECT 1 FROM sessions WHERE session_id = ?`, sessionID).Scan(&one); err != nil {
			return mapErr(err)
		}
		rows, err := tx.q.QueryContext(ctx, `SELECT agent_id, device_id, cwd, updated_at FROM session_cwds
			WHERE session_id = ? AND agent_id = '' ORDER BY device_id LIMIT ?`, sessionID, store.MaxSessionCwds)
		if err != nil {
			return mapErr(err)
		}
		out, err = collect(rows, func(s scanner) (store.SessionCwd, error) {
			var c store.SessionCwd
			err := s.Scan(&c.AgentID, &c.DeviceID, &c.Cwd, &c.UpdatedAt)
			return c, mapErr(err)
		})
		return err
	})
	return out, err
}

func (o *ops) StopSession(ctx context.Context, sessionID string, at int64, reason string) (store.Session, store.Lease, bool, error) {
	if err := store.CheckID("session id", sessionID); err != nil {
		return store.Session{}, store.Lease{}, false, err
	}
	if at == 0 {
		return store.Session{}, store.Lease{}, false, invalid("StopSession needs a time")
	}
	if err := store.CheckCodes(reason, ""); err != nil {
		return store.Session{}, store.Lease{}, false, err
	}
	var s store.Session
	var l store.Lease
	var changed bool
	err := o.unit(ctx, func(tx *ops) error {
		var err error
		if s, err = tx.getSession(ctx, sessionID, at); err != nil {
			return err
		}
		if s.State != store.SessionStopped {
			if _, err := tx.exec(ctx, `UPDATE sessions SET state = 'stopped', stopped_at = ?, reason = ?, updated_at = ?
				WHERE session_id = ?`, at, nstr(reason), at, sessionID); err != nil {
				return err
			}
			changed = true
			if s, err = tx.getSession(ctx, sessionID, at); err != nil {
				return err
			}
		}
		l, err = tx.getLease(ctx, sessionID)
		return err
	})
	return s, l, changed, err
}

func (o *ops) InterruptSession(ctx context.Context, sessionID string, at int64) (store.Session, store.Lease, bool, error) {
	if err := store.CheckID("session id", sessionID); err != nil {
		return store.Session{}, store.Lease{}, false, err
	}
	if at == 0 {
		return store.Session{}, store.Lease{}, false, invalid("InterruptSession needs a time")
	}
	var s store.Session
	var l store.Lease
	var changed bool
	err := o.unit(ctx, func(tx *ops) error {
		n, err := tx.exec(ctx, `UPDATE sessions SET updated_at = ?2, interrupt_turn_no = (
				SELECT MAX(turn_no) FROM session_turns WHERE session_id = ?1 AND ended_at IS NULL)
			WHERE session_id = ?1 AND (SELECT MAX(turn_no) FROM session_turns WHERE session_id = ?1 AND ended_at IS NULL)
				> interrupt_turn_no`, sessionID, at)
		if err != nil {
			return err
		}
		changed = n > 0
		if s, err = tx.getSession(ctx, sessionID, at); err != nil {
			return err
		}
		l, err = tx.getLease(ctx, sessionID)
		return err
	})
	return s, l, changed, err
}

func (o *ops) DeleteSession(ctx context.Context, sessionID string, now int64) error {
	if err := store.CheckID("session id", sessionID); err != nil {
		return err
	}
	if now == 0 {
		return invalid("DeleteSession needs a time")
	}
	if o.db == nil {
		return invalid("DeleteSession runs its own transactions; call it on the store, not inside a Tx")
	}
	for {
		done := false
		err := o.unit(ctx, func(tx *ops) error {
			s, err := tx.deletable(ctx, sessionID, now)
			if err != nil {
				return err
			}
			// Transcript entries first, a chunk at a time, keeping every
			// byte counter in step.
			rows, err := tx.q.QueryContext(ctx, `DELETE FROM transcript_entries WHERE rowid IN (
				SELECT rowid FROM transcript_entries WHERE session_id = ? LIMIT ?) RETURNING subpath, bytes`,
				sessionID, store.DeleteChunkRows)
			if err != nil {
				return mapErr(err)
			}
			type gone struct {
				subpath string
				bytes   int64
			}
			removed, err := collect(rows, func(sc scanner) (gone, error) {
				var g gone
				return g, mapErr(sc.Scan(&g.subpath, &g.bytes))
			})
			if err != nil {
				return err
			}
			if len(removed) > 0 {
				per := map[string]int64{}
				var total int64
				for _, g := range removed {
					per[g.subpath] += g.bytes
					total += g.bytes
				}
				for sub, b := range per {
					if _, err := tx.exec(ctx, `UPDATE transcript_subpaths SET bytes = bytes - ? WHERE session_id = ? AND subpath = ?`,
						b, sessionID, sub); err != nil {
						return err
					}
				}
				return tx.chargeTranscript(ctx, sessionID, s.UserID, -total)
			}
			n, err := tx.exec(ctx, `DELETE FROM calls WHERE rowid IN (
				SELECT rowid FROM calls WHERE session_id = ? AND user_id = ? LIMIT ?)`, sessionID, s.UserID, store.DeleteChunkRows)
			if err != nil || n > 0 {
				return err
			}
			// The bounded rest: every other table hangs off the session.
			if _, err := tx.exec(ctx, `DELETE FROM jobs WHERE session_id = ? AND user_id = ? AND state <> 'running'`, sessionID, s.UserID); err != nil {
				return err
			}
			if err := tx.chargeTranscript(ctx, sessionID, s.UserID, -s.TranscriptBytes); err != nil {
				return err
			}
			// The snapshot on the device outlives the row: it is removed
			// later, when the device can be reached.
			if err := tx.removeSessionLedger(ctx, sessionID, now); err != nil {
				return err
			}
			_, err = tx.exec(ctx, `DELETE FROM sessions WHERE session_id = ?`, sessionID)
			done = err == nil
			return err
		})
		if err != nil || done {
			return err
		}
		if hook := o.db.afterDeleteChunk; hook != nil {
			if err := hook(); err != nil {
				return err
			}
		}
	}
}

func (o *ops) CheckSessionDeletable(ctx context.Context, sessionID string, now int64) error {
	if err := store.CheckID("session id", sessionID); err != nil {
		return err
	}
	if now == 0 {
		return invalid("CheckSessionDeletable needs a time")
	}
	return o.unit(ctx, func(tx *ops) error {
		_, err := tx.deletable(ctx, sessionID, now)
		return err
	})
}

// deletable returns the session when DeleteSession's preconditions hold.
func (o *ops) deletable(ctx context.Context, sessionID string, now int64) (store.Session, error) {
	s, err := o.getSession(ctx, sessionID, now)
	if err != nil {
		return s, err
	}
	if s.State != store.SessionStopped {
		return s, fmt.Errorf("%w: session %q is %s", store.ErrSessionActive, sessionID, s.State)
	}
	l, err := o.getLease(ctx, sessionID)
	if err != nil {
		return s, err
	}
	if l.Live(now) {
		return s, fmt.Errorf("%w: worker %q holds the lease", store.ErrSessionActive, l.WorkerID)
	}
	var active int
	if err := o.q.QueryRow(ctx, `SELECT COUNT(*) FROM calls WHERE session_id = ? AND user_id = ? AND state IN `+activeCallStates,
		sessionID, s.UserID).Scan(&active); err != nil {
		return s, mapErr(err)
	}
	if active > 0 {
		return s, fmt.Errorf("%w: %d calls have no outcome yet", store.ErrSessionActive, active)
	}
	return s, nil
}

// chargeTranscript adds delta to the session's and the user's transcript
// charge.
func (o *ops) chargeTranscript(ctx context.Context, sessionID, userID string, delta int64) error {
	if delta == 0 {
		return nil
	}
	if _, err := o.exec(ctx, `UPDATE sessions SET transcript_bytes = transcript_bytes + ? WHERE session_id = ?`, delta, sessionID); err != nil {
		return err
	}
	_, err := o.exec(ctx, `UPDATE users SET transcript_bytes = transcript_bytes + ? WHERE id = ?`, delta, userID)
	return err
}

func (o *ops) TranscriptRepaired(ctx context.Context, f store.Fence) (store.Session, bool, error) {
	var s store.Session
	var repaired bool
	err := o.fenced(ctx, f, func(tx *ops) error {
		n, err := tx.exec(ctx, `UPDATE sessions SET state = 'idle', broken_turn_no = 0, updated_at = ? WHERE session_id = ? AND state = 'broken'`,
			f.Now, f.SessionID)
		if err != nil {
			return err
		}
		repaired = n > 0
		s, err = tx.getSession(ctx, f.SessionID, f.Now)
		return err
	})
	return s, repaired, err
}

func (o *ops) SettleSession(ctx context.Context, sessionID string, now int64) (store.SessionSettled, error) {
	if err := store.CheckID("session id", sessionID); err != nil {
		return store.SessionSettled{}, err
	}
	if now == 0 {
		return store.SessionSettled{}, invalid("SettleSession needs a time")
	}
	var out store.SessionSettled
	err := o.unit(ctx, func(tx *ops) error {
		l, err := tx.getLease(ctx, sessionID)
		if err != nil {
			return err
		}
		if l.WorkerID != "" && l.ExpiresAt <= now {
			loss, err := tx.loseLease(ctx, l, now)
			if err != nil {
				return err
			}
			out.Loss = &loss
			if l, err = tx.getLease(ctx, sessionID); err != nil {
				return err
			}
		}
		s, err := tx.getSession(ctx, sessionID, now)
		if err != nil {
			return err
		}
		if s.State == store.SessionIdle && !l.Live(now) {
			work, err := tx.pendingWork(ctx, sessionID)
			if err != nil {
				return err
			}
			if work {
				if err := tx.setState(ctx, sessionID, store.SessionQueued, now); err != nil {
					return err
				}
				if s, err = tx.getSession(ctx, sessionID, now); err != nil {
					return err
				}
			}
		}
		out.Session, out.Lease = s, l
		return nil
	})
	return out, err
}

// pendingWork reports whether the session has work for a worker: a
// pending input, or a job that ended and whose notification was not
// handed.
func (o *ops) pendingWork(ctx context.Context, sessionID string) (bool, error) {
	var work bool
	err := o.q.QueryRow(ctx, `SELECT
		EXISTS (SELECT 1 FROM session_inputs WHERE session_id = ?1 AND state = 'pending')
		OR EXISTS (SELECT 1 FROM session_jobs sj JOIN jobs j
			ON j.device_id = sj.device_id AND j.boot_id = sj.boot_id AND j.job_id = sj.job_id
			WHERE sj.session_id = ?1 AND sj.notice = 'no' AND j.state <> 'running'
			AND j.user_id = (SELECT user_id FROM sessions WHERE session_id = ?1))`, sessionID).Scan(&work)
	return work, mapErr(err)
}

// Leases.

const leaseCols = `session_id, epoch, holder_worker_id, holder_boot_id, holder_conn_id, acquired_at, expires_at, provider_id`

func scanLease(s scanner) (store.Lease, error) {
	var l store.Lease
	err := s.Scan(&l.SessionID, &l.Epoch, nullStr{&l.WorkerID}, nullStr{&l.BootID}, nullStr{&l.ConnID},
		nullInt{&l.AcquiredAt}, nullInt{&l.ExpiresAt}, nullStr{&l.ProviderID})
	return l, mapErr(err)
}

func (o *ops) getLease(ctx context.Context, sessionID string) (store.Lease, error) {
	return scanLease(o.q.QueryRow(ctx, `SELECT `+leaseCols+` FROM session_leases WHERE session_id = ?`, sessionID))
}

// checkHolder validates the ids that name a holder's connection.
func checkHolder(worker, boot, conn string) error {
	for _, c := range []struct{ what, v string }{{"worker id", worker}, {"boot id", boot}, {"connection id", conn}} {
		if err := store.CheckID(c.what, c.v); err != nil {
			return err
		}
	}
	return nil
}

func checkFence(f store.Fence) error {
	if err := store.CheckID("session id", f.SessionID); err != nil {
		return err
	}
	if err := checkHolder(f.WorkerID, f.BootID, f.ConnID); err != nil {
		return err
	}
	if f.Epoch < 1 || f.Now == 0 {
		return invalid("fence needs an epoch of at least 1 and a time")
	}
	return nil
}

// fence returns the lease when f matches it, else *FencedError.
func (o *ops) fence(ctx context.Context, f store.Fence) (store.Lease, error) {
	l, err := o.getLease(ctx, f.SessionID)
	if err != nil {
		return l, err
	}
	if l.Epoch != f.Epoch || l.WorkerID != f.WorkerID || l.BootID != f.BootID || l.ConnID != f.ConnID || l.ExpiresAt <= f.Now {
		return l, &store.FencedError{Epoch: l.Epoch}
	}
	return l, nil
}

// loseLease clears l's holder and applies the lease-loss rule at now.
func (o *ops) loseLease(ctx context.Context, l store.Lease, now int64) (store.LeaseLoss, error) {
	loss := store.LeaseLoss{Released: l}
	if _, err := o.exec(ctx, `UPDATE session_leases SET holder_worker_id = NULL, holder_boot_id = NULL,
		holder_conn_id = NULL, acquired_at = NULL, expires_at = NULL, provider_id = NULL, released_worker_id = ?, released_boot_id = ?
		WHERE session_id = ?`, l.WorkerID, l.BootID, l.SessionID); err != nil {
		return loss, err
	}
	n, err := o.interruptOpenTurns(ctx, l.SessionID, now)
	if err != nil {
		return loss, err
	}
	loss.Interrupted = n > 0
	if loss.Interrupted {
		if _, err := o.exec(ctx, `UPDATE sessions SET last_turn_outcome = 'interrupted', previous_turn_interrupted = 1
			WHERE session_id = ?`, l.SessionID); err != nil {
			return loss, err
		}
	}
	// Taken inputs: the transcript decides whether the CLI got them.
	if _, err := o.exec(ctx, `UPDATE session_inputs SET state = 'persisted', text = NULL, persisted_at = ?2
		WHERE session_id = ?1 AND state = 'taken' AND EXISTS (
			SELECT 1 FROM transcript_entries t WHERE t.session_id = ?1 AND t.uuid = session_inputs.input_id)`,
		l.SessionID, now); err != nil {
		return loss, err
	}
	if _, err := o.exec(ctx, `UPDATE session_inputs SET state = 'pending', taken_epoch = NULL, taken_at = NULL
		WHERE session_id = ? AND state = 'taken'`, l.SessionID); err != nil {
		return loss, err
	}
	if _, err := o.exec(ctx, `UPDATE session_jobs SET notice = 'no', notice_at = NULL
		WHERE session_id = ? AND notice = 'handed'`, l.SessionID); err != nil {
		return loss, err
	}
	s, err := o.getSession(ctx, l.SessionID, now)
	if err != nil {
		return loss, err
	}
	if s.State.Refusal() == nil {
		work, err := o.pendingWork(ctx, l.SessionID)
		if err != nil {
			return loss, err
		}
		next := store.SessionIdle
		if work {
			next = store.SessionQueued
		}
		if err := o.setState(ctx, l.SessionID, next, now); err != nil {
			return loss, err
		}
	} else if _, err := o.exec(ctx, `UPDATE sessions SET updated_at = ? WHERE session_id = ?`, now, l.SessionID); err != nil {
		return loss, err
	}
	loss.Session, err = o.getSession(ctx, l.SessionID, now)
	return loss, err
}

func (o *ops) AcquireLease(ctx context.Context, r store.LeaseRequest) (store.LeaseAcquired, error) {
	if err := store.CheckID("session id", r.SessionID); err != nil {
		return store.LeaseAcquired{}, err
	}
	if err := checkHolder(r.WorkerID, r.BootID, r.ConnID); err != nil {
		return store.LeaseAcquired{}, err
	}
	if r.Now == 0 || r.ExpiresAt <= r.Now {
		return store.LeaseAcquired{}, invalid("AcquireLease needs a time and a later expiry")
	}
	var out store.LeaseAcquired
	err := o.unit(ctx, func(tx *ops) error {
		s, err := tx.getSession(ctx, r.SessionID, r.Now)
		if err != nil {
			return err
		}
		var owner string
		if err := tx.q.QueryRow(ctx, `SELECT user_id FROM workers WHERE id = ? AND deleted_at IS NULL`, r.WorkerID).
			Scan(&owner); err != nil || owner != s.UserID {
			if err == nil || isNotFound(mapErr(err)) {
				return fmt.Errorf("%w: worker %q of the session's user", store.ErrNotFound, r.WorkerID)
			}
			return mapErr(err)
		}
		if s.WorkspaceBlocked {
			return fmt.Errorf("%w: workspace or ledger snapshot is not ready", store.ErrLeaseHeld)
		}
		l, err := tx.getLease(ctx, r.SessionID)
		if err != nil {
			return err
		}
		own := l.Live(r.Now) && l.WorkerID == r.WorkerID && l.BootID == r.BootID
		if own && !r.Fresh {
			// The holder again: same epoch, now on this connection.
			if _, err := tx.exec(ctx, `UPDATE session_leases SET holder_conn_id = ?, expires_at = ? WHERE session_id = ?`,
				r.ConnID, r.ExpiresAt, r.SessionID); err != nil {
				return err
			}
			l.ConnID, l.ExpiresAt = r.ConnID, r.ExpiresAt
			out = store.LeaseAcquired{Lease: l, Session: s, Reattached: true}
			return nil
		}
		if err := s.State.Refusal(); err != nil {
			if own {
				// A fresh holder of a session that takes no new holder has
				// lost the state it needs to release: release for it, so the
				// lease cannot be left behind. The release commits; the
				// refusal is a result.
				loss, err := tx.loseLease(ctx, l, r.Now)
				if err != nil {
					return err
				}
				out = store.LeaseAcquired{Lease: loss.Released, Session: loss.Session, Loss: &loss,
					Released: true, Refusal: fmt.Errorf("%w: session %q", s.State.Refusal(), r.SessionID)}
				return nil
			}
			return fmt.Errorf("%w: session %q", err, r.SessionID)
		}
		if s.PinnedWorkerID != "" && s.PinnedWorkerID != r.WorkerID {
			return fmt.Errorf("%w: session is pinned to another worker", store.ErrLeaseHeld)
		}
		if l.Live(r.Now) && !own {
			return fmt.Errorf("%w: epoch %d", store.ErrLeaseHeld, l.Epoch)
		}
		if err := tx.gateProvider(ctx, s, r); err != nil {
			return err
		}
		if l.WorkerID != "" {
			loss, err := tx.loseLease(ctx, l, r.Now)
			if err != nil {
				return err
			}
			out.Loss = &loss
		}
		if _, err := tx.exec(ctx, `UPDATE session_leases SET epoch = epoch + 1, holder_worker_id = ?, holder_boot_id = ?,
			holder_conn_id = ?, acquired_at = ?, expires_at = ?, provider_id = ? WHERE session_id = ?`,
			r.WorkerID, r.BootID, r.ConnID, r.Now, r.ExpiresAt, nstr(s.ProviderID), r.SessionID); err != nil {
			return err
		}
		if _, err := tx.exec(ctx, `UPDATE sessions SET state = CASE state WHEN 'queued' THEN 'idle' ELSE state END,
			updated_at = ? WHERE session_id = ?`, r.Now, r.SessionID); err != nil {
			return err
		}
		if out.Lease, err = tx.getLease(ctx, r.SessionID); err != nil {
			return err
		}
		out.Session, err = tx.getSession(ctx, r.SessionID, r.Now)
		return err
	})
	if err != nil {
		return store.LeaseAcquired{}, err
	}
	return out, nil
}

// gateProvider checks a new holder of session s against its provider:
// ErrProviderUnavailable when the provider is gone or not assigned to the
// requesting worker, ErrProviderFull when MaxSessions other sessions hold
// live leases under it. Local login is not gated.
func (o *ops) gateProvider(ctx context.Context, s store.Session, r store.LeaseRequest) error {
	if s.ProviderID == "" {
		return nil
	}
	var maxSessions int
	err := o.q.QueryRow(ctx, `SELECT max_sessions FROM providers WHERE id = ? AND user_id = ?`, s.ProviderID, s.UserID).Scan(&maxSessions)
	if isNotFound(mapErr(err)) {
		return fmt.Errorf("%w: provider deleted", store.ErrProviderUnavailable)
	}
	if err != nil {
		return mapErr(err)
	}
	assigned, err := o.count(ctx, `SELECT COUNT(*) FROM provider_workers WHERE provider_id = ? AND worker_id = ?`, s.ProviderID, r.WorkerID)
	if err != nil {
		return err
	}
	if assigned == 0 {
		return fmt.Errorf("%w: provider not assigned to worker %q", store.ErrProviderUnavailable, r.WorkerID)
	}
	if maxSessions == 0 {
		return nil
	}
	held, err := o.count(ctx, `SELECT COUNT(*) FROM session_leases WHERE provider_id = ? AND holder_worker_id IS NOT NULL
		AND expires_at > ? AND session_id <> ?`, s.ProviderID, r.Now, s.ID)
	if err != nil {
		return err
	}
	if held >= maxSessions {
		return fmt.Errorf("%w: %d of %d", store.ErrProviderFull, held, maxSessions)
	}
	return nil
}

func (o *ops) SetSessionProvider(ctx context.Context, sessionID, providerID string, at int64) (store.SessionProviderSet, error) {
	if err := store.CheckID("session id", sessionID); err != nil {
		return store.SessionProviderSet{}, err
	}
	if err := store.CheckOptionalID("provider id", providerID); err != nil {
		return store.SessionProviderSet{}, err
	}
	if at == 0 {
		return store.SessionProviderSet{}, invalid("SetSessionProvider needs a time")
	}
	var out store.SessionProviderSet
	err := o.unit(ctx, func(tx *ops) error {
		s, err := tx.getSession(ctx, sessionID, at)
		if err != nil {
			return err
		}
		if s.ProviderID == providerID {
			// No change, in any state, even for a provider deleted since.
			out.Session = s
			return nil
		}
		switch s.State {
		case store.SessionRunning:
			return store.ErrTurnRunning
		case store.SessionStopped:
			return fmt.Errorf("%w: session %q", store.ErrStopped, sessionID)
		}
		if err := tx.checkProviderRef(ctx, s.UserID, providerID); err != nil {
			return err
		}
		if _, err := tx.exec(ctx, `UPDATE sessions SET provider_id = ?, updated_at = ? WHERE session_id = ?`,
			nstr(providerID), at, sessionID); err != nil {
			return err
		}
		l, err := tx.getLease(ctx, sessionID)
		if err != nil {
			return err
		}
		if l.WorkerID != "" {
			loss, err := tx.loseLease(ctx, l, at)
			if err != nil {
				return err
			}
			out.Loss = &loss
		}
		out.Changed = true
		out.Session, err = tx.getSession(ctx, sessionID, at)
		return err
	})
	if err != nil {
		return store.SessionProviderSet{}, err
	}
	return out, nil
}

func (o *ops) HasLiveLease(ctx context.Context, workerID, bootID string, now int64) (bool, error) {
	if err := store.CheckID("worker id", workerID); err != nil {
		return false, err
	}
	if err := store.CheckID("boot id", bootID); err != nil {
		return false, err
	}
	var live bool
	err := o.q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM session_leases
		WHERE holder_worker_id = ? AND holder_boot_id = ? AND expires_at > ?)`, workerID, bootID, now).Scan(&live)
	return live, mapErr(err)
}

func (o *ops) GetLease(ctx context.Context, sessionID string) (store.Lease, error) {
	return o.getLease(ctx, sessionID)
}

func (o *ops) GetRecovery(ctx context.Context, f store.Fence) (store.Recovery, error) {
	var out store.Recovery
	err := o.fenced(ctx, f, func(tx *ops) error {
		s, err := tx.getSession(ctx, f.SessionID, f.Now)
		if err != nil {
			return err
		}
		out.LastTurnInterrupted = s.PreviousTurnInterrupted
		rows, err := tx.q.QueryContext(ctx, `SELECT `+inputCols+` FROM session_inputs
			WHERE session_id = ? AND state = 'taken' ORDER BY created_at, input_id`, f.SessionID)
		if err != nil {
			return mapErr(err)
		}
		if out.Inputs, err = collect(rows, scanInput); err != nil {
			return err
		}
		rows, err = tx.q.QueryContext(ctx, sessionJobQuery+` AND sj.notice = 'handed' ORDER BY sj.call_id`, f.SessionID)
		if err != nil {
			return mapErr(err)
		}
		if out.Jobs, err = collect(rows, scanSessionJob); err != nil {
			return err
		}
		rows, err = tx.q.QueryContext(ctx, `SELECT `+bgTaskCols+` FROM session_bgtasks
			WHERE session_id = ? AND epoch < ? ORDER BY task_id`, f.SessionID, f.Epoch)
		if err != nil {
			return mapErr(err)
		}
		if out.LostBgTasks, err = collect(rows, scanBgTask); err != nil {
			return err
		}
		rows, err = tx.q.QueryContext(ctx, `SELECT `+callCols+` FROM calls
			WHERE session_id = ? AND user_id = ? AND (persisted_at IS NULL OR notice_owed = 1)
			ORDER BY submitted_at, call_id LIMIT ?`, f.SessionID, s.UserID, store.MaxRecoveryCalls+1)
		if err != nil {
			return mapErr(err)
		}
		if out.Calls, err = collect(rows, scanCall); err != nil {
			return err
		}
		if len(out.Calls) > store.MaxRecoveryCalls {
			out.Calls, out.CallsTruncated = out.Calls[:store.MaxRecoveryCalls], true
		}
		return nil
	})
	if err != nil {
		return store.Recovery{}, err
	}
	return out, nil
}

func (o *ops) ExtendLeases(ctx context.Context, keys []store.LeaseKey, now, expiresAt int64) ([]store.LeaseKey, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	if len(keys) > store.MaxPageSize || now == 0 || expiresAt <= now {
		return nil, invalid("ExtendLeases takes 1..%d keys, a time and a later expiry", store.MaxPageSize)
	}
	args := []any{expiresAt, now}
	values := make([]string, len(keys))
	for i, k := range keys {
		if err := store.CheckID("session id", k.SessionID); err != nil {
			return nil, err
		}
		if err := checkHolder(k.WorkerID, k.BootID, k.ConnID); err != nil {
			return nil, err
		}
		if k.Epoch < 1 {
			return nil, invalid("lease key needs an epoch")
		}
		values[i] = "(?,?,?,?,?)"
		args = append(args, k.SessionID, k.Epoch, k.WorkerID, k.BootID, k.ConnID)
	}
	// Never backwards: an extension computed earlier (a delivery's) that
	// commits after a later one keeps the later expiry.
	rows, err := o.q.QueryContext(ctx, `UPDATE session_leases SET expires_at = MAX(expires_at, ?)
		WHERE expires_at > ? AND (session_id, epoch, holder_worker_id, holder_boot_id, holder_conn_id) IN (VALUES `+
		strings.Join(values, ",")+`) RETURNING session_id, epoch, holder_worker_id, holder_boot_id, holder_conn_id`, args...)
	if err != nil {
		return nil, mapErr(err)
	}
	done, err := collect(rows, func(s scanner) (store.LeaseKey, error) {
		var k store.LeaseKey
		return k, mapErr(s.Scan(&k.SessionID, &k.Epoch, &k.WorkerID, &k.BootID, &k.ConnID))
	})
	if err != nil {
		return nil, err
	}
	extended := make(map[store.LeaseKey]bool, len(done))
	for _, k := range done {
		extended[k] = true
	}
	var lost []store.LeaseKey
	for _, k := range keys {
		if !extended[k] {
			lost = append(lost, k)
		}
	}
	return lost, nil
}

func (o *ops) ReleaseLease(ctx context.Context, f store.Fence) (store.LeaseLoss, error) {
	if err := checkFence(f); err != nil {
		return store.LeaseLoss{}, err
	}
	var loss store.LeaseLoss
	err := o.unit(ctx, func(tx *ops) error {
		var l store.Lease
		var releasedWorker, releasedBoot string
		if err := tx.q.QueryRow(ctx, `SELECT `+leaseCols+`, released_worker_id, released_boot_id FROM session_leases
			WHERE session_id = ?`, f.SessionID).Scan(&l.SessionID, &l.Epoch, nullStr{&l.WorkerID}, nullStr{&l.BootID},
			nullStr{&l.ConnID}, nullInt{&l.AcquiredAt}, nullInt{&l.ExpiresAt}, nullStr{&l.ProviderID}, nullStr{&releasedWorker}, nullStr{&releasedBoot}); err != nil {
			return mapErr(err)
		}
		if l.WorkerID == "" && l.Epoch == f.Epoch && releasedWorker == f.WorkerID && releasedBoot == f.BootID {
			// A resent release: the lease is already gone.
			loss.Released, loss.Repeated = l, true
			var err error
			loss.Session, err = tx.getSession(ctx, f.SessionID, f.Now)
			return err
		}
		var err error
		if l, err = tx.fence(ctx, f); err != nil {
			return err
		}
		loss, err = tx.loseLease(ctx, l, f.Now)
		return err
	})
	return loss, err
}

// loseMany applies the lease-loss rule to every lease the query selects.
func (o *ops) loseMany(ctx context.Context, now int64, query string, args ...any) ([]store.LeaseLoss, error) {
	var out []store.LeaseLoss
	err := o.unit(ctx, func(tx *ops) error {
		rows, err := tx.q.QueryContext(ctx, `SELECT `+leaseCols+` FROM session_leases `+query, args...)
		if err != nil {
			return mapErr(err)
		}
		leases, err := collect(rows, scanLease)
		if err != nil {
			return err
		}
		for _, l := range leases {
			loss, err := tx.loseLease(ctx, l, now)
			if err != nil {
				return err
			}
			out = append(out, loss)
		}
		return nil
	})
	return out, err
}

func (o *ops) ExtendLeasesAfterRestart(ctx context.Context, now, expiresAt int64, after *store.RestartCursor, limit int) (int, *store.RestartCursor, error) {
	if err := store.CheckLimit(limit); err != nil {
		return 0, nil, err
	}
	if now == 0 || expiresAt <= now {
		return 0, nil, invalid("ExtendLeasesAfterRestart needs a time and a later expiry")
	}
	var n int
	var next *store.RestartCursor
	err := o.unit(ctx, func(tx *ops) error {
		var c store.RestartCursor
		if after != nil {
			c = *after
		}
		n, next = 0, nil
		if !c.Providers {
			// Unexpired leases and expired ones without a provider, in
			// session order from the cursor.
			rows, err := tx.q.QueryContext(ctx, `UPDATE session_leases SET expires_at = ?1 WHERE session_id IN (
				SELECT session_id FROM session_leases WHERE session_id > ?3 AND holder_worker_id IS NOT NULL
					AND expires_at < ?1 AND (expires_at > ?2 OR provider_id IS NULL)
				ORDER BY session_id LIMIT ?4) RETURNING session_id`, expiresAt, now, c.SessionID, limit)
			if err != nil {
				return mapErr(err)
			}
			ids, err := collect(rows, func(s scanner) (string, error) {
				var id string
				return id, mapErr(s.Scan(&id))
			})
			if err != nil {
				return err
			}
			n = len(ids)
			if n == limit {
				c.SessionID = slices.Max(ids)
				next = &c
				return nil
			}
			c = store.RestartCursor{Providers: true}
		}
		// Expired leases under a provider that still exists: of each
		// provider's, the oldest acquisitions that fit under its cap next
		// to its live leases (all of them without a cap). A provider is
		// taken up once its earlier ones are done; the leases a page
		// extends count as live for the next, so pages extend the same
		// leases as one pass. A full or deleted provider is passed over in
		// one step.
		for n < limit {
			if !c.Within {
				var p string
				err := tx.q.QueryRow(ctx, `SELECT provider_id FROM session_leases WHERE provider_id > ?
					AND holder_worker_id IS NOT NULL AND expires_at <= ? ORDER BY provider_id LIMIT 1`, c.ProviderID, now).Scan(&p)
				if errors.Is(err, sql.ErrNoRows) {
					return nil // the run is done
				}
				if err != nil {
					return mapErr(err)
				}
				c = store.RestartCursor{Providers: true, ProviderID: p}
			}
			room := limit - n
			var capacity int
			err := tx.q.QueryRow(ctx, `SELECT max_sessions FROM providers WHERE id = ?`, c.ProviderID).Scan(&capacity)
			if errors.Is(err, sql.ErrNoRows) {
				c.Within = false // deleted: left to ExpireLeases
				continue
			}
			if err != nil {
				return mapErr(err)
			}
			if capacity > 0 {
				live, err := tx.count(ctx, `SELECT COUNT(*) FROM session_leases WHERE provider_id = ?
					AND holder_worker_id IS NOT NULL AND expires_at > ?`, c.ProviderID, now)
				if err != nil {
					return err
				}
				if room = min(room, capacity-live); room <= 0 {
					c.Within = false
					continue
				}
			}
			// Within a provider the page seeks past the leases it already
			// passed, in the index's order, rather than reading them again.
			from := ""
			if c.Within {
				from = "AND (acquired_at, session_id) > (?5, ?6)"
			}
			rows, err := tx.q.QueryContext(ctx, `UPDATE session_leases SET expires_at = ?1 WHERE session_id IN (
				SELECT session_id FROM session_leases INDEXED BY session_leases_provider_age
				WHERE provider_id = ?3 AND holder_worker_id IS NOT NULL AND expires_at <= ?2 `+from+`
				ORDER BY acquired_at, session_id LIMIT ?4) RETURNING acquired_at, session_id`,
				expiresAt, now, c.ProviderID, room, c.AcquiredAt, c.SessionID)
			if err != nil {
				return mapErr(err)
			}
			type key struct {
				at int64
				id string
			}
			keys, err := collect(rows, func(s scanner) (key, error) {
				var k key
				return k, mapErr(s.Scan(&k.at, &k.id))
			})
			if err != nil {
				return err
			}
			n += len(keys)
			if len(keys) < room {
				c.Within = false // the provider's expired leases are done
				continue
			}
			last := slices.MaxFunc(keys, func(a, b key) int {
				if a.at != b.at {
					return cmp.Compare(a.at, b.at)
				}
				return strings.Compare(a.id, b.id)
			})
			c.Within, c.AcquiredAt, c.SessionID = true, last.at, last.id
		}
		next = &c
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	return n, next, nil
}

func (o *ops) RecoverSession(ctx context.Context, sessionID string, accept bool, at int64, maxViewBytes int64) (store.SessionRecovered, error) {
	if err := store.CheckID("session id", sessionID); err != nil {
		return store.SessionRecovered{}, err
	}
	if !accept {
		return store.SessionRecovered{}, invalid("RecoverSession needs the transcript gap accepted")
	}
	if at == 0 || maxViewBytes < 1 {
		return store.SessionRecovered{}, invalid("RecoverSession needs a time and a positive view cap")
	}
	var out store.SessionRecovered
	err := o.unit(ctx, func(tx *ops) error {
		s, err := tx.getSession(ctx, sessionID, at)
		if err != nil {
			return err
		}
		if s.State != store.SessionBroken {
			return fmt.Errorf("%w: session %q is %s", store.ErrNotBroken, sessionID, s.State)
		}
		l, err := tx.getLease(ctx, sessionID)
		if err != nil {
			return err
		}
		if l.WorkerID != "" {
			loss, err := tx.loseLease(ctx, l, at)
			if err != nil {
				return err
			}
			out.Loss = &loss
		}
		work, err := tx.pendingWork(ctx, sessionID)
		if err != nil {
			return err
		}
		next := store.SessionIdle
		if work {
			next = store.SessionQueued
		}
		// The accepted gap is the turn that broke; the model learns of it
		// from the next holder's reminder, which acknowledges the mark.
		if _, err := tx.exec(ctx, `UPDATE sessions SET state = ?, transcript_gap_turn_no = broken_turn_no,
			broken_turn_no = 0, updated_at = ? WHERE session_id = ?`, string(next), at, sessionID); err != nil {
			return err
		}
		seqs, err := tx.storeEvents(ctx, sessionID, []store.EventInput{{Kind: store.EventKindTranscriptGapAccepted, At: at, Boundary: true}}, maxViewBytes)
		if err != nil {
			return err
		}
		out.EventSeq = seqs[0]
		out.Session, err = tx.getSession(ctx, sessionID, at)
		if out.Loss != nil {
			out.Loss.Session = out.Session
		}
		return err
	})
	if err != nil {
		return store.SessionRecovered{}, err
	}
	return out, nil
}

func (o *ops) ReleaseWorkerLeases(ctx context.Context, workerID, keepBootID string, now int64, limit int) ([]store.LeaseLoss, error) {
	if err := store.CheckLimit(limit); err != nil {
		return nil, err
	}
	if err := store.CheckID("worker id", workerID); err != nil {
		return nil, err
	}
	if err := store.CheckID("boot id", keepBootID); err != nil {
		return nil, err
	}
	if now == 0 {
		return nil, invalid("ReleaseWorkerLeases needs a time")
	}
	return o.loseMany(ctx, now, `WHERE holder_worker_id = ? AND holder_boot_id <> ?
		ORDER BY acquired_at, session_id LIMIT ?`, workerID, keepBootID, limit)
}

func (o *ops) ExpireLeases(ctx context.Context, now int64, limit int) ([]store.LeaseLoss, error) {
	if err := store.CheckLimit(limit); err != nil {
		return nil, err
	}
	if now == 0 {
		return nil, invalid("ExpireLeases needs a time")
	}
	return o.loseMany(ctx, now, `WHERE holder_worker_id IS NOT NULL AND expires_at <= ?
		ORDER BY expires_at, session_id LIMIT ?`, now, limit)
}

func (o *ops) ResumeLeases(ctx context.Context, workerID, bootID, connID string, keys []store.SessionEpoch, now, expiresAt int64) ([]store.LeaseResumed, error) {
	if err := checkHolder(workerID, bootID, connID); err != nil {
		return nil, err
	}
	if now == 0 || expiresAt <= now || len(keys) > store.MaxResumeLeases {
		return nil, invalid("ResumeLeases needs a time, a later expiry and at most %d keys", store.MaxResumeLeases)
	}
	for _, k := range keys {
		if err := store.CheckID("session id", k.SessionID); err != nil {
			return nil, err
		}
		if k.Epoch < 1 {
			return nil, invalid("resume key needs an epoch")
		}
	}
	out := make([]store.LeaseResumed, len(keys))
	err := o.unit(ctx, func(tx *ops) error {
		for i, k := range keys {
			out[i].SessionID = k.SessionID
			l, err := tx.getLease(ctx, k.SessionID)
			if isNotFound(err) {
				out[i].Err = err
				continue
			}
			if err != nil {
				return err
			}
			// Any connection of the boot may resume; the lease then
			// answers this one.
			if l.Epoch != k.Epoch || l.WorkerID != workerID || l.BootID != bootID || l.ExpiresAt <= now {
				out[i].Err = &store.FencedError{Epoch: l.Epoch}
				continue
			}
			if _, err := tx.exec(ctx, `UPDATE session_leases SET holder_conn_id = ?, expires_at = ? WHERE session_id = ?`,
				connID, expiresAt, k.SessionID); err != nil {
				return err
			}
			l.ConnID, l.ExpiresAt = connID, expiresAt
			s, err := tx.getSession(ctx, k.SessionID, now)
			if err != nil {
				return err
			}
			out[i].Lease, out[i].Stopped = l, s.State == store.SessionStopped
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (o *ops) CheckFence(ctx context.Context, f store.Fence) (store.Lease, error) {
	if err := checkFence(f); err != nil {
		return store.Lease{}, err
	}
	return o.fence(ctx, f)
}

func (o *ops) AdmitSessionCall(ctx context.Context, f store.Fence, callID string, maxActive int) error {
	if err := store.CheckID("call id", callID); err != nil {
		return err
	}
	if maxActive < 1 {
		return invalid("maxActive must be positive")
	}
	return o.fenced(ctx, f, func(tx *ops) error {
		s, err := tx.getSession(ctx, f.SessionID, f.Now)
		if err != nil {
			return err
		}
		if err := s.State.Refusal(); err != nil {
			return fmt.Errorf("%w: session %q", err, f.SessionID)
		}
		var sessionID, userID string
		err = mapErr(tx.q.QueryRow(ctx, `SELECT session_id, user_id FROM calls WHERE call_id = ?`, callID).Scan(&sessionID, &userID))
		switch {
		case err == nil && (sessionID != f.SessionID || userID != s.UserID):
			return fmt.Errorf("%w: call %q of this session", store.ErrNotFound, callID)
		case err != nil && !isNotFound(err):
			return err
		}
		var active int
		if err := tx.q.QueryRow(ctx, `SELECT COUNT(*) FROM calls WHERE session_id = ? AND user_id = ? AND call_id <> ? AND state IN `+
			activeCallStates, f.SessionID, s.UserID, callID).Scan(&active); err != nil {
			return mapErr(err)
		}
		if active >= maxActive {
			return fmt.Errorf("%w: %d of %d", store.ErrTooManyCalls, active, maxActive)
		}
		var unpersisted int
		if err := tx.q.QueryRow(ctx, `SELECT COUNT(*) FROM calls WHERE session_id = ? AND user_id = ? AND call_id <> ? AND persisted_at IS NULL`,
			f.SessionID, s.UserID, callID).Scan(&unpersisted); err != nil {
			return mapErr(err)
		}
		if unpersisted >= store.MaxUnpersistedSessionCalls {
			return fmt.Errorf("%w: %d calls not yet persisted", store.ErrTooManyCalls, unpersisted)
		}
		return nil
	})
}

// fenced runs fn in a unit after checking f.
func (o *ops) fenced(ctx context.Context, f store.Fence, fn func(tx *ops) error) error {
	if err := checkFence(f); err != nil {
		return err
	}
	return o.unit(ctx, func(tx *ops) error {
		if _, err := tx.fence(ctx, f); err != nil {
			return err
		}
		return fn(tx)
	})
}

// idArgs returns ids as query arguments; they are already checked.
func idArgs(ids []string) []any {
	out := make([]any, len(ids))
	for i, id := range ids {
		out[i] = id
	}
	return out
}

func isNotFound(err error) bool { return errors.Is(err, store.ErrNotFound) }
