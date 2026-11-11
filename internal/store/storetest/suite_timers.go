package storetest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/EricWvi/circus/internal/store"
)

const lease = int64(10_000) // milliseconds

func claim(t testing.TB, ops store.Ops, now int64, instance string, limit int) []store.Timer {
	t.Helper()
	ts, err := ops.ClaimDueTimers(deadline(t), now, instance, lease, limit)
	must(t, err)
	return ts
}

func subjects(ts []store.Timer) []string {
	out := []string{}
	for _, t := range ts {
		out = append(out, t.Kind+"/"+t.SubjectID)
	}
	return out
}

func testTimers(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	must(t, s.SetTimer(ctx, "call_watchdog", "c1", t0+30))
	must(t, s.SetTimer(ctx, "device_grace", "d1", t0+10))
	must(t, s.SetTimer(ctx, "call_watchdog", "c2", t0+20))
	must(t, s.SetTimer(ctx, "call_watchdog", "c3", t0+20))
	must(t, s.SetTimer(ctx, "enroll_token_expiry", "e1", t0+99))
	ts, err := s.ListTimers(ctx)
	must(t, err)
	mustEqual(t, "listed", subjects(ts), []string{
		"device_grace/d1", "call_watchdog/c2", "call_watchdog/c3", "call_watchdog/c1", "enroll_token_expiry/e1",
	})

	mustEqual(t, "nothing due", subjects(claim(t, s, t0+9, "i1", 10)), []string{})
	// Earliest due first, bounded by limit.
	got := claim(t, s, t0+30, "i1", 2)
	mustEqual(t, "first batch", subjects(got), []string{"device_grace/d1", "call_watchdog/c2"})
	for _, c := range got {
		if c.ClaimedBy != "i1" || c.ClaimUntil != t0+30+lease || c.Attempts != 1 || c.Token == "" {
			t.Fatalf("claimed timer %+v", c)
		}
	}
	mustEqual(t, "second batch", subjects(claim(t, s, t0+30, "i2", 10)), []string{"call_watchdog/c3", "call_watchdog/c1"})
	mustEqual(t, "all held", subjects(claim(t, s, t0+30, "i3", 10)), []string{})

	ok, err := s.CompleteTimer(ctx, got[0])
	must(t, err)
	mustEqual(t, "complete", ok, true)
	ok, err = s.CompleteTimer(ctx, got[0])
	must(t, err)
	mustEqual(t, "complete twice", ok, false)

	// Re-setting a claimed timer clears its claim: the stale handler cannot
	// complete it, and the new due time governs.
	must(t, s.SetTimer(ctx, "call_watchdog", "c2", t0+50))
	ts, err = s.ListTimers(ctx)
	must(t, err)
	for _, tm := range ts {
		if tm.SubjectID == "c2" && (tm.ClaimedBy != "" || tm.ClaimUntil != 0 || tm.Attempts != 0 || tm.Token != "" || tm.DueAt != t0+50) {
			t.Fatalf("re-set timer kept its claim: %+v", tm)
		}
	}
	ok, err = s.CompleteTimer(ctx, got[1])
	must(t, err)
	mustEqual(t, "complete after re-set", ok, false)

	// Cancel deletes, claimed or not; cancelling twice is fine.
	must(t, s.CancelTimer(ctx, "call_watchdog", "c3"))
	must(t, s.CancelTimer(ctx, "call_watchdog", "c3"))
	must(t, s.CancelTimer(ctx, "enroll_token_expiry", "e1"))
	ts, err = s.ListTimers(ctx)
	must(t, err)
	mustEqual(t, "left", subjects(ts), []string{"call_watchdog/c1", "call_watchdog/c2"})

	for _, bad := range []func() error{
		func() error { _, err := s.ClaimDueTimers(ctx, t0, "", lease, 1); return err },
		func() error { _, err := s.ClaimDueTimers(ctx, t0, "i1", 0, 1); return err },
		func() error { _, err := s.ClaimDueTimers(ctx, t0, "i1", lease, 0); return err },
		func() error {
			_, err := s.CompleteTimer(ctx, store.Timer{Kind: "call_watchdog", SubjectID: "c1"})
			return err
		},
		func() error { return s.SetTimer(ctx, "", "x", t0) },
	} {
		mustIs(t, bad(), store.ErrInvalid)
	}
}

