package controlplane

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"

	"github.com/EricWvi/circus/internal/auth"
	"github.com/EricWvi/circus/internal/store"
)

// HookGrace exposes Shutdown's post-deadline collection window to timing tests.
const HookGrace = hookGrace

// SetListen replaces net.Listen in Start, so a test can hand Start a
// listener that reports any address.
func SetListen(s *Server, f func(network, addr string) (net.Listener, error)) { s.listen = f }

// ServeReady calls the /readyz handler directly. During shutdown the
// listener is closed, so a test cannot reach /readyz over the network then.
func ServeReady(s *Server, w http.ResponseWriter, r *http.Request) { s.serveReady(w, r) }

// PrepareShutdownForTest initializes only the machinery Shutdown needs, with
// no network listener, store, or background goroutines. Register hooks first.
// This lets shutdown timing tests run in a synctest bubble.
func PrepareShutdownForTest(s *Server, logOutput io.Writer) {
	s.logger = slog.New(slog.NewJSONHandler(logOutput, nil))
	s.httpSrv = new(http.Server)
	s.ln = shutdownTestListener{}
	s.stopBg = func() {}
	s.started = true
}

type shutdownTestListener struct{}

func (shutdownTestListener) Accept() (net.Conn, error) { return nil, net.ErrClosed }
func (shutdownTestListener) Close() error              { return nil }
func (shutdownTestListener) Addr() net.Addr            { return nil }

// Mount registers an extra handler; call it before Start.
func Mount(s *Server, pattern string, h http.Handler) { s.mux.Handle(pattern, h) }

// AddReadyCheck adds an extra /readyz check; call it before Start.
func AddReadyCheck(s *Server, name string, fn func(context.Context) error) {
	s.checks = append(s.checks, readyCheck{name, fn})
}

// AddShutdownHook adds an extra Shutdown step (see shutdownHook); call it
// before Start.
func AddShutdownHook(s *Server, name string, fn func(context.Context) error) {
	s.hooks = append(s.hooks, shutdownHook{name, fn})
}

// InstanceID returns the instance id present on every log line.
func InstanceID(s *Server) string { return s.instanceID }

// Authenticator returns the server's single authenticator.
func Authenticator(s *Server) *auth.Authenticator { return s.authn }

// User returns the default user.
func User(s *Server) store.User { return s.user }
