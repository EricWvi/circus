package daemon_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/clock"
	"github.com/EricWvi/circus/internal/daemon"
	"github.com/EricWvi/circus/internal/daemon/bashtool"
	"github.com/EricWvi/circus/internal/daemon/toolcontext"
	"github.com/EricWvi/circus/internal/testutil/leakcheck"
	"github.com/EricWvi/circus/internal/testutil/wait"
)

type preparingTool struct {
	fakeTool
	budget time.Duration
}

func (f preparingTool) PreparationBudget() time.Duration { return f.budget }

// stepOnArm is a fake clock that, when a timer is armed for the instant
// set by stepAt (by After or Until), first advances by step: another
// goroutine moving the clock between a reader's Now and its arming.
type stepOnArm struct {
	*clock.Fake
	step time.Duration
	mu   sync.Mutex
	at   time.Time
}

func (c *stepOnArm) stepAt(at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = at
}

func (c *stepOnArm) arming(at time.Time) {
	c.mu.Lock()
	hit := !c.at.IsZero() && at.Equal(c.at)
	if hit {
		c.at = time.Time{}
	}
	c.mu.Unlock()
	if hit {
		c.Advance(c.step)
	}
}

func (c *stepOnArm) After(d time.Duration) <-chan time.Time {
	c.arming(c.Now().Add(d))
	return c.Fake.After(d)
}

func (c *stepOnArm) Until(at time.Time) <-chan time.Time {
	c.arming(at)
	return c.Fake.Until(at)
}

// TestDeadlineDespiteAdvanceWhileArming: a tool without preparation ends
// at its deadline even when the clock reaches it while Submit arms the
// deadline timer.
func TestDeadlineDespiteAdvanceWhileArming(t *testing.T) {
	t.Parallel()
	clk := &stepOnArm{Fake: clock.New(), step: time.Second}
	active := make(chan context.Context, 1)
	tool := fakeTool{"read", func(ctx context.Context, _ *circusv1.DeviceCall) (*circusv1.DeviceResult, error) {
		active <- ctx
		<-ctx.Done()
		return output(ctx.Err().Error()), nil
	}}
	d := newDispatcher(t, daemon.Options{Clock: clk, DefaultTimeout: time.Second, Tools: []daemon.Tool{tool}})
	end := clk.Now().Add(time.Second)
	clk.stepAt(end)
	if _, err := d.Submit(readCall("arming")); err != nil {
		t.Fatal(err)
	}
	ctx := receive(t, active)
	if got, _ := ctx.Deadline(); !got.Equal(end) || !clk.Now().Equal(end) {
		t.Fatalf("deadline %s, clock %s, want both %s", got, clk.Now(), end)
	}
	receive(t, ctx.Done())
	if r := completed(t, d, "arming"); resultText(r) != context.DeadlineExceeded.Error() {
		t.Fatal(resultText(r))
	}
}

func TestWrappedContextStartsExecutionTimeout(t *testing.T) {
	t.Parallel()
	clock := &stepOnArm{Fake: clock.New(), step: time.Second}
	preparing := make(chan struct{})
	release := make(chan struct{})
	started := make(chan time.Time, 1)
	type key struct{}
	tool := preparingTool{budget: time.Hour, fakeTool: fakeTool{"read", func(ctx context.Context, _ *circusv1.DeviceCall) (*circusv1.DeviceResult, error) {
		wrapped := context.WithValue(ctx, key{}, "wrapper")
		if _, ok := wrapped.Deadline(); ok {
			t.Error("execution deadline active during preparation")
		}
		close(preparing)
		select {
		case <-release:
		case <-ctx.Done():
			return output(ctx.Err().Error()), nil
		}
		toolcontext.StartTimeout(wrapped)
		deadline, ok := wrapped.Deadline()
		if !ok {
			t.Error("wrapped context lost timeout starter")
		}
		toolcontext.StartTimeout(context.WithValue(wrapped, key{}, "again"))
		if again, _ := wrapped.Deadline(); !again.Equal(deadline) {
			t.Error("timeout restarted twice")
		}
		started <- deadline
		<-wrapped.Done()
		return output(wrapped.Err().Error()), nil
	}}}
	d := newDispatcher(t, daemon.Options{Clock: clock, DefaultTimeout: time.Second, Tools: []daemon.Tool{tool}})
	if _, err := d.Submit(readCall("wrapped")); err != nil {
		t.Fatal(err)
	}
	receive(t, preparing)
	clock.Advance(10 * time.Minute)
	// The clock reaches the execution deadline while execute arms its
	// timer, between reading the time and arming: the call must still end.
	end := clock.Now().Add(time.Second)
	clock.stepAt(end)
	close(release)
	if got := receive(t, started); !got.Equal(end) {
		t.Fatal("wrong execution deadline", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), wait.Timeout)
	defer cancel()
	status, err := d.Wait(ctx, "wrapped")
	if err != nil {
		t.Fatal("call did not time out:", err)
	}
	if status.State != circusv1.CallStatus_STATE_COMPLETED {
		t.Fatal(status)
	}
	if body := resultText(status.Result); body != context.DeadlineExceeded.Error() {
		t.Fatal(body)
	}
	// A standalone tool caller need not install dispatcher state.
	toolcontext.StartTimeout(context.WithValue(context.Background(), key{}, "direct"))
}

