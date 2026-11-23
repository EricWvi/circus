package sessions_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/controlplane/sessions"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/testutil/wait"
	wc "github.com/EricWvi/circus/internal/testutil/workerclient"
)

// eventWithin returns the first hint on ev matching pred within d, or nil.
func eventWithin(ev wc.Events, d time.Duration, pred func(*circusv1.WorkerEvent) bool) *circusv1.WorkerEvent {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	for {
		e, err := ev.Next(ctx)
		if err != nil {
			return nil
		}
		if pred(e) {
			return e
		}
	}
}

func isOffer(id string) func(*circusv1.WorkerEvent) bool {
	return func(e *circusv1.WorkerEvent) bool { return e.GetOffer().GetSessionId() == id }
}

func isRevoked(id string, epoch uint64) func(*circusv1.WorkerEvent) bool {
	return func(e *circusv1.WorkerEvent) bool {
		return e.GetLeaseRevoked().GetSessionId() == id && e.GetLeaseRevoked().GetEpoch() == epoch
	}
}

// TestExtensionUnderSchedulerLoad: a long scheduling backlog (hundreds of
// Wakes, each a slow store transaction, with acquisitions and releases
// going on) does not delay the extension of live leases.
func TestExtensionUnderSchedulerLoad(t *testing.T) { extensionUnderLoad(t, false) }

// TestExtensionUnderStoreContention: as TestExtensionUnderSchedulerLoad,
// but each scheduler transaction holds the store's one connection while it
// is slow, so the upkeep's extension competes with it for the connection.
func TestExtensionUnderStoreContention(t *testing.T) { extensionUnderLoad(t, true) }

func extensionUnderLoad(t *testing.T, contend bool) {
	slow := &slowStore{}
	f := newFixture(t, withSlowStore(slow))
	f.session("held", -1)
	c := f.dial(0, "boot-a")
	ev := f.attach(c, 2)
	f.input("held", "go", store.InputNormal)
	offerTo(t, ev, "held")
	acquire(t, c, "held")
	const n = 300
	for i := range n {
		id := fmt.Sprintf("q%03d", i)
		f.session(id, -1)
		_, err := f.st.SubmitInput(t.Context(), store.Input{InputID: id + "-i", SessionID: id, ClientMsgID: "m", Kind: store.InputUser, Text: "x", Priority: store.InputNormal, CreatedAt: ms(f)})
		must(t, err)
	}
	// n × 30 ms ≈ 9 s of scheduling work, queued at once.
	if contend {
		slow.hold.Store(int64(30 * time.Millisecond))
	} else {
		slow.settle.Store(int64(30 * time.Millisecond))
	}
	for i := range n {
		f.svc.Wake(fmt.Sprintf("q%03d", i), circusv1.WorkerEvent_Nudge_REASON_INPUT)
	}
	// Churn: the worker takes and gives back offered sessions meanwhile.
	churn, stop := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		for churn.Err() == nil {
			e, err := ev.Next(churn)
			if err != nil || e.GetOffer() == nil {
				continue
			}
			r, err := c.Session(churn, wc.Acquire(e.GetOffer().GetSessionId(), true))
			if err == nil && r.GetLease() != nil {
				_, _ = c.Session(churn, wc.Release(e.GetOffer().GetSessionId(), r.GetLease().GetEpoch(), circusv1.ReleaseLease_REASON_IDLE))
			}
		}
	}()
	defer func() { stop(); <-done }()
	time.Sleep(50 * time.Millisecond)
	if sessions.Idle(f.svc) {
		t.Fatal("the scheduler is not busy; the test proves nothing")
	}
	round := sessions.Rounds(f.svc)
	f.clk.Advance(sessions.ExtendInterval)
	want := ms(f) + sessions.LeaseLifetime.Milliseconds()
	// The round (which ends after the extension) is watched without the
	// store, whose one connection the test's own reads would wait for.
	deadline := time.Now().Add(3 * time.Second)
	for sessions.Rounds(f.svc) == round {
		if time.Now().After(deadline) {
			t.Fatal("the upkeep round did not finish while the scheduler was busy")
		}
		time.Sleep(time.Millisecond)
	}
	if sessions.Idle(f.svc) {
		t.Fatal("the scheduler finished first; the test proves nothing")
	}
	if got := f.lease("held").ExpiresAt; got != want {
		t.Fatalf("lease expires at %d, want %d: not extended while the scheduler was busy", got, want)
	}
	slow.settle.Store(0)
	slow.hold.Store(0)
}

