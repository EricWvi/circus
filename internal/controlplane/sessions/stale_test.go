package sessions_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/controlplane/sessions"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/testutil/wait"
	wc "github.com/EricWvi/circus/internal/testutil/workerclient"
)

// pauseStore holds the scheduler between its SettleSession read and what
// it does with it, and holds one transaction before it starts, so a test
// can place an AcquireLease exactly between the two. A held read or
// provider read ends with resume, a held transaction with txResume, so a
// test releases each one by name whichever reached its pause first.
type pauseStore struct {
	store.Store
	mu       sync.Mutex
	settle   string        // session whose next SettleSession pauses after reading
	tx       bool          // the next Tx pauses before it starts
	prov     bool          // the next GetProvider outside a Tx pauses after reading
	entered  chan struct{} // a pause began
	resume   chan struct{} // ends a paused read
	txResume chan struct{} // ends a paused transaction
}

func newPauseStore() *pauseStore {
	return &pauseStore{entered: make(chan struct{}, 1), resume: make(chan struct{}), txResume: make(chan struct{})}
}

func (p *pauseStore) SettleSession(ctx context.Context, id string, now int64) (store.SessionSettled, error) {
	out, err := p.Store.SettleSession(ctx, id, now)
	p.mu.Lock()
	hold := p.settle == id
	if hold {
		p.settle = ""
	}
	p.mu.Unlock()
	if hold {
		p.entered <- struct{}{}
		<-p.resume
	}
	return out, err
}

func (p *pauseStore) Tx(ctx context.Context, fn func(ctx context.Context, tx store.Ops) error) error {
	p.mu.Lock()
	hold := p.tx
	p.tx = false
	p.mu.Unlock()
	if hold {
		p.entered <- struct{}{}
		<-p.txResume
	}
	return p.Store.Tx(ctx, fn)
}

func (p *pauseStore) GetProvider(ctx context.Context, userID, id string, now int64) (store.Provider, error) {
	out, err := p.Store.GetProvider(ctx, userID, id, now)
	p.mu.Lock()
	hold := p.prov
	p.prov = false
	p.mu.Unlock()
	if hold {
		p.entered <- struct{}{}
		<-p.resume
	}
	return out, err
}

func (p *pauseStore) pauseProvider()        { p.mu.Lock(); p.prov = true; p.mu.Unlock() }
func (p *pauseStore) pauseSettle(id string) { p.mu.Lock(); p.settle = id; p.mu.Unlock() }
func (p *pauseStore) pauseTx()              { p.mu.Lock(); p.tx = true; p.mu.Unlock() }

func withPause(p *pauseStore) option {
	return func(c *sessions.Config) {
		p.Store = c.Store
		c.Store = p
	}
}

// quiet fails when ev carries an Offer of id or a LeaseRevoked for it
// within a short time.
func quiet(t *testing.T, ev wc.Events, id string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	for {
		e, err := ev.Next(ctx)
		if err != nil {
			return
		}
		if e.GetOffer().GetSessionId() == id || e.GetLeaseRevoked().GetSessionId() == id {
			t.Fatalf("stale evaluation acted: %v", e)
		}
	}
}

// stillBound checks that the lease of id is still extended by the
// scheduler (bound in memory to its connection).
func (f *fixture) stillBound(id string) {
	f.t.Helper()
	f.clk.Advance(10 * time.Second)
	sessions.Extend(f.svc, f.t.Context())
	if l := f.lease(id); l.ExpiresAt != ms(f)+sessions.LeaseLifetime.Milliseconds() {
		f.t.Fatalf("lease of %s no longer extended: %+v at %d", id, l, ms(f))
	}
}

