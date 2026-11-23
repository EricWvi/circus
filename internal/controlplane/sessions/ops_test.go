package sessions_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/controlplane/sessions"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/testutil/wait"
	wc "github.com/EricWvi/circus/internal/testutil/workerclient"
)

// held returns a connection holding session s1 at the returned epoch,
// once the scheduler is done with what led there: the input's Wake may be
// evaluated after the acquisition (the Offer came from a rescan, or the
// Wake was deferred while the acquisition held the entry), and the Lease
// reply's delivery may run after the reply reached c; both can nudge INPUT.
// held returns after both, so a later TakeInputs clears INPUT for good and
// nothing of the acquisition acts later (such Nudges may still be unread
// on the returned events).
func held(t *testing.T, f *fixture) (*wc.Client, wc.Events, uint64) {
	t.Helper()
	f.session("s1", -1)
	c := f.dial(0, "boot-a")
	ev := f.attach(c, 1)
	f.input("s1", "go", store.InputNormal)
	offerTo(t, ev, "s1")
	epoch := acquire(t, c, "s1")
	wait.For(t, func() bool { return !sessions.Pending(f.svc, "s1") })
	f.settle()
	return c, ev, epoch
}

// TestApprovalsAndGrants: a pending approval is merged by key; resolving it
// nudges APPROVAL and the holder takes the approval input and consumes the
// grant once.
func TestApprovalsAndGrants(t *testing.T) {
	f := newFixture(t)
	c, ev, epoch := held(t, f)
	call(t, c, wc.TakeInputs("s1", epoch))
	req := &circusv1.RequestApproval{ToolUseId: "t1", Tool: "bash", Device: "desk", MatchKey: matchKey, Display: "ls", Reason: "denied"}
	r := call(t, c, wc.Op("s1", epoch, req))
	id := r.GetApproval().GetApprovalId()
	if id == "" || r.GetApproval().GetDeduplicated() {
		t.Fatalf("approval %v", r)
	}
	if r := call(t, c, wc.Op("s1", epoch, req)); r.GetApproval().GetApprovalId() != id || !r.GetApproval().GetDeduplicated() {
		t.Fatalf("merged approval %v", r)
	}
	wantRejected(t, call(t, c, wc.Op("s1", epoch, &circusv1.RequestApproval{ToolUseId: "t1", Tool: "bash", Device: "nope", MatchKey: matchKey})), circusv1.SessionReply_Rejected_CODE_NOT_FOUND)
	wantRejected(t, call(t, c, wc.Op("s1", epoch, &circusv1.RequestApproval{Tool: "bash", Device: "desk", MatchKey: matchKey})), circusv1.SessionReply_Rejected_CODE_INVALID)
	wantRejected(t, call(t, c, wc.Op("s1", epoch, &circusv1.RequestApproval{ToolUseId: "t1", Tool: "bash", Device: "desk", MatchKey: "ABC"})), circusv1.SessionReply_Rejected_CODE_INVALID)
	grant := &circusv1.ConsumeGrant{ToolUseId: "t2", Tool: "bash", Device: "desk", MatchKey: matchKey}
	if call(t, c, wc.Op("s1", epoch, grant)).GetGrant().GetAllowed() {
		t.Fatal("grant before approval")
	}
	res, err := f.st.ResolveApproval(t.Context(), store.ApprovalResolution{ApprovalID: id, Approve: true, At: ms(f), GrantExpiresAt: ms(f) + 600_000,
		Input: store.Input{InputID: "approved", Text: "approved, retry"}})
	must(t, err)
	if !res.Changed {
		t.Fatal("not resolved")
	}
	f.svc.Wake("s1", circusv1.WorkerEvent_Nudge_REASON_APPROVAL)
	if n := nudge(t, ev, "s1", circusv1.WorkerEvent_Nudge_REASON_APPROVAL); !hasReason(n, circusv1.WorkerEvent_Nudge_REASON_APPROVAL) {
		t.Fatalf("nudge %v", n)
	}
	in := call(t, c, wc.TakeInputs("s1", epoch)).GetInputs().GetInputs()
	kinds := map[circusv1.SessionReply_Inputs_Input_Kind]int{}
	for _, i := range in {
		kinds[i.GetKind()]++
	}
	if len(in) != 2 || kinds[circusv1.SessionReply_Inputs_Input_KIND_APPROVAL] != 1 {
		t.Fatalf("inputs %v", in)
	}
	st := call(t, c, wc.State("s1", epoch, circusv1.GetSessionState_SECTION_GRANTS)).GetState().GetState()
	if len(st.GetGrants()) != 1 || st.GetGrants()[0].GetDevice() != "desk" || st.GetGrants()[0].GetMatchKey() != matchKey {
		t.Fatalf("grants %v", st)
	}
	grant.ToolUseId = "t3"
	if !call(t, c, wc.Op("s1", epoch, grant)).GetGrant().GetAllowed() {
		t.Fatal("grant not consumed")
	}
	if !call(t, c, wc.Op("s1", epoch, grant)).GetGrant().GetAllowed() {
		t.Fatal("grant replay")
	}
	grant.ToolUseId = "t4"
	if call(t, c, wc.Op("s1", epoch, grant)).GetGrant().GetAllowed() {
		t.Fatal("grant used twice")
	}
	unknown := &circusv1.ConsumeGrant{ToolUseId: "t5", Tool: "bash", Device: "nope", MatchKey: matchKey}
	if call(t, c, wc.Op("s1", epoch, unknown)).GetGrant().GetAllowed() {
		t.Fatal("grant for an unknown device")
	}
}

