package calls_test

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/controlplane/calls"
	"github.com/EricWvi/circus/internal/controlplane/gateway"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/testutil/wait"
	"github.com/EricWvi/circus/internal/wire"
)

// jobFixture is a router fixture with user u's session "s" leased by worker
// w, a connected device boot "boot", and the JobEnded wakes recorded.
type jobFixture struct {
	*fixture
	link  *wire.Link
	fence store.Fence
	mu    sync.Mutex
	woken []string
}

func newJobFixture(t *testing.T, configure ...func(*calls.Config, *gateway.Config)) *jobFixture {
	t.Helper()
	j := &jobFixture{}
	j.fixture = setup(t, append([]func(*calls.Config, *gateway.Config){func(c *calls.Config, _ *gateway.Config) {
		c.JobEnded = func(id string) { j.mu.Lock(); defer j.mu.Unlock(); j.woken = append(j.woken, id) }
	}}, configure...)...)
	f := j.fixture
	must(t, f.db.CreateIdentity(f.ctx, store.Identity{Kind: store.KindWorker, ID: "w", UserID: "u", Name: "w", MachineID: "mw", CreatedAt: 1}))
	_, err := f.db.CreateProject(f.ctx, store.Project{ID: "p", UserID: "u", Name: "p", CreatedAt: 1, UpdatedAt: 1}, "")
	must(t, err)
	must(t, f.db.CreateSession(f.ctx, store.Session{ID: "s", UserID: "u", ProjectID: "p", ClaudeSessionID: "c", DefaultDeviceID: "d", CreatedAt: 1}, nil, 10))
	now := f.clock.Now().UnixMilli()
	r, err := f.db.AcquireLease(f.ctx, store.LeaseRequest{SessionID: "s", WorkerID: "w", BootID: "b", ConnID: "conn", Now: now, ExpiresAt: now + 3_600_000})
	must(t, err)
	j.fence = store.Fence{SessionID: "s", Epoch: r.Lease.Epoch, WorkerID: "w", BootID: "b", ConnID: "conn", Now: now}
	j.link = f.connect("boot")
	return j
}

func (j *jobFixture) wakes() []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return slices.Clone(j.woken)
}

// background is a background bash call of session session.
func background(id, session string) *circusv1.DeviceCall {
	return &circusv1.DeviceCall{CallId: id, SessionId: session, Tool: &circusv1.DeviceCall_Bash{Bash: &circusv1.BashRequest{
		Command: "sleep 100", Cwd: "/tmp", RunInBackground: true, Description: "desc of " + id}}}
}

// submitAdmitted submits id under the session's lease and waits for its
// dispatch.
func (j *jobFixture) submitAdmitted(id string) *calls.Subscription {
	j.t.Helper()
	return j.submitAdmittedWithContext(j.ctx, id)
}

func (j *jobFixture) submitAdmittedWithContext(ctx context.Context, id string) *calls.Subscription {
	j.t.Helper()
	a := calls.Admission{Epoch: uint64(j.fence.Epoch), ToolUseID: "tu-" + id, WorkerID: "w", BootID: "b", ConnID: "conn"}
	s, err := j.router.SubmitSession(ctx, "u", "desk", background(id, "s"), a)
	must(j.t, err)
	j.t.Cleanup(func() { _ = s.Close() })
	j.dispatched(id)
	return s
}

func (j *jobFixture) dispatched(id string) {
	j.t.Helper()
	if got := receive(j.t, j.ctx, j.link); got.GetDispatchCall().GetCall().GetCallId() != id {
		j.t.Fatalf("dispatch %v, want %s", got, id)
	}
}

// startedJob sends id's result reporting background job job.
func (j *jobFixture) startedJob(id, job string) {
	send(j.t, j.ctx, j.link, startedJobResult(id, job))
}

func startedJobResult(id, job string) *circusv1.DaemonUplink {
	return &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_Result{Result: &circusv1.DeviceResult{CallId: id,
		Output: &circusv1.ToolOutput{Content: []*circusv1.ContentBlock{{Block: &circusv1.ContentBlock_Text{Text: "started"}}}},
		Detail: &circusv1.DeviceResult_Bash{Bash: &circusv1.BashResult{JobId: job, OutputPath: "/out/" + job}}}}}
}

