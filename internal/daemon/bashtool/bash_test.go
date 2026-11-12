package bashtool_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/daemon/bashtool"
	"github.com/EricWvi/circus/internal/testutil/leakcheck"
)

func TestEvalQuoting(t *testing.T) {
	t.Parallel()
	w := newWorker(t)
	cases := []struct{ command, want string }{
		{`printf '%s' 'single "double"'`, `single "double"`},
		{`printf '%s' '$HOME $(exit 19) \backslash'`, `$HOME $(exit 19) \backslash`},
		{`printf '%s' "apostrophe's"`, `apostrophe's`},
		{"printf '%s\\n' first\nprintf '%s' second", "first\nsecond"},
	}
	for _, tc := range cases {
		r := w.bashCall(t, tc.command, false, 0)
		if r.Output.IsError || r.Output.Content[0].GetText() != tc.want {
			t.Errorf("command %q: %v", tc.command, r)
		}
	}
}

func TestSnapshotRefreshesWhenStartupChanges(t *testing.T) {
	t.Parallel()
	w := newWorker(t)
	rc := filepath.Join(w.options.Home, ".bashrc")
	writeFile(t, rc, []byte("export SNAPSHOT_MARK=first\n"))
	r := w.bashCall(t, `echo "$SNAPSHOT_MARK"`, false, 0)
	if r.Output.Content[0].GetText() != "first" {
		t.Fatal(r)
	}
	original := w.snapshot
	writeFile(t, rc, []byte("export SNAPSHOT_MARK=second\n"))
	r = w.bashCall(t, `echo "$SNAPSHOT_MARK"`, false, 0)
	if r.Output.Content[0].GetText() != "second" || w.snapshot == original {
		t.Fatal("snapshot was not refreshed", r)
	}
	original = w.snapshot
	if err := os.Remove(original); err != nil {
		t.Fatal(err)
	}
	r = w.bashCall(t, `echo "$SNAPSHOT_MARK"`, false, 0)
	if r.Output.Content[0].GetText() != "second" || w.snapshot == original {
		t.Fatal("snapshot was not recreated", r)
	}
}

func TestBashDescriptionHasNoExecutionSemantics(t *testing.T) {
	t.Parallel()
	w := newWorker(t)
	const description = "informational only; touch description-command-ran"
	r, err := w.bash.Run(context.Background(), &circusv1.DeviceCall{CallId: "description", SessionId: "fixture-session", Tool: &circusv1.DeviceCall_Bash{Bash: &circusv1.BashRequest{Command: "printf executed", Description: description, Cwd: w.start}}})
	if err != nil {
		t.Fatal(err)
	}
	if r.Output.IsError || r.Output.Content[0].GetText() != "executed" {
		t.Fatal(r)
	}
	if _, err := os.Stat(filepath.Join(w.start, "description-command-ran")); !os.IsNotExist(err) {
		t.Fatal("description was executed", err)
	}
}

func TestInteractiveCollectorFallback(t *testing.T) {
	t.Parallel()
	w := newWorkerWithOptions(t, func(o *bashtool.Options) {
		writeFile(t, filepath.Join(o.Home, ".bash_profile"), []byte(`case $- in
 *i*) printf '%s\n' interactive >> "$HOME/attempts"; exit 7;;
 *) printf '%s\n' fallback >> "$HOME/attempts";;
esac
`))
		writeFile(t, filepath.Join(o.Home, ".bashrc"), []byte("export FALLBACK_MARK=ok\n"))
	})
	r := w.bashCall(t, `echo "$FALLBACK_MARK"`, false, 0)
	if r.Output.IsError || r.Output.Content[0].GetText() != "ok" {
		t.Fatal(r)
	}
	data, err := os.ReadFile(filepath.Join(w.options.Home, "attempts"))
	if err != nil || string(data) != "interactive\nfallback\n" {
		t.Fatalf("attempts=%q err=%v", data, err)
	}
}

func waitFile(t *testing.T, path string) []byte {
	t.Helper()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
				return data
			}
		case <-timer.C:
			t.Fatalf("file never became ready: %s", path)
			return nil
		}
	}
}

