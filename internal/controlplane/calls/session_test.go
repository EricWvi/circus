package calls_test

import (
	"errors"
	"strings"
	"sync"
	"testing"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/controlplane/calls"
	"github.com/EricWvi/circus/internal/controlplane/gateway"
	"github.com/EricWvi/circus/internal/store"
)

// TestJobEndedHook: a job of a session that ends, by event, by snapshot or
// by omission from a snapshot, is reported once per change; running jobs
// and repeats are not, nor jobs naming a session that does not exist or is
// another user's.
func TestJobEndedHook(t *testing.T) {
	var mu sync.Mutex
	var ended []string
	f := setup(t, func(c *calls.Config, _ *gateway.Config) {
		c.JobEnded = func(id string) { mu.Lock(); defer mu.Unlock(); ended = append(ended, id) }
	})
	got := func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), ended...) }
	_, err := f.db.CreateProject(f.ctx, store.Project{ID: "p", UserID: "u", Name: "p", CreatedAt: 1, UpdatedAt: 1})
	must(t, err)
	for _, id := range []string{"s-a", "s-b", "s-c"} {
		if _, err := f.db.CreateSession(f.ctx, store.Session{ID: id, ClientRequestID: id, UserID: "u", ProjectID: "p", ClaudeSessionID: "c-" + id, DefaultDeviceID: "d", CreatedAt: 1}, nil, 10); err != nil {
			t.Fatal(err)
		}
	}
	must(t, f.db.CreateUser(f.ctx, store.User{ID: "u2", Name: "other", CreatedAt: 1}))
	must(t, f.db.CreateIdentity(f.ctx, store.Identity{Kind: store.KindDevice, ID: "d2", UserID: "u2", Name: "desk2", MachineID: "m2", CreatedAt: 1}))
	_, err = f.db.CreateProject(f.ctx, store.Project{ID: "p2", UserID: "u2", Name: "p", CreatedAt: 1, UpdatedAt: 1})
	must(t, err)
	if _, err := f.db.CreateSession(f.ctx, store.Session{ID: "theirs", ClientRequestID: "theirs", UserID: "u2", ProjectID: "p2", ClaudeSessionID: "c2", DefaultDeviceID: "d2", CreatedAt: 1}, nil, 10); err != nil {
		t.Fatal(err)
	}
	l := f.connect("boot")
	job := func(id, session string, seq uint64, st circusv1.JobEvent_State) *circusv1.JobEvent {
		return &circusv1.JobEvent{JobId: id, SessionId: session, CallId: "call", Seq: seq, State: st}
	}
	send(t, f.ctx, l, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_JobEvent{JobEvent: job("00000000000a", "s-a", 1, circusv1.JobEvent_STATE_RUNNING)}})
	send(t, f.ctx, l, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_JobEvent{JobEvent: job("00000000000a", "s-a", 2, circusv1.JobEvent_STATE_EXITED)}})
	eventually(t, func() bool { return len(got()) == 1 && got()[0] == "s-a" })
	// A snapshot ends one job and omits a running one (lost).
	send(t, f.ctx, l, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_JobEvent{JobEvent: job("00000000000b", "s-b", 3, circusv1.JobEvent_STATE_RUNNING)}})
	send(t, f.ctx, l, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_JobEvent{JobEvent: job("00000000000c", "s-c", 4, circusv1.JobEvent_STATE_RUNNING)}})
	eventually(t, func() bool { rows, _ := f.db.ListJobs(f.ctx, store.JobFilter{Limit: 8}); return len(rows) == 3 })
	send(t, f.ctx, l, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_JobSnapshot{JobSnapshot: &circusv1.JobSnapshot{Seq: 5,
		Jobs: []*circusv1.JobEvent{job("00000000000b", "s-b", 5, circusv1.JobEvent_STATE_KILLED)}}}})
	eventually(t, func() bool { return len(got()) == 3 })
	seen := map[string]bool{}
	for _, id := range got() {
		seen[id] = true
	}
	if !seen["s-b"] || !seen["s-c"] {
		t.Fatalf("ended %v", got())
	}
	// Ended jobs naming another user's session or no session wake nothing.
	before := f.hooks.events.Load()
	send(t, f.ctx, l, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_JobEvent{JobEvent: job("00000000000d", "theirs", 6, circusv1.JobEvent_STATE_EXITED)}})
	send(t, f.ctx, l, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_JobEvent{JobEvent: job("00000000000e", "made-up", 7, circusv1.JobEvent_STATE_KILLED)}})
	eventually(t, func() bool { return f.hooks.events.Load() >= before+2 })
	if rows, err := f.db.ListJobs(f.ctx, store.JobFilter{Limit: 8}); err != nil || len(rows) != 5 {
		t.Fatalf("jobs %v %v", rows, err)
	}
	if n := len(got()); n != 3 {
		t.Fatalf("ended %v", got())
	}
	// Nor do they by snapshot, or by omission from one.
	send(t, f.ctx, l, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_JobEvent{JobEvent: job("00000000000f", "theirs", 8, circusv1.JobEvent_STATE_RUNNING)}})
	send(t, f.ctx, l, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_JobEvent{JobEvent: job("000000000010", "made-up", 9, circusv1.JobEvent_STATE_RUNNING)}})
	eventually(t, func() bool { return f.hooks.events.Load() >= before+4 })
	snaps := f.hooks.snapshots.Load()
	send(t, f.ctx, l, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_JobSnapshot{JobSnapshot: &circusv1.JobSnapshot{Seq: 11,
		Jobs: []*circusv1.JobEvent{job("000000000011", "theirs", 10, circusv1.JobEvent_STATE_EXITED), job("000000000012", "made-up", 11, circusv1.JobEvent_STATE_KILLED)}}}})
	eventually(t, func() bool { return f.hooks.snapshots.Load() > snaps })
	rows, err := f.db.ListJobs(f.ctx, store.JobFilter{Limit: 16, States: []store.JobState{store.JobRunning}})
	must(t, err)
	if len(rows) != 0 {
		t.Fatalf("omitted jobs not lost %+v", rows)
	}
	if n := len(got()); n != 3 {
		t.Fatalf("ended %v", got())
	}
}

