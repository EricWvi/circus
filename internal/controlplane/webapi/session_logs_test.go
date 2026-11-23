package webapi_test

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"
	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/controlplane/webapi"
	"github.com/EricWvi/circus/internal/store"
	"google.golang.org/protobuf/proto"
)

// logRecord is one captured record: its level, message and attributes
// (formatted with slog.Value.String).
type logRecord struct {
	level slog.Level
	msg   string
	attrs map[string]string
}

// logSink is what every handler derived from one capture shares.
type logSink struct {
	mu   sync.Mutex
	raw  bytes.Buffer
	recs []logRecord
}

// capture is a slog.Handler that keeps every record at or above level, both
// as attributes and as formatted text.
type capture struct {
	sink  *logSink
	level slog.Level
	attrs []slog.Attr
	text  slog.Handler
}

func newCapture(level slog.Level) *capture {
	s := &logSink{}
	return &capture{sink: s, level: level, text: slog.NewTextHandler(sinkWriter{s}, &slog.HandlerOptions{Level: level})}
}

// sinkWriter writes to the sink's buffer under the lock Handle holds.
type sinkWriter struct{ s *logSink }

func (w sinkWriter) Write(p []byte) (int, error) { return w.s.raw.Write(p) }

func (c *capture) Enabled(_ context.Context, l slog.Level) bool { return l >= c.level }

func (c *capture) Handle(ctx context.Context, r slog.Record) error {
	rec := logRecord{level: r.Level, msg: r.Message, attrs: map[string]string{}}
	for _, a := range c.attrs {
		rec.attrs[a.Key] = a.Value.String()
	}
	r.Attrs(func(a slog.Attr) bool {
		rec.attrs[a.Key] = a.Value.String()
		return true
	})
	c.sink.mu.Lock()
	defer c.sink.mu.Unlock()
	c.sink.recs = append(c.sink.recs, rec)
	return c.text.Handle(ctx, r)
}

func (c *capture) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &capture{sink: c.sink, level: c.level, attrs: append(append([]slog.Attr(nil), c.attrs...), attrs...), text: c.text.WithAttrs(attrs)}
}

func (c *capture) WithGroup(name string) slog.Handler {
	return &capture{sink: c.sink, level: c.level, attrs: c.attrs, text: c.text.WithGroup(name)}
}

func (c *capture) raw() string {
	c.sink.mu.Lock()
	defer c.sink.mu.Unlock()
	return c.sink.raw.String()
}

// matching returns the records with message msg whose attributes include
// every key-value pair of kv.
func (c *capture) matching(msg string, kv ...string) []logRecord {
	c.sink.mu.Lock()
	defer c.sink.mu.Unlock()
	var out []logRecord
	for _, r := range c.sink.recs {
		ok := r.msg == msg
		for i := 0; ok && i+1 < len(kv); i += 2 {
			v, found := r.attrs[kv[i]]
			ok = found && v == kv[i+1]
		}
		if ok {
			out = append(out, r)
		}
	}
	return out
}

// want checks that exactly n records with message msg and attributes kv
// were captured.
func (c *capture) want(t *testing.T, n int, msg string, kv ...string) {
	t.Helper()
	if got := len(c.matching(msg, kv...)); got != n {
		t.Fatalf("%d %q records with %v, want %d; captured:\n%s", got, msg, kv, n, c.raw())
	}
}

