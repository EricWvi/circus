package workerclient

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/EricWvi/circus/internal/credfile"
)

func transport(f credfile.File) (string, *http.Client, error) {
	u, err := url.Parse(f.ControlPlaneURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", nil, errors.New("worker link: invalid control_plane_url")
	}
	switch u.Scheme {
	case "https", "wss":
		u.Scheme = "wss"
	case "http", "ws":
		host := u.Hostname()
		ip := net.ParseIP(host)
		if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return "", nil, errors.New("worker link: plaintext requires a loopback host")
		}
		u.Scheme = "ws"
	default:
		return "", nil, errors.New("worker link: URL must use https, wss, http or ws")
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/v1/link/worker"
	u.RawPath = ""
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if f.CAFile != "" {
		data, err := os.ReadFile(f.CAFile)
		if err != nil {
			return "", nil, fmt.Errorf("worker link CA: %w", err)
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(data) {
			return "", nil, errors.New("worker link: CA file contains no certificates")
		}
		tlsConfig.RootCAs = roots
	}
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.TLSClientConfig = tlsConfig
	return u.String(), &http.Client{Transport: t, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("worker link: redirects are disabled") }}, nil
}
