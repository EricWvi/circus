package execguard

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// deathLimit bounds how long the guard may outlive a killed parent.
const deathLimit = 200 * time.Millisecond

const (
	holdWatch       = "watch"
	holdFork        = "fork"
	holdStart       = "start"
	holdStop        = "stop"
	missingChildren = "missing-children"
	threadExit      = "thread-exit"
	noParentFD      = "no-pidfd"
	blockedPIDFD    = "blocked-pidfd"
	pidfdOnly       = "pidfd-only"
	detachedSh      = "setsid sh -c 'echo \"detached $$\"; exec sleep 60' &\n"
	killExitCode    = 128 + int(unix.SIGKILL)
)

func init() {
	setupGuardHooks = func(holds string) {
		for hold := range strings.SplitSeq(holds, ",") {
			setGuardHook(hold)
		}
	}
}

func setGuardHook(hold string) {
	switch hold {
	case holdWatch:
		afterPidfd = func(parent int) {
			fmt.Printf("watch %d\n", os.Getpid())
			waitOrphaned(parent)
		}
	case holdFork:
		afterChildren = func(pid int) {
			data, err := os.ReadFile("forker.pid")
			if err != nil || string(data) != strconv.Itoa(pid) {
				return
			}
			afterChildren = nil
			writeHookFile("fork-now")
			if !pollFor(2*time.Second, func() bool {
				_, err := os.Stat("forked")
				return err == nil
			}) {
				panic("descendant did not fork during teardown")
			}
		}
	case holdStart:
		beforeStart = func(stops <-chan os.Signal) {
			fmt.Printf("launch %d\n", os.Getpid())
			if !pollFor(2*time.Second, func() bool { return len(stops) > 0 }) {
				panic("teardown signal was not queued")
			}
		}
		beforeStoppedStatus = func(int) { panic("command started after teardown was queued") }
	case holdStop:
		beforeStoppedStatus = func(pid int) {
			writeHookFile("stop-requested")
			if !pollFor(2*time.Second, func() bool { return zombieOrGone(pid) }) {
				panic("command did not exit with teardown pending")
			}
		}
	case missingChildren:
		first := true
		readProcChildren = func(string) ([]byte, error) {
			if first {
				first = false
				data, err := os.ReadFile("command.pid")
				if err != nil {
					panic(err)
				}
				pid, err := strconv.Atoi(string(data))
				if err != nil {
					panic(err)
				}
				if !pollFor(time.Second, func() bool { return zombieOrGone(pid) }) {
					panic("command was not killed before enumerating /proc")
				}
			}
			return nil, unix.ENOENT
		}
	case threadExit:
		afterIgnoredDeath = func() { fmt.Printf("thread-exit %d\n", os.Getpid()) }
	case noParentFD:
		openParentPIDFD = func(int, int) (int, error) { return -1, unix.ENOSYS }
	case blockedPIDFD:
		openParentPIDFD = func(int, int) (int, error) { return -1, unix.EPERM }
	case pidfdOnly:
		beforeGuard = func(int) {
			if err := unix.Prctl(unix.PR_SET_PDEATHSIG, 0, 0, 0, 0); err != nil {
				panic(err)
			}
		}
	}
}

func writeHookFile(name string) {
	if err := os.WriteFile(name, nil, 0o600); err != nil {
		panic(err)
	}
}

func zombieOrGone(pid int) bool {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	stat := string(data)
	return strings.HasPrefix(stat[strings.LastIndexByte(stat, ')')+1:], " Z ")
}

func pollFor(limit time.Duration, done func() bool) bool {
	deadline := time.Now().Add(limit)
	for !done() {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(2 * time.Millisecond)
	}
	return true
}

// reapOrphan waits for a process adopted by the test subreaper.
func reapOrphan(t *testing.T, pid int, limit time.Duration) syscall.WaitStatus {
	t.Helper()
	var status unix.WaitStatus
	if !pollFor(limit, func() bool {
		got, err := unix.Wait4(pid, &status, unix.WNOHANG, nil)
		if err != nil && !errors.Is(err, unix.EINTR) {
			t.Fatalf("wait4(%d): %v", pid, err)
		}
		return got == pid
	}) {
		t.Fatalf("process %d still present after %s", pid, limit)
	}
	return syscall.WaitStatus(status)
}

