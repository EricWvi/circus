package sessions_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/controlplane/providers"
	"github.com/EricWvi/circus/internal/controlplane/sessions"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/testutil/wait"
	wc "github.com/EricWvi/circus/internal/testutil/workerclient"
)

// Records are checked once the step that writes them is done: the link
// writes a request's records before its reply, the scheduler's before it
// goes idle (settle), and a connection's close (and its sender's) before
// the link's server side returns (released).

// wantEnds checks that exactly one "lease ended" was written for each of
// session id's epochs.
func wantEnds(t *testing.T, logs *capture, id string, epochs ...uint64) {
	t.Helper()
	for _, e := range epochs {
		if n := len(logs.matching("lease ended", "session_id", id, "epoch", kv(e))); n != 1 {
			t.Fatalf("%d lease ended records for %s epoch %d, want 1; captured:\n%s", n, id, e, logs.raw())
		}
	}
}

// TestSessionLogs follows one session through offer, acquisition, a turn,
// a disconnect and resumption, a takeover, a release and an expiry, and
// checks the info records an operator follows it by.
func TestSessionLogs(t *testing.T) {
	logs := newCapture(slog.LevelInfo)
	f := newFixture(t, withLogger(logs))
	sessions.DisableFallback(f.svc)
	// The expiry below is swept by hand: the background sweep must not
	// release the lease first, or the record is written by another writer.
	sessions.DisableUpkeep(f.svc)
	w, boot := f.workers[0].Identity.ID, f.boot("boot-a")
	const email = "worker-account@example.com"
	f.session("s1", -1)
	c := f.dial(0, "boot-a")
	ev := f.attachWith(c, &circusv1.Attach{MaxSessions: 2, SdkVersion: "sdk", ClaudeVersion: "cli",
		Account: &circusv1.Attach_Account{Email: email}})
	f.input("s1", "go", store.InputNormal)
	offerTo(t, ev, "s1")

	epoch := acquire(t, c, "s1")
	// The link handled the Attach before this acquisition.
	if r := logs.want(t, "worker attached", "worker_id", w, "boot_id", boot); len(r.attrs) == 0 || hasKey(r, "account_email") {
		t.Fatalf("worker attached carries the account email: %v", r.attrs)
	}
	if strings.Contains(logs.raw(), email) {
		t.Fatalf("account email logged at info:\n%s", logs.raw())
	}
	logs.want(t, "lease acquired", "session_id", "s1", "worker_id", w, "boot_id", boot, "epoch", kv(epoch),
		"fresh", "true", "reattached", "false", "provider_id", "")
	call(t, c, wc.TakeInputs("s1", epoch))
	call(t, c, wc.Started("s1", epoch, 1, circusv1.TurnOrigin_TURN_ORIGIN_INPUT))
	logs.want(t, "turn started", "session_id", "s1", "epoch", kv(epoch), "turn_no", "1", "origin", "input")
	call(t, c, wc.Ended("s1", epoch, 1, circusv1.TurnOutcome_TURN_OUTCOME_COMPLETED, nil))
	logs.want(t, "turn ended", "session_id", "s1", "epoch", kv(epoch), "turn_no", "1", "origin", "input",
		"outcome", "completed", "transcript_incomplete", "false")

	// The holder's only connection closes: the lease is marked; resuming
	// it on a new connection clears the mark.
	f.clk.Advance(time.Second)
	at := f.drop(c, 0)
	f.released()
	logs.want(t, "worker connection closed", "worker_id", w, "boot_id", boot, "last_of_boot", "true", "marked", "1")
	logs.want(t, "worker disconnect marked", "session_id", "s1", "worker_id", w, "boot_id", boot, "disconnected_at_unix_ms", kv(at))
	c2 := f.redial("boot-a", 1)
	if !call2resume(t, c2, "s1", epoch) {
		t.Fatal("resume")
	}
	logs.want(t, "leases resumed", "worker_id", w, "boot_id", boot, "result", "ok", "resumed", "1", "fenced", "0", "not_found", "0")
	logs.want(t, "worker disconnect cleared", "session_id", "s1", "worker_id", w, "boot_id", boot)

	// A fresh acquisition by the live holder replaces its own lease.
	epoch2 := acquire(t, c2, "s1")
	logs.want(t, "lease ended", "session_id", "s1", "worker_id", w, "boot_id", boot, "epoch", kv(epoch), "reason", "taken_over",
		"interrupted", "false")
	logs.want(t, "lease acquired", "session_id", "s1", "epoch", kv(epoch2))

	// A release ends the lease once; the resent release logs nothing.
	wantOk(t, call(t, c2, wc.Release("s1", epoch2, circusv1.ReleaseLease_REASON_IDLE)), false)
	wantOk(t, call(t, c2, wc.Release("s1", epoch2, circusv1.ReleaseLease_REASON_IDLE)), true)
	logs.want(t, "lease ended", "session_id", "s1", "epoch", kv(epoch2), "reason", "released")

	// A lease whose holder is gone expires, with its open turn.
	ev2 := f.attach(c2, 2)
	f.input("s1", "again", store.InputNormal)
	offerTo(t, ev2, "s1")
	epoch3 := acquire(t, c2, "s1")
	call(t, c2, wc.TakeInputs("s1", epoch3))
	call(t, c2, wc.Started("s1", epoch3, 1, circusv1.TurnOrigin_TURN_ORIGIN_INPUT))
	f.drop(c2, 0)
	f.released()
	f.clk.Advance(sessions.LeaseLifetime + time.Second)
	sessions.Expire(f.svc, t.Context())
	logs.want(t, "lease ended", "session_id", "s1", "worker_id", w, "boot_id", boot, "epoch", kv(epoch3), "reason", "expired",
		"interrupted", "true")
	f.settle()
	logs.want(t, "queue reason changed", "session_id", "s1", "reason", "worker_offline", "provider_id", "")
	wantEnds(t, logs, "s1", epoch, epoch2, epoch3)
	for _, r := range logs.matching("lease ended") {
		if r.level != slog.LevelInfo {
			t.Fatalf("lease ended at %v", r.level)
		}
	}
}

