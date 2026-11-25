package sqlite

import (
	"context"
	"fmt"

	"github.com/EricWvi/circus/internal/store"
)

// Inputs.

const inputCols = `input_id, session_id, client_msg_id, kind, text, priority, state, taken_epoch,
	created_at, taken_at, persisted_at, event_seq, note, deferred`

func scanInput(s scanner) (store.Input, error) {
	var in store.Input
	var kind, priority, state string
	err := s.Scan(&in.InputID, &in.SessionID, &in.ClientMsgID, &kind, nullStr{&in.Text}, &priority, &state,
		nullInt{&in.TakenEpoch}, &in.CreatedAt, nullInt{&in.TakenAt}, nullInt{&in.PersistedAt}, &in.EventSeq, &in.Note, &in.Deferred)
	in.Kind, in.Priority, in.State = store.InputKind(kind), store.InputPriority(priority), store.InputState(state)
	return in, mapErr(err)
}

// validNote accepts an empty note or a note category.
func validNote(note string) bool {
	if len(note) > store.MaxInputNoteBytes {
		return false
	}
	for i := range len(note) {
		if c := note[i]; !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_') {
			return false
		}
	}
	return true
}

// approvalMsgPrefix prefixes the client message id of an approval's retry
// input.
const approvalMsgPrefix = "approval:"

func checkInput(in store.Input) error {
	if err := store.CheckID("input id", in.InputID); err != nil {
		return err
	}
	if err := store.CheckID("session id", in.SessionID); err != nil {
		return err
	}
	maxClient := store.MaxIDBytes
	if in.Kind == store.InputApproval {
		maxClient += len(approvalMsgPrefix)
	}
	if err := store.CheckText("client message id", in.ClientMsgID, maxClient, false); err != nil {
		return err
	}
	if in.CreatedAt == 0 {
		return invalid("input needs a creation time")
	}
	if in.Kind != store.InputUser && in.Kind != store.InputApproval && in.Kind != store.InputNote {
		return invalid("unknown input kind %q", in.Kind)
	}
	if (in.Kind == store.InputNote) != (in.Note != "") || !validNote(in.Note) {
		return invalid("a note input, and only a note input, names its category: 1..%d bytes of [a-z0-9_]", store.MaxInputNoteBytes)
	}
	if in.Deferred && in.Kind != store.InputNote {
		return invalid("only a note input may be deferred")
	}
	if in.Priority != store.InputNormal && in.Priority != store.InputNow {
		return invalid("unknown input priority %q", in.Priority)
	}
	if in.Text == "" || len(in.Text) > store.MaxInputTextBytes {
		return invalid("input text must be 1..%d bytes", store.MaxInputTextBytes)
	}
	return nil
}

func (o *ops) SubmitInput(ctx context.Context, in store.Input) (store.InputSubmitted, error) {
	if err := checkInput(in); err != nil {
		return store.InputSubmitted{}, err
	}
	var out store.InputSubmitted
	err := o.unit(ctx, func(tx *ops) error {
		var err error
		out, err = tx.submitInput(ctx, in)
		return err
	})
	return out, err
}

