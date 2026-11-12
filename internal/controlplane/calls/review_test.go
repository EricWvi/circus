package calls_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/controlplane/bus"
	"github.com/EricWvi/circus/internal/controlplane/calls"
	"github.com/EricWvi/circus/internal/controlplane/gateway"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/wire"
)

// Tests for review m2-t5-review-1: reproductions R1-R7 now assert the fixed
// behaviour, plus the review's test gaps.

func tryReceive(t testing.TB, ctx context.Context, l *wire.Link, wait time.Duration) (*circusv1.DaemonDownlink, bool) {
	t.Helper()
	short, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	m, err := l.Receive(short)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, false
		}
		t.Fatal(err)
	}
	return m.(*circusv1.DaemonDownlink), true
}
func ackRunning(id string) *circusv1.DaemonUplink {
	return &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_CallAck{CallAck: &circusv1.CallAck{CallId: id, Outcome: &circusv1.CallAck_Status{Status: &circusv1.CallStatus{CallId: id, State: circusv1.CallStatus_STATE_RUNNING}}}}}
}
func statusCompleted(id string) *circusv1.DaemonUplink {
	return &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_CallStatus{CallStatus: &circusv1.CallStatus{CallId: id, State: circusv1.CallStatus_STATE_COMPLETED, Result: &circusv1.DeviceResult{CallId: id, Output: &circusv1.ToolOutput{}}}}}
}
func shuttingDown(id string) *circusv1.DaemonUplink {
	return &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_CallAck{CallAck: &circusv1.CallAck{CallId: id, Outcome: &circusv1.CallAck_Rejection{Rejection: &circusv1.Rejection{Code: circusv1.Rejection_CODE_SHUTTING_DOWN}}}}}
}

// R1 / M1: a Query on a registered but unsynchronized route is deferred to
// the sync; it never overtakes the sync's resend, and the resend's ack
// answers it.
func TestQueryDeferredUntilSyncResend(t *testing.T) {
	o := &observer{seen: make(chan string, 8)}
	f := setup(t, func(_ *calls.Config, g *gateway.Config) { g.Hooks = o })
	l := f.connect("boot")
	s, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
	must(t, err)
	defer s.Close()
	receive(t, f.ctx, l)
	drain(t, f.ctx, s, 2)
	f.disconnect(l)
	entered, gate := o.arm()
	l = f.open("boot")
	<-entered
	queried := make(chan *circusv1.CallUpdate, 1)
	go func() { u, _ := f.router.Query(f.ctx, "u", "call"); queried <- u }()
	eventually(t, func() bool { return f.router.Stats().QueryWaiters == 1 })
	if m, ok := tryReceive(t, f.ctx, l, 50*time.Millisecond); ok {
		t.Fatalf("command on an unready route: %v", m)
	}
	close(gate)
	if m := receive(t, f.ctx, l); m.GetDispatchCall() == nil {
		t.Fatalf("expected the resend first, got %v", m)
	}
	f.finishSync(l)
	send(t, f.ctx, l, ackRunning("call"))
	if u := <-queried; u.GetDispatched() == nil {
		t.Fatal(u)
	}
	f.result(l, "call")
	f.waitState("call", store.CallCompleted)
}

