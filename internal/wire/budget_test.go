package wire_test

import (
	"errors"
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

// A stalled link that dies returns partials AND accepted unread messages to the
// shared pool at once, without Receive or Close.
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

// drain reads l until it fails, so a healthy consumer never pauses the reader.
func drain(t *testing.T, l *wire.Link) {
	done := make(chan struct{})
	ctx := contextFor(t)
	go func() {
		defer close(done)
		for {
			if _, err := l.Receive(ctx); err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() { _ = l.Close(); wait.Recv(t, done) })
}

// A slow large send neither starves the readers of other links nor holds its
// whole reservation until completion.
func TestSlowSendDoesNotStarveOtherLinks(t *testing.T) {
	track(t)
	o := wire.Options{MaxFragmentBytes: 4 << 10, MaxMessageBytes: 640 << 10, HeartbeatInterval: 40 * time.Millisecond, HeartbeatTimeout: 250 * time.Millisecond, MaxConsumerStall: 250 * time.Millisecond}
	budget := minBudget(t, o) // send pool: exactly two maximum-size messages
	ob := o
	ob.Budget = budget
	a1, a2 := wiretest.Pipe(wiretest.Options{AtoB: wiretest.Faults{BytesPerSecond: 640 << 10}})
	sender := newLink(t, a1, wire.DaemonUplink, wire.DaemonDownlink, ob)
	peerA := newLink(t, a2, wire.DaemonDownlink, wire.DaemonUplink, o)
	b1, b2 := wiretest.Pipe(wiretest.Options{})
	healthy := newLink(t, b1, wire.DaemonUplink, wire.DaemonDownlink, ob)
	peerB := newLink(t, b2, wire.DaemonDownlink, wire.DaemonUplink, o)
	drain(t, sender)
	drain(t, healthy)
	drain(t, peerB)
	m := dispatch("big", strings.Repeat("x", 600<<10)) // ~1 s at 640 KiB/s, ~4 heartbeat/stall caps
	n := proto.Size(m)
	started := time.Now()
	send(t, sender, m)
	if s := budget.Stats(); s.Send.Used > n || s.Send.Peak != 2*n {
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
	elapsed := time.Since(started)
	if elapsed <= 2*o.HeartbeatTimeout || elapsed <= o.MaxConsumerStall {
		t.Fatalf("slow transfer lasted %s; must exceed two heartbeat timeouts (%s) and the consumer stall cap (%s)", elapsed, 2*o.HeartbeatTimeout, o.MaxConsumerStall)
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

// A long heartbeat run leaks neither reserve nor pools.
func TestHeartbeatsLeakNoBudget(t *testing.T) {
	track(t)
	clk := clock.New()
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
		awaitTimers(t, clk, 2) // both timers are armed before the clock moves
		// Keep writers and readers within one beat so fake time never outruns
		// liveness or heartbeat coalescing.
		eventually(t, func() bool {
			return left.Stats().HeartbeatsRead >= uint64(i) && right.Stats().HeartbeatsRead >= uint64(i) &&
				left.Stats().HeartbeatsWritten >= uint64(i) && right.Stats().HeartbeatsWritten >= uint64(i)
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

// A stall on this link's OWN receive space is a consumer stall, and the dead
// link returns the decoded costs it held.
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

// Close of a live link returns unread shared messages.
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

// The minimum receive pool really carries one maximum-size multi-block message,
// flat buffer included, without overrun.
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
