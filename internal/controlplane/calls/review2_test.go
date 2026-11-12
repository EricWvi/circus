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
	"github.com/EricWvi/circus/internal/controlplane/calls"
	"github.com/EricWvi/circus/internal/controlplane/gateway"
	"github.com/EricWvi/circus/internal/store"
)

// Tests for review m2-t5-review-2: its reproductions now assert the fixed
// behaviour, plus tests for the surviving non-equivalent mutants.

func statusRunning(id string) *circusv1.DaemonUplink {
	return &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_CallStatus{CallStatus: &circusv1.CallStatus{CallId: id, State: circusv1.CallStatus_STATE_RUNNING}}}
}

// waitAnswer fails the test when a query does not return within a short
// real-time bound (a stranded waiter would wait for the fixture's ctx).
func waitAnswer[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(2 * time.Second):
		t.Fatal("query not answered")
		var zero T
		return zero
	}
}

// completedCall drives "call" to completed on boot and returns its link.
func completedCall(t *testing.T, f *fixture, boot string) {
	t.Helper()
	l := f.connect(boot)
	s, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
	must(t, err)
	defer s.Close()
	receive(t, f.ctx, l)
	f.result(l, "call")
	nextResult(t, f.ctx, s)
	f.waitState("call", store.CallCompleted)
	f.disconnect(l)
}

// Review 2 M1: a sync waiting for request capacity releases its operation
// slot, so the replies it waits for can enter even with MaxOperations == 1.
func TestSyncCapacityWaitReleasesSlot(t *testing.T) {
	f := setup(t, func(c *calls.Config, _ *gateway.Config) { c.MaxOperations, c.MaxSyncRequests = 1, 1 })
	l := f.connect("boot")
	for i := range 3 {
		id := fmt.Sprintf("c%d", i)
		var s *calls.Subscription
		var err error
		eventually(t, func() bool {
			s, err = f.router.Submit(f.ctx, "u", "desk", call(id))
			return !errors.Is(err, calls.ErrOverloaded)
		})
		must(t, err)
		s.Close()
		receive(t, f.ctx, l)
		f.running(l, id)
	}
	f.disconnect(l)
	l = f.open("boot")
	for range 3 {
		m, ok := tryReceive(t, f.ctx, l, 2*time.Second)
		if !ok || m.GetCallQuery() == nil {
			t.Fatalf("sync stalled: %v; stats %+v", m, f.router.Stats())
		}
		send(t, f.ctx, l, statusRunning(m.GetCallQuery().CallId))
	}
	f.finishSync(l)
}

// Review 2 M2: the drain wait is transitive. C1 (old boot) still processes
// its final result when C2 (mid) and then C3 (new) supersede it; C3 must not
// synchronize, and lose the old-boot call, before C1 drained.
func TestChainedSupersedeKeepsFinalResult(t *testing.T) {
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
	f.open("mid")
	eventually(t, func() bool { v, ok, _ := f.registry.Lookup(f.ctx, "d"); return ok && v.Target.BootID == "mid" })
	l3 := f.open("new")
	if m, ok := tryReceive(t, f.ctx, l3, 50*time.Millisecond); ok {
		t.Fatalf("third connection synchronized before the first drained: %v", m)
	}
	release()
	<-d.done
	f.finishSync(l3)
	f.waitState("call", store.CallCompleted)
	nextResult(t, f.ctx, s)
}