// TestSessionAdmission: a session call is created only after
// AdmitSessionCall passes in the same transaction; refusals are terminal
// rejections that record nothing and keep the router healthy.
func TestSessionAdmission(t *testing.T) {
	f := setup(t)
	must(t, f.db.CreateIdentity(f.ctx, store.Identity{Kind: store.KindWorker, ID: "w", UserID: "u", Name: "w", MachineID: "m", CreatedAt: 1}))
	_, err := f.db.CreateProject(f.ctx, store.Project{ID: "p", UserID: "u", Name: "p", CreatedAt: 1, UpdatedAt: 1})
	must(t, err)
	if _, err := f.db.CreateSession(f.ctx, store.Session{ID: "s", ClientRequestID: "s", UserID: "u", ProjectID: "p", ClaudeSessionID: "c", DefaultDeviceID: "d", CreatedAt: 1}, nil, 10); err != nil {
		t.Fatal(err)
	}
	now := f.clock.Now().UnixMilli()
	r, err := f.db.AcquireLease(f.ctx, store.LeaseRequest{SessionID: "s", WorkerID: "w", BootID: "b", ConnID: "conn", Now: now, ExpiresAt: now + 60_000})
	must(t, err)
	sessionCall := func(id string) *circusv1.DeviceCall {
		return &circusv1.DeviceCall{CallId: id, SessionId: "s", Tool: &circusv1.DeviceCall_Bash{Bash: &circusv1.BashRequest{Command: "true", Cwd: "/tmp"}}}
	}
	first := func(id string, a calls.Admission) *circusv1.CallUpdate {
		t.Helper()
		s, err := f.router.SubmitSession(f.ctx, "u", "desk", sessionCall(id), a)
		must(t, err)
		defer s.Close()
		u, err := s.Next(f.ctx)
		must(t, err)
		return u
	}
	good := calls.Admission{Epoch: uint64(r.Lease.Epoch), ToolUseID: "tu", WorkerID: "w", BootID: "b", ConnID: "conn"}
	stale := good
	stale.ConnID = "other"
	for name, tc := range map[string]struct {
		a    calls.Admission
		code circusv1.CallUpdate_Rejected_Code
	}{
		"no session fields": {calls.Admission{}, circusv1.CallUpdate_Rejected_CODE_SESSION_FENCED},
		"other connection":  {stale, circusv1.CallUpdate_Rejected_CODE_SESSION_FENCED},
		"no tool use id":    {calls.Admission{Epoch: 1, WorkerID: "w", BootID: "b", ConnID: "conn"}, circusv1.CallUpdate_Rejected_CODE_SESSION_FENCED},
		"long tool use id":  {calls.Admission{Epoch: 1, ToolUseID: string(make([]byte, 129)), WorkerID: "w", BootID: "b", ConnID: "conn"}, circusv1.CallUpdate_Rejected_CODE_INVALID},
	} {
		t.Run(name, func(t *testing.T) {
			id := strings.ReplaceAll(name, " ", "-")
			if u := first(id, tc.a); u.GetRejected().GetCode() != tc.code {
				t.Fatalf("update %v, want %v", u, tc.code)
			}
			if _, err := f.db.GetCall(f.ctx, id); err == nil {
				t.Fatal("refused call recorded")
			}
		})
	}
	if u := first("ok", good); u.GetOffline() == nil {
		t.Fatalf("admitted %v", u)
	}
	if !f.router.Healthy() {
		t.Fatal("a refusal marked the store unhealthy")
	}
}

// A non-session submit naming another user's session is refused before routing.
func TestNonSessionCallOtherUsersSession(t *testing.T) {
	f := setup(t)
	must(t, f.db.CreateUser(f.ctx, store.User{ID: "u2", Name: "other", CreatedAt: 1}))
	must(t, f.db.CreateIdentity(f.ctx, store.Identity{Kind: store.KindDevice, ID: "d2", UserID: "u2", Name: "desk2", MachineID: "m2", CreatedAt: 1}))
	_, err := f.db.CreateProject(f.ctx, store.Project{ID: "p2", UserID: "u2", Name: "p", CreatedAt: 1, UpdatedAt: 1})
	must(t, err)
	if _, err := f.db.CreateSession(f.ctx, store.Session{ID: "theirs", ClientRequestID: "theirs", UserID: "u2", ProjectID: "p2", ClaudeSessionID: "c2", DefaultDeviceID: "d2", CreatedAt: 1}, nil, 10); err != nil {
		t.Fatal(err)
	}
	s, err := f.router.Submit(f.ctx, "u", "desk", &circusv1.DeviceCall{CallId: "mine", SessionId: "theirs", Tool: &circusv1.DeviceCall_Bash{Bash: &circusv1.BashRequest{Command: "true", Cwd: "/tmp"}}})
	must(t, err)
	defer s.Close()
	u, err := s.Next(f.ctx)
	must(t, err)
	if u.GetRejected().GetCode() != circusv1.CallUpdate_Rejected_CODE_SESSION_FENCED {
		t.Fatalf("update %v, want SESSION_FENCED", u)
	}
	if _, err := f.db.GetCall(f.ctx, "mine"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("refused call was stored: %v", err)
	}
}
