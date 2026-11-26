package sessions_test

import (
	"context"
	"slices"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/controlplane/sessions"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/testutil/wait"
	wc "github.com/EricWvi/circus/internal/testutil/workerclient"
)

// releaseIdle waits for a ReleaseIdle on ev and returns it; Offers and
// Nudges in between are skipped.
func (f *fixture) releaseIdle(c *wc.Client, ev wc.Events) *circusv1.WorkerEvent_ReleaseIdle {
	f.t.Helper()
	for {
		e := next(f.t, ev)
		if r := e.GetReleaseIdle(); r != nil {
			return r
		}
		f.drain(c, e)
	}
}

// drain acts on a Nudge as a worker does before it may be asked to
// release: it takes the session's inputs (TakeInputs is idempotent, so a
// session whose inputs a test takes later gets them again). A spurious
// reason a stale read raised again is cleared that way, as the real
// worker's TakeInputs clears it.
func (f *fixture) drain(c *wc.Client, e *circusv1.WorkerEvent) {
	f.t.Helper()
	n := e.GetNudge()
	if n == nil || !(hasReason(n, circusv1.WorkerEvent_Nudge_REASON_INPUT) || hasReason(n, circusv1.WorkerEvent_Nudge_REASON_APPROVAL)) {
		return
	}
	l := f.lease(n.GetSessionId())
	if l.Epoch > 0 {
		// Fenced when another connection holds it: nothing to do then.
		call(f.t, c, wc.TakeInputs(n.GetSessionId(), uint64(l.Epoch)))
	}
}

// noReleaseIdle asserts that no ReleaseIdle arrives once the scheduler
// has settled (and within a short time after); other hints are allowed.
func (f *fixture) noReleaseIdle(c *wc.Client, ev wc.Events) {
	f.t.Helper()
	f.settle()
	ctx, cancel := context.WithTimeout(f.t.Context(), 200*time.Millisecond)
	defer cancel()
	for {
		e, err := ev.Next(ctx)
		if err != nil {
			return
		}
		if e.GetReleaseIdle() != nil {
			f.t.Fatalf("unexpected release request %v", e)
		}
		f.drain(c, e)
	}
}

// heldIdle offers, acquires and runs one turn of a new session id under
// provider p on c, leaving it idle with its lease held; it returns the
// epoch.
func (f *fixture) heldIdle(c *wc.Client, ev wc.Events, id, p string) uint64 {
	f.t.Helper()
	f.providerSession(id, p, -1)
	f.input(id, "1", store.InputNormal)
	offerTo(f.t, ev, id)
	epoch := acquire(f.t, c, id)
	turn(f.t, c, id, epoch, 1)
	return epoch
}

// TestReleaseIdleForProviderFull: a session queued as PROVIDER_FULL makes
// the control plane ask the holder of the provider's idle lease to release
// it; the holder's ordinary idle release then lets the waiting session
// run.
func TestReleaseIdleForProviderFull(t *testing.T) {
	box := newBox(t, testKey)
	f := newFixture(t, withSecrets(box))
	p := f.provider(box, "prov", 1, 0)
	c0 := f.dial(0, "b0")
	ev0 := f.attach(c0, 8)
	epoch := f.heldIdle(c0, ev0, "a", p.ID)
	f.noReleaseIdle(c0, ev0)

	f.providerSession("b", p.ID, -1)
	f.input("b", "1", store.InputNormal)
	f.waitReason("b", circusv1.QueueReason_QUEUE_REASON_PROVIDER_FULL)
	r := f.releaseIdle(c0, ev0)
	if r.GetSessionId() != "a" || r.GetEpoch() != epoch {
		t.Fatalf("release request %v, want a at %d", r, epoch)
	}
	wantOk(t, call(t, c0, wc.Release("a", epoch, circusv1.ReleaseLease_REASON_IDLE)), false)
	offerTo(t, ev0, "b")
	acquire(t, c0, "b")
	f.waitReason("b", 0)
	if s := f.get("a"); s.State != store.SessionIdle || f.lease("a").WorkerID != "" {
		t.Fatalf("a after its release: %v, lease %+v", s.State, f.lease("a"))
	}
}

