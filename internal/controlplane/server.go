package controlplane

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/EricWvi/circus/gen/go/circus/v1/circusv1connect"
	"github.com/EricWvi/circus/internal/auth"
	"github.com/EricWvi/circus/internal/clock"
	"github.com/EricWvi/circus/internal/controlplane/calls"
	"github.com/EricWvi/circus/internal/controlplane/timers"
	"github.com/EricWvi/circus/internal/loopback"
	"github.com/EricWvi/circus/internal/store"
)

// Defaults for Config.
const (
	DefaultShutdownTimeout = 10 * time.Second
	// readyTimeout bounds one /readyz evaluation.
	readyTimeout = 2 * time.Second
	// hookGrace is how long Shutdown still collects hook results after its
	// deadline, for hooks that return as the deadline passes.
	hookGrace = 50 * time.Millisecond
	// maxEnrollBytes bounds an Enroll request body.
	maxEnrollBytes = 64 << 10
)

// ErrShutdownTimeout reports that Shutdown hit its deadline and closed the
// remaining connections forcibly.
var ErrShutdownTimeout = errors.New("controlplane: shutdown deadline exceeded; connections closed forcibly")

// Config configures a Server. Exactly one of TLSCert/TLSKey, BehindProxy and
// DevInsecure must be set.
type Config struct {
	// Listen is the TCP address, host:port. Port 0 picks a free port; see
	// Server.Addr.
	Listen string
	// TLSCert and TLSKey are PEM files; the server terminates TLS.
	TLSCert, TLSKey string
	// BehindProxy serves plaintext for a reverse proxy that terminates TLS.
	BehindProxy bool
	// DevInsecure serves plaintext on a loopback address, for development
	// and tests. Start refuses it on any other address.
	DevInsecure bool
	// PublicURL is the base URL clients use (https://host:port), returned by
	// Enroll as control_plane_url; empty returns an empty URL.
	PublicURL string
	// ShutdownTimeout bounds Run's graceful shutdown; zero means
	// DefaultShutdownTimeout.
	ShutdownTimeout time.Duration
	// TimerPollInterval is the durable-timer claim poll interval; zero means 1s.
	TimerPollInterval time.Duration
	// LogOutput receives JSON log lines; nil means os.Stderr.
	LogOutput io.Writer
	// LogLevel is the minimum level logged.
	LogLevel slog.Level
	// Now supplies time; nil means time.Now.
	Now func() time.Time
}

func (c Config) validate() error {
	modes := 0
	if c.TLSCert != "" || c.TLSKey != "" {
		if c.TLSCert == "" || c.TLSKey == "" {
			return errors.New("--tls-cert and --tls-key must be given together")
		}
		modes++
	}
	if c.BehindProxy {
		modes++
	}
	if c.DevInsecure {
		modes++
	}
	if modes != 1 {
		return errors.New("choose exactly one of --tls-cert/--tls-key, --behind-proxy, or --dev-insecure (loopback only)")
	}
	host, _, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return fmt.Errorf("listen address %q: %w", c.Listen, err)
	}
	if c.DevInsecure && !loopback.Host(host) {
		return fmt.Errorf("--dev-insecure serves plaintext and is allowed only on a loopback address (127.0.0.1, ::1, localhost), not %q", c.Listen)
	}
	if c.ShutdownTimeout < 0 || c.TimerPollInterval < 0 {
		return errors.New("negative timeout or interval")
	}
	return nil
}

type readyCheck struct {
	name string
	fn   func(context.Context) error
}

type shutdownHook struct {
	name string
	fn   func(context.Context) error
}

// Server is one control plane process. Its methods are safe for concurrent
// use.
type Server struct {
	cfg        Config
	st         store.Store
	instanceID string
	logger     *slog.Logger
	authn      *auth.Authenticator
	user       store.User
	mux        *http.ServeMux
	now        func() time.Time
	listen     func(network, addr string) (net.Listener, error) // net.Listen; tests replace it

	mu         sync.Mutex
	started    bool
	stopping   bool
	checks     []readyCheck
	hooks      []shutdownHook
	httpSrv    *http.Server
	ln         net.Listener
	serveErr   chan error
	closeLinks func(context.Context) error
	router     calls.Router
	runner     timers.Runner
	timersDone chan struct{}
	stopBg     context.CancelFunc
	bg         sync.WaitGroup

	shutdownOnce sync.Once
	shutdownErr  error
}

