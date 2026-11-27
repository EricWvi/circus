//go:build e2e && linux

package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/gen/go/circus/v1/circusv1connect"
	"github.com/EricWvi/circus/internal/auth"
	"github.com/EricWvi/circus/internal/credfile"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/testutil/fakeworker"
	testwait "github.com/EricWvi/circus/internal/testutil/wait"
	"github.com/EricWvi/circus/internal/testutil/workerclient"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
)

// providerWorld is a harness with two workers on machines of their own, a
// device without a worker (so its sessions are not pinned) and the
// browser's clients.
type providerWorld struct {
	h        *harness
	projects circusv1connect.ProjectServiceClient
	sessions circusv1connect.SessionServiceClient
	provs    circusv1connect.ProviderServiceClient
	workers  map[string]credfile.File // by name
	project  string

	mu sync.Mutex
	// envs records each GetLaunchEnv reply a fake worker got, by session;
	// offers and acquired record the worker of every Offer and every
	// acquisition, by session.
	envs     map[string][]launch
	offers   map[string][]string
	acquired map[string][]string
}

type launch struct {
	worker string
	reply  *pb.SessionReply
}

func (l launch) String() string {
	return l.worker + ":" + l.reply.GetLaunchEnv().GetProviderId() + "[" + strings.Join(envOf(l.reply), " ") + "]" + l.reply.GetRejected().GetCode().String()
}

// once checks that session id was offered only to workers in offered,
// acquired exactly once and started exactly once, by worker (any worker
// when ""), with exactly env; it returns that worker.
func (w *providerWorld) once(t *testing.T, what, id, worker, env string, offered ...string) string {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	ls, acq, offers := w.envs[id], w.acquired[id], w.offers[id]
	if len(acq) != 1 || len(ls) != 1 || (worker != "" && acq[0] != worker) || ls[0].worker != acq[0] ||
		strings.Join(envOf(ls[0].reply), " ") != env || ls[0].reply.GetLaunchEnv() == nil {
		t.Fatalf("%s: acquisitions %v, launches %v", what, acq, ls)
	}
	for _, o := range offers {
		if len(offered) > 0 && !slices.Contains(offered, o) {
			t.Fatalf("%s: offered to %v, want only %v", what, offers, offered)
		}
	}
	return acq[0]
}

func newProviderWorld(t *testing.T) *providerWorld {
	t.Helper()
	h := newHarness(t)
	w := &providerWorld{h: h, workers: map[string]credfile.File{}, envs: map[string][]launch{}, offers: map[string][]string{}, acquired: map[string][]string{}}
	jar, err := cookiejar.New(nil)
	must(t, err)
	httpClient := &http.Client{Jar: jar, Transport: http.DefaultTransport.(*http.Transport).Clone()}
	t.Cleanup(httpClient.CloseIdleConnections)
	ctx := testwait.Ctx(t)
	_, err = circusv1connect.NewAuthServiceClient(httpClient, h.base).Login(ctx, connect.NewRequest(&pb.LoginRequest{Email: enrollmentEmail, Password: enrollmentPassword}))
	must(t, err)
	w.projects = circusv1connect.NewProjectServiceClient(httpClient, h.base)
	w.sessions = circusv1connect.NewSessionServiceClient(httpClient, h.base)
	w.provs = circusv1connect.NewProviderServiceClient(httpClient, h.base)
	for _, name := range []string{"alpha", "beta", "gamma"} {
		token, _, err := auth.CreateEnrollToken(ctx, h.st, h.user(), store.KindWorker, time.Hour, time.Now())
		must(t, err)
		e, err := auth.Enroll(ctx, h.st, auth.EnrollRequest{Token: token, Kind: store.KindWorker, Name: name, MachineID: "machine-" + name, OS: "linux", Arch: "amd64"}, time.Now())
		must(t, err)
		w.workers[name] = credfile.File{ControlPlaneURL: h.base, Role: credfile.RoleWorker, PrincipalID: e.Identity.ID, Token: e.Token}
	}
	token, _, err := auth.CreateEnrollToken(ctx, h.st, h.user(), store.KindDevice, time.Hour, time.Now())
	must(t, err)
	solo, err := auth.Enroll(ctx, h.st, auth.EnrollRequest{Token: token, Kind: store.KindDevice, Name: "solo", MachineID: "machine-solo", OS: "linux", Arch: "amd64"}, time.Now())
	must(t, err)
	// Its daemon keeps the sessions' ledger snapshots, whose preparation
	// a session's first turn waits for.
	ledgerOnlyDaemon(t, h, solo.Identity.ID, solo.Token)
	p, err := w.projects.CreateProject(ctx, connect.NewRequest(&pb.CreateProjectRequest{Name: "Providers", Locations: []*pb.Project_Location{{Device: "solo", Path: h.dir}}}))
	must(t, err)
	w.project = p.Msg.GetProject().GetProjectId()
	return w
}

