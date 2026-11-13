package calls_test

import (
	"context"
	"testing"
	"time"

	"github.com/EricWvi/circus/internal/store"
)

// crashRestart restarts the control plane without recording the disconnect,
// as a crash (or a router closed before the gateway) leaves it.
func (f *fixture) crashRestart() {
	f.t.Helper()
	f.haltTimers()
	cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	must(f.t, f.router.Close(cleanup))
	f.restart()
}

// After a restart the device row still says connected. It counts as offline
// since the restart: a watchdog before the cutoff does not lose the call (it
// goes unknown at grace expiry), a submission inside the grace period stays
// accepted, and a same-boot reconnect recovers both.
func TestRestartTreatsConnectedRowAsOfflineSinceStart(t *testing.T) {
	f := setup(t)
	l := f.connect("boot")
	f.disconnect(l)
	l = f.connect("boot")
	f.advance(2 * time.Hour) // connected for a long time
	s, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
	must(t, err)
	receive(t, f.ctx, l)
	f.running(l, "call")
	s.Close()
	f.crashRestart()
	waiting, err := f.router.Submit(f.ctx, "u", "desk", call("waiting"))
	must(t, err)
	defer waiting.Close()
	if u, err := waiting.Next(f.ctx); err != nil || u.GetAccepted() == nil {
		t.Fatalf("submission after restart: %v %v", u, err)
	}
	f.advance(5 * time.Minute) // grace expired; first watchdog due
	f.waitState("call", store.CallUnknown)
	f.waitState("waiting", store.CallOffline)
	l = f.open("boot")
	if m := receive(t, f.ctx, l); m.GetCallQuery().GetCallId() != "call" {
		t.Fatal(m)
	}
	f.finishSync(l)
	send(t, f.ctx, l, statusCompleted("call"))
	f.waitState("call", store.CallCompleted)
}

// The result-retention cutoff after a restart counts from the restart.
func TestRestartCutoffCountsFromStart(t *testing.T) {
	f := setup(t)
	l := f.connect("boot")
	s, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
	must(t, err)
	receive(t, f.ctx, l)
	f.running(l, "call")
	s.Close()
	f.advance(2 * time.Hour)
	f.crashRestart()
	for range 5 {
		f.advance(5 * time.Minute)
	}
	time.Sleep(20 * time.Millisecond)
	if st := f.state("call"); st == store.CallLost {
		t.Fatal("lost before the cutoff")
	}
	for range 3 {
		f.advance(5 * time.Minute)
	}
	f.waitState("call", store.CallLost)
	if c, _ := f.db.GetCall(f.ctx, "call"); c.OutcomeReason != "RESULT_RETENTION_EXPIRED" {
		t.Fatal(c)
	}
}

// After a restart, a submission to a device whose row still says connected
// records the disconnect and its grace timer, so the accepted call goes
// offline at grace expiry even with no other call on the device.
func TestRestartSubmitRecordsDisconnect(t *testing.T) {
	f := setup(t)
	f.connect("boot")
	f.advance(time.Hour)
	f.crashRestart()
	started := f.clock.Now().UnixMilli()
	s, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
	must(t, err)
	defer s.Close()
	if u, err := s.Next(f.ctx); err != nil || u.GetAccepted() == nil {
		t.Fatal(u, err)
	}
	d, err := f.db.GetIdentity(f.ctx, store.KindDevice, "d")
	must(t, err)
	if d.LastDisconnectedAt != started {
		t.Fatalf("disconnect not recorded: %+v", d)
	}
	f.advance(31 * time.Second)
	f.waitState("call", store.CallOffline)
}

// Recording a restart's disconnect never moves an existing grace timer.
func TestRecordOfflineKeepsExistingGraceTimer(t *testing.T) {
	f := setup(t)
	f.connect("boot")
	f.crashRestart()
	due := f.clock.Now().Add(time.Hour).UnixMilli()
	must(t, f.db.SetTimer(f.ctx, "device_grace", "d", due))
	s, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
	must(t, err)
	defer s.Close()
	ts, err := f.db.ListTimers(f.ctx, store.TimerFilter{Kind: "device_grace", SubjectID: "d", Limit: 1})
	must(t, err)
	if len(ts) != 1 || ts[0].DueAt != due {
		t.Fatalf("grace timer moved: %+v", ts)
	}
}
