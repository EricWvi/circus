package ledgerslots

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSlots(t *testing.T) {
	s := New(2, 1, 1)
	if !s.TryTake("a") || s.TryTake("a") || !s.TryTake("b") || s.TryTake("c") {
		t.Fatal("slots beyond the limits")
	}
	taken := make(chan error, 1)
	go func() { taken <- s.Take(t.Context(), "c") }()
	// The one waiter allowed waits; a second is refused at once.
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.mu.Lock()
		waiting := s.waiters
		s.mu.Unlock()
		if waiting == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no waiter")
		}
		time.Sleep(time.Millisecond)
	}
	if err := s.Take(t.Context(), "d"); !errors.Is(err, ErrTooManyWaiters) {
		t.Fatalf("second waiter: %v", err)
	}
	select {
	case err := <-taken:
		t.Fatalf("taken while full: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	s.Release("a")
	if err := <-taken; err != nil {
		t.Fatal(err)
	}
	// A waiter whose context ends gives up.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := s.Take(ctx, "e"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled wait: %v", err)
	}
	s.Release("b")
	s.Release("c")
	if !s.TryTake("a") || !s.TryTake("b") {
		t.Fatal("slots not released")
	}
}
