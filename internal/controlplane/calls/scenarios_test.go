package calls_test

import (
	"errors"
	"fmt"
	"math"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/controlplane/calls"
	"github.com/EricWvi/circus/internal/controlplane/gateway"
	"github.com/EricWvi/circus/internal/controlplane/timers"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/store/sqlite"
	"github.com/EricWvi/circus/internal/store/storetest"
	"google.golang.org/protobuf/proto"
)

func TestReconnectUnackedAndCancelF1F3F9(t *testing.T) {
	f := setup(t)
	l := f.connect("boot")
	s, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
	must(t, err)
	defer s.Close()
	first := receive(t, f.ctx, l).GetDispatchCall()
	if first == nil {
		t.Fatal("missing initial dispatch")
	}
	persisted, err := f.db.GetCall(f.ctx, "call")
	must(t, err)
	if first.DispatchedAtUnixMs == 0 || first.DispatchedAtUnixMs != persisted.DispatchedAt {
		t.Fatalf("initial dispatch time %d does not match committed time %d", first.DispatchedAtUnixMs, persisted.DispatchedAt)
	}
	drain(t, f.ctx, s, 2)
	f.disconnect(l)
	must(t, f.router.Cancel(f.ctx, "u", "call"))
	f.advance(time.Second)
	l = f.open("boot")
	resent := receive(t, f.ctx, l).GetDispatchCall()
	if resent == nil || resent.TargetBootId != "boot" {
		t.Fatal(resent)
	}
	if resent.DispatchedAtUnixMs != first.DispatchedAtUnixMs {
		t.Fatalf("resend refreshed dispatch time: first=%d resent=%d now=%d", first.DispatchedAtUnixMs, resent.DispatchedAtUnixMs, f.clock.Now().UnixMilli())
	}
	if m := receive(t, f.ctx, l); m.GetCancelCall().GetCallId() != "call" {
		t.Fatal(m)
	}
	f.finishSync(l)
	f.running(l, "call")
	f.result(l, "call")
	nextResult(t, f.ctx, s)
	f.waitState("call", store.CallCompleted)
	if f.router.Stats().PendingBytes != 0 {
		t.Fatal("ack did not release content")
	}
}
func TestGraceExpiryAndRecoveryF2(t *testing.T) {
	f := setup(t)
	l := f.connect("boot")
	s, err := f.router.Submit(f.ctx, "u", "desk", call("running"))
	must(t, err)
	defer s.Close()
	receive(t, f.ctx, l)
	f.running(l, "running")
	drain(t, f.ctx, s, 3)
	f.disconnect(l)
	waiting, err := f.router.Submit(f.ctx, "u", "desk", call("waiting"))
	must(t, err)
	defer waiting.Close()
	drain(t, f.ctx, waiting, 1)
	f.advance(31 * time.Second)
	f.waitState("running", store.CallUnknown)
	f.waitState("waiting", store.CallOffline)
	u, err := s.Next(f.ctx)
	must(t, err)
	if u.GetUnknown() == nil {
		t.Fatal(u)
	}
	u, err = waiting.Next(f.ctx)
	must(t, err)
	if u.GetOffline() == nil {
		t.Fatal(u)
	}
	l = f.open("boot")
	if m := receive(t, f.ctx, l); m.GetCallQuery().GetCallId() != "running" {
		t.Fatal(m)
	}
	f.finishSync(l)
	f.result(l, "running")
	nextResult(t, f.ctx, s)
	f.waitState("running", store.CallCompleted)
}
func TestBootChangeLosesCallsAndJobsF4(t *testing.T) {
	f := setup(t)
	l := f.connect("old")
	s, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
	must(t, err)
	defer s.Close()
	receive(t, f.ctx, l)
	send(t, f.ctx, l, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_JobEvent{JobEvent: &circusv1.JobEvent{JobId: "012345abcdef", SessionId: "session", CallId: "call", Seq: 1, State: circusv1.JobEvent_STATE_RUNNING, OutputPath: "private output path"}}})
	eventually(t, func() bool { j, _ := f.db.ListJobs(f.ctx, store.JobFilter{Limit: 1}); return len(j) == 1 })
	f.disconnect(l)
	f.connect("new")
	f.waitState("call", store.CallLost)
	jobs, err := f.db.ListJobs(f.ctx, store.JobFilter{Limit: 1})
	must(t, err)
	if jobs[0].State != store.JobLost {
		t.Fatal(jobs)
	}
	again, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
	must(t, err)
	defer again.Close()
	u, err := again.Next(f.ctx)
	must(t, err)
	if u.GetLost() == nil {
		t.Fatal(u)
	}
}
func TestWaitingCallUsesNewBootAfterSnapshotF5(t *testing.T) {
	f := setup(t)
	l := f.connect("old")
	f.disconnect(l)
	s, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
	must(t, err)
	defer s.Close()
	drain(t, f.ctx, s, 1)
	l = f.open("new")
	f.finishSync(l)
	m := receive(t, f.ctx, l)
	if d := m.GetDispatchCall(); d == nil || d.TargetBootId != "new" {
		t.Fatal(m)
	}
	f.result(l, "call")
	nextResult(t, f.ctx, s)
}
func TestResendHorizonQueriesOnly(t *testing.T) {
	f := setup(t, func(c *calls.Config, _ *gateway.Config) { c.ResendHorizon = time.Second })
	l := f.connect("boot")
	s, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
	must(t, err)
	defer s.Close()
	receive(t, f.ctx, l)
	drain(t, f.ctx, s, 2)
	f.disconnect(l)
	f.advance(2 * time.Second)
	l = f.open("boot")
	if m := receive(t, f.ctx, l); m.GetCallQuery().GetCallId() != "call" {
		t.Fatal(m)
	}
	f.finishSync(l)
	eventually(t, func() bool { return f.router.Stats().PendingBytes == 0 })
}
func TestDuplicateAndOwnershipF8(t *testing.T) {
	f := setup(t)
	l := f.connect("boot")
	s, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
	must(t, err)
	defer s.Close()
	receive(t, f.ctx, l)
	dup, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
	must(t, err)
	defer dup.Close()
	if m := receive(t, f.ctx, l); m.GetCallQuery() == nil {
		t.Fatal(m)
	}
	mismatch := call("call")
	mismatch.GetBash().Command = "different"
	bad, err := f.router.Submit(f.ctx, "u", "desk", mismatch)
	must(t, err)
	defer bad.Close()
	u, err := bad.Next(f.ctx)
	must(t, err)
	if u.GetRejected().GetCode() != circusv1.CallUpdate_Rejected_CODE_CALL_MISMATCH {
		t.Fatal(u)
	}
	if _, err = f.router.Query(f.ctx, "another-user", "call"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal(err)
	}
	if err = f.router.Cancel(f.ctx, "another-user", "call"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal(err)
	}
	f.result(l, "call")
	nextResult(t, f.ctx, s)
	nextResult(t, f.ctx, dup)
	f.waitState("call", store.CallCompleted)
}
func TestCancelBeforeDispatchAndResultWinsF9(t *testing.T) {
	t.Run("before-dispatch", func(t *testing.T) {
		f := setup(t)
		l := f.connect("boot")
		f.disconnect(l)
		s, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
		must(t, err)
		defer s.Close()
		must(t, f.router.Cancel(f.ctx, "u", "call"))
		f.waitState("call", store.CallCancelled)
		l = f.connect("boot")
		_ = l
		if f.router.Stats().PendingCalls != 0 {
			t.Fatal("cancel retained request")
		}
	})
	t.Run("after-dispatch", func(t *testing.T) {
		f := setup(t)
		l := f.connect("boot")
		s, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
		must(t, err)
		defer s.Close()
		receive(t, f.ctx, l)
		must(t, f.router.Cancel(f.ctx, "u", "call"))
		if m := receive(t, f.ctx, l); m.GetCancelCall() == nil {
			t.Fatal(m)
		}
		f.result(l, "call")
		nextResult(t, f.ctx, s)
		must(t, f.router.Cancel(f.ctx, "u", "call"))
		f.waitState("call", store.CallCompleted)
	})
}
func TestFragmentedInputOutputAndOversizeF10F11(t *testing.T) {
	f := setup(t)
	l := f.connect("boot")
	v := call("call")
	v.Tool = &circusv1.DeviceCall_Write{Write: &circusv1.WriteRequest{Path: "file", Content: strings.Repeat("x", 128<<10)}}
	s, err := f.router.Submit(f.ctx, "u", "desk", v)
	must(t, err)
	defer s.Close()
	d := receive(t, f.ctx, l).GetDispatchCall()
	if d == nil || !proto.Equal(d.Call, v) {
		t.Fatal("fragmented dispatch changed")
	}
	output := strings.Repeat("result", 16<<10)
	send(t, f.ctx, l, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_Result{Result: &circusv1.DeviceResult{CallId: "call", Output: &circusv1.ToolOutput{Content: []*circusv1.ContentBlock{{Block: &circusv1.ContentBlock_Text{Text: output}}}}}}})
	u := nextResult(t, f.ctx, s)
	if u.GetResult().Output.Content[0].GetText() != output {
		t.Fatal("fragmented result changed")
	}
	v = call("large")
	v.GetBash().Command = strings.Repeat("x", 300<<10)
	tooLarge, err := f.router.Submit(f.ctx, "u", "desk", v)
	must(t, err)
	defer tooLarge.Close()
	u, err = tooLarge.Next(f.ctx)
	must(t, err)
	if !u.GetResult().GetOutput().GetIsError() {
		t.Fatal(u)
	}
	if _, err = f.db.GetCall(f.ctx, "large"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("oversize input persisted", err)
	}
}
func TestSubscriberOverflowF14(t *testing.T) {
	f := setup(t, func(c *calls.Config, _ *gateway.Config) { c.MaxUpdatesPerSubscription = 1 })
	l := f.connect("boot")
	s, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
	must(t, err)
	defer s.Close()
	receive(t, f.ctx, l)
	if _, err = s.Next(f.ctx); !errors.Is(err, calls.ErrOverflow) {
		t.Fatal(err)
	}
	f.result(l, "call")
	f.waitState("call", store.CallCompleted)
	if f.router.Stats().Subscribers != 0 {
		t.Fatal("overflow retained subscriber")
	}
}
func TestReplacementF16(t *testing.T) {
	f := setup(t)
	old := f.connect("old")
	s, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
	must(t, err)
	defer s.Close()
	receive(t, f.ctx, old)
	f.connect("new")
	f.waitState("call", store.CallLost)
	eventually(t, func() bool { return f.gateway.Stats().Connections == 1 })
	route, ok, err := f.registry.Lookup(f.ctx, "d")
	must(t, err)
	if !ok || route.Target.BootID != "new" {
		t.Fatal(route)
	}
}
func TestStoreFailuresF19(t *testing.T) {
	t.Run("submission", func(t *testing.T) {
		f := setup(t)
		f.connect("boot")
		f.fault.FailAllWrites(store.ErrBusy)
		s, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
		must(t, err)
		defer s.Close()
		u, err := s.Next(f.ctx)
		must(t, err)
		if u.GetRejected().GetCode() != circusv1.CallUpdate_Rejected_CODE_STORE_UNAVAILABLE {
			t.Fatal(u)
		}
		if _, err = f.db.GetCall(f.ctx, "call"); !errors.Is(err, store.ErrNotFound) {
			t.Fatal("submission persisted", err)
		}
	})
	t.Run("result", func(t *testing.T) {
		f := setup(t)
		l := f.connect("boot")
		s, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
		must(t, err)
		defer s.Close()
		receive(t, f.ctx, l)
		f.fault.FailAllWrites(store.ErrBusy)
		f.result(l, "call")
		nextResult(t, f.ctx, s)
		if f.state("call") != store.CallDispatched || f.router.Stats().StoreHealthy {
			t.Fatal("result failure concealed")
		}
		f.fault.FailAllWrites(nil)
		f.disconnect(l)
		l = f.open("boot")
		if m := receive(t, f.ctx, l); m.GetCallQuery() == nil {
			t.Fatal(m)
		}
		f.finishSync(l)
		f.result(l, "call")
		f.waitState("call", store.CallCompleted)
	})
}
func TestAckRejections(t *testing.T) {
	for _, tc := range []struct {
		name   string
		code   circusv1.Rejection_Code
		want   store.CallState
		worker circusv1.CallUpdate_Rejected_Code
	}{
		{"invalid", circusv1.Rejection_CODE_INVALID, store.CallRejected, circusv1.CallUpdate_Rejected_CODE_INVALID},
		{"mismatch", circusv1.Rejection_CODE_CALL_MISMATCH, store.CallRejected, circusv1.CallUpdate_Rejected_CODE_CALL_MISMATCH},
		{"shutdown", circusv1.Rejection_CODE_SHUTTING_DOWN, store.CallRejected, circusv1.CallUpdate_Rejected_CODE_DEVICE_SHUTTING_DOWN},
		{"boot", circusv1.Rejection_CODE_BOOT_MISMATCH, store.CallLost, 0},
		{"expired", circusv1.Rejection_CODE_EXPIRED, store.CallLost, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t)
			l := f.connect("boot")
			s, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
			must(t, err)
			defer s.Close()
			receive(t, f.ctx, l)
			drain(t, f.ctx, s, 2)
			send(t, f.ctx, l, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_CallAck{CallAck: &circusv1.CallAck{CallId: "call", Outcome: &circusv1.CallAck_Rejection{Rejection: &circusv1.Rejection{Code: tc.code, Message: "not persisted tool-like text"}}}}})
			f.waitState("call", tc.want)
			u, err := s.Next(f.ctx)
			must(t, err)
			if tc.worker != 0 && u.GetRejected().GetCode() != tc.worker {
				t.Fatal(u)
			}
			c, err := f.db.GetCall(f.ctx, "call")
			must(t, err)
			if strings.Contains(c.OutcomeReason, "tool-like") {
				t.Fatal(c)
			}
			if f.router.Stats().PendingBytes != 0 {
				t.Fatal("pending not released")
			}
		})
	}
}
func TestCompletedQueryUnavailable(t *testing.T) {
	f := setup(t)
	l := f.connect("boot")
	s, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
	must(t, err)
	defer s.Close()
	receive(t, f.ctx, l)
	f.result(l, "call")
	nextResult(t, f.ctx, s)
	f.waitState("call", store.CallCompleted)
	answer := make(chan error, 1)
	go func() { _, err := f.router.Query(f.ctx, "u", "call"); answer <- err }()
	if m := receive(t, f.ctx, l); m.GetCallQuery() == nil {
		t.Fatal(m)
	}
	send(t, f.ctx, l, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_CallStatus{CallStatus: &circusv1.CallStatus{CallId: "call", State: circusv1.CallStatus_STATE_LOST}}})
	if err = <-answer; !errors.Is(err, calls.ErrResultUnavailable) {
		t.Fatal(err)
	}
	f.waitState("call", store.CallCompleted)
	f.disconnect(l)
	if _, err = f.router.Query(f.ctx, "u", "call"); !errors.Is(err, calls.ErrDeviceOffline) {
		t.Fatal(err)
	}
}
func TestJobBeforeResultSnapshotAndSequenceF22(t *testing.T) {
	f := setup(t)
	l := f.connect("boot")
	j := &circusv1.JobEvent{JobId: "012345abcdef", SessionId: "session", CallId: "call", Seq: 1, State: circusv1.JobEvent_STATE_RUNNING}
	send(t, f.ctx, l, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_JobEvent{JobEvent: j}})
	eventually(t, func() bool { rows, _ := f.db.ListJobs(f.ctx, store.JobFilter{Limit: 1}); return len(rows) == 1 })
	newer := proto.Clone(j).(*circusv1.JobEvent)
	newer.Seq = 3
	newer.State = circusv1.JobEvent_STATE_EXITED
	send(t, f.ctx, l, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_JobEvent{JobEvent: newer}})
	if m := receive(t, f.ctx, l); m.GetListJobs() == nil {
		t.Fatal(m)
	}
	send(t, f.ctx, l, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_JobSnapshot{JobSnapshot: &circusv1.JobSnapshot{Seq: 2, Jobs: []*circusv1.JobEvent{j}}}})
	eventually(t, func() bool {
		rows, _ := f.db.ListJobs(f.ctx, store.JobFilter{Limit: 1})
		return len(rows) == 1 && rows[0].State == store.JobExited && rows[0].LastSeq == 3
	})
	send(t, f.ctx, l, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_JobSnapshot{JobSnapshot: &circusv1.JobSnapshot{Seq: math.MaxUint64}}})
	eventually(t, func() bool { return f.registry.Stats().Connections == 0 })
}
func TestStaleTimerClaimHasNoEffects(t *testing.T) {
	f := setup(t)
	l := f.connect("boot")
	s, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
	must(t, err)
	defer s.Close()
	receive(t, f.ctx, l)
	rows, err := f.db.ListTimers(f.ctx, store.TimerFilter{Kind: timers.CallWatchdog, Limit: 1})
	must(t, err)
	claimed, err := f.db.ClaimDueTimers(f.ctx, rows[0].DueAt, "other", 10000, 1)
	must(t, err)
	must(t, f.db.SetTimer(f.ctx, timers.CallWatchdog, "call", rows[0].DueAt+1))
	before, err := f.db.GetCall(f.ctx, "call")
	must(t, err)
	must(t, f.router.HandleTimer(f.ctx, claimed[0]))
	after, err := f.db.GetCall(f.ctx, "call")
	must(t, err)
	if !equalCall(before, after) {
		t.Fatal("stale timer changed state")
	}
	ts, err := f.db.ListTimers(f.ctx, store.TimerFilter{Kind: timers.CallWatchdog, SubjectID: "call", Limit: 1})
	must(t, err)
	if len(ts) != 1 || ts[0].DueAt != rows[0].DueAt+1 || ts[0].Token != "" || ts[0].ClaimedBy != "" || ts[0].Attempts != 0 {
		t.Fatalf("stale timer overwrote replacement: %+v", ts)
	}
}

