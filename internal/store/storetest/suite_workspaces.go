package storetest

import (
	"context"
	"fmt"
	"sync"
	"testing"

	pb "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/store"
)

func workspaceFixture(t *testing.T, b Backend) (store.Store, string, fixture, store.Workspace) {
	t.Helper()
	db, dir := open(t, b)
	f := seed(t, db)
	return db, dir, f, store.Workspace{
		ID: "workspace", UserID: f.user.ID, ProjectID: "session-project", DeviceID: f.device.ID, CreatedAt: t0,
	}
}

func testWorkspaceTransitions(t *testing.T, b Backend) {
	db, _, _, spec := workspaceFixture(t, b)
	ctx := deadline(t)
	w, created, err := db.EnsureWorkspace(ctx, spec)
	must(t, err)
	mustEqual(t, "new", created && w.State == store.WorkspacePending, true)
	apply := func(p store.WorkspacePatch) store.Workspace {
		t.Helper()
		p.UpdatedAt = t0
		change, err := db.TransitionWorkspace(ctx, w.ID, w.Expect(), p)
		must(t, err)
		mustEqual(t, "applied", change.Applied, true)
		x, err := db.GetWorkspace(ctx, w.ProjectID, w.DeviceID)
		must(t, err)
		mustEqual(t, "persisted", x, change.Workspace)
		return x
	}
	for _, r := range []store.WorkspaceWaiting{store.WorkspaceOffline, store.WorkspaceOutdated, store.WorkspaceLimit} {
		w = apply(store.WorkspacePatch{State: store.WorkspacePending, Waiting: r})
	}
	w = apply(store.WorkspacePatch{State: store.WorkspaceFailed, ErrorCode: pb.WorkspaceError_WORKSPACE_ERROR_UNSUPPORTED_OS})
	w = apply(store.WorkspacePatch{State: store.WorkspacePending})
	begin := func() {
		w = apply(store.WorkspacePatch{State: store.WorkspacePreparing, Attempt: w.Attempt + 1, CallID: fmt.Sprintf("ws-call%d", w.Attempt), TimeoutMs: 120000,
			RepoURL: "https://worker:secret@example.com/repo", AttemptStartedAt: t0, LostStreak: w.LostStreak})
	}
	begin()
	p := w.WorkspacePatch
	p.JobID, p.JobBootID = "job", "boot"
	w = apply(p)
	p = w.WorkspacePatch
	p.CallID, p.ResumeJobID, p.JobID, p.JobBootID = "ws-followup", w.JobID, "", ""
	w = apply(p)
	for _, reason := range []store.WorkspaceWaiting{store.WorkspaceBackoff, store.WorkspaceOffline, store.WorkspaceOutdated, ""} {
		p = store.WorkspacePatch{State: store.WorkspacePending, Waiting: reason, Attempt: w.Attempt}
		if reason == store.WorkspaceBackoff {
			p.RetryAt = t0 + 5000
		}
		if reason == "" {
			p.LostStreak = 1
		}
		w = apply(p)
		begin()
	}
	w = apply(store.WorkspacePatch{State: store.WorkspaceFailed, Attempt: w.Attempt, LostStreak: 3, ErrorCode: pb.WorkspaceError_WORKSPACE_ERROR_INTERRUPTED})
	w = apply(store.WorkspacePatch{State: store.WorkspacePending, Attempt: w.Attempt})
	begin()
	change, err := db.MarkWorkspaceReady(ctx, w.ID, w.Expect(), "/absolute", "cloned", t0)
	must(t, err)
	mustEqual(t, "ready", change.Workspace.State, store.WorkspaceReady)
	w = change.Workspace
	_, err = db.TransitionWorkspace(ctx, w.ID, w.Expect(), store.WorkspacePatch{State: store.WorkspacePending, Attempt: w.Attempt, UpdatedAt: t0})
	mustIs(t, err, store.ErrInvalidTransition)
	rows, err := db.ListActiveWorkspaces(ctx, nil, 1)
	must(t, err)
	mustEqual(t, "final rows filtered", len(rows), 0)
	must(t, db.DeleteProject(ctx, w.UserID, w.ProjectID, t0+100))
	_, err = db.GetWorkspaceByID(ctx, w.ID)
	mustIs(t, err, store.ErrNotFound)
	change, err = db.MarkWorkspaceReady(ctx, w.ID, w.Expect(), "/absolute", "cloned", t0)
	must(t, err)
	mustEqual(t, "deleted CAS", change.Applied, false)
}

