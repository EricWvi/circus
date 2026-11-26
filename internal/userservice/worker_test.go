package userservice

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const workerLauncher = "/home/me/.local/share/circus/worker/v1.2.3/circus-worker"

var workerEnv = []string{
	"CIRCUS_BIN=/home/me/.local/bin/circus", "CIRCUS_NODE=/usr/local/bin/node",
	`HTTPS_PROXY=http://user:p%ss@proxy:3128`, "LANG=en_US.UTF-8", "no_proxy=localhost,.internal",
}

func TestWorkerUnit(t *testing.T) {
	s := systemd{config: "/cfg", svc: Worker}
	if s.file() != "/cfg/systemd/user/circus-worker.service" || s.logs() != "journalctl --user -u circus-worker.service -f" {
		t.Errorf("file %q, logs %q", s.file(), s.logs())
	}
	data := s.unitText(workerLauncher, nil, workerEnv)
	checkText(t, data,
		"Description=circus worker\n",
		"Restart=on-failure\nRestartSec=5\nKillMode=mixed\nTimeoutStopSec=30\nSuccessExitStatus=78\nRestartPreventExitStatus=78\n",
		"ExecStart=\""+workerLauncher+"\"\n",
		`Environment="CIRCUS_NODE=/usr/local/bin/node"`,
		`Environment="CIRCUS_BIN=/home/me/.local/bin/circus"`,
		`Environment="HTTPS_PROXY=http://user:p%%ss@proxy:3128"`,
		`Environment="no_proxy=localhost,.internal"`,
		"WantedBy=default.target\n")
	if d := s.definition([]byte(data)); d.Program() != workerLauncher || len(d.Argv) != 1 || d.Env["HTTPS_PROXY"] != "http://user:p%ss@proxy:3128" ||
		d.Env["CIRCUS_NODE"] != "/usr/local/bin/node" {
		t.Errorf("definition = %+v", d)
	}
	if daemon := (systemd{svc: Daemon}).unitText("/bin/circus", nil, nil); strings.Contains(daemon, "KillMode") ||
		strings.Contains(daemon, "TimeoutStopSec") || strings.Contains(daemon, "RestartPreventExitStatus") {
		t.Errorf("the daemon unit changed:\n%s", daemon)
	}
	if runtime.GOOS != "linux" {
		return
	}
	analyze, err := exec.LookPath("systemd-analyze")
	if err != nil {
		t.Skip("systemd-analyze is unavailable")
	}
	dir := t.TempDir()
	launcher := filepath.Join(dir, "circus-worker")
	writeTestFile(t, launcher, "#!/bin/sh\n", 0o700)
	unit := filepath.Join(dir, Worker.unitName())
	writeTestFile(t, unit, s.unitText(launcher, nil, workerEnv), 0o600)
	for _, name := range []string{"basic", "default", "shutdown"} {
		writeTestFile(t, filepath.Join(dir, name+".target"), "[Unit]\nDescription=Verification target\nDefaultDependencies=no\n", 0o600)
	}
	cmd := exec.CommandContext(t.Context(), analyze, "--user", "verify", unit)
	cmd.Env = append(os.Environ(), "XDG_RUNTIME_DIR="+dir, "SYSTEMD_UNIT_PATH="+dir, "SYSTEMD_LOG_LEVEL=warning")
	if out, err := cmd.CombinedOutput(); err != nil || len(out) != 0 {
		t.Fatalf("verify: %v\n%s", err, out)
	}
}

