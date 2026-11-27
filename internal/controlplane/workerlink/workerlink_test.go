package workerlink_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/auth"
	"github.com/EricWvi/circus/internal/clock"
	"github.com/EricWvi/circus/internal/controlplane/calls"
	"github.com/EricWvi/circus/internal/controlplane/workerlink"
	"github.com/EricWvi/circus/internal/credfile"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/store/sqlite"
	"github.com/EricWvi/circus/internal/store/sqlite/sqlitetest"
	"github.com/EricWvi/circus/internal/testutil/wait"
	"github.com/EricWvi/circus/internal/wire"
	"github.com/EricWvi/circus/internal/workerclient"
	"github.com/coder/websocket"
	"go.uber.org/goleak"
	"google.golang.org/protobuf/proto"
)

type service struct {
	calls.Service
	started chan string
	active  atomic.Int32
	query   func(context.Context, string) (*circusv1.CallUpdate, error)
	cancel  func(context.Context, string) error
}

func (s *service) Query(ctx context.Context, _ string, id string) (*circusv1.CallUpdate, error) {
	s.active.Add(1)
	defer s.active.Add(-1)
	if s.query != nil {
		return s.query(ctx, id)
	}
	s.started <- id
	<-ctx.Done()
	return nil, ctx.Err()
}
func (s *service) Cancel(ctx context.Context, _ string, id string) error {
	if s.cancel != nil {
		return s.cancel(ctx, id)
	}
	return nil
}

type authStub struct {
	*auth.Authenticator
	mu     sync.RWMutex
	valid  func(context.Context, string) (bool, error)
	checks atomic.Int32
}

func (a *authStub) Valid(ctx context.Context, id string) (bool, error) {
	a.checks.Add(1)
	a.mu.RLock()
	fn := a.valid
	a.mu.RUnlock()
	return fn(ctx, id)
}
func (a *authStub) setValid(fn func(context.Context, string) (bool, error)) {
	a.mu.Lock()
	a.valid = fn
	a.mu.Unlock()
}

type fixture struct {
	h           *workerlink.Handler
	server      *httptest.Server
	st          *sqlite.DB
	f           credfile.File
	daemonToken string
	otherToken  string // a second worker credential of the same user
	svc         *service
	auth        *authStub
	clock       *clock.Fake
}

func setup(t *testing.T, configure ...func(*workerlink.Config)) *fixture {
	t.Helper()
	baseline := goleak.IgnoreCurrent()
	t.Cleanup(func() { goleak.VerifyNone(t, baseline) })
	st, err := sqlite.Open(t.Context(), sqlitetest.Path(t), sqlite.Options{})
	must(t, err)
	t.Cleanup(func() { _ = st.Close() })
	u := store.User{ID: "owner", Email: "owner@example.com", Role: store.RoleUser, CreatedAt: time.Now().UnixMilli()}
	must(t, st.CreateUser(t.Context(), u))
	enroll := func(k store.PrincipalKind, name string) auth.Enrollment {
		token, _, err := auth.CreateEnrollToken(t.Context(), st, u.ID, k, time.Hour, time.Now())
		must(t, err)
		e, err := auth.Enroll(t.Context(), st, auth.EnrollRequest{Token: token, Kind: k, Name: name, MachineID: "m", Hostname: "host", OS: "linux", Arch: "amd64"}, time.Now())
		must(t, err)
		return e
	}
	worker, daemon, other := enroll(store.KindWorker, "desk"), enroll(store.KindDevice, "desk"), enroll(store.KindWorker, "other")
	authenticator := &authStub{Authenticator: auth.NewAuthenticator(st, auth.Options{})}
	authenticator.valid = authenticator.Authenticator.Valid
	svc := &service{started: make(chan string, 256)}
	clk := clock.New()
	cfg := workerlink.Config{
		Auth:          authenticator,
		MetadataStore: st,
		Router:        svc,
		InstanceID:    "instance",
		Clock:         clk,
		Wire:          wire.Options{MaxMessageBytes: 1 << 20},
	}
	for _, fn := range configure {
		fn(&cfg)
	}
	h, err := workerlink.New(cfg)
	must(t, err)
	// Query and Cancel answers come from the stub's Query, handed out as a
	// small one-update subscription like the router's QueryOnce.
	workerlink.SetQuery(h, func(ctx context.Context, user, id string) (workerlink.Subscription, error) {
		u, err := svc.Query(ctx, user, id)
		if err != nil {
			return nil, err
		}
		return &fakeSub{u: u, cost: 1024}, nil
	})
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), wait.Timeout)
		defer cancel()
		must(t, h.Shutdown(ctx))
		if svc.active.Load() != 0 {
			t.Fatal("operations outlived shutdown")
		}
	})
	return &fixture{h: h, server: server, st: st, f: credfile.File{ControlPlaneURL: server.URL, Role: credfile.RoleWorker, PrincipalID: worker.Identity.ID, Token: worker.Token}, daemonToken: daemon.Token, otherToken: other.Token, svc: svc, auth: authenticator, clock: clk}
}
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestHelloRefreshesWorkerMetadata(t *testing.T) {
	f := setup(t)
	conn := raw(t, f)
	hello := &circusv1.Hello{
		ProtocolVersion: wire.ProtocolVersion, ClientVersion: "0.2.0", Os: "darwin", Arch: "arm64", Hostname: "new-host",
	}
	write(t, conn, &circusv1.WorkerUplink{Payload: &circusv1.WorkerUplink_Hello{Hello: hello}})
	if got := read(t, conn).GetWelcome().GetPrincipalId(); got != f.f.PrincipalID {
		t.Fatalf("Welcome principal = %q, want %q", got, f.f.PrincipalID)
	}
	wait.For(t, func() bool {
		worker, err := f.st.GetIdentity(t.Context(), store.KindWorker, f.f.PrincipalID)
		must(t, err)
		return worker.ClientVersion == hello.ClientVersion && worker.OS == hello.Os && worker.Arch == hello.Arch && worker.Hostname == hello.Hostname
	})
}

