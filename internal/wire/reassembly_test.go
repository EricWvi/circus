package wire_test

import (
	"runtime"
	"strings"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/clock"
	"github.com/EricWvi/circus/internal/wire"
	"github.com/EricWvi/circus/internal/wire/wiretest"
)

// Reserve two 64 MiB declarations, but allocate only received blocks.
func TestIncrementalReassemblyAllocation(t *testing.T) {
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

type quietClock struct{ *clock.Fake }

func (c quietClock) After(time.Duration) <-chan time.Time { return make(chan time.Time) }
func TestReadPathMissingFragment(t *testing.T) {
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
func TestUnfragmentedLimit(t *testing.T) {
	track(t)
	a, b := wiretest.Pipe(wiretest.Options{})
	defer a.Close()
	l := newLink(t, b, wire.DaemonUplink, wire.DaemonDownlink, smallOptions())
	m := result(strings.Repeat("x", 80))
	frame(t, a, m)
	select {
	case <-l.Done():
	case <-contextFor(t).Done():
		t.Fatal("oversized unfragmented envelope accepted")
	}
}
func TestTinyFragmentsDoNotAmplifyBlockHeaders(t *testing.T) {
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

func TestInputLimitRemainsConfigurable(t *testing.T) {
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
