package storetest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/EricWvi/circus/internal/store"
)

func expect(states ...store.CallState) store.CallExpect { return store.CallExpect{States: states} }

func transition(t testing.TB, ops store.Ops, id string, e store.CallExpect, to store.CallState, p store.CallPatch) bool {
	t.Helper()
	ok, err := ops.TransitionCall(t.Context(), id, e, to, p)
	must(t, err)
	return ok
}

// createLostAfterDispatch creates call c, dispatches it to boot and loses
// it, as when the device restarts under a running call (F4).
func createLostAfterDispatch(t testing.TB, ops store.Ops, c store.Call, boot string) {
	t.Helper()
	must(t, ops.CreateCall(t.Context(), c))
	if !transition(t, ops, c.CallID, expect(store.CallAccepted), store.CallDispatched, store.CallPatch{DispatchedBootID: boot, DispatchedAt: t0 + 1}) ||
		!transition(t, ops, c.CallID, expect(store.CallDispatched), store.CallLost, store.CallPatch{FinishedAt: t0 + 2, OutcomeReason: "daemon restarted"}) {
		t.Fatalf("drive %s to lost after dispatch", c.CallID)
	}
}

// testCallResubmit: a call lost before dispatch (control plane restart, D43
// item 5) is resubmitted as a fresh attempt, lost → accepted: it takes the
// resubmission time, keeps nothing of the lost attempt, and then follows the
// normal edges. A call lost after dispatch never leaves lost.
func testCallResubmit(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	accepted := expect(store.CallAccepted)
	resubmit := store.CallExpect{States: []store.CallState{store.CallLost}, Undispatched: true}

	// The lost attempts fill every per-attempt column the store lets a
	// router write on an undispatched call, so a leftover would show.
	loseBeforeDispatch := func(id string, submitted int64) store.Call {
		t.Helper()
		c := newCall(id, f)
		c.SubmittedAt = submitted
		must(t, s.CreateCall(ctx, c))
		ok, err := s.PatchCall(ctx, id, accepted, store.CallPatch{DispatchedAt: t0 + 2, AckedAt: t0 + 2, CancelRequestedAt: t0 + 2, JobID: "j-old"})
		must(t, err)
		if !ok || !transition(t, s, id, accepted, store.CallLost, store.CallPatch{
			FinishedAt: t0 + 3, OutcomeReason: "control plane restarted before dispatch", FaultCode: "f", IsError: true, ExitCode: ptr(int32(1)),
		}) {
			t.Fatalf("lose %s before dispatch", id)
		}
		return c
	}
	ids := []string{"c-offline", "c-cancel", "c-run"}
	for i, id := range ids {
		loseBeforeDispatch(id, t0+1+int64(i))
	}
	fresh := newCall("c-fresh", f)
	fresh.SubmittedAt = t0 + 5
	must(t, s.CreateCall(ctx, fresh))

	// The resubmission finds the row (same id, same hash) and starts a new
	// attempt; nothing of the lost one survives.
	for i, id := range ids {
		mustIs(t, s.CreateCall(ctx, newCall(id, f)), store.ErrExists)
		mustEqual(t, label("resubmit ", id), transition(t, s, id, resubmit, store.CallAccepted, store.CallPatch{SubmittedAt: t0 + 10 + int64(i)}), true)
		want := newCall(id, f)
		want.SubmittedAt = t0 + 10 + int64(i)
		mustEqual(t, label("resubmitted ", id), getCall(t, s, id), want)
	}
	// Listed in submission order with the new times: after the call
	// submitted between the two attempts.
	cs, err := s.ListCalls(ctx, store.CallFilter{Limit: store.MaxPageSize, States: []store.CallState{store.CallAccepted}})
	must(t, err)
	var order []string
	for _, c := range cs {
		order = append(order, c.CallID)
	}
	mustEqual(t, "submission order", order, append([]string{"c-fresh"}, ids...))

	// From accepted, the normal edges, with their own outcomes.
	mustEqual(t, "offline", transition(t, s, "c-offline", accepted, store.CallOffline, store.CallPatch{FinishedAt: t0 + 20, OutcomeReason: "device offline"}), true)
	mustEqual(t, "cancel", transition(t, s, "c-cancel", accepted, store.CallCancelled, store.CallPatch{FinishedAt: t0 + 21, OutcomeReason: "cancelled"}), true)
	mustEqual(t, "dispatch", transition(t, s, "c-run", accepted, store.CallDispatched, store.CallPatch{DispatchedBootID: "b2", DispatchedAt: t0 + 22}), true)
	mustEqual(t, "ack", transition(t, s, "c-run", expect(store.CallDispatched), store.CallRunning, store.CallPatch{AckedAt: t0 + 23, JobID: "j-new"}), true)
	mustEqual(t, "complete", transition(t, s, "c-run", expect(store.CallRunning), store.CallCompleted, store.CallPatch{FinishedAt: t0 + 24, ExitCode: ptr(int32(0))}), true)
	for id, mut := range map[string]func(*store.Call){
		"c-offline": func(c *store.Call) {
			c.State, c.SubmittedAt, c.FinishedAt, c.OutcomeReason = store.CallOffline, t0+10, t0+20, "device offline"
		},
		"c-cancel": func(c *store.Call) {
			c.State, c.SubmittedAt, c.FinishedAt, c.OutcomeReason = store.CallCancelled, t0+11, t0+21, "cancelled"
		},
		"c-run": func(c *store.Call) {
			c.State, c.SubmittedAt, c.DispatchedBootID, c.DispatchedAt, c.AckedAt = store.CallCompleted, t0+12, "b2", t0+22, t0+23
			c.JobID, c.FinishedAt, c.ExitCode = "j-new", t0+24, ptr(int32(0))
		},
	} {
		want := newCall(id, f)
		mut(&want)
		mustEqual(t, label("outcome of ", id), getCall(t, s, id), want)
	}

	// A resubmission with another request hash: the id is taken and the
	// stored hash differs, so the router rejects it (CALL_MISMATCH).
	other := newCall("c-run", f)
	other.RequestHash = hash("other request")
	mustIs(t, s.CreateCall(ctx, other), store.ErrExists)
	mustEqual(t, "hash after mismatched resubmit", getCall(t, s, "c-run").RequestHash, hash("c-run"))

	// Lost after dispatch to b1: never leaves lost, whatever the caller
	// expects.
	createLostAfterDispatch(t, s, newCall("c-gone", f), "b1")
	before := getCall(t, s, "c-gone")
	for _, e := range []store.CallExpect{expect(store.CallLost), resubmit} {
		mustEqual(t, label("resubmit dispatched call with ", e), transition(t, s, "c-gone", e, store.CallAccepted, store.CallPatch{SubmittedAt: t0 + 30}), false)
		for _, to := range []store.CallState{store.CallDispatched, store.CallRunning, store.CallCompleted} {
			_, err := s.TransitionCall(ctx, "c-gone", e, to, dispatchPatch(to, "b2"))
			mustIs(t, err, store.ErrInvalidTransition)
		}
	}
	mustEqual(t, "lost-after-dispatch call", getCall(t, s, "c-gone"), before)

	// The resubmission takes the submission time and nothing else; only it
	// writes the submission time.
	loseBeforeDispatch("c-bad", t0+1)
	for name, tc := range map[string]struct {
		e     store.CallExpect
		to    store.CallState
		patch store.CallPatch
	}{
		"no time":            {resubmit, store.CallAccepted, store.CallPatch{}},
		"extra column":       {resubmit, store.CallAccepted, store.CallPatch{SubmittedAt: t0 + 40, FinishedAt: t0 + 40}},
		"expects a boot":     {store.CallExpect{States: []store.CallState{store.CallLost}, BootID: "b1"}, store.CallAccepted, store.CallPatch{SubmittedAt: t0 + 40}},
		"time on other edge": {store.CallExpect{States: []store.CallState{store.CallLost}}, store.CallAccepted, store.CallPatch{SubmittedAt: t0 + 40, DispatchedBootID: "b1"}},
	} {
		_, err := s.TransitionCall(ctx, "c-bad", tc.e, tc.to, tc.patch)
		mustIs(t, err, store.ErrInvalid)
		_ = name
	}
	must(t, s.CreateCall(ctx, newCall("c-new", f)))
	_, err = s.TransitionCall(ctx, "c-new", accepted, store.CallCancelled, store.CallPatch{SubmittedAt: t0 + 40})
	mustIs(t, err, store.ErrInvalid)
	_, err = s.PatchCall(ctx, "c-new", accepted, store.CallPatch{SubmittedAt: t0 + 40})
	mustIs(t, err, store.ErrInvalid)
	mustEqual(t, "c-new submitted at", getCall(t, s, "c-new").SubmittedAt, t0)

	// Undispatched as a precondition of any write.
	undispatched := store.CallExpect{States: []store.CallState{store.CallAccepted, store.CallLost, store.CallRunning}, Undispatched: true}
	ok, err := s.PatchCall(ctx, "c-gone", undispatched, store.CallPatch{CancelRequestedAt: t0 + 5})
	must(t, err)
	mustEqual(t, "patch of dispatched call expecting undispatched", ok, false)
	ok, err = s.PatchCall(ctx, "c-new", undispatched, store.CallPatch{CancelRequestedAt: t0 + 5})
	must(t, err)
	mustEqual(t, "patch of undispatched call", ok, true)
	mustEqual(t, "cancel undispatched", transition(t, s, "c-new", store.CallExpect{States: []store.CallState{store.CallAccepted}, Undispatched: true}, store.CallCancelled, store.CallPatch{}), true)
	must(t, s.CreateCall(ctx, newCall("c4", f)))
	_, err = s.TransitionCall(ctx, "c4", store.CallExpect{States: []store.CallState{store.CallAccepted}, Undispatched: true, BootID: "b1"}, store.CallLost, store.CallPatch{})
	mustIs(t, err, store.ErrInvalid)
	_, err = s.PatchCall(ctx, "c-new", store.CallExpect{States: []store.CallState{store.CallCancelled}, Undispatched: true, BootID: "b1"}, store.CallPatch{JobID: "j"})
	mustIs(t, err, store.ErrInvalid)
}