func TestToolDeclaresPreparationBudget(t *testing.T) {
	t.Parallel()
	clock := clock.New()
	started := make(chan struct{})
	tool := preparingTool{budget: 3 * time.Second, fakeTool: fakeTool{"read", func(ctx context.Context, _ *circusv1.DeviceCall) (*circusv1.DeviceResult, error) {
		close(started)
		<-ctx.Done()
		return output("cancelled"), nil
	}}}
	d := newDispatcher(t, daemon.Options{Clock: clock, DefaultTimeout: time.Second, Tools: []daemon.Tool{tool}})
	if _, err := d.Submit(readCall("prepare")); err != nil {
		t.Fatal(err)
	}
	receive(t, started)
	clock.Advance(2 * time.Second)
	if status := d.Query("prepare"); status.State != circusv1.CallStatus_STATE_RUNNING {
		t.Fatal(status)
	}
	clock.Advance(time.Second)
	r := completed(t, d, "prepare")
	if r.GetFault().GetCode() != circusv1.DaemonFault_CODE_PREPARATION_EXHAUSTED {
		t.Fatal(r)
	}
	if body := resultText(r); body != "Error: tool preparation exceeded its budget" {
		t.Fatal(body)
	}
}

type shutdownTool struct {
	fakeTool
	shutdown func(context.Context) error
}

func (f shutdownTool) Shutdown(ctx context.Context) error { return f.shutdown(ctx) }

func TestToolShutdownContextIsBounded(t *testing.T) {
	t.Parallel()
	observed := make(chan bool, 1)
	tool := shutdownTool{fakeTool: fakeTool{name: "read"}, shutdown: func(ctx context.Context) error {
		_, bounded := ctx.Deadline()
		observed <- bounded
		<-ctx.Done()
		return ctx.Err()
	}}
	d := newDispatcher(t, daemon.Options{ToolShutdownTimeout: 20 * time.Millisecond, Tools: []daemon.Tool{tool}})
	ctx, cancel := context.WithTimeout(context.Background(), wait.Timeout)
	defer cancel()
	if err := d.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if !receive(t, observed) {
		t.Fatal("tool shutdown received an unbounded context")
	}
}

func TestDefaultToolsDoesNotCollectEnvironment(t *testing.T) {
	// Isolate even the regression path: the old constructor must not touch the
	// real HOME or state directory when this test detects unwanted collection.
	if err := os.MkdirAll(".tmp", 0700); err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(".tmp", "lazy-registry-")
	if err != nil {
		t.Fatal(err)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	leakcheck.Track(t, root)
	t.Setenv("HOME", root)
	state := filepath.Join(root, "state")
	leakcheck.StateDir(t, filepath.Join(state, "circus", "daemon"))
	leakcheck.OutputRoot(t, filepath.Join(root, "cache", "circus", "output"))
	t.Setenv("CIRCUS_HOME", "")
	t.Setenv("XDG_STATE_HOME", state)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))
	marker := filepath.Join(root, "touched")
	if err := os.WriteFile(filepath.Join(root, ".bashrc"), []byte(`echo touched > "$HOME/touched"`), 0600); err != nil {
		t.Fatal(err)
	}
	tools := daemon.DefaultTools(nil, bashtool.Options{})
	defer func() {
		for _, tool := range tools {
			if closer, ok := tool.(interface{ Shutdown(context.Context) error }); ok {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				if err := closer.Shutdown(ctx); err != nil {
					t.Error(err)
				}
				cancel()
			}
		}
	}()
	time.Sleep(100 * time.Millisecond)
	for _, path := range []string{state, marker} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("constructing default tools touched %s: %v", path, err)
		}
	}
	var names []string
	for _, tool := range tools {
		names = append(names, tool.Name())
	}
	for _, name := range []string{"read", "write", "edit", "bash", "task_stop", "prepare_workspace", "remove_workspace",
		"prepare_ledger", "collect_ledger", "rebase_ledger", "remove_ledger"} {
		if !slices.Contains(names, name) {
			t.Errorf("default tools %v lack %s", names, name)
		}
	}
}

