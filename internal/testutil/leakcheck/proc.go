package leakcheck

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type process struct {
	pid, ppid, pgid, sid int
	state                string
	start                uint64
	// execed reports that the process runs an image of its own: it has
	// exec'd since it was forked, and the exec has finished replacing what
	// it inherited. See ownState.
	execed bool
	// image is start_code and start_stack of the address space.
	image [2]uint64
}

// pfForkNoExec is PF_FORKNOEXEC in the flags field of /proc/<pid>/stat: set
// by fork, cleared by exec.
const pfForkNoExec = 0x40

// key distinguishes a recycled pid from the process recorded earlier.
func (p process) key() string { return fmt.Sprintf("%d:%d", p.pid, p.start) }

func readStat(pid int) (process, bool) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return process{}, false
	}
	return parseStat(pid, data)
}

func parseStat(pid int, data []byte) (process, bool) {
	// comm may contain spaces and parentheses; the last ')' ends it.
	i := bytes.LastIndexByte(data, ')')
	if i < 0 || i+2 > len(data) {
		return process{}, false
	}
	// fields[0] is field 3 (state) of proc_pid_stat(5).
	fields := strings.Fields(string(data[i+2:]))
	if len(fields) < 26 {
		return process{}, false
	}
	p := process{pid: pid, state: fields[0]}
	p.ppid, _ = strconv.Atoi(fields[1])
	p.pgid, _ = strconv.Atoi(fields[2])
	p.sid, _ = strconv.Atoi(fields[3])
	p.start, _ = strconv.ParseUint(fields[19], 10, 64)
	flags, _ := strconv.ParseUint(fields[6], 10, 64)
	startCode, _ := strconv.ParseUint(fields[23], 10, 64)
	startStack, _ := strconv.ParseUint(fields[25], 10, 64)
	p.image = [2]uint64{startCode, startStack}
	// The kernel sets start_code only when it maps the new image, after the
	// exec cleared PF_FORKNOEXEC and closed close-on-exec descriptors. It is
	// 0 without an address space and 1 when the reader may not see it.
	p.execed = flags&pfForkNoExec == 0 && startCode > 1
	return p, true
}

// selfImage is the test process's image, which a child it forks shares or
// copies until its exec.
var selfImage = func() [2]uint64 {
	p, _ := readStat(os.Getpid())
	return p.image
}()

// ownState reports whether the cwd, descriptors and command line of p are
// its own evidence of ownership. A child of the test process shows the test
// process's until its exec completes: between fork and exec it holds a copy
// of every descriptor of the test process, including the files of other
// parallel tests, keeps the package directory as cwd until it changes to
// cmd.Dir, and (sharing the test process's memory) shows its command line.
// The exec closes the inherited copies only after it clears PF_FORKNOEXEC,
// hence the start_code check too. The stat is read before cwd, cmdline and
// openFiles, and those only move forward from fork to exec, so a finished
// exec in it proves that they show the child's own state. Pid, start time,
// group and session survive the exec: pinning the child earlier would bind
// it to the wrong test for good.
//
// A deeper descendant inherits from a process other than the test process,
// whose state is evidence of the same owner. So does a never-exec'd orphan
// reparented to the test process (the subreaper), such as a subshell whose
// shell exited: it runs its former parent's image, not the test's. A child
// in the middle of its exec has a new, still empty address space (start_code
// 0) and stays unattributed.
//
// A direct child that the test process may not inspect, such as a setuid
// or file-capability program or one that made itself non-dumpable, reads
// start_code 1 for its whole life and is never attributed; its cwd and
// descriptors are unreadable anyway. So is a never-exec'd orphan forked by
// such a process, whose copied address space is not dumpable either. Their
// leaks are reported at package end.
func ownState(p process) bool {
	if p.execed || p.ppid != os.Getpid() {
		return true
	}
	return p.image[0] > 1 && p.image != selfImage
}

// descendants is the sampler's cheap walk (about 50 times faster) along
// /proc/<pid>/task/<tid>/children (CONFIG_PROC_CHILDREN), falling back to
// scanDescendants. It can miss a process while it is reparented to the
// subreaper, so verdicts use scanDescendants.
func descendants() []process {
	if _, err := os.Stat("/proc/self/task/" + strconv.Itoa(os.Getpid()) + "/children"); err != nil {
		return scanDescendants()
	}
	var out []process
	queue := []int{os.Getpid()}
	for len(queue) > 0 {
		next := queue[0]
		queue = queue[1:]
		tasks, _ := os.ReadDir(fmt.Sprintf("/proc/%d/task", next))
		for _, task := range tasks {
			data, _ := os.ReadFile(fmt.Sprintf("/proc/%d/task/%s/children", next, task.Name()))
			for _, field := range strings.Fields(string(data)) {
				pid, _ := strconv.Atoi(field)
				if p, ok := readStat(pid); ok && p.ppid == next {
					out = append(out, p)
					queue = append(queue, pid)
				}
			}
		}
	}
	return out
}

