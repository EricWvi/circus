package wire_test

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/clock"
	"github.com/EricWvi/circus/internal/testutil/wait"
	"github.com/EricWvi/circus/internal/wire"
	"github.com/EricWvi/circus/internal/wire/wiretest"
	"google.golang.org/protobuf/proto"
)

// Query and session stop must observe earlier dispatch.
func TestControlOrdering(t *testing.T) {
	track(t)
	a, b := wiretest.Pipe(wiretest.Options{AtoB: wiretest.Faults{FrameDelay: time.Millisecond}})
	o := wire.Options{DisableHeartbeat: true}
	sender := newLink(t, a, wire.DaemonUplink, wire.DaemonDownlink, o)
	receiver := newLink(t, b, wire.DaemonDownlink, wire.DaemonUplink, o)
	send(t, sender, dispatch("X", strings.Repeat("x", 1<<20)))
	send(t, sender, &circusv1.DaemonDownlink{Payload: &circusv1.DaemonDownlink_CallQuery{CallQuery: &circusv1.CallQuery{CallId: "X"}}})
	send(t, sender, &circusv1.DaemonDownlink{Payload: &circusv1.DaemonDownlink_StopSessionJobs{StopSessionJobs: &circusv1.StopSessionJobs{SessionId: "s"}}})
	if receive(t, receiver).(*circusv1.DaemonDownlink).GetDispatchCall() == nil {
		t.Fatal("query/stop overtook earlier dispatch")
	}
	_ = receive(t, receiver)
	_ = receive(t, receiver)
}

