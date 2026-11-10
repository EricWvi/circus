package bashtool

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"time"
)

// groupTracker owns leftover process groups without IDs, events or model tasks.
// They have no lifetime limit; ending them with the session is planned for a
// later milestone.
type groupTracker struct {
	mu     sync.Mutex
	closed bool
	stops  map[int]chan struct{}
	wg     sync.WaitGroup
	done   chan struct{}
}

// trackGroup takes sole ownership of out, keeping its active-file exemption.
func (b *Bash) trackGroup(pgid int, out *os.File, removeInline bool) {
	g := &b.groups
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		stopGroup(pgid)
		waitGroupEmpty(pgid)
		b.environment.outputBudget.finish(out.Name())
		if message := b.outputStopMessage(out); message != "" {
			_ = appendOutputLimit(out, message)
		}
		_ = out.Close()
		if removeInline {
			_ = os.Remove(out.Name())
		}
		b.environment.releaseOutput(out.Name())
		return
	}
	if g.stops == nil {
		g.stops = make(map[int]chan struct{})
	}
	stop := make(chan struct{})
	g.stops[pgid] = stop
	g.wg.Add(1)
	g.mu.Unlock()
	go func() {
		defer g.wg.Done()
		defer func() {
			b.environment.outputBudget.finish(out.Name())
			_ = out.Close()
			if removeInline {
				_ = os.Remove(out.Name())
			}
			b.environment.releaseOutput(out.Name())
			g.mu.Lock()
			delete(g.stops, pgid)
			g.mu.Unlock()
		}()
		tick := time.NewTicker(outputPollInterval)
		defer tick.Stop()
		for groupAlive(pgid) {
			select {
			case <-stop:
				// Deviation: native leaves children after CLI exit. A long-lived
				// daemon must stop its tracked groups during shutdown.
				stopGroup(pgid)
				waitGroupEmpty(pgid)
				b.environment.outputBudget.finish(out.Name())
				if message := b.outputStopMessage(out); message != "" {
					_ = appendOutputLimit(out, message)
				}
				return
			case <-tick.C:
				if message := b.outputStopMessage(out); message != "" {
					// Deviation: native leaves ampersand child output unbounded.
					killGroup(pgid)
					waitGroupEmpty(pgid)
					_ = appendOutputLimit(out, message)
					return
				}
			}
		}
	}()
}

// Shutdown stops internal groups and prevents later background shell exits from
// leaving new groups behind. It does not stop jobs.Manager's visible jobs.
// If ctx expires, shutdown continues and can be waited on again.
func (b *Bash) Shutdown(ctx context.Context) error {
	g := &b.groups
	g.mu.Lock()
	if !g.closed {
		g.closed = true
		g.done = make(chan struct{})
		for _, stop := range g.stops {
			close(stop)
		}
		b.environment.mu.Lock()
		cancel, warmDone := b.environment.warmCancel, b.environment.warmDone
		b.environment.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		go func() {
			g.wg.Wait()
			if warmDone != nil {
				<-warmDone
			}
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

func waitGroupEmpty(pgid int) { waitGroupEmptyWithin(pgid, groupExitWait) }

func waitGroupEmptyWithin(pgid int, limit time.Duration) bool {
	timer := time.NewTimer(limit)
	defer timer.Stop()
	tick := time.NewTicker(outputPollInterval)
	defer tick.Stop()
	for groupAlive(pgid) {
		select {
		case <-tick.C:
		case <-timer.C:
			slog.Warn("process group did not become empty; releasing tracker resources", "pgid", pgid, "timeout", limit)
			return false
		}
	}
	return true
}
