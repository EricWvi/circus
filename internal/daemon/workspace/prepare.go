package workspace

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/clock"
	"github.com/EricWvi/circus/internal/daemon/dirlist"
	"github.com/EricWvi/circus/internal/daemon/jobs"
	"github.com/EricWvi/circus/internal/daemon/toolcontext"
	"github.com/EricWvi/circus/internal/gitenv"
	"google.golang.org/protobuf/proto"
)

const (
	maxURLBytes = 2048
	// maxDetailBytes bounds error_detail.
	maxDetailBytes = 1024
	// maxDetailLines bounds the stderr lines kept in error_detail.
	maxDetailLines = 16
	// BackgroundLifetime is how long a clone may run after it moved to the
	// background.
	BackgroundLifetime = 2 * time.Hour
	// ResultRetention is how long a finished background preparation's
	// outcome stays available to resume_job_id.
	ResultRetention = 30 * time.Minute
	// MaxPreparations bounds concurrent preparations, foreground and
	// background together.
	MaxPreparations = 2
	// maxRetained bounds retained background outcomes.
	maxRetained = 64
	// cleanupLimit bounds the removal of a stopped clone's partial directory.
	cleanupLimit = 10 * time.Minute
	// killedExitCode is the job exit code of a stopped or expired clone.
	killedExitCode = 143
)

type (
	workspaceError = circusv1.WorkspaceError
	prepareResult  = circusv1.PrepareWorkspaceResult
)

const (
	errInvalidRequest   = circusv1.WorkspaceError_WORKSPACE_ERROR_INVALID_REQUEST
	errHomeUnknown      = circusv1.WorkspaceError_WORKSPACE_ERROR_HOME_UNKNOWN
	errUnsupportedOS    = circusv1.WorkspaceError_WORKSPACE_ERROR_UNSUPPORTED_OS
	errPathConflict     = circusv1.WorkspaceError_WORKSPACE_ERROR_PATH_CONFLICT
	errNotEmpty         = circusv1.WorkspaceError_WORKSPACE_ERROR_NOT_EMPTY
	errRemoteMismatch   = circusv1.WorkspaceError_WORKSPACE_ERROR_REMOTE_MISMATCH
	errGitMissing       = circusv1.WorkspaceError_WORKSPACE_ERROR_GIT_UNAVAILABLE
	errCloneFailed      = circusv1.WorkspaceError_WORKSPACE_ERROR_CLONE_FAILED
	errNoSpace          = circusv1.WorkspaceError_WORKSPACE_ERROR_NO_SPACE
	errPermissionDenied = circusv1.WorkspaceError_WORKSPACE_ERROR_PERMISSION_DENIED
	errTimeout          = circusv1.WorkspaceError_WORKSPACE_ERROR_TIMEOUT
	errBusyCode         = circusv1.WorkspaceError_WORKSPACE_ERROR_BUSY
	errCancelled        = circusv1.WorkspaceError_WORKSPACE_ERROR_CANCELLED
	errIO               = circusv1.WorkspaceError_WORKSPACE_ERROR_IO
	errRefused          = circusv1.WorkspaceError_WORKSPACE_ERROR_REFUSED
)

// Environment supplies the variables git runs with: the Bash tool's
// captured login-shell exports.
type Environment interface {
	Environment(context.Context) ([]string, error)
	// PreparationBudget bounds collecting the environment.
	PreparationBudget() time.Duration
}

// Options configures the workspace tools. Zero values use the host.
type Options struct {
	// Jobs receives clones that outlive their call. Required for Prepare.
	Jobs *jobs.Manager
	// Environment is required for Prepare.
	Environment Environment
	// Home overrides the daemon user's home directory.
	Home string
	// Git overrides the git executable: a path, or a name looked up on the
	// environment's PATH.
	Git string
	// Clock times the background lifetime and result retention.
	Clock clock.Clock
	// Logger receives diagnostics; it never sees credentials.
	Logger *slog.Logger
}