// submitInput is SubmitInput inside a unit; in is already checked.
func (o *ops) submitInput(ctx context.Context, in store.Input) (store.InputSubmitted, error) {
	var out store.InputSubmitted
	s, err := o.getSession(ctx, in.SessionID, in.CreatedAt)
	if err != nil {
		return out, err
	}
	old, err := scanInput(o.q.QueryRow(ctx, `SELECT `+inputCols+` FROM session_inputs
		WHERE session_id = ? AND client_msg_id = ?`, in.SessionID, in.ClientMsgID))
	switch {
	case err == nil:
		// A persisted input no longer has its text to compare.
		if old.Kind != in.Kind || old.Note != in.Note || old.Deferred != in.Deferred || old.Priority != in.Priority ||
			(old.State != store.InputPersisted && old.Text != in.Text) {
			return out, fmt.Errorf("%w: client message %q was submitted with other content", store.ErrExists, in.ClientMsgID)
		}
		out.Input = old
	case !isNotFound(err):
		return out, err
	default:
		if s.State == store.SessionStopped {
			return out, fmt.Errorf("%w: session %q", store.ErrStopped, in.SessionID)
		}
		// Deferred notes have their own bound (FoldDeferredNotes) and do
		// not count here, so they never keep the user's input out.
		if !in.Deferred {
			var open int
			if err := o.q.QueryRow(ctx, `SELECT COUNT(*) FROM session_inputs WHERE session_id = ? AND state <> 'persisted' AND deferred = 0`,
				in.SessionID).Scan(&open); err != nil {
				return out, mapErr(err)
			}
			// A note may exceed the bound a little: one that told of
			// problems must not wait behind inputs no turn takes while it
			// waits.
			if limit := store.MaxOpenInputs; open >= limit && (in.Kind != store.InputNote || open >= limit+store.MaxNoteOverflow) {
				return out, fmt.Errorf("%w: %d inputs not yet persisted", store.ErrLimit, open)
			}
		}
		if _, err := o.exec(ctx, `INSERT INTO session_inputs (input_id, session_id, client_msg_id, kind, text, priority,
			state, created_at, note, deferred) VALUES (?,?,?,?,?,?,'pending',?,?,?)`, in.InputID, in.SessionID, in.ClientMsgID,
			string(in.Kind), in.Text, string(in.Priority), in.CreatedAt, in.Note, in.Deferred); err != nil {
			return out, err
		}
		out.Created = true
		if in.Kind == store.InputUser {
			if _, err := o.exec(ctx, `UPDATE sessions SET last_activity_at = max(last_activity_at, ?) WHERE session_id = ?`, in.CreatedAt, in.SessionID); err != nil {
				return out, err
			}
		}
		if in.Kind == store.InputUser && s.Title == "" {
			if title := store.DeriveSessionTitle(in.Text); title != "" {
				if _, err := o.exec(ctx, `UPDATE sessions SET title = ? WHERE session_id = ? AND title = ''`, title, in.SessionID); err != nil {
					return out, err
				}
			}
		}
		if out.Input, err = scanInput(o.q.QueryRow(ctx, `SELECT `+inputCols+` FROM session_inputs WHERE input_id = ?`, in.InputID)); err != nil {
			return out, err
		}
	}
	if out.Lease, err = o.getLease(ctx, in.SessionID); err != nil {
		return out, err
	}
	if out.Created && !in.Deferred && s.State == store.SessionIdle && !out.Lease.Live(in.CreatedAt) {
		if err := o.setState(ctx, in.SessionID, store.SessionQueued, in.CreatedAt); err != nil {
			return out, err
		}
	}
	out.Session, err = o.getSession(ctx, in.SessionID, in.CreatedAt)
	return out, err
}

func (o *ops) FoldDeferredNotes(ctx context.Context, in store.Input) (int, error) {
	if err := checkInput(in); err != nil {
		return 0, err
	}
	if !in.Deferred {
		return 0, invalid("the replacement of deferred notes is a deferred note")
	}
	var n int
	err := o.unit(ctx, func(tx *ops) error {
		s, err := tx.getSession(ctx, in.SessionID, in.CreatedAt)
		if err != nil {
			return err
		}
		if s.State == store.SessionStopped {
			return fmt.Errorf("%w: session %q", store.ErrStopped, in.SessionID)
		}
		var oldest int64
		if err := tx.q.QueryRow(ctx, `SELECT COUNT(*), COALESCE(MIN(created_at), 0) FROM session_inputs
			WHERE session_id = ? AND state = 'pending' AND deferred = 1`, in.SessionID).Scan(&n, &oldest); err != nil {
			return mapErr(err)
		}
		if n < store.MaxDeferredNotes {
			n = 0
			return nil
		}
		if _, err := tx.exec(ctx, `DELETE FROM session_inputs WHERE session_id = ? AND state = 'pending' AND deferred = 1`, in.SessionID); err != nil {
			return err
		}
		in.CreatedAt = oldest
		_, err = tx.submitInput(ctx, in)
		return err
	})
	return n, err
}

func (o *ops) SetInputEventSeq(ctx context.Context, sessionID, inputID string, seq int64) error {
	if err := store.CheckID("session id", sessionID); err != nil {
		return err
	}
	if err := store.CheckID("input id", inputID); err != nil {
		return err
	}
	if seq < 0 {
		return invalid("event seq must not be negative")
	}
	n, err := o.exec(ctx, `UPDATE session_inputs SET event_seq = ? WHERE session_id = ? AND input_id = ?`, seq, sessionID, inputID)
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: input %q of session %q", store.ErrNotFound, inputID, sessionID)
	}
	return nil
}

