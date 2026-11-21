package workspace_test

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/clock"
	"github.com/EricWvi/circus/internal/daemon"
	"github.com/EricWvi/circus/internal/daemon/jobs"
	"github.com/EricWvi/circus/internal/daemon/workspace"
	"github.com/EricWvi/circus/internal/testutil/leakcheck"
)

func TestMain(m *testing.M) { os.Exit(leakcheck.Run(m, leakcheck.Config{})) }

const session = "workspace-0196f3c2-test"

// staticEnv stands in for the Bash tool's captured environment.
type staticEnv struct{ env []string }

func (s staticEnv) Environment(ctx context.Context) ([]string, error) {
	return append([]string(nil), s.env...), ctx.Err()
}
func (staticEnv) PreparationBudget() time.Duration { return 25 * time.Second }

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// fixture is a dispatcher with both workspace tools on a fake clock, a
// private home and a git wrapper that the test controls through files in
// ctl: "gate" holds clones until "release" exists, "nospace" and "echo"
// replace git clone by canned failures, and every run dumps its
// environment to "env".
type fixture struct {
	t        *testing.T
	root     string
	home     string
	projects string
	ctl      string
	realGit  string
	gitEnv   []string
	clock    *clock.Fake
	jobs     *jobs.Manager
	prepare  *workspace.Prepare
	d        *daemon.Dispatcher
	logs     *syncBuffer
	calls    int
}