// testCallWriteOnce: write-once columns keep their first value through
// running → unknown → running; outcome columns take the latest.
func testCallWriteOnce(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	must(t, s.CreateCall(ctx, newCall("c1", f)))
	patch := func(e store.CallExpect, p store.CallPatch) {
		t.Helper()
		ok, err := s.PatchCall(ctx, "c1", e, p)
		must(t, err)
		if !ok {
			t.Fatalf("patch %+v did not apply", p)
		}
	}
	step := func(from, to store.CallState, p store.CallPatch) {
		t.Helper()
		if !transition(t, s, "c1", expect(from), to, p) {
			t.Fatalf("%s → %s did not apply", from, to)
		}
	}
	step(store.CallAccepted, store.CallDispatched, store.CallPatch{DispatchedBootID: "b1", DispatchedAt: t0 + 1})
	step(store.CallDispatched, store.CallRunning, store.CallPatch{AckedAt: t0 + 2, JobID: "j1"})
	patch(expect(store.CallRunning), store.CallPatch{CancelRequestedAt: t0 + 3, FaultCode: "first"})
	step(store.CallRunning, store.CallUnknown, store.CallPatch{OutcomeReason: "grace expired"})
	step(store.CallUnknown, store.CallRunning, store.CallPatch{AckedAt: t0 + 9, DispatchedAt: t0 + 9, JobID: "j2"})
	patch(expect(store.CallRunning), store.CallPatch{CancelRequestedAt: t0 + 8, FaultCode: "second"})
	step(store.CallRunning, store.CallCompleted, store.CallPatch{FinishedAt: t0 + 10, ExitCode: ptr(int32(7)), OutcomeReason: "done"})
	want := newCall("c1", f)
	want.State, want.DispatchedBootID, want.DispatchedAt, want.AckedAt, want.JobID = store.CallCompleted, "b1", t0+1, t0+2, "j1"
	want.CancelRequestedAt, want.FaultCode, want.OutcomeReason = t0+3, "second", "done"
	want.FinishedAt, want.ExitCode = t0+10, ptr(int32(7))
	mustEqual(t, "call", getCall(t, s, "c1"), want)
}

