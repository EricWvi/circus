package sessions_test

import (
	"context"
	"encoding/json"
	"strconv"
	"sync"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/controlplane/sessions"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/testutil/wait"
	wc "github.com/EricWvi/circus/internal/testutil/workerclient"
)

// goneEvents returns the disconnect times the bus announced for session
// id (KIND_WORKER payloads), in order.
func (f *fixture) goneEvents(id string) []string {
	f.t.Helper()
	var out []string
	f.bus.mu.Lock()
	defer f.bus.mu.Unlock()
	for i, ev := range f.bus.ephemeral {
		if ev.GetKind() != circusv1.SessionEvent_KIND_WORKER || f.bus.ephemeralIDs[i] != id {
			continue
		}
		if !ev.GetEphemeral() || ev.GetSeq() != 0 {
			f.t.Fatalf("worker event not ephemeral: %v", ev)
		}
		var p struct {
			At *string `json:"disconnected_at_unix_ms"`
		}
		must(f.t, json.Unmarshal([]byte(ev.GetPayloadJson()), &p))
		if p.At == nil {
			f.t.Fatalf("worker event without disconnected_at_unix_ms: %s", ev.GetPayloadJson())
		}
		out = append(out, *p.At)
	}
	return out
}

// wantGone checks session id's announced disconnect times and its current
// one (the last announced, or 0 when none was).
func (f *fixture) wantGone(id string, events ...int64) {
	f.t.Helper()
	got := f.goneEvents(id)
	want := make([]string, len(events))
	for i, at := range events {
		want[i] = strconv.FormatInt(at, 10)
	}
	if len(got) != len(want) {
		f.t.Fatalf("worker events of %s %q, want %q", id, got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			f.t.Fatalf("worker events of %s %q, want %q", id, got, want)
		}
	}
	var cur int64
	if len(events) > 0 {
		cur = events[len(events)-1]
	}
	if at := f.svc.WorkerDisconnectedAt(id); at != cur {
		f.t.Fatalf("WorkerDisconnectedAt(%s) = %d, want %d", id, at, cur)
	}
}

// drop closes c and waits until the service has unregistered it (n
// connections left). It returns the close time: the clock does not move
// meanwhile.
func (f *fixture) drop(c *wc.Client, n int) int64 {
	f.t.Helper()
	at := ms(f)
	_ = c.Close()
	wait.For(f.t, func() bool { return connections(f) == n })
	return at
}

// redial connects worker 0 with boot label and waits until the service
// has registered it (n connections then).
func (f *fixture) redial(label string, n int) *wc.Client {
	f.t.Helper()
	c := f.dial(0, label)
	wait.For(f.t, func() bool { return connections(f) == n })
	return c
}

// running acquires s1 on a new connection of boot-a and starts a turn.
func running(t *testing.T, f *fixture) (*wc.Client, wc.Events, uint64) {
	t.Helper()
	c, ev, epoch := held(t, f)
	call(t, c, wc.TakeInputs("s1", epoch))
	call(t, c, wc.Started("s1", epoch, 1, circusv1.TurnOrigin_TURN_ORIGIN_INPUT))
	wait.For(t, func() bool { return connections(f) == 1 })
	return c, ev, epoch
}

// TestGoneMarkedAndResumed: when the holder's only connection closes, its
// lease is marked at the close time; resuming it on a new connection of the
// same boot clears the mark, and the turn goes on.
func TestGoneMarkedAndResumed(t *testing.T) {
	f := newFixture(t)
	sessions.DisableFallback(f.svc)
	c, _, epoch := running(t, f)
	f.wantGone("s1")
	f.clk.Advance(time.Second)
	at := f.drop(c, 0)
	f.wantGone("s1", at)
	f.clk.Advance(2 * time.Second)
	c2 := f.redial("boot-a", 1)
	// Connected again but not resumed yet: the lease is still bound to
	// none of the holder's connections.
	f.wantGone("s1", at)
	if !call2resume(t, c2, "s1", epoch) {
		t.Fatal("resume")
	}
	f.wantGone("s1", at, 0)
	if s := f.get("s1"); s.State != store.SessionRunning {
		t.Fatalf("turn lost: %+v", s)
	}
	call(t, c2, wc.Ended("s1", epoch, 1, circusv1.TurnOutcome_TURN_OUTCOME_COMPLETED, nil))
	wantOk(t, call(t, c2, wc.Release("s1", epoch, circusv1.ReleaseLease_REASON_IDLE)), false)
	f.wantGone("s1", at, 0)
}

