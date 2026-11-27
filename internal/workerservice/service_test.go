package workerservice

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/EricWvi/circus/internal/credfile"
	"github.com/EricWvi/circus/internal/testutil/fakerelease"
	"github.com/EricWvi/circus/internal/userservice"
)

// fakeSystemctl records its calls and keeps the unit's state in files:
// active, enabled; <command>.fail makes a command fail.
const fakeSystemctl = `#!/bin/sh
dir=$SERVICE_TEST_DIR
shift
printf '%s\n' "$*" >> "$dir/calls"
if [ -f "$dir/$1.fail" ]; then
    /bin/cat "$dir/$1.fail" >&2
    exit 1
fi
case "$1" in
    is-system-running) echo running ;;
    enable) : > "$dir/enabled" ;;
    disable) /bin/rm -f "$dir/enabled" "$dir/active" ;;
    start|restart) : > "$dir/active"; /bin/cp "$dir/config/systemd/user/circus-worker.service" "$dir/running-unit" ;;
    try-restart)
        if [ -f "$dir/active" ]; then /bin/cp "$dir/config/systemd/user/circus-worker.service" "$dir/running-unit"; fi ;;
    stop) /bin/rm -f "$dir/active" ;;
    show)
        if [ -f "$dir/active" ]; then echo ActiveState=active; else echo ActiveState=inactive; fi
        if [ -f "$dir/enabled" ]; then echo UnitFileState=enabled; else echo UnitFileState=disabled; fi ;;
esac
`

