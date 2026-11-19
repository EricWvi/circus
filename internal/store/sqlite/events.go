package sqlite

import (
	"context"
	"fmt"

	"github.com/EricWvi/circus/internal/store"
)

// checkEvents validates a batch's events; a worker's (worker true) may not
// carry a kind only the control plane records.
func checkEvents(events []store.EventInput, worker bool) error {
	if len(events) == 0 || len(events) > store.MaxEventBatch {
		return invalid("1..%d events per batch", store.MaxEventBatch)
	}
	for _, e := range events {
		if err := store.CheckText("event kind", e.Kind, store.MaxCodeBytes, false); err != nil {
			return err
		}
		if worker && store.ControlPlaneOnlyKind(e.Kind) {
			return invalid("event kind %s is recorded by the control plane only", e.Kind)
		}
		if err := store.CheckOptionalID("event format", e.Format); err != nil {
			return err
		}
		if e.At == 0 {
			return invalid("event needs a time")
		}
	}
	return nil
}

// turnBoundary runs only at the first closure, inside its transaction.
func (o *ops) turnBoundary(ctx context.Context, sessionID string, turnNo, at int64, outcome store.TurnOutcome) error {
	kind := store.EventKindTurnInterrupted
	if outcome == store.TurnStopped {
		kind = store.EventKindTurnStopped
	}
	_, err := o.storeEvents(ctx, sessionID, []store.EventInput{{
		Kind: kind, At: at, Boundary: true,
		Payload: fmt.Appendf(nil, `{"turn_no":"%d","outcome":"%s"}`, turnNo, outcome),
	}}, store.DefaultMaxViewBytes)
	return err
}

func (o *ops) AppendEvents(ctx context.Context, sessionID string, pub *store.EventPublish, events []store.EventInput, maxViewBytes int64) (store.EventsAppended, error) {
	if err := store.CheckID("session id", sessionID); err != nil {
		return store.EventsAppended{}, err
	}
	if maxViewBytes < 1 {
		return store.EventsAppended{}, invalid("AppendEvents needs a positive view cap")
	}
	var eventsErr error
	if pub == nil || len(events) > 0 {
		eventsErr = checkEvents(events, pub != nil)
	}
	if pub == nil {
		if eventsErr != nil {
			return store.EventsAppended{}, eventsErr
		}
	} else {
		if pub.Fence.SessionID != sessionID {
			return store.EventsAppended{}, invalid("event fence names another session")
		}
		if err := checkFence(pub.Fence); err != nil {
			return store.EventsAppended{}, err
		}
		if pub.BatchSeq < 1 {
			return store.EventsAppended{}, invalid("event batch sequence starts at 1")
		}
	}
	var out store.EventsAppended
	err := o.unit(ctx, func(tx *ops) error {
		if pub != nil {
			f := pub.Fence
			if _, err := tx.fence(ctx, f); err != nil {
				return err
			}
			var epoch, last int64
			if err := tx.q.QueryRow(ctx, `SELECT event_batch_epoch, event_batch_seq FROM sessions WHERE session_id = ?`,
				sessionID).Scan(&epoch, &last); err != nil {
				return mapErr(err)
			}
			if epoch != f.Epoch {
				last = 0 // a new epoch numbers its batches from 1
			}
			switch {
			case pub.BatchSeq <= last:
				out.Deduplicated = true
				return nil
			case pub.BatchSeq != last+1:
				return invalid("event batch %d after %d: a batch is missing", pub.BatchSeq, last)
			}
			if _, err := tx.exec(ctx, `UPDATE sessions SET event_batch_epoch = ?, event_batch_seq = ?,
				dropped_event_batches = dropped_event_batches + ? WHERE session_id = ?`,
				f.Epoch, pub.BatchSeq, b2i(eventsErr != nil), sessionID); err != nil {
				return err
			}
			if eventsErr != nil {
				// Consumed, recorded, nothing stored; a result, so it
				// commits with the caller's Tx.
				out.Dropped, out.DropReason = true, eventsErr
				return nil
			}
		}
		if len(events) == 0 {
			return nil // a worker batch of live-only events: consumed, nothing stored
		}
		var err error
		out.Seqs, err = tx.storeEvents(ctx, sessionID, events, maxViewBytes)
		return err
	})
	if err != nil {
		return store.EventsAppended{}, err
	}
	return out, nil
}

