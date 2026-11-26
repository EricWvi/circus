package userservice

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/EricWvi/circus/internal/credfile"
	"github.com/EricWvi/circus/internal/daemonlock"
)

const fakeExecutable = `/opt/circus tools/circus%"\${USER}`

const showCall = "systemctl --user show circus-daemon.service --property=ActiveState,SubState,MainPID,UnitFileState,ExecMainCode,ExecMainStatus"

const fakeManager = `#!/bin/sh
tool=${0##*/}
dir=$SERVICE_TEST_DIR
printf '%s %s\n' "$tool" "$*" >> "$dir/calls"
if [ "$tool" = systemctl ]; then shift; fi
response="$dir/$tool.$1"
if [ -f "$response.fail-once" ]; then
    /bin/cat "$response.fail-once" >&2
    /bin/rm "$response.fail-once"
    exit 1
fi
if [ -f "$response.fail" ]; then
    /bin/cat "$response.fail" >&2
    exit 1
fi
case "$tool:$1" in
    systemctl:enable) : > "$dir/enabled" ;;
    systemctl:disable)
        /bin/rm -f "$dir/enabled"
        if [ "$2" = --now ]; then /bin/rm -f "$dir/active"; fi
        ;;
    systemctl:start|systemctl:restart) : > "$dir/active" ;;
    systemctl:stop) /bin/rm -f "$dir/active" ;;
    systemctl:try-restart)
        if [ -f "$dir/active" ]; then : > "$dir/tried-restart"; fi
        ;;
    systemctl:show)
        if [ -f "$dir/show-$2" ]; then /bin/cat "$dir/show-$2"; exit 0; fi
        if [ ! -f "$response.out" ]; then
            if [ -f "$dir/active" ]; then printf 'ActiveState=active\n'; else printf 'ActiveState=inactive\n'; fi
            if [ -f "$dir/enabled" ]; then printf 'UnitFileState=enabled\n'; else printf 'UnitFileState=disabled\n'; fi
        fi
        ;;
    launchctl:print)
        case "$2" in
            gui/*/*)
                if [ ! -f "$dir/loaded" ]; then
                    printf 'Could not find service\n' >&2
                    exit 1
                fi
                if [ -f "$dir/unloading" ]; then /bin/rm -f "$dir/loaded" "$dir/unloading"; fi
                ;;
        esac
        ;;
    launchctl:bootstrap) : > "$dir/loaded" ;;
    launchctl:bootout)
        if [ -f "$dir/delay-unload" ]; then
            : > "$dir/unloading"
        else
            /bin/rm -f "$dir/loaded"
        fi
        ;;
esac
if [ -f "$response.out" ]; then /bin/cat "$response.out"; fi
`

type serviceFixture struct {
	dir, home, config, bin string
}

func fakeService(t *testing.T, platform string) serviceFixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("service manager fakes require Unix shell scripts")
	}
	oldOS, oldExe, oldWSL := goos, serviceExecutable, isWSL
	t.Cleanup(func() {
		goos, serviceExecutable, isWSL = oldOS, oldExe, oldWSL
	})
	goos = platform
	serviceExecutable = func() (string, error) { return fakeExecutable, nil }
	isWSL = func() bool { return false }
	dir := t.TempDir()
	f := serviceFixture{dir: dir, home: filepath.Join(dir, "home dir"), config: filepath.Join(dir, "config"), bin: filepath.Join(dir, "bin")}
	t.Setenv("HOME", f.home)
	t.Setenv("USER", "service-test-user")
	t.Setenv("XDG_CONFIG_HOME", f.config)
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	t.Setenv("CIRCUS_HOME", "")
	t.Setenv("SERVICE_TEST_DIR", dir)
	if err := os.Mkdir(f.bin, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"systemctl", "launchctl", "loginctl"} {
		writeTestFile(t, filepath.Join(f.bin, name), fakeManager, 0o700)
	}
	t.Setenv("PATH", f.bin)
	return f
}

func (f serviceFixture) response(t *testing.T, name, response string) {
	t.Helper()
	writeTestFile(t, filepath.Join(f.dir, name), response, 0o600)
}