// TestReleaseIdleSkipsBusy: a holder whose session runs a turn, or has an
// input waiting, is not asked; the turn's end asks at once.
func TestReleaseIdleSkipsBusy(t *testing.T) {
	box := newBox(t, testKey)
	f := newFixture(t, withSecrets(box))
	p := f.provider(box, "prov", 1, 0)
	c0 := f.dial(0, "b0")
	ev0 := f.attach(c0, 8)
	f.providerSession("a", p.ID, -1)
	f.input("a", "1", store.InputNormal)
	offerTo(t, ev0, "a")
	epoch := acquire(t, c0, "a")
	ins := call(t, c0, wc.TakeInputs("a", epoch)).GetInputs().GetInputs()
	call(t, c0, wc.Started("a", epoch, 1, circusv1.TurnOrigin_TURN_ORIGIN_INPUT))

	f.providerSession("b", p.ID, -1)
	f.input("b", "1", store.InputNormal)
	f.waitReason("b", circusv1.QueueReason_QUEUE_REASON_PROVIDER_FULL)
	f.noReleaseIdle(c0, ev0)
	// The fallback looks again while b waits: still running.
	sessions.Fallback(f.svc, t.Context())
	f.noReleaseIdle(c0, ev0)

	// An input arrives for a before its turn ends: after ENDED it is
	// pending, so a is not idle.
	f.input("a", "2", store.InputNormal)
	call(t, c0, wc.Ended("a", epoch, 1, circusv1.TurnOutcome_TURN_OUTCOME_COMPLETED, &circusv1.ReportTurn_Persisted{InputIds: []string{ins[0].GetInputId()}}))
	f.noReleaseIdle(c0, ev0)
	// The holder runs it; the end of that turn asks at once.
	turn(t, c0, "a", epoch, 2)
	if r := f.releaseIdle(c0, ev0); r.GetSessionId() != "a" || r.GetEpoch() != epoch {
		t.Fatalf("release request %v", r)
	}
}

// TestReleaseIdleInputRace: an input that arrives after the request was
// sent is never lost: a holder that releases anyway leaves it pending, the
// session queues again, and its next holder takes it. A holder that keeps
// a session it is running a turn for is asked again only after the turn.
func TestReleaseIdleInputRace(t *testing.T) {
	box := newBox(t, testKey)
	f := newFixture(t, withSecrets(box))
	p := f.provider(box, "prov", 1, 0)
	c0 := f.dial(0, "b0")
	ev0 := f.attach(c0, 8)
	epoch := f.heldIdle(c0, ev0, "a", p.ID)
	f.providerSession("b", p.ID, -1)
	f.input("b", "1", store.InputNormal)
	f.releaseIdle(c0, ev0)

	// The input lands; the worker had already decided to release.
	in := f.input("a", "late", store.InputNormal)
	wantOk(t, call(t, c0, wc.Release("a", epoch, circusv1.ReleaseLease_REASON_IDLE)), false)
	if s := f.get("a"); s.State != store.SessionQueued {
		t.Fatalf("a after the release: %v", s.State)
	}
	// One of a and b gets the lease, the other waits for it; once the
	// holder's turn has ended it is asked back, until a runs its input.
	for {
		var held string
		for held == "" {
			if id := next(t, ev0).GetOffer().GetSessionId(); id == "a" || id == "b" {
				held = id
			}
		}
		e := acquire(t, c0, held)
		ins := call(t, c0, wc.TakeInputs(held, e)).GetInputs().GetInputs()
		if held == "a" {
			if len(ins) != 1 || ins[0].GetInputId() != in.InputID {
				t.Fatalf("a's inputs after the release: %v", ins)
			}
			return
		}
		// a may have been offered too while the provider had room; a
		// worker's acquisition is refused now, and a waits.
		if r := call(t, c0, wc.Acquire("a", true)); r.GetRejected().GetCode() != circusv1.SessionReply_Rejected_CODE_PROVIDER_FULL &&
			r.GetRejected().GetCode() != circusv1.SessionReply_Rejected_CODE_NOT_OFFERED {
			t.Fatalf("acquiring a while b holds the provider: %v", r)
		}
		f.waitReason("a", circusv1.QueueReason_QUEUE_REASON_PROVIDER_FULL)
		// b's turn: a request queued while it runs would be for a busy
		// session; none comes before ENDED.
		call(t, c0, wc.Started(held, e, 1, circusv1.TurnOrigin_TURN_ORIGIN_INPUT))
		f.noReleaseIdle(c0, ev0)
		var ids []string
		for _, x := range ins {
			ids = append(ids, x.GetInputId())
		}
		call(t, c0, wc.Ended(held, e, 1, circusv1.TurnOutcome_TURN_OUTCOME_COMPLETED, &circusv1.ReportTurn_Persisted{InputIds: ids}))
		if r := f.releaseIdle(c0, ev0); r.GetSessionId() != held || r.GetEpoch() != e {
			t.Fatalf("release request %v, want %s at %d", r, held, e)
		}
		wantOk(t, call(t, c0, wc.Release(held, e, circusv1.ReleaseLease_REASON_IDLE)), false)
	}
}

