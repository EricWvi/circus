package calls_test

import (
	"context"
	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/clock"
	"github.com/EricWvi/circus/internal/controlplane/bus"
	"github.com/EricWvi/circus/internal/controlplane/calls"
	"github.com/EricWvi/circus/internal/controlplane/gateway"
	"github.com/EricWvi/circus/internal/controlplane/timers"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/store/sqlite"
	"github.com/EricWvi/circus/internal/store/storetest"
	"github.com/EricWvi/circus/internal/testutil/leakcheck"
	"github.com/EricWvi/circus/internal/testutil/wait"
	"github.com/EricWvi/circus/internal/wire"
	"github.com/EricWvi/circus/internal/wirepolicy"
	"github.com/coder/websocket"
	"go.uber.org/goleak"
	"google.golang.org/protobuf/proto"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	config := leakcheck.Config{}
	if os.Getenv("CIRCUS_MEASURE") == "1" {
		config.MemoryLimit = 64 << 30 // the opt-in measurements move 64 MiB messages
	}
	os.Exit(leakcheck.Run(m, config))
}

type authStub struct{}

func (authStub) Authenticate(_ context.Context, r *http.Request) (gateway.Principal, error) {
	switch r.Header.Get("Authorization") {
	case "Bearer test":
		return gateway.Principal{UserID: "u", DeviceID: "d", DeviceName: "desk", CredentialID: "cred"}, nil
	case "Bearer laptop":
		return gateway.Principal{UserID: "u", DeviceID: "d2", DeviceName: "laptop", CredentialID: "cred2"}, nil
	}
	return gateway.Principal{}, gateway.ErrUnauthenticated
}
func (authStub) Recheck(context.Context, gateway.Principal) error { return nil }

type fixture struct {
	daemonBase    wire.Options
	raw           []wire.Conn
	runner        timers.Runner
	stopTimers    context.CancelFunc
	timerDone     chan error
	routerConfig  calls.Config
	gatewayConfig gateway.Config
	dbPath        string
	t             *testing.T
	ctx           context.Context
	clock         *clock.Fake
	db            store.Store
	fault         *storetest.Faulty
	registry      gateway.Registry
	bus           bus.Bus
	router        calls.Router
	gateway       gateway.Handler
	server        *httptest.Server
	links         []*wire.Link
	hooks         *hookCounter
	timersRunning bool
}

// hookCounter wraps the gateway's consumer seam and counts completed hooks, so
// tests wait for real processing instead of sleeping.
type hookCounter struct {
	gateway.Hooks
	snapshots, events, acks, disconnects atomic.Int64
}

func (h *hookCounter) OnCallAck(ctx context.Context, c gateway.Connection, v *circusv1.CallAck) error {
	defer h.acks.Add(1)
	return h.Hooks.OnCallAck(ctx, c, v)
}

func (h *hookCounter) OnDisconnected(ctx context.Context, c gateway.Connection) error {
	defer h.disconnects.Add(1)
	return h.Hooks.OnDisconnected(ctx, c)
}

func (h *hookCounter) OnJobSnapshot(ctx context.Context, c gateway.Connection, v *circusv1.JobSnapshot) error {
	defer h.snapshots.Add(1)
	return h.Hooks.OnJobSnapshot(ctx, c, v)
}
func (h *hookCounter) OnJobEvent(ctx context.Context, c gateway.Connection, v *circusv1.JobEvent) error {
	defer h.events.Add(1)
	return h.Hooks.OnJobEvent(ctx, c, v)
}

// assertReleased checks the leakcheck-style ownership invariants after Close:
// no pending content, subscribers, waiters, operations, callbacks, device
// cursors, registry routes, gateway sockets, pool reservations or bus charges.
func (f *fixture) assertReleased() {
	f.t.Helper()
	s := f.router.Stats()
	if s.PendingBytes != 0 || s.PendingCalls != 0 || s.Subscribers != 0 || s.BufferedBytes != 0 || s.QueryWaiters != 0 || s.Operations != 0 || s.Callbacks != 0 || s.Devices != 0 {
		f.t.Errorf("router ownership: %+v", s)
	}
	if s := f.gateway.Stats(); s.Connections != 0 || s.Registered != 0 || s.Handshakes != 0 || s.Budget.Send.Used != 0 || s.Budget.Receive.Used != 0 {
		f.t.Errorf("gateway ownership: %+v", s)
	}
	if n := f.registry.Stats().Connections; n != 0 {
		f.t.Errorf("registry retains %d routes", n)
	}
	if s.SyncRequests != 0 || s.UnrecordedDisconnects != 0 {
		f.t.Errorf("router sync ownership: %+v", s)
	}
	if s := f.bus.Stats(); s.InFlight != 0 || s.Bytes != 0 {
		f.t.Errorf("bus ownership: %+v", s)
	}
}

