package sessions_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/controlplane/sessions"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/testutil/wait"
	"github.com/EricWvi/circus/internal/testutil/workerclient"
)

// agentStore gates ReportWorkerAgents (once, when armed) and fails
// ListWorkerAgents while failList is set.
type agentStore struct {
	store.Store
	armed    atomic.Bool
	entered  chan struct{}
	release  chan struct{}
	failList atomic.Bool
	// listArmed holds the next ListWorkerAgents after its read, until
	// listRelease is closed.
	listArmed   atomic.Bool
	listEntered chan struct{}
	listRelease chan struct{}
}

func (g *agentStore) ReportWorkerAgents(ctx context.Context, id string, from store.AgentReporter, r []store.AgentStatus, at int64) (store.WorkerAgents, error) {
	if g.armed.CompareAndSwap(true, false) {
		close(g.entered)
		<-g.release
	}
	return g.Store.ReportWorkerAgents(ctx, id, from, r, at)
}

func (g *agentStore) ListWorkerAgents(ctx context.Context, id string) (store.WorkerAgents, error) {
	if g.failList.Load() {
		return store.WorkerAgents{}, store.ErrBusy
	}
	wa, err := g.Store.ListWorkerAgents(ctx, id)
	if g.listArmed.CompareAndSwap(true, false) {
		close(g.listEntered)
		<-g.listRelease
	}
	return wa, err
}

func withAgentStore(g *agentStore) option {
	return func(c *sessions.Config) {
		g.Store = c.Store
		c.Store = g
	}
}

// noWanted asserts that no AgentsWanted arrives on ev within a short time.
func noWanted(t *testing.T, ev workerclient.Events) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	for {
		e, err := ev.NextEvent(ctx)
		if err != nil {
			return
		}
		if e.GetAgentsWanted() != nil {
			t.Fatalf("unexpected %v", e)
		}
	}
}

// TestAgentReportOrderedByCommit: a report that commits after a concurrent
// RefreshWorkerAgents has read (and applied) the worker's agents is applied
// after it: views follow the commit order, not the order the operations
// started in, so the installed worker gets the session.
func TestAgentReportOrderedByCommit(t *testing.T) {
	g := &agentStore{entered: make(chan struct{}), release: make(chan struct{})}
	f := newFixture(t, withAgentStore(g))
	f.session("s1", -1)
	c := f.dial(0, "boot-a")
	ev := f.attachWith(c, &circusv1.Attach{MaxSessions: 2, Agents: []*circusv1.AgentStatus{workerclient.ClaudeCode(notInstalled, "", "2.0")}})
	wantWishes(t, wanted(t, ev))
	f.input("s1", "go", store.InputNormal)
	f.waitReason("s1", circusv1.QueueReason_QUEUE_REASON_AGENT_NOT_INSTALLED)
	f.wish(0, true, "2.0")
	wantWishes(t, wanted(t, ev), "AGENT_KIND_CLAUDE_CODE@2.0")

	g.armed.Store(true)
	r, err := c.ReportAgents(wait.Ctx(t), workerclient.ClaudeCode(installed, "2.0", "2.0"))
	must(t, err)
	<-g.entered
	must(t, f.svc.RefreshWorkerAgents(t.Context(), f.workers[0].Identity.ID))
	close(g.release)
	reply, err := r.Wait(wait.Ctx(t))
	must(t, err)
	wantOk(t, reply, false)
	offerTo(t, ev, "s1")
	acquire(t, c, "s1")
}

// TestAgentStaleViewNoOfferLoop: when the view still says the worker runs
// the agent but the store no longer wants it (the push after the wish
// failed), AcquireLease refuses once and applies what it read: the session
// is not offered again and waits as AGENT_NOT_INSTALLED.
func TestAgentStaleViewNoOfferLoop(t *testing.T) {
	f := newFixture(t)
	c := f.dial(0, "boot-a")
	ev := f.attach(c, 4)
	wantWishes(t, wanted(t, ev), "AGENT_KIND_CLAUDE_CODE@cli")
	_, err := f.st.SetWorkerAgentWish(t.Context(), f.workers[0].Identity.ID, store.AgentClaudeCode, false, "")
	must(t, err)
	f.session("s1", 0)
	f.input("s1", "go", store.InputNormal)
	offerTo(t, ev, "s1")
	wantRejected(t, call(t, c, workerclient.Acquire("s1", true)), circusv1.SessionReply_Rejected_CODE_NOT_OFFERED)
	// The refusal applied a new view, so the worker also gets the wishes.
	wantWishes(t, wanted(t, ev))
	f.waitReason("s1", circusv1.QueueReason_QUEUE_REASON_AGENT_NOT_INSTALLED)
	f.settle()
	noOffer(t, ev)
}

