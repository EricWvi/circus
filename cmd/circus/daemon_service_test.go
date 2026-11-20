package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/EricWvi/circus/internal/credfile"
	"github.com/EricWvi/circus/internal/daemonlock"
)

func TestDaemonCLIProcess(t *testing.T) {
	if os.Getenv("CIRCUS_TEST_RUN_DAEMON_CLI") != "1" {
		return
	}
	i := slices.Index(os.Args, "--")
	if i < 0 {
		t.Fatal("missing CLI arguments")
	}
	os.Args = append(os.Args[:1], os.Args[i+1:]...)
	main()
	os.Exit(0)
}

func runDaemonCLI(t *testing.T, binary string, args ...string) (string, int) {
	t.Helper()
	if binary == "" {
		var err error
		binary, err = os.Executable()
		if err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.CommandContext(t.Context(), binary, append([]string{"-test.run=^TestDaemonCLIProcess$", "--", "daemon"}, args...)...)
	cmd.Env = append(os.Environ(), "CIRCUS_TEST_RUN_DAEMON_CLI=1", "GORACE=atexit_sleep_ms=0")
	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() < 0 {
		t.Fatalf("daemon child: %v\n%s", err, out)
	}
	return string(out), exit.ExitCode()
}

func serviceExitCode() int {
	if runtime.GOOS == "linux" {
		return 78
	}
	return 0
}

func TestDaemonServiceTerminalExit(t *testing.T) {
	for _, tc := range []struct {
		name    string
		service bool
		retry   bool
		code    int
	}{
		{"foreground", false, false, 1},
		{"service", true, false, serviceExitCode()},
		{"intermediary then terminal", true, true, serviceExitCode()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CIRCUS_HOME", t.TempDir())
			var attempts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if attempts.Add(1) == 1 && tc.retry {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				w.Header().Set("Circus-Auth", "rejected")
				w.WriteHeader(http.StatusUnauthorized)
			}))
			defer server.Close()
			path, err := credfile.Path(credfile.RoleDaemon)
			if err != nil {
				t.Fatal(err)
			}
			err = credfile.Save(path, credfile.File{Role: credfile.RoleDaemon, Token: "test", PrincipalID: "device", ControlPlaneURL: server.URL})
			if err != nil {
				t.Fatal(err)
			}
			args := []string{"--credentials", path}
			if tc.service {
				args = append(args, "--service")
			}
			out, code := runDaemonCLI(t, "", args...)
			if code != tc.code || !strings.Contains(out, "HTTP 401 authentication rejected") {
				t.Fatalf("status %d, want %d\n%s", code, tc.code, out)
			}
			wantAttempts := int32(1)
			if tc.retry {
				wantAttempts++
			}
			if got := attempts.Load(); got != wantAttempts {
				t.Errorf("connection attempts = %d, want %d", got, wantAttempts)
			}
			if !tc.retry && strings.Count(out, "\n") != 1 {
				t.Errorf("expected one log line, got %q", out)
			}
		})
	}
}

