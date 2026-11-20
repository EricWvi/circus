package storetest

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/EricWvi/circus/internal/store"
)

// testSessionRecords: working directories, file records, jobs and
// background tasks, each at its cap.
func testSessionRecords(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	createSession(t, s, f, "s1")
	fence := acquire(t, s, "s1", "w1", "b1", t0)
	get := func(sec store.RecordSections) store.SessionRecords {
		t.Helper()
		r, err := s.GetSessionRecords(ctx, fence, records(sec))
		must(t, err)
		return r
	}

	// Working directories: exactly the cap, then one more key is refused
	// and the whole patch leaves nothing.
	var cwds []store.SessionCwd
	for i := range store.MaxSessionCwds {
		cwds = append(cwds, store.SessionCwd{AgentID: fmt.Sprint("a", i), DeviceID: "d1", Cwd: "/x"})
	}
	must(t, s.PatchSessionRecords(ctx, fence, store.RecordsPatch{Cwds: cwds}))
	mustIs(t, s.PatchSessionRecords(ctx, fence, store.RecordsPatch{
		Cwds: []store.SessionCwd{{AgentID: "a0", DeviceID: "d1", Cwd: "/changed"}, {AgentID: "new", DeviceID: "d1", Cwd: "/y"}},
	}), store.ErrLimit)
	r := get(store.RecordCwds)
	mustEqual(t, "cwds", []any{len(r.Cwds), r.Cwds[0].Cwd}, []any{store.MaxSessionCwds, "/x"})
	// Updating an existing key is not a new row.
	must(t, s.PatchSessionRecords(ctx, fence, store.RecordsPatch{Cwds: []store.SessionCwd{{AgentID: "a0", DeviceID: "d1", Cwd: "/changed", UpdatedAt: t0 + 3}}}))
	mustEqual(t, "updated cwd", get(store.RecordCwds).Cwds[0], store.SessionCwd{AgentID: "a0", DeviceID: "d1", Cwd: "/changed", UpdatedAt: t0 + 3})

	// Files: past the cap the least recently touched record is evicted.
	sum := make([]byte, 32)
	var files []store.SessionFile
	for i := range store.MaxSessionFiles {
		files = append(files, store.SessionFile{DeviceID: "d1", Path: fmt.Sprintf("/f%05d", i), SHA256: sum, MtimeMs: 1, TouchedAt: t0 + int64(i)})
	}
	for i := 0; i < len(files); i += store.MaxPatchItems {
		must(t, s.PatchSessionRecords(ctx, fence, store.RecordsPatch{Files: files[i:min(i+store.MaxPatchItems, len(files))]}))
	}
	mustEqual(t, "files at cap", len(get(store.RecordFiles).Files), store.MaxSessionFiles)
	must(t, s.PatchSessionRecords(ctx, fence, store.RecordsPatch{Files: []store.SessionFile{
		{DeviceID: "d1", Path: "/new", SHA256: sum, MtimeMs: 2, TouchedAt: t0 + 100_000}}}))
	r = get(store.RecordFiles)
	mustEqual(t, "oldest evicted", []any{len(r.Files), r.Files[0].Path}, []any{store.MaxSessionFiles, "/f00001"})
	must(t, s.PatchSessionRecords(ctx, fence, store.RecordsPatch{Files: []store.SessionFile{{DeviceID: "d1", Path: "/f00005"}}}))
	mustEqual(t, "deleted", len(get(store.RecordFiles).Files), store.MaxSessionFiles-1)
	must(t, s.PatchSessionRecords(ctx, fence, store.RecordsPatch{Files: []store.SessionFile{{DeviceID: "d1", Path: "/new2", SHA256: sum, TouchedAt: t0 + 100_001}}}))
	r = get(store.RecordFiles)
	mustEqual(t, "no eviction at the cap", []any{len(r.Files), r.Files[0].Path}, []any{store.MaxSessionFiles, "/f00001"})

	// Jobs: registered by the starting call, which must be the session's
	// and dispatched; exactly the cap, then one more is refused.
	must(t, s.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		for i := range store.MaxSessionJobs + 1 {
			sessionCall(t, tx, f, fmt.Sprint("c", i), "s1", store.CallCompleted)
		}
		return nil
	}))
	var jobs []store.SessionJob
	for i := range store.MaxSessionJobs {
		jobs = append(jobs, store.SessionJob{CallID: fmt.Sprint("c", i), JobID: fmt.Sprint("j", i), Description: "d"})
	}
	must(t, s.PatchSessionRecords(ctx, fence, store.RecordsPatch{Jobs: jobs[:store.MaxPatchItems]}))
	must(t, s.PatchSessionRecords(ctx, fence, store.RecordsPatch{Jobs: jobs[store.MaxPatchItems:]}))
	extra := store.SessionJob{CallID: fmt.Sprint("c", store.MaxSessionJobs), JobID: "extra"}
	mustIs(t, s.PatchSessionRecords(ctx, fence, store.RecordsPatch{Jobs: []store.SessionJob{extra}}), store.ErrLimit)
	sessionCall(t, s, f, "foreign", "other-session", store.CallCompleted)
	sessionCall(t, s, f, "undispatched", "s1", store.CallAccepted)
	for _, id := range []string{"foreign", "undispatched", "ghost"} {
		mustIs(t, s.PatchSessionRecords(ctx, fence, store.RecordsPatch{Jobs: []store.SessionJob{{CallID: id, JobID: "j"}}}), store.ErrNotFound)
	}
	_, err := s.PutJob(ctx, store.Job{DeviceID: "d1", BootID: "b1", JobID: "j0", UserID: "u1", State: store.JobExited, ExitCode: ptr(int32(3)), LastSeq: 1, EndedAt: t0 + 9})
	must(t, err)
	r = get(store.RecordJobs)
	mustEqual(t, "job from calls row and jobs row", r.Jobs[0], store.SessionJob{CallID: "c0", DeviceID: "d1", BootID: "b1", JobID: "j0",
		Description: "d", State: store.JobExited, ExitCode: ptr(int32(3)), EndedAt: t0 + 9, Notice: store.JobNoticeNo, CreatedAt: t0})
	mustEqual(t, "job without a jobs row", r.Jobs[1].State, store.JobState(""))

	// An ended job not yet handed is pending work.
	_, err = s.ReleaseLease(ctx, fence)
	must(t, err)
	mustEqual(t, "queued for the job", getSession(t, s, "s1").State, store.SessionQueued)
	fence = acquire(t, s, "s1", "w1", "b1", t0+1)
	// Handed, then persisted by a turn, then purged once old enough.
	must(t, s.PatchSessionRecords(ctx, fence, store.RecordsPatch{HandedJobCalls: []string{"c0"}}))
	mustEqual(t, "handed", get(store.RecordJobs).Jobs[0].Notice, store.JobNoticeHanded)
	report(t, s, at(fence, t0+2), store.TurnReport{LocalSeq: 1, Phase: store.TurnEnded, Persisted: store.TurnPersisted{JobCallIDs: []string{"c0", "c1"}}})
	r = get(store.RecordJobs)
	mustEqual(t, "persisted", []any{r.Jobs[0].Notice, r.Jobs[0].NoticeAt, r.Jobs[1].Notice}, []any{store.JobNoticePersisted, t0 + 2, store.JobNoticePersisted})
	n, err := s.PurgeSessionJobs(ctx, t0+1, 10)
	must(t, err)
	mustEqual(t, "too recent", n, 0)
	n, err = s.PurgeSessionJobs(ctx, t0+2, 10)
	must(t, err)
	mustEqual(t, "purged (c1's job never ended)", n, 1)

	// Background tasks: exactly the cap, then one more; repeats are
	// harmless and ended tasks free room.
	var tasks []store.BgTask
	for i := range store.MaxSessionBgTasks {
		tasks = append(tasks, store.BgTask{TaskID: fmt.Sprint("t", i), Kind: "agent"})
	}
	must(t, s.PatchSessionRecords(ctx, fence, store.RecordsPatch{BgTasksStarted: tasks}))
	must(t, s.PatchSessionRecords(ctx, fence, store.RecordsPatch{BgTasksStarted: tasks[:1]}))
	mustIs(t, s.PatchSessionRecords(ctx, fence, store.RecordsPatch{BgTasksStarted: []store.BgTask{{TaskID: "more", Kind: "agent"}}}), store.ErrLimit)
	must(t, s.PatchSessionRecords(ctx, fence, store.RecordsPatch{BgTasksEnded: []string{"t0"}, BgTasksStarted: []store.BgTask{{TaskID: "more", Kind: "agent"}}}))
	mustEqual(t, "tasks", len(get(store.RecordBgTasks).BgTasks), store.MaxSessionBgTasks)

	// Returned calls keep their first time.
	must(t, s.PatchSessionRecords(ctx, at(fence, t0+5), store.RecordsPatch{ReturnedCallIDs: []string{"c2"}}))
	must(t, s.PatchSessionRecords(ctx, at(fence, t0+6), store.RecordsPatch{ReturnedCallIDs: []string{"c2", "foreign"}}))
	mustEqual(t, "returned", []int64{getCall(t, s, "c2").ReturnedAt, getCall(t, s, "foreign").ReturnedAt}, []int64{t0 + 5, 0})
}