func (o *ops) TakeInputs(ctx context.Context, f store.Fence) ([]store.Input, error) {
	var out []store.Input
	err := o.fenced(ctx, f, func(tx *ops) error {
		s, err := tx.getSession(ctx, f.SessionID, f.Now)
		if err != nil {
			return err
		}
		switch s.State {
		case store.SessionStopped:
			return fmt.Errorf("%w: session %q", store.ErrStopped, f.SessionID)
		case store.SessionBroken:
			return nil // inputs wait for the repair
		}
		if s.WorkspaceBlocked {
			return nil // inputs wait for the hold to end
		}
		// Deferred notes go only with an input that starts a turn. A turn
		// the CLI starts by itself (a background task ending) takes no
		// input, so it does not carry them: they wait for the next input.
		if _, err := tx.exec(ctx, `UPDATE session_inputs SET state = 'taken', taken_epoch = ?1, taken_at = ?2
			WHERE session_id = ?3 AND state = 'pending' AND (deferred = 0 OR EXISTS (SELECT 1 FROM session_inputs
				WHERE session_id = ?3 AND state = 'pending' AND deferred = 0))`, f.Epoch, f.Now, f.SessionID); err != nil {
			return err
		}
		rows, err := tx.q.QueryContext(ctx, `SELECT `+inputCols+` FROM session_inputs
			WHERE session_id = ? AND state = 'taken' AND taken_epoch = ? ORDER BY kind = 'note' DESC, created_at, input_id`, f.SessionID, f.Epoch)
		if err != nil {
			return mapErr(err)
		}
		out, err = collect(rows, scanInput)
		return err
	})
	return out, err
}

// Turns.

const turnCols = `session_id, turn_no, epoch, local_seq, origin, started_at, ended_at, outcome, input_tokens,
	output_tokens, cache_read_tokens, cache_create_tokens, cost_micro_usd, duration_ms`

func scanTurn(s scanner) (store.Turn, error) {
	var t store.Turn
	var origin, outcome string
	err := s.Scan(&t.SessionID, &t.TurnNo, &t.Epoch, &t.LocalSeq, &origin, nullInt{&t.StartedAt}, nullInt{&t.EndedAt},
		nullStr{&outcome}, &t.Usage.InputTokens, &t.Usage.OutputTokens, &t.Usage.CacheReadInputTokens,
		&t.Usage.CacheCreationInputTokens, &t.Usage.CostMicroUSD, &t.Usage.DurationMs)
	t.Origin, t.Outcome = store.TurnOrigin(origin), store.TurnOutcome(outcome)
	return t, mapErr(err)
}

func checkTurnReport(r store.TurnReport) error {
	if r.LocalSeq < 1 || r.At == 0 {
		return invalid("turn report needs a local sequence of at least 1 and a time")
	}
	if r.Origin != "" && !r.Origin.Valid() {
		return invalid("unknown turn origin %q", r.Origin)
	}
	u := r.Usage
	if u.InputTokens < 0 || u.OutputTokens < 0 || u.CacheReadInputTokens < 0 || u.CacheCreationInputTokens < 0 ||
		u.CostMicroUSD < 0 || u.DurationMs < 0 {
		return invalid("negative usage")
	}
	if err := r.Persisted.Check(); err != nil {
		return err
	}
	switch r.Phase {
	case store.TurnStarted:
		if !r.Origin.Valid() {
			return invalid("STARTED needs an origin")
		}
		if !r.Persisted.Empty() || r.TranscriptIncomplete {
			return invalid("STARTED carries no persisted set and no transcript state")
		}
	case store.TurnEnded:
		if !r.Outcome.Valid() {
			return invalid("unknown turn outcome %q", r.Outcome)
		}
		if r.TranscriptIncomplete && !r.Persisted.Empty() {
			return invalid("a turn whose transcript is incomplete proves nothing persisted")
		}
	default:
		return invalid("unknown turn phase %q", r.Phase)
	}
	return nil
}