// TestStaleEvaluationKeepsNewLease: an evaluation that read the session
// queued without a lease, while a worker acquired it before the scheduler
// took its lock, neither revokes the new lease nor offers the session
// again, and the lease stays bound (extended). It runs for a local-login
// session (the window between the read and the lock predates providers)
// and for a session with a provider (whose provider read widened it).
func TestStaleEvaluationKeepsNewLease(t *testing.T) {
	for _, withProvider := range []bool{false, true} {
		name := "local login"
		if withProvider {
			name = "provider"
		}
		t.Run(name, func(t *testing.T) {
			box := newBox(t, testKey)
			p := newPauseStore()
			f := newFixture(t, withSecrets(box), withPause(p))
			sessions.DisableFallback(f.svc)
			c0 := f.dial(0, "b0")
			ev0 := f.attach(c0, 8)
			if withProvider {
				prov := f.provider(box, "prov", 1, 0)
				f.providerSession("s", prov.ID, -1)
			} else {
				f.session("s", -1)
			}
			f.input("s", "1", store.InputNormal)
			offerTo(t, ev0, "s")
			f.settle()
			p.pauseSettle("s")
			f.svc.Wake("s")
			wait.Recv(t, p.entered)
			epoch := acquire(t, c0, "s")
			p.resume <- struct{}{}
			f.settle()
			quiet(t, ev0, "s")
			if l := f.lease("s"); uint64(l.Epoch) != epoch || l.WorkerID == "" {
				t.Fatalf("lease %+v, want epoch %d held", l, epoch)
			}
			if r := f.reason("s"); r != 0 {
				t.Fatalf("leased session has reason %v", r)
			}
			f.stillBound("s")
		})
	}
}

// TestStaleEvaluationWaitsForAcquisition: an evaluation that read the
// session queued while an AcquireLease for it was in flight does not
// offer it elsewhere; it runs again once the acquisition ends.
func TestStaleEvaluationWaitsForAcquisition(t *testing.T) {
	p := newPauseStore()
	f := newFixture(t, withPause(p))
	sessions.DisableFallback(f.svc)
	c0, c1 := f.dial(0, "b0"), f.dial(1, "b1")
	ev0 := f.attach(c0, 8)
	f.session("s", -1)
	f.input("s", "1", store.InputNormal)
	offerTo(t, ev0, "s")
	ev1 := f.attach(c1, 8)
	f.settle()
	// The Offer has run out, so a stale evaluation would offer anew, to
	// the idle connection c1.
	f.clk.Advance(sessions.ScheduleInterval)
	p.pauseSettle("s")
	f.svc.Wake("s")
	wait.Recv(t, p.entered)
	p.pauseTx()
	done := make(chan uint64, 1)
	go func() {
		r, err := c0.Session(wait.Ctx(t), wc.Acquire("s", true))
		if err != nil || r.GetLease() == nil {
			t.Errorf("acquire: %v, %v", r, err)
		}
		done <- r.GetLease().GetEpoch()
	}()
	// The acquisition holds its reservation, waiting before its
	// transaction, while the evaluation goes on with its stale read.
	wait.Recv(t, p.entered)
	p.resume <- struct{}{} // the evaluation
	wait.For(t, func() bool { return !sessions.Busy(f.svc) })
	p.txResume <- struct{}{} // the acquisition
	epoch := wait.Recv(t, done)
	f.settle()
	quiet(t, ev1, "s")
	quiet(t, ev0, "s")
	if l := f.lease("s"); uint64(l.Epoch) != epoch {
		t.Fatalf("lease %+v, want epoch %d", l, epoch)
	}
	f.stillBound("s")
}

