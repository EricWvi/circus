package execguard

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"golang.org/x/sys/unix"
)

// run supervises argv: macOS has no parent-death signal, so the guard stays
// the command's parent, watches --parent with kqueue and kills the command's
// process group on parent exit, SIGUSR1 or command exit.
//
// The watch is registered before the getppid check: a parent that exits
// before registration fails the check (its children were reparented on
// exit), and one that exits after it fires the watch.
func run(parent int, argv []string, stderr io.Writer) int {
	// Before anything else, so a termination signal from now on is forwarded
	// (or delivered once the command has started) instead of killing only the
	// guard.
	signals := make(chan os.Signal, 16)
	signal.Notify(signals, append(forwardable(), unix.SIGUSR1)...)
	defer signal.Stop(signals)

	if beforeGuard != nil {
		beforeGuard(parent)
	}
	kq, err := unix.Kqueue()
	if err != nil {
		return fail(stderr, ExitSetup, "kqueue: %v", err)
	}
	defer unix.Close(kq)
	unix.CloseOnExec(kq)
	if err := watchExit(kq, parent); err != nil {
		if errors.Is(err, unix.ESRCH) {
			return parentGone(stderr, parent, os.Getppid(), argv[0])
		}
		return fail(stderr, ExitSetup, "kevent(EVFILT_PROC %d): %v", parent, err)
	}
	if ppid := os.Getppid(); ppid != parent {
		return parentGone(stderr, parent, ppid, argv[0])
	}
	path, code, err := lookPath(argv[0])
	if err != nil {
		return fail(stderr, code, "%v", err)
	}
	// *os.File stdio is passed to the command as is; the guard copies no data.
	// A nil Env gives the command the guard's environment unchanged.
	cmd := &exec.Cmd{
		Path:        path,
		Args:        argv,
		Stdin:       os.Stdin,
		Stdout:      os.Stdout,
		Stderr:      os.Stderr,
		SysProcAttr: &syscall.SysProcAttr{Setpgid: true},
	}
	if err := cmd.Start(); err != nil {
		return fail(stderr, ExitSetup, "start %s: %v", path, err)
	}
	group := cmd.Process.Pid

	// Watch the command without reaping it. Its PID (and thus the group ID)
	// stays reserved until after we have signalled the group. ESRCH means
	// the command already exited before we could register the watch.
	if err := watchExit(kq, group); err != nil {
		_ = unix.Kill(-group, unix.SIGKILL)
		waitErr := cmd.Wait()
		if !errors.Is(err, unix.ESRCH) {
			return fail(stderr, ExitSetup, "kevent(EVFILT_PROC %d): %v", group, err)
		}
		return exitCode(waitErr, stderr, path)
	}

	stopping := make(chan error, 1)
	go func() {
		events := make([]unix.Kevent_t, 1)
		for {
			n, err := kevent(kq, nil, events)
			if err != nil || n > 0 && events[0].Fflags&unix.NOTE_EXIT != 0 {
				stopping <- err
				return
			}
		}
	}()
	for {
		select {
		case err := <-stopping:
			if err != nil {
				// Fail closed: either process could outlive its watch.
				fail(stderr, 0, "watching parent and command: %v; stopping %s", err, argv[0])
			}
			_ = unix.Kill(-group, unix.SIGKILL)
			return exitCode(cmd.Wait(), stderr, path)
		case sig := <-signals:
			if sig == unix.SIGUSR1 {
				sig = unix.SIGKILL
			}
			_ = unix.Kill(-group, sig.(syscall.Signal))
		}
	}
}

func watchExit(kq, pid int) error {
	var watch unix.Kevent_t
	unix.SetKevent(&watch, pid, unix.EVFILT_PROC, unix.EV_ADD|unix.EV_ONESHOT)
	watch.Fflags = unix.NOTE_EXIT
	_, err := kevent(kq, []unix.Kevent_t{watch}, nil)
	return err
}

// kevent retries calls interrupted by a signal.
func kevent(kq int, changes, events []unix.Kevent_t) (int, error) {
	for {
		n, err := unix.Kevent(kq, changes, events, nil)
		if !errors.Is(err, unix.EINTR) {
			return n, err
		}
	}
}

func exitCode(err error, stderr io.Writer, path string) int {
	if err == nil {
		return 0
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		return fail(stderr, ExitSetup, "wait %s: %v", path, err)
	}
	status, ok := exit.Sys().(syscall.WaitStatus)
	if ok && status.Signaled() {
		return 128 + int(status.Signal())
	}
	return exit.ExitCode()
}
