package bashtool

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/daemon/jobs"
	"github.com/EricWvi/circus/internal/testutil/leakcheck"
)

type triggeredDeadline struct {
	context.Context
	done     chan struct{}
	expired  atomic.Bool
	deadline time.Time
}

func (c *triggeredDeadline) Done() <-chan struct{} { return c.done }
func (c *triggeredDeadline) Err() error {
	if c.expired.Load() {
		return context.DeadlineExceeded
	}
	return nil
}
func (c *triggeredDeadline) Deadline() (time.Time, bool) { return c.deadline, true }
func (c *triggeredDeadline) expire() {
	if c.expired.CompareAndSwap(false, true) {
		close(c.done)
	}
}

type runReply struct {
	result *circusv1.DeviceResult
	err    error
}
type drivenCapture struct {
	name, dir, mode string
	c               *capture
	cancel          context.CancelFunc
	expiry          *triggeredDeadline
	done            chan runReply
	reply           *runReply
	write           int
}
type captureHarness struct {
	t        *testing.T
	b        *Bash
	root     string
	mu       sync.Mutex
	observed map[string]chan *capture
	runs     []*drivenCapture
}

func newCaptureHarness(t *testing.T, limit, total int64) *captureHarness {
	t.Helper()
	b, root, _ := reviewBashWithLimits(t, limit, total)
	h := &captureHarness{t: t, b: b, root: root, observed: make(map[string]chan *capture)}
	b.effects.started = func(c *capture) {
		h.mu.Lock()
		ch := h.observed[filepath.Base(c.cmd.Dir)]
		h.mu.Unlock()
		if ch != nil {
			ch <- c
		}
	}
	t.Cleanup(func() {
		for _, r := range h.runs {
			r.cancel()
			r.expiry.expire()
			_ = os.WriteFile(filepath.Join(r.dir, "exit"), nil, 0600)
			_ = os.WriteFile(filepath.Join(r.dir, "child-exit"), nil, 0600)
		}
		_ = b.jobs.Shutdown(context.Background())
		_ = b.Shutdown(context.Background())
		for _, r := range h.runs {
			if r.reply == nil {
				select {
				case reply := <-r.done:
					r.reply = &reply
				case <-time.After(3 * time.Second):
					t.Error("Run did not return", r.name)
				}
			}
		}
	})
	return h
}
func gate(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
}
func (h *captureHarness) start(name, mode string, children bool, exitCode int, trap string, lifetime uint32) *drivenCapture {
	command := `echo ready > ready; while [ ! -f exit ]; do for f in write-*; do [ -f "$f" ] || continue; n=$(cat "$f"); id=${f#write-}; if [ -f "hold-$id" ]; then : > "writing-$id"; while [ ! -f "go-$id" ]; do sleep 0.005; done; fi; head -c "$n" /dev/zero; mv "$f" "done-$id"; done; sleep 0.005; done; exit ` + fmt.Sprint(exitCode)
	if trap != "" {
		command = `trap '` + trap + `' TERM; ` + command
	}
	if children {
		command = `(echo child > child-ready; while [ ! -f child-exit ]; do sleep 0.01; done) & echo $! > child.pid; ` + command
	}
	return h.startCommand(name, mode, command, lifetime)
}
func (h *captureHarness) startCommand(name, mode, command string, lifetime uint32) *drivenCapture {
	h.t.Helper()
	dir := filepath.Join(h.root, name)
	if err := os.Mkdir(dir, 0700); err != nil {
		h.t.Fatal(err)
	}
	expiry := &triggeredDeadline{Context: context.Background(), done: make(chan struct{}), deadline: time.Now().Add(time.Second)}
	base := context.Context(context.Background())
	if mode == "promoted" {
		base = expiry
	}
	ctx, cancel := context.WithCancel(base)
	r := &drivenCapture{name: name, dir: dir, mode: mode, cancel: cancel, expiry: expiry, done: make(chan runReply, 1)}
	h.runs = append(h.runs, r)
	ch := make(chan *capture, 1)
	h.mu.Lock()
	h.observed[name] = ch
	h.mu.Unlock()
	go func() {
		result, err := h.b.Run(ctx, &circusv1.DeviceCall{CallId: name, SessionId: "review", TimeoutMs: lifetime, Tool: &circusv1.DeviceCall_Bash{Bash: &circusv1.BashRequest{Cwd: dir, Command: command, RunInBackground: mode == "background" || mode == "held-background"}}})
		r.done <- runReply{result, err}
	}()
	select {
	case r.c = <-ch:
	case reply := <-r.done:
		r.reply = &reply
		if reply.result != nil {
			leakcheck.Expect(reply.result.GetBash().GetOutputPath())
		}
		// A quick background Run can publish both channels before this select.
		// started is sent synchronously before Run returns, so prefer that evidence.
		select {
		case r.c = <-ch:
		default:
			h.t.Fatalf("capture %s did not start: %v", name, reply.err)
		}
	case <-time.After(4 * time.Second):
		h.t.Fatal("capture did not start", name)
	}
	await(h.t, func() bool { _, err := os.Stat(filepath.Join(dir, "ready")); return err == nil })
	if mode == "background" {
		h.returned(r)
	}
	return r
}
func (h *captureHarness) returned(r *drivenCapture) *runReply {
	h.t.Helper()
	if r.reply == nil {
		select {
		case reply := <-r.done:
			r.reply = &reply
		case <-time.After(4 * time.Second):
			h.t.Fatal("Run blocked", r.name)
		}
	}
	if r.reply.result != nil {
		leakcheck.Expect(r.reply.result.GetBash().GetOutputPath())
	}
	return r.reply
}
func (h *captureHarness) promote(r *drivenCapture) {
	h.t.Helper()
	r.expiry.expire()
	reply := h.returned(r)
	if reply.err != nil || reply.result.GetBash().GetJobId() == "" {
		h.t.Fatal("promotion failed", reply.err, reply.result)
	}
}
func (h *captureHarness) publishFill(r *drivenCapture, id, n int) error {
	pending := filepath.Join(r.dir, fmt.Sprintf("pending-%d", id))
	if err := os.WriteFile(pending, []byte(fmt.Sprint(n)), 0600); err != nil {
		return err
	}
	// Publish content atomically: an empty newly created file is not a request.
	return os.Rename(pending, filepath.Join(r.dir, fmt.Sprintf("write-%d", id)))
}
func (h *captureHarness) awaitFill(r *drivenCapture, id int) {
	h.t.Helper()
	await(h.t, func() bool { _, err := os.Stat(filepath.Join(r.dir, fmt.Sprintf("done-%d", id))); return err == nil })
}
func (h *captureHarness) fill(r *drivenCapture, n int, terminal bool) {
	h.t.Helper()
	id := r.write
	r.write++
	if err := h.publishFill(r, id, n); err != nil {
		h.t.Fatal(err)
	}
	if !terminal {
		h.awaitFill(r, id)
	}
}
func (h *captureHarness) job(r *drivenCapture) jobs.Info {
	h.t.Helper()
	reply := h.returned(r)
	if reply.err != nil {
		h.t.Fatal(reply.err)
	}
	id := reply.result.GetBash().GetJobId()
	if id == "" {
		h.t.Fatal("no job", r.name)
	}
	var info jobs.Info
	await(h.t, func() bool {
		var ok bool
		info, ok = h.b.jobs.Get(id)
		return ok && info.State != circusv1.JobEvent_STATE_RUNNING
	})
	return info
}
func (h *captureHarness) stop(r *drivenCapture) {
	h.t.Helper()
	reply := h.returned(r)
	result, err := NewTaskStop(h.b.jobs).Run(context.Background(), &circusv1.DeviceCall{SessionId: "review", Tool: &circusv1.DeviceCall_TaskStop{TaskStop: &circusv1.TaskStopRequest{JobId: reply.result.GetBash().JobId}}})
	if err != nil || result.Output.IsError {
		h.t.Fatal("TaskStop", err, result)
	}
}
func (h *captureHarness) resources() {
	h.t.Helper()
	await(h.t, func() bool {
		h.b.outputs.mu.Lock()
		a := len(h.b.outputs.active)
		h.b.outputs.mu.Unlock()
		h.b.outputs.outputBudget.mu.Lock()
		b := len(h.b.outputs.outputBudget.files)
		h.b.outputs.outputBudget.mu.Unlock()
		h.b.groups.mu.Lock()
		g := len(h.b.groups.stops)
		h.b.groups.mu.Unlock()
		h.b.snapshots.mu.Lock()
		leases := len(h.b.snapshots.leases)
		h.b.snapshots.mu.Unlock()
		return a == 0 && b == 0 && g == 0 && leases == 0
	})
}
func (h *captureHarness) released(r *drivenCapture, phase capturePhase, keep bool) {
	h.t.Helper()
	select {
	case <-r.c.released:
	case <-time.After(3 * time.Second):
		h.t.Fatal("capture not released", r.name)
	}
	if state := r.c.snapshotState(); state != (captureState{phase, ownerReleased}) {
		h.t.Fatal("capture state", r.name, state, "want", phase)
	}
	if groupAlive(r.c.pgid()) {
		h.t.Fatal("group remains alive", r.c.pgid())
	}
	if _, err := r.c.out.Stat(); !errors.Is(err, os.ErrClosed) {
		h.t.Fatal("descriptor not closed", err)
	}
	_, err := os.Stat(r.c.out.Name())
	if keep && err != nil || !keep && !os.IsNotExist(err) {
		h.t.Fatal("file retention", keep, err)
	}
}

