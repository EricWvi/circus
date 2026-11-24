package webapi

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

// ListDevices lists the caller's enrolled devices by
// name. Online comes from the live daemon links, not from the stored
// connection times: a process that died with links open leaves rows that
// still look connected.
func (s *Service) ListDevices(ctx context.Context, _ *connect.Request[circusv1.ListDevicesRequest]) (*connect.Response[circusv1.ListDevicesResponse], error) {
	u, err := user(ctx)
	if err != nil {
		return nil, err
	}
	out := &circusv1.ListDevicesResponse{Devices: []*circusv1.Device{}}
	devices, err := s.st.ListIdentities(ctx, store.KindDevice, u.ID)
	if err != nil {
		return nil, s.internal(ctx, "list devices", err)
	}
	online, err := s.online(ctx, u.ID)
	if err != nil {
		return nil, s.internal(ctx, "online devices", err)
	}
	now := s.now().UnixMilli()
	for _, d := range devices {
		lastSeen := max(d.LastConnectedAt, d.LastDisconnectedAt)
		if online[d.ID] {
			lastSeen = now
		}
		out.Devices = append(out.Devices, &circusv1.Device{
			Name: d.Name, Online: online[d.ID], LastSeenUnixMs: lastSeen,
			Os: d.OS, Arch: d.Arch, Version: d.ClientVersion,
		})
	}
	return connect.NewResponse(out), nil
}

const maxUnusedEnrollTokens = 16

// CreateEnrollLink issues one independent, single-use token for each requested role.
func (s *Service) CreateEnrollLink(ctx context.Context, req *connect.Request[circusv1.CreateEnrollLinkRequest]) (*connect.Response[circusv1.CreateEnrollLinkResponse], error) {
	u, err := user(ctx)
	if err != nil {
		return nil, err
	}
	var kinds []store.PrincipalKind
	switch req.Msg.GetRole() {
	case circusv1.EnrollRole_ENROLL_ROLE_DAEMON:
		kinds = []store.PrincipalKind{store.KindDevice}
	case circusv1.EnrollRole_ENROLL_ROLE_WORKER:
		kinds = []store.PrincipalKind{store.KindWorker}
	case circusv1.EnrollRole_ENROLL_ROLE_BOTH:
		kinds = []store.PrincipalKind{store.KindDevice, store.KindWorker}
	default:
		return nil, invalidField("role", "role must be daemon, worker or both")
	}
	if s.publicURL == "" {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("public_url is not configured"))
	}

	tokens := make([]string, 0, len(kinds))
	var expires int64
	err = s.st.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		now := s.now()
		for _, kind := range kinds {
			if err := auth.CheckIdentityLimit(ctx, tx, u.ID, kind); err != nil {
				return err
			}
		}
		count, err := tx.CountUnusedEnrollTokens(ctx, u.ID, now.UnixMilli())
		if err != nil {
			return err
		}
		if count+len(kinds) > maxUnusedEnrollTokens {
			return store.ErrLimit
		}
		for _, kind := range kinds {
			token, rec, err := auth.CreateEnrollToken(ctx, tx, u.ID, kind, auth.DefaultEnrollTTL, now)
			if err != nil {
				return err
			}
			tokens = append(tokens, token)
			expires = rec.ExpiresAt
		}
		return nil
	})
	if errors.Is(err, auth.ErrIdentityLimit) {
		return nil, connect.NewError(connect.CodeResourceExhausted, errors.New("live identity limit reached for the requested roles"))
	}
	if errors.Is(err, store.ErrLimit) {
		return nil, connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("at most %d unused enrollment tokens", maxUnusedEnrollTokens))
	}
	if err != nil {
		return nil, s.internal(ctx, "create enrollment link", err)
	}
	return connect.NewResponse(&circusv1.CreateEnrollLinkResponse{
		Link: s.publicURL + "/join#" + strings.Join(tokens, "."), ExpiresUnixMs: expires, ServerVersion: s.version,
	}), nil
}