// TestOlderBootRefused: once a newer boot of the worker connected, an older
// one is refused at Hello and releases nothing; the newer boot keeps its
// lease and its Offers.
func TestOlderBootRefused(t *testing.T) {
	f := newFixture(t)
	f.session("s1", -1)
	f.session("s2", -1)
	f.boot("old") // minted first: the older boot
	newer := f.dial(0, "new")
	evN := f.attach(newer, 4)
	f.input("s1", "go", store.InputNormal)
	offerTo(t, evN, "s1")
	epoch := acquire(t, newer, "s1")
	if _, err := f.tryDial(0, "old"); err == nil || !strings.Contains(err.Error(), "BAD_HELLO") {
		t.Fatalf("older boot dial: %v", err)
	}
	if r := call(t, newer, wc.TakeInputs("s1", epoch)); r.GetInputs() == nil {
		t.Fatalf("newer boot fenced after an older boot's Hello: %v", r)
	}
	f.input("s2", "go", store.InputNormal)
	offerTo(t, evN, "s2")
	// A boot id that is not a UUIDv7 is refused too.
	f.mu.Lock()
	f.boots["v4"] = "6ba7b810-9dad-41d1-80b4-00c04fd430c8"
	f.mu.Unlock()
	if _, err := f.tryDial(0, "v4"); err == nil || !strings.Contains(err.Error(), "BAD_HELLO") {
		t.Fatalf("non-UUIDv7 boot dial: %v", err)
	}
}

// TestStaleBootConnection: a connection of a boot the worker has since
// replaced gets no Offers; when it goes away the current boot's connection
// keeps getting them (staleness follows the current boot).
func TestStaleBootConnection(t *testing.T) {
	f := newFixture(t)
	f.session("s1", -1)
	f.session("s2", -1)
	old := f.dial(0, "old")
	evO := f.attach(old, 4)
	newer := f.dial(0, "new")
	evN := f.attach(newer, 1)
	f.input("s1", "go", store.InputNormal)
	offerTo(t, evN, "s1")
	acquire(t, newer, "s1")
	// The newer connection is full; the old one has room but is stale.
	f.input("s2", "go", store.InputNormal)
	f.settle()
	if e := eventWithin(evO, 200*time.Millisecond, isOffer("s2")); e != nil {
		t.Fatal("offered to a stale boot's connection")
	}
	_ = old.Close()
	turn(t, newer, "s1", 1, 1)
	call(t, newer, wc.Release("s1", 1, circusv1.ReleaseLease_REASON_IDLE))
	offerTo(t, evN, "s2")
}

