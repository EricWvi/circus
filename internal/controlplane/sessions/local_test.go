package sessions_test

import (
	"context"
	"maps"
	"slices"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/controlplane/sessions"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/testutil/wait"
	wc "github.com/EricWvi/circus/internal/testutil/workerclient"
)

// offers waits until ev has offered every session in want; Offers of other
// sessions fail the test. Nudges are skipped.
func offers(t *testing.T, ev wc.Events, want ...string) {
	t.Helper()
	missing := map[string]bool{}
	for _, id := range want {
		missing[id] = true
	}
	for len(missing) > 0 {
		e := next(t, ev)
		id := e.GetOffer().GetSessionId()
		if id == "" {
			continue
		}
		if !slices.Contains(want, id) {
			t.Fatalf("offer of %q, want only %v (missing %v)", id, want, slices.Sorted(maps.Keys(missing)))
		}
		delete(missing, id)
	}
}

// tryNext returns the next hint on ev, or false when none arrives within a
// short time.
func tryNext(t *testing.T, ev wc.Events) (*circusv1.WorkerEvent, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	e, err := ev.Next(ctx)
	return e, err == nil
}

// TestLocalOnlyOffers: a local-only connection is offered only the local
// sessions pinned to its worker, and can acquire no other, also after its
// worker changes boot; every other session is offered as before.
func TestLocalOnlyOffers(t *testing.T) {
	f := newFixture(t)
	f.session("remote", -1)
	f.session("pinned", 0) // pinned to worker 0, not local
	f.localSession("local0", 0)
	f.localSession("local1", 1)
	c0 := f.dial(0, "boot-0")
	ev0 := f.attachLocal(c0, 8)
	for _, id := range []string{"remote", "pinned", "local1"} {
		f.input(id, "1", store.InputNormal)
	}
	f.settle()
	noOffer(t, ev0)
	for _, id := range []string{"remote", "pinned", "local1"} {
		wantRejected(t, call(t, c0, wc.Acquire(id, true)), circusv1.SessionReply_Rejected_CODE_NOT_OFFERED)
		if st := f.get(id).State; st != store.SessionQueued {
			t.Fatalf("%s is %s after a refused acquisition", id, st)
		}
	}
	f.input("local0", "1", store.InputNormal)
	offers(t, ev0, "local0")
	r := call(t, c0, wc.Acquire("local0", true))
	if !r.GetLease().GetConfig().GetLocal() {
		t.Fatalf("lease of a local session = %v", r)
	}

	// The other worker's plain connection gets the remote session and its
	// own local one; the session pinned to worker 0 waits for a plain
	// connection of that worker.
	c1 := f.dial(1, "boot-1")
	ev1 := f.attach(c1, 8)
	offers(t, ev1, "remote", "local1")
	acquire(t, c1, "local1")
	if r := call(t, c1, wc.Acquire("remote", true)); r.GetLease().GetConfig().GetLocal() {
		t.Fatalf("lease of a remote session = %v", r)
	}
	wantRejected(t, call(t, c1, wc.Acquire("pinned", true)), circusv1.SessionReply_Rejected_CODE_NOT_OFFERED)

	// Worker 0 restarts (a new boot) with a local-only connection: still
	// nothing but its local sessions, also from the fallback scan.
	_ = c0.Close()
	c0 = f.dial(0, "boot-0b")
	ev0 = f.attachLocal(c0, 8)
	f.session("later", -1)
	f.input("later", "1", store.InputNormal)
	offers(t, ev1, "later")
	offers(t, ev0, "local0") // its lease went with the old boot; the input is still pending
	f.clk.Advance(sessions.ScheduleInterval)
	sessions.Fallback(f.svc, t.Context())
	f.settle()
	for {
		e, ok := tryNext(t, ev0)
		if !ok {
			break
		}
		if id := e.GetOffer().GetSessionId(); id != "" && id != "local0" {
			t.Fatalf("local-only connection offered %q", id)
		}
	}
	wantRejected(t, call(t, c0, wc.Acquire("pinned", true)), circusv1.SessionReply_Rejected_CODE_NOT_OFFERED)

	// A plain connection of the same boot takes the pinned session.
	c0p := f.dial(0, "boot-0b")
	ev0p := f.attach(c0p, 8)
	offerTo(t, ev0p, "pinned") // local0 may be offered here too
	acquire(t, c0p, "pinned")
}