type fixture struct {
	dir, bin, data, unit, circus string
	srv                          *fakerelease.Server
	node                         string
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("uses a systemctl fake")
	}
	dir := t.TempDir()
	f := fixture{dir: dir, bin: filepath.Join(dir, "bin"), data: filepath.Join(dir, "data"), srv: fakerelease.New(t)}
	f.unit = filepath.Join(dir, "config", "systemd", "user", "circus-worker.service")
	f.circus = filepath.Join(dir, "opt", "circus")
	if err := os.MkdirAll(f.bin, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, script := range map[string]string{"systemctl": fakeSystemctl, "loginctl": "#!/bin/sh\necho yes\n"} {
		if err := os.WriteFile(filepath.Join(f.bin, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f.node = fakeNode(t, f.bin, "node", "24.4.1")
	t.Setenv("PATH", f.bin)
	t.Setenv("HOME", filepath.Join(dir, "home"))
	t.Setenv("USER", "worker-test")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("XDG_DATA_HOME", f.data)
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("CIRCUS_HOME", "")
	t.Setenv("CIRCUS_NODE", "")
	t.Setenv("SERVICE_TEST_DIR", dir)
	for _, name := range append(slices.Clone(ProxyVariables), Settings...) {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	t.Setenv("LANG", "C.UTF-8")
	oldTarget := target
	target = testTarget
	t.Cleanup(func() { target = oldTarget })
	f.srv.Publish(v1, []string{testTarget}, packageFiles(v1))
	f.srv.Publish(v2, []string{testTarget}, packageFiles(v2))
	return f
}

func (f fixture) joinWorker(t *testing.T) {
	t.Helper()
	path, err := credfile.Path(credfile.RoleWorker)
	if err != nil {
		t.Fatal(err)
	}
	if err := credfile.Save(path, credfile.File{Role: credfile.RoleWorker, PrincipalID: "worker", Token: "token", ControlPlaneURL: "https://circus.example"}); err != nil {
		t.Fatal(err)
	}
}

func (f fixture) options(out io.Writer) Options {
	return Options{Release: f.srv.Release(), Circus: f.circus, Out: out}
}

func (f fixture) calls(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.dir, "calls"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(f.dir, "calls")); err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

func (f fixture) store() store { return store{root: filepath.Join(f.data, "circus", "worker")} }

func (f fixture) read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func (f fixture) exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestInstallWritesTheServiceAndRemovesOtherVersions(t *testing.T) {
	f := newFixture(t)
	f.joinWorker(t)
	t.Setenv("HTTPS_PROXY", "http://user:secret@proxy:3128")
	t.Setenv("no_proxy", "localhost,.internal")
	t.Setenv("CIRCUS_WORKER_MAX_SESSIONS", "8")
	t.Setenv("CLAUDE_CONFIG_DIR", "/home/me/.claude-work")
	t.Setenv("UNRELATED_SECRET", "nope")
	var out bytes.Buffer
	node, err := Install(t.Context(), v1, f.options(&out))
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out.String())
	}
	if node.Path != f.node || node.Version != "24.4.1" {
		t.Errorf("node = %+v", node)
	}
	s := f.store()
	unit := f.read(t, f.unit)
	for _, want := range []string{
		"Description=circus worker\n",
		"Restart=on-failure\nRestartSec=5\nKillMode=mixed\n",
		`ExecStart="` + s.launcher(v1) + `" "--service"` + "\n",
		`Environment="CIRCUS_NODE=` + f.node + `"`,
		`Environment="CIRCUS_BIN=` + f.circus + `"`,
		`Environment="HTTPS_PROXY=http://user:secret@proxy:3128"`,
		`Environment="no_proxy=localhost,.internal"`,
		`Environment="CIRCUS_WORKER_MAX_SESSIONS=8"`,
		`Environment="CLAUDE_CONFIG_DIR=/home/me/.claude-work"`,
		`Environment="HOME=` + filepath.Join(f.dir, "home") + `"`,
		`Environment="USER=worker-test"`,
		`Environment="LANG=C.UTF-8"`,
		`Environment="PATH=` + f.bin + `"`,
		`Environment="XDG_DATA_HOME=` + f.data + `"`,
		"WantedBy=default.target\n",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit lacks %q:\n%s", want, unit)
		}
	}
	if strings.Contains(unit, "UNRELATED_SECRET") {
		t.Errorf("unit records an unrelated variable:\n%s", unit)
	}
	if info, err := os.Stat(f.unit); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("unit file mode: %v, %v", info, err)
	}
	if got, want := f.calls(t), []string{"is-system-running", "daemon-reload", "enable " + f.unit, "restart circus-worker.service"}; !slices.Equal(got, want) {
		t.Errorf("calls = %v, want %v", got, want)
	}
	i, ok, err := InstalledService()
	if err != nil || !ok || i.Version != v1 || i.Root != s.root || i.Node != f.node || i.Circus != f.circus {
		t.Errorf("installed = %+v, %t, %v", i, ok, err)
	}
	if got := RecordedProxies(i.Definition); !slices.Equal(got, []string{"HTTPS_PROXY", "no_proxy"}) {
		t.Errorf("recorded proxies = %v", got)
	}

	// Installing this circus's version again downloads nothing.
	f.srv.Requests()
	if _, err := Install(t.Context(), v1, f.options(io.Discard)); err != nil {
		t.Fatal(err)
	}
	if got := f.srv.Requests(); len(got) != 0 {
		t.Errorf("reinstall downloaded %v", got)
	}

	// Another version: the service switches, then the old version goes.
	out.Reset()
	if _, err := Install(t.Context(), v2, f.options(&out)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `Replacing circus worker service: program "`+s.launcher(v1)+`" → "`+s.launcher(v2)+`"`) {
		t.Errorf("install output:\n%s", out.String())
	}
	if got := entries(t, s.root); !slices.Equal(got, []string{lockName, v2}) {
		t.Errorf("store holds %v", got)
	}
	if !strings.Contains(f.read(t, filepath.Join(f.dir, "running-unit")), s.launcher(v2)) {
		t.Error("the service was not restarted on the new version")
	}
}