func TestTimeoutKillsProcessGroup(t *testing.T) {
	t.Parallel()
	w := newWorker(t)
	// Prime the snapshot so the deadline measures execution, not shell startup.
	w.bashCall(t, "true", false, 0)
	// Waiting for the explicit child in the TERM trap avoids Bash wait reentry
	// errors and reaps it, so ESRCH proves the sleep is gone
	// even on hosts whose PID 1 does not promptly reap adopted zombies.
	command := `sleep 0; trap 'wait "$child" 2>/dev/null; exit 143' TERM; sleep 30 & child=$!; echo $child > child.pid; wait "$child"`
	r := w.bashCall(t, command, false, 600)
	if !r.GetBash().TimedOut || r.GetBash().ExitCode != 143 || !r.Output.IsError {
		t.Fatal(r)
	}
	pidData, err := os.ReadFile(filepath.Join(w.start, "child.pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidData)))
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("child %d remains after timeout: %v", pid, err)
	}
}

func TestCancelPreservesOutput(t *testing.T) {
	t.Parallel()
	w := newWorker(t)
	w.bashCall(t, "true", false, 0)
	// Exit directly in the TERM trap: reentering a no-argument wait from a
	// suspended wait can loop in Bash 5.3 when the child is already reaped.
	call := &circusv1.DeviceCall{CallId: "cancel", SessionId: "fixture-session", Tool: &circusv1.DeviceCall_Bash{Bash: &circusv1.BashRequest{Command: `trap 'exit' TERM; echo before; sleep 30 & echo ready > ready; wait`, Cwd: w.start}}}
	if _, err := w.d.Submit(call); err != nil {
		t.Fatal(err)
	}
	waitFile(t, filepath.Join(w.start, "ready"))
	if !w.d.Cancel(call.CallId) {
		t.Fatal("cancel did not find running call")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	status, err := w.d.Wait(ctx, call.CallId)
	if err != nil {
		t.Fatal(err)
	}
	r := status.Result
	if !r.GetBash().Interrupted || !r.Output.IsError || r.Output.Content[0].GetText() != "before\nCommand was interrupted" {
		t.Fatal(r)
	}
}

func TestBackgroundSurvivesCallContext(t *testing.T) {
	t.Parallel()
	w := newWorker(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r, err := w.bash.Run(ctx, &circusv1.DeviceCall{CallId: "background", SessionId: "fixture-session", Tool: &circusv1.DeviceCall_Bash{Bash: &circusv1.BashRequest{Command: `echo ready > ready; while [ ! -f release ]; do sleep 0.01; done; echo survived`, Cwd: w.start, RunInBackground: true}}})
	if err != nil {
		t.Fatal(err)
	}
	leakcheck.Expect(r.GetBash().GetOutputPath())
	waitFile(t, filepath.Join(w.start, "ready"))
	cancel()
	writeFile(t, filepath.Join(w.start, "release"), []byte("go"))
	e := w.waitJob(t, r.GetBash().JobId)
	if e.ExitCode != 0 || e.State != circusv1.JobEvent_STATE_EXITED || e.CallId != "background" || e.Seq < 2 {
		t.Fatal(e)
	}
	data, err := os.ReadFile(r.GetBash().OutputPath)
	if err != nil || string(data) != "survived\n\n[exited with code 0]\n" {
		t.Fatalf("output=%q err=%v", data, err)
	}
}

func TestStopSessionAndOwnership(t *testing.T) {
	t.Parallel()
	w := newWorker(t)
	r := w.bashCall(t, `trap 'exit 143' TERM; sleep 30 & echo ready > ready; wait`, true, 0)
	id := r.GetBash().JobId
	waitFile(t, filepath.Join(w.start, "ready"))
	other := w.call(t, &circusv1.DeviceCall{SessionId: "other-session", Tool: &circusv1.DeviceCall_TaskStop{TaskStop: &circusv1.TaskStopRequest{JobId: id}}})
	if !other.Output.IsError || other.Output.Content[0].GetText() != "<tool_use_error>No task found with ID: "+id+"</tool_use_error>" {
		t.Fatal(other)
	}
	if info, ok := w.jobs.Get(id); !ok || info.State != circusv1.JobEvent_STATE_RUNNING {
		t.Fatalf("foreign stop affected job: %+v", info)
	}
	w.jobs.StopSession("fixture-session")
	e := w.waitJob(t, id)
	if e.State != circusv1.JobEvent_STATE_KILLED {
		t.Fatal(e)
	}
	data, err := os.ReadFile(r.GetBash().OutputPath)
	if err != nil || string(data) != "\n[exited with code 143]\n" {
		t.Fatalf("output=%q err=%v", data, err)
	}
}

func TestSessionsUnderTTY(t *testing.T) {
	if os.Getenv("CIRCUS_TTY_TEST") == "1" {
		w := newWorker(t)
		writeFile(t, filepath.Join(w.options.Home, ".bashrc"), []byte("case $- in *i*) export TTY_SNAPSHOT=interactive;; *) export TTY_SNAPSHOT=fallback;; esac\n"))
		r := w.bashCall(t, `printf '%s' "$TTY_SNAPSHOT"`, false, 0)
		if got := r.Output.Content[0].GetText(); got != "interactive" {
			t.Fatalf("collector degraded under controlling tty: %q", got)
		}
		start := time.Now()
		r = w.bashCall(t, `read x < /dev/tty`, false, 2000)
		if !r.Output.IsError || r.GetBash().TimedOut || r.GetBash().JobId != "" || time.Since(start) > time.Second {
			t.Fatalf("/dev/tty did not fail fast: %v", r)
		}
		return
	}
	t.Parallel()
	script, err := exec.LookPath("script")
	if err != nil {
		t.Skip("pseudo-terminal regression requires script -qec; script is unavailable")
	}
	if output, err := exec.Command(script, "-qec", "true", "/dev/null").CombinedOutput(); err != nil {
		t.Skipf("script -qec is unavailable on this platform: %v: %s", err, output)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, script, "-qec", "'"+strings.ReplaceAll(binary, "'", "'\\''")+"' -test.run '^TestSessionsUnderTTY$' -test.v", "/dev/null")
	cmd.Env = append(os.Environ(), "CIRCUS_TTY_TEST=1")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("pseudo-terminal regression: %v\n%s", err, output)
	}
}

func TestLargeOutputMemoryBound(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("memory regression is measured on Linux; macOS accounting is unreliable")
	}
	w := newWorker(t)
	leakcheck.DiskLimit(t, 256<<20, "the 200 MB persisted output is the subject of this test")
	w.bashCall(t, "true", false, 0)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	r := w.bashCall(t, "yes | head -c 200000000", false, 0)
	runtime.ReadMemStats(&after)
	growth := after.TotalAlloc - before.TotalAlloc
	if growth > 16*1024*1024 {
		t.Fatalf("200 MB output allocated %d bytes in the daemon", growth)
	}
	if r.Output.IsError || !r.GetBash().OutputTruncated || !strings.HasPrefix(r.Output.Content[0].GetText(), "<persisted-output>") {
		t.Fatal(r)
	}
	stat, err := os.Stat(r.GetBash().OutputPath)
	if err != nil || stat.Size() != 200000000 {
		t.Fatalf("persisted output: %v, %v", stat, err)
	}
	t.Logf("200 MB output: daemon allocated %d bytes", growth)
}

func TestBackgroundLifetimeKilled(t *testing.T) {
	t.Parallel()
	w := newWorker(t)
	w.bashCall(t, "true", false, 0)
	r := w.bashCall(t, "sleep 20", true, 100)
	e := w.waitJob(t, r.GetBash().JobId)
	if e.State != circusv1.JobEvent_STATE_KILLED || e.ExitCode != 143 {
		t.Fatal(e)
	}
	ended := w.call(t, &circusv1.DeviceCall{Tool: &circusv1.DeviceCall_TaskStop{TaskStop: &circusv1.TaskStopRequest{JobId: e.JobId}}})
	if !ended.Output.IsError {
		t.Fatal(ended)
	}
}

func TestCwdValidation(t *testing.T) {
	t.Parallel()
	for _, cwd := range []string{"", "relative", "/nonexistent-circus-start-folder"} {
		t.Run(cwd, func(t *testing.T) {
			w := newWorker(t)
			r := w.call(t, &circusv1.DeviceCall{Tool: &circusv1.DeviceCall_Bash{Bash: &circusv1.BashRequest{Command: "pwd", Cwd: cwd}}})
			if !r.Output.IsError {
				t.Fatal(r)
			}
			if cwd == "" && r.Output.Content[0].GetText() != "Error: cwd is required" {
				t.Fatal(r)
			}
		})
	}
}

func TestWrapperRemovesNativeAliasesAndFunctions(t *testing.T) {
	t.Parallel()
	w := newWorker(t)
	writeFile(t, filepath.Join(w.options.Home, ".bashrc"), []byte("which() { echo bad; }; unsetenv() { echo bad; }; alias which='echo bad'; alias unsetenv='echo bad'\n"))
	r := w.bashCall(t, `if declare -F which >/dev/null || declare -F unsetenv >/dev/null || alias which >/dev/null 2>&1 || alias unsetenv >/dev/null 2>&1; then echo leaked; else echo clean; fi`, false, 0)
	if r.Output.IsError || r.Output.Content[0].GetText() != "clean" {
		t.Fatal(r)
	}
}

func TestBackgroundLifetimeTrapStillKilled(t *testing.T) {
	t.Parallel()
	for _, code := range []int{0, 7} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			t.Parallel()
			w := newWorker(t)
			w.bashCall(t, "true", false, 0)
			r := w.bashCall(t, "trap 'exit "+strconv.Itoa(code)+"' TERM; sleep 20 & wait", true, 300)
			e := w.waitJob(t, r.GetBash().JobId)
			if e.State != circusv1.JobEvent_STATE_KILLED || e.ExitCode != int32(code) {
				t.Fatalf("lifetime exit trap: %v", e)
			}
		})
	}
}

