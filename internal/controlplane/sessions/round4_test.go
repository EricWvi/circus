package sessions_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/controlplane/sessions"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/testutil/wait"
	wc "github.com/EricWvi/circus/internal/testutil/workerclient"
)

// gateStore holds one transaction before it starts (so it holds no store
// connection): once armed, the second transaction, which is the release
// step of the connecting boot (the first records its boot), waits until
// open is closed. Nothing else runs transactions through the service while
// the test holds it.
type gateStore struct {
	store.Store
	armed   atomic.Int32
	entered chan struct{}
	open    chan struct{}
}

func (s *gateStore) Tx(ctx context.Context, fn func(ctx context.Context, tx store.Ops) error) error {
	if s.armed.Load() > 0 && s.armed.Add(1) == 3 {
		s.entered <- struct{}{}
		<-s.open
	}
	return s.Store.Tx(ctx, fn)
}

// TestBootOrderFollowsStore: two processes of one worker connect at once;
// the older one's boot transaction commits first but it registers last.
// The in-memory current boot still follows the store: the newer process
// gets Offers and its leases are extended.
func TestBootOrderFollowsStore(t *testing.T) {
	gs := &gateStore{entered: make(chan struct{}, 1), open: make(chan struct{})}
	f := newFixture(t, func(c *sessions.Config) { gs.Store = c.Store; c.Store = gs })
	sessions.DisableFallback(f.svc)
	f.boot("x") // minted first: older
	f.boot("y")
	f.session("s1", -1)
	f.session("s2", -1)
	done := make(chan struct{})
	gs.armed.Store(1)
	go func() { defer close(done); _, _ = f.dialWelcomed(0, "x") }()
	<-gs.entered // x's boot transaction committed; its Connect waits in its release
	<-done       // its Welcome is out
	gs.armed.Store(0)
	y := f.dial(0, "y")
	evY := f.attach(y, 4)
	f.input("s1", "go", store.InputNormal)
	offerTo(t, evY, "s1")
	acquire(t, y, "s1")
	close(gs.open) // x registers last
	wait.For(t, func() bool { return connections(f) == 2 })
	ident, err := f.st.GetIdentity(t.Context(), store.KindWorker, f.workers[0].Identity.ID)
	must(t, err)
	if ident.CurrentBootID != f.boot("y") {
		t.Fatal("the store's current boot is not y")
	}
	f.input("s2", "go", store.InputNormal)
	offerTo(t, evY, "s2")
	before := f.lease("s1").ExpiresAt
	rounds(t, f, sessions.ExtendInterval)
	if f.lease("s1").ExpiresAt == before {
		t.Fatal("the current boot's lease was not extended")
	}
}

// TestRecentDisconnectAlive: an idle current boot that connected long ago
// counts as alive for a lease lifetime after its connection drops, so a
// replaced older process reconnecting meanwhile is refused.
func TestRecentDisconnectAlive(t *testing.T) {
	f := newFixture(t)
	f.boot("old")
	cur := f.dial(0, "new")
	f.attach(cur, 1)
	rounds(t, f, sessions.LeaseLifetime+sessions.SweepInterval) // connected long ago
	_ = cur.Close()
	wait.For(t, func() bool { return connections(f) == 0 })
	if _, err := f.tryDial(0, "old"); err == nil || !strings.Contains(err.Error(), "BAD_HELLO") {
		t.Fatalf("older boot right after the current one disconnected: %v", err)
	}
	rounds(t, f, sessions.LeaseLifetime+sessions.SweepInterval)
	if _, err := f.tryDial(0, "old"); err != nil {
		t.Fatalf("older boot a lifetime after the disconnect: %v", err)
	}
}

// TestConnectedBootAlive: a current boot that connected long ago but is
// still connected keeps refusing an older one.
func TestConnectedBootAlive(t *testing.T) {
	f := newFixture(t)
	f.boot("old")
	cur := f.dial(0, "new")
	f.attach(cur, 1)
	rounds(t, f, sessions.LeaseLifetime+sessions.SweepInterval)
	if _, err := f.tryDial(0, "old"); err == nil || !strings.Contains(err.Error(), "BAD_HELLO") {
		t.Fatalf("older boot while the current one is connected: %v", err)
	}
}

// TestResumeReleasesReservations: entries reserved by a ResumeLeases that
// resumed nothing are dropped afterwards.
func TestResumeReleasesReservations(t *testing.T) {
	f := newFixture(t)
	c := f.dial(0, "boot-a")
	r, err := c.Resume(wait.Ctx(t), &circusv1.ResumeLeases_Held{SessionId: "nope", Epoch: 3})
	must(t, err)
	if o := r.GetResumed().GetLeases(); len(o) != 1 || o[0].GetOk() {
		t.Fatalf("resume %v", r)
	}
	if sessions.Has(f.svc, "nope") {
		t.Fatal("a reservation outlived its ResumeLeases")
	}
}

// TestLoadFenceAtStart: a LoadTranscript waiting for a read permit checks
// its fence when it starts.
func TestLoadFenceAtStart(t *testing.T) {
	f := newFixture(t)
	c, _, epoch := held(t, f)
	sessions.DisableUpkeep(f.svc)
	release := holdReads(t, f)
	r, err := c.Send(wait.Ctx(t), wc.Load("s1", epoch, "", 0, 1024))
	must(t, err)
	time.Sleep(50 * time.Millisecond)
	f.clk.Advance(sessions.LeaseLifetime + time.Second)
	release()
	reply, err := r.Wait(wait.Ctx(t))
	must(t, err)
	wantFenced(t, reply, epoch)
}

// TestLateUnwatchSparesLaterAcquisition: an Unwatch of an earlier,
// delivered AcquireLease does not abandon a later reattachment of the same
// lease on the same connection.
func TestLateUnwatchSparesLaterAcquisition(t *testing.T) {
	f := newFixture(t)
	sessions.DisableFallback(f.svc)
	f.session("s1", -1)
	c := f.dial(0, "boot-a")
	ev := f.attach(c, 1)
	f.input("s1", "go", store.InputNormal)
	offerTo(t, ev, "s1")
	first, err := c.Send(wait.Ctx(t), wc.Acquire("s1", true))
	must(t, err)
	if r, err := first.Wait(wait.Ctx(t)); err != nil || r.GetLease() == nil {
		t.Fatalf("acquire %v %v", r, err)
	}
	release := holdReads(t, f)
	second, err := c.Send(wait.Ctx(t), wc.Acquire("s1", false))
	must(t, err)
	wait.For(t, func() bool { return sessions.Pending(f.svc, "s1") })
	must(t, first.Unwatch(wait.Ctx(t))) // late, for the first operation
	time.Sleep(50 * time.Millisecond)
	if sessions.Abandoned(f.svc, "s1") {
		t.Fatal("an earlier operation's Unwatch abandoned a later acquisition")
	}
	release()
	if r, err := second.Wait(wait.Ctx(t)); err != nil || !r.GetLease().GetReattached() {
		t.Fatalf("reattach %v %v", r, err)
	}
	want := ms(f) + sessions.LeaseLifetime.Milliseconds()
	wait.For(t, func() bool { return f.lease("s1").ExpiresAt == want })
}
