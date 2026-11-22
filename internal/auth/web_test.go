package auth_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"
	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/gen/go/circus/v1/circusv1connect"
	"github.com/EricWvi/circus/internal/auth"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/store/sqlite"
	"github.com/EricWvi/circus/internal/store/sqlite/sqlitetest"
	"golang.org/x/crypto/bcrypt"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
)

type webHarness struct {
	st     *sqlite.DB
	svc    *auth.WebService
	server *httptest.Server
	client *http.Client
	rpc    circusv1connect.AuthServiceClient
	token  string
	clock  atomic.Int64
	logs   bytes.Buffer
}

func newWebHarness(t *testing.T, oidc *auth.OIDCConfig, options ...func(*auth.WebConfig)) *webHarness {
	t.Helper()
	h := &webHarness{}
	h.clock.Store(time.Now().UnixMilli())
	st, err := sqlite.Open(t.Context(), sqlitetest.Path(t), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	h.st = st
	t.Cleanup(func() { st.Close() })
	mux := http.NewServeMux()
	h.server = httptest.NewServer(mux)
	t.Cleanup(h.server.Close)
	cfg := auth.WebConfig{PublicURL: h.server.URL, SetupURL: h.server.URL, DevInsecure: true,
		OIDC: oidc, Now: func() time.Time { return time.UnixMilli(h.clock.Load()) }, BcryptCost: bcrypt.MinCost, Logger: slog.New(slog.NewJSONHandler(&h.logs, nil))}
	for _, option := range options {
		option(&cfg)
	}
	h.svc, err = auth.NewWebService(t.Context(), st, cfg)
	if err != nil {
		t.Fatal(err)
	}
	h.svc.Register(mux)
	mux.Handle("POST /future", h.svc.RequireWeb(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, ok := auth.WebUser(r.Context())
		if !ok {
			t.Error("missing WebUser")
		}
		_, _ = io.WriteString(w, u.Email)
	})))
	mux.Handle("/future.v1.Service/Change", connect.NewUnaryHandler("/future.v1.Service/Change", func(ctx context.Context, _ *connect.Request[circusv1.WhoAmIRequest]) (*connect.Response[circusv1.User], error) {
		u, ok := auth.WebUser(ctx)
		if !ok {
			return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("sign in first"))
		}
		return connect.NewResponse(&circusv1.User{Email: u.Email}), nil
	}, connect.WithInterceptors(h.svc.Interceptor())))
	mux.Handle("/future.v1.Service/Watch", connect.NewServerStreamHandler("/future.v1.Service/Watch", func(ctx context.Context, _ *connect.Request[circusv1.WhoAmIRequest], stream *connect.ServerStream[circusv1.User]) error {
		u, ok := auth.WebUser(ctx)
		if !ok {
			return connect.NewError(connect.CodeUnauthenticated, errors.New("sign in first"))
		}
		return stream.Send(&circusv1.User{Email: u.Email})
	}, connect.WithInterceptors(h.svc.Interceptor())))

	for _, line := range strings.Split(h.logs.String(), "\n") {
		var rec struct {
			URL string `json:"url"`
		}
		if json.Unmarshal([]byte(line), &rec) == nil && rec.URL != "" {
			u, _ := url.Parse(rec.URL)
			h.token = u.Query().Get("token")
		}
	}
	if !auth.WellFormed(h.token) {
		t.Fatal("missing setup banner")
	}
	h.client = h.browser(t)
	h.rpc = circusv1connect.NewAuthServiceClient(h.client, h.server.URL)
	return h
}
func (h *webHarness) browser(t *testing.T) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	client := &http.Client{Jar: jar, Timeout: 10 * time.Second, Transport: transport, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	t.Cleanup(client.CloseIdleConnections)
	return client
}
func (h *webHarness) setup(t *testing.T) *circusv1.User {
	t.Helper()
	resp, err := h.rpc.Setup(t.Context(), connect.NewRequest(&circusv1.SetupRequest{Token: h.token, Email: " ADMIN@Example.com ", Password: "password-123"}))
	if err != nil {
		t.Fatal(err)
	}
	return resp.Msg.User
}
func who(t *testing.T, c circusv1connect.AuthServiceClient) *circusv1.WhoAmIResponse {
	t.Helper()
	resp, err := c.WhoAmI(t.Context(), connect.NewRequest(&circusv1.WhoAmIRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	return resp.Msg
}
func wantCode(t *testing.T, err error, code connect.Code) {
	t.Helper()
	if connect.CodeOf(err) != code {
		t.Fatalf("error = %v, want %v", err, code)
	}
}
func wantField(t *testing.T, err error, field string) {
	t.Helper()
	var ce *connect.Error
	if !errors.As(err, &ce) {
		t.Fatal(err)
	}
	for _, d := range ce.Details() {
		v, e := d.Value()
		if e == nil {
			if b, ok := v.(*errdetails.BadRequest); ok {
				for _, f := range b.FieldViolations {
					if f.Field == field {
						return
					}
				}
			}
		}
	}
	t.Fatalf("error %v lacks field %s", err, field)
}
func TestWebSetupLoginLogout(t *testing.T) {
	h := newWebHarness(t, nil)
	status := who(t, h.rpc)
	if status.GetNeedsSetup() == nil || !status.Methods.Password || status.Methods.Oidc != nil {
		t.Fatalf("state = %v", status)
	}
	_, err := h.rpc.Setup(t.Context(), connect.NewRequest(&circusv1.SetupRequest{Token: "bad", Email: "bad", Password: "short"}))
	wantCode(t, err, connect.CodePermissionDenied)
	wantField(t, err, "token")
	for _, tc := range []struct{ email, password, field string }{{"bad", "password-123", "email"}, {"a@@b", "password-123", "email"}, {"a@ b", "password-123", "email"}, {strings.Repeat("x", 254) + "@b", "password-123", "email"}, {"a@b", "short", "password"}, {"a@b", strings.Repeat("é", 37), "password"}} {
		_, err := h.rpc.Setup(t.Context(), connect.NewRequest(&circusv1.SetupRequest{Token: h.token, Email: tc.email, Password: tc.password}))
		wantCode(t, err, connect.CodeInvalidArgument)
		wantField(t, err, tc.field)
	}
	u := h.setup(t)
	if u.Email != "admin@example.com" || u.Role != circusv1.User_ROLE_SUPER {
		t.Fatalf("user = %v", u)
	}
	stored, err := h.st.GetUser(t.Context(), u.UserId)
	if err != nil {
		t.Fatal(err)
	}
	cost, err := bcrypt.Cost([]byte(stored.PasswordHash))
	if err != nil || cost != bcrypt.MinCost {
		t.Fatalf("hash cost %d, %v", cost, err)
	}
	if who(t, h.rpc).GetUser().GetUserId() != u.UserId {
		t.Fatal("setup did not sign in")
	}
	base, _ := url.Parse(h.server.URL)
	cookies := h.client.Jar.Cookies(base)
	if len(cookies) != 1 || cookies[0].Name != auth.SessionCookie {
		t.Fatalf("cookies = %v", cookies)
	}
	token := cookies[0].Value
	sess, err := h.st.UseWebSession(t.Context(), auth.HashToken(token), h.clock.Load())
	if err != nil || sess.UserID != u.UserId {
		t.Fatal("session not stored by hash", err)
	}
	_, err = h.rpc.Setup(t.Context(), connect.NewRequest(&circusv1.SetupRequest{Token: h.token, Email: "new@example.com", Password: "password-123"}))
	wantCode(t, err, connect.CodePermissionDenied)
	_, err = h.rpc.Logout(t.Context(), connect.NewRequest(&circusv1.LogoutRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if who(t, h.rpc).GetSignedOut() == nil {
		t.Fatal("logout left user signed in")
	}
	_, err = h.st.UseWebSession(t.Context(), auth.HashToken(token), h.clock.Load())
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatal("session survived logout", err)
	}
	_, err = h.rpc.Logout(t.Context(), connect.NewRequest(&circusv1.LogoutRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.rpc.Login(t.Context(), connect.NewRequest(&circusv1.LoginRequest{Email: "ADMIN@example.com", Password: "password-123"}))
	if err != nil {
		t.Fatal(err)
	}
	if who(t, h.rpc).GetUser().GetUserId() != u.UserId {
		t.Fatal("login did not sign in")
	}
	for _, raw := range []string{"garbage", strings.Repeat("A", 43)} {
		req := connect.NewRequest(&circusv1.WhoAmIRequest{})
		req.Header().Set("Cookie", auth.SessionCookie+"="+raw)
		c := circusv1connect.NewAuthServiceClient(h.browser(t), h.server.URL)
		resp, err := c.WhoAmI(t.Context(), req)
		if err != nil || resp.Msg.GetSignedOut() == nil {
			t.Fatalf("invalid cookie: %v %v", resp, err)
		}
	}
}
func TestWebOriginAndFutureServices(t *testing.T) {
	h := newWebHarness(t, nil)
	bad := connect.NewRequest(&circusv1.SetupRequest{Token: h.token, Email: "a@b", Password: "password-123"})
	bad.Header().Set("Origin", "https://evil.example")
	_, err := h.rpc.Setup(t.Context(), bad)
	wantCode(t, err, connect.CodePermissionDenied)
	good := connect.NewRequest(&circusv1.SetupRequest{Token: h.token, Email: "a@b", Password: "password-123"})
	good.Header().Set("Origin", h.server.URL)
	setup, err := h.rpc.Setup(t.Context(), good)
	if err != nil {
		t.Fatal(err)
	}
	cookies := setup.Header().Values("Set-Cookie")
	if len(cookies) != 1 || !strings.Contains(cookies[0], "HttpOnly") || !strings.Contains(cookies[0], "SameSite=Lax") || !strings.Contains(cookies[0], "Path=/") || strings.Contains(cookies[0], "Secure") {
		t.Fatalf("cookie = %v", cookies)
	}
	badLogin := connect.NewRequest(&circusv1.LoginRequest{})
	badLogin.Header().Set("Origin", "https://evil.example")
	_, err = h.rpc.Login(t.Context(), badLogin)
	wantCode(t, err, connect.CodePermissionDenied)
	badLogout := connect.NewRequest(&circusv1.LogoutRequest{})
	badLogout.Header().Set("Origin", "https://evil.example")
	_, err = h.rpc.Logout(t.Context(), badLogout)
	wantCode(t, err, connect.CodePermissionDenied)
	status := connect.NewRequest(&circusv1.WhoAmIRequest{})
	status.Header().Set("Origin", "https://evil.example")
	if _, err = h.rpc.WhoAmI(t.Context(), status); err != nil {
		t.Fatal(err)
	}
	for _, origin := range []string{h.server.URL, "https://evil.example"} {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, h.server.URL+"/future", nil)
		req.Header.Set("Origin", origin)
		resp, err := h.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		want := http.StatusOK
		if origin != h.server.URL {
			want = http.StatusForbidden
		}
		if resp.StatusCode != want {
			t.Fatalf("future origin %q: %d %s", origin, resp.StatusCode, body)
		}
	}
	future := connect.NewClient[circusv1.WhoAmIRequest, circusv1.User](h.client, h.server.URL+"/future.v1.Service/Change")
	req := connect.NewRequest(&circusv1.WhoAmIRequest{})
	req.Header().Set("Origin", h.server.URL)
	resp, err := future.CallUnary(t.Context(), req)
	if err != nil || resp.Msg.Email != "a@b" {
		t.Fatalf("future Connect user: %v %v", resp, err)
	}
	req.Header().Set("Origin", "https://evil.example")
	_, err = future.CallUnary(t.Context(), req)
	wantCode(t, err, connect.CodePermissionDenied)
	watch := connect.NewClient[circusv1.WhoAmIRequest, circusv1.User](h.client, h.server.URL+"/future.v1.Service/Watch")
	for _, origin := range []string{h.server.URL, "https://evil.example"} {
		watchReq := connect.NewRequest(&circusv1.WhoAmIRequest{})
		watchReq.Header().Set("Origin", origin)
		stream, err := watch.CallServerStream(t.Context(), watchReq)
		if err != nil {
			t.Fatal(err)
		}
		if origin == h.server.URL {
			if !stream.Receive() || stream.Msg().Email != "a@b" {
				t.Fatalf("stream user: %v", stream.Err())
			}
			if stream.Receive() || stream.Err() != nil {
				t.Fatalf("stream end: %v", stream.Err())
			}
		} else {
			if stream.Receive() {
				t.Fatal("cross-origin stream accepted")
			}
			wantCode(t, stream.Err(), connect.CodePermissionDenied)
		}
		if err := stream.Close(); err != nil {
			t.Fatal(err)
		}
	}

}
func TestWebSessionExpiryAndLimiter(t *testing.T) {
	h := newWebHarness(t, nil)
	h.setup(t)
	for i := 0; i < 5; i++ {
		_, err := h.rpc.Login(t.Context(), connect.NewRequest(&circusv1.LoginRequest{Email: "missing@example.com", Password: "wrong"}))
		wantCode(t, err, connect.CodeUnauthenticated)
	}
	_, err := h.rpc.Login(t.Context(), connect.NewRequest(&circusv1.LoginRequest{Email: " MISSING@EXAMPLE.COM ", Password: "wrong"}))
	wantCode(t, err, connect.CodeResourceExhausted)
	// A different email has an independent limiter bucket, even on the same IP.
	_, err = h.rpc.Login(t.Context(), connect.NewRequest(&circusv1.LoginRequest{Email: "admin@example.com", Password: "password-123"}))
	if err != nil {
		t.Fatal(err)
	}
	h.clock.Add((11 * time.Minute).Milliseconds())
	_, err = h.rpc.Login(t.Context(), connect.NewRequest(&circusv1.LoginRequest{Email: "missing@example.com", Password: "wrong"}))
	wantCode(t, err, connect.CodeUnauthenticated)
	h.clock.Add(store.WebSessionIdleTimeout.Milliseconds())
	if who(t, h.rpc).GetSignedOut() == nil {
		t.Fatal("idle session did not expire")
	}
	_, err = h.rpc.Login(t.Context(), connect.NewRequest(&circusv1.LoginRequest{Email: "admin@example.com", Password: "password-123"}))
	if err != nil {
		t.Fatal(err)
	}
	for day := 0; day < 29; day++ {
		h.clock.Add((24 * time.Hour).Milliseconds())
		if who(t, h.rpc).GetUser() == nil {
			t.Fatal("active session expired early")
		}
	}
	h.clock.Add((24 * time.Hour).Milliseconds())
	if who(t, h.rpc).GetSignedOut() == nil {
		t.Fatal("session exceeded hard lifetime")
	}
	// Accounts without a password pay for the dummy comparison and fail.
	if err := h.st.CreateUser(t.Context(), store.User{ID: "oidc-only", Email: "oidc@example.com", Role: store.RoleUser, CreatedAt: h.clock.Load()}); err != nil {
		t.Fatal(err)
	}
	_, err = h.rpc.Login(t.Context(), connect.NewRequest(&circusv1.LoginRequest{Email: "oidc@example.com", Password: "password-123"}))
	wantCode(t, err, connect.CodeUnauthenticated)
}
func TestWebSetupRestartAndBackoff(t *testing.T) {
	h := newWebHarness(t, nil)
	if strings.Count(h.logs.String(), "Initial setup required") != 1 {
		t.Fatal("setup banner count")
	}
	for i := 0; i < 5; i++ {
		_, err := h.rpc.Setup(t.Context(), connect.NewRequest(&circusv1.SetupRequest{Token: strings.Repeat("x", i)}))
		wantCode(t, err, connect.CodePermissionDenied)
	}
	_, err := h.rpc.Setup(t.Context(), connect.NewRequest(&circusv1.SetupRequest{Token: "another-bad-token"}))
	wantCode(t, err, connect.CodeResourceExhausted)
	_, err = h.rpc.Setup(t.Context(), connect.NewRequest(&circusv1.SetupRequest{Token: h.token}))
	wantCode(t, err, connect.CodeInvalidArgument)
	h.clock.Add((11 * time.Minute).Milliseconds())
	var logs bytes.Buffer
	svc, err := auth.NewWebService(t.Context(), h.st, auth.WebConfig{SetupURL: h.server.URL, DevInsecure: true, BcryptCost: bcrypt.MinCost, Logger: slog.New(slog.NewJSONHandler(&logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	var rec struct {
		URL string `json:"url"`
	}
	if err = json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &rec); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(rec.URL)
	newToken := u.Query().Get("token")
	if newToken == h.token || !auth.WellFormed(newToken) {
		t.Fatal("restart did not replace setup token")
	}
	mux := http.NewServeMux()
	svc.Register(mux)
	server := httptest.NewServer(mux)
	defer server.Close()
	rpc := circusv1connect.NewAuthServiceClient(h.browser(t), server.URL)
	_, err = rpc.Setup(t.Context(), connect.NewRequest(&circusv1.SetupRequest{Token: h.token}))
	wantCode(t, err, connect.CodePermissionDenied)
	resp, err := rpc.Setup(t.Context(), connect.NewRequest(&circusv1.SetupRequest{Token: newToken, Email: "a@b", Password: "password-123"}))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Msg.User.Role != circusv1.User_ROLE_SUPER {
		t.Fatal(resp.Msg)
	}
	logs.Reset()
	_, err = auth.NewWebService(t.Context(), h.st, auth.WebConfig{BcryptCost: bcrypt.MinCost, Logger: slog.New(slog.NewJSONHandler(&logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	if logs.Len() != 0 {
		t.Fatal("setup banner printed after setup")
	}
}

func TestWebPasswordByteLimit(t *testing.T) {
	for _, password := range []string{strings.Repeat("x", 72), strings.Repeat("é", 36)} {
		t.Run(fmt.Sprintf("%d runes", utf8.RuneCountInString(password)), func(t *testing.T) {
			h := newWebHarness(t, nil)
			_, err := h.rpc.Setup(t.Context(), connect.NewRequest(&circusv1.SetupRequest{Token: h.token, Email: "a@b", Password: password}))
			if err != nil {
				t.Fatal(err)
			}
			_, err = h.rpc.Login(t.Context(), connect.NewRequest(&circusv1.LoginRequest{Email: "a@b", Password: password}))
			if err != nil {
				t.Fatal(err)
			}
			_, err = h.rpc.Login(t.Context(), connect.NewRequest(&circusv1.LoginRequest{Email: "a@b", Password: password + "suffix"}))
			wantCode(t, err, connect.CodeUnauthenticated)
		})
	}
}
