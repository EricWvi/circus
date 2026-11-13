package link_test

import (
	"context"
	"sync"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/daemon"
	"github.com/EricWvi/circus/internal/daemon/jobs"
	daemonlink "github.com/EricWvi/circus/internal/daemon/link"
	"github.com/EricWvi/circus/internal/wire"
)

func ackForCall(t *testing.T, l *wire.Link, callID string) *circusv1.CallAck {
	t.Helper()
	m := receive(t, l)
	a := m.GetCallAck()
	if a == nil {
		t.Fatalf("waiting for ack for %q, got non-ack uplink: %v", callID, m)
	}
	if a.GetCallId() != callID {
		t.Fatalf("waiting for ack for %q, got ack for %q: %v", callID, a.GetCallId(), a)
	}
	return a
}

func TestDuplicateDuringShutdownReportsExistingStatus(t *testing.T) {
	started, cancelled := make(chan struct{}, 1), make(chan struct{}, 1)
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	h := start(t, func(o *daemonlink.Options) {
		o.Tools = func(*jobs.Manager) []daemon.Tool {
			return []daemon.Tool{tool{func(ctx context.Context, c *circusv1.DeviceCall) (*circusv1.DeviceResult, error) {
				if c.CallId == "slow" {
					started <- struct{}{}
					<-ctx.Done()
					cancelled <- struct{}{}
					<-release
				}
				return result("side effect happened"), nil
			}}}
		}
	}, nil)
	l, _, hello := h.handshake(t)
	dispatch(t, l, hello.BootId, call("done"))
	doneAck, doneResult := false, false
	for !doneAck || !doneResult {
		m := receive(t, l)
		switch p := m.Payload.(type) {
		case *circusv1.DaemonUplink_CallAck:
			if p.CallAck.GetCallId() != "done" {
				t.Fatalf("unexpected ack while waiting for done: %v", p.CallAck)
			}
			if doneAck {
				t.Fatalf("duplicate done ack: %v", p.CallAck)
			}
			doneAck = true
		case *circusv1.DaemonUplink_Result:
			if p.Result.GetCallId() != "done" {
				t.Fatalf("unexpected result while waiting for done: %v", p.Result)
			}
			if doneResult {
				t.Fatalf("duplicate done result: %v", p.Result)
			}
			doneResult = true
		default:
			t.Fatalf("unexpected uplink while waiting for done ack and result: %v", m)
		}
	}
	dispatch(t, l, hello.BootId, call("slow"))
	recv(t, started)
	ackForCall(t, l, "slow")
	h.cancel()
	recv(t, cancelled)
	for id, want := range map[string]circusv1.CallStatus_State{"done": circusv1.CallStatus_STATE_COMPLETED, "slow": circusv1.CallStatus_STATE_RUNNING} {
		dispatch(t, l, hello.BootId, call(id))
		a := ackForCall(t, l, id)
		if a.GetStatus().GetState() != want {
			t.Fatalf("%s reported as unexecuted: %v", id, a)
		}
		mismatch := call(id)
		mismatch.GetRead().Path = "changed"
		dispatch(t, l, hello.BootId, mismatch)
		if a = ackForCall(t, l, id); a.GetRejection().GetCode() != circusv1.Rejection_CODE_CALL_MISMATCH {
			t.Fatal(a)
		}
	}
	dispatch(t, l, "old-boot", call("new"))
	if a := ackForCall(t, l, "new"); a.GetRejection().GetCode() != circusv1.Rejection_CODE_BOOT_MISMATCH {
		t.Fatal(a)
	}
	dispatch(t, l, hello.BootId, call("new"))
	if a := ackForCall(t, l, "new"); a.GetRejection().GetCode() != circusv1.Rejection_CODE_SHUTTING_DOWN {
		t.Fatal(a)
	}
	close(release)
	if err := recv(t, h.done); err != nil {
		t.Fatal(err)
	}
	h.done <- nil
}

type stuckJob struct {
	release, entered chan struct{}
	once             sync.Once
}

