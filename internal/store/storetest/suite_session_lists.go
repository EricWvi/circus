package storetest

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/EricWvi/circus/internal/store"
)

func sessionEvents(t testing.TB, s store.Ops, id string) []store.SessionEvent {
	t.Helper()
	p, err := s.ReadEvents(context.Background(), store.EventFilter{SessionID: id, Limit: store.MaxPageSize, MaxBytes: store.MaxTranscriptPageBytes})
	must(t, err)
	return p.Events
}

func checkTurnBoundary(t testing.TB, s store.Ops, id string, no, at int64, outcome store.TurnOutcome) {
	t.Helper()
	kind := store.EventKindTurnInterrupted
	if outcome == store.TurnStopped {
		kind = store.EventKindTurnStopped
	}
	var found []store.SessionEvent
	for _, e := range sessionEvents(t, s, id) {
		if e.Kind == store.EventKindTurnInterrupted || e.Kind == store.EventKindTurnStopped {
			found = append(found, e)
		}
	}
	payload := fmt.Sprintf(`{"turn_no":"%d","outcome":"%s"}`, no, outcome)
	mustEqual(t, "one turn boundary", found, []store.SessionEvent{{SessionID: id, Seq: 1, At: at, Kind: kind, Payload: []byte(payload), PayloadBytes: int64(len(payload))}})
	mustEqual(t, "boundary activity", getSession(t, s, id).LastActivityAt, at)
}