// TestPatchAndState: patches round-trip through GetSessionState with
// device names, and their refusals write nothing.
func TestPatchAndState(t *testing.T) {
	f := newFixture(t)
	c, _, epoch := held(t, f)
	sha := strings.Repeat("ab", 32)
	p := &circusv1.PatchSessionState{
		Cwds:           []*circusv1.PatchSessionState_Cwd{{AgentId: "agent", Device: "desk", Cwd: "/sub"}},
		Files:          []*circusv1.PatchSessionState_FileRecord{{Device: "desk", Path: "/a", Sha256: sha, MtimeUnixMs: 7}},
		BgtasksStarted: []*circusv1.PatchSessionState_BackgroundTask{{TaskId: "task", ToolUseId: "tu", Kind: "agent", Description: "d"}},
	}
	wantOk(t, call(t, c, wc.Patch("s1", epoch, p)), false)
	st := call(t, c, wc.State("s1", epoch)).GetState().GetState()
	if len(st.GetCwds()) != 2 || len(st.GetFiles()) != 1 || st.GetFiles()[0].GetSha256() != sha || st.GetFiles()[0].GetDevice() != "desk" || st.GetStopped() {
		t.Fatalf("state %v", st)
	}
	wantRejected(t, call(t, c, wc.Patch("s1", epoch, &circusv1.PatchSessionState{Files: []*circusv1.PatchSessionState_FileRecord{{Device: "desk", Path: "/b", Sha256: "xyz"}}})), circusv1.SessionReply_Rejected_CODE_INVALID)
	wantRejected(t, call(t, c, wc.Patch("s1", epoch, &circusv1.PatchSessionState{Cwds: []*circusv1.PatchSessionState_Cwd{{Device: "nope", Cwd: "/x"}}})), circusv1.SessionReply_Rejected_CODE_NOT_FOUND)
	wantRejected(t, call(t, c, wc.Patch("s1", epoch, &circusv1.PatchSessionState{Cwds: []*circusv1.PatchSessionState_Cwd{{Device: "desk", Cwd: strings.Repeat("x", 4097)}}})), circusv1.SessionReply_Rejected_CODE_INVALID)
	tasks := func(from, n int) []*circusv1.PatchSessionState_BackgroundTask {
		var out []*circusv1.PatchSessionState_BackgroundTask
		for i := range n {
			out = append(out, &circusv1.PatchSessionState_BackgroundTask{TaskId: fmt.Sprintf("t%d", from+i), Kind: "agent"})
		}
		return out
	}
	wantOk(t, call(t, c, wc.Patch("s1", epoch, &circusv1.PatchSessionState{BgtasksStarted: tasks(0, 63)})), false)
	wantRejected(t, call(t, c, wc.Patch("s1", epoch, &circusv1.PatchSessionState{BgtasksStarted: tasks(63, 2)})), circusv1.SessionReply_Rejected_CODE_LIMIT)
	for _, bad := range [][]circusv1.GetSessionState_Section{
		{circusv1.GetSessionState_SECTION_UNSPECIFIED},
		{circusv1.GetSessionState_SECTION_NONE, circusv1.GetSessionState_SECTION_CWDS},
		{circusv1.GetSessionState_SECTION_CWDS, circusv1.GetSessionState_SECTION_CWDS},
	} {
		wantRejected(t, call(t, c, wc.State("s1", epoch, bad...)), circusv1.SessionReply_Rejected_CODE_INVALID)
	}
	st = call(t, c, wc.State("s1", epoch, circusv1.GetSessionState_SECTION_CALLS)).GetState().GetState()
	if len(st.GetCwds()) != 0 || len(st.GetCalls()) != 0 {
		t.Fatalf("calls only %v", st)
	}
}