func requireExit(t *testing.T, status syscall.WaitStatus, code int) {
	t.Helper()
	if !status.Exited() || status.ExitStatus() != code {
		t.Fatalf("status %v, want exit %d", status, code)
	}
}

// Once the supervisor has exited, every reported descendant must already
// have been reaped, including ones that escaped the original process group.
func requireReaped(t *testing.T, pids map[string]int) {
	t.Helper()
	for name, pid := range pids {
		if err := unix.Kill(pid, 0); !errors.Is(err, unix.ESRCH) {
			t.Errorf("%s %d was not reaped: kill(0) = %v", name, pid, err)
		}
	}
}

func TestParentGoneBeforeWatch(t *testing.T) {
	root := testRoot(t)
	marker := filepath.Join(root, "marker")
	var stderr bytes.Buffer
	p := startParent(t, root, "exit", holdOrphaned, &stderr, "touch", marker)
	guard := pidLines(t, p.stdout, "guard")["guard"]
	parent := p.cmd.Process.Pid
	// Wait also drains the guard's inherited stderr.
	requireExit(t, waitBounded(t, p.cmd), 0)
	requireExit(t, reapOrphan(t, guard, processWait), ExitParentGone)
	want := fmt.Sprintf("exec-guard: parent %d is gone", parent)
	if !strings.Contains(stderr.String(), want) {
		t.Fatalf("stderr %q, want %q", stderr.String(), want)
	}
	requireNoMarker(t, marker)
}

func TestParentGoneAfterPIDFD(t *testing.T) {
	requirePIDFD(t)
	root := testRoot(t)
	marker := filepath.Join(root, "marker")
	p := startParent(t, root, "stay", holdWatch, os.Stderr, "touch", marker)
	pids := pidLines(t, p.stdout, "guard", "watch")
	if pids["guard"] != pids["watch"] {
		t.Fatalf("watch line from %d, guard %d", pids["watch"], pids["guard"])
	}
	p.kill(t)
	requireExit(t, reapOrphan(t, pids["guard"], processWait), ExitParentGone)
	requireNoMarker(t, marker)
}

func requirePIDFD(t *testing.T) {
	t.Helper()
	fd, err := unix.PidfdOpen(os.Getpid(), 0)
	if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EPERM) {
		t.Skipf("pidfd_open is unavailable: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	_ = unix.Close(fd)
}

func requireNoMarker(t *testing.T, marker string) {
	t.Helper()
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the command ran: %v", err)
	}
}

func TestParentDiesWhileCommandRuns(t *testing.T) {
	for _, tc := range []struct{ name, hold string }{
		{"pidfd", pidfdOnly},
		{"death signal fallback ENOSYS", noParentFD},
		{"death signal fallback EPERM", blockedPIDFD},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hold := tc.hold
			if hold == pidfdOnly {
				requirePIDFD(t)
			}
			root := testRoot(t)
			p := startParent(t, root, "stay", hold, os.Stderr, "sh", "-c", detachedSh+
				`sh -c 'sleep 60 & echo "orphan $!"'; echo "ready $$"; wait`)
			pids := pidLines(t, p.stdout, "guard", "ready", "detached", "orphan")
			if sid, err := unix.Getsid(pids["detached"]); err != nil || sid != pids["detached"] {
				t.Fatalf("detached PID %d, session %d: %v", pids["detached"], sid, err)
			}
			p.kill(t)
			requireExit(t, reapOrphan(t, pids["guard"], deathLimit), killExitCode)
			requireReaped(t, pids)
		})
	}
}

func TestCommandExitTearsDownTree(t *testing.T) {
	cmd, input, output := startGuard(t, holdNone, detachedSh+`echo "ready $$"; read -r line; exit 23`)
	pids := pidLines(t, output, "ready", "detached")
	if _, err := io.WriteString(input, "exit\n"); err != nil {
		t.Fatal(err)
	}
	requireExit(t, waitBounded(t, cmd), 23)
	requireReaped(t, pids)
}

