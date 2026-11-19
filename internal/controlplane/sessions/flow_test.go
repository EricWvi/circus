package sessions_test

import (
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/controlplane/sessions"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/testutil/wait"
	wc "github.com/EricWvi/circus/internal/testutil/workerclient"
)

// TestTurnLifecycle: Offer → AcquireLease → TakeInputs → STARTED → events →
// ENDED COMPLETED → idle → release.
func TestTurnLifecycle(t *testing.T) {
	f := newFixture(t)
	f.session("s1", -1)
	c := f.dial(0, "boot-a")
	ev := f.attach(c, 2)
	in := f.input("s1", "hello", store.InputNormal)
	offerTo(t, ev, "s1")

	r := call(t, c, wc.Acquire("s1", true))
	l := r.GetLease()
	if l == nil || l.GetEpoch() != 1 || l.GetReattached() || l.GetClaudeSessionId() != "claude-s1" || l.GetHasTranscript() ||
		l.GetConfig().GetDefaultDevice() != "desk" || len(l.GetConfig().GetCwds()) != 1 || l.GetConfig().GetCwds()[0].GetCwd() != "/work" ||
		len(l.GetState().GetCwds()) != 1 || l.GetState().GetCwds()[0].GetDevice() != "desk" || l.GetState().GetStopped() {
		t.Fatalf("lease %v", r)
	}
	if f.get("s1").State != store.SessionIdle {
		t.Fatal("acquired session should be idle")
	}
	// An idempotent reacquisition by the holder keeps the epoch.
	if r := call(t, c, wc.Acquire("s1", false)); r.GetLease().GetEpoch() != 1 || !r.GetLease().GetReattached() {
		t.Fatalf("reattach %v", r)
	}

	r = call(t, c, wc.TakeInputs("s1", 1))
	if got := r.GetInputs().GetInputs(); len(got) != 1 || got[0].GetInputId() != in.InputID || got[0].GetText() != "hello" ||
		got[0].GetKind() != circusv1.SessionReply_Inputs_Input_KIND_USER || got[0].GetPriority() != circusv1.InputPriority_INPUT_PRIORITY_NORMAL {
		t.Fatalf("inputs %v", r)
	}
	r = call(t, c, wc.Started("s1", 1, 1, circusv1.TurnOrigin_TURN_ORIGIN_INPUT))
	if r.GetTurn().GetTurnNo() != 1 || f.get("s1").State != store.SessionRunning {
		t.Fatalf("started %v", r)
	}
	wantOk(t, call(t, c, wc.Append("s1", 1, "", 1, &circusv1.TranscriptEntry{Uuid: in.InputID, Json: `{"type":"user"}`}, &circusv1.TranscriptEntry{Json: `{"type":"assistant"}`})), false)
	wantOk(t, call(t, c, wc.Append("s1", 1, "", 1, &circusv1.TranscriptEntry{Uuid: in.InputID, Json: `{"type":"user"}`})), true)
	r = call(t, c, wc.Publish("s1", 1, 1,
		&circusv1.PublishEvents_Event{Kind: circusv1.SessionEvent_KIND_TOOL_PROGRESS, Json: `{"p":1}`, Ephemeral: true},
		&circusv1.PublishEvents_Event{Kind: circusv1.SessionEvent_KIND_ASSISTANT, Json: `{"a":1}`},
		&circusv1.PublishEvents_Event{Kind: circusv1.SessionEvent_KIND_RESULT, Json: `{"r":1}`}))
	wantOk(t, r, false)
	if f.bus.seq("s1") != 2 {
		t.Fatalf("durable seq %d, want 2", f.bus.seq("s1"))
	}
	if live := f.bus.live(); len(live) != 1 || live[0].GetSeq() != 0 || !live[0].GetEphemeral() || live[0].GetKind() != circusv1.SessionEvent_KIND_TOOL_PROGRESS {
		t.Fatalf("ephemeral %v", live)
	}
	// A retried batch is acknowledged without publishing again.
	wantOk(t, call(t, c, wc.Publish("s1", 1, 1, &circusv1.PublishEvents_Event{Kind: circusv1.SessionEvent_KIND_ASSISTANT, Json: `{}`})), true)
	// A live-only batch consumes its sequence and stores nothing.
	wantOk(t, call(t, c, wc.Publish("s1", 1, 2, &circusv1.PublishEvents_Event{Kind: circusv1.SessionEvent_KIND_ASSISTANT, Json: `{}`, Ephemeral: true})), false)
	if len(f.bus.live()) != 2 || f.get("s1").LastEventSeq != 2 {
		t.Fatal("live-only batch")
	}

	r = call(t, c, wc.Ended("s1", 1, 1, circusv1.TurnOutcome_TURN_OUTCOME_COMPLETED, &circusv1.ReportTurn_Persisted{InputIds: []string{in.InputID}}))
	if r.GetTurn().GetTurnNo() != 1 {
		t.Fatalf("ended %v", r)
	}
	s := f.get("s1")
	if s.State != store.SessionIdle || s.LastTurnOutcome != store.TurnCompleted || s.TurnCount != 1 {
		t.Fatalf("session after turn %+v", s)
	}
	page := call(t, c, wc.Load("s1", 1, "", 0, 1<<20)).GetTranscriptPage()
	if len(page.GetEntries()) != 2 || !page.GetDone() || page.GetNextSeq() != 2 {
		t.Fatalf("page %v", page)
	}
	if k := call(t, c, wc.Op("s1", 1, &circusv1.ListTranscriptKeys{})).GetTranscriptKeys(); len(k.GetSubpaths()) != 1 || k.GetSubpaths()[0] != "" {
		t.Fatalf("keys %v", k)
	}
	wantOk(t, call(t, c, wc.Release("s1", 1, circusv1.ReleaseLease_REASON_IDLE)), false)
	wantOk(t, call(t, c, wc.Release("s1", 1, circusv1.ReleaseLease_REASON_IDLE)), true)
	if f.lease("s1").WorkerID != "" {
		t.Fatal("lease not released")
	}
	f.settle()
	noOffer(t, ev)
	// The next acquisition sees the transcript.
	f.input("s1", "again", store.InputNormal)
	offerTo(t, ev, "s1")
	if r := call(t, c, wc.Acquire("s1", true)); r.GetLease().GetEpoch() != 2 || !r.GetLease().GetHasTranscript() {
		t.Fatalf("second lease %v", r)
	}
	wantFenced(t, call(t, c, wc.Release("s1", 1, circusv1.ReleaseLease_REASON_IDLE)), 2)
}

