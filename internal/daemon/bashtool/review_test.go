package bashtool

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/daemon/jobs"
	"github.com/EricWvi/circus/internal/testutil/leakcheck"
)

func reviewBash(t *testing.T, limit int64) (*Bash, string, <-chan *circusv1.JobEvent) {
	t.Helper()
	return reviewBashWithLimits(t, limit, 0)
}

func reviewBashWithLimits(t *testing.T, limit, totalLimit int64) (*Bash, string, <-chan *circusv1.JobEvent) {
	t.Helper()
	root := unitTempDir(t)
	leakcheck.OutputRoot(t, filepath.Join(root, fmtUID()))
	leakcheck.StateDir(t, filepath.Join(root, "state"))
	events := make(chan *circusv1.JobEvent, 32)
	manager := jobs.NewManager(jobs.Options{OnEvent: func(e *circusv1.JobEvent) { events <- e }})
	b := NewBash(manager, Options{Home: root, StateDir: filepath.Join(root, "state"), TempDir: root, Shell: "/bin/bash", OutputLimitBytes: limit, TotalOutputLimitBytes: totalLimit})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := manager.Shutdown(ctx); err != nil {
			t.Error(err)
		}
		if err := b.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	return b, root, events
}

func reviewRun(t *testing.T, b *Bash, cwd, command string, background bool) *circusv1.DeviceResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	r, err := b.Run(ctx, &circusv1.DeviceCall{SessionId: "review", CallId: "call", Tool: &circusv1.DeviceCall_Bash{Bash: &circusv1.BashRequest{Command: command, Cwd: cwd, RunInBackground: background}}})
	if err != nil {
		t.Fatal(err)
	}
	leakcheck.Expect(r.GetBash().GetOutputPath())
	return r
}

func trackedPath(t *testing.T, b *Bash) string {
	t.Helper()
	b.environment.mu.Lock()
	defer b.environment.mu.Unlock()
	if len(b.environment.active) != 1 {
		t.Fatalf("active outputs: %v", b.environment.active)
	}
	for path := range b.environment.active {
		return path
	}
	return ""
}

func childPID(t *testing.T, root string) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "child.pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

func TestTrackedGroupNaturalExit(t *testing.T) {
	t.Parallel()
	b, root, events := reviewBash(t, 0)
	r := reviewRun(t, b, root, `(while [ ! -f release ]; do sleep 0.02; done; echo later) & echo $! > child.pid; echo x`, false)
	if r.Output.IsError || r.Output.Content[0].GetText() != "x" || r.GetBash().JobId != "" {
		t.Fatal(r)
	}
	pid := childPID(t, root)
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatal("child did not survive result", err)
	}
	path := trackedPath(t, b)
	if _, err := os.Stat(path); err != nil {
		t.Fatal("live group's file was unlinked", err)
	}
	old := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	b.environment.sweepOutput(b.environment.outputRoot(), time.Now())
	// A second daemon uses the same root but does not share the active map.
	other := &environment{options: b.environment.options}
	other.sweepOutput(other.outputRoot(), time.Now())
	if _, err := os.Stat(path); err != nil {
		t.Fatal("sweep removed tracked output", err)
	}
	if err := os.WriteFile(filepath.Join(root, "release"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	await(t, func() bool { _, err := os.Stat(path); return os.IsNotExist(err) })
	await(t, func() bool { return syscall.Kill(pid, 0) == syscall.ESRCH })
	select {
	case event := <-events:
		t.Fatalf("internal group emitted event: %v", event)
	default:
	}
}

func TestTrackedGroupShutdown(t *testing.T) {
	t.Parallel()
	b, root, events := reviewBash(t, 0)
	reviewRun(t, b, root, `sleep 20 & echo $! > child.pid; echo x`, false)
	pid, path := childPID(t, root), trackedPath(t, b)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := b.Shutdown(ctx); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := b.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	await(t, func() bool { return syscall.Kill(pid, 0) == syscall.ESRCH })
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("shutdown retained inline output", err)
	}
	select {
	case event := <-events:
		t.Fatalf("internal group emitted event: %v", event)
	default:
	}
}