// storeEvents stores valid events with the session's next seqs, applying
// the view and count caps, and returns each event's seq (0 if dropped).
func (o *ops) storeEvents(ctx context.Context, sessionID string, events []store.EventInput, maxViewBytes int64) ([]int64, error) {
	seqs := make([]int64, len(events))
	var viewBytes, lastSeq, count, lastActivity int64
	var truncated bool
	if err := o.q.QueryRow(ctx, `SELECT view_bytes, view_truncated, last_event_seq, event_count, last_activity_at FROM sessions
		WHERE session_id = ?`, sessionID).Scan(&viewBytes, &truncated, &lastSeq, &count, &lastActivity); err != nil {
		return nil, mapErr(err)
	}
	for i, e := range events {
		payload, cut := e.Payload, len(e.Payload) > store.MaxEventPayloadBytes
		if cut {
			payload = nil
		}
		if !truncated && viewBytes+int64(len(payload)) > maxViewBytes {
			truncated = true
		}
		if truncated {
			if !e.Boundary && e.Kind != store.EventKindTurnInterrupted && e.Kind != store.EventKindTurnStopped {
				continue // dropped: no seq
			}
			payload, cut = nil, true
		}
		lastSeq++
		if _, err := o.exec(ctx, `INSERT INTO session_events (session_id, seq, at, kind, format, payload,
			payload_bytes, truncated) VALUES (?,?,?,?,?,?,?,?)`, sessionID, lastSeq, e.At, e.Kind, nstr(e.Format),
			payload, len(e.Payload), b2i(cut)); err != nil {
			return nil, err
		}
		viewBytes += int64(len(payload))
		count++
		seqs[i] = lastSeq
		lastActivity = max(lastActivity, e.At)
	}
	if count > store.MaxSessionEvents {
		var evicted int64
		if err := o.q.QueryRow(ctx, `SELECT COALESCE(SUM(length(payload)), 0) FROM (
			SELECT payload FROM session_events WHERE session_id = ? ORDER BY seq LIMIT ?)`,
			sessionID, count-store.MaxSessionEvents).Scan(&evicted); err != nil {
			return nil, mapErr(err)
		}
		if _, err := o.exec(ctx, `DELETE FROM session_events WHERE session_id = ?1 AND seq IN (
			SELECT seq FROM session_events WHERE session_id = ?1 ORDER BY seq LIMIT ?2)`,
			sessionID, count-store.MaxSessionEvents); err != nil {
			return nil, err
		}
		viewBytes -= evicted
		count = store.MaxSessionEvents
	}
	if _, err := o.exec(ctx, `UPDATE sessions SET view_bytes = ?, view_truncated = ?, last_event_seq = ?, event_count = ?,
		last_activity_at = max(last_activity_at, ?) WHERE session_id = ?`, viewBytes, b2i(truncated), lastSeq, count, lastActivity, sessionID); err != nil {
		return nil, err
	}
	return seqs, nil
}

func (o *ops) ReadEvents(ctx context.Context, f store.EventFilter) (store.EventPage, error) {
	if err := store.CheckLimit(f.Limit); err != nil {
		return store.EventPage{}, err
	}
	if err := store.CheckID("session id", f.SessionID); err != nil {
		return store.EventPage{}, err
	}
	if f.AfterSeq < 0 || f.MaxBytes < 1 || f.MaxBytes > store.MaxTranscriptPageBytes {
		return store.EventPage{}, invalid("ReadEvents needs a session id and a byte budget of 1..%d", store.MaxTranscriptPageBytes)
	}
	var page store.EventPage
	err := o.unit(ctx, func(tx *ops) error {
		var lastSeq int64
		if err := tx.q.QueryRow(ctx, `SELECT last_event_seq FROM sessions WHERE session_id = ?`, f.SessionID).Scan(&lastSeq); err != nil {
			return mapErr(err)
		}
		if err := tx.q.QueryRow(ctx, `SELECT COALESCE(MIN(seq), 0) FROM session_events WHERE session_id = ?`,
			f.SessionID).Scan(&page.OldestSeq); err != nil {
			return mapErr(err)
		}
		rows, err := tx.q.QueryContext(ctx, `SELECT session_id, seq, at, kind, format, payload, payload_bytes, truncated
			FROM session_events WHERE session_id = ? AND seq > ? ORDER BY seq LIMIT ?`, f.SessionID, f.AfterSeq, f.Limit)
		if err != nil {
			return mapErr(err)
		}
		defer rows.Close()
		var total int64
		for rows.Next() {
			var e store.SessionEvent
			var format string
			var cut int64
			if err := rows.Scan(&e.SessionID, &e.Seq, &e.At, &e.Kind, nullStr{&format}, &e.Payload, &e.PayloadBytes, &cut); err != nil {
				return mapErr(err)
			}
			e.Format, e.Truncated = format, cut != 0
			if len(page.Events) > 0 && total+int64(len(e.Payload)) > f.MaxBytes {
				break
			}
			total += int64(len(e.Payload))
			page.Events = append(page.Events, e)
		}
		if err := rows.Err(); err != nil {
			return mapErr(err)
		}
		// Events are evicted oldest first, so a gap can only open the page.
		page.Missed = f.AfterSeq < lastSeq && (len(page.Events) == 0 || page.Events[0].Seq > f.AfterSeq+1)
		return nil
	})
	if err != nil {
		return store.EventPage{}, err
	}
	return page, nil
}