func hasKey(r logRecord, key string) bool {
	_, ok := r.attrs[key]
	return ok
}

// TestSessionLogsStopAndBroken: an ENDED report with an incomplete
// transcript, a repair (and a repair that changes nothing), fresh
// acquisitions that release the holder's own lease of a broken or stopped
// session, and a transcript-quota stop.
func TestSessionLogsStopAndBroken(t *testing.T) {
	logs := newCapture(slog.LevelInfo)
	f := newFixture(t, withLogger(logs))
	c, _, epoch := held(t, f)
	// A repair of a session that is not broken changes nothing.
	wantOk(t, call(t, c, wc.Op("s1", epoch, &circusv1.TranscriptRepaired{})), false)
	if n := len(logs.matching("transcript repaired")); n != 0 {
		t.Fatalf("%d transcript repaired records for a session never broken", n)
	}
	call(t, c, wc.Started("s1", epoch, 1, circusv1.TurnOrigin_TURN_ORIGIN_INPUT))
	call(t, c, wc.Op("s1", epoch, &circusv1.ReportTurn{Phase: circusv1.ReportTurn_PHASE_ENDED, LocalSeq: 1,
		Outcome: circusv1.TurnOutcome_TURN_OUTCOME_COMPLETED, TranscriptIncomplete: true}))
	logs.want(t, "turn ended", "session_id", "s1", "transcript_incomplete", "true")
	if r := logs.want(t, "session broken", "session_id", "s1", "epoch", kv(epoch), "turn_no", "1"); r.level != slog.LevelWarn {
		t.Fatalf("session broken at %v", r.level)
	}
	for range 2 {
		wantOk(t, call(t, c, wc.Op("s1", epoch, &circusv1.TranscriptRepaired{})), false)
	}
	if n := len(logs.matching("transcript repaired", "session_id", "s1", "epoch", kv(epoch))); n != 1 {
		t.Fatalf("%d transcript repaired records, want 1", n)
	}
	call(t, c, wc.Started("s1", epoch, 2, circusv1.TurnOrigin_TURN_ORIGIN_INPUT))
	call(t, c, wc.Op("s1", epoch, &circusv1.ReportTurn{Phase: circusv1.ReportTurn_PHASE_ENDED, LocalSeq: 2,
		Outcome: circusv1.TurnOutcome_TURN_OUTCOME_COMPLETED, TranscriptIncomplete: true}))
	wantRejected(t, call(t, c, wc.Acquire("s1", true)), circusv1.SessionReply_Rejected_CODE_NOT_OFFERED)
	logs.want(t, "lease ended", "session_id", "s1", "epoch", kv(epoch), "reason", "broken")

	g := newFixture(t, withLogger(logs))
	c, _, epoch = held(t, g)
	_, _, _, err := g.st.StopSession(t.Context(), "s1", ms(g), "USER")
	must(t, err)
	wantRejected(t, call(t, c, wc.Acquire("s1", true)), circusv1.SessionReply_Rejected_CODE_STOPPED)
	logs.want(t, "lease ended", "session_id", "s1", "epoch", kv(epoch), "reason", "stopped")

	q := newFixture(t, withLogger(logs), func(cfg *sessions.Config) { cfg.MaxSessionTranscriptBytes = 64 })
	c, _, epoch = held(t, q)
	wantRejected(t, call(t, c, wc.Append("s1", epoch, "", 1, &circusv1.TranscriptEntry{Uuid: "u1", Json: `{"x":"` + strings.Repeat("y", 100) + `"}`})),
		circusv1.SessionReply_Rejected_CODE_TRANSCRIPT_FULL)
	logs.want(t, "session stopped", "session_id", "s1", "reason", "transcript_full")
}