func (o *ops) ReportTurn(ctx context.Context, f store.Fence, r store.TurnReport) (store.TurnReported, error) {
	if err := checkTurnReport(r); err != nil {
		return store.TurnReported{}, err
	}
	var out store.TurnReported
	err := o.fenced(ctx, f, func(tx *ops) error {
		s, err := tx.getSession(ctx, f.SessionID, f.Now)
		if err != nil {
			return err
		}
		t, err := scanTurn(tx.q.QueryRow(ctx, `SELECT `+turnCols+` FROM session_turns
			WHERE session_id = ? AND epoch = ? AND local_seq = ?`, f.SessionID, f.Epoch, r.LocalSeq))
		found := err == nil
		if err != nil && !isNotFound(err) {
			return err
		}
		var reported bool
		if found {
			if err := tx.q.QueryRow(ctx, `SELECT ended_reported FROM session_turns WHERE session_id = ? AND turn_no = ?`,
				f.SessionID, t.TurnNo).Scan(&reported); err != nil {
				return mapErr(err)
			}
		}
		switch {
		case r.Phase == store.TurnStarted && found:
			out.Duplicate = true
		case r.Phase == store.TurnStarted:
			if err := tx.startTurn(ctx, f, r, s); err != nil {
				return err
			}
		case !found:
			return invalid("ENDED for local_seq %d, never STARTED in this epoch", r.LocalSeq)
		case reported:
			// A repeat: only the marks again, which are idempotent.
			out.Duplicate = true
			if err := tx.markPersisted(ctx, f, r.Persisted, r.At); err != nil {
				return err
			}
		default:
			// The first ENDED: of an open turn, or late for a turn its
			// successor closed as interrupted, whose outcome and end time stay.
			if t.EndedAt == 0 {
				if err := tx.endTurn(ctx, f, r); err != nil {
					return err
				}
			} else if err := tx.addLateUsage(ctx, f, r); err != nil {
				return err
			}
			if _, err := tx.exec(ctx, `UPDATE session_turns SET ended_reported = 1 WHERE session_id = ? AND epoch = ? AND local_seq = ?`,
				f.SessionID, f.Epoch, r.LocalSeq); err != nil {
				return err
			}
			if r.TranscriptIncomplete {
				if _, err := tx.exec(ctx, `UPDATE sessions SET
					state = CASE state WHEN 'stopped' THEN state ELSE 'broken' END,
					broken_turn_no = CASE state WHEN 'stopped' THEN broken_turn_no WHEN 'broken' THEN broken_turn_no
						ELSE ?2 END
					WHERE session_id = ?1`, f.SessionID, t.TurnNo); err != nil {
					return err
				}
			}
			if err := tx.markPersisted(ctx, f, r.Persisted, r.At); err != nil {
				return err
			}
			if out.LedgerDue, err = tx.ledgerDue(ctx, f.SessionID, r.At); err != nil {
				return err
			}
		}
		if err := tx.trimHistory(ctx, f.SessionID); err != nil {
			return err
		}
		if out.Turn, err = scanTurn(tx.q.QueryRow(ctx, `SELECT `+turnCols+` FROM session_turns
			WHERE session_id = ? AND epoch = ? AND local_seq = ?`, f.SessionID, f.Epoch, r.LocalSeq)); err != nil {
			return err
		}
		out.Session, err = tx.getSession(ctx, f.SessionID, f.Now)
		return err
	})
	if err != nil {
		return store.TurnReported{}, err
	}
	return out, nil
}

// startTurn records a new turn: it must take the epoch's next local_seq,
// and closes a predecessor that never ENDED as interrupted.
func (o *ops) startTurn(ctx context.Context, f store.Fence, r store.TurnReport, s store.Session) error {
	var last int64
	if err := o.q.QueryRow(ctx, `SELECT COALESCE(MAX(local_seq), 0) FROM session_turns WHERE session_id = ? AND epoch = ?`,
		f.SessionID, f.Epoch).Scan(&last); err != nil {
		return mapErr(err)
	}
	if r.LocalSeq != last+1 {
		return invalid("STARTED with local_seq %d, the next is %d", r.LocalSeq, last+1)
	}
	closed, err := o.interruptOpenTurns(ctx, f.SessionID, r.At)
	if err != nil {
		return err
	}
	if closed > 0 {
		if _, err := o.exec(ctx, `UPDATE sessions SET last_turn_outcome = 'interrupted' WHERE session_id = ?`, f.SessionID); err != nil {
			return err
		}
	}
	if _, err := o.exec(ctx, `INSERT INTO session_turns (session_id, turn_no, epoch, local_seq, origin, started_at)
		VALUES (?,?,?,?,?,?)`, f.SessionID, s.TurnCount+1, f.Epoch, r.LocalSeq, string(r.Origin), r.At); err != nil {
		return err
	}
	// A turn has started: nothing was closed without ENDED since.
	_, err = o.exec(ctx, `UPDATE sessions SET turn_count = turn_count + 1, updated_at = ?, previous_turn_interrupted = 0,
		state = CASE WHEN state IN ('idle', 'queued') THEN 'running' ELSE state END
		WHERE session_id = ?`, r.At, f.SessionID)
	return err
}

