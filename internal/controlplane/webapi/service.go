package webapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/gen/go/circus/v1/circusv1connect"
	"github.com/EricWvi/circus/internal/auth"
	"github.com/EricWvi/circus/internal/controlplane/gateway"
	"github.com/EricWvi/circus/internal/controlplane/sessions"
	"github.com/EricWvi/circus/internal/store"
	"github.com/google/uuid"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
)

// Request size limits. The largest valid ProjectService request is 16
// locations of a 4096-byte path; in JSON a Windows path doubles (each
// backslash is escaped), so 16 × 8 KiB plus names and framing.
const (
	maxProjectRequestBytes = 256 << 10
	// A directory path can occupy up to six JSON bytes per input byte.
	maxMachineRequestBytes = 32 << 10
	// maxSessionRequestBytes: the largest SessionService request is a
	// SendMessage of 256 KiB of text, up to six times that in JSON when
	// every byte is escaped as \u00XX.
	maxSessionRequestBytes = 2 << 20
)

// Response size limits. The largest ProjectService response is a full
// ListProjects: the strings a user stores are bounded together by
// store.MaxProjectBytesPerUser (4 MiB, up to 8 MiB in JSON when every byte
// is an escaped backslash), and the rest is per location and per project:
// at most 1000 projects × 16 locations, each with a device name of at most
// 128 bytes and, for a default location, an effective path of about 60
// bytes, plus ids, times and framing. TestListProjectsAtBound builds that
// worst case: about 7 MiB in binary and 11.4 MiB in JSON, so 16 MiB leaves
// room. ListDevices answers about 300 bytes per device.
const (
	maxProjectResponseBytes = 16 << 20
	maxMachineResponseBytes = 4 << 20
	// maxSessionResponseBytes bounds one SessionService message. The
	// largest are a full ListSessions (1000 sessions with titles of at most
	// 1024 bytes, up to six times that in JSON, about 7 MiB), a GetSession
	// with 256 working directories of 4 KiB (up to six times that in JSON,
	// about 6 MiB) and a WatchSession event (a stored payload is at most
	// 256 KiB, an ephemeral one at most the bus's queue budget, 256 KiB; up
	// to six times that in JSON).
	maxSessionResponseBytes = 16 << 20
)

// OnlineFunc returns the ids of the devices of userID that have a daemon
// link connected now.
type OnlineFunc func(ctx context.Context, userID string) (map[string]bool, error)

// QueryFunc sends an ephemeral query to a device's current daemon link.
type QueryFunc func(ctx context.Context, deviceID string, q *circusv1.DeviceQuery) (*circusv1.DeviceQueryResult, error)

// Config configures the services.
type Config struct {
	Store store.Store
	// PublicURL is the base URL for enrollment links.
	PublicURL string
	// Version is the control plane build version; empty means dev.
	Version string
	// Interceptor resolves the browser session and checks Origin:
	// auth.WebService.Interceptor.
	Interceptor connect.Interceptor
	// Online reports live daemon links (the gateway's registry).
	Online OnlineFunc
	// Query sends live device queries. Nil reports that devices are offline.
	Query QueryFunc
	// Now supplies time; nil means time.Now.
	Now func() time.Time
	// Logger receives store failures; nil discards them.
	Logger *slog.Logger
	// Bus fans committed session events out to WatchSession streams; the
	// server shares it with the worker link.
	Bus *sessions.Bus
	// Waker hints the scheduler after commits a worker must act on.
	Waker sessions.Waker
	// Jobs stops the background device jobs of a stopped session.
	Jobs JobStopper
	// WatchKeepalive is how long a WatchSession stream may stay silent
	// before a keepalive; zero means DefaultWatchKeepalive.
	WatchKeepalive time.Duration
	// WatchRecheck is how often a stream checks its browser session; zero
	// means DefaultWatchRecheck.
	WatchRecheck time.Duration
	// WatchMaxLifetime ends a stream after about this long (±10 %); zero
	// means DefaultWatchMaxLifetime.
	WatchMaxLifetime time.Duration
	// WatchWriteTimeout ends a SessionService response whose writes made no
	// progress for this long; zero means DefaultWatchWriteTimeout.
	WatchWriteTimeout time.Duration
}

// Service implements ProjectService, MachineService and SessionService.
type Service struct {
	st               store.Store
	publicURL        string
	version          string
	interceptor      connect.Interceptor
	online           OnlineFunc
	now              func() time.Time
	logger           *slog.Logger
	bus              *sessions.Bus
	waker            sessions.Waker
	jobs             JobStopper
	keepalive        time.Duration
	recheck          time.Duration
	maxLifetime      time.Duration
	writeTimeout     time.Duration
	query            QueryFunc
	directoryMu      sync.Mutex
	directoryQueries map[string]int
}

var (
	_ circusv1connect.ProjectServiceHandler = (*Service)(nil)
	_ circusv1connect.MachineServiceHandler = (*Service)(nil)
	_ circusv1connect.SessionServiceHandler = (*Service)(nil)
)

