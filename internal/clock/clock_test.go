package clock

import (
	"testing"
	"time"
)

func TestFakeWaiters(t *testing.T) {
	c := New()
	if got := c.Waiters(); got != 0 {
		t.Fatalf("new clock has %d waiters", got)
	}
	c.After(0)
	if got := c.Waiters(); got != 0 {
		t.Fatalf("immediate timer left %d waiters", got)
	}
	c.After(time.Second)
	c.After(2 * time.Second)
	if got := c.Waiters(); got != 2 {
		t.Fatalf("armed timers = %d, want 2", got)
	}
	c.Advance(time.Second)
	if got := c.Waiters(); got != 1 {
		t.Fatalf("timers after advance = %d, want 1", got)
	}
}
