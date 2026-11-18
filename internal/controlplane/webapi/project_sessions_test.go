package webapi_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/gen/go/circus/v1/circusv1connect"
	"github.com/EricWvi/circus/internal/controlplane/webapi"
	"github.com/EricWvi/circus/internal/store"
)

func wantDeleteError(t *testing.T, err error, code connect.Code, message string) {
	t.Helper()
	wantCode(t, err, code)
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Message() != message {
		t.Fatalf("delete error = %v, want message %q", err, message)
	}
}

func projectSession(t *testing.T, h *harness, projectID, id string) store.Session {
	t.Helper()
	sess := store.Session{ID: id, UserID: h.owner.ID, ProjectID: projectID, ClaudeSessionID: "claude-" + id, DefaultDeviceID: "d1", CreatedAt: h.clock.Load()}
	if err := h.st.CreateSession(t.Context(), sess, nil, 100); err != nil {
		t.Fatal(err)
	}
	return sess
}

func stopProjectSession(t *testing.T, h *harness, id string) {
	t.Helper()
	if _, _, _, err := h.st.StopSession(t.Context(), id, h.clock.Load(), ""); err != nil {
		t.Fatal(err)
	}
}

func sessionWorker(t *testing.T, h *harness, id string) store.Fence {
	t.Helper()
	ctx := t.Context()
	now := h.clock.Load()
	if err := h.st.CreateIdentity(ctx, store.Identity{ID: "w1", Kind: store.KindWorker, UserID: h.owner.ID, Name: "worker", MachineID: "machine", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	lease, err := h.st.AcquireLease(ctx, store.LeaseRequest{SessionID: id, WorkerID: "w1", BootID: "boot", ConnID: "conn", Now: now, ExpiresAt: now + 60_000})
	if err != nil {
		t.Fatal(err)
	}
	return store.Fence{SessionID: id, Epoch: lease.Lease.Epoch, WorkerID: "w1", BootID: "boot", ConnID: "conn", Now: now}
}

func TestDeleteProjectForeignSessions(t *testing.T) {
	h, super := newHarness(t)
	ctx := t.Context()
	h.device(t, "d1", "box", "linux")
	p := super.mustCreate(t, "Project")
	for _, id := range []string{"first", "second"} {
		projectSession(t, h, p.ProjectId, id)
		stopProjectSession(t, h, id)
	}
	bob := h.signIn(t, "bob", "bob@example.com")

	_, err := bob.projects.DeleteProject(ctx, connect.NewRequest(&circusv1.DeleteProjectRequest{ProjectId: p.ProjectId}))
	wantDeleteError(t, err, connect.CodeNotFound, "project not found")
	for _, id := range []string{"first", "second"} {
		if _, err := h.st.GetSession(ctx, id, h.clock.Load()); err != nil {
			t.Fatalf("foreign delete removed session %q: %v", id, err)
		}
	}
	if _, err := super.projects.GetProject(ctx, connect.NewRequest(&circusv1.GetProjectRequest{ProjectId: p.ProjectId})); err != nil {
		t.Fatalf("owner's project is missing: %v", err)
	}
}

func TestDeleteProjectSessionsRefusal(t *testing.T) {
	for _, cause := range []string{"not-stopped", "live-lease", "non-terminal-call"} {
		t.Run(cause, func(t *testing.T) {
			h, super := newHarness(t)
			ctx := t.Context()
			h.device(t, "d1", "box", "linux")
			p := super.mustCreate(t, "Project")
			projectSession(t, h, p.ProjectId, "z-ready")
			stopProjectSession(t, h, "z-ready")
			projectSession(t, h, p.ProjectId, "a-refused")
			switch cause {
			case "live-lease":
				sessionWorker(t, h, "a-refused")
				stopProjectSession(t, h, "a-refused")
			case "non-terminal-call":
				stopProjectSession(t, h, "a-refused")
				if err := h.st.CreateCall(ctx, store.Call{CallID: "call", UserID: h.owner.ID, DeviceID: "d1", SessionID: "a-refused", Tool: "bash", RequestHash: []byte("hash"), State: store.CallAccepted, SubmittedAt: h.clock.Load()}); err != nil {
					t.Fatal(err)
				}
			}
			_, err := super.projects.DeleteProject(ctx, connect.NewRequest(&circusv1.DeleteProjectRequest{ProjectId: p.ProjectId}))
			wantDeleteError(t, err, connect.CodeFailedPrecondition, "the project has sessions that are not stopped or still busy")
			for _, id := range []string{"z-ready", "a-refused"} {
				if _, err := h.st.GetSession(ctx, id, h.clock.Load()); err != nil {
					t.Fatalf("preflight deleted %s: %v", id, err)
				}
			}
			if len(super.list(t)) != 1 {
				t.Fatal("preflight deleted project")
			}
		})
	}
}

func TestDeleteProjectSessionsCascade(t *testing.T) {
	h, super := newHarness(t)
	ctx := t.Context()
	h.device(t, "d1", "box", "linux")
	p := super.mustCreate(t, "Project")
	keep := super.mustCreate(t, "Keep")
	projectSession(t, h, p.ProjectId, "bulk")
	projectSession(t, h, p.ProjectId, "empty")
	projectSession(t, h, keep.ProjectId, "keep")
	fence := sessionWorker(t, h, "bulk")
	// More than one deletion chunk proves the RPC uses the chunked store flow.
	entries := make([]store.TranscriptEntry, store.DeleteChunkRows+3)
	for i := range entries {
		entries[i].JSON = []byte(`{}`)
	}
	q := store.TranscriptQuota{MaxEntryBytes: 1024, MaxSessionBytes: 1 << 24, MaxUserBytes: 1 << 25}
	if _, err := h.st.AppendTranscript(ctx, fence, store.TranscriptBatch{BatchSeq: 1, Entries: entries}, q); err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.AppendEvents(ctx, "bulk", nil, []store.EventInput{{Kind: "ASSISTANT", At: h.clock.Load(), Payload: []byte(`{}`)}}, store.DefaultMaxViewBytes); err != nil {
		t.Fatal(err)
	}
	if err := h.st.CreateCall(ctx, store.Call{CallID: "terminal", UserID: h.owner.ID, DeviceID: "d1", SessionID: "bulk", Tool: "bash", RequestHash: []byte("hash"), State: store.CallAccepted, SubmittedAt: h.clock.Load()}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.TransitionCall(ctx, "terminal", store.CallExpect{States: []store.CallState{store.CallAccepted}}, store.CallCancelled, store.CallPatch{FinishedAt: h.clock.Load()}); err != nil {
		t.Fatal(err)
	}
	stopProjectSession(t, h, "bulk")
	stopProjectSession(t, h, "empty")
	// An expired, unswept lease is deletable too.
	h.clock.Add(60_000)
	if _, err := super.projects.DeleteProject(ctx, connect.NewRequest(&circusv1.DeleteProjectRequest{ProjectId: p.ProjectId})); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"bulk", "empty"} {
		if _, err := h.st.GetSession(ctx, id, h.clock.Load()); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("session remains: %s %v", id, err)
		}
	}
	if _, err := h.st.GetCall(ctx, "terminal"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("call remains: %v", err)
	}
	if _, err := h.st.ReadEvents(ctx, store.EventFilter{SessionID: "bulk", Limit: 1, MaxBytes: 1}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("events remain: %v", err)
	}
	owner, err := h.st.GetUser(ctx, h.owner.ID)
	if err != nil || owner.TranscriptBytes != 0 {
		t.Fatalf("transcript accounting: %+v %v", owner, err)
	}
	if _, err := h.st.GetSession(ctx, "keep", h.clock.Load()); err != nil {
		t.Fatal(err)
	}
	if got := super.list(t); len(got) != 1 || got[0].ProjectId != keep.ProjectId {
		t.Fatalf("unrelated project changed: %v", got)
	}
}

type projectDeleteStore struct {
	store.Store
	beforeSessionDelete func(context.Context, int64) error
	beforeProjectDelete func(context.Context, string, string, int64) error
	sessionDeletes      atomic.Int64
	projectDeletes      atomic.Int64
}

func (s *projectDeleteStore) DeleteSession(ctx context.Context, id string, now int64) error {
	attempt := s.sessionDeletes.Add(1)
	if s.beforeSessionDelete != nil {
		if err := s.beforeSessionDelete(ctx, attempt); err != nil {
			return err
		}
	}
	return s.Store.DeleteSession(ctx, id, now)
}
func (s *projectDeleteStore) DeleteProject(ctx context.Context, userID, projectID string, now int64) error {
	if s.projectDeletes.Add(1) == 1 && s.beforeProjectDelete != nil {
		if err := s.beforeProjectDelete(ctx, userID, projectID, now); err != nil {
			return err
		}
	}
	return s.Store.DeleteProject(ctx, userID, projectID, now)
}

func TestDeleteProjectSessionAppearsDuringCascade(t *testing.T) {
	wrapped := &projectDeleteStore{}
	h, super := newHarness(t, func(c *webapi.Config) { wrapped.Store, c.Store = c.Store, wrapped })
	ctx := t.Context()
	h.device(t, "d1", "box", "linux")
	p := super.mustCreate(t, "Project")
	projectSession(t, h, p.ProjectId, "old")
	stopProjectSession(t, h, "old")
	wrapped.beforeProjectDelete = func(ctx context.Context, _ string, projectID string, now int64) error {
		if err := h.st.CreateSession(ctx, store.Session{ID: "new", UserID: h.owner.ID, ProjectID: projectID, ClaudeSessionID: "claude-new", DefaultDeviceID: "d1", CreatedAt: now}, nil, 100); err != nil {
			return err
		}
		_, _, _, err := h.st.StopSession(ctx, "new", now, "")
		return err
	}
	_, err := super.projects.DeleteProject(ctx, connect.NewRequest(&circusv1.DeleteProjectRequest{ProjectId: p.ProjectId}))
	wantDeleteError(t, err, connect.CodeFailedPrecondition, "a session was created while deleting; try again")
	if wrapped.sessionDeletes.Load() != 1 {
		t.Fatal("RPC did not call chunked DeleteSession")
	}
	if _, err := h.st.GetSession(ctx, "old", h.clock.Load()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("old session not deleted: %v", err)
	}
	if _, err := h.st.GetSession(ctx, "new", h.clock.Load()); err != nil {
		t.Fatal(err)
	}
	if len(super.list(t)) != 1 {
		t.Fatal("new session did not protect project")
	}
	if _, err := super.projects.DeleteProject(ctx, connect.NewRequest(&circusv1.DeleteProjectRequest{ProjectId: p.ProjectId})); err != nil {
		t.Fatal(err)
	}
	if len(super.list(t)) != 0 {
		t.Fatal("retry did not finish cascade")
	}
}

func TestDeleteProjectRetryAfterPartialFailure(t *testing.T) {
	wrapped := &projectDeleteStore{
		beforeSessionDelete: func(_ context.Context, attempt int64) error {
			if attempt == 2 {
				return store.ErrBusy
			}
			return nil
		},
	}
	h, super := newHarness(t, func(c *webapi.Config) { wrapped.Store, c.Store = c.Store, wrapped })
	ctx := t.Context()
	h.device(t, "d1", "box", "linux")
	p := super.mustCreate(t, "Project")
	for _, id := range []string{"s1", "s2", "s3"} {
		projectSession(t, h, p.ProjectId, id)
		stopProjectSession(t, h, id)
	}
	remove := func() error {
		_, err := super.projects.DeleteProject(ctx, connect.NewRequest(&circusv1.DeleteProjectRequest{ProjectId: p.ProjectId}))
		return err
	}

	wantDeleteError(t, remove(), connect.CodeUnavailable, "busy; retry")
	if attempts := wrapped.sessionDeletes.Load(); attempts != 2 || wrapped.projectDeletes.Load() != 0 {
		t.Fatalf("cascade continued after busy: %d session delete attempts, %d project delete attempts", attempts, wrapped.projectDeletes.Load())
	}
	// Equal activity times put s3 first, then s2, then s1.
	if _, err := h.st.GetSession(ctx, "s3", h.clock.Load()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("first session delete did not commit: %v", err)
	}
	for _, id := range []string{"s1", "s2"} {
		if _, err := h.st.GetSession(ctx, id, h.clock.Load()); err != nil {
			t.Fatalf("session %q did not survive partial deletion: %v", id, err)
		}
	}
	if _, err := super.projects.GetProject(ctx, connect.NewRequest(&circusv1.GetProjectRequest{ProjectId: p.ProjectId})); err != nil {
		t.Fatalf("project did not survive partial deletion: %v", err)
	}

	if err := remove(); err != nil {
		t.Fatalf("retry: %v", err)
	}
	for _, id := range []string{"s1", "s2"} {
		if _, err := h.st.GetSession(ctx, id, h.clock.Load()); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("session %q survived retry: %v", id, err)
		}
	}
	wantDeleteError(t, remove(), connect.CodeNotFound, "project not found")
}

