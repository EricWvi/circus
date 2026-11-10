package daemon_test

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/daemon"
	"github.com/EricWvi/circus/internal/daemon/clocktest"
	"github.com/EricWvi/circus/internal/testutil/leakcheck"
)

type fakeTool struct {
	name string
	run  func(context.Context, *circusv1.DeviceCall) (*circusv1.DeviceResult, error)
}

func (f fakeTool) Name() string { return f.name }
func (f fakeTool) Run(ctx context.Context, call *circusv1.DeviceCall) (*circusv1.DeviceResult, error) {
	return f.run(ctx, call)
}

func readCall(id string) *circusv1.DeviceCall {
	return &circusv1.DeviceCall{CallId: id, Tool: &circusv1.DeviceCall_Read{Read: &circusv1.ReadRequest{Path: "/x"}}}
}

func output(text string) *circusv1.DeviceResult {
	return &circusv1.DeviceResult{CallId: "wrong-id", Output: &circusv1.ToolOutput{Content: []*circusv1.ContentBlock{{Block: &circusv1.ContentBlock_Text{Text: text}}}}}
}

func resultText(r *circusv1.DeviceResult) string {
	if len(r.GetOutput().GetContent()) == 0 {
		return ""
	}
	return r.Output.Content[0].GetText()
}

func newDispatcher(t *testing.T, opts daemon.Options) *daemon.Dispatcher {
	t.Helper()
	// Registered before the Shutdown cleanup, so both checks run after it.
	leakcheck.Track(t)
	now := time.Now
	if opts.Clock != nil {
		now = opts.Clock.Now
	}
	opts.Tools = leakcheck.AuditTools(t, now, opts.Tools)
	d, err := daemon.NewDispatcher(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := d.Shutdown(ctx); err != nil {
			t.Errorf("shutdown: %v", err)
		}
	})
	return d
}

func completed(t *testing.T, d *daemon.Dispatcher, id string) *circusv1.DeviceResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s, err := d.Wait(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if s.State != circusv1.CallStatus_STATE_COMPLETED || s.Result.GetCallId() != id {
		t.Fatalf("unexpected status: %v", s)
	}
	return s.Result
}

func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for event")
		var zero T
		return zero
	}
}

func eventually(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !check() {
		if time.Now().After(deadline) {
			t.Fatal("condition did not become true")
		}
		runtime.Gosched()
	}
}

func TestDispatcherDeduplicationAndIndependentWait(t *testing.T) {
	var runs atomic.Int32
	var callbacks atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	d := newDispatcher(t, daemon.Options{
		Tools: []daemon.Tool{fakeTool{"read", func(_ context.Context, call *circusv1.DeviceCall) (*circusv1.DeviceResult, error) {
			runs.Add(1)
			close(started)
			<-release
			if call.GetRead().Path != "/x" {
				t.Errorf("call was not snapshotted: %v", call)
			}
			return output("done"), nil
		}}},
		OnResult: func(result *circusv1.DeviceResult) { callbacks.Add(1); result.Output.IsError = true },
	})
	call := readCall("one")
	if s, err := d.Submit(call); err != nil || s.State != circusv1.CallStatus_STATE_RUNNING {
		t.Fatalf("submit: %v, %v", s, err)
	}
	receive(t, started)
	var submitters sync.WaitGroup
	for range 32 {
		submitters.Go(func() {
			if s, err := d.Submit(call); err != nil || s.State != circusv1.CallStatus_STATE_RUNNING {
				t.Errorf("duplicate: %v, %v", s, err)
			}
		})
	}
	submitters.Wait()
	call.GetRead().Path = "/changed"
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := d.Wait(ctx, "one"); !errors.Is(err, context.Canceled) {
		t.Fatalf("wait: %v", err)
	}
	if s := d.Query("one"); s.State != circusv1.CallStatus_STATE_RUNNING {
		t.Fatalf("wait cancelled call: %v", s)
	}
	close(release)
	result := completed(t, d, "one")
	if resultText(result) != "done" || result.Output.IsError {
		t.Fatalf("result: %v", result)
	}
	result.Output.IsError = true
	s, err := d.Submit(readCall("one"))
	if err != nil || s.State != circusv1.CallStatus_STATE_COMPLETED || s.Result.Output.IsError {
		t.Fatalf("completed duplicate: %v, %v", s, err)
	}
	eventually(t, func() bool { return callbacks.Load() == 1 })
	if runs.Load() != 1 {
		t.Fatalf("runs = %d", runs.Load())
	}
}

func TestDispatcherCallIDMismatch(t *testing.T) {
	var runs atomic.Int32
	release := make(chan struct{})
	d := newDispatcher(t, daemon.Options{Tools: []daemon.Tool{fakeTool{"read", func(context.Context, *circusv1.DeviceCall) (*circusv1.DeviceResult, error) {
		runs.Add(1)
		<-release
		return output("done"), nil
	}}}})
	if _, err := d.Submit(readCall("reuse")); err != nil {
		t.Fatal(err)
	}
	different := readCall("reuse")
	different.GetRead().Path = "/y"
	if _, err := d.Submit(different); !errors.Is(err, daemon.ErrCallMismatch) {
		t.Fatalf("running mismatch: %v", err)
	}
	close(release)
	completed(t, d, "reuse")
	if _, err := d.Submit(different); !errors.Is(err, daemon.ErrCallMismatch) {
		t.Fatalf("completed mismatch: %v", err)
	}
	if s, err := d.Submit(readCall("reuse")); err != nil || s.State != circusv1.CallStatus_STATE_COMPLETED {
		t.Fatalf("identical resubmit: %v, %v", s, err)
	}
	if runs.Load() != 1 {
		t.Fatalf("runs = %d", runs.Load())
	}
}

