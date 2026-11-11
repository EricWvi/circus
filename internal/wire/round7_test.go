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
	"github.com/EricWvi/circus/internal/wire"
	"github.com/EricWvi/circus/internal/wire/wiretest"
	"google.golang.org/protobuf/proto"
)

func gracefulOptions() wire.Options {
	return wire.Options{MaxMessageBytes: 1 << 20, MaxFragmentBytes: 64 << 10, DisableHeartbeat: true}
}

// finalResults: reserve-only, shared unfragmented and shared fragmented results.
func finalResults() []proto.Message {
	return []proto.Message{result("small"), result(strings.Repeat("1", 40<<10)), result(strings.Repeat("2", 300<<10)), result("tail")}
}

// Review 4 High 1: results a peer delivered before closing normally reach a
// budgeted consumer that keeps calling Receive, every time.
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
		go func() {
			n := 0
			for {
				if _, err := control.Receive(context.Background()); err != nil {
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
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := daemon.Shutdown(ctx); err != nil {
			t.Fatal(err)
		}
		cancel()
		if n := <-got; n != len(want) || !errors.Is(final, io.EOF) {
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
	<-rx.Done()
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
	clk := &notifyingClock{Fake: clock.New(), registered: make(chan time.Duration, 64)}
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
	<-control.Done()
	for awaitRegistered(t, clk) != ob.MaxConsumerStall {
	}
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

// Review 4 Low 1 (Z2): an error drops from the first shared-charged message but
// still returns the leading reserve-only prefix, then the error.
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
	<-l.Done()
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