// releaseGate holds a committed ReleaseLease transaction back before the
// service applies the loss, so the scheduler can evaluate in between.
type releaseGate struct {
	store.Store
	committed chan struct{}
	resume    chan struct{}
}

type releaseGateOps struct {
	store.Ops
	releasing bool
}

func (o *releaseGateOps) ReleaseLease(ctx context.Context, f store.Fence) (store.LeaseLoss, error) {
	o.releasing = true
	return o.Ops.ReleaseLease(ctx, f)
}

func (g *releaseGate) Tx(ctx context.Context, fn func(context.Context, store.Ops) error) error {
	var release bool
	err := g.Store.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		ops := &releaseGateOps{Ops: tx}
		err := fn(ctx, ops)
		release = ops.releasing
		return err
	})
	if err == nil && release {
		close(g.committed)
		<-g.resume
	}
	return err
}

// TestSessionLogsOneEndPerRelease: the scheduler evaluates the session
// between the commit of a release and the service applying it; the lease
// still ends with exactly one "lease ended", the release's.
func TestSessionLogsOneEndPerRelease(t *testing.T) {
	logs := newCapture(slog.LevelDebug)
	g := &releaseGate{committed: make(chan struct{}), resume: make(chan struct{})}
	f := newFixture(t, withLogger(logs), func(c *sessions.Config) { g.Store, c.Store = c.Store, g })
	sessions.DisableUpkeep(f.svc)
	sessions.DisableFallback(f.svc)
	c, _, epoch := held(t, f)
	ctx := wait.Ctx(t)
	done := make(chan error, 1)
	go func() {
		_, err := c.Session(ctx, wc.Release("s1", epoch, circusv1.ReleaseLease_REASON_IDLE))
		done <- err
	}()
	resumed := false
	defer func() {
		if !resumed {
			close(g.resume)
		}
	}()
	wait.Recv(t, g.committed)
	// The evaluation reads the released lease while the entry still holds
	// it: the binding is dropped, which is no lease end of its own.
	f.svc.Wake("s1")
	f.settle()
	logs.want(t, "lease binding dropped", "session_id", "s1", "epoch", kv(epoch), "cause", "no_live_lease")
	resumed = true
	close(g.resume)
	must(t, wait.Recv(t, done))
	logs.want(t, "lease ended", "session_id", "s1", "epoch", kv(epoch), "reason", "released")
	wantEnds(t, logs, "s1", epoch)
}

