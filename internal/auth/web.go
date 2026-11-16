package auth

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/gen/go/circus/v1/circusv1connect"
	"github.com/EricWvi/circus/internal/store"
	"golang.org/x/crypto/bcrypt"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
)

const (
	// SessionCookie is the browser session cookie. Machine routes use Bearer tokens.
	SessionCookie       = "circus_session"
	DefaultBcryptCost   = 12
	bcryptWaitTimeout   = 3 * time.Second
	loginIPFailureLimit = 50
)

// OIDCConfig enables OpenID Connect when present. Discovery is lazy.
type OIDCConfig struct {
	Issuer       string `yaml:"issuer"`
	ClientID     string `yaml:"client_id"`
	ClientSecret string `yaml:"client_secret"`
	DisplayName  string `yaml:"display_name"`
}

// WebConfig supplies human authentication settings and test hooks.
// BcryptCost is programmatic only; production uses cost 12.
type WebConfig struct {
	BehindProxy bool
	PublicURL   string
	DevInsecure bool
	SetupURL    string
	OIDC        *OIDCConfig
	Now         func() time.Time
	BcryptCost  int
	Logger      *slog.Logger
	HTTPClient  *http.Client
}

// WebService serves browser authentication separately from machine credentials.
type WebService struct {
	st                     store.Store
	origin                 string
	secure                 bool
	behindProxy            bool
	bcryptSlots            chan struct{}
	proxyWarningOnce       sync.Once
	loginIPLimit           failureLimiter
	now                    func() time.Time
	logger                 *slog.Logger
	cost                   int
	dummy                  []byte
	oidc                   *OIDCConfig
	provider               *lazyProvider
	mu                     sync.Mutex
	setupHash              []byte // no plaintext setup token is retained
	setupLimit, loginLimit failureLimiter
	oidcLimit              failureLimiter
	flowKey                []byte
}

// NewWebService generates and logs a setup link only when no super user exists.
// The setup token is process-local and is replaced on every restart.
func NewWebService(ctx context.Context, st store.Store, cfg WebConfig) (*WebService, error) {
	if cfg.OIDC != nil && cfg.PublicURL == "" {
		return nil, errors.New("oidc requires public_url")
	}
	s := &WebService{st: st, origin: strings.TrimRight(cfg.PublicURL, "/"), secure: !cfg.DevInsecure,
		now: cfg.Now, logger: cfg.Logger, cost: cfg.BcryptCost, oidc: cfg.OIDC, behindProxy: cfg.BehindProxy, bcryptSlots: make(chan struct{}, 4), loginIPLimit: failureLimiter{maxAttempts: loginIPFailureLimit, backoff: 10 * time.Minute}}
	if s.now == nil {
		s.now = time.Now
	}
	if s.logger == nil {
		s.logger = slog.New(slog.DiscardHandler)
	}
	if s.cost == 0 {
		s.cost = DefaultBcryptCost
	}
	var err error
	s.dummy, err = bcrypt.GenerateFromPassword([]byte("circus-dummy-password"), s.cost)
	if err != nil {
		return nil, err
	}
	key, err := NewToken()
	if err != nil {
		return nil, err
	}
	s.flowKey = HashToken(key)
	if _, err := st.GetSuperUser(ctx); errors.Is(err, store.ErrNotFound) {
		token, err := NewToken()
		if err != nil {
			return nil, err
		}
		s.setupHash = HashToken(token)
		base := s.origin
		if base == "" {
			base = strings.TrimRight(cfg.SetupURL, "/")
		}
		hint := ""
		if base == "" && cfg.BehindProxy {
			hint = "Set public_url to the browser origin to print a complete setup link behind the proxy."
		}
		s.logger.Info("*** Initial setup required: open the link to create the super user. This token is valid only for this server run. ***", "url", base+"/setup?token="+token, "hint", hint)
	} else if err != nil {
		return nil, err
	}
	if cfg.OIDC != nil {
		client := cfg.HTTPClient
		if client == nil {
			client = &http.Client{Timeout: 15 * time.Second}
		}
		s.provider = &lazyProvider{issuer: cfg.OIDC.Issuer, clientID: cfg.OIDC.ClientID,
			clientSecret: cfg.OIDC.ClientSecret, redirectURL: s.origin + "/auth/oidc/callback", http: client}
	}
	return s, nil
}