func setup(t *testing.T, configure ...func(*calls.Config, *gateway.Config)) *fixture {
	t.Helper()
	baseline := goleak.IgnoreCurrent()
	t.Cleanup(func() { goleak.VerifyNone(t, baseline) })
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	dbPath := filepath.Join(t.TempDir(), "db")
	db, err := sqlite.Open(ctx, dbPath, sqlite.Options{})
	must(t, err)
	f := &fixture{t: t, ctx: ctx, clock: clock.New(), db: db, dbPath: dbPath}
	f.fault = storetest.NewFaulty(db)
	must(t, db.CreateUser(ctx, store.User{ID: "u", Name: "user", CreatedAt: 1}))
	must(t, db.CreateIdentity(ctx, store.Identity{Kind: store.KindDevice, ID: "d", UserID: "u", Name: "desk", MachineID: "m", CreatedAt: 1}))
	f.registry, err = gateway.NewRegistry(8)
	must(t, err)
	f.bus, err = bus.New(bus.Limits{MaxInstances: 1, MaxInFlight: 32, MaxBytes: 16 << 20})
	must(t, err)
	f.routerConfig = calls.Config{Store: f.fault, Registry: f.registry, Bus: f.bus, Clock: f.clock, MaxMessageBytes: 256 << 10}
	f.gatewayConfig = gateway.Config{InstanceID: "instance", Clock: f.clock, Auth: authStub{}, Registry: f.registry, Bus: f.bus, MaxConnections: 8, LinkOptions: wire.Options{MaxMessageBytes: 256 << 10, MaxFragmentBytes: 1024, DisableHeartbeat: true}}
	for _, fn := range configure {
		fn(&f.routerConfig, &f.gatewayConfig)
	}
	f.router, err = calls.New(f.routerConfig)
	must(t, err)
	f.hooks = &hookCounter{Hooks: f.router}
	if f.gatewayConfig.Hooks != nil {
		if setter, ok := f.gatewayConfig.Hooks.(interface{ SetHooks(gateway.Hooks) }); ok {
			setter.SetHooks(f.router)
		}
		f.hooks.Hooks = f.gatewayConfig.Hooks
	}
	f.gatewayConfig.Hooks = f.hooks
	f.gateway, err = gateway.New(f.gatewayConfig)
	must(t, err)
	f.server = httptest.NewServer(f.gateway)
	f.startTimers()
	t.Cleanup(func() {
		f.haltTimers()
		for _, l := range f.links {
			_ = l.Close()
		}
		f.fault.FailAllWrites(nil)
		cleanup, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		must(t, f.gateway.Shutdown(cleanup))
		f.server.Close()
		must(t, f.router.Close(cleanup))
		f.assertReleased()
		for after := (*store.TimerCursor)(nil); ; {
			ts, err := f.db.ListTimers(cleanup, store.TimerFilter{Limit: store.MaxPageSize, After: after})
			must(t, err)
			for _, tm := range ts {
				if tm.Kind == "call_watchdog" {
					c, err := f.db.GetCall(cleanup, tm.SubjectID)
					must(t, err)
					if c.State.Terminal() {
						t.Errorf("orphan timer %+v for %s", tm, c.State)
					}
				}
			}
			if len(ts) < store.MaxPageSize {
				break
			}
			last := ts[len(ts)-1]
			after = &store.TimerCursor{DueAt: last.DueAt, Kind: last.Kind, SubjectID: last.SubjectID}
		}
		must(t, f.db.Close())
	})
	return f
}
func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func call(id string) *circusv1.DeviceCall {
	return &circusv1.DeviceCall{CallId: id, SessionId: "session", Tool: &circusv1.DeviceCall_Bash{Bash: &circusv1.BashRequest{Command: "echo test", Cwd: "/tmp"}}}
}
func (f *fixture) open(boot string) *wire.Link {
	f.t.Helper()
	return f.openAs("test", boot)
}