// Distinct markers that must never reach a log record.
const (
	markMessage  = "MARK-MESSAGE-TEXT"
	markEntry    = "MARK-TRANSCRIPT-ENTRY"
	markPayload  = "MARK-EVENT-PAYLOAD"
	markLive     = "MARK-LIVE-PAYLOAD"
	markKey      = "MARK-PROVIDER-KEY"
	markAPIKey   = "MARK-X-API-KEY"
	markBaseURL  = "mark-base-url"
	markEnv      = "MARK-CUSTOM-ENV"
	markEnvName  = "MARK_CUSTOM_ENV_NAME"
	markApproval = "MARK-APPROVAL-TEXT"
	markRetry    = "MARK-APPROVED-RETRY"
	markUsage    = "MARK_USAGE"
)

// markedProvider stores a provider of the fixture's user for worker 0 with
// key, base URL and a custom variable carrying markers.
func (f *fixture) markedProvider(box *providers.Box, id string, scheme store.ProviderScheme, key string) {
	f.t.Helper()
	sealed, err := box.Seal(id, providers.Secrets{Key: key, Env: []providers.EnvValue{{Name: markEnvName, Value: markEnv}}})
	must(f.t, err)
	_, err = f.st.CreateProvider(f.t.Context(), store.Provider{ID: id, UserID: f.user.ID, Name: id, Kind: store.ProviderKindClaudeCode,
		Scheme: scheme, BaseURL: "https://" + markBaseURL + ".example", Secrets: sealed, KeyLast4: "last",
		EnvNames: []string{markEnvName}, EnvLast4: []string{"last"}, WorkerIDs: []string{f.workers[0].Identity.ID},
		CreatedAt: ms(f), UpdatedAt: ms(f)})
	must(f.t, err)
}

