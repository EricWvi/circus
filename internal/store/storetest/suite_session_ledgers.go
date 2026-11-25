package storetest

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/EricWvi/circus/internal/store"
)

func newSessionLedger(sessionID string, f fixture) store.SessionLedger {
	return store.SessionLedger{SessionID: sessionID, UserID: f.user.ID, ProjectID: "session-project", DeviceID: f.device.ID,
		State: store.SessionLedgerPending, CreatedAt: t0, UpdatedAt: t0}
}

// testSessionLedgers: a session's snapshot row is created, read with its
// open changeset, replaced only from the version read, listed while it has
// work, and holds the session's turns until it is ready or failed.
func testSessionLedgers(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	createSession(t, s, f, "s1")
	l := newSessionLedger("s1", f)
	mustIs(t, s.CreateSessionLedger(ctx, newSessionLedger("missing", f)), store.ErrNotFound)
	bad := l
	bad.Path = "/tmp/x"
	mustIs(t, s.CreateSessionLedger(ctx, bad), store.ErrInvalid)
	must(t, s.CreateSessionLedger(ctx, l))
	mustIs(t, s.CreateSessionLedger(ctx, l), store.ErrExists)
	got, err := s.GetSessionLedger(ctx, "s1")
	must(t, err)
	mustEqual(t, "created", got, l)
	_, err = s.GetSessionLedger(ctx, "s2")
	mustIs(t, err, store.ErrNotFound)

	// Pending holds turns, like a workspace that is not ready.
	sess, err := s.GetSession(ctx, "s1", t0)
	must(t, err)
	mustEqual(t, "held", sess.WorkspaceBlocked, true)
	_, err = s.AcquireLease(ctx, leaseReq("s1", f.worker.ID, "b1", "c1", t0))
	mustIs(t, err, store.ErrLeaseHeld)
	// So does a preparation.
	preparing := got
	preparing.State, preparing.UpdatedAt = store.SessionLedgerPreparing, got.UpdatedAt+1
	ok, err := s.UpdateSessionLedger(ctx, preparing, got.UpdatedAt)
	must(t, err)
	mustEqual(t, "preparing", ok, true)
	sess, err = s.GetSession(ctx, "s1", t0)
	must(t, err)
	mustEqual(t, "preparing holds", sess.WorkspaceBlocked && preparing.HoldsTurns(), true)
	got = preparing

	// Updates go from the version read, to a later time.
	ready := got
	ready.State, ready.Path, ready.ErrorCode, ready.UpdatedAt = store.SessionLedgerReady, "/state/circus/ledger/s1", 0, t0+2
	ok, err = s.UpdateSessionLedger(ctx, ready, t0+1)
	must(t, err)
	mustEqual(t, "update", ok, true)
	ok, err = s.UpdateSessionLedger(ctx, ready, t0+1)
	must(t, err)
	mustEqual(t, "stale update", ok, false)
	mustIs(t, errOf(s.UpdateSessionLedger(ctx, ready, t0+2)), store.ErrInvalid)
	missing := ready
	missing.SessionID, missing.UpdatedAt = "s2", t0+5
	mustIs(t, errOf(s.UpdateSessionLedger(ctx, missing, t0+2)), store.ErrNotFound)
	sess, err = s.GetSession(ctx, "s1", t0)
	must(t, err)
	mustEqual(t, "ready does not hold", sess.WorkspaceBlocked, false)
	rows, err := s.ListSessionLedgers(ctx, store.SessionLedgerFilter{Limit: 10})
	must(t, err)
	mustEqual(t, "a ready snapshot has no work", len(rows), 0)

	// A turn's end makes it due; a collection's outcome is kept whole.
	fence := acquire(t, s, "s1", f.worker.ID, "b1", t0+2)
	rep := report(t, s, fence, store.TurnReport{LocalSeq: 1, Phase: store.TurnEnded, At: t0 + 3})
	mustEqual(t, "due", rep.LedgerDue, true)
	got, err = s.GetSessionLedger(ctx, "s1")
	must(t, err)
	mustEqual(t, "due at", got.DueAt, t0+3)
	mustEqual(t, "moved", got.UpdatedAt > ready.UpdatedAt, true)
	rows, err = s.ListSessionLedgers(ctx, store.SessionLedgerFilter{DeviceID: f.device.ID, Limit: 10})
	must(t, err)
	mustEqual(t, "due listed", len(rows), 1)
	collected := got
	collected.DueAt, collected.CollectedAt, collected.UpdatedAt = 0, t0+4, got.UpdatedAt+1
	collected.Known = []store.LedgerKnownChange{{Path: "D?a-x.md", SHA256: strings.Repeat("a", 64)}, {Path: "D1-y.md", Deleted: true}}
	collected.Problems = []store.LedgerProblem{{Path: "notes.txt", Message: "unexpected file"}}
	collected.Warnings = []store.LedgerProblem{{Path: "D?a-x.md", Message: "refers to D?b"}}
	collected.ChangeCount, collected.ProblemCount, collected.NotedHash = 2, 1, "h"
	bad = collected
	bad.Known = []store.LedgerKnownChange{{Path: "x", SHA256: "abc"}}
	mustIs(t, errOf(s.UpdateSessionLedger(ctx, bad, got.UpdatedAt)), store.ErrInvalid)
	ok, err = s.UpdateSessionLedger(ctx, collected, got.UpdatedAt)
	must(t, err)
	mustEqual(t, "collected", ok, true)
	got, err = s.GetSessionLedger(ctx, "s1")
	must(t, err)
	mustEqual(t, "collected row", got, collected)
	statuses, err := s.ListSessionLedgerStatuses(ctx, []string{"s1", "s9"})
	must(t, err)
	mustEqual(t, "statuses", len(statuses), 1)
	mustEqual(t, "status", statuses["s1"], collected)
}

