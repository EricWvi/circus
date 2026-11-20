package storetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/testutil/wait"
)

func testFaultInjection(t *testing.T, b Backend) {
	inner, _ := open(t, b)
	f := NewFaulty(inner)
	ctx := deadline(t)
	user := func(id string) store.User { return store.User{ID: id, Name: "n-" + id, CreatedAt: t0} }
	absent := func(id string) {
		t.Helper()
		_, err := f.GetUser(ctx, id)
		mustIs(t, err, store.ErrNotFound)
	}

	// A busy write reaches nothing; the next one goes through.
	f.FailWrite(1, store.ErrBusy)
	mustIs(t, f.CreateUser(ctx, user("u1")), store.ErrBusy)
	absent("u1")
	must(t, f.CreateUser(ctx, user("u1")))
	mustEqual(t, "write points", f.Writes(), 2)

	// The second write of a Tx fails; fn returns the error; all rolls back.
	f.FailWrite(2, nil)
	mustIs(t, f.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		must(t, tx.CreateUser(ctx, user("u2")))
		return tx.CreateUser(ctx, user("u3"))
	}), ErrInjected)
	absent("u2")
	absent("u3")

	// A failed statement does not abort the transaction by itself: fn that
	// ignores it commits the other writes, without the failed one.
	f.FailWrite(2, nil)
	must(t, f.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		must(t, tx.CreateUser(ctx, user("u2")))
		mustIs(t, tx.CreateUser(ctx, user("u3")), ErrInjected)
		return nil
	}))
	must(t, errOf(f.GetUser(ctx, "u2")))
	absent("u3")

	// The commit is a write point too: failing it rolls back.
	f.FailWrite(2, store.ErrBusy)
	mustIs(t, f.Tx(ctx, func(ctx context.Context, tx store.Ops) error { return tx.CreateUser(ctx, user("u4")) }), store.ErrBusy)
	absent("u4")

	// A full disk: every write fails, reads still work, until cleared.
	f.FailAllWrites(ErrInjected)
	mustIs(t, f.CreateUser(ctx, user("u5")), ErrInjected)
	mustIs(t, f.SetTimer(ctx, "k", "s", t0), ErrInjected)
	must(t, errOf(f.GetUser(ctx, "u1")))
	f.FailAllWrites(nil)
	must(t, f.CreateUser(ctx, user("u5")))

	// A crash inside a Tx rolls it back even when fn swallows ErrCrashed
	// and returns nil: the dead process never committed. (Last: the store
	// stays crashed.)
	f.CrashAfterWrite(1, nil)
	mustIs(t, f.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		mustIs(t, tx.CreateUser(ctx, user("u6")), ErrCrashed)
		mustIs(t, tx.CreateUser(ctx, user("u7")), ErrCrashed)
		return nil
	}), ErrCrashed)
	_, err := f.Inner().GetUser(ctx, "u6")
	mustIs(t, err, store.ErrNotFound)
}

// testFaultyLockHonoursCtx: while a Faulty Tx holds the write lock, a write
// with an ordinary (unmarked) ctx waits for it only until that ctx ends,
// as it would on a real backend, instead of until the Tx finishes.
func testFaultyLockHonoursCtx(t *testing.T, b Backend) {
	inner, _ := open(t, b)
	f := NewFaulty(inner)
	outer := t.Context()
	must(t, f.Tx(outer, func(ctx context.Context, tx store.Ops) error {
		done := make(chan error, 1)
		go func() {
			c, cancel := context.WithTimeout(outer, 100*time.Millisecond)
			defer cancel()
			done <- f.CreateUser(c, store.User{ID: "x", Name: "x", CreatedAt: t0})
		}()
		select {
		case err := <-done:
			mustIs(t, err, context.DeadlineExceeded)
		case <-time.After(wait.Timeout):
			t.Fatal("write waited for the Faulty lock past its own deadline")
		}
		return nil
	}))
}

// testFaultySerializesWrites: while a Faulty Tx is open, a top-level write
// waits for it before reaching its write point, so a crash point inside the
// Tx never races another write; it runs once the Tx has committed.
func testFaultySerializesWrites(t *testing.T, b Backend) {
	inner, _ := open(t, b)
	f := NewFaulty(inner)
	ctx := deadline(t)
	user := func(id string) store.User { return store.User{ID: id, Name: id, CreatedAt: t0} }
	done := make(chan error, 1)
	var start int
	must(t, f.Tx(ctx, func(txCtx context.Context, tx store.Ops) error {
		start = f.Writes()
		go func() { done <- f.CreateUser(ctx, user("outside")) }() // ordinary ctx
		time.Sleep(100 * time.Millisecond)
		select {
		case err := <-done:
			t.Errorf("top-level write returned while the Tx was open: %v", err)
		default:
		}
		mustEqual(t, "write points while the Tx holds the lock", f.Writes(), start)
		return tx.CreateUser(txCtx, user("inside"))
	}))
	select {
	case err := <-done:
		must(t, err)
	case <-ctx.Done():
		t.Fatal("top-level write still waiting after the Tx committed")
	}
	mustEqual(t, "write points", f.Writes(), start+3) // inside, commit, outside
	for _, id := range []string{"inside", "outside"} {
		_, err := f.GetUser(ctx, id)
		must(t, err)
	}
}