// New returns the services. Store, Interceptor, Online, Bus, Waker and Jobs
// are required.
func New(cfg Config) (*Service, error) {
	if cfg.Store == nil || cfg.Interceptor == nil || cfg.Online == nil || cfg.Bus == nil || cfg.Waker == nil || cfg.Jobs == nil {
		return nil, errors.New("webapi: store, interceptor, online, bus, waker and jobs are required")
	}
	s := &Service{st: cfg.Store, interceptor: cfg.Interceptor, online: cfg.Online, now: cfg.Now, logger: cfg.Logger,
		publicURL: strings.TrimRight(cfg.PublicURL, "/"), version: cfg.Version,
		bus: cfg.Bus, waker: cfg.Waker, jobs: cfg.Jobs, keepalive: cfg.WatchKeepalive, recheck: cfg.WatchRecheck,
		maxLifetime: cfg.WatchMaxLifetime, writeTimeout: cfg.WatchWriteTimeout,
		query: cfg.Query, directoryQueries: make(map[string]int)}
	if s.query == nil {
		s.query = func(context.Context, string, *circusv1.DeviceQuery) (*circusv1.DeviceQueryResult, error) {
			return nil, gateway.ErrDeviceOffline
		}
	}
	if s.version == "" {
		s.version = "dev"
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.keepalive <= 0 {
		s.keepalive = DefaultWatchKeepalive
	}
	if s.recheck <= 0 {
		s.recheck = DefaultWatchRecheck
	}
	if s.maxLifetime <= 0 {
		s.maxLifetime = DefaultWatchMaxLifetime
	}
	if s.writeTimeout <= 0 {
		s.writeTimeout = DefaultWatchWriteTimeout
	}
	if s.logger == nil {
		s.logger = slog.New(slog.DiscardHandler)
	}
	return s, nil
}

// Register mounts the services on mux.
func (s *Service) Register(mux *http.ServeMux) {
	path, h := circusv1connect.NewProjectServiceHandler(s, connect.WithReadMaxBytes(maxProjectRequestBytes),
		connect.WithSendMaxBytes(maxProjectResponseBytes), connect.WithInterceptors(s.interceptor))
	mux.Handle(path, h)
	path, h = circusv1connect.NewMachineServiceHandler(s, connect.WithReadMaxBytes(maxMachineRequestBytes),
		connect.WithSendMaxBytes(maxMachineResponseBytes), connect.WithInterceptors(s.interceptor))
	mux.Handle(path, h)
	// Without response compression a stream's encoding buffer is bounded by
	// the message it sends (see the memory bound in sessions/bus.go); a
	// reverse proxy may still compress.
	path, h = circusv1connect.NewSessionServiceHandler(s, connect.WithReadMaxBytes(maxSessionRequestBytes),
		connect.WithSendMaxBytes(maxSessionResponseBytes), connect.WithInterceptors(s.interceptor),
		connect.WithCompression("gzip", nil, nil))
	mux.Handle(path, withStreamWriter(h, s.writeTimeout))
}

// user returns the signed-in Web user, or UNAUTHENTICATED.
func user(ctx context.Context) (store.User, error) {
	u, ok := auth.WebUser(ctx)
	if !ok {
		return store.User{}, connect.NewError(connect.CodeUnauthenticated, errors.New("sign in first"))
	}
	return u, nil
}

// fieldError is a refusal of one request field, with the field path in a
// google.rpc.BadRequest detail.
func fieldError(code connect.Code, field, message string) *connect.Error {
	err := connect.NewError(code, errors.New(message))
	detail, detailErr := connect.NewErrorDetail(&errdetails.BadRequest{FieldViolations: []*errdetails.BadRequest_FieldViolation{{Field: field, Description: message}}})
	if detailErr == nil {
		err.AddDetail(detail)
	}
	return err
}

func invalidField(field, message string) *connect.Error {
	return fieldError(connect.CodeInvalidArgument, field, message)
}

var errProjectNotFound = errors.New("project not found")

// internal maps a store failure the caller has no specific answer for.
func (s *Service) internal(ctx context.Context, op string, err error) error {
	// A canceled database operation can surface a driver error instead of
	// the context error. Use the request's status when it has ended.
	cause := err
	if ctxErr := ctx.Err(); ctxErr != nil {
		cause = ctxErr
	}
	switch {
	case errors.Is(cause, context.Canceled):
		return connect.NewError(connect.CodeCanceled, context.Canceled)
	case errors.Is(cause, context.DeadlineExceeded):
		return connect.NewError(connect.CodeDeadlineExceeded, context.DeadlineExceeded)
	}
	if errors.Is(err, store.ErrBusy) {
		s.logger.WarnContext(ctx, "web api store busy", "op", op, "error", err)
		return connect.NewError(connect.CodeUnavailable, errors.New("busy; retry"))
	}
	s.logger.ErrorContext(ctx, "web api failed", "op", op, "error", err)
	return connect.NewError(connect.CodeInternal, errors.New("internal error"))
}

func newID() (string, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return "", err
	}
	return id.String(), nil
}