// M1: the watchdog and Cancel send nothing on an unready route; the sync
// replays the cancel after the resend.
func TestWatchdogAndCancelWaitForSync(t *testing.T) {
	o := &observer{seen: make(chan string, 8)}
	f := setup(t, func(_ *calls.Config, g *gateway.Config) { g.Hooks = o })
	l := f.connect("boot")
	s, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
	must(t, err)
	defer s.Close()
	receive(t, f.ctx, l)
	f.disconnect(l)
	entered, gate := o.arm()
	l = f.open("boot")
	<-entered
	must(t, f.router.Cancel(f.ctx, "u", "call"))
	rows, err := f.db.ListTimers(f.ctx, store.TimerFilter{Kind: "call_watchdog", SubjectID: "call", Limit: 1})
	must(t, err)
	f.advance(time.Duration(rows[0].DueAt-f.clock.Now().UnixMilli()) * time.Millisecond)
	eventually(t, func() bool {
		ts, _ := f.db.ListTimers(f.ctx, store.TimerFilter{Kind: "call_watchdog", SubjectID: "call", Limit: 1})
		return len(ts) == 1 && ts[0].DueAt > f.clock.Now().UnixMilli()
	})
	if m, ok := tryReceive(t, f.ctx, l, 50*time.Millisecond); ok {
		t.Fatalf("command on an unready route: %v", m)
	}
	close(gate)
	if m := receive(t, f.ctx, l); m.GetDispatchCall() == nil {
		t.Fatal(m)
	}
	if m := receive(t, f.ctx, l); m.GetCancelCall().GetCallId() != "call" {
		t.Fatal(m)
	}
	f.finishSync(l)
	f.result(l, "call")
	f.waitState("call", store.CallCompleted)
}

// R2 / M2: a query in flight on a superseded connection is failed by that
// connection's disconnect; later queries reach the new route.
func TestSupersededQueryIsAnswered(t *testing.T) {
	f := setup(t)
	l := f.connect("boot")
	s, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
	must(t, err)
	defer s.Close()
	receive(t, f.ctx, l)
	f.result(l, "call")
	nextResult(t, f.ctx, s)
	f.waitState("call", store.CallCompleted)
	first := make(chan error, 1)
	go func() { _, err := f.router.Query(f.ctx, "u", "call"); first <- err }()
	if m := receive(t, f.ctx, l); m.GetCallQuery() == nil {
		t.Fatal(m)
	}
	l2 := f.open("boot")
	if err := <-first; !errors.Is(err, calls.ErrDeviceOffline) {
		t.Fatalf("superseded query: %v", err)
	}
	f.finishSync(l2)
	second := make(chan *circusv1.CallUpdate, 1)
	go func() { u, _ := f.router.Query(f.ctx, "u", "call"); second <- u }()
	if m := receive(t, f.ctx, l2); m.GetCallQuery().GetCallId() != "call" {
		t.Fatal(m)
	}
	send(t, f.ctx, l2, statusCompleted("call"))
	if u := <-second; u.GetResult() == nil {
		t.Fatal(u)
	}
	eventually(t, func() bool { return f.router.Stats().QueryWaiters == 0 })
}

// delayResult holds OnDeviceResult until released, standing in for a reader
// that is behind when the restarted daemon's new connection registers.
type delayResult struct {
	gateway.Hooks
	hold    chan struct{}
	entered chan struct{}
	done    chan struct{}
}

func (d *delayResult) SetHooks(h gateway.Hooks) { d.Hooks = h }
func (d *delayResult) OnDeviceResult(ctx context.Context, c gateway.Connection, v *circusv1.DeviceResult) error {
	close(d.entered)
	<-d.hold
	defer close(d.done)
	return d.Hooks.OnDeviceResult(ctx, c, v)
}

// R6 / M2 and D43 item 12: the old boot's final result, received before the
// restarted daemon's new boot registers, settles the call; the new boot's
// sync waits for the superseded link to drain before losing old-boot calls.
func TestOldBootFinalResultSurvivesRestart(t *testing.T) {
	d := &delayResult{hold: make(chan struct{}), entered: make(chan struct{}), done: make(chan struct{})}
	f := setup(t, func(_ *calls.Config, g *gateway.Config) { g.Hooks = d })
	release := sync.OnceFunc(func() { close(d.hold) })
	t.Cleanup(release) // runs before the fixture's teardown, also on failure
	l := f.connect("old")
	s, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
	must(t, err)
	defer s.Close()
	receive(t, f.ctx, l)
	drain(t, f.ctx, s, 2)
	f.result(l, "call")
	<-d.entered
	must(t, l.Close())
	l2 := f.open("new")
	if m, ok := tryReceive(t, f.ctx, l2, 50*time.Millisecond); ok {
		t.Fatalf("new boot synchronized before the old link drained: %v", m)
	}
	release()
	<-d.done
	f.finishSync(l2)
	f.waitState("call", store.CallCompleted)
	nextResult(t, f.ctx, s)
}