func testWorkspaceConcurrent(t *testing.T, b Backend) {
	db, dir, _, spec := workspaceFixture(t, b)
	db2 := reopen(t, b, dir)
	ctx := deadline(t)
	var wg sync.WaitGroup
	var got [2]store.Workspace
	var created [2]bool
	var applied [2]bool
	var errs [2]error
	for i, handle := range []store.Store{db, db2} {
		wg.Go(func() {
			w := spec
			w.ID = fmt.Sprintf("ws%d", i)
			got[i], created[i], errs[i] = handle.EnsureWorkspace(ctx, w)
		})
	}
	wg.Wait()
	must(t, errs[0])
	must(t, errs[1])
	mustEqual(t, "one workspace", got[0], got[1])
	mustEqual(t, "one created", created[0] != created[1], true)
	for i, handle := range []store.Store{db, db2} {
		wg.Go(func() {
			w := got[i]
			patch := w.WorkspacePatch
			patch.State, patch.Attempt, patch.CallID, patch.TimeoutMs = store.WorkspacePreparing, 1, fmt.Sprintf("ws-call%d", i), 120000
			change, err := handle.TransitionWorkspace(ctx, w.ID, w.Expect(), patch)
			applied[i], errs[i] = change.Applied, err
		})
	}
	wg.Wait()
	must(t, errs[0])
	must(t, errs[1])
	mustEqual(t, "one CAS winner", applied[0] != applied[1], true)
	current, err := db.GetWorkspaceByID(ctx, got[0].ID)
	must(t, err)
	p := current.WorkspacePatch
	p.JobID, p.JobBootID = "job", "boot"
	change, err := db.TransitionWorkspace(ctx, current.ID, current.Expect(), p)
	must(t, err)
	mustEqual(t, "background written", change.Applied, true)
	change, err = db2.TransitionWorkspace(ctx, current.ID, current.Expect(), p)
	must(t, err)
	mustEqual(t, "stale writer on same call", change.Applied, false)
}

func testWorkspaceReadyTransaction(t *testing.T, b Backend) {
	db, _, f, spec := workspaceFixture(t, b)
	ctx := deadline(t)
	w, _, err := db.EnsureWorkspace(ctx, spec)
	must(t, err)
	must(t, db.CreateIdentity(ctx, store.Identity{
		Kind: store.KindDevice, ID: "other-device", UserID: f.user.ID, Name: "other", MachineID: "other", CreatedAt: t0,
	}))
	for _, id := range []string{"a", "b", "stopped", "explicit"} {
		sess := newSession(id, f)
		sess.Workspace = id != "explicit"
		if id == "explicit" {
			sess.CreatedAt++
		}
		cwds := []store.SessionCwd{
			{DeviceID: f.device.ID, Cwd: store.DefaultWorkspacePath(w.ProjectID)},
			{DeviceID: "other-device", Cwd: "/another-device"},
		}
		_, err := db.CreateSession(ctx, sess, cwds, store.DefaultMaxSessionsPerUser)
		must(t, err)
	}
	_, _, _, err = db.StopSession(ctx, "stopped", t0+1, "user")
	must(t, err)
	submit(t, db, newInput("a", 2))
	settled, err := db.SettleSession(ctx, "a", t0+2)
	must(t, err)
	mustEqual(t, "blocked queued", settled.Session.WorkspaceBlocked && settled.Session.State == store.SessionQueued, true)
	rows, err := db.ListSessions(ctx, store.SessionFilter{Runnable: true, Now: t0, Limit: 10})
	must(t, err)
	mustEqual(t, "runnable excludes workspace", len(rows) == 1 && rows[0].ID == "explicit", true)
	_, err = db.AcquireLease(ctx, leaseReq("a", f.worker.ID, "boot", "conn", t0+2))
	mustIs(t, err, store.ErrLeaseHeld)
	patch := store.WorkspacePatch{State: store.WorkspacePreparing, CallID: "ws-call", Attempt: 1, LostStreak: 2, BackoffStreak: 3, TimeoutMs: 120000, UpdatedAt: t0 + 3}
	change, err := db.TransitionWorkspace(ctx, w.ID, w.Expect(), patch)
	must(t, err)
	w = change.Workspace
	mustEqual(t, "preparing events", change.Notices, []store.WorkspaceNotice{{SessionID: "a", Seq: 1}, {SessionID: "b", Seq: 1}})
	ready := func(ops store.Ops, ctx context.Context) (store.WorkspaceChange, error) {
		return ops.MarkWorkspaceReady(ctx, w.ID, w.Expect(), "/actual/root", "used_repository", t0+3)
	}
	err = db.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		_, err := ready(tx, ctx)
		must(t, err)
		return errRollback
	})
	mustIs(t, err, errRollback)
	before := getSession(t, db, "a")
	mustEqual(t, "rolled back session", before.WorkspaceBlocked && before.LastEventSeq == 1, true)
	cwds, err := db.ListSessionCwds(ctx, "a")
	must(t, err)
	mustEqual(t, "rolled back cwd", cwds[0].Cwd, store.DefaultWorkspacePath(w.ProjectID))
	change, err = ready(db, ctx)
	must(t, err)
	mustEqual(t, "ready events", change.Notices, []store.WorkspaceNotice{{SessionID: "a", Seq: 2}, {SessionID: "b", Seq: 2}})
	mustEqual(t, "reset lost streak", change.Workspace.LostStreak, int64(0))
	mustEqual(t, "reset backoff streak", change.Workspace.BackoffStreak, int64(0))
	mustEqual(t, "same-clock ready version", change.Workspace.UpdatedAt, w.UpdatedAt+1)
	mustEqual(t, "idle session activity", getSession(t, db, "b").LastActivityAt, t0)
	paged, err := db.ListSessions(ctx, store.SessionFilter{ProjectID: w.ProjectID, Now: t0 + 4, Limit: 2})
	must(t, err)
	mustEqual(t, "session ordering after workspace events", []string{paged[0].ID, paged[1].ID}, []string{"a", "explicit"})
	for _, id := range []string{"a", "b", "stopped", "explicit"} {
		cwds, err := db.ListSessionCwds(ctx, id)
		must(t, err)
		want := "/actual/root"
		if id == "explicit" {
			want = store.DefaultWorkspacePath(w.ProjectID)
		}
		mustEqual(t, "cwd "+id, cwds[0].Cwd, want)
		mustEqual(t, "other device cwd "+id, cwds[1].Cwd, "/another-device")
	}
	page, err := db.ReadEvents(ctx, store.EventFilter{SessionID: "a", Limit: 8, MaxBytes: 4096})
	must(t, err)
	mustEqual(t, "ready durable event", page.Events[1].Kind, store.EventKindWorkspace)
	mustEqual(t, "event versions", []int64{page.Events[0].At, page.Events[1].At}, []int64{w.UpdatedAt, change.Workspace.UpdatedAt})
	mustEqual(t, "ready payload", string(page.Events[1].Payload), string(change.Workspace.EventPayload()))
	_, err = db.AcquireLease(ctx, leaseReq("a", f.worker.ID, "boot", "conn", t0+5))
	must(t, err)
}

