package storetest

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/EricWvi/circus/internal/store"
)

func entryBodies(es []store.TranscriptEntry) []string {
	var out []string
	for _, e := range es {
		out = append(out, string(e.JSON))
	}
	return out
}

// testTranscriptAppend: batch_seq dedup and gap refusal per (epoch,
// subpath), uuid dedup within a subpath, charges, seqs, pages and subpaths.
func testTranscriptAppend(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	createSession(t, s, f, "s1")
	fence := acquire(t, s, "s1", "w1", "b1", t0)
	ch := store.TranscriptCharge
	mustEqual(t, "charge: JSON, uuid and 64 B", ch(entry("u1", `{"a":1}`)), int64(7+2+64))
	a, bb, c, d := entry("u1", `{"a":1}`), entry("", `{"b":2}`), entry("u2", `{"c":3}`), entry("u4", `{"d":4}`)

	r := appendEntries(t, s, fence, "", 1, a, bb, entry("u1", `{"dup":1}`))
	mustEqual(t, "first batch", r, store.TranscriptAppended{Inserted: 2, Skipped: 1, LastSeq: 2, SessionBytes: ch(a) + ch(bb)})
	// The same batch again is a duplicate: nothing written.
	r = appendEntries(t, s, fence, "", 1, entry("u9", `{"new":1}`))
	mustEqual(t, "repeated batch", r, store.TranscriptAppended{Deduplicated: true, LastSeq: 2, SessionBytes: ch(a) + ch(bb)})
	// A gap means a batch is missing: refused, nothing written.
	_, err := s.AppendTranscript(ctx, fence, store.TranscriptBatch{BatchSeq: 3, Entries: []store.TranscriptEntry{entry("u9", "{}")}}, quota)
	mustIs(t, err, store.ErrInvalid)
	// The next batch; uuids already present are skipped.
	r = appendEntries(t, s, fence, "", 2, entry("u1", `{"again":1}`), c)
	mustEqual(t, "next batch", r, store.TranscriptAppended{Inserted: 1, Skipped: 1, LastSeq: 3, SessionBytes: ch(a) + ch(bb) + ch(c)})
	// Another subpath has its own seqs, cursor and uuid space.
	sub := entry("u1", `{"sub":1}`)
	r = appendEntries(t, s, fence, "agent-x", 1, sub)
	mustEqual(t, "subpath batch", []any{r.Inserted, r.LastSeq}, []any{1, int64(1)})

	// A new epoch numbers its batches from 1 again.
	_, err = s.ReleaseLease(ctx, fence)
	must(t, err)
	fence2 := acquire(t, s, "s1", "w1", "b1", t0+1)
	_, err = s.AppendTranscript(ctx, fence2, store.TranscriptBatch{BatchSeq: 3, Entries: []store.TranscriptEntry{d}}, quota)
	mustIs(t, err, store.ErrInvalid)
	r = appendEntries(t, s, fence2, "", 1, d)
	mustEqual(t, "new epoch batch 1", []any{r.Deduplicated, r.LastSeq}, []any{false, int64(4)})

	page, err := s.LoadTranscript(ctx, fence2, store.TranscriptPageRequest{Limit: 100, MaxBytes: 1 << 20})
	must(t, err)
	mustEqual(t, "main thread", entryBodies(page.Entries), []string{`{"a":1}`, `{"b":2}`, `{"c":3}`, `{"d":4}`})
	mustEqual(t, "entry fields", page.Entries[3], store.TranscriptEntry{Subpath: "", Seq: 4, UUID: "u4", JSON: []byte(`{"d":4}`), Epoch: 2})
	mustEqual(t, "done", page.Done, true)
	// By limit, and by charge: never more than the budget except a single
	// entry.
	page, err = s.LoadTranscript(ctx, fence2, store.TranscriptPageRequest{AfterSeq: 1, Limit: 2, MaxBytes: 1 << 20})
	must(t, err)
	mustEqual(t, "limited page", []any{entryBodies(page.Entries), page.Done}, []any{[]string{`{"b":2}`, `{"c":3}`}, false})
	page, err = s.LoadTranscript(ctx, fence2, store.TranscriptPageRequest{Limit: 100, MaxBytes: ch(a) + ch(bb)})
	must(t, err)
	mustEqual(t, "budget exactly two", entryBodies(page.Entries), []string{`{"a":1}`, `{"b":2}`})
	page, err = s.LoadTranscript(ctx, fence2, store.TranscriptPageRequest{Limit: 100, MaxBytes: ch(a) + ch(bb) - 1})
	must(t, err)
	mustEqual(t, "budget one short", entryBodies(page.Entries), []string{`{"a":1}`})
	page, err = s.LoadTranscript(ctx, fence2, store.TranscriptPageRequest{Limit: 100, MaxBytes: 1})
	must(t, err)
	mustEqual(t, "oversized entry alone", []any{len(page.Entries), page.Done}, []any{1, false})
	page, err = s.LoadTranscript(ctx, fence2, store.TranscriptPageRequest{AfterSeq: 4, Limit: 100, MaxBytes: 1})
	must(t, err)
	mustEqual(t, "past the end", []any{len(page.Entries), page.Done}, []any{0, true})

	subs, err := s.ListTranscriptSubpaths(ctx, fence2)
	must(t, err)
	main := ch(a) + ch(bb) + ch(c) + ch(d)
	mustEqual(t, "subpaths", subs, []store.TranscriptSubpath{{Subpath: "", LastSeq: 4, Bytes: main}, {Subpath: "agent-x", LastSeq: 1, Bytes: ch(sub)}})
	for uuid, want := range map[string]bool{"u2": true, "u1": true, "nope": false} {
		got, err := s.HasTranscriptUUID(ctx, fence2, uuid)
		must(t, err)
		mustEqual(t, "has "+uuid, got, want)
	}
	mustEqual(t, "session bytes", getSession(t, s, "s1").TranscriptBytes, main+ch(sub))
	u, err := s.GetUser(ctx, "u1")
	must(t, err)
	mustEqual(t, "user bytes", u.TranscriptBytes, main+ch(sub))
}

