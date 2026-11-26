package fakeworker_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/gen/go/circus/v1/circusv1connect"
	"github.com/EricWvi/circus/internal/auth"
	"github.com/EricWvi/circus/internal/controlplane"
	"github.com/EricWvi/circus/internal/credfile"
	"github.com/EricWvi/circus/internal/daemon"
	"github.com/EricWvi/circus/internal/daemon/bashtool"
	"github.com/EricWvi/circus/internal/daemon/jobs"
	"github.com/EricWvi/circus/internal/daemon/link"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/store/sqlite"
	"github.com/EricWvi/circus/internal/store/sqlite/sqlitetest"
	"github.com/EricWvi/circus/internal/testutil/fakeworker"
	"github.com/google/uuid"
)

var updateWebFixture = flag.Bool("update", false, "rewrite the Web UI's recording of the fake worker's session events")

// webFixture is what the Web UI's transcript reducer is tested against
// (web/src/lib/session-events/transcript.test.ts).
const webFixture = "../../../web/src/lib/session-events/fake-worker.fixture.json"

// TestWebEventFixture records, for each scripted scenario, what WatchSession
// streams from a real control plane while the fake worker serves the
// session and a real daemon runs its tools, and compares it with the Web
// UI's fixture: a change of the event shapes fails here until the fixture
// is regenerated with -update (and the Web tests run against it).
//
// Ids are numbered; timestamps, durations and usage keep only their JSON
// type; keepalives are left out. Each scenario runs in a session of its own.
func TestWebEventFixture(t *testing.T) {
	if testing.Short() {
		t.Skip("records live sessions for about a minute")
	}
	env := startStack(t)
	got := map[string]recording{}
	for _, sc := range webScenarios {
		t.Run(sc.name, func(t *testing.T) {
			got[sc.name] = env.record(t, sc)
		})
	}
	if t.Failed() {
		return
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", " ")
	if err := enc.Encode(got); err != nil {
		t.Fatal(err)
	}
	want := buf.Bytes()
	if *updateWebFixture {
		if err := os.WriteFile(webFixture, want, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	file, err := os.ReadFile(webFixture)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(file, want) {
		return
	}
	fileLines, wantLines := strings.Split(string(file), "\n"), strings.Split(string(want), "\n")
	for i := range max(len(fileLines), len(wantLines)) {
		var f, w string
		if i < len(fileLines) {
			f = fileLines[i]
		}
		if i < len(wantLines) {
			w = wantLines[i]
		}
		if f != w {
			t.Errorf("%s line %d is out of date:\n  file: %.300s\n  now:  %.300s", webFixture, i+1, f, w)
			break
		}
	}
	t.Errorf("the fake worker's session events differ from %s; regenerate it with -update and run the Web tests", webFixture)
}

type recording struct {
	Final  final          `json:"final"`
	Events []fixtureEvent `json:"events"`
}

type final struct {
	State                string `json:"state"`
	LastTurnOutcome      string `json:"lastTurnOutcome"`
	PendingApprovalCount uint32 `json:"pendingApprovalCount"`
}

type fixtureEvent struct {
	Seq       uint64 `json:"seq"`
	Kind      string `json:"kind"`
	Ephemeral bool   `json:"ephemeral"`
	Truncated bool   `json:"truncated"`
	Payload   any    `json:"payload"`
}

type scenario struct {
	name string
	run  func(s *liveSession)
}

const (
	kindResult   = pb.SessionEvent_KIND_RESULT
	kindAssist   = pb.SessionEvent_KIND_ASSISTANT
	kindPending  = pb.SessionEvent_KIND_APPROVAL_PENDING
	kindResolved = pb.SessionEvent_KIND_APPROVAL_RESOLVED
)

// The scenarios that interrupt a turn act right after the first streamed
// chunk of `/fake slow 20`, whose chunks are five seconds apart, or while
// `sleep 20` runs (the longest sleep Bash allows): what was recorded doesn't depend on how fast the machine
// is.
var webScenarios = []scenario{
	{"text", func(s *liveSession) { s.send("hello"); s.await(count(kindResult, 1)) }},
	{"think", func(s *liveSession) { s.send("/fake think"); s.await(count(kindResult, 1)) }},
	{"tool", func(s *liveSession) { s.send("/fake bash echo hi"); s.await(count(kindResult, 1)) }},
	{"parallel_tools", func(s *liveSession) { s.send("/fake parallel echo hi"); s.await(count(kindResult, 1)) }},
	{"truncated_tool_result", func(s *liveSession) {
		// Over the 256 KiB view payload cap: stored truncated, without bytes.
		s.send("/fake large 300000")
		s.await(count(kindResult, 1))
	}},
	{"approve", func(s *liveSession) {
		s.send("/fake approve echo approved")
		s.await(count(kindResult, 1))
		s.resolve(true, nil)
		s.await(count(kindResult, 2))
	}},
	{"deny", func(s *liveSession) {
		s.send("/fake approve echo denied")
		s.await(count(kindResult, 1))
		s.resolve(false, nil)
		s.await(count(kindResolved, 1))
	}},
	{"interrupt", func(s *liveSession) {
		s.send("/fake slow 20")
		s.await(textDelta)
		s.call(func(ctx context.Context) error {
			_, err := s.api.Interrupt(ctx, connect.NewRequest(&pb.InterruptRequest{SessionId: s.id}))
			return err
		})
		s.await(count(pb.SessionEvent_KIND_TURN_STOPPED, 1))
	}},
	{"interrupt_tool", func(s *liveSession) {
		s.send("/fake bash sleep 20")
		s.await(func(e []*pb.SessionEvent) bool { return has(e, kindAssist, `"tool_use"`) })
		// Give the call time to reach the device; its result reads the same either way.
		time.Sleep(500 * time.Millisecond)
		s.call(func(ctx context.Context) error {
			_, err := s.api.Interrupt(ctx, connect.NewRequest(&pb.InterruptRequest{SessionId: s.id}))
			return err
		})
		s.await(count(pb.SessionEvent_KIND_TURN_STOPPED, 1))
	}},
	{"now", func(s *liveSession) {
		s.send("/fake slow 20")
		s.await(textDelta)
		s.sendWith("right now", pb.InputPriority_INPUT_PRIORITY_NOW)
		s.await(count(kindResult, 2))
	}},
	{"stop", func(s *liveSession) {
		s.send("/fake slow 20")
		s.await(textDelta)
		s.call(func(ctx context.Context) error {
			_, err := s.api.StopSession(ctx, connect.NewRequest(&pb.StopSessionRequest{SessionId: s.id}))
			return err
		})
		s.await(count(pb.SessionEvent_KIND_TURN_INTERRUPTED, 1))
	}},
	{"crash", func(s *liveSession) {
		s.send("/fake crash")
		s.await(count(pb.SessionEvent_KIND_TURN_INTERRUPTED, 1))
		// The next scenario starts with the worker back.
		s.awaitWorker()
	}},
	{"crash_recover", func(s *liveSession) {
		s.send("/fake crash")
		s.await(count(pb.SessionEvent_KIND_TURN_INTERRUPTED, 1))
		s.awaitWorker()
		s.send("after the crash")
		s.await(count(kindResult, 1))
	}},
	{"broken", func(s *liveSession) {
		s.send("hello")
		s.await(count(kindResult, 1))
		s.send("/fake break")
		s.awaitState(pb.Session_STATE_BROKEN)
		s.send("queued while broken")
		s.call(func(ctx context.Context) error {
			_, err := s.api.RecoverSession(ctx, connect.NewRequest(&pb.RecoverSessionRequest{SessionId: s.id, AcceptTranscriptGap: true}))
			return err
		})
		s.await(count(kindResult, 3))
	}},
	{"error", func(s *liveSession) { s.send("/fake error"); s.await(count(kindResult, 1)) }},
	// Last: it moves the control plane's clock past the approval's lifetime.
	{"approval_expired", func(s *liveSession) {
		s.send("/fake approve echo late")
		s.await(count(kindResult, 1))
		s.env.skew.Add(int64(11 * time.Minute))
		s.resolve(true, func(err error) bool { return connect.CodeOf(err) == connect.CodeFailedPrecondition })
		s.await(count(kindResolved, 1))
	}},
}

func count(kind pb.SessionEvent_Kind, n int) func([]*pb.SessionEvent) bool {
	return func(events []*pb.SessionEvent) bool {
		seen := 0
		for _, e := range events {
			if e.GetKind() == kind && !e.GetEphemeral() {
				seen++
			}
		}
		return seen >= n
	}
}

func has(events []*pb.SessionEvent, kind pb.SessionEvent_Kind, text string) bool {
	for _, e := range events {
		if e.GetKind() == kind && !e.GetEphemeral() && strings.Contains(e.GetPayloadJson(), text) {
			return true
		}
	}
	return false
}

func textDelta(events []*pb.SessionEvent) bool {
	for _, e := range events {
		if e.GetEphemeral() && strings.Contains(e.GetPayloadJson(), `"text_delta"`) {
			return true
		}
	}
	return false
}

// stack is a control plane with one Web user, a daemon (device "box") and
// the fake worker, all in this process.
type stack struct {
	api     circusv1connect.SessionServiceClient
	project string
	// skew moves the control plane's clock forward.
	skew atomic.Int64
}

func startStack(t *testing.T) *stack {
	t.Helper()
	env := &stack{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "circus.db")
	sqlitetest.Seed(t, dbPath)
	st, err := sqlite.Open(ctx, dbPath, sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	logs := &syncBuffer{}
	srv, err := controlplane.New(ctx, st, controlplane.Config{
		Listen: "127.0.0.1:0", DevInsecure: true, LogOutput: logs,
		Now: func() time.Time { return time.Now().Add(time.Duration(env.skew.Load())) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	base := "http://" + srv.Addr().String()

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	setup, err := circusv1connect.NewAuthServiceClient(client, base).Setup(ctx, connect.NewRequest(&pb.SetupRequest{
		Token: setupToken(t, logs.String()), Email: "web@example.com", Password: "password-123",
	}))
	if err != nil {
		t.Fatal(err)
	}
	userID := setup.Msg.GetUser().GetUserId()
	enroll := func(kind store.PrincipalKind, name string) credfile.File {
		token, _, err := auth.CreateEnrollToken(ctx, st, userID, kind, time.Hour, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		e, err := auth.Enroll(ctx, st, auth.EnrollRequest{Token: token, Kind: kind, Name: name, MachineID: "machine-" + name, OS: "linux", Arch: "amd64"}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		role := credfile.RoleWorker
		if kind == store.KindDevice {
			role = credfile.RoleDaemon
		}
		return credfile.File{ControlPlaneURL: base, Role: role, PrincipalID: e.Identity.ID, Token: e.Token}
	}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	runCtx, stop := context.WithCancel(ctx)
	var wg sync.WaitGroup
	daemon, err := link.New(link.Options{
		Credentials: enroll(store.KindDevice, "box"), Logger: quiet,
		MachineIDSource: func(context.Context) (string, error) { return "machine-box", nil },
		// Nothing of the daemon's lands in the real home directory.
		Tools: func(m *jobs.Manager) []daemon.Tool {
			return daemon.DefaultTools(m, bashtool.Options{
				Home: filepath.Join(dir, "home"), StateDir: filepath.Join(dir, "state"), TempDir: filepath.Join(dir, "tmp"),
			})
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	wg.Go(func() {
		if err := daemon.Run(runCtx); err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("daemon: %v", err)
		}
	})
	worker := enroll(store.KindWorker, "worker")
	wg.Go(func() {
		err := fakeworker.Run(runCtx, fakeworker.Options{Credential: worker, MaxSessions: 32, StreamDelay: 20 * time.Millisecond, Logger: quiet})
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("fake worker: %v", err)
		}
	})
	t.Cleanup(func() {
		stop()
		wg.Wait()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
		client.CloseIdleConnections()
	})

	work := filepath.Join(dir, "work")
	for _, d := range []string{work, filepath.Join(dir, "home"), filepath.Join(dir, "state"), filepath.Join(dir, "tmp")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	project, err := circusv1connect.NewProjectServiceClient(client, base).CreateProject(ctx, connect.NewRequest(&pb.CreateProjectRequest{
		Name: "fixture", Locations: []*pb.Project_Location{{Device: "box", Path: work}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	env.api = circusv1connect.NewSessionServiceClient(client, base)
	env.project = project.Msg.GetProject().GetProjectId()
	env.probe(ctx, t)
	return env
}

// probe runs a turn in a session of its own and returns once it completed,
// which proves the worker attached: an Offer goes only to an attached
// connection of the worker's current boot. A recorded session queued while
// the worker is not attached would wait with QUEUE_REASON_WORKER_OFFLINE,
// and its KIND_QUEUE events would enter the recording. That holds before
// the first scenario and after a scripted crash, whose new boot releases
// the old boot's leases (interrupting its turn) in its handshake, before it
// attaches.
func (env *stack) probe(ctx context.Context, t *testing.T) {
	t.Helper()
	probe, err := env.api.CreateSession(ctx, connect.NewRequest(&pb.CreateSessionRequest{ProjectId: env.project, ClientRequestId: uuid.NewString()}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.api.SendMessage(ctx, connect.NewRequest(&pb.SendMessageRequest{SessionId: probe.Msg.GetSession().GetSessionId(), ClientMsgId: "probe", Text: "probe"})); err != nil {
		t.Fatal(err)
	}
	for {
		got, err := env.api.GetSession(ctx, connect.NewRequest(&pb.GetSessionRequest{SessionId: probe.Msg.GetSession().GetSessionId()}))
		if err != nil {
			t.Fatal(err)
		}
		if got.Msg.GetLastTurnOutcome() == pb.TurnOutcome_TURN_OUTCOME_COMPLETED {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("the probe turn did not complete")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func setupToken(t *testing.T, logs string) string {
	t.Helper()
	for line := range strings.SplitSeq(logs, "\n") {
		var rec struct {
			URL string `json:"url"`
		}
		if json.Unmarshal([]byte(line), &rec) == nil && rec.URL != "" {
			u, err := url.Parse(rec.URL)
			if err == nil && u.Query().Get("token") != "" {
				return u.Query().Get("token")
			}
		}
	}
	t.Fatal("no setup link in the server log")
	return ""
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// liveSession is one scenario's session and what its stream has sent.
type liveSession struct {
	t   *testing.T
	env *stack
	api circusv1connect.SessionServiceClient
	ctx context.Context
	id  string

	mu     sync.Mutex
	events []*pb.SessionEvent
	more   chan struct{}
}

func (env *stack) record(t *testing.T, sc scenario) recording {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	created, err := env.api.CreateSession(ctx, connect.NewRequest(&pb.CreateSessionRequest{ProjectId: env.project, ClientRequestId: uuid.NewString()}))
	if err != nil {
		t.Fatal(err)
	}
	s := &liveSession{t: t, env: env, api: env.api, ctx: ctx, id: created.Msg.GetSession().GetSessionId(), more: make(chan struct{}, 1)}
	watchCtx, stopWatch := context.WithCancel(ctx)
	stream, err := env.api.WatchSession(watchCtx, connect.NewRequest(&pb.WatchSessionRequest{SessionId: s.id}))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for stream.Receive() {
			s.mu.Lock()
			s.events = append(s.events, stream.Msg())
			s.mu.Unlock()
			select {
			case s.more <- struct{}{}:
			default:
			}
		}
	}()
	// The stream opens with a keepalive.
	s.await(func(e []*pb.SessionEvent) bool { return len(e) > 0 })
	sc.run(s)
	// Settled: the session no longer runs and nothing more arrives for a while.
	var last *pb.Session
	for {
		last = s.session()
		if last.GetState() != pb.Session_STATE_RUNNING && last.GetState() != pb.Session_STATE_QUEUED {
			s.mu.Lock()
			n := len(s.events)
			s.mu.Unlock()
			time.Sleep(500 * time.Millisecond)
			s.mu.Lock()
			quiet := len(s.events) == n
			s.mu.Unlock()
			if quiet {
				break
			}
			continue
		}
		time.Sleep(100 * time.Millisecond)
	}
	stopWatch()
	<-done
	_ = stream.Close()
	return recording{
		Final: final{
			State: last.GetState().String(), LastTurnOutcome: last.GetLastTurnOutcome().String(),
			PendingApprovalCount: last.GetPendingApprovalCount(),
		},
		Events: normalize(t, s.events),
	}
}

func (s *liveSession) call(run func(context.Context) error) {
	s.t.Helper()
	if err := run(s.ctx); err != nil {
		s.t.Fatal(err)
	}
}

func (s *liveSession) send(text string) { s.sendWith(text, pb.InputPriority_INPUT_PRIORITY_NORMAL) }

func (s *liveSession) sendWith(text string, priority pb.InputPriority) {
	s.t.Helper()
	s.call(func(ctx context.Context) error {
		_, err := s.api.SendMessage(ctx, connect.NewRequest(&pb.SendMessageRequest{
			SessionId: s.id, ClientMsgId: uuid.NewString(), Text: text, Priority: priority,
		}))
		return err
	})
}

// resolve answers the session's latest pending approval; expected accepts an error the call may return.
func (s *liveSession) resolve(approve bool, expected func(error) bool) {
	s.t.Helper()
	s.mu.Lock()
	var id string
	for _, e := range s.events {
		if e.GetKind() == kindPending {
			var p struct {
				ApprovalID string `json:"approval_id"`
			}
			if err := json.Unmarshal([]byte(e.GetPayloadJson()), &p); err != nil {
				s.t.Fatal(err)
			}
			id = p.ApprovalID
		}
	}
	s.mu.Unlock()
	_, err := s.api.ResolveApproval(s.ctx, connect.NewRequest(&pb.ResolveApprovalRequest{ApprovalId: id, Approve: approve}))
	if err != nil && (expected == nil || !expected(err)) {
		s.t.Fatal(err)
	}
}

func (s *liveSession) session() *pb.Session {
	s.t.Helper()
	res, err := s.api.GetSession(s.ctx, connect.NewRequest(&pb.GetSessionRequest{SessionId: s.id}))
	if err != nil {
		s.t.Fatal(err)
	}
	return res.Msg
}

// awaitWorker returns once the worker is attached again after a scripted
// crash (see probe).
func (s *liveSession) awaitWorker() {
	s.t.Helper()
	s.env.probe(s.ctx, s.t)
}

func (s *liveSession) awaitState(state pb.Session_State) {
	s.t.Helper()
	for s.session().GetState() != state {
		select {
		case <-s.ctx.Done():
			s.t.Fatalf("session never became %v", state)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (s *liveSession) await(done func([]*pb.SessionEvent) bool) {
	s.t.Helper()
	for {
		s.mu.Lock()
		ok := done(s.events)
		s.mu.Unlock()
		if ok {
			return
		}
		select {
		case <-s.more:
		case <-s.ctx.Done():
			var kinds []string
			for _, e := range s.events {
				kinds = append(kinds, fmt.Sprintf("%s:%.120s", e.GetKind(), e.GetPayloadJson()))
			}
			s.t.Fatalf("stream did not get there; events:\n%s", strings.Join(kinds, "\n"))
		}
	}
}

var uuidPattern = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

// normalize drops keepalives, numbers ids by first use, replaces the values
// of unstable fields by type markers (fakeworker.MaskUnstable) and a
// nonzero worker disconnect time by "1".
func normalize(t *testing.T, events []*pb.SessionEvent) []fixtureEvent {
	ids := map[string]string{}
	out := []fixtureEvent{}
	for _, e := range events {
		if e.GetKind() == pb.SessionEvent_KIND_UNSPECIFIED && e.GetPayloadJson() == "" {
			continue
		}
		if e.GetKind() == pb.SessionEvent_KIND_WORKER && e.GetPayloadJson() != `{"disconnected_at_unix_ms":"0"}` {
			e = &pb.SessionEvent{Seq: e.GetSeq(), Kind: e.GetKind(), Ephemeral: e.GetEphemeral(), PayloadJson: `{"disconnected_at_unix_ms":"1"}`}
		}
		var payload any
		if raw := e.GetPayloadJson(); raw != "" {
			raw = uuidPattern.ReplaceAllStringFunc(raw, func(id string) string {
				if n, ok := ids[id]; ok {
					return n
				}
				n := fmt.Sprintf("00000000-0000-4000-8000-%012d", len(ids)+1)
				ids[id] = n
				return n
			})
			if err := json.Unmarshal([]byte(raw), &payload); err != nil {
				t.Fatalf("payload of %v: %v", e, err)
			}
			payload = fakeworker.MaskUnstable(payload)
		}
		out = append(out, fixtureEvent{
			Seq: e.GetSeq(), Kind: e.GetKind().String(), Ephemeral: e.GetEphemeral(), Truncated: e.GetTruncated(), Payload: payload,
		})
	}
	return out
}