// Rows describe public operations and expected results, never processOutcome or
// finishing methods. Both foreground and background rows go through Run.
func TestCaptureTransitions(t *testing.T) {
	rows := []struct {
		name, mode, event string
		children          bool
		exit              int
		trap              string
		lifetime          uint32
		phase             capturePhase
		code              int
		state             circusv1.JobEvent_State
		message           string
		keep              bool
	}{
		{name: "foreground-exit", mode: "foreground", event: "exit", phase: captureExited},
		{name: "foreground-failure", mode: "foreground", event: "exit", exit: 7, phase: captureExited, code: 7},
		{name: "foreground-children", mode: "foreground", event: "exit", children: true, phase: captureEmpty},
		{name: "foreground-failed-children", mode: "foreground", event: "exit", children: true, exit: 7, phase: captureEmpty, code: 7},
		{name: "foreground-persisted", mode: "foreground", event: "persist", phase: captureExited, keep: true},
		{name: "foreground-cancel", mode: "foreground", event: "cancel", children: true, phase: captureStopped, code: 143},
		{name: "foreground-cap", mode: "foreground", event: "cap", children: true, phase: captureKilled, code: 137, message: outputLimitMessage},
		{name: "background-exit", mode: "background", event: "exit", phase: captureExited, state: circusv1.JobEvent_STATE_EXITED, keep: true},
		{name: "background-failure", mode: "background", event: "exit", exit: 7, phase: captureExited, code: 7, state: circusv1.JobEvent_STATE_EXITED, keep: true},
		{name: "background-failed-children", mode: "background", event: "exit", children: true, exit: 7, phase: captureEmpty, code: 7, state: circusv1.JobEvent_STATE_EXITED, keep: true},
		{name: "promoted-failed-children", mode: "promoted", event: "exit", children: true, exit: 7, phase: captureEmpty, code: 7, state: circusv1.JobEvent_STATE_EXITED, keep: true},
		{name: "background-children", mode: "background", event: "exit", children: true, phase: captureEmpty, state: circusv1.JobEvent_STATE_EXITED, keep: true},
		{name: "background-stop", mode: "background", event: "stop", children: true, phase: captureKilled, code: 143, state: circusv1.JobEvent_STATE_KILLED, keep: true},
		{name: "background-trap-stop", mode: "background", event: "stop", trap: "exit 7", phase: captureStopped, code: 7, state: circusv1.JobEvent_STATE_EXITED, keep: true},
		{name: "background-lifetime-trap", mode: "background", event: "lifetime", trap: "exit 7", lifetime: 150, phase: captureKilled, code: 7, state: circusv1.JobEvent_STATE_KILLED, keep: true},
		{name: "background-cap", mode: "background", event: "cap", phase: captureKilled, code: 137, state: circusv1.JobEvent_STATE_KILLED, message: outputLimitMessage, keep: true},
		{name: "promote-exit", mode: "promoted", event: "exit", phase: captureExited, state: circusv1.JobEvent_STATE_EXITED, keep: true},
		{name: "promote-children", mode: "promoted", event: "exit", children: true, phase: captureEmpty, state: circusv1.JobEvent_STATE_EXITED, keep: true},
		{name: "promote-stop", mode: "promoted", event: "stop", phase: captureKilled, code: 143, state: circusv1.JobEvent_STATE_KILLED, keep: true},
		{name: "promote-cap", mode: "promoted", event: "cap", phase: captureKilled, code: 137, state: circusv1.JobEvent_STATE_KILLED, message: outputLimitMessage, keep: true},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			limit := int64(1 << 20)
			if row.event == "cap" {
				limit = 1024
			}
			h := newCaptureHarness(t, limit, 0)
			r := h.start("A", row.mode, row.children, row.exit, row.trap, row.lifetime)
			inspector, err := os.Open(r.c.out.Name())
			if err != nil {
				t.Fatal(err)
			}
			defer inspector.Close()
			if row.mode == "promoted" {
				h.promote(r)
			}
			switch row.event {
			case "exit":
				gate(t, filepath.Join(r.dir, "exit"))
			case "persist":
				h.fill(r, 31000, false)
				gate(t, filepath.Join(r.dir, "exit"))
			case "cancel":
				r.cancel()
			case "stop":
				h.stop(r)
			case "cap":
				h.fill(r, 2048, true)
			case "lifetime":
			}
			if row.mode == "foreground" {
				reply := h.returned(r)
				if reply.err != nil || int(reply.result.GetBash().ExitCode) != row.code || reply.result.GetBash().JobId != "" || (row.event == "cancel" && !reply.result.GetBash().Interrupted) {
					t.Fatal(reply.err, reply.result)
				}
			} else {
				info := h.job(r)
				if info.State != row.state || int(info.ExitCode) != row.code {
					t.Fatal("job outcome", info)
				}
			}
			if row.mode != "foreground" && row.event == "exit" {
				reply := h.returned(r)
				stopped, err := NewTaskStop(h.b.jobs).Run(context.Background(), &circusv1.DeviceCall{SessionId: "review", Tool: &circusv1.DeviceCall_TaskStop{TaskStop: &circusv1.TaskStopRequest{JobId: reply.result.GetBash().JobId}}})
				if err != nil || !stopped.Output.IsError {
					t.Fatal("late TaskStop changed natural completion", err, stopped)
				}
				if info := h.job(r); info.State != row.state || int(info.ExitCode) != row.code {
					t.Fatal("late Stop changed job", info)
				}
			}
			if row.children && row.event == "exit" {
				if state := r.c.snapshotState(); state != (captureState{captureTracked, ownerTracker}) {
					t.Fatal("no tracker", state)
				}
				if _, err := os.Stat(r.c.out.Name()); err != nil {
					t.Fatal("premature unlink", err)
				}
				gate(t, filepath.Join(r.dir, "child-exit"))
			}
			h.released(r, row.phase, row.keep)
			h.resources()
			tail := outputTail(t, inspector)
			want := 0
			if row.message != "" {
				want = 1
			}
			if strings.Count(tail, "[circus:") != want || want == 1 && !strings.HasSuffix(tail, row.message) {
				t.Fatal("diagnostic", tail)
			}
		})
	}
}

