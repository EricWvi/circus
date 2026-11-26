package storetest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/EricWvi/circus/internal/store"
)

// ttl is the lease time-to-live the session tests use.
const ttl = int64(60_000)

// quota is a transcript quota no test reaches by accident.
var quota = store.TranscriptQuota{MaxEntryBytes: 1 << 20, MaxSessionBytes: 1 << 24, MaxUserBytes: 1 << 26}

// errRollback ends a Tx used only to look, so nothing it did persists.
var errRollback = errors.New("storetest: rollback")

func newSession(id string, f fixture) store.Session {
	return store.Session{
		ID: id, ClientRequestID: id, UserID: f.user.ID, ProjectID: "session-project", ClaudeSessionID: "claude-" + id, DefaultDeviceID: f.device.ID,
		Model: "model", PermissionMode: "auto", Agent: store.AgentClaudeCode, CreatedAt: t0,
	}
}

func createSession(t testing.TB, ops store.Ops, f fixture, id string) {
	t.Helper()
	must(t, errOf(ops.CreateSession(context.Background(), newSession(id, f), nil, store.DefaultMaxSessionsPerUser)))
}

// addWorker adds a worker identity of the user.
func addWorker(t testing.TB, ops store.Ops, id, userID string) {
	t.Helper()
	must(t, ops.CreateIdentity(context.Background(), store.Identity{
		Kind: store.KindWorker, ID: id, UserID: userID, Name: "worker-" + id, MachineID: "m-" + id, CreatedAt: t0,
	}))
}

// acquire takes the session's lease at now for ttl, on connection
// boot+"-c", and returns the fence of the new holder.
func acquire(t testing.TB, ops store.Ops, sessionID, worker, boot string, now int64) store.Fence {
	t.Helper()
	return acquireOn(t, ops, sessionID, worker, boot, boot+"-c", now)
}

// acquireOn is acquire on connection conn.
func acquireOn(t testing.TB, ops store.Ops, sessionID, worker, boot, conn string, now int64) store.Fence {
	t.Helper()
	r, err := ops.AcquireLease(context.Background(), leaseReq(sessionID, worker, boot, conn, now))
	must(t, err)
	return store.Fence{SessionID: sessionID, Epoch: r.Lease.Epoch, WorkerID: worker, BootID: boot, ConnID: conn, Now: now}
}

func leaseReq(sessionID, worker, boot, conn string, now int64) store.LeaseRequest {
	return store.LeaseRequest{SessionID: sessionID, WorkerID: worker, BootID: boot, ConnID: conn, Now: now, ExpiresAt: now + ttl}
}

// allRecords reads every record section in one page.
var allRecords = store.RecordsRequest{Sections: store.RecordAll, MaxBytes: store.MaxTranscriptPageBytes}

func records(sec store.RecordSections) store.RecordsRequest {
	return store.RecordsRequest{Sections: sec, MaxBytes: store.MaxTranscriptPageBytes}
}

func at(f store.Fence, now int64) store.Fence {
	f.Now = now
	return f
}

func entry(uuid, body string) store.TranscriptEntry {
	return store.TranscriptEntry{UUID: uuid, JSON: []byte(body)}
}

func appendEntries(t testing.TB, ops store.Ops, f store.Fence, subpath string, batch int64, entries ...store.TranscriptEntry) store.TranscriptAppended {
	t.Helper()
	r, err := ops.AppendTranscript(context.Background(), f, store.TranscriptBatch{Subpath: subpath, BatchSeq: batch, Entries: entries}, quota)
	must(t, err)
	return r
}

func newInput(sessionID string, n int) store.Input {
	return store.Input{
		InputID: fmt.Sprintf("%s-in%d", sessionID, n), SessionID: sessionID, ClientMsgID: fmt.Sprintf("cm%d", n),
		Kind: store.InputUser, Text: fmt.Sprintf("hello %d", n), Priority: store.InputNormal, CreatedAt: t0 + int64(n),
	}
}

func submit(t testing.TB, ops store.Ops, in store.Input) store.InputSubmitted {
	t.Helper()
	r, err := ops.SubmitInput(context.Background(), in)
	must(t, err)
	return r
}

func report(t testing.TB, ops store.Ops, f store.Fence, r store.TurnReport) store.TurnReported {
	t.Helper()
	if r.At == 0 {
		r.At = f.Now
	}
	if r.Origin == "" {
		r.Origin = store.TurnFromInput
	}
	if r.Phase == store.TurnEnded && r.Outcome == "" {
		r.Outcome = store.TurnCompleted
	}
	if r.Phase == store.TurnEnded {
		// An ENDED needs its STARTED: for a turn not started in this epoch,
		// start the epoch's next one and end that instead.
		last, started := lastLocalSeq(t, ops, f, r.LocalSeq)
		if !started {
			r.LocalSeq = last + 1
			must(t, errOf(ops.ReportTurn(context.Background(), f, store.TurnReport{
				LocalSeq: r.LocalSeq, Phase: store.TurnStarted, Origin: r.Origin, At: r.At})))
		}
	}
	out, err := ops.ReportTurn(context.Background(), f, r)
	must(t, err)
	return out
}

