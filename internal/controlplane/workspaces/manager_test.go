package workspaces

import (
	"context"
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/clock"
	"github.com/EricWvi/circus/internal/controlplane/calls"
	"github.com/EricWvi/circus/internal/controlplane/sessions"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/store/sqlite"
	"github.com/EricWvi/circus/internal/testutil/wait"
	"github.com/EricWvi/circus/internal/wire"
	"google.golang.org/protobuf/proto"
)

type fakeUpdate struct {
	update *pb.CallUpdate
	err    error
}

type fakeSubscription chan fakeUpdate

func (s fakeSubscription) Next(ctx context.Context) (*pb.CallUpdate, error) {
	select {
	case got := <-s:
		return got.update, got.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s fakeSubscription) Close() error { return nil }

type fakeCall struct {
	request      *pb.DeviceCall
	user, device string
	reply        fakeSubscription
	ctx          context.Context
}

type managerFixture struct {
	t         *testing.T
	st        *sqlite.DB
	clk       *clock.Fake
	available atomic.Int32
	submitted chan fakeCall
	submitErr atomic.Pointer[error]
	bus       *sessions.Bus
}

func newManagerFixture(t *testing.T) *managerFixture {
	st, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "workspace.db"), sqlite.Options{})
	check(t, err)
	t.Cleanup(func() { _ = st.Close() })
	f := &managerFixture{t: t, st: st, clk: clock.New(), submitted: make(chan fakeCall, 128), bus: sessions.NewBus(sessions.BusOptions{})}
	f.available.Store(2)
	return f
}

