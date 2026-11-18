package storetest

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/EricWvi/circus/internal/store"
)

// testMatchKey: a match key is exactly 64 lowercase hex digits.
func testMatchKey(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	createSession(t, s, f, "s1")
	fence := acquire(t, s, "s1", "w1", "b1", t0)
	good := mk("ls")
	for _, bad := range []string{
		"", good[:63], good + "0", strings.ToUpper(good), good[:63] + "g", "ls",
	} {
		a := newApproval("a-bad", "x")
		a.MatchKey = bad
		_, err := s.RequestApproval(ctx, fence, a)
		mustIs(t, err, store.ErrInvalid)
		_, err = s.ConsumeGrant(ctx, fence, store.GrantKey{Tool: "bash", DeviceID: "d1", MatchKey: bad}, "tu")
		mustIs(t, err, store.ErrInvalid)
	}
	a := newApproval("a-good", "x")
	a.MatchKey = good
	_, err := s.RequestApproval(ctx, fence, a)
	must(t, err)
	_, err = s.ConsumeGrant(ctx, fence, store.GrantKey{Tool: "bash", DeviceID: "d1", MatchKey: good}, strings.Repeat("t", store.MaxIDBytes+1))
	mustIs(t, err, store.ErrInvalid)
}

// approveGrant requests and approves approval id for key with expiry, persisting
// the retry inputs when the queue is full.
func approveGrant(t testing.TB, s store.Store, fence store.Fence, id, key string, at, expires int64, turn *int64) {
	t.Helper()
	ctx := context.Background()
	_, err := s.RequestApproval(ctx, fence, newApproval(id, key))
	must(t, err)
	r := store.ApprovalResolution{ApprovalID: id, Approve: true, At: at, GrantExpiresAt: expires,
		Input: store.Input{InputID: "retry-" + id, Text: "retry"}}
	if _, err = s.ResolveApproval(ctx, r); err == nil {
		return
	}
	mustIs(t, err, store.ErrLimit)
	taken, err := s.TakeInputs(ctx, fence)
	must(t, err)
	*turn++
	report(t, s, fence, store.TurnReport{LocalSeq: *turn, Phase: store.TurnEnded, Persisted: store.TurnPersisted{InputIDs: inputIDs(taken)}})
	_, err = s.ResolveApproval(ctx, r)
	must(t, err)
}

// testConsumeGrantReplay: a tool call consumes at most one grant; its
// resent request gets the same answer while that grant would be valid.
func testConsumeGrantReplay(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	createSession(t, s, f, "s1")
	fence := at(acquire(t, s, "s1", "w1", "b1", t0), t0+1)
	var turn int64
	approveGrant(t, s, fence, "a1", "make", t0+2, t0+1000, &turn)
	approveGrant(t, s, fence, "a2", "make", t0+3, t0+2000, &turn)
	key := store.GrantKey{Tool: "bash", DeviceID: "d1", MatchKey: mk("make")}
	other := store.GrantKey{Tool: "bash", DeviceID: "d1", MatchKey: mk("rm")}
	consume := func(now int64, k store.GrantKey, tu string) bool {
		t.Helper()
		ok, err := s.ConsumeGrant(ctx, at(fence, now), k, tu)
		must(t, err)
		return ok
	}
	mustEqual(t, "first", consume(t0+10, key, "tu1"), true)
	mustEqual(t, "replay", consume(t0+11, key, "tu1"), true)
	mustEqual(t, "replay with another key", consume(t0+11, other, "tu1"), false)
	// The replays consumed nothing more: one grant is left.
	recs, err := s.GetSessionRecords(ctx, at(fence, t0+12), records(store.RecordGrants))
	must(t, err)
	mustEqual(t, "unconsumed grants", len(recs.Grants), 1)
	mustEqual(t, "second call", consume(t0+12, key, "tu2"), true)
	mustEqual(t, "nothing left", consume(t0+13, key, "tu3"), false)
	// Once the consumed grant would have expired, the replay is refused.
	mustEqual(t, "replay after expiry", consume(t0+1000, key, "tu1"), false)
	mustEqual(t, "other replay still valid", consume(t0+1000, key, "tu2"), true)

	// Consumed grants kept for replay: at most MaxSessionGrants, the
	// oldest consumption dropped first.
	createSession(t, s, f, "s2")
	f2 := at(acquire(t, s, "s2", "w1", "b1", t0), t0+1)
	turn = 0
	for i := range store.MaxSessionGrants + 1 {
		id := fmt.Sprint("g", i)
		approveGrant(t, s, f2, id, id, t0+10, t0+100_000, &turn)
		ok, err := s.ConsumeGrant(ctx, at(f2, t0+20+int64(i)), store.GrantKey{Tool: "bash", DeviceID: "d1", MatchKey: mk(id)}, "tu-"+id)
		must(t, err)
		mustEqual(t, "consumed "+id, ok, true)
	}
	replay := func(id string) bool {
		ok, err := s.ConsumeGrant(ctx, at(f2, t0+500), store.GrantKey{Tool: "bash", DeviceID: "d1", MatchKey: mk(id)}, "tu-"+id)
		must(t, err)
		return ok
	}
	mustEqual(t, "oldest consumption forgotten", replay("g0"), false)
	mustEqual(t, "newest kept", replay(fmt.Sprint("g", store.MaxSessionGrants)), true)
	mustEqual(t, "second oldest kept", replay("g1"), true)
}

