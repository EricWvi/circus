package userservice

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSystemdStatus(t *testing.T) {
	f := fakeService(t, "linux")
	saveCredential(t, "", "daemon")
	if err := InstallDaemon("", io.Discard); err != nil {
		t.Fatal(err)
	}
	f.response(t, "loginctl.show-user.out", "no\n")
	f.response(t, "systemctl.show.out", "ActiveState=active\nSubState=running\nMainPID=4242\nUnitFileState=enabled\nExecMainStatus=0\n")
	st, err := Daemon.Get()
	if err != nil || !st.Installed || !st.Running || !st.Enabled || st.PID != 4242 || st.State != "active (running)" || st.LastExit != "" {
		t.Fatalf("running status = %+v, %v", st, err)
	}
	if st.Logs != "journalctl --user -u circus-daemon.service -f" || len(st.Advice) != 1 || !strings.Contains(st.Advice[0], "sudo loginctl enable-linger ") {
		t.Errorf("log command or linger advice: %+v", st)
	}
	if advice := Daemon.Linger(); len(st.Advice) != 1 || advice != st.Advice[0] {
		t.Errorf("Linger = %q, status advice = %v", advice, st.Advice)
	}
	calls, err := os.ReadFile(filepath.Join(f.dir, "calls"))
	if err != nil || !strings.Contains(string(calls), "loginctl show-user ") || !strings.Contains(string(calls), " --property=Linger --value\n") {
		t.Errorf("missing linger query in %s (%v)", calls, err)
	}
	f.response(t, "loginctl.show-user.out", "yes\n")
	f.response(t, "systemctl.show.out", "ActiveState=activating\nSubState=auto-restart\nMainPID=0\nUnitFileState=disabled\nExecMainCode=1\nExecMainStatus=1\n")
	st, err = Daemon.Get()
	if err != nil || st.Running || st.PID != 0 || st.Enabled || st.State != "activating (auto-restart)" || !st.AtBoot || st.LastExit != "exit status 1" || len(st.Advice) != 0 {
		t.Errorf("restarting status = %+v, %v", st, err)
	}
	f.response(t, "systemctl.show.out", "ActiveState=inactive\nSubState=dead\nMainPID=0\nUnitFileState=enabled\nExecMainCode=2\nExecMainStatus=15\n")
	st, err = Daemon.Get()
	if err != nil || st.Running || !st.Enabled || st.State != "inactive (dead)" || st.LastExit != "signal 15" {
		t.Errorf("stopped status = %+v, %v", st, err)
	}
}

func TestLaunchdStatus(t *testing.T) {
	f := fakeService(t, "darwin")
	saveCredential(t, "", "daemon")
	if err := InstallDaemon("", io.Discard); err != nil {
		t.Fatal(err)
	}
	f.response(t, "launchctl.print.out", "gui/501/circus = {\n\tstate = running\n\tpid = 777\n\tlast exit code = (never exited)\n\tendpoints = {\n\t\tstate = active\n\t}\n}\n")
	st, err := Daemon.Get()
	if err != nil || !st.Installed || !st.Running || !st.Enabled || st.PID != 777 || st.State != "running" || st.LastExit != "" {
		t.Fatalf("running status = %+v, %v", st, err)
	}
	if want := "tail -f '" + filepath.Join(f.home, "Library", "Logs", "circus", "daemon.log") + "'"; st.Logs != want {
		t.Errorf("logs = %q, want %q", st.Logs, want)
	}
	f.response(t, "launchctl.print.out", "\tstate = waiting\n\tlast exit code = 34\n")
	st, err = Daemon.Get()
	if err != nil || st.Running || st.PID != 0 || st.State != "waiting" || st.LastExit != "34" {
		t.Errorf("waiting status = %+v, %v", st, err)
	}
	if err := Daemon.Stop(); err != nil {
		t.Fatal(err)
	}
	st, err = Daemon.Get()
	if err != nil || !st.Installed || !st.Enabled || st.Running || st.State != "not loaded" || len(st.Advice) != 0 {
		t.Errorf("stopped status = %+v, %v", st, err)
	}
}