// TestReleaseIdleAsManyAsNeeded: with two leases idle and one session
// waiting, only one lease (least recent activity first) is asked back;
// another waiting session asks the second. A request the holder ignores
// counts until ReleaseIdleTimeout; then the lease not asked yet goes
// first.
func TestReleaseIdleAsManyAsNeeded(t *testing.T) {
	box := newBox(t, testKey)
	f := newFixture(t, withSecrets(box))
	sessions.DisableFallback(f.svc)
	p := f.provider(box, "prov", 2, 0)
	c0 := f.dial(0, "b0")
	ev0 := f.attach(c0, 8)
	e1 := f.heldIdle(c0, ev0, "a1", p.ID)
	f.clk.Advance(time.Second)
	e2 := f.heldIdle(c0, ev0, "a2", p.ID)

	f.providerSession("b1", p.ID, -1)
	f.input("b1", "1", store.InputNormal)
	if r := f.releaseIdle(c0, ev0); r.GetSessionId() != "a1" || r.GetEpoch() != e1 {
		t.Fatalf("first request %v", r)
	}
	f.noReleaseIdle(c0, ev0)
	// The holder ignores it (busy as far as it knows). While it is
	// outstanding, nothing more is asked for b1.
	f.svc.Wake("b1")
	f.noReleaseIdle(c0, ev0)
	f.clk.Advance(sessions.ReleaseIdleTimeout)
	f.svc.Wake("b1")
	if r := f.releaseIdle(c0, ev0); r.GetSessionId() != "a2" || r.GetEpoch() != e2 {
		t.Fatalf("request after the timeout %v", r)
	}
	// A second waiting session: a1 again (a2's request is outstanding).
	f.providerSession("b2", p.ID, -1)
	f.input("b2", "1", store.InputNormal)
	if r := f.releaseIdle(c0, ev0); r.GetSessionId() != "a1" {
		t.Fatalf("request for the second waiting session %v", r)
	}
	f.noReleaseIdle(c0, ev0)
	wantOk(t, call(t, c0, wc.Release("a1", e1, circusv1.ReleaseLease_REASON_IDLE)), false)
	wantOk(t, call(t, c0, wc.Release("a2", e2, circusv1.ReleaseLease_REASON_IDLE)), false)
	got := map[string]bool{}
	for len(got) < 2 {
		if id := next(t, ev0).GetOffer().GetSessionId(); id == "b1" || id == "b2" {
			got[id] = true
		}
	}
}