// Register mounts the Connect service and the optional OIDC endpoints.
func (s *WebService) Register(mux *http.ServeMux) {
	path, h := circusv1connect.NewAuthServiceHandler(s, connect.WithReadMaxBytes(64<<10), connect.WithInterceptors(s.Interceptor()))
	mux.Handle(path, s.observeProxy(h))
	mux.Handle("GET /auth/oidc/login", s.observeProxy(http.HandlerFunc(s.oidcLogin)))
	mux.Handle("GET /auth/oidc/callback", s.observeProxy(http.HandlerFunc(s.oidcCallback)))
}

type webUserKey struct{}

// WebUser returns the user resolved by Interceptor or RequireWeb.
func WebUser(ctx context.Context) (store.User, bool) {
	u, ok := ctx.Value(webUserKey{}).(store.User)
	return u, ok
}

// Interceptor resolves cookies and applies Origin checks to state-changing
// Connect calls. Future cookie-authenticated services should install it and
// require WebUser(ctx) where authentication is mandatory. Machine services
// keep their independent Bearer authentication.
func (s *WebService) Interceptor() connect.Interceptor { return webInterceptor{s} }

type webInterceptor struct{ s *WebService }

func (i webInterceptor) requestContext(ctx context.Context, procedure string, header http.Header) (context.Context, error) {
	if procedure != circusv1connect.AuthServiceWhoAmIProcedure && !i.s.sameOrigin(header.Get("Origin")) {
		return ctx, connect.NewError(connect.CodePermissionDenied, errors.New("untrusted origin"))
	}
	switch procedure {
	case circusv1connect.AuthServiceSetupProcedure, circusv1connect.AuthServiceLoginProcedure, circusv1connect.AuthServiceLogoutProcedure:
		// These operations authenticate or remove their own credentials. In
		// particular, Setup checks its token before reading any browser session.
		return ctx, nil
	}
	u, err := i.s.resolve(ctx, header)
	if err == nil {
		return context.WithValue(ctx, webUserKey{}, u), nil
	}
	if errors.Is(err, ErrUnauthenticated) {
		return ctx, nil
	}
	return ctx, i.s.rpcError(ctx, err)
}
func (i webInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		ctx, err := i.requestContext(ctx, req.Spec().Procedure, req.Header())
		if err != nil {
			return nil, err
		}
		resp, err := next(ctx, req)
		if resp != nil {
			resp.Header().Set("Cache-Control", "no-store")
		}
		return resp, err
	}
}
func (i webInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}
func (i webInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		ctx, err := i.requestContext(ctx, conn.Spec().Procedure, conn.RequestHeader())
		if err != nil {
			return err
		}
		conn.ResponseHeader().Set("Cache-Control", "no-store")
		return next(ctx, conn)
	}
}

// RequireWeb provides the same cookie and Origin checks for HTTP handlers.
func (s *WebService) RequireWeb(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions && !s.sameOrigin(r.Header.Get("Origin")) {
			http.Error(w, "untrusted origin", http.StatusForbidden)
			return
		}
		u, err := s.resolve(r.Context(), r.Header)
		if err != nil {
			status := http.StatusUnauthorized
			if !errors.Is(err, ErrUnauthenticated) {
				s.logger.ErrorContext(r.Context(), "resolve web session", "error", err)
				status = http.StatusServiceUnavailable
			}
			http.Error(w, http.StatusText(status), status)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), webUserKey{}, u)))
	})
}

// Missing Origin is allowed for non-browser clients, matching homepage.
// Browser Connect requests cannot use simple form content types.
func (s *WebService) sameOrigin(origin string) bool {
	return s.origin == "" || origin == "" || origin == s.origin
}