func TestUSR1TearsDownTree(t *testing.T) {
	cmd, _, output := startGuard(t, holdNone, detachedSh+`echo "ready $$"; wait`)
	pids := pidLines(t, output, "ready", "detached")
	if err := cmd.Process.Signal(unix.SIGUSR1); err != nil {
		t.Fatal(err)
	}
	requireExit(t, waitBounded(t, cmd), killExitCode)
	requireReaped(t, pids)
}

func TestCommandSharesGuardSessionAndGroup(t *testing.T) {
	root := testRoot(t)
	cmd := guardCommand(t, root, "sh", "-c", `echo "$$ $(ps -o sid=,pgid= -p $$)"`)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	requireExit(t, exitStatus(t, cmd), 0)
	var pid, sid, pgid int
	if _, err := fmt.Fscan(&stdout, &pid, &sid, &pgid); err != nil {
		t.Fatalf("output %q: %v", stdout.String(), err)
	}
	guard := cmd.Process.Pid
	if pid == guard || sid != guard || pgid != guard {
		t.Fatalf("command %d, session %d, group %d, guard %d", pid, sid, pgid, guard)
	}
}

func TestArgvAsGiven(t *testing.T) {
	root := testRoot(t)
	script := `tr '\000' '\n' < /proc/$$/cmdline`
	cmd := guardCommand(t, root, "sh", "-c", script, "zeroth", "one arg")
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	requireExit(t, exitStatus(t, cmd), 0)
	want := strings.Join([]string{"sh", "-c", script, "zeroth", "one arg"}, "\n") + "\n"
	if got := stdout.String(); got != want {
		t.Fatalf("argv %q, want %q", got, want)
	}
}

