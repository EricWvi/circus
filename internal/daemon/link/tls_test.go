package link_test

import (
	"context"
	"encoding/pem"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/clock"
	"github.com/EricWvi/circus/internal/credfile"
	"github.com/EricWvi/circus/internal/daemon"
	"github.com/EricWvi/circus/internal/daemon/jobs"
	daemonlink "github.com/EricWvi/circus/internal/daemon/link"
	"github.com/EricWvi/circus/internal/testutil/leakcheck"
	"github.com/EricWvi/circus/internal/wire"
	"google.golang.org/protobuf/proto"
)

func TestRealWebSocketTLSCredentialsAndGracefulClose(t *testing.T) {
	leakcheck.Track(t)
	links := make(chan *wire.Link, 1)
	hellos := make(chan *circusv1.Hello, 1)
	failures := make(chan error, 1)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/link/daemon" || r.Header.Get("Authorization") != "Bearer tls-secret" {
			http.Error(w, "unauthorized", 401)
			return
		}
		raw, err := wire.AcceptWebSocket(w, r, nil)
		if err != nil {
			failures <- err
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		data, err := raw.ReadMessage(ctx)
		if err != nil {
			_ = raw.Close()
			failures <- err
			return
		}
		m, err := wire.Decode(wire.DaemonUplink, data, wire.MaxMetadataBytes)
		if err != nil {
			_ = raw.Close()
			failures <- err
			return
		}
		hellos <- m.(*circusv1.DaemonUplink).GetHello()
		welcome := &circusv1.Welcome{PrincipalId: "tls-device", NegotiatedVersion: 1}
		options, err := wire.FillWelcome(welcome, wire.Options{})
		if err != nil {
			_ = raw.Close()
			failures <- err
			return
		}
		data, err = proto.Marshal(&circusv1.DaemonDownlink{Payload: &circusv1.DaemonDownlink_Welcome{Welcome: welcome}})
		if err == nil {
			err = raw.WriteMessage(ctx, data)
		}
		if err != nil {
			_ = raw.Close()
			failures <- err
			return
		}
		l, err := wire.New(raw, wire.DaemonControlSide, options)
		if err != nil {
			_ = raw.Close()
			failures <- err
			return
		}
		links <- l
	}))
	defer server.Close()
	dir := t.TempDir()
	ca := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	credentials := credfile.File{Role: credfile.RoleDaemon, ControlPlaneURL: server.URL, PrincipalID: "tls-device", Token: "tls-secret", CAFile: ca}
	path := filepath.Join(dir, "daemon.json")
	if err := credfile.Save(path, credentials); err != nil {
		t.Fatal(err)
	}
	loaded, err := credfile.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	c, err := daemonlink.New(daemonlink.Options{Credentials: loaded, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Tools: func(*jobs.Manager) []daemon.Tool {
		return leakcheck.AuditTools(t, nil, []daemon.Tool{tool{func(context.Context, *circusv1.DeviceCall) (*circusv1.DeviceResult, error) {
			return result("TLS verified"), nil
		}}})
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(4 * time.Second):
			t.Error("TLS client did not stop")
		}
	})
	var l *wire.Link
	select {
	case l = <-links:
	case err := <-failures:
		t.Fatal(err)
	case <-time.After(4 * time.Second):
		t.Fatal("TLS handshake timed out")
	}
	defer l.Close()
	hello := recv(t, hellos)
	dispatch(t, l, hello.BootId, call("tls"))
	var output *circusv1.DeviceResult
	for output == nil {
		output = receive(t, l).GetResult()
	}
	if output.Output.Content[0].GetText() != "TLS verified" {
		t.Fatal(output)
	}
	cancel()
	if err := recv(t, done); err != nil {
		t.Fatal(err)
	}
	done <- nil
}

func TestTLSRejectsUntrustedServer(t *testing.T) {
	leakcheck.Track(t)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("untrusted TLS reached HTTP handler") }))
	defer server.Close()
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	clk := clock.New()
	c, err := daemonlink.New(daemonlink.Options{Credentials: credfile.File{Role: credfile.RoleDaemon, ControlPlaneURL: server.URL, PrincipalID: "d", Token: "secret"}, Clock: clk, Tools: func(*jobs.Manager) []daemon.Tool { return nil }, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	eventually(t, func() bool { return c.Stats().RetryDelay > 0 })
	cancel()
	if err := recv(t, done); err != nil {
		t.Fatal(err)
	}
	if c.Stats().Connections != 0 {
		t.Fatal("untrusted certificate accepted")
	}
}

func TestTransportPolicyRejectsUnsafeURLsAndCA(t *testing.T) {
	for _, url := range []string{"http://example.com", "ws://192.168.1.1", "http://localhost.evil", "ftp://127.0.0.1", "https://user:password@example.com", "https://example.com?token=secret"} {
		_, err := daemonlink.New(daemonlink.Options{Credentials: credfile.File{Role: credfile.RoleDaemon, ControlPlaneURL: url, PrincipalID: "d", Token: "secret"}})
		if err == nil {
			t.Fatalf("accepted unsafe URL: %s", url)
		}
	}
	path := filepath.Join(t.TempDir(), "empty-ca.pem")
	if err := os.WriteFile(path, []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := daemonlink.New(daemonlink.Options{Credentials: credfile.File{Role: credfile.RoleDaemon, ControlPlaneURL: "https://example.com", PrincipalID: "d", Token: "secret", CAFile: path}})
	if err == nil || !strings.Contains(err.Error(), "no certificates") {
		t.Fatal(err)
	}
}

func TestUpgradeRefusesRedirects(t *testing.T) {
	for _, tlsSource := range []bool{false, true} {
		t.Run(map[bool]string{false: "http", true: "https-to-http"}[tlsSource], func(t *testing.T) {
			leakcheck.Track(t)
			var redirected, attempts atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1); http.Error(w, "must not follow", 401) }))
			defer target.Close()
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				http.Redirect(w, r, target.URL+"/v1/link/daemon", http.StatusFound)
			})
			var source *httptest.Server
			if tlsSource {
				source = httptest.NewTLSServer(handler)
			} else {
				source = httptest.NewServer(handler)
			}
			defer source.Close()
			credentials := credfile.File{Role: credfile.RoleDaemon, ControlPlaneURL: source.URL, PrincipalID: "device", Token: "secret"}
			if tlsSource {
				credentials.CAFile = filepath.Join(t.TempDir(), "ca.pem")
				if err := os.WriteFile(credentials.CAFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: source.Certificate().Raw}), 0600); err != nil {
					t.Fatal(err)
				}
			}
			clk := clock.New()
			c, err := daemonlink.New(daemonlink.Options{Credentials: credentials, Clock: clk, Tools: func(*jobs.Manager) []daemon.Tool { return nil }, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- c.Run(ctx) }()
			eventually(t, func() bool { return c.Stats().RetryDelay > 0 })
			cancel()
			if err = recv(t, done); err != nil {
				t.Fatal(err)
			}
			if attempts.Load() != 1 || redirected.Load() != 0 {
				t.Fatalf("attempts=%d redirected=%d", attempts.Load(), redirected.Load())
			}
		})
	}
}