// R7 and test gap M33: a result sent just before a graceful daemon close is
// processed before the disconnect (D43 item 12).
func TestGracefulCloseDrainsFinalResult(t *testing.T) {
	f := setup(t)
	l := f.connect("boot")
	s, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
	must(t, err)
	defer s.Close()
	receive(t, f.ctx, l)
	for i := range 20 {
		send(t, f.ctx, l, jobEvent("012345abcdef", uint64(i+1)))
	}
	f.result(l, "call")
	must(t, l.Shutdown(f.ctx))
	nextResult(t, f.ctx, s)
	f.waitState("call", store.CallCompleted)
}

// R3 / H1: SHUTTING_DOWN answering a resend may follow an execution, so the
// call stays dispatched and a later result still completes it. A refusal of
// the only dispatch (TestAckRejections/shutdown) is a terminal rejection.
func TestShuttingDownOnResendKeepsDispatched(t *testing.T) {
	o := &observer{seen: make(chan string, 8)}
	f := setup(t, func(_ *calls.Config, g *gateway.Config) { g.Hooks = o })
	l := f.connect("boot")
	s, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
	must(t, err)
	defer s.Close()
	receive(t, f.ctx, l)
	drain(t, f.ctx, s, 2)
	f.disconnect(l)
	l = f.open("boot")
	if m := receive(t, f.ctx, l); m.GetDispatchCall() == nil {
		t.Fatal(m)
	}
	f.finishSync(l)
	send(t, f.ctx, l, shuttingDown("call"))
	o.wait(t, f.ctx, "ack")
	if c, _ := f.db.GetCall(f.ctx, "call"); c.State != store.CallDispatched {
		t.Fatalf("resend refusal settled the call: %s/%s", c.State, c.OutcomeReason)
	}
	quiet(t, f.ctx, s)
	f.result(l, "call")
	nextResult(t, f.ctx, s)
	f.waitState("call", store.CallCompleted)
}

// H1: after a control-plane restart the router cannot prove a first send,
// so SHUTTING_DOWN for an old dispatch keeps the call recoverable.
func TestShuttingDownAfterRestartKeepsDispatched(t *testing.T) {
	o := &observer{seen: make(chan string, 8)}
	f := setup(t, func(_ *calls.Config, g *gateway.Config) { g.Hooks = o })
	l := f.connect("boot")
	s, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
	must(t, err)
	receive(t, f.ctx, l)
	s.Close()
	f.restart()
	l = f.open("boot")
	if m := receive(t, f.ctx, l); m.GetCallQuery() == nil {
		t.Fatal(m)
	}
	f.finishSync(l)
	before := f.hooks.acks.Load()
	send(t, f.ctx, l, shuttingDown("call"))
	eventually(t, func() bool { return f.hooks.acks.Load() > before })
	if f.state("call") != store.CallDispatched {
		t.Fatalf("refusal after restart settled the call: %s", f.state("call"))
	}
	send(t, f.ctx, l, statusCompleted("call"))
	f.waitState("call", store.CallCompleted)
}

// (R4 / M3) crashRestart restarts the control plane without recording the
// disconnect, as a crash (or a router closed before the gateway) leaves it.
func (f *fixture) crashRestart() {
	f.t.Helper()
	f.haltTimers()
	cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	must(f.t, f.router.Close(cleanup))
	f.restart()
}

