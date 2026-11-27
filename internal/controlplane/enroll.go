package controlplane

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"connectrpc.com/connect"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/auth"
	"github.com/EricWvi/circus/internal/store"
)

func connectReadLimit() connect.HandlerOption { return connect.WithReadMaxBytes(maxEnrollBytes) }

// KindForRole maps a protocol role to the identity kind it enrolls.
func KindForRole(r circusv1.Role) (store.PrincipalKind, bool) {
	switch r {
	case circusv1.Role_ROLE_DAEMON:
		return store.KindDevice, true
	case circusv1.Role_ROLE_WORKER:
		return store.KindWorker, true
	}
	return "", false
}

// enrollService implements EnrollService.Enroll on auth.Enroll.
//
// All token failures return Unauthenticated with the same message. A full
// identity quota returns ResourceExhausted and a taken name returns
// AlreadyExists, pointing to a rejoin link. A rejoin whose identity is
// online, or was deleted, returns FailedPrecondition; a rejoin that names
// another name, like any malformed request, InvalidArgument. A busy store
// returns Unavailable. Other errors return Internal with a fixed message,
// with details logged on the server.
type enrollService struct{ s *Server }

func (e *enrollService) Enroll(ctx context.Context, req *connect.Request[circusv1.EnrollRequest]) (*connect.Response[circusv1.EnrollResponse], error) {
	m := req.Msg
	log := e.s.logger.With("remote", req.Peer().Addr, "role", m.GetRole().String(), "machine_id", clip(m.GetMachineId()))
	kind, ok := KindForRole(m.GetRole())
	if !ok {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("role must be daemon or worker"))
	}
	en, err := auth.Enroll(ctx, e.s.st, auth.EnrollRequest{
		Token: m.GetOneTimeToken(), Kind: kind, Name: m.GetName(), Hostname: m.GetHostname(),
		MachineID: m.GetMachineId(), OS: m.GetOs(), Arch: m.GetArch(), ClientVersion: m.GetClientVersion(),
		Online: e.s.identityOnline,
	}, e.s.now())
	switch {
	case err == nil:
	case errors.Is(err, auth.ErrIdentityLimit):
		log.InfoContext(ctx, "enrollment rejected", "reason", "live identity limit reached")
		return nil, connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("live %s limit reached; revoke one first", auth.AdminNoun(kind)))
	case errors.Is(err, auth.ErrEnrollRejected):
		// The store reports unknown, used and expired tokens as one error,
		// so the log cannot say which either; a role mismatch is folded in.
		log.InfoContext(ctx, "enrollment rejected", "reason", "token unusable for this role")
		return nil, connect.NewError(connect.CodeUnauthenticated, auth.ErrEnrollRejected)
	case errors.Is(err, auth.ErrNameTaken):
		log.InfoContext(ctx, "enrollment rejected", "reason", "name taken", "name", clip(m.GetName()))
		return nil, connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("%s. If it is this machine's earlier %s, "+
			"rejoin as it with a link from \"Rejoin…\" on that %s in the Web UI (or from "+
			"`circus server enroll-token create --user <email> --role %s --replace <name>`); otherwise choose another name with --name",
			err, auth.RoleName(kind), auth.AdminNoun(kind), auth.RoleName(kind)))
	case errors.Is(err, auth.ErrReplaceOnline):
		log.InfoContext(ctx, "enrollment rejected", "reason", "rejoin target online")
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s. Stop it (or delete it), then join again with this link", err))
	case errors.Is(err, auth.ErrReplaceGone):
		log.InfoContext(ctx, "enrollment rejected", "reason", "rejoin target gone")
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	case errors.Is(err, auth.ErrInvalidRequest):
		log.InfoContext(ctx, "enrollment rejected", "reason", "invalid request", "error", err)
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	case errors.Is(err, store.ErrBusy):
		log.WarnContext(ctx, "enrollment failed", "error", err)
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("control plane busy; retry"))
	default:
		log.ErrorContext(ctx, "enrollment failed", "error", err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
	resp := &circusv1.EnrollResponse{
		PrincipalId:     en.Identity.ID,
		LongTermToken:   en.Token,
		ControlPlaneUrl: strings.TrimRight(e.s.cfg.PublicURL, "/"),
		Name:            en.Identity.Name,
	}
	if r := en.Replaced; r.ID != "" {
		log.InfoContext(ctx, "enrollment replaced identity", "kind", r.Kind, "replaced_principal_id", r.ID,
			"replaced_name", r.Name, "user_id", r.UserID, "revoked_credentials", en.ReplacedCredentials, "principal_id", en.Identity.ID)
		resp.Replaced = &circusv1.EnrollResponse_ReplacedIdentity{PrincipalId: r.ID, Name: r.Name}
	}
	log.InfoContext(ctx, "enrolled", "kind", en.Identity.Kind, "principal_id", en.Identity.ID,
		"name", en.Identity.Name, "user_id", en.Identity.UserID, "credential_id", en.CredentialID)
	return connect.NewResponse(resp), nil
}

// identityOnline reports whether a device or worker is connected now (for
// the identity a rejoin replaces), by
// what the Web API lists as online: a ready daemon link in the registry
// (onlineDevices) or a worker connection with a boot id
// (sessions.Service.OnlineWorkers).
func (s *Server) identityOnline(ctx context.Context, kind store.PrincipalKind, userID, id string) (bool, error) {
	online := s.onlineDevices
	if kind == store.KindWorker {
		online = s.sessions.OnlineWorkers
	}
	ids, err := online(ctx, userID)
	if err != nil {
		return false, err
	}
	return ids[id], nil
}

// maxLogField bounds client-controlled strings in log lines.
const maxLogField = 128

// clip shortens a client-controlled value for the log.
func clip(s string) string {
	if len(s) <= maxLogField {
		return s
	}
	return s[:maxLogField] + "…(truncated)"
}
