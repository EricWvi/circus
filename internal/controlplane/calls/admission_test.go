package calls_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
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
	f := setup(t, func(c *calls.Config, _ *gateway.Config) { c.Pending.MaxCalls = 2 })
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
		c.Pending.MaxBytes = 2 * maxBytes
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
	f := setup(t, func(c *calls.Config, _ *gateway.Config) { c.Pending.MaxBytes = 2 })
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
	f := setup(t, func(c *calls.Config, _ *gateway.Config) { c.Pending.MaxBytes = 2 })
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

// Wait reports readiness without consuming: it returns at once while an
// update is queued, waits for a later one, honours ctx, returns once the
// subscription has ended, never changes charges, and does not hold off the
// subscriber-stall sweep.
func TestSubscriptionWait(t *testing.T) {
	f := setup(t)
	l := f.connect("boot")
	s, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
	must(t, err)
	defer s.Close()
	receive(t, f.ctx, l)
	head := mustWait(t, f, s) // Accepted and Dispatched are already queued
	buffered := f.router.Stats().BufferedBytes
	if head <= 0 || head >= buffered {
		t.Fatalf("head cost %d of %d buffered for two queued updates", head, buffered)
	}
	if again := mustWait(t, f, s); again != head {
		t.Fatalf("Wait changed the head: %d -> %d", head, again)
	}
	if buffered == 0 || f.router.Stats().BufferedBytes != buffered {
		t.Fatalf("Wait changed charges: %d -> %d", buffered, f.router.Stats().BufferedBytes)
	}
	if u, err := s.Next(f.ctx); err != nil || u.GetAccepted() == nil {
		t.Fatalf("Wait consumed the queued update: %v %v", u, err)
	}
	if u, err := s.Next(f.ctx); err != nil || u.GetDispatched() == nil {
		t.Fatalf("second queued update: %v %v", u, err)
	}
	cancelled, cancel := context.WithCancel(f.ctx)
	cancel()
	if _, err = s.Wait(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait with nothing queued and a cancelled ctx: %v", err)
	}
	ready := make(chan error, 1)
	go func() { _, err := s.Wait(f.ctx); ready <- err }()
	select {
	case err := <-ready:
		t.Fatalf("Wait returned with nothing queued: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	f.result(l, "call")
	must(t, waitAnswer(t, ready))
	mustWait(t, f, s)
	if u, err := s.Next(f.ctx); err != nil || u.GetResult() == nil {
		t.Fatalf("result after Wait: %v %v", u, err)
	}
	if cost := mustWait(t, f, s); cost != 0 { // terminal: ended, Next reports io.EOF
		t.Fatalf("ended subscription reported cost %d", cost)
	}
	if _, err = s.Next(f.ctx); !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
}

func TestSubscriptionWaitKeepsStallClock(t *testing.T) {
	f := setup(t)
	l := f.connect("boot")
	s, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
	must(t, err)
	defer s.Close()
	receive(t, f.ctx, l)
	f.result(l, "call")
	f.waitState("call", store.CallCompleted)
	// Waiting never counts as a read: the stall clock keeps its start.
	since := calls.LastRead(s)
	f.clock.Advance(5 * time.Second) // still below the 15 s bound
	mustWait(t, f, s)
	if got := calls.LastRead(s); !got.Equal(since) {
		t.Fatalf("Wait moved the stall clock from %v to %v", since, got)
	}
	// Step the clock until the sweep detaches it; a step that races the
	// sweep re-arming its timer is simply followed by another.
	eventually(t, func() bool { f.advance(time.Second); return f.router.Stats().Subscribers == 0 })
	mustWait(t, f, s)
	if _, err = s.Next(f.ctx); !errors.Is(err, calls.ErrOverflow) {
		t.Fatalf("a waiting reader escaped the stall bound: %v", err)
	}
	closed, err := f.router.Submit(f.ctx, "u", "desk", call("closed"))
	must(t, err)
	must(t, closed.Close())
	mustWait(t, f, closed) // ended by Close
}

func mustWait(t *testing.T, f *fixture, s *calls.Subscription) int64 {
	t.Helper()
	cost, err := s.Wait(f.ctx)
	must(t, err)
	return cost
}

// QueryOnce hands the answer out through a single-update subscription: the
// answer stays charged until Next takes it, later call updates do not reach
// it, and Query's errors are returned directly.
func TestQueryOnce(t *testing.T) {
	f := setup(t)
	l := f.connect("boot")
	watch, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
	must(t, err)
	defer watch.Close()
	receive(t, f.ctx, l)
	for range 2 { // Accepted, Dispatched
		if _, err = watch.Next(f.ctx); err != nil {
			t.Fatal(err)
		}
	}
	answered := make(chan *calls.Subscription, 1)
	go func() {
		s, err := f.router.QueryOnce(f.ctx, "u", "call")
		if err != nil {
			t.Error(err)
		}
		answered <- s
	}()
	if m := receive(t, f.ctx, l); m.GetCallQuery().GetCallId() != "call" {
		t.Fatal(m)
	}
	f.running(l, "call")
	s := waitAnswer(t, answered)
	if s == nil {
		return
	}
	defer s.Close()
	cost := mustWait(t, f, s)
	if cost <= 0 || f.router.Stats().BufferedBytes < cost {
		t.Fatalf("answer not charged while queued: cost %d, buffered %d", cost, f.router.Stats().BufferedBytes)
	}
	f.result(l, "call") // published to the watch, never to the answer
	nextResult(t, f.ctx, watch)
	if u, err := s.Next(f.ctx); err != nil || u.GetDispatched() == nil {
		t.Fatalf("answer: %v %v", u, err)
	}
	if _, err := s.Next(f.ctx); !errors.Is(err, io.EOF) {
		t.Fatalf("a single answer must end its subscription: %v", err)
	}
	eventually(t, func() bool { st := f.router.Stats(); return st.BufferedBytes == 0 && st.Subscribers == 0 })
	if _, err := f.router.QueryOnce(f.ctx, "u", "missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown call: %v", err)
	}
	if _, err := f.router.QueryOnce(f.ctx, "u", "bad id!"); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("invalid id: %v", err)
	}
}