func errOf[T any](_ T, err error) error { return err }

// crashStep is one step of a scripted control plane run; points is the
// number of write points it goes through (see Faulty).
type crashStep struct {
	name   string
	points int
	run    func(ctx context.Context, s store.Store) error
}

// within runs the step under its own deadline.
func (st crashStep) within(t testing.TB, s store.Store) error {
	return within(t, func(ctx context.Context) error { return st.run(ctx, s) })
}

func crashScript(f fixture) []crashStep {
	call := newCall("c1", f)
	cred := store.Credential{ID: "c-d", Kind: store.KindDevice, PrincipalID: "d1", TokenHash: hash("dev"), CreatedAt: t0}
	tx := func(fn func(ctx context.Context, tx store.Ops) error) func(context.Context, store.Store) error {
		return func(ctx context.Context, s store.Store) error {
			return s.Tx(ctx, func(ctx context.Context, tx store.Ops) error { return fn(ctx, tx) })
		}
	}
	transition := func(ctx context.Context, ops store.Ops, from, to store.CallState, p store.CallPatch) error {
		ok, err := ops.TransitionCall(ctx, "c1", store.CallExpect{States: []store.CallState{from}}, to, p)
		if err == nil && !ok {
			err = errors.New("transition did not apply")
		}
		return err
	}
	return []crashStep{
		{"user", 1, func(ctx context.Context, s store.Store) error { return s.CreateUser(ctx, f.user) }},
		{"identities", 3, tx(func(ctx context.Context, tx store.Ops) error {
			if err := tx.CreateIdentity(ctx, f.device); err != nil {
				return err
			}
			return tx.CreateIdentity(ctx, f.worker)
		})},
		{"credential", 1, func(ctx context.Context, s store.Store) error { return s.CreateCredential(ctx, cred) }},
		{"submit", 1, func(ctx context.Context, s store.Store) error { return s.CreateCall(ctx, call) }},
		{"dispatch", 3, tx(func(ctx context.Context, tx store.Ops) error {
			if err := transition(ctx, tx, store.CallAccepted, store.CallDispatched, store.CallPatch{DispatchedBootID: "b1", DispatchedAt: t0 + 1}); err != nil {
				return err
			}
			return tx.SetTimer(ctx, "call_watchdog", "c1", t0+100)
		})},
		{"connect", 1, func(ctx context.Context, s store.Store) error {
			_, err := s.SetConnected(ctx, store.KindDevice, "d1", "b1", t0+2)
			return err
		}},
		{"job", 1, func(ctx context.Context, s store.Store) error {
			_, err := s.PutJob(ctx, store.Job{DeviceID: "d1", BootID: "b1", JobID: "j1", UserID: "u1", CallID: "c1", State: store.JobRunning, LastSeq: 1})
			return err
		}},
		{"claim", 1, func(ctx context.Context, s store.Store) error {
			_, err := s.ClaimDueTimers(ctx, t0+1000, "i1", lease, 10)
			return err
		}},
		{"complete", 3, tx(func(ctx context.Context, tx store.Ops) error {
			if err := transition(ctx, tx, store.CallDispatched, store.CallCompleted, store.CallPatch{FinishedAt: t0 + 3, ExitCode: ptr(int32(0))}); err != nil {
				return err
			}
			return tx.CancelTimer(ctx, "call_watchdog", "c1")
		})},
		{"revoke", 1, func(ctx context.Context, s store.Store) error {
			_, err := s.RevokeCredentials(ctx, store.KindDevice, "d1", t0+4)
			return err
		}},
		{"project", 1, func(ctx context.Context, s store.Store) error {
			_, err := s.CreateProject(ctx, newProject("session-project", "u1", "Sessions"))
			return err
		}},
		{"session", 1, func(ctx context.Context, s store.Store) error {
			return s.CreateSession(ctx, newSession("s1", f), []store.SessionCwd{{DeviceID: "d1", Cwd: "/w"}}, 10)
		}},
		{"lease", 1, func(ctx context.Context, s store.Store) error {
			_, err := s.AcquireLease(ctx, store.LeaseRequest{SessionID: "s1", WorkerID: "w1", BootID: "wb1", ConnID: "wc1", Now: t0 + 10, ExpiresAt: t0 + 10 + ttl})
			return err
		}},
		{"append", 1, func(ctx context.Context, s store.Store) error {
			_, err := s.AppendTranscript(ctx, crashFence, store.TranscriptBatch{BatchSeq: 1, Entries: []store.TranscriptEntry{entry("s1-in1", `{"user":1}`)}}, quota)
			return err
		}},
		{"input", 1, func(ctx context.Context, s store.Store) error {
			_, err := s.SubmitInput(ctx, newInput("s1", 1))
			return err
		}},
		{"take", 1, func(ctx context.Context, s store.Store) error {
			_, err := s.TakeInputs(ctx, crashFence)
			return err
		}},
		{"turn and call", 3, tx(func(ctx context.Context, tx store.Ops) error {
			if _, err := tx.ReportTurn(ctx, crashFence, store.TurnReport{LocalSeq: 1, Phase: store.TurnStarted, Origin: store.TurnFromInput, At: t0 + 11}); err != nil {
				return err
			}
			if err := tx.AdmitSessionCall(ctx, crashFence, sessionCallRow.CallID, store.DefaultMaxActiveSessionCalls); err != nil {
				return err
			}
			return tx.CreateCall(ctx, sessionCallRow)
		})},
		{"interrupt", 1, func(ctx context.Context, s store.Store) error {
			_, _, _, err := s.InterruptSession(ctx, "s1", t0+11)
			return err
		}},
		{"await", 1, func(ctx context.Context, s store.Store) error {
			return s.PatchSessionRecords(ctx, crashFence, store.RecordsPatch{ReturnedCallIDs: []string{"c2"}, AwaitingCallIDs: []string{"c2"}})
		}},
		{"title", 1, func(ctx context.Context, s store.Store) error { return s.SetSessionTitle(ctx, "s1", "Title") }},
		{"successor", 1, func(ctx context.Context, s store.Store) error {
			_, err := s.ReportTurn(ctx, crashFence, store.TurnReport{LocalSeq: 2, Phase: store.TurnStarted, Origin: store.TurnFromInput, At: t0 + 12})
			return err
		}},
		{"end", 1, func(ctx context.Context, s store.Store) error {
			_, err := s.ReportTurn(ctx, crashFence, store.TurnReport{LocalSeq: 2, Phase: store.TurnEnded, Outcome: store.TurnStopped, At: t0 + 13,
				Persisted: store.TurnPersisted{InputIDs: []string{"s1-in1"}, CallIDs: []string{"c2"}}})
			return err
		}},
	}
}