// testSessionEvents: payload truncation, the view cap with boundary
// events, and the count cap with eviction.
func testSessionEvents(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	createSession(t, s, f, "s1")
	ev := func(kind string, n int, boundary bool) store.EventInput {
		return store.EventInput{Kind: kind, Format: "claude-sdk", Payload: []byte(strings.Repeat("x", n)), At: t0, Boundary: boundary}
	}
	appended, err := s.AppendEvents(ctx, "s1", nil, []store.EventInput{ev("a", store.MaxEventPayloadBytes, false), ev("b", store.MaxEventPayloadBytes+1, false)}, store.DefaultMaxViewBytes)
	must(t, err)
	mustEqual(t, "seqs", appended.Seqs, []int64{1, 2})
	page, err := s.ReadEvents(ctx, store.EventFilter{SessionID: "s1", Limit: 10, MaxBytes: store.MaxTranscriptPageBytes})
	must(t, err)
	mustEqual(t, "at the bound", []any{len(page.Events[0].Payload), page.Events[0].Truncated}, []any{store.MaxEventPayloadBytes, false})
	mustEqual(t, "over the bound", page.Events[1], store.SessionEvent{SessionID: "s1", Seq: 2, At: t0, Kind: "b", Format: "claude-sdk",
		PayloadBytes: store.MaxEventPayloadBytes + 1, Truncated: true})
	mustEqual(t, "view bytes", getSession(t, s, "s1").ViewBytes, int64(store.MaxEventPayloadBytes))

	// View cap: exactly the cap stored; past it only boundaries, bare.
	createSession(t, s, f, "s2")
	appended, err = s.AppendEvents(ctx, "s2", nil, []store.EventInput{ev("a", 60, false), ev("b", 40, false), ev("c", 1, false), ev("turn", 5, true), ev("d", 0, false)}, 100)
	must(t, err)
	mustEqual(t, "view cap seqs", appended.Seqs, []int64{1, 2, 0, 3, 0})
	sess := getSession(t, s, "s2")
	mustEqual(t, "view", []any{sess.ViewBytes, sess.ViewTruncated, sess.LastEventSeq}, []any{int64(100), true, int64(3)})
	page, err = s.ReadEvents(ctx, store.EventFilter{SessionID: "s2", AfterSeq: 2, Limit: 10, MaxBytes: 10})
	must(t, err)
	mustEqual(t, "bare boundary", []any{page.Events[0].Kind, page.Events[0].Payload, page.Events[0].Truncated, page.Missed}, []any{"turn", []byte(nil), true, false})
	// Byte budget: at least one event.
	page, err = s.ReadEvents(ctx, store.EventFilter{SessionID: "s2", Limit: 10, MaxBytes: 50})
	must(t, err)
	mustEqual(t, "one oversized event", len(page.Events), 1)
	page, err = s.ReadEvents(ctx, store.EventFilter{SessionID: "s2", Limit: 10, MaxBytes: 100})
	must(t, err)
	mustEqual(t, "budget", len(page.Events), 3)

	// Count cap: exactly MaxSessionEvents kept, the oldest evicted.
	createSession(t, s, f, "s3")
	appendN := func(total int) {
		t.Helper()
		must(t, s.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
			for done := 0; done < total; {
				var batch []store.EventInput
				for len(batch) < store.MaxEventBatch && done+len(batch) < total {
					batch = append(batch, ev("e", 1, false))
				}
				if _, err := tx.AppendEvents(ctx, "s3", nil, batch, store.DefaultMaxViewBytes); err != nil {
					return err
				}
				done += len(batch)
			}
			return nil
		}))
	}
	oldest := func() int64 {
		t.Helper()
		page, err := s.ReadEvents(ctx, store.EventFilter{SessionID: "s3", Limit: 1, MaxBytes: 100})
		must(t, err)
		return page.OldestSeq
	}
	appendN(store.MaxSessionEvents)
	mustEqual(t, "at the cap nothing is evicted", oldest(), int64(1))
	appendN(1)
	mustEqual(t, "one over evicts one", oldest(), int64(2))
	appendN(9)
	total := store.MaxSessionEvents + 10
	sess = getSession(t, s, "s3")
	mustEqual(t, "after eviction", []any{sess.LastEventSeq, sess.ViewBytes}, []any{int64(total), int64(store.MaxSessionEvents)})
	page, err = s.ReadEvents(ctx, store.EventFilter{SessionID: "s3", Limit: 2, MaxBytes: 100})
	must(t, err)
	mustEqual(t, "from the oldest kept", []any{page.Missed, page.OldestSeq, page.Events[0].Seq}, []any{true, int64(11), int64(11)})
	page, err = s.ReadEvents(ctx, store.EventFilter{SessionID: "s3", AfterSeq: 10, Limit: 2, MaxBytes: 100})
	must(t, err)
	mustEqual(t, "nothing missed", page.Missed, false)
	page, err = s.ReadEvents(ctx, store.EventFilter{SessionID: "s3", AfterSeq: int64(total), Limit: 2, MaxBytes: 100})
	must(t, err)
	mustEqual(t, "caught up", []any{len(page.Events), page.Missed}, []any{0, false})
	_, err = s.ReadEvents(ctx, store.EventFilter{SessionID: "ghost", Limit: 2, MaxBytes: 100})
	mustIs(t, err, store.ErrNotFound)
}

