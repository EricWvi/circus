//go:build e2e && linux

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/credfile"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/store/sqlite"
	testwait "github.com/EricWvi/circus/internal/testutil/wait"
	"github.com/EricWvi/circus/internal/wire"
	"github.com/EricWvi/circus/internal/workerclient"
	"github.com/coder/websocket"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
)

var binary, root string

func TestMain(m *testing.M) {
	os.Exit(runE2E(m))
}

// sweepE2ERuns removes only run directories whose flock can be acquired. The
// caller holds the startup lock, so a new run cannot race lock creation. Hard
// testing timeouts release the OS lock even though defers do not run.
func sweepE2ERuns(tmp string) error {
	entries, err := os.ReadDir(tmp)
	if err != nil {
		return err
	}
	const prefix = "e2e-run-"
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		lockPath := filepath.Join(tmp, entry.Name(), "run.lock")
		lock, err := os.OpenFile(lockPath, os.O_RDWR, 0)
		if errors.Is(err, os.ErrNotExist) {
			// Runs predating run.lock have no liveness lock. Delete only their
			// old directories, preserving a concurrently created young run.
			info, statErr := entry.Info()
			if statErr != nil {
				return statErr
			}
			if time.Since(info.ModTime()) > time.Hour {
				if err := os.RemoveAll(filepath.Join(tmp, entry.Name())); err != nil {
					return err
				}
			}
			continue
		}
		if err != nil {
			return err
		}
		err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		_ = lock.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			continue
		}
		if err != nil {
			return err
		}
		if err := os.RemoveAll(filepath.Join(tmp, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func acquireFlock(path string) (*os.File, error) {
	lock, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		_ = lock.Close()
		return nil, err
	}
	return lock, nil
}

func runE2E(m *testing.M) (code int) {
	cwd, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	root = filepath.Clean(filepath.Join(cwd, "../.."))
	tmp := filepath.Join(root, ".tmp")
	if err = os.MkdirAll(tmp, 0700); err != nil {
		panic(err)
	}
	startupLock, err := acquireFlock(filepath.Join(tmp, "e2e-startup.lock"))
	if err != nil {
		panic(err)
	}
	if err = sweepE2ERuns(tmp); err != nil {
		_ = startupLock.Close()
		panic(err)
	}
	runDir, err := os.MkdirTemp(tmp, "e2e-run-"+strconv.Itoa(os.Getpid())+"-")
	if err != nil {
		_ = startupLock.Close()
		panic(err)
	}
	runLock, err := acquireFlock(filepath.Join(runDir, "run.lock"))
	_ = startupLock.Close()
	if err != nil {
		panic(err)
	}
	// Normal returns remove the run root. A testing-timeout skips defers, so the
	// next invocation sweeps it after the OS releases runLock.
	defer func() { _ = os.RemoveAll(runDir); _ = runLock.Close() }()
	tempDir := filepath.Join(runDir, "temp")
	if err = os.MkdirAll(tempDir, 0700); err != nil {
		panic(err)
	}
	_ = os.Setenv("TMPDIR", tempDir)
	build := filepath.Join(runDir, "build")
	if err = os.MkdirAll(build, 0700); err != nil {
		panic(err)
	}
	binary = filepath.Join(build, "bin/circus")
	args := []string{"build", "-tags", "e2efault", "-o", binary, "./cmd/circus"}
	if raceEnabled {
		args = append(args[:1], append([]string{"-race"}, args[1:]...)...)
	}
	cmd := exec.Command("go", args...)
	cmd.Dir = root
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	buildChild := &child{cmd: cmd, done: make(chan struct{})}
	if err = startChild(buildChild); err != nil {
		return 1
	}
	<-buildChild.done
	if buildChild.err != nil {
		return 1
	}
	// Adopt only this test binary's orphaned tool descendants after F4's SIGKILL.
	// Reaping always targets a recorded PID, never arbitrary children.
	if _, _, errno := syscall.Syscall6(syscall.SYS_PRCTL, 36, 1, 0, 0, 0, 0); errno != 0 {
		panic(errno)
	}
	return m.Run()
}

type lockedBuffer struct {
	sync.Mutex
	b bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) { b.Lock(); defer b.Unlock(); return b.b.Write(p) }
func (b *lockedBuffer) String() string              { b.Lock(); defer b.Unlock(); return b.b.String() }

type child struct {
	cmd            *exec.Cmd
	stderr, stdout lockedBuffer
	done           chan struct{}
	err            error
}

// startChild keeps the exact OS thread that calls Start alive through Wait.
// Linux Pdeathsig therefore cannot fire early when another Go thread is retired.
func startChild(p *child) error {
	started := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		err := p.cmd.Start()
		started <- err
		if err == nil {
			p.err = p.cmd.Wait()
		}
		close(p.done)
	}()
	return <-started
}

func (p *child) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}
func (p *child) signal(t *testing.T, s syscall.Signal) {
	t.Helper()
	if p.exited() {
		return
	}
	if err := p.cmd.Process.Signal(s); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Fatal(err)
	}
}
func (p *child) wait(t *testing.T, d time.Duration) error {
	t.Helper()
	d = max(d, testwait.Timeout)
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-p.done:
		return p.err
	case <-timer.C:
		t.Fatalf("PID %d failed to exit: %s", p.cmd.Process.Pid, p.stderr.String())
		return nil
	}
}

