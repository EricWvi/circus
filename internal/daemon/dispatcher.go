package daemon

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/daemon/toolcontext"
	"github.com/EricWvi/circus/internal/identifier"
	"github.com/EricWvi/circus/internal/textutil"
	"google.golang.org/protobuf/proto"
)

var (
	// ErrInvalidCall rejects malformed IDs or a missing tool before execution.
	ErrInvalidCall = errors.New("invalid call")
	// ErrCallExpired rejects a retry whose result expired but tombstone remains.
	ErrCallExpired = errors.New("call result has expired")
	// ErrCallMismatch rejects a call_id reused with a different call.
	ErrCallMismatch = errors.New("call_id already used with a different call")
	// ErrShutdown rejects new work after shutdown begins.
	ErrShutdown = errors.New("daemon is shut down")
)

// Options configures a Dispatcher. Nonpositive durations use their defaults.
type Options struct {
	Tools          []Tool
	DefaultTimeout time.Duration
	MaxTimeout     time.Duration
	ResultTTL      time.Duration
	TombstoneTTL   time.Duration
	Clock          Clock
	// ToolShutdownTimeout bounds each tool's shutdown hook; default ten seconds.
	ToolShutdownTimeout time.Duration
	// OnResult runs asynchronously in completion order, after result storage.
	// It must return promptly; Shutdown waits for queued callbacks.
	OnResult func(*circusv1.DeviceResult)
	// CallbackByteLimit bounds serialized backlog (default 128 MiB, 4096 items).
	// Overflow drops the notification; OnOverflow must not block.
	CallbackByteLimit int
	OnOverflow        func()
}

type cancelRequest struct {
	timer <-chan time.Time
	at    time.Time
}

type callRecord struct {
	cancel          func(error)
	cancelRequested chan cancelRequest
	cancelOnce      sync.Once
	done            chan struct{}
	result          *circusv1.DeviceResult
	completed       time.Time
	hash            [sha256.Size]byte
}

// Dispatcher owns call execution, deduplication, and in-memory result retention.
type Dispatcher struct {
	mu             sync.Mutex
	opts           Options
	tools          map[string]Tool
	calls          map[string]*callRecord
	closed         bool
	stopSweep      chan struct{}
	stopped        chan struct{}
	wg             sync.WaitGroup
	callbackQueue  [][]byte
	callbackBytes  int
	callbackWake   chan struct{}
	callbackClosed bool
	callbackDone   chan struct{}
}

// NewDispatcher builds a tool registry and starts retention sweeping.
func NewDispatcher(opts Options) (*Dispatcher, error) {
	if opts.DefaultTimeout <= 0 {
		opts.DefaultTimeout = 2 * time.Minute
	}
	if opts.MaxTimeout <= 0 {
		opts.MaxTimeout = 10 * time.Minute
	}
	if opts.ResultTTL <= 0 {
		opts.ResultTTL = 30 * time.Minute
	}
	if opts.TombstoneTTL <= 0 {
		opts.TombstoneTTL = 24 * time.Hour
	}
	if opts.ToolShutdownTimeout <= 0 {
		// Inferred: bound tool cleanup independently of a caller's wait context.
		opts.ToolShutdownTimeout = 10 * time.Second
	}
	if opts.CallbackByteLimit <= 0 {
		opts.CallbackByteLimit = 128 << 20
	}
	if opts.Clock == nil {
		opts.Clock = realClock{}
	}
	d := &Dispatcher{
		opts:         opts,
		tools:        make(map[string]Tool),
		calls:        make(map[string]*callRecord),
		stopSweep:    make(chan struct{}),
		stopped:      make(chan struct{}),
		callbackWake: make(chan struct{}, 1),
		callbackDone: make(chan struct{}),
	}
	for _, tool := range opts.Tools {
		if tool == nil {
			return nil, errors.New("nil tool")
		}
		name := tool.Name()
		if _, ok := d.tools[name]; ok {
			return nil, fmt.Errorf("duplicate tool name %q", name)
		}
		d.tools[name] = tool
	}
	tick := opts.Clock.After(time.Minute)
	go d.sweep(tick)
	go d.deliverResults()
	return d, nil
}

func (d *Dispatcher) sweep(tick <-chan time.Time) {
	for {
		select {
		case <-d.stopSweep:
			return
		case <-tick:
			d.mu.Lock()
			d.expireLocked()
			d.mu.Unlock()
			tick = d.opts.Clock.After(time.Minute)
		}
	}
}

func (d *Dispatcher) expireLocked() {
	now := d.opts.Clock.Now()
	for id, call := range d.calls {
		if call.cancel != nil {
			continue
		}
		age := now.Sub(call.completed)
		if age >= d.opts.ResultTTL {
			call.result = nil
		}
		if call.result == nil && age >= d.opts.TombstoneTTL {
			delete(d.calls, id)
		}
	}
}