func (o *Options) defaults() {
	if o.Clock == nil {
		o.Clock = clock.Real{}
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
}

func (o *Options) lookupHome() (string, error) {
	if o.Home != "" {
		return o.Home, nil
	}
	return os.UserHomeDir()
}

// Prepare is the prepare_workspace tool.
type Prepare struct {
	opts        Options
	slots       chan struct{}
	cleanupCtx  context.Context
	stopCleanup context.CancelFunc

	mu     sync.Mutex
	jobs   map[string]*backgroundJob
	closed bool
	wg     sync.WaitGroup
}

// NewPrepare constructs the prepare_workspace tool.
func NewPrepare(opts Options) *Prepare {
	opts.defaults()
	ctx, cancel := context.WithCancel(context.Background())
	return &Prepare{opts: opts, slots: make(chan struct{}, MaxPreparations), cleanupCtx: ctx, stopCleanup: cancel, jobs: make(map[string]*backgroundJob)}
}

// Name returns the device call tool name.
func (*Prepare) Name() string { return "prepare_workspace" }

// PreparationBudget covers collecting the login-shell environment.
func (p *Prepare) PreparationBudget() time.Duration {
	if p.opts.Environment == nil {
		return 0
	}
	return p.opts.Environment.PreparationBudget()
}

// Run prepares the workspace; see PrepareWorkspaceRequest.
func (p *Prepare) Run(ctx context.Context, call *circusv1.DeviceCall) (*circusv1.DeviceResult, error) {
	req := call.GetPrepareWorkspace()
	if req == nil {
		return nil, errors.New("prepare_workspace request is required")
	}
	if p.opts.Environment == nil || p.opts.Jobs == nil {
		return nil, errors.New("prepare_workspace needs an environment and a job manager")
	}
	result := p.run(ctx, call, req)
	if result.Outcome == circusv1.PrepareWorkspaceResult_OUTCOME_FAILED {
		p.opts.Logger.Info("workspace preparation failed", "session_id", call.SessionId, "call_id", call.CallId, "path", result.Path, "error", result.Error.String())
	}
	return deviceResult(result), nil
}

func (p *Prepare) run(ctx context.Context, call *circusv1.DeviceCall, req *circusv1.PrepareWorkspaceRequest) *prepareResult {
	if runtime.GOOS == "windows" {
		return failed("", errUnsupportedOS, "")
	}
	path, _, code := resolvePath(req.Path, p.opts.lookupHome)
	if code != 0 {
		return failed("", code, "")
	}
	if !validURL(req.RepoUrl) {
		return failed(path, errInvalidRequest, "")
	}
	if req.ResumeJobId != "" {
		if result := p.resume(ctx, call, req.ResumeJobId, path); result != nil {
			return result
		}
	}
	select {
	case p.slots <- struct{}{}:
	default:
		return failed(path, errBusyCode, "")
	}
	a := &attempt{p: p, path: path, repoURL: req.RepoUrl, slot: true}
	if req.RepoUrl != "" {
		env, err := p.opts.Environment.Environment(ctx)
		if err != nil {
			a.release()
			if ctx.Err() != nil {
				return failed(path, errCancelled, "")
			}
			p.opts.Logger.Warn("workspace preparation could not collect the environment", "error", err)
			return failed(path, errIO, "")
		}
		a.env = gitEnvironment(env)
	}
	toolcontext.StartTimeout(ctx)
	result, background := a.run(ctx, call)
	if !background {
		a.release()
	}
	return result
}

// resume answers for a background job of an earlier attempt, or returns
// nil when this daemon does not know it.
func (p *Prepare) resume(ctx context.Context, call *circusv1.DeviceCall, id, path string) *prepareResult {
	p.mu.Lock()
	p.expireLocked()
	j := p.jobs[id]
	p.mu.Unlock()
	if j == nil || j.sessionID != call.SessionId || j.path != path {
		return nil
	}
	toolcontext.StartTimeout(ctx)
	select {
	case <-j.done:
		return proto.Clone(j.result).(*prepareResult)
	case <-ctx.Done():
	}
	select {
	case <-j.done:
		return proto.Clone(j.result).(*prepareResult)
	default:
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return inBackground(path, id)
	}
	j.Stop()
	<-j.done
	return proto.Clone(j.result).(*prepareResult)
}

// expireLocked drops outcomes older than ResultRetention, and the oldest
// ones beyond maxRetained.
func (p *Prepare) expireLocked() {
	now := p.opts.Clock.Now()
	var oldest *backgroundJob
	ended := 0
	for id, j := range p.jobs {
		if j.result == nil {
			continue
		}
		if now.Sub(j.ended) >= ResultRetention {
			delete(p.jobs, id)
			continue
		}
		ended++
		if oldest == nil || j.ended.Before(oldest.ended) {
			oldest = j
		}
	}
	if ended > maxRetained && oldest != nil {
		delete(p.jobs, oldest.id)
	}
}

// Shutdown stops background clones and waits for them and for partial
// directory cleanup. It can be waited on again if ctx expires.
func (p *Prepare) Shutdown(ctx context.Context) error {
	p.mu.Lock()
	p.closed = true
	var running []*backgroundJob
	for _, j := range p.jobs {
		if j.result == nil {
			running = append(running, j)
		}
	}
	p.mu.Unlock()
	for _, j := range running {
		j.Stop()
	}
	p.stopCleanup()
	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// attempt is one preparation holding a slot and, once taken, its locks.
// Its owner is the call until a clone moves to the background, then the
// background job.
type attempt struct {
	p       *Prepare
	path    string // lexical path, reported in results
	repoURL string
	env     []string
	git     string
	slot    bool
	dirs    []*os.File
	locks   []*dirLock
	target  string   // where the workspace ends up (symlinks resolved)
	parent  *os.File // directory of target
	name    string   // base name of target
	partial string   // the hidden clone directory, while it may exist
	proc    *process
}

// run executes the steps after preparation. background reports that a
// background job now owns the attempt.
func (a *attempt) run(ctx context.Context, call *circusv1.DeviceCall) (*prepareResult, bool) {
	parentPath, name := filepath.Dir(a.path), filepath.Base(a.path)
	if err := os.MkdirAll(parentPath, 0o755); err != nil {
		return a.fsFailure("create parent", err), false
	}
	parent, err := a.openDir(parentPath)
	if err != nil {
		return a.fsFailure("open parent", err), false
	}
	if result := a.lock(ctx, parent, name); result != nil {
		return result, false
	}
	a.target, a.parent, a.name = a.path, parent, name

	info, err := os.Lstat(a.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if a.repoURL == "" {
			return a.create(), false
		}
		return a.clone(ctx, call)
	case err != nil:
		return a.fsFailure("inspect", err), false
	case info.Mode()&os.ModeSymlink != 0:
		real, err := filepath.EvalSymlinks(a.path)
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			return failed(a.path, errPathConflict, ""), false
		}
		if err != nil {
			return a.fsFailure("resolve symlink", err), false
		}
		if info, err = os.Stat(real); err != nil {
			return a.fsFailure("inspect", err), false
		}
		if !info.IsDir() {
			return failed(a.path, errPathConflict, ""), false
		}
		// Clone beside the real directory, under its own lock as well.
		realParent, err := a.openDir(filepath.Dir(real))
		if err != nil {
			return a.fsFailure("open parent", err), false
		}
		if result := a.lock(ctx, realParent, filepath.Base(real)); result != nil {
			return result, false
		}
		a.target, a.parent, a.name = real, realParent, filepath.Base(real)
	case !info.IsDir():
		return failed(a.path, errPathConflict, ""), false
	}

	empty, err := emptyDir(a.target)
	switch {
	case err != nil:
		return a.fsFailure("read", err), false
	case empty && a.repoURL == "":
		return ready(a.path, circusv1.PrepareWorkspaceResult_ACTION_USED_EMPTY), false
	case empty:
		return a.clone(ctx, call)
	case a.repoURL == "":
		return failed(a.path, errNotEmpty, ""), false
	}
	return a.inspectRepository(ctx), false
}

func (a *attempt) openDir(path string) (*os.File, error) {
	dir, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	if info, err := dir.Stat(); err != nil || !info.IsDir() {
		_ = dir.Close()
		if err == nil {
			err = syscall.ENOTDIR
		}
		return nil, err
	}
	a.dirs = append(a.dirs, dir)
	return dir, nil
}

// lock takes the lock beside name in dir and removes leftovers of earlier
// attempts there.
func (a *attempt) lock(ctx context.Context, dir *os.File, name string) *prepareResult {
	l, err := lockIn(dir, name)
	if errors.Is(err, errBusy) {
		return failed(a.path, errBusyCode, "")
	}
	if err != nil {
		return a.fsFailure("lock", err)
	}
	a.locks = append(a.locks, l)
	if err := removeLeftovers(ctx, dir, name); err != nil {
		if errors.Is(err, context.Canceled) {
			return failed(a.path, errCancelled, "")
		}
		if errors.Is(err, context.DeadlineExceeded) {
			// Removal made progress; the next attempt continues it.
			a.p.opts.Logger.Warn("workspace leftovers were not removed in time", "path", a.path)
			return failed(a.path, errBusyCode, "")
		}
		return a.fsFailure("remove leftovers", err)
	}
	return nil
}

func (a *attempt) create() *prepareResult {
	err := os.Mkdir(a.path, 0o755)
	switch {
	case err == nil:
		return ready(a.path, circusv1.PrepareWorkspaceResult_ACTION_CREATED)
	case errors.Is(err, os.ErrExist):
		// Something appeared since the inspection; look again later.
		return failed(a.path, errBusyCode, "")
	}
	return a.fsFailure("create", err)
}

func emptyDir(path string) (bool, error) {
	dir, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer dir.Close()
	names, err := dir.Readdirnames(1)
	if errors.Is(err, io.EOF) {
		return true, nil
	}
	return len(names) == 0, err
}

func (a *attempt) gitExecutable() error {
	if a.git != "" {
		return nil
	}
	git, err := findGit(a.env, a.p.opts.Git)
	a.git = git
	return err
}

// git runs a short git command in dir and maps its failures.
func (a *attempt) runGit(ctx context.Context, dir string, args ...string) (gitResult, *prepareResult) {
	if err := a.gitExecutable(); err != nil {
		return gitResult{}, failed(a.path, errGitMissing, "")
	}
	result, err := runGit(ctx, a.git, a.env, dir, args...)
	switch {
	case err == nil:
		return result, nil
	case errors.Is(err, errGitUnavailable):
		return result, failed(a.path, errGitMissing, "")
	case errors.Is(err, context.Canceled):
		return result, failed(a.path, errCancelled, "")
	case errors.Is(err, context.DeadlineExceeded):
		a.p.opts.Logger.Warn("workspace git command timed out", "command", args[0], "path", a.path)
		return result, failed(a.path, errIO, "")
	}
	a.p.opts.Logger.Warn("workspace git command failed to start", "command", args[0], "path", a.path, "error", err)
	return result, failed(a.path, errIO, "")
}

// inspectRepository handles a non-empty target with a repository URL.
func (a *attempt) inspectRepository(ctx context.Context) *prepareResult {
	top, failure := a.runGit(ctx, a.target, "rev-parse", "--show-toplevel")
	if failure != nil {
		return failure
	}
	real, err := filepath.EvalSymlinks(a.target)
	if err != nil {
		return a.fsFailure("resolve", err)
	}
	if top.code != 0 || filepath.Clean(strings.TrimRight(string(top.stdout), "\r\n")) != real {
		return failed(a.path, errNotEmpty, "")
	}
	origin, failure := a.runGit(ctx, a.target, "ls-remote", "--get-url", "origin")
	if failure != nil {
		return failure
	}
	existing := strings.TrimSpace(string(origin.stdout))
	if origin.code != 0 || existing == "" || existing == "origin" {
		return failed(a.path, errRemoteMismatch, "")
	}
	wanted, failure := a.runGit(ctx, a.target, "ls-remote", "--get-url", a.repoURL)
	if failure != nil {
		return failure
	}
	expanded := strings.TrimSpace(string(wanted.stdout))
	if wanted.code != 0 || expanded == "" {
		expanded = a.repoURL
	}
	if sameRemote(existing, expanded) {
		return ready(a.path, circusv1.PrepareWorkspaceResult_ACTION_USED_REPOSITORY)
	}
	detail := ""
	if sanitized, ok := gitenv.SanitizeURL(existing); ok {
		detail = clipDetail(gitenv.RedactText(sanitized, a.repoURL))
	}
	return failed(a.path, errRemoteMismatch, detail)
}

// clone clones into a hidden sibling of the target and renames it onto the
// target once complete. A clone outliving the call's deadline moves to a
// background job.
func (a *attempt) clone(ctx context.Context, call *circusv1.DeviceCall) (*prepareResult, bool) {
	if err := a.gitExecutable(); err != nil {
		return failed(a.path, errGitMissing, ""), false
	}
	var suffix [partialSuffixLen / 2]byte
	_, _ = rand.Read(suffix[:])
	partial := filepath.Join(a.parent.Name(), partialPrefix(a.name)+hex.EncodeToString(suffix[:]))
	proc, err := startGit(a.git, a.env, a.parent.Name(), false, "clone", "--progress", "--", a.repoURL, partial)
	if err != nil {
		if errors.Is(err, errGitUnavailable) {
			return failed(a.path, errGitMissing, ""), false
		}
		return a.fsFailure("start git", err), false
	}
	a.partial, a.proc = partial, proc
	select {
	case <-proc.done:
		return a.finishClone(0), false
	case <-ctx.Done():
	}
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		proc.terminate()
		<-proc.done
		return a.finishClone(errCancelled), false
	}
	launched := false
	info, err := a.p.opts.Jobs.Start(call.SessionId, call.CallId, "git clone "+displayURL(a.repoURL), func(id string) (jobs.Job, string, error) {
		j, err := a.p.startBackground(a, id, call.SessionId)
		launched = err == nil
		return j, "", err
	})
	if err == nil {
		return inBackground(a.path, info.ID), true
	}
	if launched {
		// The manager stopped the job it could not register; the job owns
		// the attempt and reports CANCELLED.
		return failed(a.path, errCancelled, ""), true
	}
	proc.terminate()
	<-proc.done
	if errors.Is(err, jobs.ErrShutdown) || errors.Is(err, errClosed) {
		return a.finishClone(errCancelled), false
	}
	a.p.opts.Logger.Warn("workspace clone could not move to the background", "path", a.path, "error", err)
	return a.finishClone(errIO), false
}

