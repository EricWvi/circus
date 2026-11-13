package wire_test

import (
	"context"
	"errors"
	"io"
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

func TestShutdownAndEOFDrain(t *testing.T) {
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
	wait.Recv(t, r.Done())
	if got := receive(t, r); !proto.Equal(got, m) {
		t.Fatal("GoAway lost")
	}
}
func TestShutdownDeadline(t *testing.T) {
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

// deadlineOnCancel lets the test trigger deadline expiry explicitly once
// Shutdown has entered its closing state, rather than waiting for a wall clock.
type deadlineOnCancel struct{ context.Context }

func (c deadlineOnCancel) Err() error {
	if c.Context.Err() != nil {
		return context.DeadlineExceeded
	}
	return nil
}

func TestShutdownRejectsNewSends(t *testing.T) {
	track(t)
	a, b := wiretest.Pipe(wiretest.Options{AtoB: wiretest.Faults{BlockWrites: true}})
	defer b.Close()
	l := newLink(t, a, wire.DaemonUplink, wire.DaemonDownlink, smallOptions())
	send(t, l, dispatch("first", "x"))
	done := make(chan error, 1)
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := deadlineOnCancel{base}
	go func() { done <- l.Shutdown(ctx) }()
	eventually(t, func() bool { return errors.Is(testSend(l, contextFor(t), dispatch("probe", "x")), wire.ErrClosing) })
	cancel()
	err := wait.Recv(t, done)
	if !errors.Is(err, wire.ErrShutdownTimeout) || !errors.Is(err, context.DeadlineExceeded) || !errors.Is(l.Err(), wire.ErrShutdownTimeout) {
		t.Fatal("shutdown timeout identity", err, l.Err())
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

func gracefulOptions() wire.Options {
	return wire.Options{MaxMessageBytes: 1 << 20, MaxFragmentBytes: 64 << 10, DisableHeartbeat: true}
}

// finalResults: reserve-only, shared unfragmented and shared fragmented results.
func finalResults() []proto.Message {
	return []proto.Message{result("small"), result(strings.Repeat("1", 40<<10)), result(strings.Repeat("2", 300<<10)), result("tail")}
}

// Results a peer delivered before closing normally reach a budgeted consumer
// that keeps calling Receive, every time.
func TestGracefulCloseKeepsDelivered(t *testing.T) {
	track(t)
	o := gracefulOptions()
	budget := minBudget(t, o)
	for run := 0; run < 100; run++ {
		ob := o
		ob.Budget = budget
		a, b := wiretest.Pipe(wiretest.Options{})
		daemon := newLink(t, a, wire.DaemonDownlink, wire.DaemonUplink, o)
		control := newLink(t, b, wire.DaemonUplink, wire.DaemonDownlink, ob)
		got := make(chan int, 1)
		var final error
		receiveCtx := contextFor(t)
		go func() {
			n := 0
			for {
				if _, err := control.Receive(receiveCtx); err != nil {
					final = err
					got <- n
					return
				}
				n++
			}
		}()
		want := finalResults()
		for _, m := range want {
			send(t, daemon, m)
		}
		if err := daemon.Shutdown(contextFor(t)); err != nil {
			t.Fatal(err)
		}
		if n := wait.Recv(t, got); n != len(want) || !errors.Is(final, io.EOF) {
			t.Fatalf("run %d: received %d/%d before %v", run, n, len(want), final)
		}
		_ = control.Close()
		_ = daemon.Close()
		if s := budget.Stats(); s.Receive.Used != 0 {
			t.Fatal(s)
		}
	}
}

// GoAway queued behind a pool-holding message survives a graceful close even if
// the consumer only looks after Done.
func TestGracefulCloseDeliversGoAwayAfterSharedMessage(t *testing.T) {
	track(t)
	o := gracefulOptions()
	ob := o
	ob.Budget = minBudget(t, o)
	a, b := wiretest.Pipe(wiretest.Options{})
	tx := newLink(t, a, wire.DaemonUplink, wire.DaemonDownlink, o)
	rx := newLink(t, b, wire.DaemonDownlink, wire.DaemonUplink, ob)
	send(t, tx, dispatch("w", strings.Repeat("w", 40<<10)))
	// Control jumps the send queue; wait so GoAway really lands behind the
	// shared-charged dispatch in rx's received queue.
	eventually(t, func() bool { return rx.Stats().ReceivedMessages == 1 && rx.Stats().SharedReceiveBytes > 0 })
	send(t, tx, &circusv1.DaemonDownlink{Payload: &circusv1.DaemonDownlink_GoAway{GoAway: &circusv1.GoAway{ReconnectAfterMs: 30000}}})
	if err := tx.Shutdown(contextFor(t)); err != nil {
		t.Fatal(err)
	}
	wait.Recv(t, rx.Done())
	if receive(t, rx).(*circusv1.DaemonDownlink).GetDispatchCall() == nil {
		t.Fatal("dispatch lost")
	}
	if receive(t, rx).(*circusv1.DaemonDownlink).GetGoAway() == nil {
		t.Fatal("GoAway lost")
	}
	if _, err := rx.Receive(contextFor(t)); !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
}

// A consumer that never reads after a graceful close still returns the pool
// within MaxConsumerStall, without Receive or Close.
func TestGracefulCloseUndrainedReleasesWithinStall(t *testing.T) {
	track(t)
	clk := clock.New()
	o := gracefulOptions()
	budget := minBudget(t, o)
	ob := o
	ob.Budget, ob.Clock, ob.MaxConsumerStall = budget, clk, 7*time.Second
	a, b := wiretest.Pipe(wiretest.Options{})
	daemon := newLink(t, a, wire.DaemonDownlink, wire.DaemonUplink, o)
	control := newLink(t, b, wire.DaemonUplink, wire.DaemonDownlink, ob)
	want := finalResults()
	for _, m := range want {
		send(t, daemon, m)
	}
	if err := daemon.Shutdown(contextFor(t)); err != nil {
		t.Fatal(err)
	}
	wait.Recv(t, control.Done())
	// The link's original timer remains in the fake; the second timer is the
	// graceful-drain deadline.
	awaitTimers(t, clk, 2)
	if s := control.Stats(); s.ReceivedMessages != len(want) || budget.Stats().Receive.Used == 0 {
		t.Fatalf("delivered messages must survive until the deadline: %+v", s)
	}
	clk.Advance(ob.MaxConsumerStall - time.Second)
	if control.Stats().ReceivedMessages != len(want) {
		t.Fatal("dropped before the deadline")
	}
	clk.Advance(time.Second)
	eventually(t, func() bool { return budget.Stats().Receive.Used == 0 })
	// The reserve-only prefix stays; then the close error.
	if got := receive(t, control).(*circusv1.DaemonUplink).GetResult().GetOutput().GetContent()[0].GetText(); got != "small" {
		t.Fatal(got)
	}
	if _, err := control.Receive(contextFor(t)); !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
}

// An error drops from the first shared-charged message but still returns the
// leading reserve-only prefix, then the error.
func TestFailureKeepsReservePrefix(t *testing.T) {
	track(t)
	o := poolOptions()
	budget := minBudget(t, o)
	o.Budget = budget
	a, b := wiretest.Pipe(wiretest.Options{})
	defer a.Close()
	l := newLink(t, b, wire.DaemonUplink, wire.DaemonDownlink, o)
	frame(t, a, result("first"))
	frame(t, a, result(strings.Repeat("x", 60<<10)))
	frame(t, a, result("third"))
	eventually(t, func() bool { return l.Stats().ReceivedMessages == 3 })
	if err := a.WriteMessage(contextFor(t), []byte{0xff}); err != nil {
		t.Fatal(err)
	}
	wait.Recv(t, l.Done())
	if s := budget.Stats(); s.Receive.Used != 0 || l.Stats().ReceivedMessages != 1 {
		t.Fatal("error must release shared charges at once", s, l.Stats())
	}
	if got := receive(t, l).(*circusv1.DaemonUplink).GetResult().GetOutput().GetContent()[0].GetText(); got != "first" {
		t.Fatal(got)
	}
	var pe *wire.ProtocolError
	if _, err := l.Receive(contextFor(t)); !errors.As(err, &pe) {
		t.Fatal(err)
	}
}
