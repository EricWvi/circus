package webapi

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"connectrpc.com/connect"
	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/auth"
	"github.com/EricWvi/circus/internal/controlplane/sessions"
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
			Os: d.OS, Arch: d.Arch, Version: d.ClientVersion, DeviceId: d.ID,
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
	replace := req.Msg.GetReplaceId()
	if replace != "" && store.CheckID("replace id", replace) != nil {
		return nil, invalidField("replace_id", "invalid identity id")
	}
	if replace != "" && req.Msg.GetRole() == circusv1.EnrollRole_ENROLL_ROLE_BOTH {
		return nil, invalidField("role", "a rejoin link enrolls one role: daemon or worker")
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
			// A rejoin frees the slot it fills.
			if replace != "" {
				break
			}
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
			var token string
			var rec store.EnrollToken
			var err error
			if replace != "" {
				token, rec, err = auth.CreateRejoinToken(ctx, tx, u.ID, kind, replace, auth.DefaultEnrollTTL, now)
			} else {
				token, rec, err = auth.CreateEnrollToken(ctx, tx, u.ID, kind, auth.DefaultEnrollTTL, now)
			}
			if err != nil {
				return err
			}
			tokens = append(tokens, token)
			expires = rec.ExpiresAt
		}
		return nil
	})
	if errors.Is(err, auth.ErrNoSuchIdentity) {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("the identity to replace was not found"))
	}
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
	agents, err := s.st.ListUserWorkerAgents(ctx, u.ID)
	if err != nil {
		return nil, s.internal(ctx, "list worker agents", err)
	}
	byWorker := make(map[string][]store.WorkerAgent)
	for _, a := range agents {
		byWorker[a.WorkerID] = append(byWorker[a.WorkerID], a)
	}
	byMachine := make(map[string][]string)
	for _, d := range devices {
		if d.MachineID != "" {
			byMachine[d.MachineID] = append(byMachine[d.MachineID], d.Name)
		}
	}
	out := &circusv1.ListWorkersResponse{Workers: make([]*circusv1.Worker, 0, len(workers))}
	for _, w := range workers {
		out.Workers = append(out.Workers, s.workerMessage(w, online[w.ID], byMachine, byWorker[w.ID]))
	}
	return connect.NewResponse(out), nil
}

// workerMessage is the worker as ListWorkers shows it, with its agents
// (ordered by AgentKind).
func (s *Service) workerMessage(w store.Identity, online bool, byMachine map[string][]string, agents []store.WorkerAgent) *circusv1.Worker {
	lastSeen := max(w.LastConnectedAt, w.LastDisconnectedAt)
	if online {
		lastSeen = s.now().UnixMilli()
	}
	var local []string
	if w.MachineID != "" {
		local = byMachine[w.MachineID]
	}
	out := &circusv1.Worker{
		WorkerId: w.ID, Name: w.Name, Online: online, LastSeenUnixMs: lastSeen,
		Os: w.OS, Arch: w.Arch, Hostname: w.Hostname, Version: w.ClientVersion, LocalDevices: local,
		Integrator: w.Integrator,
	}
	for _, a := range agents {
		out.Agents = append(out.Agents, sessions.WorkerAgentProto(a))
	}
	return out
}

// oneWorker is the caller's worker w as ListWorkers shows it.
func (s *Service) oneWorker(ctx context.Context, userID string, w store.Identity) (*circusv1.Worker, error) {
	devices, err := s.st.ListIdentities(ctx, store.KindDevice, userID)
	if err != nil {
		return nil, s.internal(ctx, "list devices", err)
	}
	online, err := s.onlineWorkers(ctx, userID)
	if err != nil {
		return nil, s.internal(ctx, "online workers", err)
	}
	agents, err := s.st.ListWorkerAgents(ctx, w.ID)
	if err != nil {
		return nil, s.internal(ctx, "list worker agents", err)
	}
	byMachine := make(map[string][]string)
	for _, d := range devices {
		if d.MachineID != "" && d.MachineID == w.MachineID {
			byMachine[d.MachineID] = append(byMachine[d.MachineID], d.Name)
		}
	}
	return s.workerMessage(w, online[w.ID], byMachine, agents.Agents), nil
}

// DeleteDevice deletes one of the caller's devices with auth.RevokeByID,
// as `circus server device revoke` does.
func (s *Service) DeleteDevice(ctx context.Context, req *connect.Request[circusv1.DeleteDeviceRequest]) (*connect.Response[circusv1.DeleteDeviceResponse], error) {
	if err := s.deleteIdentity(ctx, store.KindDevice, "device_id", req.Msg.GetDeviceId()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&circusv1.DeleteDeviceResponse{}), nil
}

// DeleteWorker deletes one of the caller's workers with auth.RevokeByID,
// as `circus server worker revoke` does.
func (s *Service) DeleteWorker(ctx context.Context, req *connect.Request[circusv1.DeleteWorkerRequest]) (*connect.Response[circusv1.DeleteWorkerResponse], error) {
	if err := s.deleteIdentity(ctx, store.KindWorker, "worker_id", req.Msg.GetWorkerId()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&circusv1.DeleteWorkerResponse{}), nil
}