// jobEvent sends a job event naming session and call.
func (j *jobFixture) jobEvent(job, session, call string, seq uint64, st circusv1.JobEvent_State) {
	before := j.hooks.events.Load()
	send(j.t, j.ctx, j.link, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_JobEvent{JobEvent: &circusv1.JobEvent{
		JobId: job, SessionId: session, CallId: call, Seq: seq, State: st, OutputPath: "/event/" + job}}})
	eventually(j.t, func() bool { return j.hooks.events.Load() > before })
}

// lose makes the daemon report id's result unavailable, so the call
// settles LOST.
func (j *jobFixture) lose(id string) {
	send(j.t, j.ctx, j.link, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_CallAck{CallAck: &circusv1.CallAck{CallId: id,
		Outcome: &circusv1.CallAck_Status{Status: &circusv1.CallStatus{CallId: id, State: circusv1.CallStatus_STATE_LOST}}}}})
	j.waitState(id, store.CallLost)
}

// sessionJobs returns the jobs registered with fence's session.
func sessionJobs(t testing.TB, ctx context.Context, db store.Store, fence store.Fence) []store.SessionJob {
	t.Helper()
	r, err := db.GetSessionRecords(ctx, fence, store.RecordsRequest{Sections: store.RecordJobs, MaxBytes: store.MaxTranscriptPageBytes})
	must(t, err)
	return r.Jobs
}

// TestSessionJobRegistered: an admitted session call's result that starts a
// background job registers the job with the session, with the call's
// tool_use_id, the request's description and the result's output path.
func TestSessionJobRegistered(t *testing.T) {
	j := newJobFixture(t)
	s := j.submitAdmitted("call")
	j.startedJob("call", "00000000000a")
	if u := nextResult(t, j.ctx, s); u.GetResult().GetBash().GetJobId() != "00000000000a" {
		t.Fatalf("result %v", u)
	}
	j.waitState("call", store.CallCompleted)
	jobs := sessionJobs(t, j.ctx, j.db, j.fence)
	if len(jobs) != 1 {
		t.Fatalf("jobs %+v", jobs)
	}
	got := jobs[0]
	if got.CallID != "call" || got.JobID != "00000000000a" || got.ToolUseID != "tu-call" || got.Description != "desc of call" ||
		got.OutputPath != "/out/00000000000a" || got.DeviceID != "d" || got.BootID != "boot" {
		t.Fatalf("job %+v", got)
	}
}

// TestSessionJobEndsBeforeResult: a job whose end is processed before its
// call's result is settled finds nothing registered when it wakes the
// session, so the registration itself wakes it again; the registered job
// carries the ended state.
func TestSessionJobEndsBeforeResult(t *testing.T) {
	j := newJobFixture(t)
	s := j.submitAdmitted("call")
	j.jobEvent("00000000000a", "s", "call", 1, circusv1.JobEvent_STATE_RUNNING)
	j.jobEvent("00000000000a", "s", "call", 2, circusv1.JobEvent_STATE_EXITED)
	if w := j.wakes(); !slices.Equal(w, []string{"s"}) {
		t.Fatalf("wakes after the end %v", w)
	}
	if jobs := sessionJobs(t, j.ctx, j.db, j.fence); len(jobs) != 0 {
		t.Fatalf("registered before the result: %+v", jobs)
	}
	j.startedJob("call", "00000000000a")
	nextResult(t, j.ctx, s)
	eventually(t, func() bool { return len(j.wakes()) == 2 })
	if w := j.wakes(); w[1] != "s" {
		t.Fatalf("wakes %v", w)
	}
	jobs := sessionJobs(t, j.ctx, j.db, j.fence)
	if len(jobs) != 1 || jobs[0].State != store.JobExited {
		t.Fatalf("jobs %+v", jobs)
	}
}