// TestTurnRules: ReportTurn validation and usage.
func TestTurnRules(t *testing.T) {
	f := newFixture(t)
	c, _, epoch := held(t, f)
	wantRejected(t, call(t, c, wc.Ended("s1", epoch, 1, circusv1.TurnOutcome_TURN_OUTCOME_COMPLETED, nil)), circusv1.SessionReply_Rejected_CODE_INVALID)
	wantRejected(t, call(t, c, wc.Started("s1", epoch, 2, circusv1.TurnOrigin_TURN_ORIGIN_INPUT)), circusv1.SessionReply_Rejected_CODE_INVALID)
	wantRejected(t, call(t, c, wc.Started("s1", epoch, 1, circusv1.TurnOrigin_TURN_ORIGIN_UNSPECIFIED)), circusv1.SessionReply_Rejected_CODE_INVALID)
	wantRejected(t, call(t, c, wc.Op("s1", epoch, &circusv1.ReportTurn{LocalSeq: 1})), circusv1.SessionReply_Rejected_CODE_INVALID)
	call(t, c, wc.Started("s1", epoch, 1, circusv1.TurnOrigin_TURN_ORIGIN_INPUT))
	wantRejected(t, call(t, c, wc.Op("s1", epoch, &circusv1.ReportTurn{LocalSeq: 1, Phase: circusv1.ReportTurn_PHASE_ENDED,
		Outcome: circusv1.TurnOutcome_TURN_OUTCOME_COMPLETED, UsageJson: strings.Repeat(" ", 4097)})), circusv1.SessionReply_Rejected_CODE_INVALID)
	wantRejected(t, call(t, c, wc.Ended("s1", epoch, 1, circusv1.TurnOutcome_TURN_OUTCOME_UNSPECIFIED, nil)), circusv1.SessionReply_Rejected_CODE_INVALID)
	r := call(t, c, wc.Op("s1", epoch, &circusv1.ReportTurn{LocalSeq: 1, Phase: circusv1.ReportTurn_PHASE_ENDED, Outcome: circusv1.TurnOutcome_TURN_OUTCOME_COMPLETED,
		UsageJson: `{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":3,"cache_creation_input_tokens":2,"total_cost_usd":0.0125,"duration_ms":900}`}))
	if r.GetTurn().GetTurnNo() != 1 {
		t.Fatalf("ended %v", r)
	}
	u := f.get("s1").Usage
	if u != (store.TurnUsage{InputTokens: 10, OutputTokens: 5, CacheReadInputTokens: 3, CacheCreationInputTokens: 2, CostMicroUSD: 12500, DurationMs: 900}) {
		t.Fatalf("usage %+v", u)
	}
	// A repeat keeps the turn number and counts nothing twice.
	if r := call(t, c, wc.Ended("s1", epoch, 1, circusv1.TurnOutcome_TURN_OUTCOME_COMPLETED, nil)); r.GetTurn().GetTurnNo() != 1 || f.get("s1").TurnCount != 1 {
		t.Fatalf("repeat %v", r)
	}
	// A successor without ENDED interrupts its predecessor.
	call(t, c, wc.Started("s1", epoch, 2, circusv1.TurnOrigin_TURN_ORIGIN_CLI))
	before := f.bus.seq("s1")
	call(t, c, wc.Started("s1", epoch, 3, circusv1.TurnOrigin_TURN_ORIGIN_NOTIFICATION))
	if f.bus.seq("s1") <= before {
		t.Fatal("TURN_INTERRUPTED not announced")
	}
}

