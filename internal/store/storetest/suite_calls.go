package storetest

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/EricWvi/circus/internal/store"
)

func testCallCreate(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)

	c := newCall("c1", f)
	must(t, s.CreateCall(ctx, c))
	mustEqual(t, "GetCall", getCall(t, s, "c1"), c)
	mustIs(t, s.CreateCall(ctx, c), store.ErrExists)
	_, err := s.GetCall(ctx, "nope")
	mustIs(t, err, store.ErrNotFound)

	// A submission may also be recorded directly as rejected or offline.
	for _, st := range []store.CallState{store.CallRejected, store.CallOffline} {
		r := newCall("c-"+string(st), f)
		r.State, r.FinishedAt, r.OutcomeReason, r.AgentID = st, t0+1, "why", ""
		must(t, s.CreateCall(ctx, r))
		mustEqual(t, label("GetCall ", st), getCall(t, s, r.CallID), r)
	}

	for name, mut := range map[string]func(*store.Call){
		"dispatched state": func(c *store.Call) { c.State = store.CallDispatched; c.DispatchedBootID = "b1" },
		"boot on accepted": func(c *store.Call) { c.DispatchedBootID = "b1" },
		"no hash":          func(c *store.Call) { c.RequestHash = nil },
		"no session":       func(c *store.Call) { c.SessionID = "" },
		"unknown state":    func(c *store.Call) { c.State = "weird" },
	} {
		bad := newCall("bad-"+name, f)
		mut(&bad)
		mustIs(t, s.CreateCall(ctx, bad), store.ErrInvalid)
	}
	ghost := newCall("ghost", f)
	ghost.DeviceID = "no-such-device"
	mustIs(t, s.CreateCall(ctx, ghost), store.ErrNotFound)

	// A call names a device of its own user only.
	must(t, s.CreateUser(ctx, store.User{ID: "u2", Name: "other", CreatedAt: t0}))
	foreign := newCall("foreign", f)
	foreign.UserID = "u2"
	mustIs(t, s.CreateCall(ctx, foreign), store.ErrNotFound)

	// Outcome reason and fault code are short codes, never tool output.
	long := newCall("long", f)
	long.State, long.OutcomeReason = store.CallRejected, strings.Repeat("x", store.MaxCodeBytes)
	must(t, s.CreateCall(ctx, long))
	for _, mut := range []func(*store.Call){
		func(c *store.Call) { c.OutcomeReason = strings.Repeat("x", store.MaxCodeBytes+1) },
		func(c *store.Call) { c.FaultCode = strings.Repeat("x", store.MaxCodeBytes+1) },
	} {
		bad := newCall("too-long", f)
		bad.State = store.CallRejected
		mut(&bad)
		mustIs(t, s.CreateCall(ctx, bad), store.ErrInvalid)
	}
}

// testCallTransitionTable checks the state machine against wantCallEdges,
// both ways, then tries every from → to pair of states on a call in state
// from: wanted edges apply, every other pair is ErrInvalidTransition and
// changes nothing.
func testCallTransitionTable(t *testing.T, b Backend) {
	for _, from := range allStates {
		for _, to := range allStates {
			if got, want := store.CanTransition(from, to), slices.Contains(wantCallEdges[from], to); got != want {
				t.Errorf("CanTransition(%s, %s) = %v, want %v", from, to, got, want)
			}
		}
		mustEqual(t, label(from, ".Terminal()"), from.Terminal(), slices.Contains(wantCallTerminal, from))
	}
	if t.Failed() {
		t.FailNow()
	}
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	for _, from := range allStates {
		for _, to := range allStates {
			id := fmt.Sprintf("%s-%s", from, to)
			must(t, s.Tx(ctx, func(ctx context.Context, tx store.Ops) error { createIn(t, tx, newCall(id, f), from); return nil }))
			before := getCall(t, s, id)
			// Every edge writes a time: FinishedAt, or SubmittedAt for the
			// resubmission, which takes nothing else.
			patch := dispatchPatch(to, "b1")
			patch.FinishedAt = t0 + 9
			if to == store.CallAccepted {
				patch = store.CallPatch{SubmittedAt: t0 + 9}
			}
			ok, err := s.TransitionCall(ctx, id, store.CallExpect{States: []store.CallState{from}}, to, patch)
			after := getCall(t, s, id)
			if slices.Contains(wantCallEdges[from], to) {
				must(t, err)
				at := after.FinishedAt
				if to == store.CallAccepted {
					at = after.SubmittedAt
				}
				if !ok || after.State != to || at != t0+9 {
					t.Errorf("%s → %s: ok=%v state=%s time=%d", from, to, ok, after.State, at)
				}
				continue
			}
			mustIs(t, err, store.ErrInvalidTransition)
			mustEqual(t, label(from, " → ", to, " left the call"), after, before)
		}
	}
}

