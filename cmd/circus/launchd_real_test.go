package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/EricWvi/circus/internal/credfile"
	"github.com/EricWvi/circus/internal/selfupdate"
	"github.com/EricWvi/circus/internal/testutil/fakerelease"
	"github.com/EricWvi/circus/internal/userservice"
	"github.com/EricWvi/circus/internal/wire"
	"github.com/EricWvi/circus/internal/workerservice"
)

// The real launchd tests install the daemon and worker services as launchd
// agents of the user running them, in that user's gui domain, under the
// services' fixed labels. They run only where CIRCUS_REAL_LAUNCHD=1 says the
// machine is disposable: the macOS CI job.
const realLaunchdEnv = "CIRCUS_REAL_LAUNCHD"

var (
	daemonLabel = "com.github.ericwvi.circus.daemon"
	workerLabel = "com.github.ericwvi.circus.worker"
)

// workerShutdown is the worker's log line for a graceful stop.
const workerShutdown = `"message":"worker shutting down"`

const (
	// agentWait bounds every wait for launchd: longer than the worker's
	// ExitTimeOut (30 s) and launchd's ThrottleInterval (5 s) together.
	agentWait = 90 * time.Second
	// cleanupReserve is the part of the test's time kept for its cleanup:
	// a wait for in-process calls, then a budget of its own per label,
	// which may wait for the agent to unload (the worker's ExitTimeOut).
	cleanupReserve = callWait + 2*labelCleanup + 10*time.Second
	callWait       = 20 * time.Second
	labelCleanup   = 75 * time.Second
	// printTimeout bounds one `launchctl print`.
	printTimeout = 10 * time.Second
	// waitDelay bounds how long a command's children may keep its output
	// open once it ended or was killed.
	waitDelay = 5 * time.Second
)

// command runs name in a process group of its own: when ctx ends, the
// whole group is killed (the worker packaging's npm and tsc included), and
// Wait gives up on pipes its children still hold after waitDelay.
func command(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = waitDelay
	return cmd
}

func domain() string { return "gui/" + strconv.Itoa(os.Getuid()) }

// testContext ends cleanupReserve before the test's deadline, so that a
// command that hangs fails the test with time left for the cleanup.
func testContext(t *testing.T) context.Context {
	deadline, ok := t.Deadline()
	if !ok {
		deadline = time.Now().Add(30 * time.Minute)
	}
	ctx, cancel := context.WithDeadline(t.Context(), deadline.Add(-cleanupReserve))
	t.Cleanup(cancel)
	return ctx
}