// testTimerLeaseExpiry: a claimant that never completes loses the timer when
// its lease ends; another instance re-claims it with a new token, and the
// first claimant's late completion is refused.
func testTimerLeaseExpiry(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	must(t, s.SetTimer(ctx, "device_grace", "d1", t0))
	first := claim(t, s, t0, "dead", 1)
	mustEqual(t, "first claim", len(first), 1)
	end := first[0].ClaimUntil
	mustEqual(t, "held until lease end", len(claim(t, s, end-1, "live", 1)), 0)

	again := claim(t, s, end, "live", 1)
	mustEqual(t, "re-claim", len(again), 1)
	c := again[0]
	if c.ClaimedBy != "live" || c.Attempts != 2 || c.Token == first[0].Token || c.ClaimUntil != end+lease {
		t.Fatalf("re-claimed timer %+v (first %+v)", c, first[0])
	}
	ok, err := s.CompleteTimer(ctx, first[0])
	must(t, err)
	mustEqual(t, "stale completion", ok, false)
	ok, err = s.CompleteTimer(ctx, c)
	must(t, err)
	mustEqual(t, "completion", ok, true)
	ts, err := s.ListTimers(ctx)
	must(t, err)
	mustEqual(t, "timers left", len(ts), 0)
}

// testTimerConcurrentClaim: claimants on two handles drain a set of due
// timers; every timer is claimed exactly once.
func testTimerConcurrentClaim(t *testing.T, b Backend) {
	s1, dir := open(t, b)
	s2 := reopen(t, b, dir)
	ctx := t.Context()
	const timers, claimants = 40, 8
	must(t, s1.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		for i := range timers {
			if err := tx.SetTimer(ctx, "call_watchdog", fmt.Sprintf("c%02d", i), t0+int64(i%5)); err != nil {
				return err
			}
		}
		return nil
	}))
	var mu sync.Mutex
	seen := map[string]string{}
	var wg sync.WaitGroup
	for i := range claimants {
		wg.Go(func() {
			s := s1
			if i%2 == 1 {
				s = s2
			}
			me := fmt.Sprint("i", i)
			// Every non-empty batch takes at least one of the timers, so a
			// correct store drains them within `timers` batches; a store that
			// hands out held timers would otherwise loop forever.
			for range timers + 1 {
				ts, err := s.ClaimDueTimers(ctx, t0+10, me, lease, 3)
				if err != nil {
					t.Error(err)
					return
				}
				if len(ts) == 0 {
					return
				}
				mu.Lock()
				for _, tm := range ts {
					if prev, dup := seen[tm.SubjectID]; dup {
						t.Errorf("%s claimed by %s and %s", tm.SubjectID, prev, me)
					}
					seen[tm.SubjectID] = me
				}
				dup := t.Failed()
				mu.Unlock()
				if dup {
					return
				}
			}
		})
	}
	wg.Wait()
	mustEqual(t, "claimed", len(seen), timers)
}

func testTx(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	must(t, s.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		if err := tx.CreateUser(ctx, store.User{ID: "u1", Name: "a", CreatedAt: t0}); err != nil {
			return err
		}
		// Reads inside the transaction see its own writes.
		_, err := tx.GetUser(ctx, "u1")
		return err
	}))
	_, err := s.GetUser(ctx, "u1")
	must(t, err)

	errAbort := errors.New("abort")
	mustIs(t, s.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		must(t, tx.CreateUser(ctx, store.User{ID: "u2", Name: "b", CreatedAt: t0}))
		return errAbort
	}), errAbort)
	_, err = s.GetUser(ctx, "u2")
	mustIs(t, err, store.ErrNotFound)

	// A store error inside fn comes back as is when fn returns it.
	mustIs(t, s.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		return tx.CreateUser(ctx, store.User{ID: "u1", Name: "dup", CreatedAt: t0})
	}), store.ErrExists)

	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("panic in fn was swallowed")
			}
		}()
		_ = s.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
			must(t, tx.CreateUser(ctx, store.User{ID: "u3", Name: "c", CreatedAt: t0}))
			panic("boom")
		})
	}()
	// A transaction left open by the panic would hold the single writer:
	// the next calls would wait for it, so give them a short deadline.
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_, err = s.GetUser(ctx, "u3")
	mustIs(t, err, store.ErrNotFound)
	// The store is still usable after the panic.
	must(t, s.CreateUser(ctx, store.User{ID: "u4", Name: "d", CreatedAt: t0}))
}