// crashFence is the crash script's lease on session s1.
var crashFence = store.Fence{SessionID: "s1", Epoch: 1, WorkerID: "w1", BootID: "wb1", ConnID: "wc1", Now: t0 + 11}

var sessionCallRow = store.Call{
	CallID: "c2", UserID: "u1", DeviceID: "d1", SessionID: "s1", Tool: "bash", RequestHash: hash("c2"),
	RequestBytes: 1, State: store.CallAccepted, SubmittedAt: t0 + 11, ToolUseID: "tu-c2",
}

// snapshot is every row the crash script touches.
type snapshot struct {
	Users            []store.User
	Devices, Workers []store.Identity
	Credentials      []store.Credential
	Calls            []store.Call
	Jobs             []store.Job
	Timers           []store.Timer
	Projects         []store.Project
	Events           []store.SessionEvent
	Sessions         []store.Session
	Leases           []store.Lease
	Turns            []store.Turn
	Transcript       []store.TranscriptEntry
}

func takeSnapshot(t testing.TB, s store.Ops) snapshot {
	t.Helper()
	ctx := t.Context()
	var snap snapshot
	if u, err := s.GetUser(ctx, "u1"); err == nil {
		snap.Users = append(snap.Users, u)
	} else {
		mustIs(t, err, store.ErrNotFound)
	}
	if c, err := s.LookupCredential(ctx, hash("dev")); err == nil {
		snap.Credentials = append(snap.Credentials, c)
	} else {
		mustIs(t, err, store.ErrNotFound)
	}
	var err error
	snap.Devices, err = s.ListIdentities(ctx, store.KindDevice, "u1")
	must(t, err)
	snap.Workers, err = s.ListIdentities(ctx, store.KindWorker, "u1")
	must(t, err)
	snap.Calls, err = s.ListCalls(ctx, store.CallFilter{Limit: store.MaxPageSize})
	must(t, err)
	snap.Jobs, err = s.ListJobs(ctx, store.JobFilter{Limit: store.MaxPageSize})
	must(t, err)
	snap.Timers, err = s.ListTimers(ctx, store.TimerFilter{Limit: store.MaxPageSize})
	must(t, err)
	for i := range snap.Timers {
		snap.Timers[i].Token = "" // random per claim
	}
	snap.Projects, err = s.ListProjects(ctx, "u1")
	must(t, err)
	snap.Sessions, err = s.ListSessions(ctx, store.SessionFilter{Now: t0, Limit: store.MaxPageSize})
	must(t, err)
	if l, err := s.GetLease(ctx, "s1"); err == nil {
		snap.Leases = append(snap.Leases, l)
		snap.Events = sessionEvents(t, s, "s1")
		if l.WorkerID != "" {
			page, err := s.LoadTranscript(ctx, crashFence, store.TranscriptPageRequest{Limit: 100, MaxBytes: 1 << 20})
			must(t, err)
			snap.Transcript = page.Entries
		}
	} else {
		mustIs(t, err, store.ErrNotFound)
	}
	snap.Turns, err = s.ListTurns(ctx, store.TurnFilter{SessionID: "s1", Limit: store.MaxPageSize})
	must(t, err)
	return snap
}