// TestReleaseIdleNotForOtherProviders: leases without a provider, or of
// another provider, are never asked back.
func TestReleaseIdleNotForOtherProviders(t *testing.T) {
	box := newBox(t, testKey)
	f := newFixture(t, withSecrets(box))
	p := f.provider(box, "prov", 1, 0)
	other := f.provider(box, "other", 0, 0)
	c0 := f.dial(0, "b0")
	ev0 := f.attach(c0, 8)
	f.heldIdle(c0, ev0, "o", other.ID)
	f.session("local", -1)
	f.input("local", "1", store.InputNormal)
	offerTo(t, ev0, "local")
	le := acquire(t, c0, "local")
	turn(t, c0, "local", le, 1)
	ea := f.heldIdle(c0, ev0, "a", p.ID)
	// a turns busy: b waits, and nothing else is asked.
	f.input("a", "2", store.InputNormal)
	call(t, c0, wc.TakeInputs("a", ea))
	call(t, c0, wc.Started("a", ea, 2, circusv1.TurnOrigin_TURN_ORIGIN_INPUT))
	f.providerSession("b", p.ID, -1)
	f.input("b", "1", store.InputNormal)
	f.waitReason("b", circusv1.QueueReason_QUEUE_REASON_PROVIDER_FULL)
	f.noReleaseIdle(c0, ev0)
	wait.For(t, func() bool { return f.reason("b") == circusv1.QueueReason_QUEUE_REASON_PROVIDER_FULL })
}

// TestReleaseIdleOnlyForRunnableWaiters: a waiter pinned to a worker with
// no connection could not take a freed lease, so nothing is asked back
// for it; once its worker attaches, the idle lease is asked back and the
// waiter runs there.
func TestReleaseIdleOnlyForRunnableWaiters(t *testing.T) {
	box := newBox(t, testKey)
	f := newFixture(t, withSecrets(box))
	p := f.provider(box, "prov", 1, 0, 1)
	c0 := f.dial(0, "b0")
	ev0 := f.attach(c0, 8)
	epoch := f.heldIdle(c0, ev0, "a", p.ID)
	f.providerSession("b", p.ID, 1)
	f.input("b", "1", store.InputNormal)
	f.waitReason("b", circusv1.QueueReason_QUEUE_REASON_PROVIDER_FULL)
	f.noReleaseIdle(c0, ev0)
	// Re-derived and looked at again by the fallback: still nothing.
	f.svc.Wake("b")
	sessions.Fallback(f.svc, t.Context())
	f.noReleaseIdle(c0, ev0)

	c1 := f.dial(1, "b1")
	ev1 := f.attach(c1, 8)
	if r := f.releaseIdle(c0, ev0); r.GetSessionId() != "a" || r.GetEpoch() != epoch {
		t.Fatalf("release request %v", r)
	}
	wantOk(t, call(t, c0, wc.Release("a", epoch, circusv1.ReleaseLease_REASON_IDLE)), false)
	offerTo(t, ev1, "b")
	acquire(t, c1, "b")
}

// TestReleaseIdleWaitsForPendingReasons: a lease the store finds idle is
// not asked back while a Nudge reason is pending for it (here the JOB a
// resumption after a control-plane restart raises, which TakeInputs does
// not clear); reading the jobs section clears it, and the request follows.
func TestReleaseIdleWaitsForPendingReasons(t *testing.T) {
	box := newBox(t, testKey)
	f := newFixture(t, withSecrets(box))
	p := f.provider(box, "prov", 1, 0)
	c0 := f.dial(0, "b0")
	ev0 := f.attach(c0, 8)
	epoch := f.heldIdle(c0, ev0, "a", p.ID)
	f.restart()
	<-c0.Done()
	c1 := f.redial("b0", 1)
	if !call2resume(t, c1, "a", epoch) {
		t.Fatal("resume refused")
	}
	ev1 := f.attach(c1, 8)
	call(t, c1, wc.TakeInputs("a", epoch))
	f.providerSession("b", p.ID, -1)
	f.input("b", "1", store.InputNormal)
	f.waitReason("b", circusv1.QueueReason_QUEUE_REASON_PROVIDER_FULL)
	f.noReleaseIdle(c1, ev1)
	call(t, c1, wc.State("a", epoch, circusv1.GetSessionState_SECTION_JOBS))
	sessions.Fallback(f.svc, t.Context())
	if r := f.releaseIdle(c1, ev1); r.GetSessionId() != "a" || r.GetEpoch() != epoch {
		t.Fatalf("release request %v", r)
	}
}

