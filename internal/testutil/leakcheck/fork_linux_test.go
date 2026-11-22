package leakcheck

import (
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// A child of the test process shows the test process's cwd and descriptors
// until its exec completes. Frozen at the chdir to cmd.Dir that precedes its
// exec, the child holds a copy of the test process's descriptor for another
// test's file, and its cwd is still the package directory; it must be pinned
// to no test, then to its own test once it runs its own image.
func TestChildBeforeExecIsNotAttributed(t *testing.T) {
	own, other := scratch(t), scratch(t)
	ownTest := named{newRecorder(t), "TestParent/short"}
	otherTest := named{newRecorder(t), "TestParent/a-longer-sibling-name"}
	Track(ownTest, own)
	Track(otherTest, other)
	// A parallel test's open file, which every child inherits until exec.
	foreign, err := os.Create(filepath.Join(other, "output"))
	if err != nil {
		t.Fatal(err)
	}
	defer foreign.Close()
	cmd := exec.Command("/bin/sleep", "30")
	cmd.Dir = own
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	frozen := startFrozenAtChdir(t, cmd)
	child := frozen.process

	if dir := cwd(child.pid); dir == own {
		t.Fatalf("the child changed to %s before it was frozen", dir)
	}
	sample()
	if owner := ownerOf(child); owner != nil {
		t.Fatalf("child before exec pinned to %s", owner.state.t.Name())
	}

	if err := frozen.resume(); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	defer func() {
		// On Linux, Kill signals through the pidfd, never a reused pid.
		_ = cmd.Process.Kill()
		<-waited
	}()
	pid := cmd.Process.Pid
	waitExeced(t, pid)
	sample()
	if owner := ownerOf(child); owner == nil || owner.state.t != ownTest {
		t.Fatalf("process not attributed to its own test: %v", owner)
	}
	_ = foreign.Close()
	if report := otherTest.finish(); report != "" {
		t.Fatalf("another test's check reported the process:\n%s", report)
	}
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("another test's check killed the process: %v", err)
	}
	want(t, ownTest.finish(), "processes", "sleep 30")
}

// A subshell that never execs, orphaned to the test process (the subreaper)
// because its shell exited at once, shows its former parent's cwd and
// descriptors, which are evidence of its test. It is attributed, reported
// and killed by its test's check, whether or not the shell had its own
// session.
func TestNeverExecOrphanIsAttributed(t *testing.T) {
	for _, setsid := range []bool{false, true} {
		t.Run(fmt.Sprintf("setsid=%v", setsid), func(t *testing.T) {
			root := scratch(t)
			r := named{newRecorder(t), "TestParallel/orphan"}
			Track(r, root)
			if err := syscall.Mkfifo(filepath.Join(root, "fifo"), 0o600); err != nil {
				t.Fatal(err)
			}
			// The subshell blocks opening the FIFO for reading.
			cmd := exec.Command("/bin/sh", "-c", `(read -r _ < fifo) & echo $! > sub.pid; exit 0`)
			cmd.Dir = root
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: setsid}
			if err := cmd.Run(); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(root, "sub.pid"))
			if err != nil {
				t.Fatal(err)
			}
			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err != nil || pid <= 0 {
				t.Fatalf("subshell pid %q: %v", data, err)
			}
			// The shell has been reaped, so the subshell is the test
			// process's child now, and the test process reaps it.
			sub, ok := readStat(pid)
			defer func() {
				// The check normally reaps it; never signal a reused pid.
				if cur, ok := readStat(pid); ok && cur.start == sub.start {
					_ = syscall.Kill(pid, syscall.SIGKILL)
					_, _ = syscall.Wait4(pid, nil, 0, nil)
				}
			}()
			if !ok || sub.ppid != os.Getpid() {
				t.Fatalf("subshell %d is not an orphan of the test process: %+v", pid, sub)
			}
			if sub.execed {
				t.Fatalf("subshell %d has exec'd", pid)
			}
			if owner := attribute(sub, activeCheckers()); owner == nil || owner.state.t != r {
				t.Fatalf("never-exec'd orphan not attributed to its test: %v", owner)
			}
			want(t, r.finish(), "processes", fmt.Sprintf("pid=%d", pid))
			if cur, ok := readStat(pid); ok && cur.start == sub.start && cur.state != "Z" {
				t.Fatalf("the check left the orphan running: %+v", cur)
			}
		})
	}
}

