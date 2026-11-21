package webapi

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"
	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/auth"
	"github.com/EricWvi/circus/internal/controlplane/workspaces"
	"github.com/EricWvi/circus/internal/store"
)

func readSessionWorkspace(ctx context.Context, ops store.Ops, sess store.Session) (*circusv1.WorkspaceStatus, error) {
	if !sess.Workspace {
		return nil, nil
	}
	w, err := ops.GetWorkspace(ctx, sess.ProjectID, sess.DefaultDeviceID)
	if errors.Is(err, store.ErrNotFound) {
		// A missing row still blocks scheduling.
		device, err := ops.GetIdentity(ctx, store.KindDevice, sess.DefaultDeviceID)
		if err != nil {
			return nil, err
		}
		w = store.Workspace{ProjectID: sess.ProjectID, DeviceName: device.Name, WorkspacePatch: store.WorkspacePatch{State: store.WorkspacePending}}
		return w.Status(), nil
	}
	if err != nil {
		return nil, err
	}
	return w.Status(), nil
}

func retryWorkspace(ctx context.Context, tx store.Ops, w store.Workspace, now int64) (store.WorkspaceChange, error) {
	if w.State != store.WorkspaceFailed {
		return store.WorkspaceChange{Workspace: w}, nil
	}
	p := w.WorkspacePatch
	p.State, p.ErrorCode, p.ErrorDetail = store.WorkspacePending, 0, ""
	p.LostStreak, p.BackoffStreak, p.UpdatedAt = 0, 0, now
	return tx.TransitionWorkspace(ctx, w.ID, w.Expect(), p)
}

func (s *Service) workspaceCommitted(change store.WorkspaceChange) {
	workspaces.Publish(s.bus, s.waker, change)
	w := change.Workspace
	if s.workspaces != nil && (w.State == store.WorkspacePending || w.State == store.WorkspacePreparing) {
		s.workspaces.Kick(w.ID)
	}
}

func (s *Service) RetryWorkspace(ctx context.Context, req *connect.Request[circusv1.RetryWorkspaceRequest]) (*connect.Response[circusv1.RetryWorkspaceResponse], error) {
	u, err := user(ctx)
	if err != nil {
		return nil, err
	}
	id := req.Msg.GetSessionId()
	if err := sessionIDArg(id); err != nil {
		return nil, err
	}
	var change store.WorkspaceChange
	err = s.st.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		now := s.now().UnixMilli()
		sess, err := ownSession(ctx, tx, u, id, now)
		if err != nil {
			return err
		}
		if !sess.Workspace {
			return connect.NewError(connect.CodeFailedPrecondition, errors.New("the session does not use a project workspace"))
		}
		w, err := tx.GetWorkspace(ctx, sess.ProjectID, sess.DefaultDeviceID)
		if err != nil {
			return err
		}
		change, err = retryWorkspace(ctx, tx, w, now)
		return err
	})
	if err != nil {
		return nil, s.sessionError(ctx, "retry workspace", err)
	}
	s.workspaceCommitted(change)
	return connect.NewResponse(&circusv1.RetryWorkspaceResponse{Workspace: change.Workspace.Status()}), nil
}

func validateRemovalNames(names []string) error {
	if len(names) > 32 {
		return invalidField("remove_workspaces_on", "at most 32 device names")
	}
	for i, name := range names {
		if err := auth.ValidateName(name); err != nil {
			return invalidField(fmt.Sprintf("remove_workspaces_on[%d]", i), "invalid device name")
		}
	}
	return nil
}

// beginWorkspaceRemoval serializes directory removal for the same project.
// Session creation checks this gate inside its store transaction, before
// the deletion preflight can observe the project's sessions.
func (s *Service) beginWorkspaceRemoval(ctx context.Context, userID, projectID string) (func(), error) {
	key := [2]string{userID, projectID}
	for {
		s.removalMu.Lock()
		inProgress := s.removals[key]
		if inProgress == nil {
			done := make(chan struct{})
			s.removals[key] = done
			s.removalMu.Unlock()
			return func() {
				s.removalMu.Lock()
				defer s.removalMu.Unlock()
				delete(s.removals, key)
				close(done)
			}, nil
		}
		s.removalMu.Unlock()
		select {
		case <-inProgress:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (s *Service) removingWorkspaces(userID, projectID string) bool {
	s.removalMu.Lock()
	defer s.removalMu.Unlock()
	return s.removals[[2]string{userID, projectID}] != nil
}