func raw(t *testing.T, f *fixture) *wire.WebSocket { t.Helper(); return rawAs(t, f, f.f.Token) }
func rawAs(t *testing.T, f *fixture, token string) *wire.WebSocket {
	t.Helper()
	c, _, err := wire.DialWebSocket(t.Context(), "ws"+strings.TrimPrefix(f.server.URL, "http")+"/v1/link/worker", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": []string{"Bearer " + token}}})
	must(t, err)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// rawAvailable tolerates only the transient capacity race between a client
// close and the server releasing the old connection's slot.
func rawAvailable(t *testing.T, f *fixture) *wire.WebSocket {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), wait.Timeout)
	defer cancel()
	for {
		c, res, err := wire.DialWebSocket(ctx, "ws"+strings.TrimPrefix(f.server.URL, "http")+"/v1/link/worker", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": []string{"Bearer " + f.f.Token}}})
		if res != nil && res.Body != nil {
			_ = res.Body.Close()
		}
		if err == nil {
			t.Cleanup(func() { _ = c.Close() })
			return c
		}
		if res == nil || res.StatusCode != http.StatusServiceUnavailable {
			must(t, err)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("worker dial remained unavailable: %v", err)
		case <-time.After(10 * time.Millisecond):
		}
	}
}
func write(t *testing.T, c wire.Conn, m proto.Message) {
	t.Helper()
	d, err := proto.Marshal(m)
	must(t, err)
	must(t, c.WriteMessage(t.Context(), d))
}
func read(t *testing.T, c wire.Conn) *circusv1.WorkerDownlink {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), wait.Timeout)
	defer cancel()
	d, err := c.ReadMessage(ctx)
	must(t, err)
	m, err := wire.Decode(wire.WorkerDownlink, d, wire.MaxMetadataBytes)
	must(t, err)
	return m.(*circusv1.WorkerDownlink)
}
func linked(t *testing.T, f *fixture) *wire.Link { t.Helper(); return linkedAs(t, f, f.f.Token) }
func linkedAs(t *testing.T, f *fixture, token string) *wire.Link {
	t.Helper()
	conn := rawAs(t, f, token)
	write(t, conn, &circusv1.WorkerUplink{Payload: &circusv1.WorkerUplink_Hello{Hello: &circusv1.Hello{ProtocolVersion: 1}}})
	welcome := read(t, conn).GetWelcome()
	if welcome.GetMaxInFlightMessages() != 1 {
		t.Fatalf("worker Welcome max_in_flight_messages = %d, want 1", welcome.GetMaxInFlightMessages())
	}
	opts, err := wire.OptionsFromWelcome(welcome, wire.Options{})
	must(t, err)
	l, err := wire.New(conn, wire.WorkerSide, opts)
	must(t, err)
	t.Cleanup(func() { _ = l.Close() })
	return l
}
func send(t *testing.T, l *wire.Link, m *circusv1.WorkerUplink) {
	t.Helper()
	must(t, l.Send(t.Context(), m))
}
func query(rid uint64, id string) *circusv1.WorkerUplink {
	return &circusv1.WorkerUplink{RequestId: rid, Payload: &circusv1.WorkerUplink_CallQuery{CallQuery: &circusv1.CallQuery{CallId: id}}}
}
func cancelCall(rid uint64, id string) *circusv1.WorkerUplink {
	return &circusv1.WorkerUplink{RequestId: rid, Payload: &circusv1.WorkerUplink_CancelCall{CancelCall: &circusv1.CancelCall{CallId: id}}}
}
func unwatch(rid uint64) *circusv1.WorkerUplink {
	return &circusv1.WorkerUplink{RequestId: rid, Payload: &circusv1.WorkerUplink_Unwatch{Unwatch: &circusv1.Unwatch{}}}
}

// receiveDownlink returns the next message on l, or fails the test.
func receiveDownlink(t *testing.T, l *wire.Link) *circusv1.WorkerDownlink {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), wait.Timeout)
	defer cancel()
	m, err := l.Receive(ctx)
	must(t, err)
	return m.(*circusv1.WorkerDownlink)
}

// closed waits until the server closes l without sending anything.
func closed(t *testing.T, l *wire.Link) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), wait.Timeout)
	defer cancel()
	if m, err := l.Receive(ctx); err == nil {
		t.Fatalf("link stayed open and sent %v", m)
	} else if ctx.Err() != nil {
		t.Fatal("link was not closed")
	}
}

// quiet asserts that nothing arrives on l for a short while.
func quiet(t *testing.T, l *wire.Link) {
	t.Helper()
	short, stop := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer stop()
	if extra, err := l.Receive(short); err == nil {
		t.Fatalf("unexpected message: %v", extra)
	} else if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}

func client(t *testing.T, f *fixture) *workerclient.Client {
	t.Helper()
	c, err := workerclient.Dial(t.Context(), workerclient.Options{Credentials: f.f})
	must(t, err)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestHandshakeAuthAndDeadlines(t *testing.T) {
	f := setup(t)
	for _, tc := range []struct {
		token  string
		status int
	}{{"", 401}, {f.daemonToken, 403}} {
		req, _ := http.NewRequest("GET", f.server.URL+"/v1/link/worker", nil)
		req.Header.Set("Authorization", "Bearer "+tc.token)
		res, err := http.DefaultClient.Do(req)
		must(t, err)
		_ = res.Body.Close()
		if res.StatusCode != tc.status {
			t.Fatal(res.Status)
		}
	}
	for _, tc := range []struct {
		m    *circusv1.WorkerUplink
		code circusv1.Reject_Code
	}{
		{&circusv1.WorkerUplink{Payload: &circusv1.WorkerUplink_Hello{Hello: &circusv1.Hello{ProtocolVersion: 99}}}, circusv1.Reject_CODE_VERSION_UNSUPPORTED},
		{&circusv1.WorkerUplink{Payload: &circusv1.WorkerUplink_Heartbeat{Heartbeat: &circusv1.Heartbeat{}}}, circusv1.Reject_CODE_BAD_HELLO},
	} {
		c := raw(t, f)
		write(t, c, tc.m)
		if got := read(t, c).GetReject().GetCode(); got != tc.code {
			t.Fatal(got)
		}
		_ = c.Close()
	}
	// A connection that never sends Hello is closed once the Hello timeout
	// elapses on the handler's clock, and not before.
	armed := f.clock.Waiters()
	c := rawAvailable(t, f)
	wait.For(t, func() bool { return f.clock.Waiters() > armed })
	f.clock.Advance(wire.HelloTimeout - time.Millisecond)
	short, stop := context.WithTimeout(t.Context(), 50*time.Millisecond)
	_, err := c.ReadMessage(short)
	stop()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("connection closed before the Hello timeout", err)
	}
	f.clock.Advance(time.Millisecond)
	ctx, cancel := context.WithTimeout(t.Context(), wait.Timeout)
	defer cancel()
	if _, err := c.ReadMessage(ctx); err == nil || ctx.Err() != nil {
		t.Fatal("Hello deadline not enforced", err)
	}
}

