//go:build e2e && linux

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/testutil/fakemodel"
)

// TestWorkerPackage runs the worker from an unpacked release package
// (CIRCUS_E2E_WORKER_PACKAGE: the circus-worker directory of
// circus-worker-<tag>-linux-<arch>.tar.gz, outside this repository, so it
// cannot load anything from the repository's node_modules) against a real
// control plane and the fake model. The worker attaches without Claude
// Code; an install from a registry serving a corrupt tarball fails its
// digest check and leaves nothing behind; the install from the real one
// succeeds and runs a session turn; uninstalling (once the idle lease is
// released) removes the install directory.
func TestWorkerPackage(t *testing.T) {
	pkg := os.Getenv("CIRCUS_E2E_WORKER_PACKAGE")
	if pkg == "" {
		t.Skip("CIRCUS_E2E_WORKER_PACKAGE (an unpacked circus-worker release package) is not set")
	}
	pkg, err := filepath.Abs(pkg)
	must(t, err)
	if real, err := filepath.EvalSymlinks(pkg); err == nil {
		pkg = real
	}
	if repo, err := filepath.EvalSymlinks(root); err != nil || within(repo, pkg) {
		t.Fatalf("the package %s must lie outside the repository %s", pkg, root)
	}
	launcher := filepath.Join(pkg, "circus-worker")
	if _, err := os.Stat(launcher); err != nil {
		t.Fatal(err)
	}
	var log scriptLog
	main := func(r *fakemodel.Request) *fakemodel.Response {
		if r.Contains("say hello") {
			return fakemodel.Text("Hello from the packaged worker.")
		}
		return log.fail("unexpected request: %.300s", r.Last().Text())
	}
	// Idle leases go back after a second, so the uninstall is not refused
	// for long.
	s := newStack(t, launcher, fakemodel.Options{Main: main}, "CIRCUS_WORKER_IDLE_SECONDS=1")
	s.startWorkerOnly()

	a := s.waitAgent(func(a *pb.WorkerAgent) bool { return a.GetStatus().GetState() != pb.AgentStatus_STATE_UNSPECIFIED })
	target := a.GetStatus().GetTargetVersion()
	if a.GetStatus().GetState() != pb.AgentStatus_STATE_NOT_INSTALLED || a.GetWanted() || target == "" {
		t.Fatalf("the fresh worker's Claude Code: %v", a)
	}
	agents := filepath.Join(s.h.dir, "data", "circus", "agents", "claude-code")
	installed := filepath.Join(agents, target, "claude")
	entries := func() []string {
		list, err := os.ReadDir(agents)
		if os.IsNotExist(err) {
			return nil
		}
		must(t, err)
		var names []string
		for _, e := range list {
			names = append(names, e.Name())
		}
		return names
	}

	// A tarball that is not the one the worker was built with.
	s.registry.Corrupt(true)
	s.setAgent(true)
	a = s.waitAgent(func(a *pb.WorkerAgent) bool { return a.GetStatus().GetState() == pb.AgentStatus_STATE_FAILED })
	if a.GetStatus().GetError() == "" || a.GetStatus().GetInstalledVersion() != "" || !a.GetWanted() {
		t.Fatalf("after the corrupt download: %v", a)
	}
	if names := entries(); len(names) != 0 {
		t.Fatalf("a failed install left %v in %s", names, agents)
	}

	// Installing again retries, from the real tarball.
	s.registry.Corrupt(false)
	gets := s.registry.Gets()
	start := time.Now()
	s.setAgent(true)
	a = s.waitAgent(agentRuns)
	t.Logf("Claude Code %s installed in %v", a.GetStatus().GetInstalledVersion(), time.Since(start).Round(time.Millisecond))
	if a.GetStatus().GetInstalledVersion() != target || s.registry.Gets() != gets+1 {
		t.Fatalf("installed %v after %d downloads", a, s.registry.Gets()-gets)
	}
	if info, err := os.Stat(installed); err != nil || info.Mode()&0o111 == 0 {
		t.Fatalf("installed binary %s: %v, %v", installed, info, err)
	}
	if names := entries(); len(names) != 1 || names[0] != target {
		t.Fatalf("install directory holds %v", names)
	}

	// One turn of a session.
	sess := s.open("")
	w := sess.watch(0)
	logOnFailure(t, w)
	sess.send("say hello", pb.InputPriority_INPUT_PRIORITY_NORMAL)
	events := durableUntil(t, w, func(e *pb.SessionEvent) bool { return e.GetKind() == pb.SessionEvent_KIND_RESULT })
	sess.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_COMPLETED)
	if got := eventString(t, events[len(events)-1], "result"); got != "Hello from the packaged worker." {
		t.Fatalf("result %q", got)
	}
	log.check(t)

	// Uninstall: refused while the idle session still holds its lease.
	deadline := time.Now().Add(agentWait)
	for {
		_, err := s.setAgent(false)
		if err == nil {
			break
		}
		if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "lease") || time.Now().After(deadline) {
			t.Fatalf("uninstall: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	a = s.waitAgent(func(a *pb.WorkerAgent) bool { return a.GetStatus().GetState() == pb.AgentStatus_STATE_NOT_INSTALLED })
	if a.GetWanted() || a.GetStatus().GetInstalledVersion() != "" {
		t.Fatalf("after uninstall: %v", a)
	}
	if _, err := os.Stat(installed); !os.IsNotExist(err) {
		t.Fatalf("installed binary after uninstall: %v", err)
	}
	if names := entries(); len(names) != 0 {
		t.Fatalf("uninstall left %v in %s", names, agents)
	}
	// Claude Code's own configuration is untouched.
	if _, err := os.Stat(s.config); err != nil {
		t.Fatalf("CLAUDE_CONFIG_DIR after uninstall: %v", err)
	}
}
