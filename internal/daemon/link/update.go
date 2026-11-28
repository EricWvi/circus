package link

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/identifier"
	"github.com/EricWvi/circus/internal/textutil"
)

const (
	// DefaultUpdateTimeout bounds `circus update` run for UpdateMachine,
	// below the control plane's 5-minute deadline so a failure is reported
	// before the control plane gives up.
	DefaultUpdateTimeout = 4*time.Minute + 30*time.Second
	// maxUpdateError bounds UpdateFailed.error.
	maxUpdateError = 2048
	// updateOutputTail is how much of the command's output is kept.
	updateOutputTail = 8 << 10
)

// updateMachine starts `circus update` for u unless one already runs. Only
// a daemon running as the installed service (Options.Service) advertises
// the feature; another one ignores the command.
func (c *Client) updateMachine(u *circusv1.UpdateMachine) {
	id := u.GetUpdateId()
	switch {
	case !c.opts.Service:
		c.opts.Logger.Warn("ignoring a machine update: this daemon does not run as the installed service", "update_id", id)
		return
	case !identifier.Valid(id, false):
		c.opts.Logger.Warn("ignoring a machine update with an invalid id")
		return
	case !c.updating.CompareAndSwap(false, true):
		c.opts.Logger.Warn("ignoring a machine update: circus update is already running", "update_id", id)
		return
	}
	id = string([]byte(id))
	c.opts.Logger.Info("machine update started: running circus update", "update_id", id, "version", u.GetVersion())
	go func() {
		defer c.updating.Store(false)
		failure := c.runUpdate()
		if failure == "" {
			// The service manager restarts the services; reconnecting with
			// the new version completes the update.
			c.opts.Logger.Info("circus update finished", "update_id", id)
			return
		}
		c.opts.Logger.Warn("circus update failed", "update_id", id, "error", failure)
		// To the connection live now: the command may have outlived the
		// one it came on. Without one the control plane's deadline fails it.
		c.send(c.live.Load(), &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_UpdateFailed{
			UpdateFailed: &circusv1.UpdateFailed{UpdateId: id, Error: failure}}})
	}()
}

// runUpdate runs `<executable> update` with this process's environment
// in its own process group, killing the group after the update timeout,
// and once the command has exited, whatever it left in its group. It
// returns "" on success, else why it failed, with the output's tail.
//
// The group is not killed when this daemon shuts down: the shutdown is
// normally the restart `circus update` asked for as its last step, after
// the service manager already took the request (systemctl --no-block, or
// launchd's helper in its own session, outside this group), and the
// command only reports and exits after it. Stopping it then would gain
// nothing; the service manager's restart ends it with the service.
func (c *Client) runUpdate() string {
	exe := c.opts.UpdateExecutable
	if exe == "" {
		var err error
		if exe, err = os.Executable(); err == nil {
			exe, err = filepath.EvalSymlinks(exe)
		}
		if err != nil {
			return "could not locate this circus: " + err.Error()
		}
	}
	timeout := c.opts.UpdateTimeout
	if timeout <= 0 {
		timeout = DefaultUpdateTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "update")
	// A nil Stdin is the null device; a nil Env inherits this process's,
	// so the service's proxy settings apply.
	out := &tailWriter{max: updateOutputTail}
	cmd.Stdout, cmd.Stderr = out, out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return killProcessGroup(cmd.Process.Pid) }
	// A helper that inherited the output pipes cannot hold the result.
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	if cmd.Process != nil && killProcessGroup(cmd.Process.Pid) == nil {
		// The command has exited; what it left in its group goes too
		// (reaped when this process adopted it as PID 1 or a subreaper).
		_ = reapProcessGroup(cmd.Process.Pid)
	}
	if errors.Is(err, exec.ErrWaitDelay) {
		// It succeeded, but a process it left held the output open.
		err = nil
	}
	var summary string
	var exit *exec.ExitError
	switch {
	case ctx.Err() != nil:
		summary = fmt.Sprintf("circus update did not finish within %s and was stopped", timeout.Round(time.Second))
	case err == nil:
		return ""
	case errors.As(err, &exit):
		summary = "circus update failed: " + exit.String()
	default:
		summary = "could not run circus update: " + err.Error()
	}
	tail := textutil.Tail(out.String(), maxUpdateError-len(summary)-1)
	if tail == "" {
		return textutil.Tail(summary, maxUpdateError)
	}
	return summary + "\n" + tail
}

// tailWriter keeps the last max bytes written to it.
type tailWriter struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (w *tailWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	if over := len(w.buf) - w.max; over > 0 {
		w.buf = append(w.buf[:0], w.buf[over:]...)
	}
	return len(p), nil
}

func (w *tailWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return string(w.buf)
}
