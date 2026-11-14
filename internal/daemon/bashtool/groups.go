package bashtool

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// groupTracker owns leftover process groups without IDs, events or model tasks.
// They have no lifetime limit; ending them with the session is planned for a
// later milestone.
type groupTracker struct {
	mu     sync.Mutex
	closed bool
	stops  map[int]*trackedGroup
	wg     sync.WaitGroup
	done   chan struct{}
}

type trackedGroup struct {
	stop chan struct{}
	once sync.Once
}

func (g *trackedGroup) requestStop() { g.once.Do(func() { close(g.stop) }) }

// trackCapture atomically transfers the driver role and installs the tracker
// entry. The capture keeps its original file.
func (b *Bash) trackCapture(c *capture) error {
	g := &b.groups
	g.mu.Lock()
	c.mu.Lock()
	c.state = captureState{captureTracked, ownerTracker}
	if g.closed {
		c.mu.Unlock()
		g.mu.Unlock()
		b.effects.pause("tracker-register")
		c.stopLeftovers()
		c.finishBudget()
		if message := b.outputs.outputStopMessage(c.out); message != "" {
			_ = c.writeLimit(message)
		}
		return c.release(ownerTracker, c.disposition)
	}
	if g.stops == nil {
		g.stops = make(map[int]*trackedGroup)
	}
	entry := &trackedGroup{stop: make(chan struct{})}
	c.tracking = entry
	c.mu.Unlock()
	g.stops[c.pgid()] = entry
	g.wg.Add(1)
	g.mu.Unlock()
	b.effects.pause("tracker-register")
	go func() {
		defer g.wg.Done()
		defer func() {
			c.finishBudget()
			_ = c.release(ownerTracker, c.disposition)
			b.effects.pause("tracker-remove")
			g.mu.Lock()
			if g.stops[c.pgid()] == entry {
				delete(g.stops, c.pgid())
			}
			g.mu.Unlock()
			b.effects.pause("tracker-removed")
		}()
		tick := time.NewTicker(outputPollInterval)
		defer tick.Stop()
		for groupAlive(c.pgid()) {
			select {
			case <-entry.stop:
				c.stopLeftovers()
				c.finishBudget()
				if message := b.outputs.outputStopMessage(c.out); message != "" {
					_ = c.writeLimit(message)
				}
				return
			case <-tick.C:
				if message := b.outputs.outputStopMessage(c.out); message != "" {
					c.killLeftovers()
					_ = c.writeLimit(message)
					return
				}
			}
		}
	}()
	return nil
}
func (b *Bash) stopTracked(c *capture) {
	c.mu.Lock()
	entry := c.tracking
	c.mu.Unlock()
	if entry != nil {
		entry.requestStop()
	}
}

// Shutdown stops internal groups and prevents later background shell exits from
// leaving new groups behind. It does not stop jobs.Manager's visible jobs.
// Once the groups and their outputs are released, it releases the snapshot
// instance: its directory and lock go away as soon as no visible job uses it.
// If ctx expires, shutdown continues and can be waited on again.
func (b *Bash) Shutdown(ctx context.Context) error {
	g := &b.groups
	g.mu.Lock()
	if !g.closed {
		g.closed = true
		g.done = make(chan struct{})
		for _, stop := range g.stops {
			stop.requestStop()
		}
		b.snapshots.mu.Lock()
		cancel, warmDone := b.snapshots.warmCancel, b.snapshots.warmDone
		b.snapshots.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		go func() {
			g.wg.Wait()
			if warmDone != nil {
				<-warmDone
			}
			b.snapshots.releaseInstance()
			close(g.done)
		}()
	}
	done := g.done
	g.mu.Unlock()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Inferred: stop waiting five seconds after KILL even if members cannot be
// reaped or signaled. Release our files and tracker resources on that path too.
const groupExitWait = 5 * time.Second

func waitGroupEmptyWithin(pgid int, limit time.Duration) bool {
	return waitGroupEmptyWith(pgid, limit, groupAlive)
}
func waitGroupEmptyWith(pgid int, limit time.Duration, alive func(int) bool) bool {
	timer := time.NewTimer(limit)
	defer timer.Stop()
	tick := time.NewTicker(outputPollInterval)
	defer tick.Stop()
	for alive(pgid) {
		select {
		case <-tick.C:
		case <-timer.C:
			slog.Warn("process group did not become empty; releasing tracker resources", "pgid", pgid, "timeout", limit)
			return false
		}
	}
	return true
}
