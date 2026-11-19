package sessions_test

import (
	"strings"
	"testing"
	"time"

	"github.com/EricWvi/circus/internal/controlplane/sessions"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/testutil/wait"
	wc "github.com/EricWvi/circus/internal/testutil/workerclient"
)

// TestBootsAliveAfterStart: after a control-plane start every boot counts
// as alive for one lease lifetime, so a crash (which records no
// disconnect) does not let a replaced older process take over before the
// current one reconnects.
func TestBootsAliveAfterStart(t *testing.T) {
	f := newFixture(t)
	f.boot("old")
	cur := f.dial(0, "new")
	f.attach(cur, 1)
	rounds(t, f, sessions.LeaseLifetime+sessions.SweepInterval) // connected long ago
	f.restart()
	// As after a crash: no disconnect recorded since the connect.
	must(t, f.st.SetDisconnected(t.Context(), store.KindWorker, f.workers[0].Identity.ID, ms(f)-2*sessions.LeaseLifetime.Milliseconds()))
	if _, err := f.tryDial(0, "old"); err == nil || !strings.Contains(err.Error(), "BAD_HELLO") {
		t.Fatalf("older boot right after a start: %v", err)
	}
	f.clk.Advance(sessions.LeaseLifetime + time.Second)
	if _, err := f.tryDial(0, "old"); err != nil {
		t.Fatalf("older boot a lifetime after the start: %v", err)
	}
}

// TestUnwatchRingRemembersSeveral: an Unwatch of an earlier delivered
// AcquireLease still abandons its lease after later acquisitions on the
// connection.
func TestUnwatchRingRemembersSeveral(t *testing.T) {
	f := newFixture(t)
	sessions.DisableFallback(f.svc)
	f.session("s1", -1)
	f.session("s2", -1)
	c := f.dial(0, "boot-a")
	ev := f.attach(c, 2)
	f.input("s1", "go", store.InputNormal)
	f.input("s2", "go", store.InputNormal)
	offerTo(t, ev, "s1")
	first, err := c.Send(wait.Ctx(t), wc.Acquire("s1", true))
	must(t, err)
	if r, err := first.Wait(wait.Ctx(t)); err != nil || r.GetLease() == nil {
		t.Fatalf("acquire s1 %v %v", r, err)
	}
	acquire(t, c, "s2")
	must(t, first.Unwatch(wait.Ctx(t)))
	wait.For(t, func() bool { return sessions.Abandoned(f.svc, "s1") })
	if sessions.Abandoned(f.svc, "s2") {
		t.Fatal("the later acquisition was abandoned")
	}
}

// TestResumeDropsToken: once a lease is resumed, a late Unwatch of the
// AcquireLease that took it does not abandon it.
func TestResumeDropsToken(t *testing.T) {
	f := newFixture(t)
	sessions.DisableFallback(f.svc)
	f.session("s1", -1)
	c := f.dial(0, "boot-a")
	ev := f.attach(c, 1)
	f.input("s1", "go", store.InputNormal)
	offerTo(t, ev, "s1")
	first, err := c.Send(wait.Ctx(t), wc.Acquire("s1", true))
	must(t, err)
	r, err := first.Wait(wait.Ctx(t))
	must(t, err)
	if !call2resume(t, c, "s1", r.GetLease().GetEpoch()) {
		t.Fatal("resume")
	}
	must(t, first.Unwatch(wait.Ctx(t)))
	time.Sleep(50 * time.Millisecond)
	if sessions.Abandoned(f.svc, "s1") {
		t.Fatal("a late Unwatch abandoned a resumed lease")
	}
	rounds(t, f, sessions.ExtendInterval)
	if want := ms(f) + sessions.LeaseLifetime.Milliseconds(); f.lease("s1").ExpiresAt != want {
		t.Fatal("the resumed lease was not extended")
	}
}
