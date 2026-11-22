package webapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/gen/go/circus/v1/circusv1connect"
	"github.com/EricWvi/circus/internal/controlplane/webapi"
	"github.com/EricWvi/circus/internal/store"
	"google.golang.org/protobuf/proto"
)

func TestSendMessage(t *testing.T) {
	h, super := newHarness(t)
	ctx := t.Context()
	_, sess := sessionSetup(t, h, super)
	id := sess.SessionId

	first, err := super.send(t, id, "m1", "  Fix the build\nthen more")
	if err != nil {
		t.Fatal(err)
	}
	if first.InputId == "" || first.EventSeq != 1 {
		t.Fatalf("first = %v", first)
	}
	wantCalls(t, "wakes", h.waker.take(), id+":INPUT")
	got := super.getSession(t, id)
	if got.Title != "Fix the build" || got.State != circusv1.Session_STATE_QUEUED || got.LastActivityAtUnixMs != h.clock.Load() {
		t.Fatalf("after send = %v", got)
	}
	events := h.events(t, id)
	var payload map[string]string
	if len(events) != 1 || events[0].Kind != "INPUT_ACCEPTED" || json.Unmarshal(events[0].Payload, &payload) != nil {
		t.Fatalf("events = %v", events)
	}
	if payload["input_id"] != first.InputId || payload["client_msg_id"] != "m1" || payload["priority"] != "normal" || payload["text"] != "  Fix the build\nthen more" {
		t.Fatalf("payload = %v", payload)
	}

	// A repeat answers the same input and event, records nothing, wakes nobody.
	h.clock.Add(1000)
	again, err := super.send(t, id, "m1", "  Fix the build\nthen more")
	if err != nil || again.InputId != first.InputId || again.EventSeq != first.EventSeq {
		t.Fatalf("repeat = %v, %v", again, err)
	}
	if len(h.events(t, id)) != 1 || super.getSession(t, id).LastActivityAtUnixMs != h.clock.Load()-1000 {
		t.Fatal("repeat recorded something")
	}
	wantCalls(t, "wakes after repeat", h.waker.take())
	_, err = super.send(t, id, "m1", "other text")
	wantField(t, err, connect.CodeAlreadyExists, "client_msg_id")

	// Arguments.
	_, err = super.send(t, id, "", "x")
	wantField(t, err, connect.CodeInvalidArgument, "client_msg_id")
	_, err = super.send(t, id, strings.Repeat("c", 129), "x")
	wantField(t, err, connect.CodeInvalidArgument, "client_msg_id")
	_, err = super.send(t, id, "m9", " \n\t")
	wantField(t, err, connect.CodeInvalidArgument, "text")
	_, err = super.send(t, id, "m9", strings.Repeat("x", store.MaxInputTextBytes+1))
	wantField(t, err, connect.CodeInvalidArgument, "text")
	_, err = super.sessions.SendMessage(ctx, connect.NewRequest(&circusv1.SendMessageRequest{SessionId: id, ClientMsgId: "m9", Text: "x", Priority: 99}))
	wantField(t, err, connect.CodeInvalidArgument, "priority")
	if _, err := super.send(t, id, "big", viewCapText(t, "big", "x", 0)); err != nil {
		t.Fatalf("largest message: %v", err)
	}
	h.waker.take()

	// NOW interrupts a running turn; NORMAL merges into it.
	fence := h.lease(t, id)
	h.startTurn(t, fence, 1)
	now := func(msg string, priority circusv1.InputPriority) {
		t.Helper()
		if _, err := super.sessions.SendMessage(ctx, connect.NewRequest(&circusv1.SendMessageRequest{SessionId: id, ClientMsgId: msg, Text: msg, Priority: priority})); err != nil {
			t.Fatal(err)
		}
	}
	now("n1", circusv1.InputPriority_INPUT_PRIORITY_NOW)
	now("n2", circusv1.InputPriority_INPUT_PRIORITY_NORMAL)
	wantCalls(t, "running wakes", h.waker.take(), id+":INPUT,INTERRUPT", id+":INPUT")
	if s, _ := h.st.GetSession(ctx, id, h.clock.Load()); s.InterruptTurnNo != 0 {
		t.Fatalf("NOW raised the interrupt level: %d", s.InterruptTurnNo)
	}

	// At most MaxOpenInputs wait.
	for i := 4; i < store.MaxOpenInputs; i++ {
		if _, err := super.send(t, id, fmt.Sprint("q", i), "queued"); err != nil {
			t.Fatal(err)
		}
	}
	_, err = super.send(t, id, "over", "one too many")
	wantCode(t, err, connect.CodeResourceExhausted)

	// A stopped session refuses new messages but still answers repeats.
	if _, err := super.sessions.StopSession(ctx, connect.NewRequest(&circusv1.StopSessionRequest{SessionId: id})); err != nil {
		t.Fatal(err)
	}
	h.waker.take()
	_, err = super.send(t, id, "late", "after stop")
	wantCode(t, err, connect.CodeFailedPrecondition)
	again, err = super.send(t, id, "m1", "  Fix the build\nthen more")
	if err != nil || again.InputId != first.InputId || again.EventSeq != 1 {
		t.Fatalf("repeat after stop = %v, %v", again, err)
	}
	wantCalls(t, "wakes after stop", h.waker.take())
}