// testSessionLedgerChangesets: a session has at most one open changeset;
// merging or discarding it leaves its snapshot owing a rebase or a reset,
// with a durable LEDGER event, atomically.
func testSessionLedgerChangesets(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	const project = "session-project"
	createSession(t, s, f, "s1")
	l := newSessionLedger("s1", f)
	l.State, l.Path = store.SessionLedgerReady, "/snap"
	must(t, s.CreateSessionLedger(ctx, l))

	c := newChangeset("cs1", project, t0)
	c.OpenerWorkerID, c.SessionID = "", "s1"
	must(t, s.CreateLedgerChangeset(ctx, c))
	second := newChangeset("cs2", project, t0)
	second.OpenerWorkerID, second.SessionID = "", "s1"
	mustIs(t, s.CreateLedgerChangeset(ctx, second), store.ErrExists)
	opener := second
	opener.OpenerWorkerID = "w1"
	mustIs(t, s.CreateLedgerChangeset(ctx, opener), store.ErrInvalid)
	open, err := s.GetOpenSessionChangeset(ctx, "s1")
	must(t, err)
	mustEqual(t, "open", open, c)
	got, err := s.GetSessionLedger(ctx, "s1")
	must(t, err)
	mustEqual(t, "changeset with the row", got.ChangesetID, "cs1")
	page, _, err := s.ListLedgerChangesets(ctx, store.LedgerChangesetFilter{ProjectID: project, SessionID: "s1", Limit: 10})
	must(t, err)
	mustEqual(t, "by session", ids(page), []string{"cs1"})
	page, _, err = s.ListLedgerChangesets(ctx, store.LedgerChangesetFilter{ProjectID: project, SessionID: "s2", Limit: 10})
	must(t, err)
	mustEqual(t, "other session", len(page), 0)

	merge := store.LedgerMerge{Commit: strings.Repeat("b", 40), Numbers: map[string]uint32{"a": 7}}
	ok, err := s.PrepareLedgerMerge(ctx, "cs1", merge, t0+1)
	must(t, err)
	mustEqual(t, "prepare", ok, true)
	ok, err = s.FinishLedgerMerge(ctx, "cs1", merge.Commit, t0+2)
	must(t, err)
	mustEqual(t, "finish", ok, true)
	got, err = s.GetSessionLedger(ctx, "s1")
	must(t, err)
	mustEqual(t, "rebase owed", got.Pending, store.SessionLedgerRebase)
	mustEqual(t, "rebase of", got.PendingChangeset, "cs1")
	mustEqual(t, "no open changeset", got.ChangesetID, "")
	mustEqual(t, "moved", got.UpdatedAt >= t0+2, true)
	page2, err := s.ReadEvents(ctx, store.EventFilter{SessionID: "s1", Limit: 10, MaxBytes: 1 << 20})
	must(t, err)
	mustEqual(t, "event", len(page2.Events), 1)
	e := page2.Events[0]
	mustEqual(t, "event kind", e.Kind, store.EventKindLedger)
	mustEqual(t, "event payload", string(e.Payload), `{"event":"merged","changeset_id":"cs1","numbers":{"D?a":7}}`)
	mustEqual(t, "event time", e.At, got.UpdatedAt)

	// A later changeset is allowed once none is open; its discard owes a
	// reset with the reason.
	must(t, s.CreateLedgerChangeset(ctx, second))
	ok, err = s.DiscardLedgerChangeset(ctx, "cs2", "not <now>", true, t0+3)
	must(t, err)
	mustEqual(t, "discard", ok, true)
	got, err = s.GetSessionLedger(ctx, "s1")
	must(t, err)
	mustEqual(t, "reset owed", [3]string{string(got.Pending), got.PendingChangeset, got.PendingReason},
		[3]string{string(store.SessionLedgerReset), "cs2", "not <now>"})
	page2, err = s.ReadEvents(ctx, store.EventFilter{SessionID: "s1", AfterSeq: e.Seq, Limit: 10, MaxBytes: 1 << 20})
	must(t, err)
	mustEqual(t, "discard event", string(page2.Events[0].Payload), `{"event":"discarded","changeset_id":"cs2","reason":"not <now>"}`)
	sess, err := s.GetSession(ctx, "s1", t0)
	must(t, err)
	mustEqual(t, "no activity from ledger events", sess.LastActivityAt, t0)

	// A long reason keeps the event within its bound.
	createSession(t, s, f, "s3")
	l3 := newSessionLedger("s3", f)
	must(t, s.CreateSessionLedger(ctx, l3))
	third := newChangeset("cs3", project, t0)
	third.OpenerWorkerID, third.SessionID = "", "s3"
	must(t, s.CreateLedgerChangeset(ctx, third))
	_, err = s.DiscardLedgerChangeset(ctx, "cs3", strings.Repeat("\"", store.MaxLedgerDiscardReasonBytes), true, t0+4)
	must(t, err)
	page2, err = s.ReadEvents(ctx, store.EventFilter{SessionID: "s3", Limit: 10, MaxBytes: 1 << 20})
	must(t, err)
	mustEqual(t, "bounded event", len(page2.Events[0].Payload) <= 4096, true)
}

