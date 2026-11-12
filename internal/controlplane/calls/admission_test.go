package calls_test

import (
	"context"
	"errors"
	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/controlplane/bus"
	"github.com/EricWvi/circus/internal/controlplane/calls"
	"github.com/EricWvi/circus/internal/controlplane/gateway"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/wire"
	"testing"
	"time"
)

type busyBus struct{ bus.Bus }

func (b busyBus) Send(ctx context.Context, target bus.Target, msg *circusv1.DaemonDownlink) error {
	if msg.GetDispatchCall() != nil {
		return wire.ErrBusy
	}
	return b.Bus.Send(ctx, target, msg)
}
func TestFirstSendBusyIsRejectedAndKeepsBootEvidence(t *testing.T) {
	f := setup(t, func(c *calls.Config, _ *gateway.Config) { c.Bus = busyBus{c.Bus} })
	f.connect("boot")
	s, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
	must(t, err)
	defer s.Close()
	drain(t, f.ctx, s, 2)
	u, err := s.Next(f.ctx)
	must(t, err)
	if u.GetRejected().GetCode() != circusv1.CallUpdate_Rejected_CODE_OVERLOADED {
		t.Fatal(u)
	}
	c, err := f.db.GetCall(f.ctx, "call")
	must(t, err)
	if c.State != store.CallRejected || c.DispatchedBootID != "boot" {
		t.Fatal(c)
	}
	if f.router.Stats().PendingBytes != 0 {
		t.Fatal("busy send retained content")
	}
}
func TestQueriesCoalesceWithoutStarvingTheirReply(t *testing.T) {
	f := setup(t, func(c *calls.Config, _ *gateway.Config) { c.MaxOperations = 1 })
	l := f.connect("boot")
	var s *calls.Subscription
	var err error
	eventually(t, func() bool {
		s, err = f.router.Submit(f.ctx, "u", "desk", call("call"))
		return !errors.Is(err, calls.ErrOverloaded)
	})
	must(t, err)
	defer s.Close()
	receive(t, f.ctx, l)
	f.running(l, "call")
	drain(t, f.ctx, s, 3)
	type reply struct {
		u   *circusv1.CallUpdate
		err error
	}
	answers := make(chan reply, 2)
	for i := range 2 {
		eventually(t, func() bool { return f.router.Stats().Operations == 0 })
		go func() { u, err := f.router.Query(f.ctx, "u", "call"); answers <- reply{u, err} }()
		eventually(t, func() bool { return f.router.Stats().QueryWaiters == i+1 })
	}
	eventually(t, func() bool { return f.router.Stats().QueryWaiters == 2 })
	if m := receive(t, f.ctx, l); m.GetCallQuery() == nil {
		t.Fatal(m)
	}
	send(t, f.ctx, l, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_CallStatus{CallStatus: &circusv1.CallStatus{CallId: "call", State: circusv1.CallStatus_STATE_COMPLETED, Result: &circusv1.DeviceResult{CallId: "call", Output: &circusv1.ToolOutput{}}}}})
	for range 2 {
		a := <-answers
		must(t, a.err)
		if a.u.GetResult() == nil {
			t.Fatal(a.u)
		}
	}
	eventually(t, func() bool { return f.router.Stats().QueryWaiters == 0 })
	nextResult(t, f.ctx, s)
}
func TestQueryCancellationDoesNotCancelExecution(t *testing.T) {
	f := setup(t)
	l := f.connect("boot")
	s, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
	must(t, err)
	defer s.Close()
	receive(t, f.ctx, l)
	f.running(l, "call")
	drain(t, f.ctx, s, 3)
	ctx, cancel := context.WithCancel(f.ctx)
	answer := make(chan error, 1)
	go func() { _, err := f.router.Query(ctx, "u", "call"); answer <- err }()
	receive(t, f.ctx, l)
	cancel()
	if err = <-answer; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	c, err := f.db.GetCall(f.ctx, "call")
	must(t, err)
	if c.CancelRequestedAt != 0 || c.State != store.CallRunning {
		t.Fatal(c)
	}
	f.result(l, "call")
	nextResult(t, f.ctx, s)
}
func TestSubscriberContextAndStallReleaseOwnership(t *testing.T) {
	for _, mode := range []string{"context", "stall"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t)
			l := f.connect("boot")
			ctx, cancel := context.WithCancel(f.ctx)
			defer cancel()
			s, err := f.router.Submit(ctx, "u", "desk", call("call"))
			must(t, err)
			receive(t, f.ctx, l)
			if mode == "context" {
				cancel()
				eventually(t, func() bool { return f.router.Stats().Subscribers == 0 })
				if f.router.Stats().PendingBytes == 0 {
					t.Fatal("subscription cancellation cancelled execution")
				}
				f.result(l, "call")
				f.waitState("call", store.CallCompleted)
			} else {
				f.result(l, "call")
				f.waitState("call", store.CallCompleted)
				f.advance(16 * time.Second)
				eventually(t, func() bool { return f.router.Stats().Subscribers == 0 })
				if _, err = s.Next(f.ctx); !errors.Is(err, calls.ErrOverflow) {
					t.Fatal(err)
				}
			}
		})
	}
}
func TestStopSessionJobsIsBarrierAndUserScoped(t *testing.T) {
	f := setup(t)
	l := f.connect("boot")
	s, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
	must(t, err)
	defer s.Close()
	receive(t, f.ctx, l)
	f.running(l, "call")
	send(t, f.ctx, l, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_JobEvent{JobEvent: &circusv1.JobEvent{JobId: "012345abcdef", SessionId: "session", CallId: "call", Seq: 1, State: circusv1.JobEvent_STATE_RUNNING}}})
	eventually(t, func() bool { j, _ := f.db.ListJobs(f.ctx, store.JobFilter{Limit: 1}); return len(j) == 1 })
	must(t, f.router.StopSessionJobs(f.ctx, "other-user", "session"))
	must(t, f.router.StopSessionJobs(f.ctx, "u", "session"))
	m := receive(t, f.ctx, l)
	if m.GetStopSessionJobs().GetSessionId() != "session" {
		t.Fatal(m)
	}
	c, err := f.db.GetCall(f.ctx, "call")
	must(t, err)
	if c.CancelRequestedAt != 0 || c.State != store.CallRunning {
		t.Fatal(c)
	}
}

func TestAckReleasesPendingAdmission(t *testing.T) {
	f := setup(t, func(c *calls.Config, _ *gateway.Config) { c.Pending.MaxCalls = 1 })
	l := f.connect("boot")
	first, err := f.router.Submit(f.ctx, "u", "desk", call("first"))
	must(t, err)
	defer first.Close()
	receive(t, f.ctx, l)
	f.running(l, "first")
	eventually(t, func() bool { return f.router.Stats().PendingBytes == 0 })
	second, err := f.router.Submit(f.ctx, "u", "desk", call("second"))
	must(t, err)
	defer second.Close()
	m := receive(t, f.ctx, l)
	if m.GetDispatchCall().GetCall().GetCallId() != "second" {
		t.Fatal(m)
	}
}
