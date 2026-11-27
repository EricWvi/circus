package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/EricWvi/circus/internal/credfile"
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

// worker-rejected needs an absolute credential path and a pid, and keeps
// everything without an installed worker service.
func TestInternalWorkerRejected(t *testing.T) {
	t.Setenv("CIRCUS_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	hash := strings.Repeat("ab", 32)
	for _, args := range [][]string{
		nil,
		{"--credentials", "worker.json", "--pid", "1", "--principal", "w", "--token-sha256", hash},
		{"--credentials", "/w.json", "--principal", "w", "--token-sha256", hash},
		{"--credentials", "/w.json", "--pid", "1", "--token-sha256", hash},
		{"--credentials", "/w.json", "--pid", "1", "--principal", "w", "--token-sha256", "abc"},
		{"--credentials", "/w.json", "--pid", "1", "--principal", "w", "--token-sha256", hash, "x"},
		{"--credentials", "/w.json", "--pid", "1", "--principal", "w", "--token-sha256", hash, "--credential-only"},
		{"--credentials", "/w.json", "--pid", "0", "--principal", "w", "--token-sha256", hash, "--credential-only"},
		{"--credentials", "/w.json", "--pid=-1", "--principal", "w", "--token-sha256", hash, "--credential-only"},
		{"--credentials", "/w.json", "--pid", "0", "--principal", "w", "--token-sha256", hash},
		{"--credentials", "worker.json", "--principal", "w", "--token-sha256", hash, "--credential-only"},
		{"--credentials", "/w.json", "--token-sha256", hash, "--credential-only"},
		{"--credentials", "/w.json", "--principal", "w", "--token-sha256", "abc", "--credential-only"},
	} {
		var stderr bytes.Buffer
		if code := internalCommand(append([]string{"worker-rejected"}, args...), &stderr); code != 2 || !strings.Contains(stderr.String(), "usage:") {
			t.Errorf("%q: exit %d, stderr %q", args, code, stderr.String())
		}
	}
	path := filepath.Join(t.TempDir(), "worker.json")
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	if code := internalCommand([]string{"worker-rejected", "--credentials", path, "--pid", "1", "--principal", "w", "--token-sha256", hash}, &stderr); code != 1 || !strings.Contains(stderr.String(), "file are kept") {
		t.Fatalf("without a service: exit %d, stderr %q", code, stderr.String())
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("credential removed without a service: %v", err)
	}
}

// worker-rejected --credential-only, for a worker run in the foreground,
// removes the credential file while it holds the revoked credential and
// never changes a service; when the installed worker service reads that
// very file, the file is left to the service.
func TestInternalWorkerRejectedCredentialOnly(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CIRCUS_HOME", filepath.Join(dir, "home"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(dir, "data"))
	t.Setenv("HOME", filepath.Join(dir, "user"))
	path, err := credfile.Path(credfile.RoleWorker)
	if err != nil {
		t.Fatal(err)
	}
	running := credfile.File{Role: credfile.RoleWorker, PrincipalID: "worker-1", Token: "token-1", ControlPlaneURL: "https://circus.example"}
	sum := sha256.Sum256([]byte(running.Token))
	args := []string{"worker-rejected", "--credentials", path, "--principal", running.PrincipalID, "--token-sha256", hex.EncodeToString(sum[:]), "--credential-only"}
	unit := filepath.Join(dir, "config", "systemd", "user", "circus-worker.service")
	// The installed worker service reads another credential file: it runs
	// with another CIRCUS_HOME.
	execStart := "ExecStart=\"/opt/node\" \"/opt/worker/main.js\" \"--service\""
	unitText := "[Service]\n" + execStart + "\nEnvironment=\"CIRCUS_HOME=" + filepath.Join(dir, "other home") + "\"\n"
	if err := os.MkdirAll(filepath.Dir(unit), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unit, []byte(unitText), 0o600); err != nil {
		t.Fatal(err)
	}

	// A join replaced the credential: the file stays as it is.
	rejoined := credfile.File{Role: credfile.RoleWorker, PrincipalID: "worker-2", Token: "token-2", ControlPlaneURL: "https://circus.example"}
	if err := credfile.Save(path, rejoined); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	if code := internalCommand(args, &stderr); code != 1 || !strings.Contains(stderr.String(), "the file now holds another or unreadable credential; kept") {
		t.Fatalf("replaced: exit %d, stderr %q", code, stderr.String())
	}
	if got, err := credfile.Load(path); err != nil || got != rejoined {
		t.Fatalf("replaced: credential %+v, %v", got, err)
	}

	// It holds the revoked credential: it is removed.
	if err := credfile.Save(path, running); err != nil {
		t.Fatal(err)
	}
	stderr.Reset()
	if code := internalCommand(args, &stderr); code != 0 || !strings.Contains(stderr.String(), "so the credential file was removed; join again with a new link") {
		t.Fatalf("revoked: exit %d, stderr %q", code, stderr.String())
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("revoked: credential %v, want it removed", err)
	}

	// It is gone already.
	stderr.Reset()
	if code := internalCommand(args, &stderr); code != workerRejectedGone || !strings.Contains(stderr.String(), "already gone") {
		t.Fatalf("missing: exit %d, stderr %q", code, stderr.String())
	}
	if got, err := os.ReadFile(unit); err != nil || string(got) != unitText {
		t.Fatalf("the service file changed: %q, %v", got, err)
	}
	if matches, _ := filepath.Glob(path + ".revoked-*"); len(matches) != 0 {
		t.Fatalf("moved-aside files left: %q", matches)
	}

	// The installed worker service reads this very file: it is left to the
	// service, which removes both on the same proof.
	if runtime.GOOS != "linux" {
		return
	}
	unitText = "[Service]\n" + execStart + "\nEnvironment=\"CIRCUS_HOME=" + filepath.Join(dir, "home") + "\"\n"
	if err := os.WriteFile(unit, []byte(unitText), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := credfile.Save(path, running); err != nil {
		t.Fatal(err)
	}
	stderr.Reset()
	if code := internalCommand(args, &stderr); code != 1 || !strings.Contains(stderr.String(), "the installed worker service reads "+path+"; it is left to the service") {
		t.Fatalf("the service's file: exit %d, stderr %q", code, stderr.String())
	}
	if got, err := credfile.Load(path); err != nil || got != running {
		t.Fatalf("the service's file: credential %+v, %v", got, err)
	}
	if got, err := os.ReadFile(unit); err != nil || string(got) != unitText {
		t.Fatalf("the service file changed: %q, %v", got, err)
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