func TestCaptureShutdownBeforeHandover(t *testing.T) {
	for _, mode := range []string{"background", "promoted"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			h := newCaptureHarness(t, 0, 0)
			r := h.start("A", mode, true, 0, "", 0)
			if mode == "promoted" {
				h.promote(r)
			}
			if err := h.b.Shutdown(context.Background()); err != nil {
				t.Fatal(err)
			}
			gate(t, filepath.Join(r.dir, "exit"))
			info := h.job(r)
			if info.State != circusv1.JobEvent_STATE_EXITED || info.ExitCode != 0 {
				t.Fatal(info)
			}
			h.released(r, captureEmpty, true)
			h.resources()
		})
	}
}

func TestCaptureTotalVictimChildrenUseKILL(t *testing.T) {
	t.Parallel()
	h := newCaptureHarness(t, 1<<20, 10000)
	entered, resume := make(chan struct{}, 1), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(resume) })
	h.b.effects.delay = func(step string) {
		if step == "finish-shell" {
			entered <- struct{}{}
			<-resume
		}
	}
	small := h.start("small", "background", false, 0, "", 0)
	h.fill(small, 4096, false)
	victim := h.startCommand("large", "foreground", `(trap '' TERM; echo child > child-ready; while [ ! -f grow ]; do sleep 0.01; done; printf '%08192d' 0; echo grown > grown; while [ ! -f child-exit ]; do sleep 0.01; done) & echo ready > ready; while [ ! -f exit ]; do sleep 0.005; done; exit 0`, 0)
	gate(t, filepath.Join(victim.dir, "exit"))
	<-entered
	// The shell has actually exited. Its child now grows the file and the other
	// real command's shared poll selects this largest victim before finishing.
	gate(t, filepath.Join(victim.dir, "grow"))
	await(t, func() bool { _, err := os.Stat(filepath.Join(victim.dir, "grown")); return err == nil })
	await(t, func() bool {
		a := &h.b.outputs.outputBudget
		a.mu.Lock()
		defer a.mu.Unlock()
		r := a.files[victim.c.out.Name()]
		return r != nil && r.message == totalOutputLimitMessage
	})
	start := time.Now()
	once.Do(func() { close(resume) })
	reply := h.returned(victim)
	if reply.err != nil || reply.result.GetBash().ExitCode != 137 || !strings.Contains(reply.result.Output.Content[0].GetText(), strings.TrimSpace(totalOutputLimitMessage)) {
		t.Fatal("aggregate victim result", reply.err, reply.result.GetBash())
	}
	if elapsed := time.Since(start); elapsed >= 800*time.Millisecond {
		t.Fatal("aggregate cap used TERM grace", elapsed)
	}
	h.released(victim, captureKilled, false)
	id := h.returned(small).result.GetBash().JobId
	if info, _ := h.b.jobs.Get(id); info.State != circusv1.JobEvent_STATE_RUNNING {
		t.Fatal("wrong victim", info)
	}
	h.stop(small)
	h.job(small)
	h.released(small, captureKilled, true)
	h.resources()
}

func TestCaptureFormatFailureStopsChildren(t *testing.T) {
	for _, step := range []string{"format-read", "format-stat"} {
		t.Run(step, func(t *testing.T) {
			t.Parallel()
			h := newCaptureHarness(t, 0, 0)
			boom := errors.New("format failure")
			h.b.effects.before = func(s string) error {
				if s == step {
					return boom
				}
				return nil
			}
			r := h.start("A", "foreground", true, 0, "", 0)
			gate(t, filepath.Join(r.dir, "exit"))
			reply := h.returned(r)
			if !errors.Is(reply.err, boom) {
				t.Fatal("missing formatting error", reply.err)
			}
			h.released(r, captureExited, false)
			h.resources()
		})
	}
}