func TestSignalsForwarded(t *testing.T) {
	t.Run("command kills itself", func(t *testing.T) {
		cmd := guardCommand(t, testRoot(t), "sh", "-c", "kill -TERM $$")
		requireExit(t, exitStatus(t, cmd), 128+int(unix.SIGTERM))
	})
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP} {
		t.Run(unix.SignalName(sig), func(t *testing.T) {
			skipIfIgnored(t, sig)
			cmd, _, output := startGuard(t, holdNone, readySh)
			pidLines(t, output, "ready")
			if err := cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			requireExit(t, waitBounded(t, cmd), 128+int(sig))
		})
	}
	t.Run("inherited ignored SIGHUP", func(t *testing.T) {
		cmd := guardCommand(t, testRoot(t), "sh", "-c", "kill -HUP $$; exit 19")
		cmd.Args = append([]string{"sh", "-c", `trap '' HUP; exec "$@"`, "sh"}, cmd.Args...)
		cmd.Path = "/bin/sh"
		requireExit(t, exitStatus(t, cmd), 19)
	})
	t.Run("SIGTERM leaves teardown to the command", func(t *testing.T) {
		skipIfIgnored(t, unix.SIGTERM)
		cmd, _, output := startGuard(t, holdNone, detachedSh+`
			trap 'echo "forwarded $$"' TERM
			echo "ready $$"
			i=0
			while [ "$i" -lt 250 ] && [ ! -e exit-now ]; do
				i=$((i+1)); sleep 0.02
			done
			exit 42
		`)
		pids := pidLines(t, output, "ready", "detached")
		if err := cmd.Process.Signal(unix.SIGTERM); err != nil {
			t.Fatal(err)
		}
		if pid := pidLines(t, output, "forwarded")["forwarded"]; pid != pids["ready"] {
			t.Fatalf("SIGTERM handled by %d, want command %d", pid, pids["ready"])
		}
		if err := unix.Kill(pids["detached"], 0); err != nil {
			t.Fatalf("detached descendant gone before command exit: %v", err)
		}
		if err := os.WriteFile(filepath.Join(cmd.Dir, "exit-now"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		requireExit(t, waitBounded(t, cmd), 42)
		requireReaped(t, pids)
	})
}

func TestAdoptedOrphanReapedWhileCommandRuns(t *testing.T) {
	cmd, input, output := startGuard(t, holdNone, `
		sh -c 'sh -c '"'"'
			i=0
			while [ "$i" -lt 250 ] && [ ! -e orphan-exit ]; do
				i=$((i+1)); sleep 0.02
			done
			exit 19
		'"'"' & echo "orphan $!"'
		echo "ready $$"
		read -r line
		exit 24
	`)
	pids := pidLines(t, output, "ready", "orphan")
	if err := os.WriteFile(filepath.Join(cmd.Dir, "orphan-exit"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	pid := pids["orphan"]
	if !pollFor(time.Second, func() bool { return errors.Is(unix.Kill(pid, 0), unix.ESRCH) }) {
		t.Fatalf("orphan %d not reaped while the command runs", pid)
	}
	if _, err := io.WriteString(input, "exit\n"); err != nil {
		t.Fatal(err)
	}
	requireExit(t, waitBounded(t, cmd), 24)
}

func TestGuardKilledAloneKillsCommand(t *testing.T) {
	cmd, _, out := startGuard(t, holdNone, readySh)
	pid := pidLines(t, out, "ready")["ready"]
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if status := waitBounded(t, cmd); !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatalf("guard status %v, want SIGKILL", status)
	}
	if status := reapOrphan(t, pid, deathLimit); !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatalf("command status %v, want SIGKILL", status)
	}
}

func TestTeardownWithoutProcChildren(t *testing.T) {
	cmd, _, out := startGuard(t, missingChildren, detachedSh+
		`sh -c 'sleep 60 & echo "orphan $!"'; echo "ready $$"; wait`)
	pids := pidLines(t, out, "ready", "detached", "orphan")
	writePIDFile(t, cmd.Dir, "command.pid", pids["ready"])
	if err := cmd.Process.Signal(unix.SIGUSR1); err != nil {
		t.Fatal(err)
	}
	requireExit(t, waitBounded(t, cmd), killExitCode)
	requireReaped(t, pids)
}

func TestUSR1BeforeStart(t *testing.T) {
	cmd, _, out := startGuard(t, holdStart, "touch marker")
	pidLines(t, out, "launch")
	if err := cmd.Process.Signal(unix.SIGUSR1); err != nil {
		t.Fatal(err)
	}
	requireExit(t, waitBounded(t, cmd), killExitCode)
	requireNoMarker(t, filepath.Join(cmd.Dir, "marker"))
}

func TestStoppedStatusPreservesCommandExit(t *testing.T) {
	const script = `
		echo "ready $$"
		i=0
		while [ "$i" -lt 250 ] && [ ! -e stop-requested ]; do
			i=$((i+1)); sleep 0.02
		done
		exit 23
	`
	t.Run("SIGUSR1", func(t *testing.T) {
		cmd, _, out := startGuard(t, holdStop, script)
		pids := pidLines(t, out, "ready")
		if err := cmd.Process.Signal(unix.SIGUSR1); err != nil {
			t.Fatal(err)
		}
		requireExit(t, waitBounded(t, cmd), 23)
		requireReaped(t, pids)
	})
	t.Run("parent death", func(t *testing.T) {
		requirePIDFD(t)
		p := startParent(t, testRoot(t), "stay", pidfdOnly+","+holdStop, os.Stderr, "sh", "-c", script)
		pids := pidLines(t, p.stdout, "guard", "ready")
		p.kill(t)
		requireExit(t, reapOrphan(t, pids["guard"], processWait), 23)
		requireReaped(t, pids)
	})
}

func TestParentThreadExitsWithPIDFDWatch(t *testing.T) {
	requirePIDFD(t)
	retire := make(chan struct{})
	retireThread := sync.OnceFunc(func() { close(retire) })
	defer retireThread()

	start := func(cmd *exec.Cmd) error {
		started := make(chan error, 1)
		go startOnRetiringThread(cmd, started, retire)
		return <-started
	}
	cmd, in, out := startGuardWith(t, threadExit, `echo "ready $$"; read -r line; exit 21`, start)
	pidLines(t, out, "ready")
	retireThread()
	// The guard must consume the real parent-death signal and keep the
	// command alive until the test lets it exit.
	pidLines(t, out, "thread-exit")
	if _, err := io.WriteString(in, "exit\n"); err != nil {
		t.Fatal(err)
	}
	requireExit(t, waitBounded(t, cmd), 21)
}

func startOnRetiringThread(cmd *exec.Cmd, started chan<- error, retire <-chan struct{}) {
	runtime.LockOSThread()
	if unix.Gettid() == os.Getpid() {
		// Go parks its initial OS thread when a locked goroutine exits.
		// Keep it occupied until the test retires the thread that forks.
		defer runtime.UnlockOSThread()
		go startOnRetiringThread(cmd, started, retire)
	} else {
		started <- cmd.Start()
		// Returning while locked retires this non-initial thread.
	}
	<-retire
}

func TestForkAfterTeardownScan(t *testing.T) {
	const script = `
		sh -c '
			echo "forker $$"
			i=0
			while [ "$i" -lt 250 ] && [ ! -e fork-now ]; do
				i=$((i+1)); sleep 0.02
			done
			setsid sh -c '"'"'echo "late $$"; touch forked; exec sleep 60'"'"' &
			wait
		' &
		echo "ready $$"
		wait
	`
	for _, tc := range []struct{ name, holds string }{
		{"children files", holdFork},
		{"stat snapshot", holdFork + "," + missingChildren},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd, _, output := startGuard(t, tc.holds, script)
			pids := pidLines(t, output, "ready", "forker")
			writePIDFile(t, cmd.Dir, "forker.pid", pids["forker"])
			writePIDFile(t, cmd.Dir, "command.pid", pids["ready"])
			if err := cmd.Process.Signal(unix.SIGUSR1); err != nil {
				t.Fatal(err)
			}
			pids["late"] = pidLines(t, output, "late")["late"]
			requireExit(t, waitBounded(t, cmd), killExitCode)
			requireReaped(t, pids)
		})
	}
}

