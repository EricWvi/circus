package wire_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/EricWvi/circus/internal/clock"
	"github.com/coder/websocket"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/wire"
	"github.com/EricWvi/circus/internal/wire/wiretest"
)

// Reuses review R4: query and session stop must observe earlier dispatch.
func TestReviewControlOrdering(t *testing.T) {
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

// Review R1: a default pair must survive the full 200 x 300 KiB burst.
func TestReviewBoundedStartedBurst(t *testing.T) {
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
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if receiver.Err() != nil {
		t.Fatal(receiver.Err())
	}
}

// Review R7, scaled for routine runs; default-timeout version below uses a fake.
func TestReviewSlowFragmentProgress(t *testing.T) {
	track(t)
	a, b := wiretest.Pipe(wiretest.Options{AtoB: wiretest.Faults{FrameDelay: 10 * time.Millisecond}})
	o := wire.Options{DisableHeartbeat: true, MaxFragmentBytes: 1024, MaxMessageBytes: 1 << 20, FragmentTimeout: 200 * time.Millisecond}
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
func TestReviewDefaultSlowLink(t *testing.T) {
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

// Review R2: reserve two 64 MiB declarations, but allocate only received blocks.
func TestReviewIncrementalReassemblyAllocation(t *testing.T) {
	track(t)
	a, b := wiretest.Pipe(wiretest.Options{})
	defer a.Close()
	l := newLink(t, b, wire.DaemonDownlink, wire.DaemonUplink, wire.Options{DisableHeartbeat: true})
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for id := uint64(1); id <= 2; id++ {
		frame(t, a, &circusv1.DaemonDownlink{Payload: &circusv1.DaemonDownlink_Fragment{Fragment: &circusv1.Fragment{MessageId: id, TotalSize: 64 << 20, Data: []byte{0x52}}}})
	}
	eventually(t, func() bool { return l.Stats().ReassemblyMessages == 2 })
	runtime.ReadMemStats(&after)
	if delta := after.TotalAlloc - before.TotalAlloc; delta > 2<<20 {
		t.Fatalf("tiny headers allocated %d bytes", delta)
	}
	if s := l.Stats(); s.ReassemblyBytes != 128<<20 || s.ReassemblyAllocatedBytes > 128<<10 {
		t.Fatalf("reservation/real buffers: %+v", s)
	}
}

func snapshotAttack(count int) []byte {
	body := bytes.Repeat([]byte{0x12, 0}, count)
	data := protowire.AppendTag(nil, 14, protowire.BytesType)
	return protowire.AppendBytes(data, body)
}

// Review R3: test rejection BEFORE the generated decoder allocates jobs.
func TestReviewDecodeAmplification(t *testing.T) {
	track(t)
	for _, count := range []int{wire.MaxSnapshotJobs + 1, 8192, 1 << 20, 4 << 20} {
		data := snapshotAttack(count)
		_, _ = wire.Decode(wire.DaemonUplink, data, wire.DefaultMaxMessageBytes)
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		m, err := wire.Decode(wire.DaemonUplink, data, wire.DefaultMaxMessageBytes)
		runtime.ReadMemStats(&after)
		runtime.KeepAlive(m)
		if err == nil {
			t.Fatalf("accepted %d snapshot entries", count)
		}
		if n := after.TotalAlloc - before.TotalAlloc; n > 512<<10 {
			t.Fatalf("rejected packet allocated %d bytes", n)
		}
	}
	// Exercise the exact repeated-element boundary within the byte limit.
	data := snapshotAttack(wire.MaxSnapshotJobs)
	if _, err := wire.Decode(wire.DaemonUplink, data, wire.DefaultMaxMessageBytes); err != nil {
		t.Fatal(err)
	}
}
func TestReviewPayloadByteLimits(t *testing.T) {
	track(t)
	m := &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_JobEvent{JobEvent: &circusv1.JobEvent{OutputPath: strings.Repeat("x", wire.MaxMetadataBytes+1)}}}
	b, _ := proto.Marshal(m)
	if _, err := wire.Decode(wire.DaemonUplink, b, wire.DefaultMaxMessageBytes); err == nil {
		t.Fatal("metadata budget ignored")
	}
	// The negotiated envelope cap still applies to tool output, independently
	// of its node/field budgets. Use a small negotiation to keep this test cheap.
	b, _ = proto.Marshal(result(strings.Repeat("x", 1<<20)))
	if _, err := wire.Decode(wire.DaemonUplink, b, 1<<20); err == nil {
		t.Fatal("negotiated result budget ignored")
	}
}

func TestResultCarriersUseNegotiatedLimit(t *testing.T) {
	// Exercise every wrapper that can return retained tool output. In particular,
	// an accepted duplicate ack can carry a completed CallStatus with its result.
	output := result(strings.Repeat("x", 3<<20)).(*circusv1.DaemonUplink).GetResult()
	status := &circusv1.CallStatus{CallId: "c", State: circusv1.CallStatus_STATE_COMPLETED, Result: output}
	cases := []struct {
		name    string
		side    wire.Side
		message proto.Message
	}{
		{"result", wire.DaemonSide, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_Result{Result: output}}},
		{"status", wire.DaemonSide, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_CallStatus{CallStatus: status}}},
		{"ack", wire.DaemonSide, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_CallAck{CallAck: &circusv1.CallAck{CallId: "c", Outcome: &circusv1.CallAck_Status{Status: status}}}}},
		{"worker", wire.WorkerControlSide, &circusv1.WorkerDownlink{Payload: &circusv1.WorkerDownlink_CallUpdate{CallUpdate: &circusv1.CallUpdate{CallId: "c", Update: &circusv1.CallUpdate_Result{Result: output}}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			track(t)
			a, b := wiretest.Pipe(wiretest.Options{})
			sender, err := wire.New(a, tc.side, wire.Options{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = sender.Close() })
			peer := wire.DaemonControlSide
			if tc.side == wire.WorkerControlSide {
				peer = wire.WorkerSide
			}
			receiver, err := wire.New(b, peer, wire.Options{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = receiver.Close() })
			if err := sender.Send(contextFor(t), tc.message, wire.Data, "c"); err != nil {
				t.Fatal(err)
			}
			if got := receive(t, receiver); !proto.Equal(got, tc.message) {
				t.Fatal("result wrapper mismatch")
			}
		})
	}
}