// run starts a fake worker as name until the test ends.
func (w *providerWorld) run(t *testing.T, name string, idle time.Duration) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- fakeworker.Run(ctx, fakeworker.Options{
			Name: name, Credential: w.workers[name], MaxSessions: 4, IdleRelease: idle, StreamDelay: 20 * time.Millisecond,
			Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
			LaunchEnv: func(id string, r *pb.SessionReply) {
				w.mu.Lock()
				defer w.mu.Unlock()
				w.envs[id] = append(w.envs[id], launch{worker: name, reply: r})
			},
			Offer: func(id string) {
				w.mu.Lock()
				defer w.mu.Unlock()
				w.offers[id] = append(w.offers[id], name)
			},
			Acquired: func(id string, _ uint64) {
				w.mu.Lock()
				defer w.mu.Unlock()
				w.acquired[id] = append(w.acquired[id], name)
			},
		})
	}()
	t.Cleanup(func() {
		cancel()
		if err := testwait.Recv(t, done); !errors.Is(err, context.Canceled) {
			t.Errorf("fake worker %s exited: %v", name, err)
		}
	})
}

func (w *providerWorld) launches(id string) []launch {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]launch(nil), w.envs[id]...)
}

func (w *providerWorld) provider(t *testing.T, p *pb.Provider) *pb.Provider {
	t.Helper()
	res, err := w.provs.CreateProvider(testwait.Ctx(t), connect.NewRequest(&pb.CreateProviderRequest{Provider: p}))
	must(t, err)
	return res.Msg.GetProvider()
}

func (w *providerWorld) update(t *testing.T, p *pb.Provider) *pb.Provider {
	t.Helper()
	res, err := w.provs.UpdateProvider(testwait.Ctx(t), connect.NewRequest(&pb.UpdateProviderRequest{Provider: p}))
	must(t, err)
	return res.Msg.GetProvider()
}

func (w *providerWorld) session(t *testing.T, provider *string) *sessionBrowser {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	t.Cleanup(cancel)
	res, err := w.sessions.CreateSession(ctx, connect.NewRequest(&pb.CreateSessionRequest{ClientRequestId: uuid.NewString(), ProjectId: w.project, ProviderId: provider}))
	must(t, err)
	return &sessionBrowser{t: t, ctx: ctx, api: w.sessions, id: res.Msg.GetSession().GetSessionId()}
}

func (w *providerWorld) worker(name string) string { return w.workers[name].PrincipalID }

func apiProvider(name string, scheme pb.Provider_ApiEndpoint_Scheme, key string, workers ...string) *pb.Provider {
	return &pb.Provider{Name: name, Kind: pb.Provider_KIND_CLAUDE_CODE, Api: &pb.Provider_ApiEndpoint{Scheme: scheme, Key: key}, WorkerIds: workers}
}

func envOf(r *pb.SessionReply) []string {
	var out []string
	for _, v := range r.GetLaunchEnv().GetEnv() {
		out = append(out, v.GetName()+"="+v.GetValue())
	}
	return out
}

// queueReason returns the queue_reason of a KIND_QUEUE event.
func queueReason(t *testing.T, e *pb.SessionEvent) string {
	t.Helper()
	var p struct {
		QueueReason string `json:"queue_reason"`
	}
	must(t, json.Unmarshal([]byte(e.GetPayloadJson()), &p))
	return p.QueueReason
}