func TestOperationCapClosesWithoutTerminalOverload(t *testing.T) {
	f := setup(t)
	l := linked(t, f)
	for i := range workerlink.MaxOperations {
		send(t, l, query(uint64(i+1), fmt.Sprint("q", i)))
	}
	for range workerlink.MaxOperations {
		wait.Recv(t, f.svc.started)
	}
	// Cancel needs a slot like Query: one operation over the cap of any kind
	// closes the link.
	f.svc.cancel = func(context.Context, string) error {
		t.Error("over-cap Cancel reached the router")
		return nil
	}
	send(t, l, cancelCall(workerlink.MaxOperations+1, "over"))
	closed(t, l)
	ctx, cancel := context.WithTimeout(t.Context(), wait.Timeout)
	defer cancel()
	must(t, f.h.Shutdown(ctx))
	if f.svc.active.Load() != 0 {
		t.Fatal("unjoined query")
	}
}

// Request IDs must be nonzero and unique among open operations; a violation
// closes the link before the router sees the operation.
func TestRequestIDViolationsCloseLink(t *testing.T) {
	for _, tc := range []struct {
		name string
		msgs []*circusv1.WorkerUplink
	}{
		{"zero query", []*circusv1.WorkerUplink{query(0, "a")}},
		{"zero cancel", []*circusv1.WorkerUplink{cancelCall(0, "a")}},
		{"zero unwatch", []*circusv1.WorkerUplink{unwatch(0)}},
		{"zero submit", []*circusv1.WorkerUplink{{Payload: &circusv1.WorkerUplink_SubmitCall{SubmitCall: &circusv1.SubmitCall{Device: "desk", Call: &circusv1.DeviceCall{CallId: "a"}}}}}},
		{"duplicate open id", []*circusv1.WorkerUplink{query(7, "a"), query(7, "b")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t)
			var cancels atomic.Int32
			f.svc.cancel = func(context.Context, string) error { cancels.Add(1); return nil }
			l := linked(t, f)
			for i, m := range tc.msgs {
				send(t, l, m)
				if i == 0 && len(tc.msgs) == 2 {
					wait.Recv(t, f.svc.started) // the first, valid operation
				}
			}
			closed(t, l)
			select {
			case id := <-f.svc.started:
				t.Fatalf("invalid operation reached the router: %s", id)
			default:
			}
			if cancels.Load() != 0 {
				t.Fatal("invalid Cancel reached the router")
			}
		})
	}
}

// Unwatch frees its slot inline: an operation that follows it in the same
// stream at the cap is admitted. The unwatched operation stops (its router
// context ends), sends nothing, and an unknown ID is a no-op.
func TestUnwatchFreesSlotInline(t *testing.T) {
	f := setup(t)
	// The unwatched operation's goroutine finishes late, so only an inline
	// release lets the next operation in the stream find a free slot.
	f.svc.query = func(ctx context.Context, id string) (*circusv1.CallUpdate, error) {
		f.svc.started <- id
		<-ctx.Done()
		if id == "b" {
			time.Sleep(200 * time.Millisecond)
		}
		return nil, ctx.Err()
	}
	l := linked(t, f)
	for i := range 4 {
		send(t, l, query(uint64(i+1), string(rune('a'+i))))
		wait.Recv(t, f.svc.started)
	}
	send(t, l, unwatch(99)) // unknown: no-op
	send(t, l, unwatch(2))
	send(t, l, query(5, "e"))
	if got := wait.Recv(t, f.svc.started); got != "e" {
		t.Fatalf("operation after Unwatch: %q", got)
	}
	wait.For(t, func() bool { return f.svc.active.Load() == 4 })
	// The ID of an unwatched operation may be reused once it is not open.
	send(t, l, unwatch(5))
	send(t, l, query(2, "b2"))
	if got := wait.Recv(t, f.svc.started); got != "b2" {
		t.Fatalf("reused request ID: %q", got)
	}
	quiet(t, l)
}

// Every reply carries the request ID of the operation it answers, including
// two operations on the same call.
func TestRepliesCarryRequestID(t *testing.T) {
	f := setup(t)
	f.svc.query = func(_ context.Context, id string) (*circusv1.CallUpdate, error) {
		return &circusv1.CallUpdate{CallId: id, Update: &circusv1.CallUpdate_Dispatched_{Dispatched: &circusv1.CallUpdate_Dispatched{}}}, nil
	}
	l := linked(t, f)
	send(t, l, query(41, "same"))
	send(t, l, cancelCall(42, "same"))
	got := map[uint64]bool{}
	for range 2 {
		m := receiveDownlink(t, l)
		if m.GetCallUpdate().GetDispatched() == nil || m.GetCallUpdate().GetCallId() != "same" {
			t.Fatal(m)
		}
		got[m.GetRequestId()] = true
	}
	if !got[41] || !got[42] {
		t.Fatalf("request IDs %v, want 41 and 42", got)
	}
}

func TestQueryErrorMapping(t *testing.T) {
	f := setup(t)
	f.svc.query = func(_ context.Context, id string) (*circusv1.CallUpdate, error) {
		switch id {
		case "missing":
			return nil, store.ErrNotFound
		case "invalid":
			return nil, store.ErrInvalid
		case "expired":
			return nil, calls.ErrResultUnavailable
		case "offline":
			return nil, calls.ErrDeviceOffline
		default:
			return nil, calls.ErrStoreUnavailable
		}
	}
	c := client(t, f)
	for _, tc := range []struct {
		id     string
		verify func(*circusv1.CallUpdate) bool
	}{
		{"missing", func(u *circusv1.CallUpdate) bool {
			return u.GetRejected().GetCode() == circusv1.CallUpdate_Rejected_CODE_NOT_FOUND && !wire.Terminal(u)
		}},
		{"invalid", func(u *circusv1.CallUpdate) bool {
			return u.GetRejected().GetCode() == circusv1.CallUpdate_Rejected_CODE_INVALID
		}},
		{"expired", func(u *circusv1.CallUpdate) bool { return u.GetLost().GetReason() == "RESULT_UNAVAILABLE" }},
		{"offline", func(u *circusv1.CallUpdate) bool {
			return u.GetUnknown().GetReason() == "DEVICE_OFFLINE" && !wire.Terminal(u)
		}},
	} {
		u, err := c.Query(t.Context(), tc.id)
		must(t, err)
		if !tc.verify(u) {
			t.Fatalf("%s: %v", tc.id, u)
		}
		select {
		case <-c.Done():
			t.Fatalf("%s closed unrelated link operations", tc.id)
		default:
		}
	}
	if _, err := c.Query(t.Context(), "ambiguous"); err == nil {
		t.Fatal("ambiguous store error falsely advertised safe retry")
	}
}