// TestSessionLogsKeepSecrets runs sessions under providers at the debug
// level, with every kind of content carrying its own marker, and checks
// that no marker appears anywhere in the formatted output.
func TestSessionLogsKeepSecrets(t *testing.T) {
	logs := newCapture(slog.LevelDebug)
	box := newBox(t, testKey)
	f := newFixture(t, withLogger(logs), withSecrets(box))
	f.markedProvider(box, "prov", store.ProviderSchemeBearer, "sk-"+markKey)
	f.markedProvider(box, "prov2", store.ProviderSchemeXAPIKey, "sk-"+markAPIKey)
	f.providerSession("s1", "prov", -1)
	f.providerSession("s2", "prov2", -1)
	c := f.dial(0, "boot-a")
	ev := f.attach(c, 4)
	f.input("s1", markMessage, store.InputNormal)
	offerTo(t, ev, "s1")
	epoch := acquire(t, c, "s1")
	f.input("s2", markMessage, store.InputNormal)
	offerTo(t, ev, "s2")
	epoch2 := acquire(t, c, "s2")
	for _, l := range []struct {
		id    string
		epoch uint64
		key   string
	}{{"s1", epoch, markKey}, {"s2", epoch2, markAPIKey}} {
		env := call(t, c, wc.LaunchEnv(l.id, l.epoch)).GetLaunchEnv().String()
		for _, m := range []string{l.key, markEnv, markEnvName, markBaseURL} {
			if !strings.Contains(env, m) {
				t.Fatalf("launch env of %s without %s: %v", l.id, m, env)
			}
		}
	}
	if in := call(t, c, wc.TakeInputs("s1", epoch)).GetInputs().GetInputs(); len(in) != 1 || in[0].GetText() != markMessage {
		t.Fatalf("inputs %v", in)
	}
	call(t, c, wc.Started("s1", epoch, 1, circusv1.TurnOrigin_TURN_ORIGIN_INPUT))
	wantOk(t, call(t, c, wc.Append("s1", epoch, "", 1, &circusv1.TranscriptEntry{Uuid: "u1", Json: `{"text":"` + markEntry + `"}`})), false)
	wantOk(t, call(t, c, wc.Publish("s1", epoch, 1,
		&circusv1.PublishEvents_Event{Kind: circusv1.SessionEvent_KIND_ASSISTANT, Json: `{"text":"` + markPayload + `"}`},
		&circusv1.PublishEvents_Event{Kind: circusv1.SessionEvent_KIND_TOOL_PROGRESS, Json: `{"text":"` + markLive + `"}`, Ephemeral: true})), false)
	r := call(t, c, wc.Op("s1", epoch, &circusv1.RequestApproval{ToolUseId: "tu1", Tool: "Bash", Device: "desk", MatchKey: strings.Repeat("a", 64),
		Display: markApproval, Reason: markApproval}))
	if r.GetApproval() == nil {
		t.Fatalf("approval %v", r)
	}
	// The user approves (as ResolveApproval does) and the holder takes the
	// retry message.
	_, err := f.st.ResolveApproval(t.Context(), store.ApprovalResolution{ApprovalID: r.GetApproval().GetApprovalId(), Approve: true, At: ms(f),
		GrantExpiresAt: ms(f) + time.Minute.Milliseconds(), Input: store.Input{InputID: "retry-1", Text: markRetry, CreatedAt: ms(f)}})
	must(t, err)
	f.svc.Wake("s1", circusv1.WorkerEvent_Nudge_REASON_APPROVAL)
	nudge(t, ev, "s1", circusv1.WorkerEvent_Nudge_REASON_APPROVAL)
	// Inputs taken and not persisted yet come again with it.
	approved := false
	for _, in := range call(t, c, wc.TakeInputs("s1", epoch)).GetInputs().GetInputs() {
		approved = approved || (in.GetText() == markRetry && in.GetKind() == circusv1.SessionReply_Inputs_Input_KIND_APPROVAL)
	}
	if !approved {
		t.Fatal("approved input not taken")
	}
	logs.want(t, "inputs taken", "session_id", "s1", "count", "2", "kinds", "user=1,approval=1")
	call(t, c, wc.Op("s1", epoch, &circusv1.ReportTurn{Phase: circusv1.ReportTurn_PHASE_ENDED, LocalSeq: 1,
		Outcome: circusv1.TurnOutcome_TURN_OUTCOME_COMPLETED, UsageJson: `{"input_tokens":1,"` + markUsage + `":2}`}))
	f.input("s1", markMessage+"-2", store.InputNormal)
	nudge(t, ev, "s1", circusv1.WorkerEvent_Nudge_REASON_INPUT)
	wantOk(t, call(t, c, wc.Release("s1", epoch, circusv1.ReleaseLease_REASON_IDLE)), false)
	sessions.Extend(f.svc, t.Context())
	f.settle()
	f.drop(c, 0)
	f.released()

	// The debug level was on: the flow's debug records are there.
	logs.want(t, "inputs taken", "session_id", "s1", "count", "1", "kinds", "user=1,approval=0")
	logs.want(t, "transcript appended", "session_id", "s1", "entries", "1", "deduplicated", "false")
	logs.want(t, "events published", "session_id", "s1", "count", "2")
	logs.want(t, "nudge sent", "session_id", "s1")
	logs.want(t, "offer queued", "session_id", "s1", "cause", "offered")
	logs.want(t, "lease extension round")
	logs.want(t, "lease acquired", "session_id", "s1", "provider_id", "prov")
	raw := logs.raw()
	for _, m := range []string{markMessage, markEntry, markPayload, markLive, markKey, markAPIKey, markBaseURL, markEnv, markEnvName,
		markApproval, markRetry, markUsage} {
		if strings.Contains(raw, m) {
			t.Errorf("marker %s logged:\n%s", m, raw)
		}
	}
}

