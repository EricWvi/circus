package bashtool

import (
	"bytes"
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
)

// Job is a process whose lifetime is owned by the daemon's job manager.
type Job interface {
	Wait() (int, error)
	Stop() error
}

// Jobs is the small job-manager boundary needed by Bash and TaskStop.
// StartBash allocates the ID before launch and remembers the original command.
// StopBash checks session ownership atomically before requesting termination.
type Jobs interface {
	StartBash(sessionID, command string, launch func(id string) (Job, string, error)) (id, outputPath string, err error)
	StopBash(sessionID, id string) (command string, found bool, err error)
}

// Bash executes native-compatible bash calls without retaining session cwd.
type Bash struct {
	jobs        Jobs
	environment *environment
}

// NewBash uses one lazy process-wide environment snapshot. Supplying Options
// creates an isolated snapshot instead, without reading the real user's files.
func NewBash(jobs Jobs, options ...Options) *Bash {
	env := processEnvironment
	if len(options) > 0 {
		env = &environment{options: options[0]}
	}
	return &Bash{jobs: jobs, environment: env}
}

// Name returns the device call tool name.
func (*Bash) Name() string { return "bash" }

func result(body string, failed bool, detail *circusv1.BashResult) *circusv1.DeviceResult {
	return &circusv1.DeviceResult{
		Output: &circusv1.ToolOutput{IsError: failed, Content: []*circusv1.ContentBlock{{Block: &circusv1.ContentBlock_Text{Text: body}}}},
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

func (b *Bash) outputDir(sessionID, kind string) (string, error) {
	tmp := b.environment.options.TempDir
	if tmp == "" {
		tmp = os.TempDir()
	}
	root := filepath.Join(tmp, fmt.Sprintf("circus-%d", os.Getuid()))
	session := filepath.Join(root, component(sessionID))
	dir := filepath.Join(session, kind)
	for _, p := range []string{root, session, dir} {
		if err := privateDir(p); err != nil {
			return "", err
		}
	}
	return dir, nil
}

func command(request *circusv1.BashRequest, snapshot, cwdFile string, env []string) *exec.Cmd {
	wrapper := "source " + quote(snapshot) + " 2>/dev/null || true && shopt -u extglob 2>/dev/null || true && eval " + quote(request.Command) + " < /dev/null && pwd -P >| " + quote(cwdFile)
	cmd := exec.Command("/bin/bash", "-c", wrapper)
	cmd.Dir, cmd.Env = request.Cwd, env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd
}

// Run executes a foreground command or registers an independent background job.
func (b *Bash) Run(ctx context.Context, call *circusv1.DeviceCall) (*circusv1.DeviceResult, error) {
	req := call.GetBash()
	if req == nil {
		return nil, errors.New("bash request is required")
	}
	started := time.Now()
	duration := time.Duration(0)
	if deadline, ok := ctx.Deadline(); ok {
		duration = deadline.Sub(started)
	}
	snapshot, env, err := b.environment.load(ctx)
	if err != nil {
		detail := &circusv1.BashResult{Cwd: startFolder(req), ExitCode: 143}
		if errors.Is(err, context.DeadlineExceeded) {
			detail.TimedOut = true
			return result("Exit code 143\nCommand timed out after "+timeoutText(duration), true, detail), nil
		}
		if errors.Is(err, context.Canceled) {
			detail.Interrupted = true
			return result("Command was interrupted", true, detail), nil
		}
		return nil, err
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
	defer func() { _ = os.Remove(cwdPath) }()
	cmd := command(req, snapshot, cwdPath, env)
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer func() { _ = reader.Close() }()
	cmd.Stdout, cmd.Stderr = writer, writer
	if err = cmd.Start(); err != nil {
		_ = writer.Close()
		return nil, err
	}
	_ = writer.Close()
	var output bytes.Buffer
	copied := make(chan error, 1)
	go func() { _, err := io.Copy(&output, reader); copied <- err }()
	code, waitErr := waitProcess(ctx, cmd)
	// Descendants can keep the pipe open after the shell exits. Keep observing
	// cancellation until the shared pipe is drained, then join its only reader.
	select {
	case err = <-copied:
	case <-ctx.Done():
		if !errors.Is(waitErr, context.Canceled) && !errors.Is(waitErr, context.DeadlineExceeded) {
			terminateGroup(cmd.Process.Pid)
		}
		_ = reader.Close()
		err = <-copied
		waitErr = ctx.Err()
		code = 143
	}
	if err != nil && ctx.Err() == nil {
		return nil, err
	}
	detail := &circusv1.BashResult{ExitCode: int32(code), Cwd: startFolder(req)}
	body, err := b.formatOutput(call, output.Bytes(), detail)
	if err != nil {
		return nil, err
	}
	switch {
	case errors.Is(waitErr, context.DeadlineExceeded):
		detail.TimedOut = true
		body = "Command timed out after " + timeoutText(duration)
	case errors.Is(waitErr, context.Canceled):
		detail.Interrupted = true
		// Inferred: cancellation preserves collected output and appends this line.
		body = appendLine(body, "Command was interrupted")
	default:
		if code == 0 {
			if cwd, err := os.ReadFile(cwdPath); err == nil && len(cwd) > 0 {
				next := strings.TrimSuffix(string(cwd), "\n")
				if allowedCwd(next, req) {
					detail.Cwd = next
				} else {
					body = appendLine(body, "Shell cwd was reset to "+detail.Cwd)
				}
			}
		}
	}
	if code != 0 && !detail.Interrupted {
		body = appendLine(fmt.Sprintf("Exit code %d", code), body)
	}
	if body == "" {
		body = "(Bash completed with no output)"
	}
	return result(body, code != 0, detail), nil
}

func (b *Bash) formatOutput(call *circusv1.DeviceCall, raw []byte, detail *circusv1.BashResult) (string, error) {
	if len(raw) <= 30000 {
		return strings.TrimRight(string(raw), "\n"), nil
	}
	dir, err := b.outputDir(call.SessionId, "tool-results")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, component(call.CallId)+".txt")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		return "", err
	}
	preview := []rune(string(raw))
	if len(preview) > 2000 {
		preview = preview[:2000]
	}
	detail.OutputTruncated, detail.OutputPath = true, path
	return fmt.Sprintf("<persisted-output>\nOutput too large (%.1fKB). Full output saved to: %s\n\nPreview (first 2KB):\n%s\n...\n</persisted-output>", float64(len(raw))/1024, path, strings.TrimRight(string(preview), "\n")), nil
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

func (b *Bash) background(call *circusv1.DeviceCall, req *circusv1.BashRequest, snapshot, cwdPath string, env []string) (*circusv1.DeviceResult, error) {
	if b.jobs == nil {
		_ = os.Remove(cwdPath)
		return nil, errors.New("background job manager is unavailable")
	}
	id, path, err := b.jobs.StartBash(call.SessionId, req.Command, func(id string) (Job, string, error) {
		dir, err := b.outputDir(call.SessionId, "tasks")
		if err != nil {
			return nil, "", err
		}
		path := filepath.Join(dir, id+".output")
		out, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			return nil, "", err
		}
		cmd := command(req, snapshot, cwdPath, env)
		cmd.Stdout, cmd.Stderr = out, out
		if err = cmd.Start(); err != nil {
			_ = out.Close()
			return nil, "", err
		}
		lifetime := time.Duration(call.TimeoutMs) * time.Millisecond
		if lifetime == 0 {
			lifetime = 30 * time.Minute
		}
		if lifetime > 2*time.Hour {
			lifetime = 2 * time.Hour
		}
		ctx, cancel := context.WithTimeout(context.Background(), lifetime)
		job := &backgroundJob{done: make(chan struct{}), cancel: cancel}
		go func() {
			defer close(job.done)
			defer cancel()
			defer func() { _ = os.Remove(cwdPath) }()
			code, err := waitProcess(ctx, cmd)
			job.code = code
			_, writeErr := fmt.Fprintf(out, "\n[exited with code %d]\n", code)
			closeErr := out.Close()
			if err != nil && cmd.ProcessState == nil {
				job.err = err
			}
			job.err = errors.Join(job.err, writeErr, closeErr)
		}()
		return job, path, nil
	})
	if err != nil {
		_ = os.Remove(cwdPath)
		return nil, err
	}
	body := fmt.Sprintf("Command running in background with ID: %s. Output is being written to: %s. You will be notified when it completes. To check interim output, use Read on that file path.", id, path)
	return result(body, false, &circusv1.BashResult{Cwd: req.Cwd, JobId: id, OutputPath: path}), nil
}