// equalCall compares every metadata column of two call rows.
func equalCall(a, b store.Call) bool {
	return reflect.DeepEqual(a, b)
}
func TestCrashAfterDispatchCommitF7(t *testing.T) {
	f := setup(t)
	f.connect("boot")
	f.haltTimers()
	f.fault.CrashAfterWrite(4, nil)
	_, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
	if !errors.Is(err, calls.ErrStoreUnavailable) {
		t.Fatal(err)
	}
	c, err := f.db.GetCall(f.ctx, "call")
	must(t, err)
	if c.State != store.CallDispatched || c.DispatchedBootID != "boot" {
		t.Fatal(c)
	}
	f.restart()
	l := f.open("boot")
	if m := receive(t, f.ctx, l); m.GetCallQuery().GetCallId() != "call" {
		t.Fatal(m)
	}
	f.finishSync(l)
	send(t, f.ctx, l, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_CallStatus{CallStatus: &circusv1.CallStatus{CallId: "call", State: circusv1.CallStatus_STATE_LOST}}})
	f.waitState("call", store.CallLost)
}

func (f *fixture) restart() {
	f.haltTimers()
	for _, l := range f.links {
		_ = l.Close()
	}
	must(f.t, f.gateway.Shutdown(f.ctx))
	f.server.Close()
	must(f.t, f.router.Close(f.ctx))
	must(f.t, f.db.Close())
	var err error
	f.db, err = sqlite.Open(f.ctx, f.dbPath, sqlite.Options{})
	must(f.t, err)
	f.fault = storetest.NewFaulty(f.db)
	f.routerConfig.Store = f.fault
	f.router, err = calls.New(f.routerConfig)
	must(f.t, err)
	f.hooks = &hookCounter{Hooks: f.router}
	f.gatewayConfig.Hooks = f.hooks
	f.gateway, err = gateway.New(f.gatewayConfig)
	must(f.t, err)
	f.server = httptest.NewServer(f.gateway)
	f.startTimers()
}
func TestRestartAndUndispatchedResubmissionF6(t *testing.T) {
	f := setup(t)
	l := f.connect("boot")
	active, err := f.router.Submit(f.ctx, "u", "desk", call("active"))
	must(t, err)
	receive(t, f.ctx, l)
	drain(t, f.ctx, active, 2)
	f.disconnect(l)
	waiting, err := f.router.Submit(f.ctx, "u", "desk", call("waiting"))
	must(t, err)
	drain(t, f.ctx, waiting, 1)
	f.restart()
	f.waitState("waiting", store.CallLost)
	f.waitState("active", store.CallDispatched)
	f.advance(time.Millisecond)
	again, err := f.router.Submit(f.ctx, "u", "desk", call("waiting"))
	must(t, err)
	defer again.Close()
	c, err := f.db.GetCall(f.ctx, "waiting")
	must(t, err)
	if c.State != store.CallAccepted || c.FinishedAt != 0 || c.OutcomeReason != "" || c.CancelRequestedAt != 0 || c.DispatchedBootID != "" || c.AckedAt != 0 {
		t.Fatal(c)
	}
	l = f.open("boot")
	if m := receive(t, f.ctx, l); m.GetCallQuery().GetCallId() != "active" {
		t.Fatal(m)
	}
	f.finishSync(l)
	if m := receive(t, f.ctx, l); m.GetDispatchCall().GetCall().GetCallId() != "waiting" {
		t.Fatal(m)
	}
	f.result(l, "waiting")
	nextResult(t, f.ctx, again)
	f.result(l, "active")
	f.waitState("active", store.CallCompleted)
}
func TestNeverConnectedInvalidAndUnknownDevice(t *testing.T) {
	f := setup(t)
	s, err := f.router.Submit(f.ctx, "u", "desk", call("offline"))
	must(t, err)
	u, err := s.Next(f.ctx)
	must(t, err)
	if u.GetOffline() == nil {
		t.Fatal(u)
	}
	for _, id := range []string{"", "bad/id", strings.Repeat("x", 129)} {
		v := call(id)
		sub, err := f.router.Submit(f.ctx, "u", "desk", v)
		must(t, err)
		u, err := sub.Next(f.ctx)
		must(t, err)
		if u.GetRejected().GetCode() != circusv1.CallUpdate_Rejected_CODE_INVALID {
			t.Fatal(u)
		}
	}
	s, err = f.router.Submit(f.ctx, "u", "unknown", call("unknown"))
	must(t, err)
	u, err = s.Next(f.ctx)
	must(t, err)
	if u.GetRejected().GetCode() != circusv1.CallUpdate_Rejected_CODE_UNKNOWN_DEVICE {
		t.Fatal(u)
	}
}
func TestPendingAdmissionLimits(t *testing.T) {
	for _, name := range []string{"bytes", "count"} {
		t.Run(name, func(t *testing.T) {
			f := setup(t, func(c *calls.Config, _ *gateway.Config) {
				if name == "bytes" {
					c.Pending.MaxBytes = 1
				} else {
					c.Pending.MaxCalls = 1
				}
			})
			l := f.connect("boot")
			if name == "count" {
				s, err := f.router.Submit(f.ctx, "u", "desk", call("first"))
				must(t, err)
				defer s.Close()
				receive(t, f.ctx, l)
			}
			s, err := f.router.Submit(f.ctx, "u", "desk", call("full"))
			must(t, err)
			defer s.Close()
			u, err := s.Next(f.ctx)
			must(t, err)
			if u.GetRejected().GetCode() != circusv1.CallUpdate_Rejected_CODE_OVERLOADED {
				t.Fatal(u)
			}
			if _, err = f.db.GetCall(f.ctx, "full"); !errors.Is(err, store.ErrNotFound) {
				t.Fatal("overload persisted", err)
			}
		})
	}
}
func TestWatchdogRetentionCutoffAndRunningRearm(t *testing.T) {
	t.Run("offline-cutoff", func(t *testing.T) {
		f := setup(t)
		l := f.connect("boot")
		s, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
		must(t, err)
		defer s.Close()
		receive(t, f.ctx, l)
		drain(t, f.ctx, s, 2)
		f.disconnect(l)
		f.advance(31 * time.Minute)
		f.runner.Wake()
		f.waitState("call", store.CallLost)
		if c, _ := f.db.GetCall(f.ctx, "call"); c.OutcomeReason != "RESULT_RETENTION_EXPIRED" {
			t.Fatal(c)
		}
	})
	t.Run("running-rearm", func(t *testing.T) {
		f := setup(t)
		l := f.connect("boot")
		s, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
		must(t, err)
		defer s.Close()
		receive(t, f.ctx, l)
		f.running(l, "call")
		drain(t, f.ctx, s, 3)
		rows, err := f.db.ListTimers(f.ctx, store.TimerFilter{Kind: "call_watchdog", SubjectID: "call", Limit: 1})
		must(t, err)
		f.advance(time.Duration(rows[0].DueAt-f.clock.Now().UnixMilli()) * time.Millisecond)
		if m := receive(t, f.ctx, l); m.GetCallQuery() == nil {
			t.Fatal(m)
		}
		f.running(l, "call")
		eventually(t, func() bool {
			ts, _ := f.db.ListTimers(f.ctx, store.TimerFilter{Kind: "call_watchdog", SubjectID: "call", Limit: 1})
			return len(ts) == 1 && ts[0].DueAt > f.clock.Now().UnixMilli()
		})
	})
}

func TestConcurrentCancelAndResultF9(t *testing.T) {
	for i := range 8 {
		t.Run(fmt.Sprintf("race-%d", i), func(t *testing.T) {
			f := setup(t)
			l := f.connect("boot")
			s, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
			must(t, err)
			defer s.Close()
			receive(t, f.ctx, l)
			cancelled := make(chan error, 1)
			go func() { cancelled <- f.router.Cancel(f.ctx, "u", "call") }()
			f.result(l, "call")
			must(t, <-cancelled)
			nextResult(t, f.ctx, s)
			f.waitState("call", store.CallCompleted)
		})
	}
}