func TestStatusOnlyPayloadsStaySmall(t *testing.T) {
	track(t)
	text := strings.Repeat("x", wire.MaxMetadataBytes+1)
	cases := []struct {
		kind    wire.Kind
		message proto.Message
	}{
		{wire.DaemonUplink, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_CallStatus{CallStatus: &circusv1.CallStatus{CallId: text}}}},
		{wire.DaemonUplink, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_CallStatus{CallStatus: &circusv1.CallStatus{CallId: text, Result: &circusv1.DeviceResult{}}}}},
		{wire.DaemonUplink, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_CallAck{CallAck: &circusv1.CallAck{Outcome: &circusv1.CallAck_Rejection{Rejection: &circusv1.Rejection{Message: text}}}}}},
		{wire.WorkerDownlink, &circusv1.WorkerDownlink{Payload: &circusv1.WorkerDownlink_CallUpdate{CallUpdate: &circusv1.CallUpdate{Update: &circusv1.CallUpdate_Unknown_{Unknown: &circusv1.CallUpdate_Unknown{Reason: text}}}}}},
	}
	for _, tc := range cases {
		data, err := proto.Marshal(tc.message)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := wire.Decode(tc.kind, data, wire.DefaultMaxMessageBytes); err == nil {
			t.Fatal("status-only payload exceeded metadata cap")
		}
	}
}
func TestReviewShutdownAndEOFDrain(t *testing.T) {
	track(t)
	for i := 0; i < 100; i++ {
		a, b := wiretest.Pipe(wiretest.Options{})
		s, err := wire.New(a, wire.DaemonControlSide, wire.Options{DisableHeartbeat: true})
		if err != nil {
			t.Fatal(err)
		}
		r, err := wire.New(b, wire.DaemonSide, wire.Options{DisableHeartbeat: true})
		if err != nil {
			t.Fatal(err)
		}
		m := &circusv1.DaemonDownlink{Payload: &circusv1.DaemonDownlink_Reject{Reject: &circusv1.Reject{Code: circusv1.Reject_CODE_REVOKED}}}
		if err := testSend(s, contextFor(t), m); err != nil {
			t.Fatal(err)
		}
		if err := s.Shutdown(contextFor(t)); err != nil {
			t.Fatal(err)
		}
		select {
		case <-r.Done():
		case <-contextFor(t).Done():
			t.Fatal("receiver did not see EOF")
		}
		got, err := r.Receive(contextFor(t))
		if err != nil || !proto.Equal(got, m) {
			t.Fatal("Reject lost after EOF", err)
		}
		if _, err = r.Receive(contextFor(t)); err == nil {
			t.Fatal("missing terminal error")
		}
		_ = s.Close()
		_ = r.Close()
	}
	a, b := wiretest.Pipe(wiretest.Options{})
	r := newLink(t, b, wire.DaemonDownlink, wire.DaemonUplink, wire.Options{DisableHeartbeat: true})
	m := &circusv1.DaemonDownlink{Payload: &circusv1.DaemonDownlink_GoAway{GoAway: &circusv1.GoAway{ReconnectAfterMs: 30000}}}
	frame(t, a, m)
	_ = a.Close()
	<-r.Done()
	if got := receive(t, r); !proto.Equal(got, m) {
		t.Fatal("GoAway lost")
	}
}
func TestReviewShutdownDeadline(t *testing.T) {
	track(t)
	a, b := wiretest.Pipe(wiretest.Options{AtoB: wiretest.Faults{BlockWrites: true}})
	defer b.Close()
	l := newLink(t, a, wire.DaemonUplink, wire.DaemonDownlink, smallOptions())
	send(t, l, dispatch("c", "x"))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := l.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if err := testSend(l, contextFor(t), dispatch("c", "x")); err == nil {
		t.Fatal("accepted send after shutdown")
	}
	if l.Stats().QueuedBytes != 0 {
		t.Fatal("aborted queue not released")
	}
}