type harness struct {
	t                   *testing.T
	dir, db, addr, base string
	grace               string
	env                 []string
	server, daemon      *child
	st                  *sqlite.DB
	worker              credfile.File
}

func newHarness(t *testing.T, grace ...string) *harness {
	t.Helper()
	h := &harness{t: t, dir: t.TempDir(), grace: "5s"}
	if len(grace) > 0 {
		h.grace = grace[0]
	}
	h.db = filepath.Join(h.dir, "db")
	h.env = append(os.Environ(), "HOME="+h.dir, "XDG_CONFIG_HOME="+filepath.Join(h.dir, "config"), "XDG_DATA_HOME="+filepath.Join(h.dir, "data"), "XDG_CACHE_HOME="+filepath.Join(h.dir, "cache"), "CIRCUS_E2E_CRASH_DISPATCH=0", "GORACE=atexit_sleep_ms=0")
	h.startServer(false)
	link := h.run("", 0, "server", "enroll-token", "create", "--db", h.db, "--role", "both", "--url", h.base)
	h.run(link, 0, "join", "--name", "desk")
	var err error
	h.worker, err = credfile.Load(filepath.Join(h.dir, "config/circus/worker.json"))
	must(t, err)
	h.st, err = sqlite.Open(t.Context(), h.db, sqlite.Options{})
	must(t, err)
	t.Cleanup(func() { _ = h.st.Close() })
	h.startDaemon()
	h.readyDaemon()
	return h
}
func (h *harness) start(extra []string, input string, args ...string) *child {
	h.t.Helper()
	p := &child{done: make(chan struct{})}
	p.cmd = exec.Command(binary, args...)
	p.cmd.Env = append(append([]string{}, h.env...), extra...)
	p.cmd.Dir = h.dir
	p.cmd.Stdin = strings.NewReader(input)
	p.cmd.Stderr = &p.stderr
	p.cmd.Stdout = &p.stdout
	p.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	must(h.t, startChild(p))
	h.t.Cleanup(func() {
		if !p.exited() {
			p.signal(h.t, syscall.SIGTERM)
			timer := time.NewTimer(testwait.Timeout)
			select {
			case <-p.done:
				timer.Stop()
			case <-timer.C:
				p.signal(h.t, syscall.SIGKILL)
				_ = p.wait(h.t, 2*time.Second)
			}
		}
		if !p.exited() {
			h.t.Errorf("child PID %d remains alive", p.cmd.Process.Pid)
		}
		if h.t.Failed() {
			h.t.Logf("PID %d %v\nstderr:\n%s\nstdout:\n%s", p.cmd.Process.Pid, args, p.stderr.String(), p.stdout.String())
		}
	})
	return p
}
func (h *harness) run(input string, code int, args ...string) string {
	h.t.Helper()
	stdout, stderr, actual := h.runCapture(input, args...)
	if actual != code {
		h.t.Fatalf("%v: exit %d, want %d\n%s", args, actual, code, stderr)
	}
	return stdout
}
func (h *harness) runCapture(input string, args ...string) (stdout, stderr string, code int) {
	h.t.Helper()
	p := h.start(nil, input, args...)
	err := p.wait(h.t, 10*time.Second)
	if err == nil {
		return p.stdout.String(), p.stderr.String(), 0
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		h.t.Fatal(err)
	}
	return p.stdout.String(), p.stderr.String(), exit.ExitCode()
}
func (h *harness) waitCode(p *child, d time.Duration) int {
	h.t.Helper()
	err := p.wait(h.t, d)
	if err == nil {
		return 0
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		h.t.Fatal(err)
	}
	return exit.ExitCode()
}
func (h *harness) startServer(fault bool) {
	listen := h.addr
	if listen == "" {
		listen = "127.0.0.1:0"
	}
	extra := []string{}
	if fault {
		extra = append(extra, "CIRCUS_E2E_CRASH_DISPATCH=1")
	}
	h.server = h.start(extra, "", "server", "--db", h.db, "--listen", listen, "--dev-insecure", "--device-grace", h.grace, "--timer-poll-interval", "50ms", "--heartbeat-interval", "1s", "--heartbeat-timeout", "6s", "--shutdown-timeout", "4s", "--max-message-bytes", "8388608")
	wait(h.t, 5*time.Second, func() bool {
		if h.server.exited() {
			h.t.Fatalf("server exited: %s", h.server.stderr.String())
		}
		for line := range strings.Lines(h.server.stderr.String()) {
			var rec struct{ Msg, Addr string }
			if json.Unmarshal([]byte(line), &rec) == nil && rec.Msg == "control plane listening" {
				h.addr = rec.Addr
				h.base = "http://" + h.addr
				return true
			}
		}
		return false
	})
	client := &http.Client{Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()
	wait(h.t, 5*time.Second, func() bool {
		res, err := client.Get(h.base + "/readyz")
		if err != nil {
			return false
		}
		defer res.Body.Close()
		return res.StatusCode == 200
	})
}
func (h *harness) startDaemon() { h.daemon = h.start(nil, "", "daemon") }
func (h *harness) client() *workerclient.Client {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(h.t.Context(), testwait.Timeout)
	defer cancel()
	c, err := workerclient.Dial(ctx, workerclient.Options{Credentials: h.worker})
	must(h.t, err)
	h.t.Cleanup(func() { _ = c.Close() })
	return c
}
func (h *harness) rawWorker() *wire.Link {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(h.t.Context(), testwait.Timeout)
	defer cancel()
	conn, _, err := wire.DialWebSocket(ctx, "ws"+strings.TrimPrefix(h.base, "http")+"/v1/link/worker", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": []string{"Bearer " + h.worker.Token}}})
	must(h.t, err)
	data, err := proto.Marshal(&circusv1.WorkerUplink{Payload: &circusv1.WorkerUplink_Hello{Hello: &circusv1.Hello{ProtocolVersion: 1}}})
	must(h.t, err)
	must(h.t, conn.WriteMessage(ctx, data))
	data, err = conn.ReadMessage(ctx)
	must(h.t, err)
	msg, err := wire.Decode(wire.WorkerDownlink, data, wire.MaxMetadataBytes)
	must(h.t, err)
	opts, err := wire.OptionsFromWelcome(msg.(*circusv1.WorkerDownlink).GetWelcome(), wire.Options{})
	must(h.t, err)
	link, err := wire.New(conn, wire.WorkerSide, opts)
	must(h.t, err)
	h.t.Cleanup(func() { _ = link.Close() })
	return link
}
func (h *harness) sendWorker(link *wire.Link, msg *circusv1.WorkerUplink) {
	h.t.Helper()
	must(h.t, link.Send(h.t.Context(), msg))
}
func (h *harness) call(id, command string) *circusv1.DeviceCall {
	return &circusv1.DeviceCall{CallId: id, SessionId: "session", Tool: &circusv1.DeviceCall_Bash{Bash: &circusv1.BashRequest{Command: command, Cwd: h.dir}}}
}
func (h *harness) submit(c *workerclient.Client, call *circusv1.DeviceCall) *workerclient.Updates {
	h.t.Helper()
	s, err := c.Submit(h.t.Context(), "desk", call)
	must(h.t, err)
	h.t.Cleanup(func() { _ = s.Close() })
	return s
}
func (h *harness) result(s *workerclient.Updates) *circusv1.CallUpdate {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(h.t.Context(), testwait.Timeout)
	defer cancel()
	for {
		u, err := s.Next(ctx)
		must(h.t, err)
		if wire.Terminal(u) {
			return u
		}
	}
}
func (h *harness) readyDaemon() {
	h.t.Helper()
	wait(h.t, 4*time.Second, func() bool {
		ids, err := h.st.ListIdentities(h.t.Context(), store.KindDevice, h.user())
		return err == nil && len(ids) == 1 && ids[0].LastConnectedAt > 0 && ids[0].LastConnectedAt > ids[0].LastDisconnectedAt
	})
	c := h.client()
	defer c.Close()
	u := h.result(h.submit(c, h.call(uuid.NewString(), "printf ready")))
	if output(u) != "ready" {
		h.t.Fatalf("daemon not ready: %v", u)
	}
}
func (h *harness) state(id string, want store.CallState) store.Call {
	h.t.Helper()
	var row store.Call
	wait(h.t, 6*time.Second, func() bool {
		var err error
		row, err = h.st.GetCall(h.t.Context(), id)
		return err == nil && row.State == want
	})
	return row
}
func (h *harness) offline() {
	h.t.Helper()
	wait(h.t, 3*time.Second, func() bool {
		ids, err := h.st.ListIdentities(h.t.Context(), store.KindDevice, h.user())
		return err == nil && len(ids) == 1 && ids[0].LastDisconnectedAt >= ids[0].LastConnectedAt && ids[0].LastDisconnectedAt != 0
	})
}
func (h *harness) user() string {
	u, err := h.st.GetUserByName(h.t.Context(), "admin")
	must(h.t, err)
	return u.ID
}
func (h *harness) query(id string) *circusv1.CallUpdate {
	h.t.Helper()
	c := h.client()
	defer c.Close()
	ctx, cancel := context.WithTimeout(h.t.Context(), testwait.Timeout)
	defer cancel()
	u, err := c.Query(ctx, id)
	must(h.t, err)
	return u
}
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func wait(t *testing.T, d time.Duration, p func() bool) {
	t.Helper()
	d = max(d, testwait.Timeout)
	deadline := time.NewTimer(d)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if p() {
			return
		}
		select {
		case <-deadline.C:
			t.Fatal("condition deadline exceeded")
		case <-ticker.C:
		}
	}
}
func output(u *circusv1.CallUpdate) string {
	var b strings.Builder
	for _, v := range u.GetResult().GetOutput().GetContent() {
		b.WriteString(v.GetText())
	}
	return b.String()
}
func markerLines(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	must(t, err)
	return strings.Count(string(data), "\n")
}
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }

func TestRunDirectorySweepUsesFlock(t *testing.T) {
	tmp := t.TempDir()
	active := filepath.Join(tmp, "e2e-run-active")
	stale := filepath.Join(tmp, "e2e-run-stale")
	for _, dir := range []string{active, stale} {
		must(t, os.MkdirAll(dir, 0700))
	}
	activeLock, err := acquireFlock(filepath.Join(active, "run.lock"))
	must(t, err)
	staleLock, err := acquireFlock(filepath.Join(stale, "run.lock"))
	must(t, err)
	must(t, staleLock.Close())
	must(t, sweepE2ERuns(tmp))
	if _, err = os.Stat(active); err != nil {
		t.Fatal("actively locked run was swept")
	}
	if _, err = os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unlocked run was not swept")
	}
	must(t, activeLock.Close())
	must(t, sweepE2ERuns(tmp))
	if _, err = os.Stat(active); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("released run was not swept")
	}
	legacy := filepath.Join(tmp, "e2e-run-12345-legacy")
	young := filepath.Join(tmp, "e2e-run-young")
	for _, dir := range []string{legacy, young} {
		must(t, os.MkdirAll(dir, 0700))
	}
	old := time.Now().Add(-2 * time.Hour)
	must(t, os.Chtimes(legacy, old, old))
	must(t, sweepE2ERuns(tmp))
	if _, err = os.Stat(legacy); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("old lock-less run was not swept")
	}
	if _, err = os.Stat(young); err != nil {
		t.Fatal("young lock-less run was swept")
	}
}

