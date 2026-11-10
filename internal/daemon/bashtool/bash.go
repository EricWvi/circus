package bashtool

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/daemon/jobs"
	"github.com/EricWvi/circus/internal/daemon/toolcontext"
	"github.com/EricWvi/circus/internal/textutil"
	"google.golang.org/protobuf/proto"
)

// Bash executes native-compatible bash calls without retaining session cwd.
type Bash struct {
	jobs        *jobs.Manager
	environment *environment
	groups      groupTracker
	// ownsEnvironment lets Shutdown release the environment's instance. Only a
	// Bash with its own Options owns it; the process-wide one is shared.
	ownsEnvironment bool
}

// NewBash constructs a lazy Bash tool without collecting or touching startup
// files. Supplying Options isolates startup and storage for embedded callers.
func NewBash(manager *jobs.Manager, options ...Options) *Bash {
	if len(options) > 0 {
		return &Bash{jobs: manager, environment: &environment{options: options[0]}, ownsEnvironment: true}
	}
	return &Bash{jobs: manager, environment: processEnvironment}
}

// Warm explicitly starts background environment collection at daemon startup.
// Construction and DefaultTools never start it. Failed warm-up is shared with
// its waiting calls; a later independent call can retry.
func (b *Bash) Warm() {
	b.groups.mu.Lock()
	defer b.groups.mu.Unlock()
	if b.groups.closed {
		return
	}
	e := b.environment
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.warmDone != nil {
		return
	}
	e.warmDone = make(chan struct{})
	warmCtx, cancel := context.WithCancel(context.Background())
	e.warmCancel = cancel
	go func() {
		defer close(e.warmDone)
		defer cancel()
		snapshot, _, err := e.load(warmCtx)
		if err == nil {
			e.releaseSnapshot(snapshot)
		}
	}()
}

// Name returns the device call tool name.
func (*Bash) Name() string { return "bash" }

func result(body string, failed bool, detail *circusv1.BashResult) *circusv1.DeviceResult {
	return &circusv1.DeviceResult{
		Output: &circusv1.ToolOutput{IsError: failed, Content: []*circusv1.ContentBlock{{Block: &circusv1.ContentBlock_Text{Text: textutil.NodeUTF8([]byte(body))}}}},
		Detail: &circusv1.DeviceResult_Bash{Bash: detail},
	}
}

func component(s string) string {
	if s == "" {
		return "_"
	}
	if s != "." && s != ".." && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.')
	}) < 0 {
		return s
	}
	return base64.RawURLEncoding.EncodeToString([]byte(s))
}

func command(request *circusv1.BashRequest, snapshot, cwdFile string, env []string) *exec.Cmd {
	wrapper := "source " + quote(snapshot) + " 2>/dev/null || true && shopt -u extglob 2>/dev/null || true && { \\builtin unalias -- 'which'; \\builtin unset -f -- 'which'; } >/dev/null 2>&1 || true && { \\builtin unalias -- 'unsetenv'; \\builtin unset -f -- 'unsetenv'; } >/dev/null 2>&1 || true && eval " + quote(request.Command) + " < /dev/null && pwd -P >| " + quote(cwdFile)
	cmd := exec.Command("/bin/bash", "-c", wrapper)
	cmd.Dir, cmd.Env = request.Cwd, env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd
}

