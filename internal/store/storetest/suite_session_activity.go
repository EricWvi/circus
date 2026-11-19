package storetest

import (
	"context"
	"fmt"
	"testing"

	"github.com/EricWvi/circus/internal/store"
)

func testSessionActivity(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	createSession(t, s, f, "s1")
	createSession(t, s, f, "s2")
	appendAt := func(id string, times ...int64) {
		t.Helper()
		var events []store.EventInput
		for _, at := range times {
			events = append(events, store.EventInput{Kind: "ASSISTANT", At: at})
		}
		_, err := s.AppendEvents(ctx, id, nil, events, store.DefaultMaxViewBytes)
		must(t, err)
	}
	appendAt("s2", t0+15)
	in := newInput("s1", 1)
	in.CreatedAt = t0 + 20
	mustEqual(t, "input advances activity", submit(t, s, in).Session.LastActivityAt, t0+20)
	page, err := s.ListSessions(ctx, store.SessionFilter{UserID: "u1", Now: t0 + 20, Limit: 1})
	must(t, err)
	mustEqual(t, "new message sorts first", page[0].ID, "s1")
	in.CreatedAt = t0 + 100
	mustEqual(t, "retry activity", submit(t, s, in).Session.LastActivityAt, t0+20)
	in = newInput("s1", 2)
	in.CreatedAt = t0 + 5
	submit(t, s, in)
	in = newInput("s1", 3)
	in.CreatedAt = t0 + 200
	in.Kind = store.InputApproval
	submit(t, s, in)
	mustEqual(t, "older input and approval do not move activity", getSession(t, s, "s1").LastActivityAt, t0+20)
	in = newInput("s1", 4)
	in.CreatedAt = t0 + 300
	mustIs(t, s.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		_, err := tx.SubmitInput(ctx, in)
		if err != nil {
			return err
		}
		return errRollback
	}), errRollback)
	mustEqual(t, "input activity is atomic", getSession(t, s, "s1").LastActivityAt, t0+20)
	appendAt("s1", t0+30, t0+10)
	mustEqual(t, "out of order events", getSession(t, s, "s1").LastActivityAt, t0+30)
	fence := acquire(t, s, "s1", "w1", "boot", t0)
	report(t, s, fence, store.TurnReport{LocalSeq: 1, Phase: store.TurnEnded, Outcome: store.TurnStopped, At: t0 + 40})
	mustEqual(t, "boundary advances past ordinary events", getSession(t, s, "s1").LastActivityAt, t0+40)
	report(t, s, fence, store.TurnReport{LocalSeq: 2, Phase: store.TurnEnded, Outcome: store.TurnInterrupted, At: t0 + 35})
	mustEqual(t, "older boundary cannot regress", getSession(t, s, "s1").LastActivityAt, t0+40)
	report(t, s, fence, store.TurnReport{LocalSeq: 3, Phase: store.TurnStarted, At: t0 + 45})
	_, err = s.ReleaseLease(ctx, at(fence, t0+50))
	must(t, err)
	mustEqual(t, "lease loss advances activity", getSession(t, s, "s1").LastActivityAt, t0+50)
	events := sessionEvents(t, s, "s1")
	mustEqual(t, "boundary follows ordinary events", []any{events[0].Kind, events[2].Kind, events[2].At}, []any{"ASSISTANT", store.EventKindTurnStopped, t0 + 40})
}

func testSessionPagingActivity(t *testing.T, b Backend) {
	s, dir := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	other := reopen(t, b, dir)
	appendAt := func(id string, offset int64) {
		t.Helper()
		_, err := other.AppendEvents(ctx, id, nil, []store.EventInput{{Kind: "ASSISTANT", At: t0 + offset}}, store.DefaultMaxViewBytes)
		must(t, err)
	}
	for i, id := range []string{"a", "b", "c", "d", "e", "f"} {
		createSession(t, s, f, id)
		appendAt(id, int64(60-i*10))
	}
	filter := store.SessionFilter{ProjectID: "session-project", Now: t0 + 200, Limit: 2}
	var seen []string
	next := func() []string {
		t.Helper()
		page, err := s.ListSessions(ctx, filter)
		must(t, err)
		var ids []string
		for _, sess := range page {
			ids = append(ids, sess.ID)
		}
		seen = append(seen, ids...)
		if len(page) > 0 {
			last := page[len(page)-1]
			filter.After = &store.SessionCursor{LastActivityAt: last.LastActivityAt, ID: last.ID}
		}
		return ids
	}
	mustEqual(t, "first page", next(), []string{"a", "b"})
	appendAt("a", 5) // Already returned: an old stamp must not sink it below the cursor.
	appendAt("b", 80)
	appendAt("c", 90) // Not yet returned: moves above the cursor and waits for refresh.
	appendAt("d", 40) // Remains below the cursor and is still returned.
	mustEqual(t, "second page", next(), []string{"d", "e"})
	appendAt("d", 1)
	appendAt("e", 120)
	appendAt("f", 130)
	createSession(t, s, f, "g")
	appendAt("g", 140)
	mustEqual(t, "end of traversal", next(), []string(nil))
	mustEqual(t, "unchanged members once, no repeated active members", seen, []string{"a", "b", "d", "e"})
	filter.After = nil
	filter.Limit = 10
	seen = nil
	mustEqual(t, "refresh includes moved and new sessions", next(), []string{"g", "f", "e", "c", "b", "a", "d"})
}

func testUserProjectSessions(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seedProjects(t, s)
	_, err := s.CreateProject(ctx, newProject("human-project", "alice", "Human"))
	must(t, err)
	sess := newSession("human-session", f.fixture)
	sess.ProjectID = "human-project"
	mustIs(t, s.CreateSession(ctx, sess, nil, 100), store.ErrNotFound)
	sess.UserID = f.alice.ID
	sess.DefaultDeviceID = "alice-laptop"
	must(t, s.CreateSession(ctx, sess, nil, 100))
	for i := 0; i < 2; i++ {
		mustIs(t, s.DeleteUser(ctx, "alice"), store.ErrHasSessions)
		must(t, errOf(s.GetUser(ctx, "alice")))
		must(t, errOf(s.GetProject(ctx, "alice", "human-project")))
		mustEqual(t, fmt.Sprint("session survives ", i), getSession(t, s, sess.ID).ProjectID, "human-project")
		_, _, _, err = s.StopSession(ctx, sess.ID, t0, "")
		must(t, err)
	}
	must(t, s.DeleteSession(ctx, sess.ID, t0))
	must(t, s.DeleteProject(ctx, "alice", "human-project", t0))
	_, err = s.GetProject(ctx, "alice", "human-project")
	mustIs(t, err, store.ErrNotFound)
}