func TestDaemonServiceExitOnMissingCredentials(t *testing.T) {
	t.Setenv("CIRCUS_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "missing\ncredentials")
	for _, tc := range []struct {
		args []string
		code int
	}{
		{[]string{"--credentials", path}, 1},
		{[]string{"--credentials", path, "--service"}, serviceExitCode()},
	} {
		out, code := runDaemonCLI(t, "", tc.args...)
		if code != tc.code || !strings.Contains(out, "run `circus join` first") || strings.Count(out, "\n") != 1 {
			t.Fatalf("status %d, want %d\n%s", code, tc.code, out)
		}
	}
}

func TestDaemonRefusesHeldLock(t *testing.T) {
	t.Setenv("CIRCUS_HOME", t.TempDir())
	for _, mode := range []daemonlock.Mode{daemonlock.Foreground, daemonlock.Service} {
		t.Run(string(mode), func(t *testing.T) {
			lock, err := daemonlock.Acquire(mode)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			want := fmt.Sprintf("another circus daemon is running (pid %d, %s)", os.Getpid(), mode)
			if mode == daemonlock.Service {
				want += "; `circus daemon stop` it first"
			}
			argSets := [][]string{nil}
			if mode == daemonlock.Foreground {
				argSets = append(argSets, []string{"--service"})
			}
			for _, args := range argSets {
				out, code := runDaemonCLI(t, "", args...)
				wantCode := 1
				if len(args) > 0 {
					wantCode = serviceExitCode()
				}
				if code != wantCode || !strings.Contains(out, want) || strings.Count(out, "\n") != 1 {
					t.Errorf("status %d, want %d\n%s", code, wantCode, out)
				}
			}
		})
	}
}

func TestDaemonCLIStatusAndInstallWarning(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("uses a systemctl fake")
	}
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	config := filepath.Join(dir, "config")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", config)
	t.Setenv("CIRCUS_HOME", "")
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	t.Setenv("SERVICE_TEST_DIR", dir)
	t.Setenv("PATH", dir)
	for name, script := range map[string]string{
		"loginctl": "#!/bin/sh\n/bin/cat \"$SERVICE_TEST_DIR/linger\"\n",
		"systemctl": `#!/bin/sh
if [ "$2" = show ]; then
    if [ -f "$SERVICE_TEST_DIR/show-error" ]; then
        printf 'Test error\n' >&2
        exit 1
    fi
    if [ -f "$SERVICE_TEST_DIR/show-active-once" ]; then
        /bin/rm "$SERVICE_TEST_DIR/show-active-once"
        printf 'ActiveState=active\nSubState=running\nUnitFileState=enabled\n'
        exit 0
    fi
    printf 'ActiveState=inactive\nSubState=dead\nUnitFileState=enabled\nExecMainCode=1\nExecMainStatus=0\n'
fi
`,
		"linger": "no\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "circus")
	// An install requires an executable outside the transient Go build tree.
	if err := os.Link(exe, binary); err != nil {
		data, err := os.ReadFile(exe)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(binary, data, 0700); err != nil {
			t.Fatal(err)
		}
	}
	creds, err := credfile.Path(credfile.RoleDaemon)
	if err != nil {
		t.Fatal(err)
	}
	if err := credfile.Save(creds, credfile.File{Role: credfile.RoleDaemon, Token: "test", PrincipalID: "device", ControlPlaneURL: "http://127.0.0.1:1"}); err != nil {
		t.Fatal(err)
	}
	// Force the follow-up status query to fail. Install still shows the warning.
	showError := filepath.Join(dir, "show-error")
	if err := os.WriteFile(showError, nil, 0600); err != nil {
		t.Fatal(err)
	}
	out, code := runDaemonCLI(t, binary, "install")
	if code != 0 || !strings.Contains(out, "WARNING: The circus daemon service stops when you log out.") ||
		!strings.Contains(out, "sudo loginctl enable-linger ") || !strings.Contains(out, "journalctl --user") ||
		!strings.Contains(out, "Could not confirm") || strings.Contains(out, "Installed and started") {
		t.Fatalf("install exit %d\n%s", code, out)
	}
	if err := os.Remove(showError); err != nil {
		t.Fatal(err)
	}
	// A successful install command can still leave the daemon stopped.
	if err := os.Remove(filepath.Join(config, "systemd", "user", "circus-daemon.service")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "show-active-once"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	out, code = runDaemonCLI(t, binary, "install")
	if code != 0 || !strings.Contains(out, "WARNING: The circus daemon service is not running. See the logs: journalctl") ||
		strings.Contains(out, "Installed and started") {
		t.Fatalf("inactive install exit %d\n%s", code, out)
	}
	out, code = runDaemonCLI(t, binary, "status")
	if code != 0 || !strings.Contains(out, "not running") || !strings.Contains(out, "exit status 0") ||
		!strings.Contains(out, "at login") || strings.Contains(out, "at boot or login") ||
		!strings.Contains(out, "See the logs for why it stopped: journalctl") {
		t.Fatalf("status exit %d\n%s", code, out)
	}
	if err := os.WriteFile(filepath.Join(dir, "linger"), []byte("yes\n"), 0600); err != nil {
		t.Fatal(err)
	}
	out, code = runDaemonCLI(t, binary, "status")
	if code != 0 || !strings.Contains(out, "at boot or login") {
		t.Fatalf("status exit %d\n%s", code, out)
	}
}

func TestServiceLockWaitsForPreviousService(t *testing.T) {
	t.Setenv("CIRCUS_HOME", t.TempDir())
	old, err := daemonlock.Acquire(daemonlock.Service)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if old != nil {
			_ = old.Close()
		}
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel()
	if _, err := acquireDaemonLock(ctx, daemonlock.Service); !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("acquisition did not wait for the deadline: %v", err)
	} else {
		var held *daemonlock.HeldError
		if !errors.As(err, &held) || held.Mode != daemonlock.Service {
			t.Fatalf("timed out without the service owner: %v", err)
		}
	}

	release := make(chan error, 1)
	timer := time.AfterFunc(150*time.Millisecond, func() { release <- old.Close() })
	defer timer.Stop()
	ctx, finish := context.WithTimeout(t.Context(), 3*time.Second)
	defer finish()
	next, err := acquireDaemonLock(ctx, daemonlock.Service)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if err := <-release; err != nil {
		t.Fatal(err)
	}
	old = nil
}

func TestDaemonServiceLocalErrors(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CIRCUS_HOME", root)
	creds, err := credfile.Path(credfile.RoleDaemon)
	if err != nil {
		t.Fatal(err)
	}
	if err := credfile.Save(creds, credfile.File{
		Role: credfile.RoleDaemon, Token: "test", PrincipalID: "device",
		ControlPlaneURL: "https://127.0.0.1",
		CAFile:          filepath.Join(root, "missing-ca.pem"),
	}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		options []string
		code    int
	}{
		{nil, 1},
		{[]string{"--service"}, serviceExitCode()},
	} {
		if out, code := runDaemonCLI(t, "", tc.options...); code != tc.code || !strings.Contains(out, "read CA file") {
			t.Errorf("bad CA exit %d, want %d\n%s", code, tc.code, out)
		}
	}
	t.Setenv("CIRCUS_HOME", creds)
	if out, code := runDaemonCLI(t, "", "--service"); code != serviceExitCode() || !strings.Contains(out, "not a directory") {
		t.Errorf("lock error exit %d, want %d\n%s", code, serviceExitCode(), out)
	}
}