// lastLocalSeq returns the highest local_seq of f's epoch among the
// session's kept turns, and whether seq is one of them.
func lastLocalSeq(t testing.TB, ops store.Ops, f store.Fence, seq int64) (last int64, found bool) {
	t.Helper()
	var after int64
	for {
		turns, err := ops.ListTurns(context.Background(), store.TurnFilter{SessionID: f.SessionID, AfterTurnNo: after, Limit: store.MaxPageSize})
		must(t, err)
		for _, tn := range turns {
			if tn.Epoch == f.Epoch {
				last = max(last, tn.LocalSeq)
				found = found || tn.LocalSeq == seq
			}
			after = tn.TurnNo
		}
		if len(turns) < store.MaxPageSize {
			return last, found
		}
	}
}

func getSession(t testing.TB, ops store.Ops, id string) store.Session {
	t.Helper()
	s, err := ops.GetSession(context.Background(), id, t0)
	must(t, err)
	return s
}

// mk is the match key of a command or path: its SHA-256 in lowercase hex.
func mk(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func newApproval(id, key string) store.Approval {
	return store.Approval{
		ApprovalID: id, ToolUseID: "tu-" + id, Tool: "bash", DeviceID: "d1", MatchKey: mk(key),
		Display: "run " + key, Reason: "classifier", CreatedAt: t0 + 5, ExpiresAt: t0 + 5 + 600_000,
	}
}

// sessionCall creates a call of the session in state, dispatched to boot b1
// when the path goes through dispatch.
func sessionCall(t testing.TB, ops store.Ops, f fixture, id, sessionID string, state store.CallState) {
	t.Helper()
	c := newCall(id, f)
	c.SessionID, c.ToolUseID = sessionID, "tu-"+id
	createIn(t, ops, c, state)
}

func mustFenced(t testing.TB, err error, epoch int64) {
	t.Helper()
	var fe *store.FencedError
	if !errors.As(err, &fe) || !errors.Is(err, store.ErrFenced) {
		t.Fatalf("error = %v, want *FencedError", err)
	}
	mustEqual(t, "fenced epoch", fe.Epoch, epoch)
}

// sessionSnap is everything a session write could change, read through
// the interface.
type sessionSnap struct {
	Session    store.Session
	Lease      store.Lease
	Inputs     []store.Input
	Transcript store.TranscriptPage
	Subpaths   []store.TranscriptSubpath
	Records    store.SessionRecords
	Turns      []store.Turn
	Approvals  []store.Approval
	Events     store.EventPage
	Calls      []store.Call
}

// snapSession reads the session's state with the holder's fence good,
// inside a Tx it rolls back (TakeInputs is a write).
func snapSession(t testing.TB, s store.Store, good store.Fence) sessionSnap {
	t.Helper()
	var snap sessionSnap
	err := s.Tx(context.Background(), func(ctx context.Context, tx store.Ops) error {
		var err error
		sid := good.SessionID
		if snap.Session, err = tx.GetSession(ctx, sid, t0); err != nil {
			return err
		}
		if snap.Lease, err = tx.GetLease(ctx, sid); err != nil {
			return err
		}
		if snap.Inputs, err = tx.TakeInputs(ctx, good); err != nil {
			return err
		}
		if snap.Transcript, err = tx.LoadTranscript(ctx, good, store.TranscriptPageRequest{Limit: 100, MaxBytes: 1 << 20}); err != nil {
			return err
		}
		if snap.Subpaths, err = tx.ListTranscriptSubpaths(ctx, good); err != nil {
			return err
		}
		if snap.Records, err = tx.GetSessionRecords(ctx, good, records(store.RecordAll)); err != nil {
			return err
		}
		if snap.Turns, err = tx.ListTurns(ctx, store.TurnFilter{SessionID: sid, Limit: store.MaxPageSize}); err != nil {
			return err
		}
		if snap.Approvals, err = tx.ListApprovals(ctx, store.ApprovalFilter{SessionID: sid, Limit: store.MaxPageSize, MaxBytes: store.MaxTranscriptPageBytes}); err != nil {
			return err
		}
		if snap.Events, err = tx.ReadEvents(ctx, store.EventFilter{SessionID: sid, Limit: store.MaxPageSize, MaxBytes: 1 << 20}); err != nil {
			return err
		}
		if snap.Calls, err = tx.ListCalls(ctx, store.CallFilter{SessionID: sid, Limit: store.MaxPageSize}); err != nil {
			return err
		}
		return errRollback
	})
	mustIs(t, err, errRollback)
	return snap
}

// busySession builds session s1 held by w1/b1 from t0 (epoch 1) with a
// turn running and something in every table, and returns the holder's
// fence at t0+1.
func busySession(t testing.TB, s store.Store, f fixture) store.Fence {
	t.Helper()
	ctx := context.Background()
	createSession(t, s, f, "s1")
	good := at(acquire(t, s, "s1", "w1", "b1", t0), t0+1)
	appendEntries(t, s, good, "", 1, entry("e1", `{"n":1}`))
	submit(t, s, newInput("s1", 1))
	sessionCall(t, s, f, "c1", "s1", store.CallDispatched)
	_, err := s.PutJob(ctx, store.Job{DeviceID: "d1", BootID: "b1", JobID: "j1", UserID: "u1", SessionID: "s1", CallID: "c1", State: store.JobRunning, LastSeq: 1})
	must(t, err)
	must(t, s.PatchSessionRecords(ctx, good, store.RecordsPatch{
		Cwds:  []store.SessionCwd{{DeviceID: "d1", Cwd: "/home"}},
		Files: []store.SessionFile{{DeviceID: "d1", Path: "/home/a", SHA256: make([]byte, 32), MtimeMs: 1}},
		Jobs:  []store.SessionJob{{CallID: "c1", JobID: "j1", Description: "sleep"}},
	}))
	_, err = s.RequestApproval(ctx, good, newApproval("a1", "rm -rf x"))
	must(t, err)
	_, err = s.ResolveApproval(ctx, store.ApprovalResolution{ApprovalID: "a1", Approve: true, At: t0 + 6,
		GrantExpiresAt: t0 + 600_000, Input: store.Input{InputID: "s1-approval", Text: "approved, retry"}})
	must(t, err)
	_, err = s.AppendEvents(ctx, "s1", nil, []store.EventInput{{Kind: "assistant", Payload: []byte(`{}`), At: t0 + 7}}, store.DefaultMaxViewBytes)
	must(t, err)
	report(t, s, good, store.TurnReport{LocalSeq: 1, Phase: store.TurnStarted})
	return good
}

// fencedMethod calls one fenced method with fence f, with arguments under
// which the holder's call succeeds.
type fencedMethod struct {
	name string
	call func(ctx context.Context, ops store.Ops, f store.Fence) error
}

var fencedMethods = []fencedMethod{
	{"TranscriptRepaired", func(ctx context.Context, ops store.Ops, f store.Fence) error {
		_, _, err := ops.TranscriptRepaired(ctx, f)
		return err
	}},
	{"GetRecovery", func(ctx context.Context, ops store.Ops, f store.Fence) error {
		_, err := ops.GetRecovery(ctx, f)
		return err
	}},
	{"AppendEvents", func(ctx context.Context, ops store.Ops, f store.Fence) error {
		_, err := ops.AppendEvents(ctx, f.SessionID, &store.EventPublish{Fence: f, BatchSeq: 1}, []store.EventInput{{Kind: "assistant", Payload: []byte(`{}`), At: f.Now}}, store.DefaultMaxViewBytes)
		return err
	}},
	{"ReleaseLease", func(ctx context.Context, ops store.Ops, f store.Fence) error {
		_, err := ops.ReleaseLease(ctx, f)
		return err
	}},
	{"ResumeLeases", func(ctx context.Context, ops store.Ops, f store.Fence) error {
		r, err := ops.ResumeLeases(ctx, f.WorkerID, f.BootID, f.ConnID, []store.SessionEpoch{{SessionID: f.SessionID, Epoch: f.Epoch}}, f.Now, f.Now+ttl)
		if err != nil {
			return err
		}
		return r[0].Err
	}},
	{"CheckFence", func(ctx context.Context, ops store.Ops, f store.Fence) error {
		_, err := ops.CheckFence(ctx, f)
		return err
	}},
	{"AdmitSessionCall", func(ctx context.Context, ops store.Ops, f store.Fence) error {
		return ops.AdmitSessionCall(ctx, f, "c-new", store.DefaultMaxActiveSessionCalls)
	}},
	{"AppendTranscript", func(ctx context.Context, ops store.Ops, f store.Fence) error {
		_, err := ops.AppendTranscript(ctx, f, store.TranscriptBatch{BatchSeq: 2, Entries: []store.TranscriptEntry{entry("e9", `{"n":9}`)}}, quota)
		return err
	}},
	{"LoadTranscript", func(ctx context.Context, ops store.Ops, f store.Fence) error {
		_, err := ops.LoadTranscript(ctx, f, store.TranscriptPageRequest{Limit: 10, MaxBytes: 1 << 20})
		return err
	}},
	{"ListTranscriptSubpaths", func(ctx context.Context, ops store.Ops, f store.Fence) error {
		_, err := ops.ListTranscriptSubpaths(ctx, f)
		return err
	}},
	{"HasTranscriptUUID", func(ctx context.Context, ops store.Ops, f store.Fence) error {
		_, err := ops.HasTranscriptUUID(ctx, f, "e1")
		return err
	}},
	{"TakeInputs", func(ctx context.Context, ops store.Ops, f store.Fence) error {
		_, err := ops.TakeInputs(ctx, f)
		return err
	}},
	{"ReportTurn", func(ctx context.Context, ops store.Ops, f store.Fence) error {
		_, err := ops.ReportTurn(ctx, f, store.TurnReport{LocalSeq: 1, Phase: store.TurnEnded, Outcome: store.TurnCompleted,
			At: f.Now, Persisted: store.TurnPersisted{InputIDs: []string{"s1-in1"}, CallIDs: []string{"c1"}}})
		return err
	}},
	{"PatchSessionRecords", func(ctx context.Context, ops store.Ops, f store.Fence) error {
		return ops.PatchSessionRecords(ctx, f, store.RecordsPatch{
			Cwds: []store.SessionCwd{{DeviceID: "d1", Cwd: "/tmp"}}, ReturnedCallIDs: []string{"c1"},
			HandedJobCalls: []string{"c1"}, BgTasksStarted: []store.BgTask{{TaskID: "t1", Kind: "agent"}},
		})
	}},
	{"GetSessionRecords", func(ctx context.Context, ops store.Ops, f store.Fence) error {
		_, err := ops.GetSessionRecords(ctx, f, records(store.RecordAll))
		return err
	}},
	{"RequestApproval", func(ctx context.Context, ops store.Ops, f store.Fence) error {
		_, err := ops.RequestApproval(ctx, f, newApproval("a9", "ls"))
		return err
	}},
	{"ConsumeGrant", func(ctx context.Context, ops store.Ops, f store.Fence) error {
		ok, err := ops.ConsumeGrant(ctx, f, store.GrantKey{Tool: "bash", DeviceID: "d1", MatchKey: mk("rm -rf x")}, "tu-consume")
		if err == nil && !ok {
			err = errors.New("grant not consumed")
		}
		return err
	}},
}

// testSessionFencing: a wrong epoch, holder or boot, or an expired lease,
// makes every fenced method, read or write, return *FencedError with the
// current epoch and change nothing; the holder's own call succeeds.
func testSessionFencing(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	addWorker(t, s, "w2", "u1")
	good := busySession(t, s, f)
	bad := []struct {
		name  string
		fence store.Fence
	}{
		{"epoch", store.Fence{SessionID: "s1", Epoch: good.Epoch + 1, WorkerID: "w1", BootID: "b1", ConnID: good.ConnID, Now: t0 + 1}},
		{"worker", store.Fence{SessionID: "s1", Epoch: good.Epoch, WorkerID: "w2", BootID: "b1", ConnID: good.ConnID, Now: t0 + 1}},
		{"boot", store.Fence{SessionID: "s1", Epoch: good.Epoch, WorkerID: "w1", BootID: "b2", ConnID: good.ConnID, Now: t0 + 1}},
		{"connection", store.Fence{SessionID: "s1", Epoch: good.Epoch, WorkerID: "w1", BootID: "b1", ConnID: "other-conn", Now: t0 + 1}},
		{"expired", at(good, t0+ttl)},
	}
	// Each call runs within its own deadline: a hang fails fast, while the
	// table (hundreds of calls) is not timed as a whole.
	for _, m := range fencedMethods {
		resume := m.name == "ResumeLeases"
		// The holder's call succeeds (rolled back), so the fence alone
		// decides the cases below.
		mustIs(t, within(t, func(ctx context.Context) error {
			return s.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
				if err := m.call(ctx, tx, good); err != nil {
					t.Fatalf("%s with the holder's fence: %v", m.name, err)
				}
				return errRollback
			})
		}), errRollback)
		for _, c := range bad {
			if resume && c.name == "connection" {
				continue // any connection of the boot may resume: see testLeaseConnection
			}
			before := snapSession(t, s, good)
			mustFenced(t, within(t, func(ctx context.Context) error { return m.call(ctx, s, c.fence) }), good.Epoch)
			// Inside a Tx whose fn ignores the error and commits.
			must(t, within(t, func(ctx context.Context) error {
				return s.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
					mustFenced(t, m.call(ctx, tx, c.fence), good.Epoch)
					return nil
				})
			}))
			mustEqual(t, label(m.name, " with a wrong ", c.name), snapSession(t, s, good), before)
		}
	}
	// A fence for a session that does not exist.
	_, err := s.CheckFence(ctx, store.Fence{SessionID: "ghost", Epoch: 1, WorkerID: "w1", BootID: "b1", ConnID: "c", Now: t0})
	mustIs(t, err, store.ErrNotFound)
	// A malformed fence is refused before anything is read.
	for _, bad := range []store.Fence{
		{SessionID: "s1", Epoch: 0, WorkerID: "w1", BootID: "b1", ConnID: "c", Now: t0},
		{SessionID: "s1", Epoch: 1, WorkerID: "w1", BootID: "b1", Now: t0},
		{SessionID: "s1", Epoch: 1, WorkerID: "w1", BootID: "b1", ConnID: strings.Repeat("c", store.MaxIDBytes+1), Now: t0},
	} {
		_, err = s.TakeInputs(ctx, bad)
		mustIs(t, err, store.ErrInvalid)
	}
	// A wrong epoch on a released lease is fenced too, not a retried
	// release.
	_, err = s.ReleaseLease(ctx, good)
	must(t, err)
	_, err = s.ReleaseLease(ctx, store.Fence{SessionID: "s1", Epoch: good.Epoch + 1, WorkerID: "w1", BootID: "b1", ConnID: good.ConnID, Now: t0 + 2})
	mustFenced(t, err, good.Epoch)
}

