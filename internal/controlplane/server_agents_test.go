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

// TestWorkerAgentThroughServer: a worker attaches without Claude Code, so a
// session waits as AGENT_NOT_INSTALLED; SetWorkerAgent pushes the wish over
// the live link at once; the worker's report of the finished install gets
// it the session; uninstalling is refused while it holds the lease and,
// once released, pushes the empty wish list.
func TestWorkerAgentThroughServer(t *testing.T) {
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
	setup, err := circusv1connect.NewAuthServiceClient(client, h.base).Setup(ctx, connect.NewRequest(&circusv1.SetupRequest{Token: token, Email: "admin@example.com", Password: "password-123"}))
	if err != nil {
		t.Fatal(err)
	}
	owner := setup.Msg.GetUser()
	if err := h.st.CreateIdentity(ctx, store.Identity{Kind: store.KindDevice, ID: "d1", UserID: owner.GetUserId(), Name: "box", MachineID: "m1", OS: "linux", CreatedAt: time.Now().UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	enrollToken, _, err := auth.CreateEnrollToken(ctx, h.st, owner.GetUserId(), store.KindWorker, time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	worker, err := auth.Enroll(ctx, h.st, auth.EnrollRequest{Token: enrollToken, Kind: store.KindWorker, Name: "w", MachineID: "m2"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	machines := circusv1connect.NewMachineServiceClient(client, h.base)
	project, err := circusv1connect.NewProjectServiceClient(client, h.base).CreateProject(ctx, connect.NewRequest(&circusv1.CreateProjectRequest{
		Name: "Project", Locations: []*circusv1.Project_Location{{Device: "box", Path: "/work"}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	api := circusv1connect.NewSessionServiceClient(client, h.base)
	created, err := api.CreateSession(ctx, connect.NewRequest(&circusv1.CreateSessionRequest{ClientRequestId: "s", ProjectId: project.Msg.Project.ProjectId}))
	if err != nil {
		t.Fatal(err)
	}
	id := created.Msg.Session.SessionId
	if created.Msg.Session.GetConfig().GetAgent() != circusv1.AgentKind_AGENT_KIND_CLAUDE_CODE {
		t.Fatalf("session config %v", created.Msg.Session.GetConfig())
	}

	w, err := wc.Dial(ctx, wc.Options{URL: h.base, Token: worker.Token, PrincipalID: worker.Identity.ID, BootID: uuid.Must(uuid.NewV7()).String()})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	hints, err := w.Attach(ctx, &circusv1.Attach{MaxSessions: 1, Agents: []*circusv1.AgentStatus{
		wc.ClaudeCode(circusv1.AgentStatus_STATE_NOT_INSTALLED, "", "2.0")}})
	if err != nil {
		t.Fatal(err)
	}
	wishes := func() []*circusv1.WorkerEvent_AgentsWanted_Wanted {
		t.Helper()
		for {
			e, err := hints.NextEvent(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if wanted := e.GetAgentsWanted(); wanted != nil {
				return wanted.GetAgents()
			}
		}
	}
	if got := wishes(); len(got) != 0 {
		t.Fatalf("wishes after Attach %v", got)
	}
	if _, err := api.SendMessage(ctx, connect.NewRequest(&circusv1.SendMessageRequest{SessionId: id, ClientMsgId: "m1", Text: "hello"})); err != nil {
		t.Fatal(err)
	}
	wait.For(t, func() bool {
		s, err := api.GetSession(ctx, connect.NewRequest(&circusv1.GetSessionRequest{SessionId: id}))
		return err == nil && s.Msg.GetQueueReason() == circusv1.QueueReason_QUEUE_REASON_AGENT_NOT_INSTALLED
	})

	set := func(install bool) (*circusv1.Worker, error) {
		res, err := machines.SetWorkerAgent(ctx, connect.NewRequest(&circusv1.SetWorkerAgentRequest{WorkerId: worker.Identity.ID,
			Agent: circusv1.AgentKind_AGENT_KIND_CLAUDE_CODE, Install: install}))
		if err != nil {
			return nil, err
		}
		return res.Msg.GetWorker(), nil
	}
	if _, err := set(true); err != nil {
		t.Fatal(err)
	}
	if got := wishes(); len(got) != 1 || got[0].GetAgent() != circusv1.AgentKind_AGENT_KIND_CLAUDE_CODE || got[0].GetVersion() != "2.0" {
		t.Fatalf("pushed wishes %v", got)
	}
	reply := func(r wc.Reply, err error) *circusv1.SessionReply {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		out, err := r.Wait(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if r := reply(w.ReportAgents(ctx, wc.ClaudeCode(circusv1.AgentStatus_STATE_INSTALLED, "2.0", "2.0"))); r.GetOk() == nil {
		t.Fatalf("report %v", r)
	}
	hint, err := hints.Next(ctx)
	if err != nil || hint.GetOffer().GetSessionId() != id {
		t.Fatalf("hint %v, %v", hint, err)
	}
	lease, err := w.Session(ctx, wc.Acquire(id, true))
	if err != nil || lease.GetLease() == nil || lease.GetLease().GetConfig().GetAgent() != circusv1.AgentKind_AGENT_KIND_CLAUDE_CODE {
		t.Fatalf("lease %v, %v", lease, err)
	}
	if _, err := set(false); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("uninstall while leased: %v", err)
	}
	epoch := lease.GetLease().GetEpoch()
	if r, err := w.Session(ctx, wc.Release(id, epoch, circusv1.ReleaseLease_REASON_IDLE)); err != nil || r.GetOk() == nil {
		t.Fatalf("release %v, %v", r, err)
	}
	wk, err := set(false)
	if err != nil {
		t.Fatal(err)
	}
	if a := wk.GetAgents(); len(a) != 1 || a[0].GetWanted() || a[0].GetStatus().GetState() != circusv1.AgentStatus_STATE_INSTALLED {
		t.Fatalf("worker agents %v", a)
	}
	if got := wishes(); len(got) != 0 {
		t.Fatalf("wishes after uninstall %v", got)
	}
}