func TestWorkerPlist(t *testing.T) {
	l := launchd{home: "/Users/me", svc: Worker}
	if l.file() != "/Users/me/Library/LaunchAgents/com.github.ericwvi.circus.worker.plist" ||
		l.logs() != "tail -f '/Users/me/Library/Logs/circus/worker.log'" {
		t.Errorf("file %q, logs %q", l.file(), l.logs())
	}
	data := l.plist(workerLauncher, nil, workerEnv)
	checkText(t, data,
		"<key>Label</key>\n  <string>com.github.ericwvi.circus.worker</string>",
		"<key>ProgramArguments</key>\n  <array>\n    <string>"+workerLauncher+"</string>\n  </array>",
		"<key>RunAtLoad</key>\n  <true/>", "<key>SuccessfulExit</key>\n    <false/>",
		"<key>ThrottleInterval</key>\n  <integer>5</integer>\n  <key>ExitTimeOut</key>\n  <integer>30</integer>\n",
		"<key>StandardErrorPath</key>\n  <string>/Users/me/Library/Logs/circus/worker.log</string>",
		"<key>CIRCUS_NODE</key>\n    <string>/usr/local/bin/node</string>",
		"<key>HTTPS_PROXY</key>\n    <string>http://user:p%ss@proxy:3128</string>",
		"<key>no_proxy</key>\n    <string>localhost,.internal</string>")
	if err := xml.Unmarshal([]byte(data), new(struct{})); err != nil {
		t.Errorf("invalid plist XML: %v", err)
	}
	if d := l.definition([]byte(data)); d.Program() != workerLauncher || d.Env["CIRCUS_BIN"] != "/home/me/.local/bin/circus" {
		t.Errorf("definition = %+v", d)
	}
}

func TestWorkerServiceIsSeparateFromTheDaemon(t *testing.T) {
	for _, platform := range []string{"linux", "darwin"} {
		t.Run(platform, func(t *testing.T) {
			fakeService(t, platform)
			saveCredential(t, "", "daemon")
			if err := InstallDaemon("", io.Discard); err != nil {
				t.Fatal(err)
			}
			if _, ok, err := Worker.Installed(); ok || err != nil {
				t.Fatalf("worker installed = %t, %v", ok, err)
			}
			if err := Worker.Start(); !errors.Is(err, ErrNotInstalled) || !strings.Contains(err.Error(), "run `circus worker install`") {
				t.Errorf("start = %v", err)
			}
			if err := Worker.Install(workerLauncher, nil, workerEnv, io.Discard); err != nil {
				t.Fatal(err)
			}
			if Worker.Program() != workerLauncher || Daemon.Program() != fakeExecutable {
				t.Errorf("programs: worker %q, daemon %q", Worker.Program(), Daemon.Program())
			}
			if err := Worker.Uninstall(); err != nil {
				t.Fatal(err)
			}
			if Daemon.Program() != fakeExecutable {
				t.Error("uninstalling the worker removed the daemon")
			}
		})
	}
}

func TestWorkerRestartWithCredentials(t *testing.T) {
	f := fakeService(t, "linux")
	t.Setenv("CIRCUS_HOME", filepath.Join(f.dir, "home"))
	env, _, err := Environment()
	if err != nil {
		t.Fatal(err)
	}
	if err := Worker.Install(workerLauncher, nil, env, io.Discard); err != nil {
		t.Fatal(err)
	}
	f.checkCalls(t, "systemctl --user is-system-running", "systemctl --user daemon-reload",
		"systemctl --user enable "+filepath.Join(f.config, "systemd", "user", "circus-worker.service"), "systemctl --user restart circus-worker.service")
	if restarted, err := Worker.RestartWithCredentials(filepath.Join(f.dir, "home", "config", "daemon.json")); restarted || err != nil {
		t.Errorf("restart for the daemon's credential = %t, %v", restarted, err)
	}
	if restarted, err := Worker.RestartWithCredentials(filepath.Join(f.dir, "home", "config", "worker.json")); !restarted || err != nil {
		t.Errorf("restart for the worker's credential = %t, %v", restarted, err)
	}
	f.checkCalls(t, "systemctl --user restart circus-worker.service")
}