// TestProviderScheduling drives providers end to end with fake workers:
// assignment decides where a session runs, max_sessions queues the next
// session with PROVIDER_FULL until a lease ends, and GetLaunchEnv returns
// exactly the provider's variables (local login without one).
func TestProviderScheduling(t *testing.T) {
	w := newProviderWorld(t)
	xKey := w.provider(t, apiProvider("x-api-key", pb.Provider_ApiEndpoint_SCHEME_X_API_KEY, "sk-fake-x-key", w.worker("alpha")))
	bearer := apiProvider("bearer", pb.Provider_ApiEndpoint_SCHEME_BEARER, "fake-bearer-token", w.worker("alpha"), w.worker("beta"))
	bearer.Api.BaseUrl = "https://gateway.example/anthropic"
	bearer.Api.FableModel, bearer.Api.OpusModel, bearer.Api.SonnetModel, bearer.Api.HaikuModel = "fable-fake", "opus-fake", "sonnet-fake", "haiku-fake"
	bearer.Env = []*pb.Provider_EnvVar{{Name: "FAKE_REGION", Value: proto.String("fake-east")}, {Name: "FAKE_EMPTY", Value: proto.String("")}}
	bearer.MaxSessions = 1
	full := w.provider(t, bearer)
	w.run(t, "alpha", 500*time.Millisecond)
	w.run(t, "beta", 500*time.Millisecond)

	// Assigned to alpha only: alpha runs it, with exactly the endpoint key.
	s := w.session(t, proto.String(xKey.GetProviderId()))
	s.send("hello x", pb.InputPriority_INPUT_PRIORITY_NORMAL)
	s.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_COMPLETED)
	// beta is attached with room, but only alpha is offered the session.
	w.once(t, "x-api-key", s.id, "alpha", "ANTHROPIC_API_KEY=sk-fake-x-key", "alpha")
	if ls := w.launches(s.id); ls[0].reply.GetLaunchEnv().GetProviderId() != xKey.GetProviderId() {
		t.Fatalf("x-api-key launches %v", ls)
	}

	// Local login: nothing to set.
	local := w.session(t, proto.String(""))
	local.send("hello local", pb.InputPriority_INPUT_PRIORITY_NORMAL)
	local.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_COMPLETED)
	w.once(t, "local login", local.id, "", "")
	if l := w.launches(local.id)[0]; !l.reply.GetLaunchEnv().GetLocalLogin() || l.reply.GetLaunchEnv().GetProviderId() != "" {
		t.Fatalf("local login launch %v", l)
	}

	// max_sessions 1: the second session waits with PROVIDER_FULL while
	// the first holds its lease, then runs once it is released.
	first, second := w.session(t, proto.String(full.GetProviderId())), w.session(t, proto.String(full.GetProviderId()))
	firstWatch, secondWatch := first.watch(0), second.watch(0)
	firstWatch.expect(pb.SessionEvent_KIND_UNSPECIFIED)
	secondWatch.expect(pb.SessionEvent_KIND_UNSPECIFIED)
	first.send("/fake slow 2", pb.InputPriority_INPUT_PRIORITY_NORMAL)
	firstWatch.expect(pb.SessionEvent_KIND_INPUT_ACCEPTED)
	firstWatch.expect(pb.SessionEvent_KIND_TURN_STARTED)
	second.send("after the first", pb.InputPriority_INPUT_PRIORITY_NORMAL)
	secondWatch.expect(pb.SessionEvent_KIND_INPUT_ACCEPTED)
	queued := secondWatch.until(func(e *pb.SessionEvent) bool { return e.GetKind() == pb.SessionEvent_KIND_QUEUE })
	if !queued.GetEphemeral() || queued.GetSeq() != 0 || queueReason(t, queued) != "provider_full" {
		t.Fatalf("queue event %v", queued)
	}
	if got := second.state(pb.Session_STATE_QUEUED, pb.TurnOutcome_TURN_OUTCOME_UNSPECIFIED); got.GetQueueReason() != pb.QueueReason_QUEUE_REASON_PROVIDER_FULL {
		t.Fatalf("queued session %v", got)
	}
	cleared := secondWatch.until(func(e *pb.SessionEvent) bool { return e.GetKind() == pb.SessionEvent_KIND_QUEUE })
	if queueReason(t, cleared) != "unspecified" {
		t.Fatalf("queue event %v", cleared)
	}
	second.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_COMPLETED)
	first.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_COMPLETED)
	const bearerEnv = "ANTHROPIC_BASE_URL=https://gateway.example/anthropic ANTHROPIC_AUTH_TOKEN=fake-bearer-token " +
		"ANTHROPIC_DEFAULT_FABLE_MODEL=fable-fake ANTHROPIC_DEFAULT_OPUS_MODEL=opus-fake ANTHROPIC_DEFAULT_SONNET_MODEL=sonnet-fake " +
		"ANTHROPIC_DEFAULT_HAIKU_MODEL=haiku-fake FAKE_REGION=fake-east FAKE_EMPTY="
	for _, id := range []string{first.id, second.id} {
		w.once(t, "bearer", id, "", bearerEnv)
		if l := w.launches(id)[0]; l.reply.GetLaunchEnv().GetProviderId() != full.GetProviderId() {
			t.Fatalf("bearer launch %v", l)
		}
	}
}