// TestGoneAcquireClears: an AcquireLease of a lease the boot already
// holds (idempotent) binds it again and clears the mark.
func TestGoneAcquireClears(t *testing.T) {
	f := newFixture(t)
	sessions.DisableFallback(f.svc)
	c, _, epoch := running(t, f)
	at := f.drop(c, 0)
	c2 := f.redial("boot-a", 1)
	r := call(t, c2, wc.Acquire("s1", false))
	if r.GetLease().GetEpoch() != epoch || !r.GetLease().GetReattached() {
		t.Fatalf("reacquired %v, want epoch %d reattached", r, epoch)
	}
	f.wantGone("s1", at, 0)
	f.stillBound("s1")
}

// TestGoneOnlyWithoutAnyConnection: a lease whose connection closes while
// another connection of the same boot is live is not marked; when that
// one closes too, the lease (unbound since the first close) is marked at
// the second close.
func TestGoneOnlyWithoutAnyConnection(t *testing.T) {
	f := newFixture(t)
	sessions.DisableFallback(f.svc)
	c, _, _ := running(t, f)
	other := f.redial("boot-a", 2)
	f.clk.Advance(time.Second)
	f.drop(c, 1)
	f.wantGone("s1")
	f.clk.Advance(time.Second)
	at := f.drop(other, 0)
	f.wantGone("s1", at)
	// A later connection of the boot that closes again without resuming
	// moves the mark to its close.
	again := f.redial("boot-a", 1)
	f.clk.Advance(time.Second)
	later := f.drop(again, 0)
	f.wantGone("s1", at, later)
}

// TestGoneClearedByExpiry: a holder that never comes back loses its lease
// at expiry; the turn is interrupted and the mark clears, once.
func TestGoneClearedByExpiry(t *testing.T) {
	f := newFixture(t)
	sessions.DisableFallback(f.svc)
	c, _, _ := running(t, f)
	at := f.drop(c, 0)
	rounds(t, f, sessions.LeaseLifetime-sessions.SweepInterval)
	f.wantGone("s1", at)
	rounds(t, f, 2*sessions.SweepInterval)
	wait.For(t, func() bool { return f.svc.WorkerDisconnectedAt("s1") == 0 })
	f.wantGone("s1", at, 0)
	if s := f.get("s1"); s.State == store.SessionRunning || f.lease("s1").WorkerID != "" {
		t.Fatalf("lease not ended: %+v %+v", s, f.lease("s1"))
	}
	f.settle()
	rounds(t, f, sessions.SweepInterval)
	f.wantGone("s1", at, 0)
}

// TestGoneClearedByStop: stopping a session whose holder is gone keeps the
// mark (the holder still holds the lease) until the lease ends.
func TestGoneClearedByStop(t *testing.T) {
	f := newFixture(t)
	sessions.DisableFallback(f.svc)
	c, _, _ := running(t, f)
	at := f.drop(c, 0)
	_, _, changed, err := f.st.StopSession(t.Context(), "s1", ms(f), "user")
	must(t, err)
	if !changed {
		t.Fatal("not stopped")
	}
	f.svc.Wake("s1", circusv1.WorkerEvent_Nudge_REASON_STOP)
	f.settle()
	f.wantGone("s1", at)
	rounds(t, f, sessions.LeaseLifetime+sessions.SweepInterval)
	wait.For(t, func() bool { return f.svc.WorkerDisconnectedAt("s1") == 0 })
	f.wantGone("s1", at, 0)
}

// TestGoneClearedByRevocation: a lease released in the store while its
// holder is gone (a provider change) clears at the evaluation that
// revokes it.
func TestGoneClearedByRevocation(t *testing.T) {
	box := newBox(t, testKey)
	f := newFixture(t, withSecrets(box))
	sessions.DisableFallback(f.svc)
	a := f.provider(box, "a", 0, 0)
	b := f.provider(box, "b", 0, 0)
	f.providerSession("s", a.ID, -1)
	c := f.dial(0, "boot-a")
	ev := f.attach(c, 1)
	f.input("s", "1", store.InputNormal)
	offerTo(t, ev, "s")
	epoch := acquire(t, c, "s")
	turn(t, c, "s", epoch, 1)
	wait.For(t, func() bool { return connections(f) == 1 })
	at := f.drop(c, 0)
	f.wantGone("s", at)
	set, err := f.st.SetSessionProvider(t.Context(), "s", b.ID, ms(f))
	must(t, err)
	if set.Loss == nil {
		t.Fatal("no lease released")
	}
	f.svc.Wake("s")
	f.settle()
	f.wantGone("s", at, 0)
}