// New validates cfg, ensures the default user exists in st and builds the
// mux with the built-in endpoints. It does not listen yet.
func New(ctx context.Context, st store.Store, cfg Config) (*Server, error) {
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("controlplane: %w", err)
	}
	if cfg.ShutdownTimeout == 0 {
		cfg.ShutdownTimeout = DefaultShutdownTimeout
	}
	if cfg.TimerPollInterval == 0 {
		cfg.TimerPollInterval = time.Second
	}
	if cfg.LogOutput == nil {
		cfg.LogOutput = os.Stderr
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	id, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("controlplane: instance id: %w", err)
	}
	s := &Server{cfg: cfg, st: st, instanceID: id.String(), mux: http.NewServeMux(), now: now, listen: net.Listen}
	s.logger = slog.New(slog.NewJSONHandler(cfg.LogOutput, &slog.HandlerOptions{Level: cfg.LogLevel})).
		With("instance_id", s.instanceID)
	s.authn = auth.NewAuthenticator(st, auth.Options{Now: now, Logger: s.logger})
	s.user, err = auth.EnsureUser(ctx, st, auth.DefaultUserName, now())
	if err != nil {
		return nil, fmt.Errorf("controlplane: default user: %w", err)
	}

	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { writeText(w, http.StatusOK, "ok\n") })
	s.mux.HandleFunc("GET /readyz", s.serveReady)
	path, h := circusv1connect.NewEnrollServiceHandler(&enrollService{s: s}, connectReadLimit())
	s.mux.Handle(path, h)
	s.mux.Handle("GET /v1/whoami", s.authn.Require(http.HandlerFunc(serveWhoami), store.KindDevice, store.KindWorker))
	s.mux.HandleFunc("GET /join", func(w http.ResponseWriter, _ *http.Request) {
		writeText(w, http.StatusOK, "This is a circus join link. Run on the machine to enroll:\n\n  circus join '<the full link, including the part after #>'\n")
	})
	if err := s.seedCleanup(ctx); err != nil {
		return nil, err
	}
	s.runner, err = timers.New(timers.Config{Store: st, Clock: clock.Real{}, InstanceID: s.instanceID, Lease: 5 * time.Second, PollInterval: cfg.TimerPollInterval, BatchSize: 64, Handler: s.handleTimer})
	if err != nil {
		return nil, err
	}
	s.AddReadyCheck("timers", func(context.Context) error {
		if v := s.runner.Stats(); !v.Running || !v.Healthy {
			return errors.New("timers unhealthy")
		}
		return nil
	})
	return s, nil
}

// InstanceID returns this process's instance id (UUIDv7), present on every
// log line.
func (s *Server) InstanceID() string { return s.instanceID }

// Logger returns the server's logger; every line carries instance_id.
func (s *Server) Logger() *slog.Logger { return s.logger }

// Store returns the store the server was built on.
func (s *Server) Store() store.Store { return s.st }

// Authenticator returns the authenticator for link upgrade requests.
func (s *Server) Authenticator() *auth.Authenticator { return s.authn }

// User returns the default user (M2 has one, auth.DefaultUserName).
func (s *Server) User() store.User { return s.user }

// Mount registers h for pattern (http.ServeMux syntax, for example
// "/v1/link/daemon" or "GET /v1/x"). It panics after Start, or on a pattern
// that conflicts with one already registered.
func (s *Server) Mount(pattern string, h http.Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mustNotHaveStarted("Mount")
	s.mux.Handle(pattern, h)
}

// AddReadyCheck adds a check to /readyz. fn gets a context bounded by the
// readiness timeout and returns nil when its component is ready. It panics
// after Start.
func (s *Server) AddReadyCheck(name string, fn func(context.Context) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mustNotHaveStarted("AddReadyCheck")
	s.checks = append(s.checks, readyCheck{name, fn})
}

