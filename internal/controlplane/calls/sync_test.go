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
	"github.com/EricWvi/circus/internal/testutil/wait"
	"github.com/EricWvi/circus/internal/wire"
)

func ackRunning(id string) *circusv1.DaemonUplink {
	return &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_CallAck{CallAck: &circusv1.CallAck{CallId: id, Outcome: &circusv1.CallAck_Status{Status: &circusv1.CallStatus{CallId: id, State: circusv1.CallStatus_STATE_RUNNING}}}}}
}

// A Query on a registered but unsynchronized route is deferred to the sync;
// it never overtakes the sync's resend, and the resend's ack answers it.
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
	wait.Recv(t, entered)
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
	if u := wait.Recv(t, queried); u.GetDispatched() == nil {
		t.Fatal(u)
	}
	f.result(l, "call")
	f.waitState("call", store.CallCompleted)
}

// The watchdog and Cancel send nothing on an unready route; the sync replays
// the cancel after the resend.
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
	wait.Recv(t, entered)
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

// The old boot's final result, received before the restarted daemon's new
// boot registers, settles the call; the new boot's sync waits for the
// superseded link to drain before losing old-boot calls.
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
	wait.Recv(t, d.entered)
	must(t, l.Close())
	l2 := f.open("new")
	if m, ok := tryReceive(t, f.ctx, l2, 50*time.Millisecond); ok {
		t.Fatalf("new boot synchronized before the old link drained: %v", m)
	}
	release()
	wait.Recv(t, d.done)
	f.finishSync(l2)
	f.waitState("call", store.CallCompleted)
	nextResult(t, f.ctx, s)
}

// A sync waiting for request capacity releases its operation slot, so the
// replies it waits for can enter even with MaxOperations == 1.
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
		m, ok := tryReceive(t, f.ctx, l, wait.Timeout)
		if !ok || m.GetCallQuery() == nil {
			t.Fatalf("sync stalled: %v; stats %+v", m, f.router.Stats())
		}
		send(t, f.ctx, l, statusRunning(m.GetCallQuery().CallId))
	}
	f.finishSync(l)
}

// Queries deferred during a sync are flushed within the sync bound, never
// all at once.
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

// An equal duplicate Submit on an unready route sends nothing ahead of the
// sync's resend.
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
	wait.Recv(t, entered)
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
			m, ok := tryReceive(t, f.ctx, l, wait.Timeout)
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

// heldResult holds one OnDeviceResult of a boot until freed.
type heldResult struct {
	entered, release chan struct{}
	once             sync.Once
}

func (h *heldResult) free() { h.once.Do(func() { close(h.release) }) }

// holdResults holds the next OnDeviceResult of each armed boot, standing in
// for a reader that is behind while newer connections register.
type holdResults struct {
	gateway.Hooks
	mu   sync.Mutex
	held map[string]*heldResult
}

func (h *holdResults) SetHooks(x gateway.Hooks) { h.Hooks = x }
func (h *holdResults) arm(t *testing.T, boot string) *heldResult {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.held == nil {
		h.held = make(map[string]*heldResult)
	}
	held := &heldResult{entered: make(chan struct{}), release: make(chan struct{})}
	h.held[boot] = held
	t.Cleanup(held.free) // runs before the fixture's teardown, also on failure
	return held
}
func (h *holdResults) OnDeviceResult(ctx context.Context, c gateway.Connection, v *circusv1.DeviceResult) error {
	h.mu.Lock()
	held := h.held[c.Target.BootID]
	delete(h.held, c.Target.BootID)
	h.mu.Unlock()
	if held != nil {
		close(held.entered)
		<-held.release
	}
	return h.Hooks.OnDeviceResult(ctx, c, v)
}

func ghostResult() *circusv1.DaemonUplink {
	return &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_Result{Result: &circusv1.DeviceResult{CallId: "ghost", Output: &circusv1.ToolOutput{}}}}
}

func quietLink(t *testing.T, ctx context.Context, l *wire.Link, why string) {
	t.Helper()
	if m, ok := tryReceive(t, ctx, l, 50*time.Millisecond); ok {
		t.Fatalf("%s: %v", why, m)
	}
}

// The drain wait covers every older connection of the device, whichever
// finishes first: C3 synchronizes only after both C1 (old boot, holding the
// call's final result) and C2 (mid boot, holding a reply) are released.
func TestDrainWaitCoversEveryOlderConnection(t *testing.T) {
	for _, newerFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("newer-first=%v", newerFirst), func(t *testing.T) {
			h := &holdResults{}
			f := setup(t, func(_ *calls.Config, g *gateway.Config) { g.Hooks = h })
			l1 := f.connect("old")
			s, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
			must(t, err)
			defer s.Close()
			receive(t, f.ctx, l1)
			drain(t, f.ctx, s, 2)
			h1 := h.arm(t, "old")
			f.result(l1, "call")
			wait.Recv(t, h1.entered)
			must(t, l1.Close())
			h2 := h.arm(t, "mid")
			l2 := f.open("mid")
			eventually(t, func() bool { v, ok, _ := f.registry.Lookup(f.ctx, "d"); return ok && v.Target.BootID == "mid" })
			send(t, f.ctx, l2, ghostResult())
			wait.Recv(t, h2.entered)
			l3 := f.open("new")
			quietLink(t, f.ctx, l3, "synchronized while both older connections drain")
			first, second := h1, h2
			if newerFirst {
				first, second = h2, h1
			}
			first.free()
			eventually(t, func() bool { return f.gateway.Stats().Connections == 2 })
			quietLink(t, f.ctx, l3, "synchronized while an older connection drains")
			second.free()
			f.finishSync(l3)
			f.waitState("call", store.CallCompleted)
			nextResult(t, f.ctx, s)
		})
	}
}

