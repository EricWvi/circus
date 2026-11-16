package auth_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/gen/go/circus/v1/circusv1connect"
	"github.com/EricWvi/circus/internal/auth"
	"github.com/EricWvi/circus/internal/store"
)

func TestOIDCRequiresSuperUser(t *testing.T) {
	p := newFakeOIDC(t)
	t.Run("start", func(t *testing.T) {
		h := newWebHarness(t, p.config())
		resp := getRedirect(t, h.client, h.server.URL+"/auth/oidc/login?next=/devices")
		if resp.Header.Get("Location") != "/setup" {
			t.Fatalf("uninitialized login start must redirect to setup")
		}
	})
	t.Run("callback without a flow", func(t *testing.T) {
		h := newWebHarness(t, p.config())
		if next := getRedirect(t, h.client, h.server.URL+"/auth/oidc/callback").Header.Get("Location"); next != "/setup" {
			t.Fatal("uninitialized callback must redirect to setup")
		}
	})
	for _, existing := range []bool{false, true} {
		name := "new user"
		if existing {
			name = "existing identity"
		}
		t.Run("callback "+name, func(t *testing.T) {
			h := newWebHarness(t, p.config())
			admin := h.setup(t)
			if existing {
				if next := h.oidcSignIn(t, h.browser(t), ""); next != "/" {
					t.Fatal(next)
				}
			}
			browser := h.browser(t)
			callback := h.beginOIDC(t, browser, "")
			if err := h.st.DeleteUser(t.Context(), admin.UserId); err != nil {
				t.Fatal(err)
			}
			if next := h.finishOIDC(t, browser, callback); next != "/setup" {
				t.Fatalf("uninitialized callback must redirect to setup")
			}
			target, _ := url.Parse(h.server.URL)
			for _, c := range browser.Jar.Cookies(target) {
				if c.Name == auth.SessionCookie {
					t.Fatal("uninitialized callback signed in a user")
				}
			}
			if !existing {
				if _, err := h.st.GetUserByEmail(t.Context(), "alice@example.com"); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("callback created a user: %v", err)
				}
			}
		})
	}
	t.Run("password setup occupied email", func(t *testing.T) {
		h := newWebHarness(t, nil)
		if err := h.st.CreateUser(t.Context(), store.User{ID: "preexisting", Email: "admin@example.com", Role: store.RoleUser, CreatedAt: h.clock.Load()}); err != nil {
			t.Fatal(err)
		}
		_, err := h.rpc.Setup(t.Context(), connect.NewRequest(&circusv1.SetupRequest{Token: h.token, Email: "ADMIN@example.com", Password: "password-123"}))
		wantCode(t, err, connect.CodeInvalidArgument)
		wantField(t, err, "email")
		_, err = h.rpc.Setup(t.Context(), connect.NewRequest(&circusv1.SetupRequest{Token: h.token, Email: "other@example.com", Password: "password-123"}))
		if err != nil {
			t.Fatal(err)
		}
	})
}

func TestSetupValidTokenBypassesLimiter(t *testing.T) {
	t.Run("password setup", func(t *testing.T) {
		h := newWebHarness(t, nil)
		for range 5 {
			_, err := h.rpc.Setup(t.Context(), connect.NewRequest(&circusv1.SetupRequest{Token: "bad"}))
			wantCode(t, err, connect.CodePermissionDenied)
		}
		h.setup(t)
	})
	t.Run("OIDC start", func(t *testing.T) {
		p := newFakeOIDC(t)
		h := newWebHarness(t, p.config())
		for range 5 {
			getRedirect(t, h.client, h.server.URL+"/auth/oidc/login?setup_token=bad")
		}
		if next := h.oidcSignIn(t, h.client, "?setup_token="+url.QueryEscape(h.token)); next != "/" {
			t.Fatal("correct setup token was rate limited")
		}
	})
	t.Run("OIDC callback", func(t *testing.T) {
		p := newFakeOIDC(t)
		h := newWebHarness(t, p.config())
		callback := h.beginOIDC(t, h.client, "?setup_token="+url.QueryEscape(h.token))
		for range 5 {
			_, err := h.rpc.Setup(t.Context(), connect.NewRequest(&circusv1.SetupRequest{Token: "bad"}))
			wantCode(t, err, connect.CodePermissionDenied)
		}
		if next := h.finishOIDC(t, h.client, callback); next != "/" {
			t.Fatal("accepted flow was rate limited at callback")
		}
	})
}