func newFixture(t *testing.T, configure ...func(*workspace.Options)) *fixture {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	leakcheck.Track(t, root)
	f := &fixture{t: t, root: root, home: filepath.Join(root, "home"), ctl: filepath.Join(root, "ctl"), realGit: realGit, clock: clock.New(), logs: &syncBuffer{}}
	f.projects = filepath.Join(f.home, "circus", "projects")
	for _, dir := range []string{f.home, f.ctl, filepath.Join(root, "bin")} {
		mustMkdir(t, dir)
	}
	wrapper := filepath.Join(root, "bin", "git")
	script := `#!/bin/sh
ctl="$CIRCUS_TEST_CTL"
env > "$ctl/env.tmp" && mv "$ctl/env.tmp" "$ctl/env"
if [ "$1" = clone ]; then
	if [ -e "$ctl/nospace" ]; then
		printf 'Cloning into %s...\nfatal: write error: No space left on device\n' "$5" >&2
		exit 128
	fi
	if [ -e "$ctl/echo" ]; then
		printf "Cloning from %s\rReceiving objects: 10%%\rReceiving objects: 50%%\r\nfatal: unable to access '%s': The requested URL returned error: 403\n" "$4" "$4" >&2
		exit 128
	fi
	if [ -e "$ctl/gate" ]; then
		sleep 1000 </dev/null >/dev/null 2>&1 &
		: > "$ctl/started.$$"
		while [ ! -e "$ctl/release" ]; do sleep 0.01; done
	fi
fi
exec ` + realGit + ` "$@"
`
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	f.gitEnv = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + f.home, "XDG_CONFIG_HOME=" + filepath.Join(f.home, ".config"), "GIT_CONFIG_NOSYSTEM=1",
		"CIRCUS_TEST_CTL=" + f.ctl}
	env := append([]string{"GIT_DIR=/nonexistent", "GIT_WORK_TREE=/nonexistent", "GIT_TERMINAL_PROMPT=1", "LC_ALL=fr_FR.UTF-8",
		"GIT_TRACE=1", "GIT_TRACE_CURL=1", "GIT_TRACE_REDACT=0", "GIT_CURL_VERBOSE=1", "GIT_ASKPASS=/usr/bin/ksshaskpass",
		"SSH_ASKPASS=/usr/bin/ksshaskpass", "SSH_ASKPASS_REQUIRE=force", "DISPLAY=:0", "WAYLAND_DISPLAY=wayland-0", "SSH_AUTH_SOCK=/run/agent.sock"}, f.gitEnv...)
	f.jobs = jobs.NewManager(jobs.Options{Clock: f.clock})
	opts := workspace.Options{Jobs: f.jobs, Environment: staticEnv{env}, Home: f.home, Git: wrapper, Clock: f.clock,
		Logger: slog.New(slog.NewTextHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	for _, c := range configure {
		c(&opts)
	}
	f.prepare = workspace.NewPrepare(opts)
	tools := leakcheck.AuditTools(t, f.clock.Now, []daemon.Tool{f.prepare, workspace.NewRemove(opts)})
	f.d, err = daemon.NewDispatcher(daemon.Options{Tools: tools, Clock: f.clock})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := f.jobs.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := f.d.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	return f
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	mustMkdir(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// git runs the real git for fixtures, isolated like the tool's runs.
func (f *fixture) git(dir string, args ...string) string {
	f.t.Helper()
	// No automatic maintenance: it detaches into its own session.
	cmd := exec.Command(f.realGit, append([]string{"-c", "gc.auto=0", "-c", "maintenance.auto=false"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(append([]string(nil), f.gitEnv...), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// bareRepo creates a bare repository with one commit and returns its
// file:// URL.
func (f *fixture) bareRepo(name string) string {
	f.t.Helper()
	work := filepath.Join(f.root, "src", name)
	mustWrite(f.t, filepath.Join(work, "README"), "hello from "+name+"\n")
	f.git(work, "init", "-q", "-b", "main")
	f.git(work, "add", "README")
	f.git(work, "commit", "-q", "-m", "first")
	bare := filepath.Join(f.root, "remotes", name+".git")
	f.git(f.root, "clone", "-q", "--bare", work, bare)
	return "file://" + bare
}

func (f *fixture) call(tool any) *circusv1.DeviceCall {
	f.calls++
	call := &circusv1.DeviceCall{CallId: fmt.Sprintf("call-%d", f.calls), SessionId: session, TimeoutMs: 120000}
	switch req := tool.(type) {
	case *circusv1.PrepareWorkspaceRequest:
		call.Tool = &circusv1.DeviceCall_PrepareWorkspace{PrepareWorkspace: req}
	case *circusv1.RemoveWorkspaceRequest:
		call.Tool = &circusv1.DeviceCall_RemoveWorkspace{RemoveWorkspace: req}
	}
	return call
}

func (f *fixture) submit(call *circusv1.DeviceCall) string {
	f.t.Helper()
	if _, err := f.d.Submit(call); err != nil {
		f.t.Fatal(err)
	}
	return call.CallId
}

func (f *fixture) wait(id string) *circusv1.DeviceResult {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	status, err := f.d.Wait(ctx, id)
	if err != nil {
		f.t.Fatalf("waiting for %s: %v", id, err)
	}
	if status.State != circusv1.CallStatus_STATE_COMPLETED {
		f.t.Fatalf("call %s ended %s", id, status.State)
	}
	result := status.Result
	if result.Fault != nil {
		f.t.Fatalf("call %s faulted: %v %s", id, result.Fault, text(result))
	}
	return result
}

func text(r *circusv1.DeviceResult) string {
	var parts []string
	for _, block := range r.GetOutput().GetContent() {
		parts = append(parts, block.GetText())
	}
	return strings.Join(parts, "\n")
}

func (f *fixture) prepareCall(path, url, resume string) *circusv1.DeviceCall {
	return f.call(&circusv1.PrepareWorkspaceRequest{Path: path, RepoUrl: url, ResumeJobId: resume})
}

func (f *fixture) prepareNow(path, url string) *circusv1.PrepareWorkspaceResult {
	f.t.Helper()
	result := f.wait(f.submit(f.prepareCall(path, url, "")))
	r := result.GetPrepareWorkspace()
	if r == nil {
		f.t.Fatalf("no prepare_workspace detail: %s", text(result))
	}
	if result.Output.IsError != (r.Outcome == circusv1.PrepareWorkspaceResult_OUTCOME_FAILED) {
		f.t.Fatalf("is_error %v for %v", result.Output.IsError, r)
	}
	return r
}

func (f *fixture) removeNow(path string) *circusv1.RemoveWorkspaceResult {
	f.t.Helper()
	result := f.wait(f.submit(f.call(&circusv1.RemoveWorkspaceRequest{Path: path})))
	r := result.GetRemoveWorkspace()
	if r == nil {
		f.t.Fatalf("no remove_workspace detail: %s", text(result))
	}
	return r
}

// idle waits until no preparation holds a slot, i.e. every lock is
// released and every partial clone removed.
func (f *fixture) idle() {
	f.t.Helper()
	eventually(f.t, "preparations to finish", func() bool { return workspace.ActivePreparations(f.prepare) == 0 })
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// started waits until n gated clones run.
func (f *fixture) started(n int) {
	f.t.Helper()
	eventually(f.t, fmt.Sprintf("%d clones to start", n), func() bool {
		matches, _ := filepath.Glob(filepath.Join(f.ctl, "started.*"))
		return len(matches) >= n
	})
}

func (f *fixture) gate()    { mustWrite(f.t, filepath.Join(f.ctl, "gate"), "") }
func (f *fixture) release() { mustWrite(f.t, filepath.Join(f.ctl, "release"), "") }

// armed waits until the fake clock has want more timers than before,
// i.e. a submitted call has started its execution timer.
func (f *fixture) armed(before, want int) {
	f.t.Helper()
	eventually(f.t, "the call's timers", func() bool { return f.clock.Waiters() >= before+want })
}

// entries lists the names in dir.
func entries(t *testing.T, dir string) []string {
	t.Helper()
	list, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range list {
		names = append(names, entry.Name())
	}
	return names
}

func expectFailed(t *testing.T, r *circusv1.PrepareWorkspaceResult, code circusv1.WorkspaceError) {
	t.Helper()
	if r.Outcome != circusv1.PrepareWorkspaceResult_OUTCOME_FAILED || r.Error != code {
		t.Fatalf("result = %v, want FAILED %v", r, code)
	}
}

func expectReady(t *testing.T, r *circusv1.PrepareWorkspaceResult, action circusv1.PrepareWorkspaceResult_Action) {
	t.Helper()
	if r.Outcome != circusv1.PrepareWorkspaceResult_OUTCOME_READY || r.Action != action {
		t.Fatalf("result = %v, want READY %v", r, action)
	}
}
