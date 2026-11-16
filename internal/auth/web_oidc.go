package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"

	"connectrpc.com/connect"
	"github.com/EricWvi/circus/internal/store"
	"golang.org/x/oauth2"
)

const loginCookie = "circus_login"
const loginLifetime = 10 * time.Minute

// loginFlow is signed and kept only in the browser's short-lived HttpOnly
// cookie. State, nonce and PKCE bind authorization to this browser. The setup
// token is retained for retry redirects; the server keeps only its hash.
type loginFlow struct {
	State      string `json:"s"`
	Nonce      string `json:"n"`
	Verifier   string `json:"v"`
	Next       string `json:"next,omitempty"`
	SetupToken string `json:"setup,omitempty"`
	ExpiresAt  int64  `json:"exp"`
}

// validNext returns an empty string for absent or unsafe paths so failure
// redirects can distinguish an explicit next=/ from no next parameter.
func validNext(next string) string {
	u, err := url.Parse(next)
	if err != nil || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || u.IsAbs() || u.Host != "" ||
		strings.HasPrefix(u.Path, "//") || strings.ContainsAny(next, "\\") || strings.ContainsAny(u.Path, "\\") ||
		strings.ContainsFunc(next+u.Path, unicode.IsControl) {
		return ""
	}
	return next
}
func safeNext(next string) string {
	if next = validNext(next); next != "" {
		return next
	}
	return "/"
}

func (s *WebService) oidcLogin(w http.ResponseWriter, r *http.Request) {
	if s.provider == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if !s.sameOrigin(r.Header.Get("Origin")) {
		http.Error(w, "untrusted origin", http.StatusForbidden)
		return
	}
	flow := loginFlow{Next: validNext(r.URL.Query().Get("next")), SetupToken: r.URL.Query().Get("setup_token")}
	if _, present := r.URL.Query()["setup_token"]; present {
		s.mu.Lock()
		err := s.checkSetup(r.Context(), flow.SetupToken, s.clientIP(r.Header, r.RemoteAddr))
		s.mu.Unlock()
		if err != nil {
			s.setupRejected(w, r, flow, err)
			return
		}
	} else {
		if _, err := s.st.GetSuperUser(r.Context()); errors.Is(err, store.ErrNotFound) {
			http.Redirect(w, r, "/setup", http.StatusFound)
			return
		} else if err != nil {
			s.oidcFailed(w, r, flow, err)
			return
		}
	}
	// A holder of the correct setup token never enters a limiter, including
	// when an identity provider is temporarily unavailable.
	var attempt *loginAttempt
	if flow.SetupToken == "" {
		attempt = s.oidcLimit.reserve(s.clientIP(r.Header, r.RemoteAddr), s.now())
		if attempt == nil {
			s.oidcRedirect(w, r, flow, "too_many_attempts")
			return
		}
		defer func() { attempt.finish(false, s.now()) }()
	}
	p, err := s.provider.get(r.Context())
	if err != nil {
		if attempt != nil {
			attempt.finish(true, s.now())
		}
		s.oidcFailed(w, r, flow, err)
		return
	}
	flow.State, err = NewToken()
	if err != nil {
		s.oidcFailed(w, r, flow, err)
		return
	}
	flow.Nonce, err = NewToken()
	if err != nil {
		s.oidcFailed(w, r, flow, err)
		return
	}
	flow.Verifier = oauth2.GenerateVerifier()
	flow.ExpiresAt = s.now().Add(loginLifetime).UnixMilli()
	raw, err := json.Marshal(flow)
	if err != nil {
		s.oidcFailed(w, r, flow, err)
		return
	}
	payload := base64.RawURLEncoding.EncodeToString(raw)
	mac := hmac.New(sha256.New, s.flowKey)
	_, _ = mac.Write([]byte(payload))
	value := payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	http.SetCookie(w, s.flowCookie(value, int(loginLifetime.Seconds())))
	http.Redirect(w, r, p.oauth.AuthCodeURL(flow.State, oauth2.S256ChallengeOption(flow.Verifier), oauth2.SetAuthURLParam("nonce", flow.Nonce)), http.StatusFound)
}
func (s *WebService) flowCookie(value string, maxAge int) *http.Cookie {
	return &http.Cookie{Name: loginCookie, Value: value, Path: "/auth/oidc/", MaxAge: maxAge, HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode}
}
func (s *WebService) readFlow(r *http.Request) (loginFlow, bool) {
	var flow loginFlow
	c, err := r.Cookie(loginCookie)
	if err != nil {
		return flow, false
	}
	payload, signature, ok := strings.Cut(c.Value, ".")
	if !ok {
		return flow, false
	}
	sig, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil {
		return flow, false
	}
	mac := hmac.New(sha256.New, s.flowKey)
	_, _ = mac.Write([]byte(payload))
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return flow, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil || json.Unmarshal(raw, &flow) != nil {
		return loginFlow{}, false
	}
	// Even an expired, signed flow can supply safe retry parameters, but it
	// cannot be used to exchange a code or authenticate.
	return flow, flow.State != "" && flow.Nonce != "" && flow.Verifier != "" && s.now().UnixMilli() < flow.ExpiresAt
}

