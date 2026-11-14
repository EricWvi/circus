package wire_test

import (
	"context"
	"testing"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/clock"
	"github.com/EricWvi/circus/internal/testutil/leakcheck"
	"github.com/EricWvi/circus/internal/testutil/wait"
	"github.com/EricWvi/circus/internal/wire"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

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

func poolOptions() wire.Options {
	return wire.Options{MaxFragmentBytes: 64 << 10, MaxMessageBytes: 64 << 10, DisableHeartbeat: true}
}

func envField(field protowire.Number, inner []byte) []byte {
	return protowire.AppendBytes(protowire.AppendTag(nil, field, protowire.BytesType), inner)
}
func track(t *testing.T) { t.Helper(); leakcheck.Track(t) }
func contextFor(t *testing.T) context.Context {
	t.Helper()
	return wait.Ctx(t)
}
func eventually(t *testing.T, f func() bool) {
	t.Helper()
	wait.For(t, f)
}

// awaitTimers waits for a timer-count level, not for new registration events.
// clock.Fake.Waiters includes abandoned timers that have not fired, so n must
// include every abandoned timer known to remain at the call site.
func awaitTimers(t *testing.T, c *clock.Fake, n int) {
	t.Helper()
	eventually(t, func() bool { return c.Waiters() >= n })
}
func newLink(t *testing.T, c wire.Conn, in, out wire.Kind, o wire.Options) *wire.Link {
	t.Helper()
	l, err := wire.New(c, testSide(in, out), o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = l.Close()
		s := l.Stats()
		if s.QueuedBytes != 0 || s.QueuedMessages != 0 || s.ReassemblyBytes != 0 || s.ReassemblyMessages != 0 || s.ReceivedMessages != 0 || s.ReceivedBytes != 0 || s.ReassemblyAllocatedBytes != 0 || s.ReassemblyBlocks != 0 || s.ActiveMessages != 0 || s.ReadBlocked || s.SharedSendBytes != 0 || s.SharedReceiveBytes != 0 || s.LinkReserveBytes != 0 {
			t.Errorf("resources after close: %+v", s)
		}
	})
	return l
}
func send(t *testing.T, l *wire.Link, m proto.Message) {
	t.Helper()
	if err := testSend(l, contextFor(t), m); err != nil {
		t.Fatal(err)
	}
}
func receive(t *testing.T, l *wire.Link) proto.Message {
	t.Helper()
	m, err := l.Receive(contextFor(t))
	if err != nil {
		t.Fatal(err)
	}
	return m
}
func frame(t *testing.T, c wire.Conn, m proto.Message) {
	t.Helper()
	b, err := proto.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.WriteMessage(contextFor(t), b); err != nil {
		t.Fatal(err)
	}
}
func fragment(f *circusv1.Fragment) proto.Message {
	return &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_Fragment{Fragment: f}}
}
func result(s string) proto.Message {
	return &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_Result{Result: &circusv1.DeviceResult{CallId: "c", Output: &circusv1.ToolOutput{Content: []*circusv1.ContentBlock{{Block: &circusv1.ContentBlock_Text{Text: s}}}}}}}
}
func dispatch(id, s string) proto.Message {
	return &circusv1.DaemonDownlink{Payload: &circusv1.DaemonDownlink_DispatchCall{DispatchCall: &circusv1.DispatchCall{TargetBootId: "boot", DispatchedAtUnixMs: 1, Call: &circusv1.DeviceCall{CallId: id, SessionId: "s", Tool: &circusv1.DeviceCall_Write{Write: &circusv1.WriteRequest{Content: s}}}}}}
}
func smallOptions() wire.Options {
	return wire.Options{MaxFragmentBytes: 64, MaxMessageBytes: 4096, DisableHeartbeat: true}
}

// gate blocks individual physical writes, allowing deterministic scheduler checks.
type gate struct {
	wire.Conn
	entered, release chan struct{}
}

func (g *gate) WriteMessage(ctx context.Context, b []byte) error {
	select {
	case g.entered <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-g.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return g.Conn.WriteMessage(ctx, b)
}
func step(t *testing.T, g *gate, peer wire.Conn) proto.Message {
	t.Helper()
	ctx := contextFor(t)
	select {
	case <-g.entered:
	case <-ctx.Done():
		t.Fatal("writer did not enter")
	}
	g.release <- struct{}{}
	data, err := peer.ReadMessage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	m, err := wire.Decode(wire.DaemonDownlink, data, 4096)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// Caller policy: transport knows only classes and keys.
func testSide(in, out wire.Kind) wire.Side {
	switch in {
	case wire.DaemonUplink:
		return wire.DaemonControlSide
	case wire.DaemonDownlink:
		return wire.DaemonSide
	case wire.WorkerUplink:
		return wire.WorkerControlSide
	case wire.WorkerDownlink:
		return wire.WorkerSide
	}
	panic("invalid test side")
}
func testSend(l *wire.Link, ctx context.Context, m proto.Message) error {
	return l.Send(ctx, m)
}
