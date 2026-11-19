// The event bus (bus.go) carries committed session events from the commits
// that produce them (the worker link and the user-facing SessionService) to
// WatchSession streams in this process.
//
// Durable events are only announced: Bus.Durable records the highest
// committed seq, and each watcher reads the events from the store by seq.
// Ephemeral events are carried, best effort: a watcher that falls behind
// loses ephemeral events, never durable ones. Each watcher has one queue of
// items in publication order; an item is an ephemeral event or a durable
// mark ("events up to seq are committed"). Consecutive marks coalesce into
// one, and a full queue drops new ephemeral events but never a mark, so
// marks never outnumber the ephemeral events in the queue by more than one.
//
// Worst-case memory, with the defaults (DefaultBusOptions), of the bus and
// of the WatchSession streams that consume it (internal/controlplane/webapi,
// watch.go), counted from the code:
//
//   - streams: at most MaxWatchers (256) in all and MaxWatchersPerUser (32)
//     per Web user; Watch refuses more.
//   - per stream, everything it may hold at once:
//     the bus queue: ephemeral events charged at most MaxQueuedBytes
//     (256 KiB, payload plus EphemeralOverhead each, so at most 256 KiB of
//     payload), and at most 2 × MaxQueuedEvents + 1 items of 16 bytes,
//     about 16 KiB with slice growth;
//     the batch Take returned and the stream is delivering, while the
//     queue refills: the same again, 272 KiB;
//     one replay page: at most 256 KiB of payload (watchPageBytes; one
//     stored payload is at most 256 KiB) and 256 rows of about 150 bytes,
//     about 294 KiB;
//     the event being converted: its payload as a string (256 KiB) and,
//     for invalid UTF-8 only, the repaired copy (at most twice as long,
//     512 KiB);
//     the message being encoded: Connect's JSON encoding escapes a
//     payload byte into at most six (\u00XX), so about 1.5 MiB, appended to
//     a pooled buffer that grows by about 1.25× per step, so up to about
//     1.9 MiB; the binary encoding about 256 KiB. Response compression is
//     off for SessionService, so no compressor state. The progress-based
//     writer (webapi's streamWriter) copies nothing.
//     Together about 3.47 MiB per stream (3,552 KiB).
//   - in all: 256 × 3.47 MiB ≈ 888 MiB. Encoding buffers Connect's pool
//     keeps after use are released by the next GC. An ephemeral event whose charge
//     alone exceeds MaxQueuedBytes is dropped for every watcher, and
//     watchers of the same session share each queued event, so the usual
//     figure is far lower.
//
// Durable announcements cost nothing per event: a watcher keeps one mark
// per run of announcements between ephemeral events.

package sessions

import (
	"errors"
	"sync"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"google.golang.org/protobuf/proto"
)

// Bus defaults.
const (
	DefaultMaxWatchers        = 256
	DefaultMaxWatchersPerUser = 32
	DefaultMaxQueuedEvents    = 256
	DefaultMaxQueuedBytes     = 256 << 10
	// EphemeralOverhead is charged per queued ephemeral event on top of its
	// payload: the event's other fields and the queue item.
	EphemeralOverhead = 128
)

// Watch refusals and the reasons a watcher ends.
var (
	// ErrTooManyWatchers: the bus already has MaxWatchers watchers.
	ErrTooManyWatchers = errors.New("sessions: too many session watchers")
	// ErrTooManyUserWatchers: the user already has MaxWatchersPerUser
	// watchers.
	ErrTooManyUserWatchers = errors.New("sessions: too many session watchers for this user")
	// ErrBusClosed: the bus was closed (the server is shutting down).
	ErrBusClosed = errors.New("sessions: event bus closed")
	// ErrSessionDeleted: the watched session was deleted.
	ErrSessionDeleted = errors.New("sessions: session deleted")
)

