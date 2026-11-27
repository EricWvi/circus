//go:build e2e && linux

package e2e

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/gen/go/circus/v1/circusv1connect"
	"github.com/EricWvi/circus/internal/testutil/fakemodel"
	testwait "github.com/EricWvi/circus/internal/testutil/wait"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
)

// The real worker and Claude Code with providers: every endpoint the CLI
// could be pointed at (a provider, the worker's inherited environment, the
// user's or the project's settings) is a path prefix of one recording front
// before the fake model, so a request shows which endpoint and credential
// it used. Fake keys only.

const (
	providerKey     = "sk-ant-api03-circus-e2e-provider-key"
	providerBearer  = "circus-e2e-provider-bearer-token"
	inheritedKey    = "sk-ant-api03-circus-e2e-inherited-key"
	inheritedOAuth  = "sk-ant-oat01-circus-e2e-inherited-oauth"
	customOneValue  = "circus-e2e-custom-one"
	customTwoValue  = "circus-e2e-custom-two"
	providerFable   = "provider-fable-model"
	providerOpus    = "provider-opus-model"
	providerSonnet  = "provider-sonnet-model"
	providerHaiku   = "provider-haiku-model"
	userSettingsKey = "sk-ant-api03-circus-e2e-user-settings-key"
)

// frontRecord is one request as the front saw it.
type frontRecord struct {
	Endpoint, Path, APIKey, Authorization, Model string
}

// recordingFront records the endpoint (first path segment), the
// credential headers and the model of every request, then hands it to the
// fake model without that segment.
type recordingFront struct {
	srv *httptest.Server
	mu  sync.Mutex
	all []frontRecord
}

func newRecordingFront(t *testing.T, fake *fakemodel.Server) *recordingFront {
	t.Helper()
	f := &recordingFront{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		endpoint, rest, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
		body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var m struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &m)
		f.mu.Lock()
		f.all = append(f.all, frontRecord{endpoint, "/" + rest, r.Header.Get("x-api-key"), r.Header.Get("authorization"), m.Model})
		f.mu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.URL.Path, r.URL.RawPath = "/"+rest, ""
		fake.ServeHTTP(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *recordingFront) url(endpoint string) string { return f.srv.URL + "/" + endpoint }

func (f *recordingFront) records() []frontRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]frontRecord(nil), f.all...)
}

// providerStack is a claudeStack with a recording front and the
// browser's provider client.
type providerStack struct {
	*claudeStack
	front *recordingFront
	provs circusv1connect.ProviderServiceClient
}

func newProviderStack(t *testing.T, main fakemodel.Handler, workerEnv ...string) *providerStack {
	t.Helper()
	s := &providerStack{claudeStack: newClaudeStack(t, fakemodel.Options{Main: main}, workerEnv...)}
	s.front = newRecordingFront(t, s.fake)
	jar, err := cookiejar.New(nil)
	must(t, err)
	client := &http.Client{Jar: jar, Transport: http.DefaultTransport.(*http.Transport).Clone()}
	t.Cleanup(client.CloseIdleConnections)
	_, err = circusv1connect.NewAuthServiceClient(client, s.h.base).Login(testwait.Ctx(t), connect.NewRequest(&pb.LoginRequest{
		Email: enrollmentEmail, Password: enrollmentPassword,
	}))
	must(t, err)
	s.provs = circusv1connect.NewProviderServiceClient(client, s.h.base)
	return s
}

// startProviderWorker starts the worker with its inherited endpoint and key
// on the front's "inherited" endpoint, plus extra.
func (s *providerStack) startProviderWorker(extra ...string) *child {
	s.t.Helper()
	return s.startWorker(append([]string{"ANTHROPIC_BASE_URL=" + s.front.url("inherited"), "ANTHROPIC_API_KEY=" + inheritedKey}, extra...)...)
}

// provider creates a provider on endpoint with the four alias models and
// two custom variables, assigned to the stack's worker.
func (s *providerStack) provider(name, endpoint string, scheme pb.Provider_ApiEndpoint_Scheme, key string) *pb.Provider {
	s.t.Helper()
	res, err := s.provs.CreateProvider(s.ctx, connect.NewRequest(&pb.CreateProviderRequest{Provider: &pb.Provider{
		Name: name, Kind: pb.Provider_KIND_CLAUDE_CODE,
		Api: &pb.Provider_ApiEndpoint{
			BaseUrl: s.front.url(endpoint), Scheme: scheme, Key: key,
			FableModel: providerFable, OpusModel: providerOpus, SonnetModel: providerSonnet, HaikuModel: providerHaiku,
		},
		Env: []*pb.Provider_EnvVar{
			{Name: "E2E_PROVIDER_CUSTOM_ONE", Value: proto.String(customOneValue)},
			{Name: "E2E_PROVIDER_CUSTOM_TWO", Value: proto.String(customTwoValue)},
		},
		WorkerIds: []string{s.h.worker.PrincipalID},
	}}))
	must(s.t, err)
	return res.Msg.GetProvider()
}

