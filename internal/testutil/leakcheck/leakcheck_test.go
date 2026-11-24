package leakcheck

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
)

func TestMain(m *testing.M) {
	// Deliberate leaks always wait out the grace; keep the self-test fast.
	grace = 300 * time.Millisecond
	os.Exit(Run(m, Config{}))
}

// recorder runs the checks against a fake test, so deliberate leaks fail the
// recorder instead of this test.
type recorder struct {
	testing.TB
	mu       sync.Mutex
	errors   []string
	logs     []string
	cleanups []func()
}

func newRecorder(t *testing.T) *recorder { return &recorder{TB: t} }

func (r *recorder) Helper() {}
func (r *recorder) Errorf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}
func (r *recorder) Logf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.logs = append(r.logs, fmt.Sprintf(format, args...))
}
func (r *recorder) Cleanup(f func()) { r.cleanups = append(r.cleanups, f) }

// finish runs the fake test's cleanups in testing's LIFO order.
func (r *recorder) finish() string {
	for i := len(r.cleanups) - 1; i >= 0; i-- {
		r.cleanups[i]()
	}
	r.cleanups = nil
	return strings.Join(r.errors, "\n")
}

func scratch(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp(filepath.Join("..", "..", "..", ".tmp"), "leakcheck-self-")
	if err != nil {
		t.Fatal(err)
	}
	dir, _ = filepath.Abs(dir)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func want(t *testing.T, report string, parts ...string) {
	t.Helper()
	for _, part := range parts {
		if !strings.Contains(report, part) {
			t.Errorf("report lacks %q:\n%s", part, report)
		}
	}
}

func TestDetectsEscapedChild(t *testing.T) {
	root := scratch(t)
	r := newRecorder(t)
	Track(r, root)
	// A setsid shell exits at once; its orphaned child is an escapee in its
	// own session that only the subreaper keeps below the test process. The
	// shell opens held.txt before forking, so the child holds it even if rm
	// runs before the child's own redirections.
	cmd := exec.Command("/bin/sh", "-c", "exec 3>held.txt; sleep 30 >&3 2>&1 3>&- </dev/null & echo $! > child.pid; exec 3>&-; rm held.txt")
	cmd.Dir = root
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "child.pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	start := time.Now()
	report := r.finish()
	want(t, report, "processes", fmt.Sprintf("pid=%d ppid=%d", pid, os.Getpid()), "sid=", "state=", "sleep 30", "holds deleted files: fd 1 -> "+filepath.Join(root, "held.txt")+" (deleted)")
	if time.Since(start) > grace+time.Second {
		t.Errorf("check took %s", time.Since(start))
	}
	if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
		t.Fatalf("leaked child %d was not killed and reaped: %v", pid, err)
	}
}

