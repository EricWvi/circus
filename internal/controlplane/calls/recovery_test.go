package calls_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/controlplane/bus"
	"github.com/EricWvi/circus/internal/controlplane/calls"
	"github.com/EricWvi/circus/internal/controlplane/gateway"
	"github.com/EricWvi/circus/internal/store"
)

func jobEvent(id string, seq uint64) *circusv1.DaemonUplink {
	return &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_JobEvent{JobEvent: &circusv1.JobEvent{JobId: id, SessionId: "session", CallId: "call", Seq: seq, State: circusv1.JobEvent_STATE_RUNNING}}}
}
func (f *fixture) jobs() map[string]store.Job {
	f.t.Helper()
	rows, err := f.db.ListJobs(f.ctx, store.JobFilter{Limit: store.MaxPageSize})
	must(f.t, err)
	out := make(map[string]store.Job, len(rows))
	for _, j := range rows {
		out[j.JobID] = j
	}
	return out
}

// D43 item 14: an unfiltered snapshot of the current boot settles running jobs
// it omits, unless a newer live event or the watermark itself covers them.
func TestSnapshotOmissionLosesMissingJob(t *testing.T) {
	f := setup(t)
	l := f.connect("boot")
	ids := []string{"00000000000a", "00000000000b", "00000000000c"}
	for i, id := range ids {
		send(t, f.ctx, l, jobEvent(id, uint64(i+1)))
	}
	eventually(t, func() bool { return f.hooks.events.Load() == 3 })
	// A gap (seq 5 after 3) requests one coalesced snapshot.
	send(t, f.ctx, l, jobEvent("00000000000d", 5))
	if m := receive(t, f.ctx, l); m.GetListJobs() == nil {
		t.Fatal(m)
	}
	eventually(t, func() bool { return f.hooks.events.Load() == 4 })
	before := f.hooks.snapshots.Load()
	kept := &circusv1.JobEvent{JobId: ids[1], SessionId: "session", CallId: "call", Seq: 2, State: circusv1.JobEvent_STATE_RUNNING}
	send(t, f.ctx, l, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_JobSnapshot{JobSnapshot: &circusv1.JobSnapshot{Seq: 3, Jobs: []*circusv1.JobEvent{kept}}}})
	eventually(t, func() bool { return f.hooks.snapshots.Load() > before })
	jobs := f.jobs()
	want := map[string]store.JobState{
		ids[0]:         store.JobLost,    // LastSeq 1 < 3 and omitted
		ids[1]:         store.JobRunning, // present in the snapshot
		ids[2]:         store.JobLost,    // LastSeq == watermark, omitted: lost at seq+1
		"00000000000d": store.JobRunning, // newer live event than the snapshot
	}
	for id, state := range want {
		if jobs[id].State != state {
			t.Errorf("job %s: %s, want %s", id, jobs[id].State, state)
		}
	}
	if j := jobs[ids[0]]; j.LastSeq != 3 || j.EndedAt == 0 {
		t.Fatalf("lost job metadata: %+v", j)
	}
	if j := jobs[ids[2]]; j.LastSeq != 4 {
		t.Fatalf("watermark job metadata: %+v", j)
	}
	if f.registry.Stats().Connections != 1 {
		t.Fatal("omission settlement closed the link")
	}
}

// A query waiting on a daemon that disconnects is answered, not stranded; while
// the daemon is offline a nonterminal call reports its durable state.
func TestQueryAnsweredOnDisconnect(t *testing.T) {
	f := setup(t)
	l := f.connect("boot")
	s, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
	must(t, err)
	defer s.Close()
	receive(t, f.ctx, l)
	f.running(l, "call")
	type reply struct {
		u   *circusv1.CallUpdate
		err error
	}
	answer := make(chan reply, 1)
	go func() { u, err := f.router.Query(f.ctx, "u", "call"); answer <- reply{u, err} }()
	if m := receive(t, f.ctx, l); m.GetCallQuery().GetCallId() != "call" {
		t.Fatal(m)
	}
	f.disconnect(l)
	a := <-answer
	if a.err != nil || a.u.GetDispatched() == nil {
		t.Fatalf("stranded or wrong query answer: %v %v", a.u, a.err)
	}
	f.advance(31 * time.Second)
	f.waitState("call", store.CallUnknown)
	u, err := f.router.Query(f.ctx, "u", "call")
	must(t, err)
	if u.GetUnknown() == nil {
		t.Fatal(u)
	}
	if f.router.Stats().QueryWaiters != 0 {
		t.Fatal("query waiter retained")
	}
}

