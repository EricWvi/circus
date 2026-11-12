package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/EricWvi/circus/internal/loopback"
	"github.com/EricWvi/circus/internal/store/sqlite"
)

// parseArgs parses flags that may appear before, between or after
// positional arguments ("revoke laptop --db x") and returns the positionals.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		if args[0] == "--" {
			return append(pos, args[1:]...), nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

// defaultDBPath is $XDG_DATA_HOME/circus/circus.db, falling back to
// ~/.local/share/circus/circus.db.
func defaultDBPath() string {
	if x := os.Getenv("XDG_DATA_HOME"); x != "" && filepath.IsAbs(x) {
		return filepath.Join(x, "circus", "circus.db")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".local", "share", "circus", "circus.db")
	}
	return "circus.db"
}

// openStore opens the control plane database, creating its directory 0700.
func openStore(ctx context.Context, path string) (*sqlite.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	return sqlite.Open(ctx, path, sqlite.Options{})
}

// openExistingStore opens the control plane database for an admin command.
// Unlike openStore it never creates one: a mistyped --db, or a database
// that belongs to another user's HOME, would otherwise get tokens or
// revocations the server never sees. It reports the path it used on errOut.
func openExistingStore(ctx context.Context, path string, errOut io.Writer) (*sqlite.DB, error) {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("no control plane database at %s; pass --db with the path the server uses", path)
		}
		return nil, err
	}
	fmt.Fprintf(errOut, "database: %s\n", path)
	return sqlite.Open(ctx, path, sqlite.Options{})
}

// checkBaseURL accepts an https:// base URL, or http:// on a loopback host
// only (D43 item 7), with no path beyond "/", query or fragment. It returns
// the URL without a trailing slash.
func checkBaseURL(u *url.URL) (string, error) {
	if u.Host == "" || u.User != nil || u.RawQuery != "" {
		return "", fmt.Errorf("%q is not a base URL like https://host:port", u.Redacted())
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !loopback.Host(u.Hostname()) {
			return "", fmt.Errorf("plaintext http:// is allowed only for a loopback control plane; use https:// for %s", u.Host)
		}
	default:
		return "", fmt.Errorf("unsupported scheme %q; use https://", u.Scheme)
	}
	return u.Scheme + "://" + u.Host, nil
}

// errRedirect refuses every redirect: the control plane API never
// redirects, and following one could resend a bearer token or a one-time
// token in plaintext (https → http) or to another host.
var errRedirect = errors.New("the control plane answered with a redirect; refusing to follow it (check the URL and the reverse proxy)")

// httpClient talks to the control plane at baseURL, which must pass
// checkBaseURL. It verifies TLS with the system roots, or only with the
// certificates in caFile when it is set; for an http:// base URL (scheme
// compared after parsing, so in any case) it refuses any connection whose
// remote address is not loopback, whatever the host name resolved to. It
// follows no redirect.
func httpClient(baseURL, caFile string) (*http.Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("control plane URL: %w", err)
	}
	if _, err := checkBaseURL(u); err != nil {
		return nil, fmt.Errorf("control plane URL: %w", err)
	}
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read --ca-file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("--ca-file %s holds no PEM certificate", caFile)
		}
		tlsCfg.RootCAs = pool
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = tlsCfg
	if u.Scheme == "http" { // url.Parse lowercases the scheme
		tr.Proxy = nil // a proxy would be the remote address, and it sees the plaintext
		tr.DialContext = loopback.DialContext(&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second})
	}
	return &http.Client{
		Transport:     tr,
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return errRedirect },
	}, nil
}

// tlsHint explains a certificate verification failure.
func tlsHint(err error) string {
	var unknown x509.UnknownAuthorityError
	var invalid x509.CertificateInvalidError
	var hostname x509.HostnameError
	var verify *tls.CertificateVerificationError
	if errors.As(err, &unknown) || errors.As(err, &invalid) || errors.As(err, &hostname) || errors.As(err, &verify) {
		return " (the control plane's certificate did not verify; for a self-signed certificate pass --ca-file with its CA)"
	}
	return ""
}
