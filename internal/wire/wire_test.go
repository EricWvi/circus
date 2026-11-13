package wire_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/clock"
	"github.com/EricWvi/circus/internal/testutil/wait"
	"github.com/EricWvi/circus/internal/wire"
	"github.com/EricWvi/circus/internal/wire/wiretest"
	"github.com/coder/websocket"
	"google.golang.org/protobuf/proto"
)

func TestRoundTripRoles(t *testing.T) {
	for _, worker := range []bool{false, true} {
		t.Run(map[bool]string{false: "daemon", true: "worker"}[worker], func(t *testing.T) {
			track(t)
			a, b := wiretest.Pipe(wiretest.Options{})
			up, down := wire.DaemonUplink, wire.DaemonDownlink
			var m proto.Message = result(strings.Repeat("abc", 500))
			var reply proto.Message = dispatch("q", strings.Repeat("z", 500))
			if worker {
				up, down = wire.WorkerUplink, wire.WorkerDownlink
				m = &circusv1.WorkerUplink{Payload: &circusv1.WorkerUplink_SubmitCall{SubmitCall: &circusv1.SubmitCall{Device: "laptop", Call: &circusv1.DeviceCall{CallId: "c", SessionId: "s", Tool: &circusv1.DeviceCall_Write{Write: &circusv1.WriteRequest{Content: strings.Repeat("x", 1500)}}}}}}
				reply = &circusv1.WorkerDownlink{Payload: &circusv1.WorkerDownlink_CallUpdate{CallUpdate: &circusv1.CallUpdate{CallId: "c", Update: &circusv1.CallUpdate_Result{Result: &circusv1.DeviceResult{CallId: "c"}}}}}
			}
			left := newLink(t, a, down, up, smallOptions())
			right := newLink(t, b, up, down, smallOptions())
			send(t, left, m)
			send(t, right, reply)
			if !proto.Equal(receive(t, right), m) || !proto.Equal(receive(t, left), reply) {
				t.Fatal("roundtrip mismatch")
			}
			if left.Stats().FramesWritten < 2 {
				t.Fatal("expected fragmentation")
			}
		})
	}
}

func TestInvalidFragmentsClose(t *testing.T) {
	cases := map[string][]*circusv1.Fragment{
		"zero-id":            {{Index: 0, TotalSize: 3, Last: true, Data: []byte("abc")}},
		"oversize":           {{MessageId: 1, TotalSize: 4097, Data: []byte("a")}},
		"oversize-piece":     {{MessageId: 1, TotalSize: 100, Data: bytes.Repeat([]byte("a"), 65)}},
		"first-index":        {{MessageId: 1, Index: 1, TotalSize: 3, Last: true, Data: []byte("abc")}},
		"gap":                {{MessageId: 1, TotalSize: 6, Data: []byte("abc")}, {MessageId: 1, Index: 2, Last: true, Data: []byte("abc")}},
		"duplicate-index":    {{MessageId: 1, TotalSize: 6, Data: []byte("abc")}, {MessageId: 1, Index: 0, Last: true, Data: []byte("abc")}},
		"missing-last":       {{MessageId: 1, TotalSize: 3, Data: []byte("abc")}},
		"premature-last":     {{MessageId: 1, TotalSize: 6, Last: true, Data: []byte("abc")}},
		"continuation-total": {{MessageId: 1, TotalSize: 6, Data: []byte("abc")}, {MessageId: 1, Index: 1, TotalSize: 6, Last: true, Data: []byte("abc")}},
		"empty":              {{MessageId: 1, TotalSize: 6}},
		"reused-id":          {{MessageId: 1, TotalSize: 2, Last: true, Data: []byte{0x0a, 0}}, {MessageId: 1, TotalSize: 2, Last: true, Data: []byte{0x0a, 0}}},
		"nested":             {{MessageId: 1, TotalSize: 2, Last: true, Data: []byte{0x1a, 0}}},
		"malformed":          {{MessageId: 1, TotalSize: 1, Last: true, Data: []byte{0xff}}},
		"aggregate":          {{MessageId: 1, TotalSize: 3000, Data: []byte("a")}, {MessageId: 2, TotalSize: 3000, Data: []byte("b")}},
		"count":              {{MessageId: 1, TotalSize: 6, Data: []byte("a")}, {MessageId: 2, TotalSize: 6, Data: []byte("b")}},
	}
	for name, fs := range cases {
		t.Run(name, func(t *testing.T) {
			track(t)
			a, b := wiretest.Pipe(wiretest.Options{})
			t.Cleanup(func() { _ = a.Close() })
			o := smallOptions()
			o.MaxInFlightBytes = 8192
			if name == "aggregate" {
				o.MaxInFlightBytes = 4096
			}
			if name == "count" {
				o.MaxInFlightMessages = 1
			}
			l := newLink(t, b, wire.DaemonUplink, wire.DaemonDownlink, o)
			for _, f := range fs {
				frame(t, a, fragment(f))
			}
			wait.Recv(t, l.Done())
			var pe *wire.ProtocolError
			if !errors.As(l.Err(), &pe) {
				t.Fatalf("error=%v", l.Err())
			}
		})
	}
}

