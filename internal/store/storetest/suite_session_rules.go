package storetest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/EricWvi/circus/internal/store"
)

// testLeaseConnection: a lease answers one connection of its boot. A
// re-acquire or resume from another connection rebinds it, after which a
// write still in flight on the old connection is fenced, so it cannot
// overwrite what the new connection wrote.
func testLeaseConnection(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	createSession(t, s, f, "s1")
	old := acquireOn(t, s, "s1", "w1", "b1", "c1", t0)
	cur := acquireOn(t, s, "s1", "w1", "b1", "c2", t0+1) // reattached
	mustEqual(t, "same epoch", cur.Epoch, old.Epoch)
	must(t, s.PatchSessionRecords(ctx, cur, store.RecordsPatch{BgTasksStarted: []store.BgTask{{TaskID: "t1", Kind: "agent"}}}))
	must(t, s.PatchSessionRecords(ctx, cur, store.RecordsPatch{BgTasksEnded: []string{"t1"}}))
	// The old connection's stale start arrives last: fenced.
	mustFenced(t, s.PatchSessionRecords(ctx, at(old, t0+2), store.RecordsPatch{BgTasksStarted: []store.BgTask{{TaskID: "t1", Kind: "agent"}}}), old.Epoch)
	recs, err := s.GetSessionRecords(ctx, cur, records(store.RecordBgTasks))
	must(t, err)
	mustEqual(t, "no stale task", len(recs.BgTasks), 0)
	lost, err := s.ExtendLeases(ctx, []store.LeaseKey{{SessionID: "s1", Epoch: old.Epoch, WorkerID: "w1", BootID: "b1", ConnID: "c1"}}, t0+3, t0+3+ttl)
	must(t, err)
	mustEqual(t, "old connection does not extend", len(lost), 1)

	// Resume rebinds too.
	res, err := s.ResumeLeases(ctx, "w1", "b1", "c3", []store.SessionEpoch{{SessionID: "s1", Epoch: cur.Epoch}}, t0+4, t0+4+ttl)
	must(t, err)
	must(t, res[0].Err)
	_, err = s.TakeInputs(ctx, at(cur, t0+5))
	mustFenced(t, err, cur.Epoch)
	cur.ConnID = "c3"
	_, err = s.TakeInputs(ctx, at(cur, t0+5))
	must(t, err)
}

// testSessionRecovery: what a holder must recover is listed whichever
// epoch produced it, and stays listed until acknowledged: a reattach (lost
// reply) and a second handover both see it again.
func testSessionRecovery(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	addWorker(t, s, "w2", "u1")
	addWorker(t, s, "w3", "u1")
	createSession(t, s, f, "s1")
	w1 := at(acquire(t, s, "s1", "w1", "b1", t0), t0+1)
	sessionCall(t, s, f, "c-returned", "s1", store.CallCompleted)
	sessionCall(t, s, f, "c-running", "s1", store.CallRunning)
	sessionCall(t, s, f, "c-owed", "s1", store.CallUnknown)
	sessionCall(t, s, f, "c-done", "s1", store.CallCompleted)
	submit(t, s, newInput("s1", 1))
	_, err := s.TakeInputs(ctx, w1)
	must(t, err)
	_, err = s.PutJob(ctx, store.Job{DeviceID: "d1", BootID: "b1", JobID: "j1", UserID: "u1", SessionID: "s1", CallID: "c-done", State: store.JobExited, LastSeq: 1})
	must(t, err)
	must(t, s.PatchSessionRecords(ctx, w1, store.RecordsPatch{
		ReturnedCallIDs: []string{"c-returned", "c-owed", "c-done"},
		AwaitingCallIDs: []string{"c-owed"},
		Jobs:            []store.SessionJob{{CallID: "c-done", JobID: "j1"}},
		HandedJobCalls:  []string{"c-done"},
		BgTasksStarted:  []store.BgTask{{TaskID: "agent-1", ToolUseID: "tu-a", Kind: "agent", Description: "explore"}},
	}))
	// Within the epoch: taken inputs and handed notices are listed; the
	// holder's own task is not lost.
	rec, err := s.GetRecovery(ctx, w1)
	must(t, err)
	mustEqual(t, "own epoch", []any{inputIDs(rec.Inputs), len(rec.Jobs), len(rec.LostBgTasks)}, []any{[]string{"s1-in1"}, 1, 0})
	report(t, s, w1, store.TurnReport{LocalSeq: 1, Phase: store.TurnStarted})
	report(t, s, w1, store.TurnReport{LocalSeq: 1, Phase: store.TurnEnded, Persisted: store.TurnPersisted{CallIDs: []string{"c-done", "c-owed"}}})
	report(t, s, w1, store.TurnReport{LocalSeq: 2, Phase: store.TurnStarted})

	// The holder dies mid-turn; w2 takes over at epoch 2.
	w2 := at(acquire(t, s, "s1", "w2", "b2", t0+ttl), t0+ttl+1)
	want := func(rec store.Recovery) {
		t.Helper()
		var calls []string
		for _, c := range rec.Calls {
			calls = append(calls, c.CallID)
		}
		slices.Sort(calls)
		mustEqual(t, "recovery", []any{rec.LastTurnInterrupted, len(rec.Inputs), len(rec.Jobs), rec.LostBgTasks, calls, rec.CallsTruncated},
			[]any{true, 0, 0, []store.BgTask{{TaskID: "agent-1", ToolUseID: "tu-a", Kind: "agent", Description: "explore", Epoch: 1, StartedAt: t0 + 1}},
				[]string{"c-owed", "c-returned", "c-running"}, false})
	}
	rec, err = s.GetRecovery(ctx, w2)
	must(t, err)
	want(rec)
	for _, c := range rec.Calls {
		switch c.CallID {
		case "c-owed":
			mustEqual(t, "owed", []any{c.NoticeOwed, c.PersistedAt != 0, c.State.Outcome()}, []any{true, true, store.OutcomeUnknown})
		case "c-returned":
			mustEqual(t, "returned", []any{c.ReturnedAt, c.State.Outcome()}, []any{t0 + 1, store.OutcomeResult})
		case "c-running":
			mustEqual(t, "not returned", []any{c.ReturnedAt, c.State.Outcome()}, []any{int64(0), store.OutcomeDispatched})
		}
	}
	// A lost reply: w2 acquires again and gets the same lease and recovery.
	r, err := s.AcquireLease(ctx, leaseReq("s1", "w2", "b2", "b2-c", t0+ttl+2))
	must(t, err)
	mustEqual(t, "reattached", []any{r.Reattached, r.Lease.Epoch}, []any{true, w2.Epoch})
	rec, err = s.GetRecovery(ctx, w2)
	must(t, err)
	want(rec)
	// w2 dies too, having acknowledged nothing: w3 at epoch 3 sees it all.
	w3 := at(acquire(t, s, "s1", "w3", "b3", t0+3*ttl), t0+3*ttl+1)
	rec, err = s.GetRecovery(ctx, w3)
	must(t, err)
	want(rec)
	// The recovery turn acknowledges everything, split between a patch and
	// the ENDED report.
	must(t, s.PatchSessionRecords(ctx, w3, store.RecordsPatch{Persisted: store.TurnPersisted{CallIDs: []string{"c-returned"}, BgTaskIDs: []string{"agent-1"}}}))
	report(t, s, w3, store.TurnReport{LocalSeq: 1, Phase: store.TurnEnded, Origin: store.TurnFromInput,
		Persisted: store.TurnPersisted{CallIDs: []string{"c-running"}, LateCallIDs: []string{"c-owed"}}})
	rec, err = s.GetRecovery(ctx, w3)
	must(t, err)
	mustEqual(t, "acknowledged", []any{rec.LastTurnInterrupted, len(rec.Calls), len(rec.LostBgTasks)}, []any{false, 0, 0})

	// More candidates than recovery lists (only calls created without
	// admission can get there): truncated at MaxRecoveryCalls.
	must(t, s.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		for i := range store.MaxRecoveryCalls + 1 {
			sessionCall(t, tx, f, fmt.Sprint("bulk", i), "s1", store.CallCompleted)
		}
		return nil
	}))
	rec, err = s.GetRecovery(ctx, w3)
	must(t, err)
	mustEqual(t, "truncated", []any{len(rec.Calls), rec.CallsTruncated}, []any{store.MaxRecoveryCalls, true})
}

