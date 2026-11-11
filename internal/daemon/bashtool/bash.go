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
	jobs      *jobs.Manager
	snapshots *snapshotStore
	outputs   *outputStore
	groups    groupTracker
	effects   *effects
}

// NewBash constructs private, lazy stores without touching startup files.
func NewBash(manager *jobs.Manager, options Options) *Bash {
	fx := &effects{}
	b := &Bash{jobs: manager, effects: fx,
		snapshots: &snapshotStore{options: snapshotConfig(options), effects: fx},
		outputs:   &outputStore{options: outputConfig(options), effects: fx}}

	return b
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
	e := b.snapshots
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
		_, _, lease, err := e.load(warmCtx)
		if err == nil {
			lease.release()
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
	snapshot, env, lease, err := b.snapshots.load(ctx)
	if err == nil && ctx.Err() != nil {
		lease.release()
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
	defer lease.release()
	// The dispatcher arms its command budget only after environment collection.
	toolcontext.StartTimeout(ctx)
	started = time.Now()
	if deadline, ok := ctx.Deadline(); ok {
		duration = deadline.Sub(started)
	}
	var cwdFile *os.File
	err = b.effects.attempt("cwd-open", func() error { var err error; cwdFile, err = os.CreateTemp(filepath.Dir(snapshot), "cwd-"); return err })
	if err != nil {
		return nil, err
	}
	cwdPath := cwdFile.Name()
	_ = cwdFile.Close()
	if req.RunInBackground {
		return b.background(call, req, snapshot, cwdPath, env, lease)
	}
	out, err := b.outputs.outputFile(call.SessionId, "tool-results", component(call.CallId)+"-*.txt")
	if err != nil {
		_ = os.Remove(cwdPath)
		return nil, err
	}
	c, err := b.startCapture(command(req, snapshot, cwdPath, env), out, cwdPath, lease)
	if err != nil {
		return nil, err
	}
	defer c.abandonForeground()
	waited := c.waited
	var outcome processOutcome
	tick := time.NewTicker(outputPollInterval)
	defer tick.Stop()
	capMessage := ""
waitForeground:
	for {
		select {
		case <-tick.C:
			capMessage = b.outputs.outputStopMessage(out)
			if capMessage == "" {
				continue
			}
			outcome = c.terminate(true)
			break waitForeground
		case outcome = <-waited:
			break waitForeground
		case <-ctx.Done():
			select {
			case outcome = <-waited:
			default:
				if errors.Is(ctx.Err(), context.DeadlineExceeded) && !killOnTimeout(req.Command) {
					if b.jobs == nil {
						return nil, errors.New("background job manager is unavailable")
					}
					// Inferred: allow 100 ms for output written at the deadline to drain
					// before taking the bounded promotion preview.
					time.Sleep(100 * time.Millisecond)
					detail := &circusv1.BashResult{Cwd: req.Cwd}
					body, formatErr := b.outputs.formatOutput(out, detail)
					if formatErr != nil {
						return nil, formatErr
					}
					// Inferred: promotion starts a fresh remaining background lifetime,
					// default 30 minutes, capped at two hours (independent of foreground timeout).
					var stat os.FileInfo
					statErr := b.effects.attempt("promotion-stat", func() error { var err error; stat, err = out.Stat(); return err })
					if statErr != nil {
						return nil, statErr
					}
					info, startErr := b.adopt(call, req, c, backgroundLifetime(0))
					if startErr != nil {
						return nil, startErr
					}
					detail.JobId, detail.OutputPath = info.ID, info.OutputPath
					message := promotionMessage(duration, info.ID, info.OutputPath, stat.Size() == 0)
					if strings.HasPrefix(strings.TrimSpace(req.Command), "cd ") {
						message = appendLine(message, "Session cwd remains "+req.Cwd+"; directory changes made by the backgrounded command do not apply to subsequent commands.")
					}
					return result(appendLine(body, message), false, detail), nil
				}
				outcome = c.terminate(false)
				outcome.code, outcome.err = 143, ctx.Err()
			}
			break waitForeground
		}
	}
	outcome, leftovers, finishErr := c.finishShell(outcome, capMessage, false)
	if finishErr != nil {
		return nil, finishErr
	}
	detail := &circusv1.BashResult{ExitCode: int32(outcome.code), Cwd: startFolder(req)}
	body, err := b.outputs.formatOutput(out, detail)
	if err != nil {
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
	if detail.OutputTruncated {
		c.disposition = retainOutput
	}
	if leftovers {
		if err := c.track(c.disposition); err != nil {
			return nil, err
		}
		c.releaseShell()
	} else {
		if err := c.release(ownerForeground, c.disposition); err != nil {
			return nil, err
		}
	}
	return result(body, outcome.code != 0, detail), nil
}

func (s *outputStore) formatOutput(out *os.File, detail *circusv1.BashResult) (string, error) {
	// Read a bounded prefix even if descendants are still writing the file.
	raw := make([]byte, 30001)
	var n int
	err := s.effects.attempt("format-read", func() error { var err error; n, err = out.ReadAt(raw, 0); return err })
	if err != nil && err != io.EOF {
		return "", err
	}
	raw = raw[:n]
	var stat os.FileInfo
	err = s.effects.attempt("format-stat", func() error { var err error; stat, err = out.Stat(); return err })
	if err != nil {
		return "", err
	}
	if len(raw) <= 30000 && stat.Size() <= 30000 {
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

func (b *Bash) adopt(call *circusv1.DeviceCall, req *circusv1.BashRequest, c *capture, lifetime time.Duration) (jobs.Info, error) {
	var info jobs.Info
	err := b.effects.attempt("register-job", func() error {
		var err error
		info, err = b.jobs.Start(call.SessionId, req.Command, func(string) (jobs.Job, string, error) {
			job, err := c.background(lifetime)
			b.effects.pause("job-launched")
			return job, c.out.Name(), err
		})
		return err
	})
	if err != nil {
		c.reject()
	}
	return info, err
}

func (b *Bash) background(call *circusv1.DeviceCall, req *circusv1.BashRequest, snapshot, cwdPath string, env []string, lease *snapshotLease) (*circusv1.DeviceResult, error) {
	if b.jobs == nil {
		_ = os.Remove(cwdPath)
		return nil, errors.New("background job manager is unavailable")
	}
	var c *capture
	var info jobs.Info
	err := b.effects.attempt("register-job", func() error {
		var err error
		info, err = b.jobs.Start(call.SessionId, req.Command, func(id string) (jobs.Job, string, error) {
			out, err := b.outputs.outputFile(call.SessionId, "tasks", id+"-*.output")
			if err != nil {
				return nil, "", err
			}
			c, err = b.startCapture(command(req, snapshot, cwdPath, env), out, cwdPath, lease)
			if err != nil {
				return nil, "", err
			}
			lifetime := backgroundLifetime(time.Duration(call.TimeoutMs) * time.Millisecond)
			job, err := c.background(lifetime)
			b.effects.pause("job-launched")
			return job, out.Name(), err
		})
		return err
	})
	if err != nil {
		if c != nil {
			c.reject()
		}
		_ = os.Remove(cwdPath)
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
	return 2*b.snapshots.collectorTimeout() + 5*time.Second
}
