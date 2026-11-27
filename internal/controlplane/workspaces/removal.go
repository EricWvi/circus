package workspaces

import (
	"context"
	"sync"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/clock"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/wire"
)

const removeTimeout = 30 * time.Second

// PauseProject prevents new preparations while deletion cancels the existing
// ones. Releasing the pause lets the normal scan resume work if deletion failed.
func (m *Manager) PauseProject(projectID string) func() {
	m.mu.Lock()
	m.paused[projectID]++
	if a := m.admitting; a != nil && a.projectID == projectID {
		a.cancel()
	}
	for _, r := range m.runners {
		if r.workspace.ProjectID == projectID {
			r.cancel()
		}
	}
	m.mu.Unlock()
	return func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.paused[projectID]--; m.paused[projectID] == 0 {
			delete(m.paused, projectID)
		}
	}
}

type removalResult struct {
	key   string // store.IdentityNameKey of the device name
	final bool
	out   *circusv1.WorkspaceRemoval
}

// BeforeDelete runs best-effort cancellation and one removal per requested
// device in parallel. The same deadline covers lookup, submission and replies.
// It cancels what is still running at the deadline and joins it before
// returning, so no store or call-plane work outlives the request.
func (m *Manager) BeforeDelete(ctx context.Context, userID, projectID string, names []string) []*circusv1.WorkspaceRemoval {
	ctx, cancel := clock.WithTimeout(ctx, m.cfg.Clock, removeTimeout)
	var wg sync.WaitGroup
	defer func() { cancel(); wg.Wait() }()
	results := make(chan removalResult, 2*len(names)) // buffered: sends never block
	cancelled := make(chan struct{})
	wg.Go(func() {
		defer close(cancelled)
		m.cancelAttempts(ctx, userID, projectID)
	})
	// By store.IdentityNameKey: names differing only in case are one device.
	byName := make(map[string]*circusv1.WorkspaceRemoval, len(names))
	for _, name := range names {
		key := store.IdentityNameKey(name)
		if byName[key] != nil {
			continue
		}
		byName[key] = kept(name, "")
		wg.Go(func() {
			out := m.remove(ctx, userID, projectID, name, func(path string) {
				results <- removalResult{key: key, out: kept(name, path)}
			})
			results <- removalResult{key: key, final: true, out: out}
		})
	}
	for pending := len(byName); pending > 0; {
		select {
		case r := <-results:
			byName[r.key] = r.out
			if r.final {
				pending--
			}
		case <-ctx.Done():
			pending = 0
		}
	}
	select {
	case <-cancelled:
	case <-ctx.Done():
	}
	// Keep replies already available at the deadline.
	for range 2 * len(byName) {
		select {
		case r := <-results:
			byName[r.key] = r.out
		default:
		}
	}
	out := make([]*circusv1.WorkspaceRemoval, 0, len(names))
	for _, name := range names {
		out = append(out, byName[store.IdentityNameKey(name)])
	}
	return out
}

func (m *Manager) cancelAttempts(ctx context.Context, userID, projectID string) {
	if m.cfg.Cancel == nil || m.cfg.StopJobs == nil {
		return
	}
	var after string
	for ctx.Err() == nil {
		rows, err := m.cfg.Store.ListProjectWorkspaces(ctx, projectID, after, store.MaxPageSize)
		if err != nil {
			m.logError(store.Workspace{}, err)
			return
		}
		for _, w := range rows {
			if ctx.Err() != nil {
				return
			}
			if w.State == store.WorkspacePreparing && w.UserID == userID {
				m.logError(w, m.cfg.Cancel(ctx, userID, w.CallID))
				m.logError(w, m.cfg.StopJobs(ctx, userID, w.SessionID()))
			}
		}
		if len(rows) < store.MaxPageSize {
			return
		}
		after = rows[len(rows)-1].DeviceID
	}
}

func kept(name, path string) *circusv1.WorkspaceRemoval {
	return &circusv1.WorkspaceRemoval{Device: name, Outcome: circusv1.WorkspaceRemoval_OUTCOME_KEPT, Path: path}
}

func (m *Manager) remove(ctx context.Context, userID, projectID, name string, announce func(string)) *circusv1.WorkspaceRemoval {
	out := kept(name, "")
	d, err := m.cfg.Store.GetIdentityByName(ctx, store.KindDevice, userID, name)
	if err != nil {
		return out
	}
	w, err := m.cfg.Store.GetWorkspace(ctx, projectID, d.ID)
	if err != nil || w.UserID != userID || w.State != store.WorkspaceReady {
		return out
	}
	out.Path = w.Path
	announce(w.Path)
	online, supported := m.cfg.Supports(d.ID, wire.FeatureRemoveWorkspace)
	if !online || !supported {
		return out
	}
	id, err := newCallID()
	if err != nil {
		return out
	}
	call := &circusv1.DeviceCall{
		CallId: id, SessionId: w.SessionID(), TimeoutMs: uint32(removeTimeout.Milliseconds()),
		Tool: &circusv1.DeviceCall_RemoveWorkspace{RemoveWorkspace: &circusv1.RemoveWorkspaceRequest{
			Path: store.DefaultWorkspacePath(projectID),
		}},
	}
	sub, err := m.cfg.Submit(ctx, userID, name, call)
	if err != nil {
		return out
	}
	defer sub.Close()
	for {
		update, err := sub.Next(ctx)
		if err != nil {
			return out
		}
		switch u := update.GetUpdate().(type) {
		case *circusv1.CallUpdate_Result:
			return removalOutcome(out, w, u.Result)
		case *circusv1.CallUpdate_Offline, *circusv1.CallUpdate_Lost_, *circusv1.CallUpdate_Cancelled_, *circusv1.CallUpdate_Rejected_:
			return out
		}
	}
}

func removalOutcome(out *circusv1.WorkspaceRemoval, w store.Workspace, result *circusv1.DeviceResult) *circusv1.WorkspaceRemoval {
	if fault := result.GetFault(); fault != nil {
		if fault.GetCode() != circusv1.DaemonFault_CODE_TOOL_UNAVAILABLE {
			out.Outcome, out.Error = circusv1.WorkspaceRemoval_OUTCOME_FAILED, circusv1.WorkspaceError_WORKSPACE_ERROR_DAEMON_FAULT
		}
		return out
	}
	got := result.GetRemoveWorkspace()
	if store.CheckWorkspacePath(got.GetPath()) == nil {
		out.Path = got.GetPath()
	}
	switch got.GetOutcome() {
	case circusv1.RemoveWorkspaceResult_OUTCOME_REMOVED, circusv1.RemoveWorkspaceResult_OUTCOME_ABSENT:
		out.Outcome = circusv1.WorkspaceRemoval_OUTCOME_REMOVED
	case circusv1.RemoveWorkspaceResult_OUTCOME_FAILED:
		out.Outcome, out.Error = circusv1.WorkspaceRemoval_OUTCOME_FAILED, got.GetError()
		out.ErrorDetail = store.WorkspaceErrorDetail(got.GetErrorDetail(), w.RepoURL)
	default:
		out.Outcome, out.Error = circusv1.WorkspaceRemoval_OUTCOME_FAILED, circusv1.WorkspaceError_WORKSPACE_ERROR_DAEMON_FAULT
	}
	return out
}