// testSessionLedgerOpenAtomic: concurrent opens of a session's changeset
// leave exactly one open.
func testSessionLedgerOpenAtomic(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	createSession(t, s, f, "s1")
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Go(func() {
			c := newChangeset("cs"+string(rune('a'+i)), "session-project", t0)
			c.OpenerWorkerID, c.SessionID = "", "s1"
			errs[i] = s.CreateLedgerChangeset(ctx, c)
		})
	}
	wg.Wait()
	created := 0
	for _, err := range errs {
		if err == nil {
			created++
		} else {
			mustIs(t, err, store.ErrExists)
		}
	}
	mustEqual(t, "open changesets of the session", created, 1)
}

// testSessionLedgerRemovals: deleting a session turns its snapshot into a
// removal for its device; removals are found by device, forgotten, and
// dropped once old.
func testSessionLedgerRemovals(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	createSession(t, s, f, "s1")
	createSession(t, s, f, "s2")
	must(t, s.CreateSessionLedger(ctx, newSessionLedger("s1", f)))
	for _, id := range []string{"s1", "s2"} {
		_, _, _, err := s.StopSession(ctx, id, t0+1, "USER")
		must(t, err)
		must(t, s.DeleteSession(ctx, id, t0+2))
	}
	_, err := s.GetSessionLedger(ctx, "s1")
	mustIs(t, err, store.ErrNotFound)
	r, err := s.GetLedgerSnapshotRemoval(ctx, "s1")
	must(t, err)
	mustEqual(t, "removal", r, store.LedgerSnapshotRemoval{SessionID: "s1", UserID: f.user.ID, DeviceID: f.device.ID, CreatedAt: t0 + 2})
	_, err = s.GetLedgerSnapshotRemoval(ctx, "s2")
	mustIs(t, err, store.ErrNotFound)
	list, err := s.ListLedgerSnapshotRemovals(ctx, f.device.ID, "", 10)
	must(t, err)
	mustEqual(t, "by device", len(list), 1)
	list, err = s.ListLedgerSnapshotRemovals(ctx, "other", "", 10)
	must(t, err)
	mustEqual(t, "other device", len(list), 0)
	n, err := s.PruneLedgerSnapshotRemovals(ctx, t0+2, 10)
	must(t, err)
	mustEqual(t, "not old yet", n, 0)
	n, err = s.PruneLedgerSnapshotRemovals(ctx, t0+3, 10)
	must(t, err)
	mustEqual(t, "dropped", n, 1)
	must(t, s.DeleteLedgerSnapshotRemoval(ctx, "s1"))
}

// testNoteInputs: a note input names its category, starts nothing the
// user wrote (no title, no activity), and is taken like other inputs.
func testNoteInputs(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	createSession(t, s, f, "s1")
	in := store.Input{InputID: "i1", SessionID: "s1", ClientMsgID: "note-1", Kind: store.InputNote, Note: "ledger_problems",
		Text: "fix it", Priority: store.InputNormal, CreatedAt: t0 + 5}
	for _, bad := range []func(*store.Input){
		func(in *store.Input) { in.Note = "" },
		func(in *store.Input) { in.Note = "Bad-Note" },
		func(in *store.Input) { in.Note = strings.Repeat("a", store.MaxInputNoteBytes+1) },
		func(in *store.Input) { in.Kind = store.InputUser },
	} {
		x := in
		bad(&x)
		mustIs(t, errOf(s.SubmitInput(ctx, x)), store.ErrInvalid)
	}
	res, err := s.SubmitInput(ctx, in)
	must(t, err)
	mustEqual(t, "created", res.Created, true)
	mustEqual(t, "queued", res.Session.State, store.SessionQueued)
	mustEqual(t, "no title", res.Session.Title, "")
	mustEqual(t, "no activity", res.Session.LastActivityAt, t0)
	other := in
	other.Note = "ledger_merged"
	mustIs(t, errOf(s.SubmitInput(ctx, other)), store.ErrExists)
	fence := acquire(t, s, "s1", f.worker.ID, "b1", t0+6)
	taken, err := s.TakeInputs(ctx, fence)
	must(t, err)
	mustEqual(t, "taken", len(taken), 1)
	mustEqual(t, "kind", taken[0].Kind, store.InputNote)
	mustEqual(t, "note", taken[0].Note, "ledger_problems")
}

