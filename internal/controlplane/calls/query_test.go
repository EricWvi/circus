package calls_test

import (
	"errors"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/controlplane/calls"
	"github.com/EricWvi/circus/internal/controlplane/gateway"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/testutil/wait"
)

// A query in flight on a superseded connection is failed by that connection's
// disconnect; later queries reach the new route.
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
	if err := wait.Recv(t, first); !errors.Is(err, calls.ErrDeviceOffline) {
		t.Fatalf("superseded query: %v", err)
	}
	f.finishSync(l2)
	second := make(chan *circusv1.CallUpdate, 1)
	go func() { u, _ := f.router.Query(f.ctx, "u", "call"); second <- u }()
	if m := receive(t, f.ctx, l2); m.GetCallQuery().GetCallId() != "call" {
		t.Fatal(m)
	}
	send(t, f.ctx, l2, statusCompleted("call"))
	if u := wait.Recv(t, second); u.GetResult() == nil {
		t.Fatal(u)
	}
	eventually(t, func() bool { return f.router.Stats().QueryWaiters == 0 })
}

// completedCall drives "call" to completed on boot, then disconnects it.
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

// When the current, still synchronizing route drops, its deferred query
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
	wait.Recv(t, entered)
	answers := make(chan *circusv1.CallUpdate, 1)
	go func() { u, _ := f.router.Query(f.ctx, "u", "call"); answers <- u }()
	eventually(t, func() bool { return f.router.Stats().QueryWaiters == 1 })
	must(t, l.Close())
	if u := waitAnswer(t, answers); u.GetDispatched() == nil {
		t.Fatal(u)
	}
}

// A query deferred on a route of the call's dispatch boot that is superseded
// by another boot before synchronizing fails at the new boot's sync; no
// CallQuery for the old boot's call goes to the new boot.
func TestDeferredQueryForReplacedBootFails(t *testing.T) {
	o := &observer{seen: make(chan string, 8)}
	f := setup(t, func(_ *calls.Config, g *gateway.Config) { g.Hooks = o })
	completedCall(t, f, "boot")
	entered, _ := o.arm()
	f.open("boot")
	wait.Recv(t, entered)
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

// Query of a call dispatched to a boot the device no longer runs asks nobody:
// a completed call reports ErrDeviceOffline at once.
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
