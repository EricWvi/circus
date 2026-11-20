package link

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/clock"
	"github.com/EricWvi/circus/internal/daemon/dirlist"
	"github.com/EricWvi/circus/internal/gitenv"
)

const maxGitOutput = 64 << 10

func (c *Client) readGitRemote(ctx context.Context, path string) *circusv1.GitRemoteResult {
	result := &circusv1.GitRemoteResult{}
	resolved := dirlist.Resolve(path)
	if resolved.GetError() != 0 {
		result.Error = circusv1.GitRemoteError_GIT_REMOTE_ERROR_INVALID_PATH
		return result
	}
	if ctx.Err() != nil {
		result.Error = circusv1.GitRemoteError_GIT_REMOTE_ERROR_TIMEOUT
		return result
	}
	info, err := os.Stat(resolved.Path)
	switch {
	case ctx.Err() != nil:
		result.Error = circusv1.GitRemoteError_GIT_REMOTE_ERROR_TIMEOUT
	case errors.Is(err, os.ErrNotExist), errors.Is(err, syscall.ENOTDIR), err == nil && !info.IsDir():
		result.Error = circusv1.GitRemoteError_GIT_REMOTE_ERROR_NOT_FOUND
	case err != nil:
		c.opts.Logger.Warn("git remote path stat failed", "error", err)
		result.Error = circusv1.GitRemoteError_GIT_REMOTE_ERROR_FAILED
	}
	if result.Error != 0 {
		return result
	}

	// Track only the subprocess phase. A blocked Stat can hold a query slot,
	// but must not hold up shutdown or start a new subprocess during teardown.
	c.mu.Lock()
	if c.stopping.Load() || ctx.Err() != nil {
		c.mu.Unlock()
		result.Error = circusv1.GitRemoteError_GIT_REMOTE_ERROR_TIMEOUT
		return result
	}
	done := make(chan struct{})
	c.gitQueries[done] = struct{}{}
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		close(done)
		delete(c.gitQueries, done)
		c.mu.Unlock()
	}()

	_, result.Error = c.gitCommand(ctx, resolved.Path, "rev-parse", "--git-dir")
	if result.Error != 0 {
		return result
	}
	stdout, code := c.gitCommand(ctx, resolved.Path, "remote", "get-url", "origin")
	if code != 0 {
		result.Error = code
		return result
	}
	// Keep NEL for the control-character check even at the edge of the URL.
	url := bytes.TrimFunc(stdout.buffer.Bytes(), func(r rune) bool { return unicode.IsSpace(r) && r != '\u0085' })
	switch {
	case stdout.truncated || len(url) > 2048 || !utf8.Valid(url) || bytes.ContainsFunc(url, unicode.IsControl):
		c.opts.Logger.Warn("invalid git origin URL output", "path", resolved.Path)
		result.Error = circusv1.GitRemoteError_GIT_REMOTE_ERROR_FAILED
	case len(url) == 0 || bytes.Equal(url, []byte("origin")):
		result.Error = circusv1.GitRemoteError_GIT_REMOTE_ERROR_NO_ORIGIN
	default:
		sanitized, ok := sanitizeGitRemoteURL(string(url))
		if !ok {
			c.opts.Logger.Warn("could not parse git origin URL", "path", resolved.Path)
			result.Error = circusv1.GitRemoteError_GIT_REMOTE_ERROR_FAILED
			return result
		}
		result.Url = sanitized
	}
	return result
}

