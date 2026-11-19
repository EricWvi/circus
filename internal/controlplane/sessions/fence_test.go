package sessions_test

import (
	"strings"
	"testing"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/testutil/wait"
	wc "github.com/EricWvi/circus/internal/testutil/workerclient"
)

const matchKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// everyOp returns one request of every operation that carries a fence.
func everyOp(id string, epoch uint64) map[string]*circusv1.SessionRequest {
	return map[string]*circusv1.SessionRequest{
		"release":  wc.Release(id, epoch, circusv1.ReleaseLease_REASON_IDLE),
		"load":     wc.Load(id, epoch, "", 0, 1024),
		"keys":     wc.Op(id, epoch, &circusv1.ListTranscriptKeys{}),
		"append":   wc.Append(id, epoch, "", 1, &circusv1.TranscriptEntry{Uuid: "fenced-entry", Json: `{}`}),
		"take":     wc.TakeInputs(id, epoch),
		"report":   wc.Started(id, epoch, 1, circusv1.TurnOrigin_TURN_ORIGIN_INPUT),
		"patch":    wc.Patch(id, epoch, &circusv1.PatchSessionState{Cwds: []*circusv1.PatchSessionState_Cwd{{Device: "desk", Cwd: "/fenced"}}}),
		"state":    wc.State(id, epoch),
		"publish":  wc.Publish(id, epoch, 1, &circusv1.PublishEvents_Event{Kind: circusv1.SessionEvent_KIND_ASSISTANT, Json: `{}`}),
		"approval": wc.Op(id, epoch, &circusv1.RequestApproval{ToolUseId: "t", Tool: "bash", Device: "desk", MatchKey: matchKey}),
		"grant":    wc.Op(id, epoch, &circusv1.ConsumeGrant{ToolUseId: "t", Tool: "bash", Device: "desk", MatchKey: matchKey}),
		"repaired": wc.Op(id, epoch, &circusv1.TranscriptRepaired{}),
	}
}

// assertUntouched checks that no fenced write reached the store.
func (f *fixture) assertUntouched(id string, before store.Session) {
	f.t.Helper()
	s := f.get(id)
	if s.LastEventSeq != before.LastEventSeq || s.TurnCount != before.TurnCount || s.TranscriptBytes != before.TranscriptBytes ||
		s.PendingApprovalCount != before.PendingApprovalCount || s.State != before.State {
		f.t.Fatalf("a fenced operation wrote: before %+v after %+v", before, s)
	}
	recs, err := f.st.ListApprovals(f.t.Context(), store.ApprovalFilter{SessionID: id, Limit: 10, MaxBytes: 1 << 20})
	must(f.t, err)
	if len(recs) != 0 {
		f.t.Fatal("a fenced approval was recorded")
	}
}

// TestFencingEveryOp: every fenced operation with a stale epoch, on another
// worker, or on an abandoned connection of the holder's boot is answered
// Fenced with the current epoch and writes nothing.
func TestFencingEveryOp(t *testing.T) {
	f := newFixture(t)
	f.session("s1", -1)
	c := f.dial(0, "boot-a")
	ev := f.attach(c, 1)
	f.input("s1", "go", store.InputNormal)
	offerTo(t, ev, "s1")
	epoch := acquire(t, c, "s1")
	before := f.get("s1")

	other := f.dial(1, "boot-b")
	for name, req := range everyOp("s1", epoch+1) {
		t.Run("stale epoch "+name, func(t *testing.T) { wantFenced(t, call(t, c, req), epoch) })
	}
	for name, req := range everyOp("s1", epoch) {
		t.Run("other worker "+name, func(t *testing.T) { wantFenced(t, call(t, other, req), epoch) })
	}
	f.assertUntouched("s1", before)

	// A reconnect of the same boot resumes; from then on the old,
	// half-open connection commits nothing.
	c2 := f.dial(0, "boot-a")
	f.attach(c2, 1)
	r, err := c2.Resume(wait.Ctx(t), &circusv1.ResumeLeases_Held{SessionId: "s1", Epoch: epoch})
	must(t, err)
	if o := r.GetResumed().GetLeases(); len(o) != 1 || !o[0].GetOk() || o[0].GetEpoch() != epoch {
		t.Fatalf("resume %v", r)
	}
	for name, req := range everyOp("s1", epoch) {
		t.Run("abandoned connection "+name, func(t *testing.T) { wantFenced(t, call(t, c, req), epoch) })
	}
	f.assertUntouched("s1", before)
	if f.lease("s1").WorkerID == "" {
		t.Fatal("a fenced release released the lease")
	}
	// The holder's new connection works.
	if got := call(t, c2, wc.TakeInputs("s1", epoch)).GetInputs().GetInputs(); len(got) != 1 {
		t.Fatalf("inputs on the new connection %v", got)
	}
}

