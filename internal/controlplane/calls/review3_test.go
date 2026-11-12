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

// Tests for review m2-t5-review-3.

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
			<-h1.entered
			must(t, l1.Close())
			h2 := h.arm(t, "mid")
			l2 := f.open("mid")
			eventually(t, func() bool { v, ok, _ := f.registry.Lookup(f.ctx, "d"); return ok && v.Target.BootID == "mid" })
			send(t, f.ctx, l2, ghostResult())
			<-h2.entered
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

// gateQuery blocks the router's first CallQuery for one call inside Bus.Send,
// which runs under the admission lock with the operation's slot.
type gateQuery struct {
	bus.Bus
	id               string
	armed            atomic.Bool
	entered, release chan struct{}
}

func (g *gateQuery) Send(ctx context.Context, target bus.Target, m *circusv1.DaemonDownlink) error {
	if m.GetCallQuery().GetCallId() == g.id && g.armed.CompareAndSwap(true, false) {
		close(g.entered)
		<-g.release
	}
	return g.Bus.Send(ctx, target, m)
}

// After waiting for sync capacity (slot released), the sync re-acquires its
// operation slot before it continues: with MaxOperations == 1, nothing else
// is admitted while it sends.
func TestSyncReacquiresSlotAfterWait(t *testing.T) {
	g := &gateQuery{id: "c1", entered: make(chan struct{}), release: make(chan struct{})}
	f := setup(t, func(c *calls.Config, _ *gateway.Config) {
		c.MaxOperations, c.MaxSyncRequests = 1, 1
		g.Bus = c.Bus
		c.Bus = g
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
	<-g.entered                            // sync blocked sending c1's query
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
	<-held.entered
	must(t, old.Close())
	waiting := f.openAs("laptop", "y") // waits for the laptop's old connection
	l := f.open("boot")
	if m, ok := tryReceive(t, f.ctx, l, 2*time.Second); !ok || m.GetListJobs() == nil {
		t.Fatalf("desk sync delayed by the laptop's old connection: %v", m)
	}
	f.snapshot(l)
	quietLink(t, f.ctx, waiting, "laptop synchronized before its old connection drained")
	held.free()
	laptopSync(waiting)
}