// Stale boot/user rows: replies for a call dispatched to another boot, or
// claiming another user, change nothing. Replies are fenced by device and
// dispatch boot, not by generation (M2): a superseded connection of the
// dispatch boot still settles the call, and its disconnect keeps the route.
func TestReplyFencingByDeviceAndBoot(t *testing.T) {
	f := setup(t)
	l := f.connect("boot")
	s, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
	must(t, err)
	defer s.Close()
	receive(t, f.ctx, l)
	before, err := f.db.GetCall(f.ctx, "call")
	must(t, err)
	result := &circusv1.DeviceResult{CallId: "call", Output: &circusv1.ToolOutput{}}
	route, _, err := f.registry.Lookup(f.ctx, "d")
	must(t, err)
	for _, tc := range []struct {
		name string
		conn gateway.Connection
	}{
		{"boot", gateway.Connection{Target: bus.Target{InstanceID: "instance", ConnectionID: route.Target.ConnectionID, DeviceID: "d", BootID: "other"}, UserID: "u", DeviceName: "desk"}},
		{"user", gateway.Connection{Target: route.Target, UserID: "intruder", DeviceName: "desk"}},
		{"device", gateway.Connection{Target: bus.Target{InstanceID: "instance", ConnectionID: "x", DeviceID: "d2", BootID: "boot"}, UserID: "u", DeviceName: "laptop"}},
	} {
		must(t, f.router.OnDeviceResult(f.ctx, tc.conn, result))
		must(t, f.router.OnCallAck(f.ctx, tc.conn, &circusv1.CallAck{CallId: "call", Outcome: &circusv1.CallAck_Rejection{Rejection: &circusv1.Rejection{Code: circusv1.Rejection_CODE_INVALID}}}))
		must(t, f.router.OnDisconnected(f.ctx, tc.conn))
		after, err := f.db.GetCall(f.ctx, "call")
		must(t, err)
		if !equalCall(before, after) {
			t.Fatalf("%s: fenced reply changed %+v -> %+v", tc.name, before, after)
		}
		if f.registry.Stats().Connections != 1 {
			t.Fatalf("%s: stale disconnect removed the route", tc.name)
		}
	}
	superseded := gateway.Connection{Target: bus.Target{InstanceID: "instance", ConnectionID: "superseded", DeviceID: "d", BootID: "boot"}, UserID: "u", DeviceName: "desk"}
	must(t, f.router.OnDisconnected(f.ctx, superseded))
	if f.registry.Stats().Connections != 1 {
		t.Fatal("superseded disconnect removed the current route")
	}
	must(t, f.router.OnDeviceResult(f.ctx, superseded, result))
	f.waitState("call", store.CallCompleted)
	nextResult(t, f.ctx, s)
}

// A call lost to a new boot ignores that boot's later result for it.
func TestNewBootCannotSettleOldBootCall(t *testing.T) {
	f := setup(t)
	l := f.connect("boot")
	s, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
	must(t, err)
	defer s.Close()
	receive(t, f.ctx, l)
	// A new boot loses the call; that boot's later result for it is ignored.
	f.disconnect(l)
	l = f.connect("new")
	f.waitState("call", store.CallLost)
	lostRow, err := f.db.GetCall(f.ctx, "call")
	must(t, err)
	f.result(l, "call")
	send(t, f.ctx, l, jobEvent("00000000000a", 1))
	eventually(t, func() bool { return f.hooks.events.Load() == 1 })
	after, err := f.db.GetCall(f.ctx, "call")
	must(t, err)
	if !equalCall(lostRow, after) {
		t.Fatalf("new boot settled an old-boot call: %+v", after)
	}
}

