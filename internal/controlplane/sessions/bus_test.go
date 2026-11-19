package sessions

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
)

func eph(text string) *circusv1.SessionEvent {
	return &circusv1.SessionEvent{Kind: circusv1.SessionEvent_KIND_ASSISTANT, PayloadJson: text, Ephemeral: true}
}

// describe renders items as "d5" for a mark and the payload for an event.
func describe(items []Item) string {
	var parts []string
	for _, it := range items {
		if it.Event == nil {
			parts = append(parts, fmt.Sprintf("d%d", it.Seq))
		} else {
			parts = append(parts, it.Event.GetPayloadJson())
		}
	}
	return strings.Join(parts, " ")
}

func take(t *testing.T, w *Watcher) string {
	t.Helper()
	items, err := w.Take()
	if err != nil {
		t.Fatal(err)
	}
	return describe(items)
}

func mustWatch(t *testing.T, b *Bus, session, user string) *Watcher {
	t.Helper()
	w, err := b.Watch(session, user)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.Close)
	return w
}

func TestBusOrderAndCoalescing(t *testing.T) {
	b := NewBus(BusOptions{})
	w := mustWatch(t, b, "s1", "u1")
	other := mustWatch(t, b, "s2", "u1")
	select {
	case <-w.Ready():
		t.Fatal("ready before anything was published")
	default:
	}
	b.Durable("s1", 3)
	b.Durable("s1", 5)
	b.Durable("s1", 4) // older than announced: ignored
	b.Ephemeral("s1", eph("a"))
	b.Durable("s1", 6)
	b.Ephemeral("s1", eph("b"))
	b.Ephemeral("s1", eph("c"))
	b.Durable("s1", 0) // nothing
	b.Durable("s1", 8)
	b.Durable("s1", 9)
	<-w.Ready()
	if got := take(t, w); got != "d5 a d6 b c d9" {
		t.Fatalf("items = %q", got)
	}
	if got := take(t, w); got != "" {
		t.Fatalf("second take = %q", got)
	}
	b.Durable("s1", 9)
	if got := take(t, w); got != "" {
		t.Fatalf("repeated announcement queued %q", got)
	}
	if got := take(t, other); got != "" {
		t.Fatalf("other session got %q", got)
	}
	// Nothing is queued for a session without watchers.
	b.Ephemeral("nobody", eph("x"))
	b.Durable("nobody", 1)
}

func TestBusEphemeralNormalized(t *testing.T) {
	b := NewBus(BusOptions{})
	w := mustWatch(t, b, "s1", "u1")
	in := &circusv1.SessionEvent{Seq: 7, Kind: circusv1.SessionEvent_KIND_ASSISTANT, PayloadJson: "x"}
	b.Ephemeral("s1", in)
	b.Ephemeral("s1", nil)
	items, _ := w.Take()
	if len(items) != 1 || items[0].Event.GetSeq() != 0 || !items[0].Event.GetEphemeral() {
		t.Fatalf("items = %v", items)
	}
	if in.GetSeq() != 7 || in.GetEphemeral() {
		t.Fatal("the publisher's event was modified")
	}
}

func TestBusSlowWatcherLosesOnlyEphemeral(t *testing.T) {
	b := NewBus(BusOptions{MaxQueuedEvents: 3, MaxQueuedBytes: 1 << 20})
	slow := mustWatch(t, b, "s1", "u1")
	fast := mustWatch(t, b, "s1", "u2")
	var fastSeen []string
	for i := range 10 {
		b.Ephemeral("s1", eph(fmt.Sprint(i)))
		b.Durable("s1", uint64(i+1))
		fastSeen = append(fastSeen, take(t, fast))
	}
	if got := strings.Join(fastSeen, " "); got != "0 d1 1 d2 2 d3 3 d4 4 d5 5 d6 6 d7 7 d8 8 d9 9 d10" {
		t.Fatalf("fast = %q", got)
	}
	// The slow watcher kept its first three events and every mark,
	// coalesced once events stopped fitting.
	if got := take(t, slow); got != "0 d1 1 d2 2 d10" {
		t.Fatalf("slow = %q", got)
	}
	if slow.Dropped() != 7 || fast.Dropped() != 0 {
		t.Fatalf("dropped = %d, %d", slow.Dropped(), fast.Dropped())
	}
	// The queue is free again after Take.
	b.Ephemeral("s1", eph("x"))
	if got := take(t, slow); got != "x" {
		t.Fatalf("after take = %q", got)
	}
}

