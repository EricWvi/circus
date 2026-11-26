package sessions_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/testutil/wait"
	"github.com/EricWvi/circus/internal/testutil/workerclient"
)

// wanted waits for the next AgentsWanted on ev, skipping session hints, and
// returns its wishes as "agent@version".
func wanted(t *testing.T, ev workerclient.Events) []string {
	t.Helper()
	for {
		e, err := ev.NextEvent(wait.Ctx(t))
		must(t, err)
		if w := e.GetAgentsWanted(); w != nil {
			out := []string{}
			for _, a := range w.GetAgents() {
				out = append(out, a.GetAgent().String()+"@"+a.GetVersion())
			}
			return out
		}
	}
}

func wantWishes(t *testing.T, got []string, want ...string) {
	t.Helper()
	if want == nil {
		want = []string{}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("AgentsWanted %q, want %q", got, want)
	}
}

// report sends ReportAgents and checks its reply is ok.
func report(t *testing.T, c *workerclient.Client, agents ...*circusv1.AgentStatus) {
	t.Helper()
	r, err := c.ReportAgents(wait.Ctx(t), agents...)
	must(t, err)
	reply, err := r.Wait(wait.Ctx(t))
	must(t, err)
	wantOk(t, reply, false)
}

// wish records the user's wish for worker i's Claude Code as
// MachineService.SetWorkerAgent does, and applies it.
func (f *fixture) wish(i int, install bool, version string) {
	f.t.Helper()
	id := f.workers[i].Identity.ID
	_, err := f.st.SetWorkerAgentWish(f.t.Context(), id, store.AgentClaudeCode, install, version)
	must(f.t, err)
	must(f.t, f.svc.RefreshWorkerAgents(f.t.Context(), id))
}

const (
	notInstalled = circusv1.AgentStatus_STATE_NOT_INSTALLED
	installing   = circusv1.AgentStatus_STATE_INSTALLING
	installed    = circusv1.AgentStatus_STATE_INSTALLED
	uninstalling = circusv1.AgentStatus_STATE_UNINSTALLING
)

// TestAgentInstallOffers: a session waits as AGENT_NOT_INSTALLED while the
// only worker online lacks Claude Code; the user's wish reaches the worker
// at once, and its report of the finished install offers the session.
func TestAgentInstallOffers(t *testing.T) {
	f := newFixture(t)
	f.session("s1", -1)
	c := f.dial(0, "boot-a")
	ev := f.attachWith(c, &circusv1.Attach{MaxSessions: 2, Agents: []*circusv1.AgentStatus{workerclient.ClaudeCode(notInstalled, "", "2.0")}})
	wantWishes(t, wanted(t, ev))
	f.input("s1", "go", store.InputNormal)
	f.waitReason("s1", circusv1.QueueReason_QUEUE_REASON_AGENT_NOT_INSTALLED)
	noOffer(t, ev)

	f.wish(0, true, "2.0")
	wantWishes(t, wanted(t, ev), "AGENT_KIND_CLAUDE_CODE@2.0")
	// Progress and a version that is not the target run nothing.
	report(t, c, workerclient.ClaudeCode(installing, "", "2.0"))
	report(t, c, workerclient.ClaudeCode(installed, "1.0", "2.0"))
	f.settle()
	noOffer(t, ev)
	if r := f.reason("s1"); r != circusv1.QueueReason_QUEUE_REASON_AGENT_NOT_INSTALLED {
		t.Fatalf("queue reason %v while installing", r)
	}
	report(t, c, workerclient.ClaudeCode(installed, "2.0", "2.0"))
	offerTo(t, ev, "s1")
	acquire(t, c, "s1")
	if r := f.reason("s1"); r != 0 {
		t.Fatalf("queue reason %v after the install", r)
	}
}

// TestAgentUnwanted: once the user no longer wants the agent, no new
// session of it is offered to the worker, an outstanding Offer is
// withdrawn, AcquireLease refuses (it reads the wish in its transaction),
// and the worker gets the empty wish list; another worker still runs the
// sessions.
func TestAgentUnwanted(t *testing.T) {
	f := newFixture(t)
	c := f.dial(0, "boot-a")
	ev := f.attach(c, 4)
	// Reported installed without a wish: adopted, wanted at its version.
	wantWishes(t, wanted(t, ev), "AGENT_KIND_CLAUDE_CODE@cli")
	f.session("s1", 0)
	f.input("s1", "go", store.InputNormal)
	offerTo(t, ev, "s1")

	// The wish changes in the store only: the Offer stands in memory, but
	// AcquireLease reads the wish and refuses.
	_, err := f.st.SetWorkerAgentWish(t.Context(), f.workers[0].Identity.ID, store.AgentClaudeCode, false, "")
	must(t, err)
	wantRejected(t, call(t, c, workerclient.Acquire("s1", true)), circusv1.SessionReply_Rejected_CODE_NOT_OFFERED)
	must(t, f.svc.RefreshWorkerAgents(t.Context(), f.workers[0].Identity.ID))
	wantWishes(t, wanted(t, ev))
	f.waitReason("s1", circusv1.QueueReason_QUEUE_REASON_AGENT_NOT_INSTALLED)
	f.session("s2", -1)
	f.input("s2", "go", store.InputNormal)
	f.waitReason("s2", circusv1.QueueReason_QUEUE_REASON_AGENT_NOT_INSTALLED)
	noOffer(t, ev)

	// The worker uninstalls; installed again later, it runs nothing until
	// the user wants it again.
	report(t, c, workerclient.ClaudeCode(uninstalling, "cli", "cli"))
	report(t, c, workerclient.ClaudeCode(notInstalled, "", "cli"))
	report(t, c, workerclient.ClaudeInstalled()...)
	f.settle()
	noOffer(t, ev)

	// Another worker with Claude Code takes the unpinned session.
	c2 := f.dial(1, "boot-b")
	ev2 := f.attach(c2, 4)
	offerTo(t, ev2, "s2")
	acquire(t, c2, "s2")

	// Wanted again: the pinned session is offered.
	f.wish(0, true, "cli")
	wantWishes(t, wanted(t, ev), "AGENT_KIND_CLAUDE_CODE@cli")
	offerTo(t, ev, "s1")
	acquire(t, c, "s1")
}