// absent × submit after the grace period expired → Offline without a row in
// accepted; a second device of the same user mismatches an existing call ID.
func TestSubmitAfterGraceAndDeviceMismatch(t *testing.T) {
	f := setup(t)
	must(t, f.db.CreateIdentity(f.ctx, store.Identity{Kind: store.KindDevice, ID: "d2", UserID: "u", Name: "laptop", MachineID: "m2", CreatedAt: 1}))
	l := f.connect("boot")
	s, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
	must(t, err)
	defer s.Close()
	receive(t, f.ctx, l)
	other, err := f.router.Submit(f.ctx, "u", "laptop", call("call"))
	must(t, err)
	defer other.Close()
	if u, err := other.Next(f.ctx); err != nil || u.GetRejected().GetCode() != circusv1.CallUpdate_Rejected_CODE_CALL_MISMATCH {
		t.Fatal(u, err)
	}
	f.result(l, "call")
	nextResult(t, f.ctx, s)
	f.disconnect(l)
	f.advance(31 * time.Second)
	late, err := f.router.Submit(f.ctx, "u", "desk", call("late"))
	must(t, err)
	defer late.Close()
	u, err := late.Next(f.ctx)
	must(t, err)
	if u.GetOffline() == nil {
		t.Fatal(u)
	}
	if f.state("late") != store.CallOffline || f.router.Stats().PendingCalls != 0 {
		t.Fatal("offline submission retained work")
	}
}

// F14: after overflow the submitter recovers the current state with Query.
func TestOverflowThenQueryRecoversState(t *testing.T) {
	f := setup(t, func(c *calls.Config, _ *gateway.Config) { c.MaxUpdatesPerSubscription = 1 })
	l := f.connect("boot")
	s, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
	must(t, err)
	defer s.Close()
	receive(t, f.ctx, l)
	if _, err = s.Next(f.ctx); !errors.Is(err, calls.ErrOverflow) {
		t.Fatal(err)
	}
	f.running(l, "call")
	answer := make(chan *circusv1.CallUpdate, 1)
	go func() { u, _ := f.router.Query(f.ctx, "u", "call"); answer <- u }()
	if m := receive(t, f.ctx, l); m.GetCallQuery() == nil {
		t.Fatal(m)
	}
	send(t, f.ctx, l, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_CallStatus{CallStatus: &circusv1.CallStatus{CallId: "call", State: circusv1.CallStatus_STATE_COMPLETED, Result: &circusv1.DeviceResult{CallId: "call", Output: &circusv1.ToolOutput{}}}}})
	if u := <-answer; u.GetResult() == nil {
		t.Fatal(u)
	}
	f.waitState("call", store.CallCompleted)
}