// R4 / M3: after a restart the device row still says connected. It counts as
// offline since the restart: a watchdog before the cutoff does not lose the
// call (it goes unknown at grace expiry), a submission inside the grace
// period stays accepted, and a same-boot reconnect recovers both.
func TestRestartTreatsConnectedRowAsOfflineSinceStart(t *testing.T) {
	f := setup(t)
	l := f.connect("boot")
	f.disconnect(l)
	l = f.connect("boot")
	f.advance(2 * time.Hour) // connected for a long time
	s, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
	must(t, err)
	receive(t, f.ctx, l)
	f.running(l, "call")
	s.Close()
	f.crashRestart()
	waiting, err := f.router.Submit(f.ctx, "u", "desk", call("waiting"))
	must(t, err)
	defer waiting.Close()
	if u, err := waiting.Next(f.ctx); err != nil || u.GetAccepted() == nil {
		t.Fatalf("submission after restart: %v %v", u, err)
	}
	f.advance(5 * time.Minute) // grace expired; first watchdog due
	f.waitState("call", store.CallUnknown)
	f.waitState("waiting", store.CallOffline)
	l = f.open("boot")
	if m := receive(t, f.ctx, l); m.GetCallQuery().GetCallId() != "call" {
		t.Fatal(m)
	}
	f.finishSync(l)
	send(t, f.ctx, l, statusCompleted("call"))
	f.waitState("call", store.CallCompleted)
}

// M3: the result-retention cutoff after a restart counts from the restart.
func TestRestartCutoffCountsFromStart(t *testing.T) {
	f := setup(t)
	l := f.connect("boot")
	s, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
	must(t, err)
	receive(t, f.ctx, l)
	f.running(l, "call")
	s.Close()
	f.advance(2 * time.Hour)
	f.crashRestart()
	for range 5 {
		f.advance(5 * time.Minute)
	}
	time.Sleep(20 * time.Millisecond)
	if st := f.state("call"); st == store.CallLost {
		t.Fatal("lost before the cutoff")
	}
	for range 3 {
		f.advance(5 * time.Minute)
	}
	f.waitState("call", store.CallLost)
	if c, _ := f.db.GetCall(f.ctx, "call"); c.OutcomeReason != "RESULT_RETENTION_EXPIRED" {
		t.Fatal(c)
	}
}

// R5 / L1: a failed disconnect write keeps a recovery path: the sweep persists
// it, so the accepted call later goes offline and releases its content.
func TestDisconnectWriteFailureIsRetried(t *testing.T) {
	o := &observer{seen: make(chan string, 8)}
	entered, _ := o.arm()
	f := setup(t, func(_ *calls.Config, g *gateway.Config) { g.Hooks = o })
	l := f.open("boot")
	<-entered
	s, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
	must(t, err)
	defer s.Close()
	drain(t, f.ctx, s, 1)
	f.fault.FailAllWrites(errors.New("disk full"))
	must(t, l.Close())
	eventually(t, func() bool { return f.router.Stats().UnrecordedDisconnects == 1 })
	f.fault.FailAllWrites(nil)
	eventually(t, func() bool { f.advance(time.Second); return f.router.Stats().UnrecordedDisconnects == 0 })
	f.advance(31 * time.Second)
	f.waitState("call", store.CallOffline)
	if st := f.router.Stats(); st.PendingCalls != 0 {
		t.Fatalf("offline call retains content: %+v", st)
	}
}

// L3: a first send that wire proves too large was never queued: rejected.
type tooLargeBus struct{ bus.Bus }

func (b tooLargeBus) Send(ctx context.Context, target bus.Target, msg *circusv1.DaemonDownlink) error {
	if msg.GetDispatchCall() != nil {
		return wire.ErrTooLarge
	}
	return b.Bus.Send(ctx, target, msg)
}
func TestFirstSendTooLargeIsRejected(t *testing.T) {
	f := setup(t, func(c *calls.Config, _ *gateway.Config) { c.Bus = tooLargeBus{c.Bus} })
	f.connect("boot")
	s, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
	must(t, err)
	defer s.Close()
	drain(t, f.ctx, s, 2)
	if u, err := s.Next(f.ctx); err != nil || u.GetRejected().GetCode() != circusv1.CallUpdate_Rejected_CODE_INVALID {
		t.Fatal(u, err)
	}
	if f.state("call") != store.CallRejected || f.router.Stats().PendingCalls != 0 {
		t.Fatal("too-large dispatch not settled")
	}
}