func testCallTransitionPreconditions(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	createIn(t, s, newCall("c1", f), store.CallRunning)
	before := getCall(t, s, "c1")

	// State not in from: false, nothing changes.
	ok, err := s.TransitionCall(ctx, "c1", store.CallExpect{States: []store.CallState{store.CallDispatched}}, store.CallCompleted, store.CallPatch{FinishedAt: t0 + 1})
	must(t, err)
	mustEqual(t, "applied from wrong state", ok, false)
	mustEqual(t, "call after failed precondition", getCall(t, s, "c1"), before)

	// A set of source states: any member matches.
	ok, err = s.TransitionCall(ctx, "c1", store.CallExpect{States: []store.CallState{store.CallDispatched, store.CallRunning}}, store.CallUnknown, store.CallPatch{})
	must(t, err)
	mustEqual(t, "applied from state set", ok, true)

	// Expect.BootID: another boot fails the precondition and changes
	// nothing; the recorded boot passes.
	unknown := store.CallExpect{States: []store.CallState{store.CallUnknown}}
	before = getCall(t, s, "c1")
	ok, err = s.TransitionCall(ctx, "c1", store.CallExpect{States: unknown.States, BootID: "b2"}, store.CallRunning, store.CallPatch{AckedAt: t0 + 2})
	must(t, err)
	mustEqual(t, "applied expecting another boot", ok, false)
	mustEqual(t, "call after boot mismatch", getCall(t, s, "c1"), before)
	ok, err = s.TransitionCall(ctx, "c1", store.CallExpect{States: unknown.States, BootID: "b1"}, store.CallRunning, store.CallPatch{AckedAt: t0 + 2})
	must(t, err)
	mustEqual(t, "applied expecting the recorded boot", ok, true)
	mustEqual(t, "boot unchanged", getCall(t, s, "c1").DispatchedBootID, "b1")

	// The patch's DispatchedBootID is a write, legal only on the transition
	// to dispatched: anywhere else it is refused, even naming the recorded
	// boot, and the call is untouched.
	before = getCall(t, s, "c1")
	for _, boot := range []string{"b1", "b2"} {
		_, err = s.TransitionCall(ctx, "c1", store.CallExpect{States: []store.CallState{store.CallRunning}}, store.CallCompleted, store.CallPatch{DispatchedBootID: boot})
		mustIs(t, err, store.ErrInvalid)
	}
	mustEqual(t, "call after refused boot write", getCall(t, s, "c1"), before)

	must(t, s.CreateCall(ctx, newCall("c2", f)))
	accepted := store.CallExpect{States: []store.CallState{store.CallAccepted}}
	_, err = s.TransitionCall(ctx, "c2", accepted, store.CallDispatched, store.CallPatch{})
	mustIs(t, err, store.ErrInvalid) // dispatch must record the boot
	_, err = s.TransitionCall(ctx, "c2", store.CallExpect{States: accepted.States, BootID: "b1"}, store.CallDispatched, store.CallPatch{DispatchedBootID: "b1"})
	mustIs(t, err, store.ErrInvalid) // an accepted call has no boot to expect
	// A call never dispatched matches no expected boot.
	ok, err = s.TransitionCall(ctx, "c2", store.CallExpect{States: accepted.States, BootID: "b1"}, store.CallLost, store.CallPatch{})
	must(t, err)
	mustEqual(t, "undispatched call matched a boot", ok, false)
	_, err = s.TransitionCall(ctx, "c2", store.CallExpect{}, store.CallLost, store.CallPatch{})
	mustIs(t, err, store.ErrInvalid)
	_, err = s.TransitionCall(ctx, "c2", store.CallExpect{States: []store.CallState{store.CallAccepted, store.CallCompleted}}, store.CallLost, store.CallPatch{})
	mustIs(t, err, store.ErrInvalidTransition) // completed → lost is not an edge
	_, err = s.TransitionCall(ctx, "ghost", store.CallExpect{States: []store.CallState{store.CallAccepted}}, store.CallLost, store.CallPatch{})
	mustIs(t, err, store.ErrNotFound)
	mustEqual(t, "c2 untouched", getCall(t, s, "c2").State, store.CallAccepted)
}