// testNoteOrderAndLimits: notes are taken before the user's inputs that
// arrived before them, may exceed the open inputs' bound by
// MaxNoteOverflow, and are counted since the user's last input.
func testNoteOrderAndLimits(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	createSession(t, s, f, "s1")
	note := func(n int) store.Input {
		return store.Input{InputID: fmt.Sprintf("note-%d", n), SessionID: "s1", ClientMsgID: fmt.Sprintf("note-%d", n), Kind: store.InputNote,
			Note: "ledger_merged", Text: "merged", Priority: store.InputNormal, CreatedAt: t0 + 100*int64(n)}
	}
	count := func() int {
		t.Helper()
		n, err := s.CountNotesSinceUserInput(ctx, "s1")
		must(t, err)
		return n
	}
	mustEqual(t, "no notes", count(), 0)
	submit(t, s, note(0))
	mustEqual(t, "a note without a user input", count(), 1)
	for i := 1; i <= store.MaxOpenInputs-1; i++ {
		submit(t, s, newInput("s1", i))
	}
	mustEqual(t, "after the user's input", count(), 0)
	mustIs(t, errOf(s.SubmitInput(ctx, newInput("s1", store.MaxOpenInputs))), store.ErrLimit)
	for i := 1; i <= store.MaxNoteOverflow; i++ {
		submit(t, s, note(i))
	}
	mustIs(t, errOf(s.SubmitInput(ctx, note(store.MaxNoteOverflow+1))), store.ErrLimit)
	mustEqual(t, "notes since", count(), store.MaxNoteOverflow)
	fence := acquire(t, s, "s1", f.worker.ID, "b1", t0+200)
	taken, err := s.TakeInputs(ctx, fence)
	must(t, err)
	mustEqual(t, "taken", len(taken), store.MaxOpenInputs+store.MaxNoteOverflow)
	for i, in := range taken {
		mustEqual(t, fmt.Sprintf("input %d is a note", i), in.Kind == store.InputNote, i <= store.MaxNoteOverflow)
	}
	mustEqual(t, "notes in order", taken[0].InputID+","+taken[1].InputID, "note-0,note-1")
}

// testDeferredNotes: a deferred note starts no turn (the session stays
// idle and a holder takes nothing), is not counted as a note that started
// one, and is taken, before it, with the next input that starts a turn;
// given back with a lost lease, it is held again with that input.
func testDeferredNotes(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	createSession(t, s, f, "s1")
	note := func(id string, deferred bool, at int64) store.Input {
		return store.Input{InputID: id, SessionID: "s1", ClientMsgID: id, Kind: store.InputNote, Note: "ledger_merged",
			Text: "merged", Priority: store.InputNormal, CreatedAt: at, Deferred: deferred}
	}
	ids := func(list []store.Input) string {
		var out []string
		for _, in := range list {
			out = append(out, in.InputID)
		}
		return strings.Join(out, ",")
	}
	take := func(f store.Fence) []store.Input {
		t.Helper()
		taken, err := s.TakeInputs(ctx, f)
		must(t, err)
		return taken
	}
	user := newInput("s1", 1)
	user.Deferred = true
	mustIs(t, errOf(s.SubmitInput(ctx, user)), store.ErrInvalid)

	res := submit(t, s, note("held-1", true, t0+5))
	mustEqual(t, "created", res.Created, true)
	mustEqual(t, "stays idle", res.Session.State, store.SessionIdle)
	mustEqual(t, "deferred", res.Input.Deferred, true)
	mustIs(t, errOf(s.SubmitInput(ctx, note("held-1", false, t0+5))), store.ErrExists)
	n, err := s.CountNotesSinceUserInput(ctx, "s1")
	must(t, err)
	mustEqual(t, "deferred notes are not counted", n, 0)
	fence := acquire(t, s, "s1", f.worker.ID, "b1", t0+6)
	mustEqual(t, "nothing to take", ids(take(fence)), "")

	// The user's input takes the held note with it, the note first.
	submit(t, s, newInput("s1", 10))
	taken := take(fence)
	mustEqual(t, "taken together", ids(taken), "held-1,s1-in10")
	mustEqual(t, "taken deferred", taken[0].Deferred, true)
	// A note deferred while they are taken waits for the next input.
	submit(t, s, note("held-2", true, t0+20))
	mustEqual(t, "a retry", ids(take(fence)), "held-1,s1-in10")

	// A lost lease gives the taken inputs back; the session is queued for
	// the user's input, not for the notes, which stay deferred.
	loss, err := s.ReleaseLease(ctx, fence)
	must(t, err)
	mustEqual(t, "released", loss.Released.SessionID, "s1")
	sess, err := s.GetSession(ctx, "s1", t0+30)
	must(t, err)
	mustEqual(t, "queued", sess.State, store.SessionQueued)
	fence = acquire(t, s, "s1", f.worker.ID, "b2", t0+40)
	taken = take(fence)
	mustEqual(t, "taken again", ids(taken), "held-1,held-2,s1-in10")
	mustEqual(t, "still deferred", taken[0].Deferred && taken[1].Deferred, true)

	// A note that starts a turn takes a held one with it too.
	createSession(t, s, f, "s2")
	held := note("s2-held", true, t0+50)
	held.SessionID = "s2"
	problems := note("s2-problems", false, t0+60)
	problems.SessionID, problems.Note = "s2", "ledger_problems"
	submit(t, s, held)
	res = submit(t, s, problems)
	mustEqual(t, "queued by the note that starts a turn", res.Session.State, store.SessionQueued)
	n, err = s.CountNotesSinceUserInput(ctx, "s2")
	must(t, err)
	mustEqual(t, "counted", n, 1)
	other := acquire(t, s, "s2", f.worker.ID, "b3", t0+70)
	mustEqual(t, "both notes", ids(take(other)), "s2-held,s2-problems")
}

