package auth_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"go.uber.org/goleak"

	"github.com/EricWvi/circus/internal/auth"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/store/sqlite"
	"github.com/EricWvi/circus/internal/testutil/leakcheck"
)

func TestMain(m *testing.M) {
	code := leakcheck.Run(m, leakcheck.Config{})
	if err := goleak.Find(); err != nil {
		println(err.Error())
		code = 1
	}
	os.Exit(code)
}

var t0 = time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)

type fixture struct {
	st   *sqlite.DB
	user store.User
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "circus.db"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	u, err := auth.EnsureUser(ctx, st, auth.DefaultUserName, t0)
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{st: st, user: u}
}

func (f *fixture) token(t *testing.T, kind store.PrincipalKind, ttl time.Duration, now time.Time) string {
	t.Helper()
	tok, _, err := auth.CreateEnrollToken(context.Background(), f.st, f.user.ID, kind, ttl, now)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func (f *fixture) enroll(token string, kind store.PrincipalKind, name string, now time.Time) (auth.Enrollment, error) {
	return auth.Enroll(context.Background(), f.st, auth.EnrollRequest{
		Token: token, Kind: kind, Name: name, Hostname: "host.example", MachineID: "m-1",
		OS: "linux", Arch: "amd64", ClientVersion: "test",
	}, now)
}

func (f *fixture) mustEnroll(t *testing.T, kind store.PrincipalKind, name string) auth.Enrollment {
	t.Helper()
	en, err := f.enroll(f.token(t, kind, 0, t0), kind, name, t0)
	if err != nil {
		t.Fatal(err)
	}
	return en
}

func bearer(token string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/v1/link/daemon", nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	return r
}

func TestTokenShape(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		tok, err := auth.NewToken()
		if err != nil {
			t.Fatal(err)
		}
		if len(tok) != auth.TokenLen || !auth.WellFormed(tok) || seen[tok] {
			t.Fatalf("bad or repeated token %d chars", len(tok))
		}
		seen[tok] = true
		if len(auth.HashToken(tok)) != 32 {
			t.Fatal("hash is not SHA-256")
		}
	}
	for _, bad := range []string{"", "short", strings.Repeat("A", auth.TokenLen-1), strings.Repeat("=", auth.TokenLen), strings.Repeat("A", auth.TokenLen) + "A"} {
		if auth.WellFormed(bad) {
			t.Errorf("WellFormed(%q) = true", bad)
		}
	}
}

func TestEnrollCreatesIdentityAndCredential(t *testing.T) {
	f := newFixture(t)
	en := f.mustEnroll(t, store.KindDevice, "laptop")
	if en.Identity.Kind != store.KindDevice || en.Identity.UserID != f.user.ID || en.Identity.Name != "laptop" ||
		en.Identity.MachineID != "m-1" || en.Identity.Hostname != "host.example" || !auth.WellFormed(en.Token) {
		t.Fatalf("enrollment = %+v", en.Identity)
	}
	got, err := f.st.GetIdentityByName(context.Background(), store.KindDevice, f.user.ID, "laptop")
	if err != nil || got.ID != en.Identity.ID {
		t.Fatalf("identity row: %+v, %v", got, err)
	}
	cred, err := f.st.LookupCredential(context.Background(), auth.HashToken(en.Token))
	if err != nil || cred.ID != en.CredentialID || cred.PrincipalID != en.Identity.ID || cred.Kind != store.KindDevice {
		t.Fatalf("credential row: %+v, %v", cred, err)
	}
	a := auth.NewAuthenticator(f.st, auth.Options{Now: func() time.Time { return t0 }})
	p, err := a.Authenticate(context.Background(), bearer(en.Token))
	want := auth.Principal{Kind: store.KindDevice, ID: en.Identity.ID, Name: "laptop", UserID: f.user.ID, MachineID: "m-1", CredentialID: en.CredentialID}
	if err != nil || p != want {
		t.Fatalf("Authenticate = %+v, %v; want %+v", p, err, want)
	}
}