func TestQueryOverloadedAndSendFailureCloseLink(t *testing.T) {
	for _, tc := range []struct {
		name  string
		query func(context.Context, string) (*circusv1.CallUpdate, error)
	}{
		{"overloaded", func(context.Context, string) (*circusv1.CallUpdate, error) { return nil, calls.ErrOverloaded }},
		{"send failure", func(context.Context, string) (*circusv1.CallUpdate, error) {
			return &circusv1.CallUpdate{CallId: "victim", Update: &circusv1.CallUpdate_Result{Result: &circusv1.DeviceResult{Output: &circusv1.ToolOutput{Content: []*circusv1.ContentBlock{{Block: &circusv1.ContentBlock_Text{Text: strings.Repeat("x", 2<<20)}}}}}}}, nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t)
			f.svc.query = tc.query
			c := client(t, f)
			queryCtx, queryCancel := context.WithTimeout(t.Context(), wait.Timeout)
			defer queryCancel()
			if _, err := c.Query(queryCtx, "victim"); err == nil {
				t.Fatal("operation returned a terminal update or nil error")
			}
			wait.Recv(t, c.Done())
		})
	}
}

// Cancel is answered by exactly one update: the call's state read after
// the intent was recorded, or the rejection of the cancellation itself.
func TestCancelReply(t *testing.T) {
	f := setup(t)
	var queried sync.Map
	f.svc.cancel = func(_ context.Context, id string) error {
		switch id {
		case "missing":
			return store.ErrNotFound
		case "bad-id":
			return store.ErrInvalid
		case "ambiguous":
			return calls.ErrStoreUnavailable
		}
		return nil
	}
	f.svc.query = func(_ context.Context, id string) (*circusv1.CallUpdate, error) {
		queried.Store(id, true)
		u := &circusv1.CallUpdate{CallId: id}
		switch id {
		case "accepted":
			u.Update = &circusv1.CallUpdate_Cancelled_{Cancelled: &circusv1.CallUpdate_Cancelled{}}
		case "running":
			u.Update = &circusv1.CallUpdate_Dispatched_{Dispatched: &circusv1.CallUpdate_Dispatched{}}
		case "finished":
			u.Update = &circusv1.CallUpdate_Result{Result: &circusv1.DeviceResult{}}
		case "offline":
			return nil, calls.ErrDeviceOffline
		}
		return u, nil
	}
	c := client(t, f)
	for _, tc := range []struct {
		id     string
		verify func(*circusv1.CallUpdate) bool
	}{
		{"accepted", func(u *circusv1.CallUpdate) bool { return u.GetCancelled() != nil }},
		{"running", func(u *circusv1.CallUpdate) bool { return u.GetDispatched() != nil }},
		{"finished", func(u *circusv1.CallUpdate) bool { return u.GetResult() != nil }},
		{"offline", func(u *circusv1.CallUpdate) bool { return u.GetUnknown().GetReason() == "DEVICE_OFFLINE" }},
		{"missing", func(u *circusv1.CallUpdate) bool {
			return u.GetRejected().GetCode() == circusv1.CallUpdate_Rejected_CODE_NOT_FOUND && !wire.Terminal(u)
		}},
		{"bad-id", func(u *circusv1.CallUpdate) bool {
			return u.GetRejected().GetCode() == circusv1.CallUpdate_Rejected_CODE_INVALID
		}},
	} {
		u, err := c.Cancel(wait.Ctx(t), tc.id)
		must(t, err)
		if u.GetCallId() != tc.id || !tc.verify(u) {
			t.Fatalf("%s: %v", tc.id, u)
		}
	}
	for _, id := range []string{"missing", "bad-id"} {
		if _, ok := queried.Load(id); ok {
			t.Fatalf("rejected Cancel of %s still queried the call", id)
		}
	}
	if _, err := c.Cancel(t.Context(), "ambiguous"); err == nil {
		t.Fatal("ambiguous cancellation failure was answered instead of closing the link")
	}
	wait.Recv(t, c.Done())
}

// Exactly one reply per Cancel, on the raw protocol too, and nothing more.
func TestCancelSendsExactlyOneReply(t *testing.T) {
	f := setup(t)
	f.svc.query = func(_ context.Context, id string) (*circusv1.CallUpdate, error) {
		return &circusv1.CallUpdate{CallId: id, Update: &circusv1.CallUpdate_Cancelled_{Cancelled: &circusv1.CallUpdate_Cancelled{}}}, nil
	}
	l := linked(t, f)
	send(t, l, cancelCall(3, "accepted"))
	m := receiveDownlink(t, l)
	if m.GetRequestId() != 3 || m.GetCallUpdate().GetCancelled() == nil {
		t.Fatal(m)
	}
	quiet(t, l)
}