// testOwedNotices: AwaitingCallIDs marks only the session's calls, at most
// MaxOwedCalls per session; LateCallIDs clears.
func testOwedNotices(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	createSession(t, s, f, "s1")
	fence := acquire(t, s, "s1", "w1", "b1", t0)
	var ids []string
	must(t, s.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		for i := range store.MaxOwedCalls + 1 {
			id := fmt.Sprint("c", i)
			ids = append(ids, id)
			sessionCall(t, tx, f, id, "s1", store.CallCompleted)
		}
		return nil
	}))
	sessionCall(t, s, f, "foreign", "other", store.CallCompleted)
	must(t, s.PatchSessionRecords(ctx, fence, store.RecordsPatch{AwaitingCallIDs: ids[:store.MaxOwedCalls]}))
	mustIs(t, s.PatchSessionRecords(ctx, fence, store.RecordsPatch{AwaitingCallIDs: ids[store.MaxOwedCalls:]}), store.ErrLimit)
	mustIs(t, s.PatchSessionRecords(ctx, fence, store.RecordsPatch{AwaitingCallIDs: ids}), store.ErrInvalid)
	must(t, s.PatchSessionRecords(ctx, fence, store.RecordsPatch{AwaitingCallIDs: []string{"foreign"}}))
	mustEqual(t, "foreign untouched", getCall(t, s, "foreign").NoticeOwed, false)
	report(t, s, fence, store.TurnReport{LocalSeq: 1, Phase: store.TurnEnded, Persisted: store.TurnPersisted{CallIDs: ids, LateCallIDs: ids[:1]}})
	calls, err := s.ListCalls(ctx, store.CallFilter{SessionID: "s1", Recovery: true, Limit: store.MaxPageSize})
	must(t, err)
	mustEqual(t, "still owed", len(calls), store.MaxOwedCalls-1)
	calls, err = s.ListCalls(ctx, store.CallFilter{SessionID: "s1", NotPersisted: true, Limit: store.MaxPageSize})
	must(t, err)
	mustEqual(t, "all persisted", len(calls), 0)
	must(t, s.PatchSessionRecords(ctx, fence, store.RecordsPatch{AwaitingCallIDs: ids[store.MaxOwedCalls:]}))
}

// testInterruptSession: the interrupt is a level keyed to the open turn.
func testInterruptSession(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	createSession(t, s, f, "s1")
	fence := acquire(t, s, "s1", "w1", "b1", t0)
	interrupt := func() (int64, bool) {
		t.Helper()
		sess, l, changed, err := s.InterruptSession(ctx, "s1", t0+1)
		must(t, err)
		mustEqual(t, "holder", l.WorkerID, "w1")
		return sess.InterruptTurnNo, changed
	}
	no, changed := interrupt()
	mustEqual(t, "no turn", []any{no, changed}, []any{int64(0), false})
	report(t, s, fence, store.TurnReport{LocalSeq: 1, Phase: store.TurnStarted})
	no, changed = interrupt()
	mustEqual(t, "turn 1", []any{no, changed}, []any{int64(1), true})
	no, changed = interrupt()
	mustEqual(t, "repeated", []any{no, changed}, []any{int64(1), false})
	report(t, s, fence, store.TurnReport{LocalSeq: 1, Phase: store.TurnEnded, Outcome: store.TurnInterrupted})
	no, changed = interrupt()
	mustEqual(t, "after the end", []any{no, changed}, []any{int64(1), false})
	// Turn 2 is above the recorded level: not interrupted by a late hint.
	report(t, s, fence, store.TurnReport{LocalSeq: 2, Phase: store.TurnStarted})
	mustEqual(t, "level below turn 2", getSession(t, s, "s1").InterruptTurnNo, int64(1))
	report(t, s, fence, store.TurnReport{LocalSeq: 2, Phase: store.TurnEnded})
	no, changed = interrupt()
	mustEqual(t, "no open turn: an ended turn is not interrupted", []any{no, changed}, []any{int64(1), false})
	report(t, s, fence, store.TurnReport{LocalSeq: 3, Phase: store.TurnStarted}) // turn 3
	// Its successor closes turn 3: the open turn is turn 4.
	report(t, s, fence, store.TurnReport{LocalSeq: 4, Phase: store.TurnStarted}) // turn 4
	no, _ = interrupt()
	mustEqual(t, "the open turn", no, int64(4))
	// The late ENDED of turn 3 keeps the session running for turn 4.
	report(t, s, fence, store.TurnReport{LocalSeq: 3, Phase: store.TurnEnded})
	mustEqual(t, "still running", getSession(t, s, "s1").State, store.SessionRunning)
	report(t, s, fence, store.TurnReport{LocalSeq: 4, Phase: store.TurnEnded})
	mustEqual(t, "idle", getSession(t, s, "s1").State, store.SessionIdle)
	_, _, _, err := s.InterruptSession(ctx, "ghost", t0)
	mustIs(t, err, store.ErrNotFound)
}

