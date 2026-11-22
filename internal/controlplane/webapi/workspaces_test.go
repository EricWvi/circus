package webapi_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/gen/go/circus/v1/circusv1connect"
	"github.com/EricWvi/circus/internal/clock"
	"github.com/EricWvi/circus/internal/controlplane/webapi"
	"github.com/EricWvi/circus/internal/controlplane/workspaces"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/testutil/wait"
	"github.com/EricWvi/circus/internal/wire"
	"google.golang.org/protobuf/proto"
)

func workspaceCheck(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func (h *harness) prepareWorkspace(t *testing.T, projectID, deviceID string) store.Workspace {
	t.Helper()
	ctx := t.Context()
	w, _, err := h.st.EnsureWorkspace(ctx, store.Workspace{
		ID: "ws-" + deviceID, UserID: h.owner.ID, ProjectID: projectID, DeviceID: deviceID, CreatedAt: h.clock.Load(),
	})
	workspaceCheck(t, err)
	change, err := h.st.TransitionWorkspace(ctx, w.ID, w.Expect(), store.WorkspacePatch{
		State: store.WorkspacePreparing, CallID: w.ID + "-prepare", TimeoutMs: 120000,
		RepoURL: "https://user:omitted@example.com/repo", Attempt: w.Attempt + 1, UpdatedAt: h.clock.Load(),
	})
	workspaceCheck(t, err)
	if !change.Applied {
		t.Fatal("preparing CAS failed")
	}
	return change.Workspace
}

func (h *harness) markWorkspaceReady(t *testing.T, w store.Workspace) store.Workspace {
	t.Helper()
	change, err := h.st.MarkWorkspaceReady(t.Context(), w.ID, w.Expect(), "/home/user/"+w.DeviceID, "cloned", h.clock.Load())
	workspaceCheck(t, err)
	if !change.Applied {
		t.Fatal("ready CAS failed")
	}
	return change.Workspace
}

func TestWorkspaceSessionCreateAndProjection(t *testing.T) {
	h, browser := newHarness(t)
	ctx := t.Context()
	h.device(t, "d1", "box", "linux")
	p := browser.mustCreate(t, "Workspace", loc("box", ""), loc("box", "/explicit"))
	if p.Locations[0].GetWorkspace() != nil {
		t.Fatal("uncreated workspace exposed")
	}
	create := func(key string, index uint32) *pb.Session {
		t.Helper()
		s, err := browser.createSession(t, &pb.CreateSessionRequest{ProjectId: p.ProjectId, ClientRequestId: key, LocationIndex: &index})
		workspaceCheck(t, err)
		return s
	}
	first := create("first", 0)
	if first.GetWorkspace().GetState() != pb.WorkspaceStatus_STATE_PENDING || first.GetWorkspace().GetDevice() != "box" ||
		first.Config.GetCwds()[0].GetCwd() != store.DefaultWorkspacePath(p.ProjectId) {
		t.Fatalf("first session = %v", first)
	}
	page, err := h.st.ReadEvents(ctx, store.EventFilter{SessionID: first.SessionId, Limit: 4, MaxBytes: 4096})
	workspaceCheck(t, err)
	if len(page.Events) != 1 || page.Events[0].Kind != store.EventKindWorkspace || page.Events[0].At != first.GetWorkspace().GetUpdatedAtUnixMs() {
		t.Fatalf("initial workspace event = %+v, status = %v", page.Events, first.GetWorkspace())
	}
	w := h.prepareWorkspace(t, p.ProjectId, "d1")
	failed, err := h.st.TransitionWorkspace(ctx, w.ID, w.Expect(), store.WorkspacePatch{
		State: store.WorkspaceFailed, Attempt: w.Attempt, LostStreak: 3, RepoURL: w.RepoURL,
		ErrorCode: pb.WorkspaceError_WORKSPACE_ERROR_NOT_EMPTY, UpdatedAt: h.clock.Load(),
	})
	workspaceCheck(t, err)
	if got := create("first", 0); got.GetWorkspace().GetState() != pb.WorkspaceStatus_STATE_FAILED {
		t.Fatalf("idempotent create triggered a retry: %v", got)
	}
	second := create("second", 0)
	if second.GetWorkspace().GetState() != pb.WorkspaceStatus_STATE_PENDING {
		t.Fatalf("new session did not retry failed workspace: %v", second)
	}
	w, err = h.st.GetWorkspaceByID(ctx, failed.Workspace.ID)
	workspaceCheck(t, err)
	if w.LostStreak != 0 || w.ErrorCode != 0 {
		t.Fatalf("failed workspace metadata survived retry: %+v", w)
	}
	w = h.markWorkspaceReady(t, h.prepareWorkspace(t, p.ProjectId, "d1"))
	third := create("third", 0)
	if third.GetWorkspace().GetState() != pb.WorkspaceStatus_STATE_READY || third.Config.GetCwds()[0].GetCwd() != w.Path ||
		strings.Contains(third.GetWorkspace().GetRepoUrl(), "omitted") {
		t.Fatalf("ready session = %v", third)
	}
	explicit := create("explicit", 1)
	if explicit.GetWorkspace() != nil || explicit.Config.GetCwds()[0].GetCwd() != "/explicit" {
		t.Fatalf("explicit session = %v", explicit)
	}
	for _, s := range []string{first.SessionId, second.SessionId, third.SessionId} {
		got := browser.getSession(t, s)
		if !proto.Equal(got.GetWorkspace(), third.GetWorkspace()) || got.Config.GetCwds()[0].Cwd != w.Path {
			t.Fatalf("get session = %v", got)
		}
	}
	update, err := browser.sessions.UpdateSession(ctx, connect.NewRequest(&pb.UpdateSessionRequest{SessionId: first.SessionId, Title: proto.String("ready")}))
	workspaceCheck(t, err)
	if !proto.Equal(update.Msg.GetWorkspace(), third.GetWorkspace()) {
		t.Fatalf("update session = %v", update.Msg)
	}
	listed, err := browser.sessions.ListSessions(ctx, connect.NewRequest(&pb.ListSessionsRequest{ProjectId: p.ProjectId}))
	workspaceCheck(t, err)
	for _, s := range listed.Msg.Sessions {
		if s.SessionId != explicit.SessionId && !proto.Equal(s.GetWorkspace(), third.GetWorkspace()) {
			t.Fatalf("listed session missing workspace: %v", s)
		}
	}
	project, err := browser.projects.GetProject(ctx, connect.NewRequest(&pb.GetProjectRequest{ProjectId: p.ProjectId}))
	workspaceCheck(t, err)
	locations := project.Msg.GetProject().GetLocations()
	if locations[0].GetEffectivePath() != store.DefaultWorkspacePath(p.ProjectId) ||
		!proto.Equal(locations[0].GetWorkspace(), third.GetWorkspace()) || locations[1].GetWorkspace() != nil {
		t.Fatalf("project locations = %v", locations)
	}
}

func TestRetryWorkspaceIdempotent(t *testing.T) {
	h, browser := newHarness(t)
	ctx := t.Context()
	h.device(t, "d1", "box", "linux")
	p := browser.mustCreate(t, "Workspace", loc("box", ""), loc("box", "/explicit"))
	sess, err := browser.createSession(t, &pb.CreateSessionRequest{ProjectId: p.ProjectId, ClientRequestId: "first"})
	workspaceCheck(t, err)
	other, err := browser.createSession(t, &pb.CreateSessionRequest{ProjectId: p.ProjectId, ClientRequestId: "other", LocationIndex: new(uint32(1))})
	workspaceCheck(t, err)
	w := h.prepareWorkspace(t, p.ProjectId, "d1")
	_, err = h.st.TransitionWorkspace(ctx, w.ID, w.Expect(), store.WorkspacePatch{
		State: store.WorkspaceFailed, Attempt: w.Attempt, LostStreak: 3, BackoffStreak: 6,
		ErrorCode: pb.WorkspaceError_WORKSPACE_ERROR_INTERRUPTED, UpdatedAt: h.clock.Load(),
	})
	workspaceCheck(t, err)
	_, err = browser.sessions.StopSession(ctx, connect.NewRequest(&pb.StopSessionRequest{SessionId: sess.SessionId}))
	workspaceCheck(t, err)
	request := func(id string) *connect.Request[pb.RetryWorkspaceRequest] {
		return connect.NewRequest(&pb.RetryWorkspaceRequest{SessionId: id})
	}
	first, err := browser.sessions.RetryWorkspace(ctx, request(sess.SessionId))
	workspaceCheck(t, err)
	second, err := browser.sessions.RetryWorkspace(ctx, request(sess.SessionId))
	workspaceCheck(t, err)
	if first.Msg.GetWorkspace().GetState() != pb.WorkspaceStatus_STATE_PENDING || !proto.Equal(first.Msg, second.Msg) {
		t.Fatalf("retry responses %v, %v", first.Msg, second.Msg)
	}
	_, err = browser.sessions.RetryWorkspace(ctx, request(other.SessionId))
	wantCode(t, err, connect.CodeFailedPrecondition)
	w, err = h.st.GetWorkspaceByID(ctx, w.ID)
	workspaceCheck(t, err)
	if w.LostStreak != 0 || w.BackoffStreak != 0 || w.ErrorCode != 0 || w.ErrorDetail != "" {
		t.Fatalf("retry did not reset error: %+v", w)
	}
}

func TestWorkspaceConcurrentCreate(t *testing.T) {
	h, browser := newHarness(t)
	h.device(t, "d1", "box", "linux")
	p := browser.mustCreate(t, "Workspace", loc("box", ""))
	ctx := wait.Ctx(t)
	var wg sync.WaitGroup
	start := make(chan struct{})
	const count = 8
	var responses [count]*connect.Response[pb.CreateSessionResponse]
	var errs [count]error
	for i := range count {
		wg.Go(func() {
			<-start
			responses[i], errs[i] = browser.sessions.CreateSession(ctx, connect.NewRequest(&pb.CreateSessionRequest{
				ProjectId: p.ProjectId, ClientRequestId: fmt.Sprintf("client-%d", i),
			}))
		})
	}
	close(start)
	wg.Wait()
	for i, response := range responses {
		workspaceCheck(t, errs[i])
		if response.Msg.GetSession().GetWorkspace().GetState() != pb.WorkspaceStatus_STATE_PENDING {
			t.Fatalf("create = %v", response.Msg)
		}
	}
	rows, err := h.st.ListProjectWorkspaces(ctx, p.ProjectId, "", 10)
	workspaceCheck(t, err)
	if len(rows) != 1 {
		t.Fatalf("concurrent creates produced %d workspace rows", len(rows))
	}
}

type removalSubscription struct {
	update  *pb.CallUpdate
	release <-chan struct{}
}

func (s removalSubscription) Next(ctx context.Context) (*pb.CallUpdate, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.release:
		return s.update, nil
	}
}

