//go:build e2e && linux

package e2e

import (
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/store"
	testwait "github.com/EricWvi/circus/internal/testutil/wait"
	sessionclient "github.com/EricWvi/circus/internal/testutil/workerclient"
	"github.com/EricWvi/circus/internal/wire"
	"github.com/google/uuid"
)

// TestSessionTurn drives one session turn through the server binary: a
// session with pending input is offered to the worker that attaches, the
// worker acquires it, takes the input, runs a device call admitted by its
// lease (a call at another epoch is fenced), reports the turn and releases.
func TestSessionTurn(t *testing.T) {
	h := newHarness(t)
	ctx := testwait.Ctx(t)
	user := h.user()
	devices, err := h.st.ListIdentities(ctx, store.KindDevice, user)
	must(t, err)
	now := time.Now().UnixMilli()
	_, err = h.st.CreateProject(ctx, store.Project{ID: "e2e-project", UserID: user, Name: "e2e", CreatedAt: now, UpdatedAt: now})
	must(t, err)
	must(t, h.st.CreateSession(ctx, store.Session{ID: "e2e-session", UserID: user, ProjectID: "e2e-project", ClaudeSessionID: "e2e-claude",
		DefaultDeviceID: devices[0].ID, CreatedAt: now}, nil, store.DefaultMaxSessionsPerUser))
	in, err := h.st.SubmitInput(ctx, store.Input{InputID: "e2e-input", SessionID: "e2e-session", ClientMsgID: "m1", Kind: store.InputUser,
		Text: "run it", Priority: store.InputNormal, CreatedAt: now})
	must(t, err)
	if in.Session.State != store.SessionQueued {
		t.Fatalf("session %+v", in.Session)
	}

	c, err := sessionclient.Dial(ctx, sessionclient.Options{URL: h.base, Token: h.worker.Token, PrincipalID: h.worker.PrincipalID, BootID: uuid.Must(uuid.NewV7()).String()})
	must(t, err)
	defer c.Close()
	var jobKey *circusv1.JobKey
	ev, err := c.Attach(ctx, &circusv1.Attach{MaxSessions: 1})
	must(t, err)
	e, err := ev.Next(ctx)
	must(t, err)
	if e.GetOffer().GetSessionId() != "e2e-session" {
		t.Fatalf("hint %v", e)
	}
	r, err := c.Session(ctx, sessionclient.Acquire("e2e-session", true))
	must(t, err)
	epoch := r.GetLease().GetEpoch()
	if epoch != 1 || r.GetLease().GetConfig().GetDefaultDevice() != "desk" {
		t.Fatalf("lease %v", r)
	}
	r, err = c.Session(ctx, sessionclient.TakeInputs("e2e-session", epoch))
	must(t, err)
	if len(r.GetInputs().GetInputs()) != 1 {
		t.Fatalf("inputs %v", r)
	}
	r, err = c.Session(ctx, sessionclient.Started("e2e-session", epoch, 1, circusv1.TurnOrigin_TURN_ORIGIN_INPUT))
	must(t, err)
	if r.GetTurn().GetTurnNo() != 1 {
		t.Fatalf("started %v", r)
	}

	fenced, err := c.Submit(ctx, &circusv1.SubmitCall{Device: "desk", Call: &circusv1.DeviceCall{CallId: "e2e-fenced", SessionId: "e2e-session",
		Tool: &circusv1.DeviceCall_Bash{Bash: &circusv1.BashRequest{Command: "printf no", Cwd: h.dir}}}, SessionEpoch: epoch + 1, ToolUseId: "toolu_1"})
	must(t, err)
	u, err := fenced.Next(ctx)
	must(t, err)
	if u.GetRejected().GetCode() != circusv1.CallUpdate_Rejected_CODE_SESSION_FENCED {
		t.Fatalf("fenced call %v", u)
	}
	call, err := c.Submit(ctx, &circusv1.SubmitCall{Device: "desk", Call: &circusv1.DeviceCall{CallId: "e2e-call", SessionId: "e2e-session",
		Tool: &circusv1.DeviceCall_Bash{Bash: &circusv1.BashRequest{Command: "printf session", Cwd: h.dir}}}, SessionEpoch: epoch, ToolUseId: "toolu_2"})
	must(t, err)
	for {
		u, err = call.Next(ctx)
		must(t, err)
		if wire.Terminal(u) {
			break
		}
	}
	if output(u) != "session" {
		t.Fatalf("call %v", u)
	}

	// A background job the session started: registered with the session
	// when its call's result arrives; its end nudges the holder (JOB).
	bg, err := c.Submit(ctx, &circusv1.SubmitCall{Device: "desk", Call: &circusv1.DeviceCall{CallId: "e2e-job", SessionId: "e2e-session",
		Tool: &circusv1.DeviceCall_Bash{Bash: &circusv1.BashRequest{Command: "sleep 0.3; printf bg", Cwd: h.dir, RunInBackground: true, Description: "bg job"}}},
		SessionEpoch: epoch, ToolUseId: "toolu_3"})
	must(t, err)
	for {
		u, err = bg.Next(ctx)
		must(t, err)
		if wire.Terminal(u) {
			break
		}
	}
	if u.GetResult().GetBash().GetJobId() == "" {
		t.Fatalf("background call %v", u)
	}
	for {
		e, err := ev.Next(ctx)
		must(t, err)
		if n := e.GetNudge(); n.GetSessionId() == "e2e-session" && hasJob(n) {
			st, err := c.Session(ctx, sessionclient.State("e2e-session", epoch, circusv1.GetSessionState_SECTION_JOBS))
			must(t, err)
			jobs := st.GetState().GetState().GetJobs()
			if len(jobs) == 1 && jobs[0].GetState() == circusv1.JobEvent_STATE_EXITED {
				if jobs[0].GetCallId() != "e2e-job" || jobs[0].GetToolUseId() != "toolu_3" || jobs[0].GetDevice() != "desk" {
					t.Fatalf("job %v", jobs[0])
				}
				jobKey = jobs[0].GetKey()
				break
			}
		}
	}
	r, err = c.Session(ctx, sessionclient.Patch("e2e-session", epoch, &circusv1.PatchSessionState{HandedJobs: []*circusv1.JobKey{jobKey}}))
	must(t, err)
	if r.GetOk() == nil {
		t.Fatalf("handed %v", r)
	}

	r, err = c.Session(ctx, sessionclient.Ended("e2e-session", epoch, 1, circusv1.TurnOutcome_TURN_OUTCOME_COMPLETED,
		&circusv1.ReportTurn_Persisted{InputIds: []string{"e2e-input"}, CallIds: []string{"e2e-call", "e2e-job"}, Jobs: []*circusv1.JobKey{jobKey}}))
	must(t, err)
	if r.GetTurn().GetTurnNo() != 1 {
		t.Fatalf("ended %v", r)
	}
	r, err = c.Session(ctx, sessionclient.Release("e2e-session", epoch, circusv1.ReleaseLease_REASON_IDLE))
	must(t, err)
	if r.GetOk() == nil {
		t.Fatalf("release %v", r)
	}
	s, err := h.st.GetSession(ctx, "e2e-session", time.Now().UnixMilli())
	must(t, err)
	row, err := h.st.GetCall(ctx, "e2e-call")
	must(t, err)
	if s.State != store.SessionIdle || s.TurnCount != 1 || row.ToolUseID != "toolu_2" || row.PersistedAt == 0 {
		t.Fatalf("session %+v call %+v", s, row)
	}
}

func hasJob(n *circusv1.WorkerEvent_Nudge) bool {
	for _, r := range n.GetReasons() {
		if r == circusv1.WorkerEvent_Nudge_REASON_JOB {
			return true
		}
	}
	return false
}
