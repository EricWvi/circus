package sessions_test

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/auth"
	"github.com/EricWvi/circus/internal/clock"
	"github.com/EricWvi/circus/internal/controlplane/calls"
	"github.com/EricWvi/circus/internal/controlplane/gateway"
	"github.com/EricWvi/circus/internal/controlplane/sessions"
	"github.com/EricWvi/circus/internal/controlplane/workerlink"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/store/sqlite"
	"github.com/EricWvi/circus/internal/testutil/wait"
	"github.com/EricWvi/circus/internal/testutil/workerclient"
	"github.com/EricWvi/circus/internal/wire"
	"github.com/google/uuid"
	"go.uber.org/goleak"
)

// bus records what the worker link publishes.
type bus struct {
	mu        sync.Mutex
	durable   map[string]uint64
	ephemeral []*circusv1.SessionEvent
}

func (b *bus) Durable(id string, seq uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if seq > b.durable[id] {
		b.durable[id] = seq
	}
}

func (b *bus) Ephemeral(id string, ev *circusv1.SessionEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ephemeral = append(b.ephemeral, ev)
}

func (b *bus) seq(id string) uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.durable[id]
}

func (b *bus) live() []*circusv1.SessionEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]*circusv1.SessionEvent(nil), b.ephemeral...)
}

// serve starts a session service and a worker link on the fixture's store
// and router.
func (f *fixture) serve() {
	f.t.Helper()
	var err error
	f.svc, err = sessions.New(f.cfg)
	must(f.t, err)
	must(f.t, f.svc.Start(f.t.Context()))
	f.h, err = workerlink.New(workerlink.Config{Auth: auth.NewAuthenticator(f.st, auth.Options{}), Router: f.router, InstanceID: "test",
		Wire: wire.Options{MaxMessageBytes: 1 << 20}, Sessions: f.svc, Logger: f.cfg.Logger})
	must(f.t, err)
	f.srv = httptest.NewServer(f.h)
}

// restart stops the worker link and the session service, as a control
// plane crash does (leases and every other row stay in the store), and
// starts new ones; workers must dial again.
func (f *fixture) restart() {
	f.t.Helper()
	f.srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), wait.Timeout)
	defer cancel()
	must(f.t, f.h.Shutdown(ctx))
	must(f.t, f.svc.Close(ctx))
	f.serve()
}

// fixture is a worker link with the session service and a real call router
// (no daemon connected) on a SQLite store, with a fake clock for the
// session service and the router.
type fixture struct {
	t       *testing.T
	st      *sqlite.DB
	clk     *clock.Fake
	svc     *sessions.Service
	bus     *bus
	router  calls.Router
	h       *workerlink.Handler
	srv     *httptest.Server
	user    store.User
	device  store.Identity
	workers []auth.Enrollment
	stopped []string // StopJobs calls
	boots   map[string]string
	cfg     sessions.Config
	mu      sync.Mutex
}

type option func(*sessions.Config)

func newFixture(t *testing.T, opts ...option) *fixture {
	t.Helper()
	baseline := goleak.IgnoreCurrent()
	t.Cleanup(func() { goleak.VerifyNone(t, baseline) })
	ctx := t.Context()
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "db"), sqlite.Options{})
	must(t, err)
	t.Cleanup(func() { _ = st.Close() })
	f := &fixture{t: t, st: st, clk: clock.New(), bus: &bus{durable: map[string]uint64{}}}
	now := f.clk.Now()
	f.user, err = auth.EnsureUser(ctx, st, "admin", now)
	must(t, err)
	enroll := func(k store.PrincipalKind, name string) auth.Enrollment {
		token, _, err := auth.CreateEnrollToken(ctx, st, f.user.ID, k, time.Hour, now)
		must(t, err)
		e, err := auth.Enroll(ctx, st, auth.EnrollRequest{Token: token, Kind: k, Name: name, MachineID: "m-" + name, Hostname: "host-" + name, OS: "linux", Arch: "amd64"}, now)
		must(t, err)
		return e
	}
	f.device = enroll(store.KindDevice, "desk").Identity
	f.workers = []auth.Enrollment{enroll(store.KindWorker, "w1"), enroll(store.KindWorker, "w2")}
	_, err = st.CreateProject(ctx, store.Project{ID: "p1", UserID: f.user.ID, Name: "Project", CreatedAt: now.UnixMilli(), UpdatedAt: now.UnixMilli()}, "")
	must(t, err)

	registry, err := gateway.NewRegistry(8)
	must(t, err)
	f.router, err = calls.New(calls.Config{Store: st, Registry: registry, Clock: f.clk, MaxMessageBytes: 1 << 20,
		Sender: calls.SenderFunc(func(context.Context, gateway.Target, *circusv1.DaemonDownlink) error { return gateway.ErrNoRoute })})
	must(t, err)
	quiet := slog.New(slog.DiscardHandler)
	cfg := sessions.Config{Store: st, Bus: f.bus, InstanceID: "test", Clock: f.clk, MaxMessageBytes: 1 << 20, Logger: quiet,
		StopJobs: func(_ context.Context, _, id string) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.stopped = append(f.stopped, id)
			return nil
		},
		Online: func(context.Context, string) (map[string]bool, error) { return map[string]bool{f.device.ID: true}, nil }}
	for _, o := range opts {
		o(&cfg)
	}
	f.cfg = cfg
	f.serve()
	t.Cleanup(func() {
		f.srv.Close()
		ctx, cancel := context.WithTimeout(context.Background(), wait.Timeout)
		defer cancel()
		must(t, f.h.Shutdown(ctx))
		must(t, f.svc.Close(ctx))
		must(t, f.router.Close(ctx))
	})
	return f
}

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func ms(f *fixture) int64 { return f.clk.Now().UnixMilli() }