func TestHappyPath(t *testing.T) {
	h := newHarness(t)
	path := filepath.Join(h.dir, "cli.txt")
	for _, tc := range []struct {
		in       string
		args     []string
		contains string
	}{
		{"", []string{"--cwd", h.dir, "bash", "printf cli-bash"}, "cli-bash"},
		{"hello world\n", []string{"write", path}, ""},
		{"", []string{"read", path, "--offset", "1", "--limit", "1"}, "hello world"},
		{"", []string{"edit", path, "--old", "world", "--new", "circus"}, ""},
		{"", []string{"read", path}, "hello circus"},
	} {
		got := h.run(tc.in, 0, append([]string{"call", "--device", "desk"}, tc.args...)...)
		if !strings.Contains(got, tc.contains) {
			t.Fatalf("output %q lacks %q", got, tc.contains)
		}
	}
	// A tool failure remains a successful protocol result and CLI exit zero.
	h.run("", 0, "call", "--device", "desk", "read", filepath.Join(h.dir, "missing"))
	c := h.client()
	path = filepath.Join(h.dir, "client.txt")
	calls := []*circusv1.DeviceCall{
		h.call("bash", "printf client-bash"),
		{CallId: "write", SessionId: "session", Tool: &circusv1.DeviceCall_Write{Write: &circusv1.WriteRequest{Path: path, Content: "alpha\n"}}},
		{CallId: "read", SessionId: "session", Tool: &circusv1.DeviceCall_Read{Read: &circusv1.ReadRequest{Path: path}}},
		{CallId: "edit", SessionId: "session", Tool: &circusv1.DeviceCall_Edit{Edit: &circusv1.EditRequest{Path: path, OldString: "alpha", NewString: "beta"}}},
	}
	for _, call := range calls {
		u := h.result(h.submit(c, call))
		if u.GetResult() == nil || u.GetResult().GetOutput().GetIsError() {
			t.Fatal(u)
		}
		if call.CallId == "read" && !strings.Contains(output(u), "alpha") {
			t.Fatal(u)
		}
		if call.CallId == "bash" && output(u) != "client-bash" {
			t.Fatal(u)
		}
	}
	data, err := os.ReadFile(path)
	must(t, err)
	if string(data) != "beta\n" {
		t.Fatal(string(data))
	}
	h.run("", 0, "call", "query", "bash")
	h.run("", 0, "call", "cancel", "bash") // Terminal completion wins cancellation.
}

func TestCallValidationAndNotFound(t *testing.T) {
	h := newHarness(t)
	h.run("", 1, "call", "--device", "desk", "--call-id", "bad id!", "bash", "true")
	c := h.client()
	if _, err := c.Submit(t.Context(), "desk", h.call("bad id!", "true")); err == nil {
		t.Fatal("Go client sent an invalid call ID")
	}
	_ = c.Close()
	raw := h.rawWorker()
	h.sendWorker(raw, &circusv1.WorkerUplink{Payload: &circusv1.WorkerUplink_SubmitCall{SubmitCall: &circusv1.SubmitCall{Device: "desk", Call: h.call("bad id!", "true")}}})
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if update, err := raw.Receive(ctx); err == nil {
		t.Fatalf("empty-ID rejection was emitted as an unroutable update: %v", update)
	}
	_ = raw.Close()
	for _, operation := range []string{"query", "cancel"} {
		_, stderr, code := h.runCapture("", "call", operation, "does-not-exist")
		if code != 1 || !strings.Contains(stderr, "unknown call id") {
			t.Fatalf("%s: code=%d stderr=%s", operation, code, stderr)
		}
	}
}