// F20: reused, expired, unknown and wrong-role tokens all fail with the same
// error value, so nothing tells the caller which case it hit.
func TestEnrollTokenFailuresIndistinguishable(t *testing.T) {
	f := newFixture(t)
	used := f.token(t, store.KindDevice, time.Hour, t0)
	if _, err := f.enroll(used, store.KindDevice, "first", t0); err != nil {
		t.Fatal(err)
	}
	expired := f.token(t, store.KindDevice, time.Hour, t0)
	wrongRole := f.token(t, store.KindWorker, time.Hour, t0)
	unknown, _ := auth.NewToken()

	cases := map[string]func() error{
		"reused": func() error { _, err := f.enroll(used, store.KindDevice, "second", t0); return err },
		"expired at expiry": func() error {
			_, err := f.enroll(expired, store.KindDevice, "late", t0.Add(time.Hour))
			return err
		},
		"unknown":    func() error { _, err := f.enroll(unknown, store.KindDevice, "x", t0); return err },
		"malformed":  func() error { _, err := f.enroll("not-a-token", store.KindDevice, "x", t0); return err },
		"wrong role": func() error { _, err := f.enroll(wrongRole, store.KindDevice, "x", t0); return err },
	}
	for name, run := range cases {
		err := run()
		if err != auth.ErrEnrollRejected { //nolint:errorlint // identity, not just Is: no wrapping detail
			t.Errorf("%s: err = %v, want exactly ErrEnrollRejected", name, err)
		}
	}
	ids, err := f.st.ListIdentities(context.Background(), store.KindDevice, f.user.ID)
	if err != nil || len(ids) != 1 {
		t.Fatalf("devices = %d, %v; want only the first enrollment", len(ids), err)
	}
}

func TestEnrollTokenValidUntilExpiry(t *testing.T) {
	f := newFixture(t)
	tok := f.token(t, store.KindDevice, time.Hour, t0)
	if _, err := f.enroll(tok, store.KindDevice, "edge", t0.Add(time.Hour-time.Millisecond)); err != nil {
		t.Fatalf("enroll 1ms before expiry: %v", err)
	}
}

// F20: of many concurrent uses of one token exactly one succeeds.
func TestEnrollTokenConcurrentUse(t *testing.T) {
	f := newFixture(t)
	tok := f.token(t, store.KindDevice, time.Hour, t0)
	const n = 16
	var wg sync.WaitGroup
	var ok atomic.Int32
	errs := make(chan error, n)
	start := make(chan struct{})
	for i := range n {
		wg.Go(func() {
			<-start
			_, err := f.enroll(tok, store.KindDevice, "racer-"+string(rune('a'+i)), t0)
			if err == nil {
				ok.Add(1)
			} else {
				errs <- err
			}
		})
	}
	close(start)
	wg.Wait()
	close(errs)
	if ok.Load() != 1 {
		t.Fatalf("%d enrollments succeeded, want exactly 1", ok.Load())
	}
	for err := range errs {
		if err != auth.ErrEnrollRejected { //nolint:errorlint
			t.Errorf("loser err = %v, want ErrEnrollRejected", err)
		}
	}
	ids, _ := f.st.ListIdentities(context.Background(), store.KindDevice, f.user.ID)
	if len(ids) != 1 {
		t.Fatalf("%d device identities, want 1", len(ids))
	}
}

// A rejected role or a taken name rolls the whole transaction back, so the
// token survives for a corrected retry.
func TestEnrollFailureKeepsToken(t *testing.T) {
	f := newFixture(t)
	f.mustEnroll(t, store.KindDevice, "laptop")

	tok := f.token(t, store.KindDevice, time.Hour, t0)
	_, err := f.enroll(tok, store.KindDevice, "laptop", t0)
	if !errors.Is(err, auth.ErrNameTaken) || !strings.Contains(err.Error(), `"laptop"`) {
		t.Fatalf("duplicate name: %v", err)
	}
	if _, err := f.enroll(tok, store.KindWorker, "laptop", t0); err != auth.ErrEnrollRejected { //nolint:errorlint
		t.Fatalf("device token used for worker: %v", err)
	}
	if _, err := f.enroll(tok, store.KindDevice, "laptop-2", t0); err != nil {
		t.Fatalf("retry with another name: %v", err)
	}
	// Names are unique per role: a worker may share the device's name.
	f.mustEnroll(t, store.KindWorker, "laptop")
}

