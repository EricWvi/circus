package controlplane_test

import (
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/gen/go/circus/v1/circusv1connect"
	"github.com/EricWvi/circus/internal/auth"
	"github.com/EricWvi/circus/internal/controlplane"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/testutil/wait"
	wc "github.com/EricWvi/circus/internal/testutil/workerclient"
	"github.com/google/uuid"
)

// TestSessionFlowThroughServer drives one session end to end through the
// real server: a Web user (cookie) creates a project and a session and
// sends a message; the worker link offers the session to an attached
// worker, which acquires it, takes the input, reports a turn and publishes
// events; a WatchSession stream sees the view in order; Interrupt and
// StopSession reach the worker as Nudges.
func TestSessionFlowThroughServer(t *testing.T) {
	h := newServer(t, controlplane.Config{}, nil)
	ctx := wait.Ctx(t)
	var token string
	for line := range strings.SplitSeq(h.logs.String(), "\n") {
		var rec struct {
			URL string `json:"url"`
		}
		if json.Unmarshal([]byte(line), &rec) == nil && rec.URL != "" {
			u, _ := url.Parse(rec.URL)
			token = u.Query().Get("token")
		}
	}
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	t.Cleanup(client.CloseIdleConnections)
	if _, err := circusv1connect.NewAuthServiceClient(client, h.base).Setup(ctx, connect.NewRequest(&circusv1.SetupRequest{Token: token, Email: "admin@example.com", Password: "password-123"})); err != nil {
		t.Fatal(err)
	}
	owner := controlplane.User(h.srv)
	if err := h.st.CreateIdentity(ctx, store.Identity{Kind: store.KindDevice, ID: "d1", UserID: owner.ID, Name: "box", MachineID: "m1", OS: "linux", CreatedAt: time.Now().UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	enrollToken, _, err := auth.CreateEnrollToken(ctx, h.st, owner.ID, store.KindWorker, time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	worker, err := auth.Enroll(ctx, h.st, auth.EnrollRequest{Token: enrollToken, Kind: store.KindWorker, Name: "w", MachineID: "m2"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	project, err := circusv1connect.NewProjectServiceClient(client, h.base).CreateProject(ctx, connect.NewRequest(&circusv1.CreateProjectRequest{
		Name: "Project", Locations: []*circusv1.Project_Location{{Device: "box", Path: "/work"}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	api := circusv1connect.NewSessionServiceClient(client, h.base)
	created, err := api.CreateSession(ctx, connect.NewRequest(&circusv1.CreateSessionRequest{ProjectId: project.Msg.Project.ProjectId}))
	if err != nil {
		t.Fatal(err)
	}
	id := created.Msg.Session.SessionId
	stream, err := api.WatchSession(ctx, connect.NewRequest(&circusv1.WatchSessionRequest{SessionId: id}))
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	events := make(chan *circusv1.SessionEvent, 64)
	go func() {
		defer close(events)
		for stream.Receive() {
			events <- stream.Msg()
		}
	}()
	nextEvent := func(want circusv1.SessionEvent_Kind) *circusv1.SessionEvent {
		t.Helper()
		for {
			select {
			case e, ok := <-events:
				if !ok {
					t.Fatalf("stream ended waiting for %v: %v", want, stream.Err())
				}
				if e.GetKind() == want {
					return e
				}
				if !e.GetEphemeral() {
					t.Fatalf("event %v before %v", e, want)
				}
			case <-ctx.Done():
				t.Fatalf("no %v", want)
			}
		}
	}

	w, err := wc.Dial(ctx, wc.Options{URL: h.base, Token: worker.Token, PrincipalID: worker.Identity.ID, BootID: uuid.Must(uuid.NewV7()).String()})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	hints, err := w.Attach(ctx, &circusv1.Attach{MaxSessions: 1})
	if err != nil {
		t.Fatal(err)
	}
	hint := func(match func(*circusv1.WorkerEvent) bool) *circusv1.WorkerEvent {
		t.Helper()
		for {
			e, err := hints.Next(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if match(e) {
				return e
			}
		}
	}
	session := func(req *circusv1.SessionRequest) *circusv1.SessionReply {
		t.Helper()
		r, err := w.Session(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}

	sent, err := api.SendMessage(ctx, connect.NewRequest(&circusv1.SendMessageRequest{SessionId: id, ClientMsgId: "m1", Text: "hello"}))
	if err != nil {
		t.Fatal(err)
	}
	if e := nextEvent(circusv1.SessionEvent_KIND_INPUT_ACCEPTED); e.GetSeq() != sent.Msg.EventSeq || !strings.Contains(e.GetPayloadJson(), `"text":"hello"`) {
		t.Fatalf("input event %v", e)
	}
	hint(func(e *circusv1.WorkerEvent) bool { return e.GetOffer().GetSessionId() == id })
	lease := session(wc.Acquire(id, true)).GetLease()
	if lease.GetEpoch() != 1 || lease.GetConfig().GetDefaultDevice() != "box" {
		t.Fatalf("lease %v", lease)
	}
	epoch := lease.GetEpoch()
	inputs := session(wc.TakeInputs(id, epoch)).GetInputs().GetInputs()
	if len(inputs) != 1 || inputs[0].GetText() != "hello" || inputs[0].GetInputId() != sent.Msg.InputId {
		t.Fatalf("inputs %v", inputs)
	}
	if r := session(wc.Started(id, epoch, 1, circusv1.TurnOrigin_TURN_ORIGIN_INPUT)); r.GetTurn().GetTurnNo() != 1 {
		t.Fatalf("started %v", r)
	}
	if r := session(wc.Publish(id, epoch, 1,
		&circusv1.PublishEvents_Event{Kind: circusv1.SessionEvent_KIND_TURN_STARTED, Json: `{"origin":"input"}`},
		&circusv1.PublishEvents_Event{Kind: circusv1.SessionEvent_KIND_TOOL_PROGRESS, Json: `{"p":1}`, Ephemeral: true},
		&circusv1.PublishEvents_Event{Kind: circusv1.SessionEvent_KIND_ASSISTANT, Json: `{"a":1}`})); r.GetOk() == nil {
		t.Fatalf("publish %v", r)
	}
	nextEvent(circusv1.SessionEvent_KIND_TURN_STARTED)
	nextEvent(circusv1.SessionEvent_KIND_ASSISTANT)

	if _, err := api.Interrupt(ctx, connect.NewRequest(&circusv1.InterruptRequest{SessionId: id})); err != nil {
		t.Fatal(err)
	}
	n := hint(func(e *circusv1.WorkerEvent) bool {
		return e.GetNudge().GetSessionId() == id && hasNudgeReason(e.GetNudge(), circusv1.WorkerEvent_Nudge_REASON_INTERRUPT)
	}).GetNudge()
	if n.GetInterruptTurnNo() != 1 {
		t.Fatalf("interrupt nudge %v", n)
	}
	if r := session(wc.Ended(id, epoch, 1, circusv1.TurnOutcome_TURN_OUTCOME_STOPPED, &circusv1.ReportTurn_Persisted{InputIds: []string{sent.Msg.InputId}})); r.GetTurn().GetTurnNo() != 1 {
		t.Fatalf("ended %v", r)
	}
	nextEvent(circusv1.SessionEvent_KIND_TURN_STOPPED)

	if _, err := api.StopSession(ctx, connect.NewRequest(&circusv1.StopSessionRequest{SessionId: id})); err != nil {
		t.Fatal(err)
	}
	hint(func(e *circusv1.WorkerEvent) bool {
		return e.GetNudge().GetSessionId() == id && hasNudgeReason(e.GetNudge(), circusv1.WorkerEvent_Nudge_REASON_STOP)
	})
	if e := nextEvent(circusv1.SessionEvent_KIND_SESSION_STATE); e.GetPayloadJson() != `{"state":"stopped","reason":"user"}` {
		t.Fatalf("stop event %v", e)
	}
	if r := session(wc.Release(id, epoch, circusv1.ReleaseLease_REASON_STOPPED)); r.GetOk() == nil {
		t.Fatalf("release %v", r)
	}
}

func hasNudgeReason(n *circusv1.WorkerEvent_Nudge, r circusv1.WorkerEvent_Nudge_Reason) bool {
	for _, v := range n.GetReasons() {
		if v == r {
			return true
		}
	}
	return false
}
