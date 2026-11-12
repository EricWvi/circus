package controlplane

import (
	"net"
	"net/http"
)

// SetListen replaces net.Listen in Start, so a test can hand Start a
// listener that reports any address.
func SetListen(s *Server, f func(network, addr string) (net.Listener, error)) { s.listen = f }

// ServeReady calls the /readyz handler directly. During shutdown the
// listener is closed, so a test cannot reach /readyz over the network then.
func ServeReady(s *Server, w http.ResponseWriter, r *http.Request) { s.serveReady(w, r) }