// stateRule is one method's answer on a stopped or broken session: nil
// when accepted, else the error.
type stateRule struct {
	name string
	call func(ctx context.Context, s store.Store, f store.Fence) error
}

// stateRules exercises every worker- and user-facing method on session s1
// held under fence f, with a pending approval "a-pend" and a grant for
// "rm -rf x".
var stateRules = []stateRule{
	{"SubmitInput", func(ctx context.Context, s store.Store, f store.Fence) error {
		_, err := s.SubmitInput(ctx, newInput("s1", 7))
		return err
	}},
	{"TakeInputs", func(ctx context.Context, s store.Store, f store.Fence) error {
		_, err := s.TakeInputs(ctx, f)
		return err
	}},
	{"RequestApproval", func(ctx context.Context, s store.Store, f store.Fence) error {
		_, err := s.RequestApproval(ctx, f, newApproval("a-new", "ls -l"))
		return err
	}},
	{"ResolveApproval", func(ctx context.Context, s store.Store, f store.Fence) error {
		_, err := s.ResolveApproval(ctx, store.ApprovalResolution{ApprovalID: "a-pend", At: f.Now})
		return err
	}},
	{"ConsumeGrant", func(ctx context.Context, s store.Store, f store.Fence) error {
		_, err := s.ConsumeGrant(ctx, f, store.GrantKey{Tool: "bash", DeviceID: "d1", MatchKey: mk("rm -rf x")}, "tu-rule")
		return err
	}},
	{"AdmitSessionCall", func(ctx context.Context, s store.Store, f store.Fence) error {
		return s.AdmitSessionCall(ctx, f, "c-new", store.DefaultMaxActiveSessionCalls)
	}},
	{"AcquireLease by another worker", func(ctx context.Context, s store.Store, f store.Fence) error {
		_, err := s.AcquireLease(ctx, leaseReq("s1", "w2", "b9", "b9-c", f.Now+ttl))
		return err
	}},
	{"AcquireLease by the holder", func(ctx context.Context, s store.Store, f store.Fence) error {
		r, err := s.AcquireLease(ctx, leaseReq("s1", f.WorkerID, f.BootID, f.ConnID, f.Now))
		if err == nil && !r.Reattached {
			err = errors.New("not reattached")
		}
		return err
	}},
	{"AppendTranscript", func(ctx context.Context, s store.Store, f store.Fence) error {
		_, err := s.AppendTranscript(ctx, f, store.TranscriptBatch{BatchSeq: 2, Entries: []store.TranscriptEntry{entry("late", "{}")}}, quota)
		return err
	}},
	{"ReportTurn", func(ctx context.Context, s store.Store, f store.Fence) error {
		// The epoch's next turn, started and ended.
		turns, err := s.ListTurns(ctx, store.TurnFilter{SessionID: f.SessionID, Limit: store.MaxPageSize})
		if err != nil {
			return err
		}
		next := int64(1)
		for _, tn := range turns {
			if tn.Epoch == f.Epoch {
				next = max(next, tn.LocalSeq+1)
			}
		}
		for _, ph := range []store.TurnPhase{store.TurnStarted, store.TurnEnded} {
			if _, err := s.ReportTurn(ctx, f, store.TurnReport{LocalSeq: next, Phase: ph, Origin: store.TurnFromCLI,
				Outcome: store.TurnCompleted, At: f.Now}); err != nil {
				return err
			}
		}
		return nil
	}},
	{"PatchSessionRecords", func(ctx context.Context, s store.Store, f store.Fence) error {
		return s.PatchSessionRecords(ctx, f, store.RecordsPatch{Cwds: []store.SessionCwd{{DeviceID: "d1", Cwd: "/late"}}})
	}},
	{"AppendEvents", func(ctx context.Context, s store.Store, f store.Fence) error {
		_, err := s.AppendEvents(ctx, "s1", &store.EventPublish{Fence: f, BatchSeq: 1}, []store.EventInput{{Kind: "result", At: f.Now}}, store.DefaultMaxViewBytes)
		return err
	}},
	{"reads", func(ctx context.Context, s store.Store, f store.Fence) error {
		if _, err := s.LoadTranscript(ctx, f, store.TranscriptPageRequest{Limit: 10, MaxBytes: 1 << 20}); err != nil {
			return err
		}
		if _, err := s.GetSessionRecords(ctx, f, allRecords); err != nil {
			return err
		}
		if _, err := s.GetRecovery(ctx, f); err != nil {
			return err
		}
		_, err := s.ListTranscriptSubpaths(ctx, f)
		return err
	}},
	{"ExtendLeases", func(ctx context.Context, s store.Store, f store.Fence) error {
		lost, err := s.ExtendLeases(ctx, []store.LeaseKey{{SessionID: "s1", Epoch: f.Epoch, WorkerID: f.WorkerID, BootID: f.BootID, ConnID: f.ConnID}}, f.Now, f.Now+ttl)
		if err == nil && len(lost) > 0 {
			err = errors.New("not extended")
		}
		return err
	}},
	{"ResumeLeases", func(ctx context.Context, s store.Store, f store.Fence) error {
		r, err := s.ResumeLeases(ctx, f.WorkerID, f.BootID, f.ConnID, []store.SessionEpoch{{SessionID: "s1", Epoch: f.Epoch}}, f.Now, f.Now+ttl)
		if err != nil {
			return err
		}
		return r[0].Err
	}},
	{"ReleaseLease", func(ctx context.Context, s store.Store, f store.Fence) error {
		_, err := s.ReleaseLease(ctx, f)
		return err
	}},
}

