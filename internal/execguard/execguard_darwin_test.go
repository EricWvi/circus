package execguard

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// deathLimit bounds how long the command may outlive a killed parent.
const deathLimit = 200 * time.Millisecond

// waitGone polls until pid no longer exists, or reports false after limit.
// Only for processes the test is not the parent of (on macOS an orphan is
// reaped by launchd).
func waitGone(pid int, limit time.Duration) (time.Duration, bool) {
	start := time.Now()
	for {
		if err := unix.Kill(pid, 0); errors.Is(err, unix.ESRCH) {
			return time.Since(start), true
		}
		if time.Since(start) > limit {
			return time.Since(start), false
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestParentGoneBeforeCheck kills the parent before the guard registers its
// watch: the guard must notice and run nothing.
func TestParentGoneBeforeCheck(t *testing.T) {
	root := testRoot(t)
	marker := filepath.Join(root, "marker")
	var stderr bytes.Buffer
	p := startParent(t, root, "exit", holdOrphaned, &stderr, "touch", marker)
	guard := pidLines(t, p.stdout, "guard")["guard"]
	parent := p.cmd.Process.Pid
	// Wait returns once the guard, which shares the parent's stderr, exited.
	if err := p.cmd.Wait(); err != nil {
		t.Fatalf("parent: %v; stderr %q", err, stderr.String())
	}
	if _, ok := waitGone(guard, processWait); !ok {
		_ = unix.Kill(guard, unix.SIGKILL)
		t.Fatalf("guard %d still runs", guard)
	}
	want := fmt.Sprintf("exec-guard: parent %d is gone", parent)
	if !strings.Contains(stderr.String(), want) {
		t.Fatalf("stderr %q, want %q", stderr.String(), want)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the command ran: %v", err)
	}
}

// groupSh starts a grandchild in the command's process group, prints
// "child <pid>" and "ready <pid>", and waits.
const groupSh = `sleep 60 & echo "child $!"; echo "ready $$"; wait`

// requireGone fails the test unless every pid is gone within limit; it kills
// leftovers.
func requireGone(t *testing.T, limit time.Duration, pids map[string]int) {
	t.Helper()
	start := time.Now()
	for name, pid := range pids {
		left := limit - time.Since(start)
		if _, ok := waitGone(pid, left); !ok {
			for _, p := range pids {
				_ = unix.Kill(p, unix.SIGKILL)
			}
			t.Fatalf("%s %d still ran %s later", name, pid, limit)
		}
	}
	t.Logf("%v gone within %s", pids, time.Since(start))
}

// TestParentDiesWhileCommandRuns kills the parent while the command runs: the
// guard must kill the command's whole process group at once and exit.
func TestParentDiesWhileCommandRuns(t *testing.T) {
	root := testRoot(t)
	p := startParent(t, root, "stay", holdNone, os.Stderr, "sh", "-c", groupSh)
	pids := pidLines(t, p.stdout, "guard", "ready", "child")
	if pids["guard"] == pids["ready"] {
		t.Fatalf("command and guard share PID %d; want a supervised child", pids["guard"])
	}
	p.kill(t)
	requireGone(t, deathLimit, map[string]int{"command": pids["ready"], "grandchild": pids["child"]})
	requireGone(t, processWait, map[string]int{"guard": pids["guard"]})
}

func TestCommandHasOwnProcessGroup(t *testing.T) {
	root := testRoot(t)
	cmd := guardCommand(t, root, "sh", "-c", `echo "ready $$"; echo "pgid $(ps -o pgid= -p $$ | tr -d ' ')"`)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if status := exitStatus(t, cmd); status.ExitStatus() != 0 {
		t.Fatalf("status %v", status)
	}
	var ready, pgid int
	if _, err := fmt.Sscanf(stdout.String(), "ready %d\npgid %d\n", &ready, &pgid); err != nil {
		t.Fatalf("output %q: %v", stdout.String(), err)
	}
	if ready == cmd.Process.Pid || pgid != ready {
		t.Fatalf("guard %d, command %d in group %d; want the command leading its own group", cmd.Process.Pid, ready, pgid)
	}
}

func TestSignalsForwarded(t *testing.T) {
	root := testRoot(t)
	t.Run("command kills itself", func(t *testing.T) {
		cmd := guardCommand(t, root, "sh", "-c", "kill -TERM $$")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		if status := waitBounded(t, cmd); status.ExitStatus() != 128+int(syscall.SIGTERM) {
			t.Fatalf("status %v, want exit %d", status, 128+int(syscall.SIGTERM))
		}
	})
	// startReady starts the guard with script and returns the PIDs printed
	// on the "ready" (and, with groupSh, "child") lines.
	startReady := func(t *testing.T, script string, prefixes ...string) (*exec.Cmd, map[string]int) {
		t.Helper()
		cmd := guardCommand(t, root, "sh", "-c", script)
		out, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
		pids := pidLines(t, lines(out), prefixes...)
		t.Cleanup(func() { _ = unix.Kill(-pids["ready"], unix.SIGKILL) })
		return cmd, pids
	}
	t.Run("SIGTERM reaches the group", func(t *testing.T) {
		skipIfIgnored(t, syscall.SIGTERM)
		cmd, pids := startReady(t, groupSh, "ready", "child")
		if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		if status := waitBounded(t, cmd); status.ExitStatus() != 128+int(syscall.SIGTERM) {
			t.Fatalf("status %v, want exit %d", status, 128+int(syscall.SIGTERM))
		}
		requireGone(t, deathLimit, map[string]int{"command": pids["ready"], "grandchild": pids["child"]})
	})
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP} {
		name := unix.SignalName(sig)
		for _, c := range []struct {
			name, script string
			exit         int
		}{
			{"default action", readySh, 128 + int(sig)},
			// The trap proves the signal reached the command rather than
			// ending only the guard.
			{"trapped", fmt.Sprintf(`trap "exit 3" %s; echo "ready $$"; while :; do sleep 0.05; done`, strings.TrimPrefix(name, "SIG")), 3},
		} {
			t.Run(name+" "+c.name, func(t *testing.T) {
				skipIfIgnored(t, sig)
				cmd, _ := startReady(t, c.script, "ready")
				if err := cmd.Process.Signal(sig); err != nil {
					t.Fatal(err)
				}
				if status := waitBounded(t, cmd); !status.Exited() || status.ExitStatus() != c.exit {
					t.Fatalf("status %v, want exit %d", status, c.exit)
				}
			})
		}
	}
}

// startGroup starts the guard running script, which must print "child <pid>"
// for a background process in its group and "ready <pid>" for itself; it
// returns the guard, its stdin and those PIDs.
func startGroup(t *testing.T, script string) (*exec.Cmd, io.WriteCloser, map[string]int) {
	t.Helper()
	root := testRoot(t)
	cmd := guardCommand(t, root, "sh", "-c", script)
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	pids := pidLines(t, lines(out), "ready", "child")
	t.Cleanup(func() { _ = unix.Kill(-pids["ready"], unix.SIGKILL) })
	return cmd, in, pids
}

// TestCommandExitKillsItsGroup: once the command exits, the guard kills
// what is left of its process group and exits with the command's status.
func TestCommandExitKillsItsGroup(t *testing.T) {
	cmd, in, pids := startGroup(t, `sleep 60 & echo "child $!"; echo "ready $$"; read -r line; exit 23`)
	if _, err := io.WriteString(in, "exit\n"); err != nil {
		t.Fatal(err)
	}
	if status := waitBounded(t, cmd); !status.Exited() || status.ExitStatus() != 23 {
		t.Fatalf("status %v, want exit 23", status)
	}
	requireGone(t, deathLimit, map[string]int{"grandchild": pids["child"]})
}

// TestUSR1KillsTheGroup: SIGUSR1 asks the guard to tear down; it kills the
// command's whole group with SIGKILL and reports the command killed.
func TestUSR1KillsTheGroup(t *testing.T) {
	cmd, _, pids := startGroup(t, `sleep 60 & echo "child $!"; echo "ready $$"; wait`)
	if err := cmd.Process.Signal(unix.SIGUSR1); err != nil {
		t.Fatal(err)
	}
	if status := waitBounded(t, cmd); !status.Exited() || status.ExitStatus() != 128+int(unix.SIGKILL) {
		t.Fatalf("status %v, want exit %d", status, 128+int(unix.SIGKILL))
	}
	requireGone(t, deathLimit, map[string]int{"command": pids["ready"], "grandchild": pids["child"]})
}
