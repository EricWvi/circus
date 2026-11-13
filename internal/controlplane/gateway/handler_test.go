package gateway_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/clock"
	"github.com/EricWvi/circus/internal/controlplane/bus"
	"github.com/EricWvi/circus/internal/controlplane/gateway"
	"github.com/EricWvi/circus/internal/wire"
	"github.com/coder/websocket"
	"go.uber.org/goleak"
	"google.golang.org/protobuf/proto"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type auth struct{}

func (auth) Authenticate(_ context.Context, r *http.Request) (gateway.Principal, error) {
	if r.Header.Get("Authorization") != "Bearer test" {
		return gateway.Principal{}, gateway.ErrUnauthenticated
	}
	return gateway.Principal{UserID: "u", DeviceID: "d", DeviceName: "desk", CredentialID: "cred"}, nil
}
func (auth) Recheck(context.Context, gateway.Principal) error { return nil }

type hooks struct{ gateway.Hooks }

func (hooks) OnConnected(context.Context, gateway.Connection) error    { return nil }
func (hooks) OnDisconnected(context.Context, gateway.Connection) error { return nil }
func TestHTTPAuthenticationAndRawVersionReject(t *testing.T) {
	baseline := goleak.IgnoreCurrent()
	t.Cleanup(func() { goleak.VerifyNone(t, baseline) })
	c := clock.New()
	b, _ := bus.New(bus.Limits{MaxInstances: 1, MaxInFlight: 8, MaxBytes: 8 << 20})
	r, _ := gateway.NewRegistry(2)
	h, err := gateway.New(gateway.Config{InstanceID: "i", Clock: c, Auth: auth{}, Registry: r, Bus: b, Hooks: hooks{}, MaxConnections: 2, LinkOptions: wire.Options{MaxMessageBytes: 1 << 20, MaxFragmentBytes: 1024}, SendPoolBytes: 4 << 20, ReceivePoolBytes: 32 << 20})
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(h)
	defer s.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	defer func() {
		if err := h.Shutdown(ctx); err != nil {
			t.Error(err)
		}
		stats := h.Stats()
		if stats.Connections != 0 || stats.Registered != 0 || stats.Budget.Send.Used != 0 || stats.Budget.Receive.Used != 0 {
			t.Errorf("ownership: %+v", stats)
		}
	}()
	res, err := http.Get(s.URL + "/v1/link/daemon")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 401 {
		t.Fatal(res.Status)
	}
	// No need for a real bearer credential in the stub; this is the raw protocol seam.
	conn, _, err := wire.DialWebSocket(ctx, "ws"+strings.TrimPrefix(s.URL, "http")+"/v1/link/daemon", dialOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	data, _ := proto.Marshal(&circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_Hello{Hello: &circusv1.Hello{BootId: "boot", ProtocolVersion: 99}}})
	if err = conn.WriteMessage(ctx, data); err != nil {
		t.Fatal(err)
	}
	data, err = conn.ReadMessage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	m, err := wire.Decode(wire.DaemonDownlink, data, wire.MaxMetadataBytes)
	if err != nil {
		t.Fatal(err)
	}
	if m.(*circusv1.DaemonDownlink).GetReject().GetCode() != circusv1.Reject_CODE_VERSION_UNSUPPORTED {
		t.Fatal(m)
	}
}

func dialOptions() *websocket.DialOptions {
	return &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": []string{"Bearer test"}}}
}

// revocable fails Recheck once revoked is closed (F17).
type revocable struct {
	auth
	revoked chan struct{}
}

func (r revocable) Recheck(context.Context, gateway.Principal) error {
	select {
	case <-r.revoked:
		return gateway.ErrRevoked
	default:
		return nil
	}
}