func status(t *testing.T, f *fixture, token string) int {
	t.Helper()
	req, _ := http.NewRequest("GET", f.server.URL+"/v1/link/worker", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	must(t, err)
	_ = res.Body.Close()
	// The control plane marks its refusals: "revoked" (naming the identity)
	// for a revoked credential, "rejected" for any other.
	wantMarker := res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden
	got := res.Header.Get("Circus-Auth")
	revoked := got == "revoked" && res.StatusCode == http.StatusForbidden && res.Header.Get("Circus-Principal") != ""
	if (got == "rejected" || revoked) != wantMarker {
		t.Errorf("worker link HTTP %d: unexpected Circus-Auth %q", res.StatusCode, got)
	}
	return res.StatusCode
}

// Worker links count against both the credential and the Web user as well
// as the overall limit, after authentication.
func TestConnectionCaps(t *testing.T) {
	t.Run("per credential", func(t *testing.T) {
		f := setup(t)
		for range workerlink.MaxPerCredential {
			raw(t, f)
		}
		if got := status(t, f, f.f.Token); got != http.StatusServiceUnavailable {
			t.Fatalf("fifth connection of one credential: %d", got)
		}
		linkedAs(t, f, otherUserWorkerToken(t, f))
	})
	t.Run("per user", func(t *testing.T) {
		f := setup(t)
		for range 2 {
			raw(t, f)
			rawAs(t, f, f.otherToken)
		}
		if got := status(t, f, f.otherToken); got != http.StatusServiceUnavailable {
			t.Fatalf("fifth connection across two credentials of the same user: %d", got)
		}
		linkedAs(t, f, otherUserWorkerToken(t, f))
	})
	t.Run("total", func(t *testing.T) {
		f := setup(t, func(c *workerlink.Config) { c.MaxConnections = 2 })
		raw(t, f)
		rawAs(t, f, f.otherToken)
		for _, token := range []string{f.f.Token, f.otherToken} {
			if got := status(t, f, token); got != http.StatusServiceUnavailable {
				t.Fatalf("third authenticated connection: %d", got)
			}
		}
		if got := status(t, f, ""); got != http.StatusUnauthorized {
			t.Fatalf("unauthenticated request consumed a slot check: %d", got)
		}
	})
	t.Run("default", func(t *testing.T) {
		if _, err := workerlink.New(workerlink.Config{Auth: &authStub{}, Router: &service{}, InstanceID: "i", MaxConnections: -1}); err == nil {
			t.Fatal("negative MaxConnections accepted")
		}
		f := setup(t)
		for range workerlink.MaxPerCredential {
			raw(t, f)
		}
		token := otherUserWorkerToken(t, f)
		for range workerlink.DefaultMaxConnections - workerlink.MaxPerCredential {
			rawAs(t, f, token)
		}
		if got := status(t, f, token); got != http.StatusServiceUnavailable {
			t.Fatalf("connection beyond the default total: %d", got)
		}
	})
}

func otherUserWorkerToken(t *testing.T, f *fixture) string {
	t.Helper()
	ctx := t.Context()
	now := time.Now()
	u := store.User{ID: "other", Email: "other@example.com", Role: store.RoleUser, CreatedAt: now.UnixMilli()}
	must(t, f.st.CreateUser(ctx, u))
	token, _, err := auth.CreateEnrollToken(ctx, f.st, u.ID, store.KindWorker, time.Hour, now)
	must(t, err)
	enrolled, err := auth.Enroll(ctx, f.st, auth.EnrollRequest{Token: token, Kind: store.KindWorker, Name: "worker", MachineID: "m"}, now)
	must(t, err)
	return enrolled.Token
}

// Waiting for the router (and the daemon behind it) holds no global permit:
// slow Queries on one connection delay neither a Cancel reply on the same
// connection nor another connection's Query.
func TestSlowQueriesDoNotDelayReplies(t *testing.T) {
	f := setup(t)
	f.svc.query = func(ctx context.Context, id string) (*circusv1.CallUpdate, error) {
		if strings.HasPrefix(id, "slow") {
			f.svc.started <- id
			<-ctx.Done() // the daemon never answers
			return nil, ctx.Err()
		}
		return &circusv1.CallUpdate{CallId: id, Update: &circusv1.CallUpdate_Dispatched_{Dispatched: &circusv1.CallUpdate_Dispatched{}}}, nil
	}
	a, b := linked(t, f), linkedAs(t, f, f.otherToken)
	for i := range 2 * workerlink.PushPermits {
		send(t, a, query(uint64(i+1), fmt.Sprint("slow", i)))
	}
	for range 2 * workerlink.PushPermits {
		wait.Recv(t, f.svc.started)
	}
	send(t, a, cancelCall(100, "fast-cancel"))
	if m := receiveDownlink(t, a); m.GetRequestId() != 100 || m.GetCallUpdate().GetDispatched() == nil {
		t.Fatalf("Cancel reply behind slow Queries: %v", m)
	}
	send(t, b, query(1, "fast-query"))
	if m := receiveDownlink(t, b); m.GetRequestId() != 1 || m.GetCallUpdate().GetDispatched() == nil {
		t.Fatalf("another connection's Query behind slow Queries: %v", m)
	}
}

// fakeSub is a subscription with one queued update of the given charged
// cost. After it, Wait and Next block like the router's until ctx ends;
// nextErr makes Next fail instead, and afterErr makes the subscription end
// with that error once the update was taken (as a router detach would).
type fakeSub struct {
	u                 *circusv1.CallUpdate
	cost              int64
	nextErr, afterErr error
	mu                sync.Mutex
	taken             bool
	onTake            func()
}

func (s *fakeSub) Wait(ctx context.Context) (int64, error) {
	s.mu.Lock()
	ready, after := !s.taken, s.taken && s.afterErr != nil
	s.mu.Unlock()
	if ready {
		return s.cost, nil
	}
	if after {
		return 0, nil
	}
	<-ctx.Done()
	return 0, ctx.Err()
}
func (s *fakeSub) Next(ctx context.Context) (*circusv1.CallUpdate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.taken {
		if s.afterErr != nil {
			return nil, s.afterErr
		}
		s.mu.Unlock() // like the router: block until an update or ctx
		<-ctx.Done()
		s.mu.Lock()
		return nil, ctx.Err()
	}
	s.taken = true
	if s.onTake != nil {
		s.onTake()
	}
	if s.nextErr != nil {
		return nil, s.nextErr
	}
	return s.u, nil
}
func (s *fakeSub) Close() error   { return nil }
func (s *fakeSub) wasTaken() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.taken }

func accepted(id string) *circusv1.CallUpdate {
	return &circusv1.CallUpdate{CallId: id, Update: &circusv1.CallUpdate_Accepted_{Accepted: &circusv1.CallUpdate_Accepted{}}}
}
func submitMsg(rid uint64, id string) *circusv1.WorkerUplink {
	return &circusv1.WorkerUplink{RequestId: rid, Payload: &circusv1.WorkerUplink_SubmitCall{SubmitCall: &circusv1.SubmitCall{Device: "desk", Call: &circusv1.DeviceCall{CallId: id}}}}
}

// subs lets a test choose each Submit's fake subscription by call ID.
type subs struct {
	mu sync.Mutex
	m  map[string]*fakeSub
}

func useSubs(f *fixture, make func(id string) *fakeSub) *subs {
	all := &subs{m: map[string]*fakeSub{}}
	workerlink.SetSubmit(f.h, func(_ context.Context, _, _ string, call *circusv1.DeviceCall) (workerlink.Subscription, error) {
		s := make(call.GetCallId())
		all.mu.Lock()
		all.m[call.GetCallId()] = s
		all.mu.Unlock()
		return s, nil
	})
	return all
}
func (a *subs) get(id string) *fakeSub { a.mu.Lock(); defer a.mu.Unlock(); return a.m[id] }

const large = workerlink.SmallUpdateBytes + 1