// TestSessionJobTableFull: when the session's job table is full, the call
// still settles with its result and the router stays healthy; the job is
// not registered and nothing wakes the session.
func TestSessionJobTableFull(t *testing.T) {
	j := newJobFixture(t)
	s := j.submitAdmittedWithContext(t.Context(), "call")
	// Fill the table after admission (the filler calls would otherwise
	// count against the session's call caps). The subscription must survive
	// the fill; start the result deadline only after this transaction commits.
	must(t, j.db.Tx(t.Context(), func(ctx context.Context, tx store.Ops) error {
		for i := range store.MaxSessionJobs {
			id := fmt.Sprint("fill", i)
			if err := tx.CreateCall(ctx, store.Call{CallID: id, UserID: "u", DeviceID: "d", SessionID: "s", Tool: "bash", RequestHash: make([]byte, 32),
				RequestBytes: 1, State: store.CallAccepted, SubmittedAt: 1, ToolUseID: "tu-" + id}); err != nil {
				return err
			}
			if _, err := tx.TransitionCall(ctx, id, store.CallExpect{States: []store.CallState{store.CallAccepted}}, store.CallDispatched,
				store.CallPatch{DispatchedBootID: "boot"}); err != nil {
				return err
			}
			if ok, err := tx.RegisterSessionJob(ctx, store.SessionJob{CallID: id, JobID: fmt.Sprintf("%012x", 0x100000+i)}); err != nil || !ok {
				return fmt.Errorf("fill %d: %v %w", i, ok, err)
			}
		}
		return nil
	}))
	ctx := wait.Ctx(t)
	send(t, ctx, j.link, startedJobResult("call", "00000000000a"))
	if u := nextResult(t, ctx, s); u.GetResult().GetBash().GetJobId() != "00000000000a" {
		t.Fatalf("result %v", u)
	}
	wait.For(t, func() bool {
		call, err := j.db.GetCall(ctx, "call")
		return err == nil && call.State == store.CallCompleted
	})
	if !j.router.Healthy() {
		t.Fatal("a full job table marked the store unhealthy")
	}
	for _, job := range sessionJobs(t, ctx, j.db, j.fence) {
		if job.CallID == "call" {
			t.Fatalf("registered past the cap: %+v", job)
		}
	}
	if w := j.wakes(); len(w) != 0 {
		t.Fatalf("wakes %v", w)
	}
}

// TestCrossUserJobRegistration: user u's call without session fields that
// names user u2's session is routed, but its background job is not
// registered with u2's session and wakes nothing.
func TestCrossUserJobRegistration(t *testing.T) {
	j := newJobFixture(t)
	f := j.fixture
	must(t, f.db.CreateUser(f.ctx, store.User{ID: "u2", Name: "other", CreatedAt: 1}))
	must(t, f.db.CreateIdentity(f.ctx, store.Identity{Kind: store.KindDevice, ID: "d2", UserID: "u2", Name: "desk2", MachineID: "m2", CreatedAt: 1}))
	must(t, f.db.CreateIdentity(f.ctx, store.Identity{Kind: store.KindWorker, ID: "w2", UserID: "u2", Name: "w2", MachineID: "m3", CreatedAt: 1}))
	_, err := f.db.CreateProject(f.ctx, store.Project{ID: "p2", UserID: "u2", Name: "p", CreatedAt: 1, UpdatedAt: 1}, "")
	must(t, err)
	must(t, f.db.CreateSession(f.ctx, store.Session{ID: "theirs", UserID: "u2", ProjectID: "p2", ClaudeSessionID: "c2", DefaultDeviceID: "d2", CreatedAt: 1}, nil, 10))
	now := f.clock.Now().UnixMilli()
	r, err := f.db.AcquireLease(f.ctx, store.LeaseRequest{SessionID: "theirs", WorkerID: "w2", BootID: "b2", ConnID: "conn2", Now: now, ExpiresAt: now + 3_600_000})
	must(t, err)
	theirs := store.Fence{SessionID: "theirs", Epoch: r.Lease.Epoch, WorkerID: "w2", BootID: "b2", ConnID: "conn2", Now: now}

	s, err := f.router.Submit(f.ctx, "u", "desk", background("mine", "theirs"))
	must(t, err)
	defer s.Close()
	j.dispatched("mine")
	j.startedJob("mine", "00000000000a")
	nextResult(t, f.ctx, s)
	f.waitState("mine", store.CallCompleted)
	if w := j.wakes(); len(w) != 0 {
		t.Fatalf("wakes %v", w)
	}
	// A lost call of u naming u2's session does not register from its job
	// events either.
	s2, err := f.router.Submit(f.ctx, "u", "desk", background("mine2", "theirs"))
	must(t, err)
	defer s2.Close()
	j.dispatched("mine2")
	j.lose("mine2")
	j.jobEvent("00000000000b", "theirs", "mine2", 1, circusv1.JobEvent_STATE_RUNNING)
	if jobs := sessionJobs(t, f.ctx, f.db, theirs); len(jobs) != 0 {
		t.Fatalf("u's calls registered jobs with u2's session: %+v", jobs)
	}
	if w := j.wakes(); len(w) != 0 {
		t.Fatalf("wakes %v", w)
	}
}