func TestCLIExitCodes(t *testing.T) {
	h := newHarness(t)
	c := h.client()
	marker := filepath.Join(h.dir, "marker")
	pidfile := filepath.Join(h.dir, "tool.pid")
	call := h.call("running-cli", "printf 'run\n' >> "+shellQuote(marker)+"; printf '%s' $$ > "+shellQuote(pidfile)+"; exec sleep 20")
	stream := h.submit(c, call)
	h.trackTool(pidfile)
	wait(t, 3*time.Second, func() bool { return markerLines(t, marker) == 1 })
	h.run("", 4, "call", "query", call.CallId)
	h.run("", 0, "call", "cancel", call.CallId)
	if u := h.result(stream); u.GetResult() == nil {
		t.Fatalf("running cancel did not preserve the final result: %v", u)
	}
	h.daemon.signal(t, syscall.SIGTERM)
	must(t, h.daemon.wait(t, 3*time.Second))
	h.offline()
	accepted := h.call("accepted-cli", "printf should-not-run")
	stream = h.submit(c, accepted)
	ctx, cancel := context.WithTimeout(t.Context(), testwait.Timeout)
	defer cancel()
	u, err := stream.Next(ctx)
	must(t, err)
	if u.GetAccepted() == nil {
		t.Fatal(u)
	}
	h.run("", 0, "call", "cancel", accepted.CallId)
	h.run("", 1, "call", "query", accepted.CallId)
}

func TestClientCancelWatchedCall(t *testing.T) {
	h := newHarness(t)
	c := h.client()
	marker := filepath.Join(h.dir, "marker")
	pidfile := filepath.Join(h.dir, "tool.pid")
	call := h.call("watch-cancel", "printf 'run\n' >> "+shellQuote(marker)+"; printf '%s' $$ > "+shellQuote(pidfile)+"; exec sleep 20")
	stream := h.submit(c, call)
	h.trackTool(pidfile)
	wait(t, 3*time.Second, func() bool { return markerLines(t, marker) == 1 })
	must(t, c.Cancel(t.Context(), call.CallId))
	if update := h.result(stream); update.GetResult() == nil {
		t.Fatalf("watched stream did not carry the real terminal update: %v", update)
	}
}

func TestCancelErrorDoesNotBlockLaterCancel(t *testing.T) {
	h := newHarness(t)
	c := h.client()
	streams := make([]*workerclient.Updates, 4)
	for i := range 4 {
		pidfile := filepath.Join(h.dir, "hold-"+strconv.Itoa(i)+".pid")
		call := h.call("hold"+strconv.Itoa(i), "printf '%s' $$ > "+shellQuote(pidfile)+"; exec sleep 20")
		streams[i] = h.submit(c, call)
		h.trackTool(pidfile)
	}
	must(t, c.Cancel(t.Context(), "no-such-call"))
	must(t, c.Cancel(t.Context(), "hold0"))
	var row store.Call
	wait(t, 2*time.Second, func() bool {
		var err error
		row, err = h.st.GetCall(t.Context(), "hold0")
		return err == nil && row.CancelRequestedAt != 0
	})
	if row.CancelRequestedAt == 0 {
		t.Fatal("real Cancel stalled behind Cancel NOT_FOUND")
	}
	if update := h.result(streams[0]); update.GetResult() == nil {
		t.Fatalf("cancelled call did not publish its real result: %v", update)
	}
}

func TestClosedStreamGhostKeepsCap(t *testing.T) {
	h := newHarness(t)
	c := h.client()
	streams := make([]*workerclient.Updates, 4)
	calls := make([]*circusv1.DeviceCall, 4)
	for i := range 4 {
		pidfile := filepath.Join(h.dir, "tool-"+strconv.Itoa(i)+".pid")
		calls[i] = h.call("ghost-"+strconv.Itoa(i), "printf '%s' $$ > "+shellQuote(pidfile)+"; exec sleep 20")
		streams[i] = h.submit(c, calls[i])
		h.trackTool(pidfile)
	}
	must(t, streams[0].Close())
	fifth := h.call("fifth", "printf fifth")
	_, err := c.Submit(t.Context(), "desk", fifth)
	if err == nil || errors.Is(err, workerclient.ErrSubmitSent) {
		t.Fatalf("fifth Submit was not refused locally: %v", err)
	}
	select {
	case <-c.Done():
		t.Fatal("locally refused fifth Submit closed the link")
	default:
	}
	if _, err = h.st.GetCall(t.Context(), fifth.CallId); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("locally refused fifth call reached the server: %v", err)
	}
	must(t, c.Cancel(t.Context(), calls[0].CallId))
	h.state(calls[0].CallId, store.CallCompleted)
	var fifthStream *workerclient.Updates
	wait(t, 3*time.Second, func() bool {
		fifthStream, err = c.Submit(t.Context(), "desk", fifth)
		if err != nil && errors.Is(err, workerclient.ErrSubmitSent) {
			t.Fatalf("fifth Submit failed on the live link: %v", err)
		}
		return err == nil
	})
	if update := h.result(fifthStream); output(update) != "fifth" {
		t.Fatalf("ghost did not release after terminal update: %v", update)
	}
}

