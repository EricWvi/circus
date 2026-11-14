package calls_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/auth"
	"github.com/EricWvi/circus/internal/clock"
	"github.com/EricWvi/circus/internal/controlplane/calls"
	"github.com/EricWvi/circus/internal/controlplane/gateway"
	"github.com/EricWvi/circus/internal/controlplane/timers"
	"github.com/EricWvi/circus/internal/credfile"
	"github.com/EricWvi/circus/internal/daemon"
	"github.com/EricWvi/circus/internal/daemon/jobs"
	daemonlink "github.com/EricWvi/circus/internal/daemon/link"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/store/sqlite"
	"github.com/EricWvi/circus/internal/store/storetest"
	"github.com/EricWvi/circus/internal/wire"
	"github.com/coder/websocket"
	"go.uber.org/goleak"
)

// TestRandomSequences drives the real router, gateway, SQLite store, daemon
// Dispatcher and daemon link client through seeded random operation
// sequences, including faults at message boundaries
// (sequence_faults_test.go), and checks every step against the reference
// model in sequence_model_test.go.
//
// Defaults: 25 seeds, 8 under -race, 2 with -short. CIRCUS_ROUTER_SEED=<n>
// replays one seed; CIRCUS_ROUTER_SEEDS=<n> and CIRCUS_ROUTER_FIRST_SEED=<n>
// run another range (a nightly sweep), CIRCUS_ROUTER_STEPS=<n> changes the
// length and CIRCUS_ROUTER_TRACE=1 logs every trace. A failure prints the
// seed and the operation trace. The default set must reach every path in
// seqRequiredPaths, so coverage cannot silently regress.
func TestRandomSequences(t *testing.T) {
	seeds, standard := seqSeeds(t)
	steps := seqSteps(t)
	total := make(map[string]int)
	ran := 0
	for _, seed := range seeds {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			r := runSequence(t, seed, steps)
			ran++
			for k, v := range r.cov {
				total[k] += v
			}
		})
	}
	t.Logf("paths over %d seeds: %s", ran, formatCounts(total))
	// Only the whole default set must reach every path: a subtest filter
	// (-run 'TestRandomSequences$/seed-14$') runs fewer seeds.
	if standard && ran == len(seeds) && !t.Failed() {
		for _, path := range seqRequiredPaths {
			if total[path] == 0 {
				t.Errorf("the default seeds never reached %q; adjust the operation weights", path)
			}
		}
	}
}

// seqRequiredPaths are the mid-flight paths the default seeds must reach.
var seqRequiredPaths = []string{"resend", "horizon-query", "stale-resend", "stale-known-status", "shutting-down-after-resend", "misdelivery-refused", "boot-mismatch-applied", "deferred-query"}

// seqSeeds returns the seeds to run and whether they are the default set.
func seqSeeds(t *testing.T) ([]uint64, bool) {
	if v := os.Getenv("CIRCUS_ROUTER_SEED"); v != "" {
		seed, err := strconv.ParseUint(v, 10, 64)
		must(t, err)
		return []uint64{seed}, false
	}
	n, standard := 25, true
	if raceEnabled {
		n = 8
	}
	if testing.Short() {
		n, standard = 2, false
	}
	if v := os.Getenv("CIRCUS_ROUTER_SEEDS"); v != "" {
		var err error
		n, err = strconv.Atoi(v)
		must(t, err)
		standard = false
	}
	first := uint64(1)
	if v := os.Getenv("CIRCUS_ROUTER_FIRST_SEED"); v != "" {
		var err error
		first, err = strconv.ParseUint(v, 10, 64)
		must(t, err)
		standard = false
	}
	seeds := make([]uint64, n)
	for i := range seeds {
		seeds[i] = first + uint64(i)
	}
	return seeds, standard
}

func seqSteps(t *testing.T) int {
	if v := os.Getenv("CIRCUS_ROUTER_STEPS"); v != "" {
		steps, err := strconv.Atoi(v)
		must(t, err)
		return steps
	}
	return 120
}

// TestRandomSequencesReplay runs seeds twice and compares their traces byte
// for byte: a seed must replay exactly, or a CI failure cannot be
// reproduced with CIRCUS_ROUTER_SEED.
//
// Tool goroutines sleep a random moment before they record their start, so
// the two runs see different start orders: a picker that depended on start
// order (blockedExecs unsorted) diverges here.
func TestRandomSequencesReplay(t *testing.T) {
	if raceEnabled {
		t.Skip("trace replay is independent of race instrumentation")
	}
	if os.Getenv("CIRCUS_ROUTER_SEED") != "" {
		t.Skip("replaying one seed")
	}
	seqShuffleStarts.Store(true)
	defer seqShuffleStarts.Store(false)
	steps := seqSteps(t)
	for _, seed := range []uint64{4, 7, 8} { // seeds where tools start together
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			first := runSequence(t, seed, steps)
			second := runSequence(t, seed, steps)
			if t.Failed() {
				return
			}
			for i := range max(len(first.trace), len(second.trace)) {
				a, b := "<end>", "<end>"
				if i < len(first.trace) {
					a = first.trace[i]
				}
				if i < len(second.trace) {
					b = second.trace[i]
				}
				if a != b {
					t.Fatalf("seed %d diverged at trace line %d:\n  first:  %s\n  second: %s", seed, i, a, b)
				}
			}
		})
	}
}

// seqShuffleStarts randomizes when tool goroutines record their start.
var seqShuffleStarts atomic.Bool

func formatCounts(m map[string]int) string {
	var parts []string
	for _, k := range sortedKeys(m) {
		parts = append(parts, fmt.Sprintf("%s=%d", k, m[k]))
	}
	return strings.Join(parts, " ")
}

// seqTimeoutScale stretches the harness's real-time waits: 3 under -race,
// times CIRCUS_ROUTER_TIMEOUT_SCALE (default 1) for slow CI runners.
var seqTimeoutScale = func() float64 {
	scale := 1.0
	if raceEnabled {
		scale = 3
	}
	if v := os.Getenv("CIRCUS_ROUTER_TIMEOUT_SCALE"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			scale *= f
		}
	}
	return scale
}()