// TestTurnStoppedAndInterrupt: Interrupt raises the level to the running
// turn and nudges the holder; ENDED STOPPED records the boundary event.
func TestTurnStoppedAndInterrupt(t *testing.T) {
	f := newFixture(t)
	f.session("s1", -1)
	c := f.dial(0, "boot-a")
	ev := f.attach(c, 1)
	f.input("s1", "go", store.InputNormal)
	offerTo(t, ev, "s1")
	epoch := acquire(t, c, "s1")
	call(t, c, wc.TakeInputs("s1", epoch))
	call(t, c, wc.Started("s1", epoch, 1, circusv1.TurnOrigin_TURN_ORIGIN_INPUT))

	_, _, changed, err := f.st.InterruptSession(t.Context(), "s1", ms(f))
	must(t, err)
	if !changed {
		t.Fatal("interrupt changed nothing")
	}
	f.svc.Wake("s1", circusv1.WorkerEvent_Nudge_REASON_INTERRUPT)
	n := nudge(t, ev, "s1", circusv1.WorkerEvent_Nudge_REASON_INTERRUPT)
	if !hasReason(n, circusv1.WorkerEvent_Nudge_REASON_INTERRUPT) || n.GetInterruptTurnNo() != 1 {
		t.Fatalf("nudge %v", n)
	}
	st := call(t, c, wc.State("s1", epoch, circusv1.GetSessionState_SECTION_NONE)).GetState().GetState()
	if st.GetInterruptTurnNo() != 1 || len(st.GetCwds()) != 0 {
		t.Fatalf("state %v", st)
	}
	before := f.bus.seq("s1")
	call(t, c, wc.Ended("s1", epoch, 1, circusv1.TurnOutcome_TURN_OUTCOME_STOPPED, nil))
	if f.bus.seq("s1") <= before || f.get("s1").LastTurnOutcome != store.TurnStopped {
		t.Fatal("TURN_STOPPED not announced")
	}
	page, err := f.st.ReadEvents(t.Context(), store.EventFilter{SessionID: "s1", Limit: 10, MaxBytes: 1 << 20})
	must(t, err)
	if last := page.Events[len(page.Events)-1]; last.Kind != store.EventKindTurnStopped {
		t.Fatalf("last event %v", last)
	}
	// An interrupt with no running turn changes nothing.
	if _, _, changed, _ := f.st.InterruptSession(t.Context(), "s1", ms(f)); changed {
		t.Fatal("idle interrupt changed the level")
	}
}