func TestConcurrentDeleteProjectSessions(t *testing.T) {
	bothPreflighted := make(chan struct{})
	wrapped := &projectDeleteStore{
		beforeSessionDelete: func(ctx context.Context, attempt int64) error {
			// Both requests must pass preflight with the same session still
			// present. One will then hit ErrNotFound deleting that session.
			if attempt == 2 {
				close(bothPreflighted)
			}
			select {
			case <-bothPreflighted:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	}
	h, super := newHarness(t, func(c *webapi.Config) { wrapped.Store, c.Store = c.Store, wrapped })
	ctx := t.Context()
	h.device(t, "d1", "box", "linux")
	p := super.mustCreate(t, "Project")
	projectSession(t, h, p.ProjectId, "shared")
	stopProjectSession(t, h, "shared")
	results := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := super.projects.DeleteProject(ctx, connect.NewRequest(&circusv1.DeleteProjectRequest{ProjectId: p.ProjectId}))
			results <- err
		}()
	}
	first, second := <-results, <-results
	if first != nil {
		first, second = second, first
	}
	if first != nil {
		t.Fatalf("both deletes failed: %v and %v", first, second)
	}
	wantDeleteError(t, second, connect.CodeNotFound, "project not found")
	if attempts := wrapped.projectDeletes.Load(); attempts != 2 {
		t.Fatalf("only %d cascades reached the final project delete", attempts)
	}
	if _, err := h.st.GetSession(ctx, "shared", h.clock.Load()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("session survived concurrent deletes: %v", err)
	}
	if len(super.list(t)) != 0 {
		t.Fatal("project survived concurrent deletes")
	}
}