// TestStaleLiveReadAfterRelease: an evaluation that read the lease live,
// while its holder released it before the scheduler took its lock,
// neither binds the released lease again nor sends the holder a
// LeaseRevoked for it (then, or from the next extension round), and the
// connection's capacity is free for the next session. It runs for a
// local-login session and for a session with a provider, which then hold
// no further work (the release forgets the entry), and for a session
// with work left (the release keeps the entry and offers it again).
func TestStaleLiveReadAfterRelease(t *testing.T) {
	for _, tc := range []struct {
		name               string
		provider, workLeft bool
	}{{"local login", false, false}, {"provider", true, false}, {"work left", false, true}} {
		t.Run(tc.name, func(t *testing.T) {
			box := newBox(t, testKey)
			p := newPauseStore()
			f := newFixture(t, withSecrets(box), withPause(p))
			sessions.DisableFallback(f.svc)
			c0 := f.dial(0, "b0")
			ev0 := f.attach(c0, 1)
			if tc.provider {
				prov := f.provider(box, "prov", 1, 0)
				f.providerSession("s", prov.ID, -1)
				f.providerSession("t", prov.ID, -1)
			} else {
				f.session("s", -1)
				f.session("t", -1)
			}
			f.input("s", "1", store.InputNormal)
			offerTo(t, ev0, "s")
			epoch := acquire(t, c0, "s")
			turn(t, c0, "s", epoch, 1)
			reason := circusv1.ReleaseLease_REASON_IDLE
			if tc.workLeft {
				f.input("s", "2", store.InputNormal)
				reason = circusv1.ReleaseLease_REASON_ERROR
			}
			f.settle()
			p.pauseSettle("s")
			f.svc.Wake("s")
			wait.Recv(t, p.entered)
			wantOk(t, call(t, c0, wc.Release("s", epoch, reason)), false)
			p.resume <- struct{}{}
			f.settle()
			if tc.workLeft {
				// The release offers the session again; the stale read
				// neither revokes the released lease nor keeps it bound,
				// so the Offer can be taken under a new epoch.
				offerNoRevoke(t, ev0, "s")
				noRevoke(t, ev0, "s")
				if next := acquire(t, c0, "s"); next <= epoch {
					t.Fatalf("acquired epoch %d after releasing %d", next, epoch)
				}
				f.stillBound("s")
				return
			}
			quiet(t, ev0, "s")
			if sessions.Has(f.svc, "s") {
				t.Fatal("released lease is tracked again")
			}
			sessions.Extend(f.svc, t.Context())
			f.settle()
			quiet(t, ev0, "s")
			// c0 holds nothing, so its one slot takes the next session.
			f.input("t", "1", store.InputNormal)
			offerTo(t, ev0, "t")
		})
	}
}

// offerNoRevoke waits for an Offer of id on ev and fails on a
// LeaseRevoked for id before it.
func offerNoRevoke(t *testing.T, ev wc.Events, id string) {
	t.Helper()
	for {
		e := next(t, ev)
		if e.GetLeaseRevoked().GetSessionId() == id {
			t.Fatalf("released lease revoked: %v", e)
		}
		if e.GetOffer().GetSessionId() == id {
			return
		}
	}
}

// noRevoke fails when ev carries a LeaseRevoked for id within a short
// time.
func noRevoke(t *testing.T, ev wc.Events, id string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	for {
		e, err := ev.Next(ctx)
		if err != nil {
			return
		}
		if e.GetLeaseRevoked().GetSessionId() == id {
			t.Fatalf("released lease revoked: %v", e)
		}
	}
}

// TestStaleRetryKeepsReasonsPastFullQueue: a stale evaluation retried
// while the Wake queue is full keeps its Nudge reasons: they go to the
// current lease's entry and its holder is nudged at once. Here the read
// predates a fresh acquisition by the holder, which advanced the epoch and
// dropped the reasons the Wake had put on the old lease's entry.
func TestStaleRetryKeepsReasonsPastFullQueue(t *testing.T) {
	p := newPauseStore()
	f := newFixture(t, withPause(p))
	sessions.DisableFallback(f.svc)
	c0 := f.dial(0, "b0")
	ev0 := f.attach(c0, 8)
	f.session("s", -1)
	f.input("s", "1", store.InputNormal)
	offerTo(t, ev0, "s")
	epoch := acquire(t, c0, "s")
	turn(t, c0, "s", epoch, 1)
	f.settle()
	p.pauseSettle("s")
	f.svc.Wake("s", circusv1.WorkerEvent_Nudge_REASON_INPUT)
	wait.Recv(t, p.entered)
	if next := acquire(t, c0, "s"); next <= epoch {
		t.Fatalf("fresh acquisition kept epoch %d", next)
	}
	// Hints sent so far (the Lease reply's Nudge carries nothing: the old
	// lease's reasons ended with it) are skipped.
	quietFor(ev0)
	sessions.FillWakes(f.svc, sessions.MaxWakeQueue)
	p.resume <- struct{}{}
	nudge(t, ev0, "s", circusv1.WorkerEvent_Nudge_REASON_INPUT)
}