// statLine builds a /proc/<pid>/stat line with the fields ownState reads.
func statLine(pid, ppid int, flags, startCode, startStack uint64) string {
	fields := make([]string, 52)
	for i := range fields {
		fields[i] = "0"
	}
	fields[0], fields[1], fields[2] = strconv.Itoa(pid), "(a) (b)", "S"
	fields[3] = strconv.Itoa(ppid)
	fields[8] = strconv.FormatUint(flags, 10)
	fields[21] = "42"
	fields[25] = strconv.FormatUint(startCode, 10)
	fields[27] = strconv.FormatUint(startStack, 10)
	return strings.Join(fields, " ") + "\n"
}

func TestOwnStateFromStat(t *testing.T) {
	self := os.Getpid()
	code, stack := selfImage[0], selfImage[1]
	for _, tc := range []struct {
		name               string
		ppid               int
		flags, code, stack uint64
		execed, own        bool
	}{
		{"forked child before exec, sharing the test's image", self, pfForkNoExec, code, stack, false, false},
		{"child in exec, PF_FORKNOEXEC set, new empty image", self, pfForkNoExec, 0, 0, false, false},
		{"child in exec, PF_FORKNOEXEC cleared, image not mapped", self, 0, 0, 0, false, false},
		{"child that may not be inspected", self, 0, 1, 1, false, false},
		{"child after exec", self, 0, 0x400000, 0x7ffd0000, true, true},
		{"never-exec'd orphan running another image", self, pfForkNoExec, 0x5000, 0x7ffc0000, false, true},
		{"never-exec'd orphan of another binary with the same start_code", self, pfForkNoExec, code, stack + 0x1000, false, true},
		{"never-exec'd orphan with another start_code at the same stack", self, pfForkNoExec, code + 0x1000, stack, false, true},
		{"grandchild before exec", self + 1, pfForkNoExec, code, stack, false, true},
	} {
		p, ok := parseStat(1234, []byte(statLine(1234, tc.ppid, tc.flags|0x400000, tc.code, tc.stack)))
		if !ok {
			t.Fatalf("%s: unparsable", tc.name)
		}
		if p.ppid != tc.ppid || p.start != 42 || p.state != "S" {
			t.Fatalf("%s: misparsed %+v", tc.name, p)
		}
		if p.execed != tc.execed || ownState(p) != tc.own {
			t.Errorf("%s: execed %v ownState %v, want %v %v", tc.name, p.execed, ownState(p), tc.execed, tc.own)
		}
	}
}

// seccompNotif and seccompNotifResp are struct seccomp_notif and struct
// seccomp_notif_resp from <linux/seccomp.h>.
type seccompNotif struct {
	id    uint64
	pid   uint32
	flags uint32
	data  struct {
		nr                 int32
		arch               uint32
		instructionPointer uint64
		args               [6]uint64
	}
}

type seccompNotifResp struct {
	id    uint64
	val   int64
	error int32
	flags uint32
}

// startFrozenAtChdir is freezeAtChdir for a test: it skips where seccomp
// user notification is unavailable, except in CI, and releases the child at
// cleanup.
func startFrozenAtChdir(t *testing.T, cmd *exec.Cmd) *frozenChild {
	t.Helper()
	f, err := freezeAtChdir(cmd, "")
	if err != nil {
		skipUnavailable(t, err)
		t.Fatal(err)
	}
	t.Cleanup(f.release)
	return f
}

func skipUnavailable(t *testing.T, err error) {
	t.Helper()
	unavailable := errors.Is(err, unix.EBUSY) || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOSYS) || errors.Is(err, errNoAuditArch)
	if ci := os.Getenv("CI"); unavailable && (ci == "" || ci == "false" || ci == "0") {
		t.Skipf("seccomp user notification unavailable: %v", err)
	}
}

