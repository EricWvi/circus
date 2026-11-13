package calls_test

import (
	"context"
	"errors"
	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/controlplane/calls"
	"github.com/EricWvi/circus/internal/controlplane/gateway"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/testutil/wait"
	"github.com/EricWvi/circus/internal/wire"
	"io"
	"sync"
	"testing"
	"time"
)

// observer decorates the public gateway consumer seam. It observes completion
// without manufacturing a router state or bypassing the real WebSocket link.
// Its mailbox holds at most eight hook completions.
// A gate (set before setup, or later with arm) holds the next OnConnected once.
type observer struct {
	gateway.Hooks
	seen    chan string
	mu      sync.Mutex
	gate    chan struct{}
	entered chan struct{}
}

// arm gates the next OnConnected; entered closes when it is held.
func (o *observer) arm() (entered, gate chan struct{}) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.entered, o.gate = make(chan struct{}), make(chan struct{})
	return o.entered, o.gate
}
func (o *observer) SetHooks(h gateway.Hooks) { o.Hooks = h }
func (o *observer) OnConnected(ctx context.Context, c gateway.Connection) error {
	o.mu.Lock()
	gate, entered := o.gate, o.entered
	o.gate, o.entered = nil, nil
	o.mu.Unlock()
	if gate != nil {
		close(entered)
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return o.Hooks.OnConnected(ctx, c)
}
func (o *observer) OnCallAck(ctx context.Context, c gateway.Connection, v *circusv1.CallAck) error {
	err := o.Hooks.OnCallAck(ctx, c, v)
	o.seen <- "ack"
	return err
}
func (o *observer) OnCallStatus(ctx context.Context, c gateway.Connection, v *circusv1.CallStatus) error {
	err := o.Hooks.OnCallStatus(ctx, c, v)
	o.seen <- "status"
	return err
}
func (o *observer) OnDeviceResult(ctx context.Context, c gateway.Connection, v *circusv1.DeviceResult) error {
	err := o.Hooks.OnDeviceResult(ctx, c, v)
	o.seen <- "result"
	return err
}
func (o *observer) wait(t testing.TB, ctx context.Context, want string) {
	t.Helper()
	select {
	case got := <-o.seen:
		if got != want {
			t.Fatalf("hook %s, want %s", got, want)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}
func prepare(t *testing.T, state store.CallState) (*fixture, *wire.Link, *observer) {
	f, l, o, _ := prepareSub(t, state)
	return f, l, o
}

// prepareSub drives one call into state through public interfaces and returns
// its drained submit subscription.
func prepareSub(t *testing.T, state store.CallState) (*fixture, *wire.Link, *observer, *calls.Subscription) {
	o := &observer{seen: make(chan string, 8)}
	var entered, gate chan struct{}
	if state == store.CallAccepted {
		entered, gate = o.arm()
	}
	f := setup(t, func(_ *calls.Config, g *gateway.Config) { g.Hooks = o })
	var l *wire.Link
	var sub *calls.Subscription
	var err error
	switch state {
	case store.CallAccepted:
		l = f.open("boot")
		wait.Recv(t, entered)
		sub, err = f.router.Submit(f.ctx, "u", "desk", call("call"))
		must(t, err)
		drain(t, f.ctx, sub, 1)
		t.Cleanup(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err := f.router.Close(cleanup)
			close(gate)
			must(t, err)
		})
	case store.CallOffline:
		sub, err = f.router.Submit(f.ctx, "u", "desk", call("call"))
		must(t, err)
		drain(t, f.ctx, sub, 1)
		l = f.connect("boot")
	case store.CallCancelled:
		l = f.connect("boot")
		f.disconnect(l)
		sub, err = f.router.Submit(f.ctx, "u", "desk", call("call"))
		must(t, err)
		must(t, f.router.Cancel(f.ctx, "u", "call"))
		drain(t, f.ctx, sub, 2)
		l = f.connect("boot")
	default:
		l = f.connect("boot")
		sub, err = f.router.Submit(f.ctx, "u", "desk", call("call"))
		must(t, err)
		receive(t, f.ctx, l)
		drain(t, f.ctx, sub, 2)
		switch state {
		case store.CallRunning, store.CallUnknown:
			f.running(l, "call")
			o.wait(t, f.ctx, "ack")
			drain(t, f.ctx, sub, 1)
			if state == store.CallUnknown {
				f.disconnect(l)
				f.advance(31 * time.Second)
				f.waitState("call", store.CallUnknown)
				drain(t, f.ctx, sub, 1)
				l = f.open("boot")
				if m := receive(t, f.ctx, l); m.GetCallQuery() == nil {
					t.Fatal(m)
				}
				f.finishSync(l)
			}
		case store.CallCompleted:
			f.result(l, "call")
			o.wait(t, f.ctx, "result")
			nextResult(t, f.ctx, sub)
		case store.CallRejected:
			send(t, f.ctx, l, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_CallAck{CallAck: &circusv1.CallAck{CallId: "call", Outcome: &circusv1.CallAck_Rejection{Rejection: &circusv1.Rejection{Code: circusv1.Rejection_CODE_INVALID}}}}})
			o.wait(t, f.ctx, "ack")
			drain(t, f.ctx, sub, 1)
		case store.CallLost:
			f.disconnect(l)
			l = f.connect("new")
			drain(t, f.ctx, sub, 1)
		}
	}
	f.waitState("call", state)
	t.Cleanup(func() { _ = sub.Close() })
	return f, l, o, sub
}

// quiet asserts that no update is delivered within a short real-time window;
// an ended subscription (io.EOF after its terminal update) is also quiet.
func quiet(t testing.TB, ctx context.Context, s *calls.Subscription) {
	t.Helper()
	short, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if u, err := s.Next(short); err == nil {
		t.Fatalf("unexpected update %v", u)
	} else if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
}

// delivered reports whether u is the worker update for entering state.
func delivered(state store.CallState, u *circusv1.CallUpdate) bool {
	switch state {
	case store.CallRunning:
		return u.GetDispatched() != nil
	case store.CallCompleted:
		return u.GetResult() != nil
	case store.CallLost:
		return u.GetLost() != nil
	case store.CallRejected:
		return u.GetRejected() != nil
	case store.CallCancelled:
		return u.GetCancelled() != nil
	}
	return false
}
func TestStateEventTable(t *testing.T) {
	// Independent specification, including invalid edges: no store.CanTransition
	// or implementation transition helpers are consulted by this test.
	for _, row := range []struct {
		state store.CallState
		want  []store.CallState
	}{
		{store.CallAccepted, []store.CallState{store.CallAccepted, store.CallAccepted, store.CallAccepted, store.CallAccepted, store.CallCancelled}},
		{store.CallDispatched, []store.CallState{store.CallRunning, store.CallCompleted, store.CallLost, store.CallRejected, store.CallDispatched}},
		{store.CallRunning, []store.CallState{store.CallRunning, store.CallCompleted, store.CallLost, store.CallRunning, store.CallRunning}},
		{store.CallUnknown, []store.CallState{store.CallRunning, store.CallCompleted, store.CallLost, store.CallUnknown, store.CallUnknown}},
		{store.CallCompleted, []store.CallState{store.CallCompleted, store.CallCompleted, store.CallCompleted, store.CallCompleted, store.CallCompleted}},
		{store.CallOffline, []store.CallState{store.CallOffline, store.CallOffline, store.CallOffline, store.CallOffline, store.CallOffline}},
		{store.CallCancelled, []store.CallState{store.CallCancelled, store.CallCancelled, store.CallCancelled, store.CallCancelled, store.CallCancelled}},
		{store.CallRejected, []store.CallState{store.CallRejected, store.CallRejected, store.CallRejected, store.CallRejected, store.CallRejected}},
		{store.CallLost, []store.CallState{store.CallLost, store.CallLost, store.CallLost, store.CallLost, store.CallLost}},
	} {
		for _, variant := range []struct {
			name   string
			column int
		}{{"ack-running", 0}, {"ack-completed", 1}, {"query-lost", 2}, {"ack-rejected", 3}, {"cancel", 4}, {"result", 1}, {"status-running", 0}, {"status-completed", 1}, {"ack-lost", 2}} {
			i, event := variant.column, variant.name
			t.Run(string(row.state)+"/"+event, func(t *testing.T) {
				f, l, o, sub := prepareSub(t, row.state)
				before, err := f.db.GetCall(f.ctx, "call")
				must(t, err)
				// A later clock exposes any overwritten first-ack/finish time.
				f.clock.Advance(time.Millisecond)
				switch event {
				case "result":
					f.result(l, "call")
					o.wait(t, f.ctx, "result")
				case "status-running", "status-completed":
					status := &circusv1.CallStatus{CallId: "call", State: circusv1.CallStatus_STATE_RUNNING}
					if event == "status-completed" {
						status.State = circusv1.CallStatus_STATE_COMPLETED
						status.Result = &circusv1.DeviceResult{CallId: "call", Output: &circusv1.ToolOutput{}}
					}
					send(t, f.ctx, l, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_CallStatus{CallStatus: status}})
					o.wait(t, f.ctx, "status")
				case "ack-lost":
					send(t, f.ctx, l, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_CallAck{CallAck: &circusv1.CallAck{CallId: "call", Outcome: &circusv1.CallAck_Status{Status: &circusv1.CallStatus{CallId: "call", State: circusv1.CallStatus_STATE_LOST}}}}})
					o.wait(t, f.ctx, "ack")
				case "cancel":
					must(t, f.router.Cancel(f.ctx, "u", "call"))
				case "ack-running":
					send(t, f.ctx, l, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_CallAck{CallAck: &circusv1.CallAck{CallId: "call", Outcome: &circusv1.CallAck_Status{Status: &circusv1.CallStatus{CallId: "call", State: circusv1.CallStatus_STATE_RUNNING}}}}})
					o.wait(t, f.ctx, "ack")
				case "ack-completed":
					send(t, f.ctx, l, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_CallAck{CallAck: &circusv1.CallAck{CallId: "call", Outcome: &circusv1.CallAck_Status{Status: &circusv1.CallStatus{CallId: "call", State: circusv1.CallStatus_STATE_COMPLETED, Result: &circusv1.DeviceResult{CallId: "call", Output: &circusv1.ToolOutput{}}}}}}})
					o.wait(t, f.ctx, "ack")
				case "query-lost":
					send(t, f.ctx, l, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_CallStatus{CallStatus: &circusv1.CallStatus{CallId: "call", State: circusv1.CallStatus_STATE_LOST}}})
					o.wait(t, f.ctx, "status")
				case "ack-rejected":
					send(t, f.ctx, l, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_CallAck{CallAck: &circusv1.CallAck{CallId: "call", Outcome: &circusv1.CallAck_Rejection{Rejection: &circusv1.Rejection{Code: circusv1.Rejection_CODE_INVALID}}}}})
					o.wait(t, f.ctx, "ack")
				}
				after, err := f.db.GetCall(f.ctx, "call")
				must(t, err)
				if after.State != row.want[i] {
					t.Fatalf("state %s, want %s", after.State, row.want[i])
				}
				if event == "cancel" && (row.state == store.CallDispatched || row.state == store.CallRunning || row.state == store.CallUnknown) {
					// Cancel intent is recorded once; nothing else changes.
					if after.CancelRequestedAt == 0 {
						t.Fatal("cancel intent not recorded")
					}
					after.CancelRequestedAt = before.CancelRequestedAt
				}
				if row.want[i] == row.state && !equalCall(before, after) {
					t.Fatalf("invalid/duplicate event changed metadata: before %+v after %+v", before, after)
				}
				if row.want[i] != row.state {
					u, err := sub.Next(f.ctx)
					must(t, err)
					if !delivered(row.want[i], u) {
						t.Fatalf("subscriber got %v for %s", u, row.want[i])
					}
				}
				quiet(t, f.ctx, sub)
				if row.want[i] != row.state && after.DispatchedBootID != before.DispatchedBootID {
					t.Fatalf("transition rewrote dispatch evidence: %+v", after)
				}
				if s := f.router.Stats(); after.State.Terminal() && s.PendingCalls != 0 {
					t.Fatalf("terminal call retains pending content: %+v", s)
				}
			})
		}
	}
}

