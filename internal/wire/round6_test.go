package wire_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/clock"
	"github.com/EricWvi/circus/internal/wire"
	"github.com/EricWvi/circus/internal/wire/wiretest"
	"google.golang.org/protobuf/proto"
)

// drain reads l until it fails, so a healthy consumer never pauses the reader.
func drain(t *testing.T, l *wire.Link) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if _, err := l.Receive(context.Background()); err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() { _ = l.Close(); <-done })
}

// Review 3 High 1: a slow large send neither starves the readers of other links
// nor holds its whole reservation until completion.
func TestSlowSendDoesNotStarveOtherLinks(t *testing.T) {
	track(t)
	o := wire.Options{MaxFragmentBytes: 4 << 10, MaxMessageBytes: 64 << 10, HeartbeatInterval: 40 * time.Millisecond, HeartbeatTimeout: 250 * time.Millisecond}
	budget := minBudget(t, o) // send pool: exactly two maximum-size messages
	ob := o
	ob.Budget = budget
	a1, a2 := wiretest.Pipe(wiretest.Options{AtoB: wiretest.Faults{BytesPerSecond: 64 << 10}})
	sender := newLink(t, a1, wire.DaemonUplink, wire.DaemonDownlink, ob)
	peerA := newLink(t, a2, wire.DaemonDownlink, wire.DaemonUplink, o)
	b1, b2 := wiretest.Pipe(wiretest.Options{})
	healthy := newLink(t, b1, wire.DaemonUplink, wire.DaemonDownlink, ob)
	peerB := newLink(t, b2, wire.DaemonDownlink, wire.DaemonUplink, o)
	drain(t, sender)
	drain(t, healthy)
	drain(t, peerB)
	m := dispatch("big", strings.Repeat("x", 60<<10)) // ~1 s at 64 KiB/s, ~4 stall caps
	n := proto.Size(m)
	send(t, sender, m)
	if s := budget.Stats(); s.Send.Used != n || s.Send.Peak != 2*n {
		t.Fatal("fragmented send must hold its bytes after the split overlap", s)
	}
	last, partial := n, false
	got := make(chan error, 1)
	go func() { _, err := peerA.Receive(contextFor(t)); got <- err }()
	for done := false; !done; {
		select {
		case err := <-got:
			if err != nil {
				t.Fatal(err)
			}
			done = true
		case <-time.After(20 * time.Millisecond):
			used := budget.Stats().Send.Used
			if used > last {
				t.Fatal("send charge grew during transfer", used, last)
			}
			partial = partial || used > 0 && used < n
			last = used
		}
	}
	if !partial {
		t.Fatal("send reservation was not released per fragment")
	}
	time.Sleep(2 * o.HeartbeatTimeout) // more stall caps with only heartbeats
	for _, l := range []*wire.Link{sender, peerA, healthy, peerB} {
		if l.Err() != nil {
			t.Fatal("healthy link closed", l.Err())
		}
	}
	s := budget.Stats()
	if s.Receive.Peak != 0 || healthy.Stats().HeartbeatsRead == 0 || sender.Stats().HeartbeatsRead == 0 {
		t.Fatalf("heartbeats must decode from the link reserve only: %+v %+v", s, healthy.Stats())
	}
	eventually(t, func() bool { return budget.Stats().Send.Used == 0 })
}

// Review 3 Low 4 (Y20): a long heartbeat run leaks neither reserve nor pools.
func TestHeartbeatsLeakNoBudget(t *testing.T) {
	track(t)
	clk := &notifyingClock{Fake: clock.New(), registered: make(chan time.Duration, 8)}
	o := poolOptions()
	o.DisableHeartbeat = false
	o.Clock = clk
	budget := minBudget(t, o)
	o.Budget = budget
	a, b := wiretest.Pipe(wiretest.Options{})
	left := newLink(t, a, wire.DaemonDownlink, wire.DaemonUplink, o)
	right := newLink(t, b, wire.DaemonUplink, wire.DaemonDownlink, o)
	const beats = 300
	for i := 0; i < beats; i++ {
		awaitTimer(t, clk) // both timers are armed before the clock moves
		awaitTimer(t, clk)
		// Keep readers within one beat so fake time never outruns liveness.
		eventually(t, func() bool {
			return left.Stats().HeartbeatsRead >= uint64(i) && right.Stats().HeartbeatsRead >= uint64(i)
		})
		clk.Advance(5 * time.Second)
	}
	// The clock no longer moves, so no new heartbeat is sent. Wait until each
	// side has processed every heartbeat the other wrote (a frame counts as
	// read before its reserve is released) and the reserve has settled; a real
	// leak keeps it nonzero and times out here.
	eventually(t, func() bool {
		for _, p := range [][2]*wire.Link{{left, right}, {right, left}} {
			s, peer := p[0].Stats(), p[1].Stats()
			if s.HeartbeatsRead < beats-1 || s.HeartbeatsRead != peer.HeartbeatsWritten || s.FramesRead != s.HeartbeatsRead || s.LinkReserveBytes != 0 {
				return false
			}
		}
		return true
	})
	for _, l := range []*wire.Link{left, right} {
		if s := l.Stats(); l.Err() != nil || s.LinkReserveBytes != 0 || s.SharedReceiveBytes != 0 || s.SharedSendBytes != 0 {
			t.Fatalf("heartbeat leak: err=%v %+v", l.Err(), s)
		}
	}
	if s := budget.Stats(); s.Receive.Peak != 0 || s.Send.Peak != 0 {
		t.Fatal("heartbeats touched a shared pool", s)
	}
}