// testDeferredNotesAlone: a lost lease, or a settle, with only deferred
// notes pending leaves the session idle: they are no work for a worker.
// A deferred note taken with an input the CLI got, itself missing from
// the transcript, can no longer go before that input: it comes back as a
// note that starts a turn.
func testDeferredNotesAlone(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	createSession(t, s, f, "s1")
	note := func(id string, at int64) store.Input {
		return store.Input{InputID: id, SessionID: "s1", ClientMsgID: id, Kind: store.InputNote, Note: "ledger_merged",
			Text: "merged", Priority: store.InputNormal, CreatedAt: at, Deferred: true}
	}
	fence := acquire(t, s, "s1", f.worker.ID, "b1", t0+1)
	submit(t, s, note("held-1", t0+2))
	_, err := s.ReleaseLease(ctx, fence)
	must(t, err)
	sess, err := s.GetSession(ctx, "s1", t0+3)
	must(t, err)
	mustEqual(t, "idle with a held note", sess.State, store.SessionIdle)
	settled, err := s.SettleSession(ctx, "s1", t0+4)
	must(t, err)
	mustEqual(t, "settled idle", settled.Session.State, store.SessionIdle)

	fence = acquire(t, s, "s1", f.worker.ID, "b2", t0+5)
	submit(t, s, newInput("s1", 6))
	taken, err := s.TakeInputs(ctx, fence)
	must(t, err)
	mustEqual(t, "taken together", len(taken), 2)
	appendEntries(t, s, fence, "", 1, entry("s1-in6", `{"n":1}`))
	_, err = s.ReleaseLease(ctx, fence)
	must(t, err)
	sess, err = s.GetSession(ctx, "s1", t0+7)
	must(t, err)
	mustEqual(t, "queued for the note", sess.State, store.SessionQueued)
	fence = acquire(t, s, "s1", f.worker.ID, "b3", t0+8)
	taken, err = s.TakeInputs(ctx, fence)
	must(t, err)
	mustEqual(t, "the note alone", len(taken), 1)
	mustEqual(t, "no longer deferred", taken[0].InputID+" "+fmt.Sprint(taken[0].Deferred), "held-1 false")
}

// testDeferredNoteBound: deferred notes count towards no bound of the
// open inputs, so the user's input and an approval's retry are accepted
// with them at their own bound; FoldDeferredNotes replaces them at that
// bound with one note in the oldest one's place.
func testDeferredNoteBound(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	createSession(t, s, f, "s1")
	held := func(id string, at int64) store.Input {
		return store.Input{InputID: id, SessionID: "s1", ClientMsgID: id, Kind: store.InputNote, Note: "ledger_merged",
			Text: id, Priority: store.InputNormal, CreatedAt: at, Deferred: true}
	}
	fold := func(at int64) int {
		t.Helper()
		n, err := s.FoldDeferredNotes(ctx, held(fmt.Sprintf("fold-%d", at-t0), at))
		must(t, err)
		return n
	}
	for i := range store.MaxDeferredNotes {
		mustEqual(t, "below the bound", fold(t0+100+int64(i)), 0)
		submit(t, s, held(fmt.Sprintf("held-%d", i), t0+10+int64(i)))
	}
	// At the bound of held notes, every bound of the open inputs is
	// still free.
	for i := 1; i <= store.MaxOpenInputs-1; i++ {
		submit(t, s, newInput("s1", 1000+i))
	}
	approval := newInput("s1", 2000)
	approval.Kind = store.InputApproval
	submit(t, s, approval)
	mustIs(t, errOf(s.SubmitInput(ctx, newInput("s1", 3000))), store.ErrLimit)
	submit(t, s, held("beyond", t0+500))
	mustEqual(t, "folded", fold(t0+600), store.MaxDeferredNotes+1)
	mustIs(t, errOf(s.FoldDeferredNotes(ctx, store.Input{InputID: "x", SessionID: "s1", ClientMsgID: "x", Kind: store.InputNote,
		Note: "ledger_merged", Text: "x", Priority: store.InputNormal, CreatedAt: t0})), store.ErrInvalid)
	submit(t, s, held("newest", t0+700))
	fence := acquire(t, s, "s1", f.worker.ID, "b1", t0+800)
	taken, err := s.TakeInputs(ctx, fence)
	must(t, err)
	var notes []string
	for _, in := range taken {
		if in.Kind == store.InputNote {
			notes = append(notes, fmt.Sprintf("%s@%d", in.InputID, in.CreatedAt-t0))
		}
	}
	// The replacement takes the oldest one's place, before the newest.
	mustEqual(t, "held notes", strings.Join(notes, ","), "fold-600@10,newest@700")
	mustEqual(t, "all taken", len(taken), 2+store.MaxOpenInputs)
}

