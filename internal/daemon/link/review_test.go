package link_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/credfile"
	"github.com/EricWvi/circus/internal/daemon"
	"github.com/EricWvi/circus/internal/daemon/jobs"
	daemonlink "github.com/EricWvi/circus/internal/daemon/link"
	"github.com/EricWvi/circus/internal/wire"
	"github.com/EricWvi/circus/internal/wire/wiretest"
	"github.com/coder/websocket"
	"google.golang.org/protobuf/proto"
)

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
	for receive(t, l).GetResult() == nil {
	}
	dispatch(t, l, hello.BootId, call("slow"))
	recv(t, started)
	ack(t, l)
	h.cancel()
	recv(t, cancelled)
	for id, want := range map[string]circusv1.CallStatus_State{"done": circusv1.CallStatus_STATE_COMPLETED, "slow": circusv1.CallStatus_STATE_RUNNING} {
		dispatch(t, l, hello.BootId, call(id))
		a := ack(t, l)
		if a.GetStatus().GetState() != want {
			t.Fatalf("%s reported as unexecuted: %v", id, a)
		}
		mismatch := call(id)
		mismatch.GetRead().Path = "changed"
		dispatch(t, l, hello.BootId, mismatch)
		if a = ack(t, l); a.GetRejection().GetCode() != circusv1.Rejection_CODE_CALL_MISMATCH {
			t.Fatal(a)
		}
	}
	dispatch(t, l, "old-boot", call("new"))
	if a := ack(t, l); a.GetRejection().GetCode() != circusv1.Rejection_CODE_BOOT_MISMATCH {
		t.Fatal(a)
	}
	dispatch(t, l, hello.BootId, call("new"))
	if a := ack(t, l); a.GetRejection().GetCode() != circusv1.Rejection_CODE_SHUTTING_DOWN {
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

// A result no idle link can accept becomes a fixed-fault placeholder in every
// uplink view, so neither delivery nor a later resync disconnects in a loop.
func TestOversizeResultQueryAndDuplicateUsePlaceholder(t *testing.T) {
	for _, tc := range []struct {
		name      string
		queued    int
		textBytes int
	}{
		{"above-message-limit", 0, 1250000},          // harness MaxMessageBytes is 1 MiB
		{"above-wire-queue-limit", 64 << 10, 100000}, // fits the message limit only
	} {
		t.Run(tc.name, func(t *testing.T) {
			var runs atomic.Int32
			h := start(t, func(o *daemonlink.Options) {
				o.Wire.MaxQueuedBytes = tc.queued
				o.Tools = func(*jobs.Manager) []daemon.Tool {
					return []daemon.Tool{tool{func(context.Context, *circusv1.DeviceCall) (*circusv1.DeviceResult, error) {
						runs.Add(1)
						return result(strings.Repeat("x", tc.textBytes)), nil
					}}}
				}
			}, nil)
			l, _, hello := h.handshake(t)
			dispatch(t, l, hello.BootId, call("big"))
			check := func(r *circusv1.DeviceResult) {
				t.Helper()
				if r.GetCallId() != "big" || r.GetFault().GetCode() != circusv1.DaemonFault_CODE_INTERNAL || r.GetFault().GetMessage() != "RESULT_TOO_LARGE" || !r.GetOutput().GetIsError() {
					t.Fatal(r)
				}
			}
			for {
				m := receive(t, l)
				if r := m.GetResult(); r != nil {
					check(r)
					break
				}
			}
			send(t, l, &circusv1.DaemonDownlink{Payload: &circusv1.DaemonDownlink_CallQuery{CallQuery: &circusv1.CallQuery{CallId: "big"}}})
			for {
				m := receive(t, l)
				if s := m.GetCallStatus(); s != nil {
					if s.State != circusv1.CallStatus_STATE_COMPLETED {
						t.Fatal(s)
					}
					check(s.Result)
					break
				}
			}
			dispatch(t, l, hello.BootId, call("big"))
			check(ack(t, l).GetStatus().GetResult())
			if s := h.client.Stats(); !s.Connected || s.Connections != 1 || s.Overflows != 0 || runs.Load() != 1 {
				t.Fatal(s)
			}
		})
	}
}

func TestCallbackOverflowDisconnects(t *testing.T) {
	h := start(t, func(o *daemonlink.Options) {
		o.Dispatcher.CallbackByteLimit = 1
		o.Tools = func(*jobs.Manager) []daemon.Tool {
			return []daemon.Tool{tool{func(context.Context, *circusv1.DeviceCall) (*circusv1.DeviceResult, error) {
				return result("retained"), nil
			}}}
		}
	}, nil)
	l, _, hello := h.handshake(t)
	dispatch(t, l, hello.BootId, call("callback"))
	h.retry(t, 500*time.Millisecond)
	if s := h.client.Stats(); s.Overflows == 0 {
		t.Fatal(s)
	}
	next, _, _ := h.handshake(t)
	send(t, next, &circusv1.DaemonDownlink{Payload: &circusv1.DaemonDownlink_CallQuery{CallQuery: &circusv1.CallQuery{CallId: "callback"}}})
	if s := receive(t, next).GetCallStatus(); s.GetState() != circusv1.CallStatus_STATE_COMPLETED || s.GetResult().GetOutput().GetContent()[0].GetText() != "retained" {
		t.Fatal(s)
	}
}

// With a shared send budget, wire.Send refuses with ErrBusy without failing the
// link. The client must still close the connection so the control plane
// resyncs by query, rather than silently losing the result on a live link.
func TestBusySendBudgetDisconnectsForResync(t *testing.T) {
	budget, err := wire.NewBudget(2<<20, 16<<20) // the send pool fits one ~900 KB fragmented result
	if err != nil {
		t.Fatal(err)
	}
	const size = 900000
	var runs atomic.Int32
	h := start(t, func(o *daemonlink.Options) {
		o.Wire.Budget = budget
		o.Tools = func(*jobs.Manager) []daemon.Tool {
			return []daemon.Tool{tool{func(_ context.Context, c *circusv1.DeviceCall) (*circusv1.DeviceResult, error) {
				runs.Add(1)
				return result(strings.Repeat(c.CallId[:1], size)), nil
			}}}
		}
	}, nil)
	l, p, hello := h.handshake(t)
	p.client.SetFaults(wiretest.Faults{BlockWrites: true})
	dispatch(t, l, hello.BootId, call("a"))
	eventually(t, func() bool { return h.client.Stats().Wire.QueuedBytes >= size })
	dispatch(t, l, hello.BootId, call("b"))
	h.retry(t, 500*time.Millisecond)
	if s := h.client.Stats(); s.Overflows == 0 || s.Dropped == 0 {
		t.Fatal(s)
	}
	next, _, _ := h.handshake(t)
	for _, id := range []string{"a", "b"} {
		send(t, next, &circusv1.DaemonDownlink{Payload: &circusv1.DaemonDownlink_CallQuery{CallQuery: &circusv1.CallQuery{CallId: id}}})
		for {
			if s := receive(t, next).GetCallStatus(); s != nil {
				if s.State != circusv1.CallStatus_STATE_COMPLETED || len(s.GetResult().GetOutput().GetContent()[0].GetText()) != size {
					t.Fatalf("%s: %v", id, s.GetState())
				}
				break
			}
		}
	}
	if runs.Load() != 2 {
		t.Fatalf("runs=%d", runs.Load())
	}
}

type exitedJob struct{}

func (exitedJob) Wait() (int, error) { return 0, nil }
func (exitedJob) Stop() error        { return nil }

// A JobSnapshot above the negotiated limit (harness: 1 MiB) would be refused by
// wire on every reconnect. It must instead arrive truncated, RUNNING first.
func TestSnapshotAboveNegotiatedLimitIsTruncated(t *testing.T) {
	var manager *jobs.Manager
	h := start(t, func(o *daemonlink.Options) {
		o.Tools = func(m *jobs.Manager) []daemon.Tool { manager = m; return nil }
	}, nil)
	path := "/" + strings.Repeat("p", 4000)
	for i := 0; i < 300; i++ {
		if _, err := manager.Start("s", fmt.Sprintf("c%d", i), "cmd", func(string) (jobs.Job, string, error) { return exitedJob{}, path, nil }); err != nil {
			t.Fatal(err)
		}
	}
	running := []*backgroundJob{{done: make(chan struct{})}, {done: make(chan struct{})}}
	for i, job := range running {
		if _, err := manager.Start("s", fmt.Sprintf("r%d", i), "cmd", func(string) (jobs.Job, string, error) { return job, path, nil }); err != nil {
			t.Fatal(err)
		}
	}
	// Let all 602 job events be delivered (and dropped) before connecting, so
	// the live link carries the snapshot alone, not a burst of 4 KB events.
	eventually(t, func() bool { return h.client.Stats().Dropped == 602 })
	l, _, _ := h.handshake(t)
	send(t, l, &circusv1.DaemonDownlink{Payload: &circusv1.DaemonDownlink_ListJobs{ListJobs: &circusv1.ListJobs{}}})
	var snap *circusv1.JobSnapshot
	for snap == nil {
		snap = receive(t, l).GetJobSnapshot()
	}
	if !snap.Truncated || len(snap.Jobs) == 0 || len(snap.Jobs) >= 302 || snap.Seq != 602 {
		t.Fatalf("entries=%d truncated=%v seq=%d", len(snap.Jobs), snap.Truncated, snap.Seq)
	}
	if size := proto.Size(&circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_JobSnapshot{JobSnapshot: snap}}); size > 1<<20 {
		t.Fatalf("snapshot envelope %d bytes exceeds the negotiated limit", size)
	}
	for i, e := range snap.Jobs {
		if (i < len(running)) != (e.State == circusv1.JobEvent_STATE_RUNNING) {
			t.Fatalf("RUNNING jobs not first at %d: %v", i, e.State)
		}
	}
	if s := h.client.Stats(); !s.Connected || s.Connections != 1 || s.Overflows != 0 {
		t.Fatal(s)
	}
}