func (f serviceFixture) checkCalls(t *testing.T, want ...string) {
	t.Helper()
	path := filepath.Join(f.dir, "calls")
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	var got []string
	if len(data) > 0 {
		got = strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	}
	if !slices.Equal(got, want) {
		t.Errorf("manager calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}

func writeTestFile(t *testing.T, path, data string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), mode); err != nil {
		t.Fatal(err)
	}
}

func saveCredential(t *testing.T, path string, role credfile.Role) {
	t.Helper()
	if path == "" {
		var err error
		path, err = credfile.Path(credfile.RoleDaemon)
		if err != nil {
			t.Fatal(err)
		}
	}
	err := credfile.Save(path, credfile.File{Role: role, PrincipalID: "device", Token: "test-token", ControlPlaneURL: "https://circus.example"})
	if err != nil {
		t.Fatal(err)
	}
}

func readPrivate(t *testing.T, path string) string {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("%s mode = %o, want 600", path, info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func checkText(t *testing.T, data string, fragments ...string) {
	t.Helper()
	for _, fragment := range fragments {
		if !strings.Contains(data, fragment) {
			t.Errorf("missing %q from:\n%s", fragment, data)
		}
	}
}

func TestSystemdLifecycle(t *testing.T) {
	f := fakeService(t, "linux")
	t.Chdir(f.dir)
	t.Setenv("HTTPS_PROXY", `http://proxy/p%ss"\x`)
	t.Setenv("CIRCUS_HOME", "custom-home")
	credentials := `device cred%"$HOME.json`
	saveCredential(t, credentials, credfile.RoleDaemon)
	saveCredential(t, "", credfile.RoleDaemon)
	f.response(t, "systemctl.is-system-running.fail", "degraded\n")
	if err := InstallDaemon(credentials, io.Discard); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.config, "systemd", "user", "circus-daemon.service")
	checkText(t, readPrivate(t, path),
		"ExecStart=@", `$$`, `"daemon" "--service" "--credentials"`,
		"Description=circus device daemon\n",
		"Restart=on-failure\nRestartSec=5\n", "WantedBy=default.target\n",
		`Environment="HTTPS_PROXY=http://proxy/p%%ss\"\\x"`,
		`Environment="PATH=`+f.bin+`"`, `Environment="CIRCUS_HOME=`+filepath.Join(f.dir, "custom-home")+`"`)
	if got := Daemon.Program(); got != fakeExecutable {
		t.Errorf("program = %q, want %q", got, fakeExecutable)
	}
	installCalls := []string{"systemctl --user is-system-running", "systemctl --user daemon-reload", "systemctl --user enable " + path, "systemctl --user restart circus-daemon.service"}
	f.checkCalls(t, installCalls...)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := InstallDaemon("", io.Discard); err != nil {
		t.Fatal(err)
	}
	if data := readPrivate(t, path); strings.Contains(data, "--credentials") {
		t.Errorf("default credentials should be implicit:\n%s", data)
	}
	f.checkCalls(t, slices.Insert(installCalls, 1, showCall)...)
	for _, action := range []func() error{Daemon.Start, Daemon.Stop, Daemon.Restart, Daemon.Uninstall, Daemon.Uninstall} {
		if err := action(); err != nil {
			t.Fatal(err)
		}
	}
	f.checkCalls(t, "systemctl --user start circus-daemon.service", "systemctl --user stop circus-daemon.service", "systemctl --user restart circus-daemon.service",
		"systemctl --user is-system-running", "systemctl --user disable --now circus-daemon.service", "systemctl --user daemon-reload")
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("after uninstall, stat = %v", err)
	}
}

func TestLaunchdLifecycle(t *testing.T) {
	f := fakeService(t, "darwin")
	t.Chdir(f.dir)
	t.Setenv("HTTP_PROXY", "http://a&b<c>@proxy")
	credentials := `device cred%"&.json`
	saveCredential(t, credentials, credfile.RoleDaemon)
	if err := InstallDaemon(credentials, io.Discard); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.home, "Library", "LaunchAgents", "com.github.ericwvi.circus.daemon.plist")
	log := filepath.Join(f.home, "Library", "Logs", "circus", "daemon.log")
	data := readPrivate(t, path)
	checkText(t, data,
		"<key>Label</key>\n  <string>com.github.ericwvi.circus.daemon</string>",
		`<string>/opt/circus tools/circus%&#34;\${USER}</string>`, "<string>daemon</string>",
		"<string>--service</string>", "<string>--credentials</string>", "<string>"+f.dir+"/device cred%&#34;&amp;.json</string>",
		"<key>RunAtLoad</key>\n  <true/>", "<key>SuccessfulExit</key>\n    <false/>",
		"<key>ThrottleInterval</key>\n  <integer>5</integer>", "<key>ProcessType</key>\n  <string>Standard</string>",
		"<key>StandardOutPath</key>\n  <string>"+log+"</string>", "<key>StandardErrorPath</key>\n  <string>"+log+"</string>",
		"<key>HTTP_PROXY</key>\n    <string>http://a&amp;b&lt;c&gt;@proxy</string>", "<key>PATH</key>\n    <string>"+f.bin+"</string>")
	decoder := xml.NewDecoder(strings.NewReader(data))
	for {
		if _, err := decoder.Token(); err != nil {
			if !errors.Is(err, io.EOF) {
				t.Fatalf("invalid plist XML: %v", err)
			}
			break
		}
	}
	if got := Daemon.Program(); got != fakeExecutable {
		t.Errorf("program = %q, want %q", got, fakeExecutable)
	}
	if info, err := os.Stat(filepath.Dir(log)); err != nil || !info.IsDir() {
		t.Errorf("log directory was not created: %v", err)
	}
	domain := "gui/" + strconv.Itoa(os.Getuid())
	target := domain + "/com.github.ericwvi.circus.daemon"
	print, bootout, bootstrap := "launchctl print "+target, "launchctl bootout "+target, "launchctl bootstrap "+domain+" "+path
	f.checkCalls(t, print, bootstrap)
	if err := InstallDaemon(credentials, io.Discard); err != nil {
		t.Fatal(err)
	}
	f.checkCalls(t, print, bootout, print, bootstrap)
	for _, action := range []func() error{Daemon.Start, Daemon.Stop, Daemon.Stop, Daemon.Start, Daemon.Restart, Daemon.Uninstall, Daemon.Uninstall} {
		if err := action(); err != nil {
			t.Fatal(err)
		}
	}
	f.checkCalls(t, print, "launchctl kickstart "+target, print, bootout, print, print, bootstrap, print, "launchctl kickstart -k "+target, print, bootout)
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("after uninstall, stat = %v", err)
	}
}