func TestNormalToolCannotRestartTimeout(t *testing.T) {
	for _, preparedInterface := range []bool{false, true} {
		t.Run(map[bool]string{false: "normal", true: "zero preparation budget"}[preparedInterface], func(t *testing.T) {
			t.Parallel()
			clock := clock.New()
			original := clock.Now().Add(time.Second)
			started := make(chan struct{})
			release := make(chan struct{})
			deadline := make(chan time.Time, 1)
			base := fakeTool{"read", func(ctx context.Context, _ *circusv1.DeviceCall) (*circusv1.DeviceResult, error) {
				close(started)
				select {
				case <-release:
				case <-ctx.Done():
					return output(ctx.Err().Error()), nil
				}
				toolcontext.StartTimeout(ctx)
				end, _ := ctx.Deadline()
				deadline <- end
				<-ctx.Done()
				return output(ctx.Err().Error()), nil
			}}
			var tool daemon.Tool = base
			if preparedInterface {
				tool = preparingTool{fakeTool: base, budget: 0}
			}
			d := newDispatcher(t, daemon.Options{Clock: clock, DefaultTimeout: time.Second, Tools: []daemon.Tool{tool}})
			if _, err := d.Submit(readCall("normal")); err != nil {
				t.Fatal(err)
			}
			receive(t, started)
			clock.Advance(500 * time.Millisecond)
			close(release)
			if got := receive(t, deadline); !got.Equal(original) {
				t.Fatalf("normal timeout extended to %s, want %s", got, original)
			}
			clock.Advance(500 * time.Millisecond)
			if body := resultText(completed(t, d, "normal")); body != context.DeadlineExceeded.Error() {
				t.Fatal(body)
			}
		})
	}
}

// TestCancelFallbackDespiteAdvanceWhileArming: a cancelled tool that
// ignores cancellation is abandoned once its grace period ends, even when
// the clock reaches that instant while Cancel arms the timer.
func TestCancelFallbackDespiteAdvanceWhileArming(t *testing.T) {
	t.Parallel()
	clk := &stepOnArm{Fake: clock.New(), step: daemon.AbandonGrace}
	started := make(chan context.Context, 1)
	release := make(chan struct{})
	returned := make(chan struct{})
	leakcheck.Allow(t, leakcheck.Calls, "this tool deliberately ignores cancellation to exercise abandonment")
	d := newDispatcher(t, daemon.Options{Clock: clk, DefaultTimeout: 10 * time.Minute,
		Tools: []daemon.Tool{fakeTool{"read", func(ctx context.Context, _ *circusv1.DeviceCall) (*circusv1.DeviceResult, error) {
			started <- ctx
			<-release
			defer close(returned)
			return output("late result"), nil
		}}},
	})
	if _, err := d.Submit(readCall("cancel-arming")); err != nil {
		t.Fatal(err)
	}
	ctx := receive(t, started)
	clk.stepAt(clk.Now().Add(daemon.AbandonGrace))
	if !d.Cancel("cancel-arming") {
		t.Fatal("cancel returned false")
	}
	receive(t, ctx.Done())
	r := completed(t, d, "cancel-arming")
	if r.GetFault().GetCode() != circusv1.DaemonFault_CODE_ABANDONED_AFTER_CANCEL {
		t.Fatalf("cancellation fallback: %v", r)
	}
	close(release)
	receive(t, returned)
}