func TestSendMessageFullView(t *testing.T) {
	h, super := newHarness(t)
	_, sess := sessionSetup(t, h, super)
	// A view that has filled stores only boundaries from then on.
	if _, err := h.st.AppendEvents(t.Context(), sess.SessionId, nil, []store.EventInput{{Kind: "ASSISTANT", Payload: []byte(`"x"`), At: h.clock.Load()}}, 1); err != nil {
		t.Fatal(err)
	}
	resp, err := super.send(t, sess.SessionId, "m1", "hello")
	if err != nil || resp.EventSeq != 0 {
		t.Fatalf("send to a full view = %v, %v", resp, err)
	}
	again, err := super.send(t, sess.SessionId, "m1", "hello")
	if err != nil || again.EventSeq != 0 || again.InputId != resp.InputId {
		t.Fatalf("repeat = %v, %v", again, err)
	}
	wantCalls(t, "wakes", h.waker.take(), sess.SessionId+":INPUT")
}

func TestListSessions(t *testing.T) {
	h, super := newHarness(t)
	ctx := t.Context()
	p, _ := sessionSetup(t, h, super)
	other := super.mustCreate(t, "Other", loc("box", "/other"))
	for range 4 {
		h.clock.Add(1)
		super.mustSession(t, p.ProjectId)
	}
	h.clock.Add(1)
	inOther := super.mustSession(t, other.ProjectId)
	list := func(req *circusv1.ListSessionsRequest) *circusv1.ListSessionsResponse {
		t.Helper()
		resp, err := super.sessions.ListSessions(ctx, connect.NewRequest(req))
		if err != nil {
			t.Fatal(err)
		}
		return resp.Msg
	}
	all := list(&circusv1.ListSessionsRequest{})
	if len(all.Sessions) != 6 || all.NextPageToken != "" || all.Sessions[0].SessionId != inOther.SessionId {
		t.Fatalf("all = %v", all)
	}
	for _, s := range all.Sessions {
		if s.Config != nil {
			t.Fatalf("list carries config: %v", s)
		}
	}
	// Pages of two walk the same order.
	var walked []string
	req := &circusv1.ListSessionsRequest{PageSize: 2}
	for {
		page := list(req)
		for _, s := range page.Sessions {
			walked = append(walked, s.SessionId)
		}
		if page.NextPageToken == "" {
			break
		}
		req.PageToken = page.NextPageToken
	}
	var want []string
	for _, s := range all.Sessions {
		want = append(want, s.SessionId)
	}
	if strings.Join(walked, ",") != strings.Join(want, ",") {
		t.Fatalf("walked %v, want %v", walked, want)
	}
	byProject := list(&circusv1.ListSessionsRequest{ProjectId: other.ProjectId})
	if len(byProject.Sessions) != 1 || byProject.Sessions[0].SessionId != inOther.SessionId {
		t.Fatalf("by project = %v", byProject)
	}
	_, err := super.sessions.ListSessions(ctx, connect.NewRequest(&circusv1.ListSessionsRequest{PageSize: 1001}))
	wantField(t, err, connect.CodeInvalidArgument, "page_size")
	for _, token := range []string{"!!", "bm8tc2VwYXJhdG9y", strings.Repeat("A", 400)} {
		_, err = super.sessions.ListSessions(ctx, connect.NewRequest(&circusv1.ListSessionsRequest{PageToken: token}))
		wantField(t, err, connect.CodeInvalidArgument, "page_token")
	}
	_, err = super.sessions.ListSessions(ctx, connect.NewRequest(&circusv1.ListSessionsRequest{ProjectId: strings.Repeat("p", 129)}))
	wantField(t, err, connect.CodeInvalidArgument, "project_id")
	_, err = super.sessions.ListSessions(ctx, connect.NewRequest(&circusv1.ListSessionsRequest{ProjectId: "missing"}))
	wantCode(t, err, connect.CodeNotFound)
}