// testEventBatches: a worker's event batches follow the PublishEvents
// sequence rule; an invalid batch at the next sequence is consumed and
// recorded as dropped.
func testEventBatches(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	createSession(t, s, f, "s1")
	fence := acquire(t, s, "s1", "w1", "b1", t0)
	ev := func(kind string) []store.EventInput {
		return []store.EventInput{{Kind: kind, Payload: []byte(`{}`), At: t0}}
	}
	pub := func(fe store.Fence, seq int64, events []store.EventInput) (store.EventsAppended, error) {
		return s.AppendEvents(ctx, "s1", &store.EventPublish{Fence: fe, BatchSeq: seq}, events, store.DefaultMaxViewBytes)
	}
	last := func() int64 { return getSession(t, s, "s1").LastEventSeq }

	r, err := pub(fence, 1, ev("ASSISTANT"))
	must(t, err)
	mustEqual(t, "first", r, store.EventsAppended{Seqs: []int64{1}})
	r, err = pub(fence, 1, ev("ASSISTANT"))
	must(t, err)
	mustEqual(t, "repeat", []any{r.Deduplicated, last()}, []any{true, int64(1)})
	// A gap consumes nothing.
	_, err = pub(fence, 3, ev("ASSISTANT"))
	mustIs(t, err, store.ErrInvalid)
	// The next batch, invalid: consumed, recorded, nothing stored.
	// In the caller's Tx: a result with a nil error, so the consumption
	// commits.
	must(t, s.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		var err error
		r, err = tx.AppendEvents(ctx, "s1", &store.EventPublish{Fence: fence, BatchSeq: 2}, ev(store.EventKindTranscriptGapAccepted), store.DefaultMaxViewBytes)
		return err
	}))
	mustIs(t, r.DropReason, store.ErrInvalid)
	mustEqual(t, "dropped", []any{r.Dropped, last(), getSession(t, s, "s1").DroppedEventBatches}, []any{true, int64(1), int64(1)})
	r, err = pub(fence, 2, ev("ASSISTANT"))
	must(t, err)
	mustEqual(t, "dropped batch consumed", r.Deduplicated, true)
	r, err = pub(fence, 3, ev(strings.Repeat("k", store.MaxCodeBytes+1)))
	must(t, err)
	mustIs(t, r.DropReason, store.ErrInvalid)
	mustEqual(t, "over-cap kind dropped", []any{r.Dropped, getSession(t, s, "s1").DroppedEventBatches}, []any{true, int64(2)})
	r, err = pub(fence, 4, ev("RESULT"))
	must(t, err)
	mustEqual(t, "continues", r.Seqs, []int64{2})
	// Both turn boundaries are control-plane-only, like gap acceptance.
	for i, kind := range []string{store.EventKindTurnInterrupted, store.EventKindTurnStopped} {
		batch := int64(i + 5)
		r, err = pub(fence, batch, append(ev("ASSISTANT"), ev(kind)...))
		must(t, err)
		mustIs(t, r.DropReason, store.ErrInvalid)
		mustEqual(t, "reserved boundary batch dropped", []any{r.Dropped, last()}, []any{true, int64(2)})
		r, err = pub(fence, batch, ev(kind))
		must(t, err)
		mustEqual(t, "dropped retry consumed", r.Deduplicated, true)
	}
	// Fenced: nothing consumed.
	stale := fence
	stale.ConnID = "old"
	_, err = pub(stale, 5, ev("RESULT"))
	mustFenced(t, err, fence.Epoch)
	// A new epoch numbers from 1 again.
	_, err = s.ReleaseLease(ctx, fence)
	must(t, err)
	next := acquire(t, s, "s1", "w1", "b1", t0+1)
	_, err = pub(next, 2, ev("RESULT"))
	mustIs(t, err, store.ErrInvalid)
	r, err = pub(next, 1, ev("RESULT"))
	must(t, err)
	mustEqual(t, "new epoch", r.Seqs, []int64{3})
	// A batch holds at most MaxEventBatch events.
	many := func(n int) []store.EventInput {
		out := make([]store.EventInput, n)
		for i := range out {
			out[i] = store.EventInput{Kind: "ASSISTANT", At: t0}
		}
		return out
	}
	_, err = s.AppendEvents(ctx, "s1", nil, many(store.MaxEventBatch+1), store.DefaultMaxViewBytes)
	mustIs(t, err, store.ErrInvalid)
	r, err = s.AppendEvents(ctx, "s1", nil, many(store.MaxEventBatch), store.DefaultMaxViewBytes)
	must(t, err)
	mustEqual(t, "a full batch", len(r.Seqs), store.MaxEventBatch)
	// The control plane records the gap kind.
	cp, err := s.AppendEvents(ctx, "s1", nil, ev(store.EventKindTranscriptGapAccepted), store.DefaultMaxViewBytes)
	must(t, err)
	mustEqual(t, "control plane", cp.Seqs, []int64{4 + store.MaxEventBatch})
}