// A default pair must survive the full 200 x 300 KiB burst.
func TestBoundedStartedBurst(t *testing.T) {
	track(t)
	a, b := wiretest.Pipe(wiretest.Options{AtoB: wiretest.Faults{FrameDelay: 5 * time.Millisecond}})
	sender := newLink(t, a, wire.DaemonUplink, wire.DaemonDownlink, wire.Options{})
	receiver := newLink(t, b, wire.DaemonDownlink, wire.DaemonUplink, wire.Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		for i := 0; i < 200; i++ {
			if _, err := receiver.Receive(ctx); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	content := strings.Repeat("x", 300<<10)
	for i := 0; i < 200; i++ {
		if err := testSend(sender, ctx, dispatch(fmt.Sprintf("c%d", i), content)); err != nil {
			t.Fatal(err)
		}
		if sender.Stats().ActiveMessages > 2 {
			t.Fatal("more than two started messages")
		}
	}
	if err := wait.Recv(t, done); err != nil {
		t.Fatal(err)
	}
	if receiver.Err() != nil {
		t.Fatal(receiver.Err())
	}
}

// This scaled routine run keeps fragment progress within the timeout; the default-timeout version below uses a fake clock.
func TestSlowFragmentProgress(t *testing.T) {
	track(t)
	a, b := wiretest.Pipe(wiretest.Options{AtoB: wiretest.Faults{FrameDelay: 20 * time.Millisecond}})
	o := wire.Options{DisableHeartbeat: true, MaxFragmentBytes: 1024, MaxMessageBytes: 1 << 20, FragmentTimeout: 2 * time.Second}
	sender := newLink(t, a, wire.DaemonUplink, wire.DaemonDownlink, o)
	receiver := newLink(t, b, wire.DaemonDownlink, wire.DaemonUplink, o)
	for i := 0; i < 40; i++ {
		send(t, sender, dispatch(fmt.Sprintf("c%d", i), strings.Repeat("x", 3000)))
	}
	for i := 0; i < 40; i++ {
		_ = receive(t, receiver)
	}
	if receiver.Err() != nil {
		t.Fatal(receiver.Err())
	}
}

// This clock advances one complete frame at a time, without real 1 s sleeps.
type frameClock struct {
	*clock.Fake
	frames chan struct{}
}

func (c *frameClock) After(d time.Duration) <-chan time.Time {
	ch := c.Fake.After(d)
	if d == time.Second {
		c.frames <- struct{}{}
	}
	return ch
}
func TestDefaultSlowLink(t *testing.T) {
	track(t)
	clk := &frameClock{Fake: clock.New(), frames: make(chan struct{}, 2)}
	a, b := wiretest.Pipe(wiretest.Options{Clock: clk, AtoB: wiretest.Faults{FrameDelay: time.Second}})
	o := wire.Options{Clock: clk} // All transport limits and timeouts are defaults.
	sender := newLink(t, a, wire.DaemonUplink, wire.DaemonDownlink, o)
	receiver := newLink(t, b, wire.DaemonDownlink, wire.DaemonUplink, o)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		for i := 0; i < 40; i++ {
			if _, err := receiver.Receive(ctx); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	for i := 0; i < 40; i++ {
		send(t, sender, dispatch(fmt.Sprintf("c%d", i), strings.Repeat("x", 300<<10)))
	}
	// Wait until each previous physical frame has actually been read before
	// advancing. This models slow bandwidth, not a locally unscheduled reader.
	for {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
			return
		case <-clk.frames:
			expected := sender.Stats().FramesWritten
			eventually(t, func() bool { return receiver.Stats().FramesRead >= expected })
			clk.Advance(time.Second)
		case <-ctx.Done():
			t.Fatal("slow-link transfer stalled", sender.Stats(), receiver.Stats())
		}
	}
}

func TestUrgentBeforeSmallBusiness(t *testing.T) {
	track(t)
	a, b := wiretest.Pipe(wiretest.Options{})
	defer b.Close()
	g := &gate{Conn: a, entered: make(chan struct{}, 1), release: make(chan struct{})}
	l := newLink(t, g, wire.DaemonUplink, wire.DaemonDownlink, smallOptions())
	send(t, l, dispatch("large", strings.Repeat("x", 180)))
	wait.Recv(t, g.entered)
	send(t, l, dispatch("small", "x"))
	send(t, l, &circusv1.DaemonDownlink{Payload: &circusv1.DaemonDownlink_GoAway{GoAway: &circusv1.GoAway{}}})
	g.entered <- struct{}{}
	_ = step(t, g, b)
	if step(t, g, b).(*circusv1.DaemonDownlink).GetGoAway() == nil {
		t.Fatal("urgent control did not bypass small business message")
	}
}
func TestLocalTooLargeIsNotProtocolError(t *testing.T) {
	track(t)
	a, b := wiretest.Pipe(wiretest.Options{})
	defer b.Close()
	l := newLink(t, a, wire.DaemonUplink, wire.DaemonDownlink, smallOptions())
	err := testSend(l, contextFor(t), dispatch("x", strings.Repeat("x", 4096)))
	var protocolErr *wire.ProtocolError
	if !errors.Is(err, wire.ErrTooLarge) || errors.As(err, &protocolErr) || l.Err() != nil {
		t.Fatal("local oversize changed link state", err)
	}
	send(t, l, dispatch("ok", "x"))
}
func TestNegotiatedSingleSlot(t *testing.T) {
	track(t)
	a, b := wiretest.Pipe(wiretest.Options{})
	welcome := &circusv1.Welcome{}
	so, err := wire.FillWelcome(welcome, wire.Options{MaxMessageBytes: 4096, MaxFragmentBytes: 64, MaxInFlightMessages: 1, MaxInFlightBytes: 4096, DisableHeartbeat: true})
	if err != nil {
		t.Fatal(err)
	}
	o, err := wire.OptionsFromWelcome(welcome, wire.Options{DisableHeartbeat: true})
	if err != nil {
		t.Fatal(err)
	}
	sender := newLink(t, a, wire.DaemonUplink, wire.DaemonDownlink, so)
	receiver := newLink(t, b, wire.DaemonDownlink, wire.DaemonUplink, o)
	for i := 0; i < 10; i++ {
		send(t, sender, dispatch(fmt.Sprintf("single%d", i), strings.Repeat("x", 200)))
	}
	for i := 0; i < 10; i++ {
		_ = receive(t, receiver)
		if sender.Stats().ActiveMessages > 1 || receiver.Stats().ReassemblyMessages > 1 {
			t.Fatal("negotiated one-slot bound exceeded")
		}
	}
}

func TestOrderingKeyBound(t *testing.T) {
	track(t)
	a, b := wiretest.Pipe(wiretest.Options{})
	defer b.Close()
	l := newLink(t, a, wire.DaemonUplink, wire.DaemonDownlink, smallOptions())
	if err := l.SendAs(contextFor(t), dispatch("c", "x"), wire.Data, strings.Repeat("x", wire.MaxOrderingKeyBytes+1)); !errors.Is(err, wire.ErrTooLarge) {
		t.Fatal(err)
	}
	if l.Err() != nil {
		t.Fatal("oversize local ordering key closed link")
	}
}

// A small key must not retain a caller's arbitrarily large backing string.
func TestOrderingKeyDoesNotRetainBacking(t *testing.T) {
	track(t)
	a, b := wiretest.Pipe(wiretest.Options{AtoB: wiretest.Faults{BlockWrites: true}})
	defer b.Close()
	l := newLink(t, a, wire.DaemonUplink, wire.DaemonDownlink, smallOptions())
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	func() {
		backing := strings.Repeat("k", 32<<20)
		if err := l.SendAs(contextFor(t), dispatch("held", "x"), wire.Data, backing[:2]); err != nil {
			t.Fatal(err)
		}
	}()
	runtime.GC()
	runtime.GC()
	runtime.ReadMemStats(&after)
	if delta := int64(after.HeapAlloc) - int64(before.HeapAlloc); delta > 2<<20 {
		t.Fatalf("queued short key retained %d bytes", delta)
	}
	runtime.KeepAlive(l)
}

// Started messages must keep making progress even alongside small business
// bursts or a fragmented urgent ack. Priority cannot mean indefinite starvation.
func TestMixedTrafficPreservesFragmentProgress(t *testing.T) {
	for _, urgent := range []bool{false, true} {
		t.Run(fmt.Sprint(urgent), func(t *testing.T) {
			track(t)
			clk := &frameClock{Fake: clock.New(), frames: make(chan struct{}, 256)}
			a, b := wiretest.Pipe(wiretest.Options{Clock: clk, AtoB: wiretest.Faults{FrameDelay: time.Second}})
			gate := &gate{Conn: a, entered: make(chan struct{}, 256), release: make(chan struct{}, 256)}
			// At least 32 one-second frames carry each 32 KiB message, spanning
			// more than three 10 s timeouts. Mixed frames cannot starve it for 10 s.
			o := wire.Options{Clock: clk, MaxFragmentBytes: 1024, MaxMessageBytes: 1 << 20, FragmentTimeout: 10 * time.Second, DisableHeartbeat: true}
			s := newLink(t, gate, wire.DaemonDownlink, wire.DaemonUplink, o)
			r := newLink(t, b, wire.DaemonUplink, wire.DaemonDownlink, o)
			send(t, s, result(strings.Repeat("x", 32<<10)))
			wait.Recv(t, gate.entered)
			count := 41
			if urgent {
				count = 2
				output := result(strings.Repeat("y", 32<<10)).(*circusv1.DaemonUplink).GetResult()
				output.CallId = "ack"
				send(t, s, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_CallAck{CallAck: &circusv1.CallAck{CallId: "ack", Outcome: &circusv1.CallAck_Status{Status: &circusv1.CallStatus{CallId: "ack", State: circusv1.CallStatus_STATE_COMPLETED, Result: output}}}}})
			} else {
				for i := 1; i <= 40; i++ {
					send(t, s, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_JobEvent{JobEvent: &circusv1.JobEvent{Seq: uint64(i), OutputPath: strings.Repeat("p", 500)}}})
				}
			}
			for i := 0; i < 256; i++ {
				gate.release <- struct{}{}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				for i := 0; i < count; i++ {
					if _, err := r.Receive(ctx); err != nil {
						done <- err
						return
					}
				}
				done <- nil
			}()
			for {
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
					if r.Err() != nil {
						t.Fatal("healthy mixed traffic timed out", r.Err())
					}
					return
				case <-clk.frames:
					expected := s.Stats().FramesWritten
					eventually(t, func() bool { return r.Stats().FramesRead >= expected || r.Err() != nil })
					if r.Err() != nil {
						t.Fatal(r.Err())
					}
					clk.Advance(time.Second)
				case <-ctx.Done():
					t.Fatal("mixed-traffic transfer stalled", s.Stats(), r.Stats())
				}
			}
		})
	}
}
func TestHeartbeatOutsideQueueBudget(t *testing.T) {
	track(t)
	clk := clock.New()
	a, b := wiretest.Pipe(wiretest.Options{AtoB: wiretest.Faults{BlockWrites: true}})
	defer b.Close()
	m := dispatch("c", "x")
	o := smallOptions()
	o.Clock = clk
	o.DisableHeartbeat = false
	o.MaxQueuedMessages = 4
	o.MaxQueuedBytes = 4 * proto.Size(m)
	l := newLink(t, a, wire.DaemonUplink, wire.DaemonDownlink, o)
	awaitTimers(t, clk, 1)
	for i := 0; i < 4; i++ {
		send(t, l, m)
	}
	for i := 0; i < 2; i++ {
		clk.Advance(5 * time.Second)
		awaitTimers(t, clk, 1)
	}
	if s := l.Stats(); l.Err() != nil || s.QueuedMessages != 4 || s.QueuedBytes != o.MaxQueuedBytes {
		t.Fatalf("heartbeat consumed business capacity: %+v err=%v", s, l.Err())
	}
}

