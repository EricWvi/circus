// Package clock supplies shared clocks and a manually advanced fake.
package clock

import (
	"sync"
	"time"
)

type timer struct {
	at time.Time
	ch chan time.Time
}

// Fake fires buffered timers synchronously during Advance. It is safe for
// concurrent use. Advancing does not wait for consumers to process their timers.
type Fake struct {
	mu     sync.Mutex
	now    time.Time
	timers []timer
}

func New() *Fake { return &Fake{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)} }

func (c *Fake) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Waiters reports the number of armed timers.
func (c *Fake) Waiters() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.timers)
}

func (c *Fake) After(d time.Duration) <-chan time.Time {
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

func (c *Fake) Advance(d time.Duration) {
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

// Clock supplies time and timers for execution deadlines and retention.
type Clock interface {
	Now() time.Time
	After(time.Duration) <-chan time.Time
}

// Real delegates directly to the standard library clock.
type Real struct{}

func (Real) Now() time.Time                         { return time.Now() }
func (Real) After(d time.Duration) <-chan time.Time { return time.After(d) }