// BusOptions bounds the bus. Zero fields take the defaults.
type BusOptions struct {
	MaxWatchers        int
	MaxWatchersPerUser int
	// MaxQueuedEvents and MaxQueuedBytes bound each watcher's queued
	// ephemeral events, by count and by charge (payload bytes plus
	// EphemeralOverhead per event).
	MaxQueuedEvents int
	MaxQueuedBytes  int64
}

// DefaultBusOptions returns the defaults.
func DefaultBusOptions() BusOptions {
	return BusOptions{
		MaxWatchers: DefaultMaxWatchers, MaxWatchersPerUser: DefaultMaxWatchersPerUser,
		MaxQueuedEvents: DefaultMaxQueuedEvents, MaxQueuedBytes: DefaultMaxQueuedBytes,
	}
}

// Bus fans committed session events out to WatchSession streams. Publishers
// never block and never fail. Durable events are only announced (watchers
// read them from the store by seq); ephemeral events are carried, best
// effort: a watcher that falls behind loses ephemeral events, never durable
// ones.
type Bus struct {
	opts BusOptions

	mu       sync.RWMutex
	closed   bool
	sessions map[string]map[*Watcher]struct{}
	perUser  map[string]int
	total    int
}

// NewBus returns an empty bus.
func NewBus(opts BusOptions) *Bus {
	d := DefaultBusOptions()
	if opts.MaxWatchers <= 0 {
		opts.MaxWatchers = d.MaxWatchers
	}
	if opts.MaxWatchersPerUser <= 0 {
		opts.MaxWatchersPerUser = d.MaxWatchersPerUser
	}
	if opts.MaxQueuedEvents <= 0 {
		opts.MaxQueuedEvents = d.MaxQueuedEvents
	}
	if opts.MaxQueuedBytes <= 0 {
		opts.MaxQueuedBytes = d.MaxQueuedBytes
	}
	return &Bus{opts: opts, sessions: map[string]map[*Watcher]struct{}{}, perUser: map[string]int{}}
}

// Durable announces that durable events up to and including seq are
// committed for the session. Call it after commit, with the highest seq the
// transaction wrote (store results report it).
func (b *Bus) Durable(sessionID string, seq uint64) {
	if seq == 0 {
		return
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	for w := range b.sessions[sessionID] {
		w.durable(seq)
	}
}

// Ephemeral delivers a live-only event (seq 0) to current watchers. The bus
// keeps ev, shared by every watcher; the caller must not modify it
// afterwards. An event without the ephemeral flag or with a seq is copied
// and corrected first.
func (b *Bus) Ephemeral(sessionID string, ev *circusv1.SessionEvent) {
	if ev == nil {
		return
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	watchers := b.sessions[sessionID]
	if len(watchers) == 0 {
		return
	}
	if ev.GetSeq() != 0 || !ev.GetEphemeral() {
		ev = proto.CloneOf(ev)
		ev.Seq, ev.Ephemeral = 0, true
	}
	charge := int64(len(ev.GetPayloadJson())) + EphemeralOverhead
	for w := range watchers {
		w.ephemeral(ev, charge)
	}
}

// Watch registers a watcher of sessionID for the Web user userID. The
// caller authorizes the session first and must Close the watcher. It
// receives what is published after Watch returns; durable events committed
// before must be read from the store. Errors: ErrTooManyWatchers,
// ErrTooManyUserWatchers, ErrBusClosed.
func (b *Bus) Watch(sessionID, userID string) (*Watcher, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch {
	case b.closed:
		return nil, ErrBusClosed
	case b.total >= b.opts.MaxWatchers:
		return nil, ErrTooManyWatchers
	case b.perUser[userID] >= b.opts.MaxWatchersPerUser:
		return nil, ErrTooManyUserWatchers
	}
	w := &Watcher{bus: b, sessionID: sessionID, userID: userID, ready: make(chan struct{}, 1), done: make(chan struct{})}
	set := b.sessions[sessionID]
	if set == nil {
		set = map[*Watcher]struct{}{}
		b.sessions[sessionID] = set
	}
	set[w] = struct{}{}
	b.perUser[userID]++
	b.total++
	return w, nil
}

// Deleted ends every watcher of the session with ErrSessionDeleted. Call it
// after a session's deletion commits.
func (b *Bus) Deleted(sessionID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for w := range b.sessions[sessionID] {
		b.removeLocked(w)
		w.end(ErrSessionDeleted)
	}
}

// Close ends every watcher with ErrBusClosed and refuses new ones. It is
// idempotent.
func (b *Bus) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	for _, set := range b.sessions {
		for w := range set {
			b.removeLocked(w)
			w.end(ErrBusClosed)
		}
	}
}

// Watchers reports the number of registered watchers.
func (b *Bus) Watchers() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.total
}