// finishClone turns the ended clone into a result. stopped is the reason
// the clone was stopped, or zero; a clone that completed anyway counts.
func (a *attempt) finishClone(stopped workspaceError) *prepareResult {
	if a.proc.succeeded() {
		// syscall.Rename, not os.Rename: os.Rename refuses to replace any
		// existing directory, but replacing an empty one is the point.
		if err := syscall.Rename(a.partial, a.target); err != nil {
			switch {
			case errors.Is(err, syscall.ENOTEMPTY), errors.Is(err, syscall.EEXIST):
				return failed(a.path, errNotEmpty, "")
			case errors.Is(err, syscall.ENOTDIR):
				return failed(a.path, errPathConflict, "")
			}
			return a.fsFailure("rename clone", err)
		}
		a.partial = ""
		return ready(a.path, circusv1.PrepareWorkspaceResult_ACTION_CLONED)
	}
	if stopped != 0 {
		return failed(a.path, stopped, "")
	}
	stderr, dropped := a.proc.stderr.bytes()
	code := errCloneFailed
	if noSpace(stderr) {
		code = errNoSpace
	}
	detail := stderrDetail(stderr, dropped, a.repoURL)
	a.p.opts.Logger.Warn("workspace clone failed", "path", a.path, "exit_code", a.proc.exitCode(), "stderr", detail)
	return failed(a.path, code, detail)
}

