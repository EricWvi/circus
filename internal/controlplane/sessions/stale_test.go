package sessions_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/EricWvi/circus/internal/controlplane/sessions"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/testutil/wait"
	wc "github.com/EricWvi/circus/internal/testutil/workerclient"
)

// pauseStore holds the scheduler between its SettleSession read and what
// it does with it, and holds one transaction before it starts, so a test
// can place an AcquireLease exactly between the two.
type pauseStore struct {
	store.Store
	mu      sync.Mutex
	settle  string        // session whose next SettleSession pauses after reading
	tx      bool          // the next Tx pauses before it starts
	prov    bool          // the next GetProvider outside a Tx pauses after reading
	entered chan struct{} // a pause began
	resume  chan struct{} // ends the pause
}

func newPauseStore() *pauseStore {
	return &pauseStore{entered: make(chan struct{}, 1), resume: make(chan struct{})}
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
		<-p.resume
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
	p.resume <- struct{}{} // the acquisition
	epoch := wait.Recv(t, done)
	f.settle()
	quiet(t, ev1, "s")
	quiet(t, ev0, "s")
	if l := f.lease("s"); uint64(l.Epoch) != epoch {
		t.Fatalf("lease %+v, want epoch %d", l, epoch)
	}
	f.stillBound("s")
}
