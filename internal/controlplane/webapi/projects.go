package webapi

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"
	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/store"
)

func protoProject(p store.Project) *circusv1.Project {
	out := &circusv1.Project{
		ProjectId: p.ID, Name: p.Name, LastUsedLocation: uint32(p.LastUsedLocation),
		CreatedAtUnixMs: p.CreatedAt, UpdatedAtUnixMs: p.UpdatedAt, RepoUrl: p.RepoURL,
	}
	for _, l := range p.Locations {
		loc := &circusv1.Project_Location{Device: l.DeviceName, Path: l.Path, DeviceRevoked: l.DeviceRevoked}
		if l.Path == "" {
			loc.EffectivePath = defaultDir(p.ID)
		}
		out.Locations = append(out.Locations, loc)
	}
	return out
}

// defaultDir is the directory a location without a path resolves to on its
// device. It uses the project id, so renaming a project does not move it.
func defaultDir(projectID string) string { return defaultPathPrefix + projectID }

// locationsField names a location's field in the request.
func locationsField(i int, field string) string { return fmt.Sprintf("locations[%d].%s", i, field) }

// resolveLocations checks the requested locations against u's devices and
// returns them with device ids and normalized paths (empty for a device's
// default directory). The first problem, in request order, is reported: a
// location's device before its path. No locations is fine.
func (s *Service) resolveLocations(ctx context.Context, u store.User, in []*circusv1.Project_Location) ([]store.ProjectLocation, string, error) {
	if len(in) > store.MaxProjectLocations {
		return nil, "", invalidField("locations", fmt.Sprintf("at most %d locations", store.MaxProjectLocations))
	}
	owner, err := s.deviceOwner(ctx, u)
	if err != nil {
		return nil, "", s.internal(ctx, "device owner", err)
	}
	devices := map[string]store.Identity{}
	if owner != "" {
		list, err := s.st.ListIdentities(ctx, store.KindDevice, owner)
		if err != nil {
			return nil, "", s.internal(ctx, "list devices", err)
		}
		for _, d := range list {
			devices[d.Name] = d
		}
	}
	out := make([]store.ProjectLocation, 0, len(in))
	seen := map[[2]string]bool{} // (device id, path); path "" is the default
	for i, l := range in {
		d, ok := devices[l.GetDevice()]
		if len(l.GetDevice()) > maxDeviceNameBytes || !ok {
			return nil, "", invalidField(locationsField(i, "device"), "unknown device")
		}
		path, problem := checkPath(l.GetPath(), d.OS)
		if problem != "" {
			return nil, "", invalidField(locationsField(i, "path"), problem)
		}
		key := [2]string{d.ID, path}
		if seen[key] && path == "" {
			return nil, "", invalidField(locationsField(i, "path"), "duplicate default location for this device")
		}
		if seen[key] {
			return nil, "", invalidField(locationsField(i, "path"), "location is listed twice")
		}
		seen[key] = true
		out = append(out, store.ProjectLocation{DeviceID: d.ID, DeviceName: d.Name, Path: path})
	}
	return out, owner, nil
}

// writeError maps the error of a project write.
func (s *Service) writeError(ctx context.Context, op string, err error) error {
	var de *store.LocationDeviceError
	switch {
	case errors.As(err, &de):
		// The device was revoked after the request was checked.
		return invalidField(locationsField(de.Index, "device"), "unknown device")
	case errors.Is(err, store.ErrExists):
		return fieldError(connect.CodeAlreadyExists, "name", "a project with this name already exists")
	case errors.Is(err, store.ErrLocationIndex):
		return invalidField("last_used_location", "last used location is out of range")
	case errors.Is(err, store.ErrProjectStale):
		return connect.NewError(connect.CodeAborted, errors.New("the project changed since it was read"))
	case errors.Is(err, store.ErrProjectBytes):
		return connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("projects may hold at most %d bytes of names, repository URLs and paths", store.MaxProjectBytesPerUser))
	case errors.Is(err, store.ErrLimit):
		return connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("at most %d projects", store.MaxProjectsPerUser))
	case errors.Is(err, store.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, errProjectNotFound)
	}
	return s.internal(ctx, op, err)
}

// projectID checks a request's project id. An empty one is NOT_FOUND like
// any other unknown id.
func projectID(id string) error {
	if len(id) > maxProjectIDBytes {
		return invalidField("project_id", "project id is too long")
	}
	if id == "" {
		return connect.NewError(connect.CodeNotFound, errProjectNotFound)
	}
	return nil
}

func (s *Service) CreateProject(ctx context.Context, req *connect.Request[circusv1.CreateProjectRequest]) (*connect.Response[circusv1.CreateProjectResponse], error) {
	u, err := user(ctx)
	if err != nil {
		return nil, err
	}
	name, problem := checkName(req.Msg.GetName())
	if problem != "" {
		return nil, invalidField("name", problem)
	}
	repoURL, problem := checkRepoURL(req.Msg.GetRepoUrl())
	if problem != "" {
		return nil, invalidField("repo_url", problem)
	}
	locs, owner, err := s.resolveLocations(ctx, u, req.Msg.GetLocations())
	if err != nil {
		return nil, err
	}
	id, err := newID()
	if err != nil {
		return nil, s.internal(ctx, "project id", err)
	}
	now := s.now().UnixMilli()
	p, err := s.st.CreateProject(ctx, store.Project{ID: id, UserID: u.ID, Name: name, Locations: locs, RepoURL: repoURL, CreatedAt: now, UpdatedAt: now}, owner)
	if err != nil {
		return nil, s.writeError(ctx, "create project", err)
	}
	return connect.NewResponse(&circusv1.CreateProjectResponse{Project: protoProject(p)}), nil
}