// testSessionApprovals: dedup, the pending cap, resolution writing the
// grant and the retry input in one transaction, single consumption.
func testSessionApprovals(t *testing.T, b Backend) {
	s, dir := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	createSession(t, s, f, "s1")
	fence := at(acquire(t, s, "s1", "w1", "b1", t0), t0+1)
	key := store.GrantKey{Tool: "bash", DeviceID: "d1", MatchKey: mk("make deploy")}

	req, err := s.RequestApproval(ctx, fence, newApproval("a1", "make deploy"))
	must(t, err)
	mustEqual(t, "created", []any{req.Created, req.Approval.State, req.Approval.UserID, req.Approval.SessionID}, []any{true, store.ApprovalPending, "u1", "s1"})
	dup, err := s.RequestApproval(ctx, fence, newApproval("a1-again", "make deploy"))
	must(t, err)
	mustEqual(t, "dedup", []any{dup.Created, dup.Approval}, []any{false, req.Approval})

	// Approval is atomic: with the input queue full, nothing is written.
	for n := 1; n <= store.MaxOpenInputs; n++ {
		submit(t, s, newInput("s1", n))
	}
	resolution := store.ApprovalResolution{ApprovalID: "a1", Approve: true, At: t0 + 10, GrantExpiresAt: t0 + 600_000,
		Input: store.Input{InputID: "retry-a1", Text: "The user approved `make deploy` on laptop. Retry it now."}}
	_, err = s.ResolveApproval(ctx, resolution)
	mustIs(t, err, store.ErrLimit)
	a, err := s.GetApproval(ctx, "a1")
	must(t, err)
	mustEqual(t, "still pending", a.State, store.ApprovalPending)
	ok, err := s.ConsumeGrant(ctx, fence, key, "tu-early")
	must(t, err)
	mustEqual(t, "no grant", ok, false)

	taken, err := s.TakeInputs(ctx, fence)
	must(t, err)
	ids := inputIDs(taken)
	report(t, s, fence, store.TurnReport{LocalSeq: 1, Phase: store.TurnEnded, Persisted: store.TurnPersisted{InputIDs: ids}})
	res, err := s.ResolveApproval(ctx, resolution)
	must(t, err)
	mustEqual(t, "approved", []any{res.Changed, res.Approval.State, res.Approval.ResolvedAt, res.Input.Kind, res.Input.ClientMsgID, res.Input.State},
		[]any{true, store.ApprovalApproved, t0 + 10, store.InputApproval, "approval:a1", store.InputPending})
	again, err := s.ResolveApproval(ctx, store.ApprovalResolution{ApprovalID: "a1", At: t0 + 11})
	must(t, err)
	mustEqual(t, "resolved twice", []any{again.Changed, again.Approval.State}, []any{false, store.ApprovalApproved})
	recs, err := s.GetSessionRecords(ctx, fence, records(store.RecordGrants))
	must(t, err)
	mustEqual(t, "grant", recs.Grants, []store.Grant{{GrantID: "a1", SessionID: "s1", Key: key, CreatedAt: t0 + 10, ExpiresAt: t0 + 600_000}})

	// Consumed once, even by two handles at the same moment. Only this race
	// could hang (two handles on one database), so only it has a deadline;
	// the rest of the test runs hundreds of writes and is not timed.
	other := reopen(t, b, dir)
	raceCtx := deadline(t)
	var wg sync.WaitGroup
	wins := make([]bool, 2)
	for i, h := range []store.Store{s, other} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			must(t, retryBusy(func() error {
				var err error
				wins[i], err = h.ConsumeGrant(raceCtx, fence, key, fmt.Sprint("tu-race", i))
				return err
			}))
		}()
	}
	wg.Wait()
	mustEqual(t, "one consumer", wins[0] != wins[1], true)

	// Deny; expiry of approvals and grants.
	_, err = s.RequestApproval(ctx, fence, newApproval("a2", "rm x"))
	must(t, err)
	res, err = s.ResolveApproval(ctx, store.ApprovalResolution{ApprovalID: "a2", At: t0 + 12})
	must(t, err)
	mustEqual(t, "denied", []any{res.Approval.State, res.Input}, []any{store.ApprovalDenied, (*store.Input)(nil)})
	a3 := newApproval("a3", "short")
	a3.ExpiresAt = t0 + 50
	_, err = s.RequestApproval(ctx, fence, a3)
	must(t, err)
	_, err = s.ResolveApproval(ctx, store.ApprovalResolution{ApprovalID: "a3", Approve: true, At: t0 + 13, GrantExpiresAt: t0 + 60,
		Input: store.Input{InputID: "retry-a3", Text: "retry"}})
	must(t, err)
	ok, err = s.ConsumeGrant(ctx, at(fence, t0+60), store.GrantKey{Tool: "bash", DeviceID: "d1", MatchKey: mk("short")}, "tu-short")
	must(t, err)
	mustEqual(t, "expired grant", ok, false)
	a4 := newApproval("a4", "later")
	a4.ExpiresAt = t0 + 70
	_, err = s.RequestApproval(ctx, fence, a4)
	must(t, err)
	exp, err := s.ExpireApprovals(ctx, t0+70, 10)
	must(t, err)
	mustEqual(t, "expired", []any{len(exp), exp[0].ApprovalID, exp[0].State}, []any{1, "a4", store.ApprovalExpired})

	// The pending cap: the oldest pending one expires to make room.
	for i := range store.MaxPendingApprovals {
		_, err := s.RequestApproval(ctx, fence, newApproval(fmt.Sprint("p", i), fmt.Sprint("cmd", i)))
		must(t, err)
	}
	more, err := s.RequestApproval(ctx, fence, newApproval("p-more", "cmd-more"))
	must(t, err)
	mustEqual(t, "oldest expired", []any{more.Expired.ApprovalID, more.Expired.State}, []any{"p0", store.ApprovalExpired})
	pending, err := s.ListApprovals(ctx, store.ApprovalFilter{UserID: "u1", States: []store.ApprovalState{store.ApprovalPending}, Limit: store.MaxPageSize, MaxBytes: store.MaxTranscriptPageBytes})
	must(t, err)
	mustEqual(t, "pending", len(pending), store.MaxPendingApprovals)

	// The grant cap: exactly the cap, then the grant expiring first makes
	// room for one more.
	turn := int64(10)
	approve := func(id string, expires int64) {
		t.Helper()
		r := store.ApprovalResolution{ApprovalID: id, Approve: true, At: t0 + 100, GrantExpiresAt: expires,
			Input: store.Input{InputID: "retry-" + id, Text: "retry"}}
		_, err := s.ResolveApproval(ctx, r)
		if err == nil {
			return
		}
		// The open-input cap counts the retries too; persist them as a
		// turn would, then resolve again.
		mustIs(t, err, store.ErrLimit)
		taken, err := s.TakeInputs(ctx, fence)
		must(t, err)
		turn++
		report(t, s, fence, store.TurnReport{LocalSeq: turn, Phase: store.TurnEnded, Persisted: store.TurnPersisted{InputIDs: inputIDs(taken)}})
		_, err = s.ResolveApproval(ctx, r)
		must(t, err)
	}
	grantIDs := func() []string {
		t.Helper()
		recs, err := s.GetSessionRecords(ctx, at(fence, t0+100), records(store.RecordGrants))
		must(t, err)
		var ids []string
		for _, g := range recs.Grants {
			ids = append(ids, g.GrantID)
		}
		return ids
	}
	// pending is in (CreatedAt, ApprovalID) order: p-more first.
	for i, p := range pending {
		approve(p.ApprovalID, t0+1000+int64(i))
	}
	ids = grantIDs()
	mustEqual(t, "grants at the cap", []any{len(ids), ids[0]}, []any{store.MaxSessionGrants, "p-more"})
	_, err = s.RequestApproval(ctx, fence, newApproval("g-extra", "extra"))
	must(t, err)
	approve("g-extra", t0+2000)
	ids = grantIDs()
	mustEqual(t, "grants past the cap: the one expiring first is gone",
		[]any{len(ids), slices.Contains(ids, "p-more"), slices.Contains(ids, "p1"), slices.Contains(ids, "g-extra")},
		[]any{store.MaxSessionGrants, false, true, true})

	// A stopped session takes no new approvals or resolutions.
	_, err = s.RequestApproval(ctx, fence, newApproval("q1", "q"))
	must(t, err)
	_, _, _, err = s.StopSession(ctx, "s1", t0+200, "")
	must(t, err)
	_, err = s.RequestApproval(ctx, fence, newApproval("q2", "q2"))
	mustIs(t, err, store.ErrStopped)
	_, err = s.ResolveApproval(ctx, store.ApprovalResolution{ApprovalID: "q1", At: t0 + 201})
	mustIs(t, err, store.ErrStopped)
}