// release removes the partial clone, if any, then the locks and the slot.
// A partial clone is removed in the background, holding the locks, so a
// stopped clone's call returns promptly.
func (a *attempt) release() {
	finish := func() {
		for i := len(a.locks) - 1; i >= 0; i-- {
			a.locks[i].release()
		}
		for _, dir := range a.dirs {
			_ = dir.Close()
		}
		if a.slot {
			<-a.p.slots
		}
	}
	if a.partial == "" {
		finish()
		return
	}
	if _, err := os.Lstat(a.partial); errors.Is(err, os.ErrNotExist) {
		finish()
		return
	}
	cleanup := func() {
		ctx, cancel := context.WithTimeout(a.p.cleanupCtx, cleanupLimit)
		defer cancel()
		if err := newRemoval(ctx, false).removeAt(int(a.parent.Fd()), filepath.Base(a.partial), 0); err != nil {
			a.p.opts.Logger.Warn("workspace partial clone was not removed", "path", a.path, "error", err)
		}
		finish()
	}
	a.p.mu.Lock()
	if a.p.closed {
		a.p.mu.Unlock()
		cleanup()
		return
	}
	a.p.wg.Add(1)
	a.p.mu.Unlock()
	go func() {
		defer a.p.wg.Done()
		cleanup()
	}()
}