func requireRealLaunchd(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "darwin" || os.Getenv(realLaunchdEnv) != "1" {
		t.Skipf("set %s=1 on a disposable macOS machine to run the services under the real launchd", realLaunchdEnv)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	for _, label := range []string{daemonLabel, workerLabel} {
		a, err := queryAgent(ctx, label)
		if err != nil {
			t.Fatal(err)
		}
		if a.loaded {
			t.Fatalf("%s is already loaded in this user's launchd domain; these tests would replace it", label)
		}
	}
}

// agent is what `launchctl print` says about one agent.
type agent struct {
	loaded   bool
	state    string
	pid      int
	lastExit string
}

func (a agent) String() string {
	if !a.loaded {
		return "not loaded"
	}
	return fmt.Sprintf("state %q, pid %d, last exit %q", a.state, a.pid, a.lastExit)
}

func (a agent) running() bool { return a.loaded && a.state == "running" && a.pid > 0 }

// exitedZero reads a last exit code as `circus <service> status` does:
// the number it starts with ("0", or "0 (successful)" where launchctl
// says so).
func exitedZero(lastExit string) bool {
	var code int
	_, err := fmt.Sscanf(lastExit, "%d", &code)
	return err == nil && code == 0
}

// stoppedCleanly is an agent that is loaded but not running after its
// process exited 0.
func (a agent) stoppedCleanly() bool {
	return a.loaded && a.state != "running" && a.pid == 0 && exitedZero(a.lastExit)
}

// queryAgent asks launchd about label; an agent it does not know is not
// loaded.
func queryAgent(ctx context.Context, label string) (agent, error) {
	ctx, cancel := context.WithTimeout(ctx, printTimeout)
	defer cancel()
	out, err := command(ctx, "launchctl", "print", domain()+"/"+label).CombinedOutput()
	if err != nil {
		if ctx.Err() == nil && strings.Contains(string(out), "Could not find service") {
			return agent{}, nil
		}
		return agent{}, fmt.Errorf("launchctl print %s: %w\n%s", label, err, out)
	}
	a := agent{loaded: true}
	// Only the service's own properties: one tab deep.
	for line := range strings.SplitSeq(string(out), "\n") {
		if !strings.HasPrefix(line, "\t") || strings.HasPrefix(line, "\t\t") {
			continue
		}
		key, value, ok := strings.Cut(strings.TrimSpace(line), " = ")
		if !ok {
			continue
		}
		switch key {
		case "state":
			a.state = value
		case "pid":
			a.pid, _ = strconv.Atoi(value)
		case "last exit code":
			a.lastExit = value
		}
	}
	return a, nil
}

func (f *launchdFixture) agent(t *testing.T, label string) agent {
	t.Helper()
	a, err := queryAgent(f.ctx, label)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// waitAgent polls launchd until ok holds for the agent.
func (f *launchdFixture) waitAgent(t *testing.T, label, what string, ok func(agent) bool) agent {
	t.Helper()
	deadline := time.Now().Add(agentWait)
	for {
		a := f.agent(t, label)
		if ok(a) {
			return a
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: not %s within %s: %v", label, what, agentWait, a)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// holdAgent checks that ok keeps holding for d: launchd does not restart
// an agent whose process exited 0, which only the absence of a restart
// over its ThrottleInterval (5 s) shows.
func (f *launchdFixture) holdAgent(t *testing.T, label, what string, d time.Duration, ok func(agent) bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if a := f.agent(t, label); !ok(a) {
			t.Fatalf("%s: no longer %s: %v", label, what, a)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// requireStoppedCleanly checks with the product's own reading of launchd
// (userservice) that the service is installed, not running, and last
// exited 0.
func (f *launchdFixture) requireStoppedCleanly(t *testing.T, svc *userservice.Service) {
	t.Helper()
	var st userservice.Status
	f.within(t, "read the service status", nil, func() (err error) {
		st, err = svc.Get()
		return err
	})
	var code int
	if _, err := fmt.Sscanf(st.LastExit, "%d", &code); err != nil || !st.Installed || st.Running || st.PID != 0 || code != 0 || !st.StoppedAfterExit {
		t.Fatalf("status %+v; want installed, stopped after exit 0", st)
	}
}

// ready waits until the agent's current process (a, running) reached its
// link loop: its control plane received a request from exactly that
// process. Its signal handlers are in place by then.
func (f *launchdFixture) ready(t *testing.T, label string, plane *controlPlane, a agent) {
	t.Helper()
	deadline := time.Now().Add(agentWait)
	for !plane.heardFrom(a.pid) {
		if b := f.agent(t, label); b.pid != a.pid {
			t.Fatalf("%s: process %d is gone before its first link request: %v", label, a.pid, b)
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: process %d sent no link request within %s", label, a.pid, agentWait)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// inProcess is a call of within, which may change the agents of labels.
type inProcess struct {
	what   string
	labels []string
	done   chan struct{}
}

// within runs fn, an in-process entry point that takes no context and may
// change the agents of labels, and fails the test when the test's time
// runs out first. The cleanup waits for fn to return before it checks
// those agents.
func (f *launchdFixture) within(t *testing.T, what string, labels []string, fn func() error) {
	t.Helper()
	call := &inProcess{what: what, labels: labels, done: make(chan struct{})}
	f.mu.Lock()
	f.calls = append(f.calls, call)
	f.mu.Unlock()
	done := make(chan error, 1)
	go func() {
		defer close(call.done)
		done <- fn()
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	case <-f.ctx.Done():
		t.Fatalf("%s: %v", what, f.ctx.Err())
	}
}

func processGone(pid int) bool {
	return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}

func waitGoneProcess(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(agentWait)
	for !processGone(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("process %d still runs after %s", pid, agentWait)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// textInodes are the inodes of the files process pid runs from: its
// executable among them.
func (f *launchdFixture) textInodes(t *testing.T, pid int) []uint64 {
	t.Helper()
	out, err := command(f.ctx, "lsof", "-a", "-p", strconv.Itoa(pid), "-d", "txt", "-F", "i").Output()
	if err != nil {
		t.Fatalf("lsof -p %d: %v", pid, err)
	}
	var inodes []uint64
	for line := range strings.SplitSeq(string(out), "\n") {
		if value, ok := strings.CutPrefix(line, "i"); ok {
			if n, err := strconv.ParseUint(value, 10, 64); err == nil {
				inodes = append(inodes, n)
			}
		}
	}
	return inodes
}

func inode(t *testing.T, path string) uint64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Sys().(*syscall.Stat_t).Ino
}

// requireRunsBinary checks that process pid runs the executable now at path.
func (f *launchdFixture) requireRunsBinary(t *testing.T, pid int, path string) {
	t.Helper()
	if want := inode(t, path); !slices.Contains(f.textInodes(t, pid), want) {
		t.Fatalf("process %d does not run %s (inode %d): it runs %v", pid, path, want, f.textInodes(t, pid))
	}
}

// waitWorkerPackage waits until the worker service runs the worker of the
// package of version, and that worker reached its link loop: its launcher,
// a shell script, execs Node.js with the package's dist/main.js in the
// same process.
func (f *launchdFixture) waitWorkerPackage(t *testing.T, root, version string) agent {
	t.Helper()
	main := filepath.Join(root, version, "dist", "main.js")
	a := f.waitAgent(t, workerLabel, "running "+main, func(a agent) bool {
		return a.running() && strings.Contains(f.commandLine(a.pid), main)
	})
	f.ready(t, workerLabel, f.workerPlane, a)
	return a
}

// waitDaemon waits until the daemon service runs a process other than
// old, and that process reached its link loop.
func (f *launchdFixture) waitDaemon(t *testing.T, old int) agent {
	t.Helper()
	a := f.waitAgent(t, daemonLabel, "running", func(a agent) bool { return a.running() && a.pid != old })
	f.ready(t, daemonLabel, f.daemonPlane, a)
	return a
}

func (f *launchdFixture) commandLine(pid int) string {
	// ps fails for a process that is gone: nothing to match.
	out, _ := command(f.ctx, "ps", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	return strings.TrimSpace(string(out))
}

// buildRelease builds this command as release version into path.
func buildRelease(ctx context.Context, t *testing.T, path, version string) {
	t.Helper()
	cmd := command(ctx, filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-o", path, "-ldflags", "-X main.version="+version, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
}

// buildWorkerPackage builds the worker's real release package of version
// for this Mac and returns its archive.
func buildWorkerPackage(ctx context.Context, t *testing.T, node, dir, version, target string) []byte {
	t.Helper()
	cmd := command(ctx, node, filepath.Join("..", "..", "worker", "scripts", "package.mjs"), version, dir, target)
	out, err := cmd.Output()
	if err != nil {
		var stderr []byte
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			stderr = exit.Stderr
		}
		t.Fatalf("package the worker: %v\n%s%s", err, out, stderr)
	}
	data, err := os.ReadFile(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// controlPlane stands in for the control plane of one service: it refuses
// every link with 503, which daemon and worker retry, or, once reject is
// set, rejects the credential as the control plane does for a revoked
// machine, a terminal stop for both. It records the process each link
// request came from.
type controlPlane struct {
	srv    *httptest.Server
	reject atomic.Bool
	mu     sync.Mutex
	pids   map[int]bool
}

func newControlPlane(t *testing.T) *controlPlane {
	c := &controlPlane{pids: map[int]bool{}}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The connection is open while the handler runs: lsof finds the
		// process at its other end.
		if pid := peerPID(r.Context(), r.RemoteAddr); pid > 0 {
			c.mu.Lock()
			c.pids[pid] = true
			c.mu.Unlock()
		}
		if c.reject.Load() {
			w.Header().Set(wire.LinkAuthHeader, wire.LinkAuthRejected)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *controlPlane) heardFrom(pid int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pids[pid]
}

// peerPID is the process, other than this one, with a socket on the
// loopback address remote (host:port), or 0.
func peerPID(ctx context.Context, remote string) int {
	ctx, cancel := context.WithTimeout(ctx, printTimeout)
	defer cancel()
	out, _ := command(ctx, "lsof", "-nP", "-iTCP@"+remote, "-F", "p").Output()
	for line := range strings.SplitSeq(string(out), "\n") {
		if value, ok := strings.CutPrefix(line, "p"); ok {
			if pid, err := strconv.Atoi(value); err == nil && pid != os.Getpid() {
				return pid
			}
		}
	}
	return 0
}

// launchdFixture is one user's circus under the real launchd: a home and
// a CIRCUS_HOME of its own, circus v1.2.3 installed on PATH, the worker
// packages of v1.2.3 and v1.3.0 in a local release, and a Node.js wrapper
// the worker service records.
type launchdFixture struct {
	home, circus, next, node string
	daemonPlane, workerPlane *controlPlane
	// ctx ends before the test's deadline, leaving time for the cleanup.
	ctx context.Context
	// disabled are the labels the test disabled and has not enabled again.
	disabled map[string]bool
	mu       sync.Mutex
	calls    []*inProcess
	// busy are the labels of in-process calls that had not returned when
	// the test ended.
	busy map[string]bool
}

func newLaunchdFixture(t *testing.T) *launchdFixture {
	t.Helper()
	realNode, err := exec.LookPath("node")
	if err != nil {
		t.Fatalf("the worker service needs Node.js: %v", err)
	}
	// Resolved: the temporary directory is below /var, a link to
	// /private/var, and the worker service records Node.js resolved.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &launchdFixture{
		home: filepath.Join(dir, "home"), circus: filepath.Join(dir, "bin", "circus"),
		next: filepath.Join(dir, "next", "circus"), node: filepath.Join(dir, "tools", "node"),
		daemonPlane: newControlPlane(t), workerPlane: newControlPlane(t),
		ctx: testContext(t), disabled: map[string]bool{}, busy: map[string]bool{},
	}
	// Best effort, as the test runs last on a disposable runner: leave
	// nothing loaded and nothing disabled.
	t.Cleanup(func() { f.cleanup(t) })
	buildRelease(f.ctx, t, f.circus, "v1.2.3")
	buildRelease(f.ctx, t, f.next, "v1.3.0")
	target := runtime.GOOS + "-" + runtime.GOARCH
	release := fakerelease.New(t)
	for _, v := range []string{"v1.2.3", "v1.3.0"} {
		release.PublishArchive(v, []string{target}, buildWorkerPackage(f.ctx, t, realNode, filepath.Join(dir, "packages"), v, target))
	}
	// A script, not a link, so the service records it as it is and the
	// test can take it away.
	if err := os.MkdirAll(filepath.Dir(f.node), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.node, []byte("#!/bin/sh\nexec '"+realNode+"' \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("HOME", f.home)
	t.Setenv("CIRCUS_HOME", filepath.Join(dir, "circus-home"))
	t.Setenv("PATH", filepath.Dir(f.circus)+":/usr/bin:/bin:/usr/sbin:/sbin")
	t.Setenv("CIRCUS_NODE", f.node)
	for _, name := range []string{"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	for _, name := range append(slices.Clone(workerservice.ProxyVariables), workerservice.Settings...) {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	for role, plane := range map[credfile.Role]*controlPlane{credfile.RoleDaemon: f.daemonPlane, credfile.RoleWorker: f.workerPlane} {
		path, err := credfile.Path(role)
		if err != nil {
			t.Fatal(err)
		}
		// A bearer token is 43 base64url characters, as the worker checks.
		token := strings.Repeat("t", 43)
		if err := credfile.Save(path, credfile.File{Role: role, Token: token, PrincipalID: string(role), ControlPlaneURL: plane.srv.URL}); err != nil {
			t.Fatal(err)
		}
	}

	oldVersion, oldRelease, oldExe := version, workerRelease, workerExecutable
	t.Cleanup(func() { version, workerRelease, workerExecutable = oldVersion, oldRelease, oldExe })
	version = "v1.2.3"
	workerRelease = func(string) selfupdate.Release { return release.Release() }
	workerExecutable = func() (string, error) { return f.circus, nil }
	// Registered last, so it runs first: in-process calls still running
	// use HOME and the stand-ins above until they return.
	t.Cleanup(func() { f.waitCalls(t) })
	return f
}

// waitCalls waits for the in-process calls (within) that may still change
// an agent, and marks the labels of any that has not returned as busy:
// the cleanup leaves those to the runner's disposal and says so.
func (f *launchdFixture) waitCalls(t *testing.T) {
	f.mu.Lock()
	defer f.mu.Unlock()
	timeout := time.After(callWait)
	for _, call := range f.calls {
		select {
		case <-call.done:
			continue
		case <-timeout:
		}
		select {
		case <-call.done:
		default:
			for _, label := range call.labels {
				f.busy[label] = true
				t.Errorf("CLEANUP SKIPPED: %q still runs in the test process and may load %s later; boot it out by hand: launchctl bootout %s/%s",
					call.what, label, domain(), label)
			}
		}
	}
}

// cleanup boots out each label still loaded, with a budget of its own, and
// enables again what the test disabled.
func (f *launchdFixture) cleanup(t *testing.T) {
	for _, label := range []string{workerLabel, daemonLabel} {
		if f.busy[label] {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), labelCleanup)
		if a, err := queryAgent(ctx, label); err != nil || a.loaded {
			if out, err := command(ctx, "launchctl", "bootout", domain()+"/"+label).CombinedOutput(); err != nil {
				t.Logf("cleanup: launchctl bootout %s: %v\n%s", label, err, out)
			}
			// bootout returns once launchd has asked the process to stop;
			// the agent unloads when it has exited (up to its ExitTimeOut).
			for {
				a, err = queryAgent(ctx, label)
				if err == nil && !a.loaded {
					break
				}
				if ctx.Err() != nil {
					t.Errorf("CLEANUP FAILED: %s is still loaded in %s (%v, %v); boot it out by hand: launchctl bootout %s/%s",
						label, domain(), a, err, domain(), label)
					break
				}
				time.Sleep(200 * time.Millisecond)
			}
		}
		if f.disabled[label] {
			if out, err := command(ctx, "launchctl", "enable", domain()+"/"+label).CombinedOutput(); err != nil {
				t.Errorf("CLEANUP FAILED: %s stays disabled in %s: %v\n%s; enable it by hand: launchctl enable %s/%s",
					label, domain(), err, out, domain(), label)
			}
		}
		cancel()
	}
}

// launchctl runs a launchctl subcommand on label.
func (f *launchdFixture) launchctl(t *testing.T, verb, label string) {
	t.Helper()
	if out, err := command(f.ctx, "launchctl", verb, domain()+"/"+label).CombinedOutput(); err != nil {
		t.Fatalf("launchctl %s %s: %v\n%s", verb, label, err, out)
	}
}

// cli runs the installed circus as the user would.
func (f *launchdFixture) cli(t *testing.T, args ...string) string {
	t.Helper()
	out, err := command(f.ctx, f.circus, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("circus %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func (f *launchdFixture) log(t *testing.T, service string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.home, "Library", "Logs", "circus", service+".log"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func (f *launchdFixture) plist(label string) string {
	return filepath.Join(f.home, "Library", "LaunchAgents", label+".plist")
}

func requireOutput(t *testing.T, what, out string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("%s lacks %q:\n%s", what, w, out)
		}
	}
}

// lastExitZero is a status line of a last exit code of 0, as launchctl
// writes it ("0", or "0 (successful)").
var lastExitZero = regexp.MustCompile(`(?m)^  Last exit  0( \(.*\))?$`)

// TestRealLaunchdServices runs the daemon and worker services under the
// real launchd through the commands a user runs and the code `circus
// update` runs, and checks each step against launchd itself: install,
// restart, update (the binary and the worker package replaced, both
// services restarted on the new ones), SIGTERM (a graceful exit 0 that
// launchd does not restart), a crash (restarted by KeepAlive), stop,
// start, terminal stops that stay stopped, and uninstall. Before it
// restarts or signals a process, the test waits for that process's first
// link request, by which time its signal handlers are in place.
func TestRealLaunchdServices(t *testing.T) {
	requireRealLaunchd(t)
	f := newLaunchdFixture(t)
	root, err := workerservice.Root()
	if err != nil {
		t.Fatal(err)
	}
	type service struct {
		label, name, noun string
		svc               *userservice.Service
		plane             *controlPlane
	}
	services := []service{
		{daemonLabel, "daemon", "device daemon", userservice.Daemon, f.daemonPlane},
		{workerLabel, "worker", "worker", userservice.Worker, f.workerPlane},
	}

	// Install.
	out := f.cli(t, "daemon", "install")
	requireOutput(t, "daemon install", out, "✓ Installed and started the circus device daemon service.", f.plist(daemonLabel))
	daemon := f.waitDaemon(t, 0)
	f.requireRunsBinary(t, daemon.pid, f.circus)
	var stdout, stderr bytes.Buffer
	f.within(t, "worker install", []string{workerLabel}, func() error { return workerCommand([]string{"install"}, &stdout, &stderr) })
	requireOutput(t, "worker install", stdout.String(), "✓ Installed and started the circus worker service.",
		"  Version    v1.2.3\n", "  Node.js    "+f.node+"\n", f.plist(workerLabel))
	worker := f.waitWorkerPackage(t, root, "v1.2.3")
	requireOutput(t, "daemon status", f.cli(t, "daemon", "status"),
		fmt.Sprintf("installed and running (PID %d)", daemon.pid), "  Starts     at login\n")
	requireOutput(t, "worker status", f.cli(t, "worker", "status"),
		fmt.Sprintf("installed and running (PID %d)", worker.pid), "  Version    v1.2.3\n")
	// Status reads launchd's disabled list: a disabled agent does not start
	// at login. The override outlives the agent, so the cleanup enables it
	// again if the test stops in between.
	f.disabled[daemonLabel] = true
	f.launchctl(t, "disable", daemonLabel)
	requireOutput(t, "daemon status when disabled", f.cli(t, "daemon", "status"), "  Starts     only when started\n")
	f.launchctl(t, "enable", daemonLabel)
	delete(f.disabled, daemonLabel)
	requireOutput(t, "daemon status when enabled again", f.cli(t, "daemon", "status"), "  Starts     at login\n")

	// Restart: a new process of each.
	requireOutput(t, "daemon restart", f.cli(t, "daemon", "restart"), "✓ Restarted the circus device daemon service.")
	old := daemon.pid
	daemon = f.waitDaemon(t, old)
	waitGoneProcess(t, old)
	stops := strings.Count(f.log(t, "worker"), workerShutdown)
	requireOutput(t, "worker restart", f.cli(t, "worker", "restart"), "✓ Restarted the circus worker service.")
	old = worker.pid
	f.waitAgent(t, workerLabel, "restarted", func(a agent) bool { return a.running() && a.pid != old })
	waitGoneProcess(t, old)
	worker = f.waitWorkerPackage(t, root, "v1.2.3")
	// launchd stops the old worker with SIGTERM, so it shuts down gracefully.
	if n := strings.Count(f.log(t, "worker"), workerShutdown); n != stops+1 {
		t.Errorf("the worker logged %d graceful shutdowns over a restart, want 1:\n%s", n-stops, f.log(t, "worker"))
	}

	// Update: circus is replaced by v1.3.0, the worker service switched to
	// the v1.3.0 package, and both restarted by one detached helper (this
	// test binary stands in for circus there, see TestMain).
	fakeSelfUpdate(t, selfupdate.Plan{Version: "v1.3.0", Path: f.circus, Apply: func(context.Context) (selfupdate.Result, error) {
		if err := os.Rename(f.next, f.circus); err != nil {
			return selfupdate.Result{}, err
		}
		return selfupdate.Result{Message: "Updated circus to v1.3.0.", ReplacedPath: f.circus, Version: "v1.3.0"}, nil
	}})
	stdout.Reset()
	stderr.Reset()
	f.within(t, "update", []string{workerLabel, daemonLabel}, func() error { return updateCommand(nil, &stdout, &stderr) })
	requireOutput(t, "update", stdout.String(),
		"Switched the circus worker service from v1.2.3 to v1.3.0 and asked the service manager to restart it.\n",
		"Asked the service manager to restart the circus device daemon service.\n")
	if stderr.Len() != 0 {
		t.Errorf("update diagnostics:\n%s", stderr.String())
	}
	daemon = f.waitDaemon(t, daemon.pid)
	f.requireRunsBinary(t, daemon.pid, f.circus)
	worker = f.waitWorkerPackage(t, root, "v1.3.0")
	if v := strings.TrimSpace(f.cli(t, "version")); v != "v1.3.0" {
		t.Errorf("circus version = %q after the update", v)
	}
	requireOutput(t, "worker status", f.cli(t, "worker", "status"),
		fmt.Sprintf("installed and running (PID %d)", worker.pid), "  Version    v1.3.0\n")

	// SIGTERM: a graceful exit 0, which launchd reports and does not restart.
	for i, a := range []agent{daemon, worker} {
		s := services[i]
		if err := syscall.Kill(a.pid, syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		f.waitAgent(t, s.label, "stopped after exit 0", agent.stoppedCleanly)
		waitGoneProcess(t, a.pid)
		f.holdAgent(t, s.label, "stopped after exit 0", 7*time.Second, agent.stoppedCleanly)
		f.requireStoppedCleanly(t, s.svc)
		out := f.cli(t, s.name, "status")
		requireOutput(t, s.name+" status after SIGTERM", out, "is installed and not running.", "See the logs for why it stopped:")
		if !lastExitZero.MatchString(out) {
			t.Errorf("%s status after SIGTERM shows no last exit code 0:\n%s", s.name, out)
		}
	}
	requireOutput(t, "worker log", f.log(t, "worker"), `"signal":"SIGTERM"`)

	// Start, then a crash: KeepAlive restarts a process that did not exit 0.
	for _, s := range services {
		requireOutput(t, s.name+" start", f.cli(t, s.name, "start"), "✓ Started the circus "+s.noun+" service.")
		a := f.waitAgent(t, s.label, "running", agent.running)
		f.ready(t, s.label, s.plane, a)
		if err := syscall.Kill(a.pid, syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		b := f.waitAgent(t, s.label, "restarted after a crash", func(b agent) bool { return b.running() && b.pid != a.pid })
		f.ready(t, s.label, s.plane, b)
	}

	// Stop unloads the agent, start loads it again.
	for _, s := range services {
		a := f.waitAgent(t, s.label, "running", agent.running)
		requireOutput(t, s.name+" stop", f.cli(t, s.name, "stop"), "✓ Stopped the circus "+s.noun+" service.")
		if b := f.agent(t, s.label); b.loaded {
			t.Errorf("%s still loaded after stop: %v", s.label, b)
		}
		waitGoneProcess(t, a.pid)
		requireOutput(t, s.name+" status after stop", f.cli(t, s.name, "status"),
			"is installed and not running.", "  State      not loaded\n")
		requireOutput(t, s.name+" start", f.cli(t, s.name, "start"), "✓ Started the circus "+s.noun+" service.")
		b := f.waitAgent(t, s.label, "running", agent.running)
		f.ready(t, s.label, s.plane, b)
	}

	// A credential the control plane rejects stops either service for
	// good: exit 0 on macOS, so launchd leaves it stopped.
	for _, s := range services {
		s.plane.reject.Store(true)
		f.cli(t, s.name, "restart")
		f.waitAgent(t, s.label, "stopped after exit 0", agent.stoppedCleanly)
		f.holdAgent(t, s.label, "stopped after exit 0", 7*time.Second, agent.stoppedCleanly)
		f.requireStoppedCleanly(t, s.svc)
	}
	requireOutput(t, "daemon log", f.log(t, "daemon"), "circus daemon service stopped: daemon link: HTTP 401 authentication rejected")
	requireOutput(t, "worker status", f.cli(t, "worker", "status"),
		"It stopped itself and stays stopped. The control plane refused its credential")

	// So does a Node.js that is gone: the package's launcher stops for
	// good with exit 0 and says why.
	f.workerPlane.reject.Store(false)
	if err := os.Remove(f.node); err != nil {
		t.Fatal(err)
	}
	f.cli(t, "worker", "start")
	installed, _, err := workerservice.InstalledService()
	if err != nil {
		t.Fatal(err)
	}
	// The stop file still holds the previous stop until the launcher
	// replaces it, just before it exits.
	f.waitAgent(t, workerLabel, "stopped after exit 0", func(a agent) bool {
		data, _ := os.ReadFile(workerservice.StopFile(installed))
		return a.stoppedCleanly() && strings.Contains(string(data), `"reason":"node"`)
	})
	requireOutput(t, "worker log", f.log(t, "worker"), "circus-worker: Node.js 24 or later is required: "+f.node+" not found")
	requireOutput(t, "worker status", f.cli(t, "worker", "status"),
		"It stopped itself and stays stopped. Its Node.js is missing or too old",
		"The Node.js it was installed with ("+f.node+") is gone.")

	// Uninstall.
	for _, s := range services {
		a := f.agent(t, s.label)
		requireOutput(t, s.name+" uninstall", f.cli(t, s.name, "uninstall"), "✓ Removed the circus "+s.noun+" service.")
		if b := f.agent(t, s.label); b.loaded {
			t.Errorf("%s still loaded after uninstall: %v", s.label, b)
		}
		if a.pid > 0 {
			waitGoneProcess(t, a.pid)
		}
		if _, err := os.Stat(f.plist(s.label)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s remains: %v", f.plist(s.label), err)
		}
		requireOutput(t, s.name+" status after uninstall", f.cli(t, s.name, "status"), "service is not installed.")
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 1 || entries[0].Name() != ".lock" {
		t.Errorf("uninstall left %v in the worker store (%v)", entries, err)
	}
}
