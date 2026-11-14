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
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/store/sqlite"
	"github.com/EricWvi/circus/internal/wire"
	"github.com/coder/websocket"
	"google.golang.org/protobuf/proto"
)

// measureEnabled gates the opt-in measurements (CIRCUS_MEASURE=1). They use
// the real clock and large messages, so they are slow and memory-hungry.
func measureEnabled(t *testing.T) {
	if os.Getenv("CIRCUS_MEASURE") != "1" {
		t.Skip("measurement; set CIRCUS_MEASURE=1 (see calls/DESIGN.md, Capacity planning and deviation 1)")
	}
}

// measureSeconds is the per-phase duration, CIRCUS_MEASURE_SECONDS or def.
func measureSeconds(t *testing.T, def int) time.Duration {
	if v := os.Getenv("CIRCUS_MEASURE_SECONDS"); v != "" {
		n, err := strconv.Atoi(v)
		must(t, err)
		return time.Duration(n) * time.Second
	}
	return time.Duration(def) * time.Second
}

// lockSamples collects the observed lock holds of one phase.
type lockSamples struct {
	mu        sync.Mutex
	recording atomic.Bool
	held      map[string][]time.Duration
}

func (s *lockSamples) observe(kind string, held time.Duration) {
	if !s.recording.Load() {
		return
	}
	s.mu.Lock()
	s.held[kind] = append(s.held[kind], held)
	s.mu.Unlock()
}

// lockDaemon is a fake daemon for the lock measurement: it acknowledges a
// DispatchCall as running, then returns a result of the phase's size, and
// answers resends and queries from its own record across reconnects.
type lockDaemon struct {
	h     *lockHarness
	id    string
	token string
	boot  string
	mu    sync.Mutex
	link  *wire.Link
	done  map[string]int // result size of executed calls (results are regenerated)
}

type lockHarness struct {
	t       *testing.T
	ctx     context.Context
	db      store.Store
	router  calls.Router
	gateway gateway.Handler
	server  *httptest.Server
	runner  timers.Runner
	daemons []*lockDaemon
	result  string // shared result text of the phase
	work    sync.WaitGroup
}

type lockAuth struct{ h *lockHarness }

func (a lockAuth) Authenticate(_ context.Context, r *http.Request) (auth.Principal, error) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	for _, d := range a.h.daemons {
		if d.token == token {
			return auth.Principal{Kind: store.KindDevice, UserID: "u", ID: d.id, Name: "device-" + d.id, CredentialID: "cred-" + d.id}, nil
		}
	}
	return auth.Principal{}, auth.ErrUnauthenticated
}
func (lockAuth) Valid(context.Context, string) (bool, error) { return true, nil }

func (d *lockDaemon) connect() error {
	h := d.h
	conn, _, err := wire.DialWebSocket(h.ctx, "ws"+strings.TrimPrefix(h.server.URL, "http")+"/v1/link/daemon", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": []string{"Bearer " + d.token}}})
	if err != nil {
		return err
	}
	data, err := proto.Marshal(&circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_Hello{Hello: &circusv1.Hello{BootId: d.boot, ProtocolVersion: 1}}})
	if err == nil {
		err = conn.WriteMessage(h.ctx, data)
	}
	if err == nil {
		data, err = conn.ReadMessage(h.ctx)
	}
	var m proto.Message
	if err == nil {
		m, err = wire.Decode(wire.DaemonDownlink, data, wire.MaxMetadataBytes)
	}
	var opts wire.Options
	if err == nil {
		opts, err = wire.OptionsFromWelcome(m.(*circusv1.DaemonDownlink).GetWelcome(), wire.Options{})
	}
	var l *wire.Link
	if err == nil {
		l, err = wire.New(conn, wire.DaemonSide, opts)
	}
	if err != nil {
		_ = conn.Close()
		return err
	}
	d.mu.Lock()
	d.link = l
	d.mu.Unlock()
	h.work.Add(1)
	go func() { defer h.work.Done(); d.serve(l) }()
	return nil
}

