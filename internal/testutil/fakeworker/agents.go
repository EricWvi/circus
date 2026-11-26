package fakeworker

import (
	"context"
	"errors"
	"time"

	pb "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/wire"
	"google.golang.org/protobuf/proto"
)

// AgentVersion is the Claude Code version the fake worker reports as its
// target (and installed, by default).
const AgentVersion = "fake"

// agentState is the fake worker's Claude Code; w.mu guards it.
type agentState struct {
	status *pb.AgentStatus
	// uninstalling: a not-wanted wish waits for the held sessions to end.
	uninstalling bool
}

// initialAgent is the Claude Code status the worker starts with:
// Options.ClaudeCode, INSTALLED at AgentVersion by default.
func initialAgent(o Options) *pb.AgentStatus {
	target := o.AgentTarget
	if target == "" {
		target = AgentVersion
	}
	st := &pb.AgentStatus{Agent: pb.AgentKind_AGENT_KIND_CLAUDE_CODE, State: o.ClaudeCode, TargetVersion: target}
	switch o.ClaudeCode {
	case pb.AgentStatus_STATE_UNSPECIFIED:
		st.State, st.InstalledVersion = pb.AgentStatus_STATE_INSTALLED, target
	case pb.AgentStatus_STATE_INSTALLED:
		st.InstalledVersion = target
	case pb.AgentStatus_STATE_OUTDATED, pb.AgentStatus_STATE_UNINSTALLING:
		st.InstalledVersion = "old"
	}
	return st
}

// attachAgents is Attach.agents: the current status; w.mu held.
func (w *worker) attachAgents() []*pb.AgentStatus {
	return []*pb.AgentStatus{proto.Clone(w.agent.status).(*pb.AgentStatus)}
}

// wanted reconciles Claude Code with the user's wishes, minimally: a wish
// for the target version installs it at once; no wish uninstalls it once
// no session is held. w.mu held.
func (w *worker) wanted(ev *pb.WorkerEvent_AgentsWanted) {
	version, want := "", false
	for _, a := range ev.GetAgents() {
		if a.GetAgent() == pb.AgentKind_AGENT_KIND_CLAUDE_CODE {
			version, want = a.GetVersion(), true
		}
	}
	st := w.agent.status
	switch {
	case want && version == st.GetTargetVersion() && st.GetState() != pb.AgentStatus_STATE_INSTALLED:
		w.agent.uninstalling = false
		w.setAgent(&pb.AgentStatus{Agent: st.Agent, State: pb.AgentStatus_STATE_INSTALLED, InstalledVersion: st.TargetVersion,
			TargetVersion: st.TargetVersion})
	case want:
		w.agent.uninstalling = false
	case st.GetInstalledVersion() != "" && !w.agent.uninstalling:
		w.agent.uninstalling = true
		w.agents.Go(w.uninstall)
	}
}

// uninstall waits until no session is held, then reports UNINSTALLING and
// NOT_INSTALLED, unless a wish for the agent came meanwhile.
func (w *worker) uninstall() {
	for {
		w.mu.Lock()
		if !w.agent.uninstalling || w.ctx.Err() != nil {
			w.mu.Unlock()
			return
		}
		if len(w.sessions) == 0 {
			st := w.agent.status
			w.agent.uninstalling = false
			w.setAgent(&pb.AgentStatus{Agent: st.Agent, State: pb.AgentStatus_STATE_UNINSTALLING, InstalledVersion: st.InstalledVersion,
				TargetVersion: st.TargetVersion})
			w.setAgent(&pb.AgentStatus{Agent: st.Agent, State: pb.AgentStatus_STATE_NOT_INSTALLED, TargetVersion: st.TargetVersion})
			w.mu.Unlock()
			return
		}
		w.mu.Unlock()
		if !w.sleep(w.ctx, retryDelay) {
			return
		}
	}
}

// setAgent records a new status and reports it (ReportAgents, in order);
// w.mu held.
func (w *worker) setAgent(st *pb.AgentStatus) {
	w.agent.status = st
	w.log.Info("agent status", "state", st.GetState().String(), "installed_version", st.GetInstalledVersion())
	report := proto.Clone(st).(*pb.AgentStatus)
	w.reports = append(w.reports, report)
	if len(w.reports) == 1 {
		w.agents.Go(w.sendReports)
	}
}

// sendReports sends the queued reports in order, each on the current
// connection (a report lost with a connection is carried by the next
// Attach anyway).
func (w *worker) sendReports() {
	for {
		w.mu.Lock()
		if len(w.reports) == 0 || w.ctx.Err() != nil {
			w.reports = nil
			w.mu.Unlock()
			return
		}
		st := w.reports[0]
		w.mu.Unlock()
		if err := w.report(st); err != nil && w.ctx.Err() == nil {
			w.log.Warn("agent report failed; the next Attach carries the state", "error", err)
		}
		w.mu.Lock()
		w.reports = w.reports[1:]
		w.mu.Unlock()
	}
}

func (w *worker) report(st *pb.AgentStatus) error {
	ctx, cancel := context.WithTimeout(w.ctx, opTimeout)
	defer cancel()
	for {
		l, err := w.connection(ctx)
		if err != nil {
			return err
		}
		r, err := l.c.ReportAgents(ctx, st)
		if err == nil {
			var reply *pb.SessionReply
			if reply, err = r.Wait(ctx); err == nil {
				if reply.GetOk() == nil {
					return errors.New("ReportAgents refused: " + reply.String())
				}
				return nil
			}
		}
		if !errors.Is(err, wire.ErrBusy) {
			return err
		}
		select {
		case <-time.After(retryDelay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