func (removalSubscription) Close() error { return nil }

func TestDeleteProjectWorkspaceRemovals(t *testing.T) {
	type submission struct {
		device, user string
		call         *pb.DeviceCall
	}
	release := make(chan struct{})
	calls := make(chan submission, 16)
	cancelled, stopped := make(chan string, 2), make(chan string, 2)
	h, browser := newHarness(t, func(cfg *webapi.Config) {
		cfg.Workspaces = workspaces.New(workspaces.Config{
			Store:    cfg.Store,
			Cancel:   func(_ context.Context, _, callID string) error { cancelled <- callID; return nil },
			StopJobs: func(_ context.Context, _, sessionID string) error { stopped <- sessionID; return nil },
			Supports: func(deviceID, feature string) (bool, bool) {
				return deviceID != "offline", deviceID != "old" && feature == wire.FeatureRemoveWorkspace
			},
			Submit: func(_ context.Context, user, device string, request *pb.DeviceCall) (workspaces.Subscription, error) {
				calls <- submission{device, user, request}
				result := &pb.RemoveWorkspaceResult{Outcome: pb.RemoveWorkspaceResult_OUTCOME_REMOVED}
				switch device {
				case "failed":
					result.Outcome, result.Error = pb.RemoveWorkspaceResult_OUTCOME_FAILED, pb.WorkspaceError_WORKSPACE_ERROR_REFUSED
					result.ErrorDetail = "refused"
				case "absent":
					result.Outcome = pb.RemoveWorkspaceResult_OUTCOME_ABSENT
				}
				return removalSubscription{release: release, update: &pb.CallUpdate{Update: &pb.CallUpdate_Result{Result: &pb.DeviceResult{
					Detail: &pb.DeviceResult_RemoveWorkspace{RemoveWorkspace: result},
				}}}}, nil
			},
		})
	})
	for _, name := range []string{"removed", "offline", "pending", "failed", "old", "absent", "no-row"} {
		h.device(t, name, name, "linux")
	}
	p := browser.mustCreate(t, "Project", loc("removed", ""))
	ready := make(map[string]store.Workspace)
	for _, name := range []string{"removed", "offline", "failed", "old", "absent"} {
		ready[name] = h.markWorkspaceReady(t, h.prepareWorkspace(t, p.ProjectId, name))
	}
	active := h.prepareWorkspace(t, p.ProjectId, "pending")

	for _, names := range [][]string{{"bad name"}, make([]string, 33)} {
		_, err := browser.projects.DeleteProject(t.Context(), connect.NewRequest(&pb.DeleteProjectRequest{ProjectId: p.ProjectId, RemoveWorkspacesOn: names}))
		wantCode(t, err, connect.CodeInvalidArgument)
	}
	names := []string{"removed", "offline", "pending", "no-row", "failed", "old", "absent", "removed", "missing"}
	type answer struct {
		res *connect.Response[pb.DeleteProjectResponse]
		err error
	}
	deleted := make(chan answer, 1)
	ctx := wait.Ctx(t)
	go func() {
		res, err := browser.projects.DeleteProject(ctx, connect.NewRequest(&pb.DeleteProjectRequest{ProjectId: p.ProjectId, RemoveWorkspacesOn: names}))
		deleted <- answer{res, err}
	}()
	asked := make(map[string]bool)
	for range 3 {
		got := wait.Recv(t, calls)
		if asked[got.device] || got.call.GetSessionId() != ready[got.device].SessionID() || got.user != h.owner.ID ||
			got.call.GetTimeoutMs() != 30000 || got.call.GetRemoveWorkspace().GetPath() != store.DefaultWorkspacePath(p.ProjectId) {
			t.Fatalf("removal submission = %+v", got)
		}
		asked[got.device] = true
	}
	if wait.Recv(t, cancelled) != active.CallID || wait.Recv(t, stopped) != active.SessionID() {
		t.Fatal("preparation was not cancelled")
	}
	close(release)
	got := wait.Recv(t, deleted)
	workspaceCheck(t, got.err)
	results := got.res.Msg.GetWorkspaceRemovals()
	if len(results) != len(names) {
		t.Fatalf("removals = %v", results)
	}
	for i, name := range names {
		r := results[i]
		want := pb.WorkspaceRemoval_OUTCOME_KEPT
		switch name {
		case "removed", "absent":
			want = pb.WorkspaceRemoval_OUTCOME_REMOVED
		case "failed":
			want = pb.WorkspaceRemoval_OUTCOME_FAILED
		}
		if r.GetDevice() != name || r.GetOutcome() != want || r.GetPath() != ready[name].Path {
			t.Fatalf("result %d = %v, want %s %v", i, r, name, want)
		}
		if name == "failed" && (r.GetError() != pb.WorkspaceError_WORKSPACE_ERROR_REFUSED || r.GetErrorDetail() != "refused") {
			t.Fatalf("failure result = %v", r)
		}
	}
	_, err := h.st.GetProject(ctx, h.owner.ID, p.ProjectId)
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("project was not deleted: %v", err)
	}
	rows, err := h.st.ListProjectWorkspaces(ctx, p.ProjectId, "", 10)
	workspaceCheck(t, err)
	if len(rows) != 0 {
		t.Fatalf("workspace rows survived deletion: %+v", rows)
	}
}