func TestBusByteBound(t *testing.T) {
	b := NewBus(BusOptions{MaxQueuedEvents: 100, MaxQueuedBytes: 2*EphemeralOverhead + 10})
	w := mustWatch(t, b, "s1", "u1")
	b.Ephemeral("s1", eph(strings.Repeat("a", 5)))
	b.Ephemeral("s1", eph(strings.Repeat("b", 6))) // 5 + 6 > 10
	b.Ephemeral("s1", eph(strings.Repeat("c", 5)))
	b.Ephemeral("s1", eph(strings.Repeat("d", 1)))
	if got := take(t, w); got != "aaaaa ccccc" {
		t.Fatalf("items = %q", got)
	}
	// An event larger than the whole budget never fits.
	b.Ephemeral("s1", eph(strings.Repeat("e", 300)))
	if got := take(t, w); got != "" || w.Dropped() != 3 {
		t.Fatalf("items = %q, dropped %d", got, w.Dropped())
	}
}

func TestBusWatcherCaps(t *testing.T) {
	b := NewBus(BusOptions{MaxWatchers: 3, MaxWatchersPerUser: 2})
	a1 := mustWatch(t, b, "s1", "alice")
	mustWatch(t, b, "s2", "alice")
	if _, err := b.Watch("s3", "alice"); !errors.Is(err, ErrTooManyUserWatchers) {
		t.Fatalf("third of alice: %v", err)
	}
	bob := mustWatch(t, b, "s1", "bob")
	if _, err := b.Watch("s1", "carol"); !errors.Is(err, ErrTooManyWatchers) {
		t.Fatalf("fourth in all: %v", err)
	}
	a1.Close()
	a1.Close() // idempotent: frees one slot only
	if b.Watchers() != 2 {
		t.Fatalf("watchers = %d", b.Watchers())
	}
	mustWatch(t, b, "s3", "alice")
	bob.Close()
	if _, err := b.Watch("s4", "alice"); !errors.Is(err, ErrTooManyUserWatchers) {
		t.Fatalf("alice again: %v", err)
	}
}

func TestBusDeletedAndClosed(t *testing.T) {
	b := NewBus(BusOptions{})
	w1 := mustWatch(t, b, "s1", "u1")
	w2 := mustWatch(t, b, "s1", "u2")
	keep := mustWatch(t, b, "s2", "u1")
	b.Durable("s1", 4)
	b.Deleted("s1")
	for _, w := range []*Watcher{w1, w2} {
		<-w.Ready()
		if items, err := w.Take(); !errors.Is(err, ErrSessionDeleted) || items != nil {
			t.Fatalf("take after delete = %v, %v", items, err)
		}
	}
	if b.Watchers() != 1 {
		t.Fatalf("watchers = %d", b.Watchers())
	}
	w1.Close() // after Deleted removed it: no double count
	if b.Watchers() != 1 {
		t.Fatalf("watchers after close = %d", b.Watchers())
	}
	// Publishing to a deleted session's ended watcher is ignored.
	b.Durable("s1", 5)
	b.Close()
	b.Close()
	<-keep.Ready()
	if _, err := keep.Take(); !errors.Is(err, ErrBusClosed) {
		t.Fatalf("take after close = %v", err)
	}
	if _, err := b.Watch("s2", "u1"); !errors.Is(err, ErrBusClosed) {
		t.Fatalf("watch after close = %v", err)
	}
	if b.Watchers() != 0 {
		t.Fatalf("watchers after close = %d", b.Watchers())
	}
}

// TestBusConcurrent runs publishers, watchers coming and going, deletion
// and close at once; run it with -race. Every watcher that stays sees its
// marks in increasing order and the last announced seq.
func TestBusConcurrent(t *testing.T) {
	b := NewBus(BusOptions{MaxQueuedEvents: 8})
	const sessions, perSession, last = 4, 3, 2000
	var wg sync.WaitGroup
	results := make(chan error, sessions*perSession)
	for s := range sessions {
		id := fmt.Sprintf("s%d", s)
		for u := range perSession {
			w, err := b.Watch(id, fmt.Sprintf("u%d", u))
			if err != nil {
				t.Fatal(err)
			}
			wg.Go(func() {
				defer w.Close()
				var seen uint64
				for seen < last {
					<-w.Ready()
					items, err := w.Take()
					if err != nil {
						results <- err
						return
					}
					for _, it := range items {
						if it.Event == nil {
							if it.Seq <= seen {
								results <- fmt.Errorf("%s: mark %d after %d", id, it.Seq, seen)
								return
							}
							seen = it.Seq
						}
					}
				}
				results <- nil
			})
		}
		wg.Go(func() {
			for seq := uint64(1); seq <= last; seq++ {
				b.Ephemeral(id, eph("x"))
				b.Durable(id, seq)
			}
		})
	}
	// Churn: short-lived watchers of other sessions.
	wg.Go(func() {
		for i := range 500 {
			if w, err := b.Watch(fmt.Sprintf("churn%d", i%5), "churn"); err == nil {
				b.Ephemeral(fmt.Sprintf("churn%d", i%5), eph("y"))
				if i%7 == 0 {
					b.Deleted(fmt.Sprintf("churn%d", i%5))
				}
				w.Close()
			}
		}
	})
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	if b.Watchers() != 0 {
		t.Fatalf("watchers left: %d", b.Watchers())
	}
	b.Close()
}