// TestAgentRefreshRetried: when RefreshWorkerAgents cannot read the store
// after a wish changed, it fails and keeps reading in the background until
// it can: the worker then gets the wish.
func TestAgentRefreshRetried(t *testing.T) {
	g := &agentStore{}
	f := newFixture(t, withAgentStore(g))
	c := f.dial(0, "boot-a")
	ev := f.attach(c, 4)
	wantWishes(t, wanted(t, ev), "AGENT_KIND_CLAUDE_CODE@cli")
	id := f.workers[0].Identity.ID
	_, err := f.st.SetWorkerAgentWish(t.Context(), id, store.AgentClaudeCode, false, "")
	must(t, err)
	g.failList.Store(true)
	if err := f.svc.RefreshWorkerAgents(t.Context(), id); !errors.Is(err, store.ErrBusy) {
		t.Fatalf("refresh with the store failing: %v", err)
	}
	noWanted(t, ev)
	// Still failing: a retry or two pass without effect.
	f.clk.Advance(time.Second)
	noWanted(t, ev)
	g.failList.Store(false)
	got := make(chan int, 1)
	go func() {
		ctx, cancel := context.WithTimeout(t.Context(), wait.Timeout)
		defer cancel()
		for {
			e, err := ev.NextEvent(ctx)
			if err != nil {
				return
			}
			if w := e.GetAgentsWanted(); w != nil {
				got <- len(w.GetAgents())
				return
			}
		}
	}()
	wait.For(t, func() bool {
		f.clk.Advance(time.Second)
		select {
		case n := <-got:
			if n != 0 {
				t.Fatalf("wishes pushed after the retry still want %d agents", n)
			}
			return true
		case <-time.After(10 * time.Millisecond):
			return false
		}
	})
}

// TestAgentReportsOfOlderConnection: a report the worker's older connection
// of the same boot delivers after the new connection's Attach is refused,
// so it cannot overwrite the newer state.
func TestAgentReportsOfOlderConnection(t *testing.T) {
	f := newFixture(t)
	old := f.dial(0, "boot-a")
	f.attach(old, 1)
	c := f.dial(0, "boot-a")
	f.attachWith(c, &circusv1.Attach{MaxSessions: 1, Agents: []*circusv1.AgentStatus{workerclient.ClaudeCode(notInstalled, "", "cli")}})
	r, err := old.ReportAgents(wait.Ctx(t), workerclient.ClaudeInstalled()...)
	must(t, err)
	reply, err := r.Wait(wait.Ctx(t))
	must(t, err)
	wantRejected(t, reply, circusv1.SessionReply_Rejected_CODE_INVALID)
	got, err := f.st.ListWorkerAgents(t.Context(), f.workers[0].Identity.ID)
	must(t, err)
	if a, _ := got.Get(store.AgentClaudeCode); a.Status.State != store.AgentNotInstalled {
		t.Fatalf("after the older connection's report: %+v", a)
	}
	report(t, c, workerclient.ClaudeInstalled()...)
}