func (s *providerStack) assign(p *pb.Provider, workers ...string) *pb.Provider {
	s.t.Helper()
	p.WorkerIds = workers
	// Secrets are write-only: an update without them keeps them.
	p.Api.Key = ""
	for _, v := range p.Env {
		v.Value = nil
	}
	res, err := s.provs.UpdateProvider(s.ctx, connect.NewRequest(&pb.UpdateProviderRequest{Provider: p}))
	must(s.t, err)
	return res.Msg.GetProvider()
}

// openWith creates a local session of the worker with a provider ("" for
// local login) and an empty model.
func (s *providerStack) openWith(providerID string) *sessionBrowser {
	s.t.Helper()
	res, err := s.api.CreateSession(s.ctx, connect.NewRequest(&pb.CreateSessionRequest{
		ClientRequestId: uuid.NewString(), ProjectId: s.project, ProviderId: proto.String(providerID),
	}))
	must(s.t, err)
	session := res.Msg.GetSession()
	if !session.GetConfig().GetLocal() || session.GetPinnedWorkerId() != s.h.worker.PrincipalID ||
		session.GetProviderId() != providerID || session.GetConfig().GetModel() != "" {
		s.t.Fatalf("session: %v", session)
	}
	return &sessionBrowser{t: s.t, ctx: s.ctx, api: s.api, id: session.GetSessionId()}
}

// turn sends text and waits for the turn's result and the idle state.
func (s *providerStack) turn(sess *sessionBrowser, w *sessionWatch, text string) []*pb.SessionEvent {
	s.t.Helper()
	sess.send(text, pb.InputPriority_INPUT_PRIORITY_NORMAL)
	events := durableUntil(s.t, w, func(e *pb.SessionEvent) bool { return e.GetKind() == pb.SessionEvent_KIND_RESULT })
	sess.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_COMPLETED)
	return events
}

// checkEndpoint requires every request the front saw to have used endpoint
// with exactly the given headers, the Messages requests with model, and
// every request of the fake to have come through the front.
func (s *providerStack) checkEndpoint(endpoint, apiKey, authorization, model string) {
	s.t.Helper()
	records := s.front.records()
	messages := 0
	for _, r := range records {
		if r.Endpoint != endpoint || r.APIKey != apiKey || r.Authorization != authorization {
			s.t.Errorf("request %+v, want endpoint %q, x-api-key %q, authorization %q", r, endpoint, apiKey, authorization)
		}
		if r.Path == "/v1/messages" {
			messages++
			if r.Model != model {
				s.t.Errorf("request %+v, want model %q", r, model)
			}
		}
	}
	if messages == 0 {
		s.t.Errorf("no Messages request through the front: %+v", records)
	}
	if n := len(s.fake.Requests()); n != len(records) {
		s.t.Errorf("the fake saw %d requests, the front %d: some bypassed the front", n, len(records))
	}
}

// envScript answers "show the environment" with a Bash `env`, then text;
// "say hello" with text; the newest marker of the conversation decides.
func envScript(log *scriptLog) fakemodel.Handler {
	return func(r *fakemodel.Request) *fakemodel.Response {
		switch {
		case r.Contains("show the environment"):
			if r.Assistants() == 0 {
				return fakemodel.Tools(fakemodel.Bash("toolu_env", "env | sort"))
			}
			if b, ok := r.ToolResult("toolu_env"); !ok || b.IsError {
				return log.fail("env: result %+v", b)
			}
			return fakemodel.Text("Environment shown.")
		case r.Contains("say hello"):
			return fakemodel.Text("Hello.")
		}
		return log.fail("unexpected request: %.300s", r.Last().Text())
	}
}