func TestCaptureBackgroundWriteErrors(t *testing.T) {
	for _, step := range []string{"exit-marker", "explain", "close"} {
		for _, code := range []int{0, 7} {
			t.Run(fmt.Sprintf("%s/%d", step, code), func(t *testing.T) {
				t.Parallel()
				limit := int64(1 << 20)
				if step == "explain" {
					limit = 1024
				}
				h := newCaptureHarness(t, limit, 0)
				boom := errors.New("write/close failed")
				if step == "close" {
					h.b.effects.after = func(s string, err error) error {
						if s == "close" {
							return boom
						}
						return err
					}
				} else {
					h.b.effects.before = func(s string) error {
						if s == step {
							return boom
						}
						return nil
					}
				}
				if step == "explain" {
					exited := make(chan struct{})
					var once sync.Once
					h.b.effects.delay = func(s string) {
						if s == "shell-exited" {
							once.Do(func() { close(exited) })
						}
						if s == "output-check" {
							<-exited
						}
					}
				}
				r := h.start("A", "background", false, code, "", 0)
				if step == "explain" {
					h.fill(r, 2048, false)
				}
				gate(t, filepath.Join(r.dir, "exit"))
				info := h.job(r)
				actualCode, err := r.c.job.Wait()
				if !errors.Is(err, boom) {
					t.Fatal("lost background error", err)
				}
				wantCode := code
				wantState := circusv1.JobEvent_STATE_EXITED
				phase := captureExited
				if step == "explain" {
					wantState = circusv1.JobEvent_STATE_KILLED
					phase = captureKilled
				} else if code == 0 {
					wantCode = -1
				}
				if info.State != wantState || int(info.ExitCode) != wantCode || actualCode != code {
					t.Fatal("job error mapping", info, actualCode, err)
				}
				h.released(r, phase, true)
				h.resources()
			})
		}
	}
}

func TestCaptureWrongDriverRelease(t *testing.T) {
	t.Parallel()
	h := newCaptureHarness(t, 0, 0)
	r := h.start("A", "background", false, 0, "", 0)
	if err := r.c.release(ownerForeground, deleteOutput); err == nil {
		t.Fatal("non-owner release succeeded")
	}
	if _, err := r.c.out.Stat(); err != nil {
		t.Fatal("non-owner closed file", err)
	}
	gate(t, filepath.Join(r.dir, "exit"))
	h.job(r)
	h.released(r, captureExited, true)
	h.resources()
}

func TestCaptureTrackerHandoverOrder(t *testing.T) {
	t.Parallel()
	h := newCaptureHarness(t, 0, 0)
	entered, resume := make(chan struct{}, 1), make(chan struct{})
	defer close(resume)
	h.b.effects.delay = func(step string) {
		if step == "tracker-register" {
			entered <- struct{}{}
			<-resume
		}
	}
	r := h.start("A", "background", true, 0, "", 0)
	gate(t, filepath.Join(r.dir, "exit"))
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("no tracker handover")
	}
	if state := r.c.snapshotState(); state != (captureState{captureTracked, ownerTracker}) {
		t.Fatal("tracker registered before ownership transfer", state)
	}
	r.c.mu.Lock()
	entry := r.c.tracking
	r.c.mu.Unlock()
	h.b.groups.mu.Lock()
	registered := entry != nil && h.b.groups.stops[r.c.pgid()] == entry
	h.b.groups.mu.Unlock()
	if !registered {
		t.Fatal("ownership transferred without atomic tracker registration")
	}
	gate(t, filepath.Join(r.dir, "child-exit"))
	resume <- struct{}{}
	if info := h.job(r); info.State != circusv1.JobEvent_STATE_EXITED || info.ExitCode != 0 {
		t.Fatal(info)
	}
	h.released(r, captureEmpty, true)
	h.resources()
}

func TestCaptureTrackerEntriesReleased(t *testing.T) {
	t.Parallel()
	h := newCaptureHarness(t, 0, 0)
	r := h.start("A", "background", true, 0, "", 0)
	gate(t, filepath.Join(r.dir, "exit"))
	h.job(r)
	gate(t, filepath.Join(r.dir, "child-exit"))
	h.released(r, captureEmpty, true)
	h.resources()
}

func TestCaptureTrackerEntryIdentity(t *testing.T) {
	t.Parallel()
	h := newCaptureHarness(t, 0, 0)
	reached, resume := make(chan struct{}, 1), make(chan struct{})
	removed := make(chan struct{}, 2)
	defer close(resume)
	var blocked atomic.Bool
	h.b.effects.delay = func(step string) {
		if step == "tracker-removed" {
			removed <- struct{}{}
		}
		if step == "tracker-remove" && blocked.CompareAndSwap(false, true) {
			reached <- struct{}{}
			<-resume
		}
	}
	a := h.start("A", "background", true, 0, "", 0)
	b := h.start("B", "background", true, 0, "", 0)
	gate(t, filepath.Join(a.dir, "exit"))
	h.job(a)
	gate(t, filepath.Join(b.dir, "exit"))
	h.job(b)
	gate(t, filepath.Join(a.dir, "child-exit"))
	<-reached
	a.c.mu.Lock()
	old := a.c.tracking
	a.c.mu.Unlock()
	b.c.mu.Lock()
	replacement := b.c.tracking
	b.c.mu.Unlock()
	// Emulate pgid reuse in the index; both entries still belong to real captures.
	h.b.groups.mu.Lock()
	h.b.groups.stops[a.c.pgid()] = replacement
	h.b.groups.mu.Unlock()
	t.Cleanup(func() { h.b.groups.mu.Lock(); delete(h.b.groups.stops, a.c.pgid()); h.b.groups.mu.Unlock() })
	h.b.stopTracked(a.c)
	select {
	case <-old.stop:
	default:
		t.Fatal("did not stop own entry")
	}
	select {
	case <-replacement.stop:
		t.Fatal("stopped replacement entry")
	default:
	}
	resume <- struct{}{}
	<-removed
	// Wait for the old tracker defer to finish before inspecting its replacement.
	h.b.groups.mu.Lock()
	if h.b.groups.stops[a.c.pgid()] != replacement {
		t.Error("old capture removed replacement")
	}
	delete(h.b.groups.stops, a.c.pgid())
	h.b.groups.mu.Unlock()
	gate(t, filepath.Join(b.dir, "child-exit"))
	h.released(a, captureEmpty, true)
	h.released(b, captureEmpty, true)
	h.resources()
}

