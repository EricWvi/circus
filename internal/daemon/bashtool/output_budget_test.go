package bashtool

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
)

func TestTotalOutputCapStopsOnlyLargest(t *testing.T) {
	for _, mode := range []string{"foreground", "promoted", "background", "tracked"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			b, root, _ := reviewBashWithLimits(t, 1<<20, 10000)
			writer := func(name string, size int) string {
				return fmt.Sprintf(`printf '%%0%dd' 0; echo ready > %s.ready; while [ ! -f release-%s ]; do sleep 0.02; done`, size, name, name)
			}
			ready := func(name string) {
				t.Helper()
				await(t, func() bool { _, err := os.Stat(filepath.Join(root, name+".ready")); return err == nil })
			}
			small := reviewRun(t, b, root, writer("small", 4096), true).GetBash()
			ready("small")
			largeCommand := writer("large", 8192)
			if mode == "promoted" || mode == "tracked" {
				largeCommand = `while [ ! -f start-large ]; do sleep 0.02; done; ` + largeCommand
			}
			if mode == "tracked" {
				largeCommand = "(" + largeCommand + ") & echo x"
			}
			duration := 3 * time.Second
			if mode == "promoted" {
				duration = 100 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(context.Background(), duration)
			defer cancel()
			r, err := b.Run(ctx, &circusv1.DeviceCall{CallId: "large", SessionId: "review", Tool: &circusv1.DeviceCall_Bash{Bash: &circusv1.BashRequest{Command: largeCommand, Cwd: root, RunInBackground: mode == "background"}}})
			if err != nil {
				t.Fatal(err)
			}
			large := r.GetBash()
			var out *os.File
			if mode == "foreground" {
				body := r.Output.Content[0].GetText()
				if !r.Output.IsError || large.ExitCode != 137 || !strings.HasSuffix(body, strings.TrimSpace(totalOutputLimitMessage)) {
					t.Fatal(r)
				}
			} else {
				path := large.OutputPath
				if mode == "tracked" {
					if r.Output.IsError || r.Output.Content[0].GetText() != "x" || large.JobId != "" {
						t.Fatal(r)
					}
					b.environment.mu.Lock()
					for candidate := range b.environment.active {
						if candidate != small.OutputPath {
							path = candidate
						}
					}
					b.environment.mu.Unlock()
				} else if large.JobId == "" {
					t.Fatal("missing outer job", r)
				}
				out, err = os.Open(path)
				if err != nil {
					t.Fatal(err)
				}
				defer out.Close()
				if mode == "promoted" || mode == "tracked" {
					if err := os.WriteFile(filepath.Join(root, "start-large"), nil, 0600); err != nil {
						t.Fatal(err)
					}
				}
				if mode == "tracked" {
					await(t, func() bool { _, err := os.Stat(path); return os.IsNotExist(err) })
				} else {
					await(t, func() bool {
						info, ok := b.jobs.Get(large.JobId)
						return ok && info.State != circusv1.JobEvent_STATE_RUNNING
					})
					if info, _ := b.jobs.Get(large.JobId); info.State != circusv1.JobEvent_STATE_KILLED || info.ExitCode != 137 {
						t.Fatal(info)
					}
				}
				if tail := outputTail(t, out); !strings.HasSuffix(tail, totalOutputLimitMessage) || strings.Count(tail, totalOutputLimitMessage) != 1 {
					t.Fatalf("total-cap diagnostic: %q", tail)
				}
			}
			if info, ok := b.jobs.Get(small.JobId); !ok || info.State != circusv1.JobEvent_STATE_RUNNING {
				t.Fatalf("stopped the smaller writer: %+v", info)
			}
			// A completed persisted output must not consume the running budget.
			// The two remaining files total 8192, below 10000; including the old
			// larger file would incorrectly stop one of these commands.
			third := reviewRun(t, b, root, writer("third", 4096), true).GetBash()
			ready("third")
			time.Sleep(3 * outputPollInterval)
			for _, job := range []*circusv1.BashResult{small, third} {
				if info, ok := b.jobs.Get(job.JobId); !ok || info.State != circusv1.JobEvent_STATE_RUNNING {
					t.Fatalf("finished output still counted: %+v", info)
				}
				data, err := os.ReadFile(job.OutputPath)
				if err != nil || strings.Contains(string(data), "[circus:") {
					t.Fatalf("smaller output changed: %q %v", data, err)
				}
			}
			for _, name := range []string{"small", "third"} {
				if err := os.WriteFile(filepath.Join(root, "release-"+name), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			for _, job := range []*circusv1.BashResult{small, third} {
				await(t, func() bool {
					info, ok := b.jobs.Get(job.JobId)
					return ok && info.State != circusv1.JobEvent_STATE_RUNNING
				})
				if info, _ := b.jobs.Get(job.JobId); info.State != circusv1.JobEvent_STATE_EXITED || info.ExitCode != 0 {
					t.Fatal(info)
				}
			}
		})
	}
}

func TestTotalOutputCapAfterBackgroundShellExit(t *testing.T) {
	t.Parallel()
	b, root, _ := reviewBashWithLimits(t, 1<<20, 10000)
	small := reviewRun(t, b, root, `printf '%04096d' 0; echo ready > small.ready; while [ ! -f release-small ]; do sleep 0.02; done`, true).GetBash()
	await(t, func() bool { _, err := os.Stat(filepath.Join(root, "small.ready")); return err == nil })
	large := reviewRun(t, b, root, `(echo $BASHPID > child.pid; while [ ! -f start-large ]; do sleep 0.02; done; printf '%08192d' 0; while :; do sleep 0.02; done) & echo x`, true).GetBash()
	// The visible outer job has ended, but its output still belongs to a live
	// child group. Accounting must use the replacement descriptor after the
	// outer background job closes its original descriptor.
	await(t, func() bool {
		info, ok := b.jobs.Get(large.JobId)
		return ok && info.State == circusv1.JobEvent_STATE_EXITED
	})
	await(t, func() bool { _, err := os.Stat(filepath.Join(root, "child.pid")); return err == nil })
	pid := childPID(t, root)
	out, err := os.Open(large.OutputPath)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	if err := os.WriteFile(filepath.Join(root, "start-large"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	await(t, func() bool { return syscall.Kill(pid, 0) == syscall.ESRCH })
	await(t, func() bool {
		b.environment.mu.Lock()
		defer b.environment.mu.Unlock()
		return !b.environment.active[large.OutputPath]
	})
	if tail := outputTail(t, out); !strings.HasSuffix(tail, totalOutputLimitMessage) {
		t.Fatalf("transferred output diagnostic: %q", tail)
	}
	if info, _ := b.jobs.Get(small.JobId); info.State != circusv1.JobEvent_STATE_RUNNING {
		t.Fatal("stopped the smaller command", info)
	}
	if info, _ := b.jobs.Get(large.JobId); info.State != circusv1.JobEvent_STATE_EXITED {
		t.Fatal("changed the completed outer job", info)
	}
}