// daemonLink performs the raw Hello/Welcome exchange and returns a daemon link.
func daemonLink(t *testing.T, ctx context.Context, url, boot string, c *clock.Fake) *wire.Link {
	t.Helper()
	conn, _, err := wire.DialWebSocket(ctx, "ws"+strings.TrimPrefix(url, "http")+"/v1/link/daemon", dialOptions())
	if err != nil {
		t.Fatal(err)
	}
	data, _ := proto.Marshal(&circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_Hello{Hello: &circusv1.Hello{BootId: boot, ProtocolVersion: 1}}})
	if err = conn.WriteMessage(ctx, data); err != nil {
		t.Fatal(err)
	}
	if data, err = conn.ReadMessage(ctx); err != nil {
		t.Fatal(err)
	}
	m, err := wire.Decode(wire.DaemonDownlink, data, wire.MaxMetadataBytes)
	if err != nil {
		t.Fatal(err)
	}
	opts, err := wire.OptionsFromWelcome(m.(*circusv1.DaemonDownlink).GetWelcome(), wire.Options{Clock: c, DisableHeartbeat: true})
	if err != nil {
		t.Fatal(err)
	}
	l, err := wire.New(conn, wire.DaemonSide, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

// F16 and F17: a second connection with the same credential supersedes the
// first, which receives GoAway{SUPERSEDED}; revocation closes the live link
// within one recheck interval. Ownership is released afterwards.
func TestSupersedeAndRevocation(t *testing.T) {
	baseline := goleak.IgnoreCurrent()
	t.Cleanup(func() { goleak.VerifyNone(t, baseline) })
	c := clock.New()
	b, _ := bus.New(bus.Limits{MaxInstances: 1, MaxInFlight: 8, MaxBytes: 8 << 20})
	r, _ := gateway.NewRegistry(2)
	a := revocable{revoked: make(chan struct{})}
	h, err := gateway.New(gateway.Config{InstanceID: "i", Clock: c, Auth: a, Registry: r, Bus: b, Hooks: hooks{}, MaxConnections: 4, RecheckInterval: time.Second, LinkOptions: wire.Options{MaxMessageBytes: 1 << 20, MaxFragmentBytes: 1024, DisableHeartbeat: true}})
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(h)
	defer s.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	old := daemonLink(t, ctx, s.URL, "boot", c)
	waitFor(t, func() bool { v, ok, _ := r.Lookup(ctx, "d"); return ok && v.Target.BootID == "boot" })
	current := daemonLink(t, ctx, s.URL, "boot2", c)
	m, err := old.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if m.(*circusv1.DaemonDownlink).GetGoAway().GetReason() != circusv1.GoAway_REASON_SUPERSEDED {
		t.Fatal(m)
	}
	if _, err = old.Receive(ctx); err == nil {
		t.Fatal("superseded link stayed open")
	}
	waitFor(t, func() bool { return h.Stats().Connections == 1 })
	if v, ok, _ := r.Lookup(ctx, "d"); !ok || v.Target.BootID != "boot2" {
		t.Fatal("stale close removed the replacement route", v)
	}
	close(a.revoked)
	// The recheck loop may arm its next alarm after any single Advance.
	waitFor(t, func() bool { c.Advance(time.Second); return h.Stats().Connections == 0 && r.Stats().Connections == 0 })
	if _, err = current.Receive(ctx); err == nil {
		t.Fatal("revoked link stayed open")
	}
	if err := h.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if st := h.Stats(); st.Budget.Send.Used != 0 || st.Budget.Receive.Used != 0 {
		t.Fatalf("ownership: %+v", st)
	}
	if st := b.Stats(); st.Bindings != 0 || st.InFlight != 0 {
		t.Fatalf("bus: %+v", st)
	}
}

func waitFor(t *testing.T, p func() bool) {
	t.Helper()
	end := time.Now().Add(5 * time.Second)
	for !p() {
		if time.Now().After(end) {
			t.Fatal("condition did not become true")
		}
		time.Sleep(time.Millisecond)
	}
}

// A store error is retryable infrastructure failure, not invalid credentials.
type unavailableAuth struct{ auth }

func (unavailableAuth) Authenticate(context.Context, *http.Request) (gateway.Principal, error) {
	return gateway.Principal{}, context.DeadlineExceeded
}
func TestAuthenticationStoreFailureReturns503(t *testing.T) {
	b, _ := bus.New(bus.Limits{MaxInstances: 1, MaxInFlight: 1, MaxBytes: 1 << 20})
	r, _ := gateway.NewRegistry(1)
	h, err := gateway.New(gateway.Config{InstanceID: "i", Clock: clock.Real{}, Auth: unavailableAuth{}, Registry: r, Bus: b, Hooks: hooks{}})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Shutdown(t.Context())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/link/daemon", nil))
	if rec.Code != 503 {
		t.Fatal(rec.Code)
	}
}

type failingRecheck struct {
	auth
	calls atomic.Int32
}

func (a *failingRecheck) Recheck(context.Context, gateway.Principal) error {
	a.calls.Add(1)
	return errors.New("store temporarily unavailable")
}

type lockedLogs struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedLogs) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}
func (b *lockedLogs) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type armedRecheckClock struct {
	*clock.Fake
	armed chan struct{}
}