// status reports the read-time state of a call. A completed record whose
// result is older than ResultTTL is reported as lost even if the record is
// still retained, so correctness does not depend on sweep timing.
func (d *Dispatcher) status(id string, call *callRecord, now time.Time) *circusv1.CallStatus {
	s := &circusv1.CallStatus{CallId: id, State: circusv1.CallStatus_STATE_LOST}
	if call != nil {
		if call.cancel != nil {
			s.State = circusv1.CallStatus_STATE_RUNNING
		} else if call.result != nil && now.Sub(call.completed) < d.opts.ResultTTL {
			s.State = circusv1.CallStatus_STATE_COMPLETED
			s.Result = proto.Clone(call.result).(*circusv1.DeviceResult)
		}
	}
	return s
}

// callHash fingerprints a submitted call so a reused call_id can be checked
// against the original submission.
func callHash(call *circusv1.DeviceCall) ([sha256.Size]byte, error) {
	data, err := proto.MarshalOptions{Deterministic: true}.Marshal(call)
	if err != nil {
		return [sha256.Size]byte{}, fmt.Errorf("marshal call: %w", err)
	}
	return sha256.Sum256(data), nil
}

// Submit snapshots and executes a call independently of its submitter.
// Running and retained IDs return their existing state without executing again,
// including during shutdown. ErrShutdown rejects only previously unseen IDs.
func (d *Dispatcher) Submit(call *circusv1.DeviceCall) (*circusv1.CallStatus, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if call == nil || !identifier.Valid(call.GetCallId(), false) || !identifier.Valid(call.GetSessionId(), false) || !identifier.Valid(call.GetAgentId(), true) {
		return nil, ErrInvalidCall
	}
	msg := call.ProtoReflect()
	field := msg.WhichOneof(msg.Descriptor().Oneofs().ByName("tool"))
	if field == nil {
		return nil, ErrInvalidCall
	}
	hash, err := callHash(call)
	if err != nil {
		return nil, err
	}
	now := d.opts.Clock.Now()
	if existing := d.calls[call.CallId]; existing != nil {
		if existing.cancel != nil || now.Sub(existing.completed) < d.opts.TombstoneTTL {
			if existing.hash != hash {
				return nil, ErrCallMismatch
			}
			if existing.cancel == nil && now.Sub(existing.completed) >= d.opts.ResultTTL {
				return nil, ErrCallExpired
			}
			return d.status(call.CallId, existing, now), nil
		}
		// Older than TombstoneTTL: treat the record as absent and execute again.
	}
	if d.closed {
		return nil, ErrShutdown
	}
	if d.tools[string(field.Name())] == nil {
		notify := faultResult(fmt.Sprintf("tool %q is not available on this device", field.Name()), circusv1.DaemonFault_CODE_TOOL_UNAVAILABLE)
		notify.CallId = call.CallId
		r := &callRecord{result: notify, completed: now, done: make(chan struct{}), hash: hash}
		close(r.done)
		d.calls[call.CallId] = r
		d.queueResultLocked(notify)
		return d.status(call.CallId, r, now), nil
	}
	call = proto.Clone(call).(*circusv1.DeviceCall)
	timeout := time.Duration(call.TimeoutMs) * time.Millisecond
	if timeout == 0 {
		timeout = d.opts.DefaultTimeout
	}
	if timeout > d.opts.MaxTimeout {
		timeout = d.opts.MaxTimeout
	}
	ctx := &deadlineContext{deadline: d.opts.Clock.Now().Add(timeout), done: make(chan struct{}), timeout: timeout, clock: d.opts.Clock}
	preparation := time.Duration(0)
	if tool, ok := d.tools[string(field.Name())].(PreparingTool); ok {
		preparation = tool.PreparationBudget()
	}
	if preparation > 0 {
		ctx.deadline = time.Time{}
		ctx.start = make(chan struct{})
	}
	r := &callRecord{cancel: ctx.cancel, cancelRequested: make(chan cancelRequest, 1), done: make(chan struct{}), hash: hash}
	d.calls[call.CallId] = r
	var deadline, fallback <-chan time.Time
	var fallbackAt time.Time
	if preparation > 0 {
		fallback = d.opts.Clock.After(preparation)
		fallbackAt = now.Add(preparation)
	} else {
		deadline = d.opts.Clock.After(timeout)
		fallback = d.opts.Clock.After(timeout + AbandonGrace)
		fallbackAt = now.Add(timeout + AbandonGrace)
	}
	d.wg.Add(1)
	go d.execute(ctx, call, r, string(field.Name()), deadline, fallback, fallbackAt)
	return d.status(call.CallId, r, now), nil
}