func TestDispatcherTimeouts(t *testing.T) {
	for _, tc := range []struct {
		name                             string
		requested                        uint32
		defaultTimeout, maxTimeout, want time.Duration
	}{
		{"defaults", 0, 0, 0, 2 * time.Minute},
		{"default maximum", 1200000, 0, 0, 10 * time.Minute},
		{"explicit", 3000, 0, 0, 3 * time.Second},
		{"clamped", 9000, time.Second, 4 * time.Second, 4 * time.Second},
		{"configured default", 0, 6 * time.Second, 10 * time.Second, 6 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := clocktest.New()
			start := clock.Now()
			started := make(chan time.Time, 1)
			d := newDispatcher(t, daemon.Options{Clock: clock, DefaultTimeout: tc.defaultTimeout, MaxTimeout: tc.maxTimeout,
				Tools: []daemon.Tool{fakeTool{"read", func(ctx context.Context, _ *circusv1.DeviceCall) (*circusv1.DeviceResult, error) {
					deadline, ok := ctx.Deadline()
					if !ok {
						t.Error("missing deadline")
					}
					started <- deadline
					child, cancel := context.WithCancel(ctx)
					defer cancel()
					<-child.Done()
					if child.Err() != context.DeadlineExceeded || context.Cause(child) != context.DeadlineExceeded {
						t.Errorf("derived context: err %v, cause %v", child.Err(), context.Cause(child))
					}
					return output(ctx.Err().Error()), nil
				}}},
			})
			call := readCall("timeout")
			call.TimeoutMs = tc.requested
			if _, err := d.Submit(call); err != nil {
				t.Fatal(err)
			}
			if deadline := receive(t, started); !deadline.Equal(start.Add(tc.want)) {
				t.Fatalf("deadline = %v, want %v", deadline, start.Add(tc.want))
			}
			clock.Advance(tc.want - time.Millisecond)
			if s := d.Query(call.CallId); s.State != circusv1.CallStatus_STATE_RUNNING {
				t.Fatalf("early completion: %v", s)
			}
			clock.Advance(time.Millisecond)
			if text := resultText(completed(t, d, call.CallId)); text != context.DeadlineExceeded.Error() {
				t.Fatalf("timeout: %s", text)
			}
		})
	}
}

func TestDispatcherAbandonsUncooperativeTool(t *testing.T) {
	clock := clocktest.New()
	started := make(chan context.Context, 1)
	release := make(chan struct{})
	returned := make(chan struct{})
	var callbacks atomic.Int32
	leakcheck.Allow(t, leakcheck.Calls, "this tool deliberately ignores its deadline to exercise abandonment")
	d := newDispatcher(t, daemon.Options{Clock: clock, DefaultTimeout: time.Second,
		OnResult: func(*circusv1.DeviceResult) { callbacks.Add(1) },
		Tools: []daemon.Tool{fakeTool{"read", func(ctx context.Context, _ *circusv1.DeviceCall) (*circusv1.DeviceResult, error) {
			started <- ctx
			<-release
			defer close(returned)
			return output("late result"), nil
		}}},
	})
	if _, err := d.Submit(readCall("stuck")); err != nil {
		t.Fatal(err)
	}
	ctx := receive(t, started)
	clock.Advance(time.Second)
	receive(t, ctx.Done())
	clock.Advance(5*time.Second - time.Millisecond)
	if s := d.Query("stuck"); s.State != circusv1.CallStatus_STATE_RUNNING {
		t.Fatalf("early fallback: %v", s)
	}
	clock.Advance(time.Millisecond)
	r := completed(t, d, "stuck")
	if !r.Output.IsError || resultText(r) != "Error: tool did not stop after its timeout" {
		t.Fatalf("fallback: %v", r)
	}
	close(release)
	receive(t, returned)
	if err := d.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if callbacks.Load() != 1 || resultText(d.Query("stuck").Result) != resultText(r) {
		t.Fatal("late return changed completion")
	}
}