func (c *armedRecheckClock) After(d time.Duration) <-chan time.Time {
	ch := c.Fake.After(d)
	if d >= 0 && d <= time.Second {
		c.armed <- struct{}{}
	}
	return ch
}

func TestRecheckStoreErrorsUseStalenessWindow(t *testing.T) {
	c := &armedRecheckClock{Fake: clock.New(), armed: make(chan struct{}, 64)}
	b, _ := bus.New(bus.Limits{MaxInstances: 1, MaxInFlight: 8, MaxBytes: 8 << 20})
	r, _ := gateway.NewRegistry(2)
	a := &failingRecheck{}
	logs := &lockedLogs{}
	logger := slog.New(slog.NewJSONHandler(logs, nil)).With("test_logger", "configured")
	h, err := gateway.New(gateway.Config{InstanceID: "i", Clock: c, Auth: a, Registry: r, Bus: b, Hooks: hooks{}, Logger: logger, RecheckInterval: time.Second, RecheckMaxStale: 30 * time.Second, LinkOptions: wire.Options{MaxMessageBytes: 1 << 20, MaxFragmentBytes: 1024, DisableHeartbeat: true}})
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(h)
	defer s.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	link := daemonLink(t, ctx, s.URL, "boot", c.Fake)
	// armed waits for the recheck to arm its next timer. A recheck that
	// closes the link arms none, and the close may still be in progress
	// when its call is counted, so also stop once the link is gone.
	armed := func() bool {
		end := time.Now().Add(5 * time.Second)
		for {
			select {
			case <-c.armed:
				return true
			case <-time.After(time.Millisecond):
			}
			if h.Stats().Connections == 0 {
				return false
			}
			if time.Now().After(end) {
				t.Fatal("recheck neither armed its timer nor closed the link")
			}
		}
	}
	// Exactly ten seconds of store failures must not close the daemon link.
	for i := range 10 {
		if !armed() {
			t.Fatal("transient store errors closed the link within ten seconds")
		}
		c.Advance(time.Second)
		waitFor(t, func() bool { return a.calls.Load() >= int32(i+1) })
	}
	if h.Stats().Connections != 1 {
		t.Fatal("transient store errors closed the link within ten seconds")
	}
	// Once no valid check has completed for 30 seconds, the link must close.
	for i := 10; i < 31 && armed(); i++ {
		c.Advance(time.Second)
		waitFor(t, func() bool { return a.calls.Load() >= int32(i+1) })
	}
	waitFor(t, func() bool { return h.Stats().Connections == 0 })
	if _, err = link.Receive(ctx); err == nil {
		t.Fatal("stale credential revalidation left link open")
	}
	if err = h.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if got := logs.String(); !strings.Contains(got, `"test_logger":"configured"`) || !strings.Contains(got, `"msg":"daemon credential revalidation closed link"`) {
		t.Fatalf("recheck did not use configured logger: %s", got)
	}
}