// TestProviderChangesAndLeases: changing a session's provider while it
// holds an idle lease revokes the lease and the session runs under the new
// provider's worker; unassigning the holder's worker revokes nothing, but
// its next GetLaunchEnv is PROVIDER_UNAVAILABLE and, released with
// REASON_PROVIDER, the session queues with NO_ASSIGNED_WORKER.
func TestProviderChangesAndLeases(t *testing.T) {
	w := newProviderWorld(t)
	onAlpha := w.provider(t, apiProvider("on alpha", pb.Provider_ApiEndpoint_SCHEME_X_API_KEY, "sk-fake-alpha", w.worker("alpha")))
	onBeta := w.provider(t, apiProvider("on beta", pb.Provider_ApiEndpoint_SCHEME_X_API_KEY, "sk-fake-beta", w.worker("beta")))
	w.run(t, "alpha", time.Hour)
	w.run(t, "beta", time.Hour)

	s := w.session(t, proto.String(onAlpha.GetProviderId()))
	s.send("hello", pb.InputPriority_INPUT_PRIORITY_NORMAL)
	s.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_COMPLETED)
	held, err := w.h.st.GetLease(s.ctx, s.id)
	must(t, err)
	if held.WorkerID != w.worker("alpha") || held.ProviderID != onAlpha.GetProviderId() {
		t.Fatalf("lease %+v", held)
	}
	w.once(t, "before the change", s.id, "alpha", "ANTHROPIC_API_KEY=sk-fake-alpha", "alpha")
	w.mu.Lock()
	offersBefore := len(w.offers[s.id])
	w.mu.Unlock()
	res, err := w.sessions.UpdateSession(s.ctx, connect.NewRequest(&pb.UpdateSessionRequest{SessionId: s.id, ProviderId: proto.String(onBeta.GetProviderId())}))
	must(t, err)
	if res.Msg.GetProviderId() != onBeta.GetProviderId() {
		t.Fatalf("updated %v", res.Msg)
	}
	s.send("hello beta", pb.InputPriority_INPUT_PRIORITY_NORMAL)
	testwait.For(t, func() bool {
		got, err := w.sessions.GetSession(s.ctx, connect.NewRequest(&pb.GetSessionRequest{SessionId: s.id}))
		must(t, err)
		return got.Msg.GetTurnCount() == 2 && got.Msg.GetState() == pb.Session_STATE_IDLE
	})
	if l, err := w.h.st.GetLease(s.ctx, s.id); err != nil || l.WorkerID != w.worker("beta") || l.Epoch <= held.Epoch || l.ProviderID != onBeta.GetProviderId() {
		t.Fatalf("lease after the change %+v, %v", l, err)
	}
	// One acquisition and one start each: alpha's with alpha's key before
	// the change, beta's with beta's key after it, offered only to beta.
	w.mu.Lock()
	acq, ls, after := w.acquired[s.id], w.envs[s.id], w.offers[s.id][offersBefore:]
	w.mu.Unlock()
	if strings.Join(acq, " ") != "alpha beta" || len(ls) != 2 || ls[1].worker != "beta" ||
		strings.Join(envOf(ls[1].reply), " ") != "ANTHROPIC_API_KEY=sk-fake-beta" {
		t.Fatalf("after the change: acquisitions %v, launches %v", acq, ls)
	}
	for _, o := range after {
		if o != "beta" {
			t.Fatalf("offers after the change %v", after)
		}
	}

	// A scripted holder (worker gamma, no fake worker): unassigning its
	// worker revokes nothing; the next GetLaunchEnv passes the fence and is
	// PROVIDER_UNAVAILABLE.
	gamma := w.workers["gamma"]
	onGamma := w.provider(t, apiProvider("on gamma", pb.Provider_ApiEndpoint_SCHEME_BEARER, "fake-gamma-token", gamma.PrincipalID))
	ctx := testwait.Ctx(t)
	c, err := workerclient.Dial(ctx, workerclient.Options{URL: w.h.base, Token: gamma.Token, PrincipalID: gamma.PrincipalID, BootID: uuid.Must(uuid.NewV7()).String()})
	must(t, err)
	t.Cleanup(func() { _ = c.Close() })
	hints, err := c.Attach(ctx, &pb.Attach{MaxSessions: 1, Agents: workerclient.ClaudeInstalled()})
	must(t, err)
	scripted := w.session(t, proto.String(onGamma.GetProviderId()))
	scripted.send("hello gamma", pb.InputPriority_INPUT_PRIORITY_NORMAL)
	for {
		e, err := hints.Next(ctx)
		must(t, err)
		if e.GetOffer().GetSessionId() == scripted.id {
			break
		}
	}
	op := func(req *pb.SessionRequest) *pb.SessionReply {
		t.Helper()
		r, err := c.Session(ctx, req)
		must(t, err)
		return r
	}
	lease := op(workerclient.Acquire(scripted.id, true)).GetLease()
	if lease.GetEpoch() == 0 {
		t.Fatal("no lease")
	}
	if env := envOf(op(workerclient.LaunchEnv(scripted.id, lease.GetEpoch()))); strings.Join(env, " ") != "ANTHROPIC_AUTH_TOKEN=fake-gamma-token" {
		t.Fatalf("gamma env %v", env)
	}
	onGamma.WorkerIds = nil
	w.update(t, onGamma)
	r := op(workerclient.LaunchEnv(scripted.id, lease.GetEpoch()))
	if r.GetRejected().GetCode() != pb.SessionReply_Rejected_CODE_PROVIDER_UNAVAILABLE {
		t.Fatalf("GetLaunchEnv after unassignment = %v", r)
	}
	if l, err := w.h.st.GetLease(ctx, scripted.id); err != nil || l.WorkerID != gamma.PrincipalID || l.Epoch != int64(lease.GetEpoch()) {
		t.Fatalf("unassignment touched the lease: %+v, %v", l, err)
	}
	if op(workerclient.Release(scripted.id, lease.GetEpoch(), pb.ReleaseLease_REASON_PROVIDER)).GetOk() == nil {
		t.Fatal("release refused")
	}
	if got := scripted.state(pb.Session_STATE_QUEUED, pb.TurnOutcome_TURN_OUTCOME_UNSPECIFIED); got.GetProviderId() != onGamma.GetProviderId() {
		t.Fatalf("queued %v", got)
	}
	testwait.For(t, func() bool {
		got, err := w.sessions.GetSession(ctx, connect.NewRequest(&pb.GetSessionRequest{SessionId: scripted.id}))
		must(t, err)
		return got.Msg.GetQueueReason() == pb.QueueReason_QUEUE_REASON_NO_ASSIGNED_WORKER
	})
	// No LeaseRevoked reached the holder for that lease.
	for {
		hctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		e, err := hints.Next(hctx)
		cancel()
		if err != nil {
			break
		}
		if e.GetLeaseRevoked().GetSessionId() == scripted.id {
			t.Fatalf("lease revoked: %v", e)
		}
	}
}

