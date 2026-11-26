package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/EricWvi/circus/internal/credfile"
	"github.com/EricWvi/circus/internal/selfupdate"
	"github.com/EricWvi/circus/internal/testutil/fakerelease"
	"github.com/EricWvi/circus/internal/userservice"
	"github.com/EricWvi/circus/internal/workerservice"
)

// workerSystemctl fakes systemd for one user's circus units. It records
// its calls, keeps one state for every unit, and:
//   - records in blocked-<unit> a restart that waits for its job (from
//     inside that service, the caller would be killed with it);
//   - sends SIGTERM to its caller when kill-on-<verb>-<unit> exists.
const workerSystemctl = `#!/bin/sh
dir=$SERVICE_TEST_DIR
shift
printf '%s\n' "$*" >> "$dir/calls"
if [ -f "$dir/$1.fail" ]; then /bin/cat "$dir/$1.fail" >&2; exit 1; fi
verb=$1
noblock=no
for arg in "$@"; do
    if [ "$arg" = --no-block ]; then noblock=yes; fi
done
for unit in "$@"; do
    case $unit in *.service) ;; *) continue ;; esac
    case $verb in
        restart|try-restart|start) if [ $noblock = no ]; then : > "$dir/blocked-$unit"; fi ;;
    esac
    if [ -f "$dir/kill-on-$verb-$unit" ]; then kill -TERM $PPID; fi
done
case "$verb" in
    is-system-running) echo running ;;
    enable) : > "$dir/enabled" ;;
    disable) /bin/rm -f "$dir/enabled" "$dir/active" ;;
    start|restart) : > "$dir/active" ;;
    try-restart) if [ -f "$dir/active" ]; then : > "$dir/restarted"; fi ;;
    stop) /bin/rm -f "$dir/active" ;;
    show)
        if [ -f "$dir/terminal" ]; then printf 'ActiveState=inactive\nSubState=dead\nExecMainCode=exited\nExecMainStatus=78\n'
        elif [ -f "$dir/active" ]; then printf 'ActiveState=active\nSubState=running\nMainPID=4242\n'; else echo ActiveState=inactive; fi
        if [ -f "$dir/enabled" ]; then echo UnitFileState=enabled; else echo UnitFileState=disabled; fi ;;
esac
`

// The worker package's real launcher, with a stand-in for the built worker
// that records how it was started.
var launcherScript = func() string {
	data, err := os.ReadFile(filepath.Join("..", "..", "worker", "scripts", "circus-worker.sh"))
	if err != nil {
		panic(err)
	}
	return string(data)
}()

const recordingWorker = `import { writeFileSync } from "node:fs";
writeFileSync(process.env.RECORD, JSON.stringify({
  argv: process.argv.slice(2), node: process.execPath, nodeVersion: process.versions.node,
  circusBin: process.env.CIRCUS_BIN, circusNode: process.env.CIRCUS_NODE,
  proxy: process.env.HTTPS_PROXY ?? null, sessions: process.env.CIRCUS_WORKER_MAX_SESSIONS ?? null,
  path: process.env.PATH, home: process.env.HOME,
}));
`

func workerPackage(version string) []fakerelease.File {
	return []fakerelease.File{
		{Name: "circus-worker", Body: launcherScript, Mode: 0o755},
		{Name: "package.json", Body: `{"type":"module","version":"` + version + `"}`},
		{Name: "dist/main.js", Body: recordingWorker},
	}
}

type workerFixture struct {
	dir, bin, circus, unit, root string
	srv                          *fakerelease.Server
}