// TestAgentStaleBoot: once a new boot connected, the old boot's report and
// Attach are refused (checked inside the write), and the old boot's
// connections get no AgentsWanted.
func TestAgentStaleBoot(t *testing.T) {
	f := newFixture(t)
	a := f.dial(0, "boot-a")
	evA := f.attach(a, 1)
	wantWishes(t, wanted(t, evA), "AGENT_KIND_CLAUDE_CODE@cli")
	late := f.dial(0, "boot-a") // dialed before the new boot, attaches after it
	b := f.dial(0, "boot-b")
	evB := f.attachWith(b, &circusv1.Attach{MaxSessions: 1, Agents: []*circusv1.AgentStatus{workerclient.ClaudeCode(notInstalled, "", "cli")}})
	wantWishes(t, wanted(t, evB), "AGENT_KIND_CLAUDE_CODE@cli")

	r, err := a.ReportAgents(wait.Ctx(t), workerclient.ClaudeInstalled()...)
	must(t, err)
	reply, err := r.Wait(wait.Ctx(t))
	must(t, err)
	wantRejected(t, reply, circusv1.SessionReply_Rejected_CODE_INVALID)
	_, err = late.Attach(wait.Ctx(t), &circusv1.Attach{MaxSessions: 1, Agents: workerclient.ClaudeInstalled()})
	must(t, err)
	closed(t, late)
	got, err := f.st.ListWorkerAgents(t.Context(), f.workers[0].Identity.ID)
	must(t, err)
	if x, _ := got.Get(store.AgentClaudeCode); x.Status.State != store.AgentNotInstalled {
		t.Fatalf("after the old boot's reports: %+v", x)
	}

	f.wish(0, false, "")
	wantWishes(t, wanted(t, evB))
	noWanted(t, evA)
}

// TestAgentGainedRederivesReasons: a session waiting as AGENT_NOT_INSTALLED
// on a worker with no capacity left (the rescan stops there) has its
// reason derived again when the worker's agent runs again.
func TestAgentGainedRederivesReasons(t *testing.T) {
	f := newFixture(t)
	c := f.dial(0, "boot-a")
	ev := f.attach(c, 1)
	f.session("s1", 0)
	f.input("s1", "go", store.InputNormal)
	offerTo(t, ev, "s1")
	acquire(t, c, "s1")
	// The worker's data directory was wiped while it holds the lease.
	report(t, c, workerclient.ClaudeCode(notInstalled, "", "cli"))
	f.session("s2", 0)
	f.input("s2", "go", store.InputNormal)
	f.waitReason("s2", circusv1.QueueReason_QUEUE_REASON_AGENT_NOT_INSTALLED)
	report(t, c, workerclient.ClaudeInstalled()...)
	// Online with the agent but full: no reason against it.
	f.waitReason("s2", circusv1.QueueReason_QUEUE_REASON_UNSPECIFIED)
}

// TestAgentOlderReadDropped: a read of the worker's agents that finishes
// after a newer write was applied (a RefreshWorkerAgents whose read
// predates the worker's report) does not replace the newer view.
func TestAgentOlderReadDropped(t *testing.T) {
	g := &agentStore{listEntered: make(chan struct{}), listRelease: make(chan struct{})}
	f := newFixture(t, withAgentStore(g))
	c := f.dial(0, "boot-a")
	ev := f.attachWith(c, &circusv1.Attach{MaxSessions: 2, Agents: []*circusv1.AgentStatus{workerclient.ClaudeCode(notInstalled, "", "2.0")}})
	wantWishes(t, wanted(t, ev))
	f.wish(0, true, "2.0")
	wantWishes(t, wanted(t, ev), "AGENT_KIND_CLAUDE_CODE@2.0")
	f.session("s1", 0)
	f.input("s1", "go", store.InputNormal)
	f.waitReason("s1", circusv1.QueueReason_QUEUE_REASON_AGENT_NOT_INSTALLED)

	g.listArmed.Store(true)
	refreshed := make(chan error, 1)
	go func() {
		refreshed <- f.svc.RefreshWorkerAgents(context.WithoutCancel(t.Context()), f.workers[0].Identity.ID)
	}()
	<-g.listEntered
	report(t, c, workerclient.ClaudeCode(installed, "2.0", "2.0"))
	offerTo(t, ev, "s1")
	close(g.listRelease)
	must(t, wait.Recv(t, refreshed))
	f.settle()
	acquire(t, c, "s1")
}