// TestStopFlow: a stopped session refuses new work, accepts the holder's
// flush, nudges STOP and lets the holder release.
func TestStopFlow(t *testing.T) {
	f := newFixture(t)
	f.session("s1", -1)
	c := f.dial(0, "boot-a")
	ev := f.attach(c, 1)
	f.input("s1", "go", store.InputNormal)
	offerTo(t, ev, "s1")
	epoch := acquire(t, c, "s1")
	_, _, _, err := f.st.StopSession(t.Context(), "s1", ms(f), "USER")
	must(t, err)
	f.svc.Wake("s1", circusv1.WorkerEvent_Nudge_REASON_STOP)
	if n := nudge(t, ev, "s1", circusv1.WorkerEvent_Nudge_REASON_STOP); !hasReason(n, circusv1.WorkerEvent_Nudge_REASON_STOP) {
		t.Fatalf("nudge %v", n)
	}
	wantRejected(t, call(t, c, wc.TakeInputs("s1", epoch)), circusv1.SessionReply_Rejected_CODE_STOPPED)
	match := "0000000000000000000000000000000000000000000000000000000000000000"
	wantRejected(t, call(t, c, wc.Op("s1", epoch, &circusv1.RequestApproval{ToolUseId: "t1", Tool: "bash", Device: "desk", MatchKey: match})), circusv1.SessionReply_Rejected_CODE_STOPPED)
	wantRejected(t, call(t, c, wc.Op("s1", epoch, &circusv1.ConsumeGrant{ToolUseId: "t1", Tool: "bash", Device: "desk", MatchKey: match})), circusv1.SessionReply_Rejected_CODE_STOPPED)
	wantOk(t, call(t, c, wc.Append("s1", epoch, "", 1, &circusv1.TranscriptEntry{Json: `{}`})), false)
	if !call(t, c, wc.State("s1", epoch)).GetState().GetState().GetStopped() {
		t.Fatal("state not stopped")
	}
	// The holder's reattachment still works; a new holder's does not.
	if r := call(t, c, wc.Acquire("s1", false)); !r.GetLease().GetState().GetStopped() {
		t.Fatalf("reattach %v", r)
	}
	c2 := f.dial(1, "boot-b")
	wantRejected(t, call(t, c2, wc.Acquire("s1", true)), circusv1.SessionReply_Rejected_CODE_NOT_OFFERED)
	wantOk(t, call(t, c, wc.Release("s1", epoch, circusv1.ReleaseLease_REASON_STOPPED)), false)
	if f.lease("s1").WorkerID != "" {
		t.Fatal("not released")
	}
}

// TestFreshAcquireOfStoppedReleases: a holder without local state that
// acquires a stopped session fresh gets STOPPED and loses the lease.
func TestFreshAcquireOfStoppedReleases(t *testing.T) {
	f := newFixture(t)
	f.session("s1", -1)
	c := f.dial(0, "boot-a")
	ev := f.attach(c, 1)
	f.input("s1", "go", store.InputNormal)
	offerTo(t, ev, "s1")
	acquire(t, c, "s1")
	_, _, _, err := f.st.StopSession(t.Context(), "s1", ms(f), "USER")
	must(t, err)
	wantRejected(t, call(t, c, wc.Acquire("s1", true)), circusv1.SessionReply_Rejected_CODE_STOPPED)
	if f.lease("s1").WorkerID != "" {
		t.Fatal("fresh STOPPED did not release")
	}
}