// TestListSessionsAcrossStorePages crosses the store's page size within
// one response.
func TestListSessionsAcrossStorePages(t *testing.T) {
	h, super := newHarness(t)
	p, _ := sessionSetup(t, h, super)
	bob := "bob"
	h.signIn(t, bob, "bob@example.com")
	h.foreignSession(t, bob, "bob-project", "bob-session")
	for i := range store.MaxPageSize + 10 {
		now := h.clock.Load() + int64(i)
		id := fmt.Sprintf("s%03d", i)
		if _, err := h.st.CreateSession(t.Context(), store.Session{ID: id, ClientRequestID: id, UserID: h.owner.ID, ProjectID: p.ProjectId, ClaudeSessionID: fmt.Sprint("c", i), DefaultDeviceID: "d1", CreatedAt: now, UpdatedAt: now, LastActivityAt: now}, nil, 1000); err != nil {
			t.Fatal(err)
		}
	}
	resp, err := super.sessions.ListSessions(t.Context(), connect.NewRequest(&circusv1.ListSessionsRequest{PageSize: 1000}))
	if err != nil || len(resp.Msg.Sessions) != store.MaxPageSize+11 || resp.Msg.NextPageToken != "" {
		t.Fatalf("one page: %d sessions, token %q, %v", len(resp.Msg.GetSessions()), resp.Msg.GetNextPageToken(), err)
	}
	resp, err = super.sessions.ListSessions(t.Context(), connect.NewRequest(&circusv1.ListSessionsRequest{PageSize: store.MaxPageSize + 1}))
	if err != nil || len(resp.Msg.Sessions) != store.MaxPageSize+1 || resp.Msg.NextPageToken == "" {
		t.Fatalf("first page: %d, %v", len(resp.Msg.GetSessions()), err)
	}
	resp, err = super.sessions.ListSessions(t.Context(), connect.NewRequest(&circusv1.ListSessionsRequest{PageSize: 1000, PageToken: resp.Msg.NextPageToken}))
	if err != nil || len(resp.Msg.Sessions) != 10 || resp.Msg.NextPageToken != "" {
		t.Fatalf("rest: %d, %v", len(resp.Msg.GetSessions()), err)
	}
	for _, s := range resp.Msg.Sessions {
		if s.SessionId == "bob-session" {
			t.Fatal("foreign session listed")
		}
	}
}

func TestUpdateSessionTitle(t *testing.T) {
	h, super := newHarness(t)
	ctx := t.Context()
	_, sess := sessionSetup(t, h, super)
	update := func(title string) (*circusv1.Session, error) {
		resp, err := super.sessions.UpdateSession(ctx, connect.NewRequest(&circusv1.UpdateSessionRequest{SessionId: sess.SessionId, Title: proto.String(title)}))
		if err != nil {
			return nil, err
		}
		return resp.Msg, nil
	}
	h.clock.Add(5000)
	got, err := update("  Release notes 　")
	if err != nil || got.Title != "Release notes" || got.Config.DefaultDevice != "box" || got.LastActivityAtUnixMs != sess.LastActivityAtUnixMs {
		t.Fatalf("update = %v, %v", got, err)
	}
	if got, err := update(strings.Repeat("界", 200)); err != nil || got.Title != strings.Repeat("界", 200) {
		t.Fatalf("200 characters = %v, %v", got, err)
	}
	for _, bad := range []string{"", " \t\n", strings.Repeat("a", 201), "a​b", "line\nbreak", "‮evil"} {
		_, err := update(bad)
		wantField(t, err, connect.CodeInvalidArgument, "title")
	}
	// An explicit title keeps the first message from naming the session.
	if _, err := super.send(t, sess.SessionId, "m1", "Derived"); err != nil {
		t.Fatal(err)
	}
	if got := super.getSession(t, sess.SessionId); got.Title != strings.Repeat("界", 200) {
		t.Fatalf("title = %q", got.Title)
	}
}

