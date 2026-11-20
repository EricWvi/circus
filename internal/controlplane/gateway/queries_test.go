package gateway_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/clock"
	"github.com/EricWvi/circus/internal/controlplane/gateway"
	"github.com/EricWvi/circus/internal/testutil/wait"
	"github.com/EricWvi/circus/internal/wire"
	"google.golang.org/protobuf/proto"
)

type queryHooks struct {
	hooks
	connected chan string
}

func (q queryHooks) OnConnected(_ context.Context, c gateway.Connection) error {
	q.connected <- c.Target.BootID
	return nil
}

type queryLog chan string

func (l queryLog) Write(p []byte) (int, error) {
	line := string(p)
	if strings.Contains(line, "ignoring unmatched device query result") {
		l <- line
	}
	return len(p), nil
}

type queryHarness struct {
	handler   gateway.Handler
	server    *httptest.Server
	clock     *clock.Fake
	connected chan string
	unmatched queryLog
}

func newQueryHarness(t *testing.T, configure ...func(*gateway.Config)) *queryHarness {
	t.Helper()
	h := &queryHarness{clock: clock.New(), connected: make(chan string, 4), unmatched: make(queryLog, 32)}
	r, err := gateway.NewRegistry(4)
	if err != nil {
		t.Fatal(err)
	}
	cfg := gateway.Config{
		InstanceID: "test", Clock: h.clock, Auth: authStub{}, Registry: r, Hooks: queryHooks{connected: h.connected},
		Logger:      slog.New(slog.NewJSONHandler(h.unmatched, &slog.HandlerOptions{Level: slog.LevelDebug})),
		LinkOptions: wire.Options{DisableHeartbeat: true},
	}
	for _, apply := range configure {
		apply(&cfg)
	}
	h.handler, err = gateway.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h.server = httptest.NewServer(h.handler)
	t.Cleanup(h.server.Close)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), wait.Timeout)
		defer cancel()
		if err := h.handler.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	return h
}

func (h *queryHarness) dial(t *testing.T, boot string, features []string) *wire.Link {
	t.Helper()
	return daemonLinkWithHello(t, wait.Ctx(t), h.server.URL, &circusv1.Hello{
		ProtocolVersion: wire.ProtocolVersion, BootId: boot, ClientVersion: boot, Features: features,
	}, h.clock)
}

func (h *queryHarness) connect(t *testing.T, boot string, features []string) *wire.Link {
	t.Helper()
	l := h.dial(t, boot, features)
	if got := wait.Recv(t, h.connected); got != boot {
		t.Fatalf("connected = %q, want %q", got, boot)
	}
	return l
}

func newDeviceQuery(path string) *circusv1.DeviceQuery {
	return &circusv1.DeviceQuery{RequestId: "caller-supplied", Query: &circusv1.DeviceQuery_ListDirectory{
		ListDirectory: &circusv1.ListDirectoryQuery{Path: path, IncludeHidden: true},
	}}
}

type finishedQuery struct {
	result *circusv1.DeviceQueryResult
	err    error
}

func (h *queryHarness) start(ctx context.Context, path string) <-chan finishedQuery {
	done := make(chan finishedQuery, 1)
	go func() {
		result, err := h.handler.Query(ctx, "d", newDeviceQuery(path))
		done <- finishedQuery{result, err}
	}()
	return done
}

func receiveDeviceQuery(t *testing.T, l *wire.Link, path string) string {
	t.Helper()
	msg, err := l.Receive(wait.Ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	q := msg.(*circusv1.DaemonDownlink).GetDeviceQuery()
	if id := q.GetRequestId(); id != "" && id != "caller-supplied" && q.GetListDirectory().GetPath() == path && q.GetListDirectory().GetIncludeHidden() {
		return id
	}
	t.Fatalf("unexpected device query: %v", q)
	return ""
}

func sendQueryResult(t *testing.T, l *wire.Link, result *circusv1.DeviceQueryResult) {
	t.Helper()
	msg := &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_DeviceQueryResult{DeviceQueryResult: result}}
	if err := l.Send(wait.Ctx(t), msg); err != nil {
		t.Fatal(err)
	}
}

func directoryResult(id string) *circusv1.DeviceQueryResult {
	return &circusv1.DeviceQueryResult{RequestId: id, Outcome: &circusv1.DeviceQueryResult_ListDirectory{
		ListDirectory: &circusv1.ListDirectoryResult{Path: "/project", Parent: "/"},
	}}
}