// TestReleaseIdleVoidedByTurn: a request the holder ignored (the session
// was busy on its side) stops counting when the session starts a turn,
// so the turn's end asks again at once.
func TestReleaseIdleVoidedByTurn(t *testing.T) {
	box := newBox(t, testKey)
	f := newFixture(t, withSecrets(box))
	sessions.DisableFallback(f.svc)
	p := f.provider(box, "prov", 1, 0)
	c0 := f.dial(0, "b0")
	ev0 := f.attach(c0, 8)
	epoch := f.heldIdle(c0, ev0, "a", p.ID)
	f.providerSession("b", p.ID, -1)
	f.input("b", "1", store.InputNormal)
	f.releaseIdle(c0, ev0)
	f.input("a", "2", store.InputNormal)
	// The worker answers the input's Nudge (sent once it is evaluated).
	nudge(t, ev0, "a", circusv1.WorkerEvent_Nudge_REASON_INPUT)
	turn(t, c0, "a", epoch, 2)
	if r := f.releaseIdle(c0, ev0); r.GetSessionId() != "a" || r.GetEpoch() != epoch {
		t.Fatalf("release request after the turn %v", r)
	}
}

// TestReleaseIdleRebindAndClose: a request queued on a connection stops
// counting when the lease is resumed on another connection (asked again
// there) and when its connection closes (another idle lease is asked).
func TestReleaseIdleRebindAndClose(t *testing.T) {
	box := newBox(t, testKey)
	f := newFixture(t, withSecrets(box))
	sessions.DisableFallback(f.svc)
	p := f.provider(box, "prov", 2, 0)
	c0 := f.dial(0, "b0")
	ev0 := f.attach(c0, 8)
	e1 := f.heldIdle(c0, ev0, "a1", p.ID)
	f.clk.Advance(time.Second)
	c1 := f.dial(0, "b0")
	ev1 := f.attach(c1, 8)
	e2 := f.heldIdle(c1, ev1, "a2", p.ID)
	f.providerSession("b", p.ID, -1)
	f.input("b", "1", store.InputNormal)
	if r := f.releaseIdle(c0, ev0); r.GetSessionId() != "a1" {
		t.Fatalf("first request %v", r)
	}
	// a1 resumed on c1 while c0 is still open: asked again on c1.
	if !call2resume(t, c1, "a1", e1) {
		t.Fatal("resume refused")
	}
	f.svc.Wake("b")
	if r := f.releaseIdle(c1, ev1); r.GetSessionId() != "a1" || r.GetEpoch() != e1 {
		t.Fatalf("request after the resume %v", r)
	}
	// c1 closes with that request unanswered: a2's lease, resumed on c0,
	// is asked instead.
	if !call2resume(t, c0, "a2", e2) {
		t.Fatal("resume refused")
	}
	must(t, c1.Close())
	wait.For(t, func() bool { return connections(f) == 1 })
	f.svc.Wake("b")
	if r := f.releaseIdle(c0, ev0); r.GetSessionId() != "a2" || r.GetEpoch() != e2 {
		t.Fatalf("request after the close %v", r)
	}
}