func testSessionLists(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seedProjects(t, s)
	_, err := s.CreateProject(ctx, newProject("web-project", f.user.ID, "Web"))
	must(t, err)
	sess := newSession("s1", f.fixture)
	sess.ProjectID = "missing"
	mustIs(t, errOf(s.CreateSession(ctx, sess, nil, 100)), store.ErrNotFound)
	sess.ProjectID = ""
	mustIs(t, errOf(s.CreateSession(ctx, sess, nil, 100)), store.ErrInvalid)
	sess.ProjectID = "web-project"
	must(t, errOf(s.CreateSession(ctx, sess, nil, 100)))
	got := getSession(t, s, "s1")
	mustEqual(t, "list fields", []any{got.ProjectID, got.UserID, got.Title, got.LastActivityAt, got.PendingApprovalCount}, []any{"web-project", "u1", "", t0, uint32(0)})
	createSession(t, s, f.fixture, "s2")
	createSession(t, s, f.fixture, "s3")
	ids := func(filter store.SessionFilter) []string {
		t.Helper()
		filter.Now = t0
		rows, err := s.ListSessions(ctx, filter)
		must(t, err)
		var ids []string
		for _, v := range rows {
			ids = append(ids, v.ID)
		}
		return ids
	}
	mustEqual(t, "tie order", ids(store.SessionFilter{UserID: "u1", Limit: 10}), []string{"s3", "s2", "s1"})
	fence := acquire(t, s, "s1", "w1", "b1", t0)
	_, err = s.AppendEvents(ctx, "s1", &store.EventPublish{Fence: fence, BatchSeq: 1}, []store.EventInput{{Kind: "ASSISTANT", At: t0 + 20, Payload: []byte(`{}`)}}, 100)
	must(t, err)
	_, err = s.AppendEvents(ctx, "s2", nil, []store.EventInput{{Kind: "USER", At: t0 + 10}}, 100)
	must(t, err)
	mustEqual(t, "activity order", ids(store.SessionFilter{Limit: 10}), []string{"s1", "s2", "s3"})
	mustEqual(t, "keyset", ids(store.SessionFilter{Limit: 1, After: &store.SessionCursor{LastActivityAt: t0 + 20, ID: "s1"}}), []string{"s2"})
	mustEqual(t, "tie keyset", ids(store.SessionFilter{Limit: 10, After: &store.SessionCursor{LastActivityAt: t0, ID: "s3"}}), []string(nil))
	mustEqual(t, "project scope", ids(store.SessionFilter{ProjectID: "web-project", Limit: 10}), []string{"s1"})
	mustEqual(t, "user and project scope", ids(store.SessionFilter{ProjectID: "web-project", UserID: f.alice.ID, Limit: 10}), []string(nil))
	// An older input and an explicit title cannot move activity backwards.
	submit(t, s, newInput("s1", 1))
	must(t, s.SetSessionTitle(ctx, "s1", "Manual"))
	mustEqual(t, "no event activity", getSession(t, s, "s1").LastActivityAt, t0+20)
	// A refused worker batch consumes its sequence but appends no durable event.
	dropped, err := s.AppendEvents(ctx, "s1", &store.EventPublish{Fence: fence, BatchSeq: 2}, []store.EventInput{{Kind: store.EventKindTurnStopped, At: t0 + 30}}, 100)
	must(t, err)
	mustEqual(t, "refused", dropped.Dropped, true)
	mustEqual(t, "dropped activity", getSession(t, s, "s1").LastActivityAt, t0+20)
	// A truncated boundary still is a durable event; a dropped view event is not.
	_, err = s.AppendEvents(ctx, "s1", nil, []store.EventInput{{Kind: "ASSISTANT", At: t0 + 31, Payload: []byte("too much")}}, 1)
	must(t, err)
	mustEqual(t, "view drop activity", getSession(t, s, "s1").LastActivityAt, t0+20)
	for i, kind := range []string{store.EventKindTurnInterrupted, store.EventKindTurnStopped} {
		_, err = s.AppendEvents(ctx, "s1", nil, []store.EventInput{{Kind: kind, At: t0 + 40 + int64(i), Payload: []byte(`{}`)}}, 1)
		must(t, err)
	}
	events := sessionEvents(t, s, "s1")
	mustEqual(t, "retained kinds", []string{events[1].Kind, events[2].Kind}, []string{store.EventKindTurnInterrupted, store.EventKindTurnStopped})
	mustEqual(t, "truncated boundary", []any{events[2].Truncated, events[2].Payload, getSession(t, s, "s1").LastActivityAt}, []any{true, []byte(nil), t0 + 41})
	// Approval expiry is evaluated at the supplied read time, without a sweep.
	for i := 0; i < store.MaxPendingApprovals; i++ {
		_, err = s.RequestApproval(ctx, fence, store.Approval{ApprovalID: fmt.Sprintf("a%d", i), Tool: "bash", DeviceID: "d1", MatchKey: mk(fmt.Sprint(i)), CreatedAt: t0, ExpiresAt: t0 + 100 + int64(i)})
		must(t, err)
	}
	got, err = s.GetSession(ctx, "s1", t0+100)
	must(t, err)
	mustEqual(t, "expired at read", got.PendingApprovalCount, uint32(31))
	_, err = s.ResolveApproval(ctx, store.ApprovalResolution{ApprovalID: "a1", At: t0 + 100})
	must(t, err)
	got, err = s.GetSession(ctx, "s1", t0+100)
	must(t, err)
	rows, err := s.ListSessions(ctx, store.SessionFilter{ProjectID: "web-project", Now: t0 + 100, Limit: 10})
	must(t, err)
	mustEqual(t, "list and get", rows, []store.Session{got})
	mustEqual(t, "resolved excluded", got.PendingApprovalCount, uint32(30))
	got, err = s.GetSession(ctx, "s1", t0+200)
	must(t, err)
	mustEqual(t, "all expired", got.PendingApprovalCount, uint32(0))
	_, err = s.GetSession(ctx, "s1", 0)
	mustIs(t, err, store.ErrInvalid)
	_, err = s.ListSessions(ctx, store.SessionFilter{Limit: 10})
	mustIs(t, err, store.ErrInvalid)
}

