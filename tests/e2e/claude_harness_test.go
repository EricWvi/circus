//go:build e2e && linux

package e2e

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/gen/go/circus/v1/circusv1connect"
	"github.com/EricWvi/circus/internal/gitenv"
	"github.com/EricWvi/circus/internal/testutil/fakemodel"
	"github.com/EricWvi/circus/internal/testutil/npmstub"
	testwait "github.com/EricWvi/circus/internal/testutil/wait"
	"github.com/google/uuid"
)

// outsideDir is this run's directory for the projects of real Claude Code
// sessions: outside the user's home and this repository.
var outsideDir string

func within(parent, dir string) bool {
	if parent == "" {
		return false
	}
	rel, err := filepath.Rel(parent, dir)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

// outsideRoot makes this run's directory under <base or /tmp>/circus-e2e-<uid>,
// which must lie outside the user's home and the repository: Claude Code
// reads the CLAUDE.md (and .claude/CLAUDE.md) of every ancestor of its
// working directory, so a project under the home would read the user's
// own ~/.claude. Run directories are flock'd and swept like the main run
// root.
func outsideRoot(base string) (string, func(), error) {
	home, _ := os.UserHomeDir()
	if h, err := filepath.EvalSymlinks(home); err == nil {
		home = h
	}
	repo, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", nil, err
	}
	var parent string
	for _, dir := range []string{base, "/tmp"} {
		real, err := filepath.EvalSymlinks(dir)
		if err == nil && filepath.IsAbs(real) && !within(home, real) && !within(repo, real) {
			parent = real
			break
		}
	}
	if parent == "" {
		return "", nil, fmt.Errorf("no temporary directory outside the home %q and the repository %q", home, repo)
	}
	top := filepath.Join(parent, "circus-e2e-"+strconv.Itoa(os.Getuid()))
	if err := os.Mkdir(top, 0o700); err != nil && !os.IsExist(err) {
		return "", nil, err
	}
	info, err := os.Lstat(top)
	if err != nil {
		return "", nil, err
	}
	if st, ok := info.Sys().(*syscall.Stat_t); !info.IsDir() || !ok || int(st.Uid) != os.Getuid() || info.Mode().Perm()&0o077 != 0 {
		return "", nil, fmt.Errorf("%s must be a 0700 directory owned by this user", top)
	}
	startup, err := acquireFlock(filepath.Join(top, "startup.lock"))
	if err != nil {
		return "", nil, err
	}
	defer startup.Close()
	if err := sweepE2ERuns(top); err != nil {
		return "", nil, err
	}
	run, err := os.MkdirTemp(top, "e2e-run-"+strconv.Itoa(os.Getpid())+"-")
	if err != nil {
		return "", nil, err
	}
	lock, err := acquireFlock(filepath.Join(run, "run.lock"))
	if err != nil {
		_ = os.RemoveAll(run)
		return "", nil, err
	}
	return run, func() { _ = os.RemoveAll(run); _ = lock.Close() }, nil
}

var workerBuild struct {
	once sync.Once
	err  error
	log  string
}

// workerBuildInputs are what worker/dist (and the proto package it
// imports, gen/ts/dist) is built from.
var workerBuildInputs = []string{
	"worker/src", "worker/package.json", "worker/tsconfig.json",
	"gen/ts/src", "gen/ts/package.json", "gen/ts/tsconfig.json", "package-lock.json",
}