func check(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func (f *managerFixture) now() int64 { return f.clk.Now().UnixMilli() }

func (f *managerFixture) user(id string) {
	f.t.Helper()
	check(f.t, f.st.CreateUser(f.t.Context(), store.User{ID: id, Name: id, CreatedAt: f.now()}))
}

func (f *managerFixture) device(user, id, os string) {
	f.t.Helper()
	ctx := f.t.Context()
	check(f.t, f.st.CreateIdentity(ctx, store.Identity{Kind: store.KindDevice, ID: id, UserID: user, Name: id, MachineID: id, OS: os, CreatedAt: f.now()}))
	_, err := f.st.SetConnected(ctx, store.KindDevice, id, "boot", f.now())
	check(f.t, err)
}

func (f *managerFixture) workspace(user, device, id string) store.Workspace {
	f.t.Helper()
	ctx := f.t.Context()
	_, err := f.st.CreateProject(ctx, store.Project{ID: "p-" + id, Name: id, UserID: user, RepoURL: "https://user:token@example.com/repo", CreatedAt: f.now(), UpdatedAt: f.now()})
	check(f.t, err)
	w, _, err := f.st.EnsureWorkspace(ctx, store.Workspace{ID: id, UserID: user, ProjectID: "p-" + id, DeviceID: device, CreatedAt: f.now()})
	check(f.t, err)
	return w
}

func (f *managerFixture) setup(os string) store.Workspace {
	f.t.Helper()
	f.user("u")
	f.device("u", "d", os)
	return f.workspace("u", "d", "w")
}

func (f *managerFixture) start(options ...func(*Config)) (*Manager, func()) {
	f.t.Helper()
	cfg := Config{Store: f.st, Clock: f.clk, Bus: f.bus,
		Supports: func(_ string, feature string) (bool, bool) {
			if feature != wire.FeaturePrepareWorkspace {
				return false, false
			}
			v := f.available.Load()
			return v > 0, v == 2
		},
		Submit: func(ctx context.Context, user, device string, request *pb.DeviceCall) (Subscription, error) {
			call := fakeCall{request, user, device, make(fakeSubscription, 8), ctx}
			resultErr := f.submitErr.Load()
			select {
			case f.submitted <- call:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			if resultErr != nil {
				return nil, *resultErr
			}
			return call.reply, nil
		},
	}
	for _, option := range options {
		option(&cfg)
	}
	m := New(cfg)
	ctx, cancel := context.WithCancel(f.t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.Run(ctx)
	}()
	stop := func() { cancel(); wait.Recv(f.t, done) }
	f.t.Cleanup(stop)
	return m, stop
}

func (f *managerFixture) row(w store.Workspace, predicate func(store.Workspace) bool) store.Workspace {
	f.t.Helper()
	var got store.Workspace
	wait.For(f.t, func() bool {
		var err error
		got, err = f.st.GetWorkspaceByID(f.t.Context(), w.ID)
		check(f.t, err)
		return predicate(got)
	})
	return got
}

func (f *managerFixture) next() fakeCall {
	f.t.Helper()
	return wait.Recv(f.t, f.submitted)
}

func readyResult(projectID string) *pb.CallUpdate {
	return prepareResult(&pb.PrepareWorkspaceResult{
		Outcome: pb.PrepareWorkspaceResult_OUTCOME_READY, Action: pb.PrepareWorkspaceResult_ACTION_CLONED, Path: "/home/d/circus/projects/" + projectID,
	})
}

func prepareResult(result *pb.PrepareWorkspaceResult) *pb.CallUpdate {
	return &pb.CallUpdate{Update: &pb.CallUpdate_Result{Result: &pb.DeviceResult{Detail: &pb.DeviceResult_PrepareWorkspace{PrepareWorkspace: result}}}}
}

func fault(code pb.DaemonFault_Code) *pb.CallUpdate {
	return &pb.CallUpdate{Update: &pb.CallUpdate_Result{Result: &pb.DeviceResult{Fault: &pb.DaemonFault{Code: code}}}}
}

func rejected(code pb.CallUpdate_Rejected_Code) *pb.CallUpdate {
	return &pb.CallUpdate{Update: &pb.CallUpdate_Rejected_{Rejected: &pb.CallUpdate_Rejected{Code: code}}}
}

func failedResult(code pb.WorkspaceError) *pb.CallUpdate {
	return prepareResult(&pb.PrepareWorkspaceResult{Outcome: pb.PrepareWorkspaceResult_OUTCOME_FAILED, Error: code,
		ErrorDetail: "git could not open https://user:token@example.com/repo"})
}

func TestWorkspaceCallOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		reply   *pb.CallUpdate
		state   store.WorkspaceState
		waiting store.WorkspaceWaiting
		code    pb.WorkspaceError
	}{
		{"ready", readyResult("p-w"), store.WorkspaceReady, "", 0},
		{"ordinary failure", failedResult(pb.WorkspaceError_WORKSPACE_ERROR_CLONE_FAILED), store.WorkspaceFailed, "", pb.WorkspaceError_WORKSPACE_ERROR_CLONE_FAILED},
		{"busy", failedResult(pb.WorkspaceError_WORKSPACE_ERROR_BUSY), store.WorkspacePending, store.WorkspaceBackoff, 0},
		{"overloaded", rejected(pb.CallUpdate_Rejected_CODE_OVERLOADED), store.WorkspacePending, store.WorkspaceBackoff, 0},
		{"store unavailable", rejected(pb.CallUpdate_Rejected_CODE_STORE_UNAVAILABLE), store.WorkspacePending, store.WorkspaceBackoff, 0},
		{"shutdown", rejected(pb.CallUpdate_Rejected_CODE_DEVICE_SHUTTING_DOWN), store.WorkspacePending, store.WorkspaceBackoff, 0},
		{"offline", &pb.CallUpdate{Update: &pb.CallUpdate_Offline{Offline: &pb.CallUpdate_DeviceOffline{}}}, store.WorkspacePending, store.WorkspaceOffline, 0},
		{"tool unavailable", fault(pb.DaemonFault_CODE_TOOL_UNAVAILABLE), store.WorkspacePending, store.WorkspaceOutdated, 0},
		{"daemon fault", fault(pb.DaemonFault_CODE_PANIC), store.WorkspaceFailed, "", pb.WorkspaceError_WORKSPACE_ERROR_DAEMON_FAULT},
		{"revoked", rejected(pb.CallUpdate_Rejected_CODE_UNKNOWN_DEVICE), store.WorkspaceFailed, "", pb.WorkspaceError_WORKSPACE_ERROR_DEVICE_REVOKED},
		{"invalid", rejected(pb.CallUpdate_Rejected_CODE_INVALID), store.WorkspaceFailed, "", pb.WorkspaceError_WORKSPACE_ERROR_DAEMON_FAULT},
		{"mismatch", rejected(pb.CallUpdate_Rejected_CODE_CALL_MISMATCH), store.WorkspaceFailed, "", pb.WorkspaceError_WORKSPACE_ERROR_DAEMON_FAULT},
		{"cancelled", &pb.CallUpdate{Update: &pb.CallUpdate_Cancelled_{Cancelled: &pb.CallUpdate_Cancelled{}}}, store.WorkspaceFailed, "", pb.WorkspaceError_WORKSPACE_ERROR_CANCELLED},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newManagerFixture(t)
			w := f.setup("linux")
			m, _ := f.start()
			call := f.next()
			row := f.row(w, func(x store.Workspace) bool { return x.State == store.WorkspacePreparing })
			rq := call.request.GetPrepareWorkspace()
			if call.user != w.UserID || call.device != w.DeviceID || call.request.GetCallId() != row.CallID ||
				call.request.GetSessionId() != w.SessionID() || call.request.GetTimeoutMs() != 120000 ||
				rq.GetPath() != store.DefaultWorkspacePath(w.ProjectID) || rq.GetRepoUrl() != row.RepoURL {
				t.Fatalf("request does not match committed row: %v", call.request)
			}
			if tc.waiting == store.WorkspaceOffline {
				f.available.Store(0)
			}
			call.reply <- fakeUpdate{update: tc.reply}
			row = f.row(w, func(x store.Workspace) bool { return x.State == tc.state && x.Waiting == tc.waiting })
			if row.ErrorCode != tc.code || strings.Contains(row.ErrorDetail, "token") || strings.Contains(row.Status().GetRepoUrl(), "token") {
				t.Fatalf("settled workspace = %+v", row)
			}
			if tc.waiting == store.WorkspaceBackoff {
				if row.RetryAt-f.now() != 5000 {
					t.Fatalf("first backoff = %d ms", row.RetryAt-f.now())
				}
				f.clk.Advance(5 * time.Second)
				if f.next().request.GetCallId() == call.request.GetCallId() {
					t.Fatal("backoff reused the call id")
				}
			}
			if tc.waiting == store.WorkspaceOutdated {
				m.DeviceOnline(w.UserID, w.DeviceID)
				f.clk.Advance(time.Minute)
				if row = f.row(w, func(x store.Workspace) bool { return x.State == store.WorkspacePending }); row.Attempt != 1 {
					t.Fatal("daemon fault was retried on the same connection")
				}
				_, err := f.st.SetConnected(t.Context(), store.KindDevice, w.DeviceID, "new-boot", f.now())
				check(t, err)
				m.DeviceOnline(w.UserID, w.DeviceID)
				if f.next().request.GetCallId() == call.request.GetCallId() {
					t.Fatal("reconnect reused the failed call id")
				}
			}
		})
	}
}