func TestEnrollNames(t *testing.T) {
	f := newFixture(t)
	tok := f.token(t, store.KindDevice, time.Hour, t0)
	for _, bad := range []string{"-x", "a b", "a/b", strings.Repeat("n", auth.MaxNameLen+1), "名字"} {
		if _, err := f.enroll(tok, store.KindDevice, bad, t0); !errors.Is(err, auth.ErrInvalidRequest) {
			t.Errorf("name %q: %v", bad, err)
		}
	}
	en, err := auth.Enroll(context.Background(), f.st, auth.EnrollRequest{Token: tok, Kind: store.KindDevice, Hostname: "box.local", MachineID: "m-2"}, t0)
	if err != nil || en.Identity.Name != "box" {
		t.Fatalf("default name: %q, %v", en.Identity.Name, err)
	}
}

func TestCreateEnrollTokenBounds(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for _, ttl := range []time.Duration{-time.Second, time.Millisecond, auth.MaxEnrollTTL + time.Second} {
		if _, _, err := auth.CreateEnrollToken(ctx, f.st, f.user.ID, store.KindDevice, ttl, t0); !errors.Is(err, auth.ErrInvalidRequest) {
			t.Errorf("ttl %v: %v", ttl, err)
		}
	}
	if _, _, err := auth.CreateEnrollToken(ctx, f.st, f.user.ID, "admin", time.Hour, t0); !errors.Is(err, auth.ErrInvalidRequest) {
		t.Errorf("bad kind: %v", err)
	}
	_, rec, err := auth.CreateEnrollToken(ctx, f.st, f.user.ID, store.KindWorker, 0, t0)
	if err != nil || rec.ExpiresAt != t0.Add(auth.DefaultEnrollTTL).UnixMilli() || len(rec.Kinds) != 1 || rec.Kinds[0] != store.KindWorker {
		t.Fatalf("default token: %+v, %v", rec, err)
	}
}

func TestEnsureUserIdempotent(t *testing.T) {
	f := newFixture(t)
	u, err := auth.EnsureUser(context.Background(), f.st, auth.DefaultUserName, t0.Add(time.Hour))
	if err != nil || u != f.user {
		t.Fatalf("EnsureUser again = %+v, %v; want %+v", u, err, f.user)
	}
}

func TestAuthenticateRejectsBadTokens(t *testing.T) {
	f := newFixture(t)
	en := f.mustEnroll(t, store.KindDevice, "laptop")
	a := auth.NewAuthenticator(f.st, auth.Options{})
	unknown, _ := auth.NewToken()
	reqs := map[string]*http.Request{
		"missing":   bearer(""),
		"unknown":   bearer(unknown),
		"malformed": bearer("abc"),
		"basic": func() *http.Request {
			r := bearer("")
			r.Header.Set("Authorization", "Basic "+en.Token)
			return r
		}(),
	}
	for name, r := range reqs {
		if _, err := a.Authenticate(context.Background(), r); err != auth.ErrUnauthenticated { //nolint:errorlint
			t.Errorf("%s: %v", name, err)
		}
	}
	r := bearer("")
	r.Header.Set("Authorization", "bearer  "+en.Token)
	if _, err := a.Authenticate(context.Background(), r); err != nil {
		t.Errorf("lower-case scheme: %v", err)
	}
}

