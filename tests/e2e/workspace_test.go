//go:build e2e && linux

package e2e

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/gen/go/circus/v1/circusv1connect"
	"github.com/EricWvi/circus/internal/credfile"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/testutil/fakeworker"
	testwait "github.com/EricWvi/circus/internal/testutil/wait"
	"github.com/EricWvi/circus/internal/wire"
	"github.com/coder/websocket"
	"github.com/google/uuid"
)

type workspaceDaemon struct {
	t    *testing.T
	link *wire.Link
}

func connectWorkspaceDaemon(t *testing.T, h *harness, boot string, features ...string) workspaceDaemon {
	t.Helper()
	ctx := testwait.Ctx(t)
	credential, err := credfile.Load(filepath.Join(h.dir, "config/circus/daemon.json"))
	must(t, err)
	conn, _, err := wire.DialWebSocket(ctx, "ws"+strings.TrimPrefix(h.base, "http")+"/v1/link/daemon",
		&websocket.DialOptions{HTTPHeader: http.Header{"Authorization": []string{"Bearer " + credential.Token}}})
	must(t, err)
	_, opts, err := wire.DialHello(ctx, conn, wire.DaemonUplink, &pb.Hello{
		ProtocolVersion: wire.ProtocolVersion, BootId: boot, Os: "linux", Features: features,
	}, credential.PrincipalID, wire.Options{})
	must(t, err)
	link, err := wire.New(conn, wire.DaemonSide, opts)
	must(t, err)
	t.Cleanup(func() { _ = link.Close() })
	return workspaceDaemon{t, link}
}

func (d workspaceDaemon) receive(ctx context.Context) *pb.DaemonDownlink {
	d.t.Helper()
	m, err := d.link.Receive(ctx)
	must(d.t, err)
	return m.(*pb.DaemonDownlink)
}

func (d workspaceDaemon) snapshot(ctx context.Context) {
	d.t.Helper()
	must(d.t, d.link.Send(ctx, &pb.DaemonUplink{Payload: &pb.DaemonUplink_JobSnapshot{JobSnapshot: &pb.JobSnapshot{}}}))
}

func (d workspaceDaemon) dispatch(ctx context.Context) *pb.DeviceCall {
	d.t.Helper()
	for {
		m := d.receive(ctx)
		if m.GetListJobs() != nil {
			d.snapshot(ctx)
			continue
		}
		if d.ledger(ctx, m) {
			continue
		}
		if c := m.GetDispatchCall().GetCall(); c != nil {
			return c
		}
		d.t.Fatalf("expected dispatch, got %v", m)
	}
}

// ledger answers m when it concerns the session's ledger snapshot, which
// every daemon keeps and the test does not look at (ledgerReply).
func (d workspaceDaemon) ledger(ctx context.Context, m *pb.DaemonDownlink) bool {
	d.t.Helper()
	up := ledgerReply(m)
	if up != nil {
		must(d.t, d.link.Send(ctx, up))
	}
	return up != nil
}

// ledgerReply is the answer to a message about the session's ledger
// snapshot: a call answered with ledgerAnswer, a query of a call this
// daemon never ran answered as lost (the control plane sends it again);
// nil for any other message.
func ledgerReply(m *pb.DaemonDownlink) *pb.DaemonUplink {
	if c := m.GetDispatchCall().GetCall(); c != nil {
		if r := ledgerAnswer(c); r != nil {
			return &pb.DaemonUplink{Payload: &pb.DaemonUplink_Result{Result: r}}
		}
	}
	if q := m.GetCallQuery(); q != nil && strings.HasPrefix(q.GetCallId(), store.LedgerSnapshotCallPrefix) {
		return &pb.DaemonUplink{Payload: &pb.DaemonUplink_CallStatus{CallStatus: &pb.CallStatus{CallId: q.GetCallId(), State: pb.CallStatus_STATE_LOST}}}
	}
	return nil
}

func (d workspaceDaemon) result(ctx context.Context, result *pb.DeviceResult) {
	d.t.Helper()
	must(d.t, d.link.Send(ctx, &pb.DaemonUplink{Payload: &pb.DaemonUplink_Result{Result: result}}))
}