// testSessionLedgerHolds: a pending rebase or reset holds the session's
// turns unless problems stop it, a holder that keeps its lease included:
// it takes no inputs meanwhile. A turn that a new turn closes makes a
// ready snapshot due.
func testSessionLedgerHolds(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	createSession(t, s, f, "s1")
	l := newSessionLedger("s1", f)
	l.State, l.Path = store.SessionLedgerReady, "/snap"
	must(t, s.CreateSessionLedger(ctx, l))
	held := func() bool {
		t.Helper()
		sess, err := s.GetSession(ctx, "s1", t0)
		must(t, err)
		got, err := s.GetSessionLedger(ctx, "s1")
		must(t, err)
		mustEqual(t, "the rule", got.HoldsTurns(), sess.WorkspaceBlocked)
		return sess.WorkspaceBlocked
	}
	mustEqual(t, "ready", held(), false)
	fence := acquire(t, s, "s1", f.worker.ID, "b1", t0)
	c := newChangeset("cs1", "session-project", t0)
	c.OpenerWorkerID, c.SessionID = "", "s1"
	must(t, s.CreateLedgerChangeset(ctx, c))
	_, err := s.DiscardLedgerChangeset(ctx, "cs1", "no", true, t0+1)
	must(t, err)
	mustEqual(t, "reset owed", held(), true)
	submit(t, s, newInput("s1", 1))
	taken, err := s.TakeInputs(ctx, fence)
	must(t, err)
	mustEqual(t, "taken while held", len(taken), 0)
	got, err := s.GetSessionLedger(ctx, "s1")
	must(t, err)
	stopped := got
	stopped.ProblemCount, stopped.Problems, stopped.UpdatedAt = 1, []store.LedgerProblem{{Path: "x", Message: "y"}}, got.UpdatedAt+1
	ok, err := s.UpdateSessionLedger(ctx, stopped, got.UpdatedAt)
	must(t, err)
	mustEqual(t, "updated", ok, true)
	mustEqual(t, "stopped by problems", held(), false)
	taken, err = s.TakeInputs(ctx, fence)
	must(t, err)
	mustEqual(t, "taken once not held", len(taken), 1)

	// STARTED 2 closes turn 1: the snapshot is due.
	createSession(t, s, f, "s2")
	l2 := newSessionLedger("s2", f)
	l2.State, l2.Path = store.SessionLedgerReady, "/snap2"
	must(t, s.CreateSessionLedger(ctx, l2))
	fence2 := acquire(t, s, "s2", f.worker.ID, "b2", t0+2)
	report(t, s, fence2, store.TurnReport{LocalSeq: 1, Phase: store.TurnStarted, At: t0 + 3})
	report(t, s, fence2, store.TurnReport{LocalSeq: 2, Phase: store.TurnStarted, At: t0 + 4})
	got, err = s.GetSessionLedger(ctx, "s2")
	must(t, err)
	mustEqual(t, "due after an interrupted turn", got.DueAt, t0+4)
}

// testOrphanSessionChangesets: an open changeset whose session is gone is
// listed; others are not.
func testOrphanSessionChangesets(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	for _, id := range []string{"s1", "s2"} {
		createSession(t, s, f, id)
		c := newChangeset("cs-"+id, "session-project", t0)
		c.OpenerWorkerID, c.SessionID = "", id
		must(t, s.CreateLedgerChangeset(ctx, c))
	}
	must(t, s.CreateLedgerChangeset(ctx, newChangeset("cs-worker", "session-project", t0)))
	_, _, _, err := s.StopSession(ctx, "s1", t0+1, "USER")
	must(t, err)
	must(t, s.DeleteSession(ctx, "s1", t0+2))
	orphans, err := s.ListOrphanSessionChangesets(ctx, "", 10)
	must(t, err)
	mustEqual(t, "orphans", ids(orphans), []string{"cs-s1"})
	orphans, err = s.ListOrphanSessionChangesets(ctx, "cs-s1", 10)
	must(t, err)
	mustEqual(t, "after the cursor", len(orphans), 0)
	_, err = s.DiscardLedgerChangeset(ctx, "cs-s1", "session deleted", true, t0+3)
	must(t, err)
	orphans, err = s.ListOrphanSessionChangesets(ctx, "", 10)
	must(t, err)
	mustEqual(t, "after the discard", len(orphans), 0)
}