func testSessionCreate(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	addWorker(t, s, "w2", "u1")
	must(t, s.CreateUser(ctx, store.User{ID: "u2", Name: "other", CreatedAt: t0}))
	addWorker(t, s, "w-other", "u2")

	s1 := newSession("s1", f)
	s1.PinnedWorkerID = "w2"
	must(t, errOf(s.CreateSession(ctx, s1, []store.SessionCwd{{DeviceID: "d1", Cwd: "/home/u"}}, 2)))
	want := s1
	want.State, want.UpdatedAt, want.LastActivityAt = store.SessionIdle, t0, t0
	mustEqual(t, "GetSession", getSession(t, s, "s1"), want)
	l, err := s.GetLease(ctx, "s1")
	must(t, err)
	mustEqual(t, "new lease", l, store.Lease{SessionID: "s1"})
	sameID := newSession(s1.ID, f)
	sameID.ClientRequestID = "other-key"
	mustIs(t, errOf(s.CreateSession(ctx, sameID, nil, 2)), store.ErrExists)
	dup := newSession("s-dup", f)
	dup.ClaudeSessionID = s1.ClaudeSessionID
	mustIs(t, errOf(s.CreateSession(ctx, dup, nil, 2)), store.ErrExists)

	// The default device and the pinned worker belong to the user.
	foreign := newSession("s-foreign", f)
	foreign.PinnedWorkerID = "w-other"
	mustIs(t, errOf(s.CreateSession(ctx, foreign, nil, 2)), store.ErrNotFound)
	foreign = newSession("s-foreign", f)
	foreign.DefaultDeviceID = "nope"
	mustIs(t, errOf(s.CreateSession(ctx, foreign, nil, 2)), store.ErrNotFound)

	// Sessions per user: exactly the cap is accepted, one more is not.
	createSessionMax := func(id string) error { return errOf(s.CreateSession(ctx, newSession(id, f), nil, 2)) }
	must(t, createSessionMax("s2"))
	mustIs(t, createSessionMax("s3"), store.ErrLimit)

	// The pinned worker alone may take the lease.
	_, err = s.AcquireLease(ctx, leaseReq("s1", "w1", "b1", "c1", t0))
	mustIs(t, err, store.ErrLeaseHeld)
	fence := acquire(t, s, "s1", "w2", "b1", t0)
	recs, err := s.GetSessionRecords(ctx, fence, records(store.RecordCwds))
	must(t, err)
	mustEqual(t, "initial cwds", recs.Cwds, []store.SessionCwd{{DeviceID: "d1", Cwd: "/home/u", UpdatedAt: t0}})

	// Keyset pages.
	page, err := s.ListSessions(ctx, store.SessionFilter{Now: t0, UserID: "u1", Limit: 1})
	must(t, err)
	mustEqual(t, "first page", len(page), 1)
	page2, err := s.ListSessions(ctx, store.SessionFilter{Now: t0, UserID: "u1", Limit: 5, After: &store.SessionCursor{LastActivityAt: page[0].LastActivityAt, ID: page[0].ID}})
	must(t, err)
	mustEqual(t, "second page", len(page2), 1)
	if page[0].ID == page2[0].ID {
		t.Fatal("pages overlap")
	}

	// Stop is idempotent and keeps the lease for the final flush.
	st, lease, changed, err := s.StopSession(ctx, "s1", t0+5, "user")
	must(t, err)
	mustEqual(t, "stopped", []any{st.State, st.StoppedAt, st.Reason, changed, lease.WorkerID}, []any{store.SessionStopped, t0 + 5, "user", true, "w2"})
	_, _, changed, err = s.StopSession(ctx, "s1", t0+6, "again")
	must(t, err)
	mustEqual(t, "second stop", changed, false)
	appendEntries(t, s, at(fence, t0+7), "", 1, entry("last", `{}`))
	_, err = s.SubmitInput(ctx, newInput("s1", 1))
	mustIs(t, err, store.ErrStopped)
	_, err = s.TakeInputs(ctx, at(fence, t0+7))
	mustIs(t, err, store.ErrStopped)
	_, _, _, err = s.StopSession(ctx, "ghost", t0, "")
	mustIs(t, err, store.ErrNotFound)
}