// TestEventAndTranscriptRules: batch sequence rules, control-plane-only
// kinds, and the transcript quota stop.
func TestEventAndTranscriptRules(t *testing.T) {
	f := newFixture(t, func(c *sessions.Config) { c.MaxSessionTranscriptBytes = 1000 })
	c, ev, epoch := held(t, f)
	assistant := &circusv1.PublishEvents_Event{Kind: circusv1.SessionEvent_KIND_ASSISTANT, Json: `{}`}
	wantRejected(t, call(t, c, wc.Publish("s1", epoch, 2, assistant)), circusv1.SessionReply_Rejected_CODE_INVALID)
	wantOk(t, call(t, c, wc.Publish("s1", epoch, 1, assistant)), false)
	// A control-plane kind, durable or live-only, drops the batch and
	// consumes its sequence; the next one continues.
	for i, bad := range []*circusv1.PublishEvents_Event{
		{Kind: circusv1.SessionEvent_KIND_TURN_STOPPED, Json: `{}`},
		{Kind: circusv1.SessionEvent_KIND_TURN_INTERRUPTED, Json: `{}`, Ephemeral: true},
		{Kind: circusv1.SessionEvent_KIND_UNSPECIFIED, Json: `{}`},
		{Kind: circusv1.SessionEvent_Kind(99), Json: `{}`, Ephemeral: true},
		{Kind: circusv1.SessionEvent_KIND_QUEUE, Json: `{"queue_reason":"unspecified"}`, Ephemeral: true},
		{Kind: circusv1.SessionEvent_KIND_QUEUE, Json: `{"queue_reason":"unspecified"}`},
		{Kind: circusv1.SessionEvent_KIND_WORKER, Json: `{"disconnected_at_unix_ms":"0"}`, Ephemeral: true},
		{Kind: circusv1.SessionEvent_KIND_WORKER, Json: `{"disconnected_at_unix_ms":"0"}`},
	} {
		seq := uint64(i + 2)
		wantRejected(t, call(t, c, wc.Publish("s1", epoch, seq, assistant, bad)), circusv1.SessionReply_Rejected_CODE_INVALID)
		wantOk(t, call(t, c, wc.Publish("s1", epoch, seq, assistant)), true)
	}
	if len(f.bus.live()) != 0 || f.get("s1").DroppedEventBatches != 8 {
		t.Fatal("a dropped batch published")
	}

	wantRejected(t, call(t, c, wc.Append("s1", epoch, "", 2, &circusv1.TranscriptEntry{Json: `{}`})), circusv1.SessionReply_Rejected_CODE_INVALID)
	wantRejected(t, call(t, c, wc.Append("s1", epoch, "", 1, &circusv1.TranscriptEntry{Uuid: strings.Repeat("u", 129), Json: `{}`})), circusv1.SessionReply_Rejected_CODE_INVALID)
	wantRejected(t, call(t, c, wc.Append("s1", epoch, strings.Repeat("p", 1025), 1, &circusv1.TranscriptEntry{Json: `{}`})), circusv1.SessionReply_Rejected_CODE_INVALID)
	wantRejected(t, call(t, c, wc.Load("s1", epoch, "", 0, 0)), circusv1.SessionReply_Rejected_CODE_INVALID)
	wantRejected(t, call(t, c, wc.Load("s1", epoch, "", 0, 8<<20+1)), circusv1.SessionReply_Rejected_CODE_INVALID)
	wantOk(t, call(t, c, wc.Append("s1", epoch, "agent-1", 1, &circusv1.TranscriptEntry{Json: `{"sub":1}`})), false)
	if k := call(t, c, wc.Op("s1", epoch, &circusv1.ListTranscriptKeys{})).GetTranscriptKeys().GetSubpaths(); len(k) != 1 || k[0] != "agent-1" {
		t.Fatalf("keys %v", k)
	}
	// Past the session quota: TRANSCRIPT_FULL, the session stops, its
	// holder is nudged and its jobs are stopped.
	wantRejected(t, call(t, c, wc.Append("s1", epoch, "", 1, &circusv1.TranscriptEntry{Json: strings.Repeat("x", 2000)})), circusv1.SessionReply_Rejected_CODE_TRANSCRIPT_FULL)
	sess := f.get("s1")
	if sess.State != store.SessionStopped {
		t.Fatal("not stopped")
	}
	// The stop's view event, recorded with the stop and announced.
	page, err := f.st.ReadEvents(t.Context(), store.EventFilter{SessionID: "s1", AfterSeq: sess.LastEventSeq - 1, Limit: 1, MaxBytes: 1 << 20})
	must(t, err)
	if len(page.Events) != 1 || page.Events[0].Kind != store.EventKindSessionState || string(page.Events[0].Payload) != `{"state":"stopped","reason":"transcript_full"}` ||
		f.bus.seq("s1") != uint64(sess.LastEventSeq) {
		t.Fatalf("stop event %+v, announced %d", page.Events, f.bus.seq("s1"))
	}
	// A second refusal stops nothing more.
	wantRejected(t, call(t, c, wc.Append("s1", epoch, "", 1, &circusv1.TranscriptEntry{Json: strings.Repeat("y", 2000)})), circusv1.SessionReply_Rejected_CODE_TRANSCRIPT_FULL)
	if f.get("s1").LastEventSeq != sess.LastEventSeq {
		t.Fatal("a second stop event")
	}
	if n := nudge(t, ev, "s1", circusv1.WorkerEvent_Nudge_REASON_STOP); !hasReason(n, circusv1.WorkerEvent_Nudge_REASON_STOP) {
		t.Fatalf("nudge %v", n)
	}
	wait.For(t, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return len(f.stopped) == 1 && f.stopped[0] == "s1"
	})
	wantOk(t, call(t, c, wc.Release("s1", epoch, circusv1.ReleaseLease_REASON_STOPPED)), false)
}

