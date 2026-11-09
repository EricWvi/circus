package bashtool

import (
	"context"
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
	outcome.terminationRequested = signaled
	if outcome.code == 143 || outcome.code == 137 {
		outcome.err = jobs.ErrKilled
	}
	return outcome
}