func (s *Service) deleteIdentity(ctx context.Context, kind store.PrincipalKind, field, id string) error {
	u, err := user(ctx)
	if err != nil {
		return err
	}
	if store.CheckID(field, id) != nil {
		return invalidField(field, "invalid id")
	}
	ident, n, err := auth.RevokeByID(ctx, s.st, kind, u.ID, id, s.now())
	if errors.Is(err, auth.ErrNoSuchIdentity) {
		return connect.NewError(connect.CodeNotFound, fmt.Errorf("%s not found", auth.AdminNoun(kind)))
	}
	if err != nil {
		return s.internal(ctx, "delete "+auth.AdminNoun(kind), err)
	}
	s.logger.InfoContext(ctx, "identity deleted", "kind", kind, "principal_id", ident.ID, "name", ident.Name,
		"user_id", u.ID, "revoked_credentials", n)
	return nil
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
	out, err := s.oneWorker(ctx, u.ID, w)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&circusv1.SetWorkerGrantResponse{Worker: out}), nil
}

// SetWorkerAgent records the caller's wish to install or uninstall an agent
// on one of their workers and returns the worker. Installing wants the
// worker's reported target version. Every install request is counted (the
// wish's attempt grows), which the worker ignores while the agent is
// installed at that version or being installed; otherwise it cancels an
// uninstall the worker has not started, and has the worker try again after
// a failed install (a worker retries a FAILED install only for a newer
// attempt). Uninstalling is refused
// while the worker holds a live lease of a session that runs the agent; the
// check and the wish are one transaction, and AcquireLease reads the wish
// in its own, so no session of the agent is leased to the worker after it.
// A changed wish reaches the worker at once when it is online (the
// scheduler pushes AgentsWanted), else at its next Attach.
func (s *Service) SetWorkerAgent(ctx context.Context, req *connect.Request[circusv1.SetWorkerAgentRequest]) (*connect.Response[circusv1.SetWorkerAgentResponse], error) {
	u, err := user(ctx)
	if err != nil {
		return nil, err
	}
	id := req.Msg.GetWorkerId()
	if store.CheckID("worker id", id) != nil {
		return nil, invalidField("worker_id", "invalid worker id")
	}
	kind, ok := sessions.AgentKindFromProto(req.Msg.GetAgent())
	if !ok {
		return nil, invalidField("agent", "unknown agent")
	}
	install := req.Msg.GetInstall()
	var w store.Identity
	var changed bool
	var version string
	err = s.st.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		var err error
		w, err = tx.GetIdentity(ctx, store.KindWorker, id)
		if errors.Is(err, store.ErrNotFound) || (err == nil && (w.UserID != u.ID || w.DeletedAt != 0)) {
			return connect.NewError(connect.CodeNotFound, errors.New("worker not found"))
		}
		if err != nil {
			return err
		}
		agents, err := tx.ListWorkerAgents(ctx, id)
		if err != nil {
			return err
		}
		a, known := agents.Get(kind)
		reported := known && a.Status.State != ""
		if install {
			if agents.ReportedAt == 0 || (reported && a.Status.TargetVersion == "") {
				return connect.NewError(connect.CodeFailedPrecondition, errors.New("the worker has not reported its agents yet"))
			}
			if !reported {
				return invalidField("agent", "the worker does not support this agent")
			}
			version = a.Status.TargetVersion
			changed, err = tx.SetWorkerAgentWish(ctx, id, kind, true, version)
			return err
		}
		if agents.ReportedAt != 0 && !reported && !a.Wanted {
			return invalidField("agent", "the worker does not support this agent")
		}
		held, err := tx.HasAgentLease(ctx, id, kind, s.now().UnixMilli())
		if err != nil {
			return err
		}
		if held {
			return connect.NewError(connect.CodeFailedPrecondition, errors.New("a session running this agent holds a lease on the worker; stop it or wait until it is released"))
		}
		changed, err = tx.SetWorkerAgentWish(ctx, id, kind, false, "")
		return err
	})
	if err != nil {
		var ce *connect.Error
		if errors.As(err, &ce) {
			return nil, ce
		}
		return nil, s.internal(ctx, "set worker agent", err)
	}
	if changed {
		s.logger.InfoContext(ctx, "worker agent wish set", "user_id", u.ID, "worker_id", id, "agent", string(kind),
			"install", install, "version", version, "changed", changed)
		if err := s.scheduler.RefreshWorkerAgents(ctx, id); err != nil {
			// Stored; the scheduler keeps reading it in the background
			// with backoff, and the worker gets it at its next Attach at
			// the latest.
			s.logger.WarnContext(ctx, "pushing worker agent wishes failed; retrying", "worker_id", id, "error", err)
		}
	}
	out, err := s.oneWorker(ctx, u.ID, w)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&circusv1.SetWorkerAgentResponse{Worker: out}), nil
}