func TestWorkspaceWaitingAndScan(t *testing.T) {
	for _, tc := range []struct {
		name      string
		available int32
		os        string
		revoke    bool
		waiting   store.WorkspaceWaiting
		code      pb.WorkspaceError
	}{
		{"offline", 0, "linux", false, store.WorkspaceOffline, 0},
		{"old daemon", 1, "linux", false, store.WorkspaceOutdated, 0},
		{"windows", 2, "windows", false, "", pb.WorkspaceError_WORKSPACE_ERROR_UNSUPPORTED_OS},
		{"revoked", 2, "linux", true, "", pb.WorkspaceError_WORKSPACE_ERROR_DEVICE_REVOKED},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newManagerFixture(t)
			f.available.Store(tc.available)
			w := f.setup(tc.os)
			if tc.revoke {
				_, err := f.st.DeleteIdentity(t.Context(), store.KindDevice, w.DeviceID, f.now())
				check(t, err)
			}
			_, err := f.st.CreateSession(t.Context(), store.Session{
				ID: "s", ClientRequestID: "s", ClaudeSessionID: "sdk", UserID: w.UserID,
				ProjectID: w.ProjectID, DefaultDeviceID: w.DeviceID, Workspace: true, CreatedAt: f.now(),
			}, nil, 10)
			check(t, err)
			watcher, err := f.bus.Watch("s", "u")
			check(t, err)
			defer watcher.Close()
			f.start()
			f.row(w, func(x store.Workspace) bool {
				if tc.code != 0 {
					return x.State == store.WorkspaceFailed && x.ErrorCode == tc.code
				}
				return x.State == store.WorkspacePending && x.Waiting == tc.waiting
			})
			select {
			case c := <-f.submitted:
				t.Fatalf("unexpected prepare: %v", c.request)
			default:
			}
			if tc.code == 0 {
				wait.Recv(t, watcher.Ready())
				items, err := watcher.Take()
				check(t, err)
				if len(items) != 1 || !items[0].Event.GetEphemeral() || items[0].Event.GetSeq() != 0 ||
					items[0].Event.GetKind() != pb.SessionEvent_KIND_WORKSPACE {
					t.Fatalf("ephemeral notice = %+v", items)
				}
				f.available.Store(2)
				f.clk.Advance(scanEvery)
				f.next().reply <- fakeUpdate{update: readyResult(w.ProjectID)}
				f.row(w, func(x store.Workspace) bool { return x.State == store.WorkspaceReady })
			}
		})
	}
}

