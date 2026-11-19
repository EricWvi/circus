package sessions_test

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/controlplane/sessions"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/testutil/wait"
	wc "github.com/EricWvi/circus/internal/testutil/workerclient"
	"github.com/google/uuid"
)

// holdReads takes every read permit until the returned release is called.
func holdReads(t *testing.T, f *fixture) (release func()) {
	t.Helper()
	ch := make(chan struct{})
	var held atomic.Int32
	done := make(chan struct{}, sessions.ReadConcurrency)
	for range sessions.ReadConcurrency {
		go func() {
			defer func() { done <- struct{}{} }()
			f.svc.Read(context.Background(), func() { held.Add(1); <-ch })
		}()
	}
	wait.For(t, func() bool { return held.Load() == sessions.ReadConcurrency })
	var once atomic.Bool
	release = func() {
		if once.CompareAndSwap(false, true) {
			close(ch)
			for range sessions.ReadConcurrency {
				<-done
			}
		}
	}
	t.Cleanup(release)
	return release
}

// rounds advances the fake clock by d (a multiple of SweepInterval) in
// steps of SweepInterval, each of which runs an upkeep round, waiting for
// the round after each.
func rounds(t *testing.T, f *fixture, d time.Duration) {
	t.Helper()
	for d > 0 {
		step := sessions.SweepInterval
		r := sessions.Rounds(f.svc)
		f.clk.Advance(step)
		wait.For(t, func() bool { return sessions.Rounds(f.svc) > r })
		d -= step
	}
}

// v7At returns a UUIDv7 with the given Unix ms timestamp.
func v7At(t *testing.T, at int64) string {
	t.Helper()
	u, err := uuid.NewV7()
	must(t, err)
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(at))
	copy(u[0:6], b[2:8])
	return u.String()
}

// TestRestartWakeBeforeResume: after a restart, a Wake that reaches the
// scheduler before the worker resumes does not make the resumption look
// like a later one: INPUT, JOB and INTERRUPT are still re-sent.
func TestRestartWakeBeforeResume(t *testing.T) {
	f := newFixture(t)
	f.session("s1", -1)
	c := f.dial(0, "boot-a")
	ev := f.attach(c, 1)
	f.input("s1", "go", store.InputNormal)
	offerTo(t, ev, "s1")
	epoch := acquire(t, c, "s1")
	call(t, c, wc.TakeInputs("s1", epoch))
	call(t, c, wc.Started("s1", epoch, 1, circusv1.TurnOrigin_TURN_ORIGIN_INPUT))
	_, _, changed, err := f.st.InterruptSession(t.Context(), "s1", ms(f))
	must(t, err)
	if !changed {
		t.Fatal("no interrupt")
	}
	f.restart()
	f.svc.Wake("s1", circusv1.WorkerEvent_Nudge_REASON_APPROVAL)
	f.settle()
	c2 := f.dial(0, "boot-a")
	ev2 := f.attach(c2, 1)
	if !call2resume(t, c2, "s1", epoch) {
		t.Fatal("resume after restart")
	}
	n := nudge(t, ev2, "s1", circusv1.WorkerEvent_Nudge_REASON_INTERRUPT)
	if !hasReason(n, circusv1.WorkerEvent_Nudge_REASON_INPUT) || !hasReason(n, circusv1.WorkerEvent_Nudge_REASON_JOB) || n.GetInterruptTurnNo() != 1 {
		t.Fatalf("nudge %v", n)
	}
}

// TestSkewedBootAccepted: a boot minted on a clock that was ahead does not
// lock later boots out once it is gone: an older-looking boot is accepted
// and becomes current.
func TestSkewedBootAccepted(t *testing.T) {
	f := newFixture(t)
	f.mu.Lock()
	f.boots = map[string]string{
		"skewed": v7At(t, time.Now().Add(time.Hour).UnixMilli()),
		"fixed":  v7At(t, time.Now().UnixMilli()),
	}
	f.mu.Unlock()
	c := f.dial(0, "skewed")
	wait.For(t, func() bool { return connections(f) == 1 })
	_ = c.Close()
	wait.For(t, func() bool { return connections(f) == 0 })
	// Still within one lease lifetime of its connect: refused.
	if _, err := f.tryDial(0, "fixed"); err == nil || !strings.Contains(err.Error(), "BAD_HELLO") {
		t.Fatalf("older boot within the lifetime: %v", err)
	}
	f.clk.Advance(sessions.LeaseLifetime + time.Second)
	fixed := f.dial(0, "fixed")
	ev := f.attach(fixed, 1)
	f.session("s1", -1)
	f.input("s1", "go", store.InputNormal)
	offerTo(t, ev, "s1") // current, not stale
	ident, err := f.st.GetIdentity(t.Context(), store.KindWorker, f.workers[0].Identity.ID)
	must(t, err)
	if ident.CurrentBootID != f.boot("fixed") {
		t.Fatalf("current boot %s", ident.CurrentBootID)
	}
}

