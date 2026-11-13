package calls_test

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/EricWvi/circus/internal/controlplane/calls"
	"github.com/EricWvi/circus/internal/controlplane/gateway"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/testutil/wait"
)

// A failed disconnect write keeps a recovery path: the sweep persists it, so
// the accepted call later goes offline and releases its content.
func TestDisconnectWriteFailureIsRetried(t *testing.T) {
	o := &observer{seen: make(chan string, 8)}
	entered, _ := o.arm()
	f := setup(t, func(_ *calls.Config, g *gateway.Config) { g.Hooks = o })
	l := f.open("boot")
	wait.Recv(t, entered)
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

// A grace expiry larger than one page (64) drains in successive claims.
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

// Before the 30-minute result-retention cutoff, watchdogs only re-arm; a
// same-boot reconnect then recovers the result.
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

// The router withdraws the route under its lock when it records the
// disconnect, so a submission in between cannot dispatch to the closed link;
// it waits accepted for the grace period.
func TestDisconnectWithdrawsRouteUnderLock(t *testing.T) {
	g := &gatedDisconnect{entered: make(chan struct{}), gate: make(chan struct{})}
	f := setup(t, func(_ *calls.Config, c *gateway.Config) { c.Hooks = g })
	l := f.connect("boot")
	g.armed.Store(true)
	must(t, l.Close())
	wait.Recv(t, g.entered)
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

// A same-boot reconnect inside the grace period cancels the grace timer; its
// expiry then changes nothing.
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
