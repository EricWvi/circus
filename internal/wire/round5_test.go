package wire_test

import (
	"bytes"
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/clock"
	"github.com/EricWvi/circus/internal/wire"
	"github.com/EricWvi/circus/internal/wire/wiretest"
	"github.com/EricWvi/circus/internal/wirepolicy"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

type connWrapper struct{ wire.Conn }

func budgetFor(t *testing.T, send, receive int) *wire.Budget {
	t.Helper()
	b, e := wire.NewBudget(send, receive)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		if s := b.Stats(); s.Send.Used != 0 || s.Receive.Used != 0 || s.Send.Peak > s.Send.Limit || s.Receive.Peak > s.Receive.Limit {
			t.Errorf("shared budget leak or overrun: %+v", s)
		}
	})
	return b
}

// minBudget returns a budget whose pools are exactly the minimum for o.
func minBudget(t *testing.T, o wire.Options) *wire.Budget {
	t.Helper()
	send, receive, err := wire.MinBudget(o)
	if err != nil {
		t.Fatal(err)
	}
	return budgetFor(t, send, receive)
}

// fillUntilBlocked writes m until l's reader pauses; it returns accepted messages.
func fillUntilBlocked(t *testing.T, peer wire.Conn, l *wire.Link, m proto.Message) int {
	t.Helper()
	for {
		before := l.Stats().ReceivedMessages
		frame(t, peer, m)
		eventually(t, func() bool { s := l.Stats(); return s.ReceivedMessages > before || s.ReadBlocked })
		if s := l.Stats(); s.ReadBlocked {
			return s.ReceivedMessages
		}
	}
}

// Review N1: bounded local pauses remain distinct from peer inactivity.
func TestConsumerStallBound(t *testing.T) {
	track(t)
	clk := clock.New()
	a, b := wiretest.Pipe(wiretest.Options{})
	defer a.Close()
	o := smallOptions()
	o.Clock = clk
	budget := minBudget(t, o)
	o.Budget = budget
	l := newLink(t, b, wire.DaemonUplink, wire.DaemonDownlink, o)
	// Small results fill the link's own reserve; the shared pool is never used.
	accepted := fillUntilBlocked(t, a, l, result("first"))
	if s := l.Stats(); accepted < 2 || s.SharedReceiveBytes != 0 || s.LinkReserveBytes > wire.LinkReserveBytes {
		t.Fatalf("small frames used the shared pool: %+v", s)
	}
	clk.Advance(14 * time.Second)
	if l.Err() != nil {
		t.Fatal("pause expired early", l.Err())
	}
	clk.Advance(time.Second)
	select {
	case <-l.Done():
	case <-contextFor(t).Done():
		t.Fatal("unbounded consumer stall")
	}
	if !errors.Is(l.Err(), wire.ErrConsumerStalled) {
		t.Fatal(l.Err())
	}
	var pe *wire.ProtocolError
	if errors.As(l.Err(), &pe) {
		t.Fatal("local stall attributed to peer")
	}
	// Messages charged only to the link reserve hold no shared budget, so they
	// stay drainable after failure; then Receive returns the link error.
	for i := 0; i < accepted; i++ {
		_ = receive(t, l)
	}
	if _, err := l.Receive(contextFor(t)); !errors.Is(err, wire.ErrConsumerStalled) {
		t.Fatal(err)
	}
	if s := budget.Stats(); s.Receive.Peak != 0 || l.Stats().LinkReserveBytes != 0 {
		t.Fatal(s, l.Stats())
	}
}