func testWorkspacePrune(t *testing.T, b Backend) {
	db, _, f, spec := workspaceFixture(t, b)
	ctx := deadline(t)
	w, _, err := db.EnsureWorkspace(ctx, spec)
	must(t, err)
	change, err := db.TransitionWorkspace(ctx, w.ID, w.Expect(), store.WorkspacePatch{
		State: store.WorkspacePreparing, Attempt: 1, CallID: "ws-current", TimeoutMs: 120000, UpdatedAt: t0 + 1,
	})
	must(t, err)
	w = change.Workspace

	for _, id := range []string{w.CallID, "ws-retired", "ws-active"} {
		call := newCall(id, f)
		call.SessionID, call.Tool = w.SessionID(), "prepare_workspace"
		if id != "ws-active" {
			call.State, call.FinishedAt, call.OutcomeReason = store.CallOffline, t0+1, "DEVICE_OFFLINE"
		}
		must(t, db.CreateCall(ctx, call))
	}
	count, err := db.PruneWorkspaceCalls(ctx, "", 10)
	must(t, err)
	mustEqual(t, "only the retired call was pruned", count, 1)
	for _, keep := range []string{w.CallID, "ws-active"} {
		_, err = db.GetCall(ctx, keep)
		must(t, err)
	}
	_, err = db.GetCall(ctx, "ws-retired")
	mustIs(t, err, store.ErrNotFound)

	_, err = db.MarkWorkspaceReady(ctx, w.ID, w.Expect(), "/home/u/circus/projects/session-project", "created", t0+2)
	must(t, err)
	count, err = db.PruneWorkspaceCalls(ctx, w.SessionID(), 10)
	must(t, err)
	mustEqual(t, "final workspace no longer references the call", count, 1)
	_, err = db.GetCall(ctx, w.CallID)
	mustIs(t, err, store.ErrNotFound)
	_, err = db.GetCall(ctx, "ws-active")
	must(t, err)
}
