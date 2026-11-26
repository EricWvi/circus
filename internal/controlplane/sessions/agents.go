package sessions

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strings"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/store"
)

// Agents a worker installs on request.
//
// Each worker reports the state of every agent it supports at Attach and
// whenever one changes (ReportAgents); the store keeps that report and the
// user's wish per (worker, agent). This process keeps each worker's agents
// as last read from the store (agentView) and decides with it which
// connections may be offered a session: only those of a worker that reported
// the session's agent installed at its target version while the user still
// wants it (store.WorkerAgent.Runs). AcquireLease checks the same in its
// transaction, so a session is never leased to a worker whose agent the user
// asked to uninstall meanwhile. Every connection of a worker gets the
// complete set of wishes (AgentsWanted) after its Attach and whenever it
// changes.
//
// Every transaction that writes a worker's agents (a report, a changed
// wish) raises the worker's revision (store.WorkerAgents.Rev), and every
// read returns it. Writers apply what they read back, and AcquireLease what
// it read when it refused for the agent; a view is replaced only by one of
// a higher revision. The views therefore follow the commit order, whatever
// order the reads finish in. A report counts only from the worker's
// current boot and from the connection whose Attach reported last; the
// store checks both inside the write.

// agentView is a worker's agents as read from the store.
type agentView struct {
	agents store.WorkerAgents
}

// Bounds of the RefreshWorkerAgents retries: the first after
// agentRetryFirst, then retryDelay of the one before, until one succeeds,
// the worker is gone or the service closes.
const (
	agentRetryFirst = 500 * time.Millisecond
	agentRetryMax   = 30 * time.Second
)

// retryDelay is the wait after a retry that waited d: twice as long, at
// most agentRetryMax.
func retryDelay(d time.Duration) time.Duration { return min(2*d, agentRetryMax) }

// AgentKindFromProto maps a wire agent to the store's; false for
// AGENT_KIND_UNSPECIFIED and unknown values.
func AgentKindFromProto(k circusv1.AgentKind) (store.AgentKind, bool) {
	switch k {
	case circusv1.AgentKind_AGENT_KIND_CLAUDE_CODE:
		return store.AgentClaudeCode, true
	}
	return "", false
}

// AgentKindProto maps a store agent to the wire's.
func AgentKindProto(k store.AgentKind) circusv1.AgentKind {
	switch k {
	case store.AgentClaudeCode:
		return circusv1.AgentKind_AGENT_KIND_CLAUDE_CODE
	}
	return circusv1.AgentKind_AGENT_KIND_UNSPECIFIED
}

var agentStates = map[circusv1.AgentStatus_State]store.AgentState{
	circusv1.AgentStatus_STATE_NOT_INSTALLED: store.AgentNotInstalled,
	circusv1.AgentStatus_STATE_INSTALLING:    store.AgentInstalling,
	circusv1.AgentStatus_STATE_INSTALLED:     store.AgentInstalled,
	circusv1.AgentStatus_STATE_OUTDATED:      store.AgentOutdated,
	circusv1.AgentStatus_STATE_FAILED:        store.AgentFailed,
	circusv1.AgentStatus_STATE_UNINSTALLING:  store.AgentUninstalling,
}

// agentReport converts and validates a worker's complete report.
func agentReport(list []*circusv1.AgentStatus) ([]store.AgentStatus, error) {
	if len(list) > store.MaxWorkerAgents {
		return nil, fmt.Errorf("at most %d agents", store.MaxWorkerAgents)
	}
	out := make([]store.AgentStatus, 0, len(list))
	for _, a := range list {
		kind, ok := AgentKindFromProto(a.GetAgent())
		if !ok {
			return nil, fmt.Errorf("unknown agent %d", a.GetAgent())
		}
		state, ok := agentStates[a.GetState()]
		if !ok {
			return nil, fmt.Errorf("unknown state %d of agent %s", a.GetState(), kind)
		}
		if a.GetDownloadedBytes() > math.MaxInt64 || a.GetTotalBytes() > math.MaxInt64 {
			return nil, fmt.Errorf("byte counts out of range")
		}
		out = append(out, store.AgentStatus{Agent: kind, State: state, InstalledVersion: a.GetInstalledVersion(),
			TargetVersion: a.GetTargetVersion(), Error: a.GetError(),
			DownloadedBytes: int64(a.GetDownloadedBytes()), TotalBytes: int64(a.GetTotalBytes())})
	}
	if err := store.CheckAgentReport(out); err != nil {
		return nil, err
	}
	return out, nil
}