func TestProxyLimiterClientIP(t *testing.T) {
	for _, proxy := range []bool{false, true} {
		name := "direct"
		if proxy {
			name = "proxy"
		}
		t.Run(name, func(t *testing.T) {
			h := newWebHarness(t, nil, func(c *auth.WebConfig) { c.BehindProxy = proxy })
			h.setup(t)
			for range 5 {
				req := connect.NewRequest(&circusv1.LoginRequest{Email: "missing@example.com", Password: "wrong-password"})
				req.Header().Set("X-Forwarded-For", "198.51.100.90, 192.0.2.1")
				_, err := h.rpc.Login(t.Context(), req)
				wantCode(t, err, connect.CodeUnauthenticated)
			}
			req := connect.NewRequest(&circusv1.LoginRequest{Email: "missing@example.com", Password: "wrong-password"})
			req.Header().Set("X-Forwarded-For", "198.51.100.90, 192.0.2.2")
			_, err := h.rpc.Login(t.Context(), req)
			code := connect.CodeResourceExhausted
			if proxy {
				code = connect.CodeUnauthenticated
			}
			wantCode(t, err, code)
			if proxy {
				req.Header().Set("X-Forwarded-For", "198.51.100.91, 192.0.2.1")
				_, err = h.rpc.Login(t.Context(), req)
				wantCode(t, err, connect.CodeResourceExhausted)
			}
		})
	}
	t.Run("setup wrong attempts isolated by IP", func(t *testing.T) {
		h := newWebHarness(t, nil, func(c *auth.WebConfig) { c.BehindProxy = true })
		for range 5 {
			req := connect.NewRequest(&circusv1.SetupRequest{Token: "bad"})
			req.Header().Set("X-Forwarded-For", "192.0.2.1")
			_, err := h.rpc.Setup(t.Context(), req)
			wantCode(t, err, connect.CodePermissionDenied)
		}
		req := connect.NewRequest(&circusv1.SetupRequest{Token: "bad"})
		req.Header().Set("X-Forwarded-For", "192.0.2.2")
		_, err := h.rpc.Setup(t.Context(), req)
		wantCode(t, err, connect.CodePermissionDenied)
		req.Header().Set("X-Forwarded-For", "192.0.2.1")
		_, err = h.rpc.Setup(t.Context(), req)
		wantCode(t, err, connect.CodeResourceExhausted)
	})
	t.Run("OIDC start wrong attempts isolated by IP", func(t *testing.T) {
		p := newFakeOIDC(t)
		h := newWebHarness(t, p.config(), func(c *auth.WebConfig) { c.BehindProxy = true })
		start := func(ip string) string {
			req, _ := http.NewRequest(http.MethodGet, h.server.URL+"/auth/oidc/login?setup_token=bad", nil)
			req.Header.Set("X-Forwarded-For", ip)
			resp, err := h.client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			return resp.Header.Get("Location")
		}
		for range 5 {
			if start("192.0.2.1") != "/setup?error=invalid_token" {
				t.Fatal("early rate limit")
			}
		}
		if start("192.0.2.1") != "/setup?error=too_many_attempts" {
			t.Fatal("missing rate-limit redirect")
		}
		if start("192.0.2.2") != "/setup?error=invalid_token" {
			t.Fatal("another IP was limited")
		}
	})
}