func TestMissingTailExpiresDespiteHeartbeats(t *testing.T) {
	track(t)
	clk := clock.New()
	a, b := wiretest.Pipe(wiretest.Options{})
	t.Cleanup(func() { _ = a.Close() })
	o := smallOptions()
	o.Clock = clk
	l := newLink(t, b, wire.DaemonUplink, wire.DaemonDownlink, o)
	awaitTimers(t, clk, 1)
	frame(t, a, fragment(&circusv1.Fragment{MessageId: 1, TotalSize: 6, Data: []byte("abc")}))
	eventually(t, func() bool { return l.Stats().ReassemblyMessages == 1 })
	for i := 0; i < 3; i++ {
		frame(t, a, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_Heartbeat{Heartbeat: &circusv1.Heartbeat{Seq: uint64(i + 1)}}})
		eventually(t, func() bool { return l.Stats().HeartbeatsRead == uint64(i+1) })
		clk.Advance(5 * time.Second)
		if i < 2 {
			awaitTimers(t, clk, 1)
		}
	}
	wait.Recv(t, l.Done())
	var pe *wire.ProtocolError
	if !errors.As(l.Err(), &pe) || !strings.Contains(pe.Reason, "missing fragment") {
		t.Fatal(l.Err())
	}
}

func TestMissingTailAtEOF(t *testing.T) {
	track(t)
	a, b := wiretest.Pipe(wiretest.Options{})
	l := newLink(t, b, wire.DaemonUplink, wire.DaemonDownlink, smallOptions())
	frame(t, a, fragment(&circusv1.Fragment{MessageId: 1, TotalSize: 6, Data: []byte("abc")}))
	eventually(t, func() bool { return l.Stats().ReassemblyMessages == 1 })
	_ = a.Close()
	wait.Recv(t, l.Done())
	var pe *wire.ProtocolError
	if !errors.As(l.Err(), &pe) {
		t.Fatal(l.Err())
	}
}

func TestPriorityInterleavingAndCancelOrder(t *testing.T) {
	track(t)
	a, b := wiretest.Pipe(wiretest.Options{})
	t.Cleanup(func() { _ = b.Close() })
	g := &gate{Conn: a, entered: make(chan struct{}, 1), release: make(chan struct{})}
	l := newLink(t, g, wire.DaemonUplink, wire.DaemonDownlink, smallOptions())
	d1, d2 := dispatch("c", strings.Repeat("a", 180)), dispatch("other", strings.Repeat("b", 180))
	send(t, l, d1)
	wait.Recv(t, g.entered)
	send(t, l, d2)
	send(t, l, &circusv1.DaemonDownlink{Payload: &circusv1.DaemonDownlink_CancelCall{CancelCall: &circusv1.CancelCall{CallId: "c"}}})
	send(t, l, &circusv1.DaemonDownlink{Payload: &circusv1.DaemonDownlink_GoAway{GoAway: &circusv1.GoAway{Reason: circusv1.GoAway_REASON_SHUTTING_DOWN}}})
	// First frame was already in flight; return its entry token to step.
	g.entered <- struct{}{}
	first := step(t, g, b).(*circusv1.DaemonDownlink).GetFragment()
	if first == nil || first.Index != 0 {
		t.Fatal("expected first fragment")
	}
	if step(t, g, b).(*circusv1.DaemonDownlink).GetGoAway() == nil {
		t.Fatal("GoAway did not take priority")
	}
	next := step(t, g, b).(*circusv1.DaemonDownlink).GetFragment()
	if next == nil || next.MessageId == first.MessageId {
		t.Fatal("messages did not interleave")
	}
	doneFirst, cancelled := false, false
	for i := 0; i < 20 && !cancelled; i++ {
		m := step(t, g, b).(*circusv1.DaemonDownlink)
		if f := m.GetFragment(); f != nil && f.MessageId == first.MessageId && f.Last {
			doneFirst = true
		}
		if m.GetCancelCall() != nil {
			if !doneFirst {
				t.Fatal("cancel overtook dispatch")
			}
			cancelled = true
		}
	}
	if !cancelled {
		t.Fatal("cancel never delivered")
	}
}