// TestReleaseIdleTriggers: with a session waiting, a held lease whose
// background task kept it busy is asked back when the task's end is
// patched, when an evaluation without reasons finds it idle (a ledger
// merge or discard applied, here the task row removed behind the
// service's back), and by the fallback even when no connection of the
// user has room for its rescan.
func TestReleaseIdleTriggers(t *testing.T) {
	for _, how := range []string{"task end", "evaluation", "fallback"} {
		t.Run(how, func(t *testing.T) {
			box := newBox(t, testKey)
			f := newFixture(t, withSecrets(box))
			p := f.provider(box, "prov", 1, 0)
			c0 := f.dial(0, "b0")
			// One session per connection: with it full, the fallback's
			// rescan of the user stops at once, so only the fallback's
			// own look at the waiting sessions asks the lease back.
			ev0 := f.attach(c0, 1)
			epoch := f.heldIdle(c0, ev0, "a", p.ID)
			call(t, c0, wc.Patch("a", epoch, &circusv1.PatchSessionState{BgtasksStarted: []*circusv1.PatchSessionState_BackgroundTask{{TaskId: "t1", Kind: "agent"}}}))
			f.providerSession("b", p.ID, -1)
			f.input("b", "1", store.InputNormal)
			f.waitReason("b", circusv1.QueueReason_QUEUE_REASON_PROVIDER_FULL)
			f.noReleaseIdle(c0, ev0)
			switch how {
			case "task end":
				call(t, c0, wc.Patch("a", epoch, &circusv1.PatchSessionState{BgtasksEnded: []string{"t1"}}))
			default:
				l := f.lease("a")
				must(t, f.st.PatchSessionRecords(t.Context(), store.Fence{SessionID: "a", Epoch: l.Epoch, WorkerID: l.WorkerID, BootID: l.BootID,
					ConnID: l.ConnID, Now: ms(f)}, store.RecordsPatch{BgTasksEnded: []string{"t1"}}))
				f.noReleaseIdle(c0, ev0)
				if how == "evaluation" {
					f.svc.Wake("a")
				} else {
					sessions.Fallback(f.svc, t.Context())
				}
			}
			if r := f.releaseIdle(c0, ev0); r.GetSessionId() != "a" || r.GetEpoch() != epoch {
				t.Fatalf("release request %v", r)
			}
		})
	}
}

// TestReleaseIdleWithHeldNote: a session whose only pending input is a
// held (deferred) note is idle: the note's Nudge is answered by a
// TakeInputs that takes nothing, the lease is asked back, and after the
// release the note is still pending and the session idle, not queued.
func TestReleaseIdleWithHeldNote(t *testing.T) {
	box := newBox(t, testKey)
	f := newFixture(t, withSecrets(box))
	p := f.provider(box, "prov", 1, 0)
	c0 := f.dial(0, "b0")
	ev0 := f.attach(c0, 8)
	epoch := f.heldIdle(c0, ev0, "a", p.ID)
	_, err := f.st.SubmitInput(t.Context(), store.Input{InputID: "a-note", SessionID: "a", ClientMsgID: "note-1", Kind: store.InputNote,
		Note: "ledger_merged", Text: "merged", Priority: store.InputNormal, Deferred: true, CreatedAt: ms(f)})
	must(t, err)
	f.svc.Wake("a", circusv1.WorkerEvent_Nudge_REASON_INPUT)
	nudge(t, ev0, "a", circusv1.WorkerEvent_Nudge_REASON_INPUT)
	if ins := call(t, c0, wc.TakeInputs("a", epoch)).GetInputs().GetInputs(); len(ins) != 0 {
		t.Fatalf("a held note was taken alone: %v", ins)
	}
	f.providerSession("b", p.ID, -1)
	f.input("b", "1", store.InputNormal)
	if r := f.releaseIdle(c0, ev0); r.GetSessionId() != "a" || r.GetEpoch() != epoch {
		t.Fatalf("release request %v", r)
	}
	wantOk(t, call(t, c0, wc.Release("a", epoch, circusv1.ReleaseLease_REASON_IDLE)), false)
	if s := f.get("a"); s.State != store.SessionIdle {
		t.Fatalf("a after its release: %v", s.State)
	}
	offerTo(t, ev0, "b")
	e2 := acquire(t, c0, "b")
	turn(t, c0, "b", e2, 1)
	// The note waits for a's next input, which takes it along.
	f.input("a", "next", store.InputNormal)
	f.releaseIdle(c0, ev0)
	wantOk(t, call(t, c0, wc.Release("b", e2, circusv1.ReleaseLease_REASON_IDLE)), false)
	offerTo(t, ev0, "a")
	e3 := acquire(t, c0, "a")
	var ids []string
	for _, in := range call(t, c0, wc.TakeInputs("a", e3)).GetInputs().GetInputs() {
		ids = append(ids, in.GetInputId())
	}
	if len(ids) != 2 || !slices.Contains(ids, "a-note") {
		t.Fatalf("inputs of a's next lease: %v", ids)
	}
}

