package link_test

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/daemon"
	"github.com/EricWvi/circus/internal/daemon/jobs"
	daemonlink "github.com/EricWvi/circus/internal/daemon/link"
	"github.com/EricWvi/circus/internal/wire"
	"github.com/EricWvi/circus/internal/wire/wiretest"
	"google.golang.org/protobuf/proto"
)

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
	eventually(t, func() bool {
		s := h.client.Stats()
		return s.Dropped >= 602 && !s.Connected
	})
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