// endTurn records the end of the reported turn, adds its usage to the
// session and leaves running once no turn is open.
func (o *ops) endTurn(ctx context.Context, f store.Fence, r store.TurnReport) error {
	u := r.Usage
	if _, err := o.exec(ctx, `UPDATE session_turns SET ended_at = ?, outcome = ?, input_tokens = ?, output_tokens = ?,
		cache_read_tokens = ?, cache_create_tokens = ?, cost_micro_usd = ?, duration_ms = ?
		WHERE session_id = ? AND epoch = ? AND local_seq = ?`, r.At, string(r.Outcome), u.InputTokens, u.OutputTokens,
		u.CacheReadInputTokens, u.CacheCreationInputTokens, u.CostMicroUSD, u.DurationMs,
		f.SessionID, f.Epoch, r.LocalSeq); err != nil {
		return err
	}
	if r.Outcome == store.TurnStopped || r.Outcome == store.TurnInterrupted {
		var turnNo int64
		if err := o.q.QueryRow(ctx, `SELECT turn_no FROM session_turns WHERE session_id = ? AND epoch = ? AND local_seq = ?`,
			f.SessionID, f.Epoch, r.LocalSeq).Scan(&turnNo); err != nil {
			return mapErr(err)
		}
		if err := o.turnBoundary(ctx, f.SessionID, turnNo, r.At, r.Outcome); err != nil {
			return err
		}
	}
	_, err := o.exec(ctx, `UPDATE sessions SET last_turn_outcome = ?, updated_at = ?,
		input_tokens = input_tokens + ?, output_tokens = output_tokens + ?, cache_read_tokens = cache_read_tokens + ?,
		cache_create_tokens = cache_create_tokens + ?, cost_micro_usd = cost_micro_usd + ?, duration_ms = duration_ms + ?,
		state = CASE WHEN state = 'running' AND NOT EXISTS (
			SELECT 1 FROM session_turns WHERE session_id = ?9 AND ended_at IS NULL) THEN 'idle' ELSE state END
		WHERE session_id = ?9`, string(r.Outcome), r.At, u.InputTokens, u.OutputTokens, u.CacheReadInputTokens,
		u.CacheCreationInputTokens, u.CostMicroUSD, u.DurationMs, f.SessionID)
	return err
}

// interruptOpenTurns closes every open turn and records its boundary before
// returning. The caller owns the transaction and updates the session snapshot.
func (o *ops) interruptOpenTurns(ctx context.Context, sessionID string, at int64) (int, error) {
	rows, err := o.q.QueryContext(ctx, `UPDATE session_turns SET ended_at = ?, outcome = 'interrupted'
		WHERE session_id = ? AND ended_at IS NULL RETURNING turn_no`, at, sessionID)
	if err != nil {
		return 0, mapErr(err)
	}
	turns, err := collect(rows, func(s scanner) (int64, error) {
		var no int64
		err := s.Scan(&no)
		return no, mapErr(err)
	})
	if err != nil {
		return 0, err
	}
	for _, no := range turns {
		if err := o.turnBoundary(ctx, sessionID, no, at, store.TurnInterrupted); err != nil {
			return 0, err
		}
	}
	if len(turns) > 0 {
		// An interrupted turn may have changed the ledger snapshot too.
		if _, err := o.ledgerDue(ctx, sessionID, at); err != nil {
			return 0, err
		}
	}
	return len(turns), nil
}

// ledgerDue records that a turn of the session ended at, so a ready
// ledger snapshot is due for a collection, and reports whether it has one.
func (o *ops) ledgerDue(ctx context.Context, sessionID string, at int64) (bool, error) {
	n, err := o.exec(ctx, `UPDATE session_ledgers SET due_at = MAX(due_at, ?), updated_at = MAX(?, updated_at + 1)
		WHERE session_id = ? AND state = 'ready'`, max(at, 1), at, sessionID)
	return n > 0, err
}

