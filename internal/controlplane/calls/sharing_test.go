package calls_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/controlplane/calls"
	"github.com/EricWvi/circus/internal/controlplane/gateway"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/wire"
)

func sharedResult(t *testing.T) (*circusv1.DaemonUplink, int64) {
	t.Helper()
	m := statusCompleted("call")
	m.GetCallStatus().Result.Output.Content = []*circusv1.ContentBlock{{Block: &circusv1.ContentBlock_Text{Text: strings.Repeat("x", 32<<10)}}}
	u := &circusv1.CallUpdate{CallId: "call", Update: &circusv1.CallUpdate_Result{Result: m.GetCallStatus().Result}}
	cost, err := wire.Charge(u, 64<<10)
	must(t, err)
	return m, cost
}

func sharedBudget(cost int64) func(*calls.Config, *gateway.Config) {
	return func(c *calls.Config, _ *gateway.Config) {
		c.MaxMessageBytes = 64 << 10
		c.MaxBufferedUpdateBytes = 3 * cost
		c.MaxSubscriptionBytes = cost
	}
}

func assertBuffered(t *testing.T, f *fixture, want int64) {
	t.Helper()
	if got := f.router.Stats().BufferedBytes; got != want {
		t.Fatalf("buffered bytes = %d, want %d", got, want)
	}
}

func TestSharedCompletedQueryAnswer(t *testing.T) {
	for _, mode := range []string{"query_once", "duplicate_submit"} {
		t.Run(mode, func(t *testing.T) {
			m, cost := sharedResult(t)
			f := setup(t, sharedBudget(cost))
			l := f.connect("boot")
			watch, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
			must(t, err)
			receive(t, f.ctx, l)
			f.result(l, "call")
			nextResult(t, f.ctx, watch)
			assertBuffered(t, f, 0)

			type answer struct {
				u   *circusv1.CallUpdate
				err error
			}
			plain := make(chan answer, 1)
			go func() { u, err := f.router.Query(f.ctx, "u", "call"); plain <- answer{u, err} }()
			eventually(t, func() bool { return f.router.Stats().QueryWaiters == 1 })
			once := make(chan *calls.Subscription, 1)
			go func() {
				var s *calls.Subscription
				var err error
				if mode == "query_once" {
					s, err = f.router.QueryOnce(f.ctx, "u", "call")
				} else {
					s, err = f.router.Submit(f.ctx, "u", "desk", call("call"))
				}
				if err != nil {
					t.Error(err)
				}
				once <- s
			}()
			eventually(t, func() bool { return f.router.Stats().QueryWaiters == 2 })
			if u := receive(t, f.ctx, l); u.GetCallQuery() == nil {
				t.Fatal(u)
			}
			send(t, f.ctx, l, m)
			a := waitAnswer(t, plain)
			must(t, a.err)
			s := waitAnswer(t, once)
			if s == nil {
				t.Fatal("missing answer subscription")
			}
			defer s.Close()
			assertBuffered(t, f, cost)
			u, err := s.Next(f.ctx)
			must(t, err)
			if u.GetResult() == nil || u != a.u {
				t.Fatal("coalesced queries and duplicate submissions must hand out the same owned result")
			}
			assertBuffered(t, f, 0)
		})
	}
}

func TestSharedPublishedUpdate(t *testing.T) {
	for _, mode := range []string{"consume", "close", "context"} {
		t.Run(mode, func(t *testing.T) {
			m, cost := sharedResult(t)
			f := setup(t, sharedBudget(cost))
			l := f.connect("boot")
			first, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
			must(t, err)
			defer first.Close()
			receive(t, f.ctx, l)
			drain(t, f.ctx, first, 2)
			ctx, cancel := context.WithCancel(f.ctx)
			defer cancel()
			second, err := f.router.Submit(ctx, "u", "desk", call("call"))
			must(t, err)
			defer second.Close()
			drain(t, f.ctx, second, 1)
			send(t, f.ctx, l, m)
			f.waitState("call", store.CallCompleted)
			assertBuffered(t, f, cost)
			u, err := first.Next(f.ctx)
			must(t, err)
			if u.GetResult() == nil {
				t.Fatal(u)
			}
			assertBuffered(t, f, cost)
			switch mode {
			case "consume":
				v, err := second.Next(f.ctx)
				must(t, err)
				if v != u {
					t.Fatal("subscribers did not share the owned result")
				}
			case "close":
				must(t, second.Close())
			case "context":
				cancel()
				eventually(t, func() bool { return f.router.Stats().Subscribers == 0 })
			}
			assertBuffered(t, f, 0)
		})
	}
}

func TestCancelAcceptedStoreFailure(t *testing.T) {
	f := setup(t)
	l := f.connect("boot")
	f.disconnect(l)
	s, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
	must(t, err)
	defer s.Close()
	drain(t, f.ctx, s, 1)
	f.waitState("call", store.CallAccepted)
	f.fault.FailAllWrites(store.ErrBusy)
	err = f.router.Cancel(f.ctx, "u", "call")
	f.fault.FailAllWrites(nil)
	if !errors.Is(err, store.ErrBusy) {
		t.Fatalf("Cancel lost the settle error: %v", err)
	}
	row, err := f.db.GetCall(f.ctx, "call")
	must(t, err)
	if row.State != store.CallAccepted || row.CancelRequestedAt != 0 || row.FinishedAt != 0 {
		t.Fatalf("failed cancel changed durable state: %+v", row)
	}
	u, err := f.router.Query(f.ctx, "u", "call")
	must(t, err)
	if u.GetAccepted() == nil {
		t.Fatalf("failed cancel reported a different state: %v", u)
	}
	assertBuffered(t, f, 0)
}

func TestSharedSettledUpdate(t *testing.T) {
	for _, failStore := range []bool{false, true} {
		t.Run(map[bool]string{false: "committed", true: "store_failure"}[failStore], func(t *testing.T) {
			m, cost := sharedResult(t)
			f := setup(t, sharedBudget(cost))
			l := f.connect("boot")
			watch, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
			must(t, err)
			defer watch.Close()
			receive(t, f.ctx, l)
			drain(t, f.ctx, watch, 2)
			type answer struct {
				s   *calls.Subscription
				err error
			}
			replied := make(chan answer, 1)
			go func() { s, err := f.router.QueryOnce(f.ctx, "u", "call"); replied <- answer{s, err} }()
			if q := receive(t, f.ctx, l); q.GetCallQuery() == nil {
				t.Fatal(q)
			}
			eventually(t, func() bool { return f.router.Stats().QueryWaiters == 1 })
			if failStore {
				f.fault.FailAllWrites(store.ErrBusy)
			}
			send(t, f.ctx, l, m)
			a := waitAnswer(t, replied)
			f.fault.FailAllWrites(nil)
			must(t, a.err)
			defer a.s.Close()
			assertBuffered(t, f, cost)
			u, err := watch.Next(f.ctx)
			must(t, err)
			assertBuffered(t, f, cost)
			v, err := a.s.Next(f.ctx)
			must(t, err)
			if u.GetResult() == nil || v != u {
				t.Fatal("watch and query must share the settled result")
			}
			assertBuffered(t, f, 0)
		})
	}
}