// Review 2 L2: queries deferred during a sync are flushed within the sync
// bound, never all at once.
func TestDeferredFlushIsBounded(t *testing.T) {
	const done = 6
	f := setup(t, func(c *calls.Config, _ *gateway.Config) { c.MaxSyncRequests = 1 })
	l := f.connect("boot")
	for i := range done {
		id := fmt.Sprintf("done-%02d", i)
		s, err := f.router.Submit(f.ctx, "u", "desk", call(id))
		must(t, err)
		receive(t, f.ctx, l)
		f.result(l, id)
		f.waitState(id, store.CallCompleted)
		s.Close()
	}
	s, err := f.router.Submit(f.ctx, "u", "desk", call("active"))
	must(t, err)
	defer s.Close()
	receive(t, f.ctx, l)
	f.running(l, "active")
	f.disconnect(l)
	l = f.open("boot")
	if m := receive(t, f.ctx, l); m.GetCallQuery().GetCallId() != "active" {
		t.Fatal(m)
	}
	answers := make(chan error, done)
	for i := range done {
		id := fmt.Sprintf("done-%02d", i)
		go func() { _, err := f.router.Query(f.ctx, "u", id); answers <- err }()
	}
	eventually(t, func() bool { return f.router.Stats().QueryWaiters == done })
	send(t, f.ctx, l, statusRunning("active"))
	if m := receive(t, f.ctx, l); m.GetListJobs() == nil {
		t.Fatal(m)
	}
	if m, ok := tryReceive(t, f.ctx, l, 50*time.Millisecond); ok {
		t.Fatalf("deferred query sent beyond the bound: %v", m)
	}
	f.snapshot(l)
	for range done {
		m := receive(t, f.ctx, l)
		q := m.GetCallQuery()
		if q == nil {
			t.Fatal(m)
		}
		if m, ok := tryReceive(t, f.ctx, l, 20*time.Millisecond); ok {
			t.Fatalf("more than one unanswered request: %v", m)
		}
		send(t, f.ctx, l, statusCompleted(q.CallId))
	}
	for range done {
		must(t, waitAnswer(t, answers))
	}
}

// Review 1 R1 on the duplicate-Submit path: an equal duplicate on an unready
// route sends nothing ahead of the sync's resend.
func TestDuplicateSubmitDoesNotOvertakeResend(t *testing.T) {
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
	dup, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
	must(t, err)
	defer dup.Close()
	if m, ok := tryReceive(t, f.ctx, l, 50*time.Millisecond); ok {
		t.Fatalf("command on an unready route: %v", m)
	}
	close(gate)
	if m := receive(t, f.ctx, l); m.GetDispatchCall() == nil {
		t.Fatal(m)
	}
	f.finishSync(l)
	f.result(l, "call")
	f.waitState("call", store.CallCompleted)
}

// N31: a same-boot reconnect inside the grace period cancels the grace timer;
// its expiry then changes nothing.
func TestReconnectCancelsGraceTimer(t *testing.T) {
	f := setup(t)
	l := f.connect("boot")
	s, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
	must(t, err)
	defer s.Close()
	receive(t, f.ctx, l)
	f.running(l, "call")
	f.disconnect(l)
	l = f.open("boot")
	if m := receive(t, f.ctx, l); m.GetCallQuery() == nil {
		t.Fatal(m)
	}
	send(t, f.ctx, l, statusRunning("call"))
	f.finishSync(l)
	ts, err := f.db.ListTimers(f.ctx, store.TimerFilter{Kind: "device_grace", SubjectID: "d", Limit: 1})
	must(t, err)
	if len(ts) != 0 {
		t.Fatalf("grace timer survived the reconnect: %+v", ts)
	}
	f.advance(31 * time.Second)
	time.Sleep(20 * time.Millisecond)
	if st := f.state("call"); st != store.CallRunning {
		t.Fatalf("grace expiry after reconnect: %s", st)
	}
}

// N32: when the current, still synchronizing route drops, its deferred query
// waiters fail at once and report durable state.
func TestDeferredQueryFailsWhenSyncingRouteDrops(t *testing.T) {
	o := &observer{seen: make(chan string, 8)}
	f := setup(t, func(_ *calls.Config, g *gateway.Config) { g.Hooks = o })
	l := f.connect("boot")
	s, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
	must(t, err)
	defer s.Close()
	receive(t, f.ctx, l)
	f.running(l, "call")
	f.disconnect(l)
	entered, _ := o.arm()
	l = f.open("boot")
	<-entered
	answers := make(chan *circusv1.CallUpdate, 1)
	go func() { u, _ := f.router.Query(f.ctx, "u", "call"); answers <- u }()
	eventually(t, func() bool { return f.router.Stats().QueryWaiters == 1 })
	must(t, l.Close())
	if u := waitAnswer(t, answers); u.GetDispatched() == nil {
		t.Fatal(u)
	}
}

// N23: a query deferred on a route of the call's dispatch boot that is
// superseded by another boot before synchronizing fails at the new boot's
// sync; no CallQuery for the old boot's call goes to the new boot.
func TestDeferredQueryForReplacedBootFails(t *testing.T) {
	o := &observer{seen: make(chan string, 8)}
	f := setup(t, func(_ *calls.Config, g *gateway.Config) { g.Hooks = o })
	completedCall(t, f, "boot")
	entered, _ := o.arm()
	f.open("boot")
	<-entered
	answers := make(chan error, 1)
	go func() { _, err := f.router.Query(f.ctx, "u", "call"); answers <- err }()
	eventually(t, func() bool { return f.router.Stats().QueryWaiters == 1 })
	l := f.open("other")
	if err := waitAnswer(t, answers); !errors.Is(err, calls.ErrDeviceOffline) {
		t.Fatal(err)
	}
	f.finishSync(l)
	if m, ok := tryReceive(t, f.ctx, l, 50*time.Millisecond); ok {
		t.Fatalf("query for another boot's call: %v", m)
	}
}