// testTranscriptCaps: each charge cap accepts exactly its limit and refuses
// one byte more, writing nothing of the refused batch; the uuid counts;
// the uuid, subpath and subpath count are bounded.
func testTranscriptCaps(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	createSession(t, s, f, "s1")
	createSession(t, s, f, "s2")
	f1 := acquire(t, s, "s1", "w1", "b1", t0)
	f2 := acquire(t, s, "s2", "w1", "b1", t0)
	const unit = 10 + store.TranscriptEntryOverhead // a 10-byte entry without uuid
	q := store.TranscriptQuota{MaxEntryBytes: unit, MaxSessionBytes: 10 * unit, MaxUserBytes: 15 * unit}
	batches := map[string]int64{}
	add := func(fence store.Fence, es ...store.TranscriptEntry) error {
		batches[fence.SessionID]++
		r, err := s.AppendTranscript(ctx, fence, store.TranscriptBatch{BatchSeq: batches[fence.SessionID], Entries: es}, q)
		if err == nil && r.Full {
			// A refusal reported as a result: the stop it made commits.
			mustEqual(t, "full refusal", []any{r.Inserted, getSession(t, s, fence.SessionID).State, getSession(t, s, fence.SessionID).Reason},
				[]any{0, store.SessionStopped, "TRANSCRIPT_FULL"})
			err = r.FullReason
		}
		if err != nil {
			batches[fence.SessionID]-- // the sequence is consumed only by a success
		}
		return err
	}
	e := func(n int, uuid string) store.TranscriptEntry {
		return store.TranscriptEntry{UUID: uuid, JSON: []byte(strings.Repeat("x", n))}
	}
	tens := func(n int) []store.TranscriptEntry {
		out := make([]store.TranscriptEntry, n)
		for i := range out {
			out[i] = e(10, "")
		}
		return out
	}
	// The entry cap, with and without a uuid.
	must(t, add(f1, e(10, "")))
	mustIs(t, add(f1, e(5, ""), e(11, "")), store.ErrTranscriptFull)
	mustIs(t, add(f1, e(5, "uuid-6")), store.ErrTranscriptFull)
	// The session cap: exactly 10 units, then one byte more is refused.
	must(t, add(f1, tens(8)...))
	must(t, add(f1, e(5, "uuid5")))
	mustIs(t, add(f1, e(1, "")), store.ErrTranscriptFull)
	mustEqual(t, "s1 charge", getSession(t, s, "s1").TranscriptBytes, int64(10*unit))
	// The user cap: 15 units across both sessions.
	must(t, add(f2, tens(5)...))
	mustIs(t, add(f2, e(0+1, "")), store.ErrTranscriptFull)
	u, err := s.GetUser(ctx, "u1")
	must(t, err)
	mustEqual(t, "user charge", u.TranscriptBytes, int64(15*unit))
	page, err := s.LoadTranscript(ctx, f2, store.TranscriptPageRequest{Limit: 100, MaxBytes: 1 << 20})
	must(t, err)
	mustEqual(t, "s2 entries", len(page.Entries), 5)

	// Deleting s1 frees its charge for the user.
	_, err = s.ReleaseLease(ctx, f1)
	must(t, err)
	_, _, _, err = s.StopSession(ctx, "s1", t0+1, "")
	must(t, err)
	must(t, s.DeleteSession(ctx, "s1", t0+1))
	u, err = s.GetUser(ctx, "u1")
	must(t, err)
	mustEqual(t, "user charge after delete", u.TranscriptBytes, int64(5*unit))
	must(t, add(f2, tens(5)...))

	// Lengths: the uuid and the subpath, at the bound and one over.
	createSession(t, s, f, "s3")
	f3 := acquire(t, s, "s3", "w1", "b1", t0)
	batch := func(seq int64, subpath, uuid string) error {
		_, err := s.AppendTranscript(ctx, f3, store.TranscriptBatch{Subpath: subpath, BatchSeq: seq,
			Entries: []store.TranscriptEntry{{UUID: uuid, JSON: []byte("{}")}}}, quota)
		return err
	}
	must(t, batch(1, "", strings.Repeat("u", store.MaxIDBytes)))
	mustIs(t, batch(2, "", strings.Repeat("v", store.MaxIDBytes+1)), store.ErrInvalid)
	must(t, batch(1, strings.Repeat("p", store.MaxSubpathBytes), "x"))
	mustIs(t, batch(1, strings.Repeat("q", store.MaxSubpathBytes+1), "x"), store.ErrInvalid)
	big := quota
	big.MaxEntryBytes = store.MaxTranscriptEntryBytes + 1
	_, err = s.AppendTranscript(ctx, f3, store.TranscriptBatch{BatchSeq: 2, Entries: []store.TranscriptEntry{{JSON: []byte("{}")}}}, big)
	mustIs(t, err, store.ErrInvalid)

	// Subpaths per session: exactly the cap, then one more is refused.
	createSession(t, s, f, "s4")
	f4 := acquire(t, s, "s4", "w1", "b1", t0)
	must(t, s.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		for i := range store.MaxTranscriptSubpaths {
			if _, err := tx.AppendTranscript(ctx, f4, store.TranscriptBatch{Subpath: fmt.Sprint("p", i), BatchSeq: 1,
				Entries: []store.TranscriptEntry{{JSON: []byte("{}")}}}, quota); err != nil {
				return err
			}
		}
		return nil
	}))
	_, err = s.AppendTranscript(ctx, f4, store.TranscriptBatch{Subpath: "one-more", BatchSeq: 1, Entries: []store.TranscriptEntry{{JSON: []byte("{}")}}}, quota)
	mustIs(t, err, store.ErrLimit)
	subs, err := s.ListTranscriptSubpaths(ctx, f4)
	must(t, err)
	mustEqual(t, "subpaths", len(subs), store.MaxTranscriptSubpaths)
}

