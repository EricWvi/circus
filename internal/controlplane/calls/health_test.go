package calls_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/EricWvi/circus/internal/controlplane/calls"
	"github.com/EricWvi/circus/internal/controlplane/gateway"
	"github.com/EricWvi/circus/internal/store"
)

type failingTimerWalkStore struct {
	store.Store
	walks atomic.Int64
}

func (s *failingTimerWalkStore) ListTimers(context.Context, store.TimerFilter) ([]store.Timer, error) {
	s.walks.Add(1)
	return nil, errors.New("timer walk unavailable")
}

func TestHealthyDoesNotWalkDurableTimers(t *testing.T) {
	wrapper := &failingTimerWalkStore{}
	f := setup(t, func(c *calls.Config, _ *gateway.Config) {
		wrapper.Store = c.Store
		c.Store = wrapper
	})
	if !f.router.Healthy() {
		t.Fatal("new router is unhealthy")
	}
	if got := wrapper.walks.Load(); got != 0 {
		t.Fatalf("Healthy walked durable timers %d times", got)
	}
	if f.router.Stats().StoreHealthy {
		t.Fatal("Stats did not report the durable timer walk failure")
	}
	if got := wrapper.walks.Load(); got == 0 {
		t.Fatal("test did not exercise Stats' durable timer walk")
	}
	if !f.router.Healthy() {
		t.Fatal("durable timer walk failure changed the in-memory health bit")
	}
}

func TestHealthyRecoversAfterPeriodicStoreProbe(t *testing.T) {
	f := setup(t)
	f.fault.FailAllWrites(store.ErrBusy)
	s, err := f.router.Submit(f.ctx, "u", "desk", call("probe"))
	must(t, err)
	_ = s.Close()
	f.fault.FailAllWrites(nil)
	if f.router.Healthy() {
		t.Fatal("failed submission did not clear store health")
	}
	advanceUntil(t, f, f.router.Healthy)
}

type probeTestStore struct {
	store.Store
	failReads  atomic.Bool
	reads      atomic.Int64
	started    chan struct{}
	release    chan struct{}
	finished   chan struct{}
	once       sync.Once
	finishOnce sync.Once
}

func (s *probeTestStore) ListTimers(ctx context.Context, filter store.TimerFilter) ([]store.Timer, error) {
	s.reads.Add(1)
	if s.finished != nil {
		defer s.finishOnce.Do(func() { close(s.finished) })
	}
	if s.started != nil {
		s.once.Do(func() { close(s.started) })
		select {
		case <-s.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if s.failReads.Load() {
		return nil, errors.New("probe read failed")
	}
	return s.Store.ListTimers(ctx, filter)
}

func TestHealthProbeFailureKeepsUnhealthy(t *testing.T) {
	wrapper := &probeTestStore{}
	wrapper.failReads.Store(true)
	f := setup(t, func(c *calls.Config, _ *gateway.Config) {
		wrapper.Store = c.Store
		c.Store = wrapper
	})
	f.fault.FailAllWrites(store.ErrBusy)
	s, err := f.router.Submit(f.ctx, "u", "desk", call("probe-error"))
	must(t, err)
	_ = s.Close()
	f.fault.FailAllWrites(nil)
	if f.router.Healthy() {
		t.Fatal("failed submission did not clear store health")
	}
	advanceUntil(t, f, func() bool { return wrapper.reads.Load() > 0 })
	if f.router.Healthy() {
		t.Fatal("failed store probe restored health")
	}
	wrapper.failReads.Store(false)
	advanceUntil(t, f, f.router.Healthy)
}

func TestHealthProbeDoesNotHoldRouterLock(t *testing.T) {
	wrapper := &probeTestStore{started: make(chan struct{}), release: make(chan struct{})}
	f := setup(t, func(c *calls.Config, _ *gateway.Config) {
		wrapper.Store = c.Store
		c.Store = wrapper
	})
	f.fault.FailAllWrites(store.ErrBusy)
	s, err := f.router.Submit(f.ctx, "u", "desk", call("probe-block"))
	must(t, err)
	_ = s.Close()
	f.fault.FailAllWrites(nil)
	if f.router.Healthy() {
		t.Fatal("failed submission did not clear store health")
	}
	advanceUntil(t, f, func() bool { return closed(wrapper.started) })
	ctx, cancel := context.WithTimeout(f.ctx, 100*time.Millisecond)
	defer cancel()
	defer close(wrapper.release)
	s, err = f.router.Submit(ctx, "u", "desk", call("during-probe"))
	if err != nil {
		t.Fatalf("Submit queued behind the store probe: %v", err)
	}
	_ = s.Close()
}

func TestHealthProbeDoesNotOverwriteNewerFailure(t *testing.T) {
	wrapper := &probeTestStore{started: make(chan struct{}), release: make(chan struct{}), finished: make(chan struct{})}
	f := setup(t, func(c *calls.Config, _ *gateway.Config) {
		wrapper.Store = c.Store
		c.Store = wrapper
	})
	f.fault.FailAllWrites(store.ErrBusy)
	first, err := f.router.Submit(f.ctx, "u", "desk", call("first-failure"))
	must(t, err)
	_ = first.Close()
	f.fault.FailAllWrites(nil)
	if f.router.Healthy() {
		t.Fatal("first failed write did not clear health")
	}
	advanceUntil(t, f, func() bool { return closed(wrapper.started) })
	f.fault.FailAllWrites(store.ErrBusy)
	second, err := f.router.Submit(f.ctx, "u", "desk", call("second-failure"))
	must(t, err)
	_ = second.Close()
	f.fault.FailAllWrites(nil)
	if f.router.Healthy() {
		t.Fatal("second failed write did not keep health false")
	}
	close(wrapper.release)
	select {
	case <-wrapper.finished:
	case <-time.After(time.Second):
		t.Fatal("probe did not finish")
	}
	if f.router.Healthy() {
		t.Fatal("older probe overwrote a newer failed write")
	}
}

// advanceUntil advances the fake clock one sweep interval at a time until p
// holds. A single Advance can race the sweep goroutine re-arming its alarm
// (it re-arms only after the previous probe returns), so one tick may be lost
// under load; repeating the tick makes the wait independent of scheduling.
func advanceUntil(t testing.TB, f *fixture, p func() bool) {
	t.Helper()
	end := time.Now().Add(5 * time.Second)
	for !p() {
		if time.Now().After(end) {
			t.Fatal("condition did not become true")
		}
		f.clock.Advance(time.Second)
		time.Sleep(time.Millisecond)
	}
}

func closed(c <-chan struct{}) bool {
	select {
	case <-c:
		return true
	default:
		return false
	}
}