// TestNonAdmittedCallRegistersNoJob: a call that did not pass session
// admission registers no job, by its result or, once lost, by its job's
// events, even when its session id names a session of the same user (here
// created after the calls were routed).
func TestNonAdmittedCallRegistersNoJob(t *testing.T) {
	j := newJobFixture(t)
	f := j.fixture
	s, err := f.router.Submit(f.ctx, "u", "desk", background("loose", "later"))
	must(t, err)
	defer s.Close()
	j.dispatched("loose")
	s2, err := f.router.Submit(f.ctx, "u", "desk", background("loose2", "later"))
	must(t, err)
	defer s2.Close()
	j.dispatched("loose2")
	must(t, f.db.CreateSession(f.ctx, store.Session{ID: "later", UserID: "u", ProjectID: "p", ClaudeSessionID: "c-later", DefaultDeviceID: "d", CreatedAt: 1}, nil, 10))
	now := f.clock.Now().UnixMilli()
	r, err := f.db.AcquireLease(f.ctx, store.LeaseRequest{SessionID: "later", WorkerID: "w", BootID: "b", ConnID: "conn", Now: now, ExpiresAt: now + 3_600_000})
	must(t, err)
	later := store.Fence{SessionID: "later", Epoch: r.Lease.Epoch, WorkerID: "w", BootID: "b", ConnID: "conn", Now: now}
	j.startedJob("loose", "00000000000a")
	nextResult(t, f.ctx, s)
	f.waitState("loose", store.CallCompleted)
	// Nor from the job events of such a call whose result was lost.
	j.lose("loose2")
	j.jobEvent("00000000000b", "later", "loose2", 1, circusv1.JobEvent_STATE_RUNNING)
	if jobs := sessionJobs(t, f.ctx, f.db, later); len(jobs) != 0 {
		t.Fatalf("a non-admitted call registered %+v", jobs)
	}
	if w := j.wakes(); len(w) != 0 {
		t.Fatalf("wakes %v", w)
	}
}

// TestLostCallJobRegisteredFromEvents: the job of an admitted call whose
// result was lost is registered from the job's events (and from snapshot
// entries) that name the call, so its end still reaches the session.
func TestLostCallJobRegisteredFromEvents(t *testing.T) {
	j := newJobFixture(t)
	j.submitAdmitted("lost1")
	j.submitAdmitted("lost2")
	j.submitAdmitted("live")
	// Events of a call still waiting for its result register nothing:
	// the result registers the job.
	j.jobEvent("00000000000c", "s", "live", 1, circusv1.JobEvent_STATE_RUNNING)
	j.lose("lost1")
	j.lose("lost2")
	j.jobEvent("00000000000a", "s", "lost1", 2, circusv1.JobEvent_STATE_RUNNING)
	jobs := sessionJobs(t, j.ctx, j.db, j.fence)
	if len(jobs) != 1 || jobs[0].CallID != "lost1" || jobs[0].JobID != "00000000000a" || jobs[0].ToolUseID != "tu-lost1" ||
		jobs[0].OutputPath != "/event/00000000000a" || jobs[0].State != store.JobRunning {
		t.Fatalf("jobs after the running event %+v", jobs)
	}
	j.jobEvent("00000000000a", "s", "lost1", 3, circusv1.JobEvent_STATE_EXITED)
	if w := j.wakes(); !slices.Equal(w, []string{"s"}) {
		t.Fatalf("wakes %v", w)
	}
	// A snapshot entry registers the other lost call's job.
	before := j.hooks.snapshots.Load()
	send(t, j.ctx, j.link, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_JobSnapshot{JobSnapshot: &circusv1.JobSnapshot{Seq: 4, Jobs: []*circusv1.JobEvent{
		{JobId: "00000000000a", SessionId: "s", CallId: "lost1", Seq: 3, State: circusv1.JobEvent_STATE_EXITED},
		{JobId: "00000000000b", SessionId: "s", CallId: "lost2", Seq: 4, State: circusv1.JobEvent_STATE_KILLED},
		{JobId: "00000000000c", SessionId: "s", CallId: "live", Seq: 1, State: circusv1.JobEvent_STATE_RUNNING},
	}}}})
	eventually(t, func() bool { return j.hooks.snapshots.Load() > before })
	jobs = sessionJobs(t, j.ctx, j.db, j.fence)
	if len(jobs) != 2 || jobs[1].CallID != "lost2" || jobs[1].State != store.JobKilled {
		t.Fatalf("jobs after the snapshot %+v", jobs)
	}
	if w := j.wakes(); len(w) != 2 {
		t.Fatalf("wakes %v", w)
	}
}