func (s *WebService) oidcCallback(w http.ResponseWriter, r *http.Request) {
	if s.provider == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	flow, ok := s.readFlow(r)
	http.SetCookie(w, s.flowCookie("", -1))
	// Refuse normal login even for an existing identity until setup completes.
	// The transaction repeats this check after the network round trip.
	if flow.SetupToken == "" {
		if _, err := s.st.GetSuperUser(r.Context()); errors.Is(err, store.ErrNotFound) {
			http.Redirect(w, r, "/setup", http.StatusFound)
			return
		} else if err != nil {
			s.oidcFailed(w, r, flow, err)
			return
		}
	}
	if !ok || !sameHash(HashToken(flow.State), HashToken(r.URL.Query().Get("state"))) {
		s.oidcFailed(w, r, flow, errors.New("invalid OIDC flow state"))
		return
	}
	setup := flow.SetupToken != ""
	if setup {
		if _, err := s.st.GetSuperUser(r.Context()); err == nil {
			s.setupRejected(w, r, flow, connect.NewError(connect.CodeAlreadyExists, errors.New("a super user already exists")))
			return
		} else if !errors.Is(err, store.ErrNotFound) {
			s.oidcFailed(w, r, flow, err)
			return
		}
	}
	if r.URL.Query().Get("error") != "" {
		s.oidcFailed(w, r, flow, errors.New("OIDC authorization denied"))
		return
	}
	// Discovery, code exchange and UserInfo must never hold the setup mutex.
	profile, err := s.provider.identify(r.Context(), r.URL.Query().Get("code"), flow)
	if err != nil {
		s.oidcFailed(w, r, flow, err)
		return
	}
	profile.Email = store.NormalizeEmail(profile.Email)

	if setup {
		s.mu.Lock()
		defer s.mu.Unlock()
		// The signed flow already passed the start limiter. Recheck only token
		// validity and setup state, under the lock used by password setup.
		if err := s.checkSetupAvailable(r.Context()); err != nil {
			s.setupRejected(w, r, flow, err)
			return
		}
		if !s.validSetupToken(flow.SetupToken) {
			s.setupRejected(w, r, flow, fieldError(connect.CodePermissionDenied, "token", "invalid or used setup token"))
			return
		}
		if !profile.EmailVerified || store.CheckEmail(profile.Email) != nil {
			s.oidcRedirect(w, r, flow, "email_unverified")
			return
		}
	}
	var token string
	err = s.st.Tx(r.Context(), func(ctx context.Context, tx store.Ops) error {
		if !setup {
			if _, err := tx.GetSuperUser(ctx); errors.Is(err, store.ErrNotFound) {
				return errNeedsSetup
			} else if err != nil {
				return err
			}
		}
		u, err := s.oidcUser(ctx, tx, profile, setup)
		if err != nil {
			return err
		}
		var sess store.WebSession
		token, sess, err = s.newSession(u.ID)
		if err != nil {
			return err
		}
		return tx.CreateWebSession(ctx, sess)
	})
	switch {
	case errors.Is(err, errNeedsSetup):
		http.Redirect(w, r, "/setup", http.StatusFound)
		return
	case errors.Is(err, errEmailUnverified):
		s.oidcRedirect(w, r, flow, "email_unverified")
		return
	case errors.Is(err, errAccountConflict):
		s.logger.WarnContext(r.Context(), "OIDC account conflict", "issuer", profile.Identity.Issuer)
		s.oidcRedirect(w, r, flow, "account_conflict")
		return
	}
	if setup && errors.Is(err, store.ErrExists) {
		if _, superErr := s.st.GetSuperUser(r.Context()); superErr == nil {
			s.setupHash = nil
			s.setupRejected(w, r, flow, connect.NewError(connect.CodeAlreadyExists, errors.New("a super user already exists")))
			return
		}
	}
	if err != nil {
		s.oidcFailed(w, r, flow, err)
		return
	}
	if setup {
		s.setupHash = nil
	}
	http.SetCookie(w, s.sessionCookie(token))
	next := safeNext(flow.Next)
	if setup {
		next = "/"
	}
	http.Redirect(w, r, next, http.StatusFound)
}

