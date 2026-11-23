package sessions

import (
	"context"
	"log/slog"
	"testing"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/store"
)

// lockProbe is a handler that counts its calls made while s.mu is held.
type lockProbe struct {
	s              *Service
	under, handled int
}

func (h *lockProbe) check() {
	if h.s.mu.TryLock() {
		h.s.mu.Unlock()
	} else {
		h.under++
	}
}

func (h *lockProbe) Enabled(context.Context, slog.Level) bool  { h.check(); return true }
func (h *lockProbe) Handle(context.Context, slog.Record) error { h.check(); h.handled++; return nil }
func (h *lockProbe) WithAttrs([]slog.Attr) slog.Handler        { return h }
func (h *lockProbe) WithGroup(string) slog.Handler             { return h }

// TestNoHandlerCallUnderMutex: the debug records collected under s.mu
// (an Offer withdrawn, a stale read, a dropped binding) follow the level
// decided by New and are written after s.mu is released; no handler method
// runs in between.
func TestNoHandlerCallUnderMutex(t *testing.T) {
	s := &Service{dbg: true}
	h := &lockProbe{s: s}
	s.log = slog.New(h)
	s.mu.Lock()
	s.setOffer("s1", &entry{offered: &Conn{offers: 1}}, nil, "test")
	s.staleLog("stale read retried", "s1", 1)
	s.bindingDropped("s1", holder{worker: "w", boot: "b", epoch: 1}, "test")
	s.unlock()
	if h.under != 0 || h.handled != 3 {
		t.Fatalf("%d handler calls under s.mu, %d records written; want 0 and 3", h.under, h.handled)
	}
}

// panicBus panics in Durable, breaking its contract.
type panicBus struct{}

func (panicBus) Durable(string, uint64)                   { panic("durable") }
func (panicBus) Ephemeral(string, *circusv1.SessionEvent) {}

// TestLostReleasesMutexOnPanic: a callback that panics inside lost still
// leaves s.mu released.
func TestLostReleasesMutexOnPanic(t *testing.T) {
	s := &Service{cfg: Config{Bus: panicBus{}}, entries: map[string]*entry{}}
	func() {
		defer func() { _ = recover() }()
		s.lost(store.LeaseLoss{Released: store.Lease{SessionID: "s1", WorkerID: "w", Epoch: 1}, Interrupted: true,
			Session: store.Session{LastEventSeq: 3}}, nil, false, endExpired)
	}()
	if !s.mu.TryLock() {
		t.Fatal("s.mu still held after a panic in lost")
	}
	s.mu.Unlock()
}
