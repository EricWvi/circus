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
			// No service is installed for this user: nothing to remove.
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("HOME", t.TempDir())
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

const fakeSystemctl = `#!/bin/sh
printf '%s\n' "$*" >> "$SYSTEMCTL_CALLS"
case "$2" in
    disable)
        if [ "$3" = --now ]; then echo "disable --now kills the caller" >&2; exit 1; fi ;;
    stop|kill) echo "stopping from inside kills the caller" >&2; exit 1 ;;
esac
exit 0
`

// A daemon running as the installed service removes the service and its
// credential only when the control plane proves it revoked this very
// credential's identity (403, Circus-Auth: revoked, the principal id), on
// Linux without stopping its own unit; and only while the unit still reads
// that credential file, the file still holds that credential, and it runs
// as the service. Run in the foreground, it removes only the credential
// file, on the same proof, while the file still holds that credential and
// no installed daemon service reads it, and never changes the service. An
// unknown credential (401, Circus-Auth: rejected: the control plane may run
// on an empty or old database), or a 401 with the revoked marker, which
// proves nothing, only stops it.
func TestDaemonServiceRemovesItselfWhenRejected(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the fake systemctl is for Linux; macOS runs the real launchd test in CI")
	}
	for _, tc := range []struct {
		name     string
		answer   string // "unknown", or the principal id the control plane revoked (with "/401": answered 401, not 403)
		service  bool
		other    bool // the unit reads another credential
		rejoin   bool // a join wrote a new credential to the path before the answer
		removed  bool // the service and the credential file
		credOnly bool // only the credential file
		log      string
	}{
		{"revoked", "device", true, false, false, true, false, "the service and the credential file were removed"},
		{"unknown credential", "unknown", true, false, false, false, false, "HTTP 401 authentication rejected; the control plane does not know"},
		{"another identity revoked", "other-device", true, false, false, false, false, `revoked "other-device", not this credential's "device"`},
		{"another credential's service", "device", true, true, false, false, false, "no longer reads"},
		{"replaced by a rejoin", "device", true, false, true, false, false, "another join replaced it"},
		{"revoked with 401", "device/401", true, false, false, false, false, "daemon link: HTTP 401 credential refused\n"},
		{"foreground", "device", false, true, false, false, true, "so the credential file was removed; join again with a new link"},
		{"foreground, the service's file", "device", false, false, false, false, false, "; it is left to the service"},
		{"foreground, unknown credential", "unknown", false, true, false, false, false, "HTTP 401 authentication rejected; the control plane does not know"},
		{"foreground, revoked with 401", "device/401", false, true, false, false, false, "daemon link: HTTP 401 credential refused\n"},
		{"foreground, another identity revoked", "other-device", false, true, false, false, false, `revoked "other-device", not this credential's "device"`},
		{"foreground, replaced by a rejoin", "device", false, true, true, false, false, "the file now holds another or unreadable credential; kept"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			home := filepath.Join(dir, "circus home")
			t.Setenv("CIRCUS_HOME", home)
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
			t.Setenv("HOME", filepath.Join(dir, "user"))
			calls := filepath.Join(dir, "calls")
			t.Setenv("SYSTEMCTL_CALLS", calls)
			bin := filepath.Join(dir, "bin")
			if err := os.MkdirAll(bin, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(bin, "systemctl"), []byte(fakeSystemctl), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			path, err := credfile.Path(credfile.RoleDaemon)
			if err != nil {
				t.Fatal(err)
			}
			var rejoined credfile.File
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.rejoin {
					// The rejoin's join wrote its credential after this
					// daemon read the old one.
					rejoined = credfile.File{Role: credfile.RoleDaemon, Token: "new", PrincipalID: "device-2", ControlPlaneURL: "http://" + r.Host}
					if err := credfile.Save(path, rejoined); err != nil {
						t.Error(err)
					}
				}
				if tc.answer == "unknown" {
					w.Header().Set("Circus-Auth", "rejected")
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				principal, unauthorized := strings.CutSuffix(tc.answer, "/401")
				w.Header().Set("Circus-Auth", "revoked")
				w.Header().Set("Circus-Principal", principal)
				if unauthorized {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				w.WriteHeader(http.StatusForbidden)
			}))
			defer server.Close()
			if err := credfile.Save(path, credfile.File{Role: credfile.RoleDaemon, Token: "test", PrincipalID: "device", ControlPlaneURL: server.URL}); err != nil {
				t.Fatal(err)
			}
			unit := filepath.Join(dir, "config", "systemd", "user", "circus-daemon.service")
			execStart := `ExecStart="/opt/circus" "daemon" "--service"`
			if tc.other {
				execStart += ` "--credentials" "` + filepath.Join(dir, "rejoined.json") + `"`
			}
			if err := os.MkdirAll(filepath.Dir(unit), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(unit, []byte("[Service]\n"+execStart+"\nEnvironment=\"CIRCUS_HOME="+home+"\"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			var args []string
			if tc.service {
				args = append(args, "--service")
			}
			out, code := runDaemonCLI(t, "", args...)
			wantCode := 1
			if tc.service {
				wantCode = serviceExitCode()
			}
			if code != wantCode || !strings.Contains(out, tc.log) {
				t.Fatalf("status %d, want %d; want the log to say %q:\n%s", code, wantCode, tc.log, out)
			}
			// The terminal link error, said apart from the cleanup.
			// A 401 with the revoked marker proves nothing: a plain refusal.
			if principal, unauthorized := strings.CutSuffix(tc.answer, "/401"); unauthorized {
				if strings.Contains(out, "credential revoked") || strings.Contains(out, "the control plane revoked") || strings.Contains(out, "left to the service") {
					t.Fatalf("a 401 was taken as the proof of a revocation:\n%s", out)
				}
			} else if want := fmt.Sprintf("daemon link: HTTP 403 credential revoked (device %q was deleted or replaced)", principal); tc.answer != "unknown" && !strings.Contains(out, want) {
				t.Fatalf("want the log to say %q:\n%s", want, out)
			}
			unitText, unitErr := os.ReadFile(unit)
			_, credErr := os.Stat(path)
			if gone := errors.Is(unitErr, os.ErrNotExist) && errors.Is(credErr, os.ErrNotExist); gone != tc.removed {
				t.Fatalf("unit %v, credential %v; want removed %v\n%s", unitErr, credErr, tc.removed, out)
			}
			if !tc.removed && (unitErr != nil || (credErr != nil) != tc.credOnly) {
				t.Fatalf("unit %v, credential %v; want only the credential removed %v\n%s", unitErr, credErr, tc.credOnly, out)
			}
			if tc.credOnly && !errors.Is(credErr, os.ErrNotExist) {
				t.Fatalf("credential %v, want it removed\n%s", credErr, out)
			}
			if !tc.removed && !strings.Contains(string(unitText), execStart) {
				t.Fatalf("the unit changed:\n%s", unitText)
			}
			if tc.rejoin {
				if got, err := credfile.Load(path); err != nil || got.Token != rejoined.Token {
					t.Fatalf("the rejoin's credential is gone: %+v, %v", got, err)
				}
			}
			data, _ := os.ReadFile(calls)
			want := ""
			if tc.removed {
				want = "--user is-system-running\n--user disable circus-daemon.service\n--user daemon-reload\n"
			}
			if string(data) != want {
				t.Fatalf("systemctl calls:\n%s\nwant:\n%s", data, want)
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