// quietFor drains ev until it has been quiet for a short time.
func quietFor(ev wc.Events) {
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		_, err := ev.Next(ctx)
		cancel()
		if err != nil {
			return
		}
	}
}

// TestStaleEvaluationDeferredPastRefusedResume: an evaluation that read
// the session queued while a ResumeLeases of it was in flight is deferred
// to the end of that reservation; when the resumption is refused without
// queuing an evaluation of its own, the deferred one still runs, so the
// session is offered again without waiting for the fallback.
func TestStaleEvaluationDeferredPastRefusedResume(t *testing.T) {
	p := newPauseStore()
	f := newFixture(t, withPause(p))
	sessions.DisableFallback(f.svc)
	c0 := f.dial(0, "b0")
	ev0 := f.attach(c0, 8)
	f.session("s", -1)
	f.input("s", "1", store.InputNormal)
	offerTo(t, ev0, "s")
	f.settle()
	// The Offer has run out, so the evaluation offers the session anew.
	f.clk.Advance(sessions.ScheduleInterval)
	p.pauseSettle("s")
	f.svc.Wake("s")
	wait.Recv(t, p.entered)
	p.pauseTx()
	done := make(chan *circusv1.SessionReply, 1)
	go func() {
		// No lease of s exists: the resumption is refused (fenced, epoch
		// 0) and queues nothing itself.
		r, err := c0.Resume(wait.Ctx(t), &circusv1.ResumeLeases_Held{SessionId: "s", Epoch: 3})
		if err != nil {
			t.Errorf("resume: %v", err)
		}
		done <- r
	}()
	wait.Recv(t, p.entered)
	p.resume <- struct{}{} // the evaluation, deferred
	wait.For(t, func() bool { return !sessions.Busy(f.svc) })
	p.txResume <- struct{}{} // the resumption
	if o := wait.Recv(t, done).GetResumed().GetLeases(); len(o) != 1 || o[0].GetOk() {
		t.Fatalf("resumed %v, want refused", o)
	}
	offerTo(t, ev0, "s")
}

// postCommitPause holds the next transaction after it commits, until
// finish is closed.
type postCommitPause struct {
	*pauseStore
	next      atomic.Bool
	committed chan struct{}
	finish    chan struct{}
}

func (p *postCommitPause) Tx(ctx context.Context, fn func(context.Context, store.Ops) error) error {
	hold := p.next.Swap(false)
	err := p.pauseStore.Tx(ctx, fn)
	if hold {
		close(p.committed)
		select {
		case <-p.finish:
		case <-ctx.Done():
		}
	}
	return err
}

