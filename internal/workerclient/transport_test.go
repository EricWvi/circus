package workerclient

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/credfile"
	"github.com/EricWvi/circus/internal/loopback"
	"github.com/EricWvi/circus/internal/testutil/wait"
	"github.com/EricWvi/circus/internal/wire"
)

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
	if err == nil || !errors.Is(err, loopback.ErrRedirect) || reached.Load() != 0 {
		t.Fatalf("redirect: %v, target requests %d", err, reached.Load())
	}
	if _, err := Dial(ctx, Options{Credentials: credfile.File{Role: credfile.RoleDaemon, Token: "secret", PrincipalID: "worker", ControlPlaneURL: source.URL}}); err == nil {
		t.Fatal("daemon credential accepted")
	}
}

// The worker client refuses a server whose negotiated message limit exceeds
// its local cap: Options.Wire.MaxMessageBytes, or the wire default.
func TestDialEnforcesLocalMessageCap(t *testing.T) {
	for _, tc := range []struct {
		local, server int
		ok            bool
	}{
		{1 << 20, 2 << 20, false},
		{0, 2 * wire.DefaultMaxMessageBytes, false},
		{0, wire.DefaultMaxMessageBytes, true},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			conn, err := wire.AcceptWebSocket(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			_, _, err = wire.AcceptHello(r.Context(), conn, wire.WorkerUplink, wire.Options{MaxMessageBytes: tc.server}, func(*circusv1.Hello) *circusv1.Welcome {
				return &circusv1.Welcome{PrincipalId: "worker", InstanceId: "i", ServerTimeUnixMs: 1}
			})
			if err == nil {
				_, _ = conn.ReadMessage(r.Context()) // until the client closes
			}
		}))
		c, err := Dial(wait.Ctx(t), Options{Credentials: credfile.File{Role: credfile.RoleWorker, Token: "secret", PrincipalID: "worker", ControlPlaneURL: server.URL}, Wire: wire.Options{MaxMessageBytes: tc.local}})
		if tc.ok {
			if err != nil {
				t.Fatalf("local %d, server %d: %v", tc.local, tc.server, err)
			}
			_ = c.Close()
		} else if !errors.Is(err, wire.ErrIncompatible) {
			t.Fatalf("local %d, server %d: %v", tc.local, tc.server, err)
		}
		server.Close()
	}
}
