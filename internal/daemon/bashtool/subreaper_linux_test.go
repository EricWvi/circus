package bashtool

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/testutil/leakcheck"
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
			b.outputs.mu.Lock()
			defer b.outputs.mu.Unlock()
			if b.outputs.active[path] {
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

// TestStoppedGroupMembersAreReaped relies on TestMain's child subreaper: the
// shell's children are adopted by this process, so a stopped command must
// reap them before Run returns instead of leaving zombies (kill(pid, 0)
// succeeds on a zombie).
func TestStoppedGroupMembersAreReaped(t *testing.T) {
	for _, tc := range []struct {
		name, command string
		timeout       time.Duration
	}{
		{"timeout kill", `sleep 0; sleep 30 & echo $! > child.pid; wait`, 300 * time.Millisecond},
		{"timeout stop", `true; sleep 30 & echo $! > child.pid; wait`, 4 * time.Second},
		{"output cap", `true; sleep 30 & echo $! > child.pid; head -c 2000000 /dev/zero; sleep 30`, 4 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b, root, _ := reviewBash(t, 1<<20)
			reviewRun(t, b, root, "true", false)
			ctx, cancel := context.WithTimeout(context.Background(), tc.timeout)
			defer cancel()
			if tc.name == "timeout stop" {
				// Cancellation takes the TERM, two-second grace and KILL path.
				go func() {
					for {
						// Redirection creates the file before echo writes the PID. Do not
						// cancel the shell in that window and turn the fixture into an empty file.
						data, readErr := os.ReadFile(filepath.Join(root, "child.pid"))
						pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data)))
						if readErr == nil && parseErr == nil && pid > 0 || ctx.Err() != nil {
							cancel()
							return
						}
						time.Sleep(5 * time.Millisecond)
					}
				}()
			}
			r, err := b.Run(ctx, &circusv1.DeviceCall{SessionId: "review", CallId: "reap", Tool: &circusv1.DeviceCall_Bash{Bash: &circusv1.BashRequest{Cwd: root, Command: tc.command}}})
			if err != nil || !r.Output.IsError {
				t.Fatal(r, err)
			}
			leakcheck.Expect(r.GetBash().GetOutputPath())
			if pid := childPID(t, root); syscall.Kill(pid, 0) != syscall.ESRCH {
				t.Fatalf("child %d of the stopped group is alive or an unreaped zombie: %v", pid, r)
			}
		})
	}
}

// TestTimedOutCollectorChildrenAreReaped is the deterministic regression for
// stopProcess: nothing after a collector timeout polls the group, so without
// reaping the killed startup-file child stays a zombie of this subreaper.
func TestTimedOutCollectorChildrenAreReaped(t *testing.T) {
	t.Parallel()
	root := unitTempDir(t)
	rc := `sleep 30 & echo $! >> "$HOME/child.pids"; exec sleep 30` + "\n"
	if err := os.WriteFile(filepath.Join(root, ".bashrc"), []byte(rc), 0600); err != nil {
		t.Fatal(err)
	}
	e := &snapshotStore{options: snapshotOptions{Home: root, StateDir: filepath.Join(root, "state"), Shell: "/bin/bash", CollectorTimeout: 200 * time.Millisecond}}
	t.Cleanup(e.releaseInstance)
	if _, _, _, err := e.load(context.Background()); err == nil {
		t.Fatal("hanging startup files produced a snapshot")
	}
	data, err := os.ReadFile(filepath.Join(root, "child.pids"))
	if err != nil {
		t.Fatal(err)
	}
	pids := strings.Fields(string(data))
	if len(pids) == 0 {
		t.Fatal("startup files started no child")
	}
	for _, field := range pids {
		pid, err := strconv.Atoi(field)
		if err != nil {
			t.Fatal(err)
		}
		if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
			t.Errorf("collector child %d is alive or an unreaped zombie: %v", pid, err)
		}
	}
}