// TestStaleRetryPastFullQueueKeepsInterruptLevel: a stale evaluation of an
// interrupt, retried while the Wake queue is full, nudges the holder with
// the interrupt level an evaluation would send, not the older level its
// entry had. Here a ResumeLeases read level 0 and bound the lease after
// the evaluation read level 1.
func TestStaleRetryPastFullQueueKeepsInterruptLevel(t *testing.T) {
	for _, full := range []bool{false, true} {
		t.Run(fmt.Sprintf("full=%v", full), func(t *testing.T) {
			p := &postCommitPause{pauseStore: newPauseStore(), committed: make(chan struct{}), finish: make(chan struct{})}
			f := newFixture(t, func(c *sessions.Config) { p.Store = c.Store; c.Store = p })
			sessions.DisableUpkeep(f.svc)
			c0 := f.dial(0, "b0")
			ev0 := f.attach(c0, 8)
			f.session("s", -1)
			f.input("s", "1", store.InputNormal)
			offerTo(t, ev0, "s")
			epoch := acquire(t, c0, "s")
			call(t, c0, wc.TakeInputs("s", epoch))
			call(t, c0, wc.Started("s", epoch, 1, circusv1.TurnOrigin_TURN_ORIGIN_INPUT))
			c1 := f.dial(0, "b0")
			ev1 := f.attach(c1, 8)
			f.settle()
			p.next.Store(true)
			done := make(chan *circusv1.SessionReply, 1)
			go func() {
				r, _ := c1.Resume(wait.Ctx(t), &circusv1.ResumeLeases_Held{SessionId: "s", Epoch: epoch})
				done <- r
			}()
			wait.Recv(t, p.committed) // the resumption read level 0
			sess, _, changed, err := f.st.InterruptSession(t.Context(), "s", ms(f))
			must(t, err)
			if !changed || sess.InterruptTurnNo != 1 {
				t.Fatalf("interrupt changed=%v level=%d", changed, sess.InterruptTurnNo)
			}
			p.pauseSettle("s")
			f.svc.Wake("s", circusv1.WorkerEvent_Nudge_REASON_INTERRUPT)
			wait.Recv(t, p.entered) // the evaluation read level 1
			close(p.finish)
			if o := wait.Recv(t, done).GetResumed().GetLeases(); len(o) != 1 || !o[0].GetOk() {
				t.Fatalf("resume = %v", o)
			}
			if first := nudge(t, ev1, "s", circusv1.WorkerEvent_Nudge_REASON_INTERRUPT); first.GetInterruptTurnNo() != 0 {
				t.Fatalf("resume hint = %v", first)
			}
			if full {
				sessions.FillWakes(f.svc, sessions.MaxWakeQueue)
			}
			p.resume <- struct{}{}
			if again := nudge(t, ev1, "s", circusv1.WorkerEvent_Nudge_REASON_INTERRUPT); again.GetInterruptTurnNo() != 1 {
				t.Fatalf("retry hint = %v, want level 1", again)
			}
			f.settle()
		})
	}
}

// TestStaleReadWhileReservationCreatesEntry: after a restart, an
// evaluation reads an untracked lease live; the sweep ends it and a
// ResumeLeases of another epoch reserves an entry for the session before
// the evaluation takes its lock. The evaluation treats the new entry as a
// change and reads again, so the ended lease is never tracked, even after
// the resumption is refused.
func TestStaleReadWhileReservationCreatesEntry(t *testing.T) {
	p := newPauseStore()
	f := newFixture(t, withPause(p))
	c0 := f.dial(0, "b0")
	ev0 := f.attach(c0, 8)
	f.session("s", -1)
	f.input("s", "1", store.InputNormal)
	offerTo(t, ev0, "s")
	epoch := acquire(t, c0, "s")
	turn(t, c0, "s", epoch, 1)
	f.restart()
	sessions.DisableUpkeep(f.svc)
	c1 := f.dial(0, "b0")
	f.attach(c1, 8)
	f.settle()
	if sessions.Has(f.svc, "s") {
		t.Fatal("restart already tracks s")
	}
	p.pauseSettle("s")
	f.svc.Wake("s")
	wait.Recv(t, p.entered) // the evaluation read the lease live
	f.clk.Advance(sessions.LeaseLifetime + time.Second)
	sessions.Expire(f.svc, t.Context())
	if l := f.lease("s"); l.WorkerID != "" || sessions.Has(f.svc, "s") {
		t.Fatalf("sweep did not end the lease: %+v", l)
	}
	p.pauseTx()
	done := make(chan *circusv1.SessionReply, 1)
	go func() {
		r, err := c1.Resume(wait.Ctx(t), &circusv1.ResumeLeases_Held{SessionId: "s", Epoch: epoch + 1})
		if err != nil {
			t.Errorf("resume: %v", err)
		}
		done <- r
	}()
	wait.Recv(t, p.entered) // the resumption reserved its entry
	p.resume <- struct{}{}
	wait.For(t, func() bool { return !sessions.Busy(f.svc) })
	p.txResume <- struct{}{}
	if o := wait.Recv(t, done).GetResumed().GetLeases(); len(o) != 1 || o[0].GetOk() {
		t.Fatalf("resume = %v", o)
	}
	f.settle()
	if sessions.Has(f.svc, "s") {
		t.Fatal("the ended lease is tracked")
	}
}