// checkToolEnv requires the env tool result of the session's request to
// show the provider's custom variables and no credential.
func (s *providerStack) checkToolEnv(marker string, secrets ...string) {
	s.t.Helper()
	r := s.waitFake(func(r *fakemodel.Request) bool { return r.Contains(marker) && r.Assistants() == 1 })
	b, ok := r.ToolResult("toolu_env")
	if !ok || b.IsError {
		s.t.Fatalf("env tool result %+v", b)
	}
	out := b.ResultText()
	for _, want := range []string{"E2E_PROVIDER_CUSTOM_ONE=" + customOneValue, "E2E_PROVIDER_CUSTOM_TWO=" + customTwoValue} {
		if !strings.Contains(out, want) {
			s.t.Errorf("the Bash environment lacks %s:\n%s", want, out)
		}
	}
	for _, name := range []string{"ANTHROPIC_API_KEY=", "ANTHROPIC_AUTH_TOKEN=", "CLAUDE_CODE_OAUTH_TOKEN="} {
		if strings.Contains(out, name) {
			s.t.Errorf("the Bash environment has %s:\n%s", name, out)
		}
	}
	for _, secret := range secrets {
		if strings.Contains(out, secret) {
			s.t.Errorf("the Bash environment shows a credential:\n%s", out)
		}
	}
}

// TestClaudeProviderSchemes: a provider with an x-api-key and one with a
// bearer token, each in its own session with an empty model, and local
// login: only that session's endpoint, credential header and model reach
// the fake, and a Bash command sees the provider's custom variables but no
// credential.
func TestClaudeProviderSchemes(t *testing.T) {
	var log scriptLog
	s := newProviderStack(t, envScript(&log))
	s.startProviderWorker()
	xKey := s.provider("x-api-key", "xkey", pb.Provider_ApiEndpoint_SCHEME_X_API_KEY, providerKey)
	bearer := s.provider("bearer", "bearer", pb.Provider_ApiEndpoint_SCHEME_BEARER, providerBearer)

	for _, c := range []struct {
		name, provider, endpoint, apiKey, authorization string
	}{
		{"x-api-key", xKey.GetProviderId(), "xkey", providerKey, ""},
		{"bearer", bearer.GetProviderId(), "bearer", "", "Bearer " + providerBearer},
	} {
		t.Run(c.name, func(t *testing.T) {
			sess := s.openWith(c.provider)
			w := sess.watch(0)
			logOnFailure(t, w)
			w.expect(pb.SessionEvent_KIND_UNSPECIFIED)
			before := len(s.front.records())
			s.turn(sess, w, "show the environment of "+c.name)
			records := s.front.records()[before:]
			messages := 0
			for _, r := range records {
				if r.Endpoint != c.endpoint || r.APIKey != c.apiKey || r.Authorization != c.authorization {
					t.Errorf("request %+v, want endpoint %q, x-api-key %q, authorization %q", r, c.endpoint, c.apiKey, c.authorization)
				}
				if r.Path == "/v1/messages" {
					messages++
					if r.Model != providerOpus {
						t.Errorf("request %+v, want the provider's opus model", r)
					}
				}
			}
			if messages != 2 {
				t.Errorf("want 2 Messages requests (tool use, answer), got %+v", records)
			}
			s.checkToolEnv("show the environment of "+c.name, providerKey, providerBearer, inheritedKey)
		})
	}

	t.Run("local login", func(t *testing.T) {
		sess := s.openWith("")
		w := sess.watch(0)
		logOnFailure(t, w)
		w.expect(pb.SessionEvent_KIND_UNSPECIFIED)
		before := len(s.front.records())
		s.turn(sess, w, "say hello with the worker's own login")
		records := s.front.records()[before:]
		if len(records) == 0 {
			t.Fatal("no request through the front")
		}
		for _, r := range records {
			if r.Endpoint != "inherited" || r.APIKey != inheritedKey || r.Authorization != "" {
				t.Errorf("local login request %+v, want the inherited endpoint and key", r)
			}
			if r.Path == "/v1/messages" && strings.HasPrefix(r.Model, "provider-") {
				t.Errorf("local login request %+v used a provider model", r)
			}
		}
	})
	if n := len(s.fake.Requests()); n != len(s.front.records()) {
		t.Errorf("the fake saw %d requests, the front %d: some bypassed the front", n, len(s.front.records()))
	}
	log.check(t)
}