// Decision 4: waiting for the shared pool held by OTHER links is pool
// starvation, distinct from a stalled local consumer and from peer errors.
func TestStarvedReceiveReportsBudgetExhausted(t *testing.T) {
	track(t)
	budget := minBudget(t, poolOptions())
	a, b := wiretest.Pipe(wiretest.Options{})
	defer a.Close()
	o := poolOptions()
	o.Budget, o.Clock = budget, clock.New() // never advanced: the holder never expires
	holder := newLink(t, b, wire.DaemonUplink, wire.DaemonDownlink, o)
	held := fillUntilBlocked(t, a, holder, bigResult())
	c, d := wiretest.Pipe(wiretest.Options{})
	defer c.Close()
	clk := clock.New()
	vo := poolOptions()
	vo.Budget, vo.Clock = budget, clk
	victim := newLink(t, d, wire.DaemonUplink, wire.DaemonDownlink, vo)
	frame(t, c, result("small")) // the link reserve still admits small frames
	_ = receive(t, victim)
	frame(t, c, bigResult())
	eventually(t, func() bool { return victim.Stats().ReadBlocked })
	clk.Advance(15 * time.Second)
	select {
	case <-victim.Done():
	case <-contextFor(t).Done():
		t.Fatal("starved reader never expired")
	}
	var pe *wire.ProtocolError
	if !errors.Is(victim.Err(), wire.ErrBudgetExhausted) || errors.Is(victim.Err(), wire.ErrConsumerStalled) || errors.As(victim.Err(), &pe) {
		t.Fatal(victim.Err())
	}
	if holder.Err() != nil || held == 0 {
		t.Fatal("holder must be unaffected", holder.Err())
	}
}

// Review 3 Low 4 (Y19) and Medium 1: a stall on this link's OWN receive space is
// a consumer stall, and the dead link returns the decoded costs it held.
func TestLocalStallReturnsSharedCharges(t *testing.T) {
	track(t)
	clk := clock.New()
	o := poolOptions()
	_, minReceive, err := wire.MinBudget(o)
	if err != nil {
		t.Fatal(err)
	}
	budget := budgetFor(t, 2*o.MaxMessageBytes, 4*minReceive) // local MaxReceiveBytes binds first
	o.Budget, o.Clock = budget, clk
	a, b := wiretest.Pipe(wiretest.Options{})
	defer a.Close()
	l := newLink(t, b, wire.DaemonUplink, wire.DaemonDownlink, o)
	held := fillUntilBlocked(t, a, l, bigResult())
	if s := budget.Stats(); held == 0 || s.Receive.Used <= l.Stats().ReceivedBytes {
		t.Fatalf("blocked frame should hold its decoded cost: %+v %+v", s, l.Stats())
	}
	clk.Advance(15 * time.Second)
	select {
	case <-l.Done():
	case <-contextFor(t).Done():
		t.Fatal("local stall never expired")
	}
	if !errors.Is(l.Err(), wire.ErrConsumerStalled) {
		t.Fatal(l.Err())
	}
	// No Receive and no Close: everything charged to the pool is back.
	if s := budget.Stats(); s.Receive.Used != 0 || l.Stats().ReceivedMessages != 0 {
		t.Fatal("failed link kept shared budget", s, l.Stats())
	}
	if _, err := l.Receive(contextFor(t)); !errors.Is(err, wire.ErrConsumerStalled) {
		t.Fatal(err)
	}
}

// Review 3 Low 4 (Y8): Close of a live link with unread shared messages.
func TestCloseReturnsUnreadSharedMessages(t *testing.T) {
	track(t)
	o := poolOptions()
	budget := minBudget(t, o)
	o.Budget = budget
	a, b := wiretest.Pipe(wiretest.Options{})
	defer a.Close()
	l := newLink(t, b, wire.DaemonUplink, wire.DaemonDownlink, o)
	frame(t, a, result("small"))
	for i := 0; i < 3; i++ {
		frame(t, a, bigResult())
	}
	eventually(t, func() bool { return l.Stats().ReceivedMessages == 4 })
	if budget.Stats().Receive.Used == 0 || l.Stats().LinkReserveBytes == 0 {
		t.Fatal("expected shared and reserve charges", budget.Stats(), l.Stats())
	}
	_ = l.Close()
	if s := budget.Stats(); s.Receive.Used != 0 || l.Stats().LinkReserveBytes != 0 || l.Stats().ReceivedBytes != 0 {
		t.Fatal("Close kept charges", s, l.Stats())
	}
}

