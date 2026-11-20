package bashtool

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/EricWvi/circus/internal/daemon/jobs"
	"github.com/EricWvi/circus/internal/testutil/wait"
)

// collectorChild is a .bashrc line that leaves a TERM-ignoring member in the
// collector's process group and announces it as child.<pid>.
const collectorChild = `(trap '' TERM; echo $BASHPID > "$HOME/pid.$BASHPID"; mv "$HOME/pid.$BASHPID" "$HOME/child.$BASHPID"; while :; do sleep 0.01; done)`

// collectorBlocks keeps the collector running until it is timed out or cancelled.
const collectorBlocks = `while [ ! -f "$HOME/never" ]; do sleep 0.01; done`

func collectorBash(t *testing.T, rc string) (*Bash, string) {
	t.Helper()
	root := unitTempDir(t)
	if err := os.WriteFile(filepath.Join(root, ".bashrc"), []byte(rc+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	b := NewBash(jobs.NewManager(jobs.Options{}), Options{Home: root, StateDir: filepath.Join(root, "state"), TempDir: root, Shell: "/bin/bash"})
	t.Cleanup(func() {
		if err := b.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return b, root
}

type collectorMember struct{ pid, start string }

// collectorMembers adds the members announced since known was filled, by pid
// and start ticks; a member already gone when first seen has no start ticks.
func collectorMembers(t *testing.T, root string, known map[string]collectorMember) map[string]collectorMember {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(root, "child.*"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		pid := strings.TrimPrefix(filepath.Base(path), "child.")
		if _, ok := known[pid]; ok {
			continue
		}
		m := collectorMember{pid: pid}
		if p, err := strconv.Atoi(pid); err == nil {
			if stat, ok := readProcStat(p); ok {
				m.start = stat.start
			}
		}
		known[pid] = m
	}
	return known
}

type procStat struct{ state, start string }

func readProcStat(pid int) (procStat, bool) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return procStat{}, false
	}
	s := string(data)
	fields := strings.Fields(s[strings.LastIndexByte(s, ')')+2:])
	if len(fields) < 20 {
		return procStat{}, false
	}
	return procStat{fields[0], fields[19]}, true
}

// requireGone fails for a member that still runs or is an unreaped zombie.
func requireGone(t *testing.T, members map[string]collectorMember) {
	t.Helper()
	if len(members) == 0 {
		t.Fatal("no collector member was announced")
	}
	for _, m := range members {
		pid, _ := strconv.Atoi(m.pid)
		if stat, ok := readProcStat(pid); ok && m.start != "" && stat.start == m.start {
			t.Errorf("collector member %s remains in state %s", m.pid, stat.state)
		}
	}
}

// A collector that exits while members of its process group remain must not
// leave them behind: a running member would outlive every collection, and one
// the shell did not reap becomes a zombie of a daemon that is PID 1 or a
// child subreaper. A member holding the collector's output must not stall
// collection either.
func TestCollectorExitStopsAndReapsItsGroup(t *testing.T) {
	for name, rc := range map[string]string{
		// An interactive shell keeps a copy of its stderr as fd 255, which
		// every subshell inherits; closing it detaches the member completely.
		"detached member":  `(exec 255>&-; ` + collectorChild + `) >/dev/null 2>&1 &`,
		"inherited output": collectorChild + " &",
		// Exits once the collector shell has been waited for, as a process
		// substitution the shell did not reap does: a zombie left to the
		// daemon.
		"exits after the shell": `(exec 255>&-; shell=$$; echo $BASHPID > "$HOME/pid.$BASHPID"; mv "$HOME/pid.$BASHPID" "$HOME/child.$BASHPID"; while kill -0 $shell 2>/dev/null; do sleep 0.01; done) >/dev/null 2>&1 &`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			b, root := collectorBash(t, rc)
			_, _, lease, err := b.snapshots.load(wait.Ctx(t))
			if err != nil {
				t.Fatal(err)
			}
			lease.release()
			// Collection ends only after the shell's group is stopped and reaped.
			requireGone(t, collectorMembers(t, root, make(map[string]collectorMember)))
		})
	}
}

// Timed-out and cancelled collectors stop their whole group, including
// members that ignore TERM, and reap the members reparented to the daemon.
func TestCollectorTimeoutAndCancelStopTheirGroup(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		t.Parallel()
		b, root := collectorBash(t, collectorChild+" &\n"+collectorBlocks)
		attempts := make(chan *triggeredDeadline, 2)
		b.effects.collectorTimeout = func(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
			deadline := &triggeredDeadline{Context: parent, done: make(chan struct{}), deadline: time.Now().Add(d)}
			attempts <- deadline
			return context.WithCancel(deadline)
		}
		loaded := make(chan error, 1)
		go func() {
			_, _, lease, err := b.snapshots.load(context.Background())
			if lease != nil {
				lease.release()
			}
			loaded <- err
		}()
		// The interactive attempt and its fallback each time out once their
		// member is running.
		all := make(map[string]collectorMember)
		for attempt := range 2 {
			deadline := wait.Recv(t, attempts)
			wait.For(t, func() bool { return len(collectorMembers(t, root, all)) > attempt })
			deadline.expire()
		}
		if err := wait.Recv(t, loaded); err == nil {
			t.Fatal("timed-out collection succeeded")
		}
		requireGone(t, all)
	})
	t.Run("cancel", func(t *testing.T) {
		t.Parallel()
		b, root := collectorBash(t, collectorChild+" &\n"+collectorBlocks)
		b.Warm()
		members := make(map[string]collectorMember)
		wait.For(t, func() bool { return len(collectorMembers(t, root, members)) > 0 })
		// Shutdown cancels the warm-up and waits for it.
		if err := b.Shutdown(context.Background()); err != nil {
			t.Fatal(err)
		}
		requireGone(t, members)
	})
}
