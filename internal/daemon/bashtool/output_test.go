package bashtool

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/daemon/jobs"
	"github.com/EricWvi/circus/internal/testutil/leakcheck"
)

func outputTail(t *testing.T, file *os.File) string {
	t.Helper()
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	offset := max(int64(0), info.Size()-256)
	data := make([]byte, info.Size()-offset)
	if _, err := file.ReadAt(data, offset); err != nil && err != io.EOF {
		t.Fatal(err)
	}
	return string(data)
}

func TestForegroundAndBackgroundOutputCap(t *testing.T) {
	for _, background := range []bool{false, true} {
		t.Run(strconv.FormatBool(background), func(t *testing.T) {
			t.Parallel()
			b, root, events := reviewBash(t, 1024)
			leakcheck.DiskLimit(t, 2<<30, "documented overshoot: an `exec yes` writer outruns the 100 ms cap poll by hundreds of MiB")
			r := reviewRun(t, b, root, `exec yes`, background)
			path := r.GetBash().OutputPath
			if background {
				event := reviewEvent(t, events, true)
				if event.State != circusv1.JobEvent_STATE_KILLED {
					t.Fatal(event)
				}
			} else if !r.Output.IsError || r.GetBash().ExitCode != 137 || !r.GetBash().OutputTruncated {
				t.Fatal(r)
			}
			file, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			if tail := outputTail(t, file); !strings.HasSuffix(tail, outputLimitMessage) {
				t.Fatalf("cap diagnostic: %q", tail)
			}
		})
	}
}

