package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/EricWvi/circus/internal/credfile"
	"github.com/EricWvi/circus/internal/testutil/leakcheck"
)

// originalEnv is the environment before TestMain lets leakcheck point HOME
// and the XDG directories at a private directory; package initialization
// runs first. buildCircus asks the go command for its caches under it, so
// the test's build reuses the real build and module caches.
var originalEnv = os.Environ()

// buildCircus builds this command into dir/bin and returns the binary's
// path. The build gets dir as HOME, so go's own files (telemetry counters)
// stay in the test's directory.
func buildCircus(t *testing.T, dir string) string {
	t.Helper()
	gobin := filepath.Join(runtime.GOROOT(), "bin", "go")
	envCmd := exec.Command(gobin, "env", "-json", "GOCACHE", "GOMODCACHE")
	envCmd.Env = originalEnv
	out, err := envCmd.Output()
	if err != nil {
		t.Fatalf("go env: %v", err)
	}
	var caches struct{ GOCACHE, GOMODCACHE string }
	if err := json.Unmarshal(out, &caches); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "bin", "circus")
	cmd := exec.Command(gobin, "build", "-o", bin, ".")
	cmd.Env = append(os.Environ(), "GOCACHE="+caches.GOCACHE, "GOMODCACHE="+caches.GOMODCACHE,
		"HOME="+dir, "XDG_CONFIG_HOME="+filepath.Join(dir, "config"), "XDG_CACHE_HOME="+filepath.Join(dir, "cache"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

// processEnv runs circus processes with a private HOME, config and data
// directory below root.
type processEnv struct {
	t   *testing.T
	bin string
	env []string
	dir string
}

func (p *processEnv) command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, p.bin, args...)
	cmd.Env, cmd.Dir = p.env, p.dir
	return cmd
}

// run runs a short-lived circus command to completion.
func (p *processEnv) run(args ...string) (stdout, stderr string, err error) {
	p.t.Helper()
	return p.runStdin("", args...)
}

// runStdin runs a short-lived circus command with stdin holding in.
func (p *processEnv) runStdin(in string, args ...string) (stdout, stderr string, err error) {
	p.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var o, e bytes.Buffer
	cmd := p.command(ctx, args...)
	cmd.Stdin = strings.NewReader(in)
	cmd.Stdout, cmd.Stderr = &o, &e
	err = cmd.Run()
	return o.String(), e.String(), err
}

// lineLog collects a process's stderr lines.
type lineLog struct {
	mu    sync.Mutex
	lines []string
	added chan struct{}
}

