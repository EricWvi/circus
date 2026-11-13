package controlplane

import (
	"io"
	"log/slog"
	"net"
	"net/http"
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
