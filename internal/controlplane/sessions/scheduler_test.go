package sessions_test

import (
	"errors"
	"testing"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/controlplane/sessions"
	"github.com/EricWvi/circus/internal/controlplane/workspaces"
	"github.com/EricWvi/circus/internal/store"
	wc "github.com/EricWvi/circus/internal/testutil/workerclient"
)

// TestSchedulerChoice: the pinned worker gets its sessions; others go to
// the attached, non-full connection with the fewest leases; with none the
// session stays queued until a connection attaches or frees capacity.
func TestSchedulerChoice(t *testing.T) {
	f := newFixture(t)
	for _, id := range []string{"a", "b", "c", "d"} {
		f.session(id, -1)
	}
	f.session("pinned", 0)

	// Nobody attached: queued, no Offer anywhere.
	f.input("a", "1", store.InputNormal)
	f.settle()
	if f.get("a").State != store.SessionQueued {
		t.Fatal("not queued")
	}
	c0 := f.dial(0, "boot-0")
	ev0 := f.attach(c0, 2)
	offerTo(t, ev0, "a") // found by the scan at Attach
	acquire(t, c0, "a")

	c1 := f.dial(1, "boot-1")
	ev1 := f.attach(c1, 1)
	f.input("b", "1", store.InputNormal)
	offerTo(t, ev1, "b") // c1 has no lease, c0 has one
	acquire(t, c1, "b")

	f.input("pinned", "1", store.InputNormal)
	offerTo(t, ev0, "pinned")
	acquire(t, c0, "pinned")

	// Both full now: c stays queued.
	f.input("c", "1", store.InputNormal)
	f.settle()
	noOffer(t, ev0)
	noOffer(t, ev1)
	if f.get("c").State != store.SessionQueued {
		t.Fatal("c not queued")
	}
	// Releasing frees capacity: c is offered there.
	turn(t, c1, "b", 1, 1)
	r := call(t, c1, wc.Release("b", 1, circusv1.ReleaseLease_REASON_IDLE))
	wantOk(t, r, false)
	offerTo(t, ev1, "c")

	// A pinned session is never offered to another worker.
	turn(t, c0, "pinned", 1, 1)
	call(t, c0, wc.Release("pinned", 1, circusv1.ReleaseLease_REASON_IDLE))
	f.input("pinned", "2", store.InputNormal)
	offerTo(t, ev0, "pinned")
	wantRejected(t, call(t, c1, wc.Acquire("pinned", true)), circusv1.SessionReply_Rejected_CODE_NOT_OFFERED)
}

// TestScheduleFallback: an Offer that is not taken is made again by the
// session_schedule fallback, and a lost Wake is made up for by it.
func TestScheduleFallback(t *testing.T) {
	f := newFixture(t)
	f.session("s1", -1)
	c := f.dial(0, "boot-a")
	ev := f.attach(c, 1)
	f.input("s1", "go", store.InputNormal)
	offerTo(t, ev, "s1")
	// Within ScheduleInterval the Offer stays reserved.
	sessions.Fallback(f.svc, t.Context())
	noOffer(t, ev)
	// The upkeep loop's own timer makes it again.
	f.clk.Advance(sessions.ScheduleInterval)
	offerTo(t, ev, "s1")

	// A session that got input without a Wake is found by the fallback.
	f.session("s2", -1)
	_, err := f.st.SubmitInput(t.Context(), store.Input{InputID: "i2", SessionID: "s2", ClientMsgID: "m", Kind: store.InputUser, Text: "x", Priority: store.InputNormal, CreatedAt: ms(f)})
	must(t, err)
	acquire(t, c, "s1")
	turn(t, c, "s1", 1, 1)
	call(t, c, wc.Release("s1", 1, circusv1.ReleaseLease_REASON_IDLE))
	f.settle()
	f.clk.Advance(sessions.ScheduleInterval)
	sessions.Fallback(f.svc, t.Context())
	offerTo(t, ev, "s2")
}