func TestCLITransportFailureIsUnknown(t *testing.T) {
	h := newHarness(t)
	marker := filepath.Join(h.dir, "marker")
	pidfile := filepath.Join(h.dir, "tool.pid")
	command := "printf 'run\n' >> " + shellQuote(marker) + "; printf '%s' $$ > " + shellQuote(pidfile) + "; exec sleep 20"
	process := h.start(nil, "", "call", "--device", "desk", "--call-id", "transport-unknown", "--cwd", h.dir, "bash", command)
	h.trackTool(pidfile)
	wait(t, 3*time.Second, func() bool { return markerLines(t, marker) == 1 })
	h.server.signal(t, syscall.SIGKILL)
	_ = h.server.wait(t, 2*time.Second)
	if code := h.waitCode(process, 3*time.Second); code != 3 || !strings.Contains(process.stderr.String(), "transport-unknown") {
		t.Fatalf("transport failure code=%d stderr=%s", code, process.stderr.String())
	}
}

func TestWorkerCloseSlotsAndCancelOrdering(t *testing.T) {
	h := newHarness(t)
	marker := filepath.Join(h.dir, "marker")
	gate := filepath.Join(h.dir, "gate")
	must(t, syscall.Mkfifo(gate, 0600))
	f, err := os.OpenFile(gate, os.O_RDWR, 0600)
	must(t, err)
	defer f.Close()
	c := h.client()
	call := h.call("close-subscription", "printf 'run\n' >> "+shellQuote(marker)+"; read -r release < "+shellQuote(gate)+"; printf close-survived")
	h.submit(c, call)
	wait(t, 3*time.Second, func() bool { return markerLines(t, marker) == 1 })
	_ = c.Close()
	c = h.client()
	queryCtx, queryCancel := context.WithTimeout(t.Context(), testwait.Timeout)
	defer queryCancel()
	outcome, err := c.Query(queryCtx, call.CallId)
	must(t, err)
	if outcome.GetDispatched() == nil {
		t.Fatalf("call did not survive worker link close: %v", outcome)
	}
	_, err = io.WriteString(f, "release\n")
	must(t, err)
	var final *circusv1.CallUpdate
	wait(t, 4*time.Second, func() bool {
		var err error
		final, err = c.Query(queryCtx, call.CallId)
		return err == nil && final.GetResult() != nil
	})
	if output(final) != "close-survived" {
		t.Fatal(final)
	}
	// More than four terminal calls on one connection prove terminal updates
	// release server operation slots, not just local client streams.
	for i := range 6 {
		id := "slot-" + strconv.Itoa(i)
		u := h.result(h.submit(c, h.call(id, "printf "+id)))
		if output(u) != id {
			t.Fatal(u)
		}
	}
	_ = c.Close()
	h.daemon.signal(t, syscall.SIGTERM)
	must(t, h.daemon.wait(t, 3*time.Second))
	h.offline()
	raw := h.rawWorker()
	accepted := h.call("cancel-ordered", "printf should-not-run")
	h.sendWorker(raw, &circusv1.WorkerUplink{Payload: &circusv1.WorkerUplink_SubmitCall{SubmitCall: &circusv1.SubmitCall{Device: "desk", Call: accepted}}})
	h.sendWorker(raw, &circusv1.WorkerUplink{Payload: &circusv1.WorkerUplink_CancelCall{CancelCall: &circusv1.CancelCall{CallId: accepted.CallId}}})
	cancelCtx, cancelDone := context.WithTimeout(t.Context(), testwait.Timeout)
	defer cancelDone()
	for {
		msg, err := raw.Receive(cancelCtx)
		must(t, err)
		u := msg.(*circusv1.WorkerDownlink).GetCallUpdate()
		if u.GetCancelled() != nil {
			break
		}
		if u.GetRejected() != nil {
			t.Fatalf("Cancel overtook Submit: %v", u)
		}
	}
}