func (j *stuckJob) Wait() (int, error) { <-j.release; return 137, jobs.ErrKilled }
func (j *stuckJob) Stop() error        { j.once.Do(func() { close(j.entered) }); <-j.release; return nil }
func TestBlockingSessionStopDoesNotBlockReaderOrShutdown(t *testing.T) {
	job := &stuckJob{release: make(chan struct{}), entered: make(chan struct{})}
	defer func() {
		select {
		case <-job.release:
		default:
			close(job.release)
		}
	}()
	var manager *jobs.Manager
	h := start(t, func(o *daemonlink.Options) {
		o.Tools = func(m *jobs.Manager) []daemon.Tool {
			manager = m
			return []daemon.Tool{tool{func(ctx context.Context, c *circusv1.DeviceCall) (*circusv1.DeviceResult, error) {
				if c.CallId == "bg" {
					if _, err := m.Start(c.SessionId, c.CallId, "cmd", func(string) (jobs.Job, string, error) { return job, "/o", nil }); err != nil {
						return nil, err
					}
				}
				return result("done"), nil
			}}}
		}
	}, nil)
	l, _, hello := h.handshake(t)
	dispatch(t, l, hello.BootId, call("bg"))
	for receive(t, l).GetResult() == nil {
	}
	send(t, l, &circusv1.DaemonDownlink{Payload: &circusv1.DaemonDownlink_StopSessionJobs{StopSessionJobs: &circusv1.StopSessionJobs{SessionId: "session"}}})
	recv(t, job.entered)
	send(t, l, &circusv1.DaemonDownlink{Payload: &circusv1.DaemonDownlink_CallQuery{CallQuery: &circusv1.CallQuery{CallId: "bg"}}})
	for {
		m := receive(t, l)
		if s := m.GetCallStatus(); s != nil {
			if s.State != circusv1.CallStatus_STATE_COMPLETED {
				t.Fatal(s)
			}
			break
		}
	}
	dispatch(t, l, hello.BootId, call("after-stop"))
	if a := ack(t, l); a.GetStatus() == nil {
		t.Fatal(a)
	}
	h.cancel()
	eventually(t, func() bool { return h.client.Stats().ShuttingDown })
	h.clk.Advance(16 * time.Second)
	if err := recv(t, h.done); err == nil {
		t.Fatal("stuck job shutdown should report cleanup timeout")
	}
	h.done <- nil
	close(job.release)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := manager.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}

type delayedShutdownTool struct {
	tool
	entered, permit chan struct{}
	hookBudget      chan time.Duration
}

func (t delayedShutdownTool) Shutdown(ctx context.Context) error {
	deadline, _ := ctx.Deadline()
	t.hookBudget <- time.Until(deadline)
	close(t.entered)
	<-t.permit
	return nil
}
func TestShutdownReservesFinalFlushBudget(t *testing.T) {
	gate := &writeGate{predicate: func(m *circusv1.DaemonUplink) bool { return m.GetResult() != nil }, entered: make(chan struct{}), permit: make(chan struct{})}
	defer gate.open()
	hookEntered, hookPermit := make(chan struct{}), make(chan struct{})
	var hookOnce sync.Once
	defer hookOnce.Do(func() { close(hookPermit) })
	started := make(chan struct{}, 1)
	hookBudget := make(chan time.Duration, 1)
	h := start(t, func(o *daemonlink.Options) {
		o.Tools = func(*jobs.Manager) []daemon.Tool {
			return []daemon.Tool{delayedShutdownTool{hookBudget: hookBudget, tool: tool{func(ctx context.Context, _ *circusv1.DeviceCall) (*circusv1.DeviceResult, error) {
				started <- struct{}{}
				<-ctx.Done()
				return &circusv1.DeviceResult{Detail: &circusv1.DeviceResult_Bash{Bash: &circusv1.BashResult{Interrupted: true}}}, nil
			}}, entered: hookEntered, permit: hookPermit}}
		}
	}, func(c wire.Conn) wire.Conn { gate.Conn = c; return gate })
	l, _, hello := h.handshake(t)
	dispatch(t, l, hello.BootId, call("final"))
	recv(t, started)
	ack(t, l)
	h.cancel()
	recv(t, hookEntered)
	// Default split: 16s = 13s cleanup + 3s reserve. The tool hook's own
	// (wall-clock) bound is cleanup - AbandonGrace = 8s, so a hook started
	// after the abandonment grace still ends inside cleanup.
	if budget := recv(t, hookBudget); budget > 8*time.Second || budget < 7*time.Second {
		t.Fatalf("tool hook budget %v, want 8s", budget)
	}
	recv(t, gate.entered)
	h.clk.Advance(13 * time.Second)
	eventually(t, func() bool { return h.client.Stats().Flushing })
	// Cleanup used its entire slice. The reserved three seconds still permit the
	// final frame and the normal close handshake without advancing fake time.
	gate.open()
	hookOnce.Do(func() { close(hookPermit) })
	if m := receive(t, l); !m.GetResult().GetBash().GetInterrupted() {
		t.Fatal(m)
	}
	if err := recv(t, h.done); err == nil {
		t.Fatal("exhausted cleanup slice was not reported")
	}
	h.done <- nil
}