// testSessionAdmit: admission checks the fence, the session state and the
// active-call backstop at its boundary, on every router path: a new call,
// an attach to an existing one, and a lost call's resubmission.
func testSessionAdmit(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	createSession(t, s, f, "s1")
	createSession(t, s, f, "s2")
	fence := acquire(t, s, "s1", "w1", "b1", t0)
	const maxActive = 4
	for i := range maxActive - 1 {
		sessionCall(t, s, f, fmt.Sprint("c", i), "s1", store.CallRunning)
	}
	sessionCall(t, s, f, "done", "s1", store.CallCompleted)
	sessionCall(t, s, f, "elsewhere", "s2", store.CallAccepted)
	must(t, s.AdmitSessionCall(ctx, fence, "new", maxActive))
	sessionCall(t, s, f, "last", "s1", store.CallAccepted)
	mustIs(t, s.AdmitSessionCall(ctx, fence, "new", maxActive), store.ErrTooManyCalls)
	// Attaching to an existing call does not count it against itself; a
	// call of another session is not this session's.
	must(t, s.AdmitSessionCall(ctx, fence, "last", maxActive))
	mustIs(t, s.AdmitSessionCall(ctx, fence, "elsewhere", maxActive), store.ErrNotFound)
	// unknown is not terminal: it still counts.
	if !transition(t, s, "c0", expect(store.CallRunning), store.CallUnknown, store.CallPatch{}) {
		t.Fatal("c0 to unknown")
	}
	mustIs(t, s.AdmitSessionCall(ctx, fence, "new", maxActive), store.ErrTooManyCalls)
	if !transition(t, s, "last", expect(store.CallAccepted), store.CallCancelled, store.CallPatch{FinishedAt: t0 + 1}) {
		t.Fatal("cancel last")
	}

	// The resubmission of a lost call runs admission in the same Tx.
	sessionCall(t, s, f, "lostc", "s1", store.CallLost)
	resubmit := func(fe store.Fence) error {
		return s.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
			if err := tx.AdmitSessionCall(ctx, fe, "lostc", maxActive); err != nil {
				return err
			}
			if !transition(t, tx, "lostc", store.CallExpect{States: []store.CallState{store.CallLost}, Undispatched: true},
				store.CallAccepted, store.CallPatch{SubmittedAt: t0 + 2}) {
				t.Fatal("resubmit lostc")
			}
			return nil
		})
	}
	stale := fence
	stale.ConnID = "old-conn"
	mustFenced(t, resubmit(stale), fence.Epoch)
	mustEqual(t, "fenced resubmit", getCall(t, s, "lostc").State, store.CallLost)
	must(t, resubmit(fence))
	mustEqual(t, "resubmitted", getCall(t, s, "lostc").State, store.CallAccepted)

	// Broken and stopped sessions admit nothing.
	report(t, s, fence, store.TurnReport{LocalSeq: 1, Phase: store.TurnEnded, TranscriptIncomplete: true})
	mustIs(t, s.AdmitSessionCall(ctx, fence, "new", 100), store.ErrBroken)
	sess, err := s.TranscriptRepaired(ctx, fence)
	must(t, err)
	mustEqual(t, "repaired", sess.State, store.SessionIdle)
	must(t, s.AdmitSessionCall(ctx, fence, "new", 100))
	_, _, _, err = s.StopSession(ctx, "s1", t0+3, "")
	must(t, err)
	mustIs(t, s.AdmitSessionCall(ctx, fence, "new", 100), store.ErrStopped)
}