func testSessionCreateIdempotent(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	first := newSession("first", f)
	first.ClientRequestID = strings.Repeat("x", store.MaxIDBytes)
	first.LocationIndex = 1
	for _, key := range []string{"", first.ClientRequestID + "x"} {
		bad := first
		bad.ClientRequestID = key
		mustIs(t, errOf(s.CreateSession(ctx, bad, nil, 1)), store.ErrInvalid)
		mustIs(t, errOf(s.GetSessionByClientRequestID(ctx, first.ProjectID, key, t0)), store.ErrInvalid)
	}

	created, err := s.CreateSession(ctx, first, nil, 1)
	must(t, err)
	stored := getSession(t, s, first.ID)
	mustEqual(t, "fresh create", created, store.SessionCreated{Session: stored, Created: true})
	mustEqual(t, "stored creation settings", stored.ClientRequestID, first.ClientRequestID)
	mustEqual(t, "stored location index", stored.LocationIndex, first.LocationIndex)
	byKey, err := s.GetSessionByClientRequestID(ctx, first.ProjectID, first.ClientRequestID, t0)
	must(t, err)
	mustEqual(t, "key lookup", byKey, stored)

	// A retry returns the current row even at the cap.
	stopped, _, _, err := s.StopSession(ctx, first.ID, t0+1, "user")
	must(t, err)
	retry := first
	retry.ID, retry.ClaudeSessionID, retry.CreatedAt = "retry", "claude-retry", t0+2
	repeated, err := s.CreateSession(ctx, retry, nil, 1)
	must(t, err)
	mustEqual(t, "repeat", repeated, store.SessionCreated{Session: stopped})
	defaultMode := retry
	defaultMode.PermissionMode = ""
	repeated, err = s.CreateSession(ctx, defaultMode, nil, 1)
	must(t, err)
	mustEqual(t, "repeat with default permission mode", repeated, store.SessionCreated{Session: stopped})
	for _, tc := range []struct {
		name   string
		change func(*store.Session)
	}{
		{"location", func(s *store.Session) { s.LocationIndex++ }},
		{"model", func(s *store.Session) { s.Model = "other" }},
		{"permission mode", func(s *store.Session) { s.PermissionMode = "other" }},
		{"worker", func(s *store.Session) { s.PinnedWorkerID = "w1" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			different := retry
			tc.change(&different)
			mustIs(t, errOf(s.CreateSession(ctx, different, nil, 1)), store.ErrExists)
		})
	}

	_, err = s.CreateProject(ctx, newProject("other-project", first.UserID, "Other"))
	must(t, err)
	other := retry
	other.ID, other.ClaudeSessionID, other.ProjectID = "other", "claude-other", "other-project"
	created, err = s.CreateSession(ctx, other, nil, 2)
	must(t, err)
	mustEqual(t, "other project", created.Created, true)
	mustEqual(t, "other session", created.Session.ID, other.ID)

	must(t, s.DeleteSession(ctx, first.ID, t0+2))
	mustIs(t, errOf(s.GetSessionByClientRequestID(ctx, first.ProjectID, first.ClientRequestID, t0+2)), store.ErrNotFound)
	created, err = s.CreateSession(ctx, retry, nil, 2)
	must(t, err)
	mustEqual(t, "reused key created", created.Created, true)
	mustEqual(t, "reused key session", created.Session.ID, retry.ID)
}