func TestSystemdUnreachable(t *testing.T) {
	for _, tc := range []struct {
		name, response, advice string
		wsl                    bool
	}{
		{"missing systemctl", "", "no running systemd", false},
		{"no systemd", "offline\n", "Run `circus daemon`", false},
		{"unknown manager", "unknown\n", "no running systemd user manager", false},
		{"no user bus", "Failed to connect to bus: No medium found", "XDG_RUNTIME_DIR=/run/user/", false},
		{"retry from login", "Failed to connect to bus: No medium found\nunknown\n", "rerun `circus daemon install` on its own", false},
		{"new bus error", "Failed to connect to user scope bus via local transport", "Log in directly", false},
		{"WSL", "offline\n", "/etc/wsl.conf", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := fakeService(t, "linux")
			isWSL = func() bool { return tc.wsl }
			saveCredential(t, "", "daemon")
			if tc.response == "" {
				t.Setenv("PATH", t.TempDir())
			} else {
				f.response(t, "systemctl.is-system-running.fail", tc.response)
			}
			if err := InstallDaemon("", io.Discard); err == nil || !strings.Contains(err.Error(), tc.advice) {
				t.Errorf("install = %v, want %q", err, tc.advice)
			}
			path := filepath.Join(f.config, "systemd", "user", Daemon.unitName())
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("install wrote the unit: stat = %v", err)
			}
			if tc.response == "" {
				f.checkCalls(t)
			} else {
				f.checkCalls(t, "systemctl --user is-system-running")
			}
		})
	}
}

func TestLaunchdBootoutFailureKeepsServiceFile(t *testing.T) {
	f := fakeService(t, "darwin")
	saveCredential(t, "", "daemon")
	if err := InstallDaemon("", io.Discard); err != nil {
		t.Fatal(err)
	}
	f.response(t, "launchctl.bootout.fail", "Permission denied")
	if err := Daemon.Uninstall(); err == nil || !strings.Contains(err.Error(), "Permission denied") {
		t.Errorf("uninstall = %v", err)
	}
	if st, err := Daemon.Get(); err != nil || !st.Installed {
		t.Errorf("status = %+v, %v", st, err)
	}
}

func TestLaunchdDisabledStatus(t *testing.T) {
	f := fakeService(t, "darwin")
	saveCredential(t, "", "daemon")
	if err := InstallDaemon("", io.Discard); err != nil {
		t.Fatal(err)
	}
	f.response(t, "launchctl.print-disabled.out", "disabled services = {\n    \""+Daemon.label()+"\" => disabled\n}\n")
	st, err := Daemon.Get()
	if err != nil || !st.Installed || st.Enabled || st.AtBoot {
		t.Fatalf("disabled status = %+v, %v", st, err)
	}
}

func TestLaunchdRestartIfRunningKickstartsOnlyLoadedAgents(t *testing.T) {
	f := fakeService(t, "darwin")
	saveCredential(t, "", "daemon")
	if err := InstallDaemon("", io.Discard); err != nil {
		t.Fatal(err)
	}
	target := launchd{svc: Daemon}.target()
	print := "launchctl print " + target
	path := filepath.Join(f.home, "Library", "LaunchAgents", Daemon.label()+".plist")
	bootstrap := "launchctl bootstrap " + launchDomain() + " " + path
	f.checkCalls(t, print, bootstrap)
	if r := RestartIfRunning(RestartRequest{Service: Daemon})[0]; r.Err != nil || !r.Requested {
		t.Fatalf("restart loaded agent = %+v", r)
	}
	// Its plist is unchanged: the process restarts in place and the agent
	// stays loaded.
	f.checkCalls(t, print, "launchctl kickstart -k "+target)
	if err := Daemon.Stop(); err != nil {
		t.Fatal(err)
	}
	f.checkCalls(t, print, "launchctl bootout "+target)
	if r := RestartIfRunning(RestartRequest{Service: Daemon})[0]; r.Err != nil || r.Requested {
		t.Fatalf("restart stopped agent = %+v", r)
	}
	f.checkCalls(t, print)
	if err := Daemon.Restart(); err != nil {
		t.Fatal(err)
	}
	f.checkCalls(t, print, bootstrap)
}

