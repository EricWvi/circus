package userservice

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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

// launchReloadTimeout bounds one step of the helper: the unload wait
// (unloadTimeout) and the bootstraps.
var launchReloadTimeout = 90 * time.Second

// What the detached helper does to one agent.
const (
	// stepLoad bootstraps an agent that is not loaded.
	stepLoad = "load"
	// stepReload boots out a loaded agent, waits for it to unload, and
	// bootstraps it from its plist, which may have changed.
	stepReload = "reload"
	// stepKickstart restarts a loaded agent's process in place
	// (`launchctl kickstart -k`): it stays loaded, for an unchanged plist.
	stepKickstart = "kickstart"
)

// launchStep is one agent the detached helper restarts.
type launchStep struct {
	svc  *Service
	kind string
}

func loadStep(svc *Service, wasLoaded bool) launchStep {
	if wasLoaded {
		return launchStep{svc: svc, kind: stepReload}
	}
	return launchStep{svc: svc, kind: stepLoad}
}

// reloadDetached reloads the agents of steps, in order, from a helper in
// its own session, which finishes even if this process is killed. It
// reports whether a bootstrap was attempted before a failure.
func reloadDetached(home string, steps []launchStep) (bool, error) {
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
	timeout := launchReloadTimeout * time.Duration(len(steps))
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	args := []string{LaunchReloadFlag, home}
	for _, step := range steps {
		args = append(args, step.svc.name+"="+step.kind)
	}
	cmd := exec.CommandContext(ctx, exe, args...)
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
			return true, fmt.Errorf("launchd reload timed out after %s: %w", timeout, ctx.Err())
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

// RunLaunchReload runs the detached helper: its arguments are the home
// directory and, per agent in order, <service>=<load|reload|kickstart>.
// Every agent is handled even when one fails, and a failed unload does not
// skip the bootstrap: bootstrapping an agent that is still loaded fails
// harmlessly, while skipping it would leave one that did unload stopped
// until the next login. Each outcome also goes to
// ~/Library/Logs/circus/reload.log, since the parent that reads the output
// may have been killed by the restart. Its exit code tells the parent
// whether a bootstrap was attempted before a failure.
func RunLaunchReload(args []string, errOut io.Writer) int {
	if len(args) < 2 || args[0] == "" {
		fmt.Fprintln(errOut, "invalid launchd reload arguments")
		return reloadUnloadFailure
	}
	var steps []launchStep
	for _, arg := range args[1:] {
		name, kind, _ := strings.Cut(arg, "=")
		if serviceNamed(name) == nil || (kind != stepLoad && kind != stepReload && kind != stepKickstart) {
			fmt.Fprintln(errOut, "invalid launchd reload arguments")
			return reloadUnloadFailure
		}
		steps = append(steps, launchStep{svc: serviceNamed(name), kind: kind})
	}
	home := args[0]
	log := reloadLog(home, errOut)
	code := 0
	for _, step := range steps {
		l := launchd{svc: step.svc, home: home}
		switch step.kind {
		case stepKickstart:
			if _, err := run("launchctl", "kickstart", "-k", l.target()); err != nil {
				log("%s: kickstart: %v", l.label(), err)
				code = max(code, reloadUnloadFailure)
				continue
			}
			log("%s: restarted", l.label())
			continue
		case stepReload:
			err := l.bootout()
			if err == nil {
				err = l.waitForUnload()
			}
			if err != nil {
				log("%s: unload: %v; bootstrapping anyway", l.label(), err)
				code = max(code, reloadUnloadFailure)
			}
		}
		if firstErr := l.bootstrap(); firstErr != nil {
			if err := l.bootstrap(); err != nil {
				log("%s: bootstrap: %v", l.label(), errors.Join(firstErr, err))
				code = reloadBootstrapFailure
				continue
			}
		}
		log("%s: loaded", l.label())
	}
	return code
}

// reloadLog writes each line to errOut and appends it, timestamped, to the
// helper's log file.
func reloadLog(home string, errOut io.Writer) func(format string, args ...any) {
	path := filepath.Join(home, "Library", "Logs", "circus", "reload.log")
	return func(format string, args ...any) {
		line := fmt.Sprintf(format, args...)
		fmt.Fprintln(errOut, line)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return
		}
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return
		}
		defer f.Close()
		fmt.Fprintf(f, "%s %s\n", time.Now().Format(time.RFC3339), line)
	}
}