func TestNotInstalled(t *testing.T) {
	for _, platform := range []string{"linux", "darwin"} {
		t.Run(platform, func(t *testing.T) {
			f := fakeService(t, platform)
			for _, action := range []func() error{Daemon.Start, Daemon.Stop, Daemon.Restart} {
				if err := action(); !errors.Is(err, ErrNotInstalled) {
					t.Errorf("error = %v, want ErrNotInstalled", err)
				}
			}
			st, err := Daemon.Get()
			if err != nil || st.Installed || st.File == "" || st.Logs == "" || Daemon.Program() != "" {
				t.Errorf("status = %+v, %v; program = %q", st, err, Daemon.Program())
			}
			f.checkCalls(t)
		})
	}
}

func TestInstallRequiresDaemonCredentials(t *testing.T) {
	for _, platform := range []string{"linux", "darwin"} {
		t.Run(platform, func(t *testing.T) {
			f := fakeService(t, platform)
			if err := InstallDaemon("", io.Discard); !errors.Is(err, credfile.ErrNotFound) || !strings.Contains(err.Error(), "circus join") {
				t.Errorf("missing credentials: %v", err)
			}
			path := filepath.Join(f.dir, "worker.json")
			saveCredential(t, path, credfile.RoleWorker)
			if err := InstallDaemon(path, io.Discard); err == nil || !strings.Contains(err.Error(), "circus join") {
				t.Errorf("worker credentials: %v", err)
			}
			st, err := Daemon.Get()
			if err != nil || st.Installed {
				t.Errorf("status = %+v, %v", st, err)
			}
			f.checkCalls(t)
		})
	}
}

func TestUnsupported(t *testing.T) {
	f := fakeService(t, "freebsd")
	for _, action := range []func() error{func() error { return InstallDaemon("", io.Discard) }, Daemon.Start, Daemon.Stop, Daemon.Restart, Daemon.Uninstall} {
		if err := action(); !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "run `circus daemon`") {
			t.Errorf("unsupported platform: %v", err)
		}
	}
	if _, err := Daemon.Get(); !errors.Is(err, ErrUnsupported) || Daemon.Program() != "" || Daemon.Linger() != "" {
		t.Errorf("unexpected service on unsupported platform: %v", err)
	}
	f.checkCalls(t)
}