func TestOIDCEmailBindingConflict(t *testing.T) {
	p := newFakeOIDC(t)
	h := newWebHarness(t, p.config())
	admin := h.setup(t)
	p.set(fakeProfile{subject: "original-admin", email: admin.Email, verified: true})
	browser := h.browser(t)
	if next := h.oidcSignIn(t, browser, ""); next != "/" {
		t.Fatal(next)
	}
	if who(t, circusv1connect.NewAuthServiceClient(browser, h.server.URL)).GetUser().GetUserId() != admin.UserId {
		t.Fatal("first verified-email binding failed")
	}
	p.set(fakeProfile{subject: "different-account", email: admin.Email, verified: true})
	other := h.browser(t)
	if next := h.oidcSignIn(t, other, ""); next != "/login?error=account_conflict" {
		t.Fatal("different subject gained access to the bound account")
	}
	if who(t, circusv1connect.NewAuthServiceClient(other, h.server.URL)).GetSignedOut() == nil {
		t.Fatal("conflicting account signed in")
	}
	if _, err := h.st.GetUserIdentity(t.Context(), p.server.URL, "different-account"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("conflicting identity was saved")
	}
	if !strings.Contains(h.logs.String(), "account conflict") {
		t.Fatal("account conflict was not logged")
	}
	// A different issuer is an independent namespace and may bind once.
	otherProvider := newFakeOIDC(t)
	// The current service has one issuer; represent a binding at another issuer
	// on a fresh password user's row to verify that it does not block this one.
	if err := h.st.CreateUser(t.Context(), store.User{ID: "regular", Email: "regular@example.com", Role: store.RoleUser, CreatedAt: h.clock.Load()}); err != nil {
		t.Fatal(err)
	}
	if err := h.st.CreateUserIdentity(t.Context(), store.UserIdentity{Issuer: otherProvider.server.URL, Subject: "remote", UserID: "regular", CreatedAt: h.clock.Load()}); err != nil {
		t.Fatal(err)
	}
	p.set(fakeProfile{subject: "regular-here", email: "regular@example.com", verified: true})
	if next := h.oidcSignIn(t, h.browser(t), ""); next != "/" {
		t.Fatal("another issuer prevented first binding")
	}
}

func TestOIDCFailureRedirects(t *testing.T) {
	p := newFakeOIDC(t)
	t.Run("setup discovery failure preserves token", func(t *testing.T) {
		cfg := p.config()
		cfg.Issuer = "http://127.0.0.1:1"
		h := newWebHarness(t, cfg)
		next := getRedirect(t, h.client, h.server.URL+"/auth/oidc/login?setup_token="+url.QueryEscape(h.token)).Header.Get("Location")
		wantRedirectParams(t, next, "/setup", url.Values{"error": {"oidc_failed"}, "token": {h.token}})
	})
	t.Run("setup callback failure preserves token", func(t *testing.T) {
		h := newWebHarness(t, p.config())
		p.set(fakeProfile{subject: "root", email: "root@example.com", verified: true, fault: "exchange"})
		next := h.oidcSignIn(t, h.client, "?setup_token="+url.QueryEscape(h.token))
		wantRedirectParams(t, next, "/setup", url.Values{"error": {"oidc_failed"}, "token": {h.token}})
	})
	for _, next := range []string{"/devices?page=2", "/", "https://evil.example", "//evil.example", "/%2Fevil.example", ""} {
		t.Run("normal failure "+next, func(t *testing.T) {
			h := newWebHarness(t, p.config())
			h.setup(t)
			p.set(fakeProfile{subject: "alice", email: "alice@example.com", verified: true, fault: "exchange"})
			redirect := h.oidcSignIn(t, h.browser(t), "?next="+url.QueryEscape(next))
			params := url.Values{"error": {"oidc_failed"}}
			if next == "/devices?page=2" || next == "/" {
				params.Set("next", next)
			}
			wantRedirectParams(t, redirect, "/login", params)
		})
	}
	t.Run("signed state failure preserves retry context", func(t *testing.T) {
		h := newWebHarness(t, p.config())
		callback := h.beginOIDC(t, h.client, "?setup_token="+url.QueryEscape(h.token))
		target, _ := url.Parse(callback)
		params := target.Query()
		params.Set("state", "wrong")
		target.RawQuery = params.Encode()
		next := h.finishOIDC(t, h.client, target.String())
		wantRedirectParams(t, next, "/setup", url.Values{"error": {"oidc_failed"}, "token": {h.token}})
	})
	t.Run("normal discovery backoff preserves next", func(t *testing.T) {
		cfg := p.config()
		cfg.Issuer = "http://127.0.0.1:1"
		h := newWebHarness(t, cfg)
		h.setup(t)
		for range 5 {
			next := getRedirect(t, h.client, h.server.URL+"/auth/oidc/login?next=/devices").Header.Get("Location")
			wantRedirectParams(t, next, "/login", url.Values{"error": {"oidc_failed"}, "next": {"/devices"}})
		}
		next := getRedirect(t, h.client, h.server.URL+"/auth/oidc/login?next=/devices").Header.Get("Location")
		wantRedirectParams(t, next, "/login", url.Values{"error": {"too_many_attempts"}, "next": {"/devices"}})
	})
}
func wantRedirectParams(t *testing.T, redirect, path string, params url.Values) {
	t.Helper()
	target, err := url.Parse(redirect)
	if err != nil || target.Path != path || target.RawQuery != params.Encode() {
		t.Fatalf("unexpected retry redirect path or parameters (secrets redacted)")
	}
}