// TestListDevices lists the user's devices.
func TestListDevices(t *testing.T) {
	f := newFixture(t)
	c := f.dial(0, "boot-a")
	r, err := c.ListDevices(wait.Ctx(t))
	must(t, err)
	d := r.GetDevices().GetDevices()
	if len(d) != 1 || d[0].GetName() != "desk" || !d[0].GetOnline() || d[0].GetOs() != "linux" || d[0].GetHostname() != "host-desk" {
		t.Fatalf("devices %v", r)
	}
}

// closed waits for the server to close c.
func closed(t *testing.T, c *wc.Client) {
	t.Helper()
	select {
	case <-c.Done():
	case <-wait.Ctx(t).Done():
		t.Fatal("link stayed open")
	}
}

// TestAttachRules: an Attach that breaks a rule closes the link. Each case
// waits until the server has released the closed link, so the cases never
// add up to the per-credential connection cap.
func TestAttachRules(t *testing.T) {
	f := newFixture(t)
	for name, a := range map[string]*circusv1.Attach{
		"zero":   {MaxSessions: 0},
		"65":     {MaxSessions: 65},
		"string": {MaxSessions: 1, SdkVersion: strings.Repeat("v", 257)},
		"email":  {MaxSessions: 1, Account: &circusv1.Attach_Account{Email: strings.Repeat("e", 257)}},
	} {
		t.Run(name, func(t *testing.T) {
			c := f.dial(0, "boot-a")
			_, err := c.Attach(wait.Ctx(t), a)
			must(t, err)
			closed(t, c)
			f.released()
		})
	}
	t.Run("twice", func(t *testing.T) {
		c := f.dial(0, "boot-a")
		ev := f.attach(c, 1)
		must(t, ev.Unwatch(wait.Ctx(t)))
		f.attach(c, 1)
		closed(t, c)
		f.released()
	})
	t.Run("no boot", func(t *testing.T) {
		c := f.dial(0, "")
		f.attach(c, 1)
		closed(t, c)
		f.released()
	})
	t.Run("long boot", func(t *testing.T) {
		_, err := wc.Dial(wait.Ctx(t), wc.Options{URL: f.srv.URL, Token: f.workers[0].Token, PrincipalID: f.workers[0].Identity.ID, BootID: strings.Repeat("b", 129)})
		if err == nil || !strings.Contains(err.Error(), "BAD_HELLO") {
			t.Fatalf("dial with a long boot id: %v", err)
		}
	})
}