// Review R6: ordinary bursts do not close a slow reader.
func TestReviewSlowConsumerBurst(t *testing.T) {
	track(t)
	a, b := wiretest.Pipe(wiretest.Options{})
	s := newLink(t, a, wire.DaemonDownlink, wire.DaemonUplink, wire.Options{})
	r := newLink(t, b, wire.DaemonUplink, wire.DaemonDownlink, wire.Options{})
	for i := 1; i <= 40; i++ {
		send(t, s, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_JobEvent{JobEvent: &circusv1.JobEvent{Seq: uint64(i)}}})
	}
	for i := 1; i <= 40; i++ {
		if receive(t, r).(*circusv1.DaemonUplink).GetJobEvent().Seq != uint64(i) {
			t.Fatal("burst order")
		}
		time.Sleep(2 * time.Millisecond)
	}
	if r.Err() != nil {
		t.Fatal(r.Err())
	}
}
func TestReviewByteBackpressurePausesDeadlines(t *testing.T) {
	track(t)
	clk := &notifyingClock{Fake: clock.New(), registered: make(chan time.Duration, 32)}
	a, b := wiretest.Pipe(wiretest.Options{})
	defer a.Close()
	o := smallOptions()
	o.Clock = clk
	o.DisableHeartbeat = false
	o.Budget = minBudget(t, o)
	o.MaxConsumerStall = 2 * time.Minute
	l := newLink(t, b, wire.DaemonUplink, wire.DaemonDownlink, o)
	awaitTimer(t, clk)
	// Keep a partial message alive across the local-consumer pause too.
	frame(t, a, fragment(&circusv1.Fragment{MessageId: 1, TotalSize: 2, Data: []byte{0x0a}}))
	accepted := fillUntilBlocked(t, a, l, result("1"))
	clk.Advance(time.Minute)
	awaitTimer(t, clk)
	if l.Err() != nil {
		t.Fatal("local consumer pause counted as peer timeout", l.Err())
	}
	for i := 0; i <= accepted; i++ {
		_ = receive(t, l)
	}
	eventually(t, func() bool { return !l.Stats().ReadBlocked })
	frame(t, a, fragment(&circusv1.Fragment{MessageId: 1, Index: 1, Last: true, Data: []byte{0}}))
	_ = receive(t, l)
	eventually(t, func() bool { return l.Stats().ReceivedBytes == 0 && l.Stats().ReassemblyBytes == 0 })
}