func TestOIDCNetworkDoesNotLockSetup(t *testing.T) {
	p := newFakeOIDC(t)
	h := newWebHarness(t, p.config())
	callback := h.beginOIDC(t, h.client, "?setup_token="+url.QueryEscape(h.token))
	started, release := p.blockToken()
	defer release()
	done := make(chan string, 1)
	go func() {
		resp, err := h.client.Get(callback)
		if err != nil {
			done <- "request failed"
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		done <- resp.Header.Get("Location")
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("token exchange did not start")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	_, err := h.rpc.Setup(ctx, connect.NewRequest(&circusv1.SetupRequest{Token: h.token, Email: "admin@example.com", Password: "password-123"}))
	if err != nil {
		t.Fatal("network exchange held the setup mutex")
	}
	release()
	select {
	case next := <-done:
		if next != "/setup?error=already_set_up" {
			t.Fatal("callback did not recheck setup")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("callback did not finish")
	}
}

func TestOIDCSuperRemovedDuringNetwork(t *testing.T) {
	p := newFakeOIDC(t)
	h := newWebHarness(t, p.config())
	admin := h.setup(t)
	browser := h.browser(t)
	callback := h.beginOIDC(t, browser, "")
	started, release := p.blockToken()
	defer release()
	done := make(chan string, 1)
	go func() {
		resp, err := browser.Get(callback)
		if err != nil {
			done <- "request failed"
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		done <- resp.Header.Get("Location")
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("token exchange did not start")
	}
	if err := h.st.DeleteUser(t.Context(), admin.UserId); err != nil {
		t.Fatal(err)
	}
	release()
	select {
	case next := <-done:
		if next != "/setup" {
			t.Fatal("callback created an account after the super user disappeared")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("callback did not finish")
	}
	if _, err := h.st.GetUserByEmail(t.Context(), "alice@example.com"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("callback saved a user")
	}
}

func TestProxyMissingForwardedWarning(t *testing.T) {
	for _, proxy := range []bool{false, true} {
		name := "direct"
		if proxy {
			name = "proxy"
		}
		t.Run(name, func(t *testing.T) {
			h := newWebHarness(t, nil, func(c *auth.WebConfig) { c.BehindProxy = proxy })
			req := connect.NewRequest(&circusv1.WhoAmIRequest{})
			req.Header().Set("X-Forwarded-For", "192.0.2.1")
			if _, err := h.rpc.WhoAmI(t.Context(), req); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(h.logs.String(), "without X-Forwarded-For") {
				t.Fatal("warned despite a forwarded address")
			}
			var wg sync.WaitGroup
			errs := make(chan error, 16)
			for range 16 {
				wg.Go(func() {
					_, err := h.rpc.WhoAmI(t.Context(), connect.NewRequest(&circusv1.WhoAmIRequest{}))
					errs <- err
				})
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				if err != nil {
					t.Fatal(err)
				}
			}
			// Login's IP helper must share the HTTP observation's once-only warning.
			_, err := h.rpc.Login(t.Context(), connect.NewRequest(&circusv1.LoginRequest{Email: "missing@example.com", Password: "wrong-password"}))
			wantCode(t, err, connect.CodeUnauthenticated)
			count := strings.Count(h.logs.String(), "without X-Forwarded-For")
			want := 0
			if proxy {
				want = 1
			}
			if count != want {
				t.Fatalf("proxy warnings = %d, want %d", count, want)
			}
		})
	}
}
