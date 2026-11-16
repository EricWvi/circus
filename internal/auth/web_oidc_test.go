package auth_test

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/gen/go/circus/v1/circusv1connect"
	"github.com/EricWvi/circus/internal/auth"
)

type fakeProfile struct {
	subject, email string
	verified       bool
	fault          string
}
type fakeCode struct {
	nonce, challenge, redirect string
	profile                    fakeProfile
}
type fakeOIDC struct {
	server       *httptest.Server
	key          *rsa.PrivateKey
	mu           sync.Mutex
	profile      fakeProfile
	codes        map[string]fakeCode
	accesses     map[string]fakeProfile
	sequence     int
	tokenStarted chan struct{}
	tokenRelease chan struct{}
}

func newFakeOIDC(t *testing.T) *fakeOIDC {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := &fakeOIDC{key: key, codes: map[string]fakeCode{}, accesses: map[string]fakeProfile{}, profile: fakeProfile{subject: "alice", email: "alice@example.com", verified: true}}
	mux := http.NewServeMux()
	p.server = httptest.NewServer(mux)
	t.Cleanup(p.server.Close)
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		writeFakeJSON(w, map[string]any{"issuer": p.server.URL, "authorization_endpoint": p.server.URL + "/authorize", "token_endpoint": p.server.URL + "/token", "userinfo_endpoint": p.server.URL + "/userinfo", "jwks_uri": p.server.URL + "/jwks", "response_types_supported": []string{"code"}, "subject_types_supported": []string{"public"}, "id_token_signing_alg_values_supported": []string{"RS256"}})
	})
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, r *http.Request) {
		writeFakeJSON(w, map[string]any{"keys": []any{map[string]any{"kty": "RSA", "use": "sig", "alg": "RS256", "kid": "test-key", "n": base64.RawURLEncoding.EncodeToString(p.key.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(p.key.E)).Bytes())}}})
	})
	mux.HandleFunc("GET /authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("client_id") != "circus" || q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || q.Get("nonce") == "" || q.Get("state") == "" || q.Get("scope") != "openid profile email" {
			http.Error(w, "invalid authorization request", 400)
			return
		}
		p.mu.Lock()
		p.sequence++
		code := fmt.Sprintf("code-%d", p.sequence)
		profile := p.profile
		p.codes[code] = fakeCode{nonce: q.Get("nonce"), challenge: q.Get("code_challenge"), redirect: q.Get("redirect_uri"), profile: profile}
		p.mu.Unlock()
		callback, _ := url.Parse(q.Get("redirect_uri"))
		query := callback.Query()
		query.Set("state", q.Get("state"))
		query.Set("code", code)
		callback.RawQuery = query.Encode()
		http.Redirect(w, r, callback.String(), http.StatusFound)
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", 400)
			return
		}
		p.mu.Lock()
		code, ok := p.codes[r.Form.Get("code")]
		delete(p.codes, r.Form.Get("code"))
		p.mu.Unlock()
		client, secret, _ := r.BasicAuth()
		if client == "" {
			client, secret = r.Form.Get("client_id"), r.Form.Get("client_secret")
		}
		challenge := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if !ok || client != "circus" || secret != "test-secret" || r.Form.Get("grant_type") != "authorization_code" ||
			r.Form.Get("redirect_uri") != code.redirect || base64.RawURLEncoding.EncodeToString(challenge[:]) != code.challenge {
			http.Error(w, "invalid code, client or PKCE", 400)
			return
		}
		p.mu.Lock()
		started, release := p.tokenStarted, p.tokenRelease
		p.mu.Unlock()
		if started != nil {
			select {
			case started <- struct{}{}:
			default:
			}
		}
		if release != nil {
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}

		profile := code.profile
		if profile.fault == "exchange" {
			http.Error(w, "provider unavailable", 503)
			return
		}
		claims := map[string]any{"iss": p.server.URL, "sub": profile.subject, "aud": "circus", "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(), "nonce": code.nonce}
		switch profile.fault {
		case "nonce":
			claims["nonce"] = "wrong"
		case "issuer":
			claims["iss"] = "https://wrong.example"
		case "audience":
			claims["aud"] = "other"
		case "expiry":
			claims["exp"] = time.Now().Add(-time.Hour).Unix()
		}
		raw, err := p.sign(claims)
		if err != nil {
			http.Error(w, "signing failed", 500)
			return
		}
		if profile.fault == "signature" {
			raw = raw[:len(raw)-5] + "AAAAA"
		}
		access := "access-" + r.Form.Get("code")
		p.mu.Lock()
		p.accesses[access] = profile
		p.mu.Unlock()
		writeFakeJSON(w, map[string]any{"access_token": access, "token_type": "Bearer", "expires_in": 3600, "id_token": raw})
	})
	mux.HandleFunc("GET /userinfo", func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		profile, ok := p.accesses[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
		p.mu.Unlock()
		if !ok || profile.fault == "userinfo" {
			http.Error(w, "userinfo unavailable", 503)
			return
		}
		subject := profile.subject
		if profile.fault == "subject" {
			subject = "wrong-subject"
		}
		// Deliberately absent from the ID token: Authelia serves email here.
		writeFakeJSON(w, map[string]any{"sub": subject, "email": profile.email, "email_verified": profile.verified})
	})
	return p
}
func writeFakeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
func (p *fakeOIDC) sign(claims map[string]any) (string, error) {
	head, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "test-key"})
	body, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	payload := base64.RawURLEncoding.EncodeToString(head) + "." + base64.RawURLEncoding.EncodeToString(body)
	sum := sha256.Sum256([]byte(payload))
	sig, err := rsa.SignPKCS1v15(rand.Reader, p.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return payload + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}