// TestReleaseIdleReadsEveryPage: idle leases past the first page of the
// store read are asked too (here pages of one lease: the oldest, already
// asked, fills the first).
func TestReleaseIdleReadsEveryPage(t *testing.T) {
	box := newBox(t, testKey)
	f := newFixture(t, withSecrets(box))
	sessions.DisableFallback(f.svc)
	sessions.SetIdlePage(f.svc, 1)
	p := f.provider(box, "prov", 2, 0)
	c0 := f.dial(0, "b0")
	ev0 := f.attach(c0, 8)
	f.heldIdle(c0, ev0, "a1", p.ID)
	f.clk.Advance(time.Second)
	e2 := f.heldIdle(c0, ev0, "a2", p.ID)
	f.providerSession("b1", p.ID, -1)
	f.input("b1", "1", store.InputNormal)
	if r := f.releaseIdle(c0, ev0); r.GetSessionId() != "a1" {
		t.Fatalf("first request %v", r)
	}
	f.providerSession("b2", p.ID, -1)
	f.input("b2", "1", store.InputNormal)
	if r := f.releaseIdle(c0, ev0); r.GetSessionId() != "a2" || r.GetEpoch() != e2 {
		t.Fatalf("request from the second page %v", r)
	}
}

// TestReleaseIdleAfterSpuriousReason: an idle lease with a spurious INPUT
// reason (a stale read raised it again after the holder's TakeInputs) is
// not asked back while the reason stands; the holder's next TakeInputs,
// which finds nothing, clears it and asks at once, without waiting for the
// fallback.
func TestReleaseIdleAfterSpuriousReason(t *testing.T) {
	box := newBox(t, testKey)
	f := newFixture(t, withSecrets(box))
	sessions.DisableFallback(f.svc)
	p := f.provider(box, "prov", 1, 0)
	c0 := f.dial(0, "b0")
	ev0 := f.attach(c0, 8)
	epoch := f.heldIdle(c0, ev0, "a", p.ID)
	// The spurious reason: a Wake without an input.
	f.svc.Wake("a", circusv1.WorkerEvent_Nudge_REASON_INPUT)
	nudge(t, ev0, "a", circusv1.WorkerEvent_Nudge_REASON_INPUT)
	f.providerSession("b", p.ID, -1)
	f.input("b", "1", store.InputNormal)
	f.waitReason("b", circusv1.QueueReason_QUEUE_REASON_PROVIDER_FULL)
	f.settle()
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	for {
		e, err := ev0.Next(ctx)
		if err != nil {
			break
		}
		if e.GetReleaseIdle() != nil {
			t.Fatalf("asked back with a reason pending: %v", e)
		}
	}
	cancel()
	if ins := call(t, c0, wc.TakeInputs("a", epoch)).GetInputs().GetInputs(); len(ins) != 0 {
		t.Fatalf("inputs %v", ins)
	}
	if r := f.releaseIdle(c0, ev0); r.GetSessionId() != "a" || r.GetEpoch() != epoch {
		t.Fatalf("release request %v", r)
	}
}
