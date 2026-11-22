//go:build linux || darwin

package execguard

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/EricWvi/circus/internal/testutil/leakcheck"
)

// processWait bounds every wait for a process the test started.
const processWait = 10 * time.Second

// readySh prints "ready <pid>" and then becomes a long sleep with the same
// PID.
const readySh = `echo "ready $$"; exec sleep 60`

// testRoot returns a directory that attributes the test's processes to it in
// the leak check: every process here runs with it as its working directory.
func testRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	leakcheck.Track(t, root)
	return root
}

func testBinary(t *testing.T) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return self
}

// guardEnv is the test's environment with the guard role.
func guardEnv() []string { return withRole(os.Environ(), roleGuard) }

// guardCommand returns the test binary acting as the guard, with the test as
// its parent.
func guardCommand(t *testing.T, root string, command ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(testBinary(t), append([]string{"--parent", fmt.Sprint(os.Getpid()), "--"}, command...)...)
	cmd.Dir = root
	cmd.Env = guardEnv()
	cmd.WaitDelay = processWait
	return cmd
}

// exitStatus runs cmd and returns its wait status.
func exitStatus(t *testing.T, cmd *exec.Cmd) syscall.WaitStatus {
	t.Helper()
	err := cmd.Run()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatalf("run %v: %v", cmd.Args, err)
	}
	return cmd.ProcessState.Sys().(syscall.WaitStatus)
}

// lines delivers the lines of r until it closes.
func lines(r io.Reader) <-chan string {
	ch := make(chan string, 16)
	go func() {
		defer close(ch)
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			ch <- sc.Text()
		}
	}()
	return ch
}

// pidLines waits for one line per prefix ("guard", "ready") and returns
// the PID each carries.
func pidLines(t *testing.T, ch <-chan string, prefixes ...string) map[string]int {
	t.Helper()
	found := make(map[string]int)
	deadline := time.After(processWait)
	for len(found) < len(prefixes) {
		select {
		case line, ok := <-ch:
			if !ok {
				t.Fatalf("output closed; got %v, want %v", found, prefixes)
			}
			for _, p := range prefixes {
				if rest, ok := strings.CutPrefix(line, p+" "); ok {
					pid, err := strconv.Atoi(rest)
					if err != nil {
						t.Fatalf("bad line %q", line)
					}
					found[p] = pid
				}
			}
		case <-deadline:
			t.Fatalf("got %v after %s, want %v", found, processWait, prefixes)
		}
	}
	return found
}

// livePID starts a process that outlives the test's use of it and returns
// its PID; cleanup kills and reaps it.
func livePID(t *testing.T, root string) int {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	cmd.Dir = root
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return cmd.Process.Pid
}

// waitBounded waits for a started cmd, killing it and failing the test after
// processWait.
func waitBounded(t *testing.T, cmd *exec.Cmd) syscall.WaitStatus {
	t.Helper()
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(processWait):
		_ = cmd.Process.Kill()
		<-done
		t.Fatalf("%v still ran after %s", cmd.Args, processWait)
	}
	return cmd.ProcessState.Sys().(syscall.WaitStatus)
}

// skipIfIgnored skips signals the test process inherited as ignored: the
// guard and the command inherit them too (a background job of a
// non-interactive shell ignores SIGINT).
func skipIfIgnored(t *testing.T, sig os.Signal) {
	t.Helper()
	if signal.Ignored(sig) {
		t.Skipf("%v is ignored in this test process", sig)
	}
}

// parentProcess is the test binary in the parent role: it starts the guard
// as its child and either exits at once or waits to be killed.
type parentProcess struct {
	cmd    *exec.Cmd
	stdout <-chan string
}

// startParent starts the parent role; hold selects the guard's test hook.
func startParent(t *testing.T, root, mode, hold string, stderr io.Writer, command ...string) *parentProcess {
	t.Helper()
	cmd := exec.Command(testBinary(t), append([]string{mode}, command...)...)
	cmd.Dir = root
	cmd.Env = withRole(os.Environ(), roleParent)
	if hold != holdNone {
		cmd.Env = append(cmd.Env, holdEnv+"="+hold)
	}
	cmd.Stderr = stderr
	cmd.WaitDelay = processWait
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p := &parentProcess{cmd: cmd, stdout: lines(out)}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	return p
}

// kill kills the parent and reaps it.
func (p *parentProcess) kill(t *testing.T) {
	t.Helper()
	if err := p.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = p.cmd.Wait()
}

func TestParentMismatch(t *testing.T) {
	root := testRoot(t)
	marker := filepath.Join(root, "marker")
	// A live process that is not the guard's parent.
	other := livePID(t, root)
	cmd := exec.Command(testBinary(t), "--parent", fmt.Sprint(other), "--", "touch", marker)
	cmd.Dir, cmd.Env = root, guardEnv()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if status := exitStatus(t, cmd); status.ExitStatus() != ExitParentGone {
		t.Fatalf("status %v, want exit %d; stderr %q", status, ExitParentGone, stderr.String())
	}
	want := fmt.Sprintf("exec-guard: parent %d is gone (getppid=%d); not starting touch", other, os.Getpid())
	if !strings.Contains(stderr.String(), want) {
		t.Fatalf("stderr %q, want %q", stderr.String(), want)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the command ran: %v", err)
	}
}

func TestExitStatusPassesThrough(t *testing.T) {
	root := testRoot(t)
	notExecutable := filepath.Join(root, "not-executable")
	if err := os.WriteFile(notExecutable, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		command []string
		exit    int
	}{
		{"zero", []string{"true"}, 0},
		{"code", []string{"sh", "-c", "exit 7"}, 7},
		{"not found", []string{"circus-execguard-no-such-command"}, ExitNotFound},
		{"not executable", []string{notExecutable}, ExitSetup},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cmd := guardCommand(t, root, c.command...)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if status := exitStatus(t, cmd); !status.Exited() || status.ExitStatus() != c.exit {
				t.Fatalf("status %v, want exit %d; stderr %q", status, c.exit, stderr.String())
			}
		})
	}
}

func TestStdinEOFAndOutput(t *testing.T) {
	root := testRoot(t)
	cmd := guardCommand(t, root, "cat")
	cmd.Stdin = strings.NewReader("hello\n")
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if status := exitStatus(t, cmd); status.ExitStatus() != 0 || stdout.String() != "hello\n" {
		t.Fatalf("status %v, stdout %q", status, stdout.String())
	}
}

func TestEnvironmentUnchanged(t *testing.T) {
	root := testRoot(t)
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"EMPTY=",
		"EQUALS=a=b==c",
		"NEWLINE=one\ntwo",
		"SPACES=  padded  ",
		"UNICODE=héllo wörld",
		"ANTHROPIC_BASE_URL=http://127.0.0.1:1",
		roleEnv + "=" + roleGuard,
	}
	cmd := guardCommand(t, root, "env")
	cmd.Env = env
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	// env prints one variable per line; NEWLINE spans two.
	if status := exitStatus(t, cmd); status.ExitStatus() != 0 {
		t.Fatalf("status %v", status)
	}
	want := strings.Join(env, "\n") + "\n"
	if got := stdout.String(); got != want {
		t.Fatalf("environment changed:\ngot  %q\nwant %q", got, want)
	}
}