func (l *lineLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

// waitFor returns the first line matching match, waiting up to d.
func (l *lineLog) waitFor(d time.Duration, match func(string) bool) (string, bool) {
	deadline := time.After(d)
	for {
		for _, line := range l.all() {
			if match(line) {
				return line, true
			}
		}
		select {
		case <-l.added:
		case <-deadline:
			return "", false
		}
	}
}

// TestServerJoinProcess drives the real binary end to end: a TLS server on
// loopback, an enrollment token, `circus join` with --ca-file, bearer
// authentication (200, 401 before any upgrade, 403 after revocation),
// `circus doctor`, and a clean SIGTERM shutdown within its deadline.
func TestServerJoinProcess(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the circus binary")
	}
	root := t.TempDir()
	leakcheck.Track(t, root)
	home := filepath.Join(root, "home")
	for _, d := range []string{home, filepath.Join(root, "build"), filepath.Join(root, "tls")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	p := &processEnv{t: t, bin: buildCircus(t, filepath.Join(root, "build")), dir: root}
	p.env = append(os.Environ(),
		"HOME="+home,
		"XDG_CONFIG_HOME="+filepath.Join(home, "config"),
		"XDG_DATA_HOME="+filepath.Join(home, "data"),
		"XDG_CACHE_HOME="+filepath.Join(home, "cache"),
		"XDG_STATE_HOME="+filepath.Join(home, "state"),
	)
	certFile, keyFile := writeTestCert(t, filepath.Join(root, "tls"))
	began := time.Now()
	defer func() { t.Logf("process test took %v", time.Since(began)) }()

	t.Run("dev-insecure refused off loopback", func(t *testing.T) {
		_, stderr, err := p.run("server", "--dev-insecure", "--listen", "0.0.0.0:0")
		var exit *exec.ExitError
		if !errors.As(err, &exit) || !strings.Contains(stderr, "loopback") {
			t.Fatalf("err %v, stderr %q", err, stderr)
		}
	})
	t.Run("plaintext link refused off loopback", func(t *testing.T) {
		_, stderr, err := p.run("join", "http://example.com:8443/join#"+strings.Repeat("A", 43))
		if err == nil || !strings.Contains(stderr, "loopback") {
			t.Fatalf("err %v, stderr %q", err, stderr)
		}
	})

	// Start the server and stop exactly that PID at the end.
	srv := p.command(context.Background(), "server", "--listen", "127.0.0.1:0",
		"--tls-cert", certFile, "--tls-key", keyFile, "--shutdown-timeout", "3s")
	stderr, err := srv.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	logs := &lineLog{added: make(chan struct{}, 1)}
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			logs.mu.Lock()
			logs.lines = append(logs.lines, sc.Text())
			logs.mu.Unlock()
			select {
			case logs.added <- struct{}{}:
			default:
			}
		}
	}()
	waited := make(chan error, 1)
	exited := false
	t.Cleanup(func() {
		if !exited {
			_ = srv.Process.Kill()
			<-readerDone
			<-waited
		}
	})
	go func() { <-readerDone; waited <- srv.Wait() }()

	line, ok := logs.waitFor(20*time.Second, func(l string) bool { return strings.Contains(l, "control plane listening") })
	if !ok {
		t.Fatalf("server did not start: %q", logs.all())
	}
	var listening struct{ Addr string }
	if err := json.Unmarshal([]byte(line), &listening); err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(listening.Addr)
	base := "https://127.0.0.1:" + port

	pemBytes, _ := os.ReadFile(certFile)
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(pemBytes)
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	defer client.CloseIdleConnections()
	get := func(path, token string) int {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, base+path, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}
	if s := get("/readyz", ""); s != http.StatusOK {
		t.Fatalf("readyz %d", s)
	}

	// Enrollment token, written to the database directly while the server runs.
	link, tokenErr, err := p.run("server", "enroll-token", "create", "--role", "daemon", "--ttl", "10m", "--url", base)
	if err != nil {
		t.Fatalf("enroll-token create: %v\n%s", err, tokenErr)
	}
	link = strings.TrimSpace(link)
	if !strings.HasPrefix(link, base+"/join#") || strings.Contains(tokenErr, link[strings.Index(link, "#")+1:]) {
		t.Fatalf("link %q, stderr %q", link, tokenErr)
	}

	// Join without the CA fails on verification and says how to fix it.
	if _, out, err := p.run("join", link, "--name", "laptop"); err == nil || !strings.Contains(out, "--ca-file") {
		t.Fatalf("join without --ca-file: %v %q", err, out)
	}
	if !strings.Contains(tokenErr, "\n  circus join\n") {
		t.Fatalf("hint does not use the stdin form: %q", tokenErr)
	}
	// The recommended form: the link on stdin, never in argv.
	out, joinErr, err := p.runStdin(link+"\n", "join", "-", "--name", "laptop", "--ca-file", certFile)
	if err != nil {
		t.Fatalf("join: %v\n%s%s", err, out, joinErr)
	}
	credPath := filepath.Join(home, "config", "circus", "daemon.json")
	info, err := os.Stat(credPath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("credential file: %v, mode %v", err, info.Mode())
	}
	cred, err := credfile.Load(credPath)
	if err != nil || cred.Role != credfile.RoleDaemon || cred.ControlPlaneURL != base || cred.CAFile != certFile || cred.PrincipalID == "" {
		t.Fatalf("credential = %+v, %v", cred, err)
	}

	// The long-term token authenticates; a wrong one gets 401 before any upgrade.
	if s := get("/v1/whoami", cred.Token); s != http.StatusOK {
		t.Fatalf("whoami with credential: %d", s)
	}
	wrong := strings.Repeat("A", 43)
	if s := get("/v1/whoami", wrong); s != http.StatusUnauthorized {
		t.Fatalf("whoami with wrong token: %d", s)
	}
	wsURL := "wss://127.0.0.1:" + port + "/v1/whoami"
	ws, resp, err := websocket.Dial(context.Background(), wsURL, &websocket.DialOptions{
		HTTPClient: client, HTTPHeader: http.Header{"Authorization": {"Bearer " + wrong}},
	})
	if ws != nil {
		ws.CloseNow()
	}
	if err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("upgrade with wrong token: %v, %v", resp, err)
	}
	if out, _, err := p.run("doctor"); err != nil || !strings.Contains(out, "credential accepted") {
		t.Fatalf("doctor: %v\n%s", err, out)
	}

	// The link is single-use; a second join with a fresh link and the same
	// name is refused with a hint to choose another name.
	if _, out, err := p.run("join", link, "--name", "other", "--ca-file", certFile, "--force"); err == nil || !strings.Contains(out, "refused the link") {
		t.Fatalf("reused link: %v %q", err, out)
	}
	link2, _, err := p.run("server", "enroll-token", "create", "--role", "daemon", "--url", base)
	if err != nil {
		t.Fatal(err)
	}
	if _, out, err := p.run("join", strings.TrimSpace(link2), "--name", "laptop", "--ca-file", certFile); err == nil || !strings.Contains(out, "--force") {
		t.Fatalf("join over existing credential: %v %q", err, out)
	}
	if _, out, err := p.run("join", strings.TrimSpace(link2), "--name", "laptop", "--ca-file", certFile, "--force"); err == nil || !strings.Contains(out, "--name") {
		t.Fatalf("duplicate name: %v %q", err, out)
	}

	// Revocation: 403 for the next request, and doctor says so.
	if out, errOut, err := p.run("server", "device", "list"); err != nil || !strings.Contains(out, cred.PrincipalID) {
		t.Fatalf("device list: %v %q %q", err, out, errOut)
	}
	if out, errOut, err := p.run("server", "device", "revoke", "laptop"); err != nil || !strings.Contains(out, "Revoked 1") {
		t.Fatalf("revoke: %v %q %q", err, out, errOut)
	}
	if s := get("/v1/whoami", cred.Token); s != http.StatusForbidden {
		t.Fatalf("whoami after revoke: %d", s)
	}
	if out, _, err := p.run("doctor"); err == nil || !strings.Contains(out, "revoked") {
		t.Fatalf("doctor after revoke: %v\n%s", err, out)
	}

	// Revocation freed the name: the unused second link now joins as
	// "laptop" again, and the new credential works.
	if out, errOut, err := p.runStdin(strings.TrimSpace(link2), "join", "--name", "laptop", "--ca-file", certFile, "--force"); err != nil {
		t.Fatalf("rejoin after revoke: %v\n%s%s", err, out, errOut)
	}
	rejoined, err := credfile.Load(credPath)
	if err != nil || rejoined.PrincipalID == cred.PrincipalID {
		t.Fatalf("rejoined credential = %+v, %v", rejoined, err)
	}
	if s := get("/v1/whoami", rejoined.Token); s != http.StatusOK {
		t.Fatalf("whoami after rejoin: %d", s)
	}
	if s := get("/v1/whoami", cred.Token); s != http.StatusForbidden {
		t.Fatalf("old credential after rejoin: %d", s)
	}
	if out, _, err := p.run("doctor"); err != nil || !strings.Contains(out, "credential accepted") {
		t.Fatalf("doctor after rejoin: %v\n%s", err, out)
	}

	// SIGTERM: clean exit within the 3s shutdown deadline.
	start := time.Now()
	if err := srv.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-waited:
		exited = true
		if err != nil {
			t.Fatalf("server exit: %v\n%s", err, strings.Join(logs.all(), "\n"))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("server did not exit after SIGTERM")
	}
	if d := time.Since(start); d > 4*time.Second {
		t.Fatalf("shutdown took %v", d)
	}
	var instance string
	all := logs.all()
	for _, line := range all {
		var rec struct {
			InstanceID string `json:"instance_id"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil || rec.InstanceID == "" {
			t.Fatalf("log line without instance_id: %q", line)
		}
		if instance != "" && rec.InstanceID != instance {
			t.Fatalf("instance id changed within one process: %q", line)
		}
		instance = rec.InstanceID
		if strings.Contains(line, cred.Token) || strings.Contains(line, rejoined.Token) || strings.Contains(line, link[strings.Index(link, "#")+1:]) {
			t.Fatal("a token reached the server log")
		}
	}
	if !strings.Contains(strings.Join(all, "\n"), "shutdown complete") {
		t.Fatalf("no shutdown line:\n%s", strings.Join(all, "\n"))
	}
}

// writeTestCert writes a self-signed certificate for 127.0.0.1 and
// localhost, and returns the cert and key paths.
func writeTestCert(t *testing.T, dir string) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "circus test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, DNSNames: []string{"localhost"},
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA: true, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPath, keyPath := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}