// Review 3 Low 1 and Low 4 (Y4): the minimum receive pool really carries one
// maximum-size multi-block message, flat buffer included, without overrun.
func TestMinimumPoolCarriesMaximumMessage(t *testing.T) {
	track(t)
	o := wire.Options{MaxFragmentBytes: 64 << 10, MaxMessageBytes: 256 << 10, DisableHeartbeat: true}
	budget := minBudget(t, o)
	ro := o
	ro.Budget = budget
	a, b := wiretest.Pipe(wiretest.Options{})
	sender := newLink(t, a, wire.DaemonDownlink, wire.DaemonUplink, o)
	receiver := newLink(t, b, wire.DaemonUplink, wire.DaemonDownlink, ro)
	m := result(strings.Repeat("x", o.MaxMessageBytes-100))
	total := proto.Size(m)
	send(t, sender, m)
	eventually(t, func() bool { return receiver.Stats().ReceivedMessages == 1 })
	cost := receiver.Stats().ReceivedBytes
	s := budget.Stats()
	// Decoding holds the flat input, its decoded cost and transient credit.
	if s.Receive.Used != cost || s.Receive.Peak < 2*total+cost || s.Receive.Peak > s.Receive.Limit {
		t.Fatalf("multi-block admission: %+v total=%d cost=%d", s, total, cost)
	}
	if !proto.Equal(receive(t, receiver), m) {
		t.Fatal("content")
	}
}

func TestBudgetValidation(t *testing.T) {
	track(t)
	send, receive, err := wire.MinBudget(wire.Options{})
	if err != nil || send != 128<<20 || receive != 262<<20+4096 {
		t.Fatal("default minimums", send, receive, err)
	}
	if _, _, err := wire.MinBudget(wire.Options{MaxInFlightMessages: 3}); err == nil {
		t.Fatal("invalid options accepted")
	}
	if _, err := wire.NewBudget(0, 1); err == nil {
		t.Fatal("empty pool accepted")
	}
	o := smallOptions()
	send, receive, _ = wire.MinBudget(o)
	for _, c := range []struct {
		send, receive int
		ok            bool
	}{{send, receive, true}, {send - 1, receive, false}, {send, receive - 1, false}} {
		budget, _ := wire.NewBudget(c.send, c.receive)
		o.Budget = budget
		a, b := wiretest.Pipe(wiretest.Options{})
		l, err := wire.New(a, wire.DaemonSide, o)
		if (err == nil) != c.ok {
			t.Fatalf("pools %d/%d: %v", c.send, c.receive, err)
		}
		if l != nil {
			_ = l.Close()
		}
		_ = a.Close()
		_ = b.Close()
		w := &circusv1.Welcome{}
		if _, err := wire.FillWelcome(w, smallOptions()); err != nil {
			t.Fatal(err)
		}
		if _, err := wire.OptionsFromWelcome(w, wire.Options{Budget: budget}); (err == nil) != c.ok {
			t.Fatalf("negotiated pools %d/%d: %v", c.send, c.receive, err)
		}
	}
}

// Review 3 Low 2: the server writes explicit limits; the client derives the
// same values without relying on its own compiled defaults.
func TestFillWelcomeExplicitRoundTrip(t *testing.T) {
	track(t)
	w := &circusv1.Welcome{PrincipalId: "p"}
	server, err := wire.FillWelcome(w, wire.Options{MaxMessageBytes: 1 << 20, HeartbeatInterval: 2 * time.Second, MaxConsumerStall: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if w.PrincipalId != "p" || w.MaxMessageBytes != 1<<20 || w.MaxFragmentBytes != wire.DefaultMaxFragmentBytes || w.MaxInFlightMessages != 2 || w.MaxInFlightBytes != 2<<20 || w.HeartbeatIntervalMs != 2000 || w.HeartbeatTimeoutMs != 15000 {
		t.Fatalf("Welcome must carry explicit values: %+v", w)
	}
	client, err := wire.OptionsFromWelcome(w, wire.Options{MaxMessageBytes: 999, MaxInFlightBytes: 7})
	if err != nil {
		t.Fatal(err)
	}
	if client.MaxMessageBytes != server.MaxMessageBytes || client.MaxFragmentBytes != server.MaxFragmentBytes || client.MaxInFlightMessages != server.MaxInFlightMessages || client.MaxInFlightBytes != server.MaxInFlightBytes || client.HeartbeatInterval != server.HeartbeatInterval || client.HeartbeatTimeout != server.HeartbeatTimeout {
		t.Fatalf("sides diverge: server=%+v client=%+v", server, client)
	}
	if server.MaxConsumerStall != time.Minute || client.MaxConsumerStall != server.HeartbeatTimeout {
		t.Fatal("local-only options must stay local", server.MaxConsumerStall, client.MaxConsumerStall)
	}
	for _, o := range []wire.Options{{HeartbeatInterval: 1500 * time.Microsecond}, {MaxInFlightMessages: 3}} {
		if _, err := wire.FillWelcome(&circusv1.Welcome{}, o); err == nil {
			t.Fatalf("unrepresentable or invalid options accepted: %+v", o)
		}
	}
	if _, err := wire.FillWelcome(nil, wire.Options{}); err == nil {
		t.Fatal("nil Welcome accepted")
	}
}
