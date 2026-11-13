package wire_test

import (
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/testutil/wait"
	"github.com/EricWvi/circus/internal/wire"
	"github.com/EricWvi/circus/internal/wire/wiretest"
	"google.golang.org/protobuf/proto"
)

// Opt in with the existing large-test memory budget; ordinary go test stays
// fast. Every sampling goroutine is joined.
func TestReceiveMemoryPhases(t *testing.T) {
	if os.Getenv("CIRCUS_WIRE_LARGE_TEST") != "1" {
		t.Skip("explicit heap phase measurement")
	}
	track(t)
	const M = 8 << 20
	src, _ := proto.Marshal(result(strings.Repeat("x", M-64)))
	a, b := wiretest.Pipe(wiretest.Options{BufferMessages: 4})
	defer a.Close()
	r := newLink(t, b, wire.DaemonUplink, wire.DaemonDownlink, wire.Options{MaxMessageBytes: M, DisableHeartbeat: true})
	heap := func() uint64 { runtime.GC(); var m runtime.MemStats; runtime.ReadMemStats(&m); return m.HeapAlloc }
	base := heap()
	var peak atomic.Uint64
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			for old := peak.Load(); m.HeapAlloc > old; old = peak.Load() {
				if peak.CompareAndSwap(old, m.HeapAlloc) {
					break
				}
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	defer func() { close(stop); wait.Recv(t, done) }()
	write := func(id uint64, full bool) {
		var index uint32
		for off := 0; off < len(src); off += wire.DefaultMaxFragmentBytes {
			end := min(off+wire.DefaultMaxFragmentBytes, len(src))
			if !full && end == len(src) {
				return
			}
			f := &circusv1.Fragment{MessageId: id, Index: index, Last: end == len(src), Data: src[off:end]}
			if off == 0 {
				f.TotalSize = uint64(len(src))
			}
			frame(t, a, fragment(f))
			index++
		}
	}
	write(1, true)
	eventually(t, func() bool { return r.Stats().ReceivedMessages == 1 })
	write(2, false)
	write(3, true)
	eventually(t, func() bool { return r.Stats().ReadBlocked })
	live := int64(heap()) - int64(base)
	t.Logf("max_message=8 MiB: post-GC live delta=%.2f MiB, sampled peak delta=%.2f MiB, stats=%+v", float64(live)/(1<<20), float64(int64(peak.Load())-int64(base))/(1<<20), r.Stats())
	if live > 3*M+(6<<20) {
		t.Fatal("unexpected live memory outside phase model", live)
	}
	_ = receive(t, r)
	_ = receive(t, r)
	runtime.KeepAlive(src)
}