func (s *Service) ListProjects(ctx context.Context, _ *connect.Request[circusv1.ListProjectsRequest]) (*connect.Response[circusv1.ListProjectsResponse], error) {
	u, err := user(ctx)
	if err != nil {
		return nil, err
	}
	ps, err := s.st.ListProjects(ctx, u.ID)
	if err != nil {
		return nil, s.internal(ctx, "list projects", err)
	}
	out := &circusv1.ListProjectsResponse{Projects: make([]*circusv1.Project, 0, len(ps))}
	for _, p := range ps {
		out.Projects = append(out.Projects, protoProject(p))
	}
	return connect.NewResponse(out), nil
}

func (s *Service) GetProject(ctx context.Context, req *connect.Request[circusv1.GetProjectRequest]) (*connect.Response[circusv1.GetProjectResponse], error) {
	u, err := user(ctx)
	if err != nil {
		return nil, err
	}
	if err := projectID(req.Msg.GetProjectId()); err != nil {
		return nil, err
	}
	p, err := s.st.GetProject(ctx, u.ID, req.Msg.GetProjectId())
	if errors.Is(err, store.ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, errProjectNotFound)
	}
	if err != nil {
		return nil, s.internal(ctx, "get project", err)
	}
	return connect.NewResponse(&circusv1.GetProjectResponse{Project: protoProject(p)}), nil
}

func (s *Service) UpdateProject(ctx context.Context, req *connect.Request[circusv1.UpdateProjectRequest]) (*connect.Response[circusv1.UpdateProjectResponse], error) {
	u, err := user(ctx)
	if err != nil {
		return nil, err
	}
	m := req.Msg
	if err := projectID(m.GetProjectId()); err != nil {
		return nil, err
	}
	patch := store.ProjectPatch{UpdatedAt: s.now().UnixMilli()}
	if m.Name != nil {
		name, problem := checkName(m.GetName())
		if problem != "" {
			return nil, invalidField("name", problem)
		}
		patch.Name = &name
	}
	if m.RepoUrl != nil {
		repoURL, problem := checkRepoURL(m.GetRepoUrl())
		if problem != "" {
			return nil, invalidField("repo_url", problem)
		}
		patch.RepoURL = &repoURL
	}
	var owner string
	if m.Locations != nil {
		if patch.Locations, owner, err = s.resolveLocations(ctx, u, m.GetLocations().GetLocations()); err != nil {
			return nil, err
		}
	}
	if m.LastUsedLocation != nil {
		i := m.GetLastUsedLocation()
		if i >= store.MaxProjectLocations {
			return nil, invalidField("last_used_location", "last used location is out of range")
		}
		patch.LastUsedLocation = new(int(i))
	}
	if m.ExpectedUpdatedAtUnixMs != nil {
		patch.ExpectedUpdatedAt = new(m.GetExpectedUpdatedAtUnixMs())
	}
	p, err := s.st.UpdateProject(ctx, u.ID, m.GetProjectId(), patch, owner)
	if err != nil {
		return nil, s.writeError(ctx, "update project", err)
	}
	return connect.NewResponse(&circusv1.UpdateProjectResponse{Project: protoProject(p)}), nil
}

// DeleteProject preflights every session in one snapshot, deleting nothing if
// any fails DeleteSession's preconditions. It then deletes sessions individually
// through the chunked store method, followed by the empty project. These are
// separate transactions: a failure or a newly created session can leave a
// partially completed cascade. FAILED_PRECONDITION permits the client to retry.
func (s *Service) DeleteProject(ctx context.Context, req *connect.Request[circusv1.DeleteProjectRequest]) (*connect.Response[circusv1.DeleteProjectResponse], error) {
	u, err := user(ctx)
	if err != nil {
		return nil, err
	}
	if err := projectID(req.Msg.GetProjectId()); err != nil {
		return nil, err
	}
	projectID := req.Msg.GetProjectId()
	now := s.now().UnixMilli()
	var ids []string
	err = s.st.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		if _, err := tx.GetProject(ctx, u.ID, projectID); err != nil {
			return err
		}
		filter := store.SessionFilter{ProjectID: projectID, Now: now, Limit: store.MaxPageSize}
		for {
			sessions, err := tx.ListSessions(ctx, filter)
			if err != nil {
				return err
			}
			for _, sess := range sessions {
				if err := tx.CheckSessionDeletable(ctx, sess.ID, now); err != nil {
					return err
				}
				ids = append(ids, sess.ID)
			}
			if len(sessions) < filter.Limit {
				return nil
			}
			last := sessions[len(sessions)-1]
			filter.After = &store.SessionCursor{LastActivityAt: last.LastActivityAt, ID: last.ID}
		}
	})
	if err == nil {
		for _, id := range ids {
			err = s.st.DeleteSession(ctx, id, s.now().UnixMilli())
			// Another concurrent cascade may already have removed this session.
			if errors.Is(err, store.ErrNotFound) {
				err = nil
			}
			if err != nil {
				break
			}
		}
		if err == nil {
			err = s.st.DeleteProject(ctx, u.ID, projectID, s.now().UnixMilli())
		}
	}
	if err != nil {
		switch {
		case ctx.Err() != nil:
			return nil, s.internal(ctx, "delete project", err)
		case errors.Is(err, store.ErrSessionActive):
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("the project has sessions that are not stopped or still busy"))
		case errors.Is(err, store.ErrHasSessions):
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("a session was created while deleting; try again"))
		case errors.Is(err, store.ErrNotFound):
			return nil, connect.NewError(connect.CodeNotFound, errProjectNotFound)
		default:
			return nil, s.internal(ctx, "delete project", err)
		}
	}
	return connect.NewResponse(&circusv1.DeleteProjectResponse{}), nil
}