// The push permits are global: across connections, at most that many large
// updates are taken from subscriptions and not yet handed to the link.
// Pushes waiting for a permit hold only their slot: the receive loop keeps
// reading, Unwatch of a waiting push frees its slot inline (a Submit at the
// cap right after it is admitted), and Query replies still flow.
func TestPushPermitsBoundAcrossConnections(t *testing.T) {
	f := setup(t)
	permits := workerlink.PushPermits
	var held, peak atomic.Int32
	gate := make(chan struct{})
	blocked := make(chan struct{}, 128)
	useSubs(f, func(id string) *fakeSub {
		return &fakeSub{taken: strings.HasPrefix(id, "idle"), u: accepted(id), cost: large, onTake: func() {
			n := held.Add(1)
			for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
			}
		}}
	})
	workerlink.SetBeforeSend(f.h, func(ctx context.Context, u *circusv1.CallUpdate) {
		if u.GetAccepted() == nil {
			return // only pushes are held back
		}
		blocked <- struct{}{}
		select {
		case <-gate:
		case <-ctx.Done():
		}
		held.Add(-1)
	})
	f.svc.query = func(_ context.Context, id string) (*circusv1.CallUpdate, error) {
		return &circusv1.CallUpdate{CallId: id, Update: &circusv1.CallUpdate_Dispatched_{Dispatched: &circusv1.CallUpdate_Dispatched{}}}, nil
	}
	submit := submitMsg
	links := []*wire.Link{linked(t, f), linked(t, f), linkedAs(t, f, f.otherToken)}
	// Idle subscriptions (no update yet) on the first link must not take
	// permits: only a ready push competes for one.
	for j := range permits {
		send(t, links[0], submit(uint64(100+j), fmt.Sprint("idle", j)))
	}
	for i, l := range links {
		for j := range 2 {
			send(t, l, submit(uint64(j+1), fmt.Sprint("c", i, j)))
		}
	}
	for range permits {
		wait.Recv(t, blocked)
	}
	select {
	case <-blocked:
		t.Fatalf("a push beyond the %d global permits took an update", permits)
	case <-time.After(50 * time.Millisecond):
	}
	if got := peak.Load(); got != int32(permits) {
		t.Fatalf("peak updates taken but not sent %d, want %d", got, permits)
	}
	// Fill the third connection to the cap with pushes that must wait.
	l := links[2]
	for rid := uint64(3); rid <= workerlink.MaxOperations; rid++ {
		send(t, l, submit(rid, fmt.Sprint("fill", rid)))
	}
	send(t, l, unwatch(workerlink.MaxOperations))
	send(t, l, submit(workerlink.MaxOperations+1, "after-unwatch")) // at the cap again
	send(t, l, unwatch(workerlink.MaxOperations-1))
	send(t, l, query(workerlink.MaxOperations+2, "q"))
	if m := receiveDownlink(t, l); m.GetRequestId() != workerlink.MaxOperations+2 || m.GetCallUpdate().GetDispatched() == nil {
		t.Fatalf("reply while pushes wait for permits: %v", m)
	}
	if got := peak.Load(); got != int32(permits) {
		t.Fatalf("peak updates taken but not sent %d, want %d", got, permits)
	}
	close(gate)
}

func TestIdleRevocationClosesDefinitively(t *testing.T) {
	f := setup(t)
	c := client(t, f)
	u, err := f.st.GetUserByEmail(t.Context(), "owner@example.com")
	must(t, err)
	_, _, err = auth.RevokeByName(t.Context(), f.st, store.KindWorker, u.ID, "desk", time.Now())
	must(t, err)
	// The first recheck after the revoke closes the link (auth's Watch tests
	// pin its timing).
	advanceUntilDone(t, f, c, time.Second)
	if got := status(t, f, f.f.Token); got != http.StatusForbidden {
		t.Fatalf("revoked worker credential: HTTP %d", got)
	}
}

func TestRecheckStoreErrorsKeepLinkUntilStalenessBound(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(map[bool]string{false: "store error", true: "recheck timeout"}[timeout], func(t *testing.T) {
			const stale = 5 * time.Second
			f := setup(t, func(c *workerlink.Config) {
				c.Recheck = auth.RecheckPolicy{Interval: 250 * time.Millisecond, Timeout: 250 * time.Millisecond, MaxStale: stale}
			})
			f.auth.setValid(func(ctx context.Context, _ string) (bool, error) {
				if timeout {
					<-ctx.Done()
					return false, ctx.Err()
				}
				return false, store.ErrBusy
			})
			start := f.clock.Now()
			c := client(t, f)
			advanceUntilDone(t, f, c, 100*time.Millisecond)
			if elapsed := f.clock.Now().Sub(start); elapsed < stale {
				t.Fatalf("transient revalidation failure closed the link after %s, before the staleness window", elapsed)
			}
			if f.auth.checks.Load() == 0 {
				t.Fatal("link closed without a recheck")
			}
		})
	}
}

func TestShutdownReturnsAtContextWithStuckOperation(t *testing.T) {
	f := setup(t)
	release := make(chan struct{})
	f.svc.query = func(context.Context, string) (*circusv1.CallUpdate, error) {
		f.svc.started <- "stuck"
		<-release
		return nil, errors.New("released")
	}
	l := linked(t, f)
	send(t, l, query(1, "stuck"))
	wait.Recv(t, f.svc.started)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := f.h.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown = %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("Shutdown ignored its context")
	}
	close(release)
	ctx, cancel = context.WithTimeout(t.Context(), wait.Timeout)
	defer cancel()
	must(t, f.h.Shutdown(ctx))
}

func TestUnknownIsNotTerminal(t *testing.T) {
	if wire.Terminal(&circusv1.CallUpdate{Update: &circusv1.CallUpdate_Unknown_{Unknown: &circusv1.CallUpdate_Unknown{}}}) {
		t.Fatal("unknown is terminal")
	}
}

// advanceUntilDone advances the handler's clock by step until c's link
// closes. Only the recheck loop runs on that clock once the link is up.
func advanceUntilDone(t *testing.T, f *fixture, c *workerclient.Client, step time.Duration) {
	t.Helper()
	wait.For(t, func() bool {
		select {
		case <-c.Done():
			return true
		default:
			f.clock.Advance(step)
			return false
		}
	})
}

// Small updates never wait for push permits: with every permit held by large
// sends stuck on a saturated pool, a connection with only small updates
// still gets them at once, so its router subscriptions cannot stall.
func TestSmallUpdatesBypassPushPermits(t *testing.T) {
	f := setup(t)
	gate := make(chan struct{})
	defer close(gate)
	blocked := make(chan struct{}, 16)
	useSubs(f, func(id string) *fakeSub {
		cost := int64(workerlink.SmallUpdateBytes)
		if strings.HasPrefix(id, "large") {
			cost = large
		}
		return &fakeSub{u: accepted(id), cost: cost}
	})
	workerlink.SetBeforeSend(f.h, func(ctx context.Context, u *circusv1.CallUpdate) {
		if strings.HasPrefix(u.GetCallId(), "large") {
			blocked <- struct{}{}
			select {
			case <-gate:
			case <-ctx.Done():
			}
		}
	})
	a, b, c := linked(t, f), linked(t, f), linkedAs(t, f, f.otherToken)
	for i := range workerlink.PushPermits + 2 {
		send(t, []*wire.Link{a, b}[i%2], submitMsg(uint64(i+1), fmt.Sprint("large", i)))
	}
	for range workerlink.PushPermits {
		wait.Recv(t, blocked)
	}
	for i := range 3 {
		send(t, c, submitMsg(uint64(i+1), fmt.Sprint("small", i)))
	}
	for range 3 {
		if m := receiveDownlink(t, c); m.GetCallUpdate().GetAccepted() == nil {
			t.Fatalf("small update: %v", m)
		}
	}
}

