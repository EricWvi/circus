package bashtool

import (
	"context"
	"log/slog"
	"os/exec"
	"syscall"
	"time"

	"github.com/EricWvi/circus/internal/daemon/jobs"
)

type processOutcome struct {
	code                 int
	err                  error
	terminationRequested bool
}

func startWait(cmd *exec.Cmd) <-chan processOutcome {
	waited := make(chan processOutcome, 1)
	go func() { err := cmd.Wait(); waited <- processOutcome{code: exitCode(cmd), err: err} }()
	return waited
}

// waitProcess owns the sole Wait call and terminates the entire process group.
func waitProcess(ctx context.Context, cmd *exec.Cmd) (int, error) {
	waited := startWait(cmd)
	select {
	case outcome := <-waited:
		return outcome.code, outcome.err
	case <-ctx.Done():
		_ = stopProcess(cmd, waited)
		return 143, ctx.Err()
	}
}

func exitCode(cmd *exec.Cmd) int {
	if status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return 128 + int(status.Signal())
	}
	return cmd.ProcessState.ExitCode()
}

func stopProcess(cmd *exec.Cmd, waited <-chan processOutcome) processOutcome {
	// A natural exit already observed by Wait wins over a concurrent stop.
	select {
	case outcome := <-waited:
		stopGroup(cmd.Process.Pid)
		reapKilledGroup(cmd.Process.Pid)
		return outcome
	default:
	}
	signaled := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) == nil
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	var outcome processOutcome
	select {
	case outcome = <-waited:
	case <-timer.C:
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		outcome = <-waited
	}
	// Reap through the shell first, then remove any remaining descendants.
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	reapKilledGroup(cmd.Process.Pid)
	outcome.terminationRequested = signaled
	if outcome.code == 143 || outcome.code == 137 {
		outcome.err = jobs.ErrKilled
	}
	return outcome
}

// groupAlive must only be used after the original shell's Wait has completed.
// Reap adopted descendants when the daemon is PID 1 or a child subreaper;
// otherwise zombies keep kill(-pgid, 0) reporting a live group indefinitely.
func groupAlive(pgid int) bool {
	for {
		pid, err := syscall.Wait4(-pgid, nil, syscall.WNOHANG, nil)
		if err == syscall.EINTR {
			continue
		}
		if err != nil || pid <= 0 {
			break
		}
	}
	err := syscall.Kill(-pgid, 0)
	return err == nil || err == syscall.EPERM
}

// stopGroup also handles groups whose original shell has already been reaped.
func stopGroup(pgid int) {
	if !groupAlive(pgid) {
		return
	}
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	tick := time.NewTicker(outputPollInterval)
	defer tick.Stop()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for groupAlive(pgid) {
		select {
		case <-tick.C:
		case <-timer.C:
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
			reapKilledGroup(pgid)
			return
		}
	}
}

// reapPollInterval paces reaping after KILL; killed members exit within
// milliseconds, so the usual 100 ms poll would only add latency.
const reapPollInterval = 5 * time.Millisecond

// reapKilledGroup collects the exit status of killed group members that the
// daemon has adopted as PID 1 or a child subreaper, so none stays a zombie.
// It must only be used after the original shell's Wait has completed. ECHILD
// means no member is a child of the daemon any more; orphans adopted by
// another reaper (normally init) are reaped there. Members that are still
// running despite KILL are waited for at most groupExitWait.
func reapKilledGroup(pgid int) bool {
	timer := time.NewTimer(groupExitWait)
	defer timer.Stop()
	tick := time.NewTicker(reapPollInterval)
	defer tick.Stop()
	for {
		pid, err := syscall.Wait4(-pgid, nil, syscall.WNOHANG, nil)
		if err == syscall.EINTR || err == nil && pid > 0 {
			continue
		}
		if err != nil {
			return true
		}
		select {
		case <-tick.C:
		case <-timer.C:
			slog.Warn("killed process group members were not reaped", "pgid", pgid, "timeout", groupExitWait)
			return false
		}
	}
}

// Output caps need immediate KILL: a TERM-ignoring writer can fill the disk
// during the usual shutdown grace period.
func killGroup(pgid int) { _ = syscall.Kill(-pgid, syscall.SIGKILL) }

func killProcess(cmd *exec.Cmd, waited <-chan processOutcome) processOutcome {
	signaled := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) == nil
	outcome := <-waited
	outcome.terminationRequested = signaled
	if signaled {
		outcome.err = jobs.ErrKilled
	}
	return outcome
}