func TestResumeRestartsLiveness(t *testing.T) {
	track(t)
	clk := &notifyingClock{Fake: clock.New(), registered: make(chan time.Duration, 64)}
	a, b := wiretest.Pipe(wiretest.Options{})
	defer a.Close()
	o := smallOptions()
	o.Clock = clk
	o.Budget = minBudget(t, o)
	o.DisableHeartbeat = false
	o.MaxConsumerStall = time.Minute
	l := newLink(t, b, wire.DaemonUplink, wire.DaemonDownlink, o)
	awaitTimer(t, clk)
	accepted := fillUntilBlocked(t, a, l, result("first"))
	clk.Advance(20 * time.Second)
	for {
		if d := awaitRegistered(t, clk); d == 5*time.Second {
			break
		}
	}
	// Drain timer registrations: resume itself must not depend on a peer frame.
	for i := 0; i <= accepted; i++ {
		_ = receive(t, l)
	}
	eventually(t, func() bool { return !l.Stats().ReadBlocked })
	clk.Advance(5 * time.Second)
	for {
		if d := awaitRegistered(t, clk); d == 5*time.Second {
			break
		}
	}
	if l.Err() != nil {
		t.Fatal("stale peer timestamp after resume", l.Err())
	}
	clk.Advance(10 * time.Second)
	select {
	case <-l.Done():
	case <-contextFor(t).Done():
		t.Fatal("resume suppressed future peer timeout")
	}
	if !strings.Contains(l.Err().Error(), "heartbeat timeout") {
		t.Fatal(l.Err())
	}
}

func TestSharedSendBudgetBusyAndRecovery(t *testing.T) {
	track(t)
	o := smallOptions()
	budget := minBudget(t, o) // send pool: two maximum-size messages' bytes
	m := dispatch("c", strings.Repeat("x", 4000))
	n := proto.Size(m)
	var links []*wire.Link
	for i := 0; i < 3; i++ {
		a, b := wiretest.Pipe(wiretest.Options{AtoB: wiretest.Faults{BlockWrites: true}})
		t.Cleanup(func() { _ = b.Close() })
		o.Budget = budget
		links = append(links, newLink(t, a, wire.DaemonUplink, wire.DaemonDownlink, o))
	}
	send(t, links[0], m)
	for _, l := range links[1:] {
		if err := testSend(l, contextFor(t), m); !errors.Is(err, wire.ErrBusy) {
			t.Fatal("shared exhaustion did not reject send", err)
		}
		if l.Err() != nil {
			t.Fatal("ErrBusy destroyed link", l.Err())
		}
	}
	if s := budget.Stats(); s.Send.Used != n || s.Send.Peak != 2*n || s.Receive.Peak != 0 {
		t.Fatal("send must charge only the send pool, with the split overlap", s)
	}
	_ = links[0].Close()
	send(t, links[1], m)
	_ = links[1].Close()
	send(t, links[2], m)
}

func poolOptions() wire.Options {
	return wire.Options{MaxFragmentBytes: 64 << 10, MaxMessageBytes: 64 << 10, DisableHeartbeat: true}
}

// bigResult's decode admission exceeds LinkReserveBytes, so it uses the shared pool.
func bigResult() proto.Message { return result(strings.Repeat("x", 30<<10)) }

func TestSharedReceiveBudgetAcrossLinks(t *testing.T) {
	track(t)
	budget := minBudget(t, poolOptions())
	clk := clock.New()
	var links []*wire.Link
	var peers []wire.Conn
	for i := 0; i < 3; i++ {
		a, b := wiretest.Pipe(wiretest.Options{})
		t.Cleanup(func() { _ = a.Close() })
		peers = append(peers, a)
		o := poolOptions()
		o.Clock = clk
		o.Budget = budget
		links = append(links, newLink(t, b, wire.DaemonUplink, wire.DaemonDownlink, o))
	}
	held := fillUntilBlocked(t, peers[0], links[0], bigResult())
	frame(t, peers[1], bigResult())
	eventually(t, func() bool { return links[1].Stats().ReadBlocked })
	frame(t, peers[2], bigResult())
	eventually(t, func() bool { return links[2].Stats().ReadBlocked })
	if s := budget.Stats(); s.Receive.Peak > s.Receive.Limit {
		t.Fatal(s)
	}
	// Releases by one link wake readers of the others, without Send/Close.
	for i := 0; i <= held; i++ {
		_ = receive(t, links[0])
	}
	for _, l := range links[1:] {
		_ = receive(t, l)
	}
	eventually(t, func() bool { return budget.Stats().Receive.Used == 0 })
	for i, l := range links {
		frame(t, peers[i], bigResult())
		_ = receive(t, l)
	}
}

