package leakcheck

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
)

// Tool mirrors daemon.Tool, so internal test packages of tools can use the
// audit without an import cycle through the default registry.
type Tool interface {
	Name() string
	Run(context.Context, *circusv1.DeviceCall) (*circusv1.DeviceResult, error)
}

// fallbackGrace matches the dispatcher: a tool still running five seconds
// after its deadline (or preparation budget) is abandoned.
const fallbackGrace = 5 * time.Second

type execution struct {
	callID, tool string
	start, end   time.Time
	overrun      time.Duration
	limited      bool
	done         bool
}

type callAudit struct {
	state *testState
	now   func() time.Time
	mu    sync.Mutex
	runs  []*execution
}

// AuditTools wraps tools before they are passed to a dispatcher. now is the
// dispatcher's clock (nil means time.Now). T must be an interface type such
// as daemon.Tool. Wrappers forward PreparationBudget and Shutdown; a tool
// without them reports a zero budget and a no-op shutdown, which the
// dispatcher treats like the plain tool. Call before creating the
// dispatcher, so the audit runs after the dispatcher's Shutdown cleanup.
func AuditTools[T Tool](t testing.TB, now func() time.Time, tools []T) []T {
	t.Helper()
	if !supported {
		return tools
	}
	if now == nil {
		now = time.Now
	}
	a := &callAudit{state: stateFor(t), now: now}
	t.Cleanup(a.verify)
	out := make([]T, len(tools))
	for i, tool := range tools {
		if any(tool) == nil {
			continue
		}
		wrapped, ok := any(&auditedTool{inner: tool, audit: a}).(T)
		if !ok {
			t.Fatalf("leakcheck: AuditTools needs an interface element type, got %T", tool)
		}
		out[i] = wrapped
	}
	return out
}

type auditedTool struct {
	inner Tool
	audit *callAudit
}

func (w *auditedTool) Name() string { return w.inner.Name() }

func (w *auditedTool) PreparationBudget() time.Duration {
	if p, ok := w.inner.(interface{ PreparationBudget() time.Duration }); ok {
		return p.PreparationBudget()
	}
	return 0
}

func (w *auditedTool) Shutdown(ctx context.Context) error {
	if s, ok := w.inner.(interface{ Shutdown(context.Context) error }); ok {
		return s.Shutdown(ctx)
	}
	return nil
}

func (w *auditedTool) Run(ctx context.Context, call *circusv1.DeviceCall) (*circusv1.DeviceResult, error) {
	a := w.audit
	e := &execution{callID: call.GetCallId(), tool: w.inner.Name(), start: a.now()}
	a.mu.Lock()
	a.runs = append(a.runs, e)
	a.mu.Unlock()
	budget := w.PreparationBudget()
	// Deferred so panics, which the dispatcher recovers, are recorded too.
	defer func() {
		end := a.now()
		var limit time.Time
		if deadline, ok := ctx.Deadline(); ok {
			limit = deadline
		} else if budget > 0 {
			limit = e.start.Add(budget)
		}
		a.mu.Lock()
		e.end, e.done = end, true
		if !limit.IsZero() {
			e.limited, e.overrun = true, end.Sub(limit)
		}
		a.mu.Unlock()
	}()
	return w.inner.Run(ctx, call)
}

func (a *callAudit) verify() {
	s := a.state
	t := s.t
	t.Helper()
	waitFor(func() []string {
		a.mu.Lock()
		defer a.mu.Unlock()
		var found []string
		for _, e := range a.runs {
			if !e.done {
				found = append(found, e.callID)
			}
		}
		return found
	})
	a.mu.Lock()
	runs := append([]*execution(nil), a.runs...)
	counts := make(map[string]int)
	var problems []string
	for _, e := range runs {
		counts[e.callID]++
		if !e.done {
			problems = append(problems, fmt.Sprintf("call %q (%s) is still executing %s after the test", e.callID, e.tool, grace))
		} else if e.limited && e.overrun >= fallbackGrace {
			problems = append(problems, fmt.Sprintf("call %q (%s) returned %s after its deadline; the dispatcher abandons tools after %s", e.callID, e.tool, e.overrun, fallbackGrace))
		}
	}
	a.mu.Unlock()
	global.mu.Lock()
	for id, n := range counts {
		if n > 1 {
			if reason := s.reexec[id]; reason != "" {
				t.Logf("leakcheck: allowed re-execution of %q (%s)", id, reason)
				continue
			}
			problems = append(problems, fmt.Sprintf("call_id %q executed %d times", id, n))
		}
	}
	allowed := s.allowed[Calls]
	global.mu.Unlock()
	if len(problems) == 0 {
		return
	}
	sort.Strings(problems)
	var table []string
	for _, e := range runs {
		duration := "running"
		if e.done {
			duration = e.end.Sub(e.start).String()
		}
		table = append(table, fmt.Sprintf("%s %s: %s", e.callID, e.tool, duration))
	}
	message := fmt.Sprintf("dispatcher call audit:\n    %s\n  executions:\n    %s", strings.Join(problems, "\n    "), strings.Join(table, "\n    "))
	if allowed != "" {
		t.Logf("leakcheck: allowed %s (%s): %s", Calls, allowed, message)
		return
	}
	t.Errorf("leakcheck [%s]: %s", Calls, message)
}