func (a *attempt) fsFailure(step string, err error) *prepareResult {
	code := fsErrorCode(err)
	if code == errIO {
		a.p.opts.Logger.Warn("workspace preparation failed", "step", step, "path", a.path, "error", err)
	}
	return failed(a.path, code, "")
}

func fsErrorCode(err error) workspaceError {
	switch {
	case errors.Is(err, syscall.ENOSPC), errors.Is(err, syscall.EDQUOT):
		return errNoSpace
	case errors.Is(err, syscall.EACCES), errors.Is(err, syscall.EPERM):
		return errPermissionDenied
	case errors.Is(err, syscall.ENOTDIR), errors.Is(err, syscall.EEXIST), errors.Is(err, syscall.ELOOP):
		return errPathConflict
	}
	return errIO
}

var errClosed = errors.New("workspace tool is shut down")

// backgroundJob is a clone moved to the background, as a jobs.Job.
type backgroundJob struct {
	p                   *Prepare
	id, sessionID, path string
	stop                chan struct{}
	stopOnce            sync.Once
	done                chan struct{}
	// result and ended are set under p.mu before done closes.
	result *prepareResult
	ended  time.Time
	code   int
	err    error
}

func (p *Prepare) startBackground(a *attempt, id, sessionID string) (*backgroundJob, error) {
	j := &backgroundJob{p: p, id: id, sessionID: sessionID, path: a.path, stop: make(chan struct{}), done: make(chan struct{})}
	lifetime := p.opts.Clock.After(BackgroundLifetime)
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, errClosed
	}
	p.expireLocked()
	p.jobs[id] = j
	p.wg.Add(1)
	p.mu.Unlock()
	go j.run(a, lifetime)
	return j, nil
}