// Once the intent is durable, a CancelCall that cannot be queued to the
// daemon is not the caller's error: the sync or watchdog replays it.
func TestCancelDeliveryFailureAfterIntentIsRecorded(t *testing.T) {
	f := setup(t, func(c *calls.Config, _ *gateway.Config) {
		inner := c.Sender
		c.Sender = calls.SenderFunc(func(ctx context.Context, target gateway.Target, m *circusv1.DaemonDownlink) error {
			if m.GetCancelCall() != nil {
				return gateway.ErrNoRoute
			}
			return inner.Deliver(ctx, target, m)
		})
	})
	l := f.connect("boot")
	s, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
	must(t, err)
	defer s.Close()
	receive(t, f.ctx, l)
	f.running(l, "call")
	f.waitState("call", store.CallRunning)
	must(t, f.router.Cancel(f.ctx, "u", "call"))
	c, err := f.db.GetCall(f.ctx, "call")
	must(t, err)
	if c.CancelRequestedAt == 0 {
		t.Fatal("intent not recorded")
	}
	f.result(l, "call")
}

func TestPendingBudgetsShareByUser(t *testing.T) {
	for _, budget := range []string{"count", "bytes"} {
		t.Run(budget, func(t *testing.T) {
			first := call("first")
			const maxMessageBytes = 256 << 10
			cost, err := wire.Charge(&circusv1.WorkerUplink{Payload: &circusv1.WorkerUplink_SubmitCall{
				SubmitCall: &circusv1.SubmitCall{Device: "desk", Call: first},
			}}, maxMessageBytes)
			must(t, err)
			f := setup(t, func(c *calls.Config, _ *gateway.Config) {
				if budget == "count" {
					c.Pending.MaxCalls = 2
				} else {
					c.Pending.MaxBytes = 2 * (cost + int64(2*proto.Size(first)))
				}
			})
			must(t, f.db.CreateUser(f.ctx, store.User{ID: "v", Name: "v", CreatedAt: 1}))
			must(t, f.db.CreateIdentity(f.ctx, store.Identity{Kind: store.KindDevice, ID: "other", UserID: "v", Name: "desk", MachineID: "m", CreatedAt: 1}))
			// Both devices are within their disconnect grace periods.
			for _, id := range []string{"d", "other"} {
				now := f.clock.Now().UnixMilli()
				_, err := f.db.SetConnected(f.ctx, store.KindDevice, id, "boot", now)
				must(t, err)
				must(t, f.db.SetDisconnected(f.ctx, store.KindDevice, id, now))
			}
			submit := func(user string, req *circusv1.DeviceCall) *circusv1.CallUpdate {
				t.Helper()
				sub, err := f.router.Submit(f.ctx, user, "desk", req)
				must(t, err)
				defer sub.Close()
				update, err := sub.Next(f.ctx)
				must(t, err)
				return update
			}
			if u := submit("u", first); u.GetAccepted() == nil {
				t.Fatalf("first call: %v", u)
			}
			if u := submit("u", call("extra")); u.GetRejected().GetCode() != circusv1.CallUpdate_Rejected_CODE_OVERLOADED {
				t.Fatalf("same user over budget: %v", u)
			}
			if u := submit("v", call("other")); u.GetAccepted() == nil {
				t.Fatalf("another user at available capacity: %v", u)
			}
			must(t, f.router.Cancel(f.ctx, "u", "first"))
			if u := submit("u", call("retry")); u.GetAccepted() == nil {
				t.Fatalf("slot did not reopen after cancel: %v", u)
			}
		})
	}
}
