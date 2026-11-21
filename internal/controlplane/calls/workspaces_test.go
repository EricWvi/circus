package calls_test

import (
	"errors"
	"testing"

	pb "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/controlplane/calls"
	"github.com/EricWvi/circus/internal/controlplane/gateway"
	"github.com/EricWvi/circus/internal/store"
)

func (f *fixture) newWorkspace(state store.WorkspaceState) store.Workspace {
	f.t.Helper()
	now := f.clock.Now().UnixMilli()
	_, err := f.db.CreateProject(f.ctx, store.Project{ID: "p", UserID: "u", Name: "Workspace", CreatedAt: now, UpdatedAt: now})
	must(f.t, err)
	w, _, err := f.db.EnsureWorkspace(f.ctx, store.Workspace{ID: "w", UserID: "u", ProjectID: "p", DeviceID: "d", CreatedAt: now})
	must(f.t, err)
	if state == store.WorkspacePending {
		return w
	}
	change, err := f.db.TransitionWorkspace(f.ctx, w.ID, w.Expect(), store.WorkspacePatch{
		State: store.WorkspacePreparing, Attempt: 1, CallID: "ws-current", TimeoutMs: 120000, UpdatedAt: now,
	})
	must(f.t, err)
	if state == store.WorkspaceReady {
		w = change.Workspace
		change, err = f.db.MarkWorkspaceReady(f.ctx, w.ID, w.Expect(), "/home/u/circus/projects/p", "created", now)
		must(f.t, err)
	}
	if !change.Applied {
		f.t.Fatal("workspace state change was not applied")
	}
	return change.Workspace
}

func workspaceCall(w store.Workspace, id string, remove bool) *pb.DeviceCall {
	path := store.DefaultWorkspacePath(w.ProjectID)
	call := &pb.DeviceCall{SessionId: w.SessionID(), CallId: id, TimeoutMs: 120000,
		Tool: &pb.DeviceCall_PrepareWorkspace{PrepareWorkspace: &pb.PrepareWorkspaceRequest{Path: path}}}
	if remove {
		call.TimeoutMs = 30000
		call.Tool = &pb.DeviceCall_RemoveWorkspace{RemoveWorkspace: &pb.RemoveWorkspaceRequest{Path: path}}
	}
	return call
}

func TestWorkspaceCallAdmission(t *testing.T) {
	for _, tc := range []struct {
		name   string
		state  store.WorkspaceState
		id     string
		remove bool
		worker bool
	}{
		{"worker prepare", store.WorkspacePreparing, "ws-current", false, true},
		{"worker remove", store.WorkspaceReady, "ws-remove", true, true},
		{"stale prepare", store.WorkspacePreparing, "ws-stale", false, false},
		{"prepare while pending", store.WorkspacePending, "ws-new", false, false},
		{"remove before ready", store.WorkspacePreparing, "ws-remove", true, false},
		{"invalid call id", store.WorkspaceReady, "ws illegal", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t)
			f.connect("boot")
			w := f.newWorkspace(tc.state)
			call := workspaceCall(w, tc.id, tc.remove)
			var sub *calls.Subscription
			var err error
			if tc.worker {
				sub, err = f.router.SubmitSession(f.ctx, w.UserID, w.DeviceName, call, calls.Admission{})
			} else {
				sub, err = f.router.Submit(f.ctx, w.UserID, w.DeviceName, call)
			}
			must(t, err)
			defer sub.Close()
			update, err := sub.Next(f.ctx)
			must(t, err)
			if update.GetRejected().GetCode() != pb.CallUpdate_Rejected_CODE_INVALID {
				t.Fatalf("submit = %v, want INVALID", update)
			}
			if _, err := f.db.GetCall(f.ctx, tc.id); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("refused call was retained: %v", err)
			}
		})
	}
}

func TestWorkspaceRemoveRequiresReadyRoute(t *testing.T) {
	f := setup(t)
	w := f.newWorkspace(store.WorkspaceReady)
	_, err := f.registry.Register(f.ctx, gateway.Connection{
		Target:     gateway.Target{InstanceID: "instance", ConnectionID: "syncing", DeviceID: w.DeviceID, BootID: "boot"},
		DeviceName: w.DeviceName, UserID: w.UserID,
	})
	must(t, err)
	call := workspaceCall(w, "ws-remove", true)
	sub, err := f.router.Submit(f.ctx, w.UserID, w.DeviceName, call)
	must(t, err)
	defer sub.Close()
	update, err := sub.Next(f.ctx)
	must(t, err)
	if update.GetOffline() == nil || f.router.Stats().PendingCalls != 0 {
		t.Fatalf("unready route: %v, router: %+v", update, f.router.Stats())
	}
	if _, err := f.db.GetCall(f.ctx, call.CallId); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unready removal was retained: %v", err)
	}
	if ok, err := f.registry.Unregister(f.ctx, gateway.Connection{
		Target: gateway.Target{InstanceID: "instance", ConnectionID: "syncing", DeviceID: w.DeviceID, BootID: "boot"},
	}); err != nil || !ok {
		t.Fatalf("unregister: %v, %v", ok, err)
	}
}

func TestWorkspaceDeliverRequiresHelloFeature(t *testing.T) {
	for _, tc := range []struct {
		name   string
		state  store.WorkspaceState
		id     string
		remove bool
	}{
		{"prepare", store.WorkspacePreparing, "ws-current", false},
		{"remove", store.WorkspaceReady, "ws-remove", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t)
			link := f.connect("boot") // Hello advertises neither workspace feature.
			w := f.newWorkspace(tc.state)
			call := workspaceCall(w, tc.id, tc.remove)
			sub, err := f.router.Submit(f.ctx, w.UserID, w.DeviceName, call)
			must(t, err)
			defer sub.Close()
			row, err := f.db.GetCall(f.ctx, call.CallId)
			must(t, err)
			// Deliver returns synchronously inside Submit. If the gate were
			// bypassed the call would still be DISPATCHED on this idle link.
			if row.State != store.CallCompleted {
				msg := receive(t, f.ctx, link)
				t.Fatalf("old daemon received %v", msg)
			}
			for {
				update, err := sub.Next(f.ctx)
				must(t, err)
				if update.GetAccepted() != nil || update.GetDispatched() != nil {
					continue
				}
				if update.GetResult().GetFault().GetCode() != pb.DaemonFault_CODE_TOOL_UNAVAILABLE {
					t.Fatalf("submit = %v, want TOOL_UNAVAILABLE", update)
				}
				break
			}
		})
	}
}
