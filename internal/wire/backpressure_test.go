package wire_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/clock"
	"github.com/EricWvi/circus/internal/wire"
	"github.com/EricWvi/circus/internal/wire/wiretest"
)

// Ordinary bursts do not close a slow reader.
func TestSlowConsumerBurst(t *testing.T) {
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
func TestByteBackpressurePausesDeadlines(t *testing.T) {
	track(t)
	clk := clock.New()
	a, b := wiretest.Pipe(wiretest.Options{})
	defer a.Close()
	o := smallOptions()
	o.Clock = clk
	o.DisableHeartbeat = false
	o.Budget = minBudget(t, o)
	o.MaxConsumerStall = 2 * time.Minute
	l := newLink(t, b, wire.DaemonUplink, wire.DaemonDownlink, o)
	awaitTimers(t, clk, 1)
	// Keep a partial message alive across the local-consumer pause too.
	frame(t, a, fragment(&circusv1.Fragment{MessageId: 1, TotalSize: 2, Data: []byte{0x0a}}))
	accepted := fillUntilBlocked(t, a, l, result("1"))
	clk.Advance(time.Minute)
	// The consumer-stall timer remains armed while the heartbeat timer rearms.
	awaitTimers(t, clk, 2)
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

func TestReceivedAccounting(t *testing.T) {
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

// Bounded local pauses remain distinct from peer inactivity.
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
	clk := clock.New()
	a, b := wiretest.Pipe(wiretest.Options{})
	defer a.Close()
	o := smallOptions()
	o.Clock = clk
	o.Budget = minBudget(t, o)
	o.DisableHeartbeat = false
	o.MaxConsumerStall = time.Minute
	l := newLink(t, b, wire.DaemonUplink, wire.DaemonDownlink, o)
	awaitTimers(t, clk, 1)
	accepted := fillUntilBlocked(t, a, l, result("first"))
	clk.Advance(20 * time.Second)
	// The stall timer stays armed while the heartbeat timer rearms.
	awaitTimers(t, clk, 2)
	// Resume itself must not depend on a peer frame.
	for i := 0; i <= accepted; i++ {
		_ = receive(t, l)
	}
	eventually(t, func() bool { return !l.Stats().ReadBlocked })
	clk.Advance(5 * time.Second)
	awaitTimers(t, clk, 2)
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
