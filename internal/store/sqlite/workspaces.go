package sqlite

import (
	"context"
	"database/sql"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"

	"github.com/EricWvi/circus/internal/store"
)

const workspaceCols = `w.workspace_id, w.user_id, w.project_id, w.device_id, d.name, w.created_at,
	w.state, w.waiting, w.path, w.action, w.repo_url, w.attempt, w.lost_streak, w.call_id, w.timeout_ms,
	w.resume_job_id, w.job_id, w.job_boot_id, w.error_code, w.error_detail, w.retry_at,
	w.attempt_started_at, w.ready_at, w.updated_at, w.backoff_streak, w.expired_job_resumed, w.job_recorded_at`

const workspaceFrom = ` FROM workspaces w JOIN devices d ON d.id = w.device_id`

type workspaceScan struct {
	store.Workspace
	state             sql.Null[store.WorkspaceState]
	waiting           sql.Null[store.WorkspaceWaiting]
	errorCode         sql.Null[circusv1.WorkspaceError]
	expiredJobResumed sql.Null[bool]
}

func (w *workspaceScan) dest() []any {
	return []any{nullStr{&w.ID}, nullStr{&w.UserID}, nullStr{&w.ProjectID}, nullStr{&w.DeviceID}, nullStr{&w.DeviceName}, nullInt{&w.CreatedAt},
		&w.state, &w.waiting, nullStr{&w.Path}, nullStr{&w.Action}, nullStr{&w.RepoURL},
		nullInt{&w.Attempt}, nullInt{&w.LostStreak}, nullStr{&w.CallID}, nullInt{&w.TimeoutMs},
		nullStr{&w.ResumeJobID}, nullStr{&w.JobID}, nullStr{&w.JobBootID}, &w.errorCode, nullStr{&w.ErrorDetail},
		nullInt{&w.RetryAt}, nullInt{&w.AttemptStartedAt}, nullInt{&w.ReadyAt}, nullInt{&w.UpdatedAt}, nullInt{&w.BackoffStreak}, &w.expiredJobResumed, nullInt{&w.JobRecordedAt}}
}

func (w *workspaceScan) result() store.Workspace {
	w.State, w.Waiting, w.ErrorCode = w.state.V, w.waiting.V, w.errorCode.V
	w.ExpiredJobResumed = w.expiredJobResumed.V
	return w.Workspace
}

func scanWorkspace(s scanner) (store.Workspace, error) {
	var w workspaceScan
	err := s.Scan(w.dest()...)
	return w.result(), mapErr(err)
}

func (o *ops) EnsureWorkspace(ctx context.Context, w store.Workspace) (store.Workspace, bool, error) {
	for _, v := range []string{w.ID, w.UserID, w.ProjectID, w.DeviceID} {
		if err := store.CheckID("workspace key", v); err != nil {
			return store.Workspace{}, false, err
		}
	}
	if w.CreatedAt == 0 {
		return store.Workspace{}, false, invalid("workspace needs a creation time")
	}
	var got store.Workspace
	var created bool
	err := o.unit(ctx, func(tx *ops) error {
		n, err := tx.exec(ctx, `INSERT INTO workspaces (workspace_id, user_id, project_id, device_id, state, created_at, updated_at)
			SELECT ?, user_id, id, ?, 'pending', ?, ? FROM projects WHERE id = ? AND user_id = ?
			ON CONFLICT(project_id, device_id) DO NOTHING`, w.ID, w.DeviceID, w.CreatedAt, w.CreatedAt, w.ProjectID, w.UserID)
		if err != nil {
			return err
		}
		got, err = tx.GetWorkspace(ctx, w.ProjectID, w.DeviceID)
		if err == nil && got.UserID != w.UserID {
			return store.ErrNotFound
		}
		created = n == 1
		return err
	})
	if err != nil {
		return store.Workspace{}, false, err
	}
	return got, created, nil
}