// Test gap M2: crash after every write point of the reconnect sync that
// dispatches an accepted call. Dispatch evidence precedes every DispatchCall;
// a restart never re-dispatches the old attempt.
func TestCrashAtEverySyncWritePoint(t *testing.T) {
	prepare := func() (*fixture, int) {
		f := setup(t)
		l := f.connect("boot")
		f.disconnect(l)
		s, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
		must(t, err)
		t.Cleanup(func() { _ = s.Close() })
		f.haltTimers()
		return f, f.fault.Writes()
	}
	writes := func() int {
		f, start := prepare()
		l := f.open("boot")
		f.finishSync(l)
		if m := receive(t, f.ctx, l); m.GetDispatchCall() == nil {
			t.Fatal(m)
		}
		return f.fault.Writes() - start
	}()
	if writes < 3 {
		t.Fatalf("unexpectedly few write points: %d", writes)
	}
	for n := 1; n <= writes; n++ {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			f, _ := prepare()
			f.fault.CrashAfterWrite(n, nil)
			l := f.open("boot")
			executed := false
			for {
				short, cancel := context.WithTimeout(f.ctx, 100*time.Millisecond)
				v, err := l.Receive(short)
				cancel()
				if err != nil {
					break // quiet, or closed because the sync hit the crash
				}
				m := v.(*circusv1.DaemonDownlink)
				if m.GetListJobs() != nil {
					send(t, f.ctx, l, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_JobSnapshot{JobSnapshot: &circusv1.JobSnapshot{}}})
				}
				if m.GetDispatchCall() != nil {
					executed = true
					if row, err := f.db.GetCall(f.ctx, "call"); err != nil || row.DispatchedBootID != "boot" {
						t.Fatalf("dispatched without durable evidence: %+v %v", row, err)
					}
				}
			}
			eventually(t, func() bool { return f.fault.Crashed() })
			f.restart()
			l = f.open("boot")
			for {
				m := receive(t, f.ctx, l)
				if m.GetListJobs() != nil {
					break
				}
				if m.GetDispatchCall() != nil {
					t.Fatal("restart re-dispatched an old attempt")
				}
				status := &circusv1.CallStatus{CallId: m.GetCallQuery().GetCallId(), State: circusv1.CallStatus_STATE_LOST}
				if executed {
					status = statusCompleted(status.CallId).GetCallStatus()
				}
				send(t, f.ctx, l, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_CallStatus{CallStatus: status}})
			}
			f.snapshot(l)
			eventually(t, func() bool { c, _ := f.db.GetCall(f.ctx, "call"); return c.State.Terminal() })
			row, err := f.db.GetCall(f.ctx, "call")
			must(t, err)
			if (row.State == store.CallCompleted) != executed || (row.State != store.CallCompleted && row.State != store.CallLost) {
				t.Fatalf("outcome %s/%s, executed %v", row.State, row.OutcomeReason, executed)
			}
		})
	}
}

// Test gap M8: a new boot loses more than one LoseCalls batch (64) of
// old-boot calls, notifies each and cancels every watchdog (checked by the
// fixture's orphan-timer invariant).
func TestNewBootLosesManyBatches(t *testing.T) {
	const n = 2*64 + 5
	f := setup(t)
	l := f.connect("old")
	subs := make([]*calls.Subscription, 0, n)
	for i := range n {
		s, err := f.router.Submit(f.ctx, "u", "desk", call(fmt.Sprintf("call-%03d", i)))
		must(t, err)
		subs = append(subs, s)
		receive(t, f.ctx, l)
	}
	f.disconnect(l)
	f.connect("new")
	for i, s := range subs {
		for {
			u, err := s.Next(f.ctx)
			must(t, err)
			if u.GetLost() != nil {
				break
			}
		}
		if st := f.state(fmt.Sprintf("call-%03d", i)); st != store.CallLost {
			t.Fatalf("call %d: %s", i, st)
		}
		s.Close()
	}
}