func (p *fakeOIDC) set(profile fakeProfile) { p.mu.Lock(); p.profile = profile; p.mu.Unlock() }
func (p *fakeOIDC) config() *auth.OIDCConfig {
	return &auth.OIDCConfig{Issuer: p.server.URL, ClientID: "circus", ClientSecret: "test-secret", DisplayName: "Authelia"}
}
func getRedirect(t *testing.T, client *http.Client, target string) *http.Response {
	t.Helper()
	resp, err := client.Get(target)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("OIDC redirect request: status %d (URL redacted)", resp.StatusCode)
	}
	return resp
}

// beginOIDC performs start and provider authorization, leaving callback pending.
func (h *webHarness) beginOIDC(t *testing.T, c *http.Client, query string) string {
	t.Helper()
	resp := getRedirect(t, c, h.server.URL+"/auth/oidc/login"+query)
	if !strings.Contains(resp.Header.Get("Location"), "/authorize?") {
		t.Fatal("OIDC start did not redirect to the provider (URL redacted)")
	}
	resp = getRedirect(t, c, resp.Header.Get("Location"))
	return resp.Header.Get("Location")
}
func (h *webHarness) finishOIDC(t *testing.T, c *http.Client, callback string) string {
	t.Helper()
	return getRedirect(t, c, callback).Header.Get("Location")
}
func (h *webHarness) oidcSignIn(t *testing.T, c *http.Client, query string) string {
	t.Helper()
	return h.finishOIDC(t, c, h.beginOIDC(t, c, query))
}
func TestOIDCUsers(t *testing.T) {
	p := newFakeOIDC(t)
	h := newWebHarness(t, p.config())
	h.setup(t)
	if who(t, h.rpc).Methods.GetOidc().GetDisplayName() != "Authelia" {
		t.Fatal("missing OIDC login method")
	}
	browser := h.browser(t)
	rpc := circusv1connect.NewAuthServiceClient(browser, h.server.URL)
	if next := h.oidcSignIn(t, browser, "?next=%2Fdevices%3Fpage%3D2"); next != "/devices?page=2" {
		t.Fatal(next)
	}
	first := who(t, rpc).GetUser()
	if first == nil || first.Email != "alice@example.com" || first.Role != circusv1.User_ROLE_USER {
		t.Fatalf("new OIDC user = %v", first)
	}
	identity, err := h.st.GetUserIdentity(t.Context(), p.server.URL, "alice")
	if err != nil || identity.UserID != first.UserId {
		t.Fatal(identity, err)
	}
	// Existing identity wins even when its email changes or becomes unverified.
	p.set(fakeProfile{subject: "alice", email: "changed@example.com", verified: false})
	if next := h.oidcSignIn(t, browser, ""); next != "/" {
		t.Fatal(next)
	}
	if again := who(t, rpc).GetUser(); again.GetUserId() != first.UserId || again.GetEmail() != first.Email {
		t.Fatalf("identity changed user: %v", again)
	}
	// Verified email binds a new subject to an existing password user.
	p.set(fakeProfile{subject: "admin-sub", email: " ADMIN@EXAMPLE.COM ", verified: true})
	browser = h.browser(t)
	rpc = circusv1connect.NewAuthServiceClient(browser, h.server.URL)
	if next := h.oidcSignIn(t, browser, "?next=https%3A%2F%2Fevil.example"); next != "/" {
		t.Fatal(next)
	}
	admin, err := h.st.GetSuperUser(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if who(t, rpc).GetUser().GetUserId() != admin.ID {
		t.Fatal("verified email did not bind")
	}
	for _, profile := range []fakeProfile{{subject: "unverified", email: "admin@example.com", verified: false}, {subject: "missing-email", verified: true}, {subject: "invalid-email", email: "bad", verified: true}} {
		p.set(profile)
		c := h.browser(t)
		if next := h.oidcSignIn(t, c, ""); next != "/login?error=email_unverified" {
			t.Fatal(next)
		}
		if who(t, circusv1connect.NewAuthServiceClient(c, h.server.URL)).GetSignedOut() == nil {
			t.Fatal("unverified user signed in")
		}
		if _, err := h.st.GetUserIdentity(t.Context(), p.server.URL, profile.subject); err == nil {
			t.Fatal("unverified identity was saved")
		}
	}
}
func TestOIDCSetup(t *testing.T) {
	p := newFakeOIDC(t)
	t.Run("creates super and consumes token", func(t *testing.T) {
		h := newWebHarness(t, p.config())
		p.set(fakeProfile{subject: "root", email: " ROOT@example.com ", verified: true})
		if next := h.oidcSignIn(t, h.client, "?setup_token="+url.QueryEscape(h.token)+"&next=/ignored"); next != "/" {
			t.Fatal(next)
		}
		u := who(t, h.rpc).GetUser()
		if u.GetRole() != circusv1.User_ROLE_SUPER || u.GetEmail() != "root@example.com" {
			t.Fatal(u)
		}
		identity, err := h.st.GetUserIdentity(t.Context(), p.server.URL, "root")
		if err != nil || identity.UserID != u.UserId {
			t.Fatal(identity, err)
		}
		_, err = h.rpc.Setup(t.Context(), connect.NewRequest(&circusv1.SetupRequest{Token: h.token}))
		wantCode(t, err, connect.CodePermissionDenied)
		if next := getRedirect(t, h.client, h.server.URL+"/auth/oidc/login?setup_token="+url.QueryEscape(h.token)).Header.Get("Location"); next != "/setup?error=invalid_token" {
			t.Fatal(next)
		}
	})
	t.Run("invalid setup checked before discovery", func(t *testing.T) {
		cfg := p.config()
		cfg.Issuer = "http://127.0.0.1:1"
		h := newWebHarness(t, cfg)
		resp := getRedirect(t, h.client, h.server.URL+"/auth/oidc/login?setup_token=wrong")
		if resp.Header.Get("Location") != "/setup?error=invalid_token" {
			t.Fatal(resp.Header)
		}
	})
	t.Run("another setup while provider flow pending", func(t *testing.T) {
		h := newWebHarness(t, p.config())
		p.set(fakeProfile{subject: "root", email: "root@example.com", verified: true})
		callback := h.beginOIDC(t, h.client, "?setup_token="+url.QueryEscape(h.token))
		h.setup(t)
		if next := h.finishOIDC(t, h.client, callback); next != "/setup?error=already_set_up" {
			t.Fatal(next)
		}
	})
	t.Run("unverified setup preserves token", func(t *testing.T) {
		h := newWebHarness(t, p.config())
		p.set(fakeProfile{subject: "root", email: "root@example.com", verified: false})
		if next := h.oidcSignIn(t, h.client, "?setup_token="+url.QueryEscape(h.token)); next != "/setup?error=email_unverified&token="+url.QueryEscape(h.token) {
			t.Fatal(next)
		}
		if who(t, h.rpc).GetNeedsSetup() == nil {
			t.Fatal("unverified setup created a super")
		}
		h.setup(t)
	})
}
func TestOIDCFailures(t *testing.T) {
	p := newFakeOIDC(t)
	h := newWebHarness(t, p.config())
	h.setup(t)
	for _, fault := range []string{"exchange", "nonce", "issuer", "audience", "expiry", "signature", "userinfo", "subject"} {
		t.Run(fault, func(t *testing.T) {
			p.set(fakeProfile{subject: "bad-" + fault, email: "a@b", verified: true, fault: fault})
			if next := h.oidcSignIn(t, h.browser(t), ""); next != "/login?error=oidc_failed" {
				t.Fatal(next)
			}
		})
	}
	p.set(fakeProfile{subject: "alice", email: "alice@example.com", verified: true})
	t.Run("bad state and callback replay", func(t *testing.T) {
		browser := h.browser(t)
		callback := h.beginOIDC(t, browser, "")
		u, _ := url.Parse(callback)
		q := u.Query()
		q.Set("state", "bad")
		u.RawQuery = q.Encode()
		if next := h.finishOIDC(t, browser, u.String()); next != "/login?error=oidc_failed" {
			t.Fatal(next)
		}
		if next := h.finishOIDC(t, browser, callback); next != "/login?error=oidc_failed" {
			t.Fatal(next)
		}
	})
	t.Run("expired flow", func(t *testing.T) {
		browser := h.browser(t)
		callback := h.beginOIDC(t, browser, "")
		h.clock.Add((11 * time.Minute).Milliseconds())
		if next := h.finishOIDC(t, browser, callback); next != "/login?error=oidc_failed" {
			t.Fatal(next)
		}
	})
	t.Run("tampered cookie", func(t *testing.T) {
		browser := h.browser(t)
		callback := h.beginOIDC(t, browser, "")
		u, _ := url.Parse(h.server.URL + "/auth/oidc/callback")
		for _, c := range browser.Jar.Cookies(u) {
			if c.Name == "circus_login" {
				c.Value = "tampered"
				c.Path = "/auth/oidc/"
				browser.Jar.SetCookies(u, []*http.Cookie{c})
			}
		}
		if next := h.finishOIDC(t, browser, callback); next != "/login?error=oidc_failed" {
			t.Fatal(next)
		}
	})
	t.Run("start origin", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, h.server.URL+"/auth/oidc/login", nil)
		req.Header.Set("Origin", "https://evil.example")
		resp, err := h.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatal(resp.StatusCode)
		}
	})
	for _, next := range []string{"//evil.example", "/%2Fevil.example", "/\\evil.example", "/%5Cevil.example", "https://evil.example", "/%0aevil"} {
		t.Run("next "+next, func(t *testing.T) {
			if target := h.oidcSignIn(t, h.browser(t), "?next="+url.QueryEscape(next)); target != "/" {
				t.Fatal(target)
			}
		})
	}
}
func TestOIDCDisabledAndLazyDiscovery(t *testing.T) {
	h := newWebHarness(t, nil)
	for _, path := range []string{"/auth/oidc/login", "/auth/oidc/callback"} {
		resp, err := h.client.Get(h.server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatal(resp.StatusCode)
		}
	}
	unavailable := newWebHarness(t, &auth.OIDCConfig{Issuer: "http://127.0.0.1:1", ClientID: "circus"})
	if who(t, unavailable.rpc).Methods.Oidc == nil {
		t.Fatal("lazy provider disabled")
	}
	unavailable.setup(t)
	if next := getRedirect(t, unavailable.client, unavailable.server.URL+"/auth/oidc/login").Header.Get("Location"); next != "/login?error=oidc_failed" {
		t.Fatal(next)
	}
}

func (p *fakeOIDC) blockToken() (<-chan struct{}, func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tokenStarted = make(chan struct{}, 1)
	p.tokenRelease = make(chan struct{})
	release := p.tokenRelease
	var once sync.Once
	return p.tokenStarted, func() { once.Do(func() { close(release) }) }
}