func TestStopBeforeLaterSessionDispatch(t *testing.T) {
	track(t)
	a, b := wiretest.Pipe(wiretest.Options{})
	g := &gate{Conn: a, entered: make(chan struct{}), release: make(chan struct{})}
	s := newLink(t, g, wire.DaemonUplink, wire.DaemonDownlink, smallOptions())
	send(t, s, dispatch("earlier", strings.Repeat("x", 300)))
	send(t, s, &circusv1.DaemonDownlink{Payload: &circusv1.DaemonDownlink_StopSessionJobs{StopSessionJobs: &circusv1.StopSessionJobs{SessionId: "s"}}})
	send(t, s, dispatch("later", "small")) // Same session as Stop; this must survive the stop.
	stopped := false
	for i := 0; i < 15; i++ {
		d := step(t, g, b).(*circusv1.DaemonDownlink)
		if d.GetStopSessionJobs() != nil {
			stopped = true
		}
		if d.GetDispatchCall().GetCall().GetCallId() == "later" {
			if !stopped {
				t.Fatal("later session job overtook StopSessionJobs")
			}
			return
		}
	}
	t.Fatal("later dispatch never delivered")
}

func TestNegotiatedInFlightByteLimit(t *testing.T) {
	track(t)
	a, b := wiretest.Pipe(wiretest.Options{})
	g := &gate{Conn: a, entered: make(chan struct{}), release: make(chan struct{})}
	w := &circusv1.Welcome{}
	if _, err := wire.FillWelcome(w, wire.Options{MaxMessageBytes: 512, MaxFragmentBytes: 64, MaxInFlightBytes: 512, MaxInFlightMessages: 2}); err != nil {
		t.Fatal(err)
	}
	o, err := wire.OptionsFromWelcome(w, wire.Options{DisableHeartbeat: true})
	if err != nil {
		t.Fatal(err)
	}
	s := newLink(t, g, wire.DaemonUplink, wire.DaemonDownlink, o)
	send(t, s, dispatch("a", strings.Repeat("x", 300)))
	send(t, s, dispatch("b", strings.Repeat("x", 300)))
	firstDone := false
	for i := 0; i < 12; i++ {
		f := step(t, g, b).(*circusv1.DaemonDownlink).GetFragment()
		if f.MessageId == 1 && f.Last {
			firstDone = true
		}
		if f.MessageId == 2 {
			if !firstDone {
				t.Fatal("started beyond negotiated bytes")
			}
			return
		}
	}
	t.Fatal("second message never started")
}
