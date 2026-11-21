package storetest

import (
	"context"

	"github.com/EricWvi/circus/internal/store"
)

func (o *faultyOps) EnsureWorkspace(ctx context.Context, w store.Workspace) (v store.Workspace, created bool, err error) {
	err = o.write(ctx, func(s store.Ops) error { v, created, err = s.EnsureWorkspace(ctx, w); return err })
	return v, created, err
}

func (o *faultyOps) GetWorkspace(ctx context.Context, projectID, deviceID string) (v store.Workspace, err error) {
	err = o.read(ctx, func(s store.Ops) error { v, err = s.GetWorkspace(ctx, projectID, deviceID); return err })
	return v, err
}

func (o *faultyOps) GetWorkspaceByID(ctx context.Context, id string) (v store.Workspace, err error) {
	err = o.read(ctx, func(s store.Ops) error { v, err = s.GetWorkspaceByID(ctx, id); return err })
	return v, err
}

func (o *faultyOps) ListActiveWorkspaces(ctx context.Context, after *store.WorkspaceCursor, limit int) (v []store.Workspace, err error) {
	err = o.read(ctx, func(s store.Ops) error { v, err = s.ListActiveWorkspaces(ctx, after, limit); return err })
	return v, err
}

func (o *faultyOps) ListProjectWorkspaces(ctx context.Context, projectID, afterDeviceID string, limit int) (v []store.Workspace, err error) {
	err = o.read(ctx, func(s store.Ops) error {
		v, err = s.ListProjectWorkspaces(ctx, projectID, afterDeviceID, limit)
		return err
	})
	return v, err
}

func (o *faultyOps) TransitionWorkspace(ctx context.Context, id string, expect store.WorkspaceExpect, p store.WorkspacePatch) (v store.WorkspaceChange, err error) {
	err = o.write(ctx, func(s store.Ops) error { v, err = s.TransitionWorkspace(ctx, id, expect, p); return err })
	return v, err
}

func (o *faultyOps) MarkWorkspaceReady(ctx context.Context, id string, expect store.WorkspaceExpect, path, action string, now int64) (v store.WorkspaceChange, err error) {
	err = o.write(ctx, func(s store.Ops) error { v, err = s.MarkWorkspaceReady(ctx, id, expect, path, action, now); return err })
	return v, err
}

func (o *faultyOps) GetWorkspaceJob(ctx context.Context, deviceID, bootID, jobID string) (v store.Job, err error) {
	err = o.read(ctx, func(s store.Ops) error { v, err = s.GetWorkspaceJob(ctx, deviceID, bootID, jobID); return err })
	return v, err
}

func (o *faultyOps) ListRunningWorkspaceJobs(ctx context.Context, deviceID, keepBootID string, limit int) (v []store.Job, err error) {
	err = o.read(ctx, func(s store.Ops) error {
		v, err = s.ListRunningWorkspaceJobs(ctx, deviceID, keepBootID, limit)
		return err
	})
	return v, err
}

func (o *faultyOps) PruneWorkspaceCalls(ctx context.Context, sessionID string, limit int) (n int, err error) {
	err = o.write(ctx, func(s store.Ops) error { n, err = s.PruneWorkspaceCalls(ctx, sessionID, limit); return err })
	return n, err
}

func (o *faultyOps) ListDeviceWorkspaces(ctx context.Context, userID, deviceID string, limit int) (v []store.Workspace, err error) {
	err = o.read(ctx, func(s store.Ops) error { v, err = s.ListDeviceWorkspaces(ctx, userID, deviceID, limit); return err })
	return v, err
}