// F17, storage side: after revocation Authenticate fails with ErrRevoked and
// Valid turns false; the worker on the same machine is untouched.
func TestRevoke(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	dev := f.mustEnroll(t, store.KindDevice, "laptop")
	wrk := f.mustEnroll(t, store.KindWorker, "laptop")
	a := auth.NewAuthenticator(f.st, auth.Options{})
	for _, en := range []auth.Enrollment{dev, wrk} {
		if _, err := a.Authenticate(ctx, bearer(en.Token)); err != nil {
			t.Fatal(err)
		}
		if ok, err := a.Valid(ctx, en.CredentialID); !ok || err != nil {
			t.Fatalf("Valid before revoke = %v, %v", ok, err)
		}
	}

	id, n, err := auth.RevokeByName(ctx, f.st, store.KindDevice, f.user.ID, "laptop", t0)
	if err != nil || n != 1 || id.ID != dev.Identity.ID {
		t.Fatalf("RevokeByName = %+v, %d, %v", id, n, err)
	}
	if _, err := a.Authenticate(ctx, bearer(dev.Token)); err != auth.ErrRevoked { //nolint:errorlint
		t.Fatalf("Authenticate revoked = %v", err)
	}
	if ok, err := a.Valid(ctx, dev.CredentialID); ok || err != nil {
		t.Fatalf("Valid after revoke = %v, %v", ok, err)
	}
	if _, err := a.Authenticate(ctx, bearer(wrk.Token)); err != nil {
		t.Fatalf("worker after device revoke: %v", err)
	}
	if ok, err := a.Valid(ctx, wrk.CredentialID); !ok || err != nil {
		t.Fatalf("worker Valid after device revoke = %v, %v", ok, err)
	}

	if _, n, err := auth.RevokeByName(ctx, f.st, store.KindWorker, f.user.ID, "laptop", t0); n != 1 || err != nil {
		t.Fatalf("Revoke worker = %d, %v", n, err)
	}
	if ok, _ := a.Valid(ctx, wrk.CredentialID); ok {
		t.Fatal("worker still valid")
	}
	// Revocation deleted the identity: the name no longer resolves.
	if _, _, err := auth.RevokeByName(ctx, f.st, store.KindDevice, f.user.ID, "laptop", t0); !errors.Is(err, auth.ErrNoSuchIdentity) {
		t.Fatalf("second revoke by name: %v", err)
	}
	if _, _, err := auth.RevokeByName(ctx, f.st, store.KindDevice, f.user.ID, "nope", t0); !errors.Is(err, auth.ErrNoSuchIdentity) {
		t.Fatalf("revoke unknown: %v", err)
	}
	if ok, err := a.Valid(ctx, "never-seen"); ok || err != nil {
		t.Fatalf("Valid(unknown) = %v, %v", ok, err)
	}
}

// M-1: revocation frees the name, so the same machine can join again under
// the same name with a new link.
func TestRejoinAfterRevoke(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	old := f.mustEnroll(t, store.KindDevice, "laptop")
	if _, err := f.enroll(f.token(t, store.KindDevice, 0, t0), store.KindDevice, "laptop", t0); !errors.Is(err, auth.ErrNameTaken) {
		t.Fatalf("same name while live: %v", err)
	}
	if _, _, err := auth.RevokeByName(ctx, f.st, store.KindDevice, f.user.ID, "laptop", t0); err != nil {
		t.Fatal(err)
	}
	got, err := f.st.GetIdentity(ctx, store.KindDevice, old.Identity.ID)
	if err != nil || got.DeletedAt != t0.UnixMilli() {
		t.Fatalf("revoked identity: %+v, %v", got, err)
	}
	fresh := f.mustEnroll(t, store.KindDevice, "laptop")
	if fresh.Identity.ID == old.Identity.ID {
		t.Fatal("rejoin reused the old identity")
	}
	a := auth.NewAuthenticator(f.st, auth.Options{})
	if _, err := a.Authenticate(ctx, bearer(old.Token)); err != auth.ErrRevoked { //nolint:errorlint
		t.Fatalf("old token: %v", err)
	}
	if p, err := a.Authenticate(ctx, bearer(fresh.Token)); err != nil || p.ID != fresh.Identity.ID {
		t.Fatalf("new token: %+v, %v", p, err)
	}
}