func TestCaptureFaults(t *testing.T) {
	// All listed operations have a real production error branch. Close's injected
	// error is returned after real Close, representing a reported I/O close error.
	for _, step := range []string{"cwd-open", "open", "file-lock", "start", "register-job", "promotion-stat", "format-read", "format-stat", "remove"} {
		t.Run(step, func(t *testing.T) {
			t.Parallel()
			h := newCaptureHarness(t, 0, 0)
			boom := errors.New("injected " + step)
			var hits atomic.Int32
			h.b.effects.before = func(s string) error {
				if s == step {
					hits.Add(1)
					return boom
				}
				return nil
			}
			var closed atomic.Int32
			if step == "file-lock" {
				h.b.effects.after = func(s string, err error) error {
					if s == "close" && err == nil {
						closed.Add(1)
					}
					return err
				}
			}
			if step == "cwd-open" || step == "open" || step == "file-lock" || step == "start" || step == "register-job" {
				result, err := h.b.Run(context.Background(), &circusv1.DeviceCall{SessionId: "review", Tool: &circusv1.DeviceCall_Bash{Bash: &circusv1.BashRequest{Cwd: h.root, Command: "true", RunInBackground: step == "register-job"}}})
				if !errors.Is(err, boom) || result != nil {
					t.Fatal("acquisition failure", err, result)
				}
			} else {
				mode := "foreground"
				if step == "promotion-stat" {
					mode = "promoted"
				}
				r := h.start("A", mode, false, 0, "", 0)
				if mode == "promoted" {
					r.expiry.expire()
				} else {
					gate(t, filepath.Join(r.dir, "exit"))
				}
				reply := h.returned(r)
				if !errors.Is(reply.err, boom) {
					t.Fatal("lost operation error", reply.err)
				}
				// A failed unlink really leaves the pathname. Assert that branch before
				// removing the test's simulated inaccessible file; no allowance is added.
				if step == "remove" {
					if _, err := os.Stat(r.c.out.Name()); err != nil {
						t.Fatal("unlink fault did not occur", err)
					}
					if err := os.Remove(r.c.out.Name()); err != nil {
						t.Fatal(err)
					}
				}
				phase := captureExited
				if mode == "promoted" {
					phase = captureKilled
				}
				h.released(r, phase, false)
				if groupAlive(r.c.pgid()) {
					t.Fatal("error path left live group")
				}
				if _, err := r.c.out.Stat(); !errors.Is(err, os.ErrClosed) {
					t.Fatal("error path left fd", err)
				}
			}
			if step == "file-lock" && closed.Load() != 1 {
				t.Fatal("file-lock failure did not close the output descriptor")
			}
			if hits.Load() == 0 {
				t.Fatal("fault branch not reached")
			}
			h.resources()
		})
	}
}

func TestCaptureForegroundCloseError(t *testing.T) {
	t.Parallel()
	h := newCaptureHarness(t, 0, 0)
	boom := errors.New("close failed")
	h.b.effects.after = func(step string, err error) error {
		if step == "close" {
			return boom
		}
		return err
	}
	r := h.start("A", "foreground", false, 0, "", 0)
	gate(t, filepath.Join(r.dir, "exit"))
	reply := h.returned(r)
	if reply.err != nil || reply.result.GetBash().ExitCode != 0 {
		t.Fatal("foreground must preserve ignored Close error", reply.err, reply.result)
	}
	h.released(r, captureExited, false)
	h.resources()
}

func TestCaptureDelays(t *testing.T) {
	steps := []string{"cwd-open", "open", "file-lock", "start", "register-job", "promotion-stat", "format-read", "format-stat", "exit-marker", "explain", "close", "remove", "budget-add", "budget-start", "wait-shell", "shell-exited", "stop", "kill", "wait", "kill-group", "stop-group", "budget-finish", "budget-remove", "active-remove", "lease-retain", "lease-release", "tracker-register", "tracker-remove", "tracker-removed", "output-check", "job-launched", "finish-shell"}
	for _, step := range steps {
		t.Run(step, func(t *testing.T) {
			t.Parallel()
			limit := int64(1 << 20)
			if step == "kill" || step == "wait" || step == "kill-group" || step == "explain" {
				limit = 1024
			}
			h := newCaptureHarness(t, limit, 0)
			var hits atomic.Int32
			pause := func(s string) {
				if s == step {
					hits.Add(1)
					time.Sleep(outputPollInterval)
				}
			}
			h.b.effects.delay = pause
			h.b.effects.before = func(s string) error { pause(s); return nil }
			mode := "foreground"
			if step == "register-job" || step == "promotion-stat" {
				mode = "promoted"
			}
			if step == "exit-marker" || step == "job-launched" {
				mode = "background"
			}
			children := step == "tracker-register" || step == "tracker-remove" || step == "tracker-removed" || step == "stop-group" || step == "kill-group"
			command := ""
			if step == "kill-group" {
				command = `(while [ ! -f grow ]; do sleep 0.01; done; printf '%02048d' 0; while :; do sleep 0.01; done) & echo ready > ready; while [ ! -f exit ]; do sleep 0.005; done; exit 0`
			}
			var r *drivenCapture
			if command != "" {
				r = h.startCommand("A", mode, command, 0)
			} else {
				r = h.start("A", mode, children, 0, "", 0)
			}
			if mode == "promoted" {
				h.promote(r)
			}
			switch step {
			case "stop":
				r.cancel()
			case "kill", "wait", "explain":
				h.fill(r, 2048, true)
			default:
				gate(t, filepath.Join(r.dir, "exit"))
			}
			reply := h.returned(r)
			if reply.err != nil {
				t.Fatal(reply.err)
			}
			if mode != "foreground" {
				h.job(r)
			}
			if children {
				if step == "kill-group" {
					gate(t, filepath.Join(r.dir, "grow"))
				} else if step == "stop-group" {
					if err := h.b.Shutdown(context.Background()); err != nil {
						t.Fatal(err)
					}
				} else {
					gate(t, filepath.Join(r.dir, "child-exit"))
				}
			}
			select {
			case <-r.c.released:
			case <-time.After(4 * time.Second):
				t.Fatal("delay stranded capture")
			}
			phase := captureExited
			if children {
				phase = captureEmpty
			}
			if step == "kill" || step == "wait" || step == "explain" {
				phase = captureKilled
			}
			if step == "stop" {
				phase = captureStopped
			}
			h.released(r, phase, mode != "foreground")
			h.resources()
			// released closes before the tracker defer removes its entry; observe the
			// requested scheduling point rather than racing that final defer.
			await(t, func() bool { return hits.Load() > 0 })

		})
	}
}

