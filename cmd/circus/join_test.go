package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/EricWvi/circus/internal/auth"
	"github.com/EricWvi/circus/internal/controlplane"
	"github.com/EricWvi/circus/internal/credfile"
	"github.com/EricWvi/circus/internal/machineid"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/store/sqlite"
)

// joinHarness runs a real control plane in process (plaintext on loopback)
// and points the credential directory at a test directory. Tests using it
// set the environment, so they do not run in parallel.
type joinHarness struct {
	t      *testing.T
	db     string
	st     *sqlite.DB
	base   string
	cfgDir string
}

func newJoinHarness(t *testing.T) *joinHarness {
	t.Helper()
	dir := t.TempDir()
	h := &joinHarness{t: t, db: filepath.Join(dir, "circus.db"), cfgDir: filepath.Join(dir, "config")}
	t.Setenv("XDG_CONFIG_HOME", h.cfgDir)
	st, err := sqlite.Open(context.Background(), h.db, sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	h.st = st
	srv, err := controlplane.New(context.Background(), st, controlplane.Config{Listen: "127.0.0.1:0", DevInsecure: true, LogOutput: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	h.base = "http://" + srv.Addr().String()
	return h
}

// link runs `circus server enroll-token create` against the harness's
// database and returns the printed link.
func (h *joinHarness) link(role string) string {
	h.t.Helper()
	var out, errOut bytes.Buffer
	if err := enrollTokenCreate([]string{"--role", role, "--url", h.base, "--db", h.db}, &out, &errOut); err != nil {
		h.t.Fatalf("enroll-token create --role %s: %v", role, err)
	}
	return strings.TrimSpace(out.String())
}

func (h *joinHarness) join(args ...string) (string, error) {
	return h.joinStdin("", args...)
}

// joinStdin runs join with stdin holding the given text.
func (h *joinHarness) joinStdin(stdin string, args ...string) (string, error) {
	var out bytes.Buffer
	err := joinCommand(args, strings.NewReader(stdin), &out, io.Discard)
	return out.String(), err
}

// admin runs `circus server <args> --db <harness db>`.
func (h *joinHarness) admin(args ...string) (string, string, error) {
	var out, errOut bytes.Buffer
	err := serverCommand(append(args, "--db", h.db), &out, &errOut)
	return out.String(), errOut.String(), err
}

// cred loads the role's credential file, or reports it absent.
func (h *joinHarness) cred(role credfile.Role) (credfile.File, bool) {
	h.t.Helper()
	path := filepath.Join(h.cfgDir, "circus", string(role)+".json")
	f, err := credfile.Load(path)
	if err == credfile.ErrNotFound { //nolint:errorlint // sentinel returned as is
		return f, false
	}
	if err != nil {
		h.t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		h.t.Fatalf("%s: mode %v, %v", path, info.Mode(), err)
	}
	return f, true
}

// authenticates checks that the file's token authenticates as its role.
func (h *joinHarness) authenticates(f credfile.File) {
	h.t.Helper()
	r := httptest.NewRequest("GET", "/v1/whoami", nil)
	r.Header.Set("Authorization", "Bearer "+f.Token)
	p, err := auth.NewAuthenticator(h.st, auth.Options{}).Authenticate(context.Background(), r)
	if err != nil || p.ID != f.PrincipalID || auth.RoleName(p.Kind) != string(f.Role) {
		h.t.Fatalf("%s credential: principal %+v, %v", f.Role, p, err)
	}
	if want, _ := machineid.Get(); p.MachineID != want {
		h.t.Fatalf("%s machine id %q, want %q", f.Role, p.MachineID, want)
	}
}

func TestJoinBothRolesFromOneLink(t *testing.T) {
	h := newJoinHarness(t)
	link := h.link("both")
	if frag := link[strings.Index(link, "#")+1:]; strings.Count(frag, ".") != 1 {
		t.Fatalf("both-role link %q", link)
	}
	out, err := h.join(link, "--name", "box")
	if err != nil {
		t.Fatalf("join: %v\n%s", err, out)
	}
	d, okD := h.cred(credfile.RoleDaemon)
	w, okW := h.cred(credfile.RoleWorker)
	if !okD || !okW || d.PrincipalID == w.PrincipalID || d.Token == w.Token {
		t.Fatalf("credentials: daemon %v, worker %v", okD, okW)
	}
	h.authenticates(d)
	h.authenticates(w)
	// Both identities share the machine id and the name, in their own tables.
	for _, kind := range []store.PrincipalKind{store.KindDevice, store.KindWorker} {
		ids, _ := h.st.ListIdentities(context.Background(), kind, mustUser(t, h.st).ID)
		if len(ids) != 1 || ids[0].Name != "box" {
			t.Fatalf("%s identities: %+v", kind, ids)
		}
	}
	// Each token was single-use.
	if _, err := h.join(link, "--name", "box2", "--force"); err == nil || !strings.Contains(err.Error(), "refused the link") {
		t.Fatalf("reused link: %v", err)
	}
}

// The daemon enrolls, the worker fails: the daemon file stays, the error
// names the worker role and join fails.
func TestJoinBothPartialFailure(t *testing.T) {
	h := newJoinHarness(t)
	// A worker named "box" already exists for this user.
	tok, _, err := auth.CreateEnrollToken(context.Background(), h.st, mustUser(t, h.st).ID, store.KindWorker, time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := auth.Enroll(context.Background(), h.st, auth.EnrollRequest{Token: tok, Kind: store.KindWorker, Name: "box", MachineID: "other"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	out, err := h.join(h.link("both"), "--name", "box")
	if err == nil {
		t.Fatalf("join succeeded:\n%s", out)
	}
	msg := err.Error()
	if !strings.Contains(msg, "enrolling the worker role failed") || !strings.Contains(msg, "[daemon]") || !strings.Contains(msg, "--name") {
		t.Fatalf("error does not say which role failed: %v", err)
	}
	d, ok := h.cred(credfile.RoleDaemon)
	if !ok {
		t.Fatal("daemon credential not kept")
	}
	h.authenticates(d)
	if _, ok := h.cred(credfile.RoleWorker); ok {
		t.Fatal("worker credential written")
	}
	if !strings.Contains(out, `Joined as daemon "box"`) {
		t.Fatalf("output: %q", out)
	}
}

func TestJoinSingleTokenLink(t *testing.T) {
	h := newJoinHarness(t)
	worker := h.link("worker")
	if _, err := h.join(worker, "--role", "both"); err == nil || !strings.Contains(err.Error(), "one token") {
		t.Fatalf("--role both on a one-token link: %v", err)
	}
	if _, err := h.join(worker, "--role", "worker", "--name", "w"); err != nil {
		t.Fatal(err)
	}
	w, ok := h.cred(credfile.RoleWorker)
	if !ok {
		t.Fatal("no worker credential")
	}
	h.authenticates(w)
	if _, ok := h.cred(credfile.RoleDaemon); ok {
		t.Fatal("daemon credential written for a worker link")
	}
	// Without --role a one-token link enrolls the daemon.
	if _, err := h.join(h.link("daemon"), "--name", "d"); err != nil {
		t.Fatal(err)
	}
	if d, ok := h.cred(credfile.RoleDaemon); !ok {
		t.Fatal("no daemon credential")
	} else {
		h.authenticates(d)
	}
}

// --role selects one role of a two-token link; the other token stays usable.
func TestJoinRoleFilterOnBothLink(t *testing.T) {
	h := newJoinHarness(t)
	link := h.link("both")
	if _, err := h.join(link, "--role", "worker", "--name", "box"); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.cred(credfile.RoleDaemon); ok {
		t.Fatal("daemon enrolled with --role worker")
	}
	if _, err := h.join(link, "--role", "daemon", "--name", "box"); err != nil {
		t.Fatalf("daemon token of the same link: %v", err)
	}
	for _, role := range []credfile.Role{credfile.RoleDaemon, credfile.RoleWorker} {
		f, ok := h.cred(role)
		if !ok {
			t.Fatalf("no %s credential", role)
		}
		h.authenticates(f)
	}
}

func TestParseJoinLink(t *testing.T) {
	a, b := strings.Repeat("A", auth.TokenLen), strings.Repeat("B", auth.TokenLen-1)+"A"
	for _, c := range []struct {
		link string
		ok   bool
	}{
		{"https://h:1/join#" + a, true},
		{"https://h:1/join#" + a + "." + b, true},
		{"http://127.0.0.1:1/join#" + a, true},
		{"http://h:1/join#" + a, false},
		{"https://h:1/join#" + a + ".", false},
		{"https://h:1/join#" + a + "." + b + "." + a, false},
		{"https://h:1/join#" + a[:40], false},
		{"https://h:1/other#" + a, false},
		{"https://h:1/join", false},
	} {
		if _, err := parseJoinLink(c.link); (err == nil) != c.ok {
			t.Errorf("%s: %v", c.link, err)
		}
	}
}

func mustUser(t *testing.T, st store.Store) store.User {
	t.Helper()
	u, err := st.GetUserByName(context.Background(), auth.DefaultUserName)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// M-2: the link can come from stdin, with "-" or without an argument, so the
// token never reaches argv.
func TestJoinReadsLinkFromStdin(t *testing.T) {
	h := newJoinHarness(t)
	if _, err := h.joinStdin(h.link("daemon")+"\n", "-", "--name", "a"); err != nil {
		t.Fatalf("join -: %v", err)
	}
	if d, ok := h.cred(credfile.RoleDaemon); !ok {
		t.Fatal("no daemon credential")
	} else {
		h.authenticates(d)
	}
	// No argument, no trailing newline, flags only.
	if _, err := h.joinStdin("  "+h.link("worker"), "--role", "worker", "--name", "b"); err != nil {
		t.Fatalf("join without a link argument: %v", err)
	}
	if _, ok := h.cred(credfile.RoleWorker); !ok {
		t.Fatal("no worker credential")
	}
	if _, err := h.joinStdin("", "--force"); err == nil || !strings.Contains(err.Error(), "no join link on stdin") {
		t.Fatalf("empty stdin: %v", err)
	}
	if _, err := h.joinStdin("x", "a", "b"); err == nil {
		t.Fatal("two positional links accepted")
	}
}

// M-1 end to end through the CLI: revoke frees the name, list shows it gone,
// and the machine joins again under the same name.
func TestRejoinSameNameAfterRevoke(t *testing.T) {
	h := newJoinHarness(t)
	if _, err := h.join(h.link("daemon"), "--name", "laptop"); err != nil {
		t.Fatal(err)
	}
	old, _ := h.cred(credfile.RoleDaemon)
	out, errOut, err := h.admin("device", "list")
	if err != nil || !strings.Contains(out, "laptop") || !strings.Contains(out, old.PrincipalID) ||
		!strings.Contains(out, "LAST CONNECTED") || !strings.Contains(errOut, "database: "+h.db) {
		t.Fatalf("device list: %v\n%s%s", err, out, errOut)
	}
	if out, _, _ := h.admin("worker", "list"); strings.Contains(out, "laptop") {
		t.Fatalf("worker list shows the device:\n%s", out)
	}

	// While live, the same name is refused even with --force.
	machine, _ := machineid.Get()
	if _, err := h.join(h.link("daemon"), "--name", "laptop", "--force"); err == nil ||
		!strings.Contains(err.Error(), "circus server device list") || !strings.Contains(err.Error(), "revoke laptop") ||
		!strings.Contains(err.Error(), "This machine's id begins with "+machine[:12]) {
		t.Fatalf("same name while live: %v", err)
	}
	if out, _, err := h.admin("device", "revoke", "laptop"); err != nil || !strings.Contains(out, "can join again") {
		t.Fatalf("revoke: %v %q", err, out)
	}
	if out, _, _ := h.admin("device", "list"); strings.Contains(out, "laptop") {
		t.Fatalf("revoked device still listed:\n%s", out)
	}
	if _, err := h.joinStdin(h.link("daemon"), "--name", "laptop", "--force"); err != nil {
		t.Fatalf("rejoin after revoke: %v", err)
	}
	fresh, _ := h.cred(credfile.RoleDaemon)
	if fresh.PrincipalID == old.PrincipalID {
		t.Fatal("rejoin kept the old identity")
	}
	h.authenticates(fresh)
}

// L-7: admin commands never create a database.
func TestAdminCommandsNeedExistingDatabase(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope", "circus.db")
	for _, args := range [][]string{
		{"enroll-token", "create", "--role", "daemon", "--url", "https://h:1"},
		{"device", "revoke", "x"},
		{"worker", "list"},
	} {
		var out, errOut bytes.Buffer
		err := serverCommand(append(args, "--db", missing), &out, &errOut)
		if err == nil || !strings.Contains(err.Error(), missing) {
			t.Errorf("%v: %v", args, err)
		}
	}
	if _, err := os.Stat(filepath.Dir(missing)); !os.IsNotExist(err) {
		t.Fatalf("an admin command created %s", filepath.Dir(missing))
	}
}

// M21: doctor fails when the server's principal or role differs from the
// credential file.
func TestDoctorPrincipalMismatch(t *testing.T) {
	h := newJoinHarness(t)
	if _, err := h.join(h.link("both"), "--name", "box"); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := doctorCommand(nil, &out, io.Discard); err != nil || strings.Count(out.String(), "credential accepted") != 2 {
		t.Fatalf("doctor: %v\n%s", err, &out)
	}
	path := filepath.Join(h.cfgDir, "circus", "daemon.json")
	d, _ := h.cred(credfile.RoleDaemon)
	w, _ := h.cred(credfile.RoleWorker)
	for name, edit := range map[string]func(*credfile.File){
		"principal": func(f *credfile.File) { f.PrincipalID = "someone-else" },
		"role":      func(f *credfile.File) { f.Token = w.Token },
	} {
		f := d
		edit(&f)
		if err := credfile.Save(path, f); err != nil {
			t.Fatal(err)
		}
		out.Reset()
		err := doctorCommand([]string{"--role", "daemon"}, &out, io.Discard)
		if err == nil || !strings.Contains(out.String(), "not this file's principal") {
			t.Errorf("%s mismatch: %v\n%s", name, err, &out)
		}
	}
}

// L-2: the CLI client follows no redirect, so a token is never resent to
// another URL.
func TestClientRefusesRedirects(t *testing.T) {
	var hit atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit.Store(true) }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/v1/whoami", http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	client, err := httpClient(redirect.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	req, _ := http.NewRequest(http.MethodGet, redirect.URL+"/v1/whoami", nil)
	req.Header.Set("Authorization", "Bearer secret")
	if _, err := client.Do(req); !errors.Is(err, errRedirect) {
		t.Fatalf("redirect followed or wrong error: %v", err)
	}
	if hit.Load() {
		t.Fatal("redirect target was contacted")
	}
}

// NL-3: an http:// control plane URL gets the loopback dial check: a host
// that resolves to a non-loopback address is refused after the dial, before
// any request is written.
func TestClientLoopbackDialCheck(t *testing.T) {
	var hit atomic.Bool
	handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit.Store(true) })
	local := httptest.NewServer(handler)
	defer local.Close()
	// Upper-case scheme too (NL-4): still recognised as plaintext.
	client, err := httpClient(strings.Replace(local.URL, "http://", "HTTP://", 1), "")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Get(local.URL + "/x")
	if err != nil {
		t.Fatalf("loopback: %v", err)
	}
	resp.Body.Close()
	client.CloseIdleConnections()

	// A listener on this machine's own non-loopback address stands in for a
	// "localhost" that resolves elsewhere: the client built for an http://
	// base URL must refuse it at dial time, whatever the request URL says.
	far := nonLoopbackListener(t)
	if far == nil {
		t.Skip("no non-loopback IPv4 address on this machine")
	}
	srv := &http.Server{Handler: handler}
	go srv.Serve(far)
	defer srv.Close()
	hit.Store(false)
	_, port, _ := net.SplitHostPort(far.Addr().String())
	client, err = httpClient("http://localhost:"+port, "")
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	if _, err := client.Get("http://" + far.Addr().String() + "/x"); err == nil || !strings.Contains(err.Error(), "non-loopback") {
		t.Fatalf("non-loopback remote over http: %v", err)
	}
	if hit.Load() {
		t.Fatal("request reached the non-loopback server")
	}
	// The same URL as a base is refused before any dial.
	if _, err := httpClient("HTTP://"+far.Addr().String(), ""); err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("non-loopback http base URL accepted: %v", err)
	}
}

func nonLoopbackListener(t *testing.T) net.Listener {
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok || ipn.IP.IsLoopback() || ipn.IP.To4() == nil || ipn.IP.IsLinkLocalUnicast() {
			continue
		}
		if ln, err := net.Listen("tcp", net.JoinHostPort(ipn.IP.String(), "0")); err == nil {
			return ln
		}
	}
	return nil
}

// NL-4: doctor validates the URL in the credential file before sending the
// token, and accepts an upper-case scheme on loopback.
func TestDoctorValidatesFileURL(t *testing.T) {
	h := newJoinHarness(t)
	if _, err := h.join(h.link("daemon"), "--name", "box"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(h.cfgDir, "circus", "daemon.json")
	d, _ := h.cred(credfile.RoleDaemon)
	for raw, ok := range map[string]bool{
		strings.Replace(h.base, "http://", "HTTP://", 1): true,
		"HTTP://example.com:8443":                        false,
		"ftp://example.com":                              false,
	} {
		f := d
		f.ControlPlaneURL = raw
		if err := credfile.Save(path, f); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		err := doctorCommand(nil, &out, io.Discard)
		if (err == nil) != ok {
			t.Errorf("%s: %v\n%s", raw, err, &out)
		}
		if !ok && !strings.Contains(out.String(), "control_plane_url") {
			t.Errorf("%s: no URL diagnosis\n%s", raw, &out)
		}
	}
}
