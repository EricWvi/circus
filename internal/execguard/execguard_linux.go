package execguard

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	parentDeathSignal = unix.SIGUSR2
	reapBatchSize     = 64

	// The worker's GUARD_TEARDOWN_MS in worker/src/process.ts matches this
	// bound so its group-kill fallback leaves time for the guard to finish.
	teardownLimit = 5 * time.Second
)

// Linux test hooks for the startup and teardown races.
var (
	openParentPIDFD     = unix.PidfdOpen
	openDescendantPIDFD = unix.PidfdOpen
	killDescendantPID   = unix.Kill
	readProcChildren    = os.ReadFile
	afterPidfd          func(parent int)
	beforeStart         func(stops <-chan os.Signal)
	beforeStoppedStatus func(pid int)
	afterIgnoredDeath   func()
	afterChildren       func(pid int)
)

func run(parent int, argv []string, stderr io.Writer) int {
	// Arm the guard's death signal and fork the command on a thread that
	// stays alive throughout supervision.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	signals := make(chan os.Signal, 8)
	if sigs := forwardable(); len(sigs) > 0 {
		signal.Notify(signals, sigs...)
		defer signal.Stop(signals)
	}
	stops := make(chan os.Signal, 2)
	signal.Notify(stops, unix.SIGUSR1, parentDeathSignal)
	defer signal.Stop(stops)
	childChanged := make(chan os.Signal, 1)
	signal.Notify(childChanged, unix.SIGCHLD)
	defer signal.Stop(childChanged)

	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		return fail(stderr, ExitSetup, "prctl(PR_SET_CHILD_SUBREAPER): %v", err)
	}
	if err := unix.Prctl(unix.PR_SET_PDEATHSIG, uintptr(parentDeathSignal), 0, 0, 0); err != nil {
		return fail(stderr, ExitSetup, "prctl(PR_SET_PDEATHSIG): %v", err)
	}
	if beforeGuard != nil {
		beforeGuard(parent)
	}
	pidfd, err := openParentPIDFD(parent, 0)
	switch {
	case err == nil:
		defer unix.Close(pidfd)
	case errors.Is(err, unix.ENOSYS), errors.Is(err, unix.EPERM):
		pidfd = -1
	case errors.Is(err, unix.ESRCH):
		return parentGone(stderr, parent, os.Getppid(), argv[0])
	default:
		return fail(stderr, ExitSetup, "pidfd_open(%d): %v", parent, err)
	}
	// Open the watch first: after this check the pidfd still reports the
	// original parent's death even if its numeric PID is reused.
	if afterPidfd != nil {
		afterPidfd(parent)
	}
	if ppid := os.Getppid(); ppid != parent {
		return parentGone(stderr, parent, ppid, argv[0])
	}
	path, code, err := lookPath(argv[0])
	if err != nil {
		return fail(stderr, code, "%v", err)
	}

	var parentExited <-chan error
	if pidfd >= 0 {
		read, stop, err := os.Pipe()
		if err != nil {
			return fail(stderr, ExitSetup, "parent watch pipe: %v", err)
		}
		defer read.Close()
		parentExited = watchParent(pidfd, int(read.Fd()))
		defer func() {
			_ = stop.Close()
			<-parentExited
		}()
	}

	if beforeStart != nil {
		beforeStart(stops)
	}
	if code := checkBeforeStart(parent, parentExited != nil, stops, argv[0], stderr); code != 0 {
		return code
	}

	// Inherit the guard's process group, environment and stdio. The command's
	// death signal covers loss of the guard itself. Only this supervisor
	// calls wait4, so it can reap both the command and orphans.
	command, err := os.StartProcess(path, argv, &os.ProcAttr{
		Env:   os.Environ(),
		Files: []*os.File{os.Stdin, os.Stdout, os.Stderr},
		Sys:   &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL},
	})
	if err != nil {
		return fail(stderr, ExitSetup, "start %s: %v", path, err)
	}
	defer command.Release()
	code = supervise(parent, command, parentExited, signals, stops, childChanged, stderr)
	teardown(stderr)
	return code
}