// TestGoneClearedByBootChange: a new boot of the worker releases the old
// boot's leases at its connect, which clears their marks.
func TestGoneClearedByBootChange(t *testing.T) {
	f := newFixture(t)
	sessions.DisableFallback(f.svc)
	c, _, _ := running(t, f)
	at := f.drop(c, 0)
	f.redial("boot-b", 1)
	f.wantGone("s1", at, 0)
	if f.lease("s1").WorkerID != "" {
		t.Fatal("the old boot still holds s1")
	}
}

// TestGoneNeverMarkedNeverCleared: leases that end while their holder is
// connected (release, revocation at expiry, a boot change) announce
// nothing.
func TestGoneNeverMarkedNeverCleared(t *testing.T) {
	f := newFixture(t)
	sessions.DisableFallback(f.svc)
	f.session("s1", -1)
	f.session("s2", -1)
	c := f.dial(0, "boot-a")
	ev := f.attach(c, 1)
	f.input("s1", "go", store.InputNormal)
	offerTo(t, ev, "s1")
	epoch := acquire(t, c, "s1")
	turn(t, c, "s1", epoch, 1)
	wantOk(t, call(t, c, wc.Release("s1", epoch, circusv1.ReleaseLease_REASON_IDLE)), false)
	f.input("s2", "go", store.InputNormal)
	offerTo(t, ev, "s2")
	acquire(t, c, "s2")
	f.redial("boot-b", 2) // a new boot releases s2 while boot-a is still connected
	wait.For(t, func() bool { return f.lease("s2").WorkerID == "" })
	f.settle()
	f.drop(c, 1)
	f.wantGone("s1")
	f.wantGone("s2")
}

// TestGoneUnknownAfterRestart: after a control-plane restart a lease whose
// holder has not reconnected reads 0 (unknown), even once an evaluation
// tracks it; its resumption announces nothing.
func TestGoneUnknownAfterRestart(t *testing.T) {
	f := newFixture(t)
	c, _, epoch := running(t, f)
	f.restart()
	<-c.Done()
	// What the old process announced at its shutdown went with it.
	f.bus.mu.Lock()
	f.bus.ephemeral, f.bus.ephemeralIDs = nil, nil
	f.bus.mu.Unlock()
	f.svc.Wake("s1")
	f.settle()
	if !sessions.Has(f.svc, "s1") {
		t.Fatal("the evaluation did not track the held session")
	}
	f.wantGone("s1")
	c2 := f.redial("boot-a", 1)
	if !call2resume(t, c2, "s1", epoch) {
		t.Fatal("resume")
	}
	f.wantGone("s1")
}

// TestGoneStaleMarkRechecked: a marked lease that ended in the store
// without a Wake (one dropped past MaxWakeQueue) is evaluated again by the
// fallback once it should have expired, and its mark clears.
func TestGoneStaleMarkRechecked(t *testing.T) {
	f := newFixture(t)
	c, _, _ := running(t, f)
	sessions.DisableUpkeep(f.svc)
	at := f.drop(c, 0)
	// The lease ends unseen: released in the store, no Wake.
	losses, err := f.st.ReleaseWorkerLeases(t.Context(), f.workers[0].Identity.ID, f.boot("other"), ms(f), store.MaxPageSize)
	must(t, err)
	if len(losses) != 1 {
		t.Fatalf("released %d leases", len(losses))
	}
	sessions.Fallback(f.svc, t.Context())
	f.settle()
	f.wantGone("s1", at) // too early to suspect it
	f.clk.Advance(sessions.LeaseLifetime + sessions.ScheduleInterval)
	sessions.Fallback(f.svc, t.Context())
	f.settle()
	f.wantGone("s1", at, 0)
}

// afterStore pauses right after a store write committed, before the
// service applies it in memory: an expiry sweep that released a lease, or
// the transaction of a connect that released another boot's leases.
type afterStore struct {
	store.Store
	mu      sync.Mutex
	expire  bool // the next ExpireLeases that releases something pauses
	release bool // the next Tx whose ReleaseWorkerLeases releases something pauses
	entered chan struct{}
	resume  chan struct{}
}

func newAfterStore() *afterStore {
	return &afterStore{entered: make(chan struct{}, 1), resume: make(chan struct{})}
}

func withAfter(a *afterStore) option {
	return func(c *sessions.Config) {
		a.Store = c.Store
		c.Store = a
	}
}