// Run executes a foreground command or registers an independent background job.
func (b *Bash) Run(ctx context.Context, call *circusv1.DeviceCall) (*circusv1.DeviceResult, error) {
	req := call.GetBash()
	if req == nil {
		return nil, errors.New("bash request is required")
	}
	// Inferred: the daemon requires cwd, although native always supplies one.
	if req.Cwd == "" {
		return nil, errors.New("cwd is required")
	}
	if !filepath.IsAbs(req.Cwd) {
		return nil, errors.New("cwd must be absolute")
	}
	// Work on a copy: a deleted session cwd falls back to the initial directory.
	req = proto.Clone(req).(*circusv1.BashRequest)
	if _, err := os.Stat(req.Cwd); os.IsNotExist(err) {
		req.Cwd = startFolder(req)
	}
	if info, err := os.Stat(req.Cwd); err != nil {
		return nil, err
	} else if !info.IsDir() {
		return nil, errors.New("cwd is not a directory")
	}
	if !filepath.IsAbs(req.Cwd) {
		return nil, errors.New("start folder must be absolute")
	}
	if blocked := sleepGuard(req.Command, req.RunInBackground); blocked != "" {
		return result(blocked, true, &circusv1.BashResult{Cwd: req.Cwd}), nil
	}
	started := time.Now()
	duration := time.Duration(0)
	if deadline, ok := ctx.Deadline(); ok {
		duration = deadline.Sub(started)
	}
	snapshot, env, err := b.environment.load(ctx)
	if err == nil && ctx.Err() != nil {
		b.environment.releaseSnapshot(snapshot)
		err = ctx.Err()
	}
	if err != nil {
		detail := &circusv1.BashResult{Cwd: startFolder(req), ExitCode: 143}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			detail.TimedOut = true
			return result("Exit code 143\nCommand timed out after "+timeoutText(duration), true, detail), nil
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			detail.Interrupted = true
			return result("Command was interrupted", true, detail), nil
		}
		return nil, err
	}
	defer b.environment.releaseSnapshot(snapshot)
	// The dispatcher arms its command budget only after environment collection.
	toolcontext.StartTimeout(ctx)
	started = time.Now()
	if deadline, ok := ctx.Deadline(); ok {
		duration = deadline.Sub(started)
	}
	cwdFile, err := os.CreateTemp(filepath.Dir(snapshot), "cwd-")
	if err != nil {
		return nil, err
	}
	cwdPath := cwdFile.Name()
	_ = cwdFile.Close()
	if req.RunInBackground {
		return b.background(call, req, snapshot, cwdPath, env)
	}
	owned := false
	keepOutput := false
	defer func() {
		if !owned {
			_ = os.Remove(cwdPath)
		}
	}()
	out, err := b.outputFile(call.SessionId, "tool-results", component(call.CallId)+"-*.txt")
	if err != nil {
		return nil, err
	}
	defer func() {
		if !owned {
			_ = out.Close()
			if !keepOutput {
				_ = os.Remove(out.Name())
			}
			b.environment.releaseOutput(out.Name())
		}
	}()
	cmd := command(req, snapshot, cwdPath, env)
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		_ = os.Remove(out.Name())
		return nil, err
	}
	b.environment.outputBudget.start(out)
	// Native waits only for the shell; surviving group members are managed below.
	waited := startWait(cmd)
	var outcome processOutcome
	tick := time.NewTicker(outputPollInterval)
	defer tick.Stop()
	capMessage := ""