// trackTool records the exact shell PID produced by our own tool command. The
// exec sleep in F4 keeps that PID, so no unrecorded sleep process survives.
func (h *harness) trackTool(pidfile string) int {
	h.t.Helper()
	var pid int
	wait(h.t, 3*time.Second, func() bool {
		data, err := os.ReadFile(pidfile)
		if err != nil {
			return false
		}
		pid, err = strconv.Atoi(strings.TrimSpace(string(data)))
		return err == nil && pid > 1
	})
	h.t.Cleanup(func() {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		// F4's daemon is already reaped, so its tool is now our adopted child.
		wait(h.t, 2*time.Second, func() bool {
			_, _ = syscall.Wait4(pid, nil, syscall.WNOHANG, nil)
			return syscall.Kill(pid, 0) == syscall.ESRCH
		})
	})
	return pid
}
func TestDaemonCrashF4(t *testing.T) {
	h := newHarness(t, "1s")
	c := h.client()
	marker := filepath.Join(h.dir, "marker")
	pidfile := filepath.Join(h.dir, "tool.pid")
	call := h.call("f4", "printf 'run\n' >> "+shellQuote(marker)+"; printf '%s' $$ > "+shellQuote(pidfile)+"; exec sleep 20")
	s := h.submit(c, call)
	h.trackTool(pidfile)
	wait(t, 3*time.Second, func() bool { return markerLines(t, marker) == 1 })
	h.daemon.signal(t, syscall.SIGKILL)
	if h.daemon.wait(t, 2*time.Second) == nil {
		t.Fatal("SIGKILL succeeded")
	}
	h.state(call.CallId, store.CallUnknown)
	ctx, cancel := context.WithTimeout(t.Context(), testwait.Timeout)
	defer cancel()
	for {
		u, err := s.Next(ctx)
		must(t, err)
		if u.GetUnknown() != nil {
			break
		}
		if wire.Terminal(u) {
			t.Fatal(u)
		}
	}
	h.run("", 3, "call", "query", call.CallId)
	waiting := h.start(nil, "", "call", "--device", "desk", "--call-id", call.CallId, "--session", call.SessionId, "--cwd", h.dir, "--wait", "bash", call.GetBash().Command)
	wait(t, 2*time.Second, func() bool { return strings.Contains(waiting.stderr.String(), `"unknown"`) })
	if waiting.exited() {
		t.Fatal("--wait exited at Unknown")
	}
	h.startDaemon()
	u := h.result(s)
	if u.GetLost() == nil {
		t.Fatal(u)
	}
	again := h.result(h.submit(c, call))
	if again.GetLost() == nil {
		t.Fatal(again)
	}
	if code := h.waitCode(waiting, 3*time.Second); code != 1 {
		t.Fatalf("--wait terminal loss exit %d, want 1: %s", code, waiting.stderr.String())
	}
	if markerLines(t, marker) != 1 {
		t.Fatal("call executed twice")
	}
}
func TestServerCrashAcceptedF6(t *testing.T) {
	h := newHarness(t)
	h.daemon.signal(t, syscall.SIGTERM)
	must(t, h.daemon.wait(t, 3*time.Second))
	h.offline()
	c := h.client()
	marker := filepath.Join(h.dir, "marker")
	call := h.call("f6accepted", "printf 'run\n' >> "+shellQuote(marker)+"; printf accepted-recovered")
	s := h.submit(c, call)
	ctx, cancel := context.WithTimeout(t.Context(), testwait.Timeout)
	defer cancel()
	u, err := s.Next(ctx)
	must(t, err)
	if u.GetAccepted() == nil {
		t.Fatal(u)
	}
	if markerLines(t, marker) != 0 {
		t.Fatal("offline accepted call executed")
	}
	h.server.signal(t, syscall.SIGKILL)
	_ = h.server.wait(t, 2*time.Second)
	_ = c.Close()
	h.startServer(false)
	row := h.state(call.CallId, store.CallLost)
	if row.DispatchedBootID != "" {
		t.Fatal(row)
	}
	if h.query(call.CallId).GetLost() == nil {
		t.Fatal("not lost")
	}
	c = h.client()
	s = h.submit(c, call)
	h.state(call.CallId, store.CallAccepted)
	h.startDaemon()
	u = h.result(s)
	if output(u) != "accepted-recovered" || markerLines(t, marker) != 1 {
		t.Fatal(u)
	}
}
func TestServerCrashRunningF6(t *testing.T) {
	h := newHarness(t)
	marker := filepath.Join(h.dir, "marker")
	gate := filepath.Join(h.dir, "gate")
	must(t, syscall.Mkfifo(gate, 0600))
	f, err := os.OpenFile(gate, os.O_RDWR, 0600)
	must(t, err)
	defer f.Close()
	c := h.client()
	call := h.call("f6running", "printf 'run\n' >> "+shellQuote(marker)+"; read -r release < "+shellQuote(gate)+"; printf real-recovered-output")
	h.submit(c, call)
	wait(t, 3*time.Second, func() bool { return markerLines(t, marker) == 1 })
	h.state(call.CallId, store.CallRunning)
	h.server.signal(t, syscall.SIGKILL)
	_ = h.server.wait(t, 2*time.Second)
	_ = c.Close()
	_, err = io.WriteString(f, "release\n")
	must(t, err)
	h.startServer(false)
	h.state(call.CallId, store.CallCompleted)
	u := h.query(call.CallId)
	if output(u) != "real-recovered-output" {
		t.Fatal(u)
	}
	// An equal resubmit is a per-call ordered barrier through the same daemon:
	// it follows earlier traffic for this call ID and must deduplicate to the
	// recovered result without another execution before the marker is counted.
	barrier := h.result(h.submit(h.client(), call))
	if output(barrier) != "real-recovered-output" || markerLines(t, marker) != 1 {
		t.Fatalf("barrier=%v marker lines=%d", barrier, markerLines(t, marker))
	}
}
func TestDispatchCommitCrashF7(t *testing.T) {
	h := newHarness(t)
	h.server.signal(t, syscall.SIGTERM)
	must(t, h.server.wait(t, 5*time.Second))
	h.startServer(true)
	// A harmless CallQuery proves the same daemon reconnected without triggering
	// the DispatchCall-only fault point.
	wait(t, 4*time.Second, func() bool {
		ids, err := h.st.ListIdentities(t.Context(), store.KindDevice, h.user())
		return err == nil && len(ids) == 1 && ids[0].LastConnectedAt > ids[0].LastDisconnectedAt
	})
	c := h.client()
	marker := filepath.Join(h.dir, "marker")
	call := h.call("f7", "printf 'run\n' >> "+shellQuote(marker))
	h.submit(c, call)
	if h.server.wait(t, 4*time.Second) == nil {
		t.Fatal("fault did not kill server")
	}
	row, err := h.st.GetCall(t.Context(), call.CallId)
	must(t, err)
	if row.State != store.CallDispatched || row.DispatchedBootID == "" {
		t.Fatal(row)
	}
	_ = c.Close()
	h.startServer(false)
	h.state(call.CallId, store.CallLost)
	if h.query(call.CallId).GetLost() == nil {
		t.Fatal("not lost")
	}
	if h.result(h.submit(h.client(), call)).GetLost() == nil {
		t.Fatal("dispatched ID retried")
	}
	if markerLines(t, marker) != 0 {
		t.Fatal("tool executed past fault point")
	}
}
func TestRevocationF17(t *testing.T) {
	h := newHarness(t, "30s")
	c := h.client()
	marker := filepath.Join(h.dir, "marker")
	pidfile := filepath.Join(h.dir, "tool.pid")
	active := h.call("active-at-revoke", "printf 'run\n' >> "+shellQuote(marker)+"; printf '%s' $$ > "+shellQuote(pidfile)+"; exec sleep 20")
	h.submit(c, active)
	h.trackTool(pidfile)
	wait(t, 3*time.Second, func() bool { return markerLines(t, marker) == 1 })
	h.run("", 0, "server", "device", "revoke", "desk", "--db", h.db)
	// Access logging happens only after ServeHTTP released the live link.
	wait(t, 6*time.Second, func() bool {
		for line := range strings.Lines(h.server.stderr.String()) {
			var rec struct {
				Msg, Path string
				Status    int
			}
			if json.Unmarshal([]byte(line), &rec) == nil && rec.Msg == "http request" && rec.Path == "/v1/link/daemon" && rec.Status == 101 {
				return true
			}
		}
		return false
	})
	if h.daemon.wait(t, 7*time.Second) == nil {
		t.Fatal("revoked daemon exited zero")
	}
	// Its real reconnect received 403; a completed process cannot reconnect again.
	if !strings.Contains(h.daemon.stderr.String(), "revoked") && !strings.Contains(h.daemon.stderr.String(), "403") {
		t.Fatal(h.daemon.stderr.String())
	}
	f, err := credfile.Load(filepath.Join(h.dir, "config/circus/daemon.json"))
	must(t, err)
	req, err := http.NewRequest("GET", h.base+"/v1/link/daemon", nil)
	must(t, err)
	req.Header.Set("Authorization", "Bearer "+f.Token)
	client := &http.Client{Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()
	res, err := client.Do(req)
	must(t, err)
	_ = res.Body.Close()
	if res.StatusCode != 403 {
		t.Fatal(res.Status)
	}
	select {
	case <-c.Done():
		t.Fatal("worker closed before revocation")
	default:
	}
	// The subscription started before either revocation is still live here.
	h.run("", 0, "server", "worker", "revoke", "desk", "--db", h.db)
	// An active client notices revocation through its joined reader.
	ctx, cancel := context.WithTimeout(t.Context(), testwait.Timeout)
	defer cancel()
	select {
	case <-c.Done():
		if c.Err() == nil {
			t.Fatal("worker closed without error")
		}
	case <-ctx.Done():
		t.Fatal("worker revocation did not close idle link")
	}
	_, err = workerclient.Dial(ctx, workerclient.Options{Credentials: h.worker})
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatal(err)
	}
}
func TestGracefulShutdown(t *testing.T) {
	h := newHarness(t)
	c := h.client()
	h.server.signal(t, syscall.SIGTERM)
	must(t, h.server.wait(t, 5*time.Second))
	ids, err := h.st.ListIdentities(t.Context(), store.KindDevice, h.user())
	must(t, err)
	if len(ids) != 1 || ids[0].LastConnectedAt == 0 || ids[0].LastDisconnectedAt < ids[0].LastConnectedAt {
		t.Fatalf("graceful disconnect was not recorded before router close: %+v", ids)
	}
	graceTimers, err := h.st.ListTimers(t.Context(), store.TimerFilter{Kind: "device_grace", SubjectID: ids[0].ID, Limit: 1})
	must(t, err)
	if len(graceTimers) != 1 {
		t.Fatalf("device_grace timer missing after shutdown: %+v", graceTimers)
	}
	if h.daemon.exited() {
		t.Fatal("daemon exited during server restart")
	}
	select {
	case <-c.Done():
	case <-time.After(testwait.Timeout):
		t.Fatal("worker survived server shutdown")
	}
	_ = c.Close()
	h.startServer(false)
	h.readyDaemon()
}
