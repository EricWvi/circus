package webapi_test

import (
	"slices"
	"testing"

	"connectrpc.com/connect"
	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/store"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
)

const claude = circusv1.AgentKind_AGENT_KIND_CLAUDE_CODE

func (h *harness) reportAgents(t *testing.T, worker string, report ...store.AgentStatus) {
	t.Helper()
	if _, err := h.st.SetConnected(t.Context(), store.KindWorker, worker, "boot", h.clock.Load()); err != nil {
		t.Fatal(err)
	}
	from := store.AgentReporter{BootID: "boot", ConnID: "conn", Attach: true}
	if _, err := h.st.ReportWorkerAgents(t.Context(), worker, from, report, h.clock.Load()); err != nil {
		t.Fatal(err)
	}
}

func claudeStatus(state store.AgentState, installed, target string) store.AgentStatus {
	return store.AgentStatus{Agent: store.AgentClaudeCode, State: state, InstalledVersion: installed, TargetVersion: target}
}

func (c *client) setAgent(t *testing.T, worker string, agent circusv1.AgentKind, install bool) (*circusv1.Worker, error) {
	t.Helper()
	res, err := c.machines.SetWorkerAgent(t.Context(), connect.NewRequest(&circusv1.SetWorkerAgentRequest{WorkerId: worker, Agent: agent, Install: install}))
	if err != nil {
		return nil, err
	}
	return res.Msg.GetWorker(), nil
}

// TestSetWorkerAgent: the owner's wish is stored and pushed (a changed
// wish, or an install to retry); install wants the reported target version; the refusals
// follow the proto: unknown worker or another user's (NOT_FOUND), no
// agent or an unsupported one (INVALID_ARGUMENT), install before any
// report (FAILED_PRECONDITION).
func TestSetWorkerAgent(t *testing.T) {
	h, owner := newHarness(t, withWorkers)
	h.credential(t, store.KindWorker, h.owner.ID, "w1", "builder")

	_, err := owner.setAgent(t, "w1", claude, true)
	wantCode(t, err, connect.CodeFailedPrecondition)
	_, err = owner.setAgent(t, "w1", circusv1.AgentKind_AGENT_KIND_UNSPECIFIED, true)
	wantField(t, err, connect.CodeInvalidArgument, "agent")
	_, err = owner.setAgent(t, "w1", 99, true)
	wantField(t, err, connect.CodeInvalidArgument, "agent")
	_, err = owner.setAgent(t, "", claude, true)
	wantField(t, err, connect.CodeInvalidArgument, "worker_id")
	_, err = owner.setAgent(t, "missing", claude, true)
	wantCode(t, err, connect.CodeNotFound)
	stranger := h.signIn(t, "stranger", "stranger@example.com")
	h.reportAgents(t, "w1", claudeStatus(store.AgentNotInstalled, "", "2.0"))
	_, err = stranger.setAgent(t, "w1", claude, true)
	wantCode(t, err, connect.CodeNotFound)
	_, err = h.browser(t).setAgent(t, "w1", claude, true)
	wantCode(t, err, connect.CodeUnauthenticated)
	if got := h.sched.takeRefreshed(); len(got) != 0 {
		t.Fatalf("refusals pushed %v", got)
	}

	w, err := owner.setAgent(t, "w1", claude, true)
	if err != nil {
		t.Fatal(err)
	}
	want := &circusv1.WorkerAgent{Status: &circusv1.AgentStatus{Agent: claude, State: circusv1.AgentStatus_STATE_NOT_INSTALLED, TargetVersion: "2.0"},
		Wanted: true, WantedVersion: "2.0", ReportedAtUnixMs: h.clock.Load()}
	if w.GetName() != "builder" || len(w.GetAgents()) != 1 || !proto.Equal(w.GetAgents()[0], want) {
		t.Fatalf("worker %v\nwant agent %v", w, want)
	}
	if got := h.sched.takeRefreshed(); !slices.Equal(got, []string{"w1"}) {
		t.Fatalf("pushed %v", got)
	}
	// Every install request counts as a new attempt and is pushed,
	// whatever the state: the worker retries a FAILED install for a newer
	// attempt and ignores it while installing or installed.
	attempts := []int64{1}
	for _, st := range []store.AgentStatus{
		{Agent: store.AgentClaudeCode, State: store.AgentFailed, TargetVersion: "2.0", Error: "digest mismatch"},
		claudeStatus(store.AgentInstalling, "", "2.0"),
		claudeStatus(store.AgentInstalled, "2.0", "2.0"),
	} {
		h.reportAgents(t, "w1", st)
		if _, err := owner.setAgent(t, "w1", claude, true); err != nil {
			t.Fatal(err)
		}
		if got := h.sched.takeRefreshed(); !slices.Equal(got, []string{"w1"}) {
			t.Fatalf("install request with %s pushed %v", st.State, got)
		}
		wa, err := h.st.ListWorkerAgents(t.Context(), "w1")
		if err != nil {
			t.Fatal(err)
		}
		attempts = append(attempts, wa.Agents[0].WantedAttempt)
	}
	if !slices.Equal(attempts, []int64{1, 2, 3, 4}) {
		t.Fatalf("attempts %v", attempts)
	}
	list, err := owner.machines.ListWorkers(t.Context(), connect.NewRequest(&circusv1.ListWorkersRequest{}))
	if err != nil || len(list.Msg.GetWorkers()) != 1 || len(list.Msg.GetWorkers()[0].GetAgents()) != 1 {
		t.Fatalf("workers %v, %v", list, err)
	}
	if a := list.Msg.GetWorkers()[0].GetAgents()[0]; a.GetStatus().GetState() != circusv1.AgentStatus_STATE_INSTALLED || !a.GetWanted() {
		t.Fatalf("listed agent %v", a)
	}

	// The worker was upgraded: an install wants its new target.
	h.reportAgents(t, "w1", claudeStatus(store.AgentOutdated, "2.0", "3.0"))
	w, err = owner.setAgent(t, "w1", claude, true)
	if err != nil || w.GetAgents()[0].GetWantedVersion() != "3.0" {
		t.Fatalf("update = %v, %v", w, err)
	}
	if got := h.sched.takeRefreshed(); !slices.Equal(got, []string{"w1"}) {
		t.Fatalf("update pushed %v", got)
	}

	// A worker that reports no such agent does not support it.
	h.reportAgents(t, "w1")
	_, err = owner.setAgent(t, "w1", claude, true)
	wantField(t, err, connect.CodeInvalidArgument, "agent")
	// Nor can it be uninstalled there (w1 still wants it, from before).
	h.credential(t, store.KindWorker, h.owner.ID, "w3", "bare")
	h.reportAgents(t, "w3")
	_, err = owner.setAgent(t, "w3", claude, false)
	wantField(t, err, connect.CodeInvalidArgument, "agent")
	if got := h.sched.takeRefreshed(); len(got) != 0 {
		t.Fatalf("refusals pushed %v", got)
	}
}