// TestRestartResume: after a control-plane restart the first resumption
// re-sends INPUT and JOB, and INTERRUPT with the stored level when the open
// turn is at or below it; the Nudge carries that level.
func TestRestartResume(t *testing.T) {
	f := newFixture(t)
	f.session("s1", -1)
	c := f.dial(0, "boot-a")
	ev := f.attach(c, 1)
	f.input("s1", "go", store.InputNormal)
	offerTo(t, ev, "s1")
	epoch := acquire(t, c, "s1")
	call(t, c, wc.TakeInputs("s1", epoch))
	call(t, c, wc.Started("s1", epoch, 1, circusv1.TurnOrigin_TURN_ORIGIN_INPUT))
	// The interrupt is committed, but the process dies before its Wake.
	_, _, changed, err := f.st.InterruptSession(t.Context(), "s1", ms(f))
	must(t, err)
	if !changed {
		t.Fatal("no interrupt")
	}
	f.restart()
	c2 := f.dial(0, "boot-a")
	ev2 := f.attach(c2, 1)
	if !call2resume(t, c2, "s1", epoch) {
		t.Fatal("resume after restart")
	}
	n := nudge(t, ev2, "s1", circusv1.WorkerEvent_Nudge_REASON_INTERRUPT)
	for _, r := range []circusv1.WorkerEvent_Nudge_Reason{circusv1.WorkerEvent_Nudge_REASON_INPUT, circusv1.WorkerEvent_Nudge_REASON_JOB, circusv1.WorkerEvent_Nudge_REASON_INTERRUPT} {
		if !hasReason(n, r) {
			t.Fatalf("nudge %v lacks %v", n, r)
		}
	}
	if n.GetInterruptTurnNo() != 1 {
		t.Fatalf("nudge level %d, want 1", n.GetInterruptTurnNo())
	}
}

// TestAcquireKeepsInput: a reattaching AcquireLease does not clear INPUT,
// which only TakeInputs clears; it is re-sent after a later resume.
func TestAcquireKeepsInput(t *testing.T) {
	f := newFixture(t)
	f.session("s1", -1)
	a := f.dial(0, "boot-a")
	evA := f.attach(a, 4)
	f.input("s1", "go", store.InputNormal)
	offerTo(t, evA, "s1")
	epoch := acquire(t, a, "s1")
	f.input("s1", "second", store.InputNormal)
	nudge(t, evA, "s1")
	b := f.dial(0, "boot-a")
	f.attach(b, 4)
	if r := call(t, b, wc.Acquire("s1", false)); !r.GetLease().GetReattached() {
		t.Fatalf("reattach %v", r)
	}
	cc := f.dial(0, "boot-a")
	evC := f.attach(cc, 4)
	if !call2resume(t, cc, "s1", epoch) {
		t.Fatal("resume")
	}
	if n := nudge(t, evC, "s1", circusv1.WorkerEvent_Nudge_REASON_INPUT); !hasReason(n, circusv1.WorkerEvent_Nudge_REASON_INPUT) {
		t.Fatalf("nudge %v", n)
	}
}

// TestOfferMovesOnAttach: an Offer held by the worker's previous connection
// (same boot) moves to the connection that attaches.
func TestOfferMovesOnAttach(t *testing.T) {
	f := newFixture(t)
	sessions.DisableFallback(f.svc)
	f.session("s1", -1)
	a := f.dial(0, "boot-a")
	evA := f.attach(a, 4)
	f.input("s1", "go", store.InputNormal)
	offerTo(t, evA, "s1")
	b := f.dial(0, "boot-a")
	evB := f.attach(b, 4)
	offerTo(t, evB, "s1")
	if r := call(t, b, wc.Acquire("s1", true)); r.GetLease() == nil {
		t.Fatalf("acquire on the attaching connection %v", r)
	}
}