func (o *ops) GetWorkspace(ctx context.Context, projectID, deviceID string) (store.Workspace, error) {
	return scanWorkspace(o.q.QueryRow(ctx, `SELECT `+workspaceCols+workspaceFrom+
		` WHERE w.project_id = ? AND w.device_id = ?`, projectID, deviceID))
}

func (o *ops) GetWorkspaceByID(ctx context.Context, id string) (store.Workspace, error) {
	return scanWorkspace(o.q.QueryRow(ctx, `SELECT `+workspaceCols+workspaceFrom+` WHERE w.workspace_id = ?`, id))
}

func (o *ops) ListActiveWorkspaces(ctx context.Context, after *store.WorkspaceCursor, limit int) ([]store.Workspace, error) {
	if err := store.CheckLimit(limit); err != nil {
		return nil, err
	}
	query := `SELECT ` + workspaceCols + workspaceFrom + ` WHERE w.state IN ('pending', 'preparing')`
	var args []any
	if after != nil {
		query += ` AND (w.state, w.updated_at, w.workspace_id) < (?, ?, ?)`
		args = append(args, after.State, after.UpdatedAt, after.ID)
	}
	return o.listWorkspaces(ctx, query+` ORDER BY w.state DESC, w.updated_at DESC, w.workspace_id DESC LIMIT ?`, append(args, limit)...)
}

func (o *ops) ListProjectWorkspaces(ctx context.Context, projectID, afterDeviceID string, limit int) ([]store.Workspace, error) {
	if err := store.CheckLimit(limit); err != nil {
		return nil, err
	}
	return o.listWorkspaces(ctx, `SELECT `+workspaceCols+workspaceFrom+
		` WHERE w.project_id = ? AND w.device_id > ? ORDER BY w.device_id LIMIT ?`, projectID, afterDeviceID, limit)
}

func (o *ops) listWorkspaces(ctx context.Context, query string, args ...any) ([]store.Workspace, error) {
	rows, err := o.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, mapErr(err)
	}
	return collect(rows, scanWorkspace)
}

func validWorkspaceEdge(from, to store.WorkspaceState) bool {
	switch from {
	case store.WorkspacePending:
		return to == store.WorkspacePending || to == store.WorkspacePreparing || to == store.WorkspaceFailed
	case store.WorkspacePreparing:
		return to == store.WorkspacePreparing || to == store.WorkspacePending || to == store.WorkspaceReady || to == store.WorkspaceFailed
	case store.WorkspaceFailed:
		return to == store.WorkspacePending
	}
	return false
}

func (o *ops) TransitionWorkspace(ctx context.Context, id string, expect store.WorkspaceExpect, patch store.WorkspacePatch) (store.WorkspaceChange, error) {
	if patch.State == store.WorkspaceReady {
		return store.WorkspaceChange{}, invalid("use MarkWorkspaceReady to set the path")
	}
	return o.changeWorkspace(ctx, id, expect, func(store.Workspace) store.WorkspacePatch { return patch })
}

func (o *ops) MarkWorkspaceReady(ctx context.Context, id string, expect store.WorkspaceExpect, path, action string, now int64) (store.WorkspaceChange, error) {
	return o.changeWorkspace(ctx, id, expect, func(w store.Workspace) store.WorkspacePatch {
		return store.WorkspacePatch{
			State: store.WorkspaceReady, Path: path, Action: action, RepoURL: w.RepoURL, Attempt: w.Attempt,
			AttemptStartedAt: w.AttemptStartedAt, ReadyAt: now, UpdatedAt: now,
		}
	})
}

const workspaceSessions = `workspace = 1 AND project_id = ? AND default_device_id = ?`