// TestStopEventConfiguredViewCap: the stop event a TRANSCRIPT_FULL refusal
// records is held to the service's configured view cap, like the worker's
// events: past it, only the boundary is stored, without its payload.
func TestStopEventConfiguredViewCap(t *testing.T) {
	f := newFixture(t, func(c *sessions.Config) { c.MaxSessionTranscriptBytes = 1000; c.MaxViewBytes = 64 })
	c, _, epoch := held(t, f)
	wantOk(t, call(t, c, wc.Publish("s1", epoch, 1, &circusv1.PublishEvents_Event{Kind: circusv1.SessionEvent_KIND_ASSISTANT, Json: `"` + strings.Repeat("a", 40) + `"`})), false)
	wantRejected(t, call(t, c, wc.Append("s1", epoch, "", 1, &circusv1.TranscriptEntry{Json: strings.Repeat("x", 2000)})), circusv1.SessionReply_Rejected_CODE_TRANSCRIPT_FULL)
	sess := f.get("s1")
	if sess.State != store.SessionStopped {
		t.Fatal("not stopped")
	}
	page, err := f.st.ReadEvents(t.Context(), store.EventFilter{SessionID: "s1", AfterSeq: sess.LastEventSeq - 1, Limit: 1, MaxBytes: 1 << 20})
	must(t, err)
	if len(page.Events) != 1 || page.Events[0].Kind != store.EventKindSessionState || !page.Events[0].Truncated || page.Events[0].Payload != nil {
		t.Fatalf("stop event past the configured cap %+v", page.Events)
	}
}

// pagedRecoveryStore returns a short first page so the worker API follows
// the continuation path. All later pages come directly from SQLite.
type pagedRecoveryStore struct{ store.Store }

func (s pagedRecoveryStore) Tx(ctx context.Context, fn func(context.Context, store.Ops) error) error {
	return s.Store.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		return fn(ctx, pagedRecoveryOps{tx})
	})
}

type pagedRecoveryOps struct{ store.Ops }

func (s pagedRecoveryOps) GetRecovery(ctx context.Context, f store.Fence) (store.Recovery, error) {
	rec, err := s.Ops.GetRecovery(ctx, f)
	if err == nil && len(rec.Calls) > store.MaxPageSize {
		rec.Calls, rec.CallsTruncated = rec.Calls[:store.MaxPageSize], true
	}
	return rec, err
}

func TestRecoveryLaterPagesFilterByUser(t *testing.T) {
	f := newFixture(t, func(cfg *sessions.Config) { cfg.Store = pagedRecoveryStore{cfg.Store} })
	c, _, epoch := held(t, f)
	ctx, now := t.Context(), ms(f)
	foreign := store.User{ID: "other", Email: "other@example.com", Role: store.RoleUser, CreatedAt: now}
	device := store.Identity{Kind: store.KindDevice, ID: "other-device", Name: "other-device",
		UserID: foreign.ID, MachineID: "other-machine", CreatedAt: now}
	const count = 2*store.MaxPageSize + 1
	must(t, f.st.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		if err := tx.CreateUser(ctx, foreign); err != nil {
			return err
		}
		if err := tx.CreateIdentity(ctx, device); err != nil {
			return err
		}
		for i := range count {
			// Place foreign rows between the first page and subsequent
			// rows of the session's owner.
			at := now
			if i >= store.MaxPageSize {
				at += 2
			}
			for _, row := range []store.Call{
				{CallID: fmt.Sprintf("own-%04d", i), UserID: f.user.ID, DeviceID: f.device.ID, SubmittedAt: at},
				{CallID: fmt.Sprintf("foreign-%04d", i), UserID: foreign.ID, DeviceID: device.ID, SubmittedAt: now + 1},
			} {
				row.SessionID, row.Tool, row.State = "s1", "bash", store.CallAccepted
				row.RequestHash = []byte("hash")
				if err := tx.CreateCall(ctx, row); err != nil {
					return err
				}
			}
		}
		return nil
	}))

	reply := call(t, c, wc.State("s1", epoch, circusv1.GetSessionState_SECTION_CALLS))
	rows := reply.GetState().GetState().GetCalls()
	if len(rows) != count {
		t.Fatalf("recovered %d calls, want %d", len(rows), count)
	}
	for i, row := range rows {
		if want := fmt.Sprintf("own-%04d", i); row.GetCallId() != want {
			t.Fatalf("recovered call %d = %q, want %q", i, row.GetCallId(), want)
		}
	}
}