// testReleaseRepeated: a resent ReleaseLease of the holder that lost the
// lease is ok and changes nothing until another epoch is acquired.
func testReleaseRepeated(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	addWorker(t, s, "w2", "u1")
	createSession(t, s, f, "s1")
	f1 := acquire(t, s, "s1", "w1", "b1", t0)
	loss, err := s.ReleaseLease(ctx, f1)
	must(t, err)
	mustEqual(t, "released", loss.Repeated, false)
	resend := f1
	resend.ConnID, resend.Now = "new-conn", t0+5
	loss, err = s.ReleaseLease(ctx, resend)
	must(t, err)
	mustEqual(t, "resent", []any{loss.Repeated, loss.Released.WorkerID, loss.Interrupted}, []any{true, "", false})
	for _, other := range []store.Fence{
		{SessionID: "s1", Epoch: f1.Epoch, WorkerID: "w2", BootID: "b1", ConnID: "c", Now: t0 + 6},
		{SessionID: "s1", Epoch: f1.Epoch, WorkerID: "w1", BootID: "b2", ConnID: "c", Now: t0 + 6},
		{SessionID: "s1", Epoch: f1.Epoch + 1, WorkerID: "w1", BootID: "b1", ConnID: "c", Now: t0 + 6},
	} {
		_, err = s.ReleaseLease(ctx, other)
		mustFenced(t, err, f1.Epoch)
	}
	// Lost by expiry: the holder's late release is a resend too.
	f2 := acquire(t, s, "s1", "w1", "b2", t0+10)
	_, err = s.ExpireLeases(ctx, t0+10+ttl, 10)
	must(t, err)
	loss, err = s.ReleaseLease(ctx, at(f2, t0+10+ttl+1))
	must(t, err)
	mustEqual(t, "after expiry", loss.Repeated, true)
	// Another epoch acquired: the resend is fenced.
	f3 := acquire(t, s, "s1", "w2", "b9", t0+20+ttl)
	_, err = s.ReleaseLease(ctx, at(f2, t0+21+ttl))
	mustFenced(t, err, f3.Epoch)
}