func TestConnectionEventTable(t *testing.T) {
	for _, state := range []store.CallState{store.CallAccepted, store.CallDispatched, store.CallRunning, store.CallUnknown, store.CallCompleted, store.CallOffline, store.CallCancelled, store.CallRejected, store.CallLost} {
		for _, event := range []string{"disconnect", "grace-expire", "reconnect-same", "reconnect-new", "watchdog"} {
			t.Run(string(state)+"/"+event, func(t *testing.T) {
				var f *fixture
				var l *wire.Link
				if state == store.CallAccepted {
					f = setup(t)
					l = f.connect("boot")
					f.disconnect(l)
					s, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
					must(t, err)
					drain(t, f.ctx, s, 1)
				} else {
					f, l, _ = prepare(t, state)
				}
				before, err := f.db.GetCall(f.ctx, "call")
				must(t, err)
				want := state
				switch event {
				case "disconnect":
					if state != store.CallAccepted {
						f.disconnect(l)
					}
				case "grace-expire":
					if state != store.CallAccepted {
						f.disconnect(l)
					}
					f.advance(31 * time.Second)
					switch state {
					case store.CallAccepted:
						want = store.CallOffline
					case store.CallDispatched, store.CallRunning:
						want = store.CallUnknown
					}
					f.waitState("call", want)
				case "reconnect-same", "reconnect-new":
					boot := "boot"
					if state != store.CallAccepted {
						route, _, err := f.registry.Lookup(f.ctx, "d")
						must(t, err)
						boot = route.Target.BootID
						f.disconnect(l)
					}
					if event == "reconnect-new" {
						boot = "nextboot"
					}
					l = f.open(boot)
					if state == store.CallDispatched || state == store.CallRunning || state == store.CallUnknown {
						if event == "reconnect-new" {
							want = store.CallLost
						} else {
							m := receive(t, f.ctx, l)
							if state == store.CallDispatched {
								if m.GetDispatchCall() == nil {
									t.Fatal(m)
								}
							} else if m.GetCallQuery() == nil {
								t.Fatal(m)
							}
						}
					}
					f.finishSync(l)
					if state == store.CallAccepted {
						if m := receive(t, f.ctx, l); m.GetDispatchCall() == nil {
							t.Fatal(m)
						}
						want = store.CallDispatched
					}
					f.waitState("call", want)
				case "watchdog":
					if state == store.CallDispatched || state == store.CallRunning || state == store.CallUnknown {
						rows, err := f.db.ListTimers(f.ctx, store.TimerFilter{Kind: "call_watchdog", SubjectID: "call", Limit: 1})
						must(t, err)
						if len(rows) != 1 {
							t.Fatal(rows)
						}
						f.advance(time.Duration(rows[0].DueAt-f.clock.Now().UnixMilli()) * time.Millisecond)
						if m := receive(t, f.ctx, l); m.GetCallQuery().GetCallId() != "call" {
							t.Fatal(m)
						}
					} else {
						if state != store.CallAccepted {
							must(t, f.db.SetTimer(f.ctx, "call_watchdog", "call", f.clock.Now().UnixMilli()))
							f.runner.Wake()
							eventually(t, func() bool {
								rows, _ := f.db.ListTimers(f.ctx, store.TimerFilter{Kind: "call_watchdog", SubjectID: "call", Limit: 1})
								return len(rows) == 0
							})
						}
					}
				}
				after, err := f.db.GetCall(f.ctx, "call")
				must(t, err)
				if after.State != want {
					t.Fatalf("got %s want %s", after.State, want)
				}
				if want == state && !equalCall(before, after) {
					t.Fatalf("non-transition changed call metadata: %+v -> %+v", before, after)
				}
			})
		}
	}
}
