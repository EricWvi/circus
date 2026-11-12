package timers_test

import (
	"context"
	"errors"
	"github.com/EricWvi/circus/internal/clock"
	"github.com/EricWvi/circus/internal/controlplane/timers"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/store/sqlite"
	"go.uber.org/goleak"
	"path/filepath"
	"testing"
	"time"
)

func TestClaimCompleteAndJoin(t *testing.T) {
	baseline := goleak.IgnoreCurrent()
	t.Cleanup(func() { goleak.VerifyNone(t, baseline) })
	s, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "db"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := clock.New()
	if err = s.SetTimer(t.Context(), timers.DeviceGrace, "d", c.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	handled := make(chan struct{}, 1)
	r, err := timers.New(timers.Config{Store: s, Clock: c, InstanceID: "i", Lease: time.Second, PollInterval: time.Second, BatchSize: 1, Handler: func(ctx context.Context, claim store.Timer) error {
		completed := false
		err := s.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
			var err error
			completed, err = tx.CompleteTimer(ctx, claim)
			return err
		})
		if err == nil && completed {
			handled <- struct{}{}
		}
		return err
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	select {
	case <-handled:
	case <-time.After(5 * time.Second):
		t.Fatal("timer not handled")
	}
	cancel()
	<-done
	if got := r.Stats(); got.Running || got.Active != 0 || got.Claims != 0 {
		t.Fatal(got)
	}
}

// A failing handler leaves its claim to lease expiry; the timer is then claimed
// again with a fresh token. A timer that is not due is never handed out.
func TestFailedClaimIsReclaimedAfterLease(t *testing.T) {
	s, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "db"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := clock.New()
	now := c.Now().UnixMilli()
	if err = s.SetTimer(t.Context(), timers.CallWatchdog, "due", now); err != nil {
		t.Fatal(err)
	}
	if err = s.SetTimer(t.Context(), timers.CallWatchdog, "later", now+time.Hour.Milliseconds()); err != nil {
		t.Fatal(err)
	}
	claims := make(chan store.Timer, 4)
	failures := 1
	r, err := timers.New(timers.Config{Store: s, Clock: c, InstanceID: "i", Lease: time.Minute, PollInterval: time.Second, BatchSize: 4, Handler: func(ctx context.Context, claim store.Timer) error {
		claims <- claim
		if failures > 0 {
			failures--
			return errors.New("injected handler failure")
		}
		return s.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
			_, err := tx.CompleteTimer(ctx, claim)
			return err
		})
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	first := <-claims
	if first.SubjectID != "due" {
		t.Fatal(first)
	}
	waitUntil(t, func() bool { st := r.Stats(); return st.Claims == 1 })
	var second store.Timer
	waitUntil(t, func() bool {
		c.Advance(30 * time.Second)
		r.Wake()
		select {
		case second = <-claims:
			return true
		default:
			return false
		}
	})
	if second.SubjectID != "due" || second.Token == first.Token || second.Attempts != 2 {
		t.Fatalf("reclaim: %+v after %+v", second, first)
	}
	waitUntil(t, func() bool {
		rows, err := s.ListTimers(t.Context(), store.TimerFilter{Kind: timers.CallWatchdog, SubjectID: "due", Limit: 1})
		return err == nil && len(rows) == 0
	})
	cancel()
	<-done
	if c.Now().UnixMilli() < now+time.Hour.Milliseconds() {
		select {
		case extra := <-claims:
			t.Fatalf("timer handed out before due: %+v", extra)
		default:
		}
	}
	if got := r.Stats(); got.Running || got.Active != 0 {
		t.Fatal(got)
	}
}

func waitUntil(t *testing.T, p func() bool) {
	t.Helper()
	end := time.Now().Add(5 * time.Second)
	for !p() {
		if time.Now().After(end) {
			t.Fatal("condition did not become true")
		}
		time.Sleep(time.Millisecond)
	}
}
