// Package loopback holds the one loopback rule shared by the control plane
// (plaintext --dev-insecure listeners) and its clients (plaintext http://
// links): plaintext is allowed only when both ends are on this machine.
// It also builds the one HTTP client every control plane client uses.
//
// A host name is checked twice: Host accepts the literal names ("localhost",
// loopback IPs) up front, and Addr checks the address actually bound or
// dialed, because a resolver may map "localhost" elsewhere.
package loopback

import "net"

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
