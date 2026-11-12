// Package loopback holds the one loopback rule shared by the control plane
// (plaintext --dev-insecure listeners) and its clients (plaintext http://
// links): plaintext is allowed only when both ends are on this machine
// (D43 item 7).
//
// A host name is checked twice: Host accepts the literal names ("localhost",
// loopback IPs) up front, and Addr checks the address actually bound or
// dialed, because a resolver may map "localhost" elsewhere.
package loopback

import (
	"context"
	"fmt"
	"net"
)

// Host reports whether host (no port) is "localhost" or a loopback IP
// literal. An empty host means every interface and is not loopback.
func Host(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Addr reports whether a bound or dialed TCP address is a loopback address.
func Addr(a net.Addr) bool {
	t, ok := a.(*net.TCPAddr)
	return ok && t.IP.IsLoopback()
}

// DialContext dials with d and closes the connection again unless its
// remote address is loopback. Clients use it for plaintext connections.
func DialContext(d *net.Dialer) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := d.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		if !Addr(c.RemoteAddr()) {
			remote := c.RemoteAddr()
			c.Close()
			return nil, fmt.Errorf("refusing plaintext connection to %s: %s resolved to non-loopback address %v", addr, addr, remote)
		}
		return c, nil
	}
}
