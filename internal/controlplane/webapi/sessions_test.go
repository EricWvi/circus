package webapi_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"
	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/store"
)

// recordingWaker records Wake calls as "session:REASON,REASON".
type recordingWaker struct {
	mu    sync.Mutex
	calls []string
}

func (w *recordingWaker) Wake(id string, reasons ...circusv1.WorkerEvent_Nudge_Reason) {
	names := make([]string, len(reasons))
	for i, r := range reasons {
		names[i] = strings.TrimPrefix(r.String(), "REASON_")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls = append(w.calls, id+":"+strings.Join(names, ","))
}

// take returns the calls since the last take.
func (w *recordingWaker) take() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	calls := w.calls
	w.calls = nil
	return calls
}

// recordingJobs records StopSessionJobs calls as "user:session".
type recordingJobs struct {
	mu    sync.Mutex
	calls []string
	err   error
}

func (j *recordingJobs) StopSessionJobs(_ context.Context, userID, sessionID string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.calls = append(j.calls, userID+":"+sessionID)
	return j.err
}

func (j *recordingJobs) take() []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	calls := j.calls
	j.calls = nil
	return calls
}

func wantCalls(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("%s = %q, want %q", what, got, want)
	}
}

// sessionSetup gives the super user a device "box" (d1) and a project with
// the locations box:/work and box's default directory, and creates one
// session there through the API.
func sessionSetup(t *testing.T, h *harness, super *client) (*circusv1.Project, *circusv1.Session) {
	t.Helper()
	h.device(t, "d1", "box", "linux")
	p := super.mustCreate(t, "Project", loc("box", "/work"), loc("box", ""))
	return p, super.mustSession(t, p.ProjectId)
}