func TestDeviceQueryAvailability(t *testing.T) {
	h := newQueryHarness(t)
	ctx := wait.Ctx(t)
	query := func(want error) {
		t.Helper()
		if _, err := h.handler.Query(ctx, "d", newDeviceQuery("~")); !errors.Is(err, want) {
			t.Fatalf("query error = %v, want %v", err, want)
		}
	}
	query(gateway.ErrDeviceOffline)
	h.connect(t, "legacy", nil)
	query(gateway.ErrQueryUnsupported)
	features := make([]string, 32)
	for i := range features {
		features[i] = fmt.Sprintf("unknown_%d", i)
	}
	features[0] = "DEVICE_QUERY.LIST_DIRECTORY"
	features = append(features, wire.FeatureListDirectory)
	h.connect(t, "excess-features", features)
	query(gateway.ErrQueryUnsupported)
}

func TestGitRemoteQueryFeature(t *testing.T) {
	h := newQueryHarness(t)
	ctx := wait.Ctx(t)
	q := &circusv1.DeviceQuery{Query: &circusv1.DeviceQuery_GitRemote{
		GitRemote: &circusv1.GitRemoteQuery{Path: "~/project"},
	}}
	h.connect(t, "directory-only", []string{wire.FeatureListDirectory})
	if _, err := h.handler.Query(ctx, "d", q); !errors.Is(err, gateway.ErrQueryUnsupported) {
		t.Fatalf("query without git remote feature = %v", err)
	}
	l := h.connect(t, "git-remote", []string{wire.FeatureGitRemote})
	if _, err := h.handler.Query(ctx, "d", newDeviceQuery("~")); !errors.Is(err, gateway.ErrQueryUnsupported) {
		t.Fatalf("query without directory feature = %v", err)
	}
	done := make(chan finishedQuery, 1)
	go func() {
		result, err := h.handler.Query(ctx, "d", q)
		done <- finishedQuery{result, err}
	}()
	msg, err := l.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sent := msg.(*circusv1.DaemonDownlink).GetDeviceQuery()
	if sent.GetRequestId() == "" || !proto.Equal(sent.GetGitRemote(), q.GetGitRemote()) {
		t.Fatalf("forwarded query = %v", sent)
	}
	want := &circusv1.DeviceQueryResult{RequestId: sent.GetRequestId(), Outcome: &circusv1.DeviceQueryResult_GitRemote{
		GitRemote: &circusv1.GitRemoteResult{Url: "https://example.test/repo.git"},
	}}
	sendQueryResult(t, l, want)
	if got := wait.Recv(t, done); got.err != nil || !proto.Equal(got.result, want) {
		t.Fatalf("query result = %v, %v", got.result, got.err)
	}
}

func TestDeviceQueryLinkCloses(t *testing.T) {
	h := newQueryHarness(t)
	l := h.connect(t, "boot", []string{wire.FeatureListDirectory})
	done := h.start(wait.Ctx(t), "~")
	receiveDeviceQuery(t, l, "~")
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if got := wait.Recv(t, done); !errors.Is(got.err, gateway.ErrQueryLinkClosed) {
		t.Fatalf("query on closed link = %v", got)
	}
}

type replacementLogGate struct {
	slog.Handler
	registered chan struct{}
	resume     chan struct{}
}

func (g replacementLogGate) Handle(ctx context.Context, record slog.Record) error {
	if record.Message == "daemon connection superseded" {
		close(g.registered)
		<-g.resume
	}
	return g.Handler.Handle(ctx, record)
}

func TestDeviceQueryLinkReplaced(t *testing.T) {
	registered := make(chan struct{})
	resume := make(chan struct{})
	release := sync.OnceFunc(func() { close(resume) })
	defer release()
	h := newQueryHarness(t, func(cfg *gateway.Config) {
		cfg.Logger = slog.New(replacementLogGate{Handler: cfg.Logger.Handler(), registered: registered, resume: resume})
	})
	old := h.connect(t, "old", []string{wire.FeatureListDirectory})
	oldQuery := h.start(t.Context(), "/old")
	oldID := receiveDeviceQuery(t, old, "/old")
	replacement := h.dial(t, "new", []string{"", strings.Repeat("z", 65), "bad-feature", wire.FeatureListDirectory})
	// The replacement registered, but the old transport cannot begin closing
	// until the warning returns.
	wait.Recv(t, registered)
	if got := wait.Recv(t, oldQuery); !errors.Is(got.err, gateway.ErrQueryLinkClosed) || old.Err() != nil {
		t.Fatalf("query on superseded link = %v, transport error = %v", got, old.Err())
	}
	release()
	if boot := wait.Recv(t, h.connected); boot != "new" {
		t.Fatalf("connected = %q, want new", boot)
	}

	newQuery := h.start(wait.Ctx(t), "/new")
	newID := receiveDeviceQuery(t, replacement, "/new")
	if newID == oldID {
		t.Fatal("duplicate request ID")
	}
	sendQueryResult(t, replacement, directoryResult(oldID))
	if log := wait.Recv(t, h.unmatched); !strings.Contains(log, oldID) {
		t.Fatalf("wrong unmatched result: %s", log)
	}
	want := directoryResult(newID)
	sendQueryResult(t, replacement, want)
	if got := wait.Recv(t, newQuery); got.err != nil || !proto.Equal(got.result, want) {
		t.Fatalf("query on replacement = %v, want %v", got, want)
	}
}