func TestSharedBudgetChargesBlocksNotDeclarations(t *testing.T) {
	track(t)
	o := wire.Options{DisableHeartbeat: true}
	budget := minBudget(t, o)
	o.Budget = budget
	a, b := wiretest.Pipe(wiretest.Options{})
	defer a.Close()
	l := newLink(t, b, wire.DaemonUplink, wire.DaemonDownlink, o)
	for id := uint64(1); id <= 2; id++ {
		frame(t, a, fragment(&circusv1.Fragment{MessageId: id, TotalSize: 64 << 20, Data: []byte{1}}))
	}
	eventually(t, func() bool { return l.Stats().ReassemblyMessages == 2 })
	// Tiny fragment frames decode from the link reserve; only real blocks are shared.
	if s := budget.Stats(); s.Receive.Used != 128<<10 || l.Stats().SharedReceiveBytes != 128<<10 {
		t.Fatal("declarations consumed shared budget", s)
	}
	_ = l.Close()
}

func TestHeartbeatOutsideQueueBudget(t *testing.T) {
	track(t)
	clk := &notifyingClock{Fake: clock.New(), registered: make(chan time.Duration, 32)}
	a, b := wiretest.Pipe(wiretest.Options{AtoB: wiretest.Faults{BlockWrites: true}})
	defer b.Close()
	m := dispatch("c", "x")
	o := smallOptions()
	o.Clock = clk
	o.DisableHeartbeat = false
	o.MaxQueuedMessages = 4
	o.MaxQueuedBytes = 4 * proto.Size(m)
	l := newLink(t, a, wire.DaemonUplink, wire.DaemonDownlink, o)
	awaitTimer(t, clk)
	for i := 0; i < 4; i++ {
		send(t, l, m)
	}
	for i := 0; i < 2; i++ {
		clk.Advance(5 * time.Second)
		awaitTimer(t, clk)
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

func TestShutdownRejectsNewSends(t *testing.T) {
	track(t)
	a, b := wiretest.Pipe(wiretest.Options{AtoB: wiretest.Faults{BlockWrites: true}})
	defer b.Close()
	l := newLink(t, a, wire.DaemonUplink, wire.DaemonDownlink, smallOptions())
	send(t, l, dispatch("first", "x"))
	done := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	go func() { done <- l.Shutdown(ctx) }()
	eventually(t, func() bool { return errors.Is(testSend(l, contextFor(t), dispatch("probe", "x")), wire.ErrClosing) })
	err := <-done
	if !errors.Is(err, wire.ErrShutdownTimeout) || !errors.Is(err, context.DeadlineExceeded) || !errors.Is(l.Err(), wire.ErrShutdownTimeout) {
		t.Fatal("shutdown timeout identity", err, l.Err())
	}
}

func TestWelcomeOptionsValidation(t *testing.T) {
	track(t)
	budget := minBudget(t, wire.Options{})
	o, err := wire.OptionsFromWelcome(&circusv1.Welcome{}, wire.Options{Budget: budget, MaxMessageBytes: 100, MaxInFlightMessages: 1})
	if err != nil || o.MaxMessageBytes != wire.DefaultMaxMessageBytes || o.MaxInFlightMessages != 2 || o.Budget != budget || o.MaxConsumerStall != 15*time.Second {
		t.Fatal(o, err)
	}
	o, err = wire.OptionsFromWelcome(&circusv1.Welcome{HeartbeatIntervalMs: 123, HeartbeatTimeoutMs: 456}, wire.Options{})
	if err != nil || o.HeartbeatInterval != 123*time.Millisecond || o.HeartbeatTimeout != 456*time.Millisecond || o.MaxConsumerStall != 456*time.Millisecond {
		t.Fatal(o, err)
	}
	for _, w := range []*circusv1.Welcome{nil, {MaxMessageBytes: math.MaxUint64}, {MaxInFlightBytes: math.MaxUint64}, {MaxInFlightMessages: 3}, {MaxFragmentBytes: 100, MaxMessageBytes: 99}, {HeartbeatIntervalMs: 200, HeartbeatTimeoutMs: 100}} {
		if _, err := wire.OptionsFromWelcome(w, wire.Options{}); err == nil {
			t.Fatalf("invalid Welcome accepted: %+v", w)
		}
	}
	a, b := wiretest.Pipe(wiretest.Options{})
	defer a.Close()
	defer b.Close()
	if l, err := wire.New(a, wire.DaemonSide, wire.Options{MaxReceiveBytes: 1}); err == nil {
		_ = l.Close()
		t.Fatal("undersized local receive budget accepted")
	}
}

func envField(field protowire.Number, inner []byte) []byte {
	return protowire.AppendBytes(protowire.AppendTag(nil, field, protowire.BytesType), inner)
}
func TestScannerReviewGaps(t *testing.T) {
	track(t)
	empty := []byte{0x12, 0}
	nodeAttack := envField(11, bytes.Repeat([]byte{0x1a, 0}, wire.MaxDecodeNodes)) // repeated singular fault messages
	eventAttack := envField(14, envField(2, envField(5, bytes.Repeat([]byte{'x'}, wire.MaxMetadataBytes+1))))
	m := result(strings.Repeat("o", 40<<10)).(*circusv1.DaemonUplink).GetResult()
	padded := &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_CallStatus{CallStatus: &circusv1.CallStatus{CallId: strings.Repeat("m", wire.MaxMetadataBytes+1), Result: m}}}
	paddedBytes, _ := proto.Marshal(padded)
	for name, data := range map[string][]byte{"duplicate-payload": append(append([]byte{}, empty...), empty...), "nodes": nodeAttack, "nested-event": eventAttack, "result-exemption": paddedBytes} {
		t.Run(name, func(t *testing.T) {
			if _, err := wire.Decode(wire.DaemonUplink, data, wire.DefaultMaxMessageBytes); err == nil {
				t.Fatal("scanner accepted", name)
			}
		})
	}
}

func TestEnvelopeSchemaAndPolicyCoverage(t *testing.T) {
	// codec.go charges 256 bytes/node and 64 bytes/field. Packed scalars and
	// maps require element-aware costs; update those constants/scan before adding
	// either. New repeated fields need an explicit element limit as well.
	seen := map[protoreflect.FullName]bool{}
	repeated := map[protoreflect.FullName]bool{"circus.v1.JobSnapshot.jobs": true, "circus.v1.ToolOutput.content": true, "circus.v1.BashRequest.working_dirs": true, "circus.v1.CallUpdate.DeviceOffline.online_devices": true}
	var walk func(protoreflect.MessageDescriptor)
	walk = func(d protoreflect.MessageDescriptor) {
		if seen[d.FullName()] {
			return
		}
		seen[d.FullName()] = true
		for i := 0; i < d.Fields().Len(); i++ {
			f := d.Fields().Get(i)
			if f.IsMap() || f.IsPacked() {
				t.Errorf("update scanner cost for %s", f.FullName())
			}
			if f.IsList() && !repeated[f.FullName()] {
				t.Errorf("add scanner limit for %s", f.FullName())
			}
			if f.Message() != nil {
				walk(f.Message())
			}
		}
	}
	for _, m := range []proto.Message{&circusv1.DaemonUplink{}, &circusv1.DaemonDownlink{}, &circusv1.WorkerUplink{}, &circusv1.WorkerDownlink{}} {
		r := m.ProtoReflect()
		walk(r.Descriptor())
		fields := r.Descriptor().Fields()
		for i := 0; i < fields.Len(); i++ {
			f := fields.Get(i)
			body := r.NewField(f).Message()
			// A distinct, nonempty call ID per payload catches a constant key.
			callID := string(r.Descriptor().Name()) + "/" + string(f.Name())
			holder := body
			if inner := body.Descriptor().Fields().ByName("call"); inner != nil && inner.Message() != nil {
				holder = body.Mutable(inner).Message()
			}
			if id := holder.Descriptor().Fields().ByName("call_id"); id != nil {
				holder.Set(id, protoreflect.ValueOfString(callID))
			}
			r.Set(f, protoreflect.ValueOfMessage(body))
			class, key, err := wirepolicy.Classify(m)
			expected := wire.Data
			switch f.Name() {
			case "heartbeat", "go_away", "call_ack":
				expected = wire.Control
			case "list_jobs", "stop_session_jobs":
				expected = wire.Barrier
			}
			if class != expected {
				t.Errorf("class for %s: %v", f.FullName(), class)
			}
			switch f.Name() {
			case "dispatch_call", "submit_call", "cancel_call", "call_query", "call_update":
				if key != "call/"+callID {
					t.Errorf("call ordering key for %s: %q, want %q", f.FullName(), key, "call/"+callID)
				}
			}
			if err != nil {
				t.Errorf("%s: %v", f.FullName(), err)
			}
		}
	}
	if _, _, err := wirepolicy.Classify(&circusv1.DeviceResult{}); err == nil {
		t.Fatal("non-envelope classified")
	}
}

func awaitRegistered(t *testing.T, c *notifyingClock) time.Duration {
	t.Helper()
	select {
	case d := <-c.registered:
		return d
	case <-contextFor(t).Done():
		t.Fatal("timer was not rearmed")
		return 0
	}
}

// Review 3 Medium 1: a stalled link that dies returns partials AND accepted
// unread messages to the shared pool at once, without Receive or Close.
func TestSharedStallReleasesPartialsForOtherLinks(t *testing.T) {
	track(t)
	budget := minBudget(t, poolOptions())
	clk := clock.New()
	a, b := wiretest.Pipe(wiretest.Options{})
	defer a.Close()
	o := poolOptions()
	o.Clock, o.Budget = clk, budget
	stalled, err := wire.New(b, wire.DaemonControlSide, o) // its consumer never Receives or Closes
	if err != nil {
		t.Fatal(err)
	}
	for id := uint64(1); id <= 2; id++ {
		frame(t, a, fragment(&circusv1.Fragment{MessageId: id, TotalSize: 64 << 10, Data: []byte{1}}))
	}
	held := fillUntilBlocked(t, a, stalled, bigResult())
	c, d := wiretest.Pipe(wiretest.Options{})
	defer c.Close()
	po := poolOptions()
	po.Budget = budget
	other := newLink(t, d, wire.DaemonUplink, wire.DaemonDownlink, po)
	frame(t, c, bigResult())
	eventually(t, func() bool { return other.Stats().ReadBlocked })
	clk.Advance(15 * time.Second)
	select {
	case <-stalled.Done():
	case <-contextFor(t).Done():
		t.Fatal("stalled reservations never released")
	}
	// It waited for the shared pool, so the error names pool starvation.
	if !errors.Is(stalled.Err(), wire.ErrBudgetExhausted) {
		t.Fatal(stalled.Err())
	}
	if s := stalled.Stats(); held == 0 || s.ReceivedMessages != 0 || s.SharedReceiveBytes != 0 {
		t.Fatalf("dead link kept shared charges: held=%d %+v", held, s)
	}
	if got := receive(t, other).(*circusv1.DaemonUplink).GetResult().GetCallId(); got != "c" {
		t.Fatal(got)
	}
	if other.Err() != nil {
		t.Fatal(other.Err())
	}
	if s := budget.Stats(); s.Receive.Used != 0 {
		t.Fatal(s)
	}
	if _, err := stalled.Receive(contextFor(t)); !errors.Is(err, wire.ErrBudgetExhausted) {
		t.Fatal("dropped messages must surface the link error", err)
	}
	_ = stalled.Close()
}

func TestSharedDecodeTransientCredit(t *testing.T) {
	for _, big := range []bool{false, true} {
		t.Run(map[bool]string{false: "reserve", true: "shared"}[big], func(t *testing.T) {
			track(t)
			o := poolOptions()
			budget := minBudget(t, o)
			o.Budget = budget
			a, b := wiretest.Pipe(wiretest.Options{})
			defer a.Close()
			l := newLink(t, b, wire.DaemonUplink, wire.DaemonDownlink, o)
			m := result(strings.Repeat("x", 200))
			if big {
				m = bigResult()
			}
			frame(t, a, m)
			eventually(t, func() bool { return l.Stats().ReceivedMessages == 1 })
			cost := l.Stats().ReceivedBytes
			s := budget.Stats()
			if big && (s.Receive.Used != cost || s.Receive.Peak != cost+proto.Size(m) || cost+proto.Size(m) <= wire.LinkReserveBytes) {
				t.Fatalf("missing transient decoder reservation: %+v cost=%d", s, cost)
			}
			if !big && (s.Receive.Peak != 0 || l.Stats().LinkReserveBytes != cost) {
				t.Fatalf("small frame must use the link reserve: %+v %+v", s, l.Stats())
			}
			_ = receive(t, l)
			if l.Stats().LinkReserveBytes != 0 || budget.Stats().Receive.Used != 0 {
				t.Fatal("Receive did not return the charge", l.Stats(), budget.Stats())
			}
		})
	}
}

func TestReceiveReturnsTerminalError(t *testing.T) {
	track(t)
	a, b := wiretest.Pipe(wiretest.Options{})
	defer a.Close()
	l := newLink(t, b, wire.DaemonUplink, wire.DaemonDownlink, smallOptions())
	_ = l.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := l.Receive(ctx); !errors.Is(err, wire.ErrClosed) {
		t.Fatal("terminal error replaced by caller timeout", err)
	}
}

func TestSharedBudgetErrorPaths(t *testing.T) {
	for name, packets := range map[string][][]byte{
		"unmarshal":      {envField(11, envField(1, []byte{0xff}))},
		"fragment-error": {mustEncode(fragment(&circusv1.Fragment{MessageId: 1, TotalSize: 100, Data: []byte{1}})), mustEncode(fragment(&circusv1.Fragment{MessageId: 1, Index: 3, Last: true, Data: []byte{1}}))},
		"nested":         {mustEncode(fragment(&circusv1.Fragment{MessageId: 1, TotalSize: 2, Last: true, Data: []byte{0x1a, 0}}))},
	} {
		t.Run(name, func(t *testing.T) {
			track(t)
			o := smallOptions()
			budget := minBudget(t, o)
			a, b := wiretest.Pipe(wiretest.Options{})
			defer a.Close()
			o.Budget = budget
			l := newLink(t, b, wire.DaemonUplink, wire.DaemonDownlink, o)
			for _, p := range packets {
				if err := a.WriteMessage(contextFor(t), p); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case <-l.Done():
			case <-contextFor(t).Done():
				t.Fatal("invalid input did not close")
			}
			if budget.Stats().Receive.Used != 0 || l.Stats().LinkReserveBytes != 0 {
				t.Fatal("receive error leaked budget", budget.Stats(), l.Stats())
			}
		})
	}
	t.Run("send-rejection", func(t *testing.T) {
		track(t)
		o := smallOptions()
		budget := minBudget(t, o)
		a, b := wiretest.Pipe(wiretest.Options{})
		defer b.Close()
		o.Budget = budget
		l := newLink(t, a, wire.DaemonUplink, wire.DaemonDownlink, o)
		if err := testSend(l, contextFor(t), dispatch("c", string([]byte{0xff}))); err == nil {
			t.Fatal("invalid UTF8 accepted")
		}
		if budget.Stats().Send.Used != 0 {
			t.Fatal("marshal error leaked", budget.Stats())
		}
	})
}
func mustEncode(m proto.Message) []byte {
	data, err := proto.Marshal(m)
	if err != nil {
		panic(err)
	}
	return data
}

func TestLocalReceiveBackpressureWakeAndStall(t *testing.T) {
	for _, resume := range []bool{false, true} {
		t.Run(map[bool]string{false: "stall", true: "resume"}[resume], func(t *testing.T) {
			track(t)
			clk := clock.New()
			a, b := wiretest.Pipe(wiretest.Options{})
			defer a.Close()
			const M = 8192
			o := wire.Options{Clock: clk, MaxFragmentBytes: M, MaxMessageBytes: M, MaxReceiveBytes: 2*M + 6*(1<<20) + 4096, DisableHeartbeat: true}
			l := newLink(t, b, wire.DaemonUplink, wire.DaemonDownlink, o)
			m := result(strings.Repeat("x", M-128))
			frame(t, a, m)
			eventually(t, func() bool { return l.Stats().ReceivedMessages == 1 })
			capacity := o.MaxReceiveBytes / l.Stats().ReceivedBytes
			for i := 0; i < capacity; i++ {
				frame(t, a, m)
			}
			eventually(t, func() bool { return l.Stats().ReadBlocked })
			if resume {
				_ = receive(t, l)
				eventually(t, func() bool { return !l.Stats().ReadBlocked && l.Stats().ReceivedMessages == capacity })
				if l.Err() != nil {
					t.Fatal(l.Err())
				}
			} else {
				clk.Advance(15 * time.Second)
				select {
				case <-l.Done():
				case <-contextFor(t).Done():
					t.Fatal("local receive pause never expired")
				}
				if !errors.Is(l.Err(), wire.ErrConsumerStalled) {
					t.Fatal(l.Err())
				}
			}
		})
	}
}

func TestWorkerJobOrderingIdentity(t *testing.T) {
	event := func(device, boot string) proto.Message {
		return &circusv1.WorkerDownlink{Payload: &circusv1.WorkerDownlink_JobEvent{JobEvent: &circusv1.WorkerJobEvent{Device: device, BootId: boot, Event: &circusv1.JobEvent{}}}}
	}
	_, first, err := wirepolicy.Classify(event(strings.Repeat("d", 2048), "boot1"))
	if err != nil || len(first) > wire.MaxOrderingKeyBytes {
		t.Fatal("unbounded job key", len(first), err)
	}
	_, again, _ := wirepolicy.Classify(event(strings.Repeat("d", 2048), "boot1"))
	_, nextBoot, _ := wirepolicy.Classify(event(strings.Repeat("d", 2048), "boot2"))
	_, otherDevice, _ := wirepolicy.Classify(event("other", "boot1"))
	if first != again || first == nextBoot || first == otherDevice {
		t.Fatal("worker jobs lost device/boot ordering identity")
	}
}

func TestSharedSendCompletionReleasesBudget(t *testing.T) {
	track(t)
	o := smallOptions()
	budget := minBudget(t, o)
	m := dispatch("c", strings.Repeat("x", 4000))
	n := proto.Size(m)
	a, b := wiretest.Pipe(wiretest.Options{})
	defer b.Close()
	g := &gate{Conn: a, entered: make(chan struct{}), release: make(chan struct{})}
	o.Budget = budget
	l := newLink(t, g, wire.DaemonUplink, wire.DaemonDownlink, o)
	for round := 0; round < 2; round++ {
		send(t, l, m)
		if err := testSend(l, contextFor(t), m); !errors.Is(err, wire.ErrBusy) {
			t.Fatal("busy not recoverable", err)
		}
		// Progressive release: each written fragment returns its bytes at once.
		written := 0
		for {
			f := step(t, g, b).(*circusv1.DaemonDownlink).GetFragment()
			if f == nil {
				t.Fatal("expected fragmented send")
			}
			written += len(f.Data)
			eventually(t, func() bool { return budget.Stats().Send.Used == n-written })
			if f.Last {
				break
			}
		}
		eventually(t, func() bool { return budget.Stats().Send.Used == 0 && l.Stats().QueuedBytes == 0 })
		if l.Err() != nil {
			t.Fatal("ErrBusy prevented later delivery", l.Err())
		}
	}
}
