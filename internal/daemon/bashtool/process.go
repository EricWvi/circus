package bashtool

import (
	"context"
	"errors"
	"os/exec"
	"syscall"
	"time"
)

// waitProcess owns the sole Wait call and terminates the entire process group.
func waitProcess(ctx context.Context, cmd *exec.Cmd) (int, error) {
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	select {
	case err := <-waited:
		return exitCode(cmd), err
	case <-ctx.Done():
		terminateGroup(cmd.Process.Pid)
		<-waited
		return 143, ctx.Err()
	}
}

func exitCode(cmd *exec.Cmd) int {
	if status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return 128 + int(status.Signal())
	}
	return cmd.ProcessState.ExitCode()
}

func terminateGroup(pid int) {
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if errors.Is(syscall.Kill(-pid, 0), syscall.ESRCH) {
			return
		}
		select {
		case <-timer.C:
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			return
		case <-tick.C:
		}
	}
}