func TestDeleteProjectWorkspaceTimeout(t *testing.T) {
	clk := clock.New()
	submitted := make(chan *pb.DeviceCall, 1)
	finalDeadline := make(chan time.Time, 1)
	wrapped := &projectDeleteStore{
		beforeProjectDelete: func(ctx context.Context, _, _ string, _ int64) error {
			deadline, _ := ctx.Deadline()
			finalDeadline <- deadline
			return nil
		},
	}
	h, browser := newHarness(t, func(cfg *webapi.Config) {
		wrapped.Store, cfg.Store = cfg.Store, wrapped
		cfg.Workspaces = workspaces.New(workspaces.Config{
			Store: cfg.Store, Clock: clk,
			Supports: func(_, feature string) (bool, bool) { return true, feature == wire.FeatureRemoveWorkspace },
			Submit: func(_ context.Context, _, _ string, call *pb.DeviceCall) (workspaces.Subscription, error) {
				submitted <- call
				return removalSubscription{}, nil
			},
		})
	})
	ctx := wait.Ctx(t)
	h.device(t, "d1", "box", "linux")
	p := browser.mustCreate(t, "Project", loc("box", ""))
	w := h.markWorkspaceReady(t, h.prepareWorkspace(t, p.ProjectId, "d1"))
	type answer struct {
		res *connect.Response[pb.DeleteProjectResponse]
		err error
	}
	deleted := make(chan answer, 1)
	go func() {
		res, err := browser.projects.DeleteProject(ctx, connect.NewRequest(&pb.DeleteProjectRequest{
			ProjectId: p.ProjectId, RemoveWorkspacesOn: []string{"box"},
		}))
		deleted <- answer{res, err}
	}()
	if call := wait.Recv(t, submitted); call.GetTimeoutMs() != 30000 || call.GetSessionId() != w.SessionID() {
		t.Fatalf("remove call = %v", call)
	}
	_, err := browser.createSession(t, &pb.CreateSessionRequest{ProjectId: p.ProjectId, ClientRequestId: "during-delete"})
	wantCode(t, err, connect.CodeFailedPrecondition)
	clk.Advance(29*time.Second + 999*time.Millisecond)
	select {
	case res := <-deleted:
		t.Fatalf("delete finished before the timed-out call's deadline: %v", res)
	default:
	}
	// Only the device clock advances here. The final write must get a
	// fresh 30-second wall-clock deadline after the device wait has expired.
	earliestDeadline := time.Now().Add(30 * time.Second)
	clk.Advance(time.Millisecond)
	got := wait.Recv(t, deleted)
	latestDeadline := time.Now().Add(30 * time.Second)
	workspaceCheck(t, got.err)
	if deadline := wait.Recv(t, finalDeadline); deadline.Before(earliestDeadline) || deadline.After(latestDeadline) {
		t.Fatalf("final project write deadline = %v, want between %v and %v", deadline, earliestDeadline, latestDeadline)
	}
	results := got.res.Msg.GetWorkspaceRemovals()
	if len(results) != 1 || results[0].GetOutcome() != pb.WorkspaceRemoval_OUTCOME_KEPT || results[0].GetPath() != w.Path {
		t.Fatalf("timed-out removal = %v", results)
	}
	_, err = h.st.GetProject(ctx, h.owner.ID, p.ProjectId)
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("project survived the removal timeout: %v", err)
	}
}