type quietClock struct{ *clock.Fake }

func (c quietClock) After(time.Duration) <-chan time.Time { return make(chan time.Time) }
func TestReviewReadPathMissingFragment(t *testing.T) {
	track(t)
	clk := quietClock{clock.New()}
	a, b := wiretest.Pipe(wiretest.Options{})
	defer a.Close()
	o := smallOptions()
	o.Clock = clk
	l := newLink(t, b, wire.DaemonUplink, wire.DaemonDownlink, o)
	frame(t, a, fragment(&circusv1.Fragment{MessageId: 1, TotalSize: 2, Data: []byte{0x0a}}))
	eventually(t, func() bool { return l.Stats().ReassemblyMessages == 1 })
	clk.Advance(15 * time.Second)
	frame(t, a, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_Heartbeat{Heartbeat: &circusv1.Heartbeat{}}})
	select {
	case <-l.Done():
	case <-contextFor(t).Done():
		t.Fatal("read-path expiry missing")
	}
	if !strings.Contains(l.Err().Error(), "missing fragment") {
		t.Fatal(l.Err())
	}
}
func TestReviewUnfragmentedLimit(t *testing.T) {
	track(t)
	a, b := wiretest.Pipe(wiretest.Options{})
	defer a.Close()
	l := newLink(t, b, wire.DaemonUplink, wire.DaemonDownlink, smallOptions())
	m := result(strings.Repeat("x", 80))
	frame(t, a, m)
	select {
	case <-l.Done():
	case <-time.After(100 * time.Millisecond):
		t.Fatal("oversized unfragmented envelope accepted")
	}
}
func TestReviewReceivedAccounting(t *testing.T) {
	track(t)
	a, b := wiretest.Pipe(wiretest.Options{})
	defer a.Close()
	l := newLink(t, b, wire.DaemonUplink, wire.DaemonDownlink, smallOptions())
	for i := 0; i < 2; i++ {
		frame(t, a, result("m"))
		eventually(t, func() bool { return l.Stats().ReceivedBytes > 0 })
		_ = receive(t, l)
		if l.Stats().ReceivedBytes != 0 || l.Stats().ReceivedMessages != 0 {
			t.Fatal("receive charge not released", l.Stats())
		}
	}
}