func TestApprovals(t *testing.T) {
	h, super := newHarness(t)
	ctx := t.Context()
	_, sess := sessionSetup(t, h, super)
	id := sess.SessionId
	fence := h.lease(t, id)
	h.requestApproval(t, fence, "ap1", 600_000)
	h.requestApproval(t, fence, "ap2", 600_000)
	h.requestApproval(t, fence, "ap3", 10)
	h.clock.Add(10) // ap3 has expired, unswept
	list, err := super.sessions.ListApprovals(ctx, connect.NewRequest(&circusv1.ListApprovalsRequest{SessionId: id}))
	if err != nil || len(list.Msg.Approvals) != 2 {
		t.Fatalf("list = %v, %v", list, err)
	}
	a := list.Msg.Approvals[0]
	if a.ApprovalId != "ap1" || a.Device != "box" || a.Tool != "bash" || a.Display != "rm -rf build" || a.SessionId != id || a.ToolUseId != "tu-ap1" {
		t.Fatalf("approval = %v", a)
	}
	resolve := func(approvalID string, approve bool) error {
		_, err := super.sessions.ResolveApproval(ctx, connect.NewRequest(&circusv1.ResolveApprovalRequest{ApprovalId: approvalID, Approve: approve}))
		return err
	}
	if err := resolve("ap1", true); err != nil {
		t.Fatal(err)
	}
	wantCalls(t, "approve wakes", h.waker.take(), id+":APPROVAL")
	if err := resolve("ap1", true); err != nil {
		t.Fatal(err)
	}
	if err := resolve("ap1", false); err != nil {
		t.Fatal(err)
	}
	wantCalls(t, "repeat wakes", h.waker.take())
	if err := resolve("ap2", false); err != nil {
		t.Fatal(err)
	}
	// An approval that expired before the resolution: no effect, and the
	// client is told so; the expiry is still recorded, once.
	wantCode(t, resolve("ap3", true), connect.CodeFailedPrecondition)
	wantCode(t, resolve("ap3", false), connect.CodeFailedPrecondition)
	wantCalls(t, "deny and expired wakes", h.waker.take())
	var states []string
	for _, e := range h.events(t, id) {
		if e.Kind == "APPROVAL_RESOLVED" {
			states = append(states, string(e.Payload))
		}
	}
	wantCalls(t, "resolution events", states, `{"approval_id":"ap1","state":"approved"}`, `{"approval_id":"ap2","state":"denied"}`, `{"approval_id":"ap3","state":"expired"}`)
	// The approval input waits for the worker.
	inputs, err := h.st.TakeInputs(ctx, store.Fence{SessionID: id, Epoch: fence.Epoch, WorkerID: "w1", BootID: "boot", ConnID: "conn", Now: h.clock.Load()})
	if err != nil || len(inputs) != 1 || inputs[0].Kind != store.InputApproval || !strings.Contains(inputs[0].Text, "rm -rf build") {
		t.Fatalf("inputs = %v, %v", inputs, err)
	}
	list, err = super.sessions.ListApprovals(ctx, connect.NewRequest(&circusv1.ListApprovalsRequest{SessionId: id}))
	if err != nil || len(list.Msg.Approvals) != 0 {
		t.Fatalf("list after = %v, %v", list, err)
	}
	// A stopped session refuses resolutions.
	h.requestApproval(t, fence, "ap4", 600_000)
	if _, err := super.sessions.StopSession(ctx, connect.NewRequest(&circusv1.StopSessionRequest{SessionId: id})); err != nil {
		t.Fatal(err)
	}
	wantCode(t, resolve("ap4", true), connect.CodeFailedPrecondition)
	wantCode(t, resolve("nope", true), connect.CodeNotFound)
}