// frozenChild is the child of cmd, stopped in the chdir to cmd.Dir after
// fork and before exec.
type frozenChild struct {
	process
	cmd      *exec.Cmd
	listener uintptr
	notifID  uint64
	started  chan error
	exit     chan struct{}
	restore  func()
	resumed  bool
	startErr error
}

// freezeAtChdir starts cmd and returns its child frozen in the chdir to
// cmd.Dir. A seccomp filter on the starting thread, which the child
// inherits, turns chdir into a user notification that the test answers.
// fault ("recv" or "stat") makes that step fail, to test the abort path. On
// error no process of cmd is left.
//
// Go forks with CLONE_VFORK in a raw system call, so until the child execs
// the starting thread is stuck holding its P, and a stop-the-world would
// wait for it forever. While the child is frozen, garbage collection is off
// (also the collection a memory limit would force) and another P runs the
// test. Nothing may run in parallel that calls runtime.GC.
func freezeAtChdir(cmd *exec.Cmd, fault string) (*frozenChild, error) {
	procs := runtime.GOMAXPROCS(max(2, runtime.GOMAXPROCS(0)))
	gcPercent := debug.SetGCPercent(-1)
	memoryLimit := debug.SetMemoryLimit(math.MaxInt64)
	f := &frozenChild{
		cmd:     cmd,
		started: make(chan error, 1),
		exit:    make(chan struct{}),
		restore: func() {
			debug.SetMemoryLimit(memoryLimit)
			debug.SetGCPercent(gcPercent)
			runtime.GOMAXPROCS(procs)
		},
	}
	type setup struct {
		listener uintptr
		tid      int
		err      error
	}
	setups := make(chan setup, 1)
	go func() {
		// The thread keeps the filter; it exits with this goroutine because
		// it stays locked. It lives until release: the children of an
		// exiting thread move to another thread, and the sampler's walk of
		// per-thread children lists may miss them meanwhile.
		runtime.LockOSThread()
		listener, err := chdirListener()
		if err != nil {
			setups <- setup{err: err}
			return
		}
		setups <- setup{listener: listener, tid: unix.Gettid()}
		f.started <- cmd.Start()
		<-f.exit
	}()
	s := <-setups
	if s.err != nil {
		f.restore()
		return nil, fmt.Errorf("install the chdir filter: %w", s.err)
	}
	f.listener = s.listener
	// abort kills the child and waits for cmd.Start. Closing the listener
	// would not release the chdir: the child holds its own copy of the
	// listener's descriptor until it execs. Killing it ends the vfork wait.
	abort := func(pid int, err error) (*frozenChild, error) {
		pids := []int{pid}
		if pid <= 0 {
			// The pid is unknown: the forking thread has no other child.
			data, _ := os.ReadFile(fmt.Sprintf("/proc/self/task/%d/children", s.tid))
			pids = nil
			for _, field := range strings.Fields(string(data)) {
				if n, _ := strconv.Atoi(field); n > 0 {
					pids = append(pids, n)
				}
			}
		}
		for _, pid := range pids {
			if pid > 0 {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
		f.reap()
		f.resumed = true
		close(f.exit)
		_ = unix.Close(int(f.listener))
		f.restore()
		return nil, errors.Join(err, f.startErr)
	}
	fds := []unix.PollFd{{Fd: int32(f.listener), Events: unix.POLLIN}}
	if n, err := unix.Poll(fds, 10_000); n != 1 || err != nil {
		return abort(0, fmt.Errorf("the child did not reach chdir: poll %d %v", n, err))
	}
	var notif seccompNotif
	errno := unix.EINTR
	if fault != "recv" {
		_, _, errno = unix.Syscall(unix.SYS_IOCTL, f.listener, unix.SECCOMP_IOCTL_NOTIF_RECV, uintptr(unsafe.Pointer(&notif)))
	}
	if errno != 0 {
		return abort(0, fmt.Errorf("receive the chdir notification: %w", errno))
	}
	f.notifID = notif.id
	child, ok := readStat(int(notif.pid))
	if fault == "stat" || !ok || child.pid <= 0 {
		return abort(int(notif.pid), fmt.Errorf("no /proc entry for the frozen child %d", notif.pid))
	}
	f.process = child
	return f, nil
}

// reap waits for cmd.Start after the child was killed before its exec: Start
// then either reaped it itself or returns it for Wait.
func (f *frozenChild) reap() {
	f.startErr = <-f.started
	if f.startErr == nil {
		_ = f.cmd.Wait()
	}
}

// resume lets the chdir proceed and returns the error of cmd.Start. After a
// failure the child is gone and reaped.
func (f *frozenChild) resume() error {
	if f.resumed {
		return f.startErr
	}
	f.resumed = true
	defer f.restore()
	resp := seccompNotifResp{id: f.notifID, flags: unix.SECCOMP_USER_NOTIF_FLAG_CONTINUE}
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, f.listener, unix.SECCOMP_IOCTL_NOTIF_SEND, uintptr(unsafe.Pointer(&resp))); errno != 0 {
		// cmd.Start has not returned, so the child is not reaped and its
		// pid not reused.
		_ = syscall.Kill(f.pid, syscall.SIGKILL)
		f.reap()
		f.startErr = errors.Join(fmt.Errorf("resume the chdir: %w", errno), f.startErr)
		return f.startErr
	}
	f.startErr = <-f.started
	return f.startErr
}

// release kills and reaps a child that was never resumed, then lets the
// forking thread exit.
func (f *frozenChild) release() {
	if !f.resumed {
		f.resumed = true
		_ = syscall.Kill(f.pid, syscall.SIGKILL)
		f.reap()
		f.restore()
	}
	close(f.exit)
	_ = unix.Close(int(f.listener))
}

// When a step after the fork fails, the frozen child is killed and reaped
// at once, whether or not its pid is known; the failure is quick and leaves
// no process behind.
func TestFrozenChildAbortIsQuick(t *testing.T) {
	for _, fault := range []string{"recv", "stat"} {
		t.Run(fault, func(t *testing.T) {
			before := make(map[string]bool)
			for _, p := range scanDescendants() {
				before[p.key()] = true
			}
			cmd := exec.Command("/bin/sleep", "30")
			cmd.Dir = scratch(t)
			type result struct {
				f   *frozenChild
				err error
			}
			done := make(chan result, 1)
			go func() {
				f, err := freezeAtChdir(cmd, fault)
				done <- result{f, err}
			}()
			var r result
			select {
			case r = <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("the abort did not return within 10s")
			}
			if r.err == nil {
				r.f.release()
				t.Fatal("the fault did not fail the freeze")
			}
			if strings.HasPrefix(r.err.Error(), "install the chdir filter") {
				skipUnavailable(t, r.err)
				t.Fatal(r.err)
			}
			t.Logf("aborted: %v", r.err)
			for _, p := range scanDescendants() {
				if !before[p.key()] {
					t.Errorf("left behind: %s", describe(p))
				}
			}
		})
	}
}

var errNoAuditArch = errors.New("no seccomp audit architecture for " + runtime.GOARCH)

// auditArch is the seccomp_data.arch of native system calls.
var auditArch = map[string]uint32{
	"amd64":   unix.AUDIT_ARCH_X86_64,
	"arm64":   unix.AUDIT_ARCH_AARCH64,
	"riscv64": unix.AUDIT_ARCH_RISCV64,
}

// chdirListener installs, on the calling thread only, a filter that reports
// every native chdir to the returned listener.
func chdirListener() (uintptr, error) {
	arch, ok := auditArch[runtime.GOARCH]
	if !ok {
		return 0, errNoAuditArch
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return 0, err
	}
	filter := []unix.SockFilter{
		// seccomp_data.arch, at offset 4; another architecture is allowed.
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 4},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jt: 0, Jf: 3, K: arch},
		// seccomp_data.nr, at offset 0.
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 0},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jt: 0, Jf: 1, K: unix.SYS_CHDIR},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_USER_NOTIF},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW},
	}
	prog := unix.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}
	listener, _, errno := unix.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER, unix.SECCOMP_FILTER_FLAG_NEW_LISTENER, uintptr(unsafe.Pointer(&prog)))
	if errno != 0 {
		return 0, errno
	}
	return listener, nil
}