// One entry can exceed both the page and batch byte budgets. Batches with
// several entries must fit the batch budget.
func testTranscriptLargeEntries(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	createSession(t, s, f, "s1")
	fence := acquire(t, s, "s1", "w1", "b1", t0)
	q := quota
	q.MaxEntryBytes = store.MaxTranscriptEntryBytes
	q.MaxSessionBytes = store.DefaultMaxTranscriptBytes

	size := max(store.MaxTranscriptPageBytes, store.MaxTranscriptBatchBytes) + 1
	body := bytes.Repeat([]byte("x"), size)
	r, err := s.AppendTranscript(ctx, fence, store.TranscriptBatch{BatchSeq: 1, Entries: []store.TranscriptEntry{{JSON: body}}}, q)
	must(t, err)
	mustEqual(t, "oversized entry append", []any{r.Inserted, r.Full}, []any{1, false})
	page, err := s.LoadTranscript(ctx, fence, store.TranscriptPageRequest{Limit: 10, MaxBytes: store.MaxTranscriptPageBytes})
	must(t, err)
	mustEqual(t, "entries in oversized page", len(page.Entries), 1)
	mustEqual(t, "oversized entry length", len(page.Entries[0].JSON), size)

	half := store.TranscriptEntry{JSON: body[:store.MaxTranscriptBatchBytes/2]}
	r, err = s.AppendTranscript(ctx, fence, store.TranscriptBatch{BatchSeq: 2, Entries: []store.TranscriptEntry{half, half}}, q)
	must(t, err)
	mustEqual(t, "batch at byte limit", []any{r.Inserted, r.Full}, []any{2, false})
	_, err = s.AppendTranscript(ctx, fence, store.TranscriptBatch{BatchSeq: 3, Entries: []store.TranscriptEntry{half, half, {JSON: []byte("z")}}}, q)
	mustIs(t, err, store.ErrInvalid)
}