// AddShutdownHook adds fn to Shutdown. Hooks start once the listener has
// closed, so no new connection arrives, and run concurrently with the HTTP
// server's wait for in-flight requests, with Shutdown's context. A hook
// closes what the HTTP server does not track, such as hijacked WebSocket
// links; a request accepted before the listener closed may still be
// upgrading, so a hook must also refuse links that register after it began.
// A hook must return when ctx ends: Shutdown waits a short grace after the
// deadline, then stops waiting, logs the names of unfinished hooks and
// reports them in the returned error. Errors returned by hooks are part of
// that error too. It panics after
// Start.
func (s *Server) AddShutdownHook(name string, fn func(context.Context) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mustNotHaveStarted("AddShutdownHook")
	s.hooks = append(s.hooks, shutdownHook{name, fn})
}

func (s *Server) mustNotHaveStarted(op string) {
	if s.started {
		panic("controlplane: " + op + " after Start")
	}
}

// Start listens on cfg.Listen and serves in the background. It returns once
// the listener is bound; Addr then reports the address.
func (s *Server) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return errors.New("controlplane: already started")
	}
	var tlsCfg *tls.Config
	if s.cfg.TLSCert != "" {
		cert, err := tls.LoadX509KeyPair(s.cfg.TLSCert, s.cfg.TLSKey)
		if err != nil {
			return fmt.Errorf("controlplane: load TLS key pair: %w", err)
		}
		tlsCfg = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	}
	ln, err := s.listen("tcp", s.cfg.Listen)
	if err != nil {
		return fmt.Errorf("controlplane: %w", err)
	}
	// "localhost" may resolve to a non-loopback address on a broken
	// resolver; check what was really bound.
	if s.cfg.DevInsecure && !loopback.Addr(ln.Addr()) {
		ln.Close()
		return fmt.Errorf("controlplane: --dev-insecure bound non-loopback address %v", ln.Addr())
	}
	if s.cfg.BehindProxy && !loopback.Addr(ln.Addr()) {
		s.logger.Warn("--behind-proxy serves plaintext on a non-loopback address; clients that bypass the proxy send tokens unencrypted. Listen on 127.0.0.1 unless the path to the proxy is trusted",
			"addr", ln.Addr().String())
	}
	mode := "tls"
	switch {
	case tlsCfg != nil:
		ln = tls.NewListener(ln, tlsCfg)
	case s.cfg.BehindProxy:
		mode = "behind-proxy"
	default:
		mode = "dev-insecure"
	}
	s.ln = ln
	s.httpSrv = &http.Server{
		Handler:           s.accessLog(s.mux),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          slog.NewLogLogger(s.logger.Handler(), slog.LevelWarn),
	}
	s.serveErr = make(chan error, 1)
	var bgCtx context.Context
	bgCtx, s.stopBg = context.WithCancel(context.Background())
	s.started = true
	s.timersDone = make(chan struct{})
	s.bg.Add(2)
	go func() {
		defer s.bg.Done()
		err := s.httpSrv.Serve(ln)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		s.serveErr <- err
	}()
	go func() {
		defer s.bg.Done()
		defer close(s.timersDone)
		_ = s.runner.Run(bgCtx)
	}()
	s.logger.Info("control plane listening", "addr", ln.Addr().String(), "mode", mode)
	return nil
}

// Addr returns the bound listener address, or nil before Start.
func (s *Server) Addr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return nil
	}
	return s.ln.Addr()
}

// Run starts the server, serves until ctx is done or serving fails, then
// shuts down within cfg.ShutdownTimeout. It returns the serving error or the
// shutdown error.
func (s *Server) Run(ctx context.Context) error {
	if err := s.Start(); err != nil {
		cleanup, cancel := context.WithTimeout(context.Background(), s.cfg.ShutdownTimeout)
		defer cancel()
		return errors.Join(err, s.Shutdown(cleanup))
	}
	var serveErr error
	select {
	case <-ctx.Done():
		s.logger.Info("shutdown requested")
	case serveErr = <-s.serveErr:
		s.logger.Error("serve failed", "error", serveErr)
	}
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.ShutdownTimeout)
	defer cancel()
	return errors.Join(serveErr, s.Shutdown(sctx))
}

// Shutdown stops accepting connections, starts the shutdown hooks once the
// listener has closed, waits for in-flight requests and hooks, and stops the
// background purge. When ctx ends first it closes every remaining connection,
// stops waiting for unfinished hooks and returns an error that wraps
// ErrShutdownTimeout or names the hooks. It is idempotent; later calls return
// the first result.
func (s *Server) Shutdown(ctx context.Context) error {
	s.shutdownOnce.Do(func() { s.shutdownErr = s.shutdown(ctx) })
	return s.shutdownErr
}