// ruleSession builds s1 held by w1/b1 with a pending approval, a grant and
// a transcript batch, then puts it in the state set does, and returns the
// holder's fence.
func ruleSession(t testing.TB, s store.Store, set func(store.Fence)) store.Fence {
	t.Helper()
	ctx := context.Background()
	f := seed(t, s)
	addWorker(t, s, "w2", "u1")
	createSession(t, s, f, "s1")
	fence := at(acquire(t, s, "s1", "w1", "b1", t0), t0+1)
	appendEntries(t, s, fence, "", 1, entry("first", "{}"))
	_, err := s.RequestApproval(ctx, fence, newApproval("a-grant", "rm -rf x"))
	must(t, err)
	_, err = s.ResolveApproval(ctx, store.ApprovalResolution{ApprovalID: "a-grant", Approve: true, At: t0 + 6, GrantExpiresAt: t0 + 600_000,
		Input: store.Input{InputID: "retry", Text: "retry"}})
	must(t, err)
	_, err = s.RequestApproval(ctx, fence, newApproval("a-pend", "make"))
	must(t, err)
	set(fence)
	return at(fence, t0+10)
}

// testStoppedRules: a stopped session refuses new work and accepts the
// holder's flush, reads and lease upkeep.
func testStoppedRules(t *testing.T, b Backend) {
	refused := map[string]bool{"SubmitInput": true, "TakeInputs": true, "RequestApproval": true, "ResolveApproval": true,
		"ConsumeGrant": true, "AdmitSessionCall": true, "AcquireLease by another worker": true}
	for _, rule := range stateRules {
		t.Run(rule.name, func(t *testing.T) {
			s, _ := open(t, b)
			fence := ruleSession(t, s, func(f store.Fence) {
				_, _, _, err := s.StopSession(t.Context(), "s1", t0+7, "user")
				must(t, err)
			})
			err := rule.call(t.Context(), s, fence)
			if refused[rule.name] {
				mustIs(t, err, store.ErrStopped)
			} else {
				must(t, err)
			}
		})
	}
	// ResumeLeases reports the stop.
	s, _ := open(t, b)
	fence := ruleSession(t, s, func(f store.Fence) {
		_, _, _, err := s.StopSession(t.Context(), "s1", t0+7, "user")
		must(t, err)
	})
	res, err := s.ResumeLeases(t.Context(), "w1", "b1", "c9", []store.SessionEpoch{{SessionID: "s1", Epoch: fence.Epoch}}, t0+10, t0+10+ttl)
	must(t, err)
	mustEqual(t, "resumed stopped", []any{res[0].Err, res[0].Stopped, res[0].Lease.ConnID}, []any{error(nil), true, "c9"})
}

// testBrokenRules: an ENDED with an incomplete transcript makes the session
// broken; it then accepts appends (the repair) and new inputs, takes none,
// and refuses approvals, grants, calls and new holders until the holder
// reports the repair.
func testBrokenRules(t *testing.T, b Backend) {
	refused := map[string]error{"RequestApproval": store.ErrBroken, "ConsumeGrant": store.ErrBroken,
		"AdmitSessionCall": store.ErrBroken, "AcquireLease by another worker": store.ErrBroken}
	broken := func(s store.Store) func(store.Fence) {
		return func(f store.Fence) {
			report(t, s, f, store.TurnReport{LocalSeq: 1, Phase: store.TurnEnded, Outcome: store.TurnError, TranscriptIncomplete: true})
		}
	}
	for _, rule := range stateRules {
		t.Run(rule.name, func(t *testing.T) {
			s, _ := open(t, b)
			fence := ruleSession(t, s, broken(s))
			err := rule.call(t.Context(), s, fence)
			if want := refused[rule.name]; want != nil {
				mustIs(t, err, want)
			} else {
				must(t, err)
			}
		})
	}

	s, _ := open(t, b)
	ctx := t.Context()
	fence := ruleSession(t, s, func(f store.Fence) {
		submit(t, s, newInput("s1", 1))
		taken, err := s.TakeInputs(ctx, f)
		must(t, err)
		mustEqual(t, "taken before", len(taken), 2) // the approval retry and s1-in1
		_, err = s.ReportTurn(ctx, f, store.TurnReport{LocalSeq: 1, Phase: store.TurnEnded, Outcome: store.TurnError, At: f.Now,
			TranscriptIncomplete: true, Persisted: store.TurnPersisted{InputIDs: []string{"s1-in1"}}})
		mustIs(t, err, store.ErrInvalid)
		broken(s)(f)
	})
	mustEqual(t, "broken", getSession(t, s, "s1").State, store.SessionBroken)
	submit(t, s, newInput("s1", 2))
	taken, err := s.TakeInputs(ctx, fence)
	must(t, err)
	mustEqual(t, "takes none while broken", len(taken), 0)
	rec, err := s.GetRecovery(ctx, fence)
	must(t, err)
	mustEqual(t, "taken stay unpersisted", len(rec.Inputs), 2)
	// Repair is idempotent; then the inputs flow again.
	for range 2 {
		sess, err := s.TranscriptRepaired(ctx, fence)
		must(t, err)
		mustEqual(t, "repaired", sess.State, store.SessionIdle)
	}
	taken, err = s.TakeInputs(ctx, fence)
	must(t, err)
	mustEqual(t, "taken after repair", len(taken), 3)
	// Losing the lease while broken keeps it broken.
	report(t, s, fence, store.TurnReport{LocalSeq: 2, Phase: store.TurnEnded, Outcome: store.TurnError, TranscriptIncomplete: true})
	loss, err := s.ReleaseLease(ctx, fence)
	must(t, err)
	mustEqual(t, "still broken", loss.Session.State, store.SessionBroken)
}