// TestUnwatchedAcquire: the lease of an AcquireLease whose reply never
// reached the link is not extended, so it expires instead of staying
// bound to a worker that never learned it.
func TestUnwatchedAcquire(t *testing.T) {
	slow := &slowStore{}
	f := newFixture(t, withSlowStore(slow))
	sessions.DisableFallback(f.svc)
	f.session("s1", -1)
	c := f.dial(0, "boot-a")
	ev := f.attach(c, 1)
	f.input("s1", "go", store.InputNormal)
	offerTo(t, ev, "s1")
	slow.tx.Store(int64(300 * time.Millisecond))
	r, err := c.Send(wait.Ctx(t), wc.Acquire("s1", true))
	must(t, err)
	must(t, r.Unwatch(wait.Ctx(t)))
	wait.For(t, func() bool { return sessions.Pending(f.svc, "s1") })
	slow.tx.Store(0)
	time.Sleep(400 * time.Millisecond) // the aborted read is gone
	// Abandoned whichever won: the Unwatch or the reply.
	if l := f.lease("s1"); l.WorkerID == "" || !sessions.Abandoned(f.svc, "s1") {
		t.Fatalf("lease %+v", l)
	}
	before := f.lease("s1").ExpiresAt
	round := sessions.Rounds(f.svc)
	f.clk.Advance(sessions.ExtendInterval)
	wait.For(t, func() bool { return sessions.Rounds(f.svc) > round })
	if f.lease("s1").ExpiresAt != before {
		t.Fatal("an undelivered lease was extended")
	}
	f.clk.Advance(sessions.LeaseLifetime)
	wait.For(t, func() bool { return f.lease("s1").WorkerID == "" })
}

// TestLeaseDeliveredNudges: reasons still pending when a Lease reply is
// delivered are nudged after it; INPUT is among them.
func TestLeaseDeliveredNudges(t *testing.T) {
	f := newFixture(t)
	f.session("s1", -1)
	c := f.dial(0, "boot-a")
	ev := f.attach(c, 1)
	f.input("s1", "go", store.InputNormal)
	offerTo(t, ev, "s1")
	acquire(t, c, "s1")
	f.input("s1", "second", store.InputNormal)
	nudge(t, ev, "s1")
	if r := call(t, c, wc.Acquire("s1", false)); !r.GetLease().GetReattached() {
		t.Fatalf("reattach %v", r)
	}
	if n := nudge(t, ev, "s1", circusv1.WorkerEvent_Nudge_REASON_INPUT); !hasReason(n, circusv1.WorkerEvent_Nudge_REASON_INPUT) {
		t.Fatalf("nudge after the Lease reply %v", n)
	}
}

// TestReasonGeneration: an operation clears a reason only if the Wake that
// raised it was no later than what the operation observed.
func TestReasonGeneration(t *testing.T) {
	f := newFixture(t)
	f.session("s1", -1)
	c := f.dial(0, "boot-a")
	ev := f.attach(c, 1)
	f.input("s1", "go", store.InputNormal)
	offerTo(t, ev, "s1")
	epoch := int64(acquire(t, c, "s1"))
	f.settle()
	g := sessions.Generation(f.svc)
	f.input("s1", "later", store.InputNormal)
	f.settle()
	sessions.ClearInput(f.svc, "s1", epoch, g)
	if !sessions.HasInput(f.svc, "s1") {
		t.Fatal("a reason raised after the operation's generation was cleared")
	}
	sessions.ClearInput(f.svc, "s1", epoch, sessions.Generation(f.svc))
	if sessions.HasInput(f.svc, "s1") {
		t.Fatal("a seen reason stayed")
	}
}

// TestRefusedAcquireWithdrawsOffer: an AcquireLease refused because the
// session stopped frees the Offer's capacity for another session.
func TestRefusedAcquireWithdrawsOffer(t *testing.T) {
	f := newFixture(t)
	sessions.DisableFallback(f.svc)
	f.session("s1", -1)
	f.session("s2", -1)
	c := f.dial(0, "boot-a")
	ev := f.attach(c, 1)
	f.input("s1", "go", store.InputNormal)
	offerTo(t, ev, "s1")
	f.input("s2", "go", store.InputNormal)
	f.settle()
	_, _, _, err := f.st.StopSession(t.Context(), "s1", ms(f), "USER")
	must(t, err)
	wantRejected(t, call(t, c, wc.Acquire("s1", true)), circusv1.SessionReply_Rejected_CODE_STOPPED)
	offerTo(t, ev, "s2")
}

