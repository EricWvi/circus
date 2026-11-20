package daemonservice

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// LaunchReloadFlag selects the private launchd reload subprocess.
const LaunchReloadFlag = "--internal-launchd-reload"

const (
	reloadUnloadFailure    = 1
	reloadBootstrapFailure = 2
)

var launchReloadTimeout = 60 * time.Second

func (l launchd) reloadDetached(wasLoaded bool) (bool, error) {
	exe, err := os.Executable()
	if err != nil {
		return false, err
	}
	output, err := os.CreateTemp("", "circus-launchd-reload-*")
	if err != nil {
		return false, err
	}
	defer output.Close()
	// Keep the output open across exec without leaving a file if the caller dies.
	if err := os.Remove(output.Name()); err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), launchReloadTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, LaunchReloadFlag, l.home, strconv.FormatBool(wasLoaded))
	// A daemon shutting down its Bash tools may kill the install process group.
	// Put the complete reload in a separate session so it can finish.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdout, cmd.Stderr = output, output
	// Stop launchctl along with the helper before rolling back the plist.
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return true, fmt.Errorf("launchd reload timed out after %s: %w", launchReloadTimeout, ctx.Err())
		}
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			return false, fmt.Errorf("start launchd reload: %w", err)
		}
		bootstrapTried := exit.ExitCode() == reloadBootstrapFailure
		data, readErr := io.ReadAll(io.NewSectionReader(output, 0, 64<<10))
		if message := strings.TrimSpace(string(data)); message != "" {
			return bootstrapTried, fmt.Errorf("launchd reload: %s", message)
		}
		return bootstrapTried, fmt.Errorf("launchd reload: %w", errors.Join(err, readErr))
	}
	return true, nil
}

// RunLaunchReload runs the install reload in the detached subprocess. Its exit
// code tells the parent whether bootstrap was attempted before a failure.
func RunLaunchReload(args []string, errOut io.Writer) int {
	if len(args) != 2 || args[0] == "" {
		fmt.Fprintln(errOut, "invalid launchd reload arguments")
		return reloadUnloadFailure
	}
	wasLoaded, err := strconv.ParseBool(args[1])
	if err != nil {
		fmt.Fprintln(errOut, err)
		return reloadUnloadFailure
	}
	if wasLoaded {
		err := bootout()
		if err == nil {
			err = waitForUnload()
		}
		if err != nil {
			fmt.Fprintln(errOut, err)
			return reloadUnloadFailure
		}
	}
	l := launchd{home: args[0]}
	if firstErr := l.bootstrap(); firstErr != nil {
		if err := l.bootstrap(); err != nil {
			fmt.Fprintln(errOut, errors.Join(firstErr, err))
			return reloadBootstrapFailure
		}
	}
	return 0
}
