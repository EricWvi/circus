package loopback

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

func TestHost(t *testing.T) {
	for host, want := range map[string]bool{
		"localhost": true, "127.0.0.1": true, "127.1.2.3": true, "::1": true,
		"": false, "0.0.0.0": false, "::": false, "192.0.2.1": false, "example.com": false, "localhost.": false,
	} {
		if Host(host) != want {
			t.Errorf("Host(%q) = %v", host, !want)
		}
	}
}

func TestAddr(t *testing.T) {
	if !Addr(&net.TCPAddr{IP: net.ParseIP("127.0.0.1")}) || Addr(&net.TCPAddr{IP: net.ParseIP("192.0.2.1")}) || Addr(&net.UnixAddr{}) {
		t.Fatal("Addr misclassifies")
	}
}

func TestGuard(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	dial := guard((&net.Dialer{Timeout: 5 * time.Second}).DialContext)
	c, err := dial(context.Background(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	c.Close()

	// A non-loopback remote is refused after the dial. Use this machine's
	// own non-loopback address, if it has one, with a listener on it.
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok || ipn.IP.IsLoopback() || ipn.IP.To4() == nil || ipn.IP.IsLinkLocalUnicast() {
			continue
		}
		far, err := net.Listen("tcp", net.JoinHostPort(ipn.IP.String(), "0"))
		if err != nil {
			continue
		}
		defer far.Close()
		if _, err := dial(context.Background(), "tcp", far.Addr().String()); err == nil || !strings.Contains(err.Error(), "non-loopback") {
			t.Fatalf("dial to %v: %v", far.Addr(), err)
		}
		return
	}
	t.Log("no non-loopback IPv4 address; refusal not exercised")
}