// testAcquireFresh: a fresh acquisition by the holder takes the lease
// anew: next epoch, lease-loss rule applied and reported.
func testAcquireFresh(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	addWorker(t, s, "w2", "u1")
	createSession(t, s, f, "s1")
	old := acquire(t, s, "s1", "w1", "b1", t0)
	submit(t, s, newInput("s1", 1))
	_, err := s.TakeInputs(ctx, old)
	must(t, err)
	report(t, s, old, store.TurnReport{LocalSeq: 1, Phase: store.TurnStarted})
	req := leaseReq("s1", "w1", "b1", "c2", t0+1)
	req.Fresh = true
	r, err := s.AcquireLease(ctx, req)
	must(t, err)
	mustEqual(t, "fresh", []any{r.Lease.Epoch, r.Reattached, r.Loss != nil && r.Loss.Interrupted, r.Session.State},
		[]any{old.Epoch + 1, false, true, store.SessionIdle})
	_, err = s.CheckFence(ctx, at(old, t0+2))
	mustFenced(t, err, old.Epoch+1)
	next := store.Fence{SessionID: "s1", Epoch: r.Lease.Epoch, WorkerID: "w1", BootID: "b1", ConnID: "c2", Now: t0 + 2}
	taken, err := s.TakeInputs(ctx, next)
	must(t, err)
	mustEqual(t, "requeued input taken again", inputIDs(taken), []string{"s1-in1"})
	// Fresh by another worker while held: refused.
	req = leaseReq("s1", "w2", "b2", "c3", t0+3)
	req.Fresh = true
	_, err = s.AcquireLease(ctx, req)
	mustIs(t, err, store.ErrLeaseHeld)
	// On a stopped session a plain reattach still works.
	_, _, _, err = s.StopSession(ctx, "s1", t0+4, "")
	must(t, err)
	r, err = s.AcquireLease(ctx, leaseReq("s1", "w1", "b1", "c4", t0+5))
	must(t, err)
	mustEqual(t, "reattach on stopped", r.Reattached, true)
	// A non-holder is refused and changes nothing.
	req = leaseReq("s1", "w2", "b2", "c5", t0+6)
	req.Fresh = true
	_, err = s.AcquireLease(ctx, req)
	mustIs(t, err, store.ErrStopped)
	l, err := s.GetLease(ctx, "s1")
	must(t, err)
	mustEqual(t, "lease kept", []any{l.WorkerID, l.Epoch}, []any{"w1", next.Epoch})
	// The holder's fresh acquisition: refused, and the lease released for
	// it, so it is not left unreleasable.
	req = leaseReq("s1", "w1", "b1", "c6", t0+7)
	req.Fresh = true
	// In the caller's Tx, as the lease reply does: a result with a nil
	// error, so the Tx commits the release.
	must(t, s.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		var err error
		r, err = tx.AcquireLease(ctx, req)
		return err
	}))
	mustIs(t, r.Refusal, store.ErrStopped)
	mustEqual(t, "released for it", []any{r.Released, r.Loss != nil, r.Loss.Released.Epoch, r.Session.State},
		[]any{true, true, next.Epoch, store.SessionStopped})
	l, err = s.GetLease(ctx, "s1")
	must(t, err)
	mustEqual(t, "lease gone", []any{l.WorkerID, l.Epoch}, []any{"", next.Epoch})
	// Its resent release is recognized; a second fresh attempt is only a
	// refusal.
	loss, err := s.ReleaseLease(ctx, at(next, t0+8))
	must(t, err)
	mustEqual(t, "resend", loss.Repeated, true)
	r, err = s.AcquireLease(ctx, req)
	mustIs(t, err, store.ErrStopped)
	mustEqual(t, "nothing more", []any{r.Loss, r.Released}, []any{(*store.LeaseLoss)(nil), false})

	// A broken session: the fresh holder's lease is released too, the
	// session stays broken and the refusal is ErrBroken.
	createSession(t, s, f, "s2")
	b1 := acquireOn(t, s, "s2", "w1", "b1", "c1", t0)
	report(t, s, b1, store.TurnReport{LocalSeq: 1, Phase: store.TurnEnded, Outcome: store.TurnError, TranscriptIncomplete: true})
	req = leaseReq("s2", "w1", "b1", "c2", t0+1)
	req.Fresh = true
	r, err = s.AcquireLease(ctx, req)
	must(t, err)
	mustIs(t, r.Refusal, store.ErrBroken)
	mustEqual(t, "broken released", []any{r.Released, r.Session.State}, []any{true, store.SessionBroken})
	l, err = s.GetLease(ctx, "s2")
	must(t, err)
	mustEqual(t, "broken lease gone", l.WorkerID, "")
	// A non-fresh reattach is refused only for a non-holder.
	_, err = s.AcquireLease(ctx, leaseReq("s2", "w1", "b1", "c3", t0+2))
	mustIs(t, err, store.ErrBroken)
}

// testExtendLeasesAfterRestart: every held lease, expired or not, is
// extended to at least the given expiry; released leases are untouched.
func testExtendLeasesAfterRestart(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	for _, id := range []string{"s1", "s2", "s3"} {
		createSession(t, s, f, id)
	}
	acquire(t, s, "s1", "w1", "b1", t0) // expires t0+ttl
	f2 := acquire(t, s, "s2", "w1", "b1", t0)
	_, err := s.ExtendLeases(ctx, []store.LeaseKey{{SessionID: "s2", Epoch: f2.Epoch, WorkerID: "w1", BootID: "b1", ConnID: f2.ConnID}}, t0, t0+100*ttl)
	must(t, err)
	f3 := acquire(t, s, "s3", "w1", "b1", t0)
	_, err = s.ReleaseLease(ctx, f3)
	must(t, err)
	restart := t0 + 10*ttl // down far longer than a lease lifetime
	n, err := s.ExtendLeasesAfterRestart(ctx, restart+ttl)
	must(t, err)
	mustEqual(t, "extended", n, 1)
	losses, err := s.ExpireLeases(ctx, restart, 10)
	must(t, err)
	mustEqual(t, "nothing expires at restart", len(losses), 0)
	for id, want := range map[string]int64{"s1": restart + ttl, "s2": t0 + 100*ttl, "s3": 0} {
		l, err := s.GetLease(ctx, id)
		must(t, err)
		mustEqual(t, "expiry of "+id, l.ExpiresAt, want)
	}
	res, err := s.ResumeLeases(ctx, "w1", "b1", "c-new", []store.SessionEpoch{{SessionID: "s1", Epoch: 1}}, restart+1, restart+1+ttl)
	must(t, err)
	must(t, res[0].Err)
}

