package webapi_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"connectrpc.com/connect"
	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/controlplane/webapi"
	"github.com/EricWvi/circus/internal/store"
	"google.golang.org/protobuf/proto"
)

func TestCreateSessionClientRequestID(t *testing.T) {
	h, super := newHarness(t)
	h.device(t, "d1", "box", "linux")
	p := super.mustCreate(t, "Project", loc("box", "/work"))
	for _, tc := range []struct {
		key, message string
	}{
		{"", "client request id is required"},
		{strings.Repeat("é", store.MaxIDBytes/2) + "x", "client request id is too long"},
	} {
		// The malformed key is rejected before looking up the project.
		_, err := super.createSession(t, &circusv1.CreateSessionRequest{ProjectId: "missing", ClientRequestId: tc.key})
		wantField(t, err, connect.CodeInvalidArgument, "client_request_id")
		var ce *connect.Error
		if !errors.As(err, &ce) || ce.Message() != tc.message {
			t.Fatalf("error = %v, want %q", err, tc.message)
		}
	}
	req := &circusv1.CreateSessionRequest{ProjectId: p.ProjectId, ClientRequestId: strings.Repeat("é", store.MaxIDBytes/2)}
	if _, err := super.createSession(t, req); err != nil {
		t.Fatal(err)
	}
}