func testCallPatch(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	must(t, s.CreateCall(ctx, newCall("c1", f)))

	steps := []struct {
		from  store.CallState
		to    store.CallState
		patch store.CallPatch
	}{
		{store.CallAccepted, store.CallDispatched, store.CallPatch{DispatchedBootID: "b1", DispatchedAt: t0 + 1}},
		{store.CallDispatched, store.CallRunning, store.CallPatch{AckedAt: t0 + 2}},
		{store.CallRunning, store.CallCompleted, store.CallPatch{
			FinishedAt: t0 + 4, ExitCode: ptr(int32(0)), IsError: true, FaultCode: "f", JobID: "j1", OutcomeReason: "r",
		}},
	}
	for i, st := range steps {
		if i == 2 {
			// A cancel while running records the request, keeps the state.
			ok, err := s.PatchCall(ctx, "c1", store.CallExpect{States: []store.CallState{store.CallDispatched, store.CallRunning, store.CallUnknown}}, store.CallPatch{CancelRequestedAt: t0 + 3})
			must(t, err)
			mustEqual(t, "cancel patch applied", ok, true)
			mustEqual(t, "state after cancel patch", getCall(t, s, "c1").State, store.CallRunning)
		}
		ok, err := s.TransitionCall(ctx, "c1", store.CallExpect{States: []store.CallState{st.from}}, st.to, st.patch)
		must(t, err)
		mustEqual(t, label(st.from, " → ", st.to), ok, true)
	}
	// Zero patch fields left earlier values alone; exit code 0 is a value.
	want := newCall("c1", f)
	want.State, want.DispatchedBootID, want.DispatchedAt, want.AckedAt = store.CallCompleted, "b1", t0+1, t0+2
	want.CancelRequestedAt, want.FinishedAt, want.ExitCode, want.IsError = t0+3, t0+4, ptr(int32(0)), true
	want.FaultCode, want.JobID, want.OutcomeReason = "f", "j1", "r"
	mustEqual(t, "call after patches", getCall(t, s, "c1"), want)

	ok, err := s.PatchCall(ctx, "c1", store.CallExpect{States: []store.CallState{store.CallRunning}}, store.CallPatch{JobID: "j2"})
	must(t, err)
	mustEqual(t, "patch on wrong state", ok, false)
	_, err = s.PatchCall(ctx, "c1", store.CallExpect{States: []store.CallState{store.CallCompleted}}, store.CallPatch{})
	mustIs(t, err, store.ErrInvalid)
	_, err = s.PatchCall(ctx, "c1", store.CallExpect{}, store.CallPatch{JobID: "j2"})
	mustIs(t, err, store.ErrInvalid)
	// PatchCall never writes the boot, with or without other columns.
	for _, p := range []store.CallPatch{{DispatchedBootID: "b1"}, {DispatchedBootID: "b2", JobID: "j2"}} {
		_, err = s.PatchCall(ctx, "c1", store.CallExpect{States: []store.CallState{store.CallCompleted}}, p)
		mustIs(t, err, store.ErrInvalid)
	}
	// Codes longer than MaxCodeBytes are refused by patches and transitions.
	tooLong := strings.Repeat("x", store.MaxCodeBytes+1)
	_, err = s.PatchCall(ctx, "c1", store.CallExpect{States: []store.CallState{store.CallCompleted}}, store.CallPatch{OutcomeReason: tooLong})
	mustIs(t, err, store.ErrInvalid)
	_, err = s.PatchCall(ctx, "c1", store.CallExpect{States: []store.CallState{store.CallCompleted}}, store.CallPatch{FaultCode: tooLong})
	mustIs(t, err, store.ErrInvalid)
	_, err = s.TransitionCall(ctx, "c1", store.CallExpect{States: []store.CallState{store.CallRunning}}, store.CallLost, store.CallPatch{FaultCode: tooLong})
	mustIs(t, err, store.ErrInvalid)
	mustEqual(t, "call after refused long codes", getCall(t, s, "c1"), want)
	// Expect.BootID guards a patch too.
	completed := []store.CallState{store.CallCompleted}
	ok, err = s.PatchCall(ctx, "c1", store.CallExpect{States: completed, BootID: "b2"}, store.CallPatch{OutcomeReason: "other boot"})
	must(t, err)
	mustEqual(t, "patch expecting another boot", ok, false)
	mustEqual(t, "call after boot-mismatched patch", getCall(t, s, "c1"), want)
	ok, err = s.PatchCall(ctx, "c1", store.CallExpect{States: completed, BootID: "b1"}, store.CallPatch{JobID: "j1"})
	must(t, err)
	mustEqual(t, "patch expecting the recorded boot", ok, true)
	_, err = s.PatchCall(ctx, "ghost", store.CallExpect{States: []store.CallState{store.CallRunning}}, store.CallPatch{JobID: "j2"})
	mustIs(t, err, store.ErrNotFound)
	mustEqual(t, "call unchanged", getCall(t, s, "c1"), want)
}