// testSessionDelete: each precondition alone, the chunked delete of a
// transcript larger than one chunk, and a cascade that leaves nothing of
// the session, its calls, its ended device jobs or its bytes in the
// user's total.
func testSessionDelete(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	good := busySession(t, s, f)
	// More entries than one delete chunk.
	n := 2*store.DeleteChunkRows + 10
	must(t, s.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		for i := 0; i < n; i += 500 {
			var es []store.TranscriptEntry
			for j := i; j < min(i+500, n); j++ {
				es = append(es, store.TranscriptEntry{JSON: []byte("{}")})
			}
			if _, err := tx.AppendTranscript(ctx, good, store.TranscriptBatch{Subpath: "bulk", BatchSeq: int64(i/500 + 1), Entries: es}, quota); err != nil {
				return err
			}
		}
		return nil
	}))
	_, err := s.PutJob(ctx, store.Job{DeviceID: "d1", BootID: "b1", JobID: "j-ended", UserID: "u1", SessionID: "s1", State: store.JobExited, LastSeq: 1})
	must(t, err)
	createSession(t, s, f, "keep")
	keep := acquire(t, s, "keep", "w1", "b1", t0)
	appendEntries(t, s, keep, "", 1, entry("k", `{"keep":1}`))
	sessionCall(t, s, f, "keep-call", "keep", store.CallCompleted)

	mustIs(t, s.DeleteSession(ctx, "s1", t0+2), store.ErrSessionActive) // not stopped
	_, _, _, err = s.StopSession(ctx, "s1", t0+2, "")
	must(t, err)
	if !transition(t, s, "c1", expect(store.CallDispatched), store.CallCompleted, store.CallPatch{FinishedAt: t0 + 2}) {
		t.Fatal("complete c1")
	}
	mustIs(t, s.DeleteSession(ctx, "s1", t0+2), store.ErrSessionActive) // only the live lease
	_, err = s.ReleaseLease(ctx, at(good, t0+3))
	must(t, err)
	sessionCall(t, s, f, "c-unknown", "s1", store.CallUnknown)
	mustIs(t, s.DeleteSession(ctx, "s1", t0+3), store.ErrSessionActive) // only an unknown call
	if !transition(t, s, "c-unknown", expect(store.CallUnknown), store.CallLost, store.CallPatch{FinishedAt: t0 + 3}) {
		t.Fatal("lose c-unknown")
	}
	// Inside a Tx it would hold the store for the whole delete.
	mustIs(t, s.Tx(ctx, func(ctx context.Context, tx store.Ops) error { return tx.DeleteSession(ctx, "s1", t0+4) }), store.ErrInvalid)
	must(t, s.DeleteSession(ctx, "s1", t0+4))
	mustIs(t, s.DeleteSession(ctx, "s1", t0+4), store.ErrNotFound) // done already

	_, err = s.GetSession(ctx, "s1", t0)
	mustIs(t, err, store.ErrNotFound)
	_, err = s.GetLease(ctx, "s1")
	mustIs(t, err, store.ErrNotFound)
	_, err = s.ReadEvents(ctx, store.EventFilter{SessionID: "s1", Limit: 1, MaxBytes: 1})
	mustIs(t, err, store.ErrNotFound)
	turns, err := s.ListTurns(ctx, store.TurnFilter{SessionID: "s1", Limit: 10})
	must(t, err)
	approvals, err := s.ListApprovals(ctx, store.ApprovalFilter{SessionID: "s1", Limit: 10, MaxBytes: store.MaxTranscriptPageBytes})
	must(t, err)
	calls, err := s.ListCalls(ctx, store.CallFilter{SessionID: "s1", Limit: 10})
	must(t, err)
	jobs, err := s.ListJobs(ctx, store.JobFilter{SessionID: "s1", Limit: 10})
	must(t, err)
	mustEqual(t, "rows left", []int{len(turns), len(approvals), len(calls)}, []int{0, 0, 0})
	// The running device job stays until it ends; the ended one is gone.
	mustEqual(t, "device jobs left", []any{len(jobs), jobs[0].JobID}, []any{1, "j1"})
	u, err := s.GetUser(ctx, "u1")
	must(t, err)
	mustEqual(t, "user bytes", u.TranscriptBytes, store.TranscriptCharge(entry("k", `{"keep":1}`)))
	// The other session is untouched; a new session may reuse the ids,
	// and finds nothing of the old one.
	mustEqual(t, "other call", getCall(t, s, "keep-call").SessionID, "keep")
	createSession(t, s, f, "s1")
	fresh := acquire(t, s, "s1", "w1", "b1", t0+5)
	snap := snapSession(t, s, fresh)
	mustEqual(t, "fresh session", []int{len(snap.Inputs), len(snap.Transcript.Entries), len(snap.Subpaths), len(snap.Records.Cwds),
		len(snap.Records.Files), len(snap.Records.Jobs), len(snap.Records.Grants), len(snap.Events.Events)}, []int{0, 0, 0, 0, 0, 0, 0, 0})
	mustEqual(t, "epoch starts over", fresh.Epoch, int64(1))
	mustIs(t, s.DeleteSession(ctx, "ghost", t0), store.ErrNotFound)
}