func TestDetectsOpenAndDeletedFiles(t *testing.T) {
	root := scratch(t)
	output := filepath.Join(root, "output")
	if err := os.MkdirAll(filepath.Join(output, "session", "tasks"), 0o700); err != nil {
		t.Fatal(err)
	}
	r := newRecorder(t)
	Track(r, root)
	OutputRoot(r, output)
	open, err := os.Create(filepath.Join(root, "open.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer open.Close()
	gone, err := os.Create(filepath.Join(output, "session", "tasks", "gone.output"))
	if err != nil {
		t.Fatal(err)
	}
	defer gone.Close()
	if err := os.Remove(gone.Name()); err != nil {
		t.Fatal(err)
	}
	stray := filepath.Join(output, "session", "tasks", "stray.output")
	announced := filepath.Join(output, "session", "tasks", "announced.output")
	for _, path := range []string{stray, announced} {
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	Expect(announced)
	report := r.finish()
	want(t, report, "fds", "-> "+open.Name(), "holds deleted file "+gone.Name()+" (deleted)", "unannounced file in output root: "+stray)
	if strings.Contains(report, "unannounced file in output root: "+announced) {
		t.Errorf("announced file reported:\n%s", report)
	}
}

func TestDetectsStateDirectoryLeftovers(t *testing.T) {
	root := scratch(t)
	state := filepath.Join(root, "state")
	for _, path := range []string{"instance-a/snapshot-1", "instance-a/snapshot-2"} {
		if err := os.MkdirAll(filepath.Join(state, path), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{".instances.lock", "instance-a/.lock", "instance-a/snapshot-1/snapshot.sh", "instance-a/snapshot-1/cwd-123"} {
		if err := os.WriteFile(filepath.Join(state, path), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	r := newRecorder(t)
	StateDir(r, state)
	report := r.finish()
	want(t, report, "cwd-123", "obsolete snapshots")
	if strings.Contains(report, ".instances.lock") || strings.Contains(report, "snapshot.sh") {
		t.Errorf("documented state reported:\n%s", report)
	}
}

func TestDiskLimitAndAllowances(t *testing.T) {
	root := scratch(t)
	r := newRecorder(t)
	OutputRoot(r, root)
	path := filepath.Join(root, "big.txt")
	if err := os.WriteFile(path, make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	Expect(path)
	DiskLimit(r, 1024, "self-test")
	open, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer open.Close()
	Allow(r, FDs, "self-test keeps this descriptor")
	report := r.finish()
	want(t, report, "output roots hold", "above the 0.0 MiB limit")
	if strings.Contains(report, "file descriptor") {
		t.Errorf("allowed descriptor reported as an error:\n%s", report)
	}
	want(t, strings.Join(r.logs, "\n"), "allowed fds (self-test keeps this descriptor)")
}

func TestMemoryLimit(t *testing.T) {
	if failure := memoryFailure(1 << 20); !strings.Contains(failure, "exceeds the package limit 1.0 MiB") {
		t.Errorf("1 MiB limit not reported: %q", failure)
	}
	if failure := memoryFailure(1 << 40); failure != "" {
		t.Errorf("1 TiB limit reported: %q", failure)
	}
}

// TestExclusiveReadsTestingInternals is a canary for the reflection on
// testing's unexported fields: if a Go release renames them, exclusive would
// silently fall back to attribution only, so this fails loudly instead.
func TestExclusiveReadsTestingInternals(t *testing.T) {
	if !exclusive(t) {
		t.Fatal("a sequential top-level test is not exclusive: testing.common.isParallel or parent changed")
	}
	t.Run("sequential", func(t *testing.T) {
		if !exclusive(t) {
			t.Error("a sequential subtest of a sequential test is not exclusive")
		}
	})
	t.Run("parallel", func(t *testing.T) {
		t.Parallel()
		if exclusive(t) {
			t.Error("a parallel subtest is exclusive")
		}
		t.Run("child", func(t *testing.T) {
			if exclusive(t) {
				t.Error("a sequential subtest of a parallel test is exclusive")
			}
		})
	})
}

func TestUnreachableFileIsForgiven(t *testing.T) {
	root := scratch(t)
	r := newRecorder(t)
	Track(r, root)
	path := filepath.Join(root, "dropped.txt")
	func() {
		// Opened and dropped: only its finalizer closes the descriptor.
		if _, err := os.Create(path); err != nil {
			t.Fatal(err)
		}
	}()
	runFinalizers()
	for fd, target := range selfFiles() {
		if target == path {
			t.Fatalf("fd %d of an unreachable file is still open after runFinalizers", fd)
		}
	}
	if report := r.finish(); report != "" {
		t.Errorf("unreachable file reported:\n%s", report)
	}
}

// TestKillAndReapLeavesAbandonedWaitItsStatus states the contract; the
// blocked waitid usually wins even without foreignReapWait.
func TestKillAndReapLeavesAbandonedWaitItsStatus(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	p, ok := readStat(cmd.Process.Pid)
	if !ok {
		t.Fatal("no /proc entry for the child")
	}
	if left := killAndReap([]process{p}); len(left) != 0 {
		t.Fatalf("not reaped: %v", left)
	}
	err := <-waited
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
		t.Fatalf("cmd.Wait = %v, want the SIGKILL status rather than ECHILD", err)
	}
}

type fakeTool struct {
	run func(context.Context, *circusv1.DeviceCall) (*circusv1.DeviceResult, error)
}

func (fakeTool) Name() string { return "read" }
func (f fakeTool) Run(ctx context.Context, call *circusv1.DeviceCall) (*circusv1.DeviceResult, error) {
	return f.run(ctx, call)
}

func TestCallAudit(t *testing.T) {
	var mu sync.Mutex
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	deadline := now.Add(time.Second)
	slow := fakeTool{func(context.Context, *circusv1.DeviceCall) (*circusv1.DeviceResult, error) {
		mu.Lock()
		now = deadline.Add(fallbackGrace)
		mu.Unlock()
		return &circusv1.DeviceResult{}, nil
	}}
	r := newRecorder(t)
	tools := AuditTools(r, clock, []Tool{slow})
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	call := &circusv1.DeviceCall{CallId: "twice"}
	for range 2 {
		if _, err := tools[0].Run(ctx, call); err != nil {
			t.Fatal(err)
		}
	}
	report := r.finish()
	want(t, report, `call_id "twice" executed 2 times`, "returned 5s after its deadline", "executions:")

	r = newRecorder(t)
	release, started := make(chan struct{}), make(chan struct{})
	stuck := AuditTools(r, nil, []Tool{fakeTool{func(context.Context, *circusv1.DeviceCall) (*circusv1.DeviceResult, error) {
		close(started)
		<-release
		return &circusv1.DeviceResult{}, nil
	}}})
	go func() { _, _ = stuck[0].Run(context.Background(), &circusv1.DeviceCall{CallId: "stuck"}) }()
	<-started
	AllowReexecution(r, "unused", "self-test")
	report = r.finish()
	close(release)
	want(t, report, `call "stuck" (read) is still executing`)
}

// named gives a fake test its own name; attribution compares test names.
type named struct {
	*recorder
	name string
}

func (n named) Name() string { return n.name }

// waitExeced waits until the process runs its own image.
func waitExeced(t *testing.T, pid int) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; {
		if p, ok := readStat(pid); ok && p.execed {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("process %d did not finish its exec", pid)
		}
		time.Sleep(pollInterval)
	}
}

// A process that holds another test's descriptor, here one passed to it,
// matches two tests that are not nested. That makes it ambiguous: it is
// pinned to no test until only its own test matches, and the other test's
// check neither reports nor kills it.
func TestAmbiguousProcessIsNotPinnedToAnotherTest(t *testing.T) {
	own, other := scratch(t), scratch(t)
	ownTest := named{newRecorder(t), "TestParent/short"}
	otherTest := named{newRecorder(t), "TestParent/a-longer-sibling-name"}
	Track(ownTest, own)
	Track(otherTest, other)
	foreign, err := os.Create(filepath.Join(other, "output"))
	if err != nil {
		t.Fatal(err)
	}
	// The shell holds the other test's file as fd 3 until it closes it and
	// execs, keeping its pid and group.
	cmd := exec.Command("/bin/sh", "-c", `read -r _; exec 3>&-; exec /bin/sleep 30`)
	cmd.Dir = own
	cmd.ExtraFiles = []*os.File{foreign}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	gate, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Close()
	err = cmd.Start()
	_ = foreign.Close()
	if err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	defer func() {
		_ = cmd.Process.Kill()
		<-waited
	}()
	// Until its exec has finished, the shell is not attributed at all.
	waitExeced(t, cmd.Process.Pid)
	shell, ok := readStat(cmd.Process.Pid)
	if !ok {
		t.Fatal("no /proc entry for the child")
	}
	sample()
	if owner := ownerOf(shell); owner != nil {
		t.Fatalf("ambiguous process pinned to %s", owner.state.t.Name())
	}
	if _, err := gate.Write([]byte("\n")); err != nil {
		t.Fatal(err)
	}
	// The shell closes fd 3, then execs sleep; while that exec runs, the
	// process is not attributed either.
	for deadline := time.Now().Add(5 * time.Second); cmdline(cmd.Process.Pid) != "/bin/sleep 30"; {
		if time.Now().After(deadline) {
			t.Fatalf("the shell did not exec sleep: %q", cmdline(cmd.Process.Pid))
		}
		time.Sleep(pollInterval)
	}
	waitExeced(t, cmd.Process.Pid)
	// sleep may open files of its own, such as the locale archive, as fd 3.
	for fd, target := range openFiles(cmd.Process.Pid) {
		if under(target, []string{other}) {
			t.Fatalf("the child kept the foreign descriptor as fd %d", fd)
		}
	}
	sample()
	if owner := ownerOf(shell); owner == nil || owner.state.t != ownTest {
		t.Fatalf("process not attributed to its own test: %v", owner)
	}
	if report := otherTest.finish(); report != "" {
		t.Fatalf("another test's check reported the process:\n%s", report)
	}
	if err := syscall.Kill(cmd.Process.Pid, 0); err != nil {
		t.Fatalf("another test's check killed the process: %v", err)
	}
	want(t, ownTest.finish(), "processes", fmt.Sprintf("pid=%d", cmd.Process.Pid), "sleep 30")
}

// reexecHomeEnv marks the test binary that TestReexecutedBinarySharesHome
// starts.
const reexecHomeEnv = "LEAKCHECK_TEST_REEXEC_HOME"

// A test binary re-executed by a test runs Run again: it must reuse the
// parent's isolated HOME and leave it in place when it finishes.
func TestReexecutedBinarySharesHome(t *testing.T) {
	home := os.Getenv("HOME")
	if os.Getenv(homeEnv) != home {
		t.Fatalf("%s = %q, want the isolated HOME %q", homeEnv, os.Getenv(homeEnv), home)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^TestReexecutedBinaryHomeChild$", "-test.count=1")
	cmd.Env = append(os.Environ(), reexecHomeEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "child home="+home+"\n") {
		t.Fatalf("child did not report the parent's HOME %s:\n%s", home, out)
	}
	if info, err := os.Stat(home); err != nil || !info.IsDir() {
		t.Fatalf("the child removed the parent's HOME: %v", err)
	}
}

func TestReexecutedBinaryHomeChild(t *testing.T) {
	if os.Getenv(reexecHomeEnv) == "" {
		t.Skip("run by TestReexecutedBinarySharesHome")
	}
	fmt.Printf("child home=%s\n", os.Getenv("HOME"))
}