func (s *Server) shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.stopping = true
	started := s.started
	hooks := s.hooks
	s.started = true // forbid Start and registrations from now on
	s.mu.Unlock()
	if !started {
		if s.closeLinks != nil {
			return s.closeLinks(ctx)
		}
		return nil
	}
	begin := time.Now()

	// http.Server.Shutdown closes the listeners and only then calls the
	// OnShutdown functions, so the hooks start after the last accept.
	listenerClosed := make(chan struct{})
	s.httpSrv.RegisterOnShutdown(func() { close(listenerClosed) })
	httpDone := make(chan error, 1)
	go func() { httpDone <- s.httpSrv.Shutdown(ctx) }()
	<-listenerClosed
	// Shutdown closes only listeners Serve has registered; when Shutdown
	// races a just-started Serve goroutine, the listener is not registered
	// yet and stays open until Serve runs. Close it here too (a second
	// Close is harmless), so hooks never start while it accepts. This only
	// matters in that race, which no test can drive deterministically;
	// TestHooksStartAfterListenerCloses hit it once in a parallel run.
	_ = s.ln.Close()

	type hookResult struct {
		i   int
		err error
	}
	results := make(chan hookResult, len(hooks)) // buffered: an abandoned hook never blocks
	for i, h := range hooks {
		go func() { results <- hookResult{i, h.fn(ctx)} }()
	}
	var all []error
	finished := make([]bool, len(hooks))
	record := func(r hookResult) {
		finished[r.i] = true
		if r.err != nil {
			all = append(all, fmt.Errorf("shutdown hook %s: %w", hooks[r.i].name, r.err))
		}
	}
	pending := len(hooks)
	for ; pending > 0; pending-- {
		select {
		case r := <-results:
			record(r)
			continue
		case <-ctx.Done():
		}
		break
	}
	// A hook that waits for ctx and then returns finishes at the deadline,
	// a moment after ctx.Done fires. Collect such results, already sent or
	// arriving within hookGrace, before calling anything unfinished.
	if pending > 0 {
		grace := time.NewTimer(hookGrace)
	drain:
		for ; pending > 0; pending-- {
			select {
			case r := <-results:
				record(r)
			case <-grace.C:
				break drain
			}
		}
		grace.Stop()
	}
	for i, h := range hooks {
		if !finished[i] {
			s.logger.Warn("shutdown hook did not finish before the deadline", "hook", h.name)
			all = append(all, fmt.Errorf("shutdown hook %s did not finish: %w", h.name, ctx.Err()))
		}
	}
	if err := <-httpDone; err != nil {
		s.httpSrv.Close()
		all = append(all, ErrShutdownTimeout)
	}
	s.stopBg()
	s.bg.Wait()
	result := errors.Join(all...)
	if result != nil {
		s.logger.Warn("shutdown finished with errors", "error", result, "duration_ms", time.Since(begin).Milliseconds())
	} else {
		s.logger.Info("shutdown complete", "duration_ms", time.Since(begin).Milliseconds())
	}
	return result
}

// serveReady answers /readyz: the store must answer a read and every
// registered check must pass.
func (s *Server) serveReady(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	stopping, checks := s.stopping, s.checks
	s.mu.Unlock()
	if stopping {
		writeText(w, http.StatusServiceUnavailable, "shutting down\n")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), readyTimeout)
	defer cancel()
	all := append([]readyCheck{{"store", s.storeReady}}, checks...)
	var failed []string
	for _, c := range all {
		if err := c.fn(ctx); err != nil {
			s.logger.WarnContext(ctx, "readiness check failed", "check", c.name, "error", err)
			failed = append(failed, c.name)
		}
	}
	if len(failed) > 0 {
		writeText(w, http.StatusServiceUnavailable, "not ready: "+strings.Join(failed, ", ")+"\n")
		return
	}
	writeText(w, http.StatusOK, "ready\n")
}

// storeReady reads the default user's row: a real read through the store
// that fails once the store is closed or unreachable.
func (s *Server) storeReady(ctx context.Context) error {
	_, err := s.st.GetUser(ctx, s.user.ID)
	return err
}

func serveWhoami(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"role":         auth.RoleName(p.Kind),
		"principal_id": p.ID,
	})
}

func writeText(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}