func (c *client) createSession(t *testing.T, req *circusv1.CreateSessionRequest) (*circusv1.Session, error) {
	t.Helper()
	resp, err := c.sessions.CreateSession(t.Context(), connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.Session, nil
}

func (c *client) mustSession(t *testing.T, projectID string) *circusv1.Session {
	t.Helper()
	s, err := c.createSession(t, &circusv1.CreateSessionRequest{ProjectId: projectID})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (c *client) send(t *testing.T, sessionID, clientMsgID, text string) (*circusv1.SendMessageResponse, error) {
	t.Helper()
	resp, err := c.sessions.SendMessage(t.Context(), connect.NewRequest(&circusv1.SendMessageRequest{SessionId: sessionID, ClientMsgId: clientMsgID, Text: text}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (c *client) getSession(t *testing.T, id string) *circusv1.Session {
	t.Helper()
	resp, err := c.sessions.GetSession(t.Context(), connect.NewRequest(&circusv1.GetSessionRequest{SessionId: id}))
	if err != nil {
		t.Fatal(err)
	}
	return resp.Msg
}

// lease gives the session's lease to a worker of the legacy owner and
// returns its fence.
func (h *harness) lease(t *testing.T, sessionID string) store.Fence {
	t.Helper()
	ctx := t.Context()
	now := h.clock.Load()
	if _, err := h.st.GetIdentity(ctx, store.KindWorker, "w1"); errors.Is(err, store.ErrNotFound) {
		if err := h.st.CreateIdentity(ctx, store.Identity{ID: "w1", Kind: store.KindWorker, UserID: h.owner.ID, Name: "worker", MachineID: "machine", CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	r, err := h.st.AcquireLease(ctx, store.LeaseRequest{SessionID: sessionID, WorkerID: "w1", BootID: "boot", ConnID: "conn", Now: now, ExpiresAt: now + 3_600_000, Fresh: true})
	if err != nil {
		t.Fatal(err)
	}
	return store.Fence{SessionID: sessionID, Epoch: r.Lease.Epoch, WorkerID: "w1", BootID: "boot", ConnID: "conn", Now: now}
}

// startTurn makes the session running with a worker's STARTED report.
func (h *harness) startTurn(t *testing.T, f store.Fence, localSeq int64) {
	t.Helper()
	if _, err := h.st.ReportTurn(t.Context(), f, store.TurnReport{LocalSeq: localSeq, Phase: store.TurnStarted, Origin: store.TurnFromInput, At: h.clock.Load()}); err != nil {
		t.Fatal(err)
	}
}

// requestApproval records a pending approval the way the worker link does.
func (h *harness) requestApproval(t *testing.T, f store.Fence, id string, expiresIn int64) {
	t.Helper()
	now := h.clock.Load()
	_, err := h.st.RequestApproval(t.Context(), f, store.Approval{
		ApprovalID: id, ToolUseID: "tu-" + id, Tool: "bash", DeviceID: "d1", MatchKey: strings.Repeat("a", 63) + id[len(id)-1:],
		Display: "rm -rf build", Reason: "outside the project", CreatedAt: now, ExpiresAt: now + expiresIn,
	})
	if err != nil {
		t.Fatal(err)
	}
}

// events reads the session's stored view events.
func (h *harness) events(t *testing.T, id string) []store.SessionEvent {
	t.Helper()
	page, err := h.st.ReadEvents(t.Context(), store.EventFilter{SessionID: id, Limit: store.MaxPageSize, MaxBytes: store.MaxTranscriptPageBytes})
	if err != nil {
		t.Fatal(err)
	}
	return page.Events
}

// foreignSession gives the Web user webUser a project with one session
// served by the legacy owner's devices, through the store: the same
// machine owner as the super user's sessions, another project owner.
func (h *harness) foreignSession(t *testing.T, webUser, projectID, sessionID string) {
	t.Helper()
	now := h.clock.Load()
	if _, err := h.st.CreateProject(t.Context(), store.Project{ID: projectID, UserID: webUser, Name: projectID, CreatedAt: now, UpdatedAt: now}, ""); err != nil {
		t.Fatal(err)
	}
	if err := h.st.CreateSession(t.Context(), store.Session{ID: sessionID, UserID: h.owner.ID, ProjectID: projectID, ClaudeSessionID: "claude-" + sessionID, DefaultDeviceID: "d1", CreatedAt: now, UpdatedAt: now, LastActivityAt: now}, nil, 100); err != nil {
		t.Fatal(err)
	}
}

// TestSessionAuthorization calls every RPC as another Web user on the super
// user's session (or its project or approval): each answers NOT_FOUND and
// changes nothing. Every call site of the ownership check has a case here
// that would succeed, or fail differently, without it.
func TestSessionAuthorization(t *testing.T) {
	h, super := newHarness(t)
	ctx := t.Context()
	p, sess := sessionSetup(t, h, super)
	id := sess.SessionId
	if _, err := super.send(t, id, "m1", "hello"); err != nil {
		t.Fatal(err)
	}
	fence := h.lease(t, id)
	h.startTurn(t, fence, 1)
	h.requestApproval(t, fence, "ap1", 600_000)
	before := super.getSession(t, id)
	eventsBefore := len(h.events(t, id))
	h.waker.take()
	h.jobs.take()

	bob := h.signIn(t, "bob", "bob@example.com")
	h.foreignSession(t, "bob", "bob-project", "bob-session")
	notFound := func(what string, err error) {
		t.Helper()
		if connect.CodeOf(err) != connect.CodeNotFound {
			t.Fatalf("%s: %v, want NOT_FOUND", what, err)
		}
	}
	_, err := bob.createSession(t, &circusv1.CreateSessionRequest{ProjectId: p.ProjectId})
	notFound("CreateSession", err)
	_, err = bob.send(t, id, "m2", "from bob")
	notFound("SendMessage", err)
	_, err = bob.sessions.GetSession(ctx, connect.NewRequest(&circusv1.GetSessionRequest{SessionId: id}))
	notFound("GetSession", err)
	_, err = bob.sessions.UpdateSession(ctx, connect.NewRequest(&circusv1.UpdateSessionRequest{SessionId: id, Title: "bob's"}))
	notFound("UpdateSession", err)
	_, err = bob.sessions.ListApprovals(ctx, connect.NewRequest(&circusv1.ListApprovalsRequest{SessionId: id}))
	notFound("ListApprovals", err)
	_, err = bob.sessions.ResolveApproval(ctx, connect.NewRequest(&circusv1.ResolveApprovalRequest{ApprovalId: "ap1", Approve: true}))
	notFound("ResolveApproval", err)
	_, err = bob.sessions.Interrupt(ctx, connect.NewRequest(&circusv1.InterruptRequest{SessionId: id}))
	notFound("Interrupt", err)
	_, err = bob.sessions.StopSession(ctx, connect.NewRequest(&circusv1.StopSessionRequest{SessionId: id}))
	notFound("StopSession", err)
	_, err = bob.sessions.DeleteSession(ctx, connect.NewRequest(&circusv1.DeleteSessionRequest{SessionId: id}))
	notFound("DeleteSession", err)
	_, err = bob.sessions.RecoverSession(ctx, connect.NewRequest(&circusv1.RecoverSessionRequest{SessionId: id, AcceptTranscriptGap: true}))
	notFound("RecoverSession", err)
	stream, err := bob.sessions.WatchSession(ctx, connect.NewRequest(&circusv1.WatchSessionRequest{SessionId: id}))
	if err == nil {
		if stream.Receive() {
			t.Fatalf("WatchSession sent %v to another user", stream.Msg())
		}
		err = stream.Err()
		stream.Close()
	}
	notFound("WatchSession", err)
	_, err = bob.sessions.ListSessions(ctx, connect.NewRequest(&circusv1.ListSessionsRequest{ProjectId: p.ProjectId}))
	notFound("ListSessions with the project", err)
	// Unfiltered lists hold only the caller's projects' sessions, although
	// both users' sessions run on the same machine owner's devices.
	list, err := bob.sessions.ListSessions(ctx, connect.NewRequest(&circusv1.ListSessionsRequest{}))
	if err != nil || len(list.Msg.Sessions) != 1 || list.Msg.Sessions[0].SessionId != "bob-session" {
		t.Fatalf("bob's list = %v, %v", list, err)
	}
	list, err = super.sessions.ListSessions(ctx, connect.NewRequest(&circusv1.ListSessionsRequest{}))
	if err != nil || len(list.Msg.Sessions) != 1 || list.Msg.Sessions[0].SessionId != id {
		t.Fatalf("super's list = %v, %v", list, err)
	}
	_, err = super.sessions.GetSession(ctx, connect.NewRequest(&circusv1.GetSessionRequest{SessionId: "bob-session"}))
	notFound("super reading bob's session", err)

	after := super.getSession(t, id)
	if after.State != before.State || after.Title != before.Title || after.LastActivityAtUnixMs != before.LastActivityAtUnixMs || after.PendingApprovalCount != 1 {
		t.Fatalf("session changed: %v, was %v", after, before)
	}
	if n := len(h.events(t, id)); n != eventsBefore {
		t.Fatalf("events: %d, was %d", n, eventsBefore)
	}
	s, err := h.st.GetSession(ctx, id, h.clock.Load())
	if err != nil || s.InterruptTurnNo != 0 {
		t.Fatalf("interrupt level = %v, %v", s.InterruptTurnNo, err)
	}
	wantCalls(t, "wakes", h.waker.take())
	wantCalls(t, "job stops", h.jobs.take())
	if h.bus.Watchers() != 0 {
		t.Fatalf("watchers = %d", h.bus.Watchers())
	}
}

func TestSessionUnauthenticated(t *testing.T) {
	h, _ := newHarness(t)
	anonymous := h.browser(t)
	_, err := anonymous.sessions.ListSessions(t.Context(), connect.NewRequest(&circusv1.ListSessionsRequest{}))
	wantCode(t, err, connect.CodeUnauthenticated)
	stream, err := anonymous.sessions.WatchSession(t.Context(), connect.NewRequest(&circusv1.WatchSessionRequest{SessionId: "x"}))
	if err == nil {
		stream.Receive()
		err = stream.Err()
	}
	wantCode(t, err, connect.CodeUnauthenticated)
}

func TestSessionIDArguments(t *testing.T) {
	_, super := newHarness(t)
	ctx := t.Context()
	long := strings.Repeat("x", store.MaxIDBytes+1)
	_, err := super.sessions.GetSession(ctx, connect.NewRequest(&circusv1.GetSessionRequest{}))
	wantCode(t, err, connect.CodeNotFound)
	_, err = super.sessions.GetSession(ctx, connect.NewRequest(&circusv1.GetSessionRequest{SessionId: long}))
	wantField(t, err, connect.CodeInvalidArgument, "session_id")
	_, err = super.sessions.GetSession(ctx, connect.NewRequest(&circusv1.GetSessionRequest{SessionId: "missing"}))
	wantCode(t, err, connect.CodeNotFound)
	_, err = super.sessions.ResolveApproval(ctx, connect.NewRequest(&circusv1.ResolveApprovalRequest{ApprovalId: long}))
	wantField(t, err, connect.CodeInvalidArgument, "approval_id")
	_, err = super.sessions.ResolveApproval(ctx, connect.NewRequest(&circusv1.ResolveApprovalRequest{ApprovalId: "missing"}))
	wantCode(t, err, connect.CodeNotFound)
	_, err = super.createSession(t, &circusv1.CreateSessionRequest{})
	wantCode(t, err, connect.CodeNotFound)
	_, err = super.createSession(t, &circusv1.CreateSessionRequest{ProjectId: "missing"})
	wantCode(t, err, connect.CodeNotFound)
}

func TestCreateSessionLocations(t *testing.T) {
	h, super := newHarness(t)
	ctx := t.Context()
	h.device(t, "d1", "box", "linux")
	h.device(t, "d2", "spare", "linux")
	p := super.mustCreate(t, "Project", loc("box", "/work"), loc("box", ""), loc("spare", "/srv"))

	first, err := super.createSession(t, &circusv1.CreateSessionRequest{ProjectId: p.ProjectId, Config: &circusv1.SessionConfig{Model: "opus", PermissionMode: "acceptEdits"}})
	if err != nil {
		t.Fatal(err)
	}
	cfg := first.Config
	if first.ProjectId != p.ProjectId || first.State != circusv1.Session_STATE_IDLE || cfg.DefaultDevice != "box" || cfg.Model != "opus" || cfg.PermissionMode != "acceptEdits" ||
		len(cfg.Cwds) != 1 || cfg.Cwds[0].Device != "box" || cfg.Cwds[0].Cwd != "/work" || first.LastActivityAtUnixMs != h.clock.Load() || first.Title != "" {
		t.Fatalf("first session = %v", first)
	}
	// A location without a path starts in the project's default directory,
	// and becomes the project's last used location without changing it.
	second, err := super.createSession(t, &circusv1.CreateSessionRequest{ProjectId: p.ProjectId, LocationIndex: new(uint32(1))})
	if err != nil {
		t.Fatal(err)
	}
	if got := second.Config.Cwds[0].Cwd; got != "~/circus/projects/"+p.ProjectId {
		t.Fatalf("default cwd = %q", got)
	}
	resp, err := super.projects.GetProject(ctx, connect.NewRequest(&circusv1.GetProjectRequest{ProjectId: p.ProjectId}))
	if err != nil || resp.Msg.Project.LastUsedLocation != 1 || resp.Msg.Project.UpdatedAtUnixMs != p.UpdatedAtUnixMs {
		t.Fatalf("project after create = %v, %v", resp, err)
	}
	third := super.mustSession(t, p.ProjectId)
	if third.Config.Cwds[0].Cwd != second.Config.Cwds[0].Cwd {
		t.Fatalf("last used location not used: %v", third.Config)
	}
	got := super.getSession(t, first.SessionId)
	if got.Config.DefaultDevice != "box" || got.Config.Cwds[0].Cwd != "/work" || got.Config.Model != "opus" {
		t.Fatalf("GetSession config = %v", got.Config)
	}

	// Arguments.
	_, err = super.createSession(t, &circusv1.CreateSessionRequest{ProjectId: p.ProjectId, LocationIndex: new(uint32(3))})
	wantField(t, err, connect.CodeInvalidArgument, "location_index")
	_, err = super.createSession(t, &circusv1.CreateSessionRequest{ProjectId: p.ProjectId, Config: &circusv1.SessionConfig{DefaultDevice: "box"}})
	wantField(t, err, connect.CodeInvalidArgument, "config.default_device")
	_, err = super.createSession(t, &circusv1.CreateSessionRequest{ProjectId: p.ProjectId, Config: &circusv1.SessionConfig{Cwds: []*circusv1.SessionConfig_Cwd{{Device: "box", Cwd: "/"}}}})
	wantField(t, err, connect.CodeInvalidArgument, "config.cwds")
	_, err = super.createSession(t, &circusv1.CreateSessionRequest{ProjectId: p.ProjectId, Config: &circusv1.SessionConfig{Model: strings.Repeat("m", 129)}})
	wantField(t, err, connect.CodeInvalidArgument, "config.model")
	_, err = super.createSession(t, &circusv1.CreateSessionRequest{ProjectId: p.ProjectId, Config: &circusv1.SessionConfig{PermissionMode: strings.Repeat("m", 129)}})
	wantField(t, err, connect.CodeInvalidArgument, "config.permission_mode")
	_, err = super.createSession(t, &circusv1.CreateSessionRequest{ProjectId: p.ProjectId, PinnedWorkerId: new("nobody")})
	wantField(t, err, connect.CodeInvalidArgument, "pinned_worker_id")
	_, err = super.createSession(t, &circusv1.CreateSessionRequest{ProjectId: p.ProjectId, PinnedWorkerId: new(strings.Repeat("w", 129))})
	wantField(t, err, connect.CodeInvalidArgument, "pinned_worker_id")
	if err := h.st.CreateIdentity(ctx, store.Identity{ID: "w1", Kind: store.KindWorker, UserID: h.owner.ID, Name: "worker", MachineID: "m", CreatedAt: h.clock.Load()}); err != nil {
		t.Fatal(err)
	}
	pinned, err := super.createSession(t, &circusv1.CreateSessionRequest{ProjectId: p.ProjectId, PinnedWorkerId: new("w1")})
	if err != nil || pinned.PinnedWorkerId != "w1" {
		t.Fatalf("pinned = %v, %v", pinned, err)
	}

	// A revoked device's location refuses new sessions.
	if _, err := h.st.DeleteIdentity(ctx, store.KindDevice, "d2", h.clock.Load()); err != nil {
		t.Fatal(err)
	}
	_, err = super.createSession(t, &circusv1.CreateSessionRequest{ProjectId: p.ProjectId, LocationIndex: new(uint32(2))})
	wantField(t, err, connect.CodeFailedPrecondition, "location_index")
	// A device taking the revoked one's name is another machine.
	h.device(t, "d3", "spare", "linux")
	_, err = super.createSession(t, &circusv1.CreateSessionRequest{ProjectId: p.ProjectId, LocationIndex: new(uint32(2))})
	wantField(t, err, connect.CodeFailedPrecondition, "location_index")

	// A project without locations.
	empty := super.mustCreate(t, "Empty")
	_, err = super.createSession(t, &circusv1.CreateSessionRequest{ProjectId: empty.ProjectId})
	wantField(t, err, connect.CodeFailedPrecondition, "location_index")

	// A Web user without devices.
	bob := h.signIn(t, "bob", "bob@example.com")
	own := bob.mustCreate(t, "Bob's")
	_, err = bob.createSession(t, &circusv1.CreateSessionRequest{ProjectId: own.ProjectId})
	wantField(t, err, connect.CodeFailedPrecondition, "location_index")
	// Even with a location bound to someone else's device in the store.
	if _, err := h.st.UpdateProject(ctx, "bob", own.ProjectId, store.ProjectPatch{Locations: []store.ProjectLocation{{DeviceID: "d1"}}, UpdatedAt: h.clock.Load() + 1}, h.owner.ID); err != nil {
		t.Fatal(err)
	}
	_, err = bob.createSession(t, &circusv1.CreateSessionRequest{ProjectId: own.ProjectId})
	wantField(t, err, connect.CodeFailedPrecondition, "location_index")

	// Every created session is the legacy machine owner's.
	for _, s := range []*circusv1.Session{first, second, third, pinned} {
		stored, err := h.st.GetSession(ctx, s.SessionId, h.clock.Load())
		if err != nil || stored.UserID != h.owner.ID || stored.DefaultDeviceID != "d1" || stored.ProjectID != p.ProjectId {
			t.Fatalf("stored %v, %v", stored, err)
		}
	}
	wantCalls(t, "wakes", h.waker.take())
}