func TestDeleteProjectContextErrors(t *testing.T) {
	for _, tc := range []struct {
		code       connect.Code
		newContext func(context.Context) (context.Context, context.CancelFunc)
		message    string
	}{
		{connect.CodeCanceled, func(ctx context.Context) (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(ctx)
			cancel()
			return ctx, cancel
		}, context.Canceled.Error()},
		{connect.CodeDeadlineExceeded, func(ctx context.Context) (context.Context, context.CancelFunc) {
			return context.WithDeadline(ctx, time.Now().Add(-time.Second))
		}, context.DeadlineExceeded.Error()},
	} {
		t.Run(tc.code.String(), func(t *testing.T) {
			var logs bytes.Buffer
			h, super := newHarness(t, func(c *webapi.Config) {
				c.Logger = slog.New(slog.NewTextHandler(&logs, nil))
				auth := c.Interceptor
				c.Interceptor = connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
					return auth.WrapUnary(func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
						if req.Spec().Procedure == circusv1connect.ProjectServiceDeleteProjectProcedure {
							// Leave the HTTP client connected so it can observe
							// the code returned by the canceled server handler.
							var cancel context.CancelFunc
							ctx, cancel = tc.newContext(ctx)
							defer cancel()
						}
						return next(ctx, req)
					})
				})
			})
			p := super.mustCreate(t, "Project")
			_, err := super.projects.DeleteProject(t.Context(), connect.NewRequest(&circusv1.DeleteProjectRequest{ProjectId: p.ProjectId}))
			wantDeleteError(t, err, tc.code, tc.message)
			if len(super.list(t)) != 1 {
				t.Fatal("canceled request deleted project")
			}
			h.server.Close()
			if logs.Len() != 0 {
				t.Fatalf("context error was logged: %q", logs.String())
			}
		})
	}
}