func TestCaptureRejectedRegistration(t *testing.T) {
	for _, mode := range []string{"held-background", "promoted"} {
		for _, natural := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/natural=%v", mode, natural), func(t *testing.T) {
				t.Parallel()
				h := newCaptureHarness(t, 0, 0)
				manager := jobs.NewManager(jobs.Options{NewID: func() string { return "rejected" }})
				h.b.jobs = manager
				t.Cleanup(func() { _ = manager.Shutdown(context.Background()) })
				entered, resume := make(chan struct{}, 1), make(chan struct{})
				var once sync.Once
				defer once.Do(func() { close(resume) })
				h.b.effects.delay = func(step string) {
					if step == "job-launched" {
						entered <- struct{}{}
						<-resume
					}
				}
				r := h.start("A", mode, natural, 0, "", 0)
				if mode == "promoted" {
					r.expiry.expire()
				}
				<-entered
				if natural {
					gate(t, filepath.Join(r.dir, "exit"))
					code, err := r.c.job.Wait()
					if code != 0 || err != nil {
						t.Fatal(code, err)
					}
					if r.c.snapshotState().owner != ownerTracker {
						t.Fatal("no natural handover")
					}
				}
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				_ = manager.Shutdown(ctx)
				once.Do(func() { close(resume) })
				reply := h.returned(r)
				if !errors.Is(reply.err, jobs.ErrShutdown) {
					t.Fatal("registration not rejected", reply.err)
				}
				if _, ok := manager.Get("rejected"); ok {
					t.Fatal("rejected job registered")
				}
				phase := captureKilled
				if natural {
					phase = captureEmpty
				}
				h.released(r, phase, false)
				h.resources()
			})
		}
	}
}

func TestCaptureTrackerShutdown(t *testing.T) {
	t.Parallel()
	h := newCaptureHarness(t, 0, 0)
	r := h.start("A", "background", true, 0, "", 0)
	gate(t, filepath.Join(r.dir, "exit"))
	h.job(r)
	ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer cancel()
	if err := h.b.Shutdown(ctx); err != nil {
		t.Fatal("tracked shutdown did not stop group", err)
	}
	h.released(r, captureEmpty, true)
	h.resources()
}

func TestCaptureTrackedCap(t *testing.T) {
	t.Parallel()
	h := newCaptureHarness(t, 1024, 0)
	r := h.startCommand("A", "foreground", `(while [ ! -f grow ]; do sleep 0.01; done; printf '%02048d' 0; while [ ! -f child-exit ]; do sleep 0.01; done) & echo ready > ready; while [ ! -f exit ]; do sleep 0.005; done; exit 0`, 0)
	gate(t, filepath.Join(r.dir, "exit"))
	h.returned(r)
	file, err := os.Open(r.c.out.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	gate(t, filepath.Join(r.dir, "grow"))
	h.released(r, captureEmpty, false)
	tail := outputTail(t, file)
	if !strings.HasSuffix(tail, outputLimitMessage) || strings.Count(tail, outputLimitMessage) != 1 {
		t.Fatal("tracked cap explanation", tail)
	}
	h.resources()
}

func TestCaptureWaitOwnership(t *testing.T) {
	t.Parallel()
	h := newCaptureHarness(t, 1024, 0)
	entered, resume := make(chan struct{}, 1), make(chan struct{})
	var resumeOnce sync.Once
	defer resumeOnce.Do(func() { close(resume) })
	var once sync.Once
	h.b.effects.waitAlive = func(pgid int) bool { once.Do(func() { entered <- struct{}{} }); <-resume; return groupAlive(pgid) }
	r := h.start("A", "foreground", true, 0, "", 0)
	h.fill(r, 2048, true)
	select {
	case <-entered:
	case reply := <-r.done:
		r.reply = &reply
		t.Fatal("capture completed before group wait")
	case <-time.After(3 * time.Second):
		t.Fatal("no group wait")
	}
	if _, err := os.Stat(r.c.out.Name()); err != nil {
		t.Fatal("premature unlink", err)
	}
	if _, err := r.c.out.Stat(); err != nil {
		t.Fatal("premature close", err)
	}
	h.b.outputs.outputBudget.mu.Lock()
	record := h.b.outputs.outputBudget.files[r.c.out.Name()]
	running := record != nil && record.running
	h.b.outputs.outputBudget.mu.Unlock()
	if !running {
		t.Fatal("released budget before group wait")
	}
	resumeOnce.Do(func() { close(resume) })
	reply := h.returned(r)
	if reply.err != nil || reply.result.GetBash().ExitCode != 137 {
		t.Fatal(reply.err, reply.result)
	}
	h.released(r, captureKilled, false)
	h.resources()
}

func TestCaptureFinishedBudgetBeforeFormatting(t *testing.T) {
	t.Parallel()
	h := newCaptureHarness(t, 1<<20, 10000)
	entered, resume := make(chan struct{}, 1), make(chan struct{})
	defer close(resume)
	h.b.effects.before = func(step string) error {
		if step == "format-read" {
			entered <- struct{}{}
			<-resume
		}
		return nil
	}
	r := h.start("A", "foreground", false, 0, "", 0)
	h.fill(r, 8192, false)
	gate(t, filepath.Join(r.dir, "exit"))
	<-entered
	h.b.outputs.outputBudget.mu.Lock()
	record := h.b.outputs.outputBudget.files[r.c.out.Name()]
	finished := record != nil && !record.running
	h.b.outputs.outputBudget.mu.Unlock()
	if !finished {
		t.Fatal("ended shell still consumes running-output budget")
	}
	resume <- struct{}{}
	h.returned(r)
	h.released(r, captureExited, false)
	h.resources()
}

func TestCapturePerFileNaturalExit(t *testing.T) {
	for _, mode := range []string{"foreground", "background"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			h := newCaptureHarness(t, 1024, 0)
			exited := make(chan struct{})
			var once sync.Once
			h.b.effects.delay = func(step string) {
				if step == "shell-exited" {
					once.Do(func() { close(exited) })
				}
				if step == "output-check" {
					<-exited
				}
			}
			r := h.startCommand("A", mode, `echo ready > ready; while [ ! -f exit ]; do sleep 0.005; done; printf '%02048d' 0; exit 0`, 0)
			gate(t, filepath.Join(r.dir, "exit"))
			reply := h.returned(r)
			if reply.err != nil {
				t.Fatal(reply.err)
			}
			if mode == "foreground" {
				if reply.result.GetBash().ExitCode != 137 || !strings.Contains(reply.result.Output.Content[0].GetText(), strings.TrimSpace(outputLimitMessage)) {
					t.Fatal(reply.result)
				}
			} else {
				info := h.job(r)
				if info.State != circusv1.JobEvent_STATE_KILLED || info.ExitCode != 0 {
					t.Fatal(info)
				}
			}
			h.released(r, captureKilled, mode == "background")
			h.resources()
		})
	}
}