func workspaceResult(callID, path string, action pb.PrepareWorkspaceResult_Action) *pb.DeviceResult {
	return &pb.DeviceResult{CallId: callID, Output: &pb.ToolOutput{},
		Detail: &pb.DeviceResult_PrepareWorkspace{PrepareWorkspace: &pb.PrepareWorkspaceResult{
			Outcome: pb.PrepareWorkspaceResult_OUTCOME_READY, Action: action, Path: path,
		}},
	}
}

func workspaceProject(t *testing.T, ctx context.Context, projects circusv1connect.ProjectServiceClient, repo string) string {
	t.Helper()
	p, err := projects.CreateProject(ctx, connect.NewRequest(&pb.CreateProjectRequest{
		Name: "Workspace", RepoUrl: repo, Locations: []*pb.Project_Location{{Device: "desk"}},
	}))
	must(t, err)
	return p.Msg.GetProject().GetProjectId()
}

func workspaceSession(t *testing.T, ctx context.Context, api circusv1connect.SessionServiceClient, project string) *pb.Session {
	t.Helper()
	out, err := api.CreateSession(ctx, connect.NewRequest(&pb.CreateSessionRequest{
		ProjectId: project, ClientRequestId: uuid.NewString(),
	}))
	must(t, err)
	return out.Msg.GetSession()
}

func waitWorkspace(t *testing.T, s *sessionBrowser, state pb.WorkspaceStatus_State, waiting pb.WorkspaceStatus_Waiting) *pb.Session {
	t.Helper()
	var got *pb.Session
	testwait.For(t, func() bool {
		res, err := s.api.GetSession(s.ctx, connect.NewRequest(&pb.GetSessionRequest{SessionId: s.id}))
		must(t, err)
		got = res.Msg
		return got.GetWorkspace().GetState() == state && got.GetWorkspace().GetWaiting() == waiting
	})
	return got
}

