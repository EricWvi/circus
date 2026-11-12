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
// Failure codes: every token failure (unknown, used, expired, wrong role) is
// Unauthenticated with one fixed message (F20); a taken name AlreadyExists;
// a malformed request InvalidArgument; a busy store Unavailable; anything
// else Internal with a fixed message, details in the log only.
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
	}, e.s.now())
	switch {
	case err == nil:
	case errors.Is(err, auth.ErrEnrollRejected):
		// The store reports unknown, used and expired tokens as one error,
		// so the log cannot say which either; a role mismatch is folded in.
		log.InfoContext(ctx, "enrollment rejected", "reason", "token unusable for this role")
		return nil, connect.NewError(connect.CodeUnauthenticated, auth.ErrEnrollRejected)
	case errors.Is(err, auth.ErrNameTaken):
		log.InfoContext(ctx, "enrollment rejected", "reason", "name taken", "name", clip(m.GetName()))
		// Another live machine may own the name (same hostname), so the
		// hint asks for a machine id check before any revocation.
		return nil, connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("%s. Choose another with --name. "+
			"Only if that %s is this machine's own earlier identity (compare the MACHINE column of `circus server %s list` "+
			"with this machine's id) may an administrator free the name with `circus server %s revoke %s`",
			err, auth.AdminNoun(kind), auth.AdminNoun(kind), auth.AdminNoun(kind), m.GetName()))
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
	log.InfoContext(ctx, "enrolled", "kind", en.Identity.Kind, "principal_id", en.Identity.ID,
		"name", en.Identity.Name, "user_id", en.Identity.UserID, "credential_id", en.CredentialID)
	return connect.NewResponse(&circusv1.EnrollResponse{
		PrincipalId:     en.Identity.ID,
		LongTermToken:   en.Token,
		ControlPlaneUrl: strings.TrimRight(e.s.cfg.PublicURL, "/"),
	}), nil
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