func TestGoAwayZeroAndSmallDelaysRespectBackoffFloor(t *testing.T) {
	h := start(t, nil, nil)
	for i, request := range []uint32{0, 1, 1} {
		l, _, _ := h.handshake(t)
		send(t, l, &circusv1.DaemonDownlink{Payload: &circusv1.DaemonDownlink_GoAway{GoAway: &circusv1.GoAway{ReconnectAfterMs: request}}})
		h.retry(t, []time.Duration{500 * time.Millisecond, 500 * time.Millisecond, time.Second}[i])
	}
	h.handshake(t)
}

type blockedClose struct {
	wire.Conn
	entered chan struct{}
	once    sync.Once
}

func (c *blockedClose) Shutdown(ctx context.Context) error {
	c.once.Do(func() { close(c.entered) })
	<-ctx.Done()
	_ = c.Conn.Close()
	return ctx.Err()
}
func TestGoAwayCloseObservesOuterCancellation(t *testing.T) {
	entered := make(chan struct{})
	h := start(t, nil, func(c wire.Conn) wire.Conn { return &blockedClose{Conn: c, entered: entered} })
	l, _, _ := h.handshake(t)
	send(t, l, &circusv1.DaemonDownlink{Payload: &circusv1.DaemonDownlink_GoAway{GoAway: &circusv1.GoAway{}}})
	recv(t, entered)
	h.cancel()
	if err := recv(t, h.done); err != nil {
		t.Fatal(err)
	}
	h.done <- nil
}