// TestReconnectOrder: after a reconnect the worker sends Attach, then
// ResumeLeases, then its resent operations, all pipelined; none is fenced,
// and the pending Nudge reasons come again after the resume.
func TestReconnectOrder(t *testing.T) {
	f := newFixture(t)
	f.session("s1", -1)
	c := f.dial(0, "boot-a")
	ev := f.attach(c, 1)
	f.input("s1", "go", store.InputNormal)
	offerTo(t, ev, "s1")
	epoch := acquire(t, c, "s1")
	f.input("s1", "second", store.InputNormal)
	nudge(t, ev, "s1")
	_ = c.Close() // lost before the worker acted on the Nudge

	c2 := f.dial(0, "boot-a")
	ctx := wait.Ctx(t)
	ev2, err := c2.Attach(ctx, &circusv1.Attach{MaxSessions: 1})
	must(t, err)
	resume, err := c2.Open(ctx, &circusv1.WorkerUplink{Payload: &circusv1.WorkerUplink_ResumeLeases{ResumeLeases: &circusv1.ResumeLeases{
		Leases: []*circusv1.ResumeLeases_Held{{SessionId: "s1", Epoch: epoch}, {SessionId: "nope", Epoch: 3}, {SessionId: "bad id", Epoch: 1}}}}})
	must(t, err)
	appendReply, err := c2.Send(ctx, wc.Append("s1", epoch, "", 1, &circusv1.TranscriptEntry{Json: `{}`}))
	must(t, err)
	r, err := wc.Reply{Stream: resume}.Wait(ctx)
	must(t, err)
	o := r.GetResumed().GetLeases()
	if len(o) != 3 || !o[0].GetOk() || o[1].GetOk() || o[1].GetSessionId() != "nope" || o[2].GetOk() {
		t.Fatalf("resumed %v", r)
	}
	ar, err := appendReply.Wait(ctx)
	must(t, err)
	wantOk(t, ar, false)
	// The pending reason comes again after the resume; the worker acts on
	// it (a TakeInputs sent before the hint may clear it first, which is
	// equally correct for a level-triggered hint).
	n := nudge(t, ev2, "s1", circusv1.WorkerEvent_Nudge_REASON_INPUT)
	if !hasReason(n, circusv1.WorkerEvent_Nudge_REASON_INPUT) {
		t.Fatalf("re-sent nudge %v", n)
	}
	tr := call(t, c2, wc.TakeInputs("s1", epoch))
	if len(tr.GetInputs().GetInputs()) != 2 {
		t.Fatalf("take %v", tr)
	}
	if f.lease("s1").ConnID == "" {
		t.Fatal("no binding")
	}
}

// TestDeferredUntilAttach: a Nudge for a lease bound to a connection
// without an Attach stream waits for Attach.
func TestDeferredUntilAttach(t *testing.T) {
	f := newFixture(t)
	f.session("s1", -1)
	c := f.dial(0, "boot-a")
	ev := f.attach(c, 1)
	f.input("s1", "go", store.InputNormal)
	offerTo(t, ev, "s1")
	epoch := acquire(t, c, "s1")
	_ = c.Close()
	c2 := f.dial(0, "boot-a")
	r, err := c2.Resume(wait.Ctx(t), &circusv1.ResumeLeases_Held{SessionId: "s1", Epoch: epoch})
	must(t, err)
	if !r.GetResumed().GetLeases()[0].GetOk() {
		t.Fatalf("resume %v", r)
	}
	f.input("s1", "later", store.InputNormal)
	f.settle()
	ev2 := f.attach(c2, 1)
	if n := nudge(t, ev2, "s1", circusv1.WorkerEvent_Nudge_REASON_INPUT); !hasReason(n, circusv1.WorkerEvent_Nudge_REASON_INPUT) {
		t.Fatalf("deferred nudge %v", n)
	}
}