// blockOn passes records to a capture, but blocks the first record with
// message msg until release is closed, signalling entered when it does.
type blockOn struct {
	*capture
	msg              string
	entered, release chan struct{}
	once             sync.Once
}

func (h *blockOn) Handle(ctx context.Context, r slog.Record) error {
	if r.Message == h.msg {
		h.once.Do(func() { close(h.entered); <-h.release })
	}
	return h.capture.Handle(ctx, r)
}

func (h *blockOn) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *blockOn) WithGroup(string) slog.Handler      { return h }

// TestSessionLogsLossFollowUpsFirst: when the handler is handed a lease
// loss's "lease ended", the loss has been applied: the interrupted turn's
// boundary was announced and the session, queued with pending work, was
// woken and offered again. A handler that blocks holds back only the record.
func TestSessionLogsLossFollowUpsFirst(t *testing.T) {
	h := &blockOn{capture: newCapture(slog.LevelInfo), msg: "lease ended", entered: make(chan struct{}), release: make(chan struct{})}
	f := newFixture(t, func(c *sessions.Config) { c.Logger = slog.New(h) })
	sessions.DisableUpkeep(f.svc)
	sessions.DisableFallback(f.svc)
	c, ev, epoch := held(t, f)
	call(t, c, wc.TakeInputs("s1", epoch))
	call(t, c, wc.Started("s1", epoch, 1, circusv1.TurnOrigin_TURN_ORIGIN_INPUT))
	f.input("s1", "more", store.InputNormal)
	f.settle()
	seq := f.bus.seq("s1")
	released := false
	defer func() {
		if !released {
			close(h.release)
		}
	}()
	ctx := wait.Ctx(t)
	done := make(chan error, 1)
	go func() {
		_, err := c.Session(ctx, wc.Release("s1", epoch, circusv1.ReleaseLease_REASON_IDLE))
		done <- err
	}()
	wait.Recv(t, h.entered)
	if got := f.bus.seq("s1"); got <= seq {
		t.Fatalf("handler entered before the interrupted turn was announced: seq %d, was %d", got, seq)
	}
	// Without the fallback, only the loss's Wake offers the session again.
	offerTo(t, ev, "s1")
	released = true
	close(h.release)
	must(t, wait.Recv(t, done))
	h.want(t, "lease ended", "session_id", "s1", "epoch", kv(epoch), "reason", "released", "interrupted", "true")
}

// resumeFault fails ResumeLeases transactions while on is set.
type resumeFault struct {
	store.Store
	on atomic.Bool
}

type resumeFaultOps struct{ store.Ops }

func (resumeFaultOps) ResumeLeases(context.Context, string, string, string, []store.SessionEpoch, int64, int64) ([]store.LeaseResumed, error) {
	return nil, errors.New("injected resume failure")
}

func (r *resumeFault) Tx(ctx context.Context, fn func(context.Context, store.Ops) error) error {
	if !r.on.Load() {
		return r.Store.Tx(ctx, fn)
	}
	return r.Store.Tx(ctx, func(ctx context.Context, tx store.Ops) error { return fn(ctx, resumeFaultOps{tx}) })
}