// boot returns the UUIDv7 boot id named label, minted on first use, so a
// label used first is an older boot. The empty label is no boot id.
func (f *fixture) boot(label string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if label == "" {
		return ""
	}
	if f.boots == nil {
		f.boots = map[string]string{}
	}
	if id, ok := f.boots[label]; ok {
		return id
	}
	u, err := uuid.NewV7()
	must(f.t, err)
	f.boots[label] = u.String()
	return u.String()
}

// dial connects as worker i (0 or 1) with the boot named label.
func (f *fixture) dial(i int, label string) *workerclient.Client {
	f.t.Helper()
	c, err := f.tryDial(i, label)
	must(f.t, err)
	return c
}

// tryDial is dial that returns the dial error.
func (f *fixture) tryDial(i int, label string) (*workerclient.Client, error) {
	f.t.Helper()
	c, err := workerclient.Dial(wait.Ctx(f.t), workerclient.Options{URL: f.srv.URL, Token: f.workers[i].Token, PrincipalID: f.workers[i].Identity.ID, BootID: f.boot(label)})
	if err != nil {
		return nil, err
	}
	f.t.Cleanup(func() { _ = c.Close() })
	return c, nil
}

// attach attaches c with capacity max and waits until the server has
// processed the Attach (it has no reply), unless the link closes.
func (f *fixture) attach(c *workerclient.Client, max uint32) workerclient.Events {
	f.t.Helper()
	before := sessions.Attached(f.svc)
	ev, err := c.Attach(wait.Ctx(f.t), &circusv1.Attach{MaxSessions: max, SdkVersion: "sdk", ClaudeVersion: "cli"})
	must(f.t, err)
	wait.For(f.t, func() bool {
		select {
		case <-c.Done():
			return true
		default:
		}
		return sessions.Attached(f.svc) > before
	})
	return ev
}

// session creates a session of the fixture's user; pinned names a worker
// index or -1.
func (f *fixture) session(id string, pinned int) {
	f.t.Helper()
	s := store.Session{ID: id, UserID: f.user.ID, ProjectID: "p1", ClaudeSessionID: "claude-" + id, DefaultDeviceID: f.device.ID,
		Model: "m", PermissionMode: "auto", CreatedAt: ms(f)}
	if pinned >= 0 {
		s.PinnedWorkerID = f.workers[pinned].Identity.ID
	}
	must(f.t, f.st.CreateSession(f.t.Context(), s, []store.SessionCwd{{DeviceID: f.device.ID, Cwd: "/work"}}, store.DefaultMaxSessionsPerUser))
}

// input submits a user message and wakes the scheduler, as SendMessage does.
func (f *fixture) input(id, msg string, prio store.InputPriority) store.Input {
	f.t.Helper()
	r, err := f.st.SubmitInput(f.t.Context(), store.Input{InputID: id + "-" + msg, SessionID: id, ClientMsgID: msg, Kind: store.InputUser, Text: msg, Priority: prio, CreatedAt: ms(f)})
	must(f.t, err)
	reasons := []circusv1.WorkerEvent_Nudge_Reason{circusv1.WorkerEvent_Nudge_REASON_INPUT}
	if prio == store.InputNow && r.Session.State == store.SessionRunning {
		reasons = append(reasons, circusv1.WorkerEvent_Nudge_REASON_INTERRUPT)
	}
	f.svc.Wake(id, reasons...)
	return r.Input
}

// next returns the next hint, failing the test after wait.Timeout.
func next(t *testing.T, ev workerclient.Events) *circusv1.WorkerEvent {
	t.Helper()
	e, err := ev.Next(wait.Ctx(t))
	must(t, err)
	return e
}

// noEvent asserts that no hint arrives within a short time.
func noEvent(t *testing.T, ev workerclient.Events) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if e, err := ev.Next(ctx); err == nil {
		t.Fatalf("unexpected hint %v", e)
	}
}

func call(t *testing.T, c *workerclient.Client, req *circusv1.SessionRequest) *circusv1.SessionReply {
	t.Helper()
	r, err := c.Session(wait.Ctx(t), req)
	must(t, err)
	return r
}

// acquire takes the offered session id and returns its epoch.
func acquire(t *testing.T, c *workerclient.Client, id string) uint64 {
	t.Helper()
	r := call(t, c, workerclient.Acquire(id, true))
	if r.GetLease() == nil {
		t.Fatalf("AcquireLease(%s) = %v", id, r)
	}
	return r.GetLease().GetEpoch()
}

