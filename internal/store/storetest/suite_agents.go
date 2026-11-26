package storetest

import (
	"context"
	"strings"
	"testing"

	"github.com/EricWvi/circus/internal/store"
)

// reporter is the Attach of worker w's connection "c-"+w on its boot
// "b-"+w (connected: see connectWorker).
func reporter(w string) store.AgentReporter {
	return store.AgentReporter{BootID: "b-" + w, ConnID: "c-" + w, Attach: true}
}

// connectWorker makes "b-"+w worker w's current boot.
func connectWorker(t testing.TB, ops store.Ops, w string) {
	t.Helper()
	_, err := ops.SetConnected(context.Background(), store.KindWorker, w, "b-"+w, t0)
	must(t, err)
}

func claudeStatus(state store.AgentState, installed, target string) store.AgentStatus {
	return store.AgentStatus{Agent: store.AgentClaudeCode, State: state, InstalledVersion: installed, TargetVersion: target}
}

// testWorkerAgents: a report replaces the worker's reported states and
// adopts an agent on disk without a wish; wishes are kept across reports,
// changed only when they differ, and listed per worker and per user.
func testWorkerAgents(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	addWorker(t, s, "w2", "u1")
	connectWorker(t, s, "w1")
	connectWorker(t, s, "w2")

	got, err := s.ListWorkerAgents(ctx, "w1")
	must(t, err)
	mustEqual(t, "never reported", got, store.WorkerAgents{WorkerID: "w1"})
	mustIs(t, errOf(s.ListWorkerAgents(ctx, "nope")), store.ErrNotFound)
	mustIs(t, errOf(s.ReportWorkerAgents(ctx, "nope", reporter("nope"), nil, t0)), store.ErrNotFound)

	// Not installed: reported, no wish.
	got, err = s.ReportWorkerAgents(ctx, "w1", reporter("w1"), []store.AgentStatus{claudeStatus(store.AgentNotInstalled, "", "2.0")}, t0)
	must(t, err)
	mustEqual(t, "not installed", got, store.WorkerAgents{WorkerID: "w1", Rev: 1, ReportedAt: t0, Agents: []store.WorkerAgent{{
		WorkerID: "w1", Agent: store.AgentClaudeCode, Status: claudeStatus(store.AgentNotInstalled, "", "2.0"), ReportedAt: t0,
	}}})
	mustEqual(t, "does not run", got.Agents[0].Runs(), false)

	// The user wants it; the wish survives reports.
	changed, err := s.SetWorkerAgentWish(ctx, "w1", store.AgentClaudeCode, true, "2.0")
	must(t, err)
	mustEqual(t, "wish changed", changed, true)
	// Wanting it again is a new install request: the attempt grows.
	changed, err = s.SetWorkerAgentWish(ctx, "w1", store.AgentClaudeCode, true, "2.0")
	must(t, err)
	mustEqual(t, "install requested again", changed, true)
	progress := claudeStatus(store.AgentInstalling, "", "2.0")
	progress.DownloadedBytes, progress.TotalBytes = 10, 100
	got, err = s.ReportWorkerAgents(ctx, "w1", reporter("w1"), []store.AgentStatus{progress}, t0+1)
	must(t, err)
	mustEqual(t, "installing", got.Agents[0], store.WorkerAgent{WorkerID: "w1", Agent: store.AgentClaudeCode, Status: progress,
		ReportedAt: t0 + 1, Wished: true, Wanted: true, WantedVersion: "2.0", WantedAttempt: 2})
	got, err = s.ReportWorkerAgents(ctx, "w1", reporter("w1"), []store.AgentStatus{claudeStatus(store.AgentInstalled, "2.0", "2.0")}, t0+2)
	must(t, err)
	mustEqual(t, "installed runs", got.Agents[0].Runs(), true)

	// Not wanted any more: it stops running at once, whatever is on disk.
	changed, err = s.SetWorkerAgentWish(ctx, "w1", store.AgentClaudeCode, false, "")
	must(t, err)
	mustEqual(t, "unwanted changed", changed, true)
	got, err = s.ListWorkerAgents(ctx, "w1")
	must(t, err)
	mustEqual(t, "unwanted", []any{got.Agents[0].Wished, got.Agents[0].Wanted, got.Agents[0].WantedVersion, got.Agents[0].Runs()},
		[]any{true, false, "", false})
	// A report on disk does not override a recorded wish.
	got, err = s.ReportWorkerAgents(ctx, "w1", reporter("w1"), []store.AgentStatus{claudeStatus(store.AgentInstalled, "2.0", "2.0")}, t0+3)
	must(t, err)
	mustEqual(t, "wish kept", got.Agents[0].Wanted, false)

	// Adoption: w2 reports an agent on disk the control plane never heard
	// of; it is wanted at the installed version (outdated: not running).
	got, err = s.ReportWorkerAgents(ctx, "w2", reporter("w2"), []store.AgentStatus{claudeStatus(store.AgentOutdated, "1.0", "2.0")}, t0+4)
	must(t, err)
	mustEqual(t, "adopted", []any{got.Agents[0].Wished, got.Agents[0].Wanted, got.Agents[0].WantedVersion, got.Agents[0].Runs()},
		[]any{true, true, "1.0", false})

	// A report that leaves the agent out: an unwanted entry goes, a wanted
	// one stays without a state.
	got, err = s.ReportWorkerAgents(ctx, "w1", reporter("w1"), nil, t0+5)
	must(t, err)
	mustEqual(t, "unwanted entry gone", got, store.WorkerAgents{WorkerID: "w1", Rev: 8, ReportedAt: t0 + 5})
	got, err = s.ReportWorkerAgents(ctx, "w2", reporter("w2"), []store.AgentStatus{}, t0+6)
	must(t, err)
	mustEqual(t, "wanted entry kept", got.Agents, []store.WorkerAgent{{WorkerID: "w2", Agent: store.AgentClaudeCode,
		Status: store.AgentStatus{Agent: store.AgentClaudeCode}, Wished: true, Wanted: true, WantedVersion: "1.0"}})

	// Per user: live workers only, by worker id.
	_, err = s.ReportWorkerAgents(ctx, "w1", reporter("w1"), []store.AgentStatus{claudeStatus(store.AgentNotInstalled, "", "2.0")}, t0+7)
	must(t, err)
	all, err := s.ListUserWorkerAgents(ctx, "u1")
	must(t, err)
	mustEqual(t, "user workers", []string{all[0].WorkerID, all[1].WorkerID}, []string{"w1", "w2"})
	_, err = s.DeleteIdentity(ctx, store.KindWorker, "w2", t0+8)
	must(t, err)
	all, err = s.ListUserWorkerAgents(ctx, "u1")
	must(t, err)
	mustEqual(t, "deleted worker left out", len(all), 1)
	_ = f
}

