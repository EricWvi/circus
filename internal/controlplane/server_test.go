package controlplane_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"go.uber.org/goleak"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/gen/go/circus/v1/circusv1connect"
	"github.com/EricWvi/circus/internal/auth"
	"github.com/EricWvi/circus/internal/controlplane"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/store/sqlite"
	"github.com/EricWvi/circus/internal/testutil/leakcheck"
)

func TestMain(m *testing.M) {
	code := leakcheck.Run(m, leakcheck.Config{})
	if err := goleak.Find(); err != nil {
		println(err.Error())
		code = 1
	}
	os.Exit(code)
}

// syncBuffer collects log lines from concurrent writers.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type harness struct {
	st   *sqlite.DB
	srv  *controlplane.Server
	logs *syncBuffer
	base string
	http *http.Client
}

func openStore(t *testing.T) *sqlite.DB {
	t.Helper()
	st, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "circus.db"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// newServer builds a server on a fresh store, plaintext on loopback unless
// cfg selects another mode. setup runs before Start.
func newServer(t *testing.T, cfg controlplane.Config, setup func(*controlplane.Server)) *harness {
	t.Helper()
	h := &harness{st: openStore(t), logs: &syncBuffer{}, http: &http.Client{Timeout: 10 * time.Second}}
	if cfg.TLSCert == "" && !cfg.BehindProxy {
		cfg.DevInsecure = true
	}
	if cfg.Listen == "" {
		cfg.Listen = "127.0.0.1:0"
	}
	cfg.LogOutput = h.logs
	cfg.LogLevel = -4 // debug: health probes too
	srv, err := controlplane.New(context.Background(), h.st, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if setup != nil {
		setup(srv)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		h.http.CloseIdleConnections()
	})
	h.srv = srv
	h.base = "http://" + srv.Addr().String()
	return h
}

func (h *harness) get(t *testing.T, path, token string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, h.base+path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := h.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func (h *harness) token(t *testing.T, kind store.PrincipalKind, ttl time.Duration, now time.Time) string {
	t.Helper()
	tok, _, err := auth.CreateEnrollToken(context.Background(), h.st, h.srv.User().ID, kind, ttl, now)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func (h *harness) enrollClient() circusv1connect.EnrollServiceClient {
	return circusv1connect.NewEnrollServiceClient(h.http, h.base)
}

func enrollReq(token string, role circusv1.Role, name string) *connect.Request[circusv1.EnrollRequest] {
	return connect.NewRequest(&circusv1.EnrollRequest{
		OneTimeToken: token, Role: role, Name: name, Hostname: "host", MachineId: "machine-1",
		Os: "linux", Arch: "amd64", ClientVersion: "test",
	})
}

func TestEnrollThenAuthenticate(t *testing.T) {
	h := newServer(t, controlplane.Config{}, nil)
	tok := h.token(t, store.KindDevice, time.Hour, time.Now())
	resp, err := h.enrollClient().Enroll(context.Background(), enrollReq(tok, circusv1.Role_ROLE_DAEMON, "laptop"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Msg.GetControlPlaneUrl() != "" || resp.Msg.GetPrincipalId() == "" {
		t.Fatalf("response = %v", resp.Msg)
	}
	status, body := h.get(t, "/v1/whoami", resp.Msg.GetLongTermToken())
	var who map[string]string
	_ = json.Unmarshal([]byte(body), &who)
	if status != http.StatusOK || who["role"] != "daemon" || who["principal_id"] != resp.Msg.GetPrincipalId() {
		t.Fatalf("whoami = %d %s", status, body)
	}
	if status, _ := h.get(t, "/v1/whoami", tok); status != http.StatusUnauthorized {
		t.Fatalf("one-time token accepted as bearer: %d", status)
	}
	logs := h.logs.String()
	if strings.Contains(logs, tok) || strings.Contains(logs, resp.Msg.GetLongTermToken()) {
		t.Fatal("a token reached the log")
	}
}

// F20 at the RPC boundary: reused, expired, unknown, malformed and
// wrong-role tokens get byte-identical responses.
func TestEnrollFailureResponsesIndistinguishable(t *testing.T) {
	h := newServer(t, controlplane.Config{}, nil)
	used := h.token(t, store.KindDevice, time.Hour, time.Now())
	if _, err := h.enrollClient().Enroll(context.Background(), enrollReq(used, circusv1.Role_ROLE_DAEMON, "first")); err != nil {
		t.Fatal(err)
	}
	expired := h.token(t, store.KindDevice, time.Second, time.Now().Add(-time.Hour))
	worker := h.token(t, store.KindWorker, time.Hour, time.Now())
	unknown, _ := auth.NewToken()

	post := func(token string) (int, string) {
		body := fmt.Sprintf(`{"oneTimeToken":%q,"role":"ROLE_DAEMON","name":"n","hostname":"h","machineId":"m"}`, token)
		resp, err := h.http.Post(h.base+circusv1connect.EnrollServiceEnrollProcedure, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	wantStatus, wantBody := post(used)
	if wantStatus != http.StatusUnauthorized || !strings.Contains(wantBody, "unauthenticated") {
		t.Fatalf("reused token: %d %s", wantStatus, wantBody)
	}
	for name, tok := range map[string]string{"expired": expired, "unknown": unknown, "malformed": "xyz", "wrong role": worker} {
		if status, body := post(tok); status != wantStatus || body != wantBody {
			t.Errorf("%s: %d %s; want %d %s", name, status, body, wantStatus, wantBody)
		}
	}
	if strings.Contains(h.logs.String(), used) || strings.Contains(h.logs.String(), expired) {
		t.Fatal("a token reached the log")
	}
}

// F20: concurrent Enroll calls with one token; exactly one succeeds.
func TestEnrollConcurrentOverRPC(t *testing.T) {
	h := newServer(t, controlplane.Config{}, nil)
	tok := h.token(t, store.KindWorker, time.Hour, time.Now())
	const n = 8
	var wg sync.WaitGroup
	var ok, unauth atomic.Int32
	for i := range n {
		wg.Go(func() {
			_, err := h.enrollClient().Enroll(context.Background(), enrollReq(tok, circusv1.Role_ROLE_WORKER, fmt.Sprintf("w%d", i)))
			switch {
			case err == nil:
				ok.Add(1)
			case connect.CodeOf(err) == connect.CodeUnauthenticated:
				unauth.Add(1)
			default:
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
	wg.Wait()
	if ok.Load() != 1 || unauth.Load() != n-1 {
		t.Fatalf("ok=%d unauthenticated=%d", ok.Load(), unauth.Load())
	}
}

func TestEnrollRequestErrors(t *testing.T) {
	h := newServer(t, controlplane.Config{}, nil)
	c := h.enrollClient()
	ctx := context.Background()
	if _, err := c.Enroll(ctx, enrollReq(h.token(t, store.KindDevice, time.Hour, time.Now()), circusv1.Role_ROLE_DAEMON, "laptop")); err != nil {
		t.Fatal(err)
	}
	tok := h.token(t, store.KindDevice, time.Hour, time.Now())
	_, err := c.Enroll(ctx, enrollReq(tok, circusv1.Role_ROLE_DAEMON, "laptop"))
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodeAlreadyExists || !strings.Contains(ce.Message(), "--name") {
		t.Fatalf("duplicate name: %v", err)
	}
	if _, err := c.Enroll(ctx, enrollReq(tok, circusv1.Role_ROLE_UNSPECIFIED, "x")); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("unspecified role: %v", err)
	}
	if _, err := c.Enroll(ctx, enrollReq(tok, circusv1.Role_ROLE_DAEMON, "bad name")); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("bad name: %v", err)
	}
	// None of the failures consumed the token.
	if _, err := c.Enroll(ctx, enrollReq(tok, circusv1.Role_ROLE_DAEMON, "laptop-2")); err != nil {
		t.Fatalf("retry: %v", err)
	}
}

func TestTransportModes(t *testing.T) {
	st := openStore(t)
	cert, key := writeCert(t, t.TempDir())
	cases := []struct {
		name string
		cfg  controlplane.Config
		ok   bool
	}{
		{"dev-insecure on all interfaces", controlplane.Config{Listen: ":0", DevInsecure: true}, false},
		{"dev-insecure on 0.0.0.0", controlplane.Config{Listen: "0.0.0.0:0", DevInsecure: true}, false},
		{"dev-insecure on a LAN address", controlplane.Config{Listen: "192.0.2.1:8443", DevInsecure: true}, false},
		{"dev-insecure on a host name", controlplane.Config{Listen: "example.com:8443", DevInsecure: true}, false},
		{"no mode", controlplane.Config{Listen: "127.0.0.1:0"}, false},
		{"two modes", controlplane.Config{Listen: "127.0.0.1:0", DevInsecure: true, BehindProxy: true}, false},
		{"cert without key", controlplane.Config{Listen: "127.0.0.1:0", TLSCert: cert}, false},
		{"dev-insecure on ::1", controlplane.Config{Listen: "[::1]:0", DevInsecure: true}, true},
		{"dev-insecure on localhost", controlplane.Config{Listen: "localhost:0", DevInsecure: true}, true},
		{"behind proxy on all interfaces", controlplane.Config{Listen: ":0", BehindProxy: true}, true},
		{"tls", controlplane.Config{Listen: ":0", TLSCert: cert, TLSKey: key}, true},
	}
	for _, c := range cases {
		c.cfg.LogOutput = io.Discard
		_, err := controlplane.New(context.Background(), st, c.cfg)
		if (err == nil) != c.ok {
			t.Errorf("%s: err = %v", c.name, err)
		}
		if err != nil && c.cfg.DevInsecure && !c.ok && strings.HasPrefix(c.name, "dev-insecure") && !strings.Contains(err.Error(), "loopback") {
			t.Errorf("%s: error does not explain the loopback rule: %v", c.name, err)
		}
	}
}

func TestTLS(t *testing.T) {
	dir := t.TempDir()
	cert, key := writeCert(t, dir)
	h := newServer(t, controlplane.Config{TLSCert: cert, TLSKey: key, PublicURL: "https://cp.example:8443/"}, nil)
	pool := x509.NewCertPool()
	pem, _ := os.ReadFile(cert)
	pool.AppendCertsFromPEM(pem)
	h.http.Transport = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}
	h.base = "https://" + h.srv.Addr().String()
	if status, _ := h.get(t, "/healthz", ""); status != http.StatusOK {
		t.Fatalf("healthz over TLS: %d", status)
	}
	tok := h.token(t, store.KindDevice, time.Hour, time.Now())
	resp, err := h.enrollClient().Enroll(context.Background(), enrollReq(tok, circusv1.Role_ROLE_DAEMON, "tls"))
	if err != nil || resp.Msg.GetControlPlaneUrl() != "https://cp.example:8443" {
		t.Fatalf("enroll over TLS: %v, %v", resp, err)
	}
	// Without the CA the handshake fails.
	plain := &http.Client{Timeout: 5 * time.Second}
	if _, err := plain.Get(h.base + "/healthz"); err == nil {
		t.Fatal("self-signed certificate verified against system roots")
	}
	plain.CloseIdleConnections()
	h.http.CloseIdleConnections()
}

func TestHealthAndReadiness(t *testing.T) {
	var fail atomic.Bool
	h := newServer(t, controlplane.Config{}, func(s *controlplane.Server) {
		s.AddReadyCheck("gateway", func(context.Context) error {
			if fail.Load() {
				return errors.New("down")
			}
			return nil
		})
	})
	if status, _ := h.get(t, "/healthz", ""); status != http.StatusOK {
		t.Fatalf("healthz %d", status)
	}
	if status, body := h.get(t, "/readyz", ""); status != http.StatusOK {
		t.Fatalf("readyz %d %s", status, body)
	}
	fail.Store(true)
	if status, body := h.get(t, "/readyz", ""); status != http.StatusServiceUnavailable || !strings.Contains(body, "gateway") || strings.Contains(body, "down") {
		t.Fatalf("readyz with failing check: %d %q", status, body)
	}
	fail.Store(false)
	h.st.Close()
	if status, body := h.get(t, "/readyz", ""); status != http.StatusServiceUnavailable || !strings.Contains(body, "store") {
		t.Fatalf("readyz with closed store: %d %q", status, body)
	}
	if status, _ := h.get(t, "/healthz", ""); status != http.StatusOK {
		t.Fatalf("healthz with closed store: %d", status)
	}
}

func TestMountAndLogs(t *testing.T) {
	h := newServer(t, controlplane.Config{}, func(s *controlplane.Server) {
		s.Mount("/v1/link/daemon", s.Authenticator().Require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, _ := auth.PrincipalFrom(r.Context())
			fmt.Fprint(w, p.ID)
		}), store.KindDevice))
	})
	func() {
		defer func() {
			if recover() == nil {
				t.Error("Mount after Start did not panic")
			}
		}()
		h.srv.Mount("/late", http.NotFoundHandler())
	}()
	tok := h.token(t, store.KindDevice, time.Hour, time.Now())
	resp, err := h.enrollClient().Enroll(context.Background(), enrollReq(tok, circusv1.Role_ROLE_DAEMON, "laptop"))
	if err != nil {
		t.Fatal(err)
	}
	if status, body := h.get(t, "/v1/link/daemon", resp.Msg.GetLongTermToken()); status != http.StatusOK || body != resp.Msg.GetPrincipalId() {
		t.Fatalf("mounted handler: %d %q", status, body)
	}
	if status, _ := h.get(t, "/v1/link/daemon", "wrong"); status != http.StatusUnauthorized {
		t.Fatalf("wrong token: %d", status)
	}
	h.get(t, "/healthz", "")

	lines := strings.Split(strings.TrimSpace(h.logs.String()), "\n")
	if len(lines) < 4 {
		t.Fatalf("only %d log lines", len(lines))
	}
	for _, line := range lines {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not JSON: %q", line)
		}
		if rec["instance_id"] != h.srv.InstanceID() {
			t.Fatalf("log line without instance_id: %q", line)
		}
	}
	if !strings.Contains(h.logs.String(), `"path":"/v1/link/daemon"`) {
		t.Fatal("request not logged")
	}
}

// Shutdown waits for in-flight requests and hooks within its deadline, then
// closes what is left and reports it.
func TestShutdownBounded(t *testing.T) {
	entered := make(chan struct{})
	var hookCtxDone atomic.Bool
	h := newServer(t, controlplane.Config{}, func(s *controlplane.Server) {
		s.Mount("/block", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			close(entered)
			<-r.Context().Done()
		}))
		s.AddShutdownHook("links", func(ctx context.Context) error {
			<-ctx.Done()
			hookCtxDone.Store(true)
			return ctx.Err()
		})
	})
	errc := make(chan error, 1)
	go func() {
		resp, err := h.http.Get(h.base + "/block")
		if err == nil {
			resp.Body.Close()
		}
		errc <- err
	}()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := h.srv.Shutdown(ctx)
	if !errors.Is(err, controlplane.ErrShutdownTimeout) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown = %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !hookCtxDone.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !hookCtxDone.Load() {
		t.Fatal("hook never saw the deadline")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Shutdown took %v", d)
	}
	if err := <-errc; err == nil {
		t.Fatal("blocked request completed normally")
	}
	if again := h.srv.Shutdown(context.Background()); again == nil || again.Error() != err.Error() {
		t.Fatalf("second Shutdown = %v", again)
	}
}

func TestRunGracefulShutdown(t *testing.T) {
	st := openStore(t)
	logs := &syncBuffer{}
	srv, err := controlplane.New(context.Background(), st, controlplane.Config{Listen: "127.0.0.1:0", DevInsecure: true, LogOutput: logs})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for srv.Addr() == nil && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if srv.Addr() == nil {
		t.Fatal("server did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
	if !strings.Contains(logs.String(), "shutdown complete") {
		t.Fatalf("logs: %s", logs)
	}
	if _, err := net.DialTimeout("tcp", srv.Addr().String(), time.Second); err == nil {
		t.Fatal("listener still open after Run")
	}
}

func TestPurgeExpiredEnrollTokens(t *testing.T) {
	st := openStore(t)
	logs := &syncBuffer{}
	user, err := auth.EnsureUser(context.Background(), st, auth.DefaultUserName, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := auth.CreateEnrollToken(context.Background(), st, user.ID, store.KindDevice, time.Second, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := auth.CreateEnrollToken(context.Background(), st, user.ID, store.KindDevice, time.Hour, time.Now()); err != nil {
		t.Fatal(err)
	}
	srv, err := controlplane.New(context.Background(), st, controlplane.Config{
		Listen: "127.0.0.1:0", DevInsecure: true, LogOutput: logs, PurgeInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown(context.Background())
	deadline := time.Now().Add(5 * time.Second)
	purged := func() bool {
		for line := range strings.Lines(logs.String()) {
			var rec struct {
				Msg   string
				Count int
			}
			if json.Unmarshal([]byte(line), &rec) == nil && rec.Msg == "purged expired enroll tokens" && rec.Count == 1 {
				return true
			}
		}
		return false
	}
	for !purged() {
		if time.Now().After(deadline) {
			t.Fatalf("no purge logged: %s", logs)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Only the expired token went; nothing is left to purge, even in an hour.
	if n, err := st.PurgeEnrollTokens(context.Background(), time.Now().UnixMilli()); n != 0 || err != nil {
		t.Fatalf("purge after server purge = %d, %v", n, err)
	}
	if strings.Count(logs.String(), "purged expired enroll tokens") != 1 {
		t.Fatalf("purge logged more than once: %s", logs)
	}
}

// fakeAddrListener reports a fixed address for a real loopback listener.
type fakeAddrListener struct {
	net.Listener
	addr   net.Addr
	closed atomic.Bool
}

func (l *fakeAddrListener) Addr() net.Addr { return l.addr }
func (l *fakeAddrListener) Close() error   { l.closed.Store(true); return l.Listener.Close() }

func listenAs(t *testing.T, ip string) (func(network, addr string) (net.Listener, error), *fakeAddrListener) {
	fake := &fakeAddrListener{addr: &net.TCPAddr{IP: net.ParseIP(ip), Port: 8443}}
	return func(network, _ string) (net.Listener, error) {
		ln, err := net.Listen(network, "127.0.0.1:0")
		fake.Listener = ln
		return fake, err
	}, fake
}

// M8: --dev-insecure re-checks the address actually bound; "localhost"
// resolving elsewhere must not serve plaintext.
func TestDevInsecureChecksBoundAddress(t *testing.T) {
	st := openStore(t)
	srv, err := controlplane.New(context.Background(), st, controlplane.Config{Listen: "localhost:0", DevInsecure: true, LogOutput: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	listen, fake := listenAs(t, "192.0.2.10")
	controlplane.SetListen(srv, listen)
	if err := srv.Start(); err == nil || !strings.Contains(err.Error(), "non-loopback") {
		t.Fatalf("Start = %v", err)
	}
	if !fake.closed.Load() {
		t.Fatal("refused listener left open")
	}
	if srv.Addr() != nil {
		t.Fatal("server reports an address after refusing to start")
	}
}

// L-6: --behind-proxy on a non-loopback address starts but warns.
func TestBehindProxyWarnsOffLoopback(t *testing.T) {
	for _, c := range []struct {
		ip   string
		warn bool
	}{{"192.0.2.10", true}, {"127.0.0.1", false}} {
		st := openStore(t)
		logs := &syncBuffer{}
		srv, err := controlplane.New(context.Background(), st, controlplane.Config{Listen: ":0", BehindProxy: true, LogOutput: logs})
		if err != nil {
			t.Fatal(err)
		}
		listen, _ := listenAs(t, c.ip)
		controlplane.SetListen(srv, listen)
		if err := srv.Start(); err != nil {
			t.Fatal(err)
		}
		_ = srv.Shutdown(context.Background())
		if got := strings.Contains(logs.String(), `"level":"WARN","msg":"--behind-proxy serves plaintext`); got != c.warn {
			t.Errorf("%s: warning logged = %v\n%s", c.ip, got, logs)
		}
	}
}

// M19: /readyz answers 503 once shutdown has begun.
func TestReadyzDuringShutdown(t *testing.T) {
	var srv *controlplane.Server
	statuses := make(chan int, 1)
	h := newServer(t, controlplane.Config{}, func(s *controlplane.Server) {
		srv = s
		s.AddShutdownHook("probe", func(context.Context) error {
			rec := httptest.NewRecorder()
			controlplane.ServeReady(srv, rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
			statuses <- rec.Code
			return nil
		})
	})
	rec := httptest.NewRecorder()
	controlplane.ServeReady(h.srv, rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("readyz before shutdown: %d", rec.Code)
	}
	if err := h.srv.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := <-statuses; got != http.StatusServiceUnavailable {
		t.Fatalf("readyz during shutdown: %d", got)
	}
}

// L-1: hooks start only after the listener stops accepting.
func TestHooksStartAfterListenerCloses(t *testing.T) {
	// Observe the listener directly instead of dialing its port: once closed,
	// the port may be handed to a listener in another test process, and a
	// successful dial would then say nothing about this server.
	for i := range 50 {
		var ln *closeRecordingListener
		closedAtHook := make(chan bool, 1)
		h := newServer(t, controlplane.Config{}, func(s *controlplane.Server) {
			controlplane.SetListen(s, func(network, addr string) (net.Listener, error) {
				inner, err := net.Listen(network, addr)
				if err != nil {
					return nil, err
				}
				ln = &closeRecordingListener{Listener: inner}
				return ln, nil
			})
			s.AddShutdownHook("observe", func(context.Context) error {
				closedAtHook <- ln.closed.Load()
				return nil
			})
		})
		if err := h.srv.Shutdown(context.Background()); err != nil {
			t.Fatal(err)
		}
		if !<-closedAtHook {
			t.Fatalf("round %d: a hook started before the listener was closed", i)
		}
	}
}

// closeRecordingListener records whether Close was called.
type closeRecordingListener struct {
	net.Listener
	closed atomic.Bool
}

func (l *closeRecordingListener) Close() error {
	l.closed.Store(true)
	return l.Listener.Close()
}

// L-8: a hook that ignores ctx does not hold Shutdown past its deadline,
// and the log names it.
func TestShutdownAbandonsStuckHook(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	h := newServer(t, controlplane.Config{}, func(s *controlplane.Server) {
		s.AddShutdownHook("fast", func(context.Context) error { return nil })
		s.AddShutdownHook("stuck", func(context.Context) error { <-release; return nil })
	})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := h.srv.Shutdown(ctx)
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Shutdown took %v", d)
	}
	if err == nil || !strings.Contains(err.Error(), "shutdown hook stuck did not finish") || strings.Contains(err.Error(), "hook fast") {
		t.Fatalf("Shutdown = %v", err)
	}
	if !strings.Contains(h.logs.String(), `"msg":"shutdown hook did not finish before the deadline"`) ||
		!strings.Contains(h.logs.String(), `"hook":"stuck"`) {
		t.Fatalf("stuck hook not logged:\n%s", h.logs)
	}
}

// NL-3: errors returned by hooks are part of the shutdown result.
func TestShutdownReportsHookErrors(t *testing.T) {
	boom := errors.New("boom")
	h := newServer(t, controlplane.Config{}, func(s *controlplane.Server) {
		s.AddShutdownHook("ok", func(context.Context) error { return nil })
		s.AddShutdownHook("bad", func(context.Context) error { return boom })
	})
	err := h.srv.Shutdown(context.Background())
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "shutdown hook bad: boom") || strings.Contains(err.Error(), "hook ok") {
		t.Fatalf("Shutdown = %v", err)
	}
}

// NL-1: a hook that waits for ctx and returns as the deadline passes is
// finished, not abandoned, and its own error is reported.
func TestShutdownHookReturningAtDeadline(t *testing.T) {
	late := errors.New("late")
	for i := range 30 {
		h := newServer(t, controlplane.Config{}, func(s *controlplane.Server) {
			s.AddShutdownHook("waiter", func(ctx context.Context) error { <-ctx.Done(); return late })
			s.AddShutdownHook("clean", func(ctx context.Context) error { <-ctx.Done(); return nil })
		})
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		err := h.srv.Shutdown(ctx)
		cancel()
		if !errors.Is(err, late) || strings.Contains(err.Error(), "did not finish") {
			t.Fatalf("round %d: Shutdown = %v", i, err)
		}
		if strings.Contains(h.logs.String(), "did not finish") {
			t.Fatalf("round %d: hook logged as unfinished", i)
		}
	}
}

// L-5: client-controlled fields are truncated in the log.
func TestLogTruncatesClientFields(t *testing.T) {
	h := newServer(t, controlplane.Config{}, nil)
	long := strings.Repeat("m", 200) + "TAIL"
	req := enrollReq(strings.Repeat("A", auth.TokenLen), circusv1.Role_ROLE_DAEMON, "x")
	req.Msg.MachineId = long
	if _, err := h.enrollClient().Enroll(context.Background(), req); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("enroll: %v", err)
	}
	h.get(t, "/"+strings.Repeat("p", 300)+"TAIL", "")
	logs := h.logs.String()
	if strings.Contains(logs, "TAIL") || !strings.Contains(logs, "(truncated)") {
		t.Fatalf("client fields not truncated:\n%s", logs)
	}
}

// writeCert writes a self-signed ECDSA certificate for 127.0.0.1, ::1 and
// localhost, and returns the cert and key paths.
func writeCert(t *testing.T, dir string) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "circus test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}, DNSNames: []string{"localhost"},
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA: true, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPath, keyPath := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}
