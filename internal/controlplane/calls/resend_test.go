package calls_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/controlplane/bus"
	"github.com/EricWvi/circus/internal/controlplane/calls"
	"github.com/EricWvi/circus/internal/controlplane/gateway"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/wire"
	"github.com/EricWvi/circus/internal/wirepolicy"
)

// switchBus refuses DispatchCall sends with wire.ErrBusy while busy is set.
type switchBus struct {
	bus.Bus
	busy *atomic.Bool
}

func (b switchBus) Send(ctx context.Context, target bus.Target, msg *circusv1.DaemonDownlink) error {
	if msg.GetDispatchCall() != nil && b.busy.Load() {
		return wire.ErrBusy
	}
	return b.Bus.Send(ctx, target, msg)
}

// A resend may follow an execution of the first DispatchCall, so a full
// link refusing the resend must not settle the call as Rejected{OVERLOADED}
// the way a refused first send does: the call stays dispatched, the sync
// fails and a later reconnect resends again.
func TestResendOnBusyLinkKeepsCallDispatched(t *testing.T) {
	busy := new(atomic.Bool)
	f := setup(t, func(c *calls.Config, _ *gateway.Config) { c.Bus = switchBus{c.Bus, busy} })
	l := f.connect("boot")
	s, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
	must(t, err)
	defer s.Close()
	if receive(t, f.ctx, l).GetDispatchCall() == nil {
		t.Fatal("missing first dispatch")
	}
	drain(t, f.ctx, s, 2)
	f.disconnect(l)
	busy.Store(true)
	f.open("boot") // the sync's resend is refused; the sync fails and closes the link
	eventually(t, func() bool { return f.registry.Stats().Connections == 0 })
	if c, err := f.db.GetCall(f.ctx, "call"); err != nil || c.State != store.CallDispatched {
		t.Fatalf("a refused resend settled the call: %+v %v", c, err)
	}
	busy.Store(false)
	l = f.open("boot")
	if m := receive(t, f.ctx, l); m.GetDispatchCall().GetCall().GetCallId() != "call" {
		t.Fatal(m)
	}
	f.finishSync(l)
	f.result(l, "call")
	nextResult(t, f.ctx, s)
	f.waitState("call", store.CallCompleted)
}

// evidenceBus checks commit-before-send where it matters, at the bus: when
// a DispatchCall is handed over, the call's durable row must already name
// its target boot. (A failing sync closes the link, which usually discards
// an early DispatchCall, so the wire alone would hide the violation.)
// busyNext, when set, refuses the next DispatchCall with wire.ErrBusy (as
// a full send pool would); refused reports which call it was.
type evidenceBus struct {
	bus.Bus
	db       store.Store
	report   func(string)
	busyNext atomic.Bool
	refused  func(id, boot string)
}

func (b *evidenceBus) Send(ctx context.Context, target bus.Target, msg *circusv1.DaemonDownlink) error {
	if d := msg.GetDispatchCall(); d != nil {
		if b.busyNext.CompareAndSwap(true, false) {
			if b.refused != nil {
				b.refused(d.GetCall().GetCallId(), d.TargetBootId)
			}
			return wire.ErrBusy
		}
		c, err := b.db.GetCall(context.WithoutCancel(ctx), d.GetCall().GetCallId())
		active := c.State == store.CallDispatched || c.State == store.CallRunning || c.State == store.CallUnknown
		if err != nil || !active || c.DispatchedBootID != d.TargetBootId {
			b.report(fmt.Sprintf("DispatchCall for %s sent to boot %s while its row is %s on boot %q (%v): send before commit", d.GetCall().GetCallId(), d.TargetBootId, c.State, c.DispatchedBootID, err))
		}
	}
	return b.Bus.Send(ctx, target, msg)
}