func connections(f *fixture) int { return sessions.Connections(f.svc) }

// TestOlderBootRefusedWhileAlive: an older boot is refused while the
// current one has a connection, or connected within a lease lifetime, or
// still holds an unexpired lease; once none holds, it is accepted.
func TestOlderBootRefusedWhileAlive(t *testing.T) {
	f := newFixture(t)
	sessions.DisableFallback(f.svc)
	f.boot("old")
	f.session("s1", -1)
	cur := f.dial(0, "new")
	ev := f.attach(cur, 1)
	refused := func(why string) {
		t.Helper()
		if _, err := f.tryDial(0, "old"); err == nil || !strings.Contains(err.Error(), "BAD_HELLO") {
			t.Fatalf("%s: older boot dial: %v", why, err)
		}
	}
	refused("connected")
	f.input("s1", "go", store.InputNormal)
	offerTo(t, ev, "s1")
	acquire(t, cur, "s1")
	rounds(t, f, 2*sessions.ExtendInterval) // the lease now outlives the connect time + lifetime
	_ = cur.Close()
	wait.For(t, func() bool { return connections(f) == 0 })
	refused("connected within a lifetime")
	rounds(t, f, sessions.ExtendInterval+sessions.SweepInterval)
	refused("an unexpired lease")
	rounds(t, f, sessions.LeaseLifetime)
	if f.lease("s1").WorkerID != "" {
		t.Fatal("lease not expired")
	}
	old := f.dial(0, "old")
	f.attach(old, 1)
}

// TestPendingLeaseExtendedWhileQueued: a lease whose Lease reply waits for
// a read permit is extended while its AcquireLease is open, so it is still
// held when the reply is built.
func TestPendingLeaseExtendedWhileQueued(t *testing.T) {
	f := newFixture(t)
	sessions.DisableFallback(f.svc)
	f.session("s1", -1)
	c := f.dial(0, "boot-a")
	ev := f.attach(c, 1)
	f.input("s1", "go", store.InputNormal)
	offerTo(t, ev, "s1")
	release := holdReads(t, f)
	r, err := c.Send(wait.Ctx(t), wc.Acquire("s1", true))
	must(t, err)
	wait.For(t, func() bool { return sessions.Pending(f.svc, "s1") })
	rounds(t, f, 80*time.Second)
	if f.lease("s1").WorkerID == "" {
		t.Fatal("the lease of an open AcquireLease expired")
	}
	release()
	reply, err := r.Wait(wait.Ctx(t))
	must(t, err)
	if reply.GetLease().GetEpoch() != 1 {
		t.Fatalf("reply %v", reply)
	}
}

// TestDeliveryExtendsAtOnce: when a Lease reply is delivered, its lease is
// extended at once, not at the next round.
func TestDeliveryExtendsAtOnce(t *testing.T) {
	f := newFixture(t)
	sessions.DisableFallback(f.svc)
	f.session("s1", -1)
	c := f.dial(0, "boot-a")
	ev := f.attach(c, 1)
	f.input("s1", "go", store.InputNormal)
	offerTo(t, ev, "s1")
	release := holdReads(t, f)
	r, err := c.Send(wait.Ctx(t), wc.Acquire("s1", true))
	must(t, err)
	wait.For(t, func() bool { return sessions.Pending(f.svc, "s1") })
	rounds(t, f, 10*time.Second) // no extension round yet
	release()
	_, err = r.Wait(wait.Ctx(t))
	must(t, err)
	want := ms(f) + sessions.LeaseLifetime.Milliseconds()
	wait.For(t, func() bool { return f.lease("s1").ExpiresAt == want })
}

// TestResumeClearsAbandoned: a lease abandoned by an unwatched AcquireLease
// is extended again once the worker resumes it.
func TestResumeClearsAbandoned(t *testing.T) {
	f := newFixture(t)
	sessions.DisableFallback(f.svc)
	f.session("s1", -1)
	c := f.dial(0, "boot-a")
	ev := f.attach(c, 1)
	f.input("s1", "go", store.InputNormal)
	offerTo(t, ev, "s1")
	release := holdReads(t, f)
	r, err := c.Send(wait.Ctx(t), wc.Acquire("s1", true))
	must(t, err)
	wait.For(t, func() bool { return sessions.Pending(f.svc, "s1") })
	// The reply may get out before the Unwatch is processed, or not:
	// either way the Unwatch abandons the lease.
	release()
	must(t, r.Unwatch(wait.Ctx(t)))
	wait.For(t, func() bool { return sessions.Abandoned(f.svc, "s1") })
	// Resumed on the same connection: only the resumption clears it.
	if !call2resume(t, c, "s1", 1) {
		t.Fatal("resume")
	}
	rounds(t, f, sessions.ExtendInterval)
	if want := ms(f) + sessions.LeaseLifetime.Milliseconds(); f.lease("s1").ExpiresAt != want {
		t.Fatal("a resumed lease was not extended")
	}
}