waitForeground:
	for {
		select {
		case <-tick.C:
			capMessage = b.outputStopMessage(out)
			if capMessage == "" {
				continue
			}
			outcome = killProcess(cmd, waited)
			waitGroupEmpty(cmd.Process.Pid)
			break waitForeground
		case outcome = <-waited:
			break waitForeground
		case <-ctx.Done():
			select {
			case outcome = <-waited:
			default:
				if errors.Is(ctx.Err(), context.DeadlineExceeded) && !killOnTimeout(req.Command) {
					if b.jobs == nil {
						_ = stopProcess(cmd, waited)
						return nil, errors.New("background job manager is unavailable")
					}
					// Inferred: allow 100 ms for output written at the deadline to drain
					// before taking the bounded promotion preview.
					time.Sleep(100 * time.Millisecond)
					detail := &circusv1.BashResult{Cwd: req.Cwd}
					body, formatErr := formatOutput(out, detail, false)
					if formatErr != nil {
						_ = stopProcess(cmd, waited)
						return nil, formatErr
					}
					// Inferred: promotion starts a fresh remaining background lifetime,
					// default 30 minutes, capped at two hours (independent of foreground timeout).
					stat, statErr := out.Stat()
					if statErr != nil {
						_ = stopProcess(cmd, waited)
						return nil, statErr
					}
					info, transferred, startErr := b.adopt(call, req, cmd, waited, out, cwdPath, backgroundLifetime(0))
					owned = transferred
					if startErr != nil {
						_ = os.Remove(out.Name())
						return nil, startErr
					}
					owned = true
					detail.JobId, detail.OutputPath = info.ID, info.OutputPath
					message := promotionMessage(duration, info.ID, info.OutputPath, stat.Size() == 0)
					if strings.HasPrefix(strings.TrimSpace(req.Command), "cd ") {
						message = appendLine(message, "Session cwd remains "+req.Cwd+"; directory changes made by the backgrounded command do not apply to subsequent commands.")
					}
					return result(appendLine(body, message), false, detail), nil
				}
				outcome = stopProcess(cmd, waited)
				outcome.code, outcome.err = 143, ctx.Err()
			}
			break waitForeground
		}
	}
	groupIsAlive := groupAlive(cmd.Process.Pid)
	capMessage = outputMessageAfterExit(capMessage, outcome.terminationRequested, groupIsAlive)
	if capMessage != "" || !groupIsAlive {
		b.environment.outputBudget.finish(out.Name())
	}
	if capMessage == "" {
		capMessage = outputMessageAfterExit(b.outputStopMessage(out), outcome.terminationRequested, groupIsAlive)
		if capMessage != "" {
			killGroup(cmd.Process.Pid)
			waitGroupEmpty(cmd.Process.Pid)
			b.environment.outputBudget.finish(out.Name())
		}
	}
	if capMessage != "" {
		if outcome.code == 0 {
			outcome.code = 137
		}
		if err := appendOutputLimit(out, capMessage); err != nil {
			return nil, err
		}
	}
	leftovers := capMessage == "" && outcome.err != context.Canceled && outcome.err != context.DeadlineExceeded && groupAlive(cmd.Process.Pid)
	detail := &circusv1.BashResult{ExitCode: int32(outcome.code), Cwd: startFolder(req)}
	body, err := formatOutput(out, detail, !leftovers)
	if err != nil {
		if leftovers {
			stopGroup(cmd.Process.Pid)
		}
		return nil, err
	}
	switch {
	case errors.Is(outcome.err, context.DeadlineExceeded):
		detail.TimedOut = true
		body = appendLine("Command timed out after "+timeoutText(duration), body)
	case errors.Is(outcome.err, context.Canceled):
		detail.Interrupted = true
		// Inferred: cancellation preserves collected output and appends this line.
		body = appendLine(body, "Command was interrupted")
	default:
		if outcome.code == 0 {
			if cwd, err := os.ReadFile(cwdPath); err == nil && len(cwd) > 0 {
				next := strings.TrimSuffix(textutil.NodeUTF8(cwd), "\n")
				if allowedCwd(next, req) {
					detail.Cwd = next
				} else {
					body = appendLine(body, "Shell cwd was reset to "+detail.Cwd)
				}
			}
		}
	}
	if outcome.code != 0 && !detail.Interrupted {
		body = appendLine(fmt.Sprintf("Exit code %d", outcome.code), body)
	}
	if body == "" {
		body = "(Bash completed with no output)"
	}
	keepOutput = detail.OutputTruncated
	if leftovers {
		b.trackGroup(cmd.Process.Pid, out, !detail.OutputTruncated)
		owned = true
		_ = os.Remove(cwdPath)
	}
	return result(body, outcome.code != 0, detail), nil
}

func formatOutput(out *os.File, detail *circusv1.BashResult, removeInline bool) (string, error) {
	// Read a bounded prefix even if descendants are still writing the file.
	raw := make([]byte, 30001)
	n, err := out.ReadAt(raw, 0)
	if err != nil && err != io.EOF {
		return "", err
	}
	raw = raw[:n]
	stat, err := out.Stat()
	if err != nil {
		return "", err
	}
	if len(raw) <= 30000 && stat.Size() <= 30000 {
		if removeInline {
			if err := os.Remove(out.Name()); err != nil {
				return "", err
			}
		}
		return strings.TrimRight(textutil.NodeUTF8(raw), "\n"), nil
	}
	preview := textutil.NodeUTF8(raw)
	units, end := 0, 0
	for i, r := range preview {
		width := 1
		if r > 0xffff {
			width = 2
		}
		if units+width > 2000 {
			end = i
			break
		}
		units += width
		end = i + len(string(r))
	}
	detail.OutputTruncated, detail.OutputPath = true, out.Name()
	return fmt.Sprintf("<persisted-output>\nOutput too large (%.1fKB). Full output saved to: %s\n\nPreview (first 2KB):\n%s\n...\n</persisted-output>", float64(stat.Size())/1024, out.Name(), strings.TrimRight(preview[:end], "\n")), nil
}

func appendLine(body, line string) string {
	if body == "" {
		return line
	}
	if line == "" {
		return body
	}
	return body + "\n" + line
}