func TestWorkspacePreparationWithFakeDaemon(t *testing.T) {
	h := newHarnessWithDaemon(t, false)
	projects, api := browserClients(t, h)
	ctx := testwait.Ctx(t)
	const token = "workspace-fake-token"
	repoURL := "https://user:" + token + "@example.com/repo"
	project := workspaceProject(t, ctx, projects, "https://example.com/repo")
	// The project API rejects passwords in URLs. Seed a stored URL to
	// exercise the workspace snapshots' redaction defensively.
	_, err := h.st.UpdateProject(ctx, h.user(), project, store.ProjectPatch{RepoURL: &repoURL, UpdatedAt: time.Now().UnixMilli()})
	must(t, err)
	created := workspaceSession(t, ctx, api, project)
	s := &sessionBrowser{t: t, ctx: ctx, api: api, id: created.GetSessionId()}
	waitWorkspace(t, s, pb.WorkspaceStatus_STATE_PENDING, pb.WorkspaceStatus_WAITING_DEVICE_OFFLINE)
	s.send("/fake bash git log -1 --oneline", pb.InputPriority_INPUT_PRIORITY_NORMAL)

	workerCtx, stopWorker := context.WithCancel(t.Context())
	workerDone := make(chan error, 1)
	go func() {
		workerDone <- fakeworker.Run(workerCtx, fakeworker.Options{
			Credential: h.worker, MaxSessions: 1, Logger: slog.New(slog.DiscardHandler),
		})
	}()
	t.Cleanup(func() {
		stopWorker()
		if err := testwait.Recv(t, workerDone); !errors.Is(err, context.Canceled) {
			t.Errorf("scripted worker: %v", err)
		}
	})
	old := connectWorkspaceDaemon(t, h, uuid.NewString())
	if old.receive(ctx).GetListJobs() == nil {
		t.Fatal("old daemon was dispatched a workspace call")
	}
	old.snapshot(ctx)
	queued := waitWorkspace(t, s, pb.WorkspaceStatus_STATE_PENDING, pb.WorkspaceStatus_WAITING_DAEMON_OUTDATED)
	if queued.GetState() != pb.Session_STATE_QUEUED || queued.GetTurnCount() != 0 {
		t.Fatalf("session before preparation = %v", queued)
	}
	must(t, old.link.Close())
	device := connectWorkspaceDaemon(t, h, uuid.NewString(), wire.FeaturePrepareWorkspace, wire.FeatureRemoveWorkspace)
	first := device.dispatch(ctx)
	if first.GetPrepareWorkspace().GetRepoUrl() != repoURL || first.GetPrepareWorkspace().GetPath() != store.DefaultWorkspacePath(project) ||
		first.GetTimeoutMs() != 120000 || !strings.HasPrefix(first.GetSessionId(), "workspace-") {
		t.Fatalf("prepare call = %v", first)
	}
	device.result(ctx, &pb.DeviceResult{CallId: first.GetCallId(), Output: &pb.ToolOutput{IsError: true},
		Detail: &pb.DeviceResult_PrepareWorkspace{PrepareWorkspace: &pb.PrepareWorkspaceResult{
			Outcome: pb.PrepareWorkspaceResult_OUTCOME_FAILED, Error: pb.WorkspaceError_WORKSPACE_ERROR_NOT_EMPTY,
		}},
	})
	failed := waitWorkspace(t, s, pb.WorkspaceStatus_STATE_FAILED, 0)
	if failed.GetWorkspace().GetError() != pb.WorkspaceError_WORKSPACE_ERROR_NOT_EMPTY || failed.GetTurnCount() != 0 {
		t.Fatalf("failed workspace session = %v", failed)
	}
	_, err = api.RetryWorkspace(ctx, connect.NewRequest(&pb.RetryWorkspaceRequest{SessionId: s.id}))
	must(t, err)
	retry := device.dispatch(ctx)
	if retry.GetSessionId() != first.GetSessionId() || retry.GetCallId() == first.GetCallId() {
		t.Fatalf("retry call = %v", retry)
	}
	actualPath := filepath.Join(h.dir, "circus", "projects", project)
	job := &pb.JobEvent{JobId: "abcd01234567", SessionId: retry.GetSessionId(), CallId: retry.GetCallId(),
		State: pb.JobEvent_STATE_RUNNING, Seq: 1}
	must(t, device.link.Send(ctx, &pb.DaemonUplink{Payload: &pb.DaemonUplink_JobEvent{JobEvent: job}}))
	device.result(ctx, &pb.DeviceResult{CallId: retry.GetCallId(), Output: &pb.ToolOutput{},
		Detail: &pb.DeviceResult_PrepareWorkspace{PrepareWorkspace: &pb.PrepareWorkspaceResult{
			Outcome: pb.PrepareWorkspaceResult_OUTCOME_IN_BACKGROUND, JobId: job.GetJobId(),
		}},
	})
	testwait.For(t, func() bool {
		res, err := api.GetSession(ctx, connect.NewRequest(&pb.GetSessionRequest{SessionId: s.id}))
		must(t, err)
		return res.Msg.GetWorkspace().GetBackground()
	})
	must(t, device.link.Send(ctx, &pb.DaemonUplink{Payload: &pb.DaemonUplink_JobEvent{JobEvent: &pb.JobEvent{
		JobId: job.GetJobId(), SessionId: job.GetSessionId(), CallId: job.GetCallId(), State: pb.JobEvent_STATE_EXITED, Seq: 2,
	}}}))
	follow := device.dispatch(ctx)
	if follow.GetPrepareWorkspace().GetResumeJobId() != job.GetJobId() || follow.GetCallId() == retry.GetCallId() {
		t.Fatalf("resume call = %v", follow)
	}
	device.result(ctx, workspaceResult(follow.GetCallId(), actualPath, pb.PrepareWorkspaceResult_ACTION_CLONED))
	bash := device.dispatch(ctx)
	if b := bash.GetBash(); b.GetCommand() != "git log -1 --oneline" || b.GetCwd() != actualPath || b.GetWorkingDirs()[0] != actualPath {
		t.Fatalf("scripted worker call = %v", bash)
	}
	device.result(ctx, &pb.DeviceResult{CallId: bash.GetCallId(), Output: &pb.ToolOutput{
		Content: []*pb.ContentBlock{{Block: &pb.ContentBlock_Text{Text: "abc123 fixture commit"}}},
	}, Detail: &pb.DeviceResult_Bash{Bash: &pb.BashResult{Cwd: actualPath}}})
	s.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_COMPLETED)
	second := workspaceSession(t, ctx, api, project)
	if second.GetWorkspace().GetState() != pb.WorkspaceStatus_STATE_READY ||
		second.GetConfig().GetCwds()[0].GetCwd() != actualPath || strings.Contains(second.GetWorkspace().GetRepoUrl(), token) {
		t.Fatalf("second session = %v", second)
	}
	if w, err := h.st.GetWorkspaceByID(ctx, strings.TrimPrefix(retry.GetSessionId(), "workspace-")); err != nil || w.Attempt != 2 {
		t.Fatalf("second session changed workspace attempt: %+v, %v", w, err)
	}
	watch := s.watch(0)
	for {
		ev := watch.next()
		if strings.Contains(ev.GetPayloadJson(), token) {
			t.Fatal("workspace credentials leaked in a session event")
		}
		if ev.GetKind() == pb.SessionEvent_KIND_WORKSPACE && strings.Contains(ev.GetPayloadJson(), `"state":"ready"`) {
			break
		}
	}
	must(t, watch.stream.Close())
	if strings.Contains(h.server.stderr.String(), token) {
		t.Fatal("workspace credentials leaked in the control plane log")
	}
	worker := h.client()
	untrusted := h.result(h.submit(worker, &pb.DeviceCall{
		CallId: "ws-untrusted", SessionId: retry.GetSessionId(), TimeoutMs: 30000,
		Tool: &pb.DeviceCall_RemoveWorkspace{RemoveWorkspace: &pb.RemoveWorkspaceRequest{Path: store.DefaultWorkspacePath(project)}},
	}))
	if untrusted.GetRejected().GetCode() != pb.CallUpdate_Rejected_CODE_INVALID {
		t.Fatalf("worker removal = %v", untrusted)
	}
	for _, id := range []string{s.id, second.GetSessionId()} {
		_, err := api.StopSession(ctx, connect.NewRequest(&pb.StopSessionRequest{SessionId: id}))
		must(t, err)
		testwait.For(t, func() bool {
			err := h.st.CheckSessionDeletable(ctx, id, time.Now().UnixMilli())
			if errors.Is(err, store.ErrSessionActive) {
				return false
			}
			must(t, err)
			return true
		})
	}

	deleted := make(chan error, 1)
	go func() {
		res, err := projects.DeleteProject(ctx, connect.NewRequest(&pb.DeleteProjectRequest{
			ProjectId: project, RemoveWorkspacesOn: []string{"desk"},
		}))
		if err == nil && (len(res.Msg.GetWorkspaceRemovals()) != 1 ||
			res.Msg.GetWorkspaceRemovals()[0].GetOutcome() != pb.WorkspaceRemoval_OUTCOME_REMOVED) {
			err = errors.New("project deletion did not report the removed workspace")
		}
		deleted <- err
	}()
	remove := device.dispatch(ctx)
	if remove.GetSessionId() != retry.GetSessionId() || remove.GetRemoveWorkspace().GetPath() != store.DefaultWorkspacePath(project) ||
		remove.GetTimeoutMs() != 30000 {
		t.Fatalf("remove call = %v", remove)
	}
	device.result(ctx, &pb.DeviceResult{CallId: remove.GetCallId(), Output: &pb.ToolOutput{},
		Detail: &pb.DeviceResult_RemoveWorkspace{RemoveWorkspace: &pb.RemoveWorkspaceResult{
			Outcome: pb.RemoveWorkspaceResult_OUTCOME_REMOVED, Path: actualPath,
		}},
	})
	must(t, testwait.Recv(t, deleted))
}