// failStore fails ReleaseWorkerLeases once.
type failStore struct {
	store.Store
	fail atomic.Bool
}

// Tx passes ops whose ReleaseWorkerLeases can fail: the connection's
// release runs in its own Tx.
func (s *failStore) Tx(ctx context.Context, fn func(ctx context.Context, tx store.Ops) error) error {
	return s.Store.Tx(ctx, func(ctx context.Context, tx store.Ops) error { return fn(ctx, failOps{tx, s}) })
}

type failOps struct {
	store.Ops
	s *failStore
}

func (o failOps) ReleaseWorkerLeases(ctx context.Context, w, keep string, now int64, limit int) ([]store.LeaseLoss, error) {
	if o.s.fail.CompareAndSwap(true, false) {
		return nil, errors.New("injected")
	}
	return o.Ops.ReleaseWorkerLeases(ctx, w, keep, now, limit)
}

// TestBootChangeReleaseRetried: a boot change whose release failed is
// completed by the next connect of the new boot.
func TestBootChangeReleaseRetried(t *testing.T) {
	fs := &failStore{}
	f := newFixture(t, func(c *sessions.Config) { fs.Store = c.Store; c.Store = fs })
	sessions.DisableFallback(f.svc)
	f.session("s1", -1)
	old := f.dial(0, "old")
	evO := f.attach(old, 1)
	f.input("s1", "go", store.InputNormal)
	offerTo(t, evO, "s1")
	acquire(t, old, "s1")
	fs.fail.Store(true)
	if c, err := f.tryDial(0, "new"); err == nil {
		<-c.Done() // closed: its boot was recorded, the release failed
	}
	if f.lease("s1").BootID != f.boot("old") {
		t.Fatal("released by the failed connect")
	}
	newer := f.dial(0, "new")
	f.attach(newer, 1)
	if l := f.lease("s1"); l.WorkerID != "" {
		t.Fatalf("the old boot still holds s1: %+v", l)
	}
}

// TestStaleLeaseNotExtended: a lease bound to a connection of a boot that
// is no longer current is not extended.
func TestStaleLeaseNotExtended(t *testing.T) {
	fs := &failStore{}
	f := newFixture(t, func(c *sessions.Config) { fs.Store = c.Store; c.Store = fs })
	sessions.DisableFallback(f.svc)
	f.session("s1", -1)
	old := f.dial(0, "old")
	evO := f.attach(old, 1)
	f.input("s1", "go", store.InputNormal)
	offerTo(t, evO, "s1")
	acquire(t, old, "s1")
	before := f.lease("s1").ExpiresAt
	fs.fail.Store(true)
	if c, err := f.tryDial(0, "new"); err == nil {
		<-c.Done()
	}
	// The new boot is current although the release failed; until a
	// connect completes it, the old boot's lease is not kept alive.
	sessions.SetCurrentBoot(f.svc, f.workers[0].Identity.ID, f.boot("new"))
	rounds(t, f, sessions.ExtendInterval)
	if f.lease("s1").ExpiresAt != before {
		t.Fatal("a stale boot's lease was extended")
	}
}

// TestMultiSessionRescan: a rescan offers every queued session a
// connection has room for, not only the first one.
func TestMultiSessionRescan(t *testing.T) {
	f := newFixture(t)
	sessions.DisableFallback(f.svc)
	for i := range 3 {
		id := fmt.Sprintf("s%d", i)
		f.session(id, -1)
		f.input(id, "go", store.InputNormal)
	}
	f.settle()
	c := f.dial(0, "boot-a")
	ev := f.attach(c, 3)
	seen := map[string]bool{}
	for len(seen) < 3 {
		if e := next(t, ev); e.GetOffer() != nil {
			seen[e.GetOffer().GetSessionId()] = true
		}
	}
}