// Revoking only the worker frees only the worker's name; the device
// of the same name on the same machine keeps working.
func TestRevokeWorkerFreesOnlyWorkerName(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	dev := f.mustEnroll(t, store.KindDevice, "laptop")
	f.mustEnroll(t, store.KindWorker, "laptop")
	if _, n, err := auth.RevokeByName(ctx, f.st, store.KindWorker, f.user.ID, "laptop", t0); n != 1 || err != nil {
		t.Fatalf("revoke worker = %d, %v", n, err)
	}
	if _, err := f.st.GetIdentityByName(ctx, store.KindWorker, f.user.ID, "laptop"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("worker name still live: %v", err)
	}
	if _, err := f.st.GetIdentityByName(ctx, store.KindDevice, f.user.ID, "laptop"); err != nil {
		t.Fatalf("device lost its name: %v", err)
	}
	f.mustEnroll(t, store.KindWorker, "laptop") // the worker name is free again
	if _, err := f.enroll(f.token(t, store.KindDevice, 0, t0), store.KindDevice, "laptop", t0); !errors.Is(err, auth.ErrNameTaken) {
		t.Fatalf("device name freed by the worker revoke: %v", err)
	}
	if _, err := auth.NewAuthenticator(f.st, auth.Options{}).Authenticate(ctx, bearer(dev.Token)); err != nil {
		t.Fatalf("device after worker revoke: %v", err)
	}
}

// A deleted identity is treated like a revoked one even while its
// credential is not revoked.
func TestDeletedIdentityIsRevoked(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	en := f.mustEnroll(t, store.KindDevice, "laptop")
	a := auth.NewAuthenticator(f.st, auth.Options{})
	if _, err := a.Authenticate(ctx, bearer(en.Token)); err != nil {
		t.Fatal(err)
	}
	if ok, err := f.st.DeleteIdentity(ctx, store.KindDevice, en.Identity.ID, t0.UnixMilli()); !ok || err != nil {
		t.Fatalf("DeleteIdentity = %v, %v", ok, err)
	}
	if _, err := a.Authenticate(ctx, bearer(en.Token)); err != auth.ErrRevoked { //nolint:errorlint
		t.Fatalf("Authenticate deleted identity: %v", err)
	}
	if ok, err := a.Valid(ctx, en.CredentialID); ok || err != nil {
		t.Fatalf("Valid deleted identity = %v, %v", ok, err)
	}
}

func TestRequireNeedsKinds(t *testing.T) {
	a := auth.NewAuthenticator(newFixture(t).st, auth.Options{})
	for name, kinds := range map[string][]store.PrincipalKind{"none": nil, "unknown": {"robot"}} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("Require with %s kinds did not panic", name)
				}
			}()
			a.Require(http.NotFoundHandler(), kinds...)
		}()
	}
}

// LastUsedAt is written once per TouchInterval, not per request.
func TestTouchThrottled(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	en := f.mustEnroll(t, store.KindDevice, "laptop")
	now := t0
	a := auth.NewAuthenticator(f.st, auth.Options{Now: func() time.Time { return now }, TouchInterval: time.Minute})
	lastUsed := func() int64 {
		c, err := f.st.LookupCredential(ctx, auth.HashToken(en.Token))
		if err != nil {
			t.Fatal(err)
		}
		return c.LastUsedAt
	}
	steps := []struct {
		advance time.Duration
		want    time.Time
	}{
		{0, t0},
		{30 * time.Second, t0},
		{29 * time.Second, t0},
		{time.Second, t0.Add(time.Minute)},
		{10 * time.Second, t0.Add(time.Minute)},
	}
	for i, s := range steps {
		now = now.Add(s.advance)
		if _, err := a.Authenticate(ctx, bearer(en.Token)); err != nil {
			t.Fatal(err)
		}
		if got := lastUsed(); got != s.want.UnixMilli() {
			t.Fatalf("step %d: LastUsedAt = %d, want %d", i, got, s.want.UnixMilli())
		}
	}
}