func TestCaptureTotalVictimExitedBeforeKill(t *testing.T) {
	t.Parallel()
	h := newCaptureHarness(t, 1<<20, 10000)
	entered, resume, exited := make(chan struct{}, 1), make(chan struct{}), make(chan struct{})
	defer close(resume)
	var once sync.Once
	h.b.effects.delay = func(step string) {
		if step == "kill" {
			entered <- struct{}{}
			<-resume
		}
		if step == "shell-exited" {
			once.Do(func() { close(exited) })
		}
	}
	small := h.start("small", "background", false, 0, "", 0)
	h.fill(small, 4096, false)
	victim := h.start("large", "foreground", false, 0, "", 0)
	h.fill(victim, 8192, true)
	<-entered
	gate(t, filepath.Join(victim.dir, "exit"))
	<-exited
	resume <- struct{}{}
	reply := h.returned(victim)
	if reply.err != nil || reply.result.GetBash().ExitCode != 0 || strings.Contains(reply.result.Output.Content[0].GetText(), "[circus:") {
		t.Fatal("late aggregate victim changed natural exit", reply.err, reply.result)
	}
	h.released(victim, captureExited, false)
	h.stop(small)
	h.job(small)
	h.released(small, captureKilled, true)
	h.resources()
}

func TestCaptureTrackerStopUsesTERM(t *testing.T) {
	t.Parallel()
	h := newCaptureHarness(t, 0, 0)
	r := h.startCommand("A", "background", `(trap 'echo term > term-observed; exit 0' TERM; echo child > child-ready; while :; do sleep 0.01; done) & echo ready > ready; while [ ! -f exit ]; do sleep 0.005; done; exit 0`, 0)
	await(t, func() bool { _, err := os.Stat(filepath.Join(r.dir, "child-ready")); return err == nil })
	gate(t, filepath.Join(r.dir, "exit"))
	h.job(r)
	if err := h.b.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(r.dir, "term-observed")); err != nil {
		t.Fatal("tracker stop skipped TERM", err)
	}
	h.released(r, captureEmpty, true)
	h.resources()
}

func TestStoreLockIndependence(t *testing.T) {
	t.Parallel()
	h := newCaptureHarness(t, 0, 0)
	h.b.snapshots.mu.Lock()
	done := make(chan error, 1)
	go func() {
		out, err := h.b.outputs.outputFile("review", "tasks", "independent-*")
		if err == nil {
			_ = out.Close()
			_ = os.Remove(out.Name())
			h.b.outputs.releaseOutput(out.Name())
		}
		done <- err
	}()
	select {
	case err := <-done:
		h.b.snapshots.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		h.b.snapshots.mu.Unlock()
		<-done
		t.Fatal("output store depends on snapshot mutex")
	}
	h.resources()
}

func TestCaptureStoppedGroupFallback(t *testing.T) {
	t.Parallel()
	h := newCaptureHarness(t, 0, 0)
	// stopProcess has a bounded reaping wait. Simulate its final kernel probe
	// still reporting a live group, without needing an unkillable privileged
	// child or spending five seconds. Run, Stop, Wait and jobs.Manager stay real.
	h.b.effects.alive = func(int) bool { return true }
	var handovers atomic.Int32
	h.b.effects.delay = func(step string) {
		if step == "tracker-register" {
			handovers.Add(1)
		}
	}
	r := h.start("A", "background", true, 0, "", 0)
	h.stop(r)
	info := h.job(r)
	if info.State != circusv1.JobEvent_STATE_KILLED || info.ExitCode != 143 {
		t.Fatal(info)
	}
	if handovers.Load() != 0 {
		t.Fatal("stopped group was handed back to unbounded tracker")
	}
	h.released(r, captureKilled, true)
	h.resources()
}

func TestCaptureCapDuringPromotion(t *testing.T) {
	t.Parallel()
	h := newCaptureHarness(t, 1024, 0)
	entered, resume := make(chan struct{}, 1), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(resume) })
	h.b.effects.delay = func(step string) {
		if step == "job-launched" {
			entered <- struct{}{}
			<-resume
		}
	}
	r := h.start("A", "promoted", true, 0, "", 0)
	r.expiry.expire()
	<-entered
	h.fill(r, 2048, true)
	code, err := r.c.job.Wait()
	if code != 137 || !errors.Is(err, jobs.ErrKilled) {
		t.Fatal("cap lost during launch", code, err)
	}
	once.Do(func() { close(resume) })
	info := h.job(r)
	if info.State != circusv1.JobEvent_STATE_KILLED || info.ExitCode != 137 {
		t.Fatal(info)
	}
	file, err := os.Open(r.c.out.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if tail := outputTail(t, file); strings.Count(tail, outputLimitMessage) != 1 {
		t.Fatal("cap diagnostic", tail)
	}
	h.released(r, captureKilled, true)
	h.resources()
}

func TestCaptureShutdownKeepsSnapshotThroughChildTrap(t *testing.T) {
	t.Parallel()
	h := newCaptureHarness(t, 0, 0)
	r := h.startCommand("A", "background", `(trap 'if [ -r "$(cat snapshot-path)" ]; then echo yes > lease-read; else echo no > lease-read; fi; exit 0' TERM; echo child > child-ready; while :; do sleep 0.01; done) & echo ready > ready; while [ ! -f exit ]; do sleep 0.005; done; exit 0`, 0)
	if err := os.WriteFile(filepath.Join(r.dir, "snapshot-path"), []byte(r.c.lease.path), 0600); err != nil {
		t.Fatal(err)
	}
	await(t, func() bool { _, err := os.Stat(filepath.Join(r.dir, "child-ready")); return err == nil })
	if err := h.b.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	gate(t, filepath.Join(r.dir, "exit"))
	info := h.job(r)
	if info.State != circusv1.JobEvent_STATE_EXITED || info.ExitCode != 0 {
		t.Fatal(info)
	}
	data, err := os.ReadFile(filepath.Join(r.dir, "lease-read"))
	if err != nil || string(data) != "yes\n" {
		t.Fatalf("snapshot lease ended before child TERM trap: %q %v", data, err)
	}
	h.released(r, captureEmpty, true)
	h.resources()
}