// scanDescendants lists every live or zombie process below the test
// process, sorted by pid. Run makes it a subreaper, so orphans stay below it.
// Scanning all of /proc cannot miss a process that is being reparented.
func scanDescendants() []process {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	byParent := make(map[int][]process)
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		if p, ok := readStat(pid); ok {
			byParent[p.ppid] = append(byParent[p.ppid], p)
		}
	}
	var out []process
	queue := []int{os.Getpid()}
	for len(queue) > 0 {
		next := queue[0]
		queue = queue[1:]
		for _, child := range byParent[next] {
			out = append(out, child)
			queue = append(queue, child.pid)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].pid < out[j].pid })
	return out
}

func cmdline(pid int) string {
	data, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	return strings.TrimSpace(strings.ReplaceAll(string(data), "\x00", " "))
}

func cwd(pid int) string {
	target, _ := os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid))
	return target
}

// openFiles maps descriptor numbers to readlink targets. For the test process
// itself the directory descriptor used for listing is omitted.
func openFiles(pid int) map[int]string {
	dir := fmt.Sprintf("/proc/%d/fd", pid)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	fds := make(map[int]string, len(entries))
	for _, entry := range entries {
		fd, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		target, err := os.Readlink(filepath.Join(dir, entry.Name()))
		if err != nil || target == dir {
			continue
		}
		fds[fd] = target
	}
	return fds
}

func selfFiles() map[int]string { return openFiles(os.Getpid()) }

const deletedSuffix = " (deleted)"

// deleted reports a regular file whose last link was removed while open.
// Anonymous memory files are not filesystem leaks.
func deleted(target string) bool {
	return strings.HasSuffix(target, deletedSuffix) && strings.HasPrefix(target, "/") && !strings.HasPrefix(target, "/memfd:")
}

func describe(p process) string {
	cur, ok := readStat(p.pid)
	if ok && cur.start == p.start {
		p = cur
	}
	line := fmt.Sprintf("pid=%d ppid=%d pgid=%d sid=%d state=%s cwd=%q cmdline=%q", p.pid, p.ppid, p.pgid, p.sid, p.state, cwd(p.pid), cmdline(p.pid))
	if seen := lastSeen(p); seen != "" && p.state == "Z" {
		line += "\n      last seen alive as " + seen
	}
	var held []string
	for fd, target := range openFiles(p.pid) {
		if deleted(target) {
			held = append(held, fmt.Sprintf("fd %d -> %s", fd, target))
		}
	}
	sort.Strings(held)
	if len(held) > 0 {
		line += "\n      holds deleted files: " + strings.Join(held, ", ")
	}
	return line
}

// foreignReapWait gives a killed process's real waiter, such as the cmd.Wait
// of a call the test abandoned, time to collect it first. Reaping it earlier
// would make that Wait fail with ECHILD.
const foreignReapWait = 100 * time.Millisecond

// killAndReap sends KILL to exactly these processes. After foreignReapWait it
// reaps those that are (or became, once their killed parents are gone)
// children of the test process; it never waits for other pids. It returns
// processes that still exist after the grace.
func killAndReap(procs []process) []process {
	for _, p := range procs {
		if cur, ok := readStat(p.pid); ok && cur.start == p.start {
			_ = syscall.Kill(p.pid, syscall.SIGKILL)
		}
	}
	pending := procs
	start := time.Now()
	for len(pending) > 0 {
		reap := time.Since(start) >= foreignReapWait
		var next []process
		for _, p := range pending {
			if reap {
				if pid, err := syscall.Wait4(p.pid, nil, syscall.WNOHANG, nil); err == nil && pid == p.pid {
					continue
				}
			}
			if cur, ok := readStat(p.pid); !ok || cur.start != p.start {
				continue
			}
			next = append(next, p)
		}
		pending = next
		if len(pending) == 0 || time.Since(start) > foreignReapWait+grace {
			break
		}
		time.Sleep(pollInterval)
	}
	return pending
}

// peakRSS returns VmHWM in bytes.
func peakRSS() int64 {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		if rest, ok := strings.CutPrefix(line, "VmHWM:"); ok {
			fields := strings.Fields(rest)
			if len(fields) > 0 {
				kib, _ := strconv.ParseInt(fields[0], 10, 64)
				return kib << 10
			}
		}
	}
	return 0
}

func mib(n int64) string { return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20)) }