// TestSessionLogsReplaysAndResumeResults: replayed turn reports log no new
// turn line; a refused and a failed ResumeLeases each log one line with
// their result.
func TestSessionLogsReplaysAndResumeResults(t *testing.T) {
	logs := newCapture(slog.LevelInfo)
	fault := &resumeFault{}
	f := newFixture(t, withLogger(logs), func(c *sessions.Config) { fault.Store, c.Store = c.Store, fault })
	c, _, epoch := held(t, f)
	for range 2 {
		call(t, c, wc.Started("s1", epoch, 1, circusv1.TurnOrigin_TURN_ORIGIN_INPUT))
	}
	for range 2 {
		call(t, c, wc.Ended("s1", epoch, 1, circusv1.TurnOutcome_TURN_OUTCOME_COMPLETED, nil))
	}
	for _, msg := range []string{"turn started", "turn ended"} {
		if n := len(logs.matching(msg, "session_id", "s1")); n != 1 {
			t.Fatalf("%d %q records for one turn reported twice, want 1", n, msg)
		}
	}

	// A full session table refuses the whole call.
	sessions.SetMaxTracked(f.svc, 1)
	r, err := c.Resume(wait.Ctx(t), &circusv1.ResumeLeases_Held{SessionId: "r1", Epoch: 1})
	must(t, err)
	if r.GetRejected() == nil {
		t.Fatalf("resume with a full table = %v", r)
	}
	sessions.SetMaxTracked(f.svc, sessions.MaxTracked)
	logs.want(t, "leases resumed", "result", "rejected", "requested", "1")

	fault.on.Store(true)
	_, err = c.Resume(wait.Ctx(t), &circusv1.ResumeLeases_Held{SessionId: "s1", Epoch: epoch})
	must(t, err)
	fault.on.Store(false)
	logs.want(t, "leases resumed", "result", "failed", "requested", "1")
	if n := len(logs.matching("leases resumed")); n != 2 {
		t.Fatalf("%d leases resumed records for two calls, want 2", n)
	}
}

// TestSessionLogsExpiryPageAppliedFirst: two running leases expire in one
// committed page. With the handler blocked on the first "lease ended",
// both losses are applied already: both interrupted turns were announced
// and both sessions, queued with pending work, woken and offered again.
func TestSessionLogsExpiryPageAppliedFirst(t *testing.T) {
	h := &blockOn{capture: newCapture(slog.LevelInfo), msg: "lease ended", entered: make(chan struct{}), release: make(chan struct{})}
	f := newFixture(t, func(c *sessions.Config) { c.Logger = slog.New(h) })
	sessions.DisableUpkeep(f.svc)
	sessions.DisableFallback(f.svc)
	c, _, epoch1 := running(t, f)
	// The first connection has one slot: s2's lease is given to the same
	// boot directly, as one left by an earlier connection.
	f.session("s2", -1)
	f.input("s2", "go", store.InputNormal)
	now := ms(f)
	lr, err := f.st.AcquireLease(t.Context(), store.LeaseRequest{SessionID: "s2", WorkerID: f.workers[0].Identity.ID, BootID: f.boot("boot-a"),
		ConnID: "earlier", Now: now, ExpiresAt: now + sessions.LeaseLifetime.Milliseconds(), Fresh: true})
	must(t, err)
	_, err = f.st.ReportTurn(t.Context(), store.Fence{SessionID: "s2", WorkerID: lr.Lease.WorkerID, BootID: lr.Lease.BootID,
		ConnID: lr.Lease.ConnID, Epoch: lr.Lease.Epoch, Now: now}, store.TurnReport{Phase: store.TurnStarted, Origin: store.TurnFromInput, LocalSeq: 1, At: now})
	must(t, err)
	f.drop(c, 0)
	f.released()
	// Another worker waits for both sessions.
	other := f.dial(1, "boot-x")
	ev := f.attach(other, 2)
	f.settle()
	seq1, seq2 := f.bus.seq("s1"), f.bus.seq("s2")
	f.clk.Advance(sessions.LeaseLifetime + time.Second)
	released := false
	defer func() {
		if !released {
			close(h.release)
		}
	}()
	done := make(chan struct{})
	go func() { sessions.Expire(f.svc, t.Context()); close(done) }()
	wait.Recv(t, h.entered)
	if f.bus.seq("s1") <= seq1 || f.bus.seq("s2") <= seq2 {
		t.Fatalf("handler entered before both losses were announced: s1 %d (was %d), s2 %d (was %d)",
			f.bus.seq("s1"), seq1, f.bus.seq("s2"), seq2)
	}
	// Without the fallback, only the losses' Wakes offer the sessions.
	seen := map[string]bool{}
	for !seen["s1"] || !seen["s2"] {
		if o := next(t, ev).GetOffer(); o != nil {
			seen[o.GetSessionId()] = true
		}
	}
	released = true
	close(h.release)
	wait.Recv(t, done)
	wantEnds(t, h.capture, "s1", epoch1)
	wantEnds(t, h.capture, "s2", uint64(lr.Lease.Epoch))
}