func timeoutText(d time.Duration) string {
	// Inferred: durations beyond the measured 1s case use Ns below one minute,
	// and Mm Ss at or above one minute, rounded to the nearest whole second.
	seconds := int(d.Round(time.Second) / time.Second)
	if seconds < 60 {
		return fmt.Sprintf("%ds", seconds)
	}
	return fmt.Sprintf("%dm %ds", seconds/60, seconds%60)
}

func startFolder(req *circusv1.BashRequest) string {
	if len(req.WorkingDirs) > 0 {
		return req.WorkingDirs[0]
	}
	return req.Cwd
}

func allowedCwd(cwd string, req *circusv1.BashRequest) bool {
	dirs := req.WorkingDirs
	if len(dirs) == 0 {
		dirs = []string{req.Cwd}
	}
	for _, dir := range dirs {
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			dir = resolved
		}
		rel, err := filepath.Rel(dir, cwd)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

type backgroundJob struct {
	done   chan struct{}
	cancel context.CancelFunc
	code   int
	err    error
	once   sync.Once
}

func (j *backgroundJob) Wait() (int, error) { <-j.done; return j.code, j.err }
func (j *backgroundJob) Stop() error        { j.once.Do(j.cancel); <-j.done; return nil }

func (b *Bash) adopt(call *circusv1.DeviceCall, req *circusv1.BashRequest, cmd *exec.Cmd, waited <-chan processOutcome, out *os.File, cwdPath string, lifetime time.Duration) (jobs.Info, bool, error) {
	// Once the launch callback runs, the background job owns the sole Wait
	// channel, even if concurrent manager shutdown rejects its registration.
	transferred := false
	info, err := b.jobs.Start(call.SessionId, req.Command, func(string) (jobs.Job, string, error) {
		transferred = true
		return b.newBackground(cmd, waited, out, cwdPath, lifetime), out.Name(), nil
	})
	if err != nil {
		if !transferred {
			_ = stopProcess(cmd, waited)
		}
		// A rejected launch may already have observed natural shell exit and
		// transferred children internally. Failure still owns group cleanup.
		stopGroup(cmd.Process.Pid)
	}
	return info, transferred, err
}

func (b *Bash) background(call *circusv1.DeviceCall, req *circusv1.BashRequest, snapshot, cwdPath string, env []string) (*circusv1.DeviceResult, error) {
	if b.jobs == nil {
		_ = os.Remove(cwdPath)
		return nil, errors.New("background job manager is unavailable")
	}
	outputPath := ""
	pgid := 0
	info, err := b.jobs.Start(call.SessionId, req.Command, func(id string) (jobs.Job, string, error) {
		out, err := b.outputFile(call.SessionId, "tasks", id+"-*.output")
		if err != nil {
			return nil, "", err
		}
		outputPath = out.Name()
		cmd := command(req, snapshot, cwdPath, env)
		cmd.Stdout, cmd.Stderr = out, out
		if err := cmd.Start(); err != nil {
			_ = out.Close()
			_ = os.Remove(out.Name())
			b.environment.releaseOutput(out.Name())
			return nil, "", err
		}
		b.environment.outputBudget.start(out)
		pgid = cmd.Process.Pid
		lifetime := backgroundLifetime(time.Duration(call.TimeoutMs) * time.Millisecond)
		return b.newBackground(cmd, startWait(cmd), out, cwdPath, lifetime), out.Name(), nil
	})
	if err != nil {
		_ = os.Remove(cwdPath)
		if pgid > 0 {
			stopGroup(pgid)
		}
		if outputPath != "" {
			_ = os.Remove(outputPath)
		}
		return nil, err
	}
	body := fmt.Sprintf("Command running in background with ID: %s. Output is being written to: %s. You will be notified when it completes. To check interim output, use Read on that file path.", info.ID, info.OutputPath)
	return result(body, false, &circusv1.BashResult{Cwd: req.Cwd, JobId: info.ID, OutputPath: info.OutputPath}), nil
}

func promotionMessage(duration time.Duration, id, path string, empty bool) string {
	body := fmt.Sprintf("Command did not complete within its %s timeout and was moved to the background (ID: %s). Output is being written to: %s. You will be notified when it completes. If it is still running after 30m in the background, it will be stopped and you will be notified. It is running now: do not start a second copy while it runs. If it is no longer needed, stop it with TaskStop.", timeoutText(duration), id, path)
	if empty {
		body += " No output had been read from it when it was moved. A program that buffers its output, or a pipe into one such as tail, can keep the output file empty until the command finishes."
	}
	return body + " To check interim output, use Read on that file path."
}

func (b *Bash) newBackground(cmd *exec.Cmd, waited <-chan processOutcome, out *os.File, cwdPath string, lifetime time.Duration) *backgroundJob {
	b.environment.retainSnapshot(filepath.Dir(cwdPath))
	ctx, cancel := context.WithTimeout(context.Background(), lifetime)
	job := &backgroundJob{done: make(chan struct{}), cancel: cancel}
	go func() {
		outputTransferred := false
		defer close(job.done)
		defer cancel()
		defer func() {
			_ = os.Remove(cwdPath)
			if !outputTransferred {
				b.environment.releaseOutput(out.Name())
			}
			b.environment.releaseSnapshot(filepath.Join(filepath.Dir(cwdPath), "snapshot.sh"))
		}()
		var outcome processOutcome
		tick := time.NewTicker(outputPollInterval)
		defer tick.Stop()
		capMessage := ""
	waitBackground:
		for {
			select {
			case outcome = <-waited:
				break waitBackground
			case <-tick.C:
				capMessage = b.outputStopMessage(out)
				if capMessage == "" {
					continue
				}
			case <-ctx.Done():
			}
			if capMessage != "" {
				outcome = killProcess(cmd, waited)
				waitGroupEmpty(cmd.Process.Pid)
			} else {
				outcome = stopProcess(cmd, waited)
			}
			// Lifetime expiry remains a kill even when a TERM trap exits cleanly.
			if errors.Is(ctx.Err(), context.DeadlineExceeded) && outcome.terminationRequested {
				outcome.err = errors.Join(outcome.err, jobs.ErrKilled)
			}
			break
		}
		groupIsAlive := groupAlive(cmd.Process.Pid)
		capMessage = outputMessageAfterExit(capMessage, outcome.terminationRequested, groupIsAlive)
		if capMessage != "" || !groupIsAlive {
			b.environment.outputBudget.finish(out.Name())
		}
		if capMessage == "" {
			capMessage = outputMessageAfterExit(b.outputStopMessage(out), outcome.terminationRequested, groupIsAlive)
			if capMessage != "" {
				killGroup(cmd.Process.Pid)
				waitGroupEmpty(cmd.Process.Pid)
				b.environment.outputBudget.finish(out.Name())
			}
		}
		if capMessage != "" {
			outcome.err = errors.Join(outcome.err, jobs.ErrKilled)
		}
		job.code, job.err = outcome.code, outcome.err
		// Normal shell exit errors are represented by the exit code, not Wait errors.
		var exitErr *exec.ExitError
		if errors.As(job.err, &exitErr) && !errors.Is(job.err, jobs.ErrKilled) {
			job.err = nil
		}
		_, writeErr := fmt.Fprintf(out, "\n[exited with code %d]\n", job.code)
		job.err = errors.Join(job.err, writeErr)
		if capMessage != "" {
			job.err = errors.Join(job.err, appendOutputLimit(out, capMessage))
		} else if !outcome.terminationRequested && groupAlive(cmd.Process.Pid) {
			// Use an independent descriptor: the outer job can close its own now.
			childOut, err := os.OpenFile(out.Name(), os.O_RDWR|os.O_APPEND, 0600)
			if err == nil {
				err = syscall.Flock(int(childOut.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
				if err != nil {
					_ = childOut.Close()
					stopGroup(cmd.Process.Pid)
				}
			}
			if err == nil {
				b.environment.outputBudget.start(childOut)
				b.trackGroup(cmd.Process.Pid, childOut, false)
				outputTransferred = true
			} else {
				stopGroup(cmd.Process.Pid)
			}
			job.err = errors.Join(job.err, err)
		}
		job.err = errors.Join(job.err, out.Close())
	}()
	return job
}

func backgroundLifetime(requested time.Duration) time.Duration {
	if requested <= 0 {
		return 30 * time.Minute
	}
	if requested > 2*time.Hour {
		return 2 * time.Hour
	}
	return requested
}

// PreparationBudget bounds both collector attempts and their termination grace.
func (b *Bash) PreparationBudget() time.Duration {
	// Inferred: allow two bounded attempts, their two-second cleanup grace each,
	// and one second for snapshot file I/O. Default total: 25 seconds.
	return 2*b.environment.collectorTimeout() + 5*time.Second
}