func writePIDFile(t *testing.T, dir, name string, pid int) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(strconv.Itoa(pid)), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDescendantPermissionErrors(t *testing.T) {
	startTree := func(t *testing.T) (int, map[string]int) {
		t.Helper()
		cmd, in, output := startGuard(t, holdNone, `
			sh -c '
				sh -c '"'"'
					sleep 60 &
					echo "leaf $!"
					echo "target $$"
					wait
					exec sleep 60
				'"'"' &
				echo "parent $$"
				wait
			' &
			read -r line
		`)
		t.Cleanup(func() {
			_, _ = io.WriteString(in, "exit\n")
			requireExit(t, waitBounded(t, cmd), 0)
		})
		pids := pidLines(t, output, "parent", "target", "leaf")
		open, kill, read, after := openDescendantPIDFD, killDescendantPID, readProcChildren, afterChildren
		t.Cleanup(func() {
			openDescendantPIDFD, killDescendantPID, readProcChildren, afterChildren = open, kill, read, after
		})
		readProcChildren = func(string) ([]byte, error) { return nil, unix.ENOENT }
		return cmd.Process.Pid, pids
	}

	t.Run("pidfd open denied and parent changed", func(t *testing.T) {
		guard, pids := startTree(t)
		target, parent := pids["target"], pids["parent"]
		openDescendantPIDFD = func(int, int) (int, error) { return -1, unix.EPERM }
		killDescendantPID = func(pid int, _ unix.Signal) error {
			t.Fatalf("attempted to signal PID %d after reparenting", pid)
			return nil
		}
		afterChildren = func(pid int) {
			if pid != target {
				t.Fatalf("scanned PID %d, want %d", pid, target)
			}
			if err := unix.Kill(parent, unix.SIGKILL); err != nil {
				t.Fatal(err)
			}
			if !pollFor(time.Second, func() bool { return procParent(target) == guard }) {
				t.Fatalf("PID %d was not adopted by guard %d", target, guard)
			}
		}
		scan := descendantScan{deadline: time.Now().Add(teardownLimit)}
		if pending := scan.killDescendant(nil, descendant{pid: target, parent: parent}); len(pending) != 0 || len(scan.denied) != 0 {
			t.Fatalf("unverified children %v, permission denied PIDs %v", pending, scan.denied)
		}
	})

	t.Run("kill denied and children still visited", func(t *testing.T) {
		_, pids := startTree(t)
		target, leaf := pids["target"], pids["leaf"]
		openDescendantPIDFD = func(int, int) (int, error) { return -1, unix.ENOSYS }
		killDescendantPID = func(pid int, sig unix.Signal) error {
			if pid == target {
				return unix.EPERM
			}
			if pid != leaf {
				t.Fatalf("attempted to signal unexpected PID %d", pid)
			}
			return unix.Kill(pid, sig)
		}
		scan := descendantScan{deadline: time.Now().Add(teardownLimit)}
		scan.killDescendants(pids["parent"])
		if len(scan.denied) != 1 || scan.denied[0] != target {
			t.Fatalf("permission denied PIDs %v, want [%d]", scan.denied, target)
		}
		if !pollFor(time.Second, func() bool { return errors.Is(unix.Kill(leaf, 0), unix.ESRCH) }) {
			t.Fatalf("leaf %d survived below permission-denied PID %d", leaf, target)
		}
	})
}