// testRecoverSession: accepting a broken session's transcript gap revokes
// its lease, returns it to idle or queued and records the gap event.
func testRecoverSession(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	createSession(t, s, f, "s1")
	fence := acquire(t, s, "s1", "w1", "b1", t0)
	_, err := s.RecoverSession(ctx, "s1", true, t0+1, store.DefaultMaxViewBytes)
	mustIs(t, err, store.ErrNotBroken)
	report(t, s, fence, store.TurnReport{LocalSeq: 1, Phase: store.TurnEnded, Outcome: store.TurnError, TranscriptIncomplete: true})
	submit(t, s, newInput("s1", 1))
	_, err = s.RecoverSession(ctx, "s1", false, t0+2, store.DefaultMaxViewBytes)
	mustIs(t, err, store.ErrInvalid)
	mustEqual(t, "still broken", getSession(t, s, "s1").State, store.SessionBroken)
	r, err := s.RecoverSession(ctx, "s1", true, t0+3, store.DefaultMaxViewBytes)
	must(t, err)
	mustEqual(t, "recovered", []any{r.Session.State, r.Loss != nil, r.Loss.Released.Epoch, r.Loss.Session.State},
		[]any{store.SessionQueued, true, fence.Epoch, store.SessionQueued})
	page, err := s.ReadEvents(ctx, store.EventFilter{SessionID: "s1", Limit: 10, MaxBytes: 1 << 20})
	must(t, err)
	mustEqual(t, "gap event", []any{len(page.Events), page.Events[0].Kind, page.Events[0].Seq}, []any{1, store.EventKindTranscriptGapAccepted, r.EventSeq})
	// The revoked epoch is fenced; the next holder takes the next one.
	_, err = s.TakeInputs(ctx, at(fence, t0+4))
	mustFenced(t, err, fence.Epoch)
	mustEqual(t, "next epoch", acquire(t, s, "s1", "w1", "b2", t0+5).Epoch, fence.Epoch+1)

	// Without a lease or pending work: idle, no loss.
	createSession(t, s, f, "s2")
	f2 := acquire(t, s, "s2", "w1", "b1", t0)
	report(t, s, f2, store.TurnReport{LocalSeq: 1, Phase: store.TurnEnded, Outcome: store.TurnError, TranscriptIncomplete: true})
	_, err = s.ReleaseLease(ctx, f2)
	must(t, err)
	r, err = s.RecoverSession(ctx, "s2", true, t0+6, store.DefaultMaxViewBytes)
	must(t, err)
	mustEqual(t, "idle", []any{r.Session.State, r.Loss}, []any{store.SessionIdle, (*store.LeaseLoss)(nil)})
	_, err = s.RecoverSession(ctx, "ghost", true, t0, store.DefaultMaxViewBytes)
	mustIs(t, err, store.ErrNotFound)
}

// testTranscriptGapMark: the mark is a gap the user accepted and the model
// has not been told of. Breaking does not set it, recovery sets it to the
// turn that broke, repair never touches it, and only an acknowledgement
// of the equal value clears it.
func testTranscriptGapMark(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	addWorker(t, s, "w2", "u1")
	addWorker(t, s, "w3", "u1")
	createSession(t, s, f, "s1")
	fence := acquire(t, s, "s1", "w1", "b1", t0)
	gap := func() int64 { return getSession(t, s, "s1").TranscriptGapTurnNo }
	incomplete := func(fe store.Fence, seq int64) int64 {
		t.Helper()
		return report(t, s, fe, store.TurnReport{LocalSeq: seq, Phase: store.TurnEnded, Outcome: store.TurnError, TranscriptIncomplete: true}).Turn.TurnNo
	}
	broke := incomplete(fence, 1)
	incomplete(fence, 2) // still broken: not the turn that broke
	mustEqual(t, "a break alone is not the mark", gap(), int64(0))

	// Recovery: the turn that broke, kept for the next holder.
	r, err := s.RecoverSession(ctx, "s1", true, t0+1, store.DefaultMaxViewBytes)
	must(t, err)
	mustEqual(t, "set by recovery", r.Session.TranscriptGapTurnNo, broke)
	a, err := s.AcquireLease(ctx, leaseReq("s1", "w2", "b2", "c2", t0+2))
	must(t, err)
	mustEqual(t, "in the lease reply", a.Session.TranscriptGapTurnNo, broke)
	next := store.Fence{SessionID: "s1", Epoch: a.Lease.Epoch, WorkerID: "w2", BootID: "b2", ConnID: "c2", Now: t0 + 3}
	// 0 acknowledges nothing; another value changes nothing.
	must(t, s.PatchSessionRecords(ctx, next, store.RecordsPatch{Persisted: store.TurnPersisted{GapNoticeTurnNo: 0}}))
	report(t, s, next, store.TurnReport{LocalSeq: 1, Phase: store.TurnEnded, Persisted: store.TurnPersisted{GapNoticeTurnNo: broke + 1}})
	mustEqual(t, "unequal ack", gap(), broke)
	// Another break and its repair leave the mark alone.
	incomplete(next, 2)
	mustEqual(t, "break keeps the mark", gap(), broke)
	_, err = s.TranscriptRepaired(ctx, next)
	must(t, err)
	mustEqual(t, "repair keeps the mark", gap(), broke)
	must(t, s.PatchSessionRecords(ctx, next, store.RecordsPatch{Persisted: store.TurnPersisted{GapNoticeTurnNo: broke}}))
	mustEqual(t, "acknowledged by patch", gap(), int64(0))

	// A later break, after the repair, is a new turn that broke.
	broke2 := incomplete(next, 3)
	r, err = s.RecoverSession(ctx, "s1", true, t0+4, store.DefaultMaxViewBytes)
	must(t, err)
	mustEqual(t, "second recovery", r.Session.TranscriptGapTurnNo, broke2)
	a, err = s.AcquireLease(ctx, leaseReq("s1", "w3", "b3", "c3", t0+5))
	must(t, err)
	last := store.Fence{SessionID: "s1", Epoch: a.Lease.Epoch, WorkerID: "w3", BootID: "b3", ConnID: "c3", Now: t0 + 6}
	report(t, s, last, store.TurnReport{LocalSeq: 1, Phase: store.TurnEnded, Persisted: store.TurnPersisted{GapNoticeTurnNo: broke2}})
	mustEqual(t, "acknowledged by ENDED", gap(), int64(0))

	// Where an acknowledgement is not allowed.
	_, err = s.ReportTurn(ctx, last, store.TurnReport{LocalSeq: 2, Phase: store.TurnStarted, Origin: store.TurnFromInput, At: t0 + 7,
		Persisted: store.TurnPersisted{GapNoticeTurnNo: 1}})
	mustIs(t, err, store.ErrInvalid)
	_, err = s.ReportTurn(ctx, last, store.TurnReport{LocalSeq: 2, Phase: store.TurnEnded, Outcome: store.TurnError, At: t0 + 7,
		TranscriptIncomplete: true, Origin: store.TurnFromInput, Persisted: store.TurnPersisted{GapNoticeTurnNo: 1}})
	mustIs(t, err, store.ErrInvalid)
	mustIs(t, s.PatchSessionRecords(ctx, last, store.RecordsPatch{Persisted: store.TurnPersisted{GapNoticeTurnNo: -1}}), store.ErrInvalid)
}