func (o *ops) changeWorkspace(ctx context.Context, id string, expect store.WorkspaceExpect, patch func(store.Workspace) store.WorkspacePatch) (store.WorkspaceChange, error) {
	if err := store.CheckID("workspace id", id); err != nil {
		return store.WorkspaceChange{}, err
	}
	var out store.WorkspaceChange
	err := o.unit(ctx, func(tx *ops) error {
		w, err := tx.GetWorkspaceByID(ctx, id)
		if isNotFound(err) || (err == nil && w.Expect() != expect) {
			return nil
		}
		if err != nil {
			return err
		}
		p := patch(w)
		if !validWorkspaceEdge(w.State, p.State) {
			return store.ErrInvalidTransition
		}
		if err := checkWorkspacePatch(w, p); err != nil {
			return err
		}
		p.ErrorDetail = store.WorkspaceErrorDetail(p.ErrorDetail, p.RepoURL)
		switch {
		case p.JobID == "":
			p.JobRecordedAt = 0
		case p.JobID != w.JobID || p.JobBootID != w.JobBootID:
			// Use the registration clock, before applying the version bump.
			p.JobRecordedAt = p.UpdatedAt
		default:
			p.JobRecordedAt = w.JobRecordedAt
		}
		p.UpdatedAt = max(w.UpdatedAt+1, p.UpdatedAt)
		n, err := tx.exec(ctx, `UPDATE workspaces SET state = ?, waiting = ?, path = ?, action = ?, repo_url = ?,
			attempt = ?, lost_streak = ?, call_id = ?, timeout_ms = ?, resume_job_id = ?, job_id = ?, job_boot_id = ?,
			error_code = ?, error_detail = ?, retry_at = ?, attempt_started_at = ?, ready_at = ?, updated_at = ?, backoff_streak = ?, expired_job_resumed = ?, job_recorded_at = ?
			WHERE workspace_id = ? AND state = ? AND attempt = ? AND call_id IS ? AND updated_at = ?`,
			p.State, nstr(string(p.Waiting)), nstr(p.Path), nstr(p.Action), nstr(p.RepoURL),
			p.Attempt, p.LostStreak, nstr(p.CallID), nint(p.TimeoutMs), nstr(p.ResumeJobID), nstr(p.JobID), nstr(p.JobBootID),
			nint(int64(p.ErrorCode)), nstr(p.ErrorDetail), nint(p.RetryAt), nint(p.AttemptStartedAt), nint(p.ReadyAt), p.UpdatedAt, p.BackoffStreak, b2i(p.ExpiredJobResumed), nint(p.JobRecordedAt),
			id, expect.State, expect.Attempt, nstr(expect.CallID), expect.UpdatedAt)
		if err != nil || n == 0 {
			return err
		}
		if p.State == store.WorkspaceReady {
			if _, err := tx.exec(ctx, `UPDATE session_cwds SET cwd = ?, updated_at = ? WHERE agent_id = '' AND device_id = ?
				AND session_id IN (SELECT session_id FROM sessions WHERE `+workspaceSessions+`)`,
				p.Path, p.ReadyAt, w.DeviceID, w.ProjectID, w.DeviceID); err != nil {
				return err
			}
		}
		durable := w.State != p.State
		ephemeral := w.Waiting != p.Waiting || w.Background() != p.Background()
		w.WorkspacePatch = p
		out = store.WorkspaceChange{Applied: true, Workspace: w}
		if durable || ephemeral {
			out.Notices, err = tx.workspaceNotices(ctx, w, durable)
		}
		return err
	})
	if err != nil {
		return store.WorkspaceChange{}, err
	}
	return out, nil
}

func checkWorkspacePatch(w store.Workspace, p store.WorkspacePatch) error {
	attempt := w.Attempt
	if w.State == store.WorkspacePending && p.State == store.WorkspacePreparing {
		attempt++
	}
	if p.UpdatedAt == 0 || p.Attempt != attempt || p.LostStreak < 0 || p.LostStreak > 3 || p.BackoffStreak < 0 || p.BackoffStreak > 7 {
		return invalid("invalid workspace counters or time")
	}
	if p.State == store.WorkspaceReady {
		if err := store.CheckWorkspacePath(p.Path); err != nil {
			return err
		}
	}
	if err := store.CheckText("workspace repository", p.RepoURL, 2048, true); err != nil {
		return err
	}
	for _, value := range []string{p.CallID, p.ResumeJobID, p.JobID, p.JobBootID} {
		if err := store.CheckOptionalID("workspace call or job id", value); err != nil {
			return err
		}
	}
	return nil
}