func TestInstallFailureKeepsThePreviousVersion(t *testing.T) {
	f := newFixture(t)
	f.joinWorker(t)
	if _, err := Install(t.Context(), v1, f.options(io.Discard)); err != nil {
		t.Fatal(err)
	}
	old := f.read(t, f.unit)
	if err := os.WriteFile(filepath.Join(f.dir, "restart.fail"), []byte("Test failure"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(t.Context(), v2, f.options(io.Discard)); err == nil || !strings.Contains(err.Error(), "Test failure") {
		t.Fatalf("install = %v", err)
	}
	if f.read(t, f.unit) != old {
		t.Error("a failed install did not restore the service file")
	}
	if !f.exists(f.store().launcher(v1)) {
		t.Error("a failed install removed the version the service runs")
	}
}

func TestInstallRefusals(t *testing.T) {
	f := newFixture(t)
	if _, err := Install(t.Context(), "dev", f.options(io.Discard)); !errors.Is(err, ErrDevelopmentBuild) || !strings.Contains(err.Error(), "worker/README.md") {
		t.Errorf("dev build: %v", err)
	}
	if _, err := Install(t.Context(), v1, f.options(io.Discard)); err == nil || !strings.Contains(err.Error(), "circus join --role worker") {
		t.Errorf("no credential: %v", err)
	}
	path, _ := credfile.Path(credfile.RoleWorker)
	if err := credfile.Save(path, credfile.File{Role: credfile.RoleDaemon, PrincipalID: "d", Token: "t", ControlPlaneURL: "https://circus.example"}); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(t.Context(), v1, f.options(io.Discard)); err == nil || !strings.Contains(err.Error(), "holds daemon credentials") {
		t.Errorf("daemon credential: %v", err)
	}
	f.joinWorker(t)
	if err := os.Remove(f.node); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(t.Context(), v1, f.options(io.Discard)); err == nil || !strings.Contains(err.Error(), "needs Node.js 24 or later") {
		t.Errorf("no Node.js: %v", err)
	}
	if f.exists(f.unit) || f.exists(f.store().root) || len(f.calls(t)) != 0 {
		t.Error("a refused install changed something")
	}
}

func TestUpdate(t *testing.T) {
	for _, tc := range []struct {
		name          string
		running       bool
		fail          string
		wantRestarted bool
		wantErr       string
	}{
		{name: "running", running: true, wantRestarted: true},
		{name: "stopped", running: false},
		{name: "restart fails", running: true, fail: "try-restart", wantErr: "Test failure"},
		{name: "reload fails", running: true, fail: "daemon-reload", wantErr: "Test failure"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.joinWorker(t)
			t.Setenv("HTTPS_PROXY", "http://install-time-proxy")
			if _, err := Install(t.Context(), v1, f.options(io.Discard)); err != nil {
				t.Fatal(err)
			}
			old := f.read(t, f.unit)
			if !tc.running {
				if err := userservice.Worker.Stop(); err != nil {
					t.Fatal(err)
				}
			}
			f.calls(t)
			// circus update may run in another environment; the service
			// keeps what install recorded.
			t.Setenv("HTTPS_PROXY", "http://update-time-proxy")
			if tc.fail != "" {
				if err := os.WriteFile(filepath.Join(f.dir, tc.fail+".fail"), []byte("Test failure"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			os.Remove(filepath.Join(f.dir, "running-unit"))
			restarted, err := Update(t.Context(), v2, f.options(io.Discard))
			s := f.store()
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("update = %v", err)
				}
				if !f.exists(s.launcher(v1)) {
					t.Error("a failed update removed the version the worker runs")
				}
				switch tc.fail {
				case "daemon-reload":
					// The service file is restored.
					if f.read(t, f.unit) != old {
						t.Error("a failed reload did not restore the service file")
					}
				case "try-restart":
					// The file points at the new package, which is in place;
					// the worker still runs the old one, which stays too.
					if !strings.Contains(f.read(t, f.unit), s.launcher(v2)) || !f.exists(s.launcher(v2)) ||
						f.exists(filepath.Join(f.dir, "running-unit")) {
						t.Error("a failed restart request left the service inconsistent")
					}
				}
				return
			}
			if err != nil || restarted != tc.wantRestarted {
				t.Fatalf("update = %t, %v", restarted, err)
			}
			unit := f.read(t, f.unit)
			if want := strings.ReplaceAll(old, s.launcher(v1), s.launcher(v2)); unit != want {
				t.Errorf("updated unit:\n%s\nwant:\n%s", unit, want)
			}
			// The previous version stays until the next switch or install:
			// the old worker may use it until its restart is done.
			if got := entries(t, s.root); !slices.Equal(got, []string{lockName, v1, v2}) {
				t.Errorf("store holds %v", got)
			}
			if f.exists(filepath.Join(f.dir, "active")) != tc.running {
				t.Error("update changed whether the worker runs")
			}
			calls := f.calls(t)
			if slices.Contains(calls, "try-restart --no-block circus-worker.service") != tc.running || slices.Contains(calls, "restart circus-worker.service") {
				t.Errorf("calls = %v", calls)
			}
			if tc.running && !strings.Contains(f.read(t, filepath.Join(f.dir, "running-unit")), s.launcher(v2)) {
				t.Error("the worker did not restart on the new version")
			}
			// The same version again is a no-op.
			if restarted, err := Update(t.Context(), v2, f.options(io.Discard)); err != nil || restarted || len(f.calls(t)) != 0 {
				t.Errorf("second update = %t, %v", restarted, err)
			}
			// The next switch removes the version before the previous one.
			f.srv.Publish(v3, []string{testTarget}, packageFiles(v3))
			if _, err := Update(t.Context(), v3, f.options(io.Discard)); err != nil {
				t.Fatal(err)
			}
			if got := entries(t, s.root); !slices.Equal(got, []string{lockName, v2, v3}) {
				t.Errorf("after the next switch the store holds %v", got)
			}
		})
	}
}

func TestUpdateDownloadFailureKeepsTheService(t *testing.T) {
	f := newFixture(t)
	f.joinWorker(t)
	if _, err := Install(t.Context(), v1, f.options(io.Discard)); err != nil {
		t.Fatal(err)
	}
	old := f.read(t, f.unit)
	f.calls(t)
	f.srv.Set(fakerelease.Path(v2, fakerelease.PackageName(v2, testTarget)), []byte("corrupt"))
	if _, err := Update(t.Context(), v2, f.options(io.Discard)); err == nil || !strings.Contains(err.Error(), "SHA-256 mismatch") {
		t.Fatalf("update = %v", err)
	}
	if f.read(t, f.unit) != old || len(f.calls(t)) != 0 || !f.exists(f.store().launcher(v1)) {
		t.Error("a failed download touched the service")
	}
	if got := entries(t, f.store().root); !slices.Equal(got, []string{lockName, v1}) {
		t.Errorf("store holds %v", got)
	}
}

func TestUninstallRemovesThePackages(t *testing.T) {
	f := newFixture(t)
	f.joinWorker(t)
	if _, err := Install(t.Context(), v1, f.options(io.Discard)); err != nil {
		t.Fatal(err)
	}
	agents := filepath.Join(f.data, "circus", "agents", "claude-code")
	if err := os.MkdirAll(agents, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := Uninstall(t.Context(), io.Discard); err != nil {
		t.Fatal(err)
	}
	if f.exists(f.unit) {
		t.Error("uninstall left the service file")
	}
	// Only the lock file stays: a waiting command may hold it open.
	if got := entries(t, f.store().root); !slices.Equal(got, []string{lockName}) {
		t.Errorf("uninstall left %v", got)
	}
	if !f.exists(agents) {
		t.Error("uninstall removed the worker's agents")
	}
	if err := Uninstall(t.Context(), io.Discard); err != nil {
		t.Errorf("second uninstall: %v", err)
	}
}

func TestRootFollowsTheWorkersDataRules(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("CIRCUS_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	for _, tc := range []struct{ circusHome, xdg, want string }{
		{"", "", filepath.Join(dir, ".local", "share", "circus", "worker")},
		{"", "relative", filepath.Join(dir, ".local", "share", "circus", "worker")},
		{"", "/xdg/data", "/xdg/data/circus/worker"},
		{"/srv/circus", "/xdg/data", "/srv/circus/worker"},
	} {
		t.Setenv("CIRCUS_HOME", tc.circusHome)
		t.Setenv("XDG_DATA_HOME", tc.xdg)
		if got, err := Root(); err != nil || got != tc.want {
			t.Errorf("Root(CIRCUS_HOME=%q, XDG_DATA_HOME=%q) = %q, %v; want %q", tc.circusHome, tc.xdg, got, err, tc.want)
		}
	}
}

func TestStopReason(t *testing.T) {
	f := newFixture(t)
	f.joinWorker(t)
	if _, err := Install(t.Context(), v1, f.options(io.Discard)); err != nil {
		t.Fatal(err)
	}
	i, _, err := InstalledService()
	if err != nil {
		t.Fatal(err)
	}
	if reason, ok := StopReason(i); ok {
		t.Errorf("without a stop file: %s", reason)
	}
	// The service records the stop file, under the installing shell's data
	// directory, with its own directory made private.
	file := filepath.Join(f.data, "circus", "worker-state", "stop.json")
	if StopFile(i) != file || !strings.Contains(f.read(t, f.unit), `Environment="CIRCUS_WORKER_STOP_FILE=`+file+`"`) {
		t.Fatalf("recorded stop file %q", StopFile(i))
	}
	if info, err := os.Stat(filepath.Dir(file)); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("stop file directory: %v, %v", info, err)
	}
	// A shell with another data directory reads the recorded file.
	t.Setenv("XDG_DATA_HOME", filepath.Join(f.dir, "elsewhere"))
	t.Setenv("CIRCUS_HOME", filepath.Join(f.dir, "another-home"))
	for _, tc := range []struct{ reason, want string }{
		{"credential-refused", "The control plane refused its credential (HTTP 401): the worker was revoked. Enroll again with `circus join --role worker --force`"},
		{"incompatible", "The control plane does not accept this worker (HTTP 401). Update circus with `circus update`"},
		{"credential", "Its credential is missing or unusable (HTTP 401). Enroll this machine as a worker with `circus join --role worker`"},
		{"config", "A setting is invalid (HTTP 401). Fix it in your shell, then run `circus worker install`."},
		{"node", "Its Node.js is missing or too old (HTTP 401). Install Node.js 24 or later, then run `circus worker install`."},
		{"other", `It recorded "other": HTTP 401. See the logs.`},
	} {
		if err := os.WriteFile(file, []byte(`{"reason":"`+tc.reason+`","message":"HTTP 401"}`+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, ok := StopReason(i); !ok || got != tc.want && !strings.HasPrefix(got, tc.want) {
			t.Errorf("%s: %q, %t", tc.reason, got, ok)
		}
	}
	if err := os.WriteFile(file, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := StopReason(i); ok {
		t.Error("a damaged stop file gave a reason")
	}
	if NodeGone(i) {
		t.Error("the recorded Node.js is there")
	}
	if err := os.Remove(f.node); err != nil {
		t.Fatal(err)
	}
	if !NodeGone(i) {
		t.Error("the recorded Node.js is gone")
	}
}

// A worker service whose credential the control plane refused for good is
// removed with its credential, under the store's lock; its packages and
// agents stay. A service installed since for another credential stays.
func TestRemoveRejectedKeepsThePackages(t *testing.T) {
	f := newFixture(t)
	f.joinWorker(t)
	if _, err := Install(t.Context(), v1, f.options(io.Discard)); err != nil {
		t.Fatal(err)
	}
	agents := filepath.Join(f.data, "circus", "agents", "claude-code")
	if err := os.MkdirAll(agents, 0o700); err != nil {
		t.Fatal(err)
	}
	path, err := credfile.Path(credfile.RoleWorker)
	if err != nil {
		t.Fatal(err)
	}
	f.calls(t)
	file, err := credfile.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	running := userservice.RejectedCredential(file)
	if removed, err := RemoveRejected(t.Context(), filepath.Join(f.dir, "other.json"), running, 1, io.Discard); removed || err != nil {
		t.Fatalf("another credential: %v, %v", removed, err)
	}
	if !f.exists(f.unit) || !f.exists(path) {
		t.Fatal("removed another credential's service")
	}
	// A token other than the file's: the file was replaced since.
	if removed, err := RemoveRejected(t.Context(), path, userservice.Rejected{PrincipalID: file.PrincipalID}, 1, io.Discard); removed || err != nil {
		t.Fatalf("another token: %v, %v", removed, err)
	}
	removed, err := RemoveRejected(t.Context(), path, running, 1, io.Discard)
	if err != nil || !removed {
		t.Fatalf("RemoveRejected = %v, %v", removed, err)
	}
	if f.exists(f.unit) || f.exists(path) {
		t.Fatal("service file or credential kept")
	}
	if !f.exists(f.store().launcher(v1)) || !f.exists(agents) {
		t.Fatal("the worker package or the agents were removed")
	}
	for _, call := range f.calls(t) {
		if strings.Contains(call, "--now") || strings.Contains(call, " stop ") {
			t.Fatalf("stopped the unit from inside: %q", call)
		}
	}
	if removed, err := RemoveRejected(t.Context(), path, running, 1, io.Discard); removed || err != nil {
		t.Fatalf("without a service: %v, %v", removed, err)
	}
}
