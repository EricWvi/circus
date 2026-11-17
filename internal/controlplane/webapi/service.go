package webapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"connectrpc.com/connect"
	"github.com/EricWvi/circus/gen/go/circus/v1/circusv1connect"
	"github.com/EricWvi/circus/internal/auth"
	"github.com/EricWvi/circus/internal/store"
	"github.com/google/uuid"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
)

// Request size limits. The largest valid ProjectService request is 16
// locations of a 4096-byte path; in JSON a Windows path doubles (each
// backslash is escaped), so 16 × 8 KiB plus names and framing.
const (
	maxProjectRequestBytes = 256 << 10
	maxMachineRequestBytes = 4 << 10
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
)

// OnlineFunc returns the ids of the devices of userID that have a daemon
// link connected now.
type OnlineFunc func(ctx context.Context, userID string) (map[string]bool, error)

// Config configures the services.
type Config struct {
	Store store.Store
	// Interceptor resolves the browser session and checks Origin:
	// auth.WebService.Interceptor.
	Interceptor connect.Interceptor
	// Online reports live daemon links (the gateway's registry).
	Online OnlineFunc
	// Now supplies time; nil means time.Now.
	Now func() time.Time
	// Logger receives store failures; nil discards them.
	Logger *slog.Logger
}

// Service implements ProjectService and MachineService.
type Service struct {
	st          store.Store
	interceptor connect.Interceptor
	online      OnlineFunc
	now         func() time.Time
	logger      *slog.Logger
}

var (
	_ circusv1connect.ProjectServiceHandler = (*Service)(nil)
	_ circusv1connect.MachineServiceHandler = (*Service)(nil)
)

// New returns the services. Store, Interceptor and Online are required.
func New(cfg Config) (*Service, error) {
	if cfg.Store == nil || cfg.Interceptor == nil || cfg.Online == nil {
		return nil, errors.New("webapi: store, interceptor and online are required")
	}
	s := &Service{st: cfg.Store, interceptor: cfg.Interceptor, online: cfg.Online, now: cfg.Now, logger: cfg.Logger}
	if s.now == nil {
		s.now = time.Now
	}
	if s.logger == nil {
		s.logger = slog.New(slog.DiscardHandler)
	}
	return s, nil
}

// Register mounts both services on mux.
func (s *Service) Register(mux *http.ServeMux) {
	path, h := circusv1connect.NewProjectServiceHandler(s, connect.WithReadMaxBytes(maxProjectRequestBytes),
		connect.WithSendMaxBytes(maxProjectResponseBytes), connect.WithInterceptors(s.interceptor))
	mux.Handle(path, h)
	path, h = circusv1connect.NewMachineServiceHandler(s, connect.WithReadMaxBytes(maxMachineRequestBytes),
		connect.WithSendMaxBytes(maxMachineResponseBytes), connect.WithInterceptors(s.interceptor))
	mux.Handle(path, h)
}

// user returns the signed-in Web user, or UNAUTHENTICATED.
func user(ctx context.Context) (store.User, error) {
	u, ok := auth.WebUser(ctx)
	if !ok {
		return store.User{}, connect.NewError(connect.CodeUnauthenticated, errors.New("sign in first"))
	}
	return u, nil
}

// deviceOwner returns the id of the user whose devices u may see and bind
// to project locations, or "" when u may use none.
//
// Machines are still enrolled for the single legacy machine owner (the user
// named auth.DefaultUserName, which has no email and cannot sign in), not
// for Web users. Until enrollment binds machines to Web users, the super
// user stands for that owner and sees its devices; every other Web user
// sees none, and so cannot bind a location. Such a user may still create
// projects with only a name or a repository URL; that is intended, since
// those run nothing anywhere. Replace this function when machines get Web
// owners.
func (s *Service) deviceOwner(ctx context.Context, u store.User) (string, error) {
	if u.Role != store.RoleSuper {
		return "", nil
	}
	owner, err := s.st.GetUserByName(ctx, auth.DefaultUserName)
	if errors.Is(err, store.ErrNotFound) || (err == nil && owner.Email != "") {
		return "", nil
	}
	return owner.ID, err
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