// The reconnect sync dispatches accepted calls in their own transaction. A
// failed write anywhere in that sync must not let a DispatchCall out ahead
// of its durable dispatch evidence (commit before send).
func TestSyncDispatchFailedCommitSendsNothing(t *testing.T) {
	prepare := func(t *testing.T) *fixture {
		evidence := &evidenceBus{report: func(msg string) { t.Error(msg) }}
		f := setup(t, func(c *calls.Config, _ *gateway.Config) { evidence.Bus = c.Bus; c.Bus = evidence })
		evidence.db = f.db
		l := f.connect("boot")
		f.disconnect(l)
		s, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
		must(t, err)
		t.Cleanup(func() { _ = s.Close() })
		f.haltTimers()
		return f
	}
	writes := func() int {
		f := prepare(t)
		start := f.fault.Writes()
		l := f.open("boot")
		f.finishSync(l)
		if m := receive(t, f.ctx, l); m.GetDispatchCall() == nil {
			t.Fatal(m)
		}
		return f.fault.Writes() - start
	}()
	if writes < 3 {
		t.Fatalf("unexpectedly few write points: %d", writes)
	}
	for n := 1; n <= writes; n++ {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			f := prepare(t)
			f.fault.FailWrite(n, store.ErrBusy)
			l := f.open("boot")
			for {
				m, ok := tryReceiveOrClosed(f.ctx, l, 100*time.Millisecond)
				if !ok {
					break // quiet, or closed because the sync failed
				}
				if m.GetListJobs() != nil {
					// The failed sync may already be closing the link.
					trySnapshot(f.ctx, l)
				}
				if m.GetDispatchCall() != nil {
					if c, err := f.db.GetCall(f.ctx, "call"); err != nil || c.State != store.CallDispatched || c.DispatchedBootID != "boot" {
						t.Fatalf("DispatchCall sent without committed dispatch evidence: %+v %v", c, err)
					}
				}
			}
		})
	}
}

// trySnapshot answers ListJobs with an empty snapshot; the link may already
// be closing because the sync failed, so a refused send is not an error.
func trySnapshot(ctx context.Context, l *wire.Link) {
	snapshot := &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_JobSnapshot{JobSnapshot: &circusv1.JobSnapshot{}}}
	if class, key, err := wirepolicy.Classify(snapshot); err == nil {
		_ = l.Send(ctx, snapshot, class, key)
	}
}

// tryReceiveOrClosed returns the next downlink message, or false once the
// link is quiet for wait or closed.
func tryReceiveOrClosed(ctx context.Context, l *wire.Link, wait time.Duration) (*circusv1.DaemonDownlink, bool) {
	short, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	m, err := l.Receive(short)
	if err != nil {
		return nil, false
	}
	return m.(*circusv1.DaemonDownlink), true
}

// Transition table: an ack rejection applies only to a dispatched call.
// Once the daemon reported the call running (or it went unknown), a later
// rejection for it is an unlisted pair and changes nothing.
func TestAckRejectionIgnoredOnceRunning(t *testing.T) {
	for _, code := range []circusv1.Rejection_Code{circusv1.Rejection_CODE_INVALID, circusv1.Rejection_CODE_EXPIRED, circusv1.Rejection_CODE_SHUTTING_DOWN} {
		t.Run(code.String(), func(t *testing.T) {
			f := setup(t)
			l := f.connect("boot")
			s, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
			must(t, err)
			defer s.Close()
			receive(t, f.ctx, l)
			f.running(l, "call")
			before := f.hooks.acks.Load()
			send(t, f.ctx, l, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_CallAck{CallAck: &circusv1.CallAck{CallId: "call", Outcome: &circusv1.CallAck_Rejection{Rejection: &circusv1.Rejection{Code: code}}}}})
			eventually(t, func() bool { return f.hooks.acks.Load() > before })
			if st := f.state("call"); st != store.CallRunning {
				t.Fatalf("rejection %s moved a running call to %s", code, st)
			}
			f.result(l, "call")
			f.waitState("call", store.CallCompleted)
		})
	}
}

// The watchdog compares the call's dispatch boot with the device's current
// boot: when a reconnect recorded a new boot but its loss batches were
// interrupted (a crash between LoseCalls transactions), the watchdog loses
// the old-boot call instead of re-arming forever.
func TestWatchdogLosesCallOfReplacedBoot(t *testing.T) {
	f := setup(t)
	l := f.connect("old")
	s, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
	must(t, err)
	defer s.Close()
	receive(t, f.ctx, l)
	f.running(l, "call")
	// The new boot's first sync transaction committed; its LoseCalls batch
	// for this call did not (the old route is kept so no grace interferes).
	_, err = f.db.SetConnected(f.ctx, store.KindDevice, "d", "new", f.clock.Now().UnixMilli())
	must(t, err)
	rows, err := f.db.ListTimers(f.ctx, store.TimerFilter{Kind: "call_watchdog", SubjectID: "call", Limit: 1})
	must(t, err)
	f.advance(time.Duration(rows[0].DueAt-f.clock.Now().UnixMilli()) * time.Millisecond)
	f.waitState("call", store.CallLost)
	if c, _ := f.db.GetCall(f.ctx, "call"); c.OutcomeReason != "DAEMON_RESTARTED" {
		t.Fatal(c)
	}
}