// TestLocalSessionStaysOnItsWorker: a local session is offered only to its
// pinned worker; while that worker is offline it waits, through the
// worker's reconnects and boot changes, and another worker never gets it.
func TestLocalSessionStaysOnItsWorker(t *testing.T) {
	f := newFixture(t)
	f.localSession("local", 0)
	c1 := f.dial(1, "boot-1")
	ev1 := f.attach(c1, 8)
	f.input("local", "1", store.InputNormal)
	f.settle()
	noOffer(t, ev1)
	wantRejected(t, call(t, c1, wc.Acquire("local", true)), circusv1.SessionReply_Rejected_CODE_NOT_OFFERED)
	if st := f.get("local").State; st != store.SessionQueued {
		t.Fatalf("state %s while the worker is offline", st)
	}

	c0 := f.dial(0, "boot-0a")
	ev0 := f.attach(c0, 1)
	offers(t, ev0, "local")
	epoch := acquire(t, c0, "local")

	// A reconnect on the same boot, now local-only, resumes it.
	_ = c0.Close()
	c0 = f.dial(0, "boot-0a")
	ev0 = f.attachLocal(c0, 1)
	if !call2resume(t, c0, "local", epoch) {
		t.Fatal("resume of a local session on a local-only connection failed")
	}
	_ = ev0

	// The worker goes away with the lease: it expires, the pending input
	// makes the session queued again, and it waits for its worker.
	_ = c0.Close()
	// Only once the server has dropped the connection does the lease stop
	// being extended.
	wait.For(t, func() bool { return sessions.Connections(f.svc) == 1 })
	f.clk.Advance(sessions.LeaseLifetime + sessions.SweepInterval)
	sessions.Expire(f.svc, t.Context())
	f.settle()
	if st := f.get("local").State; st != store.SessionQueued {
		t.Fatalf("state %s after expiry", st)
	}
	sessions.Fallback(f.svc, t.Context())
	f.settle()
	noOffer(t, ev1)

	// A new boot of the pinned worker gets it again.
	c0 = f.dial(0, "boot-0b")
	ev0 = f.attachLocal(c0, 1)
	offers(t, ev0, "local")
	epoch = acquire(t, c0, "local")
	// Another boot change releases that lease; the session waits for the
	// newest boot's connection, not for the other worker.
	f.input("local", "2", store.InputNormal)
	c0c := f.dial(0, "boot-0c")
	f.settle()
	noOffer(t, ev1)
	ev0c := f.attach(c0c, 1)
	offers(t, ev0c, "local")
	acquire(t, c0c, "local")
	wantFenced(t, call(t, c0, wc.TakeInputs("local", epoch)), epoch+1)
	noOffer(t, ev1)
}