func (d *lockDaemon) send(l *wire.Link, m *circusv1.DaemonUplink) {
	_ = l.Send(context.Background(), m)
}

// resultFor returns a result as large as the call's request.
func (d *lockDaemon) resultFor(id string, size int) *circusv1.DeviceResult {
	return &circusv1.DeviceResult{CallId: id, Output: &circusv1.ToolOutput{Content: []*circusv1.ContentBlock{{Block: &circusv1.ContentBlock_Text{Text: d.h.result[:size]}}}}}
}

func (d *lockDaemon) status(id string) *circusv1.CallStatus {
	d.mu.Lock()
	size, done := d.done[id]
	d.mu.Unlock()
	if done {
		return &circusv1.CallStatus{CallId: id, State: circusv1.CallStatus_STATE_COMPLETED, Result: d.resultFor(id, size)}
	}
	return &circusv1.CallStatus{CallId: id, State: circusv1.CallStatus_STATE_LOST}
}

func (d *lockDaemon) serve(l *wire.Link) {
	for {
		m, err := l.Receive(context.Background())
		if err != nil {
			return
		}
		switch p := m.(*circusv1.DaemonDownlink).Payload.(type) {
		case *circusv1.DaemonDownlink_DispatchCall:
			id := p.DispatchCall.GetCall().GetCallId()
			size := max(1, len(p.DispatchCall.GetCall().GetWrite().GetContent()))
			d.mu.Lock()
			_, seen := d.done[id]
			d.done[id] = size
			d.mu.Unlock()
			if seen {
				d.send(l, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_CallAck{CallAck: &circusv1.CallAck{CallId: id, Outcome: &circusv1.CallAck_Status{Status: d.status(id)}}}})
				continue
			}
			d.send(l, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_CallAck{CallAck: &circusv1.CallAck{CallId: id, Outcome: &circusv1.CallAck_Status{Status: &circusv1.CallStatus{CallId: id, State: circusv1.CallStatus_STATE_RUNNING}}}}})
			d.send(l, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_Result{Result: d.resultFor(id, size)}})
		case *circusv1.DaemonDownlink_CallQuery:
			d.send(l, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_CallStatus{CallStatus: d.status(p.CallQuery.GetCallId())}})
		case *circusv1.DaemonDownlink_ListJobs:
			d.send(l, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_JobSnapshot{JobSnapshot: &circusv1.JobSnapshot{}}})
		case *circusv1.DaemonDownlink_GoAway:
			_ = l.Shutdown(context.Background())
			return
		}
	}
}