// Require answers before the wrapped handler runs: a wrong token never
// reaches the WebSocket upgrade.
func TestRequireBeforeUpgrade(t *testing.T) {
	f := newFixture(t)
	dev := f.mustEnroll(t, store.KindDevice, "laptop")
	wrk := f.mustEnroll(t, store.KindWorker, "laptop")
	revoked := f.mustEnroll(t, store.KindDevice, "old")
	if _, _, err := auth.RevokeByName(context.Background(), f.st, store.KindDevice, f.user.ID, "old", t0); err != nil {
		t.Fatal(err)
	}
	a := auth.NewAuthenticator(f.st, auth.Options{})
	var reached atomic.Int32
	link := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		p, ok := auth.PrincipalFrom(r.Context())
		if !ok || p.ID != dev.Identity.ID {
			http.Error(w, "no principal", http.StatusInternalServerError)
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		c.Close(websocket.StatusNormalClosure, "")
	})
	srv := httptest.NewServer(a.Require(link, store.KindDevice))
	t.Cleanup(srv.Close)
	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	unknown, _ := auth.NewToken()

	dial := func(token string) (int, string, error) {
		h := http.Header{}
		if token != "" {
			h.Set("Authorization", "Bearer "+token)
		}
		c, resp, err := websocket.Dial(context.Background(), url, &websocket.DialOptions{HTTPHeader: h})
		if c != nil {
			c.Close(websocket.StatusNormalClosure, "")
		}
		if resp == nil {
			return 0, "", err
		}
		return resp.StatusCode, resp.Header.Get("WWW-Authenticate"), err
	}
	for _, c := range []struct {
		name, token string
		status      int
	}{
		{"missing", "", http.StatusUnauthorized},
		{"unknown", unknown, http.StatusUnauthorized},
		{"revoked", revoked.Token, http.StatusForbidden},
		{"wrong role", wrk.Token, http.StatusForbidden},
	} {
		status, challenge, err := dial(c.token)
		if err == nil || status != c.status {
			t.Errorf("%s: status %d, err %v; want %d", c.name, status, err, c.status)
		}
		if (status == http.StatusUnauthorized) != (challenge == "Bearer") {
			t.Errorf("%s: WWW-Authenticate %q", c.name, challenge)
		}
	}
	if reached.Load() != 0 {
		t.Fatalf("handler reached %d times by rejected requests", reached.Load())
	}
	if status, _, err := dial(dev.Token); err != nil || status != http.StatusSwitchingProtocols {
		t.Fatalf("valid token: status %d, %v", status, err)
	}
	if reached.Load() != 1 {
		t.Fatal("valid request did not reach the handler")
	}

	f.st.Close()
	if status, _, _ := dial(dev.Token); status != http.StatusServiceUnavailable {
		t.Fatalf("closed store: status %d, want 503", status)
	}
}

// Valid knows only credentials this instance accepted, sees a revocation
// written through another store handle (the CLI process), and reports a
// store failure as an error rather than as a revocation.
func TestValidAcrossInstancesAndStoreFailure(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "db")
	st, err := sqlite.Open(ctx, path, sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	user, err := auth.EnsureUser(ctx, st, "admin", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := auth.CreateEnrollToken(ctx, st, user.ID, store.KindDevice, time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	e, err := auth.Enroll(ctx, st, auth.EnrollRequest{Token: token, Kind: store.KindDevice, Name: "desk", MachineID: "machine", Hostname: "host", OS: "linux", Arch: "amd64"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	a := auth.NewAuthenticator(st, auth.Options{})
	req := httptest.NewRequest("GET", "/v1/link/daemon", nil)
	req.Header.Set("Authorization", "Bearer "+e.Token)
	p, err := a.Authenticate(ctx, req)
	if err != nil || p.Name != "desk" || p.CredentialID != e.CredentialID {
		t.Fatal(p, err)
	}
	if ok, err := a.Valid(ctx, p.CredentialID); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if ok, err := auth.NewAuthenticator(st, auth.Options{}).Valid(ctx, p.CredentialID); ok || err != nil {
		t.Fatal("separate instance unexpectedly knows credential", ok, err)
	}
	cli, err := sqlite.Open(ctx, path, sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	if _, _, err = auth.RevokeByName(ctx, cli, store.KindDevice, user.ID, "desk", time.Now()); err != nil {
		t.Fatal(err)
	}
	if ok, err := a.Valid(ctx, p.CredentialID); ok || err != nil {
		t.Fatal(ok, err)
	}
	if _, err = a.Authenticate(ctx, req); !errors.Is(err, auth.ErrRevoked) {
		t.Fatal(err)
	}
	_ = st.Close()
	if ok, err := a.Valid(ctx, p.CredentialID); ok || err == nil {
		t.Fatal("store error not propagated", ok, err)
	}
}
