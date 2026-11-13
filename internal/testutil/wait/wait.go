// Package wait provides generous, shared bounds for asynchronous test assertions.
package wait

import (
	"context"
	"testing"
	"time"
)

// Timeout is the default upper bound for a positive test wait.
const Timeout = 10 * time.Second

// For polls p until it succeeds or Timeout expires.
func For(t testing.TB, p func() bool) {
	t.Helper()
	deadline := time.NewTimer(Timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if p() {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("condition did not become true within %s", Timeout)
		case <-ticker.C:
		}
	}
}

// Recv receives one value from ch or fails the test after Timeout.
func Recv[T any](t testing.TB, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(Timeout):
		t.Fatalf("receive did not complete within %s", Timeout)
		var zero T
		return zero
	}
}

// Ctx returns a context cancelled at Timeout and during test cleanup.
func Ctx(t testing.TB) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), Timeout)
	t.Cleanup(cancel)
	return ctx
}