func TestJobEventsRemainOrdered(t *testing.T) {
	track(t)
	a, b := wiretest.Pipe(wiretest.Options{})
	left := newLink(t, a, wire.DaemonDownlink, wire.DaemonUplink, smallOptions())
	right := newLink(t, b, wire.DaemonUplink, wire.DaemonDownlink, smallOptions())
	for i := 1; i <= 2; i++ {
		send(t, left, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_JobEvent{JobEvent: &circusv1.JobEvent{Seq: uint64(i), OutputPath: strings.Repeat("x", 200/i)}}})
	}
	for i := 1; i <= 2; i++ {
		if receive(t, right).(*circusv1.DaemonUplink).GetJobEvent().Seq != uint64(i) {
			t.Fatal("event order changed")
		}
	}
}

func TestHeartbeatAnyMessageAndHalfOpen(t *testing.T) {
	track(t)
	clk := clock.New()
	a, b := wiretest.Pipe(wiretest.Options{AtoB: wiretest.Faults{Silence: true}})
	o := smallOptions()
	o.Clock = clk
	o.DisableHeartbeat = false
	left := newLink(t, a, wire.DaemonDownlink, wire.DaemonUplink, o)
	right := newLink(t, b, wire.DaemonUplink, wire.DaemonDownlink, o)
	awaitTimers(t, clk, 2)
	// Any application frame refreshes left, while right sees only silence.
	for i := 0; i < 2; i++ {
		clk.Advance(5 * time.Second)
		send(t, right, dispatch("c", "x"))
		_ = receive(t, left)
		eventually(t, func() bool { return left.Stats().HeartbeatsWritten >= uint64(i+1) })
		awaitTimers(t, clk, 2)
	}
	clk.Advance(5 * time.Second)
	wait.Recv(t, right.Done())
	if !strings.Contains(right.Err().Error(), "heartbeat timeout") {
		t.Fatal(right.Err())
	}
}

func TestHeartbeatClockAndActivation(t *testing.T) {
	track(t)
	clk := clock.New()
	a, b := wiretest.Pipe(wiretest.Options{})
	t.Cleanup(func() { _ = a.Close() })
	o := smallOptions()
	o.Clock = clk
	l := newLink(t, b, wire.DaemonUplink, wire.DaemonDownlink, o)
	awaitTimers(t, clk, 1)
	clk.Advance(time.Hour)
	awaitTimers(t, clk, 1)
	if l.Err() != nil {
		t.Fatal("heartbeat active before activation")
	}
	l.StartHeartbeat()
	l.StartHeartbeat()
	clk.Advance(5 * time.Second)
	awaitTimers(t, clk, 1)
	raw, err := a.ReadMessage(contextFor(t))
	if err != nil {
		t.Fatal(err)
	}
	m, err := wire.Decode(wire.DaemonDownlink, raw, 64)
	if err != nil || m.(*circusv1.DaemonDownlink).GetHeartbeat() == nil {
		t.Fatal("missing heartbeat", err)
	}
	frame(t, a, result("alive"))
	_ = receive(t, l)
	clk.Advance(10 * time.Second)
	awaitTimers(t, clk, 1)
	if l.Err() != nil {
		t.Fatal("application traffic did not refresh liveness")
	}
	clk.Advance(5 * time.Second)
	wait.Recv(t, l.Done())
}