// testLoseCalls: when the device connects with a new boot, its in-flight
// calls of every other boot become lost; nothing else changes.
func testLoseCalls(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	other := f.device
	other.ID, other.Name = "d2", "desk"
	must(t, s.CreateIdentity(ctx, other))
	drive := func(id, dev, boot string, path ...store.CallState) {
		t.Helper()
		c := newCall(id, f)
		c.DeviceID = dev
		must(t, s.CreateCall(ctx, c))
		cur := store.CallAccepted
		for _, next := range path {
			if !transition(t, s, id, expect(cur), next, dispatchPatch(next, boot)) {
				t.Fatalf("drive %s", id)
			}
			cur = next
		}
	}
	D, R, U, C := store.CallDispatched, store.CallRunning, store.CallUnknown, store.CallCompleted
	drive("dispatched-b1", "d1", "b1", D)
	drive("running-b0", "d1", "b0", D, R) // a boot never recorded as current
	drive("unknown-b1", "d1", "b1", D, U)
	drive("completed-b1", "d1", "b1", D, R, C)
	drive("running-b2", "d1", "b2", D, R)
	drive("accepted", "d1", "")
	drive("other-device", "d2", "b1", D, R)
	// The grace period expired for unknown-b1 and wrote its outcome; the
	// loss replaces it.
	ok, err := s.PatchCall(ctx, "unknown-b1", expect(U), store.CallPatch{FinishedAt: t0 + 7, OutcomeReason: "grace expired"})
	must(t, err)
	mustEqual(t, "grace outcome", ok, true)
	before := map[string]store.Call{}
	cs, err := s.ListCalls(ctx, store.CallFilter{Limit: store.MaxPageSize})
	must(t, err)
	for _, c := range cs {
		before[c.CallID] = c
	}

	lost, err := s.LoseCalls(ctx, "d1", "b2", t0+50, "daemon restarted", store.MaxPageSize)
	must(t, err)
	mustEqual(t, "lost calls", lost, []store.LostCall{
		{CallID: "dispatched-b1", From: D}, {CallID: "running-b0", From: R}, {CallID: "unknown-b1", From: U},
	})
	for id, c := range before {
		if id == "dispatched-b1" || id == "running-b0" || id == "unknown-b1" {
			c.State, c.FinishedAt, c.OutcomeReason = store.CallLost, t0+50, "daemon restarted"
		}
		mustEqual(t, id, getCall(t, s, id), c)
	}
	lost, err = s.LoseCalls(ctx, "d1", "b2", t0+60, "daemon restarted", store.MaxPageSize)
	must(t, err)
	mustEqual(t, "lost again", len(lost), 0)
	// What LoseCalls lost was dispatched: it is never resubmitted.
	mustEqual(t, "resubmit", transition(t, s, "running-b0", expect(store.CallLost), store.CallAccepted, store.CallPatch{SubmittedAt: t0 + 70}), false)

	for _, bad := range []func() error{
		func() error { _, err := s.LoseCalls(ctx, "", "b2", t0, "r", store.MaxPageSize); return err },
		func() error { _, err := s.LoseCalls(ctx, "d1", "", t0, "r", store.MaxPageSize); return err },
		func() error { _, err := s.LoseCalls(ctx, "d1", "b2", 0, "r", store.MaxPageSize); return err },
		func() error {
			// No call matches, so only the argument check can refuse it.
			_, err := s.LoseCalls(ctx, "d-none", "b3", t0, string(make([]byte, store.MaxCodeBytes+1)), store.MaxPageSize)
			return err
		},
	} {
		mustIs(t, bad(), store.ErrInvalid)
	}
	mustEqual(t, "running-b2 untouched", getCall(t, s, "running-b2").State, store.CallRunning)
}