// The last update of an operation (a Query answer, a Submit's terminal
// update) becomes visible only after its slot was released: a client at the
// cap may start a new operation the moment that update arrives, while the
// server has not even returned from sending it.
func TestLastUpdateFollowsSlotRelease(t *testing.T) {
	for _, kind := range []string{"query", "submit", "failure", "not-found"} {
		t.Run(kind, func(t *testing.T) {
			f := setup(t)
			f.svc.query = func(ctx context.Context, id string) (*circusv1.CallUpdate, error) {
				if id == "answer" && kind == "not-found" { // the mapped router error is the answer
					return nil, store.ErrNotFound
				}
				if id == "answer" {
					return &circusv1.CallUpdate{CallId: id, Update: &circusv1.CallUpdate_Dispatched_{Dispatched: &circusv1.CallUpdate_Dispatched{}}}, nil
				}
				f.svc.started <- id
				<-ctx.Done()
				return nil, ctx.Err()
			}
			useSubs(f, func(id string) *fakeSub {
				s := &fakeSub{u: &circusv1.CallUpdate{CallId: id, Update: &circusv1.CallUpdate_Result{Result: &circusv1.DeviceResult{}}}, cost: 1024}
				if kind == "failure" { // the mapped subscription error replaces the last update
					s.nextErr = store.ErrInvalid
				}
				return s
			})
			release := make(chan struct{})
			workerlink.SetAfterSend(f.h, func(ctx context.Context, u *circusv1.CallUpdate) {
				if u.GetCallId() == "answer" {
					select { // hold the sender until the client has acted on it
					case <-release:
					case <-ctx.Done():
					}
				}
			})
			l := linked(t, f)
			for i := range workerlink.MaxOperations - 1 {
				send(t, l, query(uint64(i+1), fmt.Sprint("hold", i)))
			}
			for range workerlink.MaxOperations - 1 {
				wait.Recv(t, f.svc.started)
			}
			if kind == "query" || kind == "not-found" {
				send(t, l, query(workerlink.MaxOperations, "answer"))
			} else {
				send(t, l, submitMsg(workerlink.MaxOperations, "answer"))
			}
			if m := receiveDownlink(t, l); m.GetRequestId() != workerlink.MaxOperations {
				t.Fatal(m)
			}
			send(t, l, query(workerlink.MaxOperations+1, "next"))
			if got := wait.Recv(t, f.svc.started); got != "next" {
				t.Fatalf("operation after the last update: %q", got)
			}
			close(release)
		})
	}
}

// A subscription error releases its push permit: more failing large pushes
// than there are permits leave every permit usable afterwards.
func TestPushPermitReleasedWhenNextFails(t *testing.T) {
	f := setup(t)
	useSubs(f, func(id string) *fakeSub {
		s := &fakeSub{u: accepted(id), cost: large}
		if strings.HasPrefix(id, "fail") {
			s.nextErr = store.ErrInvalid // mapped to Rejected INVALID; the link stays
		}
		return s
	})
	l := linked(t, f)
	for i := range 2 * workerlink.PushPermits {
		send(t, l, submitMsg(uint64(i+1), fmt.Sprint("fail", i)))
		if m := receiveDownlink(t, l); m.GetCallUpdate().GetRejected().GetCode() != circusv1.CallUpdate_Rejected_CODE_INVALID {
			t.Fatalf("failing push %d: %v", i, m)
		}
	}
	for i := range workerlink.PushPermits {
		send(t, l, submitMsg(uint64(100+i), fmt.Sprint("ok", i)))
	}
	for range workerlink.PushPermits {
		if m := receiveDownlink(t, l); m.GetCallUpdate().GetAccepted() == nil {
			t.Fatalf("large push after failures: %v", m)
		}
	}
}

// An unwatched push stops: one waiting for a permit never takes the permit
// or its update, and one caught mid-send sends nothing and does not close
// the link when its subscription then ends (as the router's does when the
// operation's context is cancelled).
func TestUnwatchStopsWaitingAndSendingPushes(t *testing.T) {
	f := setup(t)
	f.svc.query = func(_ context.Context, id string) (*circusv1.CallUpdate, error) {
		return &circusv1.CallUpdate{CallId: id, Update: &circusv1.CallUpdate_Dispatched_{Dispatched: &circusv1.CallUpdate_Dispatched{}}}, nil
	}
	all := useSubs(f, func(id string) *fakeSub {
		if id == "mid-send" {
			return &fakeSub{u: accepted(id), cost: 1024, afterErr: calls.ErrClosed}
		}
		return &fakeSub{u: accepted(id), cost: large}
	})
	gate := make(chan struct{})
	blocked := make(chan string, 16)
	workerlink.SetBeforeSend(f.h, func(ctx context.Context, u *circusv1.CallUpdate) {
		if id := u.GetCallId(); strings.HasPrefix(id, "hold") || id == "mid-send" {
			blocked <- id
			select {
			case <-gate:
			case <-ctx.Done():
			}
		}
	})
	l, holder := linked(t, f), linked(t, f)
	for i := range workerlink.PushPermits {
		send(t, holder, submitMsg(uint64(i+1), fmt.Sprint("hold", i)))
	}
	for range workerlink.PushPermits {
		wait.Recv(t, blocked)
	}
	send(t, l, submitMsg(1, "waiting"))
	send(t, l, submitMsg(2, "mid-send"))
	if got := wait.Recv(t, blocked); got != "mid-send" {
		t.Fatal(got)
	}
	send(t, l, unwatch(1))
	send(t, l, unwatch(2))
	send(t, l, submitMsg(3, "later"))
	send(t, l, query(4, "alive")) // processed after both Unwatches
	if m := receiveDownlink(t, l); m.GetRequestId() != 4 {
		t.Fatalf("expected only the Query reply, got %v", m)
	}
	close(gate)
	if m := receiveDownlink(t, l); m.GetRequestId() != 3 || m.GetCallUpdate().GetCallId() != "later" {
		t.Fatalf("an unwatched push was sent: %v", m)
	}
	quiet(t, l)
	if all.get("waiting").wasTaken() {
		t.Fatal("an unwatched push took its update after waiting for a permit")
	}
	send(t, l, query(5, "still-alive"))
	if m := receiveDownlink(t, l); m.GetRequestId() != 5 {
		t.Fatalf("link after Unwatch mid-send: %v", m)
	}
}

