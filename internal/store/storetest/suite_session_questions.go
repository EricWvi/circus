package storetest

import (
	"fmt"
	"strings"
	"testing"

	"github.com/EricWvi/circus/internal/store"
)

const questionInput = `{"questions":[{"question":"Which color?","header":"Color","options":[{"label":"Red","description":"r"},{"label":"Blue","description":"b"}],"multiSelect":false}]}`

func newQuestion(id, toolUseID string, createdAt int64) store.Approval {
	return store.Approval{
		ApprovalID: id, Kind: store.ApprovalQuestion, ToolUseID: toolUseID, Tool: "AskUserQuestion",
		Display: "Which color?", InputJSON: questionInput, CreatedAt: createdAt,
	}
}

// testQuestionApprovals: a question's shape, dedup by tool use, supersede,
// no expiry, the pending cap counting it, answers written as an input in
// the same transaction (no grant), dismissal, idempotent resolution, and
// the end of pending questions when the session stops.
func testQuestionApprovals(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	createSession(t, s, f, "s1")
	fence := at(acquire(t, s, "s1", "w1", "b1", t0), t0+1)

	// Shape: a tool use id and an input, and no device, match key or expiry.
	for name, change := range map[string]func(*store.Approval){
		"no tool use id":    func(a *store.Approval) { a.ToolUseID = "" },
		"no input":          func(a *store.Approval) { a.InputJSON = "" },
		"input too large":   func(a *store.Approval) { a.InputJSON = strings.Repeat("x", store.MaxQuestionInputBytes+1) },
		"a device":          func(a *store.Approval) { a.DeviceID = "d1" },
		"a match key":       func(a *store.Approval) { a.MatchKey = mk("x") },
		"an expiry":         func(a *store.Approval) { a.ExpiresAt = t0 + 100 },
		"unknown kind":      func(a *store.Approval) { a.Kind = "plan" },
		"a tool with input": func(a *store.Approval) { *a = newApproval("bad", "x"); a.InputJSON = questionInput },
	} {
		a := newQuestion("bad", "tu-bad", t0+2)
		change(&a)
		_, err := s.RequestApproval(ctx, fence, a)
		mustIs(t, fmt.Errorf("%s: %w", name, err), store.ErrInvalid)
	}
	_, err := s.RequestApproval(ctx, fence, newQuestion("max", "tu-max", t0+2))
	must(t, err)
	_, err = s.ResolveApproval(ctx, store.ApprovalResolution{ApprovalID: "max", At: t0 + 2})
	must(t, err)

	q1, err := s.RequestApproval(ctx, fence, newQuestion("q1", "tu-q1", t0+3))
	must(t, err)
	mustEqual(t, "created", []any{q1.Created, q1.Approval.Kind, q1.Approval.State, q1.Approval.ExpiresAt, q1.Approval.InputJSON, q1.Approval.UserID},
		[]any{true, store.ApprovalQuestion, store.ApprovalPending, int64(0), questionInput, "u1"})
	// The same tool use asks once.
	dup, err := s.RequestApproval(ctx, fence, newQuestion("q1-again", "tu-q1", t0+4))
	must(t, err)
	mustEqual(t, "dedup", []any{dup.Created, dup.Approval.ApprovalID, len(dup.Superseded)}, []any{false, "q1", 0})

	// It never expires: counted as pending far in the future, and the
	// sweep leaves it.
	far := t0 + 365*24*3600*1000
	mustEqual(t, "pending count", getSessionAt(t, s, "s1", far).PendingApprovalCount, uint32(1))
	exp, err := s.ExpireApprovals(ctx, far, 10)
	must(t, err)
	mustEqual(t, "not swept", len(exp), 0)

	// A new question supersedes the pending one, which drops its input.
	q2, err := s.RequestApproval(ctx, fence, newQuestion("q2", "tu-q2", t0+5))
	must(t, err)
	mustEqual(t, "superseded", []any{q2.Created, len(q2.Superseded), q2.Superseded[0].ApprovalID, q2.Superseded[0].State, q2.Superseded[0].ResolvedAt},
		[]any{true, 1, "q1", store.ApprovalSuperseded, t0 + 5})
	old, err := s.GetApproval(ctx, "q1")
	must(t, err)
	mustEqual(t, "old question", []any{old.State, old.InputJSON}, []any{store.ApprovalSuperseded, ""})
	// Its tool use repeated still names it, and supersedes nothing.
	again, err := s.RequestApproval(ctx, fence, newQuestion("q1-late", "tu-q1", t0+6))
	must(t, err)
	mustEqual(t, "repeat after supersede", []any{again.Created, again.Approval.ApprovalID, again.Approval.State, len(again.Superseded)},
		[]any{false, "q1", store.ApprovalSuperseded, 0})
	mustEqual(t, "one pending", getSessionAt(t, s, "s1", far).PendingApprovalCount, uint32(1))

	// The pending cap counts the question, and expires the oldest tool
	// approval, never the question.
	for i := range store.MaxPendingApprovals - 1 {
		_, err := s.RequestApproval(ctx, fence, newApproval(fmt.Sprint("p", i), fmt.Sprint("cmd", i)))
		must(t, err)
	}
	mustEqual(t, "full", getSessionAt(t, s, "s1", t0+10).PendingApprovalCount, uint32(store.MaxPendingApprovals))
	more, err := s.RequestApproval(ctx, fence, newApproval("p-more", "cmd-more"))
	must(t, err)
	mustEqual(t, "oldest tool approval expired", more.Expired.ApprovalID, "p0")
	q3, err := s.RequestApproval(ctx, fence, newQuestion("q3", "tu-q3", t0+7))
	must(t, err)
	// A question at the cap supersedes the pending one first: room enough.
	mustEqual(t, "a question at the cap", []any{q3.Expired, q3.Superseded[0].ApprovalID}, []any{(*store.Approval)(nil), "q2"})
	pending, err := s.ListApprovals(ctx, store.ApprovalFilter{SessionID: "s1", States: []store.ApprovalState{store.ApprovalPending},
		Limit: store.MaxPageSize, MaxBytes: store.MaxTranscriptPageBytes})
	must(t, err)
	var questions []string
	for _, a := range pending {
		if a.Kind == store.ApprovalQuestion {
			questions = append(questions, a.ApprovalID+" "+a.InputJSON)
		}
	}
	mustEqual(t, "pending", []any{len(pending), questions}, []any{store.MaxPendingApprovals, []string{"q3 " + questionInput}})

	// Answering, far past any tool approval's lifetime: the answers input,
	// no grant, the input dropped.
	answers := store.Input{InputID: "answer-q3", Text: `The user answered your questions: "Which color?"="Blue". Continue with these answers in mind.`}
	res, err := s.ResolveApproval(ctx, store.ApprovalResolution{ApprovalID: "q3", Approve: true, At: far, Input: answers})
	must(t, err)
	mustEqual(t, "answered", []any{res.Changed, res.Approval.State, res.Approval.InputJSON, res.Input.Kind, res.Input.ClientMsgID, res.Input.Text, res.Input.State},
		[]any{true, store.ApprovalAnswered, "", store.InputAnswer, "approval:q3", answers.Text, store.InputPending})
	recs, err := s.GetSessionRecords(ctx, at(fence, t0+10), records(store.RecordGrants))
	must(t, err)
	mustEqual(t, "no grant", len(recs.Grants), 0)
	twice, err := s.ResolveApproval(ctx, store.ApprovalResolution{ApprovalID: "q3", Approve: true, At: far + 1,
		Input: store.Input{InputID: "answer-q3-again", Text: "other"}})
	must(t, err)
	mustEqual(t, "resolved twice", []any{twice.Changed, twice.Approval.State, twice.Input}, []any{false, store.ApprovalAnswered, (*store.Input)(nil)})
	taken, err := s.TakeInputs(ctx, at(fence, t0+10))
	must(t, err)
	mustEqual(t, "one answer input", []any{len(taken), taken[0].InputID, taken[0].Kind}, []any{1, "answer-q3", store.InputAnswer})

	// Dismissing writes nothing.
	_, err = s.RequestApproval(ctx, fence, newQuestion("q4", "tu-q4", t0+8))
	must(t, err)
	res, err = s.ResolveApproval(ctx, store.ApprovalResolution{ApprovalID: "q4", At: t0 + 9})
	must(t, err)
	mustEqual(t, "dismissed", []any{res.Changed, res.Approval.State, res.Input}, []any{true, store.ApprovalDismissed, (*store.Input)(nil)})

	// Stopping the session ends its pending question; tool approvals stay
	// until they expire.
	_, err = s.RequestApproval(ctx, fence, newQuestion("q5", "tu-q5", t0+9))
	must(t, err)
	_, _, _, err = s.StopSession(ctx, "s1", t0+20, "user")
	must(t, err)
	q5, err := s.GetApproval(ctx, "q5")
	must(t, err)
	mustEqual(t, "stopped", []any{q5.State, q5.ResolvedAt, q5.InputJSON}, []any{store.ApprovalExpired, t0 + 20, ""})
	res, err = s.ResolveApproval(ctx, store.ApprovalResolution{ApprovalID: "q5", Approve: true, At: t0 + 21, Input: answers})
	must(t, err)
	mustEqual(t, "after the stop", []any{res.Changed, res.Approval.State, res.Input}, []any{false, store.ApprovalExpired, (*store.Input)(nil)})
	mustEqual(t, "tool approvals pending", getSessionAt(t, s, "s1", t0+21).PendingApprovalCount, uint32(store.MaxPendingApprovals-1))

	// The transcript quota's stop ends them too.
	createSession(t, s, f, "s2")
	fence2 := at(acquire(t, s, "s2", "w1", "b1", t0), t0+1)
	_, err = s.RequestApproval(ctx, fence2, newQuestion("q6", "tu-q6", t0+2))
	must(t, err)
	full, err := s.AppendTranscript(ctx, at(fence2, t0+3), store.TranscriptBatch{BatchSeq: 1, Entries: []store.TranscriptEntry{{JSON: []byte(strings.Repeat("x", 200))}}},
		store.TranscriptQuota{MaxEntryBytes: 100, MaxSessionBytes: 100, MaxUserBytes: 1000})
	must(t, err)
	mustEqual(t, "full", full.Stopped, true)
	q6, err := s.GetApproval(ctx, "q6")
	must(t, err)
	mustEqual(t, "stopped by the quota", []any{q6.State, q6.ResolvedAt}, []any{store.ApprovalExpired, t0 + 3})

	// The answers are the user's: they move the session's last activity,
	// and the notes before them no longer count as notes since the user's
	// last input.
	createSession(t, s, f, "s3")
	fence3 := at(acquire(t, s, "s3", "w1", "b1", t0), t0+1)
	note := newInput("s3", 1)
	note.Kind, note.Note, note.CreatedAt = store.InputNote, "ledger_problems", t0+2
	submit(t, s, note)
	notes, err := s.CountNotesSinceUserInput(ctx, "s3")
	must(t, err)
	mustEqual(t, "a note", notes, 1)
	_, err = s.RequestApproval(ctx, fence3, newQuestion("q7", "tu-q7", t0+3))
	must(t, err)
	before := getSessionAt(t, s, "s3", t0+4).LastActivityAt
	_, err = s.ResolveApproval(ctx, store.ApprovalResolution{ApprovalID: "q7", Approve: true, At: t0 + 5000,
		Input: store.Input{InputID: "answer-q7", Text: "answers"}})
	must(t, err)
	mustEqual(t, "activity", []any{before < t0+5000, getSessionAt(t, s, "s3", t0+5000).LastActivityAt}, []any{true, t0 + 5000})
	notes, err = s.CountNotesSinceUserInput(ctx, "s3")
	must(t, err)
	mustEqual(t, "no note since the answers", notes, 0)

	// The pending cap never expires the question, even as the oldest
	// pending approval.
	createSession(t, s, f, "s4")
	fence4 := at(acquire(t, s, "s4", "w1", "b1", t0), t0+1)
	_, err = s.RequestApproval(ctx, fence4, newQuestion("q8", "tu-q8", t0+2))
	must(t, err)
	for i := range store.MaxPendingApprovals - 1 {
		a := newApproval(fmt.Sprint("s4-p", i), fmt.Sprint("s4-cmd", i))
		a.CreatedAt, a.ExpiresAt = t0+3+int64(i), t0+600_000
		_, err := s.RequestApproval(ctx, fence4, a)
		must(t, err)
	}
	over := newApproval("s4-over", "s4-over")
	over.CreatedAt, over.ExpiresAt = t0+100, t0+600_000
	capped, err := s.RequestApproval(ctx, fence4, over)
	must(t, err)
	mustEqual(t, "the oldest tool approval expires", capped.Expired.ApprovalID, "s4-p0")
	q8, err := s.GetApproval(ctx, "q8")
	must(t, err)
	mustEqual(t, "the question stays", q8.State, store.ApprovalPending)
}

func getSessionAt(t testing.TB, ops store.Ops, id string, now int64) store.Session {
	t.Helper()
	s, err := ops.GetSession(t.Context(), id, now)
	must(t, err)
	return s
}