// TestRecoverRevokes: RecoverSession releases a broken session's lease;
// the holder gets LeaseRevoked and is fenced.
func TestRecoverRevokes(t *testing.T) {
	f := newFixture(t)
	f.session("s1", -1)
	c := f.dial(0, "boot-a")
	ev := f.attach(c, 1)
	f.input("s1", "go", store.InputNormal)
	offerTo(t, ev, "s1")
	epoch := acquire(t, c, "s1")
	call(t, c, wc.TakeInputs("s1", epoch))
	call(t, c, wc.Started("s1", epoch, 1, circusv1.TurnOrigin_TURN_ORIGIN_INPUT))
	call(t, c, wc.Op("s1", epoch, &circusv1.ReportTurn{LocalSeq: 1, Phase: circusv1.ReportTurn_PHASE_ENDED, Outcome: circusv1.TurnOutcome_TURN_OUTCOME_ERROR, TranscriptIncomplete: true}))
	if f.get("s1").State != store.SessionBroken {
		t.Fatal("not broken")
	}
	// A broken session's holder takes no input; appends still work.
	if got := call(t, c, wc.TakeInputs("s1", epoch)).GetInputs().GetInputs(); len(got) != 0 {
		t.Fatalf("broken take %v", got)
	}
	wantOk(t, call(t, c, wc.Append("s1", epoch, "", 1, &circusv1.TranscriptEntry{Json: `{}`})), false)
	_, err := f.st.RecoverSession(t.Context(), "s1", true, ms(f), store.DefaultMaxViewBytes)
	must(t, err)
	f.svc.Wake("s1", circusv1.WorkerEvent_Nudge_REASON_INPUT)
	for {
		e := next(t, ev)
		if r := e.GetLeaseRevoked(); r != nil {
			if r.GetSessionId() != "s1" || r.GetEpoch() != epoch {
				t.Fatalf("revoked %v", r)
			}
			break
		}
	}
	wantFenced(t, call(t, c, wc.TakeInputs("s1", epoch)), epoch)
	// The recovered session has its taken input pending again: offered.
	offerTo(t, ev, "s1")
	r := call(t, c, wc.Acquire("s1", true))
	if r.GetLease().GetTranscriptGapTurnNo() != 1 {
		t.Fatalf("lease after recovery %v", r)
	}
}

// TestTranscriptRepaired: repairing a broken session nudges the holder for
// the inputs that waited.
func TestTranscriptRepaired(t *testing.T) {
	f := newFixture(t)
	f.session("s1", -1)
	c := f.dial(0, "boot-a")
	ev := f.attach(c, 1)
	f.input("s1", "go", store.InputNormal)
	offerTo(t, ev, "s1")
	epoch := acquire(t, c, "s1")
	call(t, c, wc.TakeInputs("s1", epoch))
	call(t, c, wc.Started("s1", epoch, 1, circusv1.TurnOrigin_TURN_ORIGIN_INPUT))
	call(t, c, wc.Op("s1", epoch, &circusv1.ReportTurn{LocalSeq: 1, Phase: circusv1.ReportTurn_PHASE_ENDED, Outcome: circusv1.TurnOutcome_TURN_OUTCOME_COMPLETED, TranscriptIncomplete: true}))
	f.input("s1", "while broken", store.InputNormal)
	nudge(t, ev, "s1")
	call(t, c, wc.TakeInputs("s1", epoch))
	wantOk(t, call(t, c, wc.Op("s1", epoch, &circusv1.TranscriptRepaired{})), false)
	if f.get("s1").State != store.SessionIdle {
		t.Fatal("not repaired")
	}
	if n := nudge(t, ev, "s1", circusv1.WorkerEvent_Nudge_REASON_INPUT); !hasReason(n, circusv1.WorkerEvent_Nudge_REASON_INPUT) {
		t.Fatalf("nudge after repair %v", n)
	}
	if got := call(t, c, wc.TakeInputs("s1", epoch)).GetInputs().GetInputs(); len(got) != 2 {
		t.Fatalf("inputs after repair %v", got)
	}
}

// TestLeaseRequestRules: epoch rules, NOT_OFFERED, NOT_FOUND and the
// connection without a boot id.
func TestLeaseRequestRules(t *testing.T) {
	f := newFixture(t)
	f.session("s1", -1)
	c := f.dial(0, "boot-a")
	f.attach(c, 1)
	wantRejected(t, call(t, c, wc.Acquire("s1", true)), circusv1.SessionReply_Rejected_CODE_NOT_OFFERED)
	wantRejected(t, call(t, c, wc.Acquire("missing", true)), circusv1.SessionReply_Rejected_CODE_NOT_FOUND)
	bad := wc.Acquire("s1", true)
	bad.Epoch = 1
	wantRejected(t, call(t, c, bad), circusv1.SessionReply_Rejected_CODE_INVALID)
	wantRejected(t, call(t, c, wc.TakeInputs("s1", 0)), circusv1.SessionReply_Rejected_CODE_INVALID)
	wantRejected(t, call(t, c, wc.TakeInputs("bad id", 1)), circusv1.SessionReply_Rejected_CODE_INVALID)
	wantRejected(t, call(t, c, wc.TakeInputs(strings.Repeat("s", 129), 1)), circusv1.SessionReply_Rejected_CODE_INVALID)
	wantRejected(t, call(t, c, &circusv1.SessionRequest{SessionId: "s1", Epoch: 1}), circusv1.SessionReply_Rejected_CODE_INVALID)
	cli := f.dial(1, "")
	wantRejected(t, call(t, cli, wc.Acquire("s1", true)), circusv1.SessionReply_Rejected_CODE_INVALID)
	r, err := cli.Resume(wait.Ctx(t), &circusv1.ResumeLeases_Held{SessionId: "s1", Epoch: 1})
	must(t, err)
	wantRejected(t, r, circusv1.SessionReply_Rejected_CODE_INVALID)
}