// testTurnRules: STARTED takes the epoch's next local_seq and closes an
// unended predecessor as interrupted; a late ENDED for that turn applies
// its marks and transcript state but keeps its outcome; an ENDED for a
// turn never started is refused. Recovery's LastTurnInterrupted means a
// turn closed without ENDED by a lost lease, until a turn starts.
func testTurnRules(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	addWorker(t, s, "w2", "u1")
	createSession(t, s, f, "s1")
	fence := acquire(t, s, "s1", "w1", "b1", t0)
	turn := func(seq int64, ph store.TurnPhase, at int64, p store.TurnPersisted, incomplete bool) (store.TurnReported, error) {
		return s.ReportTurn(ctx, fence, store.TurnReport{LocalSeq: seq, Phase: ph, Origin: store.TurnFromInput,
			Outcome: func() store.TurnOutcome {
				if seq == 1 && ph == store.TurnEnded {
					return store.TurnStopped
				}
				return store.TurnCompleted
			}(), Usage: store.TurnUsage{InputTokens: 7}, At: at, Persisted: p, TranscriptIncomplete: incomplete})
	}
	_, err := turn(2, store.TurnStarted, t0+1, store.TurnPersisted{}, false)
	mustIs(t, err, store.ErrInvalid) // the first is 1
	_, err = turn(1, store.TurnEnded, t0+1, store.TurnPersisted{}, false)
	mustIs(t, err, store.ErrInvalid) // never started
	must(t, errOf(turn(1, store.TurnStarted, t0+1, store.TurnPersisted{}, false)))
	r, err := turn(1, store.TurnStarted, t0+2, store.TurnPersisted{}, false)
	must(t, err)
	mustEqual(t, "repeat", []any{r.Duplicate, r.Session.TurnCount}, []any{true, int64(1)})
	_, err = turn(3, store.TurnStarted, t0+2, store.TurnPersisted{}, false)
	mustIs(t, err, store.ErrInvalid) // skips 2

	// STARTED 2 closes turn 1, whose ENDED never came.
	submit(t, s, newInput("s1", 1))
	_, err = s.TakeInputs(ctx, fence)
	must(t, err)
	r, err = turn(2, store.TurnStarted, t0+3, store.TurnPersisted{}, false)
	must(t, err)
	turns, err := s.ListTurns(ctx, store.TurnFilter{SessionID: "s1", Limit: 10})
	must(t, err)
	mustEqual(t, "closed by its successor", []any{turns[0].Outcome, turns[0].EndedAt, r.Session.State, r.Session.LastTurnOutcome},
		[]any{store.TurnInterrupted, t0 + 3, store.SessionRunning, store.TurnInterrupted})
	checkTurnBoundary(t, s, "s1", 1, t0+3, store.TurnInterrupted)
	rec, err := s.GetRecovery(ctx, fence)
	must(t, err)
	mustEqual(t, "a turn has started since", rec.LastTurnInterrupted, false)
	// Its late ENDED: marks applied and usage recorded, outcome and end
	// time kept.
	r, err = turn(1, store.TurnEnded, t0+4, store.TurnPersisted{InputIDs: []string{"s1-in1"}}, false)
	must(t, err)
	mustEqual(t, "late ENDED", []any{r.Duplicate, r.Turn.Outcome, r.Turn.EndedAt, r.Turn.Usage, r.Session.Usage, r.Session.State},
		[]any{false, store.TurnInterrupted, t0 + 3, store.TurnUsage{InputTokens: 7}, store.TurnUsage{InputTokens: 7}, store.SessionRunning})
	taken, err := s.TakeInputs(ctx, fence)
	must(t, err)
	mustEqual(t, "marks applied", len(taken), 0)
	r, err = turn(1, store.TurnEnded, t0+5, store.TurnPersisted{}, false)
	must(t, err)
	mustEqual(t, "repeated late ENDED", r.Duplicate, true)
	checkTurnBoundary(t, s, "s1", 1, t0+3, store.TurnInterrupted)
	r, err = turn(2, store.TurnEnded, t0+6, store.TurnPersisted{}, false)
	must(t, err)
	mustEqual(t, "turn 2 ends", []any{r.Session.State, r.Session.Usage.InputTokens, r.Session.LastTurnOutcome},
		[]any{store.SessionIdle, int64(14), store.TurnCompleted})

	// A worker-reported INTERRUPTED outcome is not "closed without ENDED".
	must(t, errOf(turn(3, store.TurnStarted, t0+7, store.TurnPersisted{}, false)))
	_, err = s.ReportTurn(ctx, fence, store.TurnReport{LocalSeq: 3, Phase: store.TurnEnded, Outcome: store.TurnInterrupted, At: t0 + 8})
	must(t, err)
	rec, err = s.GetRecovery(ctx, fence)
	must(t, err)
	mustEqual(t, "reported interrupt", rec.LastTurnInterrupted, false)
	// A confirmed user stop has its own outcome and the same recovery rule.
	must(t, errOf(turn(4, store.TurnStarted, t0+8, store.TurnPersisted{}, false)))
	stopped, err := s.ReportTurn(ctx, fence, store.TurnReport{LocalSeq: 4, Phase: store.TurnEnded, Outcome: store.TurnStopped, At: t0 + 9})
	must(t, err)
	mustEqual(t, "confirmed stop", stopped.Session.LastTurnOutcome, store.TurnStopped)
	rec, err = s.GetRecovery(ctx, fence)
	must(t, err)
	mustEqual(t, "reported stop", rec.LastTurnInterrupted, false)
	// A late incomplete ENDED of a closed turn still breaks the session,
	// with that turn as the one that broke it.
	must(t, errOf(turn(5, store.TurnStarted, t0+9, store.TurnPersisted{}, false)))
	must(t, errOf(turn(6, store.TurnStarted, t0+10, store.TurnPersisted{}, false))) // closes 5
	r, err = turn(5, store.TurnEnded, t0+11, store.TurnPersisted{}, true)
	must(t, err)
	mustEqual(t, "late incomplete", []any{r.Session.State, r.Turn.Outcome}, []any{store.SessionBroken, store.TurnInterrupted})

	// A lease lost mid-turn: the next holder's recovery says so until a
	// turn starts; local_seq starts over in the new epoch.
	_, err = s.ExpireLeases(ctx, t0+ttl, 10)
	must(t, err)
	rr, err := s.RecoverSession(ctx, "s1", true, t0+ttl+1, store.DefaultMaxViewBytes)
	must(t, err)
	mustEqual(t, "the turn that broke", rr.Session.TranscriptGapTurnNo, r.Turn.TurnNo)
	next := at(acquire(t, s, "s1", "w2", "b2", t0+ttl+2), t0+ttl+3)
	rec, err = s.GetRecovery(ctx, next)
	must(t, err)
	mustEqual(t, "closed by lease loss", rec.LastTurnInterrupted, true)
	_, err = s.ReportTurn(ctx, next, store.TurnReport{LocalSeq: 6, Phase: store.TurnStarted, Origin: store.TurnFromInput, At: t0 + ttl + 3})
	mustIs(t, err, store.ErrInvalid)
	report(t, s, next, store.TurnReport{LocalSeq: 1, Phase: store.TurnStarted})
	rec, err = s.GetRecovery(ctx, next)
	must(t, err)
	mustEqual(t, "a turn started", rec.LastTurnInterrupted, false)
}