func TestMachineIDIsCachedAcrossReconnects(t *testing.T) {
	var probes atomic.Int32
	h := start(t, func(o *daemonlink.Options) {
		o.MachineIDSource = func(context.Context) (string, error) { probes.Add(1); return "cached-machine-hash", nil }
	}, nil)
	l, _, hello := h.handshake(t)
	if hello.MachineId != "cached-machine-hash" {
		t.Fatal(hello)
	}
	_ = l.Close()
	h.retry(t, 500*time.Millisecond)
	_, _, again := h.handshake(t)
	if again.MachineId != hello.MachineId || probes.Load() != 1 {
		t.Fatalf("probes=%d hello=%v", probes.Load(), again)
	}
}

func TestMachineIDUnavailableFailsBeforeDial(t *testing.T) {
	for _, probeErr := range []error{nil, errors.New("no initialized machine ID")} {
		var dials atomic.Int32
		_, err := daemonlink.New(daemonlink.Options{Credentials: credfile.File{Role: credfile.RoleDaemon, ControlPlaneURL: "http://127.0.0.1", PrincipalID: "device", Token: "secret"}, MachineIDSource: func(context.Context) (string, error) { return "", probeErr }, Dial: func(context.Context, string, *websocket.DialOptions) (wire.Conn, *http.Response, error) {
			dials.Add(1)
			return nil, nil, errors.New("must not dial")
		}})
		if err == nil || !strings.Contains(err.Error(), "machine ID unavailable at startup") || dials.Load() != 0 {
			t.Fatalf("error=%v dials=%d", err, dials.Load())
		}
	}
}

