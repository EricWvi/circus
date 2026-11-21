package workspaces

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	pb "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/controlplane/sessions"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/testutil/wait"
)

func TestWorkspaceBackoffStreak(t *testing.T) {
	f := newManagerFixture(t)
	w := f.setup("linux")
	m, _ := f.start()
	busy := failedResult(pb.WorkspaceError_WORKSPACE_ERROR_BUSY)
	first := f.next()
	first.reply <- fakeUpdate{update: &pb.CallUpdate{Update: &pb.CallUpdate_Lost_{Lost: &pb.CallUpdate_Lost{}}}}
	call := f.next() // The lost attempt did not contribute to the streak.
	for i, delay := range []time.Duration{5 * time.Second, 10 * time.Second} {
		call.reply <- fakeUpdate{update: busy}
		row := f.row(w, func(w store.Workspace) bool { return w.Waiting == store.WorkspaceBackoff && w.Attempt == int64(i+2) })
		if row.BackoffStreak != int64(i+1) || row.RetryAt != f.now()+delay.Milliseconds() {
			t.Fatalf("backoff #%d: %+v", i+1, row.WorkspacePatch)
		}
		f.clk.Advance(delay)
		call = f.next()
	}

	// A call that ends offline breaks the streak. An advertised connection
	// can then begin another attempt with a fresh retry delay.
	f.available.Store(0)
	call.reply <- fakeUpdate{update: &pb.CallUpdate{Update: &pb.CallUpdate_Offline{Offline: &pb.CallUpdate_DeviceOffline{}}}}
	f.row(w, func(w store.Workspace) bool { return w.Waiting == store.WorkspaceOffline })
	f.available.Store(2)
	m.DeviceOnline(w.UserID, w.DeviceID)
	call = f.next()
	call.reply <- fakeUpdate{update: busy}
	row := f.row(w, func(w store.Workspace) bool { return w.Waiting == store.WorkspaceBackoff })
	if row.BackoffStreak != 1 || row.RetryAt-f.now() != 5000 {
		t.Fatalf("offline did not break the streak: %+v", row.WorkspacePatch)
	}
	f.clk.Advance(5 * time.Second)
	call = f.next()
	f.persistDispatched(w, call.request.GetCallId())
	f.runBackground(call, "abcd01234567")
	row = f.row(w, func(w store.Workspace) bool { return w.JobID != "" })
	if row.BackoffStreak != 0 {
		t.Fatalf("a successfully started background job did not reset the streak: %+v", row.WorkspacePatch)
	}
}

func TestWorkspaceBackoffResetAfterWaiting(t *testing.T) {
	for _, waiting := range []store.WorkspaceWaiting{store.WorkspaceOffline, store.WorkspaceLimit} {
		t.Run(string(waiting), func(t *testing.T) {
			f := newManagerFixture(t)
			w := f.setup("linux")
			m, _ := f.start()
			busy := failedResult(pb.WorkspaceError_WORKSPACE_ERROR_BUSY)

			f.next().reply <- fakeUpdate{update: busy}
			firstBackoff := f.row(w, func(row store.Workspace) bool {
				return row.Waiting == store.WorkspaceBackoff && row.BackoffStreak == 1
			})
			f.clk.Advance(time.Duration(firstBackoff.RetryAt-f.now()) * time.Millisecond)
			f.next().reply <- fakeUpdate{update: busy}
			secondBackoff := f.row(w, func(row store.Workspace) bool {
				return row.Waiting == store.WorkspaceBackoff && row.BackoffStreak == 2
			})

			var occupiesSlot fakeCall
			if waiting == store.WorkspaceOffline {
				f.available.Store(0)
			} else {
				for _, id := range []string{"slot-one", "slot-two"} {
					m.Kick(f.workspace(w.UserID, w.DeviceID, id).ID)
					occupiesSlot = f.next()
				}
			}
			f.clk.Advance(time.Duration(secondBackoff.RetryAt-f.now()) * time.Millisecond)
			row := f.row(w, func(row store.Workspace) bool { return row.Waiting == waiting })
			if row.Attempt != 2 || row.BackoffStreak != 2 {
				t.Fatalf("entering %s changed the attempt or streak: %+v", waiting, row.WorkspacePatch)
			}

			if waiting == store.WorkspaceOffline {
				f.available.Store(2)
				m.DeviceOnline(w.UserID, w.DeviceID)
			} else {
				occupiesSlot.reply <- fakeUpdate{update: failedResult(pb.WorkspaceError_WORKSPACE_ERROR_NOT_EMPTY)}
			}
			restarted := f.next()
			if restarted.request.GetSessionId() != w.SessionID() {
				t.Fatalf("started the wrong workspace: %v", restarted.request)
			}
			restarted.reply <- fakeUpdate{update: busy}
			row = f.row(w, func(row store.Workspace) bool { return row.Waiting == store.WorkspaceBackoff })
			if row.Attempt != 3 || row.BackoffStreak != 1 || row.RetryAt-f.now() != 5000 {
				t.Fatalf("retry after %s did not reset to 5 seconds: %+v", waiting, row.WorkspacePatch)
			}
		})
	}
}