func testSessionTitles(t *testing.T, b Backend) {
	for _, tc := range []struct{ text, want string }{
		{strings.Repeat("a", 40) + " " + strings.Repeat("b", 19) + " c", strings.Repeat("a", 40) + " " + strings.Repeat("b", 19) + "…"},
		{"\x00\x1b\u202eHello\u200b world\u2069", "Hello world"},
		{"a \u200b  b", "a b"},
		{"\x00\u200d\n next", "next"},
		{"", ""}, {"\n \t\r\n", ""}, {"\n \t First\t \u2003line \r\nsecond", "First line"},
		{strings.Repeat("界", 60), strings.Repeat("界", 60)},
		{strings.Repeat("界", 61), strings.Repeat("界", 60) + "…"},
		{strings.Repeat("a", 29) + " " + strings.Repeat("b", 40), strings.Repeat("a", 29) + " " + strings.Repeat("b", 30) + "…"},
		{strings.Repeat("a", 30) + " " + strings.Repeat("b", 40), strings.Repeat("a", 30) + "…"},
		{strings.Repeat("a", 59) + " " + "bb", strings.Repeat("a", 59) + "…"},
	} {
		mustEqual(t, "derived title", store.DeriveSessionTitle(tc.text), tc.want)
	}
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	createSession(t, s, f, "s1")
	in := newInput("s1", 1)
	in.Text = " \n \t"
	submit(t, s, in)
	mustEqual(t, "empty derivation", getSession(t, s, "s1").Title, "")
	in = newInput("s1", 2)
	in.Kind = store.InputApproval
	submit(t, s, in)
	mustEqual(t, "approval no title", getSession(t, s, "s1").Title, "")
	in = newInput("s1", 3)
	in.Text = "\n  First\t user message \nignored"
	mustIs(t, s.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		_, err := tx.SubmitInput(ctx, in)
		if err != nil {
			return err
		}
		return errRollback
	}), errRollback)
	mustEqual(t, "rollback title", getSession(t, s, "s1").Title, "")
	sub := submit(t, s, in)
	mustEqual(t, "returned title", sub.Session.Title, "First user message")
	mustEqual(t, "dedup", submit(t, s, in).Created, false)
	submit(t, s, newInput("s1", 4))
	mustEqual(t, "first title", getSession(t, s, "s1").Title, "First user message")
	must(t, s.SetSessionTitle(ctx, "s1", "Renamed"))
	mustEqual(t, "renamed", getSession(t, s, "s1").Title, "Renamed")
	createSession(t, s, f, "s2")
	must(t, s.SetSessionTitle(ctx, "s2", "Before input"))
	submit(t, s, newInput("s2", 1))
	mustEqual(t, "explicit title wins", getSession(t, s, "s2").Title, "Before input")
	mustIs(t, s.SetSessionTitle(ctx, "s2", ""), store.ErrInvalid)
	mustIs(t, s.SetSessionTitle(ctx, "s2", strings.Repeat("x", store.MaxSessionTitleBytes+1)), store.ErrInvalid)
	must(t, s.SetSessionTitle(ctx, "s2", strings.Repeat("x", store.MaxSessionTitleBytes)))
	mustIs(t, s.SetSessionTitle(ctx, "missing", "Title"), store.ErrNotFound)
	mustEqual(t, "title no activity", getSession(t, s, "s2").LastActivityAt, t0+1)
}