func TestDeviceQuerySendQueueFullClosesLink(t *testing.T) {
	h := newQueryHarness(t, func(cfg *gateway.Config) { cfg.LinkOptions.MaxQueuedBytes = 1 })
	l := h.connect(t, "boot", []string{wire.FeatureListDirectory})
	_, err := h.handler.Query(wait.Ctx(t), "d", newDeviceQuery("~"))
	if !errors.Is(err, gateway.ErrQueryLinkClosed) {
		t.Fatalf("send queue overflow = %v, want link closed", err)
	}
	wait.Recv(t, l.Done())
}

func TestDeviceQueryDeadlineAndUnmatchedResults(t *testing.T) {
	h := newQueryHarness(t)
	l := h.connect(t, "boot", []string{wire.FeatureListDirectory})
	expiring := h.start(wait.Ctx(t), "/expiring")
	expiredID := receiveDeviceQuery(t, l, "/expiring")
	h.clock.Advance(10 * time.Second)
	if got := wait.Recv(t, expiring); !errors.Is(got.err, context.DeadlineExceeded) {
		t.Fatalf("expiry = %v", got)
	}
	for _, id := range []string{expiredID, "unknown"} {
		sendQueryResult(t, l, directoryResult(id))
		if log := wait.Recv(t, h.unmatched); !strings.Contains(log, id) {
			t.Fatalf("wrong unmatched result: %s", log)
		}
	}

	malformed := h.start(wait.Ctx(t), "/malformed")
	id := receiveDeviceQuery(t, l, "/malformed")
	sendQueryResult(t, l, &circusv1.DeviceQueryResult{RequestId: id})
	if got := wait.Recv(t, malformed); got.result != nil || got.err == nil || !strings.Contains(got.err.Error(), "no outcome") {
		t.Fatalf("malformed response = %v", got)
	}
	mismatched := h.start(wait.Ctx(t), "/mismatched")
	id = receiveDeviceQuery(t, l, "/mismatched")
	sendQueryResult(t, l, &circusv1.DeviceQueryResult{RequestId: id, Outcome: &circusv1.DeviceQueryResult_GitRemote{
		GitRemote: &circusv1.GitRemoteResult{Url: "https://example.test/repo.git"},
	}})
	if got := wait.Recv(t, mismatched); got.result != nil || got.err == nil || got.err.Error() != "device query result has the wrong query kind" {
		t.Fatalf("mismatched response = %v", got)
	}
	current := h.start(wait.Ctx(t), "/current")
	want := directoryResult(receiveDeviceQuery(t, l, "/current"))
	sendQueryResult(t, l, want)
	if got := wait.Recv(t, current); got.err != nil || !proto.Equal(got.result, want) {
		t.Fatalf("query after unmatched and malformed results = %v, want %v", got, want)
	}
}

func TestDeviceQueryLinkCapAndCancellation(t *testing.T) {
	h := newQueryHarness(t)
	l := h.connect(t, "boot", []string{wire.FeatureListDirectory})
	ctx := wait.Ctx(t)
	canceledCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	first := h.start(canceledCtx, "/cancel")
	ids := map[string]bool{receiveDeviceQuery(t, l, "/cancel"): true}

	pending := make(map[string]<-chan finishedQuery)
	for i := range 15 {
		path := fmt.Sprintf("/%d", i)
		done := h.start(ctx, path)
		id := receiveDeviceQuery(t, l, path)
		if ids[id] {
			t.Fatal("duplicate request ID")
		}
		ids[id] = true
		pending[id] = done
	}
	if _, err := h.handler.Query(ctx, "d", newDeviceQuery("/excess")); !errors.Is(err, gateway.ErrQueryBusy) {
		t.Fatalf("17th request = %v", err)
	}
	cancel()
	if got := wait.Recv(t, first); !errors.Is(got.err, context.Canceled) {
		t.Fatalf("canceled request = %v", got)
	}
	reused := h.start(ctx, "/reused")
	id := receiveDeviceQuery(t, l, "/reused")
	if ids[id] {
		t.Fatal("reused request ID")
	}
	pending[id] = reused
	for id, done := range pending {
		sendQueryResult(t, l, directoryResult(id))
		if got := wait.Recv(t, done); got.err != nil || got.result.GetRequestId() != id {
			t.Fatalf("query %q = %v", id, got)
		}
	}
}