func TestInstallChecksDaemonLock(t *testing.T) {
	f := fakeService(t, "linux")
	saveCredential(t, "", credfile.RoleDaemon)
	lock, err := daemonlock.Acquire(daemonlock.Foreground)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("stop the foreground `circus daemon` (pid %d) first", os.Getpid())
	err = InstallDaemon("", io.Discard)
	if closeErr := lock.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if err == nil || err.Error() != want {
		t.Fatalf("install = %v, want %q", err, want)
	}
	f.checkCalls(t)
	lock, err = daemonlock.Acquire(daemonlock.Service)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := InstallDaemon("", io.Discard); err != nil {
		t.Fatalf("install with a service lock = %v", err)
	}
}

func TestReplacementAndEnvironmentNotices(t *testing.T) {
	for _, platform := range []string{"linux", "darwin"} {
		t.Run(platform, func(t *testing.T) {
			f := fakeService(t, platform)
			t.Setenv("CIRCUS_HOME", f.dir+"/old")
			t.Setenv("HTTP_PROXY", "bad\x1bsecret")
			t.Setenv("SSL_CERT_DIR", "/cert/dir")
			t.Setenv("SSL_CERT_FILE", "/cert/file")
			saveCredential(t, "", credfile.RoleDaemon)
			var out bytes.Buffer
			if err := InstallDaemon("", &out); err != nil {
				t.Fatal(err)
			}
			checkText(t, out.String(), `skipping environment variable "HTTP_PROXY"`)
			if strings.Contains(out.String(), "secret") || strings.Contains(out.String(), "Replacing") {
				t.Fatalf("install output = %q", out.String())
			}
			t.Setenv("CIRCUS_HOME", f.dir+"/new")
			credentials := filepath.Join(f.dir, `creds "%" $new.json`)
			saveCredential(t, credentials, credfile.RoleDaemon)
			newProgram := "/opt/new circus"
			serviceExecutable = func() (string, error) { return newProgram, nil }
			out.Reset()
			if err := InstallDaemon(credentials, &out); err != nil {
				t.Fatal(err)
			}
			if strings.Count(out.String(), "Replacing circus daemon service:") != 1 {
				t.Fatalf("reinstall output = %q", out.String())
			}
			checkText(t, out.String(),
				fmt.Sprintf("program %q → %q", fakeExecutable, newProgram),
				fmt.Sprintf("credentials %q → %q", f.dir+"/old/config/daemon.json", credentials),
				fmt.Sprintf("CIRCUS_HOME %q → %q", f.dir+"/old", f.dir+"/new"))
			m, err := Daemon.current()
			if err != nil {
				t.Fatal(err)
			}
			def := m.definition([]byte(readPrivate(t, m.file())))
			if def.Credentials(credfile.RoleDaemon) != credentials || def.Env["HTTP_PROXY"] != "" || def.Env["SSL_CERT_DIR"] != "/cert/dir" || def.Env["SSL_CERT_FILE"] != "/cert/file" {
				t.Errorf("recorded credentials or environment did not round-trip: %#v", def)
			}
		})
	}
}

func TestLaunchdInstallWaitsForUnloadAndRetriesBootstrap(t *testing.T) {
	f := fakeService(t, "darwin")
	saveCredential(t, "", credfile.RoleDaemon)
	if err := InstallDaemon("", io.Discard); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.home, "Library", "LaunchAgents", Daemon.label()+".plist")
	printAgent := "launchctl print " + launchd{svc: Daemon}.target()
	bootstrap := "launchctl bootstrap " + launchDomain() + " " + path
	f.checkCalls(t, printAgent, bootstrap)
	f.response(t, "delay-unload", "")
	f.response(t, "launchctl.bootstrap.fail-once", "Bootstrap failed: 5: Input/output error")
	if err := InstallDaemon("", io.Discard); err != nil {
		t.Fatal(err)
	}
	f.checkCalls(t, printAgent, "launchctl bootout "+launchd{svc: Daemon}.target(), printAgent, printAgent, bootstrap, "launchctl print "+launchDomain(), bootstrap)
}