// AgentStatusProto is the wire form of a stored status (state UNSPECIFIED
// when never reported).
func AgentStatusProto(s store.AgentStatus) *circusv1.AgentStatus {
	out := &circusv1.AgentStatus{Agent: AgentKindProto(s.Agent), InstalledVersion: s.InstalledVersion, TargetVersion: s.TargetVersion,
		Error: s.Error, DownloadedBytes: uint64(max(s.DownloadedBytes, 0)), TotalBytes: uint64(max(s.TotalBytes, 0))}
	for k, v := range agentStates {
		if v == s.State {
			out.State = k
		}
	}
	return out
}

// WorkerAgentProto is the wire form of one agent of a worker.
func WorkerAgentProto(a store.WorkerAgent) *circusv1.WorkerAgent {
	return &circusv1.WorkerAgent{Status: AgentStatusProto(a.Status), Wanted: a.Wanted, WantedVersion: a.WantedVersion,
		ReportedAtUnixMs: a.ReportedAt}
}

// runsAgent reports whether worker may be offered a session of agent;
// s.mu held.
func (s *Service) runsAgent(worker string, agent store.AgentKind) bool {
	a, ok := s.agents[worker].agents.Get(agent)
	return ok && a.Runs()
}

// RefreshWorkerAgents reads the worker's agents from the store after a
// change of the user's wishes (MachineService.SetWorkerAgent) and applies
// them: the worker's connections get the new AgentsWanted at once (every
// install request changes it: its attempt grows), and its sessions are
// offered or withdrawn as its agents now allow.
//
// When the store read fails, the read is retried in the background with
// backoff (agentRetryFirst, then retryDelay) until it succeeds, the worker
// is gone or the service closes, and the error is returned: the wish is
// stored, and the view must not stay behind it.
func (s *Service) RefreshWorkerAgents(ctx context.Context, workerID string) error {
	err := s.refreshAgents(ctx, workerID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.retryAgents(workerID)
	}
	return err
}

func (s *Service) refreshAgents(ctx context.Context, workerID string) error {
	tctx, cancel := storeCtx(ctx)
	wa, err := s.cfg.Store.ListWorkerAgents(tctx, workerID)
	cancel()
	if err != nil {
		return err
	}
	s.applyAgents(workerID, wa)
	return nil
}

// retryAgents schedules background retries of refreshAgents for the
// worker, one retry loop per worker at a time.
func (s *Service) retryAgents(workerID string) {
	s.mu.Lock()
	defer s.unlock()
	if s.agentRetries[workerID] || s.bg == nil || s.bg.Err() != nil {
		return
	}
	s.agentRetries[workerID] = true
	ctx := s.bg
	s.loops.Go(func() {
		delay := agentRetryFirst
		for {
			select {
			case <-ctx.Done():
				return
			case <-s.clk.After(delay):
			}
			err := s.refreshAgents(ctx, workerID)
			next := retryDelay(delay)
			s.mu.Lock()
			switch {
			case err == nil:
				delete(s.agentRetries, workerID)
				s.later(slog.LevelInfo, "worker agents read again", slog.String("worker_id", workerID))
				s.unlock()
				return
			case errors.Is(err, store.ErrNotFound):
				// The worker is gone: nothing to read any more.
				delete(s.agentRetries, workerID)
				s.later(slog.LevelInfo, "worker gone; its agents are not read again", slog.String("worker_id", workerID))
				s.unlock()
				return
			}
			s.later(slog.LevelWarn, "reading worker agents failed; retrying", slog.String("worker_id", workerID),
				slog.Any("error", err), slog.Duration("retry_in", next))
			s.unlock()
			delay = next
		}
	})
}

// reportAgents stores the report of connection c (its Attach with attach)
// at now and applies what the store then holds. ErrStaleReport: c's boot
// is not the worker's current one, or a later connection (by creation)
// claimed the worker's reports with its Attach.
func (s *Service) reportAgents(ctx context.Context, c *Conn, report []store.AgentStatus, attach bool) error {
	tctx, cancel := storeCtx(ctx)
	from := store.AgentReporter{BootID: c.bootID, ConnID: c.id, ConnStart: s.cfg.InstanceID + "." + s.nonce, ConnSeq: int64(c.seq),
		Attach: attach}
	wa, err := s.cfg.Store.ReportWorkerAgents(tctx, c.workerID, from, report, s.nowMs())
	cancel()
	if err != nil {
		return err
	}
	s.applyAgents(c.workerID, wa)
	return nil
}