func outputTail(t *testing.T, file *os.File) string {
	t.Helper()
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	offset := max(int64(0), info.Size()-256)
	data := make([]byte, info.Size()-offset)
	if _, err := file.ReadAt(data, offset); err != nil && err != io.EOF {
		t.Fatal(err)
	}
	return string(data)
}

func TestTrackedGroupOutputCap(t *testing.T) {
	t.Parallel()
	b, root, _ := reviewBash(t, 1024)
	// The release gate makes the first body deterministic, then starts a fast writer.
	r := reviewRun(t, b, root, `(while [ ! -f release ]; do sleep 0.01; done; exec yes) & echo $! > child.pid; echo x`, false)
	if r.Output.Content[0].GetText() != "x" {
		t.Fatal(r)
	}
	path, pid := trackedPath(t, b), childPID(t, root)
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := os.WriteFile(filepath.Join(root, "release"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	await(t, func() bool { _, err := os.Stat(path); return os.IsNotExist(err) })
	await(t, func() bool { return syscall.Kill(pid, 0) == syscall.ESRCH })
	if tail := outputTail(t, file); !strings.HasSuffix(tail, outputLimitMessage) || strings.Count(tail, outputLimitMessage) != 1 {
		t.Fatalf("missing unique cap diagnostic: %q", tail)
	}
}

func TestForegroundAndBackgroundOutputCap(t *testing.T) {
	for _, background := range []bool{false, true} {
		t.Run(strconv.FormatBool(background), func(t *testing.T) {
			t.Parallel()
			b, root, events := reviewBash(t, 1024)
			leakcheck.DiskLimit(t, 2<<30, "documented overshoot: an `exec yes` writer outruns the 100 ms cap poll by hundreds of MiB")
			r := reviewRun(t, b, root, `exec yes`, background)
			path := r.GetBash().OutputPath
			if background {
				event := reviewEvent(t, events, true)
				if event.State != circusv1.JobEvent_STATE_KILLED {
					t.Fatal(event)
				}
			} else if !r.Output.IsError || r.GetBash().ExitCode != 137 || !r.GetBash().OutputTruncated {
				t.Fatal(r)
			}
			file, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			if tail := outputTail(t, file); !strings.HasSuffix(tail, outputLimitMessage) {
				t.Fatalf("cap diagnostic: %q", tail)
			}
		})
	}
}

func TestSnapshotInstancesShareStateSafely(t *testing.T) {
	t.Parallel()
	b, root, _ := reviewBash(t, 0)
	snapshot, _, err := b.environment.load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer b.environment.releaseSnapshot(snapshot)
	// Hold an actual command lease while a second daemon creates its snapshot.
	call := &circusv1.DeviceCall{Tool: &circusv1.DeviceCall_Bash{Bash: &circusv1.BashRequest{Command: `echo ready > ready; while [ ! -f release ]; do sleep 0.02; done; echo A-done`, Cwd: root}}}
	returned := make(chan *circusv1.DeviceResult, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	go func() { r, _ := b.Run(ctx, call); returned <- r }()
	await(t, func() bool { _, err := os.Stat(filepath.Join(root, "ready")); return err == nil })
	second := &environment{options: b.environment.options}
	path, _, err := second.load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second.releaseSnapshot(path)
	if filepath.Dir(filepath.Dir(path)) == filepath.Dir(filepath.Dir(snapshot)) {
		t.Fatal("instances share a directory")
	}
	if _, err := os.Stat(snapshot); err != nil {
		t.Fatal("second instance removed live snapshot", err)
	}
	if err := os.WriteFile(filepath.Join(root, "release"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-returned:
		if r == nil || r.Output.IsError || r.Output.Content[0].GetText() != "A-done" {
			t.Fatal(r)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("first daemon did not finish")
	}
	// Releasing only the second instance lock permits the next startup to clean
	// that instance, while the first instance remains protected by its lease.
	// Unlock explicitly, like a process exit: Close alone keeps the lock while a
	// concurrently forked child of this process still shares the descriptor.
	if err := unlockAndClose(second.instanceLock); err != nil {
		t.Fatal(err)
	}
	third := &environment{options: b.environment.options}
	t.Cleanup(third.releaseInstance)
	newPath, _, err := third.load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	third.releaseSnapshot(newPath)
	if _, err := os.Stat(second.instanceDir); !os.IsNotExist(err) {
		t.Fatal("unlocked instance not cleaned", err)
	}
	if _, err := os.Stat(snapshot); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotRefreshFailureKeepsOld(t *testing.T) {
	t.Parallel()
	root := unitTempDir(t)
	attempts := 0
	e := &environment{options: Options{Home: root, StateDir: filepath.Join(root, "state"), Shell: "/bin/bash", Collector: func(ctx context.Context, cmd *exec.Cmd) error {
		attempts++
		if err := cmd.Start(); err != nil {
			return err
		}
		_, err := waitProcess(ctx, cmd)
		return err
	}}}
	t.Cleanup(e.releaseInstance)
	rc := filepath.Join(root, ".bashrc")
	if err := os.WriteFile(rc, []byte("export MARK=old\n"), 0600); err != nil {
		t.Fatal(err)
	}
	old, _, err := e.load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	e.releaseSnapshot(old)
	if err := os.WriteFile(rc, []byte("exit 3\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		path, env, err := e.load(context.Background())
		if err != nil || path != old || !strings.Contains(strings.Join(env, "\n"), "MARK=old") {
			t.Fatalf("lost old snapshot: %s %v %v", path, env, err)
		}
		e.releaseSnapshot(path)
	}
	if attempts != 3 {
		t.Fatalf("retried unchanged failed startup: attempts=%d", attempts)
	}
	if err := os.WriteFile(rc, []byte("export MARK=new\n"), 0600); err != nil {
		t.Fatal(err)
	}
	path, env, err := e.load(context.Background())
	if err != nil || path == old || !strings.Contains(strings.Join(env, "\n"), "MARK=new") {
		t.Fatalf("did not retry changed startup: %s %v %v", path, env, err)
	}
	e.releaseSnapshot(path)
	if attempts != 4 {
		t.Fatal(attempts)
	}
	// No usable snapshot must still fail, and be retried by the next call.
	if err := os.WriteFile(rc, []byte("exit 3\n"), 0600); err != nil {
		t.Fatal(err)
	}
	fresh := &environment{options: e.options}
	t.Cleanup(fresh.releaseInstance)
	for range 2 {
		if _, _, err := fresh.load(context.Background()); err == nil {
			t.Fatal("cold failure succeeded")
		}
	}
	if attempts != 8 {
		t.Fatal(attempts)
	}
}

func TestSweepErrorsAreBestEffort(t *testing.T) {
	t.Parallel()
	b, _, _ := reviewBash(t, 0)
	locked := filepath.Join(b.environment.outputRoot(), "locked")
	if err := os.MkdirAll(locked, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(locked, 0700)
	// Explicitly exercise walking beyond an inaccessible directory.
	dir := filepath.Join(b.environment.outputRoot(), "z", "tasks")
	if err := privateDir(dir); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, "stale")
	if err := os.WriteFile(stale, nil, 0600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	// Scheduling must actually run the sweep and continue after the locked
	// directory; merely advancing lastSweep cannot make this assertion pass.
	out, err := b.outputFile("review", "tool-results", "sweep-*")
	if err != nil {
		t.Fatal(err)
	}
	leakcheck.Expect(out.Name()) // created by the test itself
	defer out.Close()
	defer b.environment.releaseOutput(out.Name())
	await(t, func() bool { _, err := os.Stat(stale); return os.IsNotExist(err) })
}

func TestPromotionRegistrationFailureCleansOutput(t *testing.T) {
	t.Parallel()
	b, root, _ := reviewBash(t, 0)
	reviewRun(t, b, root, "true", false)
	if err := b.jobs.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := b.Run(ctx, &circusv1.DeviceCall{SessionId: "review", Tool: &circusv1.DeviceCall_Bash{Bash: &circusv1.BashRequest{Cwd: root, Command: `echo started; exec sleep 20`}}})
	if !errors.Is(err, jobs.ErrShutdown) {
		t.Fatal(err)
	}
	paths, err := filepath.Glob(filepath.Join(b.environment.outputRoot(), "review", "tool-results", "*"))
	if err != nil || len(paths) != 0 {
		t.Fatalf("failed promotion retained output: %v %v", paths, err)
	}
	b.environment.mu.Lock()
	defer b.environment.mu.Unlock()
	if len(b.environment.active) != 0 {
		t.Fatal(b.environment.active)
	}
}

func reviewEvent(t *testing.T, events <-chan *circusv1.JobEvent, ended bool) *circusv1.JobEvent {
	t.Helper()
	timer := time.NewTimer(4 * time.Second)
	defer timer.Stop()
	for {
		select {
		case event := <-events:
			if (event.State != circusv1.JobEvent_STATE_RUNNING) == ended {
				return event
			}
		case <-timer.C:
			t.Fatal("job event did not arrive")
			return nil
		}
	}
}

func TestBackgroundAndPromotedLeftovers(t *testing.T) {
	for _, background := range []bool{false, true} {
		t.Run(strconv.FormatBool(background), func(t *testing.T) {
			t.Parallel()
			b, root, events := reviewBash(t, 0)
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			command := `echo outer; sleep 20 & echo $! > child.pid; echo x`
			if !background {
				command = `echo outer; sleep 0.35; sleep 20 & echo $! > child.pid; echo x`
			}
			r, err := b.Run(ctx, &circusv1.DeviceCall{TimeoutMs: 100, SessionId: "review", Tool: &circusv1.DeviceCall_Bash{Bash: &circusv1.BashRequest{Cwd: root, Command: command, RunInBackground: background}}})
			if err != nil || r.GetBash().JobId == "" {
				t.Fatalf("%v %v", r, err)
			}
			leakcheck.Expect(r.GetBash().GetOutputPath())
			event := reviewEvent(t, events, true)
			if event.JobId != r.GetBash().JobId || event.State != circusv1.JobEvent_STATE_EXITED {
				t.Fatal(event)
			}
			pid := childPID(t, root)
			// The original background lifetime must not become a child lifetime.
			time.Sleep(200 * time.Millisecond)
			if err := syscall.Kill(pid, 0); err != nil {
				t.Fatal("leftover did not survive outer job", err)
			}
			if _, err := os.Stat(trackedPath(t, b)); err != nil {
				t.Fatal(err)
			}
			select {
			case event := <-events:
				t.Fatalf("extra child job event: %v", event)
			default:
			}
		})
	}
}

func TestForegroundInlineOutputCap(t *testing.T) {
	t.Parallel()
	b, root, _ := reviewBash(t, 1024)
	r := reviewRun(t, b, root, `printf '%02000d' 0; exec sleep 20`, false)
	if !r.Output.IsError || r.GetBash().ExitCode != 137 || !strings.HasSuffix(r.Output.Content[0].GetText(), strings.TrimSpace(outputLimitMessage)) {
		t.Fatal(r)
	}
	if r.GetBash().OutputTruncated {
		t.Fatal("small output should use normal inline body")
	}
}

func TestConcurrentRegistrationRejectionCleansOutput(t *testing.T) {
	t.Parallel()
	b, root, _ := reviewBash(t, 0)
	if err := b.jobs.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	allocated := make(chan struct{})
	manager := jobs.NewManager(jobs.Options{NewID: func() string { close(allocated); return "rejected" }})
	b.jobs = manager
	t.Cleanup(func() { _ = manager.Shutdown(context.Background()) })
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	returned := make(chan error, 1)
	go func() {
		_, err := b.Run(ctx, &circusv1.DeviceCall{SessionId: "review", Tool: &circusv1.DeviceCall_Bash{Bash: &circusv1.BashRequest{Cwd: root, Command: `echo ready > ready; exec sleep 20`}}})
		returned <- err
	}()
	await(t, func() bool { _, err := os.Stat(filepath.Join(root, "ready")); return err == nil })
	// Block newBackground's snapshot retain inside Manager.Start's callback.
	b.environment.mu.Lock()
	unlocked := false
	defer func() {
		if !unlocked {
			b.environment.mu.Unlock()
		}
	}()
	select {
	case <-allocated:
	case <-time.After(3 * time.Second):
		t.Fatal("promotion did not reach launch")
	}
	shutdownCtx, stopWaiting := context.WithCancel(context.Background())
	stopWaiting()
	_ = manager.Shutdown(shutdownCtx)
	b.environment.mu.Unlock()
	unlocked = true
	select {
	case err := <-returned:
		if !errors.Is(err, jobs.ErrShutdown) {
			t.Fatal(err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("rejected promotion did not finish")
	}
	paths, err := filepath.Glob(filepath.Join(b.environment.outputRoot(), "review", "tool-results", "*"))
	if err != nil || len(paths) != 0 {
		t.Fatalf("rejected launch retained output: %v %v", paths, err)
	}
}

func TestTrackedGroupShutdownEscalatesToKill(t *testing.T) {
	t.Parallel()
	b, root, _ := reviewBash(t, 0)
	reviewRun(t, b, root, `bash -c 'trap "" TERM; echo ready > ready; exec sleep 20' & echo $! > child.pid; echo x`, false)
	await(t, func() bool { _, err := os.Stat(filepath.Join(root, "ready")); return err == nil })
	pid := childPID(t, root)
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	if err := b.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if time.Since(started) < 1900*time.Millisecond {
		t.Fatal("did not allow the two-second TERM grace period")
	}
	if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
		t.Fatal("TERM-ignoring child survived KILL", err)
	}
}

func TestOutputCapKillsTERMIgnoringWriters(t *testing.T) {
	for _, mode := range []string{"foreground", "background", "tracked"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			b, root, events := reviewBash(t, 4<<20)
			writer := `trap '' TERM; while :; do head -c 1048576 /dev/zero; sleep 0.05; done`
			command := writer
			if mode == "tracked" {
				command = `(while [ ! -f release ]; do sleep 0.01; done; ` + writer + `) & echo x`
			}
			started := time.Now()
			r := reviewRun(t, b, root, command, mode == "background")
			path := r.GetBash().OutputPath
			if mode == "tracked" {
				path = trackedPath(t, b)
			}
			out, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer out.Close()
			if mode == "tracked" {
				started = time.Now()
				if err := os.WriteFile(filepath.Join(root, "release"), nil, 0600); err != nil {
					t.Fatal(err)
				}
				await(t, func() bool { _, err := os.Stat(path); return os.IsNotExist(err) })
			} else if mode == "background" {
				if event := reviewEvent(t, events, true); event.State != circusv1.JobEvent_STATE_KILLED || event.ExitCode != 137 {
					t.Fatal(event)
				}
			} else if r.GetBash().ExitCode != 137 {
				t.Fatal(r)
			}
			if time.Since(started) >= 2*time.Second {
				t.Fatal("output cap allowed the TERM grace period")
			}
			info, err := out.Stat()
			if err != nil || info.Size() > 16<<20 {
				t.Fatalf("output overshot during TERM grace: %v %v", info, err)
			}
			if !strings.HasSuffix(outputTail(t, out), outputLimitMessage) {
				t.Fatal("missing output-cap diagnostic")
			}
		})
	}
}

func TestShutdownReleasesInstance(t *testing.T) {
	t.Parallel()
	b, root, events := reviewBash(t, 0)
	job := reviewRun(t, b, root, `while [ ! -f release ]; do sleep 0.02; done`, true)
	if job.GetBash().JobId == "" {
		t.Fatal(job)
	}
	b.environment.mu.Lock()
	dir, lock := b.environment.instanceDir, b.environment.instanceLock
	b.environment.mu.Unlock()
	if err := b.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	// A visible job still sources and writes files in the instance.
	if _, err := os.Stat(dir); err != nil {
		t.Fatal("instance released while a background job uses it", err)
	}
	if _, err := lock.Stat(); err != nil {
		t.Fatal("instance lock closed while a background job uses it", err)
	}
	if err := os.WriteFile(filepath.Join(root, "release"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	reviewEvent(t, events, true)
	await(t, func() bool { _, err := os.Stat(dir); return os.IsNotExist(err) })
	if _, err := lock.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatal("instance lock still open after release", err)
	}
	// A shared environment stays usable: the next load locks a new instance.
	t.Cleanup(b.environment.releaseInstance)
	snapshot, _, err := b.environment.load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b.environment.releaseSnapshot(snapshot)
	if next := filepath.Dir(filepath.Dir(snapshot)); next == dir || filepath.Dir(next) != filepath.Dir(dir) {
		t.Fatalf("reload used %s after releasing %s", next, dir)
	}
}