func testSessionCreateConcurrent(t *testing.T, b Backend) {
	s, dir := open(t, b)
	f := seed(t, s)
	handles := []store.Store{s, reopen(t, b, dir)}
	ctx := deadline(t)
	const n = 16
	var wg sync.WaitGroup
	var results [n]store.SessionCreated
	var errs [n]error
	start := make(chan struct{})
	for i := range n {
		wg.Go(func() {
			sess := newSession(fmt.Sprintf("session-%d", i), f)
			sess.ClientRequestID = "shared"
			<-start
			results[i], errs[i] = handles[i%len(handles)].CreateSession(ctx, sess, nil, 1)
		})
	}
	close(start)
	wg.Wait()
	rows, err := s.ListSessions(ctx, store.SessionFilter{UserID: f.user.ID, Now: t0, Limit: n})
	must(t, err)
	mustEqual(t, "session count", len(rows), 1)
	var winners int
	for i, res := range results {
		must(t, errs[i])
		mustEqual(t, "returned session", res.Session, rows[0])
		if res.Created {
			winners++
		}
	}
	mustEqual(t, "creations", winners, 1)
}

// testLeaseAcquire covers who may take a lease and how its epoch moves.
func testLeaseAcquire(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	addWorker(t, s, "w2", "u1")
	must(t, s.CreateUser(ctx, store.User{ID: "u2", Name: "other", CreatedAt: t0}))
	addWorker(t, s, "w-other", "u2")
	createSession(t, s, f, "s1")
	acq := func(worker, boot, conn string, now int64) (store.LeaseAcquired, error) {
		return s.AcquireLease(ctx, leaseReq("s1", worker, boot, conn, now))
	}

	_, err := acq("w-other", "b1", "c1", t0)
	mustIs(t, err, store.ErrNotFound)
	r, err := acq("w1", "b1", "c1", t0)
	must(t, err)
	mustEqual(t, "first", []any{r.Lease.Epoch, r.Lease.ConnID, r.Reattached, r.Loss}, []any{int64(1), "c1", false, (*store.LeaseLoss)(nil)})
	// The same worker and boot again (a lost reply, from a new connection):
	// same epoch, rebound, extended.
	r, err = acq("w1", "b1", "c2", t0+10)
	must(t, err)
	mustEqual(t, "reattached", []any{r.Lease.Epoch, r.Lease.ConnID, r.Lease.ExpiresAt, r.Reattached}, []any{int64(1), "c2", t0 + 10 + ttl, true})
	_, err = acq("w2", "b1", "c9", t0+20)
	mustIs(t, err, store.ErrLeaseHeld)
	_, err = acq("w1", "b2", "c9", t0+20)
	mustIs(t, err, store.ErrLeaseHeld)
	// Expired: another worker takes over with the next epoch and reports
	// the old holder's loss.
	r, err = acq("w2", "b1", "c3", t0+10+ttl)
	must(t, err)
	mustEqual(t, "takeover", []any{r.Lease.Epoch, r.Lease.WorkerID, r.Reattached, r.Loss.Released.Epoch, r.Loss.Released.ConnID},
		[]any{int64(2), "w2", false, int64(1), "c2"})

	// Extend: only keys that still match, connection included, and have
	// not expired.
	keys := []store.LeaseKey{
		{SessionID: "s1", Epoch: 2, WorkerID: "w2", BootID: "b1", ConnID: "c3"},
		{SessionID: "s1", Epoch: 1, WorkerID: "w1", BootID: "b1", ConnID: "c2"},
		{SessionID: "s1", Epoch: 2, WorkerID: "w2", BootID: "b1", ConnID: "c-old"},
	}
	lost, err := s.ExtendLeases(ctx, keys, t0+20+ttl, t0+20+3*ttl)
	must(t, err)
	mustEqual(t, "lost keys", lost, keys[1:])
	l, err := s.GetLease(ctx, "s1")
	must(t, err)
	mustEqual(t, "extended", l.ExpiresAt, t0+20+3*ttl)
	// An extension computed earlier that lands later never moves the
	// expiry back; its key is not lost.
	lost, err = s.ExtendLeases(ctx, keys[:1], t0+10+ttl, t0+20+2*ttl)
	must(t, err)
	mustEqual(t, "earlier extension: not lost", len(lost), 0)
	l, err = s.GetLease(ctx, "s1")
	must(t, err)
	mustEqual(t, "never backwards", l.ExpiresAt, t0+20+3*ttl)
	lost, err = s.ExtendLeases(ctx, keys[:1], t0+20+3*ttl, t0+20+4*ttl)
	must(t, err)
	mustEqual(t, "expired key is lost", lost, keys[:1])

	// Resume: per key; any connection of the boot, which the lease then
	// answers.
	createSession(t, s, f, "s2")
	f2 := acquireOn(t, s, "s2", "w1", "b1", "c1", t0)
	res, err := s.ResumeLeases(ctx, "w1", "b1", "c5", []store.SessionEpoch{{SessionID: "s2", Epoch: f2.Epoch}, {SessionID: "s2", Epoch: 9}, {SessionID: "ghost", Epoch: 1}}, t0+1, t0+1+ttl)
	must(t, err)
	must(t, res[0].Err)
	mustEqual(t, "resumed", []any{res[0].Lease.ExpiresAt, res[0].Lease.ConnID, res[0].Stopped}, []any{t0 + 1 + ttl, "c5", false})
	mustFenced(t, res[1].Err, f2.Epoch)
	mustIs(t, res[2].Err, store.ErrNotFound)
	_, err = s.CheckFence(ctx, at(f2, t0+2))
	mustFenced(t, err, f2.Epoch) // the old connection
	f2.ConnID = "c5"
	_, err = s.CheckFence(ctx, at(f2, t0+2))
	must(t, err)
	res, err = s.ResumeLeases(ctx, "w1", "b2", "c6", []store.SessionEpoch{{SessionID: "s2", Epoch: f2.Epoch}}, t0+2, t0+2+ttl)
	must(t, err)
	mustFenced(t, res[0].Err, f2.Epoch) // another boot
	res, err = s.ResumeLeases(ctx, "w1", "b1", "c6", []store.SessionEpoch{{SessionID: "s2", Epoch: f2.Epoch}}, t0+2+ttl, t0+3*ttl)
	must(t, err)
	mustFenced(t, res[0].Err, f2.Epoch) // expired

	// A released lease keeps its epoch; a retried release changes nothing.
	createSession(t, s, f, "s3")
	f3 := acquire(t, s, "s3", "w1", "b1", t0)
	_, err = s.ReleaseLease(ctx, at(f3, t0+1))
	must(t, err)
	loss, err := s.ReleaseLease(ctx, at(f3, t0+2))
	must(t, err)
	mustEqual(t, "retried release", []any{loss.Released.WorkerID, loss.Released.Epoch, loss.Interrupted}, []any{"", int64(1), false})
	mustEqual(t, "next epoch", acquire(t, s, "s3", "w1", "b1", t0+3).Epoch, int64(2))
}