func (j *backgroundJob) run(a *attempt, lifetime <-chan time.Time) {
	defer j.p.wg.Done()
	var stopped workspaceError
	select {
	case <-a.proc.done:
	case <-j.stop:
		stopped = errCancelled
	case <-lifetime:
		stopped = errTimeout
	}
	if stopped != 0 {
		a.proc.terminate()
		<-a.proc.done
	}
	result := a.finishClone(stopped)
	a.release()
	switch {
	case result.Outcome == circusv1.PrepareWorkspaceResult_OUTCOME_READY:
		j.code = 0
	case stopped != 0 && result.Error == stopped:
		j.code, j.err = killedExitCode, fmt.Errorf("workspace clone stopped: %w", jobs.ErrKilled)
	default:
		j.code = 1
	}
	j.p.opts.Logger.Info("workspace background clone ended", "job_id", j.id, "path", j.path, "outcome", result.Outcome.String(), "error", result.Error.String())
	j.p.mu.Lock()
	j.result, j.ended = result, j.p.opts.Clock.Now()
	j.p.mu.Unlock()
	close(j.done)
}

// Wait implements jobs.Job.
func (j *backgroundJob) Wait() (int, error) {
	<-j.done
	return j.code, j.err
}

// Stop implements jobs.Job: it requests the stop and returns.
func (j *backgroundJob) Stop() error {
	j.stopOnce.Do(func() { close(j.stop) })
	return nil
}