// TestBootChange: a new worker process releases every lease of the old
// boot, closing its open turn as interrupted.
func TestBootChange(t *testing.T) {
	f := newFixture(t)
	f.session("s1", -1)
	c := f.dial(0, "boot-a")
	ev := f.attach(c, 1)
	f.input("s1", "go", store.InputNormal)
	offerTo(t, ev, "s1")
	epoch := acquire(t, c, "s1")
	call(t, c, wc.TakeInputs("s1", epoch))
	call(t, c, wc.Started("s1", epoch, 1, circusv1.TurnOrigin_TURN_ORIGIN_INPUT))
	seq := f.bus.seq("s1")

	c2 := f.dial(0, "boot-b")
	// The old connection (half-open, in a real crash) is told and fenced.
	e := next(t, ev)
	if e.GetLeaseRevoked().GetSessionId() != "s1" || e.GetLeaseRevoked().GetEpoch() != epoch {
		t.Fatalf("revoked %v", e)
	}
	l := f.lease("s1")
	s := f.get("s1")
	if l.WorkerID != "" || s.LastTurnOutcome != store.TurnInterrupted || f.bus.seq("s1") <= seq {
		t.Fatalf("after boot change: lease %+v session %+v", l, s)
	}
	wantFenced(t, call(t, c, wc.TakeInputs("s1", epoch)), epoch)
	// The input taken by the lost epoch is pending again: offered to the
	// new process once it attaches.
	ev2 := f.attach(c2, 1)
	offerTo(t, ev2, "s1")
	r := call(t, c2, wc.Acquire("s1", true))
	if r.GetLease().GetEpoch() != epoch+1 || !r.GetLease().GetRecovery().GetPreviousTurnInterrupted() {
		t.Fatalf("new lease %v", r)
	}
}

// TestExtensionAndExpiry: leases bound to a live connection are extended
// by the upkeep loop; a closed connection's lease expires, its open turn
// is interrupted and the session is offered again.
func TestExtensionAndExpiry(t *testing.T) {
	f := newFixture(t)
	// Without the fallback the re-offer must come from the lease loss.
	sessions.DisableFallback(f.svc)
	f.session("s1", -1)
	c := f.dial(0, "boot-a")
	ev := f.attach(c, 1)
	f.input("s1", "go", store.InputNormal)
	offerTo(t, ev, "s1")
	epoch := acquire(t, c, "s1")
	call(t, c, wc.TakeInputs("s1", epoch))
	call(t, c, wc.Started("s1", epoch, 1, circusv1.TurnOrigin_TURN_ORIGIN_INPUT))
	for range 5 {
		round := sessions.Rounds(f.svc)
		f.clk.Advance(sessions.ExtendInterval)
		want := ms(f) + sessions.LeaseLifetime.Milliseconds()
		wait.For(t, func() bool { return f.lease("s1").ExpiresAt == want && sessions.Rounds(f.svc) > round })
	}
	if r := call(t, c, wc.TakeInputs("s1", epoch)); r.GetInputs() == nil {
		t.Fatalf("live lease fenced after 100 s: %v", r)
	}

	// Another worker waits; the first one disappears mid-turn.
	c2 := f.dial(1, "boot-b")
	ev2 := f.attach(c2, 1)
	f.input("s1", "more", store.InputNormal)
	n := nudge(t, ev, "s1", circusv1.WorkerEvent_Nudge_REASON_INPUT)
	if !hasReason(n, circusv1.WorkerEvent_Nudge_REASON_INPUT) {
		t.Fatalf("nudge %v", n)
	}
	seq := f.bus.seq("s1")
	_ = c.Close()
	f.clk.Advance(sessions.LeaseLifetime + time.Second)
	sessions.Expire(f.svc, t.Context())
	offerTo(t, ev2, "s1")
	s := f.get("s1")
	if f.bus.seq("s1") <= seq || s.LastTurnOutcome != store.TurnInterrupted || s.State != store.SessionQueued {
		t.Fatalf("after expiry %+v", s)
	}
	r := call(t, c2, wc.Acquire("s1", true))
	if r.GetLease().GetEpoch() != epoch+1 || !r.GetLease().GetRecovery().GetPreviousTurnInterrupted() {
		t.Fatalf("takeover %v", r)
	}
	if got := call(t, c2, wc.TakeInputs("s1", epoch+1)).GetInputs().GetInputs(); len(got) != 2 {
		t.Fatalf("requeued inputs %v", got)
	}
}