// testPersistedFromPatch: a persisted set sent with PatchSessionRecords
// marks like one sent with ENDED; repeats are harmless; other sessions'
// ids are ignored.
func testPersistedFromPatch(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	createSession(t, s, f, "s1")
	fence := acquire(t, s, "s1", "w1", "b1", t0)
	submit(t, s, newInput("s1", 1))
	_, err := s.TakeInputs(ctx, fence)
	must(t, err)
	sessionCall(t, s, f, "c1", "s1", store.CallCompleted)
	sessionCall(t, s, f, "foreign", "other", store.CallCompleted)
	_, err = s.PutJob(ctx, store.Job{DeviceID: "d1", BootID: "b1", JobID: "j1", UserID: "u1", SessionID: "s1", CallID: "c1", State: store.JobExited, LastSeq: 1})
	must(t, err)
	must(t, s.PatchSessionRecords(ctx, fence, store.RecordsPatch{
		Jobs: []store.SessionJob{{CallID: "c1", JobID: "j1"}}, HandedJobCalls: []string{"c1"},
		BgTasksStarted: []store.BgTask{{TaskID: "t1", Kind: "agent"}},
	}))
	set := store.TurnPersisted{InputIDs: []string{"s1-in1"}, CallIDs: []string{"c1", "foreign"}, JobCallIDs: []string{"c1"}, BgTaskIDs: []string{"t1"}}
	for i := range 2 {
		must(t, s.PatchSessionRecords(ctx, at(fence, t0+int64(10+i)), store.RecordsPatch{Persisted: set}))
	}
	rec, err := s.GetRecovery(ctx, fence)
	must(t, err)
	mustEqual(t, "nothing to recover", []any{len(rec.Inputs), len(rec.Calls), len(rec.LostBgTasks)}, []any{0, 0, 0})
	c := getCall(t, s, "c1")
	mustEqual(t, "first mark kept", []int64{c.PersistedAt, c.ReturnedAt}, []int64{t0 + 10, t0 + 10})
	mustEqual(t, "foreign untouched", getCall(t, s, "foreign").PersistedAt, int64(0))
	recs, err := s.GetSessionRecords(ctx, fence, records(store.RecordJobs|store.RecordBgTasks))
	must(t, err)
	mustEqual(t, "job notice", []any{recs.Jobs[0].Notice, recs.Jobs[0].NoticeAt, len(recs.BgTasks)}, []any{store.JobNoticePersisted, t0 + 10, 0})
	// A repeated ENDED does not move the notice time either.
	report(t, s, at(fence, t0+20), store.TurnReport{LocalSeq: 1, Phase: store.TurnEnded, Persisted: set})
	report(t, s, at(fence, t0+21), store.TurnReport{LocalSeq: 1, Phase: store.TurnEnded, Persisted: set})
	recs, err = s.GetSessionRecords(ctx, fence, records(store.RecordJobs))
	must(t, err)
	mustEqual(t, "notice time kept", recs.Jobs[0].NoticeAt, t0+10)
}

// testCallOutcome pins the outcome of every call state.
func testCallOutcome(t *testing.T, _ Backend) {
	want := map[store.CallState]store.CallOutcome{
		store.CallAccepted: store.OutcomeAccepted, store.CallDispatched: store.OutcomeDispatched,
		store.CallRunning: store.OutcomeDispatched, store.CallUnknown: store.OutcomeUnknown,
		store.CallCompleted: store.OutcomeResult, store.CallOffline: store.OutcomeOffline,
		store.CallCancelled: store.OutcomeCancelled, store.CallRejected: store.OutcomeRejected,
		store.CallLost: store.OutcomeLost, "weird": "",
	}
	for st, o := range want {
		mustEqual(t, string(st), st.Outcome(), o)
	}
}