func TestInterruptStopDelete(t *testing.T) {
	h, super := newHarness(t)
	ctx := t.Context()
	_, sess := sessionSetup(t, h, super)
	id := sess.SessionId
	interrupt := func() {
		t.Helper()
		if _, err := super.sessions.Interrupt(ctx, connect.NewRequest(&circusv1.InterruptRequest{SessionId: id})); err != nil {
			t.Fatal(err)
		}
	}
	interrupt() // no turn: nothing to do
	wantCalls(t, "idle interrupt", h.waker.take())
	fence := h.lease(t, id)
	h.startTurn(t, fence, 1)
	interrupt()
	wantCalls(t, "interrupt", h.waker.take(), id+":INTERRUPT")
	if s, _ := h.st.GetSession(ctx, id, h.clock.Load()); s.InterruptTurnNo != 1 {
		t.Fatalf("interrupt level = %d", s.InterruptTurnNo)
	}

	_, err := super.sessions.DeleteSession(ctx, connect.NewRequest(&circusv1.DeleteSessionRequest{SessionId: id}))
	wantCode(t, err, connect.CodeFailedPrecondition)
	stop := func() {
		t.Helper()
		if _, err := super.sessions.StopSession(ctx, connect.NewRequest(&circusv1.StopSessionRequest{SessionId: id})); err != nil {
			t.Fatal(err)
		}
	}
	stop()
	wantCalls(t, "stop wakes", h.waker.take(), id+":STOP")
	wantCalls(t, "stop jobs", h.jobs.take(), h.owner.ID+":"+id)
	h.jobs.err = errors.New("device offline")
	stop() // idempotent; the job stop is retried and its failure only logged
	wantCalls(t, "second stop wakes", h.waker.take())
	wantCalls(t, "second stop jobs", h.jobs.take(), h.owner.ID+":"+id)
	var stopEvents []string
	for _, e := range h.events(t, id) {
		if e.Kind == "SESSION_STATE" {
			stopEvents = append(stopEvents, string(e.Payload))
		}
	}
	wantCalls(t, "stop events", stopEvents, `{"state":"stopped","reason":"user"}`)
	got := super.getSession(t, id)
	if got.State != circusv1.Session_STATE_STOPPED || got.StoppedAtUnixMs == 0 {
		t.Fatalf("after stop = %v", got)
	}
	// The holder still has its lease.
	_, err = super.sessions.DeleteSession(ctx, connect.NewRequest(&circusv1.DeleteSessionRequest{SessionId: id}))
	wantCode(t, err, connect.CodeFailedPrecondition)
	if _, err := h.st.ReleaseLease(ctx, fence); err != nil {
		t.Fatal(err)
	}
	if _, err := super.sessions.DeleteSession(ctx, connect.NewRequest(&circusv1.DeleteSessionRequest{SessionId: id})); err != nil {
		t.Fatal(err)
	}
	_, err = super.sessions.DeleteSession(ctx, connect.NewRequest(&circusv1.DeleteSessionRequest{SessionId: id}))
	wantCode(t, err, connect.CodeNotFound)
}

func TestRecoverSession(t *testing.T) {
	h, super := newHarness(t)
	ctx := t.Context()
	_, sess := sessionSetup(t, h, super)
	id := sess.SessionId
	recoverSession := func(accept bool) error {
		_, err := super.sessions.RecoverSession(ctx, connect.NewRequest(&circusv1.RecoverSessionRequest{SessionId: id, AcceptTranscriptGap: accept}))
		return err
	}
	wantField(t, recoverSession(false), connect.CodeInvalidArgument, "accept_transcript_gap")
	wantCode(t, recoverSession(true), connect.CodeFailedPrecondition)
	fence := h.lease(t, id)
	h.startTurn(t, fence, 1)
	if _, err := h.st.ReportTurn(ctx, fence, store.TurnReport{LocalSeq: 1, Phase: store.TurnEnded, Outcome: store.TurnCompleted, At: h.clock.Load(), TranscriptIncomplete: true}); err != nil {
		t.Fatal(err)
	}
	if got := super.getSession(t, id); got.State != circusv1.Session_STATE_BROKEN {
		t.Fatalf("state = %v", got.State)
	}
	h.waker.take()
	if err := recoverSession(true); err != nil {
		t.Fatal(err)
	}
	wantCalls(t, "recover wakes", h.waker.take(), id+":INPUT")
	events := h.events(t, id)
	if last := events[len(events)-1]; last.Kind != store.EventKindTranscriptGapAccepted {
		t.Fatalf("last event = %v", last)
	}
	if got := super.getSession(t, id); got.State != circusv1.Session_STATE_IDLE {
		t.Fatalf("state after recover = %v", got.State)
	}
	if _, err := h.st.CheckFence(ctx, fence); !errors.Is(err, store.ErrFenced) {
		t.Fatalf("the holder's lease survived: %v", err)
	}
}