// testSessionUnitAtomic: a session method that fails after some of its
// writes leaves none of them, even inside a Tx whose fn ignores the error
// and commits, and an injected failure leaves nothing either.
func testSessionUnitAtomic(t *testing.T, b Backend) {
	inner, _ := open(t, b)
	s := NewFaulty(inner)
	ctx := deadline(t)
	f := seed(t, s)
	createSession(t, s, f, "s1")
	fence := acquire(t, s, "s1", "w1", "b1", t0)
	var cwds []store.SessionCwd
	for i := range store.MaxSessionCwds + 1 {
		cwds = append(cwds, store.SessionCwd{AgentID: fmt.Sprint("a", i), DeviceID: "d1", Cwd: "/x"})
	}
	must(t, s.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		mustIs(t, tx.PatchSessionRecords(ctx, fence, store.RecordsPatch{Cwds: cwds}), store.ErrLimit)
		_, err := tx.AppendTranscript(ctx, fence, store.TranscriptBatch{BatchSeq: 1, Entries: []store.TranscriptEntry{entry("u", "{}")}}, quota)
		return err
	}))
	recs, err := s.GetSessionRecords(ctx, fence, records(store.RecordCwds))
	must(t, err)
	mustEqual(t, "no cwd of the failed patch", len(recs.Cwds), 0)
	has, err := s.HasTranscriptUUID(ctx, fence, "u")
	must(t, err)
	mustEqual(t, "the rest of the Tx committed", has, true)

	// An injected failure of a session write reaches nothing.
	s.FailWrite(1, store.ErrBusy)
	_, err = s.AppendTranscript(ctx, fence, store.TranscriptBatch{BatchSeq: 2, Entries: []store.TranscriptEntry{entry("v", "{}")}}, quota)
	mustIs(t, err, store.ErrBusy)
	has, err = s.HasTranscriptUUID(ctx, fence, "v")
	must(t, err)
	mustEqual(t, "failed append", has, false)
	// The retry of the same batch is not taken for a duplicate.
	r := appendEntries(t, s, fence, "", 2, entry("v", "{}"))
	mustEqual(t, "retried batch", r.Inserted, 1)
}

