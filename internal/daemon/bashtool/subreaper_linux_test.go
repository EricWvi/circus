package bashtool

import (
	"context"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestSubreaperReapsTrackedGroups(t *testing.T) {
	if os.Getenv("CIRCUS_SUBREAPER_TEST") != "1" {
		t.Parallel()
		binary, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, "-test.run=^TestSubreaperReapsTrackedGroups$", "-test.v")
		cmd.Env = append(os.Environ(), "CIRCUS_SUBREAPER_TEST=1")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("subreaper subprocess: %v\n%s", err, output)
		}
		return
	}
	// This setting affects only this subprocess, never the parent test runner.
	const prSetChildSubreaper = 36
	if _, _, errno := syscall.Syscall6(syscall.SYS_PRCTL, prSetChildSubreaper, 1, 0, 0, 0, 0); errno != 0 {
		t.Fatal(errno)
	}
	for _, natural := range []bool{true, false} {
		t.Run(map[bool]string{true: "natural", false: "shutdown"}[natural], func(t *testing.T) {
			b, root, _ := reviewBash(t, 0)
			command := `sleep 20 & echo $! > child.pid; echo x`
			if natural {
				command = `sleep 0.3 & echo $! > child.pid; echo x`
			}
			r := reviewRun(t, b, root, command, false)
			if r.Output.IsError || r.Output.Content[0].GetText() != "x" {
				t.Fatal(r)
			}
			pid, path := childPID(t, root), trackedPath(t, b)
			if natural {
				await(t, func() bool { _, err := os.Stat(path); return os.IsNotExist(err) })
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := b.Shutdown(ctx); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
				t.Fatalf("adopted child %d was not reaped: %v", pid, err)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatal("group output retained", err)
			}
			b.environment.mu.Lock()
			defer b.environment.mu.Unlock()
			if b.environment.active[path] {
				t.Fatal("output lease retained")
			}
		})
	}
}

func TestGroupEmptyWaitIsBounded(t *testing.T) {
	t.Parallel()
	cmd := exec.Command("sleep", "10")
	cmd.Dir = unitTempDir(t)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { killGroup(cmd.Process.Pid); _ = cmd.Wait() }()
	started := time.Now()
	// Leave this group alive to exercise the same bounded wait used after an
	// unsuccessful KILL (for example EPERM), without requiring root privileges.
	if waitGroupEmptyWithin(cmd.Process.Pid, 40*time.Millisecond) {
		t.Fatal("live group reported empty")
	}
	if time.Since(started) > time.Second {
		t.Fatal("group wait exceeded its bound")
	}
}