// testJobTerminalIsFinal: once a job has ended (exited, killed, or lost by
// LoseJobs), no later event moves it, whatever its seq: a buffered RUNNING
// event of a dead boot cannot resurrect a lost job.
func testJobTerminalIsFinal(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	seed(t, s)
	put := func(j store.Job) bool {
		t.Helper()
		ok, err := s.PutJob(ctx, j)
		must(t, err)
		return ok
	}
	job := func(id string, st store.JobState, seq int64) store.Job {
		return store.Job{DeviceID: "d1", BootID: "b1", JobID: id, UserID: "u1", SessionID: "s1", State: st, LastSeq: seq, UpdatedAt: t0 + seq}
	}
	put(job("lost", store.JobRunning, 5))
	n, err := s.LoseJobs(ctx, "d1", "b2", t0+10)
	must(t, err)
	mustEqual(t, "lost", n, 1)
	put(job("exited", store.JobExited, 5))
	put(job("killed", store.JobKilled, 5))
	before, err := s.ListJobs(ctx, store.JobFilter{Limit: store.MaxPageSize})
	must(t, err)
	for _, id := range []string{"lost", "exited", "killed"} {
		for _, st := range []store.JobState{store.JobRunning, store.JobExited, store.JobKilled, store.JobLost} {
			late := job(id, st, 6)
			late.ExitCode, late.EndedAt = ptr(int32(9)), t0+99
			mustEqual(t, label("late ", st, " event for ", id), put(late), false)
		}
	}
	after, err := s.ListJobs(ctx, store.JobFilter{Limit: store.MaxPageSize})
	must(t, err)
	mustEqual(t, "jobs after late events", after, before)
}

