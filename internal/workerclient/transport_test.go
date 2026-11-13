package workerclient

import (
	"encoding/pem"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/EricWvi/circus/internal/credfile"
	"github.com/EricWvi/circus/internal/testutil/wait"
)

func TestTransportTLSAndPlaintextPolicy(t *testing.T) {
	for _, url := range []string{"http://example.com", "ws://192.0.2.1", "ftp://localhost", "http://user:pass@localhost", "https://localhost/?x=1", "http://localhost/#secret"} {
		if _, _, err := transport(credfile.File{ControlPlaneURL: url}); err == nil {
			t.Fatalf("accepted %s", url)
		}
	}
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }))
	s.Config.ErrorLog = log.New(io.Discard, "", 0)
	s.StartTLS()
	defer s.Close()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	_, client, err := transport(credfile.File{ControlPlaneURL: s.URL, CAFile: ca})
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	res, err := client.Get(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	_, untrusted, err := transport(credfile.File{ControlPlaneURL: s.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer untrusted.CloseIdleConnections()
	if res, err := untrusted.Get(s.URL); err == nil {
		_ = res.Body.Close()
		t.Fatal("untrusted certificate accepted")
	}
	if _, _, err := transport(credfile.File{ControlPlaneURL: s.URL, CAFile: filepath.Join(t.TempDir(), "missing")}); err == nil {
		t.Fatal("missing CA accepted")
	}
}
func TestDialNeverFollowsRedirects(t *testing.T) {
	var reached atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { reached.Add(1); w.WriteHeader(500) }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	ctx := wait.Ctx(t)
	_, err := Dial(ctx, Options{Credentials: credfile.File{Role: credfile.RoleWorker, Token: "secret", PrincipalID: "worker", ControlPlaneURL: source.URL}})
	if err == nil || !strings.Contains(err.Error(), "redirects are disabled") || reached.Load() != 0 {
		t.Fatalf("redirect: %v, target requests %d", err, reached.Load())
	}
	if _, err := Dial(ctx, Options{Credentials: credfile.File{Role: credfile.RoleDaemon, Token: "secret", PrincipalID: "worker", ControlPlaneURL: source.URL}}); err == nil {
		t.Fatal("daemon credential accepted")
	}
}