// testLeaseConcurrentAcquire: two handles race for a free lease; exactly
// one wins, with epoch 1.
func testLeaseConcurrentAcquire(t *testing.T, b Backend) {
	s, dir := open(t, b)
	ctx := deadline(t)
	f := seed(t, s)
	addWorker(t, s, "w2", "u1")
	for round := range 5 {
		sid := fmt.Sprintf("s%d", round)
		createSession(t, s, f, sid)
		handles := []store.Store{s, reopen(t, b, dir)}
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i, w := range []string{"w1", "w2"} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs[i] = retryBusy(func() error {
					_, err := handles[i].AcquireLease(ctx, leaseReq(sid, w, "b1", "c1", t0))
					return err
				})
			}()
		}
		wg.Wait()
		wins := 0
		for _, err := range errs {
			if err == nil {
				wins++
			} else {
				mustIs(t, err, store.ErrLeaseHeld)
			}
		}
		mustEqual(t, "winners", wins, 1)
		l, err := s.GetLease(ctx, sid)
		must(t, err)
		mustEqual(t, "epoch", l.Epoch, int64(1))
	}
}

// lossSetup builds session s1 held by w1/b1 (epoch 1) with a turn running,
// two taken inputs (the first already in the transcript), and an ended job
// whose notification was handed. It returns the holder's fence.
func lossSetup(t testing.TB, s store.Store, f fixture) store.Fence {
	t.Helper()
	ctx := context.Background()
	addWorker(t, s, "w2", "u1")
	createSession(t, s, f, "s1")
	fence := at(acquire(t, s, "s1", "w1", "b1", t0), t0+1)
	sessionCall(t, s, f, "c1", "s1", store.CallCompleted)
	_, err := s.PutJob(ctx, store.Job{DeviceID: "d1", BootID: "b1", JobID: "j1", UserID: "u1", SessionID: "s1", CallID: "c1", State: store.JobExited, LastSeq: 2})
	must(t, err)
	must(t, s.PatchSessionRecords(ctx, fence, store.RecordsPatch{
		Jobs: []store.SessionJob{{CallID: "c1", JobID: "j1"}}, HandedJobCalls: []string{"c1"},
	}))
	submit(t, s, newInput("s1", 1))
	submit(t, s, newInput("s1", 2))
	taken, err := s.TakeInputs(ctx, fence)
	must(t, err)
	mustEqual(t, "taken", len(taken), 2)
	appendEntries(t, s, fence, "", 1, entry("s1-in1", `{"type":"user"}`))
	report(t, s, fence, store.TurnReport{LocalSeq: 1, Phase: store.TurnStarted})
	mustEqual(t, "running", getSession(t, s, "s1").State, store.SessionRunning)
	return fence
}