// TestSessionLogsBootReleaseAfterRegistration: a new boot's release of the
// old boot's lease is logged only once Connect has registered the new boot
// and withdrawn the old connection's Offers, so while the handler blocks on
// the release's summary the old boot cannot acquire the session again.
func TestSessionLogsBootReleaseAfterRegistration(t *testing.T) {
	h := &blockOn{capture: newCapture(slog.LevelInfo), msg: "old boot leases released", entered: make(chan struct{}), release: make(chan struct{})}
	f := newFixture(t, func(c *sessions.Config) { c.Logger = slog.New(h) })
	sessions.DisableUpkeep(f.svc)
	old, _, epoch := held(t, f)
	worker, newBoot := f.workers[0].Identity.ID, f.boot("boot-b")
	released := false
	defer func() {
		if !released {
			close(h.release)
		}
	}()
	type connected struct {
		conn *sessions.Conn
		err  error
	}
	done := make(chan connected, 1)
	go func() {
		conn, err := f.svc.Connect(t.Context(), sessions.ConnInfo{WorkerID: worker, UserID: f.user.ID, BootID: newBoot})
		done <- connected{conn, err}
	}()
	wait.Recv(t, h.entered)
	h.want(t, "lease ended", "session_id", "s1", "epoch", kv(epoch), "reason", "boot_changed")
	f.settle()
	wantRejected(t, call(t, old, wc.Acquire("s1", true)), circusv1.SessionReply_Rejected_CODE_NOT_OFFERED)
	released = true
	close(h.release)
	got := wait.Recv(t, done)
	must(t, got.err)
	defer got.conn.Close()
	if l := f.lease("s1"); l.WorkerID != "" {
		t.Fatalf("the old boot holds the session again: %+v", l)
	}
}

// panicOn passes records to a capture but panics on message msg.
type panicOn struct {
	*capture
	msg string
}

func (h *panicOn) Handle(ctx context.Context, r slog.Record) error {
	if r.Message == h.msg {
		panic("handler panic: " + h.msg)
	}
	return h.capture.Handle(ctx, r)
}

func (h *panicOn) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *panicOn) WithGroup(string) slog.Handler      { return h }

// TestSessionLogsConnectPanicClosesConnection: a handler that panics on a
// boot release's summary, written after Connect registered the new
// connection, gets its panic back from Connect, and the connection nobody
// holds is closed: it is no longer registered, and once its boot's grace
// has passed it keeps no older boot out.
func TestSessionLogsConnectPanicClosesConnection(t *testing.T) {
	h := &panicOn{capture: newCapture(slog.LevelInfo), msg: "old boot leases released"}
	f := newFixture(t, func(c *sessions.Config) { c.Logger = slog.New(h) })
	sessions.DisableUpkeep(f.svc)
	_, _, epoch := held(t, f)
	w := f.workers[0].Identity.ID
	var conn *sessions.Conn
	p := func() (p any) {
		defer func() { p = recover() }()
		conn, _ = f.svc.Connect(t.Context(), sessions.ConnInfo{WorkerID: w, UserID: f.user.ID, BootID: f.boot("boot-b")})
		return nil
	}()
	if p != "handler panic: old boot leases released" || conn != nil {
		t.Fatalf("Connect: panic %v, connection returned %v", p, conn != nil)
	}
	if n := sessions.Connections(f.svc); n != 1 {
		t.Fatalf("%d connections registered after the panic, want only the old one", n)
	}
	wantEnds(t, h.capture, "s1", epoch)
	h.want(t, "worker connection closed", "worker_id", w, "boot_id", f.boot("boot-b"), "last_of_boot", "true")
	f.clk.Advance(sessions.LeaseLifetime + time.Second)
	if err := f.svc.CheckBoot(t.Context(), w, f.boot("boot-a")); err != nil {
		t.Fatalf("the closed connection still keeps the older boot out: %v", err)
	}
}