func (a *afterStore) take(flag *bool) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	v := *flag
	*flag = false
	return v
}

func (a *afterStore) pause() {
	a.entered <- struct{}{}
	<-a.resume
}

func (a *afterStore) ExpireLeases(ctx context.Context, now int64, limit int) ([]store.LeaseLoss, error) {
	out, err := a.Store.ExpireLeases(ctx, now, limit)
	if err == nil && len(out) > 0 && a.take(&a.expire) {
		a.pause()
	}
	return out, err
}

func (a *afterStore) Tx(ctx context.Context, fn func(ctx context.Context, tx store.Ops) error) error {
	released := false
	err := a.Store.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		return fn(ctx, afterOps{Ops: tx, released: &released})
	})
	if err == nil && released && a.take(&a.release) {
		a.pause()
	}
	return err
}

type afterOps struct {
	store.Ops
	released *bool
}

func (o afterOps) ReleaseWorkerLeases(ctx context.Context, w, keep string, now int64, limit int) ([]store.LeaseLoss, error) {
	out, err := o.Ops.ReleaseWorkerLeases(ctx, w, keep, now, limit)
	if len(out) > 0 {
		*o.released = true
	}
	return out, err
}

// TestGoneRaceSweep: the holder's last connection closes between the
// expiry sweep's commit and its release in memory, and the other way
// round. Either way the lease ends unmarked: marked then cleared, or never
// marked.
func TestGoneRaceSweep(t *testing.T) {
	t.Run("close between commit and release", func(t *testing.T) {
		a := newAfterStore()
		f := newFixture(t, withAfter(a))
		sessions.DisableFallback(f.svc)
		c, _, _ := running(t, f)
		sessions.DisableUpkeep(f.svc) // the lease is not extended and expires while connected
		f.clk.Advance(sessions.LeaseLifetime + time.Second)
		a.mu.Lock()
		a.expire = true
		a.mu.Unlock()
		done := make(chan struct{})
		go func() { defer close(done); sessions.Expire(f.svc, context.Background()) }()
		wait.Recv(t, a.entered)
		at := f.drop(c, 0)
		f.wantGone("s1", at)
		a.resume <- struct{}{}
		wait.Recv(t, done)
		f.wantGone("s1", at, 0)
	})
	t.Run("release before close", func(t *testing.T) {
		f := newFixture(t)
		sessions.DisableFallback(f.svc)
		c, _, _ := running(t, f)
		sessions.DisableUpkeep(f.svc)
		f.clk.Advance(sessions.LeaseLifetime + time.Second)
		sessions.Expire(f.svc, t.Context())
		f.drop(c, 0)
		f.wantGone("s1")
	})
}

// TestGoneRaceBootChange: the old boot's last connection closes while a
// new boot's connect has released the old boot's leases in the store but
// not yet in memory: the lease is marked, then cleared by the release.
func TestGoneRaceBootChange(t *testing.T) {
	a := newAfterStore()
	f := newFixture(t, withAfter(a))
	sessions.DisableFallback(f.svc)
	c, _, _ := running(t, f)
	a.mu.Lock()
	a.release = true
	a.mu.Unlock()
	dialed := make(chan *wc.Client, 1)
	go func() {
		nc, err := f.tryDial(0, "boot-b")
		if err != nil {
			t.Errorf("dial boot-b: %v", err)
		}
		dialed <- nc
	}()
	wait.Recv(t, a.entered)
	at := f.drop(c, 0)
	f.wantGone("s1", at)
	a.resume <- struct{}{}
	wait.Recv(t, dialed)
	wait.For(t, func() bool { return connections(f) == 1 })
	f.wantGone("s1", at, 0)
}

// TestGoneRaceReconnect: a new connection of the boot registered before
// the old one's close keeps the lease unmarked, even with its resumption
// still in flight; one that arrives after the close clears the mark.
func TestGoneRaceReconnect(t *testing.T) {
	t.Run("resume in flight during close", func(t *testing.T) {
		p := newPauseStore()
		f := newFixture(t, withPause(p))
		sessions.DisableFallback(f.svc)
		c, _, epoch := running(t, f)
		c2 := f.redial("boot-a", 2)
		p.pauseTx()
		resumed := make(chan bool, 1)
		go func() {
			r, err := c2.Resume(wait.Ctx(t), &circusv1.ResumeLeases_Held{SessionId: "s1", Epoch: epoch})
			if err != nil {
				t.Errorf("resume: %v", err)
			}
			resumed <- r.GetResumed().GetLeases()[0].GetOk()
		}()
		wait.Recv(t, p.entered)
		f.drop(c, 1)
		f.wantGone("s1")
		p.txResume <- struct{}{}
		if !wait.Recv(t, resumed) {
			t.Fatal("resume")
		}
		f.wantGone("s1")
		f.stillBound("s1")
	})
	t.Run("resumed before close", func(t *testing.T) {
		f := newFixture(t)
		sessions.DisableFallback(f.svc)
		c, _, epoch := running(t, f)
		c2 := f.redial("boot-a", 2)
		if !call2resume(t, c2, "s1", epoch) {
			t.Fatal("resume")
		}
		f.drop(c, 1)
		f.wantGone("s1")
		f.stillBound("s1")
	})
}