func testProjectSessions(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seedProjects(t, s)
	fence := busySession(t, s, f.fixture)
	// A second, already deletable session must survive any refused cascade.
	createSession(t, s, f.fixture, "s2")
	_, _, _, err := s.StopSession(ctx, "s2", t0, "")
	must(t, err)
	before := snapSession(t, s, fence)
	mustIs(t, s.DeleteProject(ctx, "bob", "session-project", t0+2), store.ErrNotFound)
	mustIs(t, s.CheckSessionDeletable(ctx, "s1", t0+2), store.ErrSessionActive)
	mustIs(t, s.DeleteProject(ctx, "u1", "session-project", t0+2), store.ErrHasSessions)
	mustEqual(t, "refusal leaves every row", snapSession(t, s, fence), before)
	mustEqual(t, "other session survives refusal", getSession(t, s, "s2").State, store.SessionStopped)
	_, _, _, err = s.StopSession(ctx, "s1", t0+2, "")
	must(t, err)
	transition(t, s, "c1", expect(store.CallDispatched), store.CallCompleted, store.CallPatch{FinishedAt: t0 + 2})
	mustIs(t, s.CheckSessionDeletable(ctx, "s1", t0+2), store.ErrSessionActive)
	mustIs(t, s.DeleteProject(ctx, "u1", "session-project", t0+2), store.ErrHasSessions) // live lease only
	_, err = s.ReleaseLease(ctx, at(fence, t0+3))
	must(t, err)
	sessionCall(t, s, f.fixture, "unknown", "s1", store.CallUnknown)
	mustIs(t, s.CheckSessionDeletable(ctx, "s1", t0+3), store.ErrSessionActive)
	mustIs(t, s.DeleteProject(ctx, "u1", "session-project", t0+3), store.ErrHasSessions) // call only
	transition(t, s, "unknown", expect(store.CallUnknown), store.CallLost, store.CallPatch{FinishedAt: t0 + 3})
	_, err = s.PutJob(ctx, store.Job{DeviceID: "d1", BootID: "b1", JobID: "ended", UserID: "u1", SessionID: "s1", State: store.JobExited, LastSeq: 1})
	must(t, err)
	// Keep an unrelated project's transcript charge intact.
	_, err = s.CreateProject(ctx, newProject("keep-project", f.user.ID, "Keep"))
	must(t, err)
	keep := newSession("keep", f.fixture)
	keep.ProjectID = "keep-project"
	must(t, errOf(s.CreateSession(ctx, keep, nil, 100)))
	keepFence := acquire(t, s, "keep", "w1", "b1", t0)
	appendEntries(t, s, keepFence, "", 1, entry("keep", `{}`))
	// Even fully deletable sessions block the store's project deletion.
	must(t, s.CheckSessionDeletable(ctx, "s1", t0+4))
	must(t, s.CheckSessionDeletable(ctx, "s2", t0+4))
	must(t, s.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		mustIs(t, tx.DeleteProject(ctx, "u1", "session-project", t0+4), store.ErrHasSessions)
		return nil
	}))
	mustEqual(t, "refusal preserves stopped session", getSession(t, s, "s1").State, store.SessionStopped)
	must(t, s.DeleteSession(ctx, "s1", t0+4))
	mustIs(t, s.DeleteProject(ctx, "u1", "session-project", t0+4), store.ErrHasSessions)
	must(t, s.DeleteSession(ctx, "s2", t0+4))
	must(t, s.DeleteProject(ctx, "u1", "session-project", t0+4))
	mustIs(t, s.DeleteProject(ctx, "u1", "session-project", t0+4), store.ErrNotFound)
	for _, id := range []string{"s1", "s2"} {
		_, err = s.GetSession(ctx, id, t0+4)
		mustIs(t, err, store.ErrNotFound)
		_, err = s.GetLease(ctx, id)
		mustIs(t, err, store.ErrNotFound)
		_, err = s.ReadEvents(ctx, store.EventFilter{SessionID: id, Limit: 1, MaxBytes: 1})
		mustIs(t, err, store.ErrNotFound)
		turns, err := s.ListTurns(ctx, store.TurnFilter{SessionID: id, Limit: 10})
		must(t, err)
		mustEqual(t, "turns deleted", len(turns), 0)
		approvals, err := s.ListApprovals(ctx, store.ApprovalFilter{SessionID: id, Limit: 10, MaxBytes: store.MaxTranscriptPageBytes})
		must(t, err)
		mustEqual(t, "approvals deleted", len(approvals), 0)
		calls, err := s.ListCalls(ctx, store.CallFilter{SessionID: id, Limit: 10})
		must(t, err)
		mustEqual(t, "calls deleted", len(calls), 0)
	}
	jobs, err := s.ListJobs(ctx, store.JobFilter{SessionID: "s1", Limit: 10})
	must(t, err)
	mustEqual(t, "running device job retained", []any{len(jobs), jobs[0].JobID}, []any{1, "j1"})
	owner, err := s.GetUser(ctx, "u1")
	must(t, err)
	mustEqual(t, "charge preserved for other project", owner.TranscriptBytes, getSession(t, s, "keep").TranscriptBytes)
	// An expired holder does not block the cascade.
	_, err = s.CreateProject(ctx, newProject("expired-project", f.user.ID, "Expired"))
	must(t, err)
	expired := newSession("expired", f.fixture)
	expired.ProjectID = "expired-project"
	must(t, errOf(s.CreateSession(ctx, expired, nil, 100)))
	acquire(t, s, "expired", "w1", "b1", t0)
	_, _, _, err = s.StopSession(ctx, "expired", t0+1, "")
	must(t, err)
	mustIs(t, s.DeleteProject(ctx, f.user.ID, "expired-project", t0+ttl), store.ErrHasSessions)
	must(t, s.CheckSessionDeletable(ctx, "expired", t0+ttl))
	must(t, s.DeleteSession(ctx, "expired", t0+ttl))
	must(t, s.DeleteProject(ctx, f.user.ID, "expired-project", t0+ttl))
	mustIs(t, s.CheckSessionDeletable(ctx, "expired", t0+ttl), store.ErrNotFound)
	mustIs(t, s.CheckSessionDeletable(ctx, "keep", 0), store.ErrInvalid)
	mustIs(t, s.CheckSessionDeletable(ctx, "", t0), store.ErrInvalid)
}