func TestLaunchdFirstInstallWithoutGUIDomain(t *testing.T) {
	f := fakeService(t, "darwin")
	saveCredential(t, "", credfile.RoleDaemon)
	f.response(t, "launchctl.print.fail", "Bad request.\nCould not find domain for")
	if err := InstallDaemon("", io.Discard); !errors.Is(err, errNoGUIDomain) || !strings.Contains(err.Error(), "rerun `circus daemon install` on its own") {
		t.Fatalf("install with no GUI login = %v", err)
	}
	m, _ := Daemon.current()
	if _, err := os.Stat(m.file()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("service file stat = %v", err)
	}
}

func TestFirstInstallRollsBackFile(t *testing.T) {
	for _, tc := range []struct{ os, command string }{
		{"linux", "systemctl.daemon-reload"},
		{"linux", "systemctl.restart"},
		{"darwin", "launchctl.bootstrap"},
	} {
		t.Run(tc.command, func(t *testing.T) {
			f := fakeService(t, tc.os)
			saveCredential(t, "", credfile.RoleDaemon)
			f.response(t, tc.command+".fail", "Test failure")
			if err := InstallDaemon("", io.Discard); err == nil || !strings.Contains(err.Error(), "Test failure") {
				t.Fatalf("install = %v", err)
			}
			m, _ := Daemon.current()
			for _, path := range []string{m.file(), filepath.Join(f.dir, "enabled"), filepath.Join(f.dir, "active"), filepath.Join(f.dir, "loaded")} {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("failed install left %s: %v", path, err)
				}
			}
		})
	}
}

func TestReinstallRollsBackFileAndServiceState(t *testing.T) {
	for _, platform := range []string{"linux", "darwin"} {
		t.Run(platform, func(t *testing.T) {
			f := fakeService(t, platform)
			saveCredential(t, "", credfile.RoleDaemon)
			if err := InstallDaemon("", io.Discard); err != nil {
				t.Fatal(err)
			}
			m, _ := Daemon.current()
			old := readPrivate(t, m.file())
			serviceExecutable = func() (string, error) { return "/opt/new", nil }
			if platform == "linux" {
				f.response(t, "systemctl.restart.fail-once", "Test failure")
			} else {
				// Fail both attempts for the new file, then accept the restored one.
				script := strings.Replace(fakeManager, `launchctl:bootstrap) : > "$dir/loaded" ;;`,
					`launchctl:bootstrap)
                       if /bin/cat "$3" | /usr/bin/grep -q /opt/new; then
                           printf 'Test failure\n' >&2
                           exit 1
                       fi
                       : > "$dir/loaded"
                       ;;`, 1)
				writeTestFile(t, filepath.Join(f.bin, "launchctl"), script, 0o700)
			}
			if err := InstallDaemon("", io.Discard); err == nil || !strings.Contains(err.Error(), "Test failure") {
				t.Fatalf("reinstall = %v", err)
			}
			if data := readPrivate(t, m.file()); data != old {
				t.Error("reinstall did not restore the original service file")
			}
			state := "loaded"
			if platform == "linux" {
				state = "active"
			}
			if _, err := os.Stat(filepath.Join(f.dir, state)); err != nil {
				t.Errorf("reinstall did not restore service state: %v", err)
			}
		})
	}
}

func TestSystemdUninstallToleratesUnknownUnit(t *testing.T) {
	f := fakeService(t, "linux")
	saveCredential(t, "", credfile.RoleDaemon)
	if err := InstallDaemon("", io.Discard); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.config, "systemd", "user", Daemon.unitName())
	f.checkCalls(t, "systemctl --user is-system-running", "systemctl --user daemon-reload", "systemctl --user enable "+path, "systemctl --user restart "+Daemon.unitName())
	f.response(t, "systemctl.disable.fail", "Failed to disable unit: Unit circus-daemon.service does not exist")
	if err := Daemon.Uninstall(); err != nil {
		t.Fatal(err)
	}
	f.checkCalls(t, "systemctl --user is-system-running", "systemctl --user disable --now "+Daemon.unitName(), "systemctl --user daemon-reload")
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("uninstall left the file: %v", err)
	}
}