// TestLocalOnlyKeepsRemoteLeasesOut: a local-only connection does not take
// over what its worker's boot holds of remote sessions: their leases are
// not resumed or reattached there, and their Offers do not move to it.
func TestLocalOnlyKeepsRemoteLeasesOut(t *testing.T) {
	f := newFixture(t)
	f.session("remote", -1)
	f.session("waiting", -1)
	f.localSession("local", 0)
	c := f.dial(0, "boot")
	ev := f.attach(c, 8)
	f.input("remote", "1", store.InputNormal)
	offers(t, ev, "remote")
	remote := acquire(t, c, "remote")
	f.input("local", "1", store.InputNormal)
	offers(t, ev, "local")
	local := acquire(t, c, "local")
	f.input("waiting", "1", store.InputNormal)
	offers(t, ev, "waiting") // not taken: the Offer stays with c

	lc := f.dial(0, "boot")
	lev := f.attachLocal(lc, 8)
	f.settle()
	noOffer(t, lev)
	r, err := lc.Resume(t.Context(), &circusv1.ResumeLeases_Held{SessionId: "remote", Epoch: remote}, &circusv1.ResumeLeases_Held{SessionId: "local", Epoch: local})
	must(t, err)
	got := r.GetResumed().GetLeases()
	if got[0].GetOk() || got[0].GetEpoch() != remote || !got[1].GetOk() || got[1].GetEpoch() != local {
		t.Fatalf("resume on a local-only connection = %v", got)
	}
	wantRejected(t, call(t, lc, wc.Acquire("remote", false)), circusv1.SessionReply_Rejected_CODE_NOT_OFFERED)
	wantRejected(t, call(t, lc, wc.Acquire("remote", true)), circusv1.SessionReply_Rejected_CODE_NOT_OFFERED)
	wantRejected(t, call(t, lc, wc.Acquire("waiting", true)), circusv1.SessionReply_Rejected_CODE_NOT_OFFERED)

	// The remote lease is still bound to the plain connection, and the
	// local one moved to the local-only connection.
	if l := f.lease("remote"); l.Epoch != int64(remote) || l.ConnID == "" {
		t.Fatalf("remote lease = %+v", l)
	}
	if r := call(t, c, wc.TakeInputs("remote", remote)); len(r.GetInputs().GetInputs()) != 1 {
		t.Fatalf("the plain connection lost the remote lease: %v", r)
	}
	wantFenced(t, call(t, c, wc.TakeInputs("local", local)), local)
	if r := call(t, lc, wc.TakeInputs("local", local)); len(r.GetInputs().GetInputs()) != 1 {
		t.Fatalf("take on the local-only connection = %v", r)
	}
	// The plain connection can still take the Offer it holds.
	acquire(t, c, "waiting")
}

// TestLocalOfferMovesToLocalOnly: an Offer of a local session held by a
// plain connection moves at Attach to a local-only connection of the same
// worker and boot (the worker reconnected), which can then acquire it.
func TestLocalOfferMovesToLocalOnly(t *testing.T) {
	f := newFixture(t)
	f.localSession("local", 0)
	c := f.dial(0, "boot")
	ev := f.attach(c, 8)
	f.input("local", "1", store.InputNormal)
	offers(t, ev, "local") // not taken
	lc := f.dial(0, "boot")
	lev := f.attachLocal(lc, 8)
	offers(t, lev, "local")
	if r := call(t, lc, wc.Acquire("local", true)); !r.GetLease().GetConfig().GetLocal() {
		t.Fatalf("acquire of the moved Offer = %v", r)
	}
}

// TestLocalOnlyResumeUnknownSession: on a local-only connection, a resume
// naming a session that does not exist fails only that outcome; the other
// leases of the request still resume.
func TestLocalOnlyResumeUnknownSession(t *testing.T) {
	f := newFixture(t)
	f.localSession("local", 0)
	c := f.dial(0, "boot")
	ev := f.attachLocal(c, 8)
	f.input("local", "1", store.InputNormal)
	offers(t, ev, "local")
	epoch := acquire(t, c, "local")
	lc := f.dial(0, "boot")
	f.attachLocal(lc, 8)
	r, err := lc.Resume(t.Context(), &circusv1.ResumeLeases_Held{SessionId: "missing", Epoch: 1}, &circusv1.ResumeLeases_Held{SessionId: "local", Epoch: epoch})
	must(t, err)
	got := r.GetResumed().GetLeases()
	if len(got) != 2 || got[0].GetOk() || !got[1].GetOk() || got[1].GetEpoch() != epoch {
		t.Fatalf("resume = %v", r)
	}
}
