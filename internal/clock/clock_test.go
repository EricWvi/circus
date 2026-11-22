package clock

import (
	"context"
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

func TestUntil(t *testing.T) {
	c := New()
	start := c.Now()
	late := Until(c, start.Add(time.Second))
	c.Advance(time.Second)
	select {
	case <-late:
	default:
		t.Fatal("timer armed for a deadline did not fire at it")
	}
	select {
	case <-Until(c, start):
	default:
		t.Fatal("timer armed for a past deadline did not fire immediately")
	}
	if got := c.Waiters(); got != 0 {
		t.Fatalf("waiters = %d, want 0", got)
	}
}

func TestWithTimeoutUsesClock(t *testing.T) {
	c := New()
	ctx, cancel := WithTimeout(context.Background(), c, time.Second)
	defer cancel()
	c.Advance(time.Second - time.Nanosecond)
	select {
	case <-ctx.Done():
		t.Fatal("expired early")
	default:
	}
	c.Advance(time.Nanosecond)
	<-ctx.Done()
	parent, stop := context.WithCancel(context.Background())
	child, cancel2 := WithTimeout(parent, c, time.Hour)
	stop()
	<-child.Done()
	cancel2()
}