// failingSessionStore fails the reads and writes of the session API.
type failingSessionStore struct {
	store.Store
	err error
}

func (f failingSessionStore) ListSessions(context.Context, store.SessionFilter) ([]store.Session, error) {
	return nil, f.err
}

func (f failingSessionStore) Tx(ctx context.Context, fn func(context.Context, store.Ops) error) error {
	return f.err
}

func TestSessionStoreErrors(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code connect.Code
	}{
		{store.ErrBusy, connect.CodeUnavailable},
		{errors.New("disk on fire: /secret/path"), connect.CodeInternal},
	} {
		h, super := newHarness(t, func(c *webapi.Config) { c.Store = failingSessionStore{c.Store, tc.err} })
		ctx := t.Context()
		h.device(t, "d1", "box", "linux")
		_, err := super.sessions.ListSessions(ctx, connect.NewRequest(&circusv1.ListSessionsRequest{}))
		wantCode(t, err, tc.code)
		_, err = super.sessions.GetSession(ctx, connect.NewRequest(&circusv1.GetSessionRequest{SessionId: "s"}))
		wantCode(t, err, tc.code)
		_, err = super.send(t, "s", "m", "x")
		wantCode(t, err, tc.code)
		var ce *connect.Error
		if errors.As(err, &ce) && strings.Contains(ce.Message(), "secret") {
			t.Fatalf("store text leaked: %v", ce)
		}
	}
}

// cancelingInterceptor authenticates as the Web service does and then ends
// the handler's context of one procedure, leaving the client connected to
// read the code.
type cancelingInterceptor struct {
	connect.Interceptor
	procedure string
	deadline  bool
}

func (c cancelingInterceptor) end(ctx context.Context) (context.Context, context.CancelFunc) {
	if c.deadline {
		return context.WithDeadline(ctx, time.Now().Add(-time.Second))
	}
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	return ctx, cancel
}

func (c cancelingInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return c.Interceptor.WrapUnary(func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if req.Spec().Procedure == c.procedure {
			var cancel context.CancelFunc
			ctx, cancel = c.end(ctx)
			defer cancel()
		}
		return next(ctx, req)
	})
}

func (c cancelingInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return c.Interceptor.WrapStreamingHandler(func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		if conn.Spec().Procedure == c.procedure {
			var cancel context.CancelFunc
			ctx, cancel = c.end(ctx)
			defer cancel()
		}
		return next(ctx, conn)
	})
}

func TestSessionContextErrors(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		for _, procedure := range []string{circusv1connect.SessionServiceSendMessageProcedure, circusv1connect.SessionServiceWatchSessionProcedure, circusv1connect.SessionServiceListSessionsProcedure} {
			want := connect.CodeCanceled
			if deadline {
				want = connect.CodeDeadlineExceeded
			}
			t.Run(fmt.Sprint(procedure, deadline), func(t *testing.T) {
				h, super := newHarness(t, func(c *webapi.Config) {
					c.Interceptor = cancelingInterceptor{c.Interceptor, procedure, deadline}
				})
				_, sess := sessionSetup(t, h, super)
				var err error
				switch procedure {
				case circusv1connect.SessionServiceSendMessageProcedure:
					_, err = super.send(t, sess.SessionId, "m", "x")
				case circusv1connect.SessionServiceListSessionsProcedure:
					_, err = super.sessions.ListSessions(t.Context(), connect.NewRequest(&circusv1.ListSessionsRequest{}))
				default:
					var stream *connect.ServerStreamForClient[circusv1.SessionEvent]
					stream, err = super.sessions.WatchSession(t.Context(), connect.NewRequest(&circusv1.WatchSessionRequest{SessionId: sess.SessionId}))
					if err == nil {
						for stream.Receive() {
						}
						err = stream.Err()
					}
				}
				wantCode(t, err, want)
				if len(h.events(t, sess.SessionId)) != 0 {
					t.Fatal("a canceled request recorded an event")
				}
			})
		}
	}
}