func (f *managerFixture) runBackground(call fakeCall, jobID string) {
	call.reply <- fakeUpdate{update: prepareResult(&pb.PrepareWorkspaceResult{
		Outcome: pb.PrepareWorkspaceResult_OUTCOME_IN_BACKGROUND, JobId: jobID,
	})}
}

func TestWorkspaceBackgroundOnlyEvent(t *testing.T) {
	f := newManagerFixture(t)
	w := f.setup("linux")
	ctx := t.Context()
	watchers := make(map[string]*sessions.Watcher)
	for _, id := range []string{"one", "two", "stopped"} {
		_, err := f.st.CreateSession(ctx, store.Session{
			ID: id, ClientRequestID: id, ClaudeSessionID: "sdk-" + id, UserID: w.UserID,
			ProjectID: w.ProjectID, DefaultDeviceID: w.DeviceID, Workspace: true, CreatedAt: f.now(),
		}, nil, 10)
		check(t, err)
		if id == "stopped" {
			_, _, _, err = f.st.StopSession(ctx, id, f.now(), "user")
			check(t, err)
		}
		watcher, err := f.bus.Watch(id, w.UserID)
		check(t, err)
		defer watcher.Close()
		watchers[id] = watcher
	}

	_, stop := f.start()
	call := f.next()
	f.persistDispatched(w, call.request.GetCallId())
	preparing := f.row(w, func(row store.Workspace) bool { return row.State == store.WorkspacePreparing })
	for _, id := range []string{"one", "two"} {
		wait.Recv(t, watchers[id].Ready())
		items, err := watchers[id].Take()
		check(t, err)
		if len(items) != 1 || items[0].Seq != 1 || items[0].Event != nil {
			t.Fatalf("preparing notice for %s: %+v", id, items)
		}
	}

	f.runBackground(call, "abcd01234567")
	background := f.row(w, func(row store.Workspace) bool { return row.Background() })
	if background.State != preparing.State || background.Waiting != preparing.Waiting || background.UpdatedAt != preparing.UpdatedAt+1 {
		t.Fatalf("background-only transition: %+v", background.WorkspacePatch)
	}
	for _, id := range []string{"one", "two"} {
		wait.Recv(t, watchers[id].Ready())
		items, err := watchers[id].Take()
		check(t, err)
		if len(items) != 1 {
			t.Fatalf("background notices for %s: %+v", id, items)
		}
		event := items[0].Event
		if items[0].Seq != 0 || event.GetKind() != pb.SessionEvent_KIND_WORKSPACE || !event.GetEphemeral() ||
			event.GetSeq() != 0 || event.GetTsUnixMs() != background.UpdatedAt {
			t.Fatalf("background event for %s: %v", id, event)
		}
		var payload struct{ Background bool }
		check(t, json.Unmarshal([]byte(event.GetPayloadJson()), &payload))
		if !payload.Background {
			t.Fatalf("background flag missing from %s's event: %v", id, event)
		}
		page, err := f.st.ReadEvents(ctx, store.EventFilter{SessionID: id, Limit: 4, MaxBytes: 4096})
		check(t, err)
		if len(page.Events) != 1 || page.Events[0].At != preparing.UpdatedAt {
			t.Fatalf("background-only update changed replay for %s: %+v", id, page.Events)
		}
	}
	stop()
	items, err := watchers["stopped"].Take()
	check(t, err)
	if len(items) != 0 {
		t.Fatalf("stopped session received workspace notices: %+v", items)
	}
}

