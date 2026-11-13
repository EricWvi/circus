package calls_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/controlplane/calls"
	"github.com/EricWvi/circus/internal/controlplane/gateway"
	"github.com/EricWvi/circus/internal/store"
)

// Found by TestRandomSequences (seed 24): a Query waiting for the daemon's
// CallStatus was never answered when the reply could not be applied because
// store writes failed (RUNNING rearms the watchdog, LOST settles the call).
// The waiter stayed until the worker's ctx ended. It must instead fall back
// to the durable state, like a query whose daemon cannot be asked.
func TestQueryAnsweredWhenReplyCannotPersist(t *testing.T) {
	for _, state := range []circusv1.CallStatus_State{circusv1.CallStatus_STATE_RUNNING, circusv1.CallStatus_STATE_LOST} {
		t.Run(state.String(), func(t *testing.T) {
			f := setup(t)
			l := f.connect("boot")
			s, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
			must(t, err)
			defer s.Close()
			receive(t, f.ctx, l)
			f.running(l, "call")
			f.fault.FailAllWrites(store.ErrBusy)
			type reply struct {
				u   *circusv1.CallUpdate
				err error
			}
			answer := make(chan reply, 1)
			ctx, cancel := context.WithTimeout(f.ctx, 3*time.Second)
			defer cancel()
			go func() { u, err := f.router.Query(ctx, "u", "call"); answer <- reply{u, err} }()
			if m := receive(t, f.ctx, l); m.GetCallQuery().GetCallId() != "call" {
				t.Fatal(m)
			}
			send(t, f.ctx, l, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_CallStatus{CallStatus: &circusv1.CallStatus{CallId: "call", State: state}}})
			a := <-answer
			if a.err != nil || a.u.GetDispatched() == nil {
				t.Fatalf("query not answered with the durable state: %v %v", a.u, a.err)
			}
			f.fault.FailAllWrites(nil)
		})
	}
}

// readFailStore fails top-level GetCall reads while failReads is set.
type readFailStore struct {
	store.Store
	failReads atomic.Bool
}

func (s *readFailStore) GetCall(ctx context.Context, id string) (store.Call, error) {
	if s.failReads.Load() {
		return store.Call{}, store.ErrBusy
	}
	return s.Store.GetCall(ctx, id)
}

// A daemon reply that cannot be applied because the store read before it
// fails: the waiting Query of a completed call reports the store failure
// (ErrStoreUnavailable; the device is online, so not ErrDeviceOffline),
// while an unfinished call falls back to its durable state.
func TestQueryReplyUnreadableAfterStoreReadFailure(t *testing.T) {
	for _, completed := range []bool{true, false} {
		name := "unfinished"
		if completed {
			name = "completed"
		}
		t.Run(name, func(t *testing.T) {
			reads := &readFailStore{}
			f := setup(t, func(c *calls.Config, _ *gateway.Config) { reads.Store = c.Store; c.Store = reads })
			l := f.connect("boot")
			s, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
			must(t, err)
			defer s.Close()
			receive(t, f.ctx, l)
			f.running(l, "call")
			reply := &circusv1.CallStatus{CallId: "call", State: circusv1.CallStatus_STATE_RUNNING}
			if completed {
				f.result(l, "call")
				nextResult(t, f.ctx, s)
				f.waitState("call", store.CallCompleted)
				reply = statusCompleted("call").GetCallStatus()
			}
			type answer struct {
				u   *circusv1.CallUpdate
				err error
			}
			done := make(chan answer, 1)
			ctx, cancel := context.WithTimeout(f.ctx, 3*time.Second)
			defer cancel()
			go func() { u, err := f.router.Query(ctx, "u", "call"); done <- answer{u, err} }()
			if m := receive(t, f.ctx, l); m.GetCallQuery().GetCallId() != "call" {
				t.Fatal(m)
			}
			reads.failReads.Store(true)
			send(t, f.ctx, l, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_CallStatus{CallStatus: reply}})
			a := <-done
			reads.failReads.Store(false)
			switch {
			case completed && !errors.Is(a.err, calls.ErrStoreUnavailable):
				t.Fatalf("completed call: got %v %v, want ErrStoreUnavailable", a.u, a.err)
			case !completed && (a.err != nil || a.u.GetDispatched() == nil):
				t.Fatalf("unfinished call: got %v %v, want its durable state", a.u, a.err)
			}
		})
	}
}