// writeGate holds one selected physical frame while the real wire scheduler
// accepts later messages. It stores no frame copy and is cancellable by wire.
type writeGate struct {
	wire.Conn
	predicate       func(*circusv1.DaemonUplink) bool
	entered, permit chan struct{}
	once, openOnce  sync.Once
}

func (g *writeGate) open() { g.openOnce.Do(func() { close(g.permit) }) }
func (g *writeGate) WriteMessage(ctx context.Context, data []byte) error {
	m, err := wire.Decode(wire.DaemonUplink, data, 1<<20)
	if err != nil {
		return err
	}
	blocked := false
	if g.predicate(m.(*circusv1.DaemonUplink)) {
		g.once.Do(func() { blocked = true; close(g.entered) })
	}
	if blocked {
		select {
		case <-g.permit:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return g.Conn.WriteMessage(ctx, data)
}
func TestAckHasControlPriority(t *testing.T) {
	gate := &writeGate{predicate: func(m *circusv1.DaemonUplink) bool { return m.GetFragment() != nil }, entered: make(chan struct{}), permit: make(chan struct{})}
	defer gate.open()
	big, small := make(chan struct{}), make(chan struct{})
	h := start(t, func(o *daemonlink.Options) {
		o.Tools = func(*jobs.Manager) []daemon.Tool {
			return []daemon.Tool{tool{func(ctx context.Context, c *circusv1.DeviceCall) (*circusv1.DeviceResult, error) {
				switch c.CallId {
				case "big":
					select {
					case <-big:
					case <-ctx.Done():
					}
					return result(strings.Repeat("x", 128<<10)), nil
				case "small":
					select {
					case <-small:
					case <-ctx.Done():
					}
					return result("small"), nil
				default:
					<-ctx.Done()
					return result("cancelled"), nil
				}
			}}}
		}
	}, func(c wire.Conn) wire.Conn { gate.Conn = c; return gate })
	l, _, hello := h.handshake(t)
	dispatch(t, l, hello.BootId, call("big"))
	ack(t, l)
	dispatch(t, l, hello.BootId, call("small"))
	ack(t, l)
	close(big)
	recv(t, gate.entered)
	close(small)
	eventually(t, func() bool { return h.client.Stats().Wire.QueuedMessages >= 2 }) // partial + small result
	dispatch(t, l, hello.BootId, call("urgent"))
	eventually(t, func() bool { return h.client.Stats().Wire.QueuedMessages >= 3 })
	gate.open()
	for {
		m := receive(t, l)
		if m.GetResult().GetCallId() == "small" {
			t.Fatal("data result overtook control ack")
		}
		if m.GetCallAck().GetCallId() == "urgent" {
			break
		}
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