// testSessionWebReads covers what the user-facing API reads and records
// beyond the worker link: the Web user filter, main-thread working
// directories and the view event of an input.
func testSessionWebReads(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seedProjects(t, s)
	for _, p := range []store.Project{newProject("alice-1", f.alice.ID, "One"), newProject("alice-2", f.alice.ID, "Two"), newProject("bob-1", f.bob.ID, "Bob")} {
		_, err := s.CreateProject(ctx, p)
		must(t, err)
	}
	for i, pair := range [][2]string{{"a1", "alice-1"}, {"a2", "alice-2"}, {"b1", "bob-1"}, {"m1", "session-project"}, {"a3", "alice-1"}} {
		sess := newSession(pair[0], f.fixture)
		sess.ProjectID, sess.CreatedAt = pair[1], t0+int64(i)
		switch pair[0][0] {
		case 'a':
			sess.UserID, sess.DefaultDeviceID = f.alice.ID, "alice-laptop"
		case 'b':
			sess.UserID, sess.DefaultDeviceID = f.bob.ID, "d9"
		}
		must(t, errOf(s.CreateSession(ctx, sess, nil, 100)))
	}
	ids := func(filter store.SessionFilter) []string {
		t.Helper()
		filter.Now, filter.Limit = t0, 10
		rows, err := s.ListSessions(ctx, filter)
		must(t, err)
		var out []string
		for _, v := range rows {
			out = append(out, v.ID)
		}
		return out
	}
	mustEqual(t, "alice", ids(store.SessionFilter{WebUserID: f.alice.ID}), []string{"a3", "a2", "a1"})
	mustEqual(t, "bob", ids(store.SessionFilter{WebUserID: f.bob.ID}), []string{"b1"})
	mustEqual(t, "seeded user", ids(store.SessionFilter{WebUserID: f.user.ID}), []string{"m1"})
	mustEqual(t, "nobody", ids(store.SessionFilter{WebUserID: "nobody"}), []string(nil))
	mustEqual(t, "with project", ids(store.SessionFilter{WebUserID: f.alice.ID, ProjectID: "alice-1"}), []string{"a3", "a1"})
	mustEqual(t, "foreign project", ids(store.SessionFilter{WebUserID: f.alice.ID, ProjectID: "bob-1"}), []string(nil))
	mustEqual(t, "keyset", ids(store.SessionFilter{WebUserID: f.alice.ID, After: &store.SessionCursor{LastActivityAt: t0 + 4, ID: "a3"}}), []string{"a2", "a1"})
	_, err := s.ListSessions(ctx, store.SessionFilter{WebUserID: strings.Repeat("x", store.MaxIDBytes+1), Now: t0, Limit: 1})
	mustIs(t, err, store.ErrInvalid)

	// Working directories: only the main thread's, by device id.
	sess := newSession("cw", f.fixture)
	sess.UserID, sess.ProjectID, sess.DefaultDeviceID = f.alice.ID, "alice-1", "alice-laptop"
	must(t, s.CreateIdentity(ctx, store.Identity{Kind: store.KindWorker, ID: "alice-worker", UserID: f.alice.ID, Name: "worker", MachineID: "m1", CreatedAt: t0}))
	must(t, errOf(s.CreateSession(ctx, sess, []store.SessionCwd{{DeviceID: "d2", Cwd: `C:\work`, UpdatedAt: t0}, {DeviceID: "alice-laptop", Cwd: "/work", UpdatedAt: t0}}, 100)))
	fence := acquire(t, s, "cw", "alice-worker", "b1", t0)
	must(t, s.PatchSessionRecords(ctx, fence, store.RecordsPatch{Cwds: []store.SessionCwd{{AgentID: "agent", DeviceID: "alice-laptop", Cwd: "/agent", UpdatedAt: t0 + 1}, {DeviceID: "alice-laptop", Cwd: "/moved", UpdatedAt: t0 + 1}}}))
	cwds, err := s.ListSessionCwds(ctx, "cw")
	must(t, err)
	mustEqual(t, "main thread cwds", cwds, []store.SessionCwd{{DeviceID: "alice-laptop", Cwd: "/moved", UpdatedAt: t0 + 1}, {DeviceID: "d2", Cwd: `C:\work`, UpdatedAt: t0}})
	cwds, err = s.ListSessionCwds(ctx, "a1")
	must(t, err)
	mustEqual(t, "no cwds", len(cwds), 0)
	_, err = s.ListSessionCwds(ctx, "missing")
	mustIs(t, err, store.ErrNotFound)

	// The input's view event, remembered for a repeated submission.
	in := newInput("a1", 1)
	mustEqual(t, "new input", submit(t, s, in).Input.EventSeq, int64(0))
	mustIs(t, s.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		if err := tx.SetInputEventSeq(ctx, "a1", in.InputID, 7); err != nil {
			return err
		}
		return errRollback
	}), errRollback)
	mustEqual(t, "rolled back", submit(t, s, in).Input.EventSeq, int64(0))
	must(t, s.SetInputEventSeq(ctx, "a1", in.InputID, 7))
	again := submit(t, s, in)
	mustEqual(t, "repeat", []any{again.Created, again.Input.EventSeq}, []any{false, int64(7)})
	mustIs(t, s.SetInputEventSeq(ctx, "a2", in.InputID, 8), store.ErrNotFound)
	mustIs(t, s.SetInputEventSeq(ctx, "a1", "missing", 8), store.ErrNotFound)
	mustIs(t, s.SetInputEventSeq(ctx, "a1", in.InputID, -1), store.ErrInvalid)
	// The seq survives the input's persistence.
	fence = acquire(t, s, "a1", "alice-worker", "b1", t0+2)
	taken, err := s.TakeInputs(ctx, at(fence, t0+2))
	must(t, err)
	mustEqual(t, "taken", []any{len(taken), taken[0].EventSeq}, []any{1, int64(7)})
}