func TestWorkspaceExpiredBackgroundRecord(t *testing.T) {
	f := newManagerFixture(t)
	w := f.setup("linux")
	m, stop := f.start()
	first := f.next()
	f.persistDispatched(w, first.request.GetCallId())
	const expiredAfter = 2*time.Hour + 32*time.Minute + time.Second
	const firstJob = "abcd01234567"
	f.runBackground(first, firstJob)
	f.row(w, func(w store.Workspace) bool { return w.JobID == firstJob })
	f.clk.Advance(expiredAfter)
	follow := f.next()
	if follow.request.GetPrepareWorkspace().GetResumeJobId() != firstJob {
		t.Fatalf("expired job follow-up = %v", follow.request)
	}

	const secondJob = "abcd01234568"
	f.persistDispatched(w, follow.request.GetCallId())
	f.runBackground(follow, secondJob)
	second := f.row(w, func(w store.Workspace) bool { return w.JobID == secondJob })
	stop()
	if !second.ExpiredJobResumed || second.JobRecordedAt != f.now() {
		t.Fatalf("follow-up job registration was not persisted: %+v", second.WorkspacePatch)
	}
	f.clk.Advance(120 * time.Second)
	cont, err := m.checkJob(t.Context(), second, store.Identity{CurrentBootID: "boot"}, make(chan struct{}, 1))
	check(t, err)
	got, err := f.st.GetWorkspaceByID(t.Context(), w.ID)
	check(t, err)
	if !cont || got != second {
		t.Fatalf("fresh job expired on the original attempt's clock: %+v", got.WorkspacePatch)
	}
	// A later workspace write must not move the job's expiration time.
	patch := second.WorkspacePatch
	patch.UpdatedAt = f.now()
	change, err := f.st.TransitionWorkspace(t.Context(), w.ID, second.Expect(), patch)
	check(t, err)
	if !change.Applied || change.Workspace.UpdatedAt <= second.UpdatedAt || change.Workspace.JobRecordedAt != second.JobRecordedAt {
		t.Fatalf("touch changed the job registration time: %+v", change)
	}

	f.clk.Advance(expiredAfter - 120*time.Second)
	f.start()
	f.row(w, func(w store.Workspace) bool {
		return w.State == store.WorkspaceFailed && w.ErrorCode == pb.WorkspaceError_WORKSPACE_ERROR_TIMEOUT
	})
	select {
	case call := <-f.submitted:
		t.Fatalf("unexpected second expiry follow-up: %v", call.request)
	default:
	}
}

func TestWorkspaceEventClock(t *testing.T) {
	f := newManagerFixture(t)
	f.available.Store(0)
	w := f.setup("linux")
	_, err := f.st.CreateSession(t.Context(), store.Session{
		ID: "s", ClientRequestID: "s", ClaudeSessionID: "sdk", UserID: w.UserID,
		ProjectID: w.ProjectID, DefaultDeviceID: w.DeviceID, Workspace: true, CreatedAt: f.now(),
	}, nil, 10)
	check(t, err)
	watcher, err := f.bus.Watch("s", "u")
	check(t, err)
	defer watcher.Close()
	m, _ := f.start()
	previous := w.UpdatedAt
	for _, tc := range []struct {
		mode    int32
		waiting store.WorkspaceWaiting
	}{
		{0, store.WorkspaceOffline},
		{1, store.WorkspaceOutdated},
	} {
		if tc.mode != 0 {
			f.available.Store(tc.mode)
			m.DeviceOnline(w.UserID, w.DeviceID)
		}
		row := f.row(w, func(w store.Workspace) bool { return w.Waiting == tc.waiting })
		wait.Recv(t, watcher.Ready())
		items, err := watcher.Take()
		check(t, err)
		if len(items) != 1 || items[0].Event.GetKind() != pb.SessionEvent_KIND_WORKSPACE ||
			!items[0].Event.GetEphemeral() || items[0].Event.GetSeq() != 0 ||
			items[0].Event.GetTsUnixMs() != row.Status().GetUpdatedAtUnixMs() || row.UpdatedAt != previous+1 {
			t.Fatalf("waiting-only change at %d did not advance from %d with matching event: %+v", row.UpdatedAt, previous, items)
		}
		previous = row.UpdatedAt
	}
	f.available.Store(2)
	m.DeviceOnline(w.UserID, w.DeviceID)
	call := f.next()
	preparing := f.row(w, func(w store.Workspace) bool { return w.State == store.WorkspacePreparing })
	call.reply <- fakeUpdate{update: readyResult(w.ProjectID)}
	ready := f.row(w, func(w store.Workspace) bool { return w.State == store.WorkspaceReady })
	page, err := f.st.ReadEvents(t.Context(), store.EventFilter{SessionID: "s", Limit: 8, MaxBytes: 4096})
	check(t, err)
	if preparing.UpdatedAt != previous+1 || ready.UpdatedAt != preparing.UpdatedAt+1 ||
		len(page.Events) != 2 || page.Events[0].At != preparing.Status().GetUpdatedAtUnixMs() ||
		page.Events[1].At != ready.Status().GetUpdatedAtUnixMs() {
		t.Fatalf("durable versions %d, %d after %d, events: %+v", preparing.UpdatedAt, ready.UpdatedAt, previous, page.Events)
	}
}