func TestWorkspaceRecoveryAndLostStreak(t *testing.T) {
	f := newManagerFixture(t)
	w := f.setup("linux")
	_, stop := f.start()
	first := f.next()
	newURL := "https://new.example.com/repo"
	_, err := f.st.UpdateProject(t.Context(), w.UserID, w.ProjectID, store.ProjectPatch{RepoURL: &newURL, UpdatedAt: f.now()})
	check(t, err)
	stop()
	m, _ := f.start()
	attached := f.next()
	if !proto.Equal(first.request, attached.request) {
		t.Fatalf("different request after restart: %v", attached.request)
	}
	for _, disconnect := range []error{calls.ErrOverflow, calls.ErrClosed} {
		attached.reply <- fakeUpdate{err: disconnect}
		m.Kick(w.ID)
		attached = f.next()
		if !proto.Equal(first.request, attached.request) {
			t.Fatalf("different request after subscription loss: %v", attached.request)
		}
	}
	attached.reply <- fakeUpdate{update: &pb.CallUpdate{Update: &pb.CallUpdate_Unknown_{Unknown: &pb.CallUpdate_Unknown{}}}}
	attached.reply <- fakeUpdate{err: calls.ErrResultUnavailable}
	for i := int64(1); i <= 2; i++ {
		next := f.next()
		row := f.row(w, func(x store.Workspace) bool { return x.State == store.WorkspacePreparing && x.LostStreak == i })
		if next.request.GetCallId() == attached.request.GetCallId() || next.request.GetPrepareWorkspace().GetRepoUrl() != newURL || row.CallID != next.request.GetCallId() {
			t.Fatalf("lost result did not start a fresh attempt: %v", next.request)
		}
		attached = next
		attached.reply <- fakeUpdate{update: &pb.CallUpdate{Update: &pb.CallUpdate_Lost_{Lost: &pb.CallUpdate_Lost{}}}}
	}
	f.row(w, func(x store.Workspace) bool {
		return x.State == store.WorkspaceFailed && x.ErrorCode == pb.WorkspaceError_WORKSPACE_ERROR_INTERRUPTED && x.LostStreak == 3
	})
}

func TestWorkspaceBackground(t *testing.T) {
	for _, state := range []store.JobState{store.JobExited, store.JobKilled, store.JobLost} {
		t.Run(string(state), func(t *testing.T) {
			f := newManagerFixture(t)
			w := f.setup("linux")
			m, stop := f.start()
			first := f.next()
			ctx := t.Context()
			id := first.request.GetCallId()
			f.persistDispatched(w, id)
			job := store.Job{DeviceID: w.DeviceID, BootID: "boot", UserID: w.UserID, SessionID: w.SessionID(),
				CallID: id, JobID: "abcd01234567", LastSeq: 1, State: store.JobRunning, StartedAt: f.now(), UpdatedAt: f.now()}
			_, err := f.st.PutJob(ctx, job)
			check(t, err)
			first.reply <- fakeUpdate{update: prepareResult(&pb.PrepareWorkspaceResult{Outcome: pb.PrepareWorkspaceResult_OUTCOME_IN_BACKGROUND, JobId: job.JobID})}
			f.row(w, func(x store.Workspace) bool { return x.JobID == job.JobID && x.JobBootID == "boot" })
			if state == store.JobKilled {
				stop()
			}
			job.State, job.LastSeq, job.EndedAt = state, 2, f.now()
			if state != store.JobLost {
				job.ExitCode = new(int32(0))
			}
			_, err = f.st.PutJob(ctx, job)
			check(t, err)
			if state == store.JobKilled {
				f.start() // existing terminal job is sufficient on recovery
			} else {
				m.WorkspaceJobEnded(w.UserID, w.SessionID(), job.JobID)
			}
			next := f.next()
			resume := job.JobID
			if state == store.JobLost {
				resume = ""
				f.row(w, func(x store.Workspace) bool { return x.LostStreak == 1 })
			}
			if next.request.GetPrepareWorkspace().GetResumeJobId() != resume || next.request.GetCallId() == id {
				t.Fatalf("follow-up request = %v", next.request)
			}
			next.reply <- fakeUpdate{update: readyResult(w.ProjectID)}
			f.row(w, func(x store.Workspace) bool { return x.State == store.WorkspaceReady })
		})
	}
}