// TestProviderFullReleasesIdleLease: with max_sessions 1, a session queued
// behind an idle session of the same provider starts promptly: the
// control plane asks the fake worker for the idle lease (ReleaseIdle)
// instead of waiting for its idle timer (an hour here). A holder whose
// session runs a turn keeps it: the waiting session starts once that turn
// has ended, which completes rather than being interrupted.
func TestProviderFullReleasesIdleLease(t *testing.T) {
	w := newProviderWorld(t)
	capped := apiProvider("capped", pb.Provider_ApiEndpoint_SCHEME_X_API_KEY, "sk-fake-capped", w.worker("alpha"))
	capped.MaxSessions = 1
	p := w.provider(t, capped)
	w.run(t, "alpha", time.Hour)

	a := w.session(t, proto.String(p.GetProviderId()))
	a.send("hello a", pb.InputPriority_INPUT_PRIORITY_NORMAL)
	a.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_COMPLETED)
	held, err := w.h.st.GetLease(a.ctx, a.id)
	must(t, err)
	if !held.Live(time.Now().UnixMilli()) {
		t.Fatalf("a's lease after its turn: %+v", held)
	}

	// b waits for the cap only until a's idle lease is given back.
	b := w.session(t, proto.String(p.GetProviderId()))
	begun := time.Now()
	b.send("hello b", pb.InputPriority_INPUT_PRIORITY_NORMAL)
	b.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_COMPLETED)
	if took := time.Since(begun); took > 10*time.Second {
		t.Fatalf("b started after %v", took)
	}
	if l, err := w.h.st.GetLease(a.ctx, a.id); err != nil || l.WorkerID != "" || l.Epoch != held.Epoch {
		t.Fatalf("a's lease after b started: %+v, %v", l, err)
	}
	if got := a.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_COMPLETED); got.GetTurnCount() != 1 {
		t.Fatalf("a after its release: %v", got)
	}

	// a runs a slow turn (b's idle lease is asked back for it); b's next
	// input waits while that turn runs.
	aWatch := a.watch(0)
	a.send("/fake slow 2", pb.InputPriority_INPUT_PRIORITY_NORMAL)
	started := 0
	aWatch.until(func(e *pb.SessionEvent) bool {
		if e.GetKind() == pb.SessionEvent_KIND_TURN_STARTED && !e.GetEphemeral() {
			started++
		}
		return started == 2
	})
	running, err := w.h.st.GetLease(a.ctx, a.id)
	must(t, err)
	b.send("after a", pb.InputPriority_INPUT_PRIORITY_NORMAL)
	if got := b.state(pb.Session_STATE_QUEUED, pb.TurnOutcome_TURN_OUTCOME_COMPLETED); got.GetQueueReason() != pb.QueueReason_QUEUE_REASON_PROVIDER_FULL {
		t.Fatalf("b while a runs: %v", got)
	}
	// a's turn completes on the lease it started with; then b runs.
	if got := a.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_COMPLETED); got.GetTurnCount() != 2 {
		t.Fatalf("a after its slow turn: %v", got)
	}
	testwait.For(t, func() bool {
		got, err := w.sessions.GetSession(b.ctx, connect.NewRequest(&pb.GetSessionRequest{SessionId: b.id}))
		must(t, err)
		return got.Msg.GetTurnCount() == 2 && got.Msg.GetState() == pb.Session_STATE_IDLE
	})
	if l, err := w.h.st.GetLease(a.ctx, a.id); err != nil || l.Epoch != running.Epoch || running.WorkerID == "" {
		t.Fatalf("a's lease: during its turn %+v, after %+v, %v", running, l, err)
	}
	for _, e := range aWatch.all {
		if e.GetKind() == pb.SessionEvent_KIND_TURN_INTERRUPTED {
			t.Fatalf("a's turn was interrupted: %v", e)
		}
	}
}