func TestWorkspaceReattachAfterControlPlaneRestart(t *testing.T) {
	h := newHarnessWithDaemon(t, false)
	projects, api := browserClients(t, h)
	ctx := testwait.Ctx(t)
	project := workspaceProject(t, ctx, projects, "")
	created := workspaceSession(t, ctx, api, project)
	boot := uuid.NewString()
	device := connectWorkspaceDaemon(t, h, boot, wire.FeaturePrepareWorkspace)
	call := device.dispatch(ctx)
	status := &pb.CallStatus{CallId: call.GetCallId(), State: pb.CallStatus_STATE_RUNNING}
	must(t, device.link.Send(ctx, &pb.DaemonUplink{Payload: &pb.DaemonUplink_CallAck{
		CallAck: &pb.CallAck{CallId: call.GetCallId(), Outcome: &pb.CallAck_Status{Status: status}},
	}}))
	h.state(call.GetCallId(), store.CallRunning)
	h.server.signal(t, syscall.SIGTERM)
	if code := h.waitCode(h.server, testwait.Timeout); code != 0 {
		t.Fatalf("server shutdown exited %d", code)
	}
	h.startServer(false)
	ctx = testwait.Ctx(t)
	device = connectWorkspaceDaemon(t, h, boot, wire.FeaturePrepareWorkspace)
	path := filepath.Join(h.dir, "circus", "projects", project)
	for {
		m := device.receive(ctx)
		if m.GetListJobs() != nil {
			device.snapshot(ctx)
			break
		}
		if device.ledger(ctx, m) {
			continue
		}
		query := m.GetCallQuery()
		if query.GetCallId() != call.GetCallId() {
			t.Fatalf("restart sent %v", m)
		}
		must(t, device.link.Send(ctx, &pb.DaemonUplink{Payload: &pb.DaemonUplink_CallStatus{
			CallStatus: &pb.CallStatus{CallId: query.GetCallId(), State: pb.CallStatus_STATE_COMPLETED,
				Result: workspaceResult(query.GetCallId(), path, pb.PrepareWorkspaceResult_ACTION_CREATED)},
		}}))
	}
	// Recovery of the completed call may query its retained result once
	// more if its earlier sync reply came before the runner attached.
	seen := make(chan error, 1)
	readCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		for {
			msg, err := device.link.Receive(readCtx)
			if err != nil {
				seen <- err
				return
			}
			m := msg.(*pb.DaemonDownlink)
			if up := ledgerReply(m); up != nil {
				if err := device.link.Send(readCtx, up); err != nil {
					seen <- err
					return
				}
				continue
			}
			q := m.GetCallQuery()
			if q.GetCallId() != call.GetCallId() {
				seen <- errors.New("recovery did not query the original call")
				return
			}
			if err := device.link.Send(readCtx, &pb.DaemonUplink{Payload: &pb.DaemonUplink_CallStatus{
				CallStatus: &pb.CallStatus{CallId: q.GetCallId(), State: pb.CallStatus_STATE_COMPLETED,
					Result: workspaceResult(q.GetCallId(), path, pb.PrepareWorkspaceResult_ACTION_CREATED)},
			}}); err != nil {
				seen <- err
				return
			}
		}
	}()
	s := &sessionBrowser{t: t, ctx: ctx, api: api, id: created.GetSessionId()}
	out := waitWorkspace(t, s, pb.WorkspaceStatus_STATE_READY, 0)
	cancel()
	if err := testwait.Recv(t, seen); !errors.Is(err, context.Canceled) {
		t.Fatalf("retained-result recovery: %v", err)
	}
	if out.GetConfig().GetCwds()[0].GetCwd() != path {
		t.Fatalf("recovered session = %v", out)
	}
}