// testRepairOutsideBroken: TranscriptRepaired changes nothing in any state
// but broken.
func testRepairOutsideBroken(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	createSession(t, s, f, "s1")
	fence := acquire(t, s, "s1", "w1", "b1", t0)
	check := func(want store.SessionState) {
		t.Helper()
		before := getSession(t, s, "s1")
		mustEqual(t, "state before", before.State, want)
		after, err := s.TranscriptRepaired(ctx, at(fence, t0+50))
		must(t, err)
		mustEqual(t, "unchanged by repair", after, before)
	}
	check(store.SessionIdle)
	report(t, s, fence, store.TurnReport{LocalSeq: 1, Phase: store.TurnStarted})
	check(store.SessionRunning)
	_, _, _, err := s.StopSession(ctx, "s1", t0+2, "")
	must(t, err)
	check(store.SessionStopped)
}

// testUnpersistedCallCap: admission refuses a session's call past
// MaxUnpersistedSessionCalls calls not yet persisted.
func testUnpersistedCallCap(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	createSession(t, s, f, "s1")
	fence := acquire(t, s, "s1", "w1", "b1", t0)
	must(t, s.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		for i := range store.MaxUnpersistedSessionCalls - 1 {
			sessionCall(t, tx, f, fmt.Sprint("c", i), "s1", store.CallCompleted)
		}
		return nil
	}))
	must(t, s.AdmitSessionCall(ctx, fence, "next", store.DefaultMaxActiveSessionCalls))
	sessionCall(t, s, f, "last", "s1", store.CallCompleted)
	mustIs(t, s.AdmitSessionCall(ctx, fence, "next", store.DefaultMaxActiveSessionCalls), store.ErrTooManyCalls)
	// An existing call does not count against itself.
	must(t, s.AdmitSessionCall(ctx, fence, "last", store.DefaultMaxActiveSessionCalls))
	// Persisting one makes room.
	report(t, s, fence, store.TurnReport{LocalSeq: 1, Phase: store.TurnEnded, Persisted: store.TurnPersisted{CallIDs: []string{"c0"}}})
	must(t, s.AdmitSessionCall(ctx, fence, "next", store.DefaultMaxActiveSessionCalls))
}