func TestCollectorTimeDoesNotConsumeCommandTimeout(t *testing.T) {
	t.Parallel()
	started := make(chan struct{}, 1)
	w := newWorkerWithOptions(t, func(o *bashtool.Options) {
		o.Collector = func(ctx context.Context, cmd *exec.Cmd) error {
			select {
			case started <- struct{}{}:
			default:
			}
			timer := time.NewTimer(1200 * time.Millisecond)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
			}
			if err := cmd.Start(); err != nil {
				return err
			}
			return cmd.Wait()
		}
	})
	// Daemon startup explicitly warms collection, before any Bash call.
	w.bash.Warm()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("snapshot warm-up did not start")
	}
	r := w.bashCall(t, `sleep 0.8; echo done`, false, 1000)
	if r.Output.IsError || r.GetBash().TimedOut || r.Output.Content[0].GetText() != "done" {
		t.Fatal(r)
	}
	// A later refresh gets the same separation from the execution budget.
	writeFile(t, filepath.Join(w.options.Home, ".bashrc"), []byte("export REFRESH=1\n"))
	r = w.bashCall(t, `sleep 0.8; echo done`, false, 1000)
	if r.Output.IsError || r.Output.Content[0].GetText() != "done" {
		t.Fatal(r)
	}
}

func TestPromotionDrainsDeadlineOutput(t *testing.T) {
	t.Parallel()
	w := newWorker(t)
	// Prime the snapshot so the deadline measures execution, not shell startup.
	w.bashCall(t, "true", false, 0)
	r := w.bashCall(t, `echo 1; sleep 1; echo 2; sleep 3`, false, 1000)
	if r.Output.IsError || r.GetBash().JobId == "" || !strings.HasPrefix(r.Output.Content[0].GetText(), "1\n2\nCommand did not complete") {
		t.Fatal(r)
	}
}