// testStringCaps: every string the store takes is bounded; one byte over
// is ErrInvalid before anything is read, exactly the bound is accepted.
func testStringCaps(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	createSession(t, s, f, "s1")
	fence := acquire(t, s, "s1", "w1", "b1", t0)
	sessionCall(t, s, f, "c1", "s1", store.CallCompleted)
	x := func(n int) string { return strings.Repeat("x", n) }
	id, tool, desc, path := store.MaxIDBytes, store.MaxToolBytes, store.MaxDescriptionBytes, store.MaxPathBytes
	cases := []struct {
		name string
		max  int
		call func(v string) error
	}{
		{"session id", id, func(v string) error {
			sess := newSession(v, f)
			sess.ClaudeSessionID = "cl-" + fmt.Sprint(len(v))
			return s.CreateSession(ctx, sess, nil, 100)
		}},
		{"model", id, func(v string) error {
			sess := newSession(fmt.Sprint("m", len(v)), f)
			sess.ClaudeSessionID, sess.Model = sess.ID, v
			return s.CreateSession(ctx, sess, nil, 100)
		}},
		{"cwd", path, func(v string) error {
			return s.PatchSessionRecords(ctx, fence, store.RecordsPatch{Cwds: []store.SessionCwd{{DeviceID: "d1", Cwd: v}}})
		}},
		{"agent id", id, func(v string) error {
			return s.PatchSessionRecords(ctx, fence, store.RecordsPatch{Cwds: []store.SessionCwd{{AgentID: v, DeviceID: "d1", Cwd: "/"}}})
		}},
		{"file path", path, func(v string) error {
			return s.PatchSessionRecords(ctx, fence, store.RecordsPatch{Files: []store.SessionFile{{DeviceID: "d1", Path: v, SHA256: make([]byte, 32)}}})
		}},
		{"job description", desc, func(v string) error {
			return s.PatchSessionRecords(ctx, fence, store.RecordsPatch{Jobs: []store.SessionJob{{CallID: "c1", JobID: "j", Description: v}}})
		}},
		{"job output path", path, func(v string) error {
			return s.PatchSessionRecords(ctx, fence, store.RecordsPatch{Jobs: []store.SessionJob{{CallID: "c1", JobID: "j", OutputPath: v}}})
		}},
		{"job id", id, func(v string) error {
			return s.PatchSessionRecords(ctx, fence, store.RecordsPatch{Jobs: []store.SessionJob{{CallID: "c1", JobID: v}}})
		}},
		{"task id", id, func(v string) error {
			err := s.PatchSessionRecords(ctx, fence, store.RecordsPatch{BgTasksStarted: []store.BgTask{{TaskID: v, Kind: "agent"}}})
			if err == nil {
				err = s.PatchSessionRecords(ctx, fence, store.RecordsPatch{BgTasksEnded: []string{v}})
			}
			return err
		}},
		{"task description", desc, func(v string) error {
			return s.PatchSessionRecords(ctx, fence, store.RecordsPatch{BgTasksStarted: []store.BgTask{{TaskID: "t", Kind: "agent", Description: v}}})
		}},
		{"persisted id", id, func(v string) error {
			return s.PatchSessionRecords(ctx, fence, store.RecordsPatch{Persisted: store.TurnPersisted{CallIDs: []string{v}}})
		}},
		{"input id", id, func(v string) error {
			in := newInput("s1", len(v))
			in.InputID = v
			_, err := s.SubmitInput(ctx, in)
			return err
		}},
		{"client message id", id, func(v string) error {
			in := newInput("s1", 1000+len(v))
			in.ClientMsgID = v
			_, err := s.SubmitInput(ctx, in)
			return err
		}},
		{"tool", tool, func(v string) error {
			a := newApproval(fmt.Sprint("a-tool", len(v)), "k")
			a.Tool = v
			_, err := s.RequestApproval(ctx, fence, a)
			return err
		}},
		{"approval display", desc, func(v string) error {
			a := newApproval(fmt.Sprint("a-disp", len(v)), fmt.Sprint("k", len(v)))
			a.Display = v
			_, err := s.RequestApproval(ctx, fence, a)
			return err
		}},
		{"approval reason", desc, func(v string) error {
			a := newApproval(fmt.Sprint("a-reas", len(v)), fmt.Sprint("r", len(v)))
			a.Reason = v
			_, err := s.RequestApproval(ctx, fence, a)
			return err
		}},
		{"approval id", id, func(v string) error {
			a := newApproval(v, fmt.Sprint("i", len(v)))
			a.ToolUseID = "tu"
			_, err := s.RequestApproval(ctx, fence, a)
			return err
		}},
		{"event format", id, func(v string) error {
			_, err := s.AppendEvents(ctx, "s1", nil, []store.EventInput{{Kind: "k", Format: v, At: t0}}, store.DefaultMaxViewBytes)
			return err
		}},
		{"admitted call id", id, func(v string) error {
			return s.AdmitSessionCall(ctx, fence, v, 100)
		}},
		// Last: it rebinds the lease away from fence's connection.
		{"connection id", id, func(v string) error {
			_, err := s.AcquireLease(ctx, leaseReq("s1", "w1", "b1", v, t0+1))
			return err
		}},
	}
	for _, c := range cases {
		mustIs(t, c.call(x(c.max+1)), store.ErrInvalid)
		if err := c.call(x(c.max)); err != nil {
			t.Fatalf("%s at the bound: %v", c.name, err)
		}
	}
}

// testRecordsPaging: a byte budget splits the records into pages that,
// joined, are exactly the whole; every page holds at least one row.
func testRecordsPaging(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	createSession(t, s, f, "s1")
	fence := acquire(t, s, "s1", "w1", "b1", t0)
	var files []store.SessionFile
	for i := range 40 {
		files = append(files, store.SessionFile{DeviceID: "d1", Path: fmt.Sprintf("/f%02d-%s", i, strings.Repeat("p", i*20)), SHA256: make([]byte, 32), TouchedAt: t0})
	}
	var cwds []store.SessionCwd
	for i := range 5 {
		cwds = append(cwds, store.SessionCwd{AgentID: fmt.Sprint("a", i), DeviceID: "d1", Cwd: "/w", UpdatedAt: t0})
	}
	must(t, s.PatchSessionRecords(ctx, fence, store.RecordsPatch{Files: files, Cwds: cwds,
		BgTasksStarted: []store.BgTask{{TaskID: "t1", Kind: "agent"}, {TaskID: "t2", Kind: "agent"}}}))
	whole, err := s.GetSessionRecords(ctx, fence, allRecords)
	must(t, err)
	mustEqual(t, "whole", []any{len(whole.Cwds), len(whole.Files), len(whole.BgTasks), whole.Next}, []any{5, 40, 2, (*store.RecordsCursor)(nil)})
	for _, budget := range []int64{1, 200, 1000, 5000} {
		var joined store.SessionRecords
		req := store.RecordsRequest{Sections: store.RecordAll, MaxBytes: budget}
		for pages := 0; ; pages++ {
			if pages > 200 {
				t.Fatalf("budget %d: no end", budget)
			}
			page, err := s.GetSessionRecords(ctx, fence, req)
			must(t, err)
			rows := len(page.Cwds) + len(page.Files) + len(page.Jobs) + len(page.BgTasks) + len(page.Grants)
			if rows == 0 {
				t.Fatalf("budget %d: empty page", budget)
			}
			var size int64
			for _, c := range page.Cwds {
				size += c.Size()
			}
			for _, x := range page.Files {
				size += x.Size()
			}
			for _, x := range page.BgTasks {
				size += x.Size()
			}
			if rows > 1 && size > budget {
				t.Fatalf("budget %d: page of %d bytes", budget, size)
			}
			joined.Cwds = append(joined.Cwds, page.Cwds...)
			joined.Files = append(joined.Files, page.Files...)
			joined.BgTasks = append(joined.BgTasks, page.BgTasks...)
			if page.Next == nil {
				break
			}
			req.After = page.Next
		}
		mustEqual(t, label("joined pages, budget ", budget), joined, whole)
	}
	_, err = s.GetSessionRecords(ctx, fence, store.RecordsRequest{Sections: store.RecordCwds, MaxBytes: 100,
		After: &store.RecordsCursor{Section: store.RecordFiles}})
	mustIs(t, err, store.ErrInvalid)
	_, err = s.GetSessionRecords(ctx, fence, store.RecordsRequest{Sections: store.RecordAll, MaxBytes: store.MaxTranscriptPageBytes + 1})
	mustIs(t, err, store.ErrInvalid)
}