// TestAgentLateAttachOfOlderConnection: the worker's older connection of
// the same boot has its Attach processed after the newer connection's
// (its receive loop was behind): it does not take the reports over, its
// Attach closes the link, and the live connection's reports count.
func TestAgentLateAttachOfOlderConnection(t *testing.T) {
	f := newFixture(t)
	f.session("s1", 0)
	old := f.dial(0, "boot-a")
	c := f.dial(0, "boot-a")
	evC := f.attachWith(c, &circusv1.Attach{MaxSessions: 2, Agents: []*circusv1.AgentStatus{workerclient.ClaudeCode(notInstalled, "", "cli")}})
	f.wish(0, true, "cli")
	_, err := old.Attach(wait.Ctx(t), &circusv1.Attach{MaxSessions: 1, Agents: []*circusv1.AgentStatus{workerclient.ClaudeCode(notInstalled, "", "cli")}})
	must(t, err)
	closed(t, old)
	report(t, c, workerclient.ClaudeInstalled()...)
	got, err := f.st.ListWorkerAgents(t.Context(), f.workers[0].Identity.ID)
	must(t, err)
	if a, _ := got.Get(store.AgentClaudeCode); a.Status.State != store.AgentInstalled {
		t.Fatalf("stored %+v", a)
	}
	f.input("s1", "go", store.InputNormal)
	offerTo(t, evC, "s1")
	acquire(t, c, "s1")
}

// TestAgentRetryStops: the background read stops when the worker is gone
// (a missing worker is not read again, nor retried at all), and Close
// returns while a retry is pending; a later failure, after a retry
// succeeded, is retried again.
func TestAgentRetryStops(t *testing.T) {
	g := &agentStore{}
	f := newFixture(t, withAgentStore(g))
	id := f.workers[0].Identity.ID
	advanceUntil := func(cond func() bool) {
		t.Helper()
		wait.For(t, func() bool {
			if cond() {
				return true
			}
			f.clk.Advance(time.Second)
			return cond()
		})
	}

	// A worker the store does not have: nothing to retry.
	if err := f.svc.RefreshWorkerAgents(t.Context(), "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("refresh of a missing worker: %v", err)
	}
	if sessions.AgentRetrying(f.svc, "nope") {
		t.Fatal("a missing worker is retried")
	}
	// Failing first, then found missing: the retry ends.
	g.failList.Store(true)
	if err := f.svc.RefreshWorkerAgents(t.Context(), "gone"); !errors.Is(err, store.ErrBusy) {
		t.Fatalf("refresh with the store failing: %v", err)
	}
	if !sessions.AgentRetrying(f.svc, "gone") {
		t.Fatal("no retry after a failed read")
	}
	g.failList.Store(false)
	advanceUntil(func() bool { return !sessions.AgentRetrying(f.svc, "gone") })

	// A retry that succeeds ends; a later failure is retried again.
	for range 2 {
		g.failList.Store(true)
		if err := f.svc.RefreshWorkerAgents(t.Context(), id); !errors.Is(err, store.ErrBusy) {
			t.Fatalf("refresh with the store failing: %v", err)
		}
		if !sessions.AgentRetrying(f.svc, id) {
			t.Fatal("no retry after a failed read")
		}
		g.failList.Store(false)
		advanceUntil(func() bool { return !sessions.AgentRetrying(f.svc, id) })
	}

	// Close with a retry pending returns.
	g.failList.Store(true)
	if err := f.svc.RefreshWorkerAgents(t.Context(), id); err == nil {
		t.Fatal("refresh succeeded with the store failing")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	must(t, f.svc.Close(ctx))
}

// TestAgentsWantedAttempt: every install request reaches the worker with a
// higher attempt, though the wanted version is the same.
func TestAgentsWantedAttempt(t *testing.T) {
	f := newFixture(t)
	c := f.dial(0, "boot-a")
	ev := f.attachWith(c, &circusv1.Attach{MaxSessions: 1, Agents: []*circusv1.AgentStatus{workerclient.ClaudeCode(notInstalled, "", "cli")}})
	wantWishes(t, wanted(t, ev))
	for want := uint64(1); want <= 2; want++ {
		f.wish(0, true, "cli")
		for {
			e, err := ev.NextEvent(wait.Ctx(t))
			must(t, err)
			if w := e.GetAgentsWanted(); w != nil {
				if len(w.GetAgents()) != 1 || w.GetAgents()[0].GetAttempt() != want {
					t.Fatalf("AgentsWanted %v, want attempt %d", w, want)
				}
				break
			}
		}
	}
}
