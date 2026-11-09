package bashtool_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
)

func TestEvalQuoting(t *testing.T) {
	w := newWorker(t)
	cases := []struct{ command, want string }{
		{`printf '%s' 'single "double"'`, `single "double"`},
		{`printf '%s' '$HOME $(exit 19) \backslash'`, `$HOME $(exit 19) \backslash`},
		{`printf '%s' "apostrophe's"`, `apostrophe's`},
		{"printf '%s\\n' first\nprintf '%s' second", "first\nsecond"},
	}
	for _, tc := range cases {
		r := w.bashCall(t, tc.command, false, 0)
		if r.Output.IsError || r.Output.Content[0].GetText() != tc.want {
			t.Errorf("command %q: %v", tc.command, r)
		}
	}
}

func TestSnapshotRecreatedOnlyWhenDeleted(t *testing.T) {
	w := newWorker(t)
	rc := filepath.Join(w.options.Home, ".bashrc")
	writeFile(t, rc, []byte("export SNAPSHOT_MARK=first\n"))
	r := w.bashCall(t, `echo "$SNAPSHOT_MARK"`, false, 0)
	if r.Output.Content[0].GetText() != "first" {
		t.Fatal(r)
	}
	original := w.snapshot
	writeFile(t, rc, []byte("export SNAPSHOT_MARK=second\n"))
	r = w.bashCall(t, `echo "$SNAPSHOT_MARK"`, false, 0)
	if r.Output.Content[0].GetText() != "first" || w.snapshot != original {
		t.Fatal("snapshot was not reused", r)
	}
	if err := os.Remove(original); err != nil {
		t.Fatal(err)
	}
	r = w.bashCall(t, `echo "$SNAPSHOT_MARK"`, false, 0)
	if r.Output.Content[0].GetText() != "second" || w.snapshot == original {
		t.Fatal("snapshot was not recreated", r)
	}
}

func TestInteractiveCollectorFallback(t *testing.T) {
	w := newWorker(t)
	writeFile(t, filepath.Join(w.options.Home, ".bash_profile"), []byte(`case $- in
 *i*) printf '%s\n' interactive >> "$HOME/attempts"; exit 7;;
 *) printf '%s\n' fallback >> "$HOME/attempts";;
esac
`))
	writeFile(t, filepath.Join(w.options.Home, ".bashrc"), []byte("export FALLBACK_MARK=ok\n"))
	r := w.bashCall(t, `echo "$FALLBACK_MARK"`, false, 0)
	if r.Output.IsError || r.Output.Content[0].GetText() != "ok" {
		t.Fatal(r)
	}
	data, err := os.ReadFile(filepath.Join(w.options.Home, "attempts"))
	if err != nil || string(data) != "interactive\nfallback\n" {
		t.Fatalf("attempts=%q err=%v", data, err)
	}
}

func waitFile(t *testing.T, path string) []byte {
	t.Helper()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
				return data
			}
		case <-timer.C:
			t.Fatalf("file never became ready: %s", path)
			return nil
		}
	}
}

func TestTimeoutKillsProcessGroup(t *testing.T) {
	w := newWorker(t)
	// Prime the snapshot so the deadline measures execution, not shell startup.
	w.bashCall(t, "true", false, 0)
	// Waiting in the TERM trap reaps the child, so ESRCH proves the sleep is gone
	// even on hosts whose PID 1 does not promptly reap adopted zombies.
	command := `trap 'wait; exit 143' TERM; sleep 30 & echo $! > child.pid; wait`
	r := w.bashCall(t, command, false, 150)
	if !r.GetBash().TimedOut || r.GetBash().ExitCode != 143 || !r.Output.IsError {
		t.Fatal(r)
	}
	pidData, err := os.ReadFile(filepath.Join(w.start, "child.pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidData)))
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("child %d remains after timeout: %v", pid, err)
	}
}

func TestCancelPreservesOutput(t *testing.T) {
	w := newWorker(t)
	w.bashCall(t, "true", false, 0)
	call := &circusv1.DeviceCall{CallId: "cancel", SessionId: "fixture-session", Tool: &circusv1.DeviceCall_Bash{Bash: &circusv1.BashRequest{Command: `trap 'wait; exit' TERM; echo before; sleep 30 & echo ready > ready; wait`, Cwd: w.start}}}
	if _, err := w.d.Submit(call); err != nil {
		t.Fatal(err)
	}
	waitFile(t, filepath.Join(w.start, "ready"))
	if !w.d.Cancel(call.CallId) {
		t.Fatal("cancel did not find running call")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	status, err := w.d.Wait(ctx, call.CallId)
	if err != nil {
		t.Fatal(err)
	}
	r := status.Result
	if !r.GetBash().Interrupted || !r.Output.IsError || r.Output.Content[0].GetText() != "before\nCommand was interrupted" {
		t.Fatal(r)
	}
}

func TestBackgroundSurvivesCallContext(t *testing.T) {
	w := newWorker(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r, err := w.bash.Run(ctx, &circusv1.DeviceCall{CallId: "background", SessionId: "fixture-session", Tool: &circusv1.DeviceCall_Bash{Bash: &circusv1.BashRequest{Command: `echo ready > ready; while [ ! -f release ]; do sleep 0.01; done; echo survived`, Cwd: w.start, RunInBackground: true}}})
	if err != nil {
		t.Fatal(err)
	}
	waitFile(t, filepath.Join(w.start, "ready"))
	cancel()
	writeFile(t, filepath.Join(w.start, "release"), []byte("go"))
	e := w.waitJob(t, r.GetBash().JobId)
	if e.ExitCode != 0 || e.State != circusv1.JobEvent_STATE_EXITED {
		t.Fatal(e)
	}
	data, err := os.ReadFile(r.GetBash().OutputPath)
	if err != nil || string(data) != "survived\n\n[exited with code 0]\n" {
		t.Fatalf("output=%q err=%v", data, err)
	}
}

func TestStopSessionAndOwnership(t *testing.T) {
	w := newWorker(t)
	r := w.bashCall(t, `echo ready > ready; trap 'wait; exit 143' TERM; sleep 30 & wait`, true, 0)
	id := r.GetBash().JobId
	waitFile(t, filepath.Join(w.start, "ready"))
	other := w.call(t, &circusv1.DeviceCall{SessionId: "other-session", Tool: &circusv1.DeviceCall_TaskStop{TaskStop: &circusv1.TaskStopRequest{JobId: id}}})
	if !other.Output.IsError || other.Output.Content[0].GetText() != "<tool_use_error>No task found with ID: "+id+"</tool_use_error>" {
		t.Fatal(other)
	}
	if info, ok := w.jobs.Get(id); !ok || info.State != circusv1.JobEvent_STATE_RUNNING {
		t.Fatalf("foreign stop affected job: %+v", info)
	}
	w.jobs.StopSession("fixture-session")
	e := w.waitJob(t, id)
	if e.State != circusv1.JobEvent_STATE_KILLED {
		t.Fatal(e)
	}
	data, err := os.ReadFile(r.GetBash().OutputPath)
	if err != nil || string(data) != "\n[exited with code 143]\n" {
		t.Fatalf("output=%q err=%v", data, err)
	}
}
