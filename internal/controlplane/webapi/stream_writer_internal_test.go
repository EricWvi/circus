package webapi

import (
	"bytes"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// deadlines records the deadlines a streamWriter sets, in order.
type deadlines struct {
	mu   sync.Mutex
	set  []time.Time
	hold chan struct{} // when non-nil, the first call waits for it
	in   chan struct{} // closed when the first call starts waiting
}

func (d *deadlines) setDeadline(t time.Time) error {
	d.mu.Lock()
	hold := d.hold
	d.hold = nil
	d.mu.Unlock()
	if hold != nil {
		close(d.in)
		<-hold
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.set = append(d.set, t)
	return nil
}

func (d *deadlines) all() []time.Time {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]time.Time(nil), d.set...)
}

// TestStreamWriterProgress: a large write gets a fresh deadline per chunk,
// so only a chunk without progress can time out.
func TestStreamWriterProgress(t *testing.T) {
	d := &deadlines{}
	rec := httptest.NewRecorder()
	sw := &streamWriter{ResponseWriter: rec, setDeadline: d.setDeadline, timeout: time.Hour}
	data := bytes.Repeat([]byte("x"), 3*streamWriteChunk+5)
	before := time.Now()
	if n, err := sw.Write(data); err != nil || n != len(data) {
		t.Fatalf("write = %d, %v", n, err)
	}
	if !bytes.Equal(rec.Body.Bytes(), data) {
		t.Fatal("data changed")
	}
	set := d.all()
	if len(set) != 4 {
		t.Fatalf("%d deadlines for 4 chunks", len(set))
	}
	for _, dl := range set {
		if dl.Before(before.Add(time.Hour)) {
			t.Fatalf("deadline %v is not a full timeout away", dl)
		}
	}
}

// TestStreamWriterAbort: after abort every write runs against a deadline
// in the past; resume restores the timeout.
func TestStreamWriterAbort(t *testing.T) {
	d := &deadlines{}
	sw := &streamWriter{ResponseWriter: httptest.NewRecorder(), setDeadline: d.setDeadline, timeout: time.Hour}
	sw.abort()
	if _, err := sw.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	set := d.all()
	if last := set[len(set)-1]; last.After(time.Now()) {
		t.Fatalf("write after abort set deadline %v", last)
	}
	sw.resume()
	if _, err := sw.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	set = d.all()
	if last := set[len(set)-1]; last.Before(time.Now().Add(time.Minute)) {
		t.Fatalf("write after resume set deadline %v", last)
	}
}

// TestStreamWriterAbortRace: a chunk whose deadline is being set when the
// stream ends cannot leave its later deadline in place: abort's deadline
// is always the last one.
func TestStreamWriterAbortRace(t *testing.T) {
	release := make(chan struct{})
	d := &deadlines{hold: release, in: make(chan struct{})}
	sw := &streamWriter{ResponseWriter: httptest.NewRecorder(), setDeadline: d.setDeadline, timeout: time.Hour}
	var wg sync.WaitGroup
	wg.Go(func() { _, _ = sw.Write([]byte("x")) })
	<-d.in // the chunk is setting now + timeout
	wg.Go(sw.abort)
	time.Sleep(50 * time.Millisecond) // abort is under way
	close(release)
	wg.Wait()
	set := d.all()
	if len(set) != 2 || set[1].After(time.Now()) {
		t.Fatalf("deadlines = %v; the last must be abort's", set)
	}
}

func TestStreamLifetimeJitter(t *testing.T) {
	const base = time.Hour
	lo, hi := base, time.Duration(0)
	for range 2000 {
		d := streamLifetime(base)
		if d < base*9/10 || d > base*11/10 {
			t.Fatalf("lifetime %v outside ±10 %%", d)
		}
		lo, hi = min(lo, d), max(hi, d)
	}
	if hi-lo < base/10 {
		t.Fatalf("lifetimes spread only %v", hi-lo)
	}
}