// TestExtensionLossRevokes: a lease the store no longer has as bound here
// (released behind this process's back) is revoked when the extension
// finds it lost.
func TestExtensionLossRevokes(t *testing.T) {
	f := newFixture(t)
	sessions.DisableFallback(f.svc)
	f.session("s1", -1)
	c := f.dial(0, "boot-a")
	ev := f.attach(c, 1)
	f.input("s1", "go", store.InputNormal)
	offerTo(t, ev, "s1")
	epoch := acquire(t, c, "s1")
	l := f.lease("s1")
	_, err := f.st.ReleaseLease(t.Context(), store.Fence{SessionID: "s1", Epoch: l.Epoch, WorkerID: l.WorkerID, BootID: l.BootID, ConnID: l.ConnID, Now: ms(f)})
	must(t, err)
	f.clk.Advance(sessions.ExtendInterval)
	if eventWithin(ev, wait.Timeout, isRevoked("s1", epoch)) == nil {
		t.Fatal("no LeaseRevoked after the extension lost the lease")
	}
}

// TestExpiryRevokesAndOffers: a lease that expires while its connection is
// still registered is revoked there, and the session with work is offered
// again by the lease loss itself (the fallback is off).
func TestExpiryRevokesAndOffers(t *testing.T) {
	f := newFixture(t)
	sessions.DisableFallback(f.svc)
	f.session("s1", -1)
	c := f.dial(0, "boot-a")
	ev := f.attach(c, 1)
	f.input("s1", "go", store.InputNormal)
	offerTo(t, ev, "s1")
	epoch := acquire(t, c, "s1")
	c2 := f.dial(1, "boot-b")
	ev2 := f.attach(c2, 1)
	// One jump past the lifetime: the sweep runs before any extension.
	f.clk.Advance(sessions.LeaseLifetime + time.Second)
	if eventWithin(ev, wait.Timeout, isRevoked("s1", epoch)) == nil {
		t.Fatal("no LeaseRevoked on expiry")
	}
	offerTo(t, ev2, "s1")
}

// TestTakeoverRevokes: a fresh acquisition on another connection of the
// holder's boot revokes the lease on the old connection.
func TestTakeoverRevokes(t *testing.T) {
	f := newFixture(t)
	f.session("s1", -1)
	a := f.dial(0, "boot-a")
	evA := f.attach(a, 1)
	f.input("s1", "go", store.InputNormal)
	offerTo(t, evA, "s1")
	epoch := acquire(t, a, "s1")
	b := f.dial(0, "boot-a")
	f.attach(b, 1)
	if r := call(t, b, wc.Acquire("s1", true)); r.GetLease().GetEpoch() != epoch+1 {
		t.Fatalf("fresh acquisition %v", r)
	}
	if eventWithin(evA, wait.Timeout, isRevoked("s1", epoch)) == nil {
		t.Fatal("no LeaseRevoked on the old connection")
	}
}

// TestBrokenSessionRefusals: a broken session's approvals and grants are
// BROKEN, and its calls SESSION_STOPPED.
func TestBrokenSessionRefusals(t *testing.T) {
	f := newFixture(t)
	c, _, epoch := held(t, f)
	call(t, c, wc.TakeInputs("s1", epoch))
	call(t, c, wc.Started("s1", epoch, 1, circusv1.TurnOrigin_TURN_ORIGIN_INPUT))
	call(t, c, wc.Op("s1", epoch, &circusv1.ReportTurn{LocalSeq: 1, Phase: circusv1.ReportTurn_PHASE_ENDED, Outcome: circusv1.TurnOutcome_TURN_OUTCOME_ERROR, TranscriptIncomplete: true}))
	wantRejected(t, call(t, c, wc.Op("s1", epoch, &circusv1.RequestApproval{ToolUseId: "t", Tool: "bash", Device: "desk", MatchKey: matchKey})), circusv1.SessionReply_Rejected_CODE_BROKEN)
	wantRejected(t, call(t, c, wc.Op("s1", epoch, &circusv1.ConsumeGrant{ToolUseId: "t", Tool: "bash", Device: "desk", MatchKey: matchKey})), circusv1.SessionReply_Rejected_CODE_BROKEN)
	wantCallRejected(t, submit(t, c, &circusv1.SubmitCall{Device: "desk", Call: bash("b1", "s1"), SessionEpoch: epoch, ToolUseId: "tu"}), circusv1.CallUpdate_Rejected_CODE_SESSION_STOPPED)
}