// testApprovalRules: expiry at resolution, grant selection and expiry,
// dedup by the whole key, resolved-approval retention and the list's byte
// budget.
func testApprovalRules(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	createSession(t, s, f, "s1")
	fence := at(acquire(t, s, "s1", "w1", "b1", t0), t0+1)
	retry := func(id string) store.Input { return store.Input{InputID: "retry-" + id, Text: "retry"} }

	// Past its expiry an approval resolves to expired: no grant, no input.
	late := newApproval("late", "cmd")
	late.ExpiresAt = t0 + 50
	_, err := s.RequestApproval(ctx, fence, late)
	must(t, err)
	res, err := s.ResolveApproval(ctx, store.ApprovalResolution{ApprovalID: "late", Approve: true, At: t0 + 50, GrantExpiresAt: t0 + 1000, Input: retry("late")})
	must(t, err)
	mustEqual(t, "late resolution", []any{res.Changed, res.Approval.State, res.Input}, []any{false, store.ApprovalExpired, (*store.Input)(nil)})
	ok, err := s.ConsumeGrant(ctx, fence, store.GrantKey{Tool: "bash", DeviceID: "d1", MatchKey: mk("cmd")}, "tu-late")
	must(t, err)
	mustEqual(t, "no grant", ok, false)

	// Dedup needs the whole key: another device is another approval.
	a1 := newApproval("a1", "make")
	_, err = s.RequestApproval(ctx, fence, a1)
	must(t, err)
	other := newApproval("a1-other-device", "make")
	other.DeviceID = "d2"
	r, err := s.RequestApproval(ctx, fence, other)
	must(t, err)
	mustEqual(t, "another device", r.Created, true)

	// Two grants of one key: the one expiring first is consumed first;
	// an expired one is neither listed nor consumable, and the sweep
	// deletes it.
	a2 := newApproval("a2", "make")
	key := store.GrantKey{Tool: "bash", DeviceID: "d1", MatchKey: mk("make")}
	_, err = s.ResolveApproval(ctx, store.ApprovalResolution{ApprovalID: "a1", Approve: true, At: t0 + 10, GrantExpiresAt: t0 + 5000, Input: retry("a1")})
	must(t, err)
	_, err = s.RequestApproval(ctx, fence, a2)
	must(t, err)
	_, err = s.ResolveApproval(ctx, store.ApprovalResolution{ApprovalID: "a2", Approve: true, At: t0 + 11, GrantExpiresAt: t0 + 3000, Input: retry("a2")})
	must(t, err)
	grants := func(now int64) []string {
		recs, err := s.GetSessionRecords(ctx, at(fence, now), records(store.RecordGrants))
		must(t, err)
		var ids []string
		for _, g := range recs.Grants {
			ids = append(ids, g.GrantID)
		}
		return ids
	}
	mustEqual(t, "both", grants(t0+12), []string{"a1", "a2"})
	mustEqual(t, "expired not listed", grants(t0+3000), []string{"a1"})
	ok, err = s.ConsumeGrant(ctx, at(fence, t0+100), key, "tu-soonest")
	must(t, err)
	mustEqual(t, "consumed", ok, true)
	mustEqual(t, "soonest consumed first", grants(t0+100), []string{"a1"})
	_, err = s.RequestApproval(ctx, fence, newApproval("a3", "make"))
	must(t, err)
	_, err = s.ResolveApproval(ctx, store.ApprovalResolution{ApprovalID: "a3", Approve: true, At: t0 + 12, GrantExpiresAt: t0 + 200, Input: retry("a3")})
	must(t, err)
	_, err = s.ExpireApprovals(ctx, t0+5000, 10)
	must(t, err)
	mustEqual(t, "swept", grants(t0+13), []string(nil))

	// Resolved approvals: the newest KeptResolvedApprovals are kept.
	must(t, s.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		for i := range store.KeptResolvedApprovals + 5 {
			id := fmt.Sprintf("r%03d", i)
			if _, err := tx.RequestApproval(ctx, fence, newApproval(id, id)); err != nil {
				return err
			}
			if _, err := tx.ResolveApproval(ctx, store.ApprovalResolution{ApprovalID: id, At: t0 + 100 + int64(i)}); err != nil {
				return err
			}
		}
		return nil
	}))
	resolved, err := s.ListApprovals(ctx, store.ApprovalFilter{SessionID: "s1", States: []store.ApprovalState{store.ApprovalApproved, store.ApprovalDenied, store.ApprovalExpired},
		Limit: store.MaxPageSize, MaxBytes: store.MaxTranscriptPageBytes})
	must(t, err)
	mustEqual(t, "resolved kept", len(resolved), store.KeptResolvedApprovals)
	_, err = s.GetApproval(ctx, "r000")
	mustIs(t, err, store.ErrNotFound)
	_, err = s.GetApproval(ctx, fmt.Sprintf("r%03d", store.KeptResolvedApprovals+4))
	must(t, err)

	// The list's byte budget: at least one, never over except one.
	all, err := s.ListApprovals(ctx, store.ApprovalFilter{UserID: "u1", Limit: store.MaxPageSize, MaxBytes: store.MaxTranscriptPageBytes})
	must(t, err)
	one := store.ApprovalSize(all[0])
	page, err := s.ListApprovals(ctx, store.ApprovalFilter{UserID: "u1", Limit: store.MaxPageSize, MaxBytes: 1})
	must(t, err)
	mustEqual(t, "one over budget", len(page), 1)
	page, err = s.ListApprovals(ctx, store.ApprovalFilter{UserID: "u1", Limit: store.MaxPageSize, MaxBytes: one + store.ApprovalSize(all[1])})
	must(t, err)
	mustEqual(t, "exactly two", len(page), 2)
	_, err = s.ListApprovals(ctx, store.ApprovalFilter{UserID: "u1", Limit: 10})
	mustIs(t, err, store.ErrInvalid)
}

