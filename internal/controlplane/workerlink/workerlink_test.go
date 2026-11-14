package workerlink_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
	svc         *service
	auth        *authStub
	clock       *clock.Fake
}

func setup(t *testing.T, configure ...func(*workerlink.Config)) *fixture {
	t.Helper()
	baseline := goleak.IgnoreCurrent()
	t.Cleanup(func() { goleak.VerifyNone(t, baseline) })
	st, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "db"), sqlite.Options{})
	must(t, err)
	t.Cleanup(func() { _ = st.Close() })
	u, err := auth.EnsureUser(t.Context(), st, "admin", time.Now())
	must(t, err)
	enroll := func(k store.PrincipalKind) auth.Enrollment {
		token, _, err := auth.CreateEnrollToken(t.Context(), st, u.ID, k, time.Hour, time.Now())
		must(t, err)
		e, err := auth.Enroll(t.Context(), st, auth.EnrollRequest{Token: token, Kind: k, Name: "desk", MachineID: "m", Hostname: "host", OS: "linux", Arch: "amd64"}, time.Now())
		must(t, err)
		return e
	}
	worker, daemon := enroll(store.KindWorker), enroll(store.KindDevice)
	authenticator := &authStub{Authenticator: auth.NewAuthenticator(st, auth.Options{})}
	authenticator.valid = authenticator.Authenticator.Valid
	svc := &service{started: make(chan string, 16)}
	clk := clock.New()
	cfg := workerlink.Config{
		Auth:       authenticator,
		Router:     svc,
		InstanceID: "instance",
		Clock:      clk,
		Wire:       wire.Options{MaxMessageBytes: 1 << 20},
	}
	for _, fn := range configure {
		fn(&cfg)
	}
	h, err := workerlink.New(cfg)
	must(t, err)
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
	return &fixture{h: h, server: server, st: st, f: credfile.File{ControlPlaneURL: server.URL, Role: credfile.RoleWorker, PrincipalID: worker.Identity.ID, Token: worker.Token}, daemonToken: daemon.Token, svc: svc, auth: authenticator, clock: clk}
}
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func raw(t *testing.T, f *fixture) *wire.WebSocket {
	t.Helper()
	c, _, err := wire.DialWebSocket(t.Context(), "ws"+strings.TrimPrefix(f.server.URL, "http")+"/v1/link/worker", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": []string{"Bearer " + f.f.Token}}})
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
func linked(t *testing.T, f *fixture) *wire.Link {
	t.Helper()
	conn := raw(t, f)
	write(t, conn, &circusv1.WorkerUplink{Payload: &circusv1.WorkerUplink_Hello{Hello: &circusv1.Hello{ProtocolVersion: 1}}})
	welcome := read(t, conn).GetWelcome()
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
	for i := range 4 {
		id := string(rune('a' + i))
		send(t, l, &circusv1.WorkerUplink{Payload: &circusv1.WorkerUplink_CallQuery{CallQuery: &circusv1.CallQuery{CallId: id}}})
		if got := wait.Recv(t, f.svc.started); got != id {
			t.Fatalf("query %q, want %q", got, id)
		}
	}
	cancelled := make(chan struct{})
	f.svc.cancel = func(context.Context, string) error {
		close(cancelled)
		return nil
	}
	send(t, l, &circusv1.WorkerUplink{Payload: &circusv1.WorkerUplink_CancelCall{CancelCall: &circusv1.CancelCall{CallId: "victim"}}})
	wait.Recv(t, cancelled)
	send(t, l, &circusv1.WorkerUplink{Payload: &circusv1.WorkerUplink_CallQuery{CallQuery: &circusv1.CallQuery{CallId: "fifth"}}})
	ctx, cancel := context.WithTimeout(t.Context(), wait.Timeout)
	defer cancel()
	if m, err := l.Receive(ctx); err == nil {
		t.Fatalf("overload produced an update instead of closing: %v", m)
	}
	select {
	case id := <-f.svc.started:
		t.Fatalf("fifth operation reached the router: %s", id)
	default:
	}
	must(t, f.h.Shutdown(ctx))
	if f.svc.active.Load() != 0 {
		t.Fatal("unjoined query")
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

func TestCancelSendThenQueryAndNotFound(t *testing.T) {
	f := setup(t)
	f.svc.cancel = func(_ context.Context, id string) error {
		if id == "missing" {
			return store.ErrNotFound
		}
		return nil
	}
	f.svc.query = func(_ context.Context, id string) (*circusv1.CallUpdate, error) {
		if id == "missing" {
			return nil, store.ErrNotFound
		}
		return &circusv1.CallUpdate{CallId: id, Update: &circusv1.CallUpdate_Cancelled_{Cancelled: &circusv1.CallUpdate_Cancelled{}}}, nil
	}
	c := client(t, f)
	must(t, c.Cancel(t.Context(), "accepted"))
	u, err := c.Query(t.Context(), "accepted")
	must(t, err)
	if u.GetCancelled() == nil {
		t.Fatal(u)
	}
	must(t, c.Cancel(t.Context(), "missing"))
	u, err = c.Query(t.Context(), "missing")
	must(t, err)
	if u.GetRejected().GetCode() != circusv1.CallUpdate_Rejected_CODE_NOT_FOUND || wire.Terminal(u) {
		t.Fatal(u)
	}
	select {
	case <-c.Done():
		t.Fatal("not-found cancel closed link")
	default:
	}
}

func TestCancelUnknownOrInvalidSendsNothingAndKeepsLink(t *testing.T) {
	for _, tc := range []struct {
		name string
		id   string
		err  error
	}{
		{"unknown", "missing", store.ErrNotFound},
		{"invalid", "bad id!", store.ErrInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t)
			cancelled := make(chan struct{})
			f.svc.cancel = func(context.Context, string) error {
				close(cancelled)
				return tc.err
			}
			f.svc.query = func(_ context.Context, id string) (*circusv1.CallUpdate, error) {
				return &circusv1.CallUpdate{CallId: id, Update: &circusv1.CallUpdate_Accepted_{Accepted: &circusv1.CallUpdate_Accepted{}}}, nil
			}
			l := linked(t, f)
			send(t, l, &circusv1.WorkerUplink{Payload: &circusv1.WorkerUplink_CancelCall{CancelCall: &circusv1.CancelCall{CallId: tc.id}}})
			wait.Recv(t, cancelled)
			send(t, l, &circusv1.WorkerUplink{Payload: &circusv1.WorkerUplink_CallQuery{CallQuery: &circusv1.CallQuery{CallId: tc.id}}})
			ctx, cancel := context.WithTimeout(t.Context(), wait.Timeout)
			defer cancel()
			m, err := l.Receive(ctx)
			must(t, err)
			if m.(*circusv1.WorkerDownlink).GetCallUpdate().GetAccepted() == nil {
				t.Fatalf("expected the subsequent Query answer, got %v", m)
			}
			short, stop := context.WithTimeout(t.Context(), 50*time.Millisecond)
			defer stop()
			if extra, err := l.Receive(short); err == nil {
				t.Fatalf("Cancel produced a reply: %v", extra)
			} else if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal(err)
			}
		})
	}
}

func TestConnectionCapAfterAuthentication(t *testing.T) {
	f := setup(t)
	first, second := raw(t, f), raw(t, f)
	defer first.Close()
	defer second.Close()
	req, _ := http.NewRequest("GET", f.server.URL+"/v1/link/worker", nil)
	req.Header.Set("Authorization", "Bearer "+f.f.Token)
	res, err := http.DefaultClient.Do(req)
	must(t, err)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("third authenticated connection: %s", res.Status)
	}
	res, err = http.Get(f.server.URL + "/v1/link/worker")
	must(t, err)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated request consumed a slot check: %s", res.Status)
	}
}

func TestIdleRevocationClosesDefinitively(t *testing.T) {
	f := setup(t)
	c := client(t, f)
	u, err := f.st.GetUserByName(t.Context(), "admin")
	must(t, err)
	_, _, err = auth.RevokeByName(t.Context(), f.st, store.KindWorker, u.ID, "desk", time.Now())
	must(t, err)
	// The first recheck after the revoke closes the link (auth's Watch tests
	// pin its timing).
	advanceUntilDone(t, f, c, time.Second)
	if _, err = workerclient.Dial(t.Context(), workerclient.Options{Credentials: f.f}); err == nil {
		t.Fatal("revoked upgrade accepted")
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
	send(t, l, &circusv1.WorkerUplink{Payload: &circusv1.WorkerUplink_CallQuery{CallQuery: &circusv1.CallQuery{CallId: "stuck"}}})
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
