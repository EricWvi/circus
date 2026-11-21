package workspace

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	// maxCapture bounds what is kept of each git output stream.
	maxCapture = 64 << 10
	// termGrace is how long a process group gets between TERM and KILL.
	termGrace = 2 * time.Second
	// pipeGrace bounds Wait when a group member keeps git's pipes open.
	pipeGrace = time.Second
	// groupGrace bounds the wait for an external adopter to reap killed members.
	groupGrace = time.Second
)

// errGitUnavailable reports that no git executable could be started.
var errGitUnavailable = errors.New("git is not available")

// capture keeps the first (head) or last (tail) maxCapture bytes written.
type capture struct {
	mu      sync.Mutex
	tail    bool
	buf     []byte
	dropped bool
}

func (c *capture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := len(p)
	if !c.tail {
		keep := min(len(p), maxCapture-len(c.buf))
		c.dropped = c.dropped || keep < len(p)
		c.buf = append(c.buf, p[:keep]...)
		return n, nil
	}
	if len(p) >= maxCapture {
		p, c.buf, c.dropped = p[len(p)-maxCapture:], c.buf[:0], true
	}
	if over := len(c.buf) + len(p) - maxCapture; over > 0 {
		c.buf, c.dropped = append(c.buf[:0], c.buf[over:]...), true
	}
	c.buf = append(c.buf, p...)
	return n, nil
}

func (c *capture) bytes() ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return bytes.Clone(c.buf), c.dropped
}

// process is one git run without a shell, in its own session and process
// group, with stdin on the null device. Its group is killed and reaped once
// the leader exits, so nothing it started outlives it.
type process struct {
	cmd            *exec.Cmd
	pgid           int
	stdout, stderr *capture
	done           chan struct{}
	err            error
	termOnce       sync.Once
}

func startGit(executable string, env []string, dir string, captureStdout bool, args ...string) (*process, error) {
	cmd := exec.Command(executable, args...)
	cmd.Env, cmd.Dir = env, dir
	p := &process{cmd: cmd, stderr: &capture{tail: true}, done: make(chan struct{})}
	if captureStdout {
		p.stdout = &capture{}
		cmd.Stdout = p.stdout
	}
	cmd.Stderr = p.stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	// Pdeathsig on Linux: assumes the starting OS thread outlives git
	// (no LockOSThread in the daemon); see setDeathSignal.
	setDeathSignal(cmd.SysProcAttr)
	cmd.WaitDelay = pipeGrace
	if err := cmd.Start(); err != nil {
		return nil, startError(err, executable)
	}
	p.pgid = cmd.Process.Pid
	go p.wait()
	return p, nil
}

func (p *process) wait() {
	err := p.cmd.Wait()
	if errors.Is(err, exec.ErrWaitDelay) && p.cmd.ProcessState != nil && p.cmd.ProcessState.Success() {
		err = nil
	}
	// The leader is gone; kill what it left in its group and reap members
	// adopted by this process (PID 1 or a child subreaper).
	if killErr := syscall.Kill(-p.pgid, syscall.SIGKILL); killErr == nil {
		for {
			_, reapErr := syscall.Wait4(-p.pgid, nil, 0, nil)
			if reapErr == nil || errors.Is(reapErr, syscall.EINTR) {
				continue
			}
			break
		}
		deadline := time.Now().Add(groupGrace)
		for time.Now().Before(deadline) {
			if alive := syscall.Kill(-p.pgid, 0); errors.Is(alive, syscall.ESRCH) {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	p.err = err
	close(p.done)
}

// terminate sends TERM to the group, then KILL after termGrace unless the
// leader has exited. It returns at once; done closes once all is reaped.
func (p *process) terminate() {
	p.termOnce.Do(func() {
		select {
		case <-p.done:
			return
		default:
		}
		_ = syscall.Kill(-p.pgid, syscall.SIGTERM)
		go func() {
			timer := time.NewTimer(termGrace)
			defer timer.Stop()
			select {
			case <-p.done:
			case <-timer.C:
				_ = syscall.Kill(-p.pgid, syscall.SIGKILL)
			}
		}()
	})
}

// succeeded reports a zero exit; valid after done.
func (p *process) succeeded() bool { return p.err == nil }

// exitCode returns git's exit status, or -1 for a signal; valid after done.
func (p *process) exitCode() int {
	if p.cmd.ProcessState == nil {
		return -1
	}
	return p.cmd.ProcessState.ExitCode()
}

// gitResult is the outcome of a short git command.
type gitResult struct {
	stdout []byte
	stderr []byte
	code   int
}

// runGit runs a short git command bounded by ctx. err is ctx.Err(),
// errGitUnavailable or a start failure; a non-zero exit is not an error.
func runGit(ctx context.Context, executable string, env []string, dir string, args ...string) (gitResult, error) {
	if err := ctx.Err(); err != nil {
		return gitResult{}, err
	}
	p, err := startGit(executable, env, dir, true, args...)
	if err != nil {
		return gitResult{}, err
	}
	select {
	case <-p.done:
	case <-ctx.Done():
		p.terminate()
		<-p.done
		return gitResult{}, ctx.Err()
	}
	stdout, _ := p.stdout.bytes()
	stderr, _ := p.stderr.bytes()
	return gitResult{stdout: stdout, stderr: stderr, code: p.exitCode()}, nil
}

// findGit returns override when it names a path, or the first executable
// regular file "git" (or override) on the environment's PATH, which can
// differ from the daemon's own. Relative PATH entries are ignored.
func findGit(env []string, override string) (string, error) {
	name := "git"
	if override != "" {
		if strings.ContainsRune(override, '/') {
			return override, nil
		}
		name = override
	}
	path := ""
	for _, entry := range env {
		if value, ok := strings.CutPrefix(entry, "PATH="); ok {
			path = value
		}
	}
	for _, dir := range filepath.SplitList(path) {
		if !filepath.IsAbs(dir) {
			continue
		}
		candidate := filepath.Join(dir, name)
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", errGitUnavailable
}

// startError reports errGitUnavailable when git itself cannot run: not
// found, or not an executable regular file. Anything else, such as a
// working directory that vanished (the parent removed between the lock
// and the clone) or a failed fork, is returned as is.
func startError(err error, executable string) error {
	if errors.Is(err, exec.ErrNotFound) {
		return errGitUnavailable
	}
	info, statErr := os.Stat(executable)
	if statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return errGitUnavailable
	}
	return err
}