func TestDispatcherShutdownStopsInternalGroups(t *testing.T) {
	t.Parallel()
	w := newWorker(t)
	r := w.bashCall(t, `sleep 20 & echo $! > child.pid; echo x`, false, 0)
	if r.Output.Content[0].GetText() != "x" || r.GetBash().JobId != "" {
		t.Fatal(r)
	}
	data, err := os.ReadFile(filepath.Join(w.start, "child.pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	if err := w.d.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
		t.Fatal("dispatcher shutdown left an internal group", err)
	}
}

func TestHangingStartupSharesWarmFailure(t *testing.T) {
	t.Parallel()
	w := newWorkerWithOptions(t, func(o *bashtool.Options) {
		o.CollectorTimeout = 150 * time.Millisecond
		writeFile(t, filepath.Join(o.Home, ".bashrc"), []byte(`printf x >> "$HOME/attempts"; sleep 5`))
	})
	w.bash.Warm()
	waitFile(t, filepath.Join(w.options.Home, "attempts"))
	started := time.Now()
	r := w.bashCall(t, "echo must-not-run", false, 1000)
	body := r.Output.Content[0].GetText()
	if !r.Output.IsError || !strings.Contains(body, "collect bash environment: context deadline exceeded") || strings.Contains(body, "tool did not stop") {
		t.Fatal(r)
	}
	if time.Since(started) > time.Second {
		t.Fatal("waiter started a second collection")
	}
	data, err := os.ReadFile(filepath.Join(w.options.Home, "attempts"))
	if err != nil || string(data) != "xx" {
		t.Fatalf("collection attempts=%q err=%v", data, err)
	}
	// An independent cold call can retry, rather than caching a failure forever.
	r = w.bashCall(t, "echo must-not-run", false, 1000)
	data, err = os.ReadFile(filepath.Join(w.options.Home, "attempts"))
	if !r.Output.IsError || err != nil || string(data) != "xxxx" {
		t.Fatalf("retry=%v attempts=%q err=%v", r, data, err)
	}
}
