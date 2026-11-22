package execguard

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/EricWvi/circus/internal/testutil/leakcheck"
)

// The test binary doubles as the guard and as the guard's parent, selected by
// roleEnv, so the tests can hold the guard through beforeGuard without a
// public flag.
const (
	roleEnv = "CIRCUS_EXECGUARD_TEST_ROLE"
	// holdOrphaned waits for parent exit before opening the watch.
	// The other modes install Linux-specific hooks.
	holdEnv      = "CIRCUS_EXECGUARD_TEST_HOLD"
	holdNone     = ""
	holdOrphaned = "orphaned"

	roleGuard  = "guard"
	roleParent = "parent"
)

func init() {
	// Use the main OS thread for parent-death notification, as cmd/circus does.
	if os.Getenv(roleEnv) == roleGuard {
		runtime.LockOSThread()
	}
}

func TestMain(m *testing.M) {
	switch os.Getenv(roleEnv) {
	case roleGuard:
		if hold := os.Getenv(holdEnv); hold == holdOrphaned {
			beforeGuard = waitOrphaned
		} else if setupGuardHooks != nil {
			setupGuardHooks(hold)
		}
		os.Exit(Run(os.Args[1:], os.Stderr))
	case roleParent:
		os.Exit(runParent(os.Args[1:]))
	}
	os.Exit(leakcheck.Run(m, leakcheck.Config{}))
}

// waitOrphaned waits until parent is no longer the process's parent, at most
// parentLife.
func waitOrphaned(parent int) {
	deadline := time.Now().Add(parentLife)
	for os.Getppid() == parent && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
}

var setupGuardHooks func(string)

// parentLife bounds how long a "stay" parent waits to be killed.
const parentLife = 30 * time.Second

// runParent starts the guard as its child with --parent set to itself and the
// rest of args as the command, prints "guard <pid>" and then, by args[0],
// exits at once ("exit") or waits to be killed ("stay"). The guard inherits
// stdout and stderr.
func runParent(args []string) int {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "parent: want <exit|stay> <command...>")
		return 2
	}
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "parent:", err)
		return 1
	}
	cmd := exec.Command(self, append([]string{"--parent", fmt.Sprint(os.Getpid()), "--"}, args[1:]...)...)
	cmd.Env = withRole(os.Environ(), roleGuard)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "parent:", err)
		return 1
	}
	fmt.Printf("guard %d\n", cmd.Process.Pid)
	if args[0] == "stay" {
		time.Sleep(parentLife)
	}
	return 0
}

// withRole returns env with roleEnv set to role.
func withRole(env []string, role string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if !strings.HasPrefix(kv, roleEnv+"=") {
			out = append(out, kv)
		}
	}
	return append(out, roleEnv+"="+role)
}

func TestUsage(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"--parent", "100"},
		{"--parent", "100", "--"},
		{"--parent", "100", "--", ""},
		{"--parent", "100", "true"},
		{"--", "true"},
		{"--parent", "x", "--", "true"},
		{"--parent", "1", "--", "true"},
		{"--parent", "-5", "--", "true"},
		{"--parent", "100", "--bogus", "--", "true"},
		{"-h"},
	} {
		var stderr bytes.Buffer
		if code := Run(args, &stderr); code != ExitUsage {
			t.Errorf("%q: exit %d, want %d", args, code, ExitUsage)
		}
		if !strings.Contains(stderr.String(), Usage) {
			t.Errorf("%q: stderr %q lacks the usage line", args, stderr.String())
		}
	}
}