func testSessionCallsAndJobsByOwner(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	createSession(t, s, f, "s")
	fence := acquire(t, s, "s", "w1", "boot", t0)
	other := fixture{
		user:   store.User{ID: "other", Email: "other@example.com", Role: store.RoleUser, CreatedAt: t0},
		device: store.Identity{ID: "other-device", Kind: store.KindDevice, UserID: "other", Name: "desk", MachineID: "m", CreatedAt: t0},
	}
	must(t, s.CreateUser(ctx, other.user))
	must(t, s.CreateIdentity(ctx, other.device))
	must(t, s.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		for i := range store.MaxUnpersistedSessionCalls {
			sessionCall(t, tx, other, fmt.Sprintf("other-%d", i), "s", store.CallRunning)
		}
		return nil
	}))
	must(t, s.AdmitSessionCall(ctx, fence, "next", 1))
	must(t, s.AdmitSessionCall(ctx, fence, "next", store.MaxUnpersistedSessionCalls+1))
	mustIs(t, s.AdmitSessionCall(ctx, fence, "other-0", store.MaxUnpersistedSessionCalls+1), store.ErrNotFound)
	mustIs(t, s.PatchSessionRecords(ctx, fence, store.RecordsPatch{Jobs: []store.SessionJob{{CallID: "other-0", JobID: "foreign"}}}), store.ErrNotFound)
	must(t, s.PatchSessionRecords(ctx, fence, store.RecordsPatch{ReturnedCallIDs: []string{"other-0"}, AwaitingCallIDs: []string{"other-0"}}))
	row, err := s.GetCall(ctx, "other-0")
	must(t, err)
	mustEqual(t, "foreign call remains unmarked", []any{row.ReturnedAt, row.NoticeOwed}, []any{int64(0), false})
	sessionCall(t, s, f, "mine", "s", store.CallCompleted)
	recovery, err := s.GetRecovery(ctx, fence)
	must(t, err)
	mustEqual(t, "recovery call count", len(recovery.Calls), 1)
	mustEqual(t, "recovery call", recovery.Calls[0].CallID, "mine")

	for _, user := range []fixture{f, other} {
		_, err := s.PutJob(ctx, store.Job{DeviceID: user.device.ID, UserID: user.user.ID,
			BootID: "boot", JobID: "ended", SessionID: "s", State: store.JobExited, LastSeq: 1})
		must(t, err)
	}
	_, _, _, err = s.StopSession(ctx, "s", t0+1, "test")
	must(t, err)
	_, err = s.ReleaseLease(ctx, at(fence, t0+1))
	must(t, err)
	must(t, s.CheckSessionDeletable(ctx, "s", t0+1))
	must(t, s.DeleteSession(ctx, "s", t0+1))
	_, err = s.GetCall(ctx, "mine")
	mustIs(t, err, store.ErrNotFound)
	_, err = s.GetCall(ctx, "other-0")
	must(t, err)
	jobs, err := s.ListJobs(ctx, store.JobFilter{SessionID: "s", Limit: 10})
	must(t, err)
	mustEqual(t, "remaining job count", len(jobs), 1)
	mustEqual(t, "remaining job owner", jobs[0].UserID, other.user.ID)
}