func TestWorkspaceRetryAfterDaemonRestart(t *testing.T) {
	h := newHarnessWithDaemon(t, false)
	projects, api := browserClients(t, h)
	ctx := testwait.Ctx(t)
	project := workspaceProject(t, ctx, projects, "https://example.com/repo")
	created := workspaceSession(t, ctx, api, project)
	firstDevice := connectWorkspaceDaemon(t, h, uuid.NewString(), wire.FeaturePrepareWorkspace)
	first := firstDevice.dispatch(ctx)
	must(t, firstDevice.link.Close())

	secondDevice := connectWorkspaceDaemon(t, h, uuid.NewString(), wire.FeaturePrepareWorkspace)
	retried := secondDevice.dispatch(ctx)
	if retried.GetCallId() == first.GetCallId() || retried.GetSessionId() != first.GetSessionId() ||
		retried.GetPrepareWorkspace().GetPath() != first.GetPrepareWorkspace().GetPath() {
		t.Fatalf("call after a new boot = %v", retried)
	}
	path := filepath.Join(h.dir, "circus", "projects", project)
	secondDevice.result(ctx, workspaceResult(retried.GetCallId(), path, pb.PrepareWorkspaceResult_ACTION_USED_REPOSITORY))
	s := &sessionBrowser{t: t, ctx: ctx, api: api, id: created.GetSessionId()}
	out := waitWorkspace(t, s, pb.WorkspaceStatus_STATE_READY, 0)
	if out.GetWorkspace().GetAction() != pb.PrepareWorkspaceResult_ACTION_USED_REPOSITORY || out.GetWorkspace().GetPath() != path {
		t.Fatalf("recovered workspace = %v", out.GetWorkspace())
	}
}