// ListWorkers lists the caller's workers that are not deleted, by name.
// Online comes from the live worker links, as ListDevices' does from the
// daemon links; local_devices are the caller's devices on the same
// machine.
func (s *Service) ListWorkers(ctx context.Context, _ *connect.Request[circusv1.ListWorkersRequest]) (*connect.Response[circusv1.ListWorkersResponse], error) {
	u, err := user(ctx)
	if err != nil {
		return nil, err
	}
	workers, err := s.st.ListIdentities(ctx, store.KindWorker, u.ID)
	if err != nil {
		return nil, s.internal(ctx, "list workers", err)
	}
	devices, err := s.st.ListIdentities(ctx, store.KindDevice, u.ID)
	if err != nil {
		return nil, s.internal(ctx, "list devices", err)
	}
	online, err := s.onlineWorkers(ctx, u.ID)
	if err != nil {
		return nil, s.internal(ctx, "online workers", err)
	}
	byMachine := make(map[string][]string)
	for _, d := range devices {
		if d.MachineID != "" {
			byMachine[d.MachineID] = append(byMachine[d.MachineID], d.Name)
		}
	}
	out := &circusv1.ListWorkersResponse{Workers: make([]*circusv1.Worker, 0, len(workers))}
	for _, w := range workers {
		out.Workers = append(out.Workers, s.workerMessage(w, online[w.ID], byMachine))
	}
	return connect.NewResponse(out), nil
}

func (s *Service) workerMessage(w store.Identity, online bool, byMachine map[string][]string) *circusv1.Worker {
	lastSeen := max(w.LastConnectedAt, w.LastDisconnectedAt)
	if online {
		lastSeen = s.now().UnixMilli()
	}
	var local []string
	if w.MachineID != "" {
		local = byMachine[w.MachineID]
	}
	return &circusv1.Worker{
		WorkerId: w.ID, Name: w.Name, Online: online, LastSeenUnixMs: lastSeen,
		Os: w.OS, Arch: w.Arch, Hostname: w.Hostname, Version: w.ClientVersion, LocalDevices: local,
		Integrator: w.Integrator,
	}
}

// SetWorkerGrant turns the integrator grant of one of the caller's
// workers on or off and returns the worker as ListWorkers shows it.
func (s *Service) SetWorkerGrant(ctx context.Context, req *connect.Request[circusv1.SetWorkerGrantRequest]) (*connect.Response[circusv1.SetWorkerGrantResponse], error) {
	u, err := user(ctx)
	if err != nil {
		return nil, err
	}
	id := req.Msg.GetWorkerId()
	if store.CheckID("worker id", id) != nil {
		return nil, invalidField("worker_id", "invalid worker id")
	}
	w, err := s.st.SetWorkerIntegrator(ctx, u.ID, id, req.Msg.GetIntegrator())
	if errors.Is(err, store.ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("worker not found"))
	}
	if err != nil {
		return nil, s.internal(ctx, "set worker grant", err)
	}
	s.logger.InfoContext(ctx, "worker grant changed", "user_id", u.ID, "worker_id", w.ID, "integrator", w.Integrator)
	devices, err := s.st.ListIdentities(ctx, store.KindDevice, u.ID)
	if err != nil {
		return nil, s.internal(ctx, "list devices", err)
	}
	online, err := s.onlineWorkers(ctx, u.ID)
	if err != nil {
		return nil, s.internal(ctx, "online workers", err)
	}
	byMachine := make(map[string][]string)
	for _, d := range devices {
		if d.MachineID != "" && d.MachineID == w.MachineID {
			byMachine[d.MachineID] = append(byMachine[d.MachineID], d.Name)
		}
	}
	return connect.NewResponse(&circusv1.SetWorkerGrantResponse{Worker: s.workerMessage(w, online[w.ID], byMachine)}), nil
}