// testLeaseLossRule: every way of losing a lease interrupts the running
// turn, settles taken inputs by the transcript, un-hands job notifications
// and queues the session for its pending work.
func testLeaseLossRule(t *testing.T, b Backend) {
	const lostAt = t0 + ttl
	ways := []struct {
		name string
		lose func(t *testing.T, s store.Store, fence store.Fence) *store.LeaseLoss
	}{
		{"release", func(t *testing.T, s store.Store, fence store.Fence) *store.LeaseLoss {
			// Released by the holder just before expiry: the end of the
			// turn is the release time.
			loss, err := s.ReleaseLease(t.Context(), at(fence, lostAt-1))
			must(t, err)
			return &loss
		}},
		{"new boot", func(t *testing.T, s store.Store, _ store.Fence) *store.LeaseLoss {
			losses, err := s.ReleaseWorkerLeases(t.Context(), "w1", "b2", lostAt, 10)
			must(t, err)
			mustEqual(t, "released", len(losses), 1)
			return &losses[0]
		}},
		{"expiry", func(t *testing.T, s store.Store, _ store.Fence) *store.LeaseLoss {
			losses, err := s.ExpireLeases(t.Context(), lostAt, 10)
			must(t, err)
			mustEqual(t, "expired", len(losses), 1)
			return &losses[0]
		}},
		{"settle", func(t *testing.T, s store.Store, _ store.Fence) *store.LeaseLoss {
			r, err := s.SettleSession(t.Context(), "s1", lostAt)
			must(t, err)
			return r.Loss
		}},
		{"takeover", func(t *testing.T, s store.Store, _ store.Fence) *store.LeaseLoss {
			r, err := s.AcquireLease(t.Context(), leaseReq("s1", "w2", "b9", "b9-c", lostAt))
			must(t, err)
			mustEqual(t, "taken over", []any{r.Lease.Epoch, r.Session.State}, []any{int64(2), store.SessionIdle})
			return r.Loss
		}},
	}
	for _, w := range ways {
		t.Run(w.name, func(t *testing.T) {
			s, _ := open(t, b)
			ctx := t.Context()
			f := seed(t, s)
			fence := lossSetup(t, s, f)
			loss := w.lose(t, s, fence)
			sess := getSession(t, s, "s1")
			if loss == nil {
				t.Fatal("no loss reported")
			}
			mustEqual(t, "loss", []any{loss.Interrupted, loss.Released.Epoch, loss.Released.WorkerID, loss.Session.State, loss.Session.LastTurnOutcome},
				[]any{true, int64(1), "w1", store.SessionQueued, store.TurnInterrupted})
			takeover := w.name == "takeover"
			if !takeover {
				mustEqual(t, "loss session", loss.Session, sess)
			}
			mustEqual(t, "last outcome", sess.LastTurnOutcome, store.TurnInterrupted)
			turns, err := s.ListTurns(ctx, store.TurnFilter{SessionID: "s1", Limit: 10})
			must(t, err)
			end := lostAt
			if w.name == "release" {
				end = lostAt - 1
			}
			mustEqual(t, "interrupted turn", []any{turns[0].Outcome, turns[0].EndedAt}, []any{store.TurnInterrupted, end})
			checkTurnBoundary(t, s, "s1", turns[0].TurnNo, end, store.TurnInterrupted)

			next := store.Fence{SessionID: "s1", Epoch: 2, WorkerID: "w2", BootID: "b9", ConnID: "b9-c", Now: lostAt + 1}
			if !takeover {
				next = acquire(t, s, "s1", "w1", "b2", lostAt+1)
			}
			mustEqual(t, "new epoch", next.Epoch, int64(2))
			// The input in the transcript is persisted; the other is
			// delivered again.
			taken, err := s.TakeInputs(ctx, next)
			must(t, err)
			mustEqual(t, "re-delivered inputs", inputIDs(taken), []string{"s1-in2"})
			recs, err := s.GetSessionRecords(ctx, next, records(store.RecordJobs))
			must(t, err)
			mustEqual(t, "job notice", recs.Jobs[0].Notice, store.JobNoticeNo)
		})
	}

	// Without pending work the session goes idle.
	s, _ := open(t, b)
	f := seed(t, s)
	createSession(t, s, f, "s1")
	fence := acquire(t, s, "s1", "w1", "b1", t0)
	report(t, s, fence, store.TurnReport{LocalSeq: 1, Phase: store.TurnStarted})
	loss, err := s.ReleaseLease(t.Context(), at(fence, t0+5))
	must(t, err)
	mustEqual(t, "idle after loss", []any{loss.Session.State, loss.Interrupted}, []any{store.SessionIdle, true})
}

func inputIDs(ins []store.Input) []string {
	var ids []string
	for _, in := range ins {
		ids = append(ids, in.InputID)
	}
	return ids
}
