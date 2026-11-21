package workspaces

import (
	"context"
	"errors"
	"strings"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/controlplane/calls"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/wire"
)

func (m *Manager) runAttempt(ctx context.Context, initial store.Workspace, wake chan struct{}) {
	w := initial
	for ctx.Err() == nil {
		if w.State != store.WorkspacePreparing {
			return
		}
		cont, err := m.step(ctx, w, wake)
		m.logError(w, err)
		if !cont {
			return
		}
		var retry <-chan time.Time
		if err != nil {
			retry = m.cfg.Clock.After(time.Second)
		}
		select {
		case <-ctx.Done():
			return
		case <-wake:
		case <-retry:
		}
		for ctx.Err() == nil {
			latest, err := m.cfg.Store.GetWorkspaceByID(ctx, w.ID)
			if errors.Is(err, store.ErrNotFound) {
				return
			}
			if err == nil {
				w = latest
				break
			}
			m.logError(w, err)
			select {
			case <-ctx.Done():
				return
			case <-m.cfg.Clock.After(time.Second):
			}
		}
	}
}

// step returns whether the durable attempt still needs a runner. A nil error
// with more work means it is waiting for a device, job or scan hint.
func (m *Manager) step(ctx context.Context, w store.Workspace, wake chan struct{}) (bool, error) {
	device, err := m.cfg.Store.GetIdentity(ctx, store.KindDevice, w.DeviceID)
	switch {
	case errors.Is(err, store.ErrNotFound) || (err == nil && device.DeletedAt != 0):
		return m.apply(ctx, w, failed(w, circusv1.WorkspaceError_WORKSPACE_ERROR_DEVICE_REVOKED, "", m.now()))
	case err != nil:
		return true, err
	}
	if w.JobID != "" {
		return m.checkJob(ctx, w, device, wake)
	}
	online, supported := m.cfg.Supports(w.DeviceID, wire.FeaturePrepareWorkspace)
	if !online || !supported {
		// A recorded call can still be attached to observe an offline or
		// lost outcome. A new dispatch has to respect the current Hello.
		c, err := m.cfg.Store.GetCall(ctx, w.CallID)
		switch {
		case errors.Is(err, store.ErrNotFound) && !online:
			return true, nil
		case errors.Is(err, store.ErrNotFound), err == nil && online && c.FaultCode == circusv1.DaemonFault_CODE_TOOL_UNAVAILABLE.String():
			return m.apply(ctx, w, pending(w, store.WorkspaceOutdated, 0, m.now()))
		case err != nil:
			return true, err
		case !online && c.State == store.CallLost && c.DispatchedBootID == "":
			// The router can replay this same never-dispatched request
			// when the device returns.
			return true, nil
		}
	}
	call := &circusv1.DeviceCall{
		CallId: w.CallID, SessionId: w.SessionID(), TimeoutMs: uint32(w.TimeoutMs),
		Tool: &circusv1.DeviceCall_PrepareWorkspace{PrepareWorkspace: &circusv1.PrepareWorkspaceRequest{
			Path: store.DefaultWorkspacePath(w.ProjectID), RepoUrl: w.RepoURL, ResumeJobId: w.ResumeJobID,
		}},
	}
	sub, err := m.cfg.Submit(ctx, w.UserID, device.Name, call)
	if err != nil {
		return m.subscriptionError(ctx, w, err)
	}
	defer sub.Close()
	for {
		update, err := sub.Next(ctx)
		if err != nil {
			return m.subscriptionError(ctx, w, err)
		}
		switch u := update.GetUpdate().(type) {
		case *circusv1.CallUpdate_Result:
			return m.result(ctx, w, device, u.Result, wake)
		case *circusv1.CallUpdate_Offline:
			return m.apply(ctx, w, pending(w, store.WorkspaceOffline, 0, m.now()))
		case *circusv1.CallUpdate_Lost_:
			return m.apply(ctx, w, m.lost(w))
		case *circusv1.CallUpdate_Cancelled_:
			return m.apply(ctx, w, failed(w, circusv1.WorkspaceError_WORKSPACE_ERROR_CANCELLED, "", m.now()))
		case *circusv1.CallUpdate_Rejected_:
			return m.rejected(ctx, w, u.Rejected.GetCode())
		}
		// Accepted, dispatched and unknown are all non-terminal.
	}
}