func (s *WebService) resolve(ctx context.Context, header http.Header) (store.User, error) {
	c, err := (&http.Request{Header: header}).Cookie(SessionCookie)
	if err != nil || !WellFormed(c.Value) {
		return store.User{}, ErrUnauthenticated
	}
	sess, err := s.st.UseWebSession(ctx, HashToken(c.Value), s.now().UnixMilli())
	if errors.Is(err, store.ErrNotFound) {
		return store.User{}, ErrUnauthenticated
	}
	if err != nil {
		return store.User{}, err
	}
	u, err := s.st.GetUser(ctx, sess.UserID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && u.Email == "") {
		return store.User{}, ErrUnauthenticated
	}
	return u, err
}

func protoUser(u store.User) *circusv1.User {
	role := circusv1.User_ROLE_USER
	if u.Role == store.RoleSuper {
		role = circusv1.User_ROLE_SUPER
	}
	return &circusv1.User{UserId: u.ID, Email: u.Email, Role: role}
}

func (s *WebService) WhoAmI(ctx context.Context, _ *connect.Request[circusv1.WhoAmIRequest]) (*connect.Response[circusv1.WhoAmIResponse], error) {
	out := &circusv1.WhoAmIResponse{Methods: &circusv1.LoginMethods{Password: true}}
	if s.oidc != nil {
		out.Methods.Oidc = &circusv1.LoginMethods_OidcMethod{DisplayName: s.oidc.DisplayName}
	}
	if _, err := s.st.GetSuperUser(ctx); errors.Is(err, store.ErrNotFound) {
		out.State = &circusv1.WhoAmIResponse_NeedsSetup_{NeedsSetup: &circusv1.WhoAmIResponse_NeedsSetup{}}
	} else if err != nil {
		return nil, s.rpcError(ctx, err)
	} else if u, ok := WebUser(ctx); ok {
		out.State = &circusv1.WhoAmIResponse_User{User: protoUser(u)}
	} else {
		out.State = &circusv1.WhoAmIResponse_SignedOut_{SignedOut: &circusv1.WhoAmIResponse_SignedOut{}}
	}
	return connect.NewResponse(out), nil
}

func fieldError(code connect.Code, field, message string) *connect.Error {
	err := connect.NewError(code, errors.New(message))
	detail, detailErr := connect.NewErrorDetail(&errdetails.BadRequest{FieldViolations: []*errdetails.BadRequest_FieldViolation{{Field: field, Description: message}}})
	if detailErr == nil {
		err.AddDetail(detail)
	}
	return err
}
func checkPassword(password string) error {
	if len(password) < 8 || len(password) > 72 {
		return fieldError(connect.CodeInvalidArgument, "password", "password must be 8 to 72 bytes")
	}
	return nil
}

// validSetupToken must run under mu. Both comparison inputs have fixed
// SHA-256 length, including after the token has been consumed.
func (s *WebService) validSetupToken(token string) bool {
	got := HashToken(token)
	want := s.setupHash
	if len(want) == 0 {
		want = make([]byte, len(got))
	}
	equal := sameHash(got, want)
	return equal && len(s.setupHash) != 0
}

// checkSetup must run under mu. A correct token always bypasses backoff.
// Only wrong attempts reserve the caller's IP bucket.
func (s *WebService) checkSetup(ctx context.Context, token, ip string) error {
	if !s.validSetupToken(token) {
		attempt := s.setupLimit.reserve(ip, s.now())
		if attempt == nil {
			return tooManyAttempts()
		}
		attempt.finish(true, s.now())
		return fieldError(connect.CodePermissionDenied, "token", "invalid or used setup token")
	}
	return s.checkSetupAvailable(ctx)
}
func (s *WebService) checkSetupAvailable(ctx context.Context) error {
	if _, err := s.st.GetSuperUser(ctx); err == nil {
		s.setupHash = nil
		return connect.NewError(connect.CodeAlreadyExists, errors.New("a super user already exists"))
	} else if !errors.Is(err, store.ErrNotFound) {
		return s.rpcError(ctx, err)
	}
	return nil
}

func tooManyAttempts() error {
	return connect.NewError(connect.CodeResourceExhausted, errors.New("too_many_attempts"))
}