func TestWorkspaceInvalidReadyPath(t *testing.T) {
	for _, path := range []string{"/", "/etc", "/home/u/notcircus/projects/p-w", "/home/u/circus/projects/other"} {
		t.Run(path, func(t *testing.T) {
			f := newManagerFixture(t)
			w := f.setup("linux")
			f.start()
			result := readyResult(w.ProjectID)
			result.GetResult().GetPrepareWorkspace().Path = path
			f.next().reply <- fakeUpdate{update: result}
			row := f.row(w, func(w store.Workspace) bool { return w.State != store.WorkspacePreparing })
			if row.State != store.WorkspaceFailed || row.ErrorCode != pb.WorkspaceError_WORKSPACE_ERROR_DAEMON_FAULT || row.Path != "" {
				t.Fatalf("invalid READY path marked usable: %+v", row.WorkspacePatch)
			}
		})
	}
}

func TestWorkspaceSlotFreed(t *testing.T) {
	f := newManagerFixture(t)
	w := f.setup("linux")
	others := []store.Workspace{w, f.workspace(w.UserID, w.DeviceID, "second"), f.workspace(w.UserID, w.DeviceID, "third")}
	f.start()
	first := f.next()
	second := f.next()
	var waiting store.Workspace
	for _, w := range others {
		if w.SessionID() != first.request.SessionId && w.SessionID() != second.request.SessionId {
			waiting = w
		}
	}
	f.row(waiting, func(w store.Workspace) bool { return w.Waiting == store.WorkspaceLimit })
	first.reply <- fakeUpdate{update: failedResult(pb.WorkspaceError_WORKSPACE_ERROR_NOT_EMPTY)}
	if call := f.next(); call.request.GetSessionId() != waiting.SessionID() {
		t.Fatalf("freed slot started %v instead of %s", call.request, waiting.ID)
	}
	// No clock advance or device-online hint was needed.
}

func TestWorkspacePausedProject(t *testing.T) {
	f := newManagerFixture(t)
	w := f.setup("linux")
	m, _ := f.start()
	first := f.next()
	resume := m.PauseProject(w.ProjectID)
	wait.Recv(t, first.ctx.Done())
	wait.For(t, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.runners[w.ID] == nil
	})
	other := f.workspace(w.UserID, w.DeviceID, "other")
	m.Kick(w.ID)
	m.Kick(other.ID)
	if got := f.next(); got.request.GetSessionId() != other.SessionID() {
		t.Fatalf("paused project started %v", got.request)
	}
	resume()
	m.Kick(w.ID)
	if got := f.next(); got.request.GetCallId() != first.request.GetCallId() {
		t.Fatalf("resumed project changed its call ID: %v", got.request)
	}
}

type blockedAdmissionStore struct {
	store.Store
	once             sync.Once
	entered, release chan struct{}
}

func (s *blockedAdmissionStore) Tx(ctx context.Context, fn func(context.Context, store.Ops) error) error {
	s.once.Do(func() {
		close(s.entered)
		<-s.release
	})
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.Store.Tx(ctx, fn)
}

func TestWorkspacePauseDoesNotWaitForStore(t *testing.T) {
	f := newManagerFixture(t)
	w := f.setup("linux")
	st := &blockedAdmissionStore{Store: f.st, entered: make(chan struct{}), release: make(chan struct{})}
	m, _ := f.start(func(c *Config) { c.Store = st })
	var release sync.Once
	defer release.Do(func() { close(st.release) })
	wait.Recv(t, st.entered)
	paused := make(chan func(), 1)
	go func() { paused <- m.PauseProject(w.ProjectID) }()
	resume := wait.Recv(t, paused)
	release.Do(func() { close(st.release) })
	other := f.workspace(w.UserID, w.DeviceID, "barrier")
	m.Kick(w.ID)
	m.Kick(other.ID)
	if got := f.next(); got.request.GetSessionId() != other.SessionID() {
		t.Fatalf("paused admission submitted %v", got.request)
	}
	row, err := f.st.GetWorkspaceByID(t.Context(), w.ID)
	check(t, err)
	if row.State != store.WorkspacePending || row.CallID != "" {
		t.Fatalf("paused admission wrote a call: %+v", row.WorkspacePatch)
	}
	resume()
	m.Kick(w.ID)
	if got := f.next(); got.request.GetSessionId() != w.SessionID() {
		t.Fatalf("unpaused project did not start: %v", got.request)
	}
}

func TestWorkspaceRevokedMidAttempt(t *testing.T) {
	f := newManagerFixture(t)
	w := f.setup("linux")
	f.start()
	call := f.next() // It's still subscribed and the device never replies.
	_, err := f.st.DeleteIdentity(t.Context(), store.KindDevice, w.DeviceID, f.now())
	check(t, err)
	f.clk.Advance(scanEvery)
	f.row(w, func(w store.Workspace) bool {
		return w.State == store.WorkspaceFailed && w.ErrorCode == pb.WorkspaceError_WORKSPACE_ERROR_DEVICE_REVOKED
	})
	wait.Recv(t, call.ctx.Done())
}