// Crash-and-reopen at every write point of submit → dispatch → ack → result.
// The fake daemon executes a call at most once per received DispatchCall. After
// restart the router never sends DispatchCall for the old attempt; dispatch
// evidence precedes every execution; outcomes match what the daemon did.
func TestCrashAtEveryWritePoint(t *testing.T) {
	// Dry run: count the write points of the uncrashed sequence.
	writes := func() int {
		f := setup(t)
		l := f.connect("boot")
		f.haltTimers()
		start := f.fault.Writes()
		s, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
		must(t, err)
		defer s.Close()
		receive(t, f.ctx, l)
		f.running(l, "call")
		f.result(l, "call")
		f.waitState("call", store.CallCompleted)
		return f.fault.Writes() - start
	}()
	if writes < 7 {
		t.Fatalf("unexpectedly few write points: %d", writes)
	}
	for n := 1; n <= writes; n++ {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			f := setup(t)
			l := f.connect("boot")
			f.haltTimers()
			f.fault.CrashAfterWrite(n, nil)
			s, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
			if err == nil {
				defer s.Close()
			}
			executed := false
			short, cancel := context.WithTimeout(f.ctx, 200*time.Millisecond)
			m, rerr := l.Receive(short)
			cancel()
			if rerr == nil {
				d := m.(*circusv1.DaemonDownlink).GetDispatchCall()
				if d == nil {
					t.Fatal(m)
				}
				executed = true
				row, err := f.db.GetCall(f.ctx, "call")
				if err != nil || row.DispatchedBootID != "boot" {
					t.Fatalf("executed without durable dispatch evidence: %+v %v", row, err)
				}
				send(t, f.ctx, l, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_CallAck{CallAck: &circusv1.CallAck{CallId: "call", Outcome: &circusv1.CallAck_Status{Status: &circusv1.CallStatus{CallId: "call", State: circusv1.CallStatus_STATE_RUNNING}}}}})
				f.result(l, "call")
			}
			eventually(t, func() bool { return f.fault.Crashed() })
			f.restart()
			l = f.open("boot")
			for {
				m := receive(t, f.ctx, l)
				if m.GetListJobs() != nil {
					break
				}
				if m.GetDispatchCall() != nil {
					t.Fatal("restart re-dispatched an old attempt")
				}
				q := m.GetCallQuery()
				if q == nil {
					t.Fatal(m)
				}
				status := &circusv1.CallStatus{CallId: q.CallId, State: circusv1.CallStatus_STATE_LOST}
				if executed {
					status = &circusv1.CallStatus{CallId: q.CallId, State: circusv1.CallStatus_STATE_COMPLETED, Result: &circusv1.DeviceResult{CallId: q.CallId, Output: &circusv1.ToolOutput{}}}
				}
				send(t, f.ctx, l, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_CallStatus{CallStatus: status}})
			}
			f.snapshot(l)
			row, err := f.db.GetCall(f.ctx, "call")
			if errors.Is(err, store.ErrNotFound) {
				if executed {
					t.Fatal("executed call has no row")
				}
				return
			}
			must(t, err)
			eventually(t, func() bool { c, _ := f.db.GetCall(f.ctx, "call"); return c.State.Terminal() })
			row, err = f.db.GetCall(f.ctx, "call")
			must(t, err)
			switch {
			case row.State == store.CallCompleted && !executed:
				t.Fatal("completed without execution")
			case row.State != store.CallCompleted && row.State != store.CallLost:
				t.Fatalf("unexpected outcome %s", row.State)
			case executed && row.State == store.CallLost && row.OutcomeReason == "CONTROL_PLANE_RESTARTED":
				t.Fatal("executed attempt reset as never dispatched")
			}
			// A lost, never-dispatched attempt may be resubmitted with the same ID;
			// a dispatched one stays lost.
			if row.State != store.CallLost {
				return
			}
			again, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
			must(t, err)
			defer again.Close()
			if row.DispatchedBootID == "" {
				if m := receive(t, f.ctx, l); m.GetDispatchCall().GetCall().GetCallId() != "call" {
					t.Fatal(m)
				}
				f.result(l, "call")
				nextResult(t, f.ctx, again)
			} else if u, err := again.Next(f.ctx); err != nil || u.GetLost() == nil {
				t.Fatal(u, err)
			}
		})
	}
}

// Dispatch reads the device's current boot in the dispatch Tx. When the store
// records another boot than the ready route's (as another writer would), the
// call is not dispatched, so no DispatchCall can name a boot the link lacks.
func TestDispatchReadsCurrentBootInTx(t *testing.T) {
	f := setup(t)
	l := f.connect("boot")
	_, err := f.db.SetConnected(f.ctx, store.KindDevice, "d", "elsewhere", f.clock.Now().UnixMilli())
	must(t, err)
	s, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
	must(t, err)
	defer s.Close()
	short, cancel := context.WithTimeout(f.ctx, 50*time.Millisecond)
	defer cancel()
	if m, err := l.Receive(short); err == nil {
		t.Fatalf("dispatched despite boot mismatch: %v", m)
	}
	c, err := f.db.GetCall(f.ctx, "call")
	must(t, err)
	if c.State != store.CallAccepted || c.DispatchedBootID != "" {
		t.Fatal(c)
	}
}