func TestSweepErrorsAreBestEffort(t *testing.T) {
	t.Parallel()
	b, _, _ := reviewBash(t, 0)
	locked := filepath.Join(b.environment.outputRoot(), "locked")
	if err := os.MkdirAll(locked, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(locked, 0700)
	// Explicitly exercise walking beyond an inaccessible directory.
	dir := filepath.Join(b.environment.outputRoot(), "z", "tasks")
	if err := privateDir(dir); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, "stale")
	if err := os.WriteFile(stale, nil, 0600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	// Scheduling must actually run the sweep and continue after the locked
	// directory; merely advancing lastSweep cannot make this assertion pass.
	out, err := b.outputFile("review", "tool-results", "sweep-*")
	if err != nil {
		t.Fatal(err)
	}
	leakcheck.Expect(out.Name()) // created by the test itself
	defer out.Close()
	defer b.environment.releaseOutput(out.Name())
	await(t, func() bool { _, err := os.Stat(stale); return os.IsNotExist(err) })
}

func TestPromotionRegistrationFailureCleansOutput(t *testing.T) {
	t.Parallel()
	b, root, _ := reviewBash(t, 0)
	reviewRun(t, b, root, "true", false)
	if err := b.jobs.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := b.Run(ctx, &circusv1.DeviceCall{SessionId: "review", Tool: &circusv1.DeviceCall_Bash{Bash: &circusv1.BashRequest{Cwd: root, Command: `echo started; exec sleep 20`}}})
	if !errors.Is(err, jobs.ErrShutdown) {
		t.Fatal(err)
	}
	paths, err := filepath.Glob(filepath.Join(b.environment.outputRoot(), "review", "tool-results", "*"))
	if err != nil || len(paths) != 0 {
		t.Fatalf("failed promotion retained output: %v %v", paths, err)
	}
	b.environment.mu.Lock()
	defer b.environment.mu.Unlock()
	if len(b.environment.active) != 0 {
		t.Fatal(b.environment.active)
	}
}

func TestForegroundInlineOutputCap(t *testing.T) {
	t.Parallel()
	b, root, _ := reviewBash(t, 1024)
	r := reviewRun(t, b, root, `printf '%02000d' 0; exec sleep 20`, false)
	if !r.Output.IsError || r.GetBash().ExitCode != 137 || !strings.HasSuffix(r.Output.Content[0].GetText(), strings.TrimSpace(outputLimitMessage)) {
		t.Fatal(r)
	}
	if r.GetBash().OutputTruncated {
		t.Fatal("small output should use normal inline body")
	}
}

func TestConcurrentRegistrationRejectionCleansOutput(t *testing.T) {
	t.Parallel()
	b, root, _ := reviewBash(t, 0)
	// Prime the snapshot before replacing the job manager and starting the
	// short-deadline call.
	reviewRun(t, b, root, "true", false)
	if err := b.jobs.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	allocated := make(chan struct{})
	manager := jobs.NewManager(jobs.Options{NewID: func() string { close(allocated); return "rejected" }})
	b.jobs = manager
	t.Cleanup(func() { _ = manager.Shutdown(context.Background()) })
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	returned := make(chan error, 1)
	go func() {
		_, err := b.Run(ctx, &circusv1.DeviceCall{SessionId: "review", Tool: &circusv1.DeviceCall_Bash{Bash: &circusv1.BashRequest{Cwd: root, Command: `echo ready > ready; exec sleep 20`}}})
		returned <- err
	}()
	await(t, func() bool { _, err := os.Stat(filepath.Join(root, "ready")); return err == nil })
	// Block newBackground's snapshot retain inside Manager.Start's callback.
	b.environment.mu.Lock()
	unlocked := false
	defer func() {
		if !unlocked {
			b.environment.mu.Unlock()
		}
	}()
	select {
	case <-allocated:
	case <-time.After(3 * time.Second):
		t.Fatal("promotion did not reach launch")
	}
	shutdownCtx, stopWaiting := context.WithCancel(context.Background())
	stopWaiting()
	_ = manager.Shutdown(shutdownCtx)
	b.environment.mu.Unlock()
	unlocked = true
	select {
	case err := <-returned:
		if !errors.Is(err, jobs.ErrShutdown) {
			t.Fatal(err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("rejected promotion did not finish")
	}
	paths, err := filepath.Glob(filepath.Join(b.environment.outputRoot(), "review", "tool-results", "*"))
	if err != nil || len(paths) != 0 {
		t.Fatalf("rejected launch retained output: %v %v", paths, err)
	}
}

func TestOutputCapKillsTERMIgnoringWriters(t *testing.T) {
	for _, mode := range []string{"foreground", "background", "tracked"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			b, root, events := reviewBash(t, 4<<20)
			writer := `trap '' TERM; while :; do head -c 1048576 /dev/zero; sleep 0.05; done`
			command := writer
			if mode == "tracked" {
				command = `(while [ ! -f release ]; do sleep 0.01; done; ` + writer + `) & echo x`
			}
			started := time.Now()
			r := reviewRun(t, b, root, command, mode == "background")
			path := r.GetBash().OutputPath
			if mode == "tracked" {
				path = trackedPath(t, b)
			}
			out, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer out.Close()
			if mode == "tracked" {
				started = time.Now()
				if err := os.WriteFile(filepath.Join(root, "release"), nil, 0600); err != nil {
					t.Fatal(err)
				}
				await(t, func() bool { _, err := os.Stat(path); return os.IsNotExist(err) })
			} else if mode == "background" {
				if event := reviewEvent(t, events, true); event.State != circusv1.JobEvent_STATE_KILLED || event.ExitCode != 137 {
					t.Fatal(event)
				}
			} else if r.GetBash().ExitCode != 137 {
				t.Fatal(r)
			}
			if time.Since(started) >= 2*time.Second {
				t.Fatal("output cap allowed the TERM grace period")
			}
			info, err := out.Stat()
			if err != nil || info.Size() > 16<<20 {
				t.Fatalf("output overshot during TERM grace: %v %v", info, err)
			}
			if !strings.HasSuffix(outputTail(t, out), outputLimitMessage) {
				t.Fatal("missing output-cap diagnostic")
			}
		})
	}
}

func TestOutputMessageAfterNaturalExit(t *testing.T) {
	for _, tc := range []struct {
		name, message     string
		terminated, alive bool
		want              string
	}{
		{"total victim exited", totalOutputLimitMessage, false, false, ""},
		{"total victim still has children", totalOutputLimitMessage, false, true, totalOutputLimitMessage},
		{"total victim was killed", totalOutputLimitMessage, true, false, totalOutputLimitMessage},
		{"per-file limit remains enforced", outputLimitMessage, false, false, outputLimitMessage},
		{"no selection", "", false, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := outputMessageAfterExit(tc.message, tc.terminated, tc.alive); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}