// applyAgents replaces the worker's view with wa unless the view already
// has wa's revision or a higher one. A change of the wishes is pushed to
// the worker's attached connections; a
// change of what the worker may run
// withdraws its Offers that no longer fit, offers the user's queued sessions
// again and re-derives their queue reasons.
func (s *Service) applyAgents(workerID string, wa store.WorkerAgents) {
	s.mu.Lock()
	defer s.unlock()
	old, had := s.agents[workerID]
	if had && old.agents.Rev >= wa.Rev {
		return
	}
	s.agents[workerID] = agentView{agents: wa}
	wishes := !slices.Equal(wantedList(old.agents), wantedList(wa))
	gained, lost := false, false
	for _, k := range store.AgentKinds {
		a, _ := old.agents.Get(k)
		b, _ := wa.Get(k)
		if a.Runs() != b.Runs() {
			gained, lost = gained || b.Runs(), lost || a.Runs()
		}
	}
	if wishes {
		s.pushWanted(workerID)
	}
	if !gained && !lost {
		return
	}
	users := make(map[string]struct{})
	for _, c := range s.conns {
		if c.workerID == workerID && !c.closed {
			users[c.userID] = struct{}{}
		}
	}
	s.later(slog.LevelInfo, "worker agents changed", slog.String("worker_id", workerID), slog.String("runs", runsList(wa)))
	for id, e := range s.entries {
		if lost && e.offered != nil && e.offered.workerID == workerID {
			s.setOffer(id, e, nil, "agent_unavailable")
			s.queueWake(id, 0)
		}
		if _, ok := users[e.user]; ok && e.queue != 0 {
			s.queueWake(id, 0)
		}
	}
	for u := range users {
		s.rescan[u] = struct{}{}
	}
	s.signal()
}

// pushWanted queues the worker's AgentsWanted on its attached connections
// of its current boot; s.mu held.
func (s *Service) pushWanted(workerID string) {
	for _, c := range s.conns {
		if c.workerID == workerID && !c.closed && c.attached && !s.stale(c) {
			c.sendAgentsWanted()
		}
	}
}

// wantedList is the AgentsWanted content of a worker's agents.
func wantedList(wa store.WorkerAgents) []string {
	var out []string
	for _, a := range wa.Agents {
		if a.Wanted {
			out = append(out, fmt.Sprintf("%s@%s#%d", a.Agent, a.WantedVersion, a.WantedAttempt))
		}
	}
	return out
}

// runsList names the agents the worker runs, for logs.
func runsList(wa store.WorkerAgents) string {
	var out []string
	for _, a := range wa.Agents {
		if a.Runs() {
			out = append(out, string(a.Agent))
		}
	}
	return strings.Join(out, ",")
}

// agentsWantedEvent builds the worker's AgentsWanted from its view; s.mu
// held.
func (s *Service) agentsWantedEvent(workerID string) *circusv1.WorkerEvent {
	w := &circusv1.WorkerEvent_AgentsWanted{}
	for _, a := range s.agents[workerID].agents.Agents {
		if a.Wanted {
			w.Agents = append(w.Agents, &circusv1.WorkerEvent_AgentsWanted_Wanted{Agent: AgentKindProto(a.Agent), Version: a.WantedVersion,
				Attempt: uint64(max(a.WantedAttempt, 0))})
		}
	}
	return &circusv1.WorkerEvent{Event: &circusv1.WorkerEvent_AgentsWanted_{AgentsWanted: w}}
}

// sendAgentsWanted queues the worker's current AgentsWanted on c; s.mu
// held. Before Attach it waits for the sender Attach starts.
func (c *Conn) sendAgentsWanted() {
	if c.closed {
		return
	}
	c.agentsDue = true
	select {
	case c.kick <- struct{}{}:
	default:
	}
}

// ReportAgents is the worker's ReportAgents: its agents' states replace
// the ones stored. A report that breaks a field rule is Rejected INVALID;
// so is one of a replaced boot (its process is gone) or of a connection
// the worker has attached again since (a late report there is older than
// the new Attach's), both checked inside the write.
func (c *Conn) ReportAgents(ctx context.Context, r *circusv1.ReportAgents) *circusv1.SessionReply {
	s := c.s
	if c.bootID == "" {
		return invalidReply("ReportAgents needs a worker boot")
	}
	report, err := agentReport(r.GetAgents())
	if err != nil {
		return invalidReply(err.Error())
	}
	if err := s.reportAgents(ctx, c, report, false); err != nil {
		if errors.Is(err, store.ErrStaleReport) {
			return invalidReply("not the worker's current boot or latest attached connection")
		}
		return s.failure(ctx, "report_agents", err)
	}
	if s.debug(ctx) {
		s.log.LogAttrs(ctx, slog.LevelDebug, "worker agents reported", slog.String("worker_id", c.workerID), slog.String("conn_id", c.id),
			slog.String("agents", reportSummary(report)))
	}
	return ok(false)
}

// reportSummary is a report for logs: agent=state[@version], ...
func reportSummary(report []store.AgentStatus) string {
	parts := make([]string, 0, len(report))
	for _, a := range report {
		p := string(a.Agent) + "=" + string(a.State)
		if a.InstalledVersion != "" {
			p += "@" + a.InstalledVersion
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, ",")
}