func wantOk(t *testing.T, r *circusv1.SessionReply, dedup bool) {
	t.Helper()
	if r.GetOk() == nil || r.GetOk().GetDeduplicated() != dedup {
		t.Fatalf("reply %v, want ok deduplicated=%v", r, dedup)
	}
}

func wantFenced(t *testing.T, r *circusv1.SessionReply, epoch uint64) {
	t.Helper()
	if r.GetFenced() == nil || r.GetFenced().GetEpoch() != epoch {
		t.Fatalf("reply %v, want fenced at epoch %d", r, epoch)
	}
}

func wantRejected(t *testing.T, r *circusv1.SessionReply, code circusv1.SessionReply_Rejected_Code) {
	t.Helper()
	if r.GetRejected().GetCode() != code {
		t.Fatalf("reply %v, want rejected %v", r, code)
	}
}

func (f *fixture) get(id string) store.Session {
	f.t.Helper()
	s, err := f.st.GetSession(f.t.Context(), id, ms(f))
	must(f.t, err)
	return s
}

func (f *fixture) lease(id string) store.Lease {
	f.t.Helper()
	l, err := f.st.GetLease(f.t.Context(), id)
	must(f.t, err)
	return l
}

// settle waits until the scheduler has processed every Wake.
func (f *fixture) settle() {
	f.t.Helper()
	wait.For(f.t, func() bool { return sessions.Idle(f.svc) })
}

// offerTo waits for an Offer of id on ev.
func offerTo(t *testing.T, ev workerclient.Events, id string) {
	t.Helper()
	for {
		e := next(t, ev)
		if e.GetOffer().GetSessionId() == id {
			return
		}
	}
}

// nudge waits for a Nudge of id on ev carrying every reason in want (any
// Nudge when want is empty) and returns it. Other hints are skipped:
// hints are level-triggered and may repeat.
func nudge(t *testing.T, ev workerclient.Events, id string, want ...circusv1.WorkerEvent_Nudge_Reason) *circusv1.WorkerEvent_Nudge {
	t.Helper()
	for {
		e := next(t, ev)
		n := e.GetNudge()
		if n.GetSessionId() != id {
			continue
		}
		all := true
		for _, r := range want {
			all = all && hasReason(n, r)
		}
		if all {
			return n
		}
	}
}

func hasReason(n *circusv1.WorkerEvent_Nudge, r circusv1.WorkerEvent_Nudge_Reason) bool {
	for _, v := range n.GetReasons() {
		if v == r {
			return true
		}
	}
	return false
}

// turn runs one complete turn on the held session: it takes the inputs,
// reports STARTED and ENDED COMPLETED with them persisted, so the session
// has no pending work afterwards.
func turn(t *testing.T, c *workerclient.Client, id string, epoch, localSeq uint64) {
	t.Helper()
	var ids []string
	for _, in := range call(t, c, workerclient.TakeInputs(id, epoch)).GetInputs().GetInputs() {
		ids = append(ids, in.GetInputId())
	}
	call(t, c, workerclient.Started(id, epoch, localSeq, circusv1.TurnOrigin_TURN_ORIGIN_INPUT))
	call(t, c, workerclient.Ended(id, epoch, localSeq, circusv1.TurnOutcome_TURN_OUTCOME_COMPLETED, &circusv1.ReportTurn_Persisted{InputIds: ids}))
}

// slowStore delays the scheduler's per-session transaction and, while
// txDelay is set, every transaction.
type slowStore struct {
	store.Store
	settle, tx atomic.Int64 // nanoseconds
	// hold makes SettleSession sleep inside its transaction, holding the
	// store's one connection, as a slow scheduler transaction does.
	hold atomic.Int64
}

func (s *slowStore) SettleSession(ctx context.Context, id string, now int64) (store.SessionSettled, error) {
	if d := s.hold.Load(); d > 0 {
		var out store.SessionSettled
		err := s.Store.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
			time.Sleep(time.Duration(d))
			var err error
			out, err = tx.SettleSession(ctx, id, now)
			return err
		})
		return out, err
	}
	time.Sleep(time.Duration(s.settle.Load()))
	return s.Store.SettleSession(ctx, id, now)
}

func (s *slowStore) Tx(ctx context.Context, fn func(ctx context.Context, tx store.Ops) error) error {
	if d := s.tx.Load(); d > 0 {
		select {
		case <-time.After(time.Duration(d)):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return s.Store.Tx(ctx, fn)
}

// withSlowStore makes the session service use a slowStore over the
// fixture's store.
func withSlowStore(slow *slowStore) option {
	return func(c *sessions.Config) {
		slow.Store = c.Store
		c.Store = slow
	}
}

// noOffer asserts that no Offer arrives within a short time; other hints
// (repeated Nudges) are allowed.
func noOffer(t *testing.T, ev workerclient.Events) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	for {
		e, err := ev.Next(ctx)
		if err != nil {
			return
		}
		if e.GetOffer() != nil {
			t.Fatalf("unexpected offer %v", e)
		}
	}
}
