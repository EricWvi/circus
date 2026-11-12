package bashtool

import (
	"context"
	"errors"
	"os"
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
	b.outputs.mu.Lock()
	defer b.outputs.mu.Unlock()
	if len(b.outputs.active) != 1 {
		t.Fatalf("active outputs: %v", b.outputs.active)
	}
	for path := range b.outputs.active {
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
	b.outputs.sweepOutput(b.outputs.outputRoot(), time.Now())
	// A second daemon uses the same root but does not share the active map.
	other := &outputStore{options: b.outputs.options}
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
			// Prime the snapshot so the short caller deadline measures execution,
			// not cold shell-environment collection.
			reviewRun(t, b, root, "true", false)
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			command := `echo outer; sleep 20 & echo $! > child.pid; echo x`
			if !background {
				command = `echo outer; sleep 0.35; sleep 20 & echo $! > child.pid; echo x`
			}
			r, err := b.Run(ctx, &circusv1.DeviceCall{CallId: "leftovers", TimeoutMs: 100, SessionId: "review", Tool: &circusv1.DeviceCall_Bash{Bash: &circusv1.BashRequest{Cwd: root, Command: command, RunInBackground: background}}})
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