// testSessionInputs: idempotent submission, the open-input cap, text
// bound, queueing, and TakeInputs idempotency within an epoch.
func testSessionInputs(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	createSession(t, s, f, "s1")

	r := submit(t, s, newInput("s1", 1))
	mustEqual(t, "created", []any{r.Created, r.Input.State, r.Session.State}, []any{true, store.InputPending, store.SessionQueued})
	again := submit(t, s, newInput("s1", 1))
	mustEqual(t, "repeat", []any{again.Created, again.Input}, []any{false, r.Input})
	other := newInput("s1", 1)
	other.Text = "different"
	_, err := s.SubmitInput(ctx, other)
	mustIs(t, err, store.ErrExists)

	fence := acquire(t, s, "s1", "w1", "b1", t0+10)
	mustEqual(t, "acquire leaves queued", getSession(t, s, "s1").State, store.SessionIdle)
	r = submit(t, s, newInput("s1", 2))
	mustEqual(t, "held: stays idle", []any{r.Session.State, r.Lease.WorkerID}, []any{store.SessionIdle, "w1"})

	taken, err := s.TakeInputs(ctx, at(fence, t0+11))
	must(t, err)
	mustEqual(t, "taken", inputIDs(taken), []string{"s1-in1", "s1-in2"})
	mustEqual(t, "taken state", []any{taken[0].State, taken[0].TakenEpoch, taken[0].TakenAt}, []any{store.InputTaken, fence.Epoch, t0 + 11})
	submit(t, s, newInput("s1", 3))
	retry, err := s.TakeInputs(ctx, at(fence, t0+12))
	must(t, err)
	mustEqual(t, "retry returns the same plus new", inputIDs(retry), []string{"s1-in1", "s1-in2", "s1-in3"})
	mustEqual(t, "first take kept", retry[0], taken[0])

	// Persisted inputs keep their idempotency without their text.
	report(t, s, fence, store.TurnReport{LocalSeq: 1, Phase: store.TurnEnded, Persisted: store.TurnPersisted{InputIDs: []string{"s1-in1"}}})
	again = submit(t, s, newInput("s1", 1))
	mustEqual(t, "persisted repeat", []any{again.Created, again.Input.State, again.Input.Text}, []any{false, store.InputPersisted, ""})

	// The open-input cap: exactly MaxOpenInputs, then one more is refused.
	for n := 4; n <= store.MaxOpenInputs+1; n++ {
		submit(t, s, newInput("s1", n))
	}
	_, err = s.SubmitInput(ctx, newInput("s1", store.MaxOpenInputs+2))
	mustIs(t, err, store.ErrLimit)

	// Text: exactly the bound, then one byte more.
	createSession(t, s, f, "s2")
	big := newInput("s2", 1)
	big.Text = strings.Repeat("x", store.MaxInputTextBytes)
	submit(t, s, big)
	big = newInput("s2", 2)
	big.Text = strings.Repeat("x", store.MaxInputTextBytes+1)
	_, err = s.SubmitInput(ctx, big)
	mustIs(t, err, store.ErrInvalid)
	_, err = s.SubmitInput(ctx, newInput("ghost", 1))
	mustIs(t, err, store.ErrNotFound)
}