func (w *seqWorld) timeout(d time.Duration) time.Duration {
	return time.Duration(float64(d) * seqTimeoutScale)
}

// seqJitter fixes the daemon's reconnect jitter, so its retry delays are a
// known set the device clock can recognize (see seqDevClock).
const seqJitter = 0.37

var seqRetryDelays = func() map[time.Duration]bool {
	out := make(map[time.Duration]bool)
	for base := 500 * time.Millisecond; ; base = min(30*time.Second, base*2) {
		out[time.Duration(float64(base)*(0.5+0.5*seqJitter))] = true
		if base == 30*time.Second {
			return out
		}
	}
}()

var errSeqDown = errors.New("sequence: device link is down")

// seqDevClock is a daemon's clock: the shared fake clock, except that the
// link client's reconnect backoff timers (recognized by their duration) are
// held here, so the sequence can reconnect a device without moving time.
// They still fire when shared time passes them (fire after every advance).
type seqDevClock struct {
	shared  *clock.Fake
	mu      sync.Mutex
	retries []seqRetry
}
type seqRetry struct {
	at time.Time
	ch chan time.Time
}

func (c *seqDevClock) Now() time.Time { return c.shared.Now() }
func (c *seqDevClock) After(d time.Duration) <-chan time.Time {
	if !seqRetryDelays[d] {
		return c.shared.After(d)
	}
	ch := make(chan time.Time, 1)
	c.mu.Lock()
	c.retries = append(c.retries, seqRetry{at: c.shared.Now().Add(d), ch: ch})
	c.mu.Unlock()
	return ch
}

// pending reports whether a backoff timer is waiting.
func (c *seqDevClock) pending() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.retries) > 0
}

// fire delivers due backoff timers, or all of them when force is set.
func (c *seqDevClock) fire(force bool) int {
	now := c.shared.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	kept, fired := c.retries[:0], 0
	for _, r := range c.retries {
		if force || !r.at.After(now) {
			r.ch <- now
			fired++
		} else {
			kept = append(kept, r)
		}
	}
	clear(c.retries[len(kept):])
	c.retries = kept
	return fired
}

// seqHooks counts hook activity, so settle can tell when the router is idle,
// and remembers which generations finished OnConnected.
// It also records hook errors for the model and can hold a device's next
// sync before it starts (its route is then registered but not ready).
type seqHooks struct {
	gateway.Hooks
	w             *seqWorld
	active, total atomic.Int64
	mu            sync.Mutex
	synced        map[string]bool
	gates         map[string]*seqGate // device -> gate of its next sync
	errors        []string            // hook and timer errors no fault explains
}

// note records an error that no injected fault, crash or closing link
// explains; checkStep fails on it.
func (h *seqHooks) note(where string, err error) error {
	if err != nil && !h.w.errorExplained(err) {
		h.mu.Lock()
		h.errors = append(h.errors, where+": "+err.Error())
		h.mu.Unlock()
	}
	return err
}

func (h *seqHooks) takeErrors() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := h.errors
	h.errors = nil
	return out
}

// seqGate holds one OnConnected before it starts. entered is closed when
// the sync waits at it, released by the operation.
type seqGate struct {
	h                 *seqHooks
	device            string
	entered, released chan struct{}
	once              sync.Once
}

// holdSync makes the device's next OnConnected wait until the gate is
// released.
func (h *seqHooks) holdSync(device string) *seqGate {
	g := &seqGate{h: h, device: device, entered: make(chan struct{}), released: make(chan struct{})}
	h.mu.Lock()
	h.gates[device] = g
	h.mu.Unlock()
	return g
}

// held reports whether a sync waits at this gate.
func (g *seqGate) held() bool {
	select {
	case <-g.entered:
		return true
	default:
		return false
	}
}

// release lets the held sync run; an unused gate is withdrawn, so it
// cannot hold a later, unrelated reconnect.
func (g *seqGate) release() {
	g.once.Do(func() {
		g.h.mu.Lock()
		if g.h.gates[g.device] == g {
			delete(g.h.gates, g.device)
		}
		g.h.mu.Unlock()
		close(g.released)
	})
}

func (h *seqHooks) enter() func() {
	h.total.Add(1)
	h.active.Add(1)
	return func() { h.total.Add(1); h.active.Add(-1) }
}
func (h *seqHooks) isSynced(connection string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.synced[connection]
}
func (h *seqHooks) OnConnected(ctx context.Context, c gateway.Connection) error {
	h.mu.Lock()
	gate := h.gates[c.Target.DeviceID]
	delete(h.gates, c.Target.DeviceID)
	h.mu.Unlock()
	if gate != nil {
		close(gate.entered)
		select {
		case <-gate.released:
		case <-ctx.Done():
		}
	}
	defer h.enter()()
	err := h.note("OnConnected", h.Hooks.OnConnected(ctx, c))
	h.mu.Lock()
	h.synced[c.Target.ConnectionID] = true
	h.mu.Unlock()
	return err
}
func (h *seqHooks) OnDisconnected(ctx context.Context, c gateway.Connection) error {
	defer h.enter()()
	return h.note("OnDisconnected", h.Hooks.OnDisconnected(ctx, c))
}
func (h *seqHooks) OnCallAck(ctx context.Context, c gateway.Connection, v *circusv1.CallAck) error {
	defer h.enter()()
	return h.note("OnCallAck", h.Hooks.OnCallAck(ctx, c, v))
}
func (h *seqHooks) OnDeviceResult(ctx context.Context, c gateway.Connection, v *circusv1.DeviceResult) error {
	defer h.enter()()
	return h.note("OnDeviceResult", h.Hooks.OnDeviceResult(ctx, c, v))
}
func (h *seqHooks) OnCallStatus(ctx context.Context, c gateway.Connection, v *circusv1.CallStatus) error {
	defer h.enter()()
	return h.note("OnCallStatus", h.Hooks.OnCallStatus(ctx, c, v))
}
func (h *seqHooks) OnJobEvent(ctx context.Context, c gateway.Connection, v *circusv1.JobEvent) error {
	defer h.enter()()
	return h.note("OnJobEvent", h.Hooks.OnJobEvent(ctx, c, v))
}
func (h *seqHooks) OnJobSnapshot(ctx context.Context, c gateway.Connection, v *circusv1.JobSnapshot) error {
	defer h.enter()()
	return h.note("OnJobSnapshot", h.Hooks.OnJobSnapshot(ctx, c, v))
}