// testSessionChangesetTurnRefusal: a merge or a discard of a session's
// changeset is refused (ErrTurnRunning) while its session has inputs taken
// for a turn, or runs one; once the turn ended and persisted its inputs,
// or the session is gone, it is allowed.
func testSessionChangesetTurnRefusal(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	createSession(t, s, f, "s1")
	l := newSessionLedger("s1", f)
	l.State, l.Path = store.SessionLedgerReady, "/snap"
	must(t, s.CreateSessionLedger(ctx, l))
	c := newChangeset("cs1", "session-project", t0)
	c.OpenerWorkerID, c.SessionID = "", "s1"
	must(t, s.CreateLedgerChangeset(ctx, c))
	merge := store.LedgerMerge{Commit: strings.Repeat("b", 40), Numbers: map[string]uint32{"a": 7}}
	refused := func(what string) {
		t.Helper()
		mustIs(t, errOf(s.DiscardLedgerChangeset(ctx, "cs1", "no", true, t0+5)), store.ErrTurnRunning)
		mustIs(t, errOf(s.PrepareLedgerMerge(ctx, "cs1", merge, t0+5)), store.ErrTurnRunning)
		got, err := s.GetLedgerChangeset(ctx, "cs1")
		must(t, err)
		mustEqual(t, what+": still open without a merge", got.State == store.LedgerChangesetOpen && got.Merge.Commit == "", true)
	}
	fence := acquire(t, s, "s1", f.worker.ID, "b1", t0+1)
	in := newInput("s1", 1)
	submit(t, s, in)
	taken, err := s.TakeInputs(ctx, fence)
	must(t, err)
	mustEqual(t, "taken", len(taken), 1)
	refused("inputs taken")
	report(t, s, fence, store.TurnReport{LocalSeq: 1, Phase: store.TurnStarted, At: t0 + 2})
	refused("turn running")
	report(t, s, fence, store.TurnReport{LocalSeq: 1, Phase: store.TurnEnded, At: t0 + 3, Persisted: store.TurnPersisted{InputIDs: []string{in.InputID}}})
	// A turn the CLI starts on its own takes no input.
	report(t, s, fence, store.TurnReport{LocalSeq: 2, Phase: store.TurnStarted, Origin: store.TurnFromCLI, At: t0 + 3})
	refused("turn of the CLI running")
	report(t, s, fence, store.TurnReport{LocalSeq: 2, Phase: store.TurnEnded, Origin: store.TurnFromCLI, At: t0 + 3})
	ok, err := s.PrepareLedgerMerge(ctx, "cs1", merge, t0+4)
	must(t, err)
	mustEqual(t, "merge prepared after the turn", ok, true)

	// A deleted session's changeset is discarded whatever its inputs were.
	createSession(t, s, f, "s2")
	c2 := newChangeset("cs2", "session-project", t0)
	c2.OpenerWorkerID, c2.SessionID = "", "s2"
	must(t, s.CreateLedgerChangeset(ctx, c2))
	_, _, _, err = s.StopSession(ctx, "s2", t0+5, "USER")
	must(t, err)
	must(t, s.DeleteSession(ctx, "s2", t0+6))
	ok, err = s.DiscardLedgerChangeset(ctx, "cs2", "session deleted", true, t0+7)
	must(t, err)
	mustEqual(t, "orphan discarded", ok, true)
}

// testLedgerCalls: terminal snapshot calls are pruned; others stay.
func testLedgerCalls(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	done := newCall("lg-1", f)
	done.SessionID, done.Tool = "ledger-s1", "collect_ledger"
	done.State, done.FinishedAt = store.CallOffline, t0
	must(t, s.CreateCall(ctx, done))
	active := newCall("lg-2", f)
	active.SessionID, active.Tool = "ledger-s1", "collect_ledger"
	must(t, s.CreateCall(ctx, active))
	other := newCall("other", f)
	other.SessionID, other.State, other.FinishedAt = "ledger-s1", store.CallOffline, t0
	must(t, s.CreateCall(ctx, other))
	n, err := s.PruneLedgerCalls(ctx, 10)
	must(t, err)
	mustEqual(t, "pruned", n, 1)
	_, err = s.GetCall(ctx, "lg-2")
	must(t, err)
	_, err = s.GetCall(ctx, "other")
	must(t, err)
}

// queuedCommand is the transcript entry Claude Code writes for a message
// it absorbed into a running turn, in the shape the recorded SDK sequence
// shows (worker/test/fixtures/sdk/merged.json): an attachment with a uuid
// of its own whose source_uuid is the pushed message's uuid.
func queuedCommand(uuid, source string) store.TranscriptEntry {
	return entry(uuid, `{"parentUuid":"p","isSidechain":false,"attachment":{"type":"queued_command","prompt":"also this",`+
		`"source_uuid":"`+source+`","delivery_id":"d","commandMode":"prompt","timestamp":"2026-11-21T16:48:51.485Z"},`+
		`"type":"attachment","uuid":"`+uuid+`","timestamp":"2026-11-21T16:48:51.485Z"}`)
}