// TestClaudeProviderConflictingConfig: the worker machine is configured
// against the provider: its inherited environment carries another key,
// endpoint and an OAuth token; the user's settings.json sets env, an
// apiKeyHelper and a model; the project's settings.local.json sets its own
// env and model. Only the provider's endpoint, key and opus model are used.
func TestClaudeProviderConflictingConfig(t *testing.T) {
	var log scriptLog
	s := newProviderStack(t, envScript(&log))
	settings := map[string]any{
		"env": map[string]string{
			"ANTHROPIC_BASE_URL": s.front.url("user"), "ANTHROPIC_API_KEY": userSettingsKey,
			"ANTHROPIC_AUTH_TOKEN": "circus-e2e-user-settings-token", "ANTHROPIC_MODEL": "user-env-model",
			"ANTHROPIC_DEFAULT_OPUS_MODEL": "user-opus-model",
		},
		"apiKeyHelper": "echo sk-ant-api03-circus-e2e-helper-key",
		"model":        "user-settings-model",
	}
	data, err := json.Marshal(settings)
	must(t, err)
	must(t, os.WriteFile(filepath.Join(s.config, "settings.json"), data, 0o600))
	must(t, os.MkdirAll(filepath.Join(s.dir, ".claude"), 0o700))
	project := map[string]any{
		"env": map[string]string{
			"ANTHROPIC_BASE_URL": s.front.url("project"), "ANTHROPIC_API_KEY": "sk-ant-api03-circus-e2e-project-key",
			"ANTHROPIC_DEFAULT_OPUS_MODEL": "project-opus-model", "CLAUDE_CODE_USE_BEDROCK": "1",
		},
		"model": "project-settings-model",
	}
	data, err = json.Marshal(project)
	must(t, err)
	must(t, os.WriteFile(filepath.Join(s.dir, ".claude", "settings.local.json"), data, 0o600))
	s.startProviderWorker("CLAUDE_CODE_OAUTH_TOKEN="+inheritedOAuth, "CLAUDE_CODE_SUBAGENT_MODEL=inherited-subagent-model")
	p := s.provider("x-api-key", "provider", pb.Provider_ApiEndpoint_SCHEME_X_API_KEY, providerKey)

	sess := s.openWith(p.GetProviderId())
	w := sess.watch(0)
	logOnFailure(t, w)
	w.expect(pb.SessionEvent_KIND_UNSPECIFIED)
	s.turn(sess, w, "show the environment despite the settings")
	s.checkEndpoint("provider", providerKey, "", providerOpus)
	s.checkToolEnv("show the environment despite the settings", providerKey, inheritedKey, inheritedOAuth, userSettingsKey)
	log.check(t)
}

// workerLogged waits for a JSON log line of the worker with message and
// the given fields.
func workerLogged(t *testing.T, worker *child, message string, fields map[string]string) {
	t.Helper()
	wait(t, 20*time.Second, func() bool {
		for _, line := range strings.Split(worker.stderr.String(), "\n") {
			var l map[string]any
			if json.Unmarshal([]byte(line), &l) != nil || l["message"] != message {
				continue
			}
			match := true
			for k, v := range fields {
				if s, _ := l[k].(string); s != v {
					match = false
				}
			}
			if match {
				return true
			}
		}
		return false
	})
}