// Rewrite changes only the file; RestartIfRunning then queues the restarts
// in the order given, without waiting for them.
func TestRewriteThenRestartInOrder(t *testing.T) {
	next := strings.Replace(workerLauncher, "v1.2.3", "v1.3.0", 1)
	t.Run("linux", func(t *testing.T) {
		f := fakeService(t, "linux")
		if err := Worker.Rewrite(next, nil, workerEnv); !errors.Is(err, ErrNotInstalled) {
			t.Fatalf("rewrite before install = %v", err)
		}
		saveCredential(t, "", "daemon")
		if err := InstallDaemon("", io.Discard); err != nil {
			t.Fatal(err)
		}
		if err := Worker.Install(workerLauncher, nil, workerEnv, io.Discard); err != nil {
			t.Fatal(err)
		}
		// The fake keeps one state for both units; only what follows counts.
		os.Remove(filepath.Join(f.dir, "calls"))
		if err := Worker.Rewrite(next, nil, workerEnv); err != nil {
			t.Fatal(err)
		}
		f.checkCalls(t, "systemctl --user is-system-running", "systemctl --user daemon-reload")
		if Worker.Program() != next {
			t.Errorf("program = %q", Worker.Program())
		}
		if r := RestartIfRunning(RestartRequest{Service: Worker, Rewritten: true}, RestartRequest{Service: Daemon}); r[0] != (Restart{Requested: true}) || r[1] != (Restart{Requested: true}) {
			t.Fatalf("restart = %+v", r)
		}
		// Both states first, then one request naming both units, worker first:
		// every job exists before either restart can stop the caller.
		show := " --property=ActiveState,SubState,MainPID,UnitFileState,ExecMainCode,ExecMainStatus"
		f.checkCalls(t, "systemctl --user show circus-worker.service"+show, "systemctl --user show circus-daemon.service"+show,
			"systemctl --user try-restart --no-block circus-worker.service circus-daemon.service")
		// A unit that stopped itself is started; a stopped one stays stopped.
		f.response(t, "systemctl.show.out", "ActiveState=inactive\nExecMainCode=exited\nExecMainStatus=78\n")
		if r := RestartIfRunning(RestartRequest{Service: Worker, Rewritten: true})[0]; r != (Restart{Requested: true}) {
			t.Fatalf("restart = %+v", r)
		}
		f.checkCalls(t, "systemctl --user show circus-worker.service"+show, "systemctl --user start --no-block circus-worker.service")
		f.response(t, "systemctl.show.out", "ActiveState=inactive\nExecMainCode=killed\nExecMainStatus=15\n")
		if r := RestartIfRunning(RestartRequest{Service: Worker, Rewritten: true})[0]; r != (Restart{}) {
			t.Fatalf("restart = %+v", r)
		}
		f.checkCalls(t, "systemctl --user show circus-worker.service"+show)
		// A failed reload restores the old file; a failed request is
		// reported for every unit it named.
		f.response(t, "systemctl.daemon-reload.fail-once", "Test failure")
		if err := Worker.Rewrite(workerLauncher, nil, workerEnv); err == nil || !strings.Contains(err.Error(), "Test failure") {
			t.Fatalf("rewrite = %v", err)
		}
		if Worker.Program() != next {
			t.Errorf("program after a failed rewrite = %q", Worker.Program())
		}
		os.Remove(filepath.Join(f.dir, "systemctl.show.out"))
		f.response(t, "active", "")
		f.response(t, "systemctl.try-restart.fail-once", "Test failure")
		f.checkCalls(t, "systemctl --user is-system-running", "systemctl --user daemon-reload", "systemctl --user daemon-reload")
		// The one request covered both: its failure names both units and is
		// the same error for each, not a failure of each.
		r := RestartIfRunning(RestartRequest{Service: Worker, Rewritten: true}, RestartRequest{Service: Daemon})
		if r[0].Err == nil || r[0].Err != r[1].Err || r[0].Requested || r[1].Requested ||
			!strings.Contains(r[0].Err.Error(), "the one request to restart circus-worker.service and circus-daemon.service failed (it covered all of them)") {
			t.Fatalf("restart = %+v", r)
		}
		// Alone, a unit's failure is its own.
		f.response(t, "systemctl.try-restart.fail-once", "Test failure")
		os.Remove(filepath.Join(f.dir, "calls"))
		if r := RestartIfRunning(RestartRequest{Service: Daemon})[0]; r.Err == nil || !strings.HasPrefix(r.Err.Error(), "restart the circus daemon service: ") {
			t.Fatalf("restart = %+v", r)
		}
		os.Remove(filepath.Join(f.dir, "calls"))
		// A unit that stopped itself is started before the running one is
		// restarted: the start can't stop the caller, the restart can.
		f.response(t, "show-circus-worker.service", "ActiveState=active\nSubState=running\n")
		f.response(t, "show-circus-daemon.service", "ActiveState=inactive\nExecMainCode=exited\nExecMainStatus=78\n")
		if r := RestartIfRunning(RestartRequest{Service: Worker, Rewritten: true}, RestartRequest{Service: Daemon}); r[0] != (Restart{Requested: true}) || r[1] != (Restart{Requested: true}) {
			t.Fatalf("restart = %+v", r)
		}
		f.checkCalls(t, "systemctl --user show circus-worker.service"+show, "systemctl --user show circus-daemon.service"+show,
			"systemctl --user start --no-block circus-daemon.service", "systemctl --user try-restart --no-block circus-worker.service")
	})
	t.Run("darwin", func(t *testing.T) {
		f := fakeService(t, "darwin")
		saveCredential(t, "", "daemon")
		if err := InstallDaemon("", io.Discard); err != nil {
			t.Fatal(err)
		}
		if err := Worker.Install(workerLauncher, nil, workerEnv, io.Discard); err != nil {
			t.Fatal(err)
		}
		w, d := launchd{home: f.home, svc: Worker}, launchd{home: f.home, svc: Daemon}
		os.Remove(filepath.Join(f.dir, "calls"))
		f.response(t, "loaded", "")
		if err := Worker.Rewrite(next, nil, workerEnv); err != nil {
			t.Fatal(err)
		}
		f.checkCalls(t)
		if Worker.Program() != next {
			t.Errorf("program = %q", Worker.Program())
		}
		if r := RestartIfRunning(RestartRequest{Service: Worker, Rewritten: true}, RestartRequest{Service: Daemon}); r[0] != (Restart{Requested: true}) || r[1] != (Restart{Requested: true}) {
			t.Fatalf("restart = %+v", r)
		}
		// One detached helper, started once for both: the worker is reloaded
		// from its new plist, then the daemon, whose plist did not change,
		// restarts in place.
		f.checkCalls(t, "launchctl print "+w.target(), "launchctl print "+d.target(),
			"launchctl bootout "+w.target(), "launchctl print "+w.target(), "launchctl bootstrap "+launchDomain()+" "+w.file(),
			"launchctl kickstart -k "+d.target())
		// A worker whose plist did not change restarts in place too.
		if r := RestartIfRunning(RestartRequest{Service: Worker}); r[0] != (Restart{Requested: true}) {
			t.Fatalf("restart = %+v", r)
		}
		f.checkCalls(t, "launchctl print "+w.target(), "launchctl kickstart -k "+w.target())
	})
}

