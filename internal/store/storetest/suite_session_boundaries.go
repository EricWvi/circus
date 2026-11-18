package storetest

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/EricWvi/circus/internal/store"
)

func testTurnBoundaries(t *testing.T, b Backend) {
	for _, path := range []string{"fresh", "fresh-stopped", "fresh-broken", "recover", "stop-release", "quota-release", "shutdown-release"} {
		t.Run(path, func(t *testing.T) {
			s, _ := open(t, b)
			ctx := t.Context()
			f := seed(t, s)
			createSession(t, s, f, "s1")
			fence := acquire(t, s, "s1", "w1", "b1", t0)
			// A late mirror failure can break the session while a successor is open.
			if path == "fresh-broken" || path == "recover" {
				report(t, s, fence, store.TurnReport{LocalSeq: 1, Phase: store.TurnEnded, Outcome: store.TurnError, TranscriptIncomplete: true})
			}
			seq := int64(1)
			if path == "fresh-broken" || path == "recover" {
				seq = 2
			}
			report(t, s, fence, store.TurnReport{LocalSeq: seq, Phase: store.TurnStarted})
			switch path {
			case "fresh-stopped", "stop-release":
				_, _, _, err := s.StopSession(ctx, "s1", t0+1, "")
				must(t, err)
			case "quota-release":
				_, err := s.AppendTranscript(ctx, fence, store.TranscriptBatch{BatchSeq: 1, Entries: []store.TranscriptEntry{entry("x", `{}`)}}, store.TranscriptQuota{MaxEntryBytes: 1000, MaxSessionBytes: 1, MaxUserBytes: 1000})
				must(t, err)
			}
			mustEqual(t, "stop has not closed turn", len(sessionEvents(t, s, "s1")), 0)
			closeAt := t0 + 2
			switch path {
			case "fresh", "fresh-stopped", "fresh-broken":
				req := leaseReq("s1", "w1", "b1", "new", closeAt)
				req.Fresh = true
				_, err := s.AcquireLease(ctx, req)
				must(t, err)
			case "recover":
				_, err := s.RecoverSession(ctx, "s1", true, closeAt, store.DefaultMaxViewBytes)
				must(t, err)
			default:
				_, err := s.ReleaseLease(ctx, at(fence, closeAt))
				must(t, err)
			}
			checkTurnBoundary(t, s, "s1", seq, closeAt, store.TurnInterrupted)
			if path == "fresh-stopped" || path == "stop-release" || path == "quota-release" {
				mustEqual(t, "stopped preserved", getSession(t, s, "s1").State, store.SessionStopped)
			}
			// A stale holder cannot turn cleanup into a confirmed user stop.
			_, err := s.ReportTurn(ctx, fence, store.TurnReport{LocalSeq: seq, Phase: store.TurnEnded, Outcome: store.TurnStopped, At: t0 + 3})
			mustIs(t, err, store.ErrFenced)
			checkTurnBoundary(t, s, "s1", seq, closeAt, store.TurnInterrupted)
		})
	}
	for _, outcome := range []store.TurnOutcome{store.TurnCompleted, store.TurnError, store.TurnStopped, store.TurnInterrupted} {
		for _, incomplete := range []bool{false, true} {
			s, dir := open(t, b)
			ctx := t.Context()
			f := seed(t, s)
			createSession(t, s, f, "s1")
			fence := acquire(t, s, "s1", "w1", "b1", t0)
			report(t, s, fence, store.TurnReport{LocalSeq: 1, Phase: store.TurnStarted})
			_, _, _, err := s.InterruptSession(ctx, "s1", t0+1)
			must(t, err)
			end := store.TurnReport{LocalSeq: 1, Phase: store.TurnEnded, Outcome: outcome, TranscriptIncomplete: incomplete, At: t0 + 2}
			// No outcome or boundary escapes an outer rollback.
			mustIs(t, s.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
				_, err := tx.ReportTurn(ctx, fence, end)
				if err != nil {
					return err
				}
				return errRollback
			}), errRollback)
			mustEqual(t, "rolled back events", len(sessionEvents(t, s, "s1")), 0)
			mustEqual(t, "still open", getSession(t, s, "s1").State, store.SessionRunning)
			r, err := s.ReportTurn(ctx, fence, end)
			must(t, err)
			wantState := store.SessionIdle
			if incomplete {
				wantState = store.SessionBroken
			}
			mustEqual(t, "outcome and mirror precedence", []any{r.Session.LastTurnOutcome, r.Session.State}, []any{outcome, wantState})
			other := reopen(t, b, dir)
			_, err = other.ReportTurn(ctx, fence, end)
			must(t, err)
			_, err = other.ReleaseLease(ctx, at(fence, t0+3))
			must(t, err)
			mustEqual(t, "release preserves committed outcome", getSession(t, s, "s1").LastTurnOutcome, outcome)
			if outcome == store.TurnStopped || outcome == store.TurnInterrupted {
				checkTurnBoundary(t, s, "s1", 1, t0+2, outcome)
			} else {
				mustEqual(t, "no stop boundary", len(sessionEvents(t, s, "s1")), 0)
			}
		}
	}
	// Real generated boundaries are retained bare after view truncation.
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	createSession(t, s, f, "s1")
	fence := acquire(t, s, "s1", "w1", "b1", t0)
	_, err := s.AppendEvents(ctx, "s1", nil, []store.EventInput{{Kind: "ASSISTANT", Payload: []byte("xx"), At: t0}}, 1)
	must(t, err)
	report(t, s, fence, store.TurnReport{LocalSeq: 1, Phase: store.TurnEnded, Outcome: store.TurnStopped, At: t0 + 1})
	report(t, s, fence, store.TurnReport{LocalSeq: 2, Phase: store.TurnStarted})
	_, err = s.ReleaseLease(ctx, at(fence, t0+2))
	must(t, err)
	events := sessionEvents(t, s, "s1")
	mustEqual(t, "truncated real boundaries", []any{len(events), events[0].Kind, events[1].Kind, events[0].Truncated, events[1].Truncated, events[0].Payload, events[1].Payload}, []any{2, store.EventKindTurnStopped, store.EventKindTurnInterrupted, true, true, []byte(nil), []byte(nil)})
}

