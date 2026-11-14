package loopback

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// ErrRedirect refuses every redirect: the control plane API never redirects,
// and following one could resend a bearer token or a one-time token in
// plaintext (https → http) or to another host.
var ErrRedirect = errors.New("the control plane answered with a redirect; refusing to follow it (check the URL and the reverse proxy)")

type dialFunc = func(ctx context.Context, network, addr string) (net.Conn, error)

var plaintextDialer = &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}

// Client returns an HTTP client for the control plane at u, whose scheme is
// https/wss, or http/ws on a loopback host name. It verifies TLS with the
// system roots, or only with the certificates in caFile when that is set.
// For a plaintext scheme it uses no proxy and refuses any connection whose
// remote address is not loopback, whatever the host name resolved to. It
// follows no redirect and sets no overall timeout (WebSocket links are
// long-lived); callers that need one set Timeout on the result.
func Client(u *url.URL, caFile string) (*http.Client, error) {
	return newClient(u, caFile, plaintextDialer.DialContext)
}

func newClient(u *url.URL, caFile string, dial dialFunc) (*http.Client, error) {
	plaintext, err := CheckScheme(u)
	if err != nil {
		return nil, err
	}
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read CA file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("CA file %s contains no certificates", caFile)
		}
		tlsCfg.RootCAs = pool
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = tlsCfg
	if plaintext {
		tr.Proxy = nil // a proxy would be the remote address, and it sees the plaintext
		tr.DialContext = guard(dial)
	}
	return &http.Client{
		Transport:     tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return ErrRedirect },
	}, nil
}

// CheckScheme accepts https and wss, and http and ws only for a loopback
// host name (the dial check in Client then verifies the resolved address).
// It reports whether the scheme is plaintext. url.Parse lowercases the
// scheme, so any case is recognised.
func CheckScheme(u *url.URL) (plaintext bool, err error) {
	switch strings.ToLower(u.Scheme) {
	case "https", "wss":
		return false, nil
	case "http", "ws":
		if !Host(u.Hostname()) {
			return true, fmt.Errorf("plaintext %s:// is allowed only for a loopback control plane; use https:// for %s", strings.ToLower(u.Scheme), u.Host)
		}
		return true, nil
	default:
		return false, fmt.Errorf("unsupported scheme %q; use https://", u.Scheme)
	}
}

// Link validates a control plane base URL and returns the WebSocket URL of
// linkPath under it, with the client to dial it (see Client).
func Link(controlPlaneURL, caFile, linkPath string) (string, *http.Client, error) {
	u, err := linkURL(controlPlaneURL, linkPath)
	if err != nil {
		return "", nil, err
	}
	h, err := Client(u, caFile)
	if err != nil {
		return "", nil, err
	}
	return u.String(), h, nil
}

func linkURL(controlPlaneURL, linkPath string) (*url.URL, error) {
	u, err := url.Parse(controlPlaneURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("invalid control_plane_url")
	}
	plaintext, err := CheckScheme(u)
	if err != nil {
		return nil, err
	}
	u.Scheme = "wss"
	if plaintext {
		u.Scheme = "ws"
	}
	u.Path = strings.TrimRight(u.Path, "/") + linkPath
	u.RawPath = ""
	return u, nil
}

// guard dials with dial and closes the connection again unless its remote
// address is loopback. Clients use it for plaintext connections.
func guard(dial dialFunc) dialFunc {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := dial(ctx, network, addr)
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
