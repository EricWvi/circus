package loopback

import (
	"context"
	"encoding/pem"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// farConn is a connection whose remote end is not loopback, as if
// "localhost" had resolved to another machine.
type farConn struct {
	net.Conn
	written atomic.Int64
}

func (c *farConn) RemoteAddr() net.Addr { return &net.TCPAddr{IP: net.ParseIP("192.0.2.10"), Port: 80} }
func (c *farConn) Write(p []byte) (int, error) {
	c.written.Add(int64(len(p)))
	return c.Conn.Write(p)
}

// A plaintext URL that names "localhost" is refused when the dial lands on
// a non-loopback address: the bearer token never leaves the machine in
// plaintext, whatever the resolver says.
func TestPlaintextLocalhostDialledElsewhereIsRefused(t *testing.T) {
	for _, raw := range []string{"http://localhost:8080", "ws://localhost:8080", "HTTP://localhost:8080"} {
		t.Run(raw, func(t *testing.T) {
			var dialled []string
			var conns []*farConn
			dial := func(_ context.Context, _, addr string) (net.Conn, error) {
				dialled = append(dialled, addr)
				a, b := net.Pipe()
				go func() { _, _ = b.Read(make([]byte, 1)); b.Close() }()
				c := &farConn{Conn: a}
				conns = append(conns, c)
				return c, nil
			}
			u, err := linkURL(raw, "/v1/link/daemon")
			if err != nil {
				t.Fatal(err)
			}
			client, err := newClient(u, "", dial)
			if err != nil {
				t.Fatal(err)
			}
			u.Scheme = "http" // the WebSocket upgrade is an HTTP request
			req, _ := http.NewRequest(http.MethodGet, u.String(), nil)
			req.Header.Set("Authorization", "Bearer secret")
			_, err = client.Do(req)
			if err == nil || !strings.Contains(err.Error(), "non-loopback") {
				t.Fatalf("plaintext request to a non-loopback remote: %v", err)
			}
			if len(dialled) == 0 || !strings.HasPrefix(strings.ToLower(dialled[0]), "localhost:") {
				t.Fatalf("dialled %v", dialled)
			}
			for _, c := range conns {
				if n := c.written.Load(); n != 0 {
					t.Fatalf("%d bytes written to a non-loopback remote", n)
				}
			}
		})
	}
}

// The same dial check does not apply to TLS: there the certificate, not
// the address, authenticates the server.
func TestTLSURLHasNoLoopbackDialCheck(t *testing.T) {
	u, _ := url.Parse("https://example.com")
	c, err := newClient(u, "", func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("unused")
	})
	if err != nil {
		t.Fatal(err)
	}
	if tr := c.Transport.(*http.Transport); tr.Proxy == nil {
		t.Fatal("https client lost the proxy setting")
	}
}

func TestLinkURLPolicy(t *testing.T) {
	for _, raw := range []string{"http://example.com", "ws://192.168.1.1", "http://localhost.evil", "ftp://127.0.0.1", "https://user:password@example.com", "https://example.com?token=secret", "http://localhost/#secret", ""} {
		if _, err := linkURL(raw, "/v1/link/worker"); err == nil {
			t.Errorf("accepted %q", raw)
		}
	}
	for raw, want := range map[string]string{
		"https://cp.example/":     "wss://cp.example/v1/link/worker",
		"http://127.0.0.1:9/base": "ws://127.0.0.1:9/base/v1/link/worker",
		"WS://[::1]:9":            "ws://[::1]:9/v1/link/worker",
	} {
		u, err := linkURL(raw, "/v1/link/worker")
		if err != nil || u.String() != want {
			t.Errorf("linkURL(%q) = %v, %v; want %s", raw, u, err, want)
		}
	}
}

func TestClientRefusesRedirectsAndBadCA(t *testing.T) {
	u, _ := url.Parse("https://example.com")
	c, err := Client(u, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CheckRedirect(nil, nil); !errors.Is(err, ErrRedirect) {
		t.Fatal(err)
	}
	if _, err := Client(u, t.TempDir()+"/missing"); err == nil {
		t.Fatal("missing CA accepted")
	}
}

func TestClientTLSRoots(t *testing.T) {
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }))
	s.Config.ErrorLog = log.New(io.Discard, "", 0)
	s.StartTLS()
	defer s.Close()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(s.URL)
	client, err := Client(u, ca)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	res, err := client.Get(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	untrusted, err := Client(u, "")
	if err != nil {
		t.Fatal(err)
	}
	defer untrusted.CloseIdleConnections()
	if res, err := untrusted.Get(s.URL); err == nil {
		_ = res.Body.Close()
		t.Fatal("untrusted certificate accepted")
	}
	empty := filepath.Join(t.TempDir(), "empty.pem")
	if err := os.WriteFile(empty, []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Client(u, empty); err == nil || !strings.Contains(err.Error(), "no certificates") {
		t.Fatalf("empty CA: %v", err)
	}
}