// deadlineContext uses Clock for cancellation instead of wall-clock timers.
// It implements Context directly so derived contexts inherit DeadlineExceeded.
type deadlineContext struct {
	deadline  time.Time
	done      chan struct{}
	mu        sync.Mutex
	err       error
	start     chan struct{}
	startOnce sync.Once
	timeout   time.Duration
	clock     Clock
}

// startTimeout is delivered through a context value so wrappers retain it.
func (c *deadlineContext) startTimeout() {
	c.startOnce.Do(func() {
		c.mu.Lock()
		c.deadline = c.clock.Now().Add(c.timeout)
		c.mu.Unlock()
		close(c.start)
	})
}

func (c *deadlineContext) Deadline() (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.deadline, !c.deadline.IsZero()
}
func (c *deadlineContext) Done() <-chan struct{} { return c.done }
func (c *deadlineContext) Value(any) any         { return nil }
func (c *deadlineContext) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}
func (c *deadlineContext) cancel(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err == nil {
		c.err = err
		close(c.done)
	}
}

// AbandonGrace is how long a timed-out or cancelled tool may take to return
// before its call completes with an ABANDONED_* fault.
const AbandonGrace = 5 * time.Second

func faultResult(message string, code circusv1.DaemonFault_Code) *circusv1.DeviceResult {
	result := errorResult(message)
	result.Fault = &circusv1.DaemonFault{Code: code, Message: code.String()}
	return result
}

func errorResult(message string) *circusv1.DeviceResult {
	return &circusv1.DeviceResult{Output: &circusv1.ToolOutput{IsError: true, Content: []*circusv1.ContentBlock{{Block: &circusv1.ContentBlock_Text{Text: "Error: " + textutil.NodeUTF8([]byte(message))}}}}}
}

func runTool(ctx context.Context, tool Tool, call *circusv1.DeviceCall) (result *circusv1.DeviceResult) {
	defer func() {
		if value := recover(); value != nil {
			slog.Error("tool panicked", "tool", tool.Name(), "panic", value, "stack", string(debug.Stack()))
			result = faultResult(fmt.Sprint(value), circusv1.DaemonFault_CODE_PANIC)
		}
	}()
	result, err := tool.Run(ctx, call)
	if err != nil {
		return faultResult(err.Error(), circusv1.DaemonFault_CODE_INTERNAL)
	}
	if result == nil {
		return faultResult("tool returned no result", circusv1.DaemonFault_CODE_INTERNAL)
	}
	return result
}

func (d *Dispatcher) execute(ctx *deadlineContext, call *circusv1.DeviceCall, r *callRecord, name string, deadline, fallback <-chan time.Time, fallbackAt time.Time) {
	defer d.wg.Done()
	tool := d.tools[name]
	id := call.CallId
	returned := make(chan *circusv1.DeviceResult, 1)
	toolCtx := context.Context(ctx)
	if ctx.start != nil {
		toolCtx = toolcontext.WithTimeoutStarter(ctx, ctx.startTimeout)
	}
	go func() { returned <- runTool(toolCtx, tool, call) }()
	start := ctx.start
	cancelRequested := r.cancelRequested
	cancelSeen, cancelFallback := false, false
	for {
		select {
		case <-start:
			start = nil
			if !cancelSeen {
				end, _ := ctx.Deadline()
				now := d.opts.Clock.Now()
				remaining := end.Sub(now)
				deadline = d.opts.Clock.After(remaining)
				fallback = d.opts.Clock.After(remaining + AbandonGrace)
				fallbackAt = now.Add(remaining + AbandonGrace)
			}
		case result := <-returned:
			d.complete(id, r, result)
			return
		case <-deadline:
			r.cancel(context.DeadlineExceeded)
			deadline = nil
		case request := <-cancelRequested:
			cancelRequested = nil
			cancelSeen = true
			if fallback == nil || request.at.Before(fallbackAt) {
				fallback = request.timer
				fallbackAt = request.at
				cancelFallback = true
			}
		case <-fallback:
			// Also cancel when a fake clock advances past both timers at once.
			r.cancel(context.DeadlineExceeded)
			code := circusv1.DaemonFault_CODE_ABANDONED_AFTER_TIMEOUT
			message := "tool did not stop after its timeout"
			if cancelFallback {
				code = circusv1.DaemonFault_CODE_ABANDONED_AFTER_CANCEL
				message = "tool did not stop after cancellation"
			} else if end, ok := ctx.Deadline(); !ok || end.IsZero() {
				// Inferred: distinguish preparation exhaustion from a command timeout.
				code = circusv1.DaemonFault_CODE_PREPARATION_EXHAUSTED
				message = "tool preparation exceeded its budget"
			}
			d.complete(id, r, faultResult(message, code))
			return
		}
	}
}