// openAs connects as the device of credential token ("test": d/desk,
// "laptop": d2/laptop, which the test must create).
func (f *fixture) openAs(token, boot string) *wire.Link {
	f.t.Helper()
	conn, _, err := wire.DialWebSocket(f.ctx, "ws"+strings.TrimPrefix(f.server.URL, "http")+"/v1/link/daemon", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": []string{"Bearer " + token}}})
	must(f.t, err)
	data, err := proto.Marshal(&circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_Hello{Hello: &circusv1.Hello{BootId: boot, ProtocolVersion: 1}}})
	must(f.t, err)
	must(f.t, conn.WriteMessage(f.ctx, data))
	data, err = conn.ReadMessage(f.ctx)
	must(f.t, err)
	m, err := wire.Decode(wire.DaemonDownlink, data, wire.MaxMetadataBytes)
	must(f.t, err)
	opts, err := wire.OptionsFromWelcome(m.(*circusv1.DaemonDownlink).GetWelcome(), f.daemonOptions())
	must(f.t, err)
	l, err := wire.New(conn, wire.DaemonSide, opts)
	must(f.t, err)
	f.links = append(f.links, l)
	f.raw = append(f.raw, conn)
	return l
}
func (f *fixture) connect(boot string) *wire.Link {
	l := f.open(boot)
	if got := receive(f.t, f.ctx, l); got.GetListJobs() == nil {
		f.t.Fatalf("expected initial snapshot request: %v", got)
	}
	f.snapshot(l)
	return l
}

// snapshot answers the sync's ListJobs with an empty snapshot and waits until
// the router processed it and activated the route.
func (f *fixture) snapshot(l *wire.Link) {
	f.t.Helper()
	before := f.hooks.snapshots.Load()
	send(f.t, f.ctx, l, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_JobSnapshot{JobSnapshot: &circusv1.JobSnapshot{}}})
	eventually(f.t, func() bool { return f.hooks.snapshots.Load() > before })
	eventually(f.t, func() bool { r, ok, _ := f.registry.Lookup(f.ctx, "d"); return ok && r.Ready })
}
func send(t testing.TB, ctx context.Context, l *wire.Link, m proto.Message) {
	t.Helper()
	class, key, err := wirepolicy.Classify(m)
	must(t, err)
	must(t, l.Send(ctx, m, class, key))
}
func receive(t testing.TB, ctx context.Context, l *wire.Link) *circusv1.DaemonDownlink {
	t.Helper()
	m, err := l.Receive(ctx)
	must(t, err)
	return m.(*circusv1.DaemonDownlink)
}
func eventually(t testing.TB, p func() bool) {
	t.Helper()
	wait.For(t, p)
}
func TestResultBeforeAckF22(t *testing.T) {
	f := setup(t)
	l := f.connect("boot")
	s, err := f.router.Submit(f.ctx, "u", "desk", call("call"))
	must(t, err)
	defer s.Close()
	dispatch := receive(t, f.ctx, l).GetDispatchCall()
	if dispatch == nil || dispatch.TargetBootId != "boot" {
		t.Fatal(dispatch)
	}
	persisted, err := f.db.GetCall(f.ctx, "call")
	must(t, err)
	if persisted.State != store.CallDispatched || persisted.DispatchedBootID != "boot" {
		t.Fatal(persisted)
	}
	send(t, f.ctx, l, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_Result{Result: &circusv1.DeviceResult{CallId: "call", Output: &circusv1.ToolOutput{Content: []*circusv1.ContentBlock{{Block: &circusv1.ContentBlock_Text{Text: "result"}}}}}}})
	for {
		u, err := s.Next(f.ctx)
		must(t, err)
		if u.GetResult() != nil {
			if u.GetResult().Output.Content[0].GetText() != "result" {
				t.Fatal(u)
			}
			break
		}
	}
	eventually(t, func() bool { c, _ := f.db.GetCall(f.ctx, "call"); return c.State == store.CallCompleted })
}

func (f *fixture) startTimers() {
	var err error
	f.runner, err = timers.New(timers.Config{Store: f.fault, Clock: f.clock, InstanceID: "instance", Lease: time.Minute, PollInterval: time.Second, BatchSize: 64, Handler: func(ctx context.Context, t store.Timer) error { return f.router.HandleTimer(ctx, t) }})
	must(f.t, err)
	var ctx context.Context
	ctx, f.stopTimers = context.WithCancel(f.ctx)
	f.timerDone = make(chan error, 1)
	f.timersRunning = true
	go func() { f.timerDone <- f.runner.Run(ctx) }()
	eventually(f.t, func() bool { return f.runner.Stats().Running })
}