// resolvePath resolves a request path like ListDirectoryQuery.path, with
// "" and "~" alone invalid. It returns the lexical path and the home.
func resolvePath(path string, lookupHome func() (string, error)) (string, string, workspaceError) {
	if path == "" || path == "~" {
		return "", "", errInvalidRequest
	}
	resolved := dirlist.ResolveWith(path, lookupHome)
	if resolved.Error != 0 {
		if strings.HasPrefix(path, "~/") && resolved.Home == "" && utf8.ValidString(path) && len(path) <= 4096 && !strings.ContainsRune(path, 0) {
			return "", "", errHomeUnknown
		}
		return "", "", errInvalidRequest
	}
	if resolved.Parent == "" {
		return "", "", errInvalidRequest
	}
	return resolved.Path, resolved.Home, 0
}

func validURL(raw string) bool {
	if raw == "" {
		return true
	}
	if len(raw) > maxURLBytes || !utf8.ValidString(raw) || strings.HasPrefix(raw, "-") || strings.ContainsFunc(raw, unicode.IsControl) {
		return false
	}
	_, ok := gitenv.SanitizeURL(raw)
	return ok
}

// displayURL is the repository URL as it may leave the device.
func displayURL(raw string) string {
	if sanitized, ok := gitenv.SanitizeURL(raw); ok {
		return gitenv.RedactText(sanitized)
	}
	return "[redacted]"
}

// gitEnvironment is env without repository-locating variables, tracing
// (which can print credentials and floods stderr) and anything that could
// open a graphical prompt, and with prompts, editors and translations
// turned off. An empty GIT_ASKPASS also stops git from falling back to
// core.askPass or SSH_ASKPASS; SSH_AUTH_SOCK stays, so ssh-agent works.
func gitEnvironment(env []string) []string {
	env = gitenv.WithoutRepository(env)
	out := make([]string, 0, len(env)+7)
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "GIT_TRACE") {
			continue
		}
		switch name {
		case "GIT_TERMINAL_PROMPT", "GIT_EDITOR", "LC_ALL", "GCM_INTERACTIVE", "GIT_CURL_VERBOSE", "GIT_REDACT_COOKIES",
			"GIT_ASKPASS", "SSH_ASKPASS", "SSH_ASKPASS_REQUIRE", "DISPLAY", "WAYLAND_DISPLAY":
			continue
		}
		out = append(out, entry)
	}
	return append(out, "GIT_TERMINAL_PROMPT=0", "GIT_EDITOR=true", "LC_ALL=C", "GCM_INTERACTIVE=never",
		"GIT_ASKPASS=", "SSH_ASKPASS_REQUIRE=never")
}

func noSpace(stderr []byte) bool {
	s := string(stderr)
	return strings.Contains(s, "No space left on device") || strings.Contains(s, "Disk quota exceeded")
}