// testCallTransitionConcurrent races the four ways out of dispatched for
// each of several calls, from two handles: exactly one transition per call
// applies and the row ends in the winner's state.
func testCallTransitionConcurrent(t *testing.T, b Backend) {
	s1, dir := open(t, b)
	s2 := reopen(t, b, dir)
	ctx := t.Context()
	f := seed(t, s1)
	targets := []store.CallState{store.CallRunning, store.CallCompleted, store.CallUnknown, store.CallLost}
	const calls, rounds = 4, 2
	for c := range calls {
		createIn(t, s1, newCall(fmt.Sprint("c", c), f), store.CallDispatched)
	}
	type win struct {
		to store.CallState
		ok bool
	}
	results := make([][]win, calls)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for c := range calls {
		for r := range rounds {
			for i, to := range targets {
				wg.Go(func() {
					s := s1
					if (i+r)%2 == 1 {
						s = s2
					}
					ok, err := s.TransitionCall(ctx, fmt.Sprint("c", c), store.CallExpect{States: []store.CallState{store.CallDispatched}}, to, store.CallPatch{FinishedAt: t0 + 1})
					if err != nil {
						t.Errorf("c%d → %s: %v", c, to, err)
					}
					mu.Lock()
					results[c] = append(results[c], win{to, ok})
					mu.Unlock()
				})
			}
		}
	}
	wg.Wait()
	for c, rs := range results {
		var winners []store.CallState
		for _, r := range rs {
			if r.ok {
				winners = append(winners, r.to)
			}
		}
		if len(winners) != 1 {
			t.Fatalf("c%d: winners %v, want exactly one", c, winners)
		}
		mustEqual(t, label("c", c, " state"), getCall(t, s1, fmt.Sprint("c", c)).State, winners[0])
	}
}

func testCallList(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	other := f.device
	other.ID, other.Name = "d2", "desk"
	must(t, s.CreateIdentity(ctx, other))

	mk := func(id, dev string, at int64, st store.CallState, boot string) {
		c := newCall(id, f)
		c.DeviceID, c.SubmittedAt = dev, at
		must(t, s.CreateCall(ctx, c))
		cur := store.CallAccepted
		for _, next := range pathTo[st] {
			ok, err := s.TransitionCall(ctx, id, store.CallExpect{States: []store.CallState{cur}}, next, dispatchPatch(next, boot))
			must(t, err)
			if !ok {
				t.Fatalf("drive %s", id)
			}
			cur = next
		}
	}
	mk("c3", "d1", t0+3, store.CallRunning, "b1")
	mk("c1", "d1", t0+1, store.CallAccepted, "")
	mk("c2", "d1", t0+2, store.CallDispatched, "b2")
	mk("c4", "d2", t0+0, store.CallRunning, "b1")
	mk("c5", "d1", t0+5, store.CallCompleted, "b1")

	ids := func(f store.CallFilter) []string {
		t.Helper()
		f.Limit = store.MaxPageSize
		cs, err := s.ListCalls(ctx, f)
		must(t, err)
		out := []string{}
		for _, c := range cs {
			out = append(out, c.CallID)
		}
		return out
	}
	inflight := []store.CallState{store.CallDispatched, store.CallRunning, store.CallUnknown}
	mustEqual(t, "all", ids(store.CallFilter{Limit: store.MaxPageSize}), []string{"c4", "c1", "c2", "c3", "c5"})
	mustEqual(t, "device", ids(store.CallFilter{Limit: store.MaxPageSize, DeviceID: "d1"}), []string{"c1", "c2", "c3", "c5"})
	mustEqual(t, "device in flight", ids(store.CallFilter{Limit: store.MaxPageSize, DeviceID: "d1", States: inflight}), []string{"c2", "c3"})
	mustEqual(t, "boot in flight", ids(store.CallFilter{Limit: store.MaxPageSize, DispatchedBootID: "b1", States: inflight}), []string{"c4", "c3"})
	mustEqual(t, "device boot", ids(store.CallFilter{Limit: store.MaxPageSize, DeviceID: "d1", DispatchedBootID: "b1"}), []string{"c3", "c5"})
	mustEqual(t, "accepted", ids(store.CallFilter{Limit: store.MaxPageSize, States: []store.CallState{store.CallAccepted}}), []string{"c1"})
	mustEqual(t, "no match", ids(store.CallFilter{Limit: store.MaxPageSize, DeviceID: "d9"}), []string{})
	_, err := s.ListCalls(ctx, store.CallFilter{Limit: store.MaxPageSize, States: []store.CallState{"weird"}})
	mustIs(t, err, store.ErrInvalid)
}