// haltTimers stops the runner once and joins it.
func (f *fixture) haltTimers() {
	if f.timersRunning {
		f.stopTimers()
		<-f.timerDone
		f.timersRunning = false
	}
}
func (f *fixture) advance(d time.Duration) { f.clock.Advance(d); f.runner.Wake() }
func (f *fixture) state(id string) store.CallState {
	c, err := f.db.GetCall(f.ctx, id)
	must(f.t, err)
	return c.State
}
func (f *fixture) waitState(id string, state store.CallState) {
	eventually(f.t, func() bool { return f.state(id) == state })
}
func (f *fixture) disconnect(l *wire.Link) {
	f.t.Helper()
	disconnectedAt := f.clock.Now().UnixMilli()
	before := f.hooks.disconnects.Load()
	must(f.t, l.Close())
	f.awaitDisconnectRecorded(before, disconnectedAt)
}

// awaitDisconnectRecorded waits until the router recorded a disconnect after
// the hook count before, at fake time disconnectedAt (ms), so a following
// advance cannot land before OnDisconnected reads Now.
func (f *fixture) awaitDisconnectRecorded(before int64, disconnectedAt int64) {
	f.t.Helper()
	grace := f.routerConfig.GracePeriod
	if grace == 0 {
		grace = 30 * time.Second
	}
	eventually(f.t, func() bool {
		if f.registry.Stats().Connections != 0 || f.hooks.disconnects.Load() == before {
			return false
		}
		// A same-fake-time reconnect can legitimately recreate a row with the
		// same LastDisconnectedAt and DueAt, so DueAt > its previous value is
		// not a freshness condition. This hook completion is: the counter is
		// deferred until OnDisconnected, including its Now and Tx, has returned.
		// The exact durable values and no unrecorded disconnect then prove that
		// this disconnect's write landed rather than a stale grace row surviving.
		d, err := f.db.GetIdentity(f.ctx, store.KindDevice, "d")
		if err != nil || d.LastDisconnectedAt != disconnectedAt {
			return false
		}
		rows, err := f.db.ListTimers(f.ctx, store.TimerFilter{Kind: "device_grace", SubjectID: "d", Limit: 1})
		return err == nil && len(rows) == 1 && rows[0].DueAt == disconnectedAt+grace.Milliseconds() && f.router.Stats().UnrecordedDisconnects == 0
	})
}
func (f *fixture) finishSync(l *wire.Link) {
	f.t.Helper()
	if got := receive(f.t, f.ctx, l); got.GetListJobs() == nil {
		f.t.Fatalf("expected ListJobs, got %v", got)
	}
	f.snapshot(l)
}
func (f *fixture) running(l *wire.Link, id string) {
	send(f.t, f.ctx, l, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_CallAck{CallAck: &circusv1.CallAck{CallId: id, Outcome: &circusv1.CallAck_Status{Status: &circusv1.CallStatus{CallId: id, State: circusv1.CallStatus_STATE_RUNNING}}}}})
	f.waitState(id, store.CallRunning)
}
func (f *fixture) result(l *wire.Link, id string) {
	send(f.t, f.ctx, l, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_Result{Result: &circusv1.DeviceResult{CallId: id, Output: &circusv1.ToolOutput{Content: []*circusv1.ContentBlock{{Block: &circusv1.ContentBlock_Text{Text: "done"}}}}}}})
}
func drain(t testing.TB, ctx context.Context, s *calls.Subscription, n int) {
	t.Helper()
	for range n {
		_, err := s.Next(ctx)
		must(t, err)
	}
}
func nextResult(t testing.TB, ctx context.Context, s *calls.Subscription) *circusv1.CallUpdate {
	t.Helper()
	for range 16 {
		u, err := s.Next(ctx)
		must(t, err)
		if u.GetResult() != nil {
			return u
		}
	}
	t.Fatal("no result")
	return nil
}

func (f *fixture) daemonOptions() wire.Options {
	o := f.daemonBase
	o.Clock = f.clock
	o.DisableHeartbeat = true
	return o
}