func TestWorkerTeardownBudget(t *testing.T) {
	source, err := os.ReadFile("../../worker/src/process.ts")
	if err != nil {
		t.Fatal(err)
	}
	match := regexp.MustCompile(`(?m)^const GUARD_TEARDOWN_MS = ([0-9_]+);$`).FindStringSubmatch(string(source))
	if match == nil {
		t.Fatal("GUARD_TEARDOWN_MS is missing from worker/src/process.ts")
	}
	millis, err := strconv.ParseInt(strings.ReplaceAll(match[1], "_", ""), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	if want := teardownLimit.Milliseconds(); millis != want {
		t.Fatalf("worker budget is %d ms, want %d ms to match exec-guard", millis, want)
	}
}

func TestTeardownWhileForking(t *testing.T) {
	cmd, _, output := startGuard(t, holdNone, `
		setsid sh -c '
			i=0
			while [ "$i" -lt 32 ]; do
				i=$((i+1)); sleep 8 &
				echo "$!" >> children
				if [ "$i" -eq 1 ]; then echo "forker $$"; fi
			done
			wait
		' &
		echo "ready $$"
		wait
	`)
	pids := pidLines(t, output, "ready", "forker")
	start := time.Now()
	if err := cmd.Process.Signal(unix.SIGUSR1); err != nil {
		t.Fatal(err)
	}
	requireExit(t, waitBounded(t, cmd), killExitCode)
	if elapsed := time.Since(start); elapsed > teardownLimit {
		t.Errorf("teardown took %s, want at most %s", elapsed, teardownLimit)
	}
	data, err := os.ReadFile(filepath.Join(cmd.Dir, "children"))
	if err != nil {
		t.Fatal(err)
	}
	for i, field := range strings.Fields(string(data)) {
		pid, err := strconv.Atoi(field)
		if err != nil {
			t.Fatal(err)
		}
		pids[fmt.Sprintf("fork %d", i)] = pid
	}
	requireReaped(t, pids)
}

func startGuard(t *testing.T, hold, script string) (*exec.Cmd, io.Writer, <-chan string) {
	t.Helper()
	return startGuardWith(t, hold, script, (*exec.Cmd).Start)
}

func startGuardWith(t *testing.T, hold, script string, start func(*exec.Cmd) error) (*exec.Cmd, io.Writer, <-chan string) {
	t.Helper()
	cmd := guardCommand(t, testRoot(t), "sh", "-c", script)
	cmd.Env = append(cmd.Env, holdEnv+"="+hold)
	cmd.Stderr = os.Stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := start(cmd); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = in.Close()
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	return cmd, in, lines(out)
}