// TestGoneRaceEvaluation: an evaluation that read the lease before the
// holder's last connection closed keeps the mark; one that read it before
// a new connection resumed it neither marks it again nor unbinds the
// resumed lease.
func TestGoneRaceEvaluation(t *testing.T) {
	t.Run("close during evaluation", func(t *testing.T) {
		p := newPauseStore()
		f := newFixture(t, withPause(p))
		sessions.DisableFallback(f.svc)
		c, _, _ := running(t, f)
		f.settle()
		p.pauseSettle("s1")
		f.svc.Wake("s1")
		wait.Recv(t, p.entered)
		at := f.drop(c, 0)
		p.resume <- struct{}{}
		f.settle()
		f.wantGone("s1", at)
	})
	t.Run("resume during evaluation", func(t *testing.T) {
		p := newPauseStore()
		f := newFixture(t, withPause(p))
		sessions.DisableFallback(f.svc)
		c, _, epoch := running(t, f)
		at := f.drop(c, 0)
		f.settle()
		p.pauseSettle("s1")
		f.svc.Wake("s1")
		wait.Recv(t, p.entered) // read: bound to the closed connection
		c2 := f.redial("boot-a", 1)
		if !call2resume(t, c2, "s1", epoch) {
			t.Fatal("resume")
		}
		p.resume <- struct{}{}
		f.settle()
		f.wantGone("s1", at, 0)
		f.stillBound("s1")
	})
}

// TestGoneStaleEvaluationAfterRelease: an evaluation that read the lease
// live, while its holder's connection closed and the sweep then released
// it, does not track a holder again for the ended lease, so a later close
// of that boot's connection marks nothing.
func TestGoneStaleEvaluationAfterRelease(t *testing.T) {
	p := newPauseStore()
	f := newFixture(t, withPause(p))
	sessions.DisableFallback(f.svc)
	c, _, epoch := running(t, f)
	call(t, c, wc.Ended("s1", epoch, 1, circusv1.TurnOutcome_TURN_OUTCOME_COMPLETED, nil))
	if _, _, changed, err := f.st.StopSession(t.Context(), "s1", ms(f), "user"); err != nil || !changed {
		t.Fatalf("stop: %v %v", changed, err)
	}
	f.svc.Wake("s1", circusv1.WorkerEvent_Nudge_REASON_STOP)
	sessions.DisableUpkeep(f.svc)
	f.settle()
	p.pauseSettle("s1")
	f.svc.Wake("s1")
	wait.Recv(t, p.entered) // read: live, bound to c
	at := f.drop(c, 0)
	f.clk.Advance(sessions.LeaseLifetime + time.Second)
	sessions.Expire(f.svc, t.Context())
	f.wantGone("s1", at, 0)
	p.resume <- struct{}{}
	f.settle()
	if sessions.Has(f.svc, "s1") {
		t.Fatal("the stale evaluation tracked the ended lease")
	}
	c2 := f.redial("boot-a", 1)
	f.clk.Advance(time.Second)
	f.drop(c2, 0)
	f.wantGone("s1", at, 0)
}