// acquireBcrypt queues expensive password work across emails and IPs. A
// caller waits for a slot until its request ends or the queue timeout elapses.
func (s *WebService) acquireBcrypt(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, bcryptContextError(err)
	}
	timer := time.NewTimer(bcryptWaitTimeout)
	defer timer.Stop()
	select {
	case s.bcryptSlots <- struct{}{}:
		release := func() { <-s.bcryptSlots }
		if err := ctx.Err(); err != nil {
			release()
			return nil, bcryptContextError(err)
		}
		return release, nil
	case <-ctx.Done():
		return nil, bcryptContextError(ctx.Err())
	case <-timer.C:
		if err := ctx.Err(); err != nil {
			return nil, bcryptContextError(err)
		}
		return nil, tooManyAttempts()
	}
}
func bcryptContextError(err error) error {
	code := connect.CodeCanceled
	if errors.Is(err, context.DeadlineExceeded) {
		code = connect.CodeDeadlineExceeded
	}
	return connect.NewError(code, err)
}

func (s *WebService) Setup(ctx context.Context, req *connect.Request[circusv1.SetupRequest]) (*connect.Response[circusv1.SetupResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkSetup(ctx, req.Msg.Token, s.clientIP(req.Header(), req.Peer().Addr)); err != nil {
		return nil, err
	}
	email := store.NormalizeEmail(req.Msg.Email)
	if err := store.CheckEmail(email); err != nil {
		return nil, fieldError(connect.CodeInvalidArgument, "email", "invalid email address")
	}
	if err := checkPassword(req.Msg.Password); err != nil {
		return nil, err
	}
	// Setup is serialized by mu and requires the correct bootstrap token.
	// Login saturation must not prevent its holder from completing setup.
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Msg.Password), s.cost)
	if err != nil {
		return nil, s.rpcError(ctx, err)
	}
	id, err := newID()
	if err != nil {
		return nil, s.rpcError(ctx, err)
	}
	u := store.User{ID: id, Email: email, Role: store.RoleSuper, PasswordHash: string(hash), CreatedAt: s.now().UnixMilli()}
	token, sess, err := s.newSession(u.ID)
	if err != nil {
		return nil, s.rpcError(ctx, err)
	}
	err = s.st.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		if existing, err := tx.GetUserByEmail(ctx, email); err == nil {
			if existing.Role != store.RoleSuper {
				return fieldError(connect.CodeInvalidArgument, "email", "email is already in use")
			}
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if err := tx.CreateSuperUser(ctx, u, nil); err != nil {
			return err
		}
		return tx.CreateWebSession(ctx, sess)
	})
	if err != nil {
		var ce *connect.Error
		if errors.As(err, &ce) {
			return nil, err
		}
		return nil, s.rpcError(ctx, err)
	}

	s.setupHash = nil
	out := connect.NewResponse(&circusv1.SetupResponse{User: protoUser(u)})
	out.Header().Add("Set-Cookie", s.sessionCookie(token).String())
	return out, nil
}

func (s *WebService) Login(ctx context.Context, req *connect.Request[circusv1.LoginRequest]) (*connect.Response[circusv1.LoginResponse], error) {
	email := store.NormalizeEmail(req.Msg.Email)
	ip := s.clientIP(req.Header(), req.Peer().Addr)
	ipAttempt := s.loginIPLimit.reserve(ip, s.now())
	if ipAttempt == nil {
		return nil, tooManyAttempts()
	}
	defer func() { ipAttempt.finish(false, s.now()) }()
	key := email + "\x00" + ip
	attempt := s.loginLimit.reserve(key, s.now())
	if attempt == nil {
		return nil, tooManyAttempts()
	}
	defer func() { attempt.finish(false, s.now()) }()
	release, err := s.acquireBcrypt(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	u, err := s.st.GetUserByEmail(ctx, email)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, s.rpcError(ctx, err)
	}
	hash := s.dummy
	if err == nil && u.PasswordHash != "" {
		hash = []byte(u.PasswordHash)
	}
	// bcrypt comparison can ignore a suffix past 72 bytes. Never accept it,
	// while still performing one comparison for malformed and unknown logins.
	validLength := len(req.Msg.Password) >= 8 && len(req.Msg.Password) <= 72
	password := []byte(req.Msg.Password)
	if !validLength {
		password = []byte("circus-invalid-password")
	}
	match := bcrypt.CompareHashAndPassword(hash, password) == nil
	if !match || !validLength || err != nil || u.PasswordHash == "" {
		attempt.finish(true, s.now())
		ipAttempt.finish(true, s.now())
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("incorrect email or password"))
	}
	token, sess, err := s.newSession(u.ID)
	if err == nil {
		err = s.st.CreateWebSession(ctx, sess)
	}
	if err != nil {
		return nil, s.rpcError(ctx, err)
	}
	attempt.finish(false, s.now())
	out := connect.NewResponse(&circusv1.LoginResponse{User: protoUser(u)})
	out.Header().Add("Set-Cookie", s.sessionCookie(token).String())
	return out, nil
}