// TestUninstallWorkerAgent: uninstalling is refused while the worker holds
// a live lease of a session running the agent, and allowed once it holds
// none; the worker gets the empty wish list.
func TestUninstallWorkerAgent(t *testing.T) {
	h, owner := newHarness(t, withWorkers)
	_, sess := sessionSetup(t, h, owner)
	if sess.GetConfig().GetAgent() != claude {
		t.Fatalf("session agent %v", sess.GetConfig().GetAgent())
	}
	f := h.lease(t, sess.GetSessionId())
	h.reportAgents(t, "w1", claudeStatus(store.AgentInstalled, "2.0", "2.0"))
	_, err := owner.setAgent(t, "w1", claude, false)
	wantCode(t, err, connect.CodeFailedPrecondition)
	if got := h.sched.takeRefreshed(); len(got) != 0 {
		t.Fatalf("a refusal pushed %v", got)
	}
	if _, err := h.st.ReleaseLease(t.Context(), f); err != nil {
		t.Fatal(err)
	}
	w, err := owner.setAgent(t, "w1", claude, false)
	if err != nil {
		t.Fatal(err)
	}
	if a := w.GetAgents()[0]; a.GetWanted() || a.GetWantedVersion() != "" || a.GetStatus().GetState() != circusv1.AgentStatus_STATE_INSTALLED {
		t.Fatalf("after uninstall: %v", a)
	}
	if got := h.sched.takeRefreshed(); !slices.Equal(got, []string{"w1"}) {
		t.Fatalf("pushed %v", got)
	}
	// Uninstalling before the worker ever reported only records the wish.
	h.credential(t, store.KindWorker, h.owner.ID, "w2", "fresh")
	if _, err := owner.setAgent(t, "w2", claude, false); err != nil {
		t.Fatal(err)
	}
}

// TestCreateSessionAgent: a session runs Claude Code when the request names
// no agent; an unknown agent is refused, and a replay must name the same
// agent.
func TestCreateSessionAgent(t *testing.T) {
	h, super := newHarness(t)
	h.device(t, "d1", "box", "linux")
	p := super.mustCreate(t, "Project", loc("box", "/work"))
	id := uuid.NewString()
	s, err := super.createSession(t, &circusv1.CreateSessionRequest{ClientRequestId: id, ProjectId: p.ProjectId,
		Config: &circusv1.SessionConfig{Agent: claude}})
	if err != nil || s.GetConfig().GetAgent() != claude {
		t.Fatalf("session %v, %v", s, err)
	}
	stored, err := h.st.GetSession(t.Context(), s.GetSessionId(), h.clock.Load())
	if err != nil || stored.Agent != store.AgentClaudeCode {
		t.Fatalf("stored %+v, %v", stored, err)
	}
	// The replay without an agent means Claude Code too.
	if _, err := super.createSession(t, &circusv1.CreateSessionRequest{ClientRequestId: id, ProjectId: p.ProjectId}); err != nil {
		t.Fatal(err)
	}
	_, err = super.createSession(t, &circusv1.CreateSessionRequest{ClientRequestId: uuid.NewString(), ProjectId: p.ProjectId,
		Config: &circusv1.SessionConfig{Agent: 42}})
	wantField(t, err, connect.CodeInvalidArgument, "config.agent")
}