func TestCaptureForegroundHandoffReleasesSnapshotBeforeChildExit(t *testing.T) {
	t.Parallel()
	h := newCaptureHarness(t, 0, 0)
	a := h.start("A", "foreground", true, 0, "", 0)
	oldDir := filepath.Dir(a.c.lease.path)
	gate(t, filepath.Join(a.dir, "exit"))
	reply := h.returned(a)
	if reply.err != nil || reply.result.GetBash().ExitCode != 0 {
		t.Fatal(reply.err, reply.result)
	}
	if !groupAlive(a.c.pgid()) {
		t.Fatal("child exited before snapshot refresh")
	}
	if _, err := os.Stat(a.c.cwdPath); !os.IsNotExist(err) {
		t.Fatal("foreground handoff retained cwd scratch file", err)
	}
	// Creating .bashrc changes the startup stamp set even on a coarse filesystem
	// timestamp. The next command must collect a replacement snapshot.
	if err := os.WriteFile(filepath.Join(h.root, ".bashrc"), []byte("export SNAPSHOT_REFRESH=1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	b := h.start("B", "foreground", false, 0, "", 0)
	if filepath.Dir(b.c.lease.path) == oldDir {
		t.Fatal("startup change did not refresh the snapshot")
	}
	gate(t, filepath.Join(b.dir, "exit"))
	h.returned(b)
	if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
		t.Fatal("obsolete snapshot remains leased by a foreground child", err)
	}
	if !groupAlive(a.c.pgid()) {
		t.Fatal("child did not remain alive across snapshot refresh")
	}
	gate(t, filepath.Join(a.dir, "child-exit"))
	h.released(a, captureEmpty, false)
	h.released(b, captureExited, false)
	h.resources()
}

func TestCaptureTrackerReleaseErrorsIgnored(t *testing.T) {
	for _, state := range []string{"live", "closed"} {
		for _, step := range []string{"close", "remove"} {
			t.Run(state+"/"+step, func(t *testing.T) {
				t.Parallel()
				h := newCaptureHarness(t, 0, 0)
				boom := errors.New("tracker " + step + " failed")
				var hits atomic.Int32
				if step == "close" {
					h.b.effects.after = func(s string, err error) error {
						if s == step {
							hits.Add(1)
							return boom
						}
						return err
					}
				} else {
					h.b.effects.before = func(s string) error {
						if s == step {
							hits.Add(1)
							return boom
						}
						return nil
					}
				}
				r := h.start("A", "foreground", true, 0, "", 0)
				if state == "closed" {
					if err := h.b.Shutdown(context.Background()); err != nil {
						t.Fatal(err)
					}
				}
				gate(t, filepath.Join(r.dir, "exit"))
				reply := h.returned(r)
				if reply.err != nil || reply.result.GetBash().ExitCode != 0 {
					t.Fatal("tracker cleanup changed the foreground result", reply.err, reply.result)
				}
				if state == "live" {
					gate(t, filepath.Join(r.dir, "child-exit"))
				}
				if step == "close" {
					h.released(r, captureEmpty, false)
				} else {
					h.released(r, captureEmpty, true)
					if err := os.Remove(r.c.out.Name()); err != nil {
						t.Fatal(err)
					}
				}
				if err := h.b.Shutdown(context.Background()); err != nil {
					t.Fatal("tracker cleanup changed Shutdown", err)
				}
				h.resources()
				if hits.Load() != 1 {
					t.Fatal("tracker cleanup fault was not reached", hits.Load())
				}
			})
		}
	}
}

func TestCaptureClosedTrackerExplainsLateCap(t *testing.T) {
	t.Parallel()
	h := newCaptureHarness(t, 1024, 0)
	entered, resume := make(chan struct{}, 1), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(resume) })
	h.b.effects.delay = func(step string) {
		if step == "stop-group" {
			entered <- struct{}{}
			<-resume
		}
	}
	r := h.startCommand("A", "background", `(while [ ! -f grow ]; do sleep 0.01; done; printf '%02048d' 0; echo grown > grown; while [ ! -f child-exit ]; do sleep 0.01; done) & echo ready > ready; while [ ! -f exit ]; do sleep 0.005; done; exit 0`, 0)
	if err := h.b.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	gate(t, filepath.Join(r.dir, "exit"))
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("closed tracker did not begin child cleanup")
	}
	// Grow only after finishShell's cap check, while the closed tracker is
	// synchronously stopping the leftover group.
	gate(t, filepath.Join(r.dir, "grow"))
	await(t, func() bool { _, err := os.Stat(filepath.Join(r.dir, "grown")); return err == nil })
	once.Do(func() { close(resume) })
	if info := h.job(r); info.State != circusv1.JobEvent_STATE_EXITED || info.ExitCode != 0 {
		t.Fatal(info)
	}
	h.released(r, captureEmpty, true)
	file, err := os.Open(r.c.out.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if tail := outputTail(t, file); strings.Count(tail, outputLimitMessage) != 1 || !strings.HasSuffix(tail, outputLimitMessage) {
		t.Fatal("closed tracker lost late cap explanation", tail)
	}
	h.resources()
}

func TestCaptureBackgroundCapAndMarkerErrors(t *testing.T) {
	t.Parallel()
	h := newCaptureHarness(t, 1024, 0)
	markerBoom := errors.New("marker failed")
	explainBoom := errors.New("explanation failed")
	h.b.effects.before = func(step string) error {
		switch step {
		case "exit-marker":
			return markerBoom
		case "explain":
			return explainBoom
		default:
			return nil
		}
	}
	exited := make(chan struct{})
	var once sync.Once
	h.b.effects.delay = func(step string) {
		if step == "shell-exited" {
			once.Do(func() { close(exited) })
		}
		if step == "output-check" {
			<-exited
		}
	}
	r := h.start("A", "background", false, 0, "", 0)
	h.fill(r, 2048, false)
	gate(t, filepath.Join(r.dir, "exit"))
	if info := h.job(r); info.State != circusv1.JobEvent_STATE_KILLED || info.ExitCode != 0 {
		t.Fatal(info)
	}
	code, err := r.c.job.Wait()
	if code != 0 || !errors.Is(err, markerBoom) || !errors.Is(err, explainBoom) || !errors.Is(err, jobs.ErrKilled) {
		t.Fatal("cap and marker errors were not both retained", code, err)
	}
	h.released(r, captureKilled, true)
	h.resources()
}