func TestDaemonPlistHasTheDefaultExitTimeout(t *testing.T) {
	if strings.Contains(launchd{home: "/Users/me", svc: Daemon}.plist("/bin/circus", nil, nil), "ExitTimeOut") {
		t.Error("the daemon plist sets ExitTimeOut")
	}
}

func TestTerminalStatus(t *testing.T) {
	t.Run("linux", func(t *testing.T) {
		f := fakeService(t, "linux")
		if err := Worker.Install(workerLauncher, nil, workerEnv, io.Discard); err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			show              string
			stopped, terminal bool
		}{
			{"ActiveState=inactive\nExecMainCode=exited\nExecMainStatus=78\n", true, true},
			{"ActiveState=inactive\nExecMainCode=exited\nExecMainStatus=0\n", true, false},
			{"ActiveState=activating\nSubState=auto-restart\nExecMainCode=exited\nExecMainStatus=1\n", false, false},
			{"ActiveState=active\nExecMainCode=exited\nExecMainStatus=78\n", false, false},
		} {
			f.response(t, "systemctl.show.out", tc.show)
			st, err := Worker.Get()
			if err != nil || st.StoppedAfterExit != tc.stopped || st.Terminal != tc.terminal {
				t.Errorf("%q: status %+v, %v", tc.show, st, err)
			}
		}
	})
	t.Run("darwin", func(t *testing.T) {
		f := fakeService(t, "darwin")
		if err := Worker.Install(workerLauncher, nil, workerEnv, io.Discard); err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			print    string
			terminal bool
		}{
			// Exit 0 is both the terminal status and a plain kill on macOS:
			// the worker's stop file tells them apart, not the status.
			{"state = not running\nlast exit code = 0\n", false},
			{"state = not running\nlast exit code = 1\n", false},
			{"state = running\npid = 7\nlast exit code = 0\n", false},
		} {
			f.response(t, "launchctl.print.out", tc.print)
			st, err := Worker.Get()
			if err != nil || st.Terminal != tc.terminal {
				t.Errorf("%q: status %+v, %v", tc.print, st, err)
			}
		}
	})
}