// stderrDetail turns git's stderr into error_detail: credentials removed,
// progress lines folded to their last state, the last lines kept, at most
// maxDetailBytes of valid UTF-8. When the capture dropped its beginning,
// the first, possibly partial, line goes too: a URL cut in half could
// otherwise keep part of a password past redaction.
func stderrDetail(stderr []byte, dropped bool, repoURL string) string {
	text := string(stderr)
	if dropped {
		_, text, _ = strings.Cut(text, "\n")
	}
	text = gitenv.RedactText(strings.ToValidUTF8(text, "�"), repoURL)
	var lines []string
	for line := range strings.SplitSeq(text, "\n") {
		line = strings.TrimRight(line, "\r")
		if i := strings.LastIndexByte(line, '\r'); i >= 0 {
			line = line[i+1:]
		}
		if line = strings.TrimRightFunc(line, unicode.IsSpace); line != "" {
			lines = append(lines, line)
		}
	}
	lines = lines[max(0, len(lines)-maxDetailLines):]
	return clipDetail(strings.Join(lines, "\n"))
}

// clipDetail keeps the last maxDetailBytes of s on a rune boundary.
func clipDetail(s string) string {
	s = strings.ToValidUTF8(s, "�")
	if len(s) <= maxDetailBytes {
		return s
	}
	cut := len(s) - maxDetailBytes
	for cut < len(s) && !utf8.RuneStart(s[cut]) {
		cut++
	}
	return s[cut:]
}

func ready(path string, action circusv1.PrepareWorkspaceResult_Action) *prepareResult {
	return &prepareResult{Outcome: circusv1.PrepareWorkspaceResult_OUTCOME_READY, Path: path, Action: action}
}

func inBackground(path, jobID string) *prepareResult {
	return &prepareResult{Outcome: circusv1.PrepareWorkspaceResult_OUTCOME_IN_BACKGROUND, Path: path, JobId: jobID}
}

func failed(path string, code workspaceError, detail string) *prepareResult {
	return &prepareResult{Outcome: circusv1.PrepareWorkspaceResult_OUTCOME_FAILED, Path: path, Error: code, ErrorDetail: detail}
}

var actionText = map[circusv1.PrepareWorkspaceResult_Action]string{
	circusv1.PrepareWorkspaceResult_ACTION_CREATED:         "created an empty directory",
	circusv1.PrepareWorkspaceResult_ACTION_USED_EMPTY:      "using the existing empty directory",
	circusv1.PrepareWorkspaceResult_ACTION_CLONED:          "cloned the repository",
	circusv1.PrepareWorkspaceResult_ACTION_USED_REPOSITORY: "using the existing repository",
}

// errorText names a workspace error in output text, e.g. "not empty".
func errorText(code workspaceError) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimPrefix(code.String(), "WORKSPACE_ERROR_"), "_", " "))
}

func deviceResult(r *prepareResult) *circusv1.DeviceResult {
	var text string
	switch r.Outcome {
	case circusv1.PrepareWorkspaceResult_OUTCOME_READY:
		text = fmt.Sprintf("Workspace ready at %s (%s)", r.Path, actionText[r.Action])
	case circusv1.PrepareWorkspaceResult_OUTCOME_IN_BACKGROUND:
		text = fmt.Sprintf("Cloning into %s continues in the background as job %s", r.Path, r.JobId)
	default:
		where := r.Path
		if where == "" {
			where = "the workspace"
		}
		text = fmt.Sprintf("Couldn't prepare %s: %s", where, errorText(r.Error))
	}
	return &circusv1.DeviceResult{
		Output: &circusv1.ToolOutput{IsError: r.Outcome == circusv1.PrepareWorkspaceResult_OUTCOME_FAILED, Content: []*circusv1.ContentBlock{{Block: &circusv1.ContentBlock_Text{Text: text}}}},
		Detail: &circusv1.DeviceResult_PrepareWorkspace{PrepareWorkspace: r},
	}
}