// TestGoneStaleEvaluationAfterUntrackedRelease: the same with no entry
// before the read (a restarted control plane): the lease's loss, applied
// meanwhile, makes the evaluation read again instead of tracking a holder
// for the ended lease.
func TestGoneStaleEvaluationAfterUntrackedRelease(t *testing.T) {
	p := newPauseStore()
	f := newFixture(t, withPause(p))
	c, _, epoch := running(t, f)
	call(t, c, wc.Ended("s1", epoch, 1, circusv1.TurnOutcome_TURN_OUTCOME_COMPLETED, nil))
	if _, _, changed, err := f.st.StopSession(t.Context(), "s1", ms(f), "user"); err != nil || !changed {
		t.Fatalf("stop: %v %v", changed, err)
	}
	f.restart()
	<-c.Done()
	sessions.DisableFallback(f.svc)
	sessions.DisableUpkeep(f.svc)
	f.bus.mu.Lock()
	f.bus.ephemeral, f.bus.ephemeralIDs = nil, nil
	f.bus.mu.Unlock()
	if sessions.Has(f.svc, "s1") {
		t.Fatal("tracked before any evaluation")
	}
	p.pauseSettle("s1")
	f.svc.Wake("s1")
	wait.Recv(t, p.entered) // read: live, no entry
	f.clk.Advance(sessions.LeaseLifetime + time.Second)
	sessions.Expire(f.svc, t.Context())
	if f.lease("s1").WorkerID != "" {
		t.Fatal("not released")
	}
	p.resume <- struct{}{}
	f.settle()
	if sessions.Has(f.svc, "s1") {
		t.Fatal("the stale evaluation tracked the ended lease")
	}
	c2 := f.redial("boot-a", 1)
	f.clk.Advance(time.Second)
	f.drop(c2, 0)
	f.wantGone("s1")
}

// TestStaleEvaluationAfterReleaseKeptEntry: an evaluation that read the
// lease live while its holder released it, leaving the session queued
// (its entry stays: the session waits for an Offer), reads again rather
// than tracking the released holder: the holder gets an Offer, never a
// Nudge or a LeaseRevoked for the lease it gave up.
func TestStaleEvaluationAfterReleaseKeptEntry(t *testing.T) {
	p := newPauseStore()
	f := newFixture(t, withPause(p))
	sessions.DisableFallback(f.svc)
	c, ev, epoch := held(t, f) // s1's input is still pending
	// held may leave Nudges of INPUT sent before the release unread on ev.
	// A Wake raising JOB, which nothing else here raises, is a marker: c's
	// hints are sent in order, so once its Nudge arrives every earlier one
	// has, and any later Nudge for s1 was built after it.
	f.svc.Wake("s1", circusv1.WorkerEvent_Nudge_REASON_JOB)
	nudge(t, ev, "s1", circusv1.WorkerEvent_Nudge_REASON_JOB)
	f.settle()
	p.pauseSettle("s1")
	f.svc.Wake("s1", circusv1.WorkerEvent_Nudge_REASON_INPUT)
	wait.Recv(t, p.entered) // read: live, bound to c
	wantOk(t, call(t, c, wc.Release("s1", epoch, circusv1.ReleaseLease_REASON_IDLE)), false)
	if !sessions.Has(f.svc, "s1") {
		t.Fatal("the queued session's entry was forgotten")
	}
	if s := f.get("s1"); s.State != store.SessionQueued {
		t.Fatalf("released session %v, want queued", s.State)
	}
	p.resume <- struct{}{}
	f.settle()
	for {
		e := next(t, ev)
		if e.GetNudge().GetSessionId() == "s1" || e.GetLeaseRevoked().GetSessionId() == "s1" {
			t.Fatalf("the stale evaluation acted on the released lease: %v", e)
		}
		if e.GetOffer().GetSessionId() == "s1" {
			break
		}
	}
}

// TestStaleRetriesBounded: an evaluation whose read keeps turning out stale
// (the lease is resumed during every read) is repeated at once at most
// MaxStaleRetries times; then the session waits for the fallback, which
// evaluates it again, and nothing acts on a stale read meanwhile.
func TestStaleRetriesBounded(t *testing.T) {
	p := newPauseStore()
	f := newFixture(t, withPause(p))
	c, ev, epoch := running(t, f)
	f.settle()
	p.pauseSettle("s1")
	f.svc.Wake("s1")
	for i := range sessions.MaxStaleRetries + 1 {
		wait.Recv(t, p.entered) // a read of s1, made stale by the resumption below
		if !call2resume(t, c, "s1", epoch) {
			t.Fatal("resume")
		}
		if i < sessions.MaxStaleRetries {
			p.pauseSettle("s1") // the retry's read pauses too
		}
		p.resume <- struct{}{}
	}
	f.settle()
	if !sessions.Parked(f.svc, "s1") {
		t.Fatal("not parked after MaxStaleRetries stale reads")
	}
	quiet(t, ev, "s1")
	sessions.Fallback(f.svc, t.Context())
	f.settle()
	if sessions.Parked(f.svc, "s1") {
		t.Fatal("still parked after the fallback")
	}
	f.stillBound("s1")
	f.wantGone("s1")
}