// viewCapText returns text built from unit whose INPUT_ACCEPTED event payload, for
// client message id msgID and a server-chosen 36-character input id, is
// exactly the view's payload cap plus extra bytes.
func viewCapText(t *testing.T, msgID, unit string, extra int) string {
	t.Helper()
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(map[string]string{"input_id": strings.Repeat("0", 36), "client_msg_id": msgID, "priority": "normal", "text": ""}); err != nil {
		t.Fatal(err)
	}
	room := store.MaxEventPayloadBytes + extra - (b.Len() - 1)
	text := strings.Repeat(unit, room/len(unit))
	return text + strings.Repeat("a", room-len(text))
}

// TestSendMessageViewCap: a message is accepted exactly when its INPUT_ACCEPTED event
// keeps the whole text, with "<", ">" and "&" unescaped and multi-byte
// text counted in bytes.
func TestSendMessageViewCap(t *testing.T) {
	h, super := newHarness(t)
	_, sess := sessionSetup(t, h, super)
	for i, unit := range []string{"<>&", "界<>&é", "\"\\"} {
		msgID := fmt.Sprint("m", i)
		_, err := super.send(t, sess.SessionId, msgID, viewCapText(t, msgID, unit, 1))
		wantField(t, err, connect.CodeInvalidArgument, "text")
		text := viewCapText(t, msgID, unit, 0)
		if unit == "\"\\" {
			// Each of these takes two bytes in JSON.
			text = strings.Repeat(unit, (len(text)-1)/4) + "a"
		}
		res, err := super.send(t, sess.SessionId, msgID, text)
		if err != nil {
			t.Fatalf("%q at the cap: %v", unit, err)
		}
		var stored store.SessionEvent
		for _, e := range h.events(t, sess.SessionId) {
			if e.Seq == int64(res.EventSeq) {
				stored = e
			}
		}
		var payload map[string]string
		if stored.Truncated || json.Unmarshal(stored.Payload, &payload) != nil || payload["text"] != text {
			t.Fatalf("%q: stored %d bytes, truncated %v", unit, len(stored.Payload), stored.Truncated)
		}
		if unit == "<>&" && len(stored.Payload) != store.MaxEventPayloadBytes {
			t.Fatalf("payload is %d bytes, want the cap", len(stored.Payload))
		}
	}
	// The reserved prefix of approval inputs.
	_, err := super.send(t, sess.SessionId, "approval:ap1", "hi")
	wantField(t, err, connect.CodeInvalidArgument, "client_msg_id")
}