func TestReviewUrgentBeforeSmallBusiness(t *testing.T) {
	track(t)
	a, b := wiretest.Pipe(wiretest.Options{})
	defer b.Close()
	g := &gate{Conn: a, entered: make(chan struct{}, 1), release: make(chan struct{})}
	l := newLink(t, g, wire.DaemonUplink, wire.DaemonDownlink, smallOptions())
	send(t, l, dispatch("large", strings.Repeat("x", 180)))
	<-g.entered
	send(t, l, dispatch("small", "x"))
	send(t, l, &circusv1.DaemonDownlink{Payload: &circusv1.DaemonDownlink_GoAway{GoAway: &circusv1.GoAway{}}})
	g.entered <- struct{}{}
	_ = step(t, g, b)
	if step(t, g, b).(*circusv1.DaemonDownlink).GetGoAway() == nil {
		t.Fatal("urgent control did not bypass small business message")
	}
}
func TestReviewLocalTooLargeIsNotProtocolError(t *testing.T) {
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
func TestReviewPropagationPipelines(t *testing.T) {
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
	go func() {
		for i := 0; i < 8; i++ {
			data, err := b.ReadMessage(context.Background())
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
	if delay := <-clk.registered; delay != time.Second {
		t.Fatal(delay)
	}
	clk.Advance(time.Second)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if a.Writes() != 8 {
		t.Fatal("propagation serialized writes")
	}
}
func TestReviewBlockedWritesCancel(t *testing.T) {
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

func TestReviewWebSocketGracefulShutdown(t *testing.T) {
	track(t)
	serverDone := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := wire.AcceptWebSocket(w, r, nil)
		if err != nil {
			serverDone <- err
			return
		}
		// Handshake stays on the raw connection.
		bytes, err := raw.ReadMessage(r.Context())
		if err != nil {
			serverDone <- err
			_ = raw.Close()
			return
		}
		if _, err = wire.Decode(wire.DaemonUplink, bytes, wire.MaxMetadataBytes); err != nil {
			serverDone <- err
			_ = raw.Close()
			return
		}
		hw := &circusv1.Welcome{PrincipalId: "daemon"}
		so, err := wire.FillWelcome(hw, wire.Options{})
		if err != nil {
			serverDone <- err
			_ = raw.Close()
			return
		}
		welcome, _ := proto.Marshal(&circusv1.DaemonDownlink{Payload: &circusv1.DaemonDownlink_Welcome{Welcome: hw}})
		if err = raw.WriteMessage(r.Context(), welcome); err != nil {
			serverDone <- err
			_ = raw.Close()
			return
		}
		l, err := wire.New(connWrapper{raw}, wire.DaemonControlSide, so)
		if err != nil {
			serverDone <- err
			_ = raw.Close()
			return
		}
		defer l.Close()
		err = testSend(l, r.Context(), &circusv1.DaemonDownlink{Payload: &circusv1.DaemonDownlink_GoAway{GoAway: &circusv1.GoAway{ReconnectAfterMs: 30000}}})
		if err == nil {
			ctx, cancel := context.WithTimeout(r.Context(), time.Second)
			err = l.Shutdown(ctx)
			cancel()
		}
		serverDone <- err
	}))
	defer srv.Close()
	raw, _, err := wire.DialWebSocket(contextFor(t), "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	hello, _ := proto.Marshal(&circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_Hello{Hello: &circusv1.Hello{ProtocolVersion: 1}}})
	if err = raw.WriteMessage(contextFor(t), hello); err != nil {
		t.Fatal(err)
	}
	welcome, err := raw.ReadMessage(contextFor(t))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := wire.Decode(wire.DaemonDownlink, welcome, wire.MaxMetadataBytes)
	if err != nil || decoded.(*circusv1.DaemonDownlink).GetWelcome().GetPrincipalId() != "daemon" {
		t.Fatal("raw Welcome", err)
	}
	co, err := wire.OptionsFromWelcome(decoded.(*circusv1.DaemonDownlink).GetWelcome(), wire.Options{})
	if err != nil {
		t.Fatal(err)
	}
	l := newLink(t, connWrapper{raw}, wire.DaemonDownlink, wire.DaemonUplink, co)
	<-l.Done()
	if receive(t, l).(*circusv1.DaemonDownlink).GetGoAway() == nil {
		t.Fatal("GoAway lost")
	}
	if websocket.CloseStatus(l.Err()) != websocket.StatusNormalClosure {
		t.Fatal("missing normal close handshake", l.Err())
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}
func TestReviewWorkerJobIdentityAndProtoEnums(t *testing.T) {
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

func TestReviewNegotiatedSingleSlot(t *testing.T) {
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

func TestReviewOrderingKeyBound(t *testing.T) {
	track(t)
	a, b := wiretest.Pipe(wiretest.Options{})
	defer b.Close()
	l := newLink(t, a, wire.DaemonUplink, wire.DaemonDownlink, smallOptions())
	if err := l.Send(contextFor(t), dispatch("c", "x"), wire.Data, strings.Repeat("x", wire.MaxOrderingKeyBytes+1)); !errors.Is(err, wire.ErrTooLarge) {
		t.Fatal(err)
	}
	if l.Err() != nil {
		t.Fatal("oversize local ordering key closed link")
	}
}

// A small key must not retain a caller's arbitrarily large backing string.
func TestReviewOrderingKeyDoesNotRetainBacking(t *testing.T) {
	track(t)
	a, b := wiretest.Pipe(wiretest.Options{AtoB: wiretest.Faults{BlockWrites: true}})
	defer b.Close()
	l := newLink(t, a, wire.DaemonUplink, wire.DaemonDownlink, smallOptions())
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	func() {
		backing := strings.Repeat("k", 32<<20)
		if err := l.Send(contextFor(t), dispatch("held", "x"), wire.Data, backing[:2]); err != nil {
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

func TestReviewWebSocketHandshakeDeadline(t *testing.T) {
	for _, secure := range []bool{false, true} {
		t.Run(fmt.Sprint(secure), func(t *testing.T) {
			track(t)
			done := make(chan error, 1)
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, err := wire.AcceptWebSocket(w, r, nil)
				if err != nil {
					done <- err
					return
				}
				defer raw.Close()
				if _, err = raw.ReadMessage(r.Context()); err != nil {
					done <- err
					return
				}
				hw := &circusv1.Welcome{}
				so, err := wire.FillWelcome(hw, wire.Options{})
				if err != nil {
					done <- err
					return
				}
				welcome, _ := proto.Marshal(&circusv1.DaemonDownlink{Payload: &circusv1.DaemonDownlink_Welcome{Welcome: hw}})
				if err = raw.WriteMessage(r.Context(), welcome); err != nil {
					done <- err
					return
				}
				l, err := wire.New(connWrapper{raw}, wire.DaemonControlSide, so)
				if err != nil {
					done <- err
					return
				}
				defer l.Close()
				ctx, cancel := context.WithTimeout(r.Context(), 20*time.Millisecond)
				defer cancel()
				start := time.Now()
				err = l.Shutdown(ctx)
				if time.Since(start) > time.Second {
					done <- fmt.Errorf("close handshake ignored deadline")
					return
				}
				done <- err
			})
			var srv *httptest.Server
			if secure {
				srv = httptest.NewTLSServer(handler)
			} else {
				srv = httptest.NewServer(handler)
			}
			defer srv.Close()
			raw, _, err := wire.DialWebSocket(contextFor(t), "ws"+strings.TrimPrefix(srv.URL, "http"), &websocket.DialOptions{HTTPClient: srv.Client()})
			if err != nil {
				t.Fatal(err)
			}
			defer raw.Close()
			hello, _ := proto.Marshal(&circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_Hello{Hello: &circusv1.Hello{}}})
			if err = raw.WriteMessage(contextFor(t), hello); err != nil {
				t.Fatal(err)
			}
			if _, err = raw.ReadMessage(contextFor(t)); err != nil {
				t.Fatal(err)
			}
			// Deliberately do not read the close frame, so no peer acknowledgement occurs.
			select {
			case err := <-done:
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal(err)
				}
			case <-contextFor(t).Done():
				t.Fatal("close helper did not stop")
			}
		})
	}
}

func TestReviewWebSocketNegotiatedReadLimit(t *testing.T) {
	track(t)
	done := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := wire.AcceptWebSocket(w, r, nil)
		if err != nil {
			done <- err
			return
		}
		defer raw.Close()
		if _, err = raw.ReadMessage(r.Context()); err != nil {
			done <- err
			return
		}
		hw := &circusv1.Welcome{}
		so, err := wire.FillWelcome(hw, wire.Options{MaxFragmentBytes: wire.DefaultMaxFragmentBytes})
		if err != nil {
			done <- err
			return
		}
		welcome, _ := proto.Marshal(&circusv1.DaemonDownlink{Payload: &circusv1.DaemonDownlink_Welcome{Welcome: hw}})
		if err = raw.WriteMessage(r.Context(), welcome); err != nil {
			done <- err
			return
		}
		l, err := wire.New(connWrapper{raw}, wire.DaemonControlSide, so)
		if err != nil {
			done <- err
			return
		}
		defer l.Close()
		ctx, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		m, err := l.Receive(ctx)
		if err == nil && !proto.Equal(m, result(strings.Repeat("x", 300<<10))) {
			err = fmt.Errorf("negotiated large frame content")
		}
		done <- err
	}))
	defer srv.Close()
	raw, _, err := wire.DialWebSocket(contextFor(t), "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	hello, _ := proto.Marshal(&circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_Hello{Hello: &circusv1.Hello{}}})
	if err = raw.WriteMessage(contextFor(t), hello); err != nil {
		t.Fatal(err)
	}
	welcome, err := raw.ReadMessage(contextFor(t))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := wire.Decode(wire.DaemonDownlink, welcome, wire.MaxMetadataBytes)
	if err != nil {
		t.Fatal(err)
	}
	co, err := wire.OptionsFromWelcome(decoded.(*circusv1.DaemonDownlink).GetWelcome(), wire.Options{})
	if err != nil || co.MaxFragmentBytes != wire.DefaultMaxFragmentBytes {
		t.Fatal(co, err)
	}
	l := newLink(t, connWrapper{raw}, wire.DaemonDownlink, wire.DaemonUplink, co)
	send(t, l, result(strings.Repeat("x", 300<<10)))
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-contextFor(t).Done():
		t.Fatal("negotiated frame transfer stalled")
	}
}

func TestReviewTinyFragmentsDoNotAmplifyBlockHeaders(t *testing.T) {
	track(t)
	a, b := wiretest.Pipe(wiretest.Options{})
	defer a.Close()
	l := newLink(t, b, wire.DaemonUplink, wire.DaemonDownlink, wire.Options{MaxFragmentBytes: 1, MaxMessageBytes: 8192, DisableHeartbeat: true})
	for i := 0; i < 128; i++ {
		f := &circusv1.Fragment{MessageId: 1, Index: uint32(i), Data: []byte{0}}
		if i == 0 {
			f.TotalSize = 8192
		}
		frame(t, a, fragment(f))
	}
	eventually(t, func() bool { return l.Stats().FramesRead == 128 })
	if s := l.Stats(); s.ReassemblyBlocks != 1 || s.ReassemblyAllocatedBytes != 8192 {
		t.Fatal("block headers scale with tiny fragments", s)
	}
}

func TestReviewInputLimitRemainsConfigurable(t *testing.T) {
	track(t)
	a, b := wiretest.Pipe(wiretest.Options{})
	defer a.Close()
	o := wire.Options{MaxMessageBytes: 128 << 20, DisableHeartbeat: true}
	l := newLink(t, b, wire.DaemonDownlink, wire.DaemonUplink, o)
	frame(t, a, &circusv1.DaemonDownlink{Payload: &circusv1.DaemonDownlink_Fragment{Fragment: &circusv1.Fragment{MessageId: 1, TotalSize: (64 << 20) + 1, Data: []byte{0x52}}}})
	eventually(t, func() bool { return l.Stats().ReassemblyBytes == (64<<20)+1 })
	if l.Err() != nil || l.Stats().ReassemblyAllocatedBytes > 64<<10 {
		t.Fatal("configured input limit or incremental allocation", l.Err(), l.Stats())
	}
}

// Started messages must keep making progress even alongside small business
// bursts or a fragmented urgent ack. Priority cannot mean indefinite starvation.
func TestReviewMixedTrafficPreservesFragmentProgress(t *testing.T) {
	for _, urgent := range []bool{false, true} {
		t.Run(fmt.Sprint(urgent), func(t *testing.T) {
			track(t)
			a, b := wiretest.Pipe(wiretest.Options{AtoB: wiretest.Faults{FrameDelay: 10 * time.Millisecond}})
			gate := &gate{Conn: a, entered: make(chan struct{}, 256), release: make(chan struct{}, 256)}
			o := wire.Options{MaxFragmentBytes: 1024, MaxMessageBytes: 1 << 20, FragmentTimeout: 100 * time.Millisecond, DisableHeartbeat: true}
			s := newLink(t, gate, wire.DaemonDownlink, wire.DaemonUplink, o)
			r := newLink(t, b, wire.DaemonUplink, wire.DaemonDownlink, o)
			send(t, s, result(strings.Repeat("x", 32<<10)))
			<-gate.entered
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
			for i := 0; i < count; i++ {
				_ = receive(t, r)
			}
			if r.Err() != nil {
				t.Fatal("healthy mixed traffic timed out", r.Err())
			}
		})
	}
}