// Check for requests made during path lookup or pipe setup. Linux reparents
// children on parent exit. The pidfd watcher handles exits racing this check.
func checkBeforeStart(parent int, watchActive bool, stops <-chan os.Signal, command string, stderr io.Writer) int {
	if ppid := os.Getppid(); ppid != parent {
		return parentGone(stderr, parent, ppid, command)
	}
	for range cap(stops) {
		select {
		case sig := <-stops:
			if sig == unix.SIGUSR1 {
				return 128 + int(unix.SIGKILL)
			}
			ppid := os.Getppid()
			if !watchActive || ppid != parent {
				return parentGone(stderr, parent, ppid, command)
			}
		default:
			return 0
		}
	}
	return 0
}

func watchParent(pidfd, stop int) <-chan error {
	done := make(chan error, 1)
	go func() {
		defer close(done)
		// Closing the stop pipe's writer wakes the poll with POLLHUP.
		fds := []unix.PollFd{{Fd: int32(pidfd), Events: unix.POLLIN}, {Fd: int32(stop)}}
		for {
			_, err := unix.Poll(fds, -1)
			if !errors.Is(err, unix.EINTR) {
				done <- err
				return
			}
		}
	}()
	return done
}

func supervise(parent int, command *os.Process, parentExited <-chan error, signals, stops <-chan os.Signal, childChanged chan os.Signal, stderr io.Writer) int {
	for {
		select {
		case err := <-parentExited:
			if err != nil {
				fail(stderr, 0, "watching parent %d: %v; stopping command", parent, err)
			}
			return stoppedStatus(command)
		case sig := <-stops:
			// A parent thread can exit while its process is still alive.
			// With a pidfd, let it report the process exit.
			if sig == unix.SIGUSR1 || parentExited == nil || os.Getppid() != parent {
				return stoppedStatus(command)
			}
			if afterIgnoredDeath != nil {
				afterIgnoredDeath()
			}
		case sig := <-signals:
			_ = command.Signal(sig)
		case <-childChanged:
			// Drain coalesced notifications in batches, giving the other
			// events a chance even when descendants are constantly exiting.
			for i := 0; i < reapBatchSize; i++ {
				pid, status, err := waitChild(-1)
				if err != nil {
					if !errors.Is(err, unix.ECHILD) {
						_ = command.Signal(unix.SIGKILL)
					}
					return fail(stderr, ExitSetup, "wait4: %v", err)
				}
				if pid == command.Pid && (status.Exited() || status.Signaled()) {
					return statusCode(status)
				}
				if pid == 0 {
					break
				}
				if i == reapBatchSize-1 {
					select {
					case childChanged <- unix.SIGCHLD:
					default:
					}
				}
			}
		}
	}
}

// stoppedStatus preserves the command's status if it already exited when
// a teardown request arrived, even if SIGCHLD is still pending.
func stoppedStatus(command *os.Process) int {
	if beforeStoppedStatus != nil {
		beforeStoppedStatus(command.Pid)
	}
	pid, status, err := waitChild(command.Pid)
	if pid == command.Pid && (status.Exited() || status.Signaled()) {
		return statusCode(status)
	}
	// Start teardown by signalling the command, even if /proc cannot
	// enumerate it. The PID is safe even without a pidfd while unreaped.
	if !errors.Is(err, unix.ECHILD) {
		_ = command.Signal(unix.SIGKILL)
	}
	return 128 + int(unix.SIGKILL)
}

func statusCode(status unix.WaitStatus) int {
	if status.Signaled() {
		return 128 + int(status.Signal())
	}
	return status.ExitStatus()
}

func waitChild(pid int) (int, unix.WaitStatus, error) {
	for {
		var status unix.WaitStatus
		got, err := unix.Wait4(pid, &status, unix.WNOHANG|unix.WALL, nil)
		if !errors.Is(err, unix.EINTR) {
			return got, status, err
		}
	}
}

func teardown(stderr io.Writer) {
	scan := descendantScan{deadline: time.Now().Add(teardownLimit)}
	for time.Now().Before(scan.deadline) {
		scan.killDescendants(os.Getpid())
		for time.Now().Before(scan.deadline) {
			pid, _, err := waitChild(-1)
			if errors.Is(err, unix.ECHILD) {
				return
			}
			if err != nil {
				fail(stderr, 0, "reaping descendants: %v", err)
				return
			}
			if pid == 0 {
				break
			}
		}
		time.Sleep(min(5*time.Millisecond, time.Until(scan.deadline)))
	}
	if len(scan.denied) > 0 {
		fail(stderr, 0, "timed out after %s killing and reaping descendants (permission denied for PIDs %v)", teardownLimit, scan.denied)
	} else {
		fail(stderr, 0, "timed out after %s killing and reaping descendants", teardownLimit)
	}
}