// testSessionTurns: ReportTurn idempotency, state and usage, persisted
// sets re-applied, and the kept history.
func testSessionTurns(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	createSession(t, s, f, "s1")
	fence := acquire(t, s, "s1", "w1", "b1", t0)
	submit(t, s, newInput("s1", 1))
	submit(t, s, newInput("s1", 2))
	_, err := s.TakeInputs(ctx, fence)
	must(t, err)

	started := report(t, s, at(fence, t0+1), store.TurnReport{LocalSeq: 1, Phase: store.TurnStarted})
	mustEqual(t, "started", []any{started.Duplicate, started.Turn.TurnNo, started.Session.State, started.Session.TurnCount},
		[]any{false, int64(1), store.SessionRunning, int64(1)})
	dup := report(t, s, at(fence, t0+2), store.TurnReport{LocalSeq: 1, Phase: store.TurnStarted})
	mustEqual(t, "repeated STARTED", []any{dup.Duplicate, dup.Turn, dup.Session.TurnCount}, []any{true, started.Turn, int64(1)})

	usage := store.TurnUsage{InputTokens: 10, OutputTokens: 5, CostMicroUSD: 300, DurationMs: 40}
	ended := report(t, s, at(fence, t0+3), store.TurnReport{LocalSeq: 1, Phase: store.TurnEnded, Usage: usage,
		Persisted: store.TurnPersisted{InputIDs: []string{"s1-in1"}}})
	mustEqual(t, "ended", []any{ended.Duplicate, ended.Turn.Outcome, ended.Turn.EndedAt, ended.Session.State, ended.Session.Usage, ended.Session.LastTurnOutcome},
		[]any{false, store.TurnCompleted, t0 + 3, store.SessionIdle, usage, store.TurnCompleted})
	// A repeated ENDED changes nothing but applies its persisted set.
	dup = report(t, s, at(fence, t0+4), store.TurnReport{LocalSeq: 1, Phase: store.TurnEnded, Usage: usage, Outcome: store.TurnError,
		Persisted: store.TurnPersisted{InputIDs: []string{"s1-in1", "s1-in2", "unknown"}}})
	mustEqual(t, "repeated ENDED", []any{dup.Duplicate, dup.Turn, dup.Session.Usage, dup.Session.TurnCount},
		[]any{true, ended.Turn, usage, int64(1)})
	open, err := s.TakeInputs(ctx, fence)
	must(t, err)
	mustEqual(t, "all persisted", len(open), 0)

	// ENDED for a turn never STARTED is refused (testTurnRules covers the
	// rest of the turn rules).
	_, err = s.ReportTurn(ctx, fence, store.TurnReport{LocalSeq: 2, Phase: store.TurnEnded, Outcome: store.TurnCompleted, At: t0 + 5})
	mustIs(t, err, store.ErrInvalid)
	report(t, s, at(fence, t0+5), store.TurnReport{LocalSeq: 2, Phase: store.TurnStarted, Origin: store.TurnFromCLI})
	only := report(t, s, at(fence, t0+5), store.TurnReport{LocalSeq: 2, Phase: store.TurnEnded})
	mustEqual(t, "CLI turn", []any{only.Turn.TurnNo, only.Turn.Origin, only.Session.State},
		[]any{int64(2), store.TurnFromCLI, store.SessionIdle})

	// History: the newest KeptTurns turns and KeptInputs persisted inputs.
	must(t, s.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		for n := 3; n <= store.KeptTurns+1; n++ {
			in := newInput("s1", n)
			if _, err := tx.SubmitInput(ctx, in); err != nil {
				return err
			}
			if _, err := tx.TakeInputs(ctx, fence); err != nil {
				return err
			}
			for _, ph := range []store.TurnPhase{store.TurnStarted, store.TurnEnded} {
				if _, err := tx.ReportTurn(ctx, fence, store.TurnReport{LocalSeq: int64(n), Phase: ph, Origin: store.TurnFromInput,
					Outcome: store.TurnCompleted, At: t0 + 10 + int64(n), Persisted: persistedIf(ph, in.InputID)}); err != nil {
					return err
				}
			}
		}
		return nil
	}))
	turns, err := s.ListTurns(ctx, store.TurnFilter{SessionID: "s1", Limit: 1})
	must(t, err)
	mustEqual(t, "oldest kept turn", turns[0].TurnNo, int64(2))
	turns, err = s.ListTurns(ctx, store.TurnFilter{SessionID: "s1", AfterTurnNo: store.KeptTurns, Limit: 10})
	must(t, err)
	mustEqual(t, "newest turn", turns[0].TurnNo, int64(store.KeptTurns+1))
	// s1-in1 and s1-in2 were persisted first; 1001 persisted in all, so
	// the oldest no longer answers a retry and would be a new input.
	r := submit(t, s, newInput("s1", 1))
	mustEqual(t, "trimmed input is new again", r.Created, true)
	r = submit(t, s, newInput("s1", 3))
	mustEqual(t, "kept input", r.Created, false)
}

func persistedIf(ph store.TurnPhase, inputID string) store.TurnPersisted {
	if ph == store.TurnEnded {
		return store.TurnPersisted{InputIDs: []string{inputID}}
	}
	return store.TurnPersisted{}
}
