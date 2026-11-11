package daemon_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/daemon"
	"github.com/EricWvi/circus/internal/daemon/bashtool"
	"github.com/EricWvi/circus/internal/daemon/clocktest"
	"github.com/EricWvi/circus/internal/daemon/toolcontext"
	"github.com/EricWvi/circus/internal/testutil/leakcheck"
)

type preparingTool struct {
	fakeTool
	budget time.Duration
}

func (f preparingTool) PreparationBudget() time.Duration { return f.budget }

func TestWrappedContextStartsExecutionTimeout(t *testing.T) {
	t.Parallel()
	clock := clocktest.New()
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
	close(release)
	if end := receive(t, started); !end.Equal(clock.Now().Add(time.Second)) {
		t.Fatal("wrong execution deadline", end)
	}
	clock.Advance(time.Second)
	if body := resultText(completed(t, d, "wrapped")); body != context.DeadlineExceeded.Error() {
		t.Fatal(body)
	}
	// A standalone tool caller need not install dispatcher state.
	toolcontext.StartTimeout(context.WithValue(context.Background(), key{}, "direct"))
}

func TestToolDeclaresPreparationBudget(t *testing.T) {
	t.Parallel()
	clock := clocktest.New()
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
	if body := resultText(completed(t, d, "prepare")); body != "Error: tool preparation exceeded its budget" {
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
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
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
}

func TestNormalToolCannotRestartTimeout(t *testing.T) {
	for _, preparedInterface := range []bool{false, true} {
		t.Run(map[bool]string{false: "normal", true: "zero preparation budget"}[preparedInterface], func(t *testing.T) {
			t.Parallel()
			clock := clocktest.New()
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