// reconnect aborts the daemon's socket and connects again with the same
// boot, so the router runs a reconnect sync (resends and queries).
func (d *lockDaemon) reconnect() {
	d.mu.Lock()
	l := d.link
	d.mu.Unlock()
	if l != nil {
		_ = l.Close()
	}
	for d.h.ctx.Err() == nil {
		if err := d.connect(); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// lockPhase runs concurrency closed-loop workers; the first big of them use
// bigBytes requests and results instead of bytes.
type lockPhase struct {
	name          string
	bytes         int
	concurrency   int
	big, bigBytes int
}

// TestMeasureLockHold measures how long each operation holds the router-wide
// admission lock (DESIGN.md deviation 1) under load: 64 devices, concurrent
// submits and results of one size per phase, and a reconnect sync every
// 250 ms. Requests and results have the phase's size, so marshal, hash and
// clone under the lock scale with it. CIRCUS_MEASURE_DIR places the SQLite
// file (default: the test's temporary directory, often tmpfs).
func TestMeasureLockHold(t *testing.T) {
	measureEnabled(t)
	duration := measureSeconds(t, 10)
	phases := []lockPhase{
		{"1KiB", 1 << 10, 64, 0, 0},
		{"64KiB", 64 << 10, 64, 0, 0},
		{"1MiB", 1 << 20, 32, 0, 0},
		{"8MiB", 8 << 20, 8, 0, 0},
		{"63MiB", 63 << 20, 2, 0, 0},
		{"mixed", 1 << 10, 64, 1, 63 << 20},
	}
	if v := os.Getenv("CIRCUS_MEASURE_PHASES"); v != "" {
		keep := strings.Split(v, ",")
		phases = slices.DeleteFunc(phases, func(p lockPhase) bool { return !slices.Contains(keep, p.name) })
	}
	samples := &lockSamples{held: make(map[string][]time.Duration)}
	var report strings.Builder
	fmt.Fprintf(&report, "\n| size | conc. | calls | calls/s | lock busy | kind | count | p50 | p99 | max | mean | total/call |\n|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	for _, phase := range phases {
		runLockPhase(t, phase, duration, samples, &report)
	}
	t.Log(report.String())
}

func runLockPhase(t *testing.T, phase lockPhase, duration time.Duration, samples *lockSamples, report *strings.Builder) {
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer slog.SetDefault(previous)
	dir := t.TempDir()
	if v := os.Getenv("CIRCUS_MEASURE_DIR"); v != "" {
		var err error
		dir, err = os.MkdirTemp(v, "lock-measure-")
		must(t, err)
		defer os.RemoveAll(dir)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	h := &lockHarness{t: t, ctx: ctx, result: strings.Repeat("r", max(phase.bytes, phase.bigBytes))}
	db, err := sqlite.Open(ctx, filepath.Join(dir, "db"), sqlite.Options{})
	must(t, err)
	h.db = db
	must(t, db.CreateUser(ctx, store.User{ID: "u", Name: "user", CreatedAt: 1}))
	for i := range 64 {
		d := &lockDaemon{h: h, id: fmt.Sprintf("d%02d", i), token: fmt.Sprintf("token-%02d", i), boot: fmt.Sprintf("boot-%02d", i), done: make(map[string]int)}
		must(t, db.CreateIdentity(ctx, store.Identity{Kind: store.KindDevice, ID: d.id, UserID: "u", Name: "device-" + d.id, MachineID: "m" + d.id, CreatedAt: 1}))
		h.daemons = append(h.daemons, d)
	}
	registry, err := gateway.NewRegistry(128)
	must(t, err)
	clk := clock.Real{}
	config := calls.Config{Store: db, Registry: registry, Sender: senderTo(&h.gateway), Clock: clk, Pending: calls.PendingBudget{MaxBytes: 8 << 30, MaxCalls: 4096}, MaxSubscriptionBytes: 1 << 30, MaxBufferedUpdateBytes: 8 << 30}
	calls.WithLockObserver(&config, samples.observe)
	h.router, err = calls.New(config)
	must(t, err)
	h.gateway, err = gateway.New(gateway.Config{InstanceID: "instance", Clock: clk, Auth: lockAuth{h}, Registry: registry, Hooks: h.router, MaxConnections: 128, SendPoolBytes: 2 << 30, ReceivePoolBytes: 4 << 30})
	must(t, err)
	h.server = httptest.NewServer(h.gateway)
	h.runner, err = timers.New(timers.Config{Store: db, Clock: clk, InstanceID: "instance", Lease: time.Minute, PollInterval: time.Second, BatchSize: 64, Handler: h.router.HandleTimer})
	must(t, err)
	timerCtx, stopTimers := context.WithCancel(ctx)
	timerDone := make(chan error, 1)
	go func() { timerDone <- h.runner.Run(timerCtx) }()
	for _, d := range h.daemons {
		must(t, d.connect())
	}
	eventually(t, func() bool {
		for _, d := range h.daemons {
			if r, ok, _ := registry.Lookup(ctx, d.id); !ok || !r.Ready {
				return false
			}
		}
		return true
	})

	samples.mu.Lock()
	clear(samples.held)
	samples.mu.Unlock()
	samples.recording.Store(true)
	start := time.Now()
	deadline := start.Add(duration)
	var completed, failed atomic.Int64
	var latencies []time.Duration
	var latencyMu sync.Mutex
	var workers sync.WaitGroup
	content := strings.Repeat("w", max(phase.bytes, phase.bigBytes))
	for w := range phase.concurrency {
		workers.Add(1)
		size := phase.bytes
		if w < phase.big {
			size = phase.bigBytes
		}
		go func() {
			defer workers.Done()
			d := h.daemons[w%len(h.daemons)]
			for n := 0; time.Now().Before(deadline); n++ {
				id := fmt.Sprintf("w%02d-%06d", w, n)
				c := &circusv1.DeviceCall{CallId: id, SessionId: "session", Tool: &circusv1.DeviceCall_Write{Write: &circusv1.WriteRequest{Path: "/file", Content: content[:size]}}}
				began := time.Now()
				if !runLockCall(ctx, h, d, c) {
					failed.Add(1)
					continue
				}
				completed.Add(1)
				if w < phase.big {
					continue // the small calls' latency is the measured one
				}
				latencyMu.Lock()
				latencies = append(latencies, time.Since(began))
				latencyMu.Unlock()
			}
		}()
	}
	reconnects := 0
	rng := rand.New(rand.NewPCG(1, 2))
	for time.Now().Before(deadline) {
		time.Sleep(250 * time.Millisecond)
		h.daemons[rng.IntN(len(h.daemons))].reconnect()
		reconnects++
	}
	workers.Wait()
	elapsed := time.Since(start)
	samples.recording.Store(false)

	stopTimers()
	<-timerDone
	shutdown, done := context.WithTimeout(context.Background(), 30*time.Second)
	defer done()
	must(t, h.gateway.Shutdown(shutdown))
	h.server.Close()
	must(t, h.router.Close(shutdown))
	cancel()
	h.work.Wait()
	must(t, db.Close())

	samples.mu.Lock()
	defer samples.mu.Unlock()
	var busy time.Duration
	for _, list := range samples.held {
		for _, d := range list {
			busy += d
		}
	}
	n := completed.Load()
	slices.Sort(latencies)
	t.Logf("%s: %d calls (%d failed) in %s, %d reconnects; call latency p50 %s p99 %s; lock busy %.1f%%", phase.name, n, failed.Load(), elapsed.Round(time.Millisecond), reconnects, pct(latencies, 0.5), pct(latencies, 0.99), 100*busy.Seconds()/elapsed.Seconds())
	for _, kind := range sortedKeys(samples.held) {
		list := samples.held[kind]
		slices.Sort(list)
		var total time.Duration
		for _, d := range list {
			total += d
		}
		perCall := "-"
		if n > 0 {
			perCall = (total / time.Duration(n)).String()
		}
		fmt.Fprintf(report, "| %s | %d | %d | %.0f | %.1f%% | %s | %d | %s | %s | %s | %s | %s |\n", phase.name, phase.concurrency, n, float64(n)/elapsed.Seconds(), 100*busy.Seconds()/elapsed.Seconds(), kind, len(list), pct(list, 0.5), pct(list, 0.99), list[len(list)-1].Round(time.Microsecond), (total / time.Duration(len(list))).Round(time.Microsecond), perCall)
	}
}

// runLockCall submits one call and waits for its terminal update.
func runLockCall(ctx context.Context, h *lockHarness, d *lockDaemon, c *circusv1.DeviceCall) bool {
	s, err := h.router.Submit(ctx, "u", "device-"+d.id, c)
	if err != nil {
		return false
	}
	defer s.Close()
	wait, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	for {
		u, err := s.Next(wait)
		if err != nil {
			return errors.Is(err, io.EOF)
		}
		if u.GetResult() != nil {
			return true
		}
		if isTerminal(u) {
			return false
		}
	}
}

func pct(sorted []time.Duration, q float64) string {
	if len(sorted) == 0 {
		return "-"
	}
	return sorted[min(len(sorted)-1, int(q*float64(len(sorted))))].Round(time.Microsecond).String()
}