// testTxReentrant: inside fn, calling the store handle itself (an Ops
// method or a nested Tx) with fn's ctx, or a ctx derived from it, fails at
// once with ErrInvalid instead of waiting for the transaction fn holds. fn's
// tx keeps working and commits. The Faulty wrapper behaves the same, and so
// does the store it wraps when called inside the wrapper's Tx. After Tx
// returns, the ctx fn was given is ordinary again.
func testTxReentrant(t *testing.T, b Backend) {
	inner, _ := open(t, b)
	for i, s := range []store.Store{inner, NewFaulty(inner)} {
		// A store that waits instead of failing ends with DeadlineExceeded,
		// which the ErrInvalid checks below reject, within this deadline.
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		id := fmt.Sprint("u", i)
		var leaked context.Context
		start := time.Now()
		err := s.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
			leaked = ctx
			derived, cancel := context.WithCancel(ctx)
			defer cancel()
			for _, c := range []context.Context{ctx, derived} {
				for _, h := range []store.Store{s, inner} {
					_, err := h.GetUser(c, id)
					mustIs(t, err, store.ErrInvalid)
					mustIs(t, h.CreateUser(c, store.User{ID: id + "-outer", Name: id + "-outer", CreatedAt: t0}), store.ErrInvalid)
					_, err = h.TransitionCall(c, "c1", store.CallExpect{States: []store.CallState{store.CallAccepted}}, store.CallLost, store.CallPatch{})
					mustIs(t, err, store.ErrInvalid)
					_, err = h.ClaimDueTimers(c, t0, "i1", lease, 1)
					mustIs(t, err, store.ErrInvalid)
					mustIs(t, h.Tx(c, func(context.Context, store.Ops) error {
						t.Error("nested Tx ran fn")
						return nil
					}), store.ErrInvalid)
				}
			}
			return tx.CreateUser(ctx, store.User{ID: id, Name: id, CreatedAt: t0})
		})
		must(t, err)
		if waited := time.Since(start); waited > 2*time.Second {
			t.Fatalf("re-entrant calls took %v: they waited instead of failing", waited)
		}
		_, err = s.GetUser(leaked, id) // committed, and the ctx works again
		must(t, err)
		_, err = s.GetUser(t.Context(), id+"-outer")
		mustIs(t, err, store.ErrNotFound)
		cancel()
	}
}

// testTxTimerAtomicity: a timer set or cancelled with a domain write commits
// or rolls back with it.
func testTxTimerAtomicity(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	errAbort := errors.New("abort")

	mustIs(t, s.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		must(t, tx.CreateCall(ctx, newCall("c1", f)))
		must(t, tx.SetTimer(ctx, "call_watchdog", "c1", t0+100))
		return errAbort
	}), errAbort)
	_, err := s.GetCall(ctx, "c1")
	mustIs(t, err, store.ErrNotFound)
	ts, err := s.ListTimers(ctx)
	must(t, err)
	mustEqual(t, "timers after rollback", len(ts), 0)

	must(t, s.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		createIn(t, tx, newCall("c1", f), store.CallDispatched)
		return tx.SetTimer(ctx, "call_watchdog", "c1", t0+100)
	}))
	finish := func(ctx context.Context, tx store.Ops) error {
		ok, err := tx.TransitionCall(ctx, "c1", store.CallExpect{States: []store.CallState{store.CallDispatched}}, store.CallCompleted, store.CallPatch{FinishedAt: t0 + 1})
		if err != nil || !ok {
			return fmt.Errorf("transition: %v %v", ok, err)
		}
		return tx.CancelTimer(ctx, "call_watchdog", "c1")
	}
	mustIs(t, s.Tx(ctx, func(ctx context.Context, tx store.Ops) error { must(t, finish(ctx, tx)); return errAbort }), errAbort)
	mustEqual(t, "state after rollback", getCall(t, s, "c1").State, store.CallDispatched)
	ts, err = s.ListTimers(ctx)
	must(t, err)
	mustEqual(t, "watchdog kept", subjects(ts), []string{"call_watchdog/c1"})

	must(t, s.Tx(ctx, finish))
	mustEqual(t, "state after commit", getCall(t, s, "c1").State, store.CallCompleted)
	ts, err = s.ListTimers(ctx)
	must(t, err)
	mustEqual(t, "watchdog gone", len(ts), 0)
}