func TestDispatcherCancellationShutdownAndRetention(t *testing.T) {
	clock := clocktest.New()
	contexts := make(chan context.Context, 8)
	d := newDispatcher(t, daemon.Options{Clock: clock, ResultTTL: time.Minute, TombstoneTTL: time.Hour,
		Tools: []daemon.Tool{fakeTool{"read", func(ctx context.Context, _ *circusv1.DeviceCall) (*circusv1.DeviceResult, error) {
			contexts <- ctx
			<-ctx.Done()
			return output("interrupted"), nil
		}}},
	})
	if d.Query("unknown").State != circusv1.CallStatus_STATE_LOST || d.Cancel("unknown") {
		t.Fatal("unknown call should be lost")
	}
	call := readCall("cancel")
	if _, err := d.Submit(call); err != nil {
		t.Fatal(err)
	}
	receive(t, contexts)
	clock.Advance(time.Minute)
	if s := d.Query(call.CallId); s.State != circusv1.CallStatus_STATE_RUNNING {
		t.Fatalf("running call expired: %v", s)
	}
	if !d.Cancel(call.CallId) {
		t.Fatal("cancel returned false")
	}
	if text := resultText(completed(t, d, call.CallId)); text != "interrupted" {
		t.Fatalf("cancel: %s", text)
	}
	if d.Cancel(call.CallId) {
		t.Fatal("completed call cancelled")
	}
	clock.Advance(time.Minute)
	if s := d.Query(call.CallId); s.State != circusv1.CallStatus_STATE_LOST || s.Result != nil {
		t.Fatalf("expired: %v", s)
	}
	if s, err := d.Wait(context.Background(), call.CallId); err != nil || s.State != circusv1.CallStatus_STATE_LOST {
		t.Fatalf("expired wait: %v, %v", s, err)
	}
	if _, err := d.Submit(call); !errors.Is(err, daemon.ErrCallExpired) {
		t.Fatalf("expired submit: %v", err)
	}
	leakcheck.AllowReexecution(t, call.CallId, "a call_id may execute again once its tombstone expires")
	clock.Advance(59 * time.Minute)
	if _, err := d.Submit(call); err != nil {
		t.Fatalf("tombstone expired: %v", err)
	}
	receive(t, contexts)
	if err := d.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if text := resultText(completed(t, d, call.CallId)); text != "interrupted" {
		t.Fatalf("shutdown: %s", text)
	}
	if _, err := d.Submit(readCall("after-shutdown")); !errors.Is(err, daemon.ErrShutdown) {
		t.Fatalf("submit after shutdown: %v", err)
	}
}

func TestDispatcherValidationAndToolFailures(t *testing.T) {
	tool := fakeTool{"read", func(context.Context, *circusv1.DeviceCall) (*circusv1.DeviceResult, error) {
		return output("unused"), nil
	}}
	if _, err := daemon.NewDispatcher(daemon.Options{Tools: []daemon.Tool{tool, tool}}); err == nil {
		t.Fatal("duplicate tool names accepted")
	}
	d := newDispatcher(t, daemon.Options{})
	for _, call := range []*circusv1.DeviceCall{nil, {}, {CallId: "missing"}, {Tool: readCall("x").Tool}} {
		if _, err := d.Submit(call); err == nil {
			t.Fatalf("invalid call accepted: %v", call)
		}
	}
	s, err := d.Submit(readCall("unknown-tool"))
	if err != nil || s.State != circusv1.CallStatus_STATE_COMPLETED || !s.Result.Output.IsError || resultText(s.Result) != `Error: tool "read" is not available on this device` {
		t.Fatalf("unknown tool: %v, %v", s, err)
	}
	for _, tc := range []struct {
		name, want string
		run        func(context.Context, *circusv1.DeviceCall) (*circusv1.DeviceResult, error)
	}{
		{"error", "Error: failed", func(context.Context, *circusv1.DeviceCall) (*circusv1.DeviceResult, error) {
			return nil, errors.New("failed")
		}},
		{"panic", "Error: boom", func(context.Context, *circusv1.DeviceCall) (*circusv1.DeviceResult, error) { panic("boom") }},
		{"nil", "Error: tool returned no result", func(context.Context, *circusv1.DeviceCall) (*circusv1.DeviceResult, error) { return nil, nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newDispatcher(t, daemon.Options{Tools: []daemon.Tool{fakeTool{"read", tc.run}}})
			if _, err := d.Submit(readCall(tc.name)); err != nil {
				t.Fatal(err)
			}
			r := completed(t, d, tc.name)
			if !r.Output.IsError || resultText(r) != tc.want || strings.Contains(resultText(r), "goroutine") {
				t.Fatalf("failure: %v", r)
			}
		})
	}
}

func TestDispatcherShutdownContextOnlyStopsWaiting(t *testing.T) {
	clock := clocktest.New()
	release := make(chan struct{})
	started := make(chan context.Context, 1)
	d := newDispatcher(t, daemon.Options{Clock: clock, Tools: []daemon.Tool{fakeTool{"read", func(ctx context.Context, _ *circusv1.DeviceCall) (*circusv1.DeviceResult, error) {
		started <- ctx
		<-release
		return output("finished"), nil
	}}}})
	if _, err := d.Submit(readCall("shutdown")); err != nil {
		t.Fatal(err)
	}
	toolCtx := receive(t, started)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := d.Shutdown(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("shutdown wait: %v", err)
	}
	receive(t, toolCtx.Done())
	if _, err := d.Submit(readCall("later")); !errors.Is(err, daemon.ErrShutdown) {
		t.Fatalf("shutdown accepted submit: %v", err)
	}
	close(release)
	if err := d.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	completed(t, d, "shutdown")
}