// addLateUsage records the usage of a late ENDED, for a turn its
// successor closed: on the turn and in the session's totals. The turn's
// outcome and end time stay.
func (o *ops) addLateUsage(ctx context.Context, f store.Fence, r store.TurnReport) error {
	u := r.Usage
	if _, err := o.exec(ctx, `UPDATE session_turns SET input_tokens = ?, output_tokens = ?, cache_read_tokens = ?,
		cache_create_tokens = ?, cost_micro_usd = ?, duration_ms = ? WHERE session_id = ? AND epoch = ? AND local_seq = ?`,
		u.InputTokens, u.OutputTokens, u.CacheReadInputTokens, u.CacheCreationInputTokens, u.CostMicroUSD, u.DurationMs,
		f.SessionID, f.Epoch, r.LocalSeq); err != nil {
		return err
	}
	_, err := o.exec(ctx, `UPDATE sessions SET updated_at = ?,
		input_tokens = input_tokens + ?, output_tokens = output_tokens + ?, cache_read_tokens = cache_read_tokens + ?,
		cache_create_tokens = cache_create_tokens + ?, cost_micro_usd = cost_micro_usd + ?, duration_ms = duration_ms + ?
		WHERE session_id = ?`, r.At, u.InputTokens, u.OutputTokens, u.CacheReadInputTokens,
		u.CacheCreationInputTokens, u.CostMicroUSD, u.DurationMs, f.SessionID)
	return err
}

// markPersisted applies a turn's persisted set.
func (o *ops) markPersisted(ctx context.Context, f store.Fence, p store.TurnPersisted, at int64) error {
	type mark struct {
		ids   []string
		query string
		pre   []any
	}
	for _, m := range []mark{
		{p.InputIDs, `UPDATE session_inputs SET state = 'persisted', text = NULL, persisted_at = ?
			WHERE session_id = ? AND state = 'taken' AND taken_epoch = ? AND input_id IN `, []any{at, f.SessionID, f.Epoch}},
		{p.CallIDs, `UPDATE calls SET persisted_at = COALESCE(persisted_at, ?), returned_at = COALESCE(returned_at, ?)
			WHERE session_id = ? AND ` + callMatchesSessionOwner + ` AND call_id IN `, []any{at, at, f.SessionID}},
		{p.JobCallIDs, `UPDATE session_jobs SET notice = 'persisted', notice_at = ?
			WHERE session_id = ? AND ` + sessionJobDeviceMatchesOwner + ` AND notice <> 'persisted' AND call_id IN `, []any{at, f.SessionID}},
		{p.BgTaskIDs, `DELETE FROM session_bgtasks WHERE session_id = ? AND task_id IN `, []any{f.SessionID}},
		{p.LateCallIDs, `UPDATE calls SET notice_owed = 0 WHERE session_id = ? AND ` + callMatchesSessionOwner + ` AND call_id IN `, []any{f.SessionID}},
	} {
		if len(m.ids) == 0 {
			continue
		}
		ids := idArgs(m.ids)
		if _, err := o.exec(ctx, m.query+`(`+placeholders(len(ids))+`)`, append(m.pre, ids...)...); err != nil {
			return err
		}
	}
	if p.GapNoticeTurnNo > 0 {
		// Only the reminder for the gap still marked clears it: a later gap
		// keeps its own mark.
		if _, err := o.exec(ctx, `UPDATE sessions SET transcript_gap_turn_no = 0
			WHERE session_id = ? AND transcript_gap_turn_no = ?`, f.SessionID, p.GapNoticeTurnNo); err != nil {
			return err
		}
	}
	return nil
}

// trimHistory keeps the newest KeptTurns turns and KeptInputs persisted
// inputs.
func (o *ops) trimHistory(ctx context.Context, sessionID string) error {
	if _, err := o.exec(ctx, `DELETE FROM session_turns WHERE session_id = ?1
		AND turn_no <= (SELECT turn_count FROM sessions WHERE session_id = ?1) - ?2`, sessionID, store.KeptTurns); err != nil {
		return err
	}
	_, err := o.exec(ctx, `DELETE FROM session_inputs WHERE input_id IN (
		SELECT input_id FROM session_inputs WHERE session_id = ? AND state = 'persisted'
		ORDER BY persisted_at DESC, input_id DESC LIMIT -1 OFFSET ?)`, sessionID, store.KeptInputs)
	return err
}

func (o *ops) ListTurns(ctx context.Context, f store.TurnFilter) ([]store.Turn, error) {
	if err := store.CheckLimit(f.Limit); err != nil {
		return nil, err
	}
	if err := store.CheckID("session id", f.SessionID); err != nil {
		return nil, err
	}
	rows, err := o.q.QueryContext(ctx, `SELECT `+turnCols+` FROM session_turns WHERE session_id = ? AND turn_no > ?
		ORDER BY turn_no LIMIT ?`, f.SessionID, f.AfterTurnNo, f.Limit)
	if err != nil {
		return nil, mapErr(err)
	}
	return collect(rows, scanTurn)
}