func testJobs(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	seed(t, s)
	put := func(j store.Job) bool {
		t.Helper()
		ok, err := s.PutJob(ctx, j)
		must(t, err)
		return ok
	}
	list := func(f store.JobFilter) []store.Job {
		t.Helper()
		f.Limit = store.MaxPageSize
		js, err := s.ListJobs(ctx, f)
		must(t, err)
		return js
	}
	// An event arrives before anything else knows about the job.
	j := store.Job{DeviceID: "d1", BootID: "b1", JobID: "j1", UserID: "u1", State: store.JobRunning, LastSeq: 5, StartedAt: t0, UpdatedAt: t0}
	mustEqual(t, "insert", put(j), true)
	mustEqual(t, "repeat", put(j), false)
	older := j
	older.LastSeq, older.State = 4, store.JobExited
	mustEqual(t, "stale", put(older), false)
	mustEqual(t, "after stale", list(store.JobFilter{Limit: store.MaxPageSize}), []store.Job{j})

	// A newer event fills in the session and call and does not lose them
	// when the next one omits them.
	newer := j
	newer.LastSeq, newer.SessionID, newer.CallID, newer.UpdatedAt = 6, "s1", "c1", t0+1
	mustEqual(t, "newer", put(newer), true)
	done := store.Job{DeviceID: "d1", BootID: "b1", JobID: "j1", UserID: "u1", State: store.JobExited, ExitCode: ptr(int32(3)), LastSeq: 7, EndedAt: t0 + 2, UpdatedAt: t0 + 2}
	mustEqual(t, "exit", put(done), true)
	want := done
	want.SessionID, want.CallID, want.StartedAt = "s1", "c1", t0
	mustEqual(t, "after exit", list(store.JobFilter{Limit: store.MaxPageSize}), []store.Job{want})

	// Job ids are unique per boot only.
	j2 := store.Job{DeviceID: "d1", BootID: "b2", JobID: "j1", UserID: "u1", SessionID: "s2", State: store.JobRunning, LastSeq: 1}
	j3 := store.Job{DeviceID: "d1", BootID: "b1", JobID: "j3", UserID: "u1", SessionID: "s1", State: store.JobRunning, LastSeq: 8}
	mustEqual(t, "other boot", put(j2), true)
	mustEqual(t, "j3", put(j3), true)
	mustEqual(t, "by boot", len(list(store.JobFilter{Limit: store.MaxPageSize, DeviceID: "d1", BootID: "b1"})), 2)
	mustEqual(t, "by session", list(store.JobFilter{Limit: store.MaxPageSize, SessionID: "s2"}), []store.Job{j2})
	mustEqual(t, "running", len(list(store.JobFilter{Limit: store.MaxPageSize, States: []store.JobState{store.JobRunning}})), 2)

	// The device reconnects with boot b2: running jobs of other boots are
	// lost, ended jobs and the live boot's jobs are untouched.
	n, err := s.LoseJobs(ctx, "d1", "b2", t0+10)
	must(t, err)
	mustEqual(t, "lost", n, 1)
	lost := j3
	lost.State, lost.EndedAt, lost.UpdatedAt = store.JobLost, t0+10, t0+10
	mustEqual(t, "after LoseJobs", list(store.JobFilter{Limit: store.MaxPageSize}), []store.Job{want, lost, j2})
	for _, st := range []store.JobState{store.JobRunning, store.JobExited, store.JobKilled, store.JobLost} {
		mustEqual(t, label(st, ".Terminal()"), st.Terminal(), slices.Contains(wantJobTerminal, st))
	}

	_, err = s.PutJob(ctx, store.Job{DeviceID: "ghost", BootID: "b1", JobID: "j1", UserID: "u1", State: store.JobRunning, LastSeq: 1})
	mustIs(t, err, store.ErrNotFound)
	// A job names a device of its own user only.
	must(t, s.CreateUser(ctx, store.User{ID: "u2", Name: "other", CreatedAt: t0}))
	_, err = s.PutJob(ctx, store.Job{DeviceID: "d1", BootID: "b1", JobID: "j8", UserID: "u2", State: store.JobRunning, LastSeq: 1})
	mustIs(t, err, store.ErrNotFound)
	_, err = s.PutJob(ctx, store.Job{DeviceID: "d1", BootID: "b1", JobID: "j9", UserID: "u1", State: "weird"})
	mustIs(t, err, store.ErrInvalid)
	_, err = s.LoseJobs(ctx, "d1", "", t0)
	mustIs(t, err, store.ErrInvalid)
}