func TestWorkspaceLimits(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		users, devices, perDevice int
		want                      int
	}{
		{"device", 1, 1, 3, 2},
		{"user", 1, 3, 2, 4},
		{"global", 17, 2, 2, 64},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newManagerFixture(t)
			var rows []store.Workspace
			for i := range tc.users {
				u := fmt.Sprintf("u%d", i)
				f.user(u)
				for j := range tc.devices {
					d := fmt.Sprintf("d%d-%d", i, j)
					f.device(u, d, "linux")
					for k := range tc.perDevice {
						rows = append(rows, f.workspace(u, d, fmt.Sprintf("%s-w%d", d, k)))
					}
				}
			}
			m, _ := f.start()
			for _, w := range rows {
				m.Kick(w.ID)
			}
			for range tc.want {
				f.next()
			}
			active := 0
			for _, w := range rows {
				row := f.row(w, func(x store.Workspace) bool {
					return x.State == store.WorkspacePreparing || x.Waiting == store.WorkspaceLimit
				})
				if row.State == store.WorkspacePreparing {
					active++
				}
			}
			if active != tc.want {
				t.Fatalf("preparing = %d, want %d", active, tc.want)
			}
		})
	}
}

func TestBackoffCap(t *testing.T) {
	m := New(Config{Clock: clock.New()})
	for i, want := range []time.Duration{5, 10, 20, 40, 80, 160, 300, 300} {
		w := store.Workspace{WorkspacePatch: store.WorkspacePatch{Attempt: 30, BackoffStreak: int64(i)}}
		if got := m.backoff(w).RetryAt - m.now(); got != (want * time.Second).Milliseconds() {
			t.Fatalf("backoff after %d failures = %d ms", i, got)
		}
	}
}

func TestWorkspaceOfflineReattach(t *testing.T) {
	f := newManagerFixture(t)
	w := f.setup("linux")
	f.submitErr.Store(new(error(calls.ErrDeviceOffline)))
	m, _ := f.start()
	first := f.next()
	f.submitErr.Store(nil)
	m.DeviceOnline(w.UserID, w.DeviceID)
	next := f.next()
	if !proto.Equal(first.request, next.request) {
		t.Fatalf("different request after offline attach: %v", next.request)
	}
	next.reply <- fakeUpdate{update: readyResult(w.ProjectID)}
	f.row(w, func(x store.Workspace) bool { return x.State == store.WorkspaceReady })
}

func (f *managerFixture) persistDispatched(w store.Workspace, id string) {
	f.t.Helper()
	ctx := f.t.Context()
	hash := sha256.Sum256([]byte(id))
	check(f.t, f.st.CreateCall(ctx, store.Call{
		CallID: id, UserID: w.UserID, DeviceID: w.DeviceID, SessionID: w.SessionID(),
		Tool: "prepare_workspace", RequestHash: hash[:], RequestBytes: 42,
		State: store.CallAccepted, SubmittedAt: f.now(),
	}))
	changed, err := f.st.TransitionCall(ctx, id, store.CallExpect{States: []store.CallState{store.CallAccepted}}, store.CallDispatched,
		store.CallPatch{DispatchedBootID: "boot", DispatchedAt: f.now()})
	check(f.t, err)
	if !changed {
		f.t.Fatal("dispatch did not apply")
	}
}