// TestWakeHeldNudges: Wake of a held session nudges its holder with the
// reasons; a reason the holder acted on is not sent again after a resume.
func TestWakeHeldNudges(t *testing.T) {
	f := newFixture(t)
	f.session("s1", -1)
	c := f.dial(0, "boot-a")
	ev := f.attach(c, 1)
	f.input("s1", "go", store.InputNormal)
	offerTo(t, ev, "s1")
	epoch := acquire(t, c, "s1")
	f.svc.Wake("s1", circusv1.WorkerEvent_Nudge_REASON_JOB, circusv1.WorkerEvent_Nudge_REASON_APPROVAL)
	n := nudge(t, ev, "s1", circusv1.WorkerEvent_Nudge_REASON_JOB, circusv1.WorkerEvent_Nudge_REASON_APPROVAL)
	if !hasReason(n, circusv1.WorkerEvent_Nudge_REASON_JOB) || !hasReason(n, circusv1.WorkerEvent_Nudge_REASON_APPROVAL) {
		t.Fatalf("nudge %v", n)
	}
	// The Nudge may come from the Lease reply's delivery before the
	// scheduler evaluates the Wake; that evaluation raises the reasons
	// again at a newer generation (a spurious Nudge by design), so the
	// holder acts only after it.
	f.settle()
	call(t, c, wc.State("s1", epoch, circusv1.GetSessionState_SECTION_JOBS))
	call(t, c, wc.TakeInputs("s1", epoch))
	_ = c.Close()
	c2 := f.dial(0, "boot-a")
	ev2 := f.attach(c2, 1)
	r := call2resume(t, c2, "s1", epoch)
	if !r {
		t.Fatal("resume failed")
	}
	f.settle()
	noEvent(t, ev2)
}

func call2resume(t *testing.T, c *wc.Client, id string, epoch uint64) bool {
	t.Helper()
	r, err := c.Resume(t.Context(), &circusv1.ResumeLeases_Held{SessionId: id, Epoch: epoch})
	must(t, err)
	return r.GetResumed().GetLeases()[0].GetOk()
}

// TestTableBound: the in-memory table forgets a session that has neither a
// known lease nor an Offer.
func TestTableBound(t *testing.T) {
	f := newFixture(t)
	f.session("s1", -1)
	c := f.dial(0, "boot-a")
	ev := f.attach(c, 1)
	f.input("s1", "go", store.InputNormal)
	offerTo(t, ev, "s1")
	epoch := acquire(t, c, "s1")
	if sessions.Tracked(f.svc) != 1 {
		t.Fatal("not tracked")
	}
	turn(t, c, "s1", epoch, 1)
	call(t, c, wc.Release("s1", epoch, circusv1.ReleaseLease_REASON_IDLE))
	f.settle()
	if n := sessions.Tracked(f.svc); n != 0 {
		t.Fatalf("%d tracked after release", n)
	}
}

func TestWorkspaceSchedulingGate(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	w, _, err := f.st.EnsureWorkspace(ctx, store.Workspace{
		ID: "workspace", UserID: f.user.ID, ProjectID: "p1", DeviceID: f.device.ID, CreatedAt: ms(f),
	})
	must(t, err)
	_, err = f.st.CreateSession(ctx, store.Session{
		ID: "blocked", ClientRequestID: "blocked", ClaudeSessionID: "workspace-claude",
		UserID: f.user.ID, ProjectID: w.ProjectID, DefaultDeviceID: f.device.ID, CreatedAt: ms(f), Workspace: true,
	}, []store.SessionCwd{{DeviceID: f.device.ID, Cwd: store.DefaultWorkspacePath(w.ProjectID)}}, 10)
	must(t, err)
	f.input("blocked", "message", store.InputNormal)
	c := f.dial(0, "boot")
	events := f.attach(c, 1)
	f.settle()
	noOffer(t, events)
	// A wake after attachment exercises evaluate directly, without the
	// Runnable filter on the attachment scan.
	f.input("blocked", "after-attach", store.InputNow)
	f.settle()
	noOffer(t, events)
	wantRejected(t, call(t, c, wc.Acquire("blocked", true)), circusv1.SessionReply_Rejected_CODE_NOT_OFFERED)
	_, err = f.st.AcquireLease(ctx, store.LeaseRequest{SessionID: "blocked", WorkerID: f.workers[0].Identity.ID,
		BootID: "boot", ConnID: "conn", Now: ms(f), ExpiresAt: ms(f) + 1000})
	if !errors.Is(err, store.ErrLeaseHeld) {
		t.Fatalf("blocked direct acquire = %v", err)
	}

	change, err := f.st.TransitionWorkspace(ctx, w.ID, w.Expect(), store.WorkspacePatch{
		State: store.WorkspacePreparing, Attempt: 1, CallID: "ws-call", TimeoutMs: 120000, UpdatedAt: ms(f),
	})
	must(t, err)
	change, err = f.st.MarkWorkspaceReady(ctx, w.ID, change.Workspace.Expect(), "/home/device/project", "created", ms(f))
	must(t, err)
	workspaces.Publish(nil, f.svc, change)
	offerTo(t, events, "blocked")
	reply := call(t, c, wc.Acquire("blocked", true))
	if cwd := reply.GetLease().GetConfig().GetCwds()[0].GetCwd(); cwd != "/home/device/project" {
		t.Fatalf("lease cwd = %q", cwd)
	}
}
