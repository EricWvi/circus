// Package execguard implements `circus internal exec-guard`, the supervisor
// the worker starts to run the Claude Code CLI:
//
//	circus internal exec-guard --parent <pid> -- <command> [args...]
//
// The guard starts the command with its stdin, stdout, stderr, environment and
// argv. It forwards SIGTERM, SIGINT and SIGHUP. An inherited ignored SIGHUP
// or SIGINT stays ignored. Go installs its own SIGTERM handler at startup.
// The guard exits with the command's status (128+signal when a signal ended
// it). Parent exit and SIGUSR1 request teardown and yield 128+SIGKILL if
// the command was still running.
//
// On Linux the guard watches the parent via pidfd and is a child subreaper.
// It forwards signals to the command PID and starts the command in the
// guard's process group. On parent exit, SIGUSR1 or command exit, it kills
// and reaps the whole descendant tree, including children that change groups
// or sessions, with a five-second deadline. If pidfd_open is unavailable or
// blocked with EPERM, the parent-death signal alone watches the parent. In
// that mode, exit of the parent's forking thread also triggers teardown.
// Forcibly killing the guard kills the command via its SIGKILL death signal.
// Its descendants can survive. The worker's cleanup can reach those still
// in the guard's session or process group.
//
// On macOS the guard watches the parent via kqueue and forwards signals to
// the command's process group. The same teardown triggers kill that group.
// macOS has no child subreaper, so descendants that leave the group (e.g.
// with setsid) can survive. If the guard is forcibly killed, the command's
// separate process group can also survive.
//
// Exit codes of the guard itself: 2 bad usage or unsupported platform, 125 the
// parent is already gone (nothing was started), 126 the guard could not set
// itself up or the command is not executable, 127 the command was not found.
// A command may exit with these codes too; the guard's own failures are the
// ones it explains on stderr with an "exec-guard: " prefix.
package execguard

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os/exec"
	"strconv"
)

const (
	// ExitUsage reports bad arguments or an unsupported platform.
	ExitUsage = 2
	// ExitParentGone reports that --parent died before the guard could
	// watch it; the command was not started.
	ExitParentGone = 125
	// ExitSetup reports a failure to arm the guard or to start the command,
	// including a command that is not executable.
	ExitSetup = 126
	// ExitNotFound reports that the command was not found.
	ExitNotFound = 127
)

// Usage is the command line the guard accepts.
const Usage = "usage: circus internal exec-guard --parent <pid> -- <command> [args...]"

// beforeGuard, when set by a test, runs before opening the parent watch,
// so a test can make the parent die between starting the guard and the watch.
var beforeGuard func(parent int)

// Run supervises the command with args (everything after "exec-guard") and
// returns the guard's exit code.
func Run(args []string, stderr io.Writer) int {
	parent, argv, err := parse(args)
	if err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintf(stderr, "exec-guard: %v\n", err)
		}
		fmt.Fprintln(stderr, Usage)
		return ExitUsage
	}
	return run(parent, argv, stderr)
}

func parse(args []string) (int, []string, error) {
	fs := flag.NewFlagSet("exec-guard", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	parentFlag := fs.String("parent", "", "PID of the process whose exit ends the command")
	if err := fs.Parse(args); err != nil {
		return 0, nil, err
	}
	rest := fs.Args()
	if consumed := len(args) - len(rest); consumed == 0 || args[consumed-1] != "--" {
		return 0, nil, errors.New("missing -- before the command")
	}
	if len(rest) == 0 || rest[0] == "" {
		return 0, nil, errors.New("missing command")
	}
	if *parentFlag == "" {
		return 0, nil, errors.New("missing --parent")
	}
	parent, err := strconv.Atoi(*parentFlag)
	if err != nil || parent <= 1 {
		return 0, nil, fmt.Errorf("bad --parent %q: want a PID above 1", *parentFlag)
	}
	return parent, rest, nil
}

func fail(stderr io.Writer, code int, format string, args ...any) int {
	fmt.Fprintf(stderr, "exec-guard: "+format+"\n", args...)
	return code
}

func parentGone(stderr io.Writer, parent, ppid int, command string) int {
	return fail(stderr, ExitParentGone, "parent %d is gone (getppid=%d); not starting %s", parent, ppid, command)
}

// lookPath resolves command like exec.LookPath and, on failure, returns the
// guard's exit code: 126 for a file that is not executable, 127 otherwise.
func lookPath(command string) (string, int, error) {
	path, err := exec.LookPath(command)
	if err == nil {
		return path, 0, nil
	}
	if errors.Is(err, fs.ErrPermission) {
		return "", ExitSetup, err
	}
	return "", ExitNotFound, err
}