// Test gap M29: a grace expiry larger than one page (64) drains in
// successive claims.
func TestGraceExpiryDrainsManyBatches(t *testing.T) {
	const n = 2*64 + 5
	f := setup(t)
	l := f.connect("boot")
	f.disconnect(l)
	for i := range n {
		s, err := f.router.Submit(f.ctx, "u", "desk", call(fmt.Sprintf("call-%03d", i)))
		must(t, err)
		s.Close()
	}
	f.advance(31 * time.Second)
	for i := range n {
		f.waitState(fmt.Sprintf("call-%03d", i), store.CallOffline)
	}
	if st := f.router.Stats(); st.PendingCalls != 0 {
		t.Fatalf("offline calls retain content: %+v", st)
	}
}

// Test gap M18: before the 30-minute result-retention cutoff, watchdogs only
// re-arm; a same-boot reconnect then recovers the result.
func TestRetentionCutoffNotYetReached(t *testing.T) {
	f := setup(t)
	l := f.connect("boot")
	s, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
	must(t, err)
	defer s.Close()
	receive(t, f.ctx, l)
	f.running(l, "call")
	f.disconnect(l)
	f.advance(31 * time.Second)
	f.waitState("call", store.CallUnknown)
	for range 5 {
		f.advance(5 * time.Minute) // about 25.5 minutes offline
	}
	f.advance(3 * time.Minute)
	time.Sleep(20 * time.Millisecond)
	if f.state("call") != store.CallUnknown {
		t.Fatalf("settled before the cutoff: %s", f.state("call"))
	}
	l = f.open("boot")
	if m := receive(t, f.ctx, l); m.GetCallQuery() == nil {
		t.Fatal(m)
	}
	f.finishSync(l)
	send(t, f.ctx, l, statusCompleted("call"))
	f.waitState("call", store.CallCompleted)
}

// Test gap M26: concurrent queries coalesce onto exactly one CallQuery.
func TestQueryCoalescingSendsOneCallQuery(t *testing.T) {
	f := setup(t)
	l := f.connect("boot")
	s, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
	must(t, err)
	defer s.Close()
	receive(t, f.ctx, l)
	f.running(l, "call")
	answers := make(chan *circusv1.CallUpdate, 3)
	for range 3 {
		go func() { u, _ := f.router.Query(f.ctx, "u", "call"); answers <- u }()
	}
	eventually(t, func() bool { return f.router.Stats().QueryWaiters == 3 })
	if m := receive(t, f.ctx, l); m.GetCallQuery() == nil {
		t.Fatal(m)
	}
	if m, ok := tryReceive(t, f.ctx, l, 50*time.Millisecond); ok {
		t.Fatalf("second command for coalesced queries: %v", m)
	}
	send(t, f.ctx, l, statusCompleted("call"))
	for range 3 {
		if u := <-answers; u.GetResult() == nil {
			t.Fatal(u)
		}
	}
}

// Test gap M22: a non-protocol hook error (store failure) does not stop the
// gateway reading that link; the next reply settles the call without a
// reconnect.
func TestGatewayKeepsReadingAfterHookError(t *testing.T) {
	f := setup(t)
	l := f.connect("boot")
	route, _, err := f.registry.Lookup(f.ctx, "d")
	must(t, err)
	s, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
	must(t, err)
	defer s.Close()
	receive(t, f.ctx, l)
	f.fault.FailAllWrites(store.ErrBusy)
	f.result(l, "call")
	nextResult(t, f.ctx, s)
	f.fault.FailAllWrites(nil)
	send(t, f.ctx, l, statusCompleted("call"))
	f.waitState("call", store.CallCompleted)
	if now, ok, _ := f.registry.Lookup(f.ctx, "d"); !ok || now.Target != route.Target {
		t.Fatal("hook error closed the link")
	}
}