// A Submit the router refuses is answered off the receive loop: while that
// answer waits on the send path, later messages are still read and served.
func TestSubmitFailureDoesNotBlockReceiveLoop(t *testing.T) {
	f := setup(t)
	f.svc.query = func(_ context.Context, id string) (*circusv1.CallUpdate, error) {
		return &circusv1.CallUpdate{CallId: id, Update: &circusv1.CallUpdate_Dispatched_{Dispatched: &circusv1.CallUpdate_Dispatched{}}}, nil
	}
	workerlink.SetSubmit(f.h, func(context.Context, string, string, *circusv1.DeviceCall) (workerlink.Subscription, error) {
		return nil, store.ErrInvalid
	})
	gate := make(chan struct{})
	workerlink.SetBeforeSend(f.h, func(ctx context.Context, u *circusv1.CallUpdate) {
		if u.GetRejected() != nil {
			select {
			case <-gate:
			case <-ctx.Done():
			}
		}
	})
	l := linked(t, f)
	send(t, l, submitMsg(1, "refused"))
	send(t, l, query(2, "q"))
	if m := receiveDownlink(t, l); m.GetRequestId() != 2 {
		t.Fatalf("Query behind a blocked Submit rejection: %v", m)
	}
	close(gate)
	if m := receiveDownlink(t, l); m.GetRequestId() != 1 || m.GetCallUpdate().GetRejected().GetCode() != circusv1.CallUpdate_Rejected_CODE_INVALID {
		t.Fatal(m)
	}
}

func TestCancelReplyWaitsForRecordedIntent(t *testing.T) {
	f := setup(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var released sync.Once
	resume := func() { released.Do(func() { close(release) }) }
	defer resume()
	var recorded atomic.Bool
	f.svc.cancel = func(ctx context.Context, _ string) error {
		close(entered)
		select {
		case <-release:
			recorded.Store(true)
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	queried := make(chan struct{}, 1)
	f.svc.query = func(_ context.Context, id string) (*circusv1.CallUpdate, error) {
		if !recorded.Load() {
			t.Error("Cancel reply state read before intent was recorded")
		}
		queried <- struct{}{}
		return &circusv1.CallUpdate{CallId: id, Update: &circusv1.CallUpdate_Dispatched_{Dispatched: &circusv1.CallUpdate_Dispatched{}}}, nil
	}
	l := linked(t, f)
	send(t, l, cancelCall(3, "running"))
	wait.Recv(t, entered)
	quiet(t, l)
	select {
	case <-queried:
		t.Fatal("Cancel queried its reply before returning")
	default:
	}
	resume()
	m := receiveDownlink(t, l)
	if m.GetRequestId() != 3 || m.GetCallUpdate().GetDispatched() == nil {
		t.Fatal(m)
	}
	wait.Recv(t, queried)
}

// reasonLog records whether any log record written so far contains want.
// slog handlers write each record with one Write call.
type reasonLog struct {
	mu    sync.Mutex
	want  []byte
	found chan struct{}
}

func (w *reasonLog) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.found != nil && bytes.Contains(p, w.want) {
		close(w.found)
		w.found = nil
	}
	return len(p), nil
}

// Session dispatch is deliberately absent until the session service is wired
// in: every new uplink payload takes the unexpected-payload close.
func TestSessionPayloadsCloseLinkAsUnexpectedPayload(t *testing.T) {
	for name, m := range map[string]*circusv1.WorkerUplink{
		"attach":        {RequestId: 1, Payload: &circusv1.WorkerUplink_Attach{Attach: &circusv1.Attach{MaxSessions: 1}}},
		"session":       {RequestId: 1, Payload: &circusv1.WorkerUplink_Session{Session: &circusv1.SessionRequest{SessionId: "session", Epoch: 1, Op: &circusv1.SessionRequest_TakeInputs{TakeInputs: &circusv1.TakeInputs{}}}}},
		"resume_leases": {RequestId: 1, Payload: &circusv1.WorkerUplink_ResumeLeases{ResumeLeases: &circusv1.ResumeLeases{Leases: []*circusv1.ResumeLeases_Held{{SessionId: "session", Epoch: 1}}}}},
		"list_devices":  {RequestId: 1, Payload: &circusv1.WorkerUplink_ListDevices{ListDevices: &circusv1.ListDevices{}}},
	} {
		t.Run(name, func(t *testing.T) {
			found := make(chan struct{})
			logs := &reasonLog{want: []byte(`"reason":"unexpected payload"`), found: found}
			f := setup(t, func(cfg *workerlink.Config) {
				cfg.Logger = slog.New(slog.NewJSONHandler(logs, nil))
			})
			l := linked(t, f)
			send(t, l, m)
			closed(t, l)
			wait.Recv(t, (<-chan struct{})(found))
		})
	}
}

type metadataStoreFunc func(context.Context, store.PrincipalKind, string, store.HelloMetadata) (bool, error)

func (f metadataStoreFunc) UpdateHelloMetadata(ctx context.Context, kind store.PrincipalKind, id string, metadata store.HelloMetadata) (bool, error) {
	return f(ctx, kind, id, metadata)
}

// A running Hello metadata update does not delay its connection's teardown
// (the peer slot is released), but Shutdown waits for it.
func TestShutdownWaitsForHelloMetadataUpdate(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var finished atomic.Bool
	f := setup(t, func(c *workerlink.Config) {
		c.MetadataStore = metadataStoreFunc(func(context.Context, store.PrincipalKind, string, store.HelloMetadata) (bool, error) {
			close(started)
			<-release
			finished.Store(true)
			return true, nil
		})
	})
	conn := raw(t, f)
	write(t, conn, &circusv1.WorkerUplink{Payload: &circusv1.WorkerUplink_Hello{Hello: &circusv1.Hello{ProtocolVersion: wire.ProtocolVersion, ClientVersion: "0.2.0"}}})
	wait.Recv(t, started)
	_ = conn.Close()
	wait.For(t, func() bool { return workerlink.Peers(f.h) == 0 })
	shutdown := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), wait.Timeout)
		defer cancel()
		shutdown <- f.h.Shutdown(ctx)
	}()
	select {
	case err := <-shutdown:
		close(release)
		t.Fatalf("Shutdown returned while the metadata update was running (err %v)", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := wait.Recv(t, shutdown); err != nil {
		t.Fatal(err)
	}
	if !finished.Load() {
		t.Fatal("metadata update still running after Shutdown returned")
	}
}