func TestDeleteProjectWorkspaceClientDisconnect(t *testing.T) {
	requests := make(chan *pb.DeviceCall, 1)
	deviceReply := make(chan struct{})
	release := sync.OnceFunc(func() { close(deviceReply) })
	defer release()
	serverCanceled := make(chan struct{})
	h, browser := newHarness(t, func(cfg *webapi.Config) {
		auth := cfg.Interceptor
		cfg.Interceptor = connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
			handle := auth.WrapUnary(next)
			return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
				if req.Spec().Procedure == circusv1connect.ProjectServiceDeleteProjectProcedure {
					context.AfterFunc(ctx, func() { close(serverCanceled) })
				}
				return handle(ctx, req)
			}
		})
		cfg.Workspaces = workspaces.New(workspaces.Config{
			Store: cfg.Store, Clock: clock.New(),
			Supports: func(_, feature string) (bool, bool) { return true, feature == wire.FeatureRemoveWorkspace },
			Submit: func(_ context.Context, _, _ string, call *pb.DeviceCall) (workspaces.Subscription, error) {
				requests <- call
				return removalSubscription{release: deviceReply, update: &pb.CallUpdate{
					Update: &pb.CallUpdate_Result{Result: &pb.DeviceResult{
						Detail: &pb.DeviceResult_RemoveWorkspace{RemoveWorkspace: &pb.RemoveWorkspaceResult{Outcome: pb.RemoveWorkspaceResult_OUTCOME_REMOVED}},
					}},
				}}, nil
			},
		})
	})
	h.device(t, "d1", "box", "linux")
	p := browser.mustCreate(t, "Project", loc("box", ""))
	sess, err := browser.createSession(t, &pb.CreateSessionRequest{ProjectId: p.ProjectId, ClientRequestId: "before-delete"})
	workspaceCheck(t, err)
	_, err = browser.sessions.StopSession(t.Context(), connect.NewRequest(&pb.StopSessionRequest{SessionId: sess.SessionId}))
	workspaceCheck(t, err)
	w := h.markWorkspaceReady(t, h.prepareWorkspace(t, p.ProjectId, "d1"))

	ctx, cancel := context.WithCancel(wait.Ctx(t))
	defer cancel()
	client := make(chan error, 1)
	go func() {
		_, err := browser.projects.DeleteProject(ctx, connect.NewRequest(&pb.DeleteProjectRequest{
			ProjectId: p.ProjectId, RemoveWorkspacesOn: []string{"box"},
		}))
		client <- err
	}()
	if call := wait.Recv(t, requests); call.GetSessionId() != w.SessionID() || call.GetRemoveWorkspace() == nil {
		t.Fatalf("unexpected removal call: %v", call)
	}
	cancel()
	wantCode(t, wait.Recv(t, client), connect.CodeCanceled)
	wait.Recv(t, serverCanceled)
	release()

	wait.For(t, func() bool {
		_, err := h.st.GetProject(t.Context(), h.owner.ID, p.ProjectId)
		return errors.Is(err, store.ErrNotFound)
	})
	_, err = h.st.GetWorkspaceByID(t.Context(), w.ID)
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("ready workspace survived project deletion: %v", err)
	}
}
