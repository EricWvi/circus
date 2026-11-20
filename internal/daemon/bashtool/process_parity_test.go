package bashtool_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/EricWvi/circus/internal/testutil/wait"
)

// procStart retains Linux start ticks so a recycled PID cannot become a probe.
func procStart(pid string) string {
	data, err := os.ReadFile(filepath.Join("/proc", pid, "stat"))
	if err != nil {
		return ""
	}
	_, rest, ok := strings.Cut(string(data), ") ")
	fields := strings.Fields(rest)
	if !ok || len(fields) < 20 {
		return ""
	}
	return fields[19]
}

func ownedSleeps(cwd string) map[string]string { return inCwd(cwd, "sleep") }

// inCwd returns the processes running in cwd, by pid and start ticks, limited
// to command name comm unless it is empty. Zombies have no cwd.
func inCwd(cwd, comm string) map[string]string {
	owned := make(map[string]string)
	entries, _ := os.ReadDir("/proc")
	for _, entry := range entries {
		pid := entry.Name()
		if _, err := strconv.Atoi(pid); err != nil {
			continue
		}
		start := procStart(pid)
		dir, err := os.Readlink(filepath.Join("/proc", pid, "cwd"))
		if err != nil || dir != cwd {
			continue
		}
		name, _ := os.ReadFile(filepath.Join("/proc", pid, "comm"))
		if (comm == "" || strings.TrimSpace(string(name)) == comm) && start != "" && start == procStart(pid) {
			owned[pid] = start
		}
	}
	return owned
}

// waitOwnedSleeps waits until the probe child runs sleep. Until it has
// exec'd, the shell or the forked child still runs in cwd, and a busy machine
// can take any time to get there, so only an empty cwd ends the wait as a
// failure: the child was killed. A second scan confirms that, because a scan
// can list /proc just before the shell forks and read it just after it exits.
func waitOwnedSleeps(t *testing.T, cwd string) map[string]string {
	t.Helper()
	var owned map[string]string
	wait.For(t, func() bool {
		owned = ownedSleeps(cwd)
		return len(owned) > 0 || len(inCwd(cwd, "")) == 0 && len(inCwd(cwd, "")) == 0
	})
	if len(owned) == 0 {
		if owned = ownedSleeps(cwd); len(owned) == 0 {
			t.Fatal("probe child is not alive after the first result")
		}
	}
	return owned
}

// Apply only the host-row normalization described in the capture README.
// Ownership comes from isolated cwd and retained start ticks, never argv text.
func normalizeSleepTable(t *testing.T, body string, owned map[string]string) string {
	t.Helper()
	lines := strings.Split(body, "\n")
	if strings.Join(strings.Fields(lines[0]), " ") != "PID PGID SID PPID STAT COMMAND" {
		t.Fatalf("unexpected ps header: %q", lines[0])
	}
	// leakcheck.Run makes the test process a child subreaper, so it adopts the
	// orphan where native's host init (PID 1) did.
	labels := map[string]string{"1": "{{INIT_PID}}", strconv.Itoa(os.Getpid()): "{{INIT_PID}}"}
	var rows []string
	for _, line := range lines[1:] {
		fields := strings.Fields(line)
		if len(fields) < 6 {
			t.Fatalf("unexpected ps row: %q", line)
		}
		start, ok := owned[fields[0]]
		if !ok {
			continue
		}
		if start != procStart(fields[0]) {
			t.Fatalf("probe PID %s disappeared or was reused", fields[0])
		}
		labels[fields[0]] = "{{CHILD_PID}}"
		labels[fields[1]] = "{{CHILD_PGID}}"
		// SID is deliberately looked up by numeric identity, not assigned a
		// role: leaving the original group/session must fail golden comparison.
		for i := range 4 {
			if label, ok := labels[fields[i]]; ok {
				fields[i] = label
			}
		}
		rows = append(rows, strings.Join(fields, " "))
	}
	if len(rows) != len(owned) {
		t.Fatalf("owned children missing from ps table: %s", body)
	}
	return fmt.Sprintf("PID PGID SID PPID STAT COMMAND\n{{UNRELATED_SLEEP_ROWS}}\n%s", strings.Join(rows, "\n"))
}