func (d *Dispatcher) complete(id string, r *callRecord, result *circusv1.DeviceResult) {
	result = proto.Clone(result).(*circusv1.DeviceResult)
	result.CallId = id
	d.mu.Lock()
	r.cancel(context.Canceled)
	r.cancel = nil
	r.result = result
	r.completed = d.opts.Clock.Now()
	close(r.done)
	d.queueResultLocked(result)
	d.mu.Unlock()
}

// Queue under the state lock so callbacks follow the order results were stored.
func (d *Dispatcher) queueResultLocked(result *circusv1.DeviceResult) {
	if d.opts.OnResult == nil {
		return
	}
	n := proto.Size(result)
	if n > d.opts.CallbackByteLimit-d.callbackBytes || len(d.callbackQueue) >= 4096 {
		if d.opts.OnOverflow != nil {
			d.opts.OnOverflow()
		}
		return
	}
	data, err := proto.Marshal(result)
	if err != nil {
		if d.opts.OnOverflow != nil {
			d.opts.OnOverflow()
		}
		return
	}
	d.callbackQueue = append(d.callbackQueue, data)
	d.callbackBytes += len(data)
	select {
	case d.callbackWake <- struct{}{}:
	default:
	}
}

func (d *Dispatcher) deliverResults() {
	defer close(d.callbackDone)
	for {
		d.mu.Lock()
		if len(d.callbackQueue) == 0 {
			closed := d.callbackClosed
			d.mu.Unlock()
			if closed {
				return
			}
			<-d.callbackWake
			continue
		}
		data := d.callbackQueue[0]
		d.callbackBytes -= len(data)
		d.callbackQueue[0] = nil
		d.callbackQueue = d.callbackQueue[1:]
		d.mu.Unlock()
		result := new(circusv1.DeviceResult)
		if proto.Unmarshal(data, result) == nil {
			d.opts.OnResult(result)
		}
	}
}

// Query returns a snapshot, reporting lost for unknown or expired results.
func (d *Dispatcher) Query(callID string) *circusv1.CallStatus {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.status(callID, d.calls[callID], d.opts.Clock.Now())
}

// Wait waits for completion; cancelling ctx leaves the call running.
func (d *Dispatcher) Wait(ctx context.Context, callID string) (*circusv1.CallStatus, error) {
	d.mu.Lock()
	r := d.calls[callID]
	if r == nil || r.cancel == nil {
		s := d.status(callID, r, d.opts.Clock.Now())
		d.mu.Unlock()
		return s, nil
	}
	d.mu.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-r.done:
		return d.Query(callID), nil
	}
}

// requestCancel interrupts a tool and starts its independent abandonment grace period.
func (d *Dispatcher) requestCancel(r *callRecord) {
	r.cancel(context.Canceled)
	r.cancelOnce.Do(func() {
		at := d.opts.Clock.Now().Add(AbandonGrace)
		r.cancelRequested <- cancelRequest{timer: d.opts.Clock.After(AbandonGrace), at: at}
	})
}

// Cancel requests interruption and reports whether the call is still running.
func (d *Dispatcher) Cancel(callID string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if r := d.calls[callID]; r != nil && r.cancel != nil {
		d.requestCancel(r)
		return true
	}
	return false
}

// Shutdown rejects new calls, cancels running calls, and drains callbacks.
// If ctx expires, shutdown continues and may be waited on again.
func (d *Dispatcher) Shutdown(ctx context.Context) error {
	d.mu.Lock()
	d.expireLocked()
	if !d.closed {
		d.closed = true
		close(d.stopSweep)
		for _, r := range d.calls {
			if r.cancel != nil {
				d.requestCancel(r)
			}
		}
		go func() {
			d.wg.Wait()
			// Tools with internal processes finish their own shutdown after calls
			// settle. Late job-to-group transfers are stopped by the closed tracker.
			for _, tool := range d.tools {
				if closer, ok := tool.(interface{ Shutdown(context.Context) error }); ok {
					cleanupCtx, cancel := context.WithTimeout(context.Background(), d.opts.ToolShutdownTimeout)
					if err := closer.Shutdown(cleanupCtx); err != nil {
						slog.Warn("tool shutdown failed", "tool", tool.Name(), "error", err)
					}
					cancel()
				}
			}
			d.mu.Lock()
			d.callbackClosed = true
			select {
			case d.callbackWake <- struct{}{}:
			default:
			}
			d.mu.Unlock()
			<-d.callbackDone
			close(d.stopped)
		}()
	}
	d.mu.Unlock()
	select {
	case <-d.stopped:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