// Test gap M20: events at or below the snapshot watermark are dropped, even
// for a job the store has never seen.
func TestStaleEventBelowWatermarkDropped(t *testing.T) {
	f := setup(t)
	l := f.connect("boot")
	send(t, f.ctx, l, jobEvent("00000000000a", 7)) // gap: requests a snapshot
	if m := receive(t, f.ctx, l); m.GetListJobs() == nil {
		t.Fatal(m)
	}
	before := f.hooks.snapshots.Load()
	kept := &circusv1.JobEvent{JobId: "00000000000a", SessionId: "session", CallId: "call", Seq: 7, State: circusv1.JobEvent_STATE_RUNNING}
	send(t, f.ctx, l, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_JobSnapshot{JobSnapshot: &circusv1.JobSnapshot{Seq: 9, Jobs: []*circusv1.JobEvent{kept}}}})
	eventually(t, func() bool { return f.hooks.snapshots.Load() > before })
	send(t, f.ctx, l, jobEvent("00000000000b", 8))
	send(t, f.ctx, l, jobEvent("00000000000c", 10))
	eventually(t, func() bool { return f.hooks.events.Load() == 3 })
	jobs := f.jobs()
	if _, ok := jobs["00000000000b"]; ok {
		t.Fatal("event below the snapshot watermark created a job")
	}
	if jobs["00000000000c"].State != store.JobRunning {
		t.Fatal("event above the watermark was dropped")
	}
}

// D43 item 14: a truncated snapshot never settles omissions.
func TestTruncatedSnapshotSettlesNothing(t *testing.T) {
	f := setup(t)
	l := f.connect("boot")
	send(t, f.ctx, l, jobEvent("00000000000a", 1))
	send(t, f.ctx, l, jobEvent("00000000000b", 3)) // gap: requests a snapshot
	if m := receive(t, f.ctx, l); m.GetListJobs() == nil {
		t.Fatal(m)
	}
	before := f.hooks.snapshots.Load()
	send(t, f.ctx, l, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_JobSnapshot{JobSnapshot: &circusv1.JobSnapshot{Seq: 5, Truncated: true}}})
	eventually(t, func() bool { return f.hooks.snapshots.Load() > before })
	for id, j := range f.jobs() {
		if j.State != store.JobRunning {
			t.Fatalf("truncated snapshot settled job %s: %s", id, j.State)
		}
	}
}

// gatedDisconnect pauses after the router's OnDisconnected returns, before
// the gateway's own Unregister.
type gatedDisconnect struct {
	gateway.Hooks
	armed   atomic.Bool
	entered chan struct{}
	gate    chan struct{}
}

func (g *gatedDisconnect) SetHooks(h gateway.Hooks) { g.Hooks = h }
func (g *gatedDisconnect) OnDisconnected(ctx context.Context, c gateway.Connection) error {
	err := g.Hooks.OnDisconnected(ctx, c)
	if g.armed.CompareAndSwap(true, false) {
		close(g.entered)
		<-g.gate
	}
	return err
}

// Test gap M25: the router withdraws the route under its lock when it records
// the disconnect, so a submission in between cannot dispatch to the closed
// link; it waits accepted for the grace period.
func TestDisconnectWithdrawsRouteUnderLock(t *testing.T) {
	g := &gatedDisconnect{entered: make(chan struct{}), gate: make(chan struct{})}
	f := setup(t, func(_ *calls.Config, c *gateway.Config) { c.Hooks = g })
	l := f.connect("boot")
	g.armed.Store(true)
	must(t, l.Close())
	<-g.entered
	if _, ok, _ := f.registry.Lookup(f.ctx, "d"); ok {
		t.Fatal("route still registered after the recorded disconnect")
	}
	s, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
	must(t, err)
	defer s.Close()
	if u, err := s.Next(f.ctx); err != nil || u.GetAccepted() == nil {
		t.Fatal(u, err)
	}
	if f.state("call") != store.CallAccepted {
		t.Fatal(f.state("call"))
	}
	close(g.gate)
}