func testStopReportRace(t *testing.T, b Backend) {
	for _, order := range []string{"report-first", "loss-first", "concurrent"} {
		t.Run(order, func(t *testing.T) {
			s, dir := open(t, b)
			ctx := deadline(t)
			f := seed(t, s)
			createSession(t, s, f, "s1")
			fence := acquire(t, s, "s1", "w1", "b1", t0)
			other := reopen(t, b, dir)
			report(t, s, fence, store.TurnReport{LocalSeq: 1, Phase: store.TurnStarted})
			_, _, _, err := s.InterruptSession(ctx, "s1", t0+1)
			must(t, err)
			end := func() error {
				_, err := s.ReportTurn(ctx, fence, store.TurnReport{LocalSeq: 1, Phase: store.TurnEnded, Outcome: store.TurnStopped, At: t0 + 2})
				return err
			}
			loss := func() error { _, err := other.ReleaseLease(ctx, at(fence, t0+2)); return err }
			var endErr error
			switch order {
			case "report-first":
				must(t, end())
				must(t, loss())
			case "loss-first":
				must(t, loss())
				endErr = end()
			default:
				var wg sync.WaitGroup
				var lossErr error
				wg.Go(func() { endErr = retryBusy(end) })
				wg.Go(func() { lossErr = retryBusy(loss) })
				wg.Wait()
				must(t, lossErr)
			}
			outcome := store.TurnStopped
			if order == "loss-first" || errors.Is(endErr, store.ErrFenced) {
				outcome = store.TurnInterrupted
				mustIs(t, endErr, store.ErrFenced)
			} else {
				must(t, endErr)
			}
			mustEqual(t, "first closure wins", getSession(t, s, "s1").LastTurnOutcome, outcome)
			checkTurnBoundary(t, s, "s1", 1, t0+2, outcome)
		})
	}
}