// workerSourceHash hashes the paths and contents of the build inputs.
func workerSourceHash() (string, error) {
	h := sha256.New()
	for _, input := range workerBuildInputs {
		err := filepath.WalkDir(filepath.Join(root, input), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			fmt.Fprintf(h, "%s\x00%d\x00", rel, len(data))
			h.Write(data)
			return nil
		})
		if err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// buildWorker builds worker/dist once per run, and only when its inputs
// changed since the last e2e build (a stamp under .tmp records their hash).
// The build and the check hold one lock, so concurrent e2e runs never
// rewrite worker/dist while another one starts a worker from it.
func buildWorker(t *testing.T) {
	t.Helper()
	workerBuild.once.Do(func() {
		lock, err := acquireFlock(filepath.Join(root, ".tmp", "e2e-worker-build.lock"))
		if err != nil {
			workerBuild.err = err
			return
		}
		defer lock.Close()
		stamp := filepath.Join(root, ".tmp", "e2e-worker-build.stamp")
		hash, err := workerSourceHash()
		if err != nil {
			workerBuild.err = err
			return
		}
		if old, err := os.ReadFile(stamp); err == nil && string(old) == hash {
			if _, err := os.Stat(filepath.Join(root, "worker/dist/main.js")); err == nil {
				return
			}
		}
		_ = os.Remove(stamp)
		defer func() {
			if workerBuild.err == nil {
				workerBuild.err = os.WriteFile(stamp, []byte(hash), 0o600)
			}
		}()
		cmd := exec.Command("npm", "-w", "@circus/worker", "run", "build")
		cmd.Dir = root
		cmd.Env = gitenv.WithoutRepository(os.Environ())
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
		p := &child{cmd: cmd, done: make(chan struct{})}
		cmd.Stdout, cmd.Stderr = &p.stdout, &p.stderr
		if workerBuild.err = startChild(p); workerBuild.err != nil {
			return
		}
		timer := time.NewTimer(3 * time.Minute)
		defer timer.Stop()
		select {
		case <-p.done:
			workerBuild.err = p.err
		case <-timer.C:
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-p.done
			workerBuild.err = fmt.Errorf("worker build timed out")
		}
		workerBuild.log = p.stdout.String() + p.stderr.String()
	})
	if workerBuild.err != nil {
		t.Fatalf("worker build: %v\n%s", workerBuild.err, workerBuild.log)
	}
}

// claudeEnv builds the worker's environment, which Claude Code and every
// Bash command the fake model scripts inherit, from an allowlist of the
// harness environment: no credential, proxy, agent socket or git variable
// of the caller gets through (the test may itself run inside Claude Code).
// The stack adds what it sets.
func claudeEnv(env []string) []string {
	keep := func(key string) bool {
		switch key {
		case "PATH", "LANG", "TZ", "USER", "LOGNAME", "SHELL", "TERM", "TMPDIR", "HOME",
			"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME":
			return true
		}
		return strings.HasPrefix(key, "LC_")
	}
	last := map[string]string{}
	var order []string
	for _, kv := range env {
		key, value, _ := strings.Cut(kv, "=")
		if !keep(key) {
			continue
		}
		if _, seen := last[key]; !seen {
			order = append(order, key)
		}
		last[key] = value
	}
	out := make([]string, 0, len(order)+2)
	for _, key := range order {
		out = append(out, key+"="+last[key])
	}
	// Everything the stack talks to is on loopback.
	return append(out, "NO_PROXY=127.0.0.1,localhost", "no_proxy=127.0.0.1,localhost")
}

// claudeStack is the harness plus a fake model, a project directory
// outside the home, and real worker processes running Claude Code against
// the fake.
type claudeStack struct {
	t       *testing.T
	h       *harness
	fake    *fakemodel.Server
	fakeLog lockedBuffer
	api     circusv1connect.SessionServiceClient
	project string
	// dir is the project directory: the sessions' cwd.
	dir string
	// home, config and runtime are the worker's HOME, CLAUDE_CONFIG_DIR and
	// XDG_RUNTIME_DIR.
	home, config, runtime string
	workerEnv             []string
	ctx                   context.Context
	// launcher starts the worker: empty for node worker/dist/main.js, else
	// a release package's circus-worker.
	launcher string
	// registry serves the Claude Code package the worker installs, and
	// machines is the Web user's MachineService.
	registry *npmstub.Server
	machines circusv1connect.MachineServiceClient

	mu sync.Mutex
	// unrouted are requests the fake did not route to the main conversation.
	unrouted []string
}

const fakeAPIKey = "sk-ant-api03-circus-e2e-fake-key-no-network"

// newClaudeStack starts the harness and a fake model answering the main
// conversation with main, for workers built from this repository.
func newClaudeStack(t *testing.T, opts fakemodel.Options, workerEnv ...string) *claudeStack {
	t.Helper()
	buildWorker(t)
	return newStack(t, "", opts, workerEnv...)
}

// claudePackage is the platform package the worker installs, and a
// verified copy of its tarball (from the npm cache, else downloaded once
// into .tmp), found once per run.
var claudePackage struct {
	once sync.Once
	pkg  npmstub.Package
	file string
	err  error
}

// claudeRegistry starts a local npm registry serving the Claude Code
// package of this platform.
func claudeRegistry(t *testing.T) *npmstub.Server {
	t.Helper()
	claudePackage.once.Do(func() {
		name, err := npmstub.PlatformName()
		if err != nil {
			claudePackage.err = err
			return
		}
		if claudePackage.pkg, err = npmstub.Lookup(filepath.Join(root, "package-lock.json"), name); err != nil {
			claudePackage.err = err
			return
		}
		claudePackage.file, claudePackage.err = npmstub.Tarball(claudePackage.pkg, filepath.Join(root, ".tmp", "npm-tarballs"))
	})
	if claudePackage.err != nil {
		t.Fatalf("Claude Code package: %v", claudePackage.err)
	}
	reg := npmstub.Start(claudePackage.pkg, claudePackage.file)
	t.Cleanup(reg.Close)
	return reg
}

// newStack is newClaudeStack with the worker started by launcher (empty:
// node worker/dist/main.js, which the caller has built).
func newStack(t *testing.T, launcher string, opts fakemodel.Options, workerEnv ...string) *claudeStack {
	t.Helper()
	s := &claudeStack{t: t, workerEnv: workerEnv, launcher: launcher}
	// Registered first, so it runs last: after the worker, the daemon and
	// the server are stopped, nothing the test started may live on.
	t.Cleanup(s.reapAll)
	s.registry = claudeRegistry(t)
	opts.Logf = func(format string, args ...any) {
		_, _ = fmt.Fprintf(&s.fakeLog, time.Now().Format("15:04:05.000 ")+format+"\n", args...)
	}
	// Strict: every request must be the main conversation's.
	opts.Side = func(r *fakemodel.Request) *fakemodel.Response {
		s.mu.Lock()
		s.unrouted = append(s.unrouted, fmt.Sprintf("#%d %s %s system %.200q last %.300q", r.Seq, r.Method, r.Path, r.System, r.Last().Text()))
		s.mu.Unlock()
		return &fakemodel.Response{Status: 400, ErrorType: "invalid_request_error", Error: "fake: request not routed to the test's script"}
	}
	var err error
	s.fake, err = fakemodel.Start(opts)
	must(t, err)
	t.Cleanup(func() {
		_ = s.fake.Close()
		s.mu.Lock()
		for _, u := range s.unrouted {
			t.Errorf("model request not routed to the main conversation: %s", u)
		}
		s.mu.Unlock()
		if t.Failed() {
			t.Logf("fake model requests:\n%s", s.fakeLog.String())
		}
	})
	s.h = newHarness(t)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Second)
	t.Cleanup(cancel)
	s.ctx = ctx
	s.dir, err = os.MkdirTemp(outsideDir, "project-")
	must(t, err)
	s.home = s.h.dir
	s.config = filepath.Join(s.h.dir, "claude-config")
	must(t, os.MkdirAll(s.config, 0o700))
	// Outside HOME (the worker keeps its CLI records there).
	s.runtime, err = os.MkdirTemp("", "xdg-runtime-")
	must(t, err)
	must(t, os.Chmod(s.runtime, 0o700))
	projects, sessions := browserClients(t, s.h)
	s.machines = browserMachines(t, s.h)
	res, err := projects.CreateProject(testwait.Ctx(t), connect.NewRequest(&pb.CreateProjectRequest{
		Name: "Claude", Locations: []*pb.Project_Location{{Device: "desk", Path: s.dir}},
	}))
	must(t, err)
	s.api, s.project = sessions, res.Msg.GetProject().GetProjectId()
	return s
}