type seqAuth struct{ devices []*seqDevice }

func (a seqAuth) Authenticate(_ context.Context, r *http.Request) (auth.Principal, error) {
	for _, d := range a.devices {
		if r.Header.Get("Authorization") == "Bearer "+d.token {
			return auth.Principal{Kind: store.KindDevice, UserID: "u", ID: d.id, Name: d.name, CredentialID: "cred-" + d.id}, nil
		}
	}
	return auth.Principal{}, auth.ErrUnauthenticated
}
func (seqAuth) Valid(context.Context, string) (bool, error) { return true, nil }

// seqDevice is one enrolled device. up is the dial gate: a down device's
// daemon cannot connect. A ghost device never runs a daemon.
type seqDevice struct {
	w               *seqWorld
	index           int
	id, name, token string
	ghost           bool
	up              atomic.Bool
	daemon          *seqDaemon
	// Model: when the device was last seen without a ready route (zero while
	// ready) and how often it went down.
	downSince time.Time
	downEpoch int
	everReady bool
	boots     int // daemons started, for boot labels
}

// seqDaemon is one daemon process (one boot) of a device.
type seqDaemon struct {
	dev      *seqDevice
	boot     string
	label    string // device#n, stable across runs (boot IDs are random)
	client   *daemonlink.Client
	clock    *seqDevClock
	cancel   context.CancelFunc
	done     chan error
	stopped  bool         // the process ended: its boot is dead
	graceful bool         // it ended through graceful shutdown
	expected atomic.Int64 // dial attempts the sequence caused
	dials    atomic.Int64 // dial attempts finished
	mu       sync.Mutex
	conn     *seqConn
	// Frame bookkeeping and the armed fault, guarded by fmu (the link's
	// reader and writer goroutines update them).
	fmu           sync.Mutex
	fault         *seqFault
	dispatches    map[string]int       // DispatchCall frames of this boot received, dropped ones included
	dropped       map[string]seqDrop   // DispatchCalls that never reached the daemon
	acked         map[string]bool      // calls the daemon acked as running
	corrupted     map[string]bool      // calls whose DispatchCall target was rewritten
	recorded      map[string]bool      // calls the daemon answered with a status (it holds a record)
	staled        map[string]bool      // calls whose resend the harness made older than MaxDispatchAge
	firstDispatch map[string]time.Time // when the first DispatchCall of a call arrived
	lastDispatch  []byte               // the last DispatchCall frame, for misdelivery
}

func (d *seqDaemon) current() *seqConn {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.conn
}

type seqTool struct{ d *seqDaemon }

func (seqTool) Name() string { return "bash" }

// Run records the execution and blocks until the sequence releases it or the
// daemon cancels it (cancel, deadline or shutdown).
func (t seqTool) Run(ctx context.Context, call *circusv1.DeviceCall) (*circusv1.DeviceResult, error) {
	m := t.d.dev.w.m
	if seqShuffleStarts.Load() {
		// Vary the start order of tools started together (replay test).
		time.Sleep(time.Duration(rand.IntN(3000)) * time.Microsecond)
	}
	e := m.startExec(call.GetCallId(), t.d.boot, call.GetBash().GetCommand(), ctx)
	outcome := "ok"
	select {
	case <-e.release:
	case <-ctx.Done():
		outcome = "interrupted"
		if m.isStubborn(e) {
			<-e.release // ignores cancellation until the sequence releases it
		}
	}
	out := outcome + ":" + t.d.label + ":" + e.command
	m.finishExec(e, out)
	return &circusv1.DeviceResult{Output: &circusv1.ToolOutput{Content: []*circusv1.ContentBlock{{Block: &circusv1.ContentBlock_Text{Text: out}}}}}, nil
}