// TestFullWakeQueueNudgesDirectly: with the Wake queue full (the scheduler
// stuck), a Wake of a held session nudges its holder at once.
func TestFullWakeQueueNudgesDirectly(t *testing.T) {
	slow := &slowStore{}
	f := newFixture(t, withSlowStore(slow))
	c, ev, _ := held(t, f)
	_ = c
	f.settle()
	slow.settle.Store(int64(2 * time.Second))
	// Get the scheduler stuck on one evaluation, then fill the queue.
	f.svc.Wake("stuck")
	wait.For(t, func() bool { return sessions.Busy(f.svc) })
	for i := range sessions.MaxWakeQueue {
		f.svc.Wake(fmt.Sprintf("bogus%d", i))
	}
	f.svc.Wake("s1", circusv1.WorkerEvent_Nudge_REASON_JOB)
	got := eventWithin(ev, time.Second, func(e *circusv1.WorkerEvent) bool {
		return e.GetNudge().GetSessionId() == "s1" && hasReason(e.GetNudge(), circusv1.WorkerEvent_Nudge_REASON_JOB)
	})
	slow.settle.Store(0)
	if got == nil {
		t.Fatal("no direct Nudge with the Wake queue full")
	}
}

// TestReadFenceAtStart: a read waiting for a permit checks its fence when
// it starts: a lease that expired meanwhile is Fenced.
func TestReadFenceAtStart(t *testing.T) {
	f := newFixture(t)
	c, _, epoch := held(t, f)
	sessions.DisableUpkeep(f.svc)
	release := holdReads(t, f)
	r, err := c.Send(wait.Ctx(t), wc.State("s1", epoch))
	must(t, err)
	time.Sleep(50 * time.Millisecond) // received and waiting
	f.clk.Advance(sessions.LeaseLifetime + time.Second)
	release()
	reply, err := r.Wait(wait.Ctx(t))
	must(t, err)
	wantFenced(t, reply, epoch)
}

// TestTableFullRefusals: with the session table full, a resumption or a
// reattachment is refused (retryable) instead of leaving its lease
// untracked.
func TestTableFullRefusals(t *testing.T) {
	f := newFixture(t)
	c, _, epoch := held(t, f)
	_ = c
	f.restart()
	sessions.SetMaxTracked(f.svc, 0)
	c2 := f.dial(0, "boot-a")
	r, err := c2.Resume(wait.Ctx(t), &circusv1.ResumeLeases_Held{SessionId: "s1", Epoch: epoch})
	must(t, err)
	wantRejected(t, r, circusv1.SessionReply_Rejected_CODE_STORE_UNAVAILABLE)
	wantRejected(t, call(t, c2, wc.Acquire("s1", false)), circusv1.SessionReply_Rejected_CODE_STORE_UNAVAILABLE)
	sessions.SetMaxTracked(f.svc, sessions.MaxTracked)
	if !call2resume(t, c2, "s1", epoch) {
		t.Fatal("resume with room")
	}
}

// TestReservedEntryKept: the scheduler does not forget an entry a
// ResumeLeases holds while its transaction runs.
func TestReservedEntryKept(t *testing.T) {
	slow := &slowStore{}
	f := newFixture(t, withSlowStore(slow))
	_, _, epoch := held(t, f)
	f.restart()
	c2 := f.dial(0, "boot-a")
	slow.tx.Store(int64(300 * time.Millisecond))
	done := make(chan bool, 1)
	go func() {
		r, err := c2.Resume(context.Background(), &circusv1.ResumeLeases_Held{SessionId: "s1", Epoch: epoch})
		done <- err == nil && r.GetResumed().GetLeases()[0].GetOk()
	}()
	wait.For(t, func() bool { return sessions.Has(f.svc, "s1") })
	sessions.Forget(f.svc, "s1")
	if !sessions.Has(f.svc, "s1") {
		t.Fatal("a reserved entry was forgotten")
	}
	slow.tx.Store(0)
	if !<-done {
		t.Fatal("resume")
	}
}

// TestDisconnectStampedBeforeRemoval: a closing connection's disconnect is
// stamped no later than the moment it left the service's connections,
// although the store write lands after its sender stopped. So a test (or a
// reconnect) that sees the connection gone and then moves the clock on
// never finds a disconnect stamped after that move.
func TestDisconnectStampedBeforeRemoval(t *testing.T) {
	for i := range 40 {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			f := newFixture(t)
			c := f.dial(0, "a")
			f.attach(c, 1)
			id := f.workers[0].Identity.ID
			before, err := f.st.GetIdentity(t.Context(), store.KindWorker, id)
			must(t, err)
			_ = c.Close()
			wait.For(t, func() bool { return connections(f) == 0 })
			gone := f.clk.Now().UnixMilli()
			f.clk.Advance(sessions.LeaseLifetime + time.Second)
			var stamp int64
			wait.For(t, func() bool {
				ident, err := f.st.GetIdentity(t.Context(), store.KindWorker, id)
				stamp = ident.LastDisconnectedAt
				return err == nil && stamp != before.LastDisconnectedAt
			})
			if stamp > gone {
				t.Fatalf("disconnect stamped at %d, after the connection was gone at %d", stamp, gone)
			}
		})
	}
}