// Bounded resync (internal/daemon/link): at most the sync limit of requests
// is unanswered at once, so a large batch does not overflow the daemon's reply
// queue; the link survives the whole sync. Queries (running calls) are freed
// by CallStatus, resends (unacked calls) by CallAck. The limit is
// min(MaxSyncRequests, MaxSyncReplyBytes/MaxMessageBytes).
func TestResyncRequestsAreBounded(t *testing.T) {
	for _, tc := range []struct {
		name       string
		resend     bool
		requests   int
		replyBytes int64
		limit      int
	}{
		{"queries", false, 4, 1 << 30, 4},
		{"resends", true, 4, 1 << 30, 4},
		{"reply-bytes", false, 16, 0, 2}, // default budget: two worst-case replies
	} {
		t.Run(tc.name, func(t *testing.T) {
			checkBoundedResync(t, tc.resend, tc.requests, tc.replyBytes, tc.limit)
		})
	}
}

func checkBoundedResync(t *testing.T, resend bool, requests int, replyBytes int64, limit int) {
	const n = 24
	f := setup(t, func(c *calls.Config, _ *gateway.Config) {
		c.MaxSyncRequests, c.MaxSyncReplyBytes = requests, replyBytes
	})
	l := f.connect("boot")
	for i := range n {
		id := fmt.Sprintf("call-%02d", i)
		s, err := f.router.Submit(f.ctx, "u", "desk", call(id))
		must(t, err)
		s.Close()
		receive(t, f.ctx, l)
		if !resend {
			f.running(l, id)
		}
	}
	f.disconnect(l)
	l = f.open("boot")
	// The gateway registers after Welcome; wait for this generation's route.
	var route gateway.Connection
	eventually(t, func() bool {
		v, ok, _ := f.registry.Lookup(f.ctx, "d")
		route = v
		return ok
	})
	answered := 0
	for answered < n {
		var batch []string
		for len(batch) < limit {
			m, ok := tryReceive(t, f.ctx, l, 200*time.Millisecond)
			if !ok {
				break
			}
			switch {
			case !resend && m.GetCallQuery() != nil:
				batch = append(batch, m.GetCallQuery().CallId)
			case resend && m.GetDispatchCall() != nil:
				batch = append(batch, m.GetDispatchCall().GetCall().GetCallId())
			default:
				t.Fatalf("unexpected %v after %d replies", m, answered)
			}
		}
		if len(batch) == 0 || (len(batch) < limit && answered+len(batch) < n) {
			t.Fatalf("sync stalled after %d requests", answered+len(batch))
		}
		if m, ok := tryReceive(t, f.ctx, l, 20*time.Millisecond); ok {
			t.Fatalf("more than %d unanswered sync requests: %v", limit, m)
		}
		if st := f.router.Stats(); st.SyncRequests > limit {
			t.Fatalf("sync requests %d", st.SyncRequests)
		}
		for _, id := range batch {
			if resend {
				send(t, f.ctx, l, ackRunning(id))
			} else {
				send(t, f.ctx, l, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_CallStatus{CallStatus: &circusv1.CallStatus{CallId: id, State: circusv1.CallStatus_STATE_RUNNING}}})
			}
		}
		answered += len(batch)
	}
	f.finishSync(l)
	if now, ok, _ := f.registry.Lookup(f.ctx, "d"); !ok || now.Target != route.Target {
		t.Fatal("resync replaced the link")
	}
	eventually(t, func() bool { return f.router.Stats().SyncRequests == 0 })
}