// ObserveProxyRequest lets the HTTP host report missing proxy headers on
// every route, including health probes and machine endpoints. It logs once.
func (s *WebService) ObserveProxyRequest(r *http.Request) {
	s.warnMissingForwardedFor(r.Header)
}
func (s *WebService) observeProxy(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.ObserveProxyRequest(r)
		next.ServeHTTP(w, r)
	})
}
func (s *WebService) warnMissingForwardedFor(header http.Header) {
	if !s.behindProxy {
		return
	}
	for _, v := range header.Values("X-Forwarded-For") {
		if strings.TrimSpace(v) != "" {
			return
		}
	}
	s.proxyWarningOnce.Do(func() {
		s.logger.Warn("behind_proxy request without X-Forwarded-For; rate limits fall back to the proxy IP")
	})
}

// clientIP trusts only the rightmost forwarded address when explicitly
// behind a proxy. The proxy must append or replace X-Forwarded-For.
func (s *WebService) clientIP(header http.Header, peer string) string {
	s.warnMissingForwardedFor(header)
	if s.behindProxy {
		values := header.Values("X-Forwarded-For")
		if len(values) > 0 {
			last := values[len(values)-1]
			parts := strings.Split(last, ",")
			if ip, err := netip.ParseAddr(strings.TrimSpace(parts[len(parts)-1])); err == nil {
				return ip.Unmap().String()
			}
		}
	}
	host, _, err := net.SplitHostPort(peer)
	if err != nil {
		host = peer
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		return ip.Unmap().String()
	}
	return host
}

func (s *WebService) Logout(ctx context.Context, req *connect.Request[circusv1.LogoutRequest]) (*connect.Response[circusv1.LogoutResponse], error) {
	c, err := (&http.Request{Header: req.Header()}).Cookie(SessionCookie)
	if err == nil && WellFormed(c.Value) {
		if err := s.st.DeleteWebSession(ctx, HashToken(c.Value)); err != nil {
			return nil, s.rpcError(ctx, err)
		}
	}
	out := connect.NewResponse(&circusv1.LogoutResponse{})
	c = s.sessionCookie("")
	c.MaxAge = -1
	out.Header().Add("Set-Cookie", c.String())
	return out, nil
}
func (s *WebService) newSession(userID string) (string, store.WebSession, error) {
	token, err := NewToken()
	if err != nil {
		return "", store.WebSession{}, err
	}
	now := s.now()
	return token, store.WebSession{TokenHash: HashToken(token), UserID: userID, CreatedAt: now.UnixMilli(), LastUsedAt: now.UnixMilli(), ExpiresAt: now.Add(store.WebSessionIdleTimeout).UnixMilli()}, nil
}
func (s *WebService) sessionCookie(token string) *http.Cookie {
	return &http.Cookie{Name: SessionCookie, Value: token, Path: "/", MaxAge: int(store.WebSessionMaxLifetime.Seconds()), HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode}
}
func (s *WebService) rpcError(ctx context.Context, err error) error {
	if errors.Is(err, store.ErrExists) {
		return connect.NewError(connect.CodeAlreadyExists, errors.New("user or identity already exists"))
	}
	s.logger.ErrorContext(ctx, "web authentication failed", "error", err)
	code := connect.CodeInternal
	if errors.Is(err, store.ErrBusy) {
		code = connect.CodeUnavailable
	}
	return connect.NewError(code, errors.New("authentication service unavailable"))
}
