package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
)

// AgentKind is a coding agent a worker can run sessions with
// (circusv1.AgentKind).
type AgentKind string

// AgentClaudeCode is Claude Code, the only agent so far.
const AgentClaudeCode AgentKind = "claude_code"

// AgentKinds lists every agent in circusv1.AgentKind order.
var AgentKinds = []AgentKind{AgentClaudeCode}

// Valid reports whether k is a known agent.
func (k AgentKind) Valid() bool { return slices.Contains(AgentKinds, k) }

// rank orders agents as circusv1.AgentKind does.
func (k AgentKind) rank() int { return slices.Index(AgentKinds, k) }

// AgentState is a worker's reported state of one agent
// (circusv1.AgentStatus.State).
type AgentState string

const (
	AgentNotInstalled AgentState = "not_installed"
	AgentInstalling   AgentState = "installing"
	AgentInstalled    AgentState = "installed"
	AgentOutdated     AgentState = "outdated"
	AgentFailed       AgentState = "failed"
	AgentUninstalling AgentState = "uninstalling"
)

// Valid reports whether s is a known state.
func (s AgentState) Valid() bool {
	switch s {
	case AgentNotInstalled, AgentInstalling, AgentInstalled, AgentOutdated, AgentFailed, AgentUninstalling:
		return true
	}
	return false
}

// Bounds of an agent report.
const (
	// MaxWorkerAgents bounds the agents of one report.
	MaxWorkerAgents = 16
	// MaxAgentVersionBytes bounds a version.
	MaxAgentVersionBytes = 64
	// MaxAgentErrorBytes bounds a FAILED status's error.
	MaxAgentErrorBytes = 1024
)

// AgentStatus is what a worker reports about one agent.
type AgentStatus struct {
	Agent            AgentKind
	State            AgentState
	InstalledVersion string
	TargetVersion    string
	Error            string
	DownloadedBytes  int64
	TotalBytes       int64
}

// Runs reports whether the status lets sessions of the agent run: the
// agent is installed at the worker's target version.
func (s AgentStatus) Runs() bool {
	return s.State == AgentInstalled && s.InstalledVersion != "" && s.InstalledVersion == s.TargetVersion
}

// CheckAgentReport validates a worker's complete report: at most
// MaxWorkerAgents statuses, one per known agent, each with a known state
// and bounded fields (ErrInvalid).
func CheckAgentReport(report []AgentStatus) error {
	if len(report) > MaxWorkerAgents {
		return fmt.Errorf("%w: at most %d agents", ErrInvalid, MaxWorkerAgents)
	}
	seen := make(map[AgentKind]bool, len(report))
	for _, s := range report {
		switch {
		case !s.Agent.Valid():
			return fmt.Errorf("%w: unknown agent %q", ErrInvalid, s.Agent)
		case seen[s.Agent]:
			return fmt.Errorf("%w: agent %q reported twice", ErrInvalid, s.Agent)
		case !s.State.Valid():
			return fmt.Errorf("%w: unknown state %q of agent %q", ErrInvalid, s.State, s.Agent)
		case len(s.InstalledVersion) > MaxAgentVersionBytes || len(s.TargetVersion) > MaxAgentVersionBytes:
			return fmt.Errorf("%w: an agent version is longer than %d bytes", ErrInvalid, MaxAgentVersionBytes)
		case len(s.Error) > MaxAgentErrorBytes:
			return fmt.Errorf("%w: an agent error is longer than %d bytes", ErrInvalid, MaxAgentErrorBytes)
		case s.DownloadedBytes < 0 || s.TotalBytes < 0:
			return fmt.Errorf("%w: negative byte counts", ErrInvalid)
		}
		seen[s.Agent] = true
	}
	return nil
}

// WorkerAgent is one agent of a worker: the worker's last report and the
// user's wish.
type WorkerAgent struct {
	WorkerID string
	Agent    AgentKind
	// Status is the worker's last report of the agent; its State is empty
	// when the worker never reported it (or its last report left it out).
	Status AgentStatus
	// ReportedAt is when Status was reported; 0 with an empty State.
	ReportedAt int64
	// Wished: a wish is recorded (by the user, or adopted from a report
	// of the agent on disk); Wanted and WantedVersion are that wish.
	// WantedVersion is set exactly when Wanted.
	Wished        bool
	Wanted        bool
	WantedVersion string
	// WantedAttempt counts the user's install requests for the agent
	// (SetWorkerAgentWish with wanted); 0 for an adopted wish.
	WantedAttempt int64
}