// The helper waits for an unload longer than the worker's graceful stop,
// bootstraps even after a failed unload, and goes on to the next agent
// after a failure; each outcome is logged where a killed parent can't lose
// it.
func TestLaunchReloadHelperGoesOn(t *testing.T) {
	if unloadTimeout < time.Duration(Worker.stopTimeout)*time.Second+10*time.Second {
		t.Errorf("unload wait %s is not longer than the worker's ExitTimeOut (%d s) with a margin", unloadTimeout, Worker.stopTimeout)
	}
	if launchReloadTimeout <= unloadTimeout {
		t.Errorf("a helper step (%s) cannot outlast its unload wait (%s)", launchReloadTimeout, unloadTimeout)
	}
	f := fakeService(t, "darwin")
	w, d := launchd{home: f.home, svc: Worker}, launchd{home: f.home, svc: Daemon}
	log := filepath.Join(f.home, "Library", "Logs", "circus", "reload.log")
	f.response(t, "loaded", "")
	f.response(t, "launchctl.bootout.fail", "Boot-out failed: 5: Input/output error")
	var out bytes.Buffer
	if code := RunLaunchReload([]string{f.home, "worker=reload", "daemon=kickstart"}, &out); code != reloadUnloadFailure {
		t.Errorf("exit code %d\n%s", code, out.String())
	}
	f.checkCalls(t, "launchctl bootout "+w.target(), "launchctl bootstrap "+launchDomain()+" "+w.file(), "launchctl kickstart -k "+d.target())
	checkText(t, readPrivate(t, log), w.label()+": unload: ", "bootstrapping anyway", w.label()+": loaded", d.label()+": restarted")
	os.Remove(filepath.Join(f.dir, "launchctl.bootout.fail"))
	// Both bootstraps of the worker fail: the daemon is still restarted.
	f.response(t, "launchctl.bootstrap.fail", "Bootstrap failed: 5: Input/output error")
	out.Reset()
	if code := RunLaunchReload([]string{f.home, "worker=reload", "daemon=kickstart"}, &out); code != reloadBootstrapFailure {
		t.Errorf("exit code %d\n%s", code, out.String())
	}
	f.checkCalls(t, "launchctl bootout "+w.target(), "launchctl print "+w.target(), "launchctl bootstrap "+launchDomain()+" "+w.file(),
		"launchctl print "+launchDomain(), "launchctl bootstrap "+launchDomain()+" "+w.file(), "launchctl print "+launchDomain(), "launchctl kickstart -k "+d.target())
	checkText(t, readPrivate(t, log), w.label()+": bootstrap: ", "Bootstrap failed")
	if code := RunLaunchReload([]string{f.home, "worker=restart"}, &out); code != reloadUnloadFailure {
		t.Errorf("unknown step: exit code %d", code)
	}
}