// TestCreateSessionOwnership checks that session locations and pinned
// workers belong to the caller and are live.
func TestCreateSessionOwnership(t *testing.T) {
	h, super := newHarness(t)
	ctx := t.Context()
	h.device(t, "d1", "box", "linux")
	p := super.mustCreate(t, "Project", loc("box", "/work"))
	now := h.clock.Load()
	if err := h.st.CreateUser(ctx, store.User{ID: "other", Name: "other", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []store.Identity{
		{Kind: store.KindDevice, ID: "d9", UserID: "other", Name: "elsewhere", MachineID: "m9", OS: "linux", CreatedAt: now},
		{Kind: store.KindWorker, ID: "w9", UserID: "other", Name: "elsewhere", MachineID: "m9", CreatedAt: now},
		{Kind: store.KindWorker, ID: "w1", UserID: h.owner.ID, Name: "gone", MachineID: "m1", CreatedAt: now},
	} {
		if err := h.st.CreateIdentity(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.st.DeleteIdentity(ctx, store.KindWorker, "w1", now); err != nil {
		t.Fatal(err)
	}
	_, err := super.createSession(t, &circusv1.CreateSessionRequest{ClientRequestId: "foreign-worker", ProjectId: p.ProjectId, PinnedWorkerId: new("w9")})
	wantField(t, err, connect.CodeInvalidArgument, "pinned_worker_id")
	_, err = super.createSession(t, &circusv1.CreateSessionRequest{ClientRequestId: "revoked-worker", ProjectId: p.ProjectId, PinnedWorkerId: new("w1")})
	wantField(t, err, connect.CodeInvalidArgument, "pinned_worker_id")
	_, err = super.update(t, &circusv1.UpdateProjectRequest{ProjectId: p.ProjectId, Locations: locations(loc("elsewhere", "/x"))})
	wantField(t, err, connect.CodeInvalidArgument, "locations[0].device")
	if _, err := h.st.DeleteIdentity(ctx, store.KindDevice, "d1", now); err != nil {
		t.Fatal(err)
	}
	_, err = super.createSession(t, &circusv1.CreateSessionRequest{ClientRequestId: "revoked-device", ProjectId: p.ProjectId})
	wantField(t, err, connect.CodeFailedPrecondition, "location_index")
	list, err := super.sessions.ListSessions(ctx, connect.NewRequest(&circusv1.ListSessionsRequest{}))
	if err != nil || len(list.Msg.Sessions) != 0 {
		t.Fatalf("sessions = %v, %v", list, err)
	}
}

func TestSendMessageNowWhileIdle(t *testing.T) {
	h, super := newHarness(t)
	_, sess := sessionSetup(t, h, super)
	if _, err := super.sessions.SendMessage(t.Context(), connect.NewRequest(&circusv1.SendMessageRequest{SessionId: sess.SessionId, ClientMsgId: "m1", Text: "now", Priority: circusv1.InputPriority_INPUT_PRIORITY_NOW})); err != nil {
		t.Fatal(err)
	}
	wantCalls(t, "wakes", h.waker.take(), sess.SessionId+":INPUT")
}

// TestStopEventInFullView: the stop event is a boundary, kept when the
// view only stores boundaries.
func TestStopEventInFullView(t *testing.T) {
	h, super := newHarness(t)
	_, sess := sessionSetup(t, h, super)
	if _, err := h.st.AppendEvents(t.Context(), sess.SessionId, nil, []store.EventInput{{Kind: "ASSISTANT", Payload: []byte(`"x"`), At: h.clock.Load()}}, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := super.sessions.StopSession(t.Context(), connect.NewRequest(&circusv1.StopSessionRequest{SessionId: sess.SessionId})); err != nil {
		t.Fatal(err)
	}
	events := h.events(t, sess.SessionId)
	if len(events) != 1 || events[0].Kind != "SESSION_STATE" {
		t.Fatalf("events = %v", events)
	}
}

type staleLocationStore struct {
	store.Store
}

func (s staleLocationStore) Tx(ctx context.Context, fn func(context.Context, store.Ops) error) error {
	return s.Store.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		return fn(ctx, staleLocationOps{tx})
	})
}

type staleLocationOps struct{ store.Ops }

func (s staleLocationOps) GetProject(ctx context.Context, userID, projectID string) (store.Project, error) {
	p, err := s.Ops.GetProject(ctx, userID, projectID)
	if err == nil && len(p.Locations) == 1 {
		// Simulate a stored location created before the owner check.
		p.Locations[0].DeviceID = "foreign-device"
	}
	return p, err
}

func TestCreateSessionRejectsForeignDeviceAtLocation(t *testing.T) {
	h, super := newHarness(t, func(c *webapi.Config) { c.Store = staleLocationStore{c.Store} })
	ctx := t.Context()
	h.device(t, "d1", "desk", "linux")
	h.signIn(t, "bob", "bob@example.com")
	if err := h.st.CreateIdentity(ctx, store.Identity{
		Kind: store.KindDevice, ID: "foreign-device", UserID: "bob", Name: "desk",
		MachineID: "m2", OS: "linux", CreatedAt: h.clock.Load(),
	}); err != nil {
		t.Fatal(err)
	}
	project := super.mustCreate(t, "Project", loc("desk", "/work"))
	_, err := super.createSession(t, &circusv1.CreateSessionRequest{ClientRequestId: "foreign-device", ProjectId: project.ProjectId})
	wantField(t, err, connect.CodeFailedPrecondition, "location_index")
}