// reapAll follows the harness's own process waits. Any remaining children
// were adopted by this subreaper and must be killed and reaped, including
// descendants that change sessions. The harness also checks recorded
// leaders in case their workers failed before removing the records.
func (s *claudeStack) reapAll() {
	if s.dir != "" {
		defer os.RemoveAll(s.dir)
	}
	if s.runtime != "" {
		defer os.RemoveAll(s.runtime)
	}
	defer s.reapAdopted()
	for _, r := range s.cliRecords() {
		if r.PID > 1 && r.PID != os.Getpid() && r.Start != "" && processAlive(r.PID, r.Start) {
			_ = syscall.Kill(r.PID, syscall.SIGKILL)
			s.t.Logf("killed recorded process %d of session %s", r.PID, r.SessionID)
		}
	}
}

func (s *claudeStack) reapAdopted() {
	deadline := time.Now().Add(testwait.Timeout)
	for time.Now().Before(deadline) {
		for _, pid := range childrenOf(os.Getpid()) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		for time.Now().Before(deadline) {
			pid, err := syscall.Wait4(-1, nil, syscall.WNOHANG, nil)
			switch {
			case errors.Is(err, syscall.EINTR):
				continue
			case errors.Is(err, syscall.ECHILD):
				return
			case err != nil:
				s.t.Errorf("reaping adopted processes: %v", err)
				return
			}
			if pid == 0 {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	s.t.Errorf("adopted processes remain after cleanup: %v", childrenOf(os.Getpid()))
}

func childrenOf(parent int) []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	ppid := strconv.Itoa(parent)
	var out []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		fields := processFields(pid)
		// fields[1] is field 4, the parent pid.
		if len(fields) > 1 && fields[1] == ppid {
			out = append(out, pid)
		}
	}
	return out
}

// startWorker starts a real worker process (node worker/dist/main.js, or
// the stack's launcher) and has Claude Code installed on it the way a user
// does: once the worker reported its agents, SetWorkerAgent unless the
// agent is installed and wanted already (a restarted worker keeps it), and
// a wait until the worker reports it installed (from the stack's local
// registry).
func (s *claudeStack) startWorker(extra ...string) *child {
	s.t.Helper()
	c := s.startWorkerOnly(extra...)
	if a := s.waitAgent(func(a *pb.WorkerAgent) bool { return a.GetStatus().GetState() != pb.AgentStatus_STATE_UNSPECIFIED }); !agentRuns(a) {
		start := time.Now()
		s.setAgent(true)
		a = s.waitAgent(agentRuns)
		s.t.Logf("Claude Code %s installed in %v", a.GetStatus().GetInstalledVersion(), time.Since(start).Round(time.Millisecond))
	}
	return c
}

// agentRuns reports whether a is installed at the worker's target and
// wanted: the worker may run its sessions.
func agentRuns(a *pb.WorkerAgent) bool {
	st := a.GetStatus()
	return a.GetWanted() && st.GetState() == pb.AgentStatus_STATE_INSTALLED && st.GetInstalledVersion() == st.GetTargetVersion()
}

// claudeAgent returns the worker's Claude Code as ListWorkers shows it, nil
// when it has none.
func (s *claudeStack) claudeAgent() *pb.WorkerAgent {
	s.t.Helper()
	res, err := s.machines.ListWorkers(s.ctx, connect.NewRequest(&pb.ListWorkersRequest{}))
	must(s.t, err)
	for _, w := range res.Msg.GetWorkers() {
		if w.GetWorkerId() != s.h.worker.PrincipalID {
			continue
		}
		for _, a := range w.GetAgents() {
			if a.GetStatus().GetAgent() == pb.AgentKind_AGENT_KIND_CLAUDE_CODE {
				return a
			}
		}
	}
	return nil
}

// waitAgent waits until the worker's Claude Code matches, and returns it.
func (s *claudeStack) waitAgent(match func(*pb.WorkerAgent) bool) *pb.WorkerAgent {
	s.t.Helper()
	var last *pb.WorkerAgent
	deadline := time.Now().Add(agentWait)
	for time.Now().Before(deadline) {
		if last = s.claudeAgent(); last != nil && match(last) {
			return last
		}
		time.Sleep(50 * time.Millisecond)
	}
	s.t.Fatalf("worker's Claude Code did not reach the expected state; last %v", last)
	return nil
}

// agentWait bounds an install (a local download, verification and unpack
// of the package, about 400 MB unpacked) and every other agent change.
const agentWait = 90 * time.Second

// setAgent installs or uninstalls the worker's Claude Code
// (MachineService.SetWorkerAgent).
func (s *claudeStack) setAgent(install bool) (*pb.Worker, error) {
	s.t.Helper()
	res, err := s.machines.SetWorkerAgent(s.ctx, connect.NewRequest(&pb.SetWorkerAgentRequest{WorkerId: s.h.worker.PrincipalID,
		Agent: pb.AgentKind_AGENT_KIND_CLAUDE_CODE, Install: install}))
	if err != nil {
		if install {
			s.t.Fatal(err)
		}
		return nil, err
	}
	return res.Msg.GetWorker(), nil
}

// startWorkerOnly starts the worker without waiting for its agents.
func (s *claudeStack) startWorkerOnly(extra ...string) *child {
	s.t.Helper()
	env := append(claudeEnv(s.h.env),
		"HOME="+s.home,
		"CIRCUS_HOME=",
		"CIRCUS_BIN="+binary,
		"CIRCUS_WORKER_LOG_LEVEL=debug",
		// Idle sessions keep their leases: room for every session of a test.
		"CIRCUS_WORKER_MAX_SESSIONS=8",
		"ANTHROPIC_BASE_URL="+s.fake.URL(),
		"ANTHROPIC_API_KEY="+fakeAPIKey,
		"CLAUDE_CONFIG_DIR="+s.config,
		"XDG_RUNTIME_DIR="+s.runtime,
		"DISABLE_TELEMETRY=1",
		"DISABLE_ERROR_REPORTING=1",
		"DISABLE_AUTOUPDATER=1",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
		"CIRCUS_NPM_REGISTRY="+s.registry.URL,
	)
	env = append(append(env, s.workerEnv...), extra...)
	cmd := exec.Command("node", filepath.Join(root, "worker/dist/main.js"))
	cmd.Dir = root
	if s.launcher != "" {
		cmd = exec.Command(s.launcher)
		cmd.Dir = filepath.Dir(s.launcher)
	}
	return s.h.startCommandEnv(cmd, env)
}

// open creates a session in the project with a permission mode ("" is auto).
func (s *claudeStack) open(mode string) *sessionBrowser {
	s.t.Helper()
	req := &pb.CreateSessionRequest{ClientRequestId: uuid.NewString(), ProjectId: s.project}
	if mode != "" {
		req.Config = &pb.SessionConfig{PermissionMode: mode}
	}
	res, err := s.api.CreateSession(s.ctx, connect.NewRequest(req))
	must(s.t, err)
	session := res.Msg.GetSession()
	if !session.GetConfig().GetLocal() || session.GetPinnedWorkerId() != s.h.worker.PrincipalID {
		s.t.Fatalf("session is not local to the worker: %v", session)
	}
	return &sessionBrowser{t: s.t, ctx: s.ctx, api: s.api, id: session.GetSessionId()}
}

// waitFake waits for a recorded request matching match.
func (s *claudeStack) waitFake(match func(*fakemodel.Request) bool) fakemodel.Request {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
	defer cancel()
	r, err := s.fake.Wait(ctx, match)
	if err != nil {
		s.t.Fatalf("no matching model request: %v", err)
	}
	return r
}

// transcriptEntry is one stored transcript entry of a session.
type transcriptEntry struct {
	Subpath string
	Seq     int64
	UUID    string
	JSON    string
	Epoch   int64
}

// transcript reads a session's stored transcript straight from the database.
func (s *claudeStack) transcript(sessionID string) []transcriptEntry {
	s.t.Helper()
	db, err := sql.Open("sqlite", "file:"+s.h.db+"?mode=ro&_pragma=busy_timeout(5000)")
	must(s.t, err)
	defer db.Close()
	rows, err := db.QueryContext(s.ctx, "SELECT subpath, seq, COALESCE(uuid, ''), entry, epoch FROM transcript_entries WHERE session_id = ? ORDER BY subpath, seq", sessionID)
	must(s.t, err)
	defer rows.Close()
	var out []transcriptEntry
	for rows.Next() {
		var e transcriptEntry
		var raw []byte
		must(s.t, rows.Scan(&e.Subpath, &e.Seq, &e.UUID, &raw, &e.Epoch))
		e.JSON = string(raw)
		out = append(out, e)
	}
	must(s.t, rows.Err())
	return out
}

// waitTranscript waits until an entry of the session's transcript matches.
func (s *claudeStack) waitTranscript(sessionID string, match func(transcriptEntry) bool) transcriptEntry {
	s.t.Helper()
	var found transcriptEntry
	wait(s.t, 20*time.Second, func() bool {
		for _, e := range s.transcript(sessionID) {
			if match(e) {
				found = e
				return true
			}
		}
		return false
	})
	return found
}

// checkTranscript fails on duplicated conversation entries in a session's
// main transcript: a user input (string content) or a tool result twice, or
// the same block of an API message twice.
func (s *claudeStack) checkTranscript(sessionID string) []transcriptEntry {
	s.t.Helper()
	entries := s.transcript(sessionID)
	seen := map[string]int64{}
	inputs, answers := 0, 0
	for _, e := range entries {
		if e.Subpath != "" {
			continue
		}
		var entry struct {
			Type    string
			Message struct {
				ID      string
				Content json.RawMessage
			}
		}
		if json.Unmarshal([]byte(e.JSON), &entry) != nil {
			continue
		}
		var keys []string
		switch entry.Type {
		case "user":
			var text string
			var blocks []struct {
				Type      string
				ToolUseID string `json:"tool_use_id"`
			}
			if json.Unmarshal(entry.Message.Content, &text) == nil {
				keys = append(keys, "input "+text)
			} else if json.Unmarshal(entry.Message.Content, &blocks) == nil {
				for _, b := range blocks {
					if b.Type == "tool_result" {
						keys = append(keys, "tool_result "+b.ToolUseID)
					}
				}
			}
		case "assistant":
			keys = append(keys, "assistant "+entry.Message.ID+" "+string(entry.Message.Content))
		}
		for _, k := range keys {
			if strings.HasPrefix(k, "input ") {
				inputs++
			} else if strings.HasPrefix(k, "assistant ") {
				answers++
			}
			if prev, dup := seen[k]; dup {
				s.t.Fatalf("transcript entry %d duplicates entry %d: %.500s", e.Seq, prev, e.JSON)
			}
			seen[k] = e.Seq
		}
	}
	if inputs == 0 || answers == 0 {
		s.t.Fatalf("transcript of %s has %d inputs and %d answers", sessionID, inputs, answers)
	}
	return entries
}

// cliRecord names the session leader (the exec guard when available).
type cliRecord struct {
	SessionID string `json:"sessionId"`
	PID       int    `json:"pid"`
	Start     string `json:"start"`
	Worker    struct {
		PID int `json:"pid"`
	} `json:"worker"`
	file string
}

// cliRecords lists the workers' records of Claude Code processes.
func (s *claudeStack) cliRecords() []cliRecord {
	s.t.Helper()
	if s.runtime == "" {
		return nil
	}
	dir := filepath.Join(s.runtime, "circus-worker-"+strconv.Itoa(os.Getuid()))
	names, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	must(s.t, err)
	var out []cliRecord
	for _, n := range names {
		if !strings.HasSuffix(n.Name(), ".cli.json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, n.Name()))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		must(s.t, err)
		var r cliRecord
		if json.Unmarshal(data, &r) == nil {
			r.file = n.Name()
			out = append(out, r)
		}
	}
	return out
}

// liveCLI returns the live session leader recorded by the worker.
func (s *claudeStack) liveCLI(sessionID string) cliRecord {
	s.t.Helper()
	var found cliRecord
	wait(s.t, 10*time.Second, func() bool {
		for _, r := range s.cliRecords() {
			if r.SessionID == sessionID && processAlive(r.PID, r.Start) {
				found = r
				return true
			}
		}
		return false
	})
	return found
}

// guardCommand finds the CLI as a direct child in the guard's process group.
func (s *claudeStack) guardCommand(guard cliRecord) (int, string) {
	s.t.Helper()
	var cli int
	var cliStart string
	for _, pid := range childrenOf(guard.PID) {
		fields := processFields(pid)
		if len(fields) > 19 && fields[0] != "Z" && fields[2] == strconv.Itoa(guard.PID) {
			if cli != 0 {
				s.t.Fatalf("multiple command processes under guard %d: %d, %d", guard.PID, cli, pid)
			}
			cli, cliStart = pid, fields[19]
		}
	}
	if cli == 0 {
		s.t.Fatalf("no CLI in the exec guard %d's process group", guard.PID)
	}
	return cli, cliStart
}

func requireReaped(t *testing.T, pid int, start string) {
	t.Helper()
	if fields := processFields(pid); len(fields) > 19 && fields[19] == start {
		t.Fatalf("process %d outlived the worker and its guard: %v", pid, fields)
	}
}

func processFields(pid int) []string {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return nil
	}
	text := string(data)
	return strings.Fields(text[strings.LastIndexByte(text, ')')+2:])
}

// processAlive: the process with this pid and /proc start time runs (a
// zombie does not count).
func processAlive(pid int, start string) bool {
	fields := processFields(pid)
	// fields[0] is field 3 (state); field 22 (starttime) is fields[19].
	return len(fields) > 19 && fields[0] != "Z" && fields[0] != "X" && (start == "" || fields[19] == start)
}

// reapExited waits until a process is gone, reaping it if it became this
// test process's child (the harness adopts the exec guard of a killed worker).
func reapExited(t *testing.T, pid int, start string, limit time.Duration) {
	t.Helper()
	wait(t, limit, func() bool {
		fields := processFields(pid)
		if len(fields) <= 19 || start != "" && fields[19] != start {
			return true
		}
		got, err := syscall.Wait4(pid, nil, syscall.WNOHANG, nil)
		if err != nil && !errors.Is(err, syscall.ECHILD) && !errors.Is(err, syscall.EINTR) {
			t.Fatalf("wait4 %d: %v", pid, err)
		}
		return got == pid
	})
}