// TestAgentOfferWithdrawn: an Offer to a worker whose agent stops running
// (uninstall started) is withdrawn and goes to a worker that runs it.
func TestAgentOfferWithdrawn(t *testing.T) {
	f := newFixture(t)
	c := f.dial(0, "boot-a")
	ev := f.attach(c, 4)
	f.session("s1", -1)
	f.input("s1", "go", store.InputNormal)
	offerTo(t, ev, "s1")
	report(t, c, workerclient.ClaudeCode(uninstalling, "cli", "cli"))
	wantRejected(t, call(t, c, workerclient.Acquire("s1", true)), circusv1.SessionReply_Rejected_CODE_NOT_OFFERED)
	c2 := f.dial(1, "boot-b")
	ev2 := f.attach(c2, 4)
	offerTo(t, ev2, "s1")
	acquire(t, c2, "s1")
}

// TestAgentsWantedEveryAttach: every Attach gets the worker's wishes first,
// a reconnect included, and only the worker's own connections get them.
func TestAgentsWantedEveryAttach(t *testing.T) {
	f := newFixture(t)
	f.wish(0, true, "cli")
	c := f.dial(0, "boot-a")
	ev := f.attachWith(c, &circusv1.Attach{MaxSessions: 1, Agents: []*circusv1.AgentStatus{workerclient.ClaudeCode(notInstalled, "", "cli")}})
	wantWishes(t, wanted(t, ev), "AGENT_KIND_CLAUDE_CODE@cli")
	other := f.dial(1, "boot-b")
	ev2 := f.attach(other, 1)
	wantWishes(t, wanted(t, ev2), "AGENT_KIND_CLAUDE_CODE@cli")
	f.wish(0, false, "")
	wantWishes(t, wanted(t, ev))
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if e, err := ev2.NextEvent(ctx); err == nil {
		t.Fatalf("another worker got %v", e)
	}
	// The same wish again is no change: nothing is pushed.
	f.wish(0, false, "")
	noEvent(t, ev)
	c2 := f.dial(0, "boot-a")
	ev3 := f.attach(c2, 1)
	wantWishes(t, wanted(t, ev3))
}

// TestReportAgentsRules: a report that breaks a field rule is Rejected
// INVALID and changes nothing; a valid one replaces the stored states.
func TestReportAgentsRules(t *testing.T) {
	f := newFixture(t)
	c := f.dial(0, "boot-a")
	f.attach(c, 1)
	for name, agents := range map[string][]*circusv1.AgentStatus{
		"twice":         append(workerclient.ClaudeInstalled(), workerclient.ClaudeInstalled()...),
		"unknown agent": {{Agent: 7, State: installed}},
		"no state":      {workerclient.ClaudeCode(0, "", "1")},
		"long target":   {workerclient.ClaudeCode(installed, "1", strings.Repeat("v", 65))},
	} {
		r, err := c.ReportAgents(wait.Ctx(t), agents...)
		must(t, err)
		reply, err := r.Wait(wait.Ctx(t))
		must(t, err)
		if reply.GetRejected().GetCode() != circusv1.SessionReply_Rejected_CODE_INVALID {
			t.Errorf("%s: %v", name, reply)
		}
	}
	got, err := f.st.ListWorkerAgents(t.Context(), f.workers[0].Identity.ID)
	must(t, err)
	if len(got.Agents) != 1 || !got.Agents[0].Runs() {
		t.Fatalf("after refused reports: %+v", got)
	}
	failed := &circusv1.AgentStatus{Agent: circusv1.AgentKind_AGENT_KIND_CLAUDE_CODE, State: circusv1.AgentStatus_STATE_FAILED,
		TargetVersion: "cli", Error: "digest mismatch"}
	report(t, c, failed)
	got, err = f.st.ListWorkerAgents(t.Context(), f.workers[0].Identity.ID)
	must(t, err)
	if a := got.Agents[0]; a.Status.State != store.AgentFailed || a.Status.Error != "digest mismatch" || a.Runs() {
		t.Fatalf("after a failed report: %+v", a)
	}
}