func TestSessionLogs(t *testing.T) {
	logs := newCapture(slog.LevelDebug)
	h, super := newHarness(t, func(c *webapi.Config) { c.Logger = slog.New(logs) })
	ctx := t.Context()
	p, sess := sessionSetup(t, h, super)
	id := sess.SessionId
	logs.want(t, 1, "session created", "session_id", id, "user_id", h.owner.ID, "project_id", p.ProjectId, "device_id", "d1",
		"pinned_worker_id", "", "local", "false", "provider_id", "")

	// Free-form settings never reach the log.
	const model, mode = "https://user:MARK-MODEL-SECRET@example.invalid", "MARK-PERMISSION-MODE"
	marked, err := super.createSession(t, &circusv1.CreateSessionRequest{ClientRequestId: "marked", ProjectId: p.ProjectId,
		Config: &circusv1.SessionConfig{Model: model, PermissionMode: mode}})
	if err != nil {
		t.Fatal(err)
	}
	logs.want(t, 1, "session created", "session_id", marked.SessionId)
	if raw := logs.raw(); strings.Contains(raw, "MARK-MODEL-SECRET") || strings.Contains(raw, mode) {
		t.Fatalf("session settings logged:\n%s", raw)
	}

	// An idempotent replay creates nothing and logs nothing.
	req := &circusv1.CreateSessionRequest{ClientRequestId: "replayed", ProjectId: p.ProjectId}
	first, err := super.createSession(t, req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := super.createSession(t, req); err != nil {
		t.Fatal(err)
	}
	logs.want(t, 1, "session created", "session_id", first.SessionId)
	logs.want(t, 3, "session created")

	const text = "MARK-WEB-MESSAGE-TEXT"
	if _, err := super.send(t, id, "m1", text); err != nil {
		t.Fatal(err)
	}
	logs.want(t, 1, "message accepted", "session_id", id, "priority", "normal", "bytes", "21")

	fence := h.lease(t, id)
	// An approval's text never reaches the log, nor does the retry message
	// built from it.
	const display = "MARK-APPROVAL-DISPLAY"
	now := h.clock.Load()
	if _, err := h.st.RequestApproval(ctx, fence, store.Approval{ApprovalID: "ap1", ToolUseID: "tu-ap1", Tool: "bash", DeviceID: "d1",
		MatchKey: strings.Repeat("a", 64), Display: display, Reason: display, CreatedAt: now, ExpiresAt: now + 600_000}); err != nil {
		t.Fatal(err)
	}
	if _, err := super.sessions.ResolveApproval(ctx, connect.NewRequest(&circusv1.ResolveApprovalRequest{ApprovalId: "ap1", Approve: true})); err != nil {
		t.Fatal(err)
	}
	logs.want(t, 1, "approval resolved", "session_id", id, "approval_id", "ap1", "decision", "approved")
	h.startTurn(t, fence, 1)
	if _, err := super.sessions.Interrupt(ctx, connect.NewRequest(&circusv1.InterruptRequest{SessionId: id})); err != nil {
		t.Fatal(err)
	}
	logs.want(t, 1, "interrupt requested", "session_id", id, "level", "1")

	for range 2 {
		if _, err := super.sessions.StopSession(ctx, connect.NewRequest(&circusv1.StopSessionRequest{SessionId: id})); err != nil {
			t.Fatal(err)
		}
	}
	// Only the stop that changed the session is logged.
	logs.want(t, 1, "session stopped", "session_id", id, "reason", "user")
	if raw := logs.raw(); strings.Contains(raw, text) || strings.Contains(raw, display) {
		t.Fatalf("message or approval text logged:\n%s", raw)
	}
}

// TestSessionLogsWebLeaseEnds: the web API's own transactions that end a
// lease log it once, from the loss the store returned, even for a lease no
// scheduler of this process tracks.
func TestSessionLogsWebLeaseEnds(t *testing.T) {
	logs := newCapture(slog.LevelInfo)
	h, super := newHarness(t, func(c *webapi.Config) { c.Logger = slog.New(logs) })
	ctx := t.Context()
	_, sess := sessionSetup(t, h, super)
	id := sess.SessionId

	// Recovery releases the broken session's lease.
	f := h.lease(t, id)
	h.startTurn(t, f, 1)
	if _, err := h.st.ReportTurn(ctx, f, store.TurnReport{LocalSeq: 1, Phase: store.TurnEnded, Outcome: store.TurnCompleted,
		At: h.clock.Load(), TranscriptIncomplete: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := super.sessions.RecoverSession(ctx, connect.NewRequest(&circusv1.RecoverSessionRequest{SessionId: id, AcceptTranscriptGap: true})); err != nil {
		t.Fatal(err)
	}
	logs.want(t, 1, "session recovered", "session_id", id)
	logs.want(t, 1, "lease ended", "session_id", id, "worker_id", f.WorkerID, "boot_id", f.BootID, "epoch", fmt.Sprint(f.Epoch),
		"reason", "recovered", "interrupted", "false")

	// A provider change releases the lease and interrupts the open turn
	// (a broken session's turn may still run).
	p := super.mustProvider(t, fakeProvider("Gateway"))
	f = h.lease(t, id)
	h.startTurn(t, f, 1)
	if _, err := h.st.ReportTurn(ctx, f, store.TurnReport{LocalSeq: 1, Phase: store.TurnEnded, Outcome: store.TurnCompleted,
		At: h.clock.Load(), TranscriptIncomplete: true}); err != nil {
		t.Fatal(err)
	}
	h.startTurn(t, f, 2)
	if _, err := super.sessions.UpdateSession(ctx, connect.NewRequest(&circusv1.UpdateSessionRequest{SessionId: id,
		ProviderId: proto.String(p.GetProviderId())})); err != nil {
		t.Fatal(err)
	}
	logs.want(t, 1, "session provider changed", "session_id", id, "old_provider_id", "", "provider_id", p.GetProviderId())
	logs.want(t, 1, "lease ended", "session_id", id, "epoch", fmt.Sprint(f.Epoch), "reason", "provider_changed", "interrupted", "true")
	logs.want(t, 2, "lease ended", "session_id", id)
}