// N33: Query of a call dispatched to a boot the device no longer runs asks
// nobody: a completed call reports ErrDeviceOffline at once.
func TestQueryOfOldBootCallAsksNobody(t *testing.T) {
	f := setup(t)
	completedCall(t, f, "boot")
	l := f.connect("other")
	answers := make(chan error, 1)
	go func() { _, err := f.router.Query(f.ctx, "u", "call"); answers <- err }()
	if err := waitAnswer(t, answers); !errors.Is(err, calls.ErrDeviceOffline) {
		t.Fatal(err)
	}
	if m, ok := tryReceive(t, f.ctx, l, 50*time.Millisecond); ok {
		t.Fatalf("query sent to another boot: %v", m)
	}
}

// N27: after a restart, a submission to a device whose row still says
// connected records the disconnect and its grace timer, so the accepted call
// goes offline at grace expiry even with no other call on the device.
func TestRestartSubmitRecordsDisconnect(t *testing.T) {
	f := setup(t)
	f.connect("boot")
	f.advance(time.Hour)
	f.crashRestart()
	started := f.clock.Now().UnixMilli()
	s, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
	must(t, err)
	defer s.Close()
	if u, err := s.Next(f.ctx); err != nil || u.GetAccepted() == nil {
		t.Fatal(u, err)
	}
	d, err := f.db.GetIdentity(f.ctx, store.KindDevice, "d")
	must(t, err)
	if d.LastDisconnectedAt != started {
		t.Fatalf("disconnect not recorded: %+v", d)
	}
	f.advance(31 * time.Second)
	f.waitState("call", store.CallOffline)
}

// N12: recording a restart's disconnect never moves an existing grace timer.
func TestRecordOfflineKeepsExistingGraceTimer(t *testing.T) {
	f := setup(t)
	f.connect("boot")
	f.crashRestart()
	due := f.clock.Now().Add(time.Hour).UnixMilli()
	must(t, f.db.SetTimer(f.ctx, "device_grace", "d", due))
	s, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
	must(t, err)
	defer s.Close()
	ts, err := f.db.ListTimers(f.ctx, store.TimerFilter{Kind: "device_grace", SubjectID: "d", Limit: 1})
	must(t, err)
	if len(ts) != 1 || ts[0].DueAt != due {
		t.Fatalf("grace timer moved: %+v", ts)
	}
}

// ctxErrStore makes GetIdentityByName fail with a chosen error, standing in
// for a store call cut short by its context.
type ctxErrStore struct {
	store.Store
	err atomic.Pointer[error]
}

func (s *ctxErrStore) GetIdentityByName(ctx context.Context, kind store.PrincipalKind, user, name string) (store.Identity, error) {
	if e := s.err.Load(); e != nil {
		return store.Identity{}, *e
	}
	return s.Store.GetIdentityByName(ctx, kind, user, name)
}

// N29 / review 1 L2: a context ending inside a store call is not a store
// failure; readiness stays healthy. A real failure still marks it.
func TestContextEndIsNotStoreFailure(t *testing.T) {
	wrapper := &ctxErrStore{}
	f := setup(t, func(c *calls.Config, _ *gateway.Config) { wrapper.Store = c.Store; c.Store = wrapper })
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		wrapper.err.Store(&cause)
		if _, err := f.router.Submit(f.ctx, "u", "desk", call("call")); !errors.Is(err, cause) {
			t.Fatal(err)
		}
		if !f.router.Stats().StoreHealthy {
			t.Fatalf("%v marked the store unhealthy", cause)
		}
	}
	broken := errors.New("disk failure")
	wrapper.err.Store(&broken)
	if _, err := f.router.Submit(f.ctx, "u", "desk", call("call")); !errors.Is(err, calls.ErrStoreUnavailable) {
		t.Fatal(err)
	}
	if f.router.Stats().StoreHealthy {
		t.Fatal("store failure not reported")
	}
	wrapper.err.Store(nil)
}