// TestUnpersistedCallCap: 1024 calls not yet persisted refuse the next one
// with OVERLOADED, recording nothing.
func TestUnpersistedCallCap(t *testing.T) {
	f := newFixture(t)
	c, _, epoch := held(t, f)
	for i := range store.MaxUnpersistedSessionCalls {
		must(t, f.st.CreateCall(t.Context(), store.Call{CallID: fmt.Sprintf("done%d", i), UserID: f.user.ID, DeviceID: f.device.ID, SessionID: "s1",
			Tool: "bash", RequestHash: []byte{1}, RequestBytes: 1, State: store.CallOffline, SubmittedAt: ms(f), FinishedAt: ms(f)}))
	}
	wantCallRejected(t, submit(t, c, &circusv1.SubmitCall{Device: "desk", Call: bash("over", "s1"), SessionEpoch: epoch, ToolUseId: "tu"}), circusv1.CallUpdate_Rejected_CODE_OVERLOADED)
	if _, err := f.st.GetCall(t.Context(), "over"); err == nil {
		t.Fatal("recorded")
	}
}

// TestEndedClearsInterrupt: ENDED clears INTERRUPT, so a later resume does
// not re-send it.
func TestEndedClearsInterrupt(t *testing.T) {
	f := newFixture(t)
	// held returns after its input Wake was evaluated, so TakeInputs clears
	// INPUT for good and any Nudge after the resume below can only be a
	// re-sent INTERRUPT.
	c, ev, epoch := held(t, f)
	call(t, c, wc.TakeInputs("s1", epoch))
	call(t, c, wc.Started("s1", epoch, 1, circusv1.TurnOrigin_TURN_ORIGIN_INPUT))
	_, _, _, err := f.st.InterruptSession(t.Context(), "s1", ms(f))
	must(t, err)
	f.svc.Wake("s1", circusv1.WorkerEvent_Nudge_REASON_INTERRUPT)
	nudge(t, ev, "s1", circusv1.WorkerEvent_Nudge_REASON_INTERRUPT)
	f.settle()
	call(t, c, wc.Ended("s1", epoch, 1, circusv1.TurnOutcome_TURN_OUTCOME_STOPPED, nil))
	c2 := f.dial(0, "boot-a")
	ev2 := f.attach(c2, 1)
	if !call2resume(t, c2, "s1", epoch) {
		t.Fatal("resume")
	}
	f.settle()
	if e := eventWithin(ev2, 300*time.Millisecond, func(e *circusv1.WorkerEvent) bool { return e.GetNudge() != nil }); e != nil {
		t.Fatalf("re-sent after ENDED: %v", e)
	}
}

// TestUnwatchAttach: an unwatched Attach stream gets no more Offers, and
// those it had go to another connection.
func TestUnwatchAttach(t *testing.T) {
	f := newFixture(t)
	sessions.DisableFallback(f.svc)
	f.session("s1", -1)
	c := f.dial(0, "boot-a")
	ev := f.attach(c, 1)
	c2 := f.dial(1, "boot-b")
	ev2 := f.attach(c2, 1)
	f.input("s1", "go", store.InputNormal)
	offerTo(t, ev2, "s1") // the newest connection wins a tie
	mine, other := ev2, ev
	before := sessions.Attached(f.svc)
	must(t, mine.Unwatch(wait.Ctx(t)))
	wait.For(t, func() bool { return sessions.Attached(f.svc) < before })
	offerTo(t, other, "s1")
}