func (d *seqDaemon) dial(ctx context.Context, _ string, opts *websocket.DialOptions) (wire.Conn, *http.Response, error) {
	defer d.dials.Add(1)
	if !d.dev.up.Load() {
		return nil, nil, errSeqDown
	}
	conn, response, err := wire.DialWebSocket(ctx, d.dev.w.wsURL.Load().(string), opts)
	if err != nil {
		return nil, response, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.dev.up.Load() || d.stopped {
		_ = conn.Close()
		return nil, nil, errSeqDown
	}
	d.conn = newSeqConn(d, conn)
	return d.conn, response, nil
}

// cut aborts the daemon's current socket, as a network failure would.
func (d *seqDaemon) cut() {
	d.mu.Lock()
	conn := d.conn
	d.conn = nil
	d.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

// kick fires the daemon's backoff timers (force) or only due ones.
func (d *seqDaemon) kick(force bool) {
	if d.stopped {
		return
	}
	d.expected.Add(int64(d.clock.fire(force)))
}

type seqWorld struct {
	t           *testing.T
	seed        uint64
	rng         *rand.Rand
	ctx         context.Context
	clock       *clock.Fake
	dbPath      string
	db          store.Store
	fault       *storetest.Faulty
	registry    gateway.Registry
	router      calls.Router
	hooks       *seqHooks
	gateway     gateway.Handler
	server      *httptest.Server
	wsURL       atomic.Value
	runner      timers.Runner
	stopTimers  context.CancelFunc
	timerDone   chan error
	incarnation int
	devices     []*seqDevice
	m           *seqModel
	step        int
	trace       []string
	failed      bool
	summary     string
	quiet       *slog.Logger
	// faultTarget is the write point an armed one-shot failure hits;
	// faultAll is set while every write fails; crashArmed while a crash
	// point is pending.
	faultTarget int
	faultAll    bool
	crashArmed  bool
	// inEffect names the faults injected or still pending during the
	// current step (guarded by covMu); errorExplained accepts only errors
	// those faults can cause.
	inEffect map[string]bool
	store    *seqStore       // the router's store: Faulty plus a dispatch-commit failure
	evidence *evidenceSender // the router's sender: commit-before-send check, busy injection
	// cov counts the mid-flight paths reached (sequence_faults_test.go).
	covMu        sync.Mutex
	cov          map[string]int
	violations   []string
	shutdownSeen int
	pumps        sync.WaitGroup // seqConn pump goroutines
}

// seqResult is what one seed produced.
type seqResult struct {
	trace []string
	cov   map[string]int
}

func runSequence(t *testing.T, seed uint64, steps int) *seqResult {
	baseline := goleak.IgnoreCurrent()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()
	w := &seqWorld{t: t, seed: seed, rng: rand.New(rand.NewPCG(seed, 0x5eed)), ctx: ctx, clock: clock.New(), dbPath: filepath.Join(t.TempDir(), "db"), quiet: slog.New(slog.NewTextHandler(io.Discard, nil)), cov: make(map[string]int)}
	w.m = newSeqModel(w)
	previous := slog.Default()
	slog.SetDefault(w.quiet)
	defer slog.SetDefault(previous)
	db, err := sqlite.Open(ctx, w.dbPath, sqlite.Options{})
	must(t, err)
	must(t, db.CreateUser(ctx, store.User{ID: "u", Name: "user", CreatedAt: 1}))
	for i, name := range []string{"desk", "laptop", "ghost"} {
		d := &seqDevice{w: w, index: i, id: fmt.Sprintf("d%d", i), name: name, token: "token-" + name, ghost: name == "ghost"}
		must(t, db.CreateIdentity(ctx, store.Identity{Kind: store.KindDevice, ID: d.id, UserID: "u", Name: name, MachineID: "m" + d.id, CreatedAt: 1}))
		w.devices = append(w.devices, d)
	}
	must(t, db.Close())
	w.openControlPlane()
	for _, d := range w.devices {
		if !d.ghost {
			d.up.Store(true)
			w.startDaemon(d)
		}
	}
	w.settle("start")
	for w.step = 1; w.step <= steps && !w.failed; w.step++ {
		w.beginStep()
		if w.fault.Crashed() {
			w.record("control plane crashed at its armed write point; restart")
			w.restartControlPlane()
			w.settle("restart")
			if w.failed {
				break
			}
		}
		w.randomOp()
		if !w.failed {
			w.settle(w.trace[len(w.trace)-1])
		}
		if !w.failed {
			w.m.checkStep()
		}
	}
	if !w.failed {
		w.quiesce()
	}
	w.teardown()
	w.pumps.Wait()
	if !w.failed {
		goleak.VerifyNone(t, baseline)
	}
	if !w.failed {
		t.Logf("seed %d: %s; paths: %s", seed, w.summary, formatCounts(w.cov))
	}
	if os.Getenv("CIRCUS_ROUTER_TRACE") != "" {
		t.Logf("seed %d trace:\n  %s", seed, strings.Join(w.trace, "\n  "))
	}
	return &seqResult{trace: w.trace, cov: w.cov}
}

func (w *seqWorld) record(format string, args ...any) {
	line := fmt.Sprintf("%3d t+%-9s %s", w.step, w.clock.Now().Sub(clock.New().Now()).Round(time.Millisecond), fmt.Sprintf(format, args...))
	w.trace = append(w.trace, line)
}

// fail reports a violation with the seed and trace and stops the sequence.
func (w *seqWorld) fail(format string, args ...any) {
	w.t.Helper()
	if w.failed {
		w.t.Errorf("also: "+format, args...)
		return
	}
	w.failed = true
	w.t.Errorf("seed %d step %d: %s\nreplay: CIRCUS_ROUTER_SEED=%d go test -run 'TestRandomSequences' ./internal/controlplane/calls/\ntrace:\n  %s", w.seed, w.step, fmt.Sprintf(format, args...), w.seed, strings.Join(w.trace, "\n  "))
}

// openControlPlane builds a fresh control-plane incarnation on the SQLite
// file: store, registry, router, gateway, HTTP server and timer runner.
func (w *seqWorld) openControlPlane() {
	t := w.t
	w.incarnation++
	db, err := sqlite.Open(w.ctx, w.dbPath, sqlite.Options{})
	must(t, err)
	w.db = db
	w.fault = storetest.NewFaulty(db)
	w.store = &seqStore{Store: w.fault}
	w.faultTarget, w.faultAll, w.crashArmed = 0, false, false
	w.registry, err = gateway.NewRegistry(16)
	must(t, err)
	// The router's sender checks commit-before-send on every DispatchCall.
	w.evidence = &evidenceSender{Sender: senderTo(&w.gateway), db: db, report: w.violation, refused: w.busyRefused}
	w.router, err = calls.New(calls.Config{Store: w.store, Registry: w.registry, Sender: w.evidence, Clock: w.clock, MaxMessageBytes: 256 << 10, Pending: calls.PendingBudget{MaxCalls: 4}})
	must(t, err)
	w.hooks = &seqHooks{Hooks: w.router, w: w, synced: make(map[string]bool), gates: make(map[string]*seqGate)}
	w.beginStep()
	w.gateway, err = gateway.New(gateway.Config{InstanceID: "instance", Clock: w.clock, Auth: seqAuth{w.devices}, Registry: w.registry, Hooks: w.hooks, MaxConnections: 16, LinkOptions: wire.Options{MaxMessageBytes: 256 << 10, MaxFragmentBytes: 16 << 10, DisableHeartbeat: true}})
	must(t, err)
	w.server = httptest.NewServer(w.gateway)
	w.wsURL.Store("ws" + strings.TrimPrefix(w.server.URL, "http") + "/v1/link/daemon")
	w.runner, err = timers.New(timers.Config{Store: seqClaims{Faulty: w.fault}, Clock: w.clock, InstanceID: "instance", Lease: time.Minute, PollInterval: time.Second, BatchSize: 64, Handler: func(ctx context.Context, tm store.Timer) error {
		return w.hooks.note("timer "+tm.Kind, w.router.HandleTimer(ctx, tm))
	}})
	must(t, err)
	var ctx context.Context
	ctx, w.stopTimers = context.WithCancel(w.ctx)
	w.timerDone = make(chan error, 1)
	go func() { w.timerDone <- w.runner.Run(ctx) }()
	eventually(t, func() bool { return w.runner.Stats().Running })
}

func (w *seqWorld) haltTimers() {
	if w.stopTimers != nil {
		w.stopTimers()
		<-w.timerDone
		w.stopTimers = nil
	}
}

// closeControlPlane tears the incarnation down. For a crash the store has
// already crashed, so nothing more persists; sockets are cut client-side
// first, as a dead process's would be.
func (w *seqWorld) closeControlPlane(crash bool) {
	w.m.closing = true
	defer func() { w.m.closing = false }()
	w.haltTimers()
	if crash {
		for _, d := range w.devices {
			if d.daemon != nil {
				d.daemon.cut()
			}
		}
	}
	ctx, cancel := context.WithTimeout(w.ctx, 10*time.Second)
	defer cancel()
	if err := w.gateway.Shutdown(ctx); err != nil {
		w.fail("gateway shutdown: %v", err)
	}
	w.server.Close()
	if err := w.router.Close(ctx); err != nil {
		w.fail("router close: %v", err)
	}
	w.m.drainSubs() // the closed router ends every subscription
	must(w.t, w.db.Close())
}

// restartControlPlane drops every in-memory object of a crashed control
// plane, reopens the same SQLite file and rebuilds the incarnation.
func (w *seqWorld) restartControlPlane() {
	w.closeControlPlane(true)
	w.m.controlPlaneCrashed()
	w.openControlPlane()
	for _, d := range w.devices {
		if d.daemon != nil && !d.daemon.stopped {
			// Kick only once the daemon noticed the cut and holds its
			// backoff timer; an earlier kick would find no timer.
			dm := d.daemon
			w.wait("backoff of "+dm.label, func() bool { s := dm.client.Stats(); return !s.Connected && s.RetryDelay > 0 && dm.clock.pending() })
			dm.kick(true)
		}
	}
}

func (w *seqWorld) startDaemon(dev *seqDevice) {
	dev.boots++
	d := &seqDaemon{dev: dev, label: fmt.Sprintf("%s#%d", dev.name, dev.boots), clock: &seqDevClock{shared: w.clock}, done: make(chan error, 1), dispatches: make(map[string]int), dropped: make(map[string]seqDrop), acked: make(map[string]bool), corrupted: make(map[string]bool), recorded: make(map[string]bool), staled: make(map[string]bool), firstDispatch: make(map[string]time.Time)}
	var err error
	d.client, err = daemonlink.New(daemonlink.Options{
		Credentials:     credfile.File{ControlPlaneURL: "http://127.0.0.1", Role: credfile.RoleDaemon, PrincipalID: dev.id, Token: dev.token},
		Clock:           d.clock,
		Logger:          w.quiet,
		Tools:           func(*jobs.Manager) []daemon.Tool { return []daemon.Tool{seqTool{d}} },
		Wire:            wire.Options{MaxMessageBytes: 256 << 10, MaxFragmentBytes: 16 << 10, DisableHeartbeat: true},
		Dial:            d.dial,
		MachineIDSource: func(context.Context) (string, error) { return "m" + dev.id, nil },
		Jitter:          func() float64 { return seqJitter },
	})
	must(w.t, err)
	d.boot = d.client.BootID()
	w.m.boots[d.boot] = d
	dev.daemon = d
	ctx, cancel := context.WithCancel(w.ctx)
	d.cancel = cancel
	d.expected.Add(1) // Run dials at once
	go func() { d.done <- d.client.Run(ctx) }()
}

// stopDaemon ends a daemon process. A crash cuts the socket first, so
// nothing it does afterwards reaches the control plane; a graceful stop
// lets the client flush its final results.
func (w *seqWorld) stopDaemon(d *seqDaemon, graceful bool) {
	if !graceful {
		d.cut()
	}
	d.mu.Lock()
	d.stopped = true
	d.mu.Unlock()
	d.graceful = graceful
	d.cancel()
	select {
	case <-d.done:
	case <-time.After(w.timeout(20 * time.Second)):
		w.fail("daemon %s did not stop", d.label)
	}
}

// settle waits until the system is idle: no hook or router operation in
// flight, no queued or unreceived wire message, every finished tool's result
// handed to the link, every sync finished, every due timer handled and every
// caused dial attempted, stable over consecutive polls.
func (w *seqWorld) settle(what string) {
	deadline := time.Now().Add(w.timeout(30 * time.Second))
	var last seqActivity
	stable := 0
	reason := ""
	for {
		w.m.drainSubs()
		reason = w.unsettled()
		act := w.activity()
		if reason == "" && act == last {
			stable++
		} else {
			stable = 0
		}
		last = act
		if stable >= 3 {
			break
		}
		if time.Now().After(deadline) {
			w.fail("system did not settle after %q: %s", what, reason)
			return
		}
		time.Sleep(time.Millisecond) // polling for a condition, not ordering
	}
	w.m.observe()
}

type seqActivity struct {
	hooks, dials, results, connections, frames int64
	execs, finished                            int
}

func (w *seqWorld) activity() seqActivity {
	var a seqActivity
	a.hooks = w.hooks.total.Load()
	for _, d := range w.devices {
		if d.daemon == nil {
			continue
		}
		for _, dm := range w.m.daemonsOf(d) {
			a.dials += dm.dials.Load()
			s := dm.client.Stats()
			a.results += int64(s.Results)
			a.connections += int64(s.Connections)
			a.frames += int64(s.Wire.FramesRead + s.Wire.FramesWritten)
		}
	}
	gs := w.gateway.Stats()
	a.frames += int64(gs.Wire.FramesRead + gs.Wire.FramesWritten)
	a.execs, a.finished = w.m.execCounts()
	return a
}

func (w *seqWorld) unsettled() string {
	if n := w.hooks.active.Load(); n > 0 {
		return fmt.Sprintf("%d hooks running", n)
	}
	rs := w.router.Stats()
	if rs.Operations > 0 || rs.SyncRequests > 0 {
		return fmt.Sprintf("router busy %+v", rs)
	}
	gs := w.gateway.Stats()
	if gs.Handshakes > 0 || gs.Wire.QueuedMessages > 0 || gs.Wire.ReceivedMessages > 0 || gs.Wire.ReassemblyMessages > 0 || gs.Wire.ActiveMessages > 0 {
		return fmt.Sprintf("gateway busy %+v", gs)
	}
	if reason := w.m.toolsUnsettled(); reason != "" {
		return reason
	}
	for _, d := range w.devices {
		if d.ghost {
			continue
		}
		dm := d.daemon
		if !dm.stopped {
			if dm.dials.Load() < dm.expected.Load() {
				return fmt.Sprintf("%s: dial pending", d.name)
			}
			s := dm.client.Stats()
			if s.Connected && (s.Wire.QueuedMessages > 0 || s.Wire.ReceivedMessages > 0 || s.Wire.ReassemblyMessages > 0 || s.Wire.ActiveMessages > 0) {
				return fmt.Sprintf("%s: daemon wire busy", d.name)
			}
			if !s.Connected && s.RetryDelay == 0 {
				return fmt.Sprintf("%s: daemon between connections", d.name)
			}
			if int(s.Results) < w.m.finishedOn(dm.boot) {
				return fmt.Sprintf("%s: results not yet handed to the link", d.name)
			}
			if id := w.m.unstarted(dm); id != "" {
				return fmt.Sprintf("%s: %s acked but its tool has not started", d.name, id)
			}
		}
		route, ok, _ := w.registry.Lookup(w.ctx, d.id)
		if ok && !route.Ready && !w.hooks.isSynced(route.Target.ConnectionID) {
			return fmt.Sprintf("%s: sync pending", d.name)
		}
	}
	if w.fault.Crashed() {
		return ""
	}
	if w.runner.Stats().Active > 0 {
		return "timer handler running"
	}
	if w.faultAll || w.faultTarget > w.fault.Writes() {
		return "" // failing writes leave timers claimed until their lease ends
	}
	now := w.clock.Now().UnixMilli()
	timersDue, err := w.db.ListTimers(w.ctx, store.TimerFilter{Limit: store.MaxPageSize})
	if err != nil {
		return "timer read: " + err.Error()
	}
	for _, tm := range timersDue {
		if tm.DueAt <= now && (tm.Token == "" || tm.ClaimUntil <= now) && (tm.Kind == timers.CallWatchdog || tm.Kind == timers.DeviceGrace) {
			w.runner.Wake() // claims do not count as write points (seqClaims)
			return fmt.Sprintf("timer %s/%s due", tm.Kind, tm.SubjectID)
		}
	}
	return ""
}

// advance moves shared time, fires due daemon backoff timers and wakes the
// timer runner.
// The control plane's timers run first; daemons whose backoff expired
// reconnect only after that, so the order of a grace expiry and a reconnect
// at the same instant does not depend on goroutine scheduling.
func (w *seqWorld) advance(d time.Duration) {
	w.m.beforeAdvance()
	w.clock.Advance(d)
	w.runner.Wake()
	w.m.afterAdvance()
	w.settle(fmt.Sprintf("advance %s", d))
	w.detectHalfOpen()
	for _, dev := range w.devices {
		if dev.daemon != nil {
			dev.daemon.kick(false)
		}
	}
}

func (w *seqWorld) pick(n int) int { return w.rng.IntN(n) }

// wait polls a condition inside an operation that cannot settle yet.
func (w *seqWorld) wait(what string, ok func() bool) {
	deadline := time.Now().Add(w.timeout(10 * time.Second))
	for !ok() {
		if time.Now().After(deadline) {
			// A broken invariant seen meanwhile is the likelier cause.
			for _, msg := range w.takeViolations() {
				w.fail("%s", msg)
			}
			w.fail("timed out waiting for %s", what)
			return
		}
		time.Sleep(time.Millisecond) // polling for a condition, not ordering
	}
}

type seqOp struct {
	name   string
	weight int
	run    func(*seqWorld) bool
}

var seqOps = []seqOp{
	{"submit", 18, (*seqWorld).opSubmit},
	{"duplicate", 6, (*seqWorld).opDuplicate},
	{"mismatch", 3, (*seqWorld).opMismatch},
	{"cancel", 5, (*seqWorld).opCancel},
	{"complete", 16, (*seqWorld).opComplete},
	{"disconnect", 4, (*seqWorld).opDisconnect},
	{"reconnect", 6, (*seqWorld).opReconnect},
	{"daemon-crash", 2, func(w *seqWorld) bool { return w.opDaemonRestart(false) }},
	{"daemon-restart", 2, func(w *seqWorld) bool { return w.opDaemonRestart(true) }},
	{"cp-crash", 2, (*seqWorld).opControlPlaneCrash},
	{"advance", 9, (*seqWorld).opAdvance},
	{"store-fault", 3, (*seqWorld).opStoreFault},
	{"stall", 2, (*seqWorld).opStall},
	{"query", 5, (*seqWorld).opQuery},
	{"fault-dispatch", 6, (*seqWorld).opFaultDispatch},
	{"fault-result", 4, (*seqWorld).opFaultResult},
	{"half-open", 2, (*seqWorld).opHalfOpen},
	{"misdeliver", 3, (*seqWorld).opMisdeliver},
	{"deferred-query", 3, (*seqWorld).opDeferredQuery},
	{"resend-during-shutdown", 3, (*seqWorld).opResendDuringShutdown},
	{"fail-sync-dispatch", 3, (*seqWorld).opFailSyncDispatch},
	{"deferred-query-boot-change", 2, (*seqWorld).opDeferredQueryBootChange},
	{"busy-link", 3, (*seqWorld).opBusyLink},
	{"corrupt-target", 2, (*seqWorld).opCorruptTarget},
}

func (w *seqWorld) randomOp() {
	total := 0
	for _, op := range seqOps {
		total += op.weight
	}
	for {
		n := w.pick(total)
		for _, op := range seqOps {
			if n -= op.weight; n < 0 {
				if op.run(w) {
					return
				}
				break
			}
		}
	}
}

func (w *seqWorld) liveDevice() *seqDevice {
	var live []*seqDevice
	for _, d := range w.devices {
		if !d.ghost {
			live = append(live, d)
		}
	}
	return live[w.pick(len(live))]
}

func seqRequest(id, command string) *circusv1.DeviceCall {
	return &circusv1.DeviceCall{CallId: id, SessionId: "session", Tool: &circusv1.DeviceCall_Bash{Bash: &circusv1.BashRequest{Command: command, Cwd: "/tmp"}}}
}

func (w *seqWorld) opSubmit() bool {
	dev := w.liveDevice()
	if w.pick(10) == 0 {
		dev = w.devices[2]
	}
	id := fmt.Sprintf("c%03d", w.step)
	c := w.m.newCall(id, dev)
	command := fmt.Sprintf("cmd-%d", w.step)
	w.record("submit %s to %s (%s)", id, dev.name, command)
	w.m.submit(c, dev, command, "new")
	return true
}

func (w *seqWorld) opDuplicate() bool {
	c := w.m.pickCall(func(c *seqCall) bool { return c.content != "" })
	if c == nil || c.dev.halfOpen() {
		return false // a duplicate of a completed call queries the daemon
	}
	w.record("duplicate %s to %s", c.id, c.dev.name)
	w.m.submit(c, c.dev, c.content, "duplicate")
	return true
}

func (w *seqWorld) opMismatch() bool {
	c := w.m.pickCall(func(c *seqCall) bool { return c.content != "" && w.m.row(c.id) != nil })
	if c == nil {
		return false
	}
	if w.pick(2) == 0 {
		other := w.devices[(c.dev.index+1)%len(w.devices)]
		w.record("mismatching submit %s to other device %s", c.id, other.name)
		w.m.submit(c, other, c.content, "mismatch")
	} else {
		w.record("mismatching submit %s with other content", c.id)
		w.m.submit(c, c.dev, c.content+"-other", "mismatch")
	}
	return true
}

func (w *seqWorld) opCancel() bool {
	c := w.m.pickActive()
	if c == nil {
		return false
	}
	w.record("cancel %s", c.id)
	w.m.cancel(c)
	return true
}

func (w *seqWorld) opComplete() bool {
	e := w.m.pickBlocked()
	if e == nil {
		return false
	}
	w.record("tool completes %s on %s", e.callID, w.m.label(e.boot))
	w.m.release(e)
	return true
}

func (w *seqWorld) opDisconnect() bool {
	dev := w.liveDevice()
	if !dev.up.Load() {
		return false
	}
	w.record("disconnect %s", dev.name)
	dev.up.Store(false)
	dev.daemon.cut()
	w.m.deviceDown(dev)
	return true
}

func (w *seqWorld) opReconnect() bool {
	dev := w.liveDevice()
	if dev.up.Load() && w.pick(3) > 0 {
		return false
	}
	w.record("reconnect %s (same boot %s)", dev.name, dev.daemon.label)
	dev.up.Store(true)
	dev.daemon.kick(true)
	return true
}

func (w *seqWorld) opDaemonRestart(graceful bool) bool {
	dev := w.liveDevice()
	old := dev.daemon
	if graceful {
		w.record("daemon %s restarts gracefully (old boot %s)", dev.name, old.label)
	} else {
		w.record("daemon %s crashes and restarts (old boot %s)", dev.name, old.label)
	}
	old.graceful = graceful // from the moment its shutdown may begin
	if stubborn := w.m.blockedOn(old.boot); graceful && len(stubborn) > 0 && w.m.ready(dev) && old.armed() == nil && !w.m.faultPending() && w.pick(2) == 0 {
		// A tool ignores cancellation, so the dispatcher is closed while the
		// link still answers: a new dispatch is refused with SHUTTING_DOWN.
		e := stubborn[w.pick(len(stubborn))]
		w.m.setStubborn(e)
		old.cancel()
		w.wait("dispatcher closed", func() bool { return e.ctx.Err() != nil })
		id := fmt.Sprintf("c%03d", w.step)
		w.record("  %s ignores cancellation; submit %s to %s during its shutdown", e.callID, id, dev.name)
		c := w.m.newCall(id, dev)
		w.m.submit(c, dev, fmt.Sprintf("cmd-%d", w.step), "new")
		w.wait("refusal of "+id, func() bool { row := w.m.row(id); return row == nil || row.State.Terminal() || len(w.m.execsOf(c)) > 0 })
		w.m.drainSubs()
		w.m.release(e)
	}
	// A dispatch racing the shutdown's start is not used: its outcome
	// depends on scheduling, and a seed must replay exactly. The stubborn
	// variant above and opResendDuringShutdown reach the refusals
	// deterministically.
	w.stopDaemon(old, graceful)
	w.m.daemonStopped(old)
	w.settle("daemon stop")
	if w.failed {
		return true
	}
	w.startDaemon(dev)
	w.record("  new boot %s", dev.daemon.label)
	return true
}

func (w *seqWorld) opControlPlaneCrash() bool {
	if w.fault.Crashed() {
		return false
	}
	if w.pick(2) == 0 && !w.crashArmed {
		n := 1 + w.pick(12)
		w.record("arm control-plane crash after %d more store writes", n)
		w.fault.CrashAfterWrite(n, nil)
		w.crashArmed = true
		w.effect("store")
		w.m.storeFault() // a crash point can fail a result commit
		return true
	}
	w.record("control plane crashes now")
	w.fault.FailAllWrites(nil) // an injected failure would preempt the crash point
	w.fault.FailWrite(1<<40, nil)
	w.fault.CrashAfterWrite(1, nil)
	_ = w.fault.CancelTimer(w.ctx, "sequence", "crash") // the crash point
	w.restartControlPlane()
	return true
}

func (w *seqWorld) opAdvance() bool {
	steps := []time.Duration{time.Second, 5 * time.Second, 16 * time.Second, 31 * time.Second, 2*time.Minute + time.Second, 5 * time.Minute, 11 * time.Minute, 31 * time.Minute}
	d := steps[w.pick(len(steps))]
	w.record("advance %s", d)
	w.advance(d)
	return true
}

func (w *seqWorld) opStoreFault() bool {
	switch {
	case w.faultAll:
		w.record("store writes recover")
		w.fault.FailAllWrites(nil)
		w.faultAll = false
	case w.pick(3) == 0:
		w.record("every store write fails")
		w.fault.FailAllWrites(store.ErrBusy)
		w.faultAll = true
		w.effect("store")
		w.m.storeFault()
	default:
		n := 1 + w.pick(6)
		w.record("store write +%d fails once", n)
		w.faultTarget = w.fault.Writes() + n
		w.fault.FailWrite(n, store.ErrBusy)
		w.effect("store")
		w.m.storeFault()
	}
	return true
}

func (w *seqWorld) opStall() bool {
	s := w.m.pickSub()
	if s == nil {
		return false
	}
	s.stalled = !s.stalled
	if s.stalled {
		w.record("subscriber of %s (%s) stalls", s.call.id, s.kind)
	} else {
		w.record("subscriber of %s (%s) resumes", s.call.id, s.kind)
	}
	return true
}

func (w *seqWorld) opQuery() bool {
	c := w.m.pickActive()
	if c == nil || c.dev.halfOpen() {
		return false // the query would wait for the heartbeat timeout
	}
	w.record("query %s", c.id)
	w.m.query(c, false)
	return true
}

// quiesce ends the sequence: heal the store, restart a crashed control
// plane, resume subscribers, reconnect every device, release every tool and
// advance past every timer until all calls are terminal; then query them.
func (w *seqWorld) quiesce() {
	w.step++
	w.record("quiesce")
	w.beginStep()
	w.fault.FailAllWrites(nil)
	w.faultAll = false
	w.fault.FailWrite(1<<40, nil)
	w.faultTarget = 0
	if w.crashArmed {
		w.fault.CrashAfterWrite(1<<40, nil)
		w.crashArmed = false
	}
	if w.fault.Crashed() {
		w.restartControlPlane()
	}
	w.m.unstallAll()
	for _, d := range w.devices {
		if !d.ghost && d.daemon.armed() != nil {
			if d.halfOpen() {
				d.daemon.cut() // its heartbeat timeout
			}
			d.daemon.disarm()
		}
	}
	w.settle("quiesce faults")
	for _, d := range w.devices {
		if !d.ghost {
			d.up.Store(true)
			d.daemon.kick(true)
		}
	}
	w.settle("quiesce reconnect")
	for round := 0; round < 16 && !w.failed; round++ {
		released := w.m.releaseAll()
		w.settle("quiesce release")
		if !released && w.m.allTerminal() {
			break
		}
		if !released {
			w.advance(5 * time.Minute)
			for _, d := range w.devices {
				if !d.ghost {
					d.daemon.kick(true)
				}
			}
			w.settle("quiesce advance")
		}
	}
	if w.failed {
		return
	}
	w.m.checkStep()
	w.m.finalQueries()
	w.advance(31 * time.Minute)
	w.settle("quiesce retention")
	if !w.failed {
		w.m.checkFinal()
		w.summary = w.m.summary()
	}
}

// teardown stops every daemon, then the control plane, and checks that
// nothing is retained: router and gateway gauges, registry routes,
// and no watchdog timer of a terminal call.
func (w *seqWorld) teardown() {
	w.m.closing = true
	w.haltTimers()
	for _, d := range w.devices {
		if d.daemon != nil && !d.daemon.stopped {
			w.m.releaseAll()
			w.stopDaemon(d.daemon, true)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := w.gateway.Shutdown(ctx); err != nil {
		w.fail("gateway shutdown: %v", err)
	}
	w.server.Close()
	if err := w.router.Close(ctx); err != nil {
		w.fail("router close: %v", err)
	}
	w.m.drainSubs()
	if s := w.router.Stats(); s.PendingBytes != 0 || s.PendingCalls != 0 || s.Subscribers != 0 || s.BufferedBytes != 0 || s.QueryWaiters != 0 || s.Operations != 0 || s.Callbacks != 0 || s.Devices != 0 || s.SyncRequests != 0 || s.UnrecordedDisconnects != 0 {
		w.fail("router retains resources after close: %+v", s)
	}
	if s := w.gateway.Stats(); s.Connections != 0 || s.Registered != 0 || s.Handshakes != 0 || s.Budget.Send.Used != 0 || s.Budget.Receive.Used != 0 {
		w.fail("gateway retains resources after shutdown: %+v", s)
	}
	if n := w.registry.Stats().Connections; n != 0 {
		w.fail("registry retains %d routes", n)
	}
	rows, err := w.db.ListTimers(ctx, store.TimerFilter{Kind: timers.CallWatchdog, Limit: store.MaxPageSize})
	must(w.t, err)
	for _, tm := range rows {
		if c, err := w.db.GetCall(ctx, tm.SubjectID); err != nil || c.State.Terminal() {
			w.fail("watchdog %+v points at a terminal or missing call (%s, %v)", tm, c.State, err)
		}
	}
	must(w.t, w.db.Close())
}

// sortedKeys keeps map walks deterministic for the operation picker.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