func (o *ops) workspaceNotices(ctx context.Context, w store.Workspace, durable bool) ([]store.WorkspaceNotice, error) {
	rows, err := o.q.QueryContext(ctx, `SELECT session_id FROM sessions WHERE `+workspaceSessions+
		` AND state <> 'stopped' ORDER BY session_id`, w.ProjectID, w.DeviceID)
	if err != nil {
		return nil, mapErr(err)
	}
	out, err := collect(rows, func(s scanner) (store.WorkspaceNotice, error) {
		var n store.WorkspaceNotice
		err := s.Scan(&n.SessionID)
		return n, err
	})
	if err != nil || !durable {
		return out, err
	}
	event := []store.EventInput{{Kind: store.EventKindWorkspace, At: w.UpdatedAt, Payload: w.EventPayload(), Boundary: true}}
	for i := range out {
		seqs, err := o.storeEvents(ctx, out[i].SessionID, event, store.DefaultMaxViewBytes)
		if err != nil {
			return nil, err
		}
		out[i].Seq = seqs[0]
	}
	return out, nil
}

func (o *ops) GetWorkspaceJob(ctx context.Context, deviceID, bootID, jobID string) (store.Job, error) {
	return scanJob(o.q.QueryRow(ctx, `SELECT `+jobCols+` FROM jobs WHERE device_id = ? AND boot_id = ? AND job_id = ?`,
		deviceID, bootID, jobID))
}

func (o *ops) PruneWorkspaceCalls(ctx context.Context, sessionID string, limit int) (int, error) {
	if err := store.CheckLimit(limit); err != nil {
		return 0, err
	}
	query := `DELETE FROM calls WHERE call_id IN (SELECT c.call_id FROM calls c
		WHERE c.session_id GLOB 'workspace-*' AND c.call_id GLOB 'ws-*'
		AND c.tool IN ('prepare_workspace', 'remove_workspace')
		AND c.state IN ('completed', 'offline', 'cancelled', 'rejected', 'lost')
		AND NOT EXISTS (SELECT 1 FROM workspaces w
			WHERE w.workspace_id = substr(c.session_id, 11) AND w.state = 'preparing' AND w.call_id = c.call_id)`
	var args []any
	if sessionID != "" {
		query += ` AND c.session_id = ?`
		args = append(args, sessionID)
	}
	return o.exec(ctx, query+` ORDER BY c.session_id, c.call_id LIMIT ?)`, append(args, limit)...)
}

func (o *ops) ListRunningWorkspaceJobs(ctx context.Context, deviceID, keepBootID string, limit int) ([]store.Job, error) {
	if err := store.CheckLimit(limit); err != nil {
		return nil, err
	}
	rows, err := o.q.QueryContext(ctx, `SELECT `+jobCols+` FROM jobs j
		WHERE j.device_id = ? AND j.boot_id <> ? AND j.state = 'running'
		AND j.session_id GLOB 'workspace-*' AND EXISTS (SELECT 1 FROM workspaces w
			WHERE w.workspace_id = substr(j.session_id, 11) AND w.state = 'preparing'
			AND (w.call_id = j.call_id OR (w.job_id = j.job_id AND w.job_boot_id = j.boot_id)))
		ORDER BY j.boot_id, j.job_id LIMIT ?`, deviceID, keepBootID, limit)
	if err != nil {
		return nil, mapErr(err)
	}
	return collect(rows, scanJob)
}

func (o *ops) ListDeviceWorkspaces(ctx context.Context, userID, deviceID string, limit int) ([]store.Workspace, error) {
	if limit < 1 || limit > store.MaxProjectsPerUser {
		return nil, invalid("workspace device scan limit")
	}
	return o.listWorkspaces(ctx, `SELECT `+workspaceCols+workspaceFrom+`
		WHERE w.user_id = ? AND w.device_id = ? AND w.state IN ('pending', 'preparing')
		ORDER BY w.state DESC, w.updated_at DESC, w.workspace_id DESC LIMIT ?`, userID, deviceID, limit)
}