func (c *Client) waitGitQueries(ctx context.Context) error {
	// stopping is already set, so no more subprocess phases can register.
	c.mu.Lock()
	active := make([]<-chan struct{}, 0, len(c.gitQueries))
	for done := range c.gitQueries {
		active = append(active, done)
	}
	c.mu.Unlock()
	for _, done := range active {
		if err := waitDone(ctx, done); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) gitCommand(ctx context.Context, path string, args ...string) (*gitOutput, circusv1.GitRemoteError) {
	executable := c.opts.GitExecutable
	if executable == "" {
		executable = "git"
	}
	cmd := exec.CommandContext(ctx, executable, append([]string{"-C", path}, args...)...)
	cmd.Env = append(gitenv.WithoutRepository(os.Environ()), "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0", "GCM_INTERACTIVE=never", "LC_ALL=C")
	// A nil Stdin connects the child to the null device.
	stdout, stderr := &gitOutput{}, &gitOutput{}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return killGitGroup(cmd.Process.Pid) }
	cmd.WaitDelay = 100 * time.Millisecond
	err := waitGitCommand(cmd, path, c.opts.Logger)
	var exit *exec.ExitError
	switch {
	case ctx.Err() != nil:
		return nil, circusv1.GitRemoteError_GIT_REMOTE_ERROR_TIMEOUT
	case err == nil:
		return stdout, 0
	case errors.Is(err, exec.ErrNotFound):
		return nil, circusv1.GitRemoteError_GIT_REMOTE_ERROR_GIT_UNAVAILABLE
	case errors.As(err, &exit):
		if args[0] == "rev-parse" && exit.ExitCode() == 128 && bytes.Contains(stderr.buffer.Bytes(), []byte("not a git repository")) {
			return nil, circusv1.GitRemoteError_GIT_REMOTE_ERROR_NOT_REPOSITORY
		}
		if args[0] == "remote" && (exit.ExitCode() == 2 ||
			exit.ExitCode() == 128 && bytes.Contains(stderr.buffer.Bytes(), []byte("No such remote"))) {
			return nil, circusv1.GitRemoteError_GIT_REMOTE_ERROR_NO_ORIGIN
		}
	}
	const maxLoggedStderr = 1 << 10
	output := stderr.buffer.Bytes()
	c.opts.Logger.Warn("git remote command failed", "command", args[0], "path", path, "error", err, "stderr", string(output[:min(len(output), maxLoggedStderr)]))
	return nil, circusv1.GitRemoteError_GIT_REMOTE_ERROR_FAILED
}

type gitOutput struct {
	buffer    bytes.Buffer
	truncated bool
}

func (o *gitOutput) Write(p []byte) (int, error) {
	keep := min(len(p), maxGitOutput-o.buffer.Len())
	if keep < len(p) {
		o.truncated = true
	}
	_, _ = o.buffer.Write(p[:keep])
	return len(p), nil
}

func killGitGroup(pgid int) error {
	err := syscall.Kill(-pgid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}

func waitGitCommand(cmd *exec.Cmd, path string, logger *slog.Logger) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	err := cmd.Wait()
	pgid := cmd.Process.Pid
	// Wait owns the leader. Cancel may not have run if the leader exited
	// while a descendant still held its output pipes or ran independently.
	if killErr := killGitGroup(pgid); killErr != nil {
		if errors.Is(killErr, os.ErrProcessDone) {
			return err
		}
		return killErr
	}
	// Reap adopted children when running as PID 1 or a child subreaper.
	for {
		_, reapErr := syscall.Wait4(-pgid, nil, 0, nil)
		if errors.Is(reapErr, syscall.ECHILD) {
			break
		}
		if reapErr != nil && !errors.Is(reapErr, syscall.EINTR) {
			return reapErr
		}
	}
	if groupErr := waitGitGroup(pgid, path, logger, clock.Real{}, syscall.Kill); groupErr != nil {
		return groupErr
	}
	return err
}

func waitGitGroup(pgid int, path string, logger *slog.Logger, clk clock.Clock, signal func(int, syscall.Signal) error) error {
	// An external adopter may leave killed descendants as zombies indefinitely.
	deadline := clk.After(time.Second)
	for {
		groupErr := signal(-pgid, 0)
		if errors.Is(groupErr, syscall.ESRCH) {
			return nil
		}
		if groupErr != nil && !errors.Is(groupErr, syscall.EPERM) {
			return groupErr
		}
		select {
		case <-deadline:
			logger.Warn("git process group still exists after cleanup", "pgid", pgid, "path", path)
			return nil
		case <-clk.After(5 * time.Millisecond):
		}
	}
}
