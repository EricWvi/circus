// Package clocktest provides a manually advanced clock for daemon tests.
package clocktest

import (
	"sync"
	"time"
)

type timer struct {
	at time.Time
	ch chan time.Time
}

// Clock fires buffered timers synchronously during Advance. It is safe for
// concurrent use. Advancing does not wait for consumers to process their timers.
type Clock struct {
	mu     sync.Mutex
	now    time.Time
	timers []timer
}

func New() *Clock { return &Clock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)} }

func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *Clock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	at := c.now.Add(d)
	if d <= 0 {
		ch <- c.now
	} else {
		c.timers = append(c.timers, timer{at: at, ch: ch})
	}
	return ch
}

func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	pending := c.timers[:0]
	for _, timer := range c.timers {
		if !timer.at.After(c.now) {
			timer.ch <- timer.at
		} else {
			pending = append(pending, timer)
		}
	}
	clear(c.timers[len(pending):])
	c.timers = pending
}