func TestSendReceiveBoundsAndCancellation(t *testing.T) {
	track(t)
	a, b := wiretest.Pipe(wiretest.Options{AtoB: wiretest.Faults{BlockWrites: true}})
	t.Cleanup(func() { _ = b.Close() })
	o := smallOptions()
	o.MaxQueuedBytes = 100
	l := newLink(t, a, wire.DaemonUplink, wire.DaemonDownlink, o)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !errors.Is(testSend(l, ctx, dispatch("c", "x")), context.Canceled) {
		t.Fatal("send ignored cancellation")
	}
	if _, err := l.Receive(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("receive ignored cancellation")
	}
	if err := testSend(l, contextFor(t), result("x")); err == nil {
		t.Fatal("accepted wrong direction")
	}
	if err := testSend(l, contextFor(t), dispatch("c", strings.Repeat("x", 4096))); err == nil {
		t.Fatal("accepted oversized message")
	}
	if err := testSend(l, contextFor(t), &circusv1.DaemonDownlink{Payload: &circusv1.DaemonDownlink_Heartbeat{Heartbeat: &circusv1.Heartbeat{}}}); err == nil {
		t.Fatal("accepted reserved heartbeat")
	}
	send(t, l, dispatch("c", strings.Repeat("x", 60)))
	if err := testSend(l, contextFor(t), dispatch("q", strings.Repeat("x", 60))); !errors.Is(err, wire.ErrQueueFull) {
		t.Fatal("queue did not close", err)
	}
	wait.Recv(t, l.Done())
}

func TestMalformedEnvelopeCloses(t *testing.T) {
	for _, data := range [][]byte{nil, {0xff}, {0x78, 0}, bytes.Repeat([]byte("x"), 180)} {
		t.Run(string(rune(len(data)+'a')), func(t *testing.T) {
			track(t)
			a, b := wiretest.Pipe(wiretest.Options{})
			t.Cleanup(func() { _ = a.Close() })
			l := newLink(t, b, wire.DaemonUplink, wire.DaemonDownlink, smallOptions())
			if err := a.WriteMessage(contextFor(t), data); err != nil {
				t.Fatal(err)
			}
			select {
			case <-l.Done():
			case <-contextFor(t).Done():
				t.Fatal("malformed envelope did not close link")
			}
			var pe *wire.ProtocolError
			if !errors.As(l.Err(), &pe) {
				t.Fatal(l.Err())
			}
		})
	}
}

func TestWebSocketAdapter(t *testing.T) {
	for _, mode := range []string{"binary", "text", "over-limit"} {
		t.Run(mode, func(t *testing.T) {
			track(t)
			serverDone := make(chan struct{})
			errCh := make(chan error, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(serverDone)
				raw, err := wire.AcceptWebSocket(w, r, nil)
				if err != nil {
					errCh <- err
					return
				}
				ws := raw
				ws.SetReadLimit(64 + wire.FrameOverhead)
				defer ws.Close()
				data, err := ws.ReadMessage(r.Context())
				if mode != "binary" {
					if err == nil {
						errCh <- errors.New("accepted invalid frame")
					}
					return
				}
				if err != nil {
					errCh <- err
					return
				}
				errCh <- ws.WriteMessage(r.Context(), data)
			}))
			t.Cleanup(srv.Close)
			c, _, err := websocket.Dial(contextFor(t), "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer c.CloseNow()
			typ, data := websocket.MessageBinary, []byte("abc")
			if mode == "text" {
				typ = websocket.MessageText
			}
			if mode == "over-limit" {
				data = bytes.Repeat([]byte("x"), 300)
			}
			if err := c.Write(contextFor(t), typ, data); err != nil {
				t.Fatal(err)
			}
			if mode == "binary" {
				_, got, err := c.Read(contextFor(t))
				if err != nil || !bytes.Equal(got, data) {
					t.Fatal("binary roundtrip", err)
				}
			}
			wait.Recv(t, serverDone)
			select {
			case err := <-errCh:
				if err != nil {
					t.Fatal(err)
				}
			default:
			}
		})
	}
}

// notifyingClock retains exact delay assertions for wiretest's fault injector.
type notifyingClock struct {
	*clock.Fake
	registered chan time.Duration
}

func (c *notifyingClock) After(d time.Duration) <-chan time.Time {
	ch := c.Fake.After(d)
	c.registered <- d
	return ch
}

func awaitRegistered(t *testing.T, c *notifyingClock) time.Duration {
	t.Helper()
	return wait.Recv(t, c.registered)
}

