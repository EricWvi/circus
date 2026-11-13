package calls_test

import (
	"errors"
	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/controlplane/calls"
	"github.com/EricWvi/circus/internal/controlplane/gateway"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/wire"
	"github.com/EricWvi/circus/internal/wirepolicy"
	"google.golang.org/protobuf/proto"
	"strings"
	"testing"
	"time"
)

func TestDaemonQueueOverflowResyncF12(t *testing.T) {
	f := setup(t)
	f.daemonBase.MaxQueuedBytes = 1024
	l := f.connect("boot")
	s, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
	must(t, err)
	defer s.Close()
	receive(t, f.ctx, l)
	f.running(l, "call")
	drain(t, f.ctx, s, 3)
	result := &circusv1.DeviceResult{CallId: "call", Output: &circusv1.ToolOutput{Content: []*circusv1.ContentBlock{{Block: &circusv1.ContentBlock_Text{Text: strings.Repeat("x", 4096)}}}}}
	m := &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_Result{Result: result}}
	class, key, err := wirepolicy.Classify(m)
	must(t, err)
	if err = l.Send(f.ctx, m, class, key); !errors.Is(err, wire.ErrQueueFull) {
		t.Fatal(err)
	}
	eventually(t, func() bool { return f.registry.Stats().Connections == 0 })
	f.daemonBase.MaxQueuedBytes = 0
	l = f.open("boot")
	if m := receive(t, f.ctx, l); m.GetCallQuery() == nil {
		t.Fatal(m)
	}
	f.finishSync(l)
	send(t, f.ctx, l, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_CallStatus{CallStatus: &circusv1.CallStatus{CallId: "call", State: circusv1.CallStatus_STATE_COMPLETED, Result: result}}})
	nextResult(t, f.ctx, s)
	f.waitState("call", store.CallCompleted)
}
func TestGatewayQueueOverflowResyncF13(t *testing.T) {
	f := setup(t, func(c *calls.Config, g *gateway.Config) {
		g.LinkOptions.MaxQueuedBytes = 1024
		c.ResendHorizon = time.Millisecond
	})
	f.connect("boot")
	v := call("call")
	v.GetBash().Command = strings.Repeat("x", 4096)
	s, err := f.router.Submit(f.ctx, "u", "desk", v)
	must(t, err)
	defer s.Close()
	drain(t, f.ctx, s, 2)
	eventually(t, func() bool { return f.registry.Stats().Connections == 0 })
	f.advance(2 * time.Millisecond)
	l := f.open("boot")
	if m := receive(t, f.ctx, l); m.GetCallQuery() == nil {
		t.Fatal(m)
	}
	f.finishSync(l)
	send(t, f.ctx, l, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_CallStatus{CallStatus: &circusv1.CallStatus{CallId: "call", State: circusv1.CallStatus_STATE_LOST}}})
	f.waitState("call", store.CallLost)
}
func TestHalfOpenHeartbeatThenGraceF15(t *testing.T) {
	f := setup(t, func(_ *calls.Config, g *gateway.Config) { g.LinkOptions.DisableHeartbeat = false })
	l := f.connect("boot")
	s, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
	must(t, err)
	defer s.Close()
	receive(t, f.ctx, l)
	drain(t, f.ctx, s, 2)
	before := f.hooks.disconnects.Load()
	f.advance(16 * time.Second)
	f.awaitDisconnectRecorded(before, f.clock.Now().UnixMilli())
	f.advance(31 * time.Second)
	f.waitState("call", store.CallUnknown)
}
func TestMalformedFragmentsCloseAndResyncF23(t *testing.T) {
	for _, name := range []string{"malformed", "out-of-order", "too-large", "missing-tail"} {
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			l := f.connect("boot")
			s, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
			must(t, err)
			defer s.Close()
			receive(t, f.ctx, l)
			f.running(l, "call")
			drain(t, f.ctx, s, 3)
			raw := []byte{0xff}
			if name != "malformed" {
				fragment := &circusv1.Fragment{MessageId: 1, TotalSize: 2, Data: []byte{1}}
				if name == "out-of-order" {
					fragment.Index = 1
					fragment.TotalSize = 0
				}
				if name == "too-large" {
					fragment.TotalSize = 300 << 10
				}
				raw, err = proto.Marshal(&circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_Fragment{Fragment: fragment}})
				must(t, err)
			}
			must(t, f.raw[len(f.raw)-1].WriteMessage(f.ctx, raw))
			if name == "missing-tail" {
				eventually(t, func() bool { return f.gateway.Stats().Wire.ReassemblyMessages > 0 })
				f.advance(16 * time.Second)
			}
			eventually(t, func() bool { return f.registry.Stats().Connections == 0 })
			l = f.open("boot")
			if m := receive(t, f.ctx, l); m.GetCallQuery() == nil {
				t.Fatal(m)
			}
			f.finishSync(l)
			f.result(l, "call")
			nextResult(t, f.ctx, s)
			f.waitState("call", store.CallCompleted)
		})
	}
}

// A result sent just before a graceful daemon close is processed before the
// disconnect (D43 item 12).
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

// A non-protocol hook error (store failure) does not stop the gateway reading
// that link; the next reply settles the call without a reconnect.
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