// removeLocked unregisters w once; b.mu is held for writing.
func (b *Bus) removeLocked(w *Watcher) {
	set, ok := b.sessions[w.sessionID]
	if !ok {
		return
	}
	if _, ok := set[w]; !ok {
		return
	}
	delete(set, w)
	if len(set) == 0 {
		delete(b.sessions, w.sessionID)
	}
	if b.perUser[w.userID]--; b.perUser[w.userID] == 0 {
		delete(b.perUser, w.userID)
	}
	b.total--
}

// Item is one queued item: an ephemeral Event, or, with Event nil, a mark
// announcing that durable events up to and including Seq are committed.
type Item struct {
	Seq   uint64
	Event *circusv1.SessionEvent
}

// Watcher is one registered watcher. Its methods are safe for concurrent
// use, but one goroutine normally consumes it.
type Watcher struct {
	bus               *Bus
	sessionID, userID string
	// ready holds a signal when items are queued or the watcher ended.
	ready chan struct{}
	// done is closed when the watcher ended (deletion or Close).
	done chan struct{}

	mu        sync.Mutex
	items     []Item
	events    int   // ephemeral events in items
	charge    int64 // their charge
	announced uint64
	dropped   uint64
	err       error
}

// Ready is signalled when Take has something to return.
func (w *Watcher) Ready() <-chan struct{} { return w.ready }

// Done is closed once the watcher has ended: its session was deleted or
// the bus closed. Close does not close it.
func (w *Watcher) Done() <-chan struct{} { return w.done }

// Take returns the queued items in publication order and empties the
// queue. Once the watcher has ended it returns the reason
// (ErrSessionDeleted or ErrBusClosed) and no items.
func (w *Watcher) Take() ([]Item, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return nil, w.err
	}
	items := w.items
	w.items, w.events, w.charge = nil, 0, 0
	return items, nil
}

// Dropped reports how many ephemeral events the watcher lost because its
// queue was full.
func (w *Watcher) Dropped() uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.dropped
}

// Close unregisters the watcher and releases its queue. It is idempotent.
func (w *Watcher) Close() {
	w.bus.mu.Lock()
	w.bus.removeLocked(w)
	w.bus.mu.Unlock()
	w.mu.Lock()
	w.items, w.events, w.charge = nil, 0, 0
	w.mu.Unlock()
}

func (w *Watcher) signal() {
	select {
	case w.ready <- struct{}{}:
	default:
	}
}

func (w *Watcher) durable(seq uint64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil || seq <= w.announced {
		return
	}
	w.announced = seq
	if n := len(w.items); n > 0 && w.items[n-1].Event == nil {
		w.items[n-1].Seq = seq
	} else {
		w.items = append(w.items, Item{Seq: seq})
	}
	w.signal()
}

func (w *Watcher) ephemeral(ev *circusv1.SessionEvent, charge int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return
	}
	if w.events >= w.bus.opts.MaxQueuedEvents || w.charge+charge > w.bus.opts.MaxQueuedBytes {
		w.dropped++
		return
	}
	w.items = append(w.items, Item{Event: ev})
	w.events++
	w.charge += charge
	w.signal()
}

func (w *Watcher) end(err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err == nil {
		w.err = err
		w.items, w.events, w.charge = nil, 0, 0
		close(w.done)
	}
	w.signal()
}