type descendant struct {
	pid, parent int
}

type descendantScan struct {
	deadline time.Time
	byParent map[int][]int
	denied   []int
}

// Children lists are best effort while processes exit or fork. Once a
// parent dies, its surviving children are adopted here and the next pass
// sees them. Only wait4(ECHILD) proves the tree is gone.
func (scan *descendantScan) killDescendants(self int) {
	scan.byParent = nil
	pending := scan.appendChildren(nil, self)
	for len(pending) > 0 && time.Now().Before(scan.deadline) {
		child := pending[len(pending)-1]
		pending = scan.killDescendant(pending[:len(pending)-1], child)
	}
}

func (scan *descendantScan) killDescendant(pending []descendant, child descendant) []descendant {
	// Descendants can be reaped by their own parents while we walk the
	// tree. Pin the target of the signal when possible, and check that
	// the observed PID still belongs to the expected parent.
	pidfd, openErr := openDescendantPIDFD(child.pid, 0)
	if openErr == nil {
		defer unix.Close(pidfd)
	}
	if errors.Is(openErr, unix.ESRCH) || procParent(child.pid) != child.parent {
		return pending
	}
	next := scan.appendChildren(pending, child.pid)
	var killErr error
	if openErr == nil {
		killErr = unix.PidfdSendSignal(pidfd, unix.SIGKILL, nil, 0)
	} else {
		// Recheck after reading children, before acting on a numeric PID.
		if procParent(child.pid) != child.parent {
			return pending
		}
		killErr = killDescendantPID(child.pid, unix.SIGKILL)
	}
	if killErr == nil {
		return next
	}
	if errors.Is(killErr, unix.EPERM) {
		// An inaccessible parent may have children we can still kill.
		if len(scan.denied) < 8 && !slices.Contains(scan.denied, child.pid) {
			scan.denied = append(scan.denied, child.pid)
		}
		return next
	}
	// The next scan sees any children reparented in the meantime.
	return pending
}

func procParent(pid int) int {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return -1
	}
	stat := string(data)
	fields := strings.Fields(stat[strings.LastIndexByte(stat, ')')+1:])
	if len(fields) < 2 {
		return -1
	}
	parent, err := strconv.Atoi(fields[1])
	if err != nil {
		return -1
	}
	return parent
}

func (scan *descendantScan) appendChildren(pending []descendant, pid int) []descendant {
	for _, child := range scan.childrenOf(pid) {
		pending = append(pending, descendant{pid: child, parent: pid})
	}
	if afterChildren != nil {
		afterChildren(pid)
	}
	return pending
}

func (scan *descendantScan) childrenOf(pid int) []int {
	if scan.byParent != nil {
		return scan.byParent[pid]
	}
	tasks := fmt.Sprintf("/proc/%d/task", pid)
	threads, _ := os.ReadDir(tasks)
	var children []int
	for _, thread := range threads {
		if time.Now().After(scan.deadline) {
			break
		}
		data, err := readProcChildren(tasks + "/" + thread.Name() + "/children")
		if errors.Is(err, unix.ENOENT) {
			// Kernels without CONFIG_PROC_CHILDREN omit these files.
			// Take one snapshot for this pass and keep validating each PID
			// against its expected parent before signalling it.
			scan.byParent = scan.readParents()
			return scan.byParent[pid]
		}
		for _, field := range strings.Fields(string(data)) {
			if child, err := strconv.Atoi(field); err == nil && child > 1 {
				children = append(children, child)
			}
		}
	}
	return children
}

func (scan *descendantScan) readParents() map[int][]int {
	byParent := make(map[int][]int)
	entries, _ := os.ReadDir("/proc")
	for _, entry := range entries {
		if time.Now().After(scan.deadline) {
			break
		}
		if pid, err := strconv.Atoi(entry.Name()); err == nil && pid > 1 {
			if parent := procParent(pid); parent > 0 {
				byParent[parent] = append(byParent[parent], pid)
			}
		}
	}
	return byParent
}