// testWorkerAgentValidation: reports and wishes are checked before
// anything is written.
func testWorkerAgentValidation(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	seed(t, s)
	connectWorker(t, s, "w1")
	ok := claudeStatus(store.AgentInstalled, "1", "1")
	for name, report := range map[string][]store.AgentStatus{
		"unknown agent":    {{Agent: "codex", State: store.AgentInstalled}},
		"twice":            {ok, ok},
		"unknown state":    {{Agent: store.AgentClaudeCode, State: "done"}},
		"no state":         {{Agent: store.AgentClaudeCode}},
		"long version":     {claudeStatus(store.AgentInstalled, strings.Repeat("v", 65), "1")},
		"long target":      {claudeStatus(store.AgentInstalled, "1", strings.Repeat("v", 65))},
		"long error":       {{Agent: store.AgentClaudeCode, State: store.AgentFailed, Error: strings.Repeat("e", 1025)}},
		"negative bytes":   {{Agent: store.AgentClaudeCode, State: store.AgentInstalling, DownloadedBytes: -1}},
		"too many reports": make([]store.AgentStatus, store.MaxWorkerAgents+1),
	} {
		if !isInvalid(errOf(s.ReportWorkerAgents(ctx, "w1", reporter("w1"), report, t0))) {
			t.Errorf("%s: accepted", name)
		}
	}
	mustIs(t, errOf(s.ReportWorkerAgents(ctx, "w1", reporter("w1"), []store.AgentStatus{ok}, 0)), store.ErrInvalid)
	got, err := s.ListWorkerAgents(ctx, "w1")
	must(t, err)
	mustEqual(t, "nothing written", got, store.WorkerAgents{WorkerID: "w1"})
	// Bounds at the limit are accepted.
	edge := claudeStatus(store.AgentFailed, strings.Repeat("v", 64), strings.Repeat("v", 64))
	edge.Error = strings.Repeat("e", 1024)
	must(t, errOf(s.ReportWorkerAgents(ctx, "w1", reporter("w1"), []store.AgentStatus{edge}, t0)))

	for name, err := range map[string]error{
		"unknown agent":        errOf(s.SetWorkerAgentWish(ctx, "w1", "codex", true, "1")),
		"wanted no version":    errOf(s.SetWorkerAgentWish(ctx, "w1", store.AgentClaudeCode, true, "")),
		"unwanted, a version":  errOf(s.SetWorkerAgentWish(ctx, "w1", store.AgentClaudeCode, false, "1")),
		"long version":         errOf(s.SetWorkerAgentWish(ctx, "w1", store.AgentClaudeCode, true, strings.Repeat("v", 65))),
		"lease, unknown agent": errOf(s.HasAgentLease(ctx, "w1", "codex", t0)),
		"lease, no time":       errOf(s.HasAgentLease(ctx, "w1", store.AgentClaudeCode, 0)),
	} {
		if !isInvalid(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
	mustIs(t, errOf(s.SetWorkerAgentWish(ctx, "nope", store.AgentClaudeCode, true, "1")), store.ErrNotFound)
}

// testAgentLeases: HasAgentLease sees only live leases of the worker on
// sessions of the agent; a session stores its agent, Claude Code by
// default, and refuses an unknown one.
func testAgentLeases(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	addWorker(t, s, "w2", "u1")
	def := newSession("s1", f)
	def.Agent = ""
	must(t, errOf(s.CreateSession(ctx, def, nil, store.DefaultMaxSessionsPerUser)))
	sess, err := s.GetSession(ctx, "s1", t0)
	must(t, err)
	mustEqual(t, "default agent", sess.Agent, store.AgentClaudeCode)
	bad := newSession("s2", f)
	bad.Agent = "codex"
	mustIs(t, errOf(s.CreateSession(ctx, bad, nil, store.DefaultMaxSessionsPerUser)), store.ErrInvalid)

	held := func(worker string, now int64) bool {
		t.Helper()
		v, err := s.HasAgentLease(ctx, worker, store.AgentClaudeCode, now)
		must(t, err)
		return v
	}
	mustEqual(t, "no lease", held("w1", t0), false)
	acquire(t, s, "s1", "w1", "b1", t0)
	mustEqual(t, "live lease", held("w1", t0+1), true)
	mustEqual(t, "another worker", held("w2", t0+1), false)
	mustEqual(t, "expired", held("w1", t0+ttl), false)
}

// testAgentReportFence: a report counts only from the worker's current boot
// and, unless it is an Attach, from the connection that attached last;
// every write of the worker's agents (a report, a changed wish) raises its
// revision, and a refused one writes nothing.
func testAgentReportFence(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	seed(t, s)
	connectWorker(t, s, "w1")
	installed := []store.AgentStatus{claudeStatus(store.AgentInstalled, "1", "1")}
	report := func(from store.AgentReporter, at int64, r []store.AgentStatus) (store.WorkerAgents, error) {
		return s.ReportWorkerAgents(ctx, "w1", from, r, at)
	}
	a := store.AgentReporter{BootID: "b-w1", ConnID: "c-a", ConnStart: "s1", ConnSeq: 1, Attach: true}
	got, err := report(a, t0, installed)
	must(t, err)
	mustEqual(t, "first rev", got.Rev, int64(1))
	a.Attach = false
	got, err = report(a, t0+1, installed)
	must(t, err)
	mustEqual(t, "second rev", got.Rev, int64(2))

	// Connection b attaches: a's late report is refused.
	bconn := store.AgentReporter{BootID: "b-w1", ConnID: "c-b", ConnStart: "s1", ConnSeq: 2, Attach: true}
	_, err = report(bconn, t0+2, []store.AgentStatus{claudeStatus(store.AgentNotInstalled, "", "1")})
	must(t, err)
	mustIs(t, errOf(report(a, t0+3, installed)), store.ErrStaleReport)
	// Nor does a's Attach, processed late, take the claim back: b was
	// made later by the same start.
	a.Attach = true
	mustIs(t, errOf(report(a, t0+3, installed)), store.ErrStaleReport)
	a.Attach = false
	// A connection that never attached may not report either.
	mustIs(t, errOf(report(store.AgentReporter{BootID: "b-w1", ConnID: "c-x"}, t0+3, installed)), store.ErrStaleReport)
	// Another boot, Attach or not, is refused.
	mustIs(t, errOf(report(store.AgentReporter{BootID: "b-old", ConnID: "c-o", ConnStart: "s1", ConnSeq: 9, Attach: true}, t0+3, installed)), store.ErrStaleReport)
	got, err = s.ListWorkerAgents(ctx, "w1")
	must(t, err)
	mustEqual(t, "refused reports wrote nothing", []any{got.Rev, got.ReportedAt, got.Agents[0].Status.State},
		[]any{int64(3), t0 + 2, store.AgentNotInstalled})

	// A changed wish raises the revision, the same wish does not.
	_, err = s.SetWorkerAgentWish(ctx, "w1", store.AgentClaudeCode, false, "")
	must(t, err)
	_, err = s.SetWorkerAgentWish(ctx, "w1", store.AgentClaudeCode, false, "")
	must(t, err)
	got, err = s.ListWorkerAgents(ctx, "w1")
	must(t, err)
	mustEqual(t, "rev after wishes", got.Rev, int64(4))
	// A boot change makes the new boot's Attach the reporter.
	_, err = s.SetConnected(ctx, store.KindWorker, "w1", "b-new", t0+4)
	must(t, err)
	mustIs(t, errOf(report(bconn, t0+5, installed)), store.ErrStaleReport)
	must(t, errOf(report(store.AgentReporter{BootID: "b-new", ConnID: "c-n", ConnStart: "s1", ConnSeq: 3, Attach: true}, t0+5, installed)))
	// After a control-plane restart its connections count from 1 again:
	// another start's connection claims the reports whatever its number.
	_, err = s.SetConnected(ctx, store.KindWorker, "w1", "b-new2", t0+6)
	must(t, err)
	must(t, errOf(report(store.AgentReporter{BootID: "b-new2", ConnID: "c-r", ConnStart: "s2", ConnSeq: 1, Attach: true}, t0+6, installed)))
	mustIs(t, errOf(report(store.AgentReporter{BootID: "b-new2", ConnID: "c-r0", ConnStart: "s2", ConnSeq: 0, Attach: true}, t0+7, installed)),
		store.ErrStaleReport)

	// Every install request raises the attempt, an uninstall keeps it.
	for i := range 2 {
		_, err = s.SetWorkerAgentWish(ctx, "w1", store.AgentClaudeCode, true, "1")
		must(t, err)
		got, err = s.ListWorkerAgents(ctx, "w1")
		must(t, err)
		mustEqual(t, "attempt", got.Agents[0].WantedAttempt, int64(i+1))
	}
	_, err = s.SetWorkerAgentWish(ctx, "w1", store.AgentClaudeCode, false, "")
	must(t, err)
	got, err = s.ListWorkerAgents(ctx, "w1")
	must(t, err)
	mustEqual(t, "attempt after uninstall", got.Agents[0].WantedAttempt, int64(2))
}