// testAbsorbedInputs: on a lost lease, an input the CLI absorbed into a
// running turn (a queued_command attachment naming it) counts as got: it
// is persisted, not given again, and a held note absorbed so stays
// delivered rather than coming back as a note that starts a turn.
func testAbsorbedInputs(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	createSession(t, s, f, "s1")
	if got := store.QueuedCommandSource(queuedCommand("a1", "s1-in2").JSON); got != "s1-in2" {
		t.Fatalf("source %q", got)
	}
	for _, other := range []string{`{"type":"user","uuid":"x","message":{"content":"queued_command"}}`,
		`{"type":"attachment","attachment":{"type":"queued_command"}}`, `{"type":"attachment","attachment":{"type":"other","source_uuid":"x"}}`, `queued_command`} {
		if got := store.QueuedCommandSource([]byte(other)); got != "" {
			t.Fatalf("source of %s: %q", other, got)
		}
	}

	// A user input starts a turn; a second one is absorbed into it.
	fence := acquire(t, s, "s1", f.worker.ID, "b1", t0+1)
	submit(t, s, newInput("s1", 1))
	submit(t, s, newInput("s1", 2))
	taken, err := s.TakeInputs(ctx, fence)
	must(t, err)
	mustEqual(t, "taken", len(taken), 2)
	appendEntries(t, s, fence, "", 1, entry("s1-in1", `{"type":"user"}`), queuedCommand("a1", "s1-in2"))
	_, err = s.ReleaseLease(ctx, fence)
	must(t, err)
	sess, err := s.GetSession(ctx, "s1", t0+3)
	must(t, err)
	mustEqual(t, "nothing to give again", sess.State, store.SessionIdle)
	fence = acquire(t, s, "s1", f.worker.ID, "b2", t0+4)
	taken, err = s.TakeInputs(ctx, fence)
	must(t, err)
	mustEqual(t, "taken again", len(taken), 0)

	// A held note absorbed into a running turn, the user's input that
	// took it starting the next one.
	submit(t, s, store.Input{InputID: "held-1", SessionID: "s1", ClientMsgID: "held-1", Kind: store.InputNote, Note: "ledger_merged",
		Text: "merged", Priority: store.InputNormal, CreatedAt: t0 + 5, Deferred: true})
	submit(t, s, newInput("s1", 6))
	taken, err = s.TakeInputs(ctx, fence)
	must(t, err)
	mustEqual(t, "taken together", len(taken), 2)
	appendEntries(t, s, fence, "", 1, queuedCommand("a2", "held-1"), entry("s1-in6", `{"type":"user"}`))
	_, err = s.ReleaseLease(ctx, fence)
	must(t, err)
	fence = acquire(t, s, "s1", f.worker.ID, "b3", t0+7)
	taken, err = s.TakeInputs(ctx, fence)
	must(t, err)
	mustEqual(t, "the note is not given again", len(taken), 0)
}

// testFoldKeepsTakenNotes: the fold replaces only the pending held notes;
// those taken with a running turn's input stay in flight.
func testFoldKeepsTakenNotes(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	createSession(t, s, f, "s1")
	held := func(id string, at int64) store.Input {
		return store.Input{InputID: id, SessionID: "s1", ClientMsgID: id, Kind: store.InputNote, Note: "ledger_merged",
			Text: id, Priority: store.InputNormal, CreatedAt: at, Deferred: true}
	}
	for i := range store.MaxDeferredNotes {
		submit(t, s, held(fmt.Sprintf("taken-%d", i), t0+1+int64(i)))
	}
	submit(t, s, newInput("s1", 100))
	fence := acquire(t, s, "s1", f.worker.ID, "b1", t0+200)
	taken, err := s.TakeInputs(ctx, fence)
	must(t, err)
	mustEqual(t, "in flight", len(taken), store.MaxDeferredNotes+1)
	for i := range store.MaxDeferredNotes {
		submit(t, s, held(fmt.Sprintf("pending-%d", i), t0+300+int64(i)))
	}
	n, err := s.FoldDeferredNotes(ctx, held("fold", t0+400))
	must(t, err)
	mustEqual(t, "folded the pending ones", n, store.MaxDeferredNotes)
	taken, err = s.TakeInputs(ctx, fence)
	must(t, err)
	mustEqual(t, "the ones in flight remain", len(taken), store.MaxDeferredNotes+1)
	_, err = s.ReleaseLease(ctx, fence)
	must(t, err)
	fence = acquire(t, s, "s1", f.worker.ID, "b2", t0+500)
	taken, err = s.TakeInputs(ctx, fence)
	must(t, err)
	var ids []string
	for _, in := range taken {
		ids = append(ids, in.InputID)
	}
	mustEqual(t, "after the loss", strings.Join(ids, ","),
		"taken-0,taken-1,taken-2,taken-3,taken-4,taken-5,taken-6,taken-7,fold,s1-in100")
}