// Runs reports whether the worker may be offered a session of the agent:
// it reported the agent installed at its target version and the agent is
// still wanted.
func (a WorkerAgent) Runs() bool { return a.Wanted && a.Status.Runs() }

// WorkerAgents is a worker's agents, ordered by AgentKind.
type WorkerAgents struct {
	WorkerID string
	// Rev counts the transactions that wrote the worker's agents (reports
	// and changed wishes), so of two reads the one with the higher Rev is
	// the newer; 0 before any.
	Rev int64
	// ReportedAt is when the worker last reported its agents; 0 when it
	// never has.
	ReportedAt int64
	Agents     []WorkerAgent
}

// Get returns the worker's entry for agent k.
func (w WorkerAgents) Get(k AgentKind) (WorkerAgent, bool) {
	for _, a := range w.Agents {
		if a.Agent == k {
			return a, true
		}
	}
	return WorkerAgent{}, false
}

// SortWorkerAgents orders agents by worker id, then AgentKind.
func SortWorkerAgents(agents []WorkerAgent) {
	slices.SortFunc(agents, func(a, b WorkerAgent) int {
		if a.WorkerID != b.WorkerID {
			if a.WorkerID < b.WorkerID {
				return -1
			}
			return 1
		}
		return a.Agent.rank() - b.Agent.rank()
	})
}

// AgentReporter is who sends a report: the worker's boot and connection,
// and whether the report is the connection's Attach. ConnStart names the
// control plane's start that made the connection and ConnSeq numbers the
// connection within it, taken before its Welcome was sent, so connections
// of one start are ordered as the worker opened them.
type AgentReporter struct {
	BootID, ConnID string
	ConnStart      string
	ConnSeq        int64
	Attach         bool
}

// ErrStaleReport refuses a report of a boot that is not the worker's
// current one, or of a connection whose Attach is not the latest.
var ErrStaleReport = errors.New("store: agent report from a replaced boot or connection")

// AgentOps stores the agents of workers: what each worker last reported
// and what its user wants installed.
type AgentOps interface {
	// ReportWorkerAgents replaces the worker's reported agent states with
	// report (CheckAgentReport) at time at, which also becomes the
	// worker's ReportedAt, and returns the worker's agents. The reporter's
	// boot must be the worker's current one, and a report that is not an
	// Attach must come from the connection that claimed the reports. An
	// Attach claims them unless the connection holding the claim was made
	// later by the same control-plane start (an older connection's Attach
	// processed late); otherwise ErrStaleReport and nothing is written. An agent the
	// report leaves out loses its reported state, and its entry goes when
	// it is not wanted either. An agent reported installed or outdated
	// without a recorded wish gets one: wanted at its installed version, so
	// its files are kept and the user decides (an agent installed before
	// the control plane knew of it).
	// ErrNotFound for an unknown worker.
	ReportWorkerAgents(ctx context.Context, workerID string, from AgentReporter, report []AgentStatus, at int64) (WorkerAgents, error)
	// ListWorkerAgents returns the worker's agents. ErrNotFound for an
	// unknown worker.
	ListWorkerAgents(ctx context.Context, workerID string) (WorkerAgents, error)
	// ListUserWorkerAgents returns the agents of every live worker of the
	// user, ordered by worker id, then AgentKind.
	ListUserWorkerAgents(ctx context.Context, userID string) ([]WorkerAgent, error)
	// SetWorkerAgentWish records the user's wish for the worker's agent:
	// wanted at version (required), or not wanted (version must be
	// empty). Wanting it is a new install request: the agent's
	// WantedAttempt grows by one, so it always changes. It reports whether
	// the stored wish changed. ErrNotFound for an unknown worker.
	SetWorkerAgentWish(ctx context.Context, workerID string, agent AgentKind, wanted bool, version string) (changed bool, err error)
	// HasAgentLease reports whether the worker holds a live lease (expiring
	// after now) of a session that runs agent.
	HasAgentLease(ctx context.Context, workerID string, agent AgentKind, now int64) (bool, error)
}