// testSettleSession: without a live lease, pending work queues an idle
// session; with one, it stays idle.
func testSettleSession(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	createSession(t, s, f, "s1")
	fence := acquire(t, s, "s1", "w1", "b1", t0)
	sessionCall(t, s, f, "c1", "s1", store.CallCompleted)
	must(t, s.PatchSessionRecords(ctx, fence, store.RecordsPatch{Jobs: []store.SessionJob{{CallID: "c1", JobID: "j1"}}}))
	// A running job is not pending work.
	_, err := s.PutJob(ctx, store.Job{DeviceID: "d1", BootID: "b1", JobID: "j1", UserID: "u1", SessionID: "s1", CallID: "c1", State: store.JobRunning, LastSeq: 1})
	must(t, err)
	_, err = s.ReleaseLease(ctx, fence)
	must(t, err)
	r, err := s.SettleSession(ctx, "s1", t0+1)
	must(t, err)
	mustEqual(t, "running job", []any{r.Session.State, r.Loss}, []any{store.SessionIdle, (*store.LeaseLoss)(nil)})
	// The job ends: settle queues the session.
	_, err = s.PutJob(ctx, store.Job{DeviceID: "d1", BootID: "b1", JobID: "j1", UserID: "u1", SessionID: "s1", CallID: "c1", State: store.JobExited, LastSeq: 2})
	must(t, err)
	r, err = s.SettleSession(ctx, "s1", t0+2)
	must(t, err)
	mustEqual(t, "ended job queues", r.Session.State, store.SessionQueued)
	// With a live holder it stays idle.
	fence = acquire(t, s, "s1", "w1", "b1", t0+3)
	mustEqual(t, "acquired", getSession(t, s, "s1").State, store.SessionIdle)
	r, err = s.SettleSession(ctx, "s1", t0+4)
	must(t, err)
	mustEqual(t, "held", r.Session.State, store.SessionIdle)
	// A persisted notice of an ended job is purged once old enough, a
	// running job's never.
	sessionCall(t, s, f, "c2", "s1", store.CallCompleted)
	_, err = s.PutJob(ctx, store.Job{DeviceID: "d1", BootID: "b1", JobID: "j2", UserID: "u1", SessionID: "s1", CallID: "c2", State: store.JobRunning, LastSeq: 1})
	must(t, err)
	must(t, s.PatchSessionRecords(ctx, at(fence, t0+5), store.RecordsPatch{Jobs: []store.SessionJob{{CallID: "c2", JobID: "j2"}},
		Persisted: store.TurnPersisted{JobCallIDs: []string{"c1", "c2"}}}))
	n, err := s.PurgeSessionJobs(ctx, t0+5, 10)
	must(t, err)
	mustEqual(t, "purged only the ended job", n, 1)
	recs, err := s.GetSessionRecords(ctx, at(fence, t0+6), records(store.RecordJobs))
	must(t, err)
	mustEqual(t, "running job kept", []any{len(recs.Jobs), recs.Jobs[0].CallID}, []any{1, "c2"})
}

// testInputDedupPriority: a repeat differing only in priority is a
// conflict.
func testInputDedupPriority(t *testing.T, b Backend) {
	s, _ := open(t, b)
	f := seed(t, s)
	createSession(t, s, f, "s1")
	submit(t, s, newInput("s1", 1))
	in := newInput("s1", 1)
	in.Priority = store.InputNow
	_, err := s.SubmitInput(t.Context(), in)
	mustIs(t, err, store.ErrExists)
	in = newInput("s1", 1)
	in.Kind = store.InputApproval
	_, err = s.SubmitInput(t.Context(), in)
	mustIs(t, err, store.ErrExists)
}

// testLeaseLossRace: the expiry sweep, a settle, a takeover and the old
// holder's release race on two handles; exactly one of them applies the
// lease-loss rule.
func testLeaseLossRace(t *testing.T, b Backend) {
	for round := range 4 {
		s, dir := open(t, b)
		ctx := deadline(t)
		f := seed(t, s)
		addWorker(t, s, "w2", "u1")
		createSession(t, s, f, "s1")
		fence := acquire(t, s, "s1", "w1", "b1", t0)
		report(t, s, fence, store.TurnReport{LocalSeq: 1, Phase: store.TurnStarted})
		other := reopen(t, b, dir)
		now := t0 + ttl
		var mu sync.Mutex
		losses := 0
		count := func(n int) {
			mu.Lock()
			losses += n
			mu.Unlock()
		}
		paths := []func(h store.Store) error{
			func(h store.Store) error {
				l, err := h.ExpireLeases(ctx, now, 10)
				count(len(l))
				return err
			},
			func(h store.Store) error {
				r, err := h.SettleSession(ctx, "s1", now)
				if r.Loss != nil {
					count(1)
				}
				return err
			},
			func(h store.Store) error {
				r, err := h.AcquireLease(ctx, leaseReq("s1", "w2", "b2", "b2-c", now))
				if r.Loss != nil {
					count(1)
				}
				return err
			},
			func(h store.Store) error {
				// The old holder's release of its expired lease: fenced while
				// it still holds or after the takeover, a retried release
				// (no change) after the sweep cleared it. Never a loss.
				loss, err := h.ReleaseLease(ctx, at(fence, now))
				switch {
				case errors.Is(err, store.ErrFenced):
					return nil
				case err != nil:
					return err
				case loss.Released.WorkerID != "" || loss.Interrupted:
					return fmt.Errorf("release of an expired lease applied a loss: %+v", loss)
				}
				return nil
			},
		}
		var wg sync.WaitGroup
		errs := make([]error, len(paths))
		for i, p := range paths {
			h := []store.Store{s, other}[(i+round)%2]
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs[i] = retryBusy(func() error { return p(h) })
			}()
		}
		wg.Wait()
		for _, err := range errs {
			must(t, err)
		}
		mustEqual(t, label("round ", round, " losses"), losses, 1)
		turns, err := s.ListTurns(ctx, store.TurnFilter{SessionID: "s1", Limit: 10})
		must(t, err)
		mustEqual(t, "interrupted once", []any{turns[0].Outcome, turns[0].EndedAt}, []any{store.TurnInterrupted, now})
		l, err := s.GetLease(ctx, "s1")
		must(t, err)
		mustEqual(t, "taken over", []any{l.Epoch, l.WorkerID}, []any{int64(2), "w2"})
	}
}
