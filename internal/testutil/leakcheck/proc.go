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
}

// key distinguishes a recycled pid from the process recorded earlier.
func (p process) key() string { return fmt.Sprintf("%d:%d", p.pid, p.start) }

func readStat(pid int) (process, bool) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return process{}, false
	}
	// comm may contain spaces and parentheses; the last ')' ends it.
	i := bytes.LastIndexByte(data, ')')
	if i < 0 || i+2 > len(data) {
		return process{}, false
	}
	fields := strings.Fields(string(data[i+2:]))
	if len(fields) < 20 {
		return process{}, false
	}
	p := process{pid: pid, state: fields[0]}
	p.ppid, _ = strconv.Atoi(fields[1])
	p.pgid, _ = strconv.Atoi(fields[2])
	p.sid, _ = strconv.Atoi(fields[3])
	p.start, _ = strconv.ParseUint(fields[19], 10, 64)
	return p, true
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
