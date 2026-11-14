package calls_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/controlplane/calls"
	"github.com/EricWvi/circus/internal/controlplane/gateway"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/wire"
	"google.golang.org/protobuf/proto"
)

type busySender struct{ calls.Sender }

func (b busySender) Deliver(ctx context.Context, target gateway.Target, msg *circusv1.DaemonDownlink) error {
	if msg.GetDispatchCall() != nil {
		return wire.ErrBusy
	}
	return b.Sender.Deliver(ctx, target, msg)
}
func TestFirstSendBusyIsRejectedAndKeepsBootEvidence(t *testing.T) {
	f := setup(t, func(c *calls.Config, _ *gateway.Config) { c.Sender = busySender{c.Sender} })
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

// tooLargeSender proves that a first dispatch was too large to be queued.
type tooLargeSender struct{ calls.Sender }

func (b tooLargeSender) Deliver(ctx context.Context, target gateway.Target, msg *circusv1.DaemonDownlink) error {
	if msg.GetDispatchCall() != nil {
		return wire.ErrTooLarge
	}
	return b.Sender.Deliver(ctx, target, msg)
}

func TestFirstSendTooLargeIsRejected(t *testing.T) {
	f := setup(t, func(c *calls.Config, _ *gateway.Config) { c.Sender = tooLargeSender{c.Sender} })
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

type queryCountSender struct {
	calls.Sender
	queries atomic.Int64
}

func (b *queryCountSender) Deliver(ctx context.Context, target gateway.Target, msg *circusv1.DaemonDownlink) error {
	if msg.GetCallQuery() != nil {
		b.queries.Add(1)
	}
	return b.Sender.Deliver(ctx, target, msg)
}

func TestQueriesCoalesceWithoutStarvingTheirReply(t *testing.T) {
	counted := &queryCountSender{}
	f := setup(t, func(c *calls.Config, _ *gateway.Config) {
		c.MaxOperations = 1
		counted.Sender = c.Sender
		c.Sender = counted
	})
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
	// Both waiters have passed the send decision. Exactly the leader may have
	// sent a CallQuery; this does not depend on wire scheduling or a quiet window.
	if got := counted.queries.Load(); got != 1 {
		t.Fatalf("CallQuery sends for two coalesced queries = %d, want 1", got)
	}
	send(t, f.ctx, l, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_CallStatus{CallStatus: &circusv1.CallStatus{CallId: "call", State: circusv1.CallStatus_STATE_COMPLETED, Result: &circusv1.DeviceResult{CallId: "call", Output: &circusv1.ToolOutput{}}}}})
	for range 2 {
		a := waitAnswer(t, answers)
		must(t, a.err)
		if a.u.GetResult() == nil {
			t.Fatal(a.u)
		}
	}
	eventually(t, func() bool { return f.router.Stats().QueryWaiters == 0 })
	if got := counted.queries.Load(); got != 1 {
		t.Fatalf("CallQuery sends after the shared reply = %d, want 1", got)
	}
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

func TestEqualDuplicateAttachesWhenPendingScratchIsFull(t *testing.T) {
	const maxMessageBytes = 256 << 10
	existing := call("existing")
	existing.GetBash().Command = strings.Repeat("x", 8192)
	filler := call("filler")
	filler.GetBash().Command = "x"
	filler.GetBash().Cwd = ""
	charge := func(c *circusv1.DeviceCall) int64 {
		envelope := &circusv1.WorkerUplink{Payload: &circusv1.WorkerUplink_SubmitCall{SubmitCall: &circusv1.SubmitCall{Device: "desk", Call: c}}}
		cost, err := wire.Charge(envelope, maxMessageBytes)
		must(t, err)
		return cost
	}
	existingScratch := int64(2 * proto.Size(existing))
	maxBytes := charge(existing) + existingScratch
	f := setup(t, func(c *calls.Config, _ *gateway.Config) {
		c.Pending.MaxBytes = maxBytes
	})
	l := f.connect("boot")

	first, err := f.router.Submit(f.ctx, "u", "desk", existing)
	must(t, err)
	defer first.Close()
	if m := receive(t, f.ctx, l); m.GetDispatchCall().GetCall().GetCallId() != existing.GetCallId() {
		t.Fatal(m)
	}
	second, err := f.router.Submit(f.ctx, "u", "desk", filler)
	must(t, err)
	defer second.Close()
	if u, err := second.Next(f.ctx); err != nil || u.GetAccepted() == nil {
		t.Fatalf("filler was not admitted: %v, %v", u, err)
	}
	if m := receive(t, f.ctx, l); m.GetDispatchCall().GetCall().GetCallId() != filler.GetCallId() {
		t.Fatal(m)
	}
	if remaining := maxBytes - f.router.Stats().PendingBytes; remaining >= existingScratch {
		t.Fatalf("test did not fill serialization scratch budget: remaining=%d scratch=%d", remaining, existingScratch)
	}

	duplicate, err := f.router.Submit(f.ctx, "u", "desk", existing)
	must(t, err)
	defer duplicate.Close()
	u, err := duplicate.Next(f.ctx)
	must(t, err)
	if u.GetCallId() != existing.GetCallId() || u.GetDispatched() == nil {
		t.Fatalf("equal duplicate must report existing dispatched state, got %v", u)
	}
}

func TestNewCallRejectsWhenScratchExhausted(t *testing.T) {
	f := setup(t, func(c *calls.Config, _ *gateway.Config) { c.Pending.MaxBytes = 1 })
	s, err := f.router.Submit(f.ctx, "u", "desk", call("new-scratch"))
	must(t, err)
	defer s.Close()
	u, err := s.Next(f.ctx)
	must(t, err)
	if u.GetRejected().GetCode() != circusv1.CallUpdate_Rejected_CODE_OVERLOADED {
		t.Fatalf("new call bypassed scratch admission: %v", u)
	}
	if _, err = f.db.GetCall(f.ctx, "new-scratch"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("rejected new call persisted: %v", err)
	}
}

func TestUndispatchedLostResubmitRejectsWhenScratchExhausted(t *testing.T) {
	f := setup(t, func(c *calls.Config, _ *gateway.Config) { c.Pending.MaxBytes = 1 })
	original := call("lost-scratch")
	data, err := proto.MarshalOptions{Deterministic: true}.Marshal(original)
	must(t, err)
	hash := sha256.Sum256(data)
	must(t, f.db.CreateCall(f.ctx, store.Call{
		CallID: original.CallId, UserID: "u", DeviceID: "d", SessionID: original.SessionId,
		Tool: "bash", RequestHash: hash[:], RequestBytes: int64(len(data)),
		State: store.CallAccepted, SubmittedAt: 1,
	}))
	changed, err := f.db.TransitionCall(f.ctx, original.CallId, store.CallExpect{States: []store.CallState{store.CallAccepted}, Undispatched: true}, store.CallLost, store.CallPatch{FinishedAt: 1, OutcomeReason: "CONTROL_PLANE_RESTARTED"})
	must(t, err)
	if !changed {
		t.Fatal("could not seed undispatched Lost call")
	}
	s, err := f.router.Submit(f.ctx, "u", "desk", original)
	must(t, err)
	defer s.Close()
	u, err := s.Next(f.ctx)
	must(t, err)
	if u.GetRejected().GetCode() != circusv1.CallUpdate_Rejected_CODE_OVERLOADED {
		t.Fatalf("lost resubmit bypassed scratch admission: %v", u)
	}
	row, err := f.db.GetCall(f.ctx, original.CallId)
	must(t, err)
	if row.State != store.CallLost || row.DispatchedBootID != "" {
		t.Fatalf("rejected resubmit changed the attempt: %+v", row)
	}
}