var (
	errNeedsSetup      = errors.New("super user setup is required")
	errEmailUnverified = errors.New("missing or unverified email")
	errAccountConflict = errors.New("account already has an identity at this issuer")
)

// oidcUser runs inside the transaction that creates the browser session.
func (s *WebService) oidcUser(ctx context.Context, tx store.Ops, profile oidcProfile, setup bool) (store.User, error) {
	identity := profile.Identity
	identity.CreatedAt = s.now().UnixMilli()
	if !setup {
		found, err := tx.GetUserIdentity(ctx, identity.Issuer, identity.Subject)
		if err == nil {
			return tx.GetUser(ctx, found.UserID)
		}
		if !errors.Is(err, store.ErrNotFound) {
			return store.User{}, err
		}
	}
	if !profile.EmailVerified || store.CheckEmail(profile.Email) != nil {
		return store.User{}, errEmailUnverified
	}
	var u store.User
	if !setup {
		var err error
		u, err = tx.GetUserByEmail(ctx, profile.Email)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return u, err
		}
		if u.ID != "" {
			exists, err := tx.HasUserIdentityAtIssuer(ctx, u.ID, identity.Issuer)
			if err != nil {
				return u, err
			}
			if exists {
				return u, errAccountConflict
			}
		}
	}
	if u.ID == "" {
		id, err := newID()
		if err != nil {
			return u, err
		}
		u = store.User{ID: id, Email: profile.Email, Role: store.RoleUser, CreatedAt: identity.CreatedAt}
		identity.UserID = u.ID
		if setup {
			u.Role = store.RoleSuper
			return u, tx.CreateSuperUser(ctx, u, &identity)
		}
		if err := tx.CreateUser(ctx, u); err != nil {
			return u, err
		}
	}
	identity.UserID = u.ID
	return u, tx.CreateUserIdentity(ctx, identity)
}

// setupRejected maps setup credential/state errors without exposing provider
// details. Invalid credentials are never echoed back in redirect parameters.
func (s *WebService) setupRejected(w http.ResponseWriter, r *http.Request, flow loginFlow, err error) {
	reason := "invalid_token"
	switch connect.CodeOf(err) {
	case connect.CodePermissionDenied:
	case connect.CodeAlreadyExists:
		reason = "already_set_up"
	case connect.CodeResourceExhausted:
		reason = "too_many_attempts"
	default:
		s.oidcFailed(w, r, flow, err)
		return
	}
	http.Redirect(w, r, "/setup?error="+reason, http.StatusFound)
}
func (s *WebService) oidcRedirect(w http.ResponseWriter, r *http.Request, flow loginFlow, reason string) {
	path := "/login"
	params := url.Values{"error": {reason}}
	if flow.SetupToken != "" {
		path = "/setup"
		params.Set("token", flow.SetupToken)
	} else if next := validNext(flow.Next); next != "" {
		params.Set("next", next)
	}
	http.Redirect(w, r, path+"?"+params.Encode(), http.StatusFound)
}
func (s *WebService) oidcFailed(w http.ResponseWriter, r *http.Request, flow loginFlow, err error) {
	s.logger.WarnContext(r.Context(), "OIDC login failed", "error", err)
	s.oidcRedirect(w, r, flow, "oidc_failed")
}