// testTranscriptFullInTx: a charge cap's refusal stops the session, and the
// stop commits in the caller's Tx because the refusal is a result.
func testTranscriptFullInTx(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	createSession(t, s, f, "s1")
	fence := acquire(t, s, "s1", "w1", "b1", t0)
	q := store.TranscriptQuota{MaxEntryBytes: 100, MaxSessionBytes: 100, MaxUserBytes: 1000}
	var r store.TranscriptAppended
	must(t, s.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		var err error
		r, err = tx.AppendTranscript(ctx, fence, store.TranscriptBatch{BatchSeq: 1, Entries: []store.TranscriptEntry{{JSON: []byte(strings.Repeat("x", 200))}}}, q)
		return err
	}))
	mustIs(t, r.FullReason, store.ErrTranscriptFull)
	sess := getSession(t, s, "s1")
	mustEqual(t, "stopped", []any{r.Full, r.Inserted, sess.State, sess.Reason, sess.TranscriptBytes}, []any{true, 0, store.SessionStopped, "TRANSCRIPT_FULL", int64(0)})
	// The holder may still flush what fits.
	ok, err := s.AppendTranscript(ctx, fence, store.TranscriptBatch{BatchSeq: 1, Entries: []store.TranscriptEntry{{JSON: []byte("{}")}}}, q)
	must(t, err)
	mustEqual(t, "flush after the stop", []any{ok.Inserted, ok.Full}, []any{1, false})
}

// testGrantReplayTrim: past the cap, expired replay records go before the
// oldest live one.
func testGrantReplayTrim(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	createSession(t, s, f, "s1")
	fence := at(acquire(t, s, "s1", "w1", "b1", t0), t0+1)
	var turn int64
	for i := range store.MaxSessionGrants + 1 {
		id := fmt.Sprint("g", i)
		expires := int64(t0 + 100_000)
		if i == 1 {
			expires = t0 + 1_000 // the second consumption expires early
		}
		now := t0 + 10 + int64(i)
		if i == store.MaxSessionGrants {
			now = t0 + 2_000 // the last one after g1 expired
		}
		approveGrant(t, s, at(fence, now), id, id, now, expires, &turn)
		ok, err := s.ConsumeGrant(ctx, at(fence, now), store.GrantKey{Tool: "bash", DeviceID: "d1", MatchKey: mk(id)}, "tu-"+id)
		must(t, err)
		mustEqual(t, "consumed "+id, ok, true)
	}
	ok, err := s.ConsumeGrant(ctx, at(fence, t0+2_001), store.GrantKey{Tool: "bash", DeviceID: "d1", MatchKey: mk("g0")}, "tu-g0")
	must(t, err)
	mustEqual(t, "the oldest live record kept", ok, true)
}