func (m *Manager) subscriptionError(ctx context.Context, w store.Workspace, err error) (bool, error) {
	switch {
	case ctx.Err() != nil:
		return false, ctx.Err()
	case errors.Is(err, calls.ErrDeviceOffline):
		return true, nil
	case errors.Is(err, calls.ErrResultUnavailable):
		return m.apply(ctx, w, m.lost(w))
	default:
		// Overflow, a closed reader and infrastructure errors may leave
		// a durable call. Reattach the same request after a short pause.
		return true, err
	}
}

func (m *Manager) result(ctx context.Context, w store.Workspace, device store.Identity, result *circusv1.DeviceResult, wake chan struct{}) (bool, error) {
	now := m.now()
	if fault := result.GetFault(); fault != nil {
		if fault.GetCode() == circusv1.DaemonFault_CODE_TOOL_UNAVAILABLE {
			return m.apply(ctx, w, pending(w, store.WorkspaceOutdated, max(device.LastConnectedAt, 1), now))
		}
		return m.apply(ctx, w, failed(w, circusv1.WorkspaceError_WORKSPACE_ERROR_DAEMON_FAULT, "", now))
	}
	got := result.GetPrepareWorkspace()
	switch got.GetOutcome() {
	case circusv1.PrepareWorkspaceResult_OUTCOME_READY:
		action := store.WorkspaceActionName(got.GetAction())
		if action == "" || circusv1.PrepareWorkspaceResult_Action_name[int32(got.GetAction())] == "" || store.CheckWorkspacePath(got.GetPath()) != nil ||
			!strings.HasSuffix(got.GetPath(), strings.TrimPrefix(store.DefaultWorkspacePath(w.ProjectID), "~")) {
			break
		}
		change, err := m.cfg.Store.MarkWorkspaceReady(ctx, w.ID, w.Expect(), got.GetPath(), action, now)
		return m.changed(ctx, w, change, err)
	case circusv1.PrepareWorkspaceResult_OUTCOME_IN_BACKGROUND:
		if store.CheckID("workspace job id", got.GetJobId()) != nil {
			break
		}
		c, err := m.cfg.Store.GetCall(ctx, w.CallID)
		if err != nil {
			return true, err
		}
		if c.DispatchedBootID == "" {
			break
		}
		p := w.WorkspacePatch
		p.JobID, p.JobBootID, p.UpdatedAt = got.GetJobId(), c.DispatchedBootID, now
		p.BackoffStreak = 0 // The daemon started work and moved it to a job.
		cont, err := m.apply(ctx, w, p)
		if cont && err == nil {
			poke(wake)
		}
		return cont, err
	case circusv1.PrepareWorkspaceResult_OUTCOME_FAILED:
		code := got.GetError()
		switch {
		case code == circusv1.WorkspaceError_WORKSPACE_ERROR_BUSY:
			return m.apply(ctx, w, m.backoff(w))
		case code > 0 && code < 100 && circusv1.WorkspaceError_name[int32(code)] != "":
			return m.apply(ctx, w, failed(w, code, got.GetErrorDetail(), now))
		}
	}
	m.cfg.Logger.Warn("invalid workspace result", "workspace_id", w.ID)
	return m.apply(ctx, w, failed(w, circusv1.WorkspaceError_WORKSPACE_ERROR_DAEMON_FAULT, "", now))
}