func TestStatusHintsAtDaemonExitLogs(t *testing.T) {
	for _, tc := range []struct{ platform, response, output string }{
		{"linux", "systemctl.show.out", "ActiveState=inactive\nExecMainCode=1\nExecMainStatus=78\n"},
		{"darwin", "launchctl.print.out", "state = not running\nlast exit code = 0 (successful)\n"},
	} {
		t.Run(tc.platform, func(t *testing.T) {
			f := fakeService(t, tc.platform)
			saveCredential(t, "", "daemon")
			if err := InstallDaemon("", io.Discard); err != nil {
				t.Fatal(err)
			}
			f.response(t, tc.response, tc.output)
			st, err := Daemon.Get()
			if err != nil || st.Running || !st.StoppedAfterExit || st.Logs == "" {
				t.Fatalf("status = %+v, %v", st, err)
			}
		})
	}
}

func TestLaunchdRestartReportsMissingGUIDomain(t *testing.T) {
	f := fakeService(t, "darwin")
	path := filepath.Join(f.home, "Library", "LaunchAgents", Daemon.label()+".plist")
	if err := writePrivate(path, "test"); err != nil {
		t.Fatal(err)
	}
	f.response(t, "launchctl.print.fail", "Could not find domain for")
	if r := RestartIfRunning(RestartRequest{Service: Daemon})[0]; r.Requested || !errors.Is(r.Err, errNoGUIDomain) {
		t.Fatalf("restart = %+v", r)
	}
	f.checkCalls(t, "launchctl print "+launchd{svc: Daemon}.target())
}

func TestLaunchdInstallStatusWaitsForRunning(t *testing.T) {
	f := fakeService(t, "darwin")
	path := filepath.Join(f.home, "Library", "LaunchAgents", Daemon.label()+".plist")
	if err := writePrivate(path, "test"); err != nil {
		t.Fatal(err)
	}
	f.response(t, "loaded", "")
	writeTestFile(t, filepath.Join(f.bin, "launchctl"), strings.Replace(fakeManager, "launchctl:print)\n", `launchctl:print)
        if [ -f "$dir/starting" ]; then
            printf 'state = running\npid = 123\n'
        else
            : > "$dir/starting"
            printf 'state = waiting\n'
        fi
`, 1), 0700)
	st, err := Daemon.pollInstallStatus(time.Second)
	if err != nil || !st.Running || st.PID != 123 {
		t.Fatalf("running status = %+v, %v", st, err)
	}
	print := "launchctl print " + launchd{svc: Daemon}.target()
	disabled := "launchctl print-disabled " + launchDomain()
	f.checkCalls(t, print, disabled, print, disabled)
}

func TestSystemdInstallStatusCompletesQueriesAfterTimeout(t *testing.T) {
	f := fakeService(t, "linux")
	saveCredential(t, "", "daemon")
	if err := InstallDaemon("", io.Discard); err != nil {
		t.Fatal(err)
	}
	// The linger query outlasts the whole poll timeout. Its answer must still
	// be part of the result instead of being cut off at the deadline.
	writeTestFile(t, filepath.Join(f.bin, "loginctl"), "#!/bin/sh\n/bin/sleep 0.3\nprintf 'no\\n'\n", 0o700)
	st, err := Daemon.pollInstallStatus(10 * time.Millisecond)
	if err != nil || !st.Installed || len(st.Advice) != 1 || !strings.Contains(st.Advice[0], "sudo loginctl enable-linger ") {
		t.Fatalf("install status = %+v, %v", st, err)
	}
}
