package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/EricWvi/circus/internal/execguard"
	"github.com/EricWvi/circus/internal/testutil/leakcheck"
)

func TestInternalUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"nope"}, {"exec-guard"}, {"exec-guard", "--parent", "100", "true"}} {
		var stderr bytes.Buffer
		if code := internalCommand(args, &stderr); code != execguard.ExitUsage {
			t.Errorf("%q: exit %d, want %d", args, code, execguard.ExitUsage)
		}
		if !strings.Contains(stderr.String(), execguard.Usage) {
			t.Errorf("%q: stderr %q", args, stderr.String())
		}
	}
	if !isExecGuard([]string{"circus", "internal", "exec-guard", "--parent", "1"}) || isExecGuard([]string{"circus", "internal"}) {
		t.Fatal("isExecGuard")
	}
}

// TestExecGuardProcess runs the guard through the real binary; the guard's
// behaviour is covered in internal/execguard.
func TestExecGuardProcess(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the circus binary")
	}
	if runtime.GOOS != "linux" {
		t.Skip("checks the Linux supervisor")
	}
	root := t.TempDir()
	leakcheck.Track(t, root)
	bin := buildCircus(t, filepath.Join(root, "build"))
	// run returns stdout, stderr, the exit code and the guard's PID.
	run := func(args ...string) (string, string, int, int) {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Dir = root
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		var exit *exec.ExitError
		if err != nil && !errors.As(err, &exit) {
			t.Fatalf("%v: %v", args, err)
		}
		return stdout.String(), stderr.String(), cmd.ProcessState.ExitCode(), cmd.Process.Pid
	}
	if _, stderr, code, _ := run("internal"); code != execguard.ExitUsage || !strings.Contains(stderr, execguard.Usage) {
		t.Fatalf("circus internal: exit %d, stderr %q", code, stderr)
	}
	// A live process that is not the guard's parent.
	other := exec.Command("sleep", "60")
	other.Dir = root
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Process.Kill(); _ = other.Wait() })
	if _, stderr, code, _ := run("internal", "exec-guard", "--parent", fmt.Sprint(other.Process.Pid), "--", "true"); code != execguard.ExitParentGone {
		t.Fatalf("wrong parent: exit %d, stderr %q", code, stderr)
	}
	stdout, stderr, code, pid := run("internal", "exec-guard", "--parent", fmt.Sprint(os.Getpid()), "--", "sh", "-c", "echo $$ $PPID; exit 9")
	var command, parent int
	_, err := fmt.Sscan(stdout, &command, &parent)
	if err != nil || code != 9 || command == pid || parent != pid {
		t.Fatalf("exit %d, command and parent PIDs %q, guard PID %d, stderr %q", code, stdout, pid, stderr)
	}
	if strings.Contains(usage, "internal") {
		t.Fatalf("usage %q lists the hidden internal command", usage)
	}
}