func TestFaultInjectionPipe(t *testing.T) {
	t.Run("disconnect", func(t *testing.T) {
		track(t)
		a, b := wiretest.Pipe(wiretest.Options{AtoB: wiretest.Faults{DisconnectAfter: 1}})
		defer a.Close()
		readCtx := contextFor(t)
		readDone := make(chan struct{})
		go func() {
			_, _ = b.ReadMessage(readCtx)
			close(readDone)
		}()
		if err := a.WriteMessage(contextFor(t), []byte("ok")); err != nil {
			t.Fatal(err)
		}
		wait.Recv(t, readDone)
		if _, err := b.ReadMessage(contextFor(t)); !errors.Is(err, io.EOF) {
			t.Fatal("not disconnected after N messages", err)
		}
		if err := a.WriteMessage(contextFor(t), []byte("no")); !errors.Is(err, io.EOF) {
			t.Fatal(err)
		}
		if _, err := b.ReadMessage(contextFor(t)); !errors.Is(err, io.EOF) {
			t.Fatal(err)
		}
	})
	t.Run("delay-rate-truncate", func(t *testing.T) {
		track(t)
		clk := &notifyingClock{Fake: clock.New(), registered: make(chan time.Duration, 1)}
		a, b := wiretest.Pipe(wiretest.Options{Clock: clk, AtoB: wiretest.Faults{FrameDelay: time.Second, BytesPerSecond: 3, Truncate: true, KeepBytes: 2}})
		defer a.Close()
		done := make(chan error, 1)
		writeCtx := contextFor(t)
		go func() { done <- a.WriteMessage(writeCtx, []byte("abc")) }()
		if delay := awaitRegistered(t, clk); delay != 2*time.Second {
			t.Fatalf("delay+rate = %s", delay)
		}
		clk.Advance(2*time.Second - time.Millisecond)
		ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
		if _, err := b.ReadMessage(ctx); !errors.Is(err, context.DeadlineExceeded) {
			cancel()
			t.Fatal("delivered before delay elapsed", err)
		}
		cancel()
		clk.Advance(time.Millisecond)
		data, err := b.ReadMessage(contextFor(t))
		if err != nil || string(data) != "ab" {
			t.Fatal("truncated delivery", string(data), err)
		}
		if err := wait.Recv(t, done); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("close-unblocks", func(t *testing.T) {
		track(t)
		a, b := wiretest.Pipe(wiretest.Options{AtoB: wiretest.Faults{BlockWrites: true}})
		done := make(chan error, 1)
		writeCtx := contextFor(t)
		go func() { done <- a.WriteMessage(writeCtx, []byte("a")) }()
		_ = b.Close()
		if err := wait.Recv(t, done); !errors.Is(err, io.EOF) {
			t.Fatal(err)
		}
	})
}

func TestInterleavedReassembly(t *testing.T) {
	track(t)
	a, b := wiretest.Pipe(wiretest.Options{})
	t.Cleanup(func() { _ = a.Close() })
	l := newLink(t, b, wire.DaemonUplink, wire.DaemonDownlink, smallOptions())
	first, second := result(strings.Repeat("a", 80)), result(strings.Repeat("b", 80))
	one, _ := proto.Marshal(first)
	two, _ := proto.Marshal(second)
	for i, data := range [][]byte{one, two} {
		frame(t, a, fragment(&circusv1.Fragment{MessageId: uint64(i + 1), TotalSize: uint64(len(data)), Data: data[:60]}))
	}
	frame(t, a, fragment(&circusv1.Fragment{MessageId: 2, Index: 1, Last: true, Data: two[60:]}))
	if !proto.Equal(receive(t, l), second) {
		t.Fatal("second assembly content")
	}
	frame(t, a, fragment(&circusv1.Fragment{MessageId: 1, Index: 1, Last: true, Data: one[60:]}))
	if !proto.Equal(receive(t, l), first) {
		t.Fatal("first assembly content")
	}
	eventually(t, func() bool { return l.Stats().ReassemblyBytes == 0 })
}

func TestTruncatedPipeClosesLink(t *testing.T) {
	track(t)
	a, b := wiretest.Pipe(wiretest.Options{AtoB: wiretest.Faults{Truncate: true, KeepBytes: 1}})
	t.Cleanup(func() { _ = a.Close() })
	l := newLink(t, b, wire.DaemonUplink, wire.DaemonDownlink, smallOptions())
	frame(t, a, result("truncated"))
	wait.Recv(t, l.Done())
	var pe *wire.ProtocolError
	if !errors.As(l.Err(), &pe) {
		t.Fatal(l.Err())
	}
}

func TestInvalidOptions(t *testing.T) {
	track(t)
	for _, o := range []wire.Options{
		{MaxMessageBytes: -1},
		{MaxFragmentBytes: -1},
		{MaxMessageBytes: 1},
		{MaxInFlightBytes: -1},
		{MaxInFlightMessages: -1},
		{MaxQueuedMessages: -1},
		{HeartbeatInterval: 15 * time.Second, HeartbeatTimeout: 5 * time.Second},
	} {
		a, b := wiretest.Pipe(wiretest.Options{})
		l, err := wire.New(a, wire.DaemonControlSide, o)
		if err == nil {
			_ = l.Close()
			t.Error("accepted invalid options")
		}
		_ = a.Close()
		_ = b.Close()
	}
}

func TestMessageCountBudget(t *testing.T) {
	track(t)
	a, b := wiretest.Pipe(wiretest.Options{AtoB: wiretest.Faults{BlockWrites: true}})
	t.Cleanup(func() { _ = b.Close() })
	o := smallOptions()
	o.MaxQueuedMessages = 1
	l := newLink(t, a, wire.DaemonUplink, wire.DaemonDownlink, o)
	send(t, l, dispatch("1", "a"))
	if err := testSend(l, contextFor(t), dispatch("2", "b")); !errors.Is(err, wire.ErrQueueFull) {
		t.Fatal(err)
	}
	wait.Recv(t, l.Done())
}

func TestPropagationPipelines(t *testing.T) {
	track(t)
	clk := &notifyingClock{Fake: clock.New(), registered: make(chan time.Duration, 2)}
	a, b := wiretest.Pipe(wiretest.Options{Clock: clk, AtoB: wiretest.Faults{Delay: time.Second}, BufferMessages: 8})
	defer a.Close()
	for i := 0; i < 8; i++ {
		if err := a.WriteMessage(contextFor(t), []byte{byte(i)}); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan error, 1)
	readCtx := contextFor(t)
	go func() {
		for i := 0; i < 8; i++ {
			data, err := b.ReadMessage(readCtx)
			if err != nil {
				done <- err
				return
			}
			if len(data) != 1 || data[0] != byte(i) {
				done <- fmt.Errorf("propagation order")
				return
			}
		}
		done <- nil
	}()
	if delay := awaitRegistered(t, clk); delay != time.Second {
		t.Fatal(delay)
	}
	clk.Advance(time.Second)
	if err := wait.Recv(t, done); err != nil {
		t.Fatal(err)
	}
	if a.Writes() != 8 {
		t.Fatal("propagation serialized writes")
	}
}
func TestBlockedWritesCancel(t *testing.T) {
	track(t)
	a, b := wiretest.Pipe(wiretest.Options{AtoB: wiretest.Faults{BlockWrites: true}})
	defer a.Close()
	defer b.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.WriteMessage(ctx, []byte("x")); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if a.Writes() != 0 {
		t.Fatal("blocked write was accepted")
	}
}

func TestWorkerJobIdentityAndProtoEnums(t *testing.T) {
	track(t)
	a, b := wiretest.Pipe(wiretest.Options{})
	s := newLink(t, a, wire.WorkerUplink, wire.WorkerDownlink, wire.Options{})
	r := newLink(t, b, wire.WorkerDownlink, wire.WorkerUplink, wire.Options{})
	for _, device := range []string{"laptop", "desktop"} {
		m := &circusv1.WorkerDownlink{Payload: &circusv1.WorkerDownlink_JobEvent{JobEvent: &circusv1.WorkerJobEvent{Device: device, BootId: "boot", Event: &circusv1.JobEvent{JobId: "012345abcdef", State: circusv1.JobEvent_STATE_LOST}}}}
		send(t, s, m)
		if !proto.Equal(receive(t, r), m) {
			t.Fatal("job machine/boot identity lost")
		}
	}
	m := &circusv1.WorkerDownlink{Payload: &circusv1.WorkerDownlink_CallUpdate{CallUpdate: &circusv1.CallUpdate{CallId: "c", Update: &circusv1.CallUpdate_Rejected_{Rejected: &circusv1.CallUpdate_Rejected{Code: circusv1.CallUpdate_Rejected_CODE_UNKNOWN_DEVICE, Message: "unknown"}}}}}
	send(t, s, m)
	if !proto.Equal(receive(t, r), m) {
		t.Fatal("rejection enum lost")
	}
	if (&circusv1.EnrollResponse{PrincipalId: "worker"}).GetPrincipalId() != "worker" {
		t.Fatal("principal naming")
	}
}