func TestCreateSessionRepeat(t *testing.T) {
	h, super := newHarness(t)
	ctx := t.Context()
	h.device(t, "d1", "box", "linux")
	h.setOnline("d1", true)
	p := super.mustCreate(t, "Project", loc("box", "/work"), loc("box", "/other"))
	req := &circusv1.CreateSessionRequest{
		ProjectId: p.ProjectId, ClientRequestId: "key",
		Config: &circusv1.SessionConfig{Model: "opus", PermissionMode: "acceptEdits"},
	}
	first, err := super.createSession(t, req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := super.createSession(t, &circusv1.CreateSessionRequest{
		ProjectId: p.ProjectId, ClientRequestId: "other", LocationIndex: new(uint32(1)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := super.sessions.UpdateSession(ctx, connect.NewRequest(&circusv1.UpdateSessionRequest{
		SessionId: first.SessionId, Title: "Renamed",
	})); err != nil {
		t.Fatal(err)
	}
	h.setOnline("d1", false)
	current := super.getSession(t, first.SessionId)
	got, err := super.createSession(t, req)
	if err != nil || current.GetTitle() != "Renamed" || !proto.Equal(got, current) {
		t.Fatalf("retry while offline = %v, %v, want %v", got, err, current)
	}
	if _, err := h.st.DeleteIdentity(ctx, store.KindDevice, "d1", h.clock.Load()); err != nil {
		t.Fatal(err)
	}
	explicit := proto.CloneOf(req)
	explicit.LocationIndex = new(uint32(0))
	explicit.PinnedWorkerId = new("")
	got, err = super.createSession(t, explicit)
	if err != nil || !proto.Equal(got, current) {
		t.Fatalf("retry after revoke = %v, %v, want %v", got, err, current)
	}
	project, err := super.projects.GetProject(ctx, connect.NewRequest(&circusv1.GetProjectRequest{ProjectId: p.ProjectId}))
	if err != nil || project.Msg.Project.LastUsedLocation != 1 {
		t.Fatalf("project after retries = %v, %v", project, err)
	}

	bob := h.signIn(t, "bob", "bob@example.com")
	_, err = bob.createSession(t, req)
	wantCode(t, err, connect.CodeNotFound)
}

func TestCreateSessionConflicts(t *testing.T) {
	h, super := newHarness(t)
	h.device(t, "d1", "box", "linux")
	p := super.mustCreate(t, "Project", loc("box", "/work"), loc("box", "/other"))
	req := &circusv1.CreateSessionRequest{
		ProjectId: p.ProjectId, ClientRequestId: "key", LocationIndex: new(uint32(1)),
		Config: &circusv1.SessionConfig{Model: "opus", PermissionMode: "acceptEdits"},
	}
	if _, err := super.createSession(t, req); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		change func(*circusv1.CreateSessionRequest)
	}{
		{"location", func(r *circusv1.CreateSessionRequest) { r.LocationIndex = new(uint32(0)) }},
		{"out of range location", func(r *circusv1.CreateSessionRequest) { r.LocationIndex = new(uint32(99)) }},
		{"model", func(r *circusv1.CreateSessionRequest) { r.Config.Model = "sonnet" }},
		{"permission mode", func(r *circusv1.CreateSessionRequest) { r.Config.PermissionMode = "plan" }},
		{"worker", func(r *circusv1.CreateSessionRequest) { r.PinnedWorkerId = new("unknown") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			different := proto.CloneOf(req)
			tc.change(different)
			_, err := super.createSession(t, different)
			wantField(t, err, connect.CodeAlreadyExists, "client_request_id")
			var ce *connect.Error
			if !errors.As(err, &ce) || ce.Message() != "this client request id was used for a different session" {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestCreateSessionPermissionModeDefault(t *testing.T) {
	h, super := newHarness(t)
	h.device(t, "d1", "box", "linux")
	p := super.mustCreate(t, "Project", loc("box", "/work"))
	for _, tc := range []struct {
		key, firstMode, retryMode string
	}{
		{"implicit-first", "", "auto"},
		{"explicit-first", "auto", ""},
	} {
		t.Run(tc.key, func(t *testing.T) {
			req := &circusv1.CreateSessionRequest{
				ProjectId: p.ProjectId, ClientRequestId: tc.key,
				Config: &circusv1.SessionConfig{PermissionMode: tc.firstMode},
			}
			first, err := super.createSession(t, req)
			if err != nil {
				t.Fatal(err)
			}
			req.Config.PermissionMode = tc.retryMode
			got, err := super.createSession(t, req)
			if err != nil || !proto.Equal(got, first) {
				t.Fatalf("retry with permission mode %q = %v, %v, want %v", tc.retryMode, got, err, first)
			}
		})
	}
}

func TestCreateSessionRepeatAtCap(t *testing.T) {
	h, super := newHarness(t)
	device := h.device(t, "d1", "box", "linux")
	p := super.mustCreate(t, "Project", loc("box", "/work"))
	const limit = store.DefaultMaxSessionsPerUser
	now := h.clock.Load()
	if err := h.st.Tx(t.Context(), func(ctx context.Context, tx store.Ops) error {
		for i := range limit - 1 {
			id := fmt.Sprintf("seed-%d", i)
			_, err := tx.CreateSession(ctx, store.Session{
				ID: id, ClientRequestID: id, ClaudeSessionID: id, UserID: h.owner.ID,
				ProjectID: p.ProjectId, DefaultDeviceID: device.ID, CreatedAt: now,
			}, nil, limit)
			if err != nil {
				return fmt.Errorf("seed session %d: %w", i, err)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	req := &circusv1.CreateSessionRequest{ProjectId: p.ProjectId, ClientRequestId: "last-key"}
	last, err := super.createSession(t, req)
	if err != nil {
		t.Fatal(err)
	}
	got, err := super.createSession(t, req)
	if err != nil || !proto.Equal(got, last) {
		t.Fatalf("retry at cap = %v, %v, want %v", got, err, last)
	}
	req.ClientRequestId = "new-key"
	_, err = super.createSession(t, req)
	wantCode(t, err, connect.CodeResourceExhausted)
}

func TestCreateSessionReuseAfterDelete(t *testing.T) {
	h, super := newHarness(t)
	ctx := t.Context()
	h.device(t, "d1", "box", "linux")
	p := super.mustCreate(t, "Project", loc("box", "/work"))
	req := &circusv1.CreateSessionRequest{ProjectId: p.ProjectId, ClientRequestId: "key"}
	first, err := super.createSession(t, req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := super.sessions.StopSession(ctx, connect.NewRequest(&circusv1.StopSessionRequest{SessionId: first.SessionId})); err != nil {
		t.Fatal(err)
	}
	if _, err := super.sessions.DeleteSession(ctx, connect.NewRequest(&circusv1.DeleteSessionRequest{SessionId: first.SessionId})); err != nil {
		t.Fatal(err)
	}
	second, err := super.createSession(t, req)
	if err != nil || second.GetSessionId() == first.SessionId {
		t.Fatalf("create after delete = %v, %v, original was %v", second, err, first)
	}
}

func TestCreateSessionConcurrent(t *testing.T) {
	h, super := newHarness(t)
	h.device(t, "d1", "box", "linux")
	p := super.mustCreate(t, "Project", loc("box", "/work"))
	const n = 16
	var wg sync.WaitGroup
	var sessions [n]*circusv1.Session
	var errs [n]error
	start := make(chan struct{})
	for i := range n {
		wg.Go(func() {
			req := &circusv1.CreateSessionRequest{ProjectId: p.ProjectId, ClientRequestId: "shared"}
			<-start
			sessions[i], errs[i] = super.createSession(t, req)
		})
	}
	close(start)
	wg.Wait()
	page, err := super.sessions.ListSessions(t.Context(), connect.NewRequest(&circusv1.ListSessionsRequest{ProjectId: p.ProjectId}))
	if err != nil || len(page.Msg.Sessions) != 1 {
		t.Fatalf("sessions = %v, %v, want one", page, err)
	}
	for i, session := range sessions {
		if errs[i] != nil || session.GetSessionId() != page.Msg.Sessions[0].SessionId || !proto.Equal(session, sessions[0]) {
			t.Fatalf("create %d = %v, %v, want the one stored session", i, session, errs[i])
		}
	}
}

// createConflictStore simulates an insert losing to a concurrent commit.
// SQLite's immediate write transactions serialize the normal race.
type createConflictStore struct {
	store.Store
	fail *atomic.Int32
}

func (s createConflictStore) Tx(ctx context.Context, fn func(context.Context, store.Ops) error) error {
	return s.Store.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		return fn(ctx, &createConflictOps{Ops: tx, fail: s.fail})
	})
}

type createConflictOps struct {
	store.Ops
	fail     *atomic.Int32
	conflict bool
}

func (o *createConflictOps) GetSessionByClientRequestID(ctx context.Context, projectID, key string, now int64) (store.Session, error) {
	if n := o.fail.Load(); n > 0 && o.fail.CompareAndSwap(n, n-1) {
		o.conflict = true
		return store.Session{}, store.ErrNotFound
	}
	return o.Ops.GetSessionByClientRequestID(ctx, projectID, key, now)
}

func (o *createConflictOps) CreateSession(ctx context.Context, s store.Session, cwds []store.SessionCwd, maxSessions int) (store.SessionCreated, error) {
	if o.conflict {
		return store.SessionCreated{}, store.ErrExists
	}
	return o.Ops.CreateSession(ctx, s, cwds, maxSessions)
}

func TestCreateSessionRereadsAfterUniqueConflict(t *testing.T) {
	var fail atomic.Int32
	h, super := newHarness(t, func(c *webapi.Config) { c.Store = createConflictStore{c.Store, &fail} })
	h.device(t, "d1", "box", "linux")
	p := super.mustCreate(t, "Project", loc("box", "/work"))
	req := &circusv1.CreateSessionRequest{ProjectId: p.ProjectId, ClientRequestId: "key"}
	first, err := super.createSession(t, req)
	if err != nil {
		t.Fatal(err)
	}
	fail.Store(1)
	got, err := super.createSession(t, req)
	if err != nil || !proto.Equal(got, first) {
		t.Fatalf("retry after insert conflict = %v, %v, want %v", got, err, first)
	}
	fail.Store(1)
	req.Config = &circusv1.SessionConfig{Model: "other"}
	_, err = super.createSession(t, req)
	wantField(t, err, connect.CodeAlreadyExists, "client_request_id")

	fail.Store(2)
	req.Config = nil
	_, err = super.createSession(t, req)
	wantField(t, err, connect.CodeAlreadyExists, "client_request_id")
	if n := fail.Load(); n != 0 {
		t.Fatalf("repeated conflicts left %d unused injections", n)
	}
}