// TestClaudeProviderUnassigned: the provider is unassigned from the worker
// while it holds the session's lease with no CLI running (the previous
// one ended after a failed login). The next input's CLI start gets
// PROVIDER_UNAVAILABLE: no CLI starts, the lease is released with
// PROVIDER and the session queues with NO_ASSIGNED_WORKER. Assigning the
// provider again lets the queued input run.
func TestClaudeProviderUnassigned(t *testing.T) {
	var log scriptLog
	main := func(r *fakemodel.Request) *fakemodel.Response {
		switch {
		case r.Contains("after the reassignment"):
			return fakemodel.Text("Answer after the reassignment.")
		case r.Contains("fail the login"):
			return &fakemodel.Response{Status: 401, ErrorType: "authentication_error", Error: "fake: invalid x-api-key"}
		}
		return log.fail("unexpected request: %.300s", r.Last().Text())
	}
	s := newProviderStack(t, main)
	worker := s.startProviderWorker()
	p := s.provider("x-api-key", "provider", pb.Provider_ApiEndpoint_SCHEME_X_API_KEY, providerKey)
	sess := s.openWith(p.GetProviderId())
	w := sess.watch(0)
	logOnFailure(t, w)
	w.expect(pb.SessionEvent_KIND_UNSPECIFIED)

	sess.send("fail the login", pb.InputPriority_INPUT_PRIORITY_NORMAL)
	durableUntil(t, w, func(e *pb.SessionEvent) bool { return e.GetKind() == pb.SessionEvent_KIND_RESULT })
	sess.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_ERROR)
	// The worker replaces the failed login's query: its CLI exits, the lease stays.
	workerLogged(t, worker, "Claude Code failed authentication; the next input starts a new query", map[string]string{"session_id": sess.id})
	wait(t, 20*time.Second, func() bool {
		for _, r := range s.cliRecords() {
			if r.SessionID == sess.id && processAlive(r.PID, r.Start) {
				return false
			}
		}
		return true
	})
	held, err := s.h.st.GetLease(s.ctx, sess.id)
	must(t, err)
	if held.WorkerID != s.h.worker.PrincipalID {
		t.Fatalf("lease after the failed login: %+v", held)
	}
	requests := len(s.fake.Requests())

	p = s.assign(p)
	sess.send("after the reassignment", pb.InputPriority_INPUT_PRIORITY_NORMAL)
	workerLogged(t, worker, "session lease released", map[string]string{"session_id": sess.id, "reason": "PROVIDER"})
	queued := sess.state(pb.Session_STATE_QUEUED, pb.TurnOutcome_TURN_OUTCOME_ERROR)
	if queued.GetProviderId() != p.GetProviderId() {
		t.Fatalf("queued %v", queued)
	}
	testwait.For(t, func() bool {
		got, err := s.api.GetSession(s.ctx, connect.NewRequest(&pb.GetSessionRequest{SessionId: sess.id}))
		must(t, err)
		return got.Msg.GetQueueReason() == pb.QueueReason_QUEUE_REASON_NO_ASSIGNED_WORKER
	})
	if released, err := s.h.st.GetLease(s.ctx, sess.id); err != nil || released.WorkerID != "" {
		t.Fatalf("lease after PROVIDER_UNAVAILABLE: %+v, %v", released, err)
	}
	// No CLI started for the queued input.
	for _, r := range s.cliRecords() {
		if r.SessionID == sess.id && processAlive(r.PID, r.Start) {
			t.Fatalf("a CLI runs for the unassigned provider: %+v", r)
		}
	}
	if n := len(s.fake.Requests()); n != requests {
		t.Fatalf("the fake got %d requests after the unassignment", n-requests)
	}

	s.assign(p, s.h.worker.PrincipalID)
	events := durableUntil(t, w, func(e *pb.SessionEvent) bool { return e.GetKind() == pb.SessionEvent_KIND_RESULT })
	if got := eventString(t, events[len(events)-1], "result"); got != "Answer after the reassignment." {
		t.Fatalf("after the reassignment: %v", events)
	}
	sess.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_COMPLETED)
	s.checkEndpoint("provider", providerKey, "", providerOpus)
	log.check(t)
}

// TestClaudeProviderResume: after an idle release, the next input resumes
// the session from the control plane's store; the SDK runs that CLI with a
// temporary config directory, and the provider's credentials are still the
// ones used.
func TestClaudeProviderResume(t *testing.T) {
	var log scriptLog
	answering := fakemodel.NewGate()
	main := func(r *fakemodel.Request) *fakemodel.Response {
		switch {
		case r.Contains("second provider input"):
			if r.Count("first provider input") != 1 || r.Count("First answer.") != 1 {
				return log.fail("resumed request lacks the history: %.3000s", r.Text())
			}
			return &fakemodel.Response{Text: "Second answer.", Hold: answering}
		case r.Contains("first provider input"):
			return fakemodel.Text("First answer.")
		}
		return log.fail("unexpected request: %.300s", r.Last().Text())
	}
	s := newProviderStack(t, main, "CIRCUS_WORKER_IDLE_SECONDS=1")
	s.startProviderWorker()
	p := s.provider("bearer", "provider", pb.Provider_ApiEndpoint_SCHEME_BEARER, providerBearer)
	sess := s.openWith(p.GetProviderId())
	w := sess.watch(0)
	logOnFailure(t, w)
	w.expect(pb.SessionEvent_KIND_UNSPECIFIED)
	s.turn(sess, w, "first provider input")
	first := s.liveCLI(sess.id)
	held, err := s.h.st.GetLease(s.ctx, sess.id)
	must(t, err)
	wait(t, 20*time.Second, func() bool {
		l, err := s.h.st.GetLease(s.ctx, sess.id)
		must(t, err)
		return l.WorkerID == "" && !processAlive(first.PID, first.Start)
	})

	sess.send("second provider input", pb.InputPriority_INPUT_PRIORITY_NORMAL)
	streamed(t, w, answering)
	// The resumed CLI runs with the SDK's temporary config directory.
	resumed := s.liveCLI(sess.id)
	if dir := resumeDirOf(t, s.claudeStack, resumed); !strings.Contains(filepath.Base(dir), "claude-resume-") {
		t.Fatalf("resumed CLI's config directory %q", dir)
	}
	answering.Release()
	durableUntil(t, w, func(e *pb.SessionEvent) bool { return e.GetKind() == pb.SessionEvent_KIND_RESULT })
	if state := sess.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_COMPLETED); state.GetTurnCount() != 2 {
		t.Fatalf("after the resume: %v", state)
	}
	again, err := s.h.st.GetLease(s.ctx, sess.id)
	must(t, err)
	if again.Epoch <= held.Epoch {
		t.Fatalf("lease before the release %+v, after %+v", held, again)
	}
	s.checkEndpoint("provider", "", "Bearer "+providerBearer, providerOpus)
	s.checkTranscript(sess.id)
	log.check(t)
}