func newWorkerFixture(t *testing.T, cliVersion string) workerFixture {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("uses a systemctl fake")
	}
	dir := t.TempDir()
	f := workerFixture{dir: dir, bin: filepath.Join(dir, "bin"), srv: fakerelease.New(t)}
	f.circus = filepath.Join(dir, "local", "bin", "circus")
	f.unit = filepath.Join(dir, "config", "systemd", "user", "circus-worker.service")
	f.root = filepath.Join(dir, "data", "circus", "worker")
	for _, d := range []string{f.bin, filepath.Dir(f.circus)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path, text string) {
		if err := os.WriteFile(path, []byte(text), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(f.bin, "systemctl"), workerSystemctl)
	write(filepath.Join(f.bin, "loginctl"), "#!/bin/sh\necho yes\n")
	write(f.circus, "circus binary")
	t.Setenv("PATH", f.bin)
	t.Setenv("HOME", filepath.Join(dir, "home"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(dir, "data"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	t.Setenv("CIRCUS_HOME", "")
	t.Setenv("SERVICE_TEST_DIR", dir)
	for _, name := range append(slices.Clone(workerservice.ProxyVariables), workerservice.Settings...) {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	oldVersion, oldRelease, oldExe := version, workerRelease, workerExecutable
	t.Cleanup(func() { version, workerRelease, workerExecutable = oldVersion, oldRelease, oldExe })
	version = cliVersion
	workerRelease = func(string) selfupdate.Release { return f.srv.Release() }
	workerExecutable = func() (string, error) { return f.circus, nil }
	target := runtime.GOOS + "-" + runtime.GOARCH
	for _, v := range []string{"v1.2.3", "v1.3.0"} {
		f.srv.Publish(v, []string{target}, workerPackage(v))
	}
	return f
}

// hostNode is the Node.js on the PATH the tests started with.
var hostNode, hostNodeErr = func() (string, error) {
	if node := os.Getenv("CIRCUS_NODE"); node != "" {
		return exec.LookPath(node)
	}
	return exec.LookPath("node")
}()

// useNode puts a Node.js 24 or later on PATH as node, or skips the test.
func (f workerFixture) useNode(t *testing.T) string {
	t.Helper()
	node, err := hostNode, hostNodeErr
	if err != nil {
		t.Skipf("Node.js is not installed: %v", err)
	}
	out, err := exec.Command(node, "-p", "process.versions.node").Output()
	major, _, _ := strings.Cut(strings.TrimSpace(string(out)), ".")
	if n, convErr := strconv.Atoi(major); err != nil || convErr != nil || n < workerservice.MinNodeMajor {
		t.Skipf("Node.js %q is older than %d: %v", out, workerservice.MinNodeMajor, err)
	}
	link := filepath.Join(f.bin, "node")
	if err := os.Symlink(node, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CIRCUS_NODE", "")
	return link
}

func joinAsWorker(t *testing.T) {
	t.Helper()
	path, err := credfile.Path(credfile.RoleWorker)
	if err != nil {
		t.Fatal(err)
	}
	if err := credfile.Save(path, credfile.File{Role: credfile.RoleWorker, PrincipalID: "w", Token: "t", ControlPlaneURL: "https://circus.example"}); err != nil {
		t.Fatal(err)
	}
}

func runWorker(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	err := workerCommand(args, &out, &errOut)
	return out.String(), errOut.String(), err
}

// TestWorkerServiceRunsTheInstalledPackage installs the worker service as
// the enrollment command does, then starts what the service file says (its
// program and its environment, nothing else) the way systemd would: the
// package's own launcher finds the recorded Node.js and starts the worker
// with the recorded circus and proxy.
func TestWorkerServiceRunsTheInstalledPackage(t *testing.T) {
	f := newWorkerFixture(t, "v1.2.3")
	// The link on PATH lies in the temporary directory, which may vanish, so
	// the service records the executable it links to.
	node, err := filepath.EvalSymlinks(f.useNode(t))
	if err != nil {
		t.Fatal(err)
	}
	// The launcher needs the usual tools from the recorded PATH; the stubs
	// come first.
	path := f.bin + ":/usr/bin:/bin"
	t.Setenv("PATH", path)
	joinAsWorker(t)
	t.Setenv("HTTPS_PROXY", "http://user:secret@proxy:3128")
	t.Setenv("CIRCUS_WORKER_MAX_SESSIONS", "2")
	out, errOut, err := runWorker(t, "install")
	if err != nil {
		t.Fatalf("install: %v\n%s%s", err, out, errOut)
	}
	launcher := filepath.Join(f.root, "v1.2.3", "circus-worker")
	for _, want := range []string{
		"Downloading circus-worker-v1.2.3-",
		"✓ Installed and started the circus worker service.",
		"  Version    v1.2.3\n",
		"  Package    " + filepath.Join(f.root, "v1.2.3") + "\n",
		"  Node.js    " + node + "\n",
		"  File       " + f.unit + "\n",
		"  Logs       journalctl --user -u circus-worker.service -f\n",
		"The service file records HTTPS_PROXY as set in this shell",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("install output lacks %q:\n%s", want, out)
		}
	}
	if errOut != "" {
		t.Errorf("install diagnostics:\n%s", errOut)
	}

	d, ok, err := userservice.Worker.Installed()
	if err != nil || !ok || !slices.Equal(d.Argv, []string{launcher}) {
		t.Fatalf("service runs %v (%t, %v)", d.Argv, ok, err)
	}
	record := filepath.Join(f.dir, "record.json")
	cmd := exec.CommandContext(t.Context(), d.Program())
	cmd.Env = append(userservice.EnvironmentList(d.Env), "RECORD="+record)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the service's program: %v\n%s", err, output)
	}
	var got struct {
		Argv                                     []string
		Node, NodeVersion, CircusBin, CircusNode string
		Proxy, Sessions                          *string
		Path, Home                               string
	}
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.CircusNode != node || got.Node != node || got.CircusBin != f.circus || len(got.Argv) != 0 ||
		got.Proxy == nil || *got.Proxy != "http://user:secret@proxy:3128" || got.Sessions == nil || *got.Sessions != "2" ||
		got.Path != path || got.Home != filepath.Join(f.dir, "home") {
		t.Errorf("the worker started with %s", data)
	}

	out, _, err = runWorker(t, "status")
	if err != nil || !strings.Contains(out, "The circus worker service is installed and running (PID 4242).") ||
		!strings.Contains(out, "  Version    v1.2.3\n") || !strings.Contains(out, "  Starts     at boot or login\n") ||
		!strings.Contains(out, "  Logs       journalctl --user -u circus-worker.service -f\n") {
		t.Errorf("status: %v\n%s", err, out)
	}
	for _, action := range []string{"stop", "start", "restart"} {
		if out, _, err := runWorker(t, action); err != nil || !strings.Contains(out, "the circus worker service.") {
			t.Errorf("%s: %v\n%s", action, err, out)
		}
	}
	out, _, err = runWorker(t, "uninstall")
	if err != nil || !strings.Contains(out, "✓ Removed the circus worker service.") {
		t.Fatalf("uninstall: %v\n%s", err, out)
	}
	// The store keeps its lock file, which a waiting command may hold open.
	if entries, err := os.ReadDir(f.root); err != nil || len(entries) != 1 || entries[0].Name() != ".lock" {
		t.Errorf("uninstall left %v in the store (%v)", entries, err)
	}
	out, _, err = runWorker(t, "status")
	if err != nil || !strings.Contains(out, "not installed. Run `circus worker install`.") {
		t.Errorf("status after uninstall: %v\n%s", err, out)
	}
}

func TestWorkerCommandRefusals(t *testing.T) {
	f := newWorkerFixture(t, "dev")
	if _, _, err := runWorker(t, "install"); err == nil || !strings.Contains(err.Error(), "development build") || !strings.Contains(err.Error(), "worker/README.md") {
		t.Errorf("dev build: %v", err)
	}
	version = "v1.2.3"
	if _, _, err := runWorker(t, "install"); err == nil || !strings.Contains(err.Error(), "circus join --role worker") {
		t.Errorf("no credential: %v", err)
	}
	joinAsWorker(t)
	t.Setenv("CIRCUS_NODE", filepath.Join(f.dir, "no-node"))
	if _, _, err := runWorker(t, "install"); err == nil || !strings.Contains(err.Error(), "needs Node.js 24 or later") {
		t.Errorf("no Node.js: %v", err)
	}
	if _, _, err := runWorker(t, "start"); err == nil || !strings.Contains(err.Error(), "run `circus worker install`") {
		t.Errorf("start before install: %v", err)
	}
	for _, args := range [][]string{nil, {"run"}, {"install", "extra"}} {
		if _, _, err := runWorker(t, args...); err == nil || !strings.Contains(err.Error(), "usage: circus worker") {
			t.Errorf("%v: %v", args, err)
		}
	}
	if _, err := os.Stat(f.unit); !os.IsNotExist(err) {
		t.Errorf("a refused install wrote the unit: %v", err)
	}
}

// fakeNodeScript stands in for Node.js 24.
const fakeNodeScript = "#!/bin/sh\necho 24.0.0\n"

// updateFixture is a worker service on v1.2.3 run by f.circus, and a daemon
// service run by the same circus, with circus v1.3.0 released.
func updateFixture(t *testing.T) workerFixture {
	t.Helper()
	f := newWorkerFixture(t, "v1.2.3")
	node := filepath.Join(f.bin, "node")
	if err := os.WriteFile(node, []byte(fakeNodeScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CIRCUS_NODE", node)
	joinAsWorker(t)
	if out, errOut, err := runWorker(t, "install"); err != nil {
		t.Fatalf("install: %v\n%s%s", err, out, errOut)
	}
	daemonUnit := filepath.Join(filepath.Dir(f.unit), "circus-daemon.service")
	if err := os.WriteFile(daemonUnit, []byte("[Service]\nExecStart=\""+f.circus+"\" \"daemon\" \"--service\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(f.dir, "calls"))
	// The install's own restart waits for its job, as it should.
	blocked, _ := filepath.Glob(filepath.Join(f.dir, "blocked-*"))
	for _, path := range blocked {
		os.Remove(path)
	}
	return f
}

// release offers v1.3.0 of circus at path. Its Apply records the swap in
// calls and checks that the worker package was in place before it.
func (f workerFixture) release(t *testing.T, path string) *bool {
	applied := new(bool)
	fakeSelfUpdate(t, selfupdate.Plan{Version: "v1.3.0", Path: path, Apply: func(context.Context) (selfupdate.Result, error) {
		*applied = true
		f.call(t, "swap circus")
		if err := os.WriteFile(path, []byte("circus v1.3.0"), 0o755); err != nil {
			t.Fatal(err)
		}
		return selfupdate.Result{Message: "Updated circus to v1.3.0", ReplacedPath: path, Version: "v1.3.0"}, nil
	}})
	return applied
}

func (f workerFixture) call(t *testing.T, line string) {
	t.Helper()
	calls, err := os.OpenFile(filepath.Join(f.dir, "calls"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer calls.Close()
	if _, err := fmt.Fprintln(calls, line); err != nil {
		t.Fatal(err)
	}
}

func (f workerFixture) calls(t *testing.T) []string {
	t.Helper()
	data, _ := os.ReadFile(filepath.Join(f.dir, "calls"))
	os.Remove(filepath.Join(f.dir, "calls"))
	return strings.Fields(strings.ReplaceAll(strings.TrimSpace(string(data)), " ", "_"))
}

func (f workerFixture) workerVersion(t *testing.T) string {
	t.Helper()
	i, _, err := workerservice.InstalledService()
	if err != nil {
		t.Fatal(err)
	}
	return i.Version
}

func (f workerFixture) fail(t *testing.T, command string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, command+".fail"), []byte("Test failure"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateCommandUpdatesWorkerService(t *testing.T) {
	for _, tc := range []struct {
		name, output string
		stopped      bool
		wantCalls    []string
	}{
		{name: "running", output: "Switched the circus worker service from v1.2.3 to v1.3.0 and asked the service manager to restart it.\n" +
			"Asked the service manager to restart the circus device daemon service.\n" +
			"Check that they came up with `circus worker status` and `circus daemon status`.\n",
			// The worker switches before the daemon restarts.
			// Both service files are in place before either restart is
			// asked for, and neither request waits for its job.
			// in one request naming both, worker first; nothing waits for
			// them.
			wantCalls: []string{"swap_circus", "is-system-running", "daemon-reload",
				"show_circus-worker.service_--property=ActiveState,SubState,MainPID,UnitFileState,ExecMainCode,ExecMainStatus",
				"show_circus-daemon.service_--property=ActiveState,SubState,MainPID,UnitFileState,ExecMainCode,ExecMainStatus",
				"try-restart_--no-block_circus-worker.service_circus-daemon.service"}},
		{name: "stopped", stopped: true, output: "it is not running: run `circus worker start`"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := updateFixture(t)
			if tc.stopped {
				if _, _, err := runWorker(t, "stop"); err != nil {
					t.Fatal(err)
				}
				f.calls(t)
			}
			f.release(t, f.circus)
			var out, errOut bytes.Buffer
			if err := updateCommand(nil, &out, &errOut); err != nil {
				t.Fatalf("update: %v\n%s%s", err, out.String(), errOut.String())
			}
			if !strings.Contains(out.String(), tc.output) || errOut.Len() != 0 {
				t.Errorf("update output:\n%s\ndiagnostics:\n%s", out.String(), errOut.String())
			}
			calls := f.calls(t)
			if tc.wantCalls != nil && !slices.Equal(calls, tc.wantCalls) {
				t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(calls, "\n"), strings.Join(tc.wantCalls, "\n"))
			}
			if v := f.workerVersion(t); v != "v1.3.0" {
				t.Errorf("the worker service runs %s", v)
			}
			// The previous version stays until the next switch: the old
			// worker may still use it while its restart is underway.
			if entries, _ := os.ReadDir(f.root); len(entries) != 3 {
				t.Errorf("store holds %d entries, want the lock, v1.2.3 and v1.3.0", len(entries))
			}
			f.noBlockedRestarts(t)
		})
	}
}

// A worker package that can't be had stops the whole update: the binary
// is not replaced and nothing else changes.
func TestUpdateCommandAbortsWithoutTheWorkerPackage(t *testing.T) {
	f := updateFixture(t)
	target := runtime.GOOS + "-" + runtime.GOARCH
	f.srv.Set(fakerelease.Path("v1.3.0", fakerelease.PackageName("v1.3.0", target)), []byte("corrupt"))
	unit, _ := os.ReadFile(f.unit)
	applied := f.release(t, f.circus)
	var out, errOut bytes.Buffer
	err := updateCommand(nil, &out, &errOut)
	if err == nil || !strings.Contains(err.Error(), "nothing was updated") || !strings.Contains(err.Error(), "SHA-256 mismatch") {
		t.Fatalf("update = %v", err)
	}
	if *applied {
		t.Error("the binary was replaced")
	}
	if after, _ := os.ReadFile(f.unit); !bytes.Equal(unit, after) || f.workerVersion(t) != "v1.2.3" {
		t.Error("the worker service changed")
	}
	if calls := f.calls(t); len(calls) != 0 {
		t.Errorf("service manager calls: %v", calls)
	}
}

// A worker service that runs another circus is left alone, and the update
// goes on.
func TestUpdateCommandLeavesAWorkerOfAnotherCircus(t *testing.T) {
	f := updateFixture(t)
	other := filepath.Join(f.dir, "other-circus")
	if err := os.WriteFile(other, []byte("other"), 0o755); err != nil {
		t.Fatal(err)
	}
	applied := f.release(t, other)
	var out, errOut bytes.Buffer
	if err := updateCommand(nil, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if !*applied || !strings.Contains(errOut.String(), "uses another circus") || f.workerVersion(t) != "v1.2.3" {
		t.Errorf("applied %t, worker %s, diagnostics %q", *applied, f.workerVersion(t), errOut.String())
	}
}

func TestUpdateCommandWithoutWorkerService(t *testing.T) {
	f := newWorkerFixture(t, "v1.2.3")
	applied := f.release(t, f.circus)
	var out, errOut bytes.Buffer
	if err := updateCommand(nil, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if !*applied || out.String() != "Updated circus to v1.3.0\n" || errOut.Len() != 0 {
		t.Errorf("applied %t\n%s%s", *applied, out.String(), errOut.String())
	}
	if calls := f.calls(t); !slices.Equal(calls, []string{"swap_circus"}) {
		t.Errorf("calls: %v", calls)
	}
}

// A failed switch after the swap is an error that names the retry, and
// `circus update` with the CLI already current finishes the switch.
func TestUpdateCommandRetriesTheWorkerSwitch(t *testing.T) {
	f := updateFixture(t)
	f.release(t, f.circus)
	f.fail(t, "daemon-reload")
	var out, errOut bytes.Buffer
	err := updateCommand(nil, &out, &errOut)
	if err == nil || !strings.Contains(err.Error(), "could not switch the circus worker service to v1.3.0") ||
		!strings.Contains(err.Error(), "It still runs v1.2.3; run `circus update` again") {
		t.Fatalf("update = %v\n%s", err, out.String())
	}
	if v := f.workerVersion(t); v != "v1.2.3" {
		t.Fatalf("after a failed switch the worker service runs %s", v)
	}
	// The binary was replaced, and the daemon still restarted last.
	if calls := f.calls(t); calls[0] != "swap_circus" || calls[len(calls)-1] != "try-restart_--no-block_circus-daemon.service" {
		t.Errorf("calls: %v", calls)
	}

	os.Remove(filepath.Join(f.dir, "daemon-reload.fail"))
	fakeSelfUpdate(t, selfupdate.Plan{Version: "v1.3.0", UpToDate: true, Message: "circus is already at v1.3.0"})
	oldExe := runningExecutable
	t.Cleanup(func() { runningExecutable = oldExe })
	runningExecutable = func() (string, error) { return f.circus, nil }
	out.Reset()
	if err := updateCommand(nil, &out, &errOut); err != nil {
		t.Fatalf("retry: %v\n%s", err, out.String())
	}
	if !strings.HasPrefix(out.String(), "circus is already at v1.3.0\n") ||
		!strings.HasSuffix(out.String(), "Switched the circus worker service from v1.2.3 to v1.3.0 and asked the service manager to restart it.\n"+
			"Check that it came up with `circus worker status`.\n") {
		t.Errorf("retry output:\n%s", out.String())
	}
	if v := f.workerVersion(t); v != "v1.3.0" {
		t.Errorf("after the retry the worker service runs %s", v)
	}
	// Current and switched: nothing more to do.
	out.Reset()
	if err := updateCommand(nil, &out, &errOut); err != nil || out.String() != "circus is already at v1.3.0\n" {
		t.Errorf("second retry: %v\n%s", err, out.String())
	}
}

// `circus update` run by a Bash tool inside one of the services is killed
// when that service's restart job runs (systemd stops the whole cgroup of
// the daemon at once, and the rest of the worker's once Node.js exits).
// Every service file is written first and both restarts are asked for
// without waiting, worker first, so whichever kills it, both happen. The
// fake signals the caller when it is asked to restart the service it runs
// in, and records every request that would have waited for its job.
func TestUpdateCommandFromInsideAService(t *testing.T) {
	for _, unit := range []string{"circus-daemon.service", "circus-worker.service"} {
		t.Run(unit, func(t *testing.T) {
			f := updateFixture(t)
			if err := os.WriteFile(filepath.Join(f.dir, "active"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(f.dir, "kill-on-try-restart-"+unit), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			f.release(t, f.circus)
			var out, errOut bytes.Buffer
			err := updateCommand(nil, &out, &errOut)
			if v := f.workerVersion(t); err != nil || v != "v1.3.0" {
				t.Errorf("update = %v; the worker service runs %s\n%s%s", err, v, out.String(), errOut.String())
			}
			calls := f.calls(t)
			// One request names both, worker first, before either can act.
			if !slices.Contains(calls, "try-restart_--no-block_circus-worker.service_circus-daemon.service") {
				t.Errorf("restarts asked for: %v", calls)
			}
			f.noBlockedRestarts(t)
		})
	}
}

func (f workerFixture) noBlockedRestarts(t *testing.T) {
	t.Helper()
	if blocked, _ := filepath.Glob(filepath.Join(f.dir, "blocked-*")); len(blocked) != 0 {
		t.Errorf("restarts that wait for their job: %v", blocked)
	}
}

// `circus worker status` says why a worker that stopped itself did, from
// the stop file the worker or its launcher wrote, never from the exit
// status alone.
func TestWorkerStatusSaysWhyItStopped(t *testing.T) {
	f := updateFixture(t)
	status := func() string {
		t.Helper()
		out, _, err := runWorker(t, "status")
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	// Exit 78 without a stop file: point at the logs.
	if err := os.WriteFile(filepath.Join(f.dir, "terminal"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if out := status(); !strings.Contains(out, "  Last exit  exit status 78\n") ||
		!strings.Contains(out, "It stopped itself and stays stopped. See the logs for why: journalctl") {
		t.Errorf("status after exit 78:\n%s", out)
	}
	stopFile := filepath.Join(f.dir, "data", "circus", "worker-state", "stop.json")
	// From here the worker exited 0, as it does on macOS for a terminal
	// stop: only the stop file tells.
	os.Remove(filepath.Join(f.dir, "terminal"))
	os.Remove(filepath.Join(f.dir, "active"))
	for reason, want := range map[string]string{
		"credential-refused": "It stopped itself and stays stopped. The control plane refused its credential (worker upgrade HTTP 401): the worker was revoked. Enroll again with `circus join --role worker --force`",
		"node":               "It stopped itself and stays stopped. Its Node.js is missing or too old (worker upgrade HTTP 401). Install Node.js 24 or later, then run `circus worker install`.",
	} {
		if err := os.WriteFile(stopFile, []byte(`{"reason":"`+reason+`","message":"worker upgrade HTTP 401"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if out := status(); !strings.Contains(out, want) {
			t.Errorf("status with %s:\n%s", reason, out)
		}
	}
	// A clean exit (a plain kill, exit 0 on macOS) says nothing of the kind.
	os.Remove(stopFile)
	if out := status(); strings.Contains(out, "stopped itself") || strings.Contains(out, "refused") {
		t.Errorf("status after a clean exit:\n%s", out)
	}
	// A running worker ignores a stale stop file.
	if err := os.WriteFile(filepath.Join(f.dir, "active"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stopFile, []byte(`{"reason":"config","message":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if out := status(); strings.Contains(out, "stopped itself") {
		t.Errorf("status of a running worker:\n%s", out)
	}
	// The recorded Node.js is gone (a version manager removed it).
	if err := os.Remove(filepath.Join(f.bin, "node")); err != nil {
		t.Fatal(err)
	}
	if out := status(); !strings.Contains(out, "The Node.js it was installed with ("+filepath.Join(f.bin, "node")+") is gone. Run `circus worker install`") {
		t.Errorf("status without Node.js:\n%s", out)
	}
}

// Without a usable Node.js the package's launcher exits with the terminal
// status, which the worker unit does not restart.
func TestLauncherExitsTerminallyWithoutNode(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the terminal status is 0 on macOS")
	}
	dir := t.TempDir()
	launcher := filepath.Join(dir, "circus-worker")
	old := filepath.Join(dir, "old-node")
	for path, text := range map[string]string{launcher: launcherScript, old: "#!/bin/sh\necho 22.11.0\n"} {
		if err := os.WriteFile(path, []byte(text), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// The stop file is the recorded one, under an existing data directory;
	// $CIRCUS_HOME points elsewhere and is never created.
	data := filepath.Join(dir, "data")
	if err := os.Mkdir(data, 0o755); err != nil {
		t.Fatal(err)
	}
	stop := filepath.Join(data, "worker-state", "stop.json")
	for node, want := range map[string]string{
		filepath.Join(dir, "missing"): "Node.js 24 or later is required: " + filepath.Join(dir, "missing") + " not found",
		old:                           "Node.js 24 or later is required: " + old + " is 22.11.0",
	} {
		cmd := exec.CommandContext(t.Context(), launcher)
		cmd.Env = []string{"PATH=/usr/bin:/bin", "CIRCUS_NODE=" + node, "CIRCUS_HOME=" + filepath.Join(dir, "unused-home"), "CIRCUS_WORKER_STOP_FILE=" + stop}
		out, err := cmd.CombinedOutput()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 78 || !strings.Contains(string(out), want) {
			t.Errorf("CIRCUS_NODE=%s: %v\n%s", node, err, out)
		}
		var recorded struct{ Reason, Message string }
		data, err := os.ReadFile(stop)
		if err != nil || json.Unmarshal(data, &recorded) != nil || recorded.Reason != "node" || !strings.HasPrefix(recorded.Message, want) {
			t.Errorf("stop file: %q, %v", data, err)
		}
		if info, err := os.Stat(stop); err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("stop file mode: %v, %v", info, err)
		}
		if info, err := os.Stat(filepath.Dir(stop)); err != nil || info.Mode().Perm() != 0o700 {
			t.Errorf("stop file directory: %v, %v", info, err)
		}
		if entries, _ := os.ReadDir(filepath.Dir(stop)); len(entries) != 1 {
			t.Errorf("the write left %v", entries)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "unused-home")); !os.IsNotExist(err) {
		t.Errorf("the launcher created $CIRCUS_HOME: %v", err)
	}
	// It creates only the file's own directory, never one above it.
	cmd := exec.CommandContext(t.Context(), launcher)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "CIRCUS_NODE=" + old, "CIRCUS_WORKER_STOP_FILE=" + filepath.Join(dir, "absent", "worker-state", "stop.json")}
	if _, err := cmd.CombinedOutput(); err == nil {
		t.Fatal("the launcher ran without Node.js")
	}
	if _, err := os.Stat(filepath.Join(dir, "absent")); !os.IsNotExist(err) {
		t.Errorf("the launcher created the data directory: %v", err)
	}
}

// A refused request is an error. When one request covered both services,
// its failure is reported once, naming both units, not as a failure of
// each.
func TestUpdateCommandReportsARefusedRestart(t *testing.T) {
	f := updateFixture(t)
	f.release(t, f.circus)
	f.fail(t, "try-restart")
	var out, errOut bytes.Buffer
	err := updateCommand(nil, &out, &errOut)
	if err == nil || !strings.Contains(err.Error(), "switched the circus worker service from v1.2.3 to v1.3.0, but could not restart the worker and daemon services") ||
		!strings.Contains(err.Error(), "the one request to restart circus-worker.service and circus-daemon.service failed (it covered all of them)") ||
		!strings.Contains(err.Error(), "Run `circus worker restart` and `circus daemon restart`") || errOut.Len() != 0 ||
		strings.Contains(out.String(), "asked the service manager") {
		t.Fatalf("update = %v\n%s%s", err, out.String(), errOut.String())
	}
}

// lockedWriter collects output and notices when a command starts waiting
// for the worker store's lock.
type lockedWriter struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	once    sync.Once
	waiting chan struct{}
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if bytes.Contains(p, []byte("Waiting for another install or update")) {
		w.once.Do(func() { close(w.waiting) })
	}
	return w.buf.Write(p)
}

func (w *lockedWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// `circus update` decides on the service file as it reads it under the
// store's lock: one that another command moved to the new version while it
// waited is not switched again.
func TestUpdateCommandRereadsTheServiceUnderTheLock(t *testing.T) {
	f := updateFixture(t)
	lock, err := os.OpenFile(filepath.Join(f.root, ".lock"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	f.release(t, f.circus)
	out := &lockedWriter{waiting: make(chan struct{})}
	var errOut bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- updateCommand(nil, out, &errOut) }()
	<-out.waiting
	// Meanwhile another command switched the service to v1.3.0.
	unit, err := os.ReadFile(f.unit)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.unit, bytes.ReplaceAll(unit, []byte("/v1.2.3/"), []byte("/v1.3.0/")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("update: %v\n%s", err, out.String())
	}
	if strings.Contains(out.String(), "Switched") || strings.Contains(out.String(), "Downloading") {
		t.Errorf("the update switched the worker again:\n%s", out.String())
	}
	if calls := f.calls(t); slices.Contains(calls, "try-restart_--no-block_circus-worker.service") || slices.Contains(calls, "daemon-reload") {
		t.Errorf("calls: %v", calls)
	}
}