// testCrashAtEveryWritePoint kills the process after each write point of the
// script in turn, restarts it on the same database, and checks that exactly
// the steps committed before the crash survive (a Tx interrupted before its
// commit leaves nothing), then finishes the script on the restarted store
// and checks it reaches the same end state as an uninterrupted run.
func testCrashAtEveryWritePoint(t *testing.T, b Backend) {
	// Each step runs under its own deadline: a step left hanging by the
	// crash fails fast, while the dozens of restarts, snapshots and steps
	// around it are not timed (under parallel -race load they take long).
	script := crashScript(fixtureRows())

	ref, _ := open(t, b)
	prefix := []snapshot{takeSnapshot(t, ref)}
	total := 0
	for _, st := range script {
		must(t, st.within(t, NewFaulty(ref)))
		prefix = append(prefix, takeSnapshot(t, ref))
		total += st.points
	}

	for n := 1; n <= total; n++ {
		p := StartProcess(t, b, t.TempDir())
		p.CrashAfterWrite(n)
		crashedIn, end := -1, 0
		for i, st := range script {
			end += st.points
			err := st.within(t, p.Store())
			if errors.Is(err, ErrCrashed) {
				crashedIn = i
				break
			}
			if err != nil {
				t.Fatalf("n=%d step %s: %v", n, st.name, err)
			}
		}
		if crashedIn < 0 || end-script[crashedIn].points >= n || n > end {
			t.Fatalf("n=%d: crash in step %d, want the step holding write point %d", n, crashedIn, n)
		}
		durable := crashedIn
		if n == end {
			durable++ // crashed right after the step's last write or commit
		}
		p.Restart()
		mustEqual(t, label("n=", n, " after restart (", durable, " steps durable)"), takeSnapshot(t, p.Store()), prefix[durable])
		for _, st := range script[durable:] {
			if err := st.within(t, p.Store()); err != nil {
				t.Fatalf("n=%d resume step %s: %v", n, st.name, err)
			}
		}
		mustEqual(t, label("n=", n, " final"), takeSnapshot(t, p.Store()), prefix[len(script)])
	}
}

func fixtureRows() fixture {
	var f fixture
	f.user = store.User{ID: "u1", Name: "admin", CreatedAt: t0}
	f.device = store.Identity{Kind: store.KindDevice, ID: "d1", UserID: "u1", Name: "laptop", MachineID: "m1", CreatedAt: t0}
	f.worker = store.Identity{Kind: store.KindWorker, ID: "w1", UserID: "u1", Name: "laptop", MachineID: "m1", CreatedAt: t0}
	return f
}

// testCrashKeepsTimerClaim: a process dies holding a timer claim. The claim
// is durable, so the restarted process cannot take the timer before the
// lease ends, and takes it over once it has.
func testCrashKeepsTimerClaim(t *testing.T, b Backend) {
	p := StartProcess(t, b, t.TempDir())
	ctx := deadline(t)
	must(t, p.Store().SetTimer(ctx, "device_grace", "d1", t0))
	p.CrashAfterWrite(1)
	_, err := p.Store().ClaimDueTimers(ctx, t0, "old", lease, 1)
	mustIs(t, err, ErrCrashed)
	p.Restart()

	ts, err := p.Store().ListTimers(ctx, store.TimerFilter{Limit: store.MaxPageSize})
	must(t, err)
	if len(ts) != 1 || ts[0].ClaimedBy != "old" || ts[0].Attempts != 1 {
		t.Fatalf("timers after restart: %+v", ts)
	}
	end := ts[0].ClaimUntil
	mustEqual(t, "before lease end", len(claim(t, p.Store(), end-1, "new", 1)), 0)
	got := claim(t, p.Store(), end, "new", 1)
	if len(got) != 1 || got[0].ClaimedBy != "new" || got[0].Attempts != 2 {
		t.Fatalf("re-claim after restart: %+v", got)
	}
}