// testTxSerializable: transactions on two handles each read a value and
// write it back incremented. Serializable transactions lose no update.
func testTxSerializable(t *testing.T, b Backend) {
	s1, dir := open(t, b)
	s2 := reopen(t, b, dir)
	ctx := t.Context()
	must(t, s1.SetTimer(ctx, "counter", "x", 0))
	const workers, rounds = 4, 8
	var wg sync.WaitGroup
	for i := range workers {
		wg.Go(func() {
			s := s1
			if i%2 == 1 {
				s = s2
			}
			for range rounds {
				err := retryBusy(func() error {
					return s.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
						ts, err := tx.ListTimers(ctx, store.TimerFilter{Limit: store.MaxPageSize})
						if err != nil {
							return err
						}
						return tx.SetTimer(ctx, "counter", "x", ts[0].DueAt+1)
					})
				})
				if err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	wg.Wait()
	ts, err := s1.ListTimers(ctx, store.TimerFilter{Limit: store.MaxPageSize})
	must(t, err)
	mustEqual(t, "counter", ts[0].DueAt, int64(workers*rounds))
}

// retryBusy retries fn while it fails with ErrBusy, as callers may.
func retryBusy(fn func() error) error {
	for {
		if err := fn(); !errors.Is(err, store.ErrBusy) {
			return err
		}
	}
}

// testSetConnectedConcurrent: concurrent connections from two handles see
// one chain of boots. Each previous boot is returned exactly once, and the
// boot left current is the one no connection saw as previous.
func testSetConnectedConcurrent(t *testing.T, b Backend) {
	s1, dir := open(t, b)
	s2 := reopen(t, b, dir)
	ctx := t.Context()
	seed(t, s1)
	const n = 16
	prevs := make([]string, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			s := s1
			if i%2 == 1 {
				s = s2
			}
			var err error
			prevs[i], err = s.SetConnected(ctx, store.KindDevice, "d1", fmt.Sprint("boot-", i), t0+int64(i)+1)
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	seen := map[string]bool{}
	for _, p := range prevs {
		if seen[p] {
			t.Fatalf("previous boot %q returned twice: %v", p, prevs)
		}
		seen[p] = true
	}
	if !seen[""] {
		t.Fatalf("no connection was the first: %v", prevs)
	}
	d, err := s1.GetIdentity(ctx, store.KindDevice, "d1")
	must(t, err)
	if seen[d.CurrentBootID] {
		t.Fatalf("current boot %q was also returned as previous: %v", d.CurrentBootID, prevs)
	}
}