// resumeDirOf reads the SDK's resume directory from the worker's record of
// a CLI.
func resumeDirOf(t *testing.T, s *claudeStack, r cliRecord) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(s.runtime, "circus-worker-"+strconv.Itoa(os.Getuid()), r.file))
	must(t, err)
	var rec struct {
		ResumeDir *struct {
			Dir string `json:"dir"`
		} `json:"resumeDir"`
	}
	must(t, json.Unmarshal(data, &rec))
	if rec.ResumeDir == nil {
		return ""
	}
	return rec.ResumeDir.Dir
}

// TestClaudeProviderFullAfterRestart: with max_sessions 1, the real
// worker gives an idle lease back as soon as another session of the
// provider waits, also for a lease it resumed after a control-plane
// restart (whose resumption raises Nudge reasons the worker must
// acknowledge, JOB included), long before its idle timer (300 s). The
// released session's transcript keeps what the CLI wrote after its last
// result (cost-state).
func TestClaudeProviderFullAfterRestart(t *testing.T) {
	var log scriptLog
	main := func(r *fakemodel.Request) *fakemodel.Response {
		switch {
		case r.Contains("first of b"):
			return fakemodel.Text("B answered.")
		case r.Contains("first of a"):
			return fakemodel.Text("A answered.")
		}
		return log.fail("unexpected request: %.300s", r.Last().Text())
	}
	s := newProviderStack(t, main)
	s.startProviderWorker()
	p := s.provider("capped", "provider", pb.Provider_ApiEndpoint_SCHEME_BEARER, providerBearer)
	p.MaxSessions = 1
	p = s.assign(p, s.h.worker.PrincipalID)
	if p.GetMaxSessions() != 1 {
		t.Fatalf("provider %v", p)
	}
	a := s.openWith(p.GetProviderId())
	wa := a.watch(0)
	logOnFailure(t, wa)
	wa.expect(pb.SessionEvent_KIND_UNSPECIFIED)
	s.turn(a, wa, "first of a")
	cli := s.liveCLI(a.id)
	before, err := s.h.st.GetLease(s.ctx, a.id)
	must(t, err)
	must(t, wa.stream.Close())

	s.h.server.signal(t, syscall.SIGTERM)
	must(t, s.h.server.wait(t, 10*time.Second))
	s.h.startServer(false)
	wait(t, 20*time.Second, func() bool {
		l, err := s.h.st.GetLease(s.ctx, a.id)
		must(t, err)
		return l.ConnID != "" && l.ConnID != before.ConnID
	})

	b := s.openWith(p.GetProviderId())
	wb := b.watch(0)
	logOnFailure(t, wb)
	wb.expect(pb.SessionEvent_KIND_UNSPECIFIED)
	begun := time.Now()
	s.turn(b, wb, "first of b")
	if took := time.Since(begun); took > 30*time.Second {
		t.Fatalf("b ran after %v", took)
	}
	released, err := s.h.st.GetLease(s.ctx, a.id)
	must(t, err)
	if released.WorkerID != "" || released.Epoch != before.Epoch {
		t.Fatalf("a's lease before the restart %+v, after b ran %+v", before, released)
	}
	reapExited(t, cli.PID, cli.Start, 20*time.Second)
	costState := false
	for _, e := range s.transcript(a.id) {
		costState = costState || (e.Subpath == "" && strings.Contains(e.JSON, `"type":"cost-state"`))
	}
	if !costState {
		t.Fatal("a's transcript lacks the CLI's cost-state")
	}
	s.checkTranscript(a.id)
	s.checkTranscript(b.id)
	log.check(t)
}