func (m *Manager) checkJob(ctx context.Context, w store.Workspace, device store.Identity, wake chan struct{}) (bool, error) {
	job, err := m.cfg.Store.GetWorkspaceJob(ctx, w.DeviceID, w.JobBootID, w.JobID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return true, err
	}
	if err == nil && (job.UserID != w.UserID || job.SessionID != w.SessionID()) {
		return m.apply(ctx, w, failed(w, circusv1.WorkspaceError_WORKSPACE_ERROR_DAEMON_FAULT, "", m.now()))
	}
	if job.State == store.JobLost || (err != nil && device.CurrentBootID != "" && device.CurrentBootID != w.JobBootID) {
		return m.apply(ctx, w, m.lost(w))
	}
	// Each recorded job gets its own lifetime and retention window,
	// including jobs announced by a follow-up call.
	const jobRecordWindow = 2*time.Hour + 32*time.Minute
	expired := errors.Is(err, store.ErrNotFound) && m.now()-w.JobRecordedAt >= jobRecordWindow.Milliseconds()
	if !job.State.Terminal() && !expired {
		return true, nil
	}
	if expired && w.ExpiredJobResumed {
		return m.apply(ctx, w, failed(w, circusv1.WorkspaceError_WORKSPACE_ERROR_TIMEOUT, "", m.now()))
	}
	id, err := newCallID()
	if err != nil {
		return true, err
	}
	p := w.WorkspacePatch
	p.CallID, p.ResumeJobID, p.JobID, p.JobBootID, p.UpdatedAt = id, w.JobID, "", "", m.now()
	p.ExpiredJobResumed = w.ExpiredJobResumed || expired
	cont, err := m.apply(ctx, w, p)
	if cont && err == nil {
		poke(wake)
	}
	return cont, err
}

func (m *Manager) rejected(ctx context.Context, w store.Workspace, code circusv1.CallUpdate_Rejected_Code) (bool, error) {
	switch code {
	case circusv1.CallUpdate_Rejected_CODE_OVERLOADED, circusv1.CallUpdate_Rejected_CODE_STORE_UNAVAILABLE,
		circusv1.CallUpdate_Rejected_CODE_DEVICE_SHUTTING_DOWN:
		return m.apply(ctx, w, m.backoff(w))
	case circusv1.CallUpdate_Rejected_CODE_UNKNOWN_DEVICE:
		return m.apply(ctx, w, failed(w, circusv1.WorkspaceError_WORKSPACE_ERROR_DEVICE_REVOKED, "", m.now()))
	default:
		m.cfg.Logger.Warn("workspace call rejected", "workspace_id", w.ID, "code", code)
		return m.apply(ctx, w, failed(w, circusv1.WorkspaceError_WORKSPACE_ERROR_DAEMON_FAULT, "", m.now()))
	}
}

func (m *Manager) apply(ctx context.Context, w store.Workspace, patch store.WorkspacePatch) (bool, error) {
	change, err := m.cfg.Store.TransitionWorkspace(ctx, w.ID, w.Expect(), patch)
	return m.changed(ctx, w, change, err)
}

func (m *Manager) changed(ctx context.Context, w store.Workspace, change store.WorkspaceChange, err error) (bool, error) {
	if err != nil {
		return true, err
	}
	if !change.Applied {
		return false, nil
	}
	m.Publish(change)
	if change.Workspace.State == store.WorkspaceReady || change.Workspace.State == store.WorkspaceFailed {
		_, err := m.cfg.Store.PruneWorkspaceCalls(ctx, w.SessionID(), store.MaxPageSize)
		m.logError(w, err)
	}
	return change.Workspace.State == store.WorkspacePreparing, nil
}

func pending(w store.Workspace, waiting store.WorkspaceWaiting, retryAt, now int64) store.WorkspacePatch {
	return store.WorkspacePatch{
		State: store.WorkspacePending, Waiting: waiting, RetryAt: retryAt, UpdatedAt: now,
		Attempt: w.Attempt, LostStreak: w.LostStreak, RepoURL: w.RepoURL, AttemptStartedAt: w.AttemptStartedAt,
	}
}

func failed(w store.Workspace, code circusv1.WorkspaceError, detail string, now int64) store.WorkspacePatch {
	p := pending(w, "", 0, now)
	p.State, p.ErrorCode, p.ErrorDetail = store.WorkspaceFailed, code, detail
	return p
}

func (m *Manager) lost(w store.Workspace) store.WorkspacePatch {
	p := pending(w, "", 0, m.now())
	p.LostStreak++
	if p.LostStreak >= 3 {
		p.State, p.ErrorCode = store.WorkspaceFailed, circusv1.WorkspaceError_WORKSPACE_ERROR_INTERRUPTED
	}
	return p
}

func (m *Manager) backoff(w store.Workspace) store.WorkspacePatch {
	delay := min(5*time.Second<<uint(min(w.BackoffStreak, 6)), 5*time.Minute)
	now := m.now()
	p := pending(w, store.WorkspaceBackoff, now+delay.Milliseconds(), now)
	p.BackoffStreak = min(w.BackoffStreak+1, 7)
	return p
}