// gateQuery blocks the router's first CallQuery for one call inside Sender.Deliver,
// which runs under the admission lock with the operation's slot.
type gateQuery struct {
	calls.Sender
	id               string
	armed            atomic.Bool
	entered, release chan struct{}
}

func (g *gateQuery) Deliver(ctx context.Context, target gateway.Target, m *circusv1.DaemonDownlink) error {
	if m.GetCallQuery().GetCallId() == g.id && g.armed.CompareAndSwap(true, false) {
		close(g.entered)
		<-g.release
	}
	return g.Sender.Deliver(ctx, target, m)
}

// After waiting for sync capacity (slot released), the sync re-acquires its
// operation slot before it continues: with MaxOperations == 1, nothing else
// is admitted while it sends.
func TestSyncReacquiresSlotAfterWait(t *testing.T) {
	g := &gateQuery{id: "c1", entered: make(chan struct{}), release: make(chan struct{})}
	f := setup(t, func(c *calls.Config, _ *gateway.Config) {
		c.MaxOperations, c.MaxSyncRequests = 1, 1
		g.Sender = c.Sender
		c.Sender = g
	})
	release := sync.OnceFunc(func() { close(g.release) })
	t.Cleanup(release)
	l := f.connect("boot")
	for i := range 2 {
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
	g.armed.Store(true)
	l = f.open("boot")
	if m := receive(t, f.ctx, l); m.GetCallQuery().GetCallId() != "c0" {
		t.Fatal(m)
	}
	send(t, f.ctx, l, statusRunning("c0")) // frees capacity; the sync wakes
	wait.Recv(t, g.entered)                // sync blocked sending c1's query
	answer := make(chan error, 1)
	go func() { _, err := f.router.Query(f.ctx, "u", "c0"); answer <- err }()
	if err := waitAnswer(t, answer); !errors.Is(err, calls.ErrOverloaded) {
		t.Fatalf("admitted beyond MaxOperations while the sync runs: %v", err)
	}
	release()
	if m := receive(t, f.ctx, l); m.GetCallQuery().GetCallId() != "c1" {
		t.Fatal(m)
	}
	send(t, f.ctx, l, statusRunning("c1"))
	f.finishSync(l)
}

func TestResendHorizonRecheckedAfterSyncWait(t *testing.T) {
	f := setup(t, func(c *calls.Config, _ *gateway.Config) {
		c.MaxSyncRequests = 1
		c.ResendHorizon = time.Second
	})
	l := f.connect("boot")
	for _, id := range []string{"c0", "c1"} {
		s, err := f.router.Submit(f.ctx, "u", "desk", call(id))
		must(t, err)
		s.Close()
		receive(t, f.ctx, l)
	}
	f.disconnect(l)
	timers := f.clock.Waiters()
	l = f.open("boot")
	if m := receive(t, f.ctx, l); m.GetDispatchCall().GetCall().GetCallId() != "c0" {
		t.Fatal(m)
	}
	// Wait until c1 has passed the resend check before the wait and is parked
	// on the only sync slot: it armed the DeliveryTimeout alarm and gave up its
	// operation slot.
	eventually(t, func() bool { return f.clock.Waiters() > timers && f.router.Stats().Operations == 0 })
	if s := f.router.Stats(); s.PendingCalls != 2 {
		t.Fatalf("pending before the horizon: %+v", s)
	}
	// Cross the horizon before c0's reply releases the slot; c1 must recover
	// by query rather than a late resend.
	f.advance(2 * time.Second)
	send(t, f.ctx, l, ackRunning("c0"))
	if m := receive(t, f.ctx, l); m.GetCallQuery().GetCallId() != "c1" {
		t.Fatalf("expected a query after the sync wait, got %v", m)
	}
	// The late replay released c1's content itself; no reply has arrived yet
	// that would release it.
	if s := f.router.Stats(); s.PendingCalls != 0 {
		t.Fatalf("query-only recovery retained request content: %+v", s)
	}
	send(t, f.ctx, l, statusRunning("c1"))
	f.finishSync(l)
}

// A device's sync never waits for another device's old connection.
func TestOtherDeviceOldConnectionDoesNotDelaySync(t *testing.T) {
	h := &holdResults{}
	f := setup(t, func(_ *calls.Config, g *gateway.Config) { g.Hooks = h })
	must(t, f.db.CreateIdentity(f.ctx, store.Identity{Kind: store.KindDevice, ID: "d2", UserID: "u", Name: "laptop", MachineID: "m2", CreatedAt: 1}))
	laptopSync := func(l *wire.Link) {
		t.Helper()
		if m := receive(t, f.ctx, l); m.GetListJobs() == nil {
			t.Fatal(m)
		}
		send(t, f.ctx, l, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_JobSnapshot{JobSnapshot: &circusv1.JobSnapshot{}}})
		eventually(t, func() bool { v, ok, _ := f.registry.Lookup(f.ctx, "d2"); return ok && v.Ready })
	}
	old := f.openAs("laptop", "x")
	laptopSync(old)
	held := h.arm(t, "x")
	send(t, f.ctx, old, ghostResult())
	wait.Recv(t, held.entered)
	must(t, old.Close())
	waiting := f.openAs("laptop", "y") // waits for the laptop's old connection
	l := f.open("boot")
	if m, ok := tryReceive(t, f.ctx, l, wait.Timeout); !ok || m.GetListJobs() == nil {
		t.Fatalf("desk sync delayed by the laptop's old connection: %v", m)
	}
	f.snapshot(l)
	quietLink(t, f.ctx, waiting, "laptop synchronized before its old connection drained")
	held.free()
	laptopSync(waiting)
}
