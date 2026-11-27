package storetest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/EricWvi/circus/internal/store"
)

// newProvider is a valid provider of user with the given workers.
func newProvider(id, user, name string, workers ...string) store.Provider {
	return store.Provider{
		ID: id, UserID: user, Name: name, Kind: store.ProviderKindClaudeCode, Scheme: store.ProviderSchemeXAPIKey,
		BaseURL: "https://api.example.com", FableModel: "fable-x", OpusModel: "opus-x", KeyLast4: "abcd", Secrets: []byte("sealed-" + id),
		EnvNames: []string{"FOO", "BAR"}, EnvLast4: []string{"1234", ""}, WorkerIDs: workers, CreatedAt: t0, UpdatedAt: t0,
	}
}

// read fills what reads add to a written provider.
func read(p store.Provider, leased int, isDefault bool) store.Provider {
	p.LeasedSessions, p.IsDefault = leased, isDefault
	if len(p.WorkerIDs) == 0 {
		p.WorkerIDs = nil
	}
	return p
}

func createProvider(t testing.TB, ops store.Ops, p store.Provider) store.Provider {
	t.Helper()
	got, err := ops.CreateProvider(context.Background(), p)
	must(t, err)
	return got
}

func getProvider(t testing.TB, ops store.Ops, user, id string, now int64) store.Provider {
	t.Helper()
	p, err := ops.GetProvider(context.Background(), user, id, now)
	must(t, err)
	return p
}

// sessionWithProvider creates session id naming provider.
func sessionWithProvider(t testing.TB, ops store.Ops, f fixture, id, provider string) {
	t.Helper()
	s := newSession(id, f)
	s.ProviderID = provider
	must(t, errOf(ops.CreateSession(context.Background(), s, nil, store.DefaultMaxSessionsPerUser)))
}

func testProviders(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	addWorker(t, s, "w2", "u1")
	must(t, s.CreateUser(ctx, store.User{ID: "u2", Name: "other", CreatedAt: t0}))
	addWorker(t, s, "wx", "u2")

	p := newProvider("p1", "u1", "Prod", "w2", "w1")
	p.IsDefault = true
	mustEqual(t, "created", createProvider(t, s, p), read(p, 0, true))
	mustEqual(t, "get", getProvider(t, s, "u1", "p1", t0), read(p, 0, true))
	u, err := s.GetUser(ctx, "u1")
	must(t, err)
	mustEqual(t, "user default", u.DefaultProviderID, "p1")

	// Another user's provider does not exist for u1.
	_, err = s.GetProvider(ctx, "u2", "p1", t0)
	mustIs(t, err, store.ErrNotFound)

	// Names are unique per user ignoring case; another user may reuse one.
	mustIs(t, errOf(s.CreateProvider(ctx, newProvider("p2", "u1", "PROD"))), store.ErrExists)
	createProvider(t, s, newProvider("px", "u2", "Prod"))
	// The id is unique across users.
	mustIs(t, errOf(s.CreateProvider(ctx, newProvider("p1", "u2", "Other"))), store.ErrExists)
	// Workers must be live workers of the provider's user.
	_, err = s.CreateProvider(ctx, newProvider("p2", "u1", "Bad", "w1", "wx"))
	var we *store.ProviderWorkerError
	if !errors.As(err, &we) || we.Index != 1 {
		t.Fatalf("foreign worker: %v", err)
	}
	mustIs(t, err, store.ErrNotFound)
	mustIs(t, errOf(s.CreateProvider(ctx, newProvider("p2", "u1", "Bad", "nope"))), store.ErrNotFound)
	mustIs(t, errOf(s.CreateProvider(ctx, newProvider("p2", "nobody", "Bad"))), store.ErrNotFound)

	// A second default replaces the first.
	p2 := newProvider("p2", "u1", "alpha")
	p2.IsDefault = true
	createProvider(t, s, p2)
	list, err := s.ListProviders(ctx, "u1", t0)
	must(t, err)
	mustEqual(t, "list by name key", list, []store.Provider{read(p2, 0, true), read(p, 0, false)})

	// Update replaces every field; the version moves once per change.
	q := newProvider("p1", "u1", "Prod 2", "w1")
	q.UpdatedAt, q.MaxSessions, q.Scheme, q.BaseURL = t0+5, 3, store.ProviderSchemeBearer, ""
	q.EnvNames, q.EnvLast4, q.Secrets = nil, nil, []byte("sealed-again")
	q.FableModel, q.HaikuModel = "fable-y", "haiku-y"
	got, err := s.UpdateProvider(ctx, "u1", q, ptr(t0))
	must(t, err)
	want := read(q, 0, false)
	want.CreatedAt = t0
	mustEqual(t, "updated", got, want)
	// Stale.
	mustIs(t, errOf(s.UpdateProvider(ctx, "u1", q, ptr(t0))), store.ErrProviderStale)
	// The same fields again are no change, even at a later time.
	q.UpdatedAt = t0 + 9
	got, err = s.UpdateProvider(ctx, "u1", q, ptr(t0+5))
	must(t, err)
	mustEqual(t, "unchanged version", got.UpdatedAt, t0+5)
	// A change within the same millisecond still moves forward.
	q.UpdatedAt, q.IsDefault = t0+5, true
	got, err = s.UpdateProvider(ctx, "u1", q, nil)
	must(t, err)
	mustEqual(t, "default change", []any{got.UpdatedAt, got.IsDefault}, []any{t0 + 6, true})
	mustEqual(t, "other default cleared", getProvider(t, s, "u1", "p2", t0).IsDefault, false)
	// Clearing is_default leaves the user without one.
	q.UpdatedAt, q.IsDefault = t0+7, false
	must(t, errOf(s.UpdateProvider(ctx, "u1", q, nil)))
	u, err = s.GetUser(ctx, "u1")
	must(t, err)
	mustEqual(t, "no default", u.DefaultProviderID, "")
	// Renaming onto another provider's name, unknown providers, and
	// workers of another user are refused without a change.
	r := q
	r.Name = "ALPHA"
	mustIs(t, errOf(s.UpdateProvider(ctx, "u1", r, nil)), store.ErrExists)
	r = q
	r.ID = "nope"
	mustIs(t, errOf(s.UpdateProvider(ctx, "u1", r, nil)), store.ErrNotFound)
	mustIs(t, errOf(s.UpdateProvider(ctx, "u2", q, nil)), store.ErrInvalid)
	r = q
	r.WorkerIDs = []string{"wx"}
	mustIs(t, errOf(s.UpdateProvider(ctx, "u1", r, nil)), store.ErrNotFound)
	mustEqual(t, "after refusals", getProvider(t, s, "u1", "p1", t0).WorkerIDs, []string{"w1"})

	// A deleted worker leaves the provider, which moves its version.
	before := getProvider(t, s, "u1", "p1", t0)
	_, err = s.DeleteIdentity(ctx, store.KindWorker, "w1", t0+100)
	must(t, err)
	after := getProvider(t, s, "u1", "p1", t0)
	mustEqual(t, "worker dropped", []any{after.WorkerIDs, after.UpdatedAt}, []any{[]string(nil), max(t0+100, before.UpdatedAt+1)})

	// Deletion.
	must(t, s.DeleteProvider(ctx, "u1", "p2", t0))
	mustIs(t, errOf(s.GetProvider(ctx, "u1", "p2", t0)), store.ErrNotFound)
	mustIs(t, s.DeleteProvider(ctx, "u1", "p2", t0), store.ErrNotFound)
	mustIs(t, s.DeleteProvider(ctx, "u2", "p1", t0), store.ErrNotFound)
	_ = f
}

func testProviderBounds(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	seed(t, s)
	for i := range store.MaxProvidersPerUser {
		createProvider(t, s, newProvider(fmt.Sprintf("p%d", i), "u1", fmt.Sprintf("P %d", i)))
	}
	mustIs(t, errOf(s.CreateProvider(ctx, newProvider("extra", "u1", "Extra"))), store.ErrLimit)

	for name, mutate := range map[string]func(*store.Provider){
		"no id":            func(p *store.Provider) { p.ID = "" },
		"empty name":       func(p *store.Provider) { p.Name = "" },
		"long name":        func(p *store.Provider) { p.Name = strings.Repeat("é", store.MaxProviderNameRunes+1) },
		"kind":             func(p *store.Provider) { p.Kind = "other" },
		"scheme":           func(p *store.Provider) { p.Scheme = "" },
		"base url":         func(p *store.Provider) { p.BaseURL = strings.Repeat("u", store.MaxProviderBaseURLBytes+1) },
		"model":            func(p *store.Provider) { p.HaikuModel = strings.Repeat("m", store.MaxProviderModelBytes+1) },
		"fable model":      func(p *store.Provider) { p.FableModel = strings.Repeat("m", store.MaxProviderModelBytes+1) },
		"no secrets":       func(p *store.Provider) { p.Secrets = nil },
		"large secrets":    func(p *store.Provider) { p.Secrets = make([]byte, store.MaxProviderSecretsBytes+1) },
		"last4 mismatch":   func(p *store.Provider) { p.EnvLast4 = p.EnvLast4[:1] },
		"duplicate name":   func(p *store.Provider) { p.EnvNames = []string{"A", "A"} },
		"empty var name":   func(p *store.Provider) { p.EnvNames = []string{"", "B"} },
		"long var name":    func(p *store.Provider) { p.EnvNames[0] = strings.Repeat("A", store.MaxProviderEnvNameBytes+1) },
		"too many vars":    func(p *store.Provider) { p.EnvNames, p.EnvLast4 = make([]string, 65), make([]string, 65) },
		"too many workers": func(p *store.Provider) { p.WorkerIDs = make([]string, store.MaxProviderWorkers+1) },
		"duplicate worker": func(p *store.Provider) { p.WorkerIDs = []string{"w1", "w1"} },
		"max sessions":     func(p *store.Provider) { p.MaxSessions = store.MaxProviderMaxSessions + 1 },
		"negative max":     func(p *store.Provider) { p.MaxSessions = -1 },
		"no time":          func(p *store.Provider) { p.CreatedAt = 0 },
	} {
		p := newProvider("p0", "u1", "P 0")
		mutate(&p)
		mustIs(t, errOf(s.CreateProvider(ctx, p)), store.ErrInvalid)
		p.UpdatedAt = t0 + 1
		if name != "no time" {
			if _, err := s.UpdateProvider(ctx, "u1", p, nil); !isInvalid(err) {
				t.Fatalf("%s update: %v", name, err)
			}
		}
	}
	// The largest valid provider is accepted.
	p := newProvider("p0", "u1", "P 0")
	p.EnvNames, p.EnvLast4 = make([]string, store.MaxProviderEnv), make([]string, store.MaxProviderEnv)
	for i := range p.EnvNames {
		p.EnvNames[i] = fmt.Sprintf("V%03d%s", i, strings.Repeat("X", store.MaxProviderEnvNameBytes-4))
		p.EnvLast4[i] = "zzzz"
	}
	p.Secrets, p.MaxSessions, p.UpdatedAt = make([]byte, store.MaxProviderSecretsBytes), store.MaxProviderMaxSessions, t0+1
	p.FableModel = strings.Repeat("m", store.MaxProviderModelBytes)
	got, err := s.UpdateProvider(ctx, "u1", p, nil)
	must(t, err)
	mustEqual(t, "largest", []any{len(got.EnvNames), len(got.Secrets)}, []any{store.MaxProviderEnv, store.MaxProviderSecretsBytes})
}

func isInvalid(err error) bool { return errors.Is(err, store.ErrInvalid) }

// testProviderLeases: AcquireLease gates new holders by the provider's
// assignment and cap in its transaction, records the provider on the lease
// and forgets it when the lease loses its holder; DeleteProvider refuses
// while a lease is live.
func testProviderLeases(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	addWorker(t, s, "w2", "u1")
	p := newProvider("p1", "u1", "Prod", "w1")
	p.MaxSessions = 1
	createProvider(t, s, p)
	sessionWithProvider(t, s, f, "s1", "p1")
	sessionWithProvider(t, s, f, "s2", "p1")
	createSession(t, s, f, "local")

	// Not assigned to w2: refused, nothing written.
	mustIs(t, errOf(s.AcquireLease(ctx, leaseReq("s1", "w2", "b2", "c2", t0))), store.ErrProviderUnavailable)
	l, err := s.GetLease(ctx, "s1")
	must(t, err)
	mustEqual(t, "untouched", l, store.Lease{SessionID: "s1"})
	// Local login is not gated.
	acquire(t, s, "local", "w2", "b2", t0)
	l, err = s.GetLease(ctx, "local")
	must(t, err)
	mustEqual(t, "local lease provider", l.ProviderID, "")

	f1 := acquire(t, s, "s1", "w1", "b1", t0)
	l, err = s.GetLease(ctx, "s1")
	must(t, err)
	mustEqual(t, "recorded", l.ProviderID, "p1")
	mustEqual(t, "leased", getProvider(t, s, "u1", "p1", t0).LeasedSessions, 1)
	n, err := s.CountProviderLeases(ctx, "p1", t0)
	must(t, err)
	mustEqual(t, "count", n, 1)

	// The cap: s2 is refused with nothing written.
	mustIs(t, errOf(s.AcquireLease(ctx, leaseReq("s2", "w1", "b1", "c1", t0+1))), store.ErrProviderFull)
	l, err = s.GetLease(ctx, "s2")
	must(t, err)
	mustEqual(t, "full untouched", l.Epoch, int64(0))

	// The holder re-attaching is not gated, even unassigned and full.
	p.WorkerIDs, p.MaxSessions, p.UpdatedAt = []string{"w2"}, 1, t0+2
	must(t, errOf(s.UpdateProvider(ctx, "u1", p, nil)))
	r, err := s.AcquireLease(ctx, leaseReq("s1", "w1", "b1", "b1-c2", t0+2))
	must(t, err)
	mustEqual(t, "reattached", []any{r.Reattached, r.Lease.ProviderID, r.Lease.Epoch}, []any{true, "p1", f1.Epoch})
	// A fresh acquisition is a new holder: gated.
	req := leaseReq("s1", "w1", "b1", "b1-c3", t0+3)
	req.Fresh = true
	mustIs(t, errOf(s.AcquireLease(ctx, req)), store.ErrProviderUnavailable)

	// Deleting is refused while the lease is live.
	mustIs(t, s.DeleteProvider(ctx, "u1", "p1", t0+3), store.ErrProviderInUse)
	// An expired lease neither counts nor blocks.
	n, err = s.CountProviderLeases(ctx, "p1", t0+ttl+2)
	must(t, err)
	mustEqual(t, "expired not counted", n, 0)
	// w2 takes s2 now that s1's lease expired: the cap counts live leases.
	acquire(t, s, "s2", "w2", "b2", t0+ttl+3)

	// s1's expired lease is not taken over past the cap, and stays.
	mustIs(t, errOf(s.AcquireLease(ctx, leaseReq("s1", "w2", "b2", "c2", t0+ttl+4))), store.ErrProviderFull)
	l, err = s.GetLease(ctx, "s1")
	must(t, err)
	mustEqual(t, "expired lease kept", []any{l.WorkerID, l.Epoch, l.ProviderID}, []any{"w1", f1.Epoch, "p1"})
}

// testIdleLeases: ListIdleLeases lists the live leases of a provider whose
// sessions are idle, least recent activity first and in pages, and leaves
// out every session with work, a running turn, a pending tool approval (a
// pending question does not count), an owed
// or handed job notice, a background task of the lease's epoch or a ledger
// merge or discard still to apply (one problems stop does not count).
func testIdleLeases(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	createProvider(t, s, newProvider("p1", "u1", "Prod", "w1"))
	createProvider(t, s, newProvider("p2", "u1", "Other", "w1"))
	list := func(now int64, after *store.SessionCursor, limit int) []store.IdleLease {
		t.Helper()
		ls, err := s.ListIdleLeases(ctx, "p1", now, after, limit)
		must(t, err)
		return ls
	}
	idle := func(now int64, want ...string) {
		t.Helper()
		var got []string
		for _, l := range list(now, nil, store.MaxPageSize) {
			if l.ProviderID != "p1" || l.WorkerID != "w1" || l.Epoch < 1 {
				t.Fatalf("lease %+v", l)
			}
			got = append(got, l.SessionID)
		}
		mustEqual(t, "idle leases", strings.Join(got, " "), strings.Join(want, " "))
	}
	ids := []string{"idle", "input", "taken", "running", "approval", "bgtask", "ledger", "problems", "jobowed", "jobhanded", "jobrunning", "question", "other"}
	fences := map[string]store.Fence{}
	for i, id := range ids {
		prov := "p1"
		if id == "other" {
			prov = "p2"
		}
		// Created in reverse order: least recent activity is not id order.
		sess := newSession(id, f)
		sess.ProviderID, sess.CreatedAt = prov, t0-int64(i)
		must(t, errOf(s.CreateSession(ctx, sess, nil, store.DefaultMaxSessionsPerUser)))
		fences[id] = acquire(t, s, id, "w1", "b1", t0+int64(i))
	}
	// A session without a lease is never listed.
	sessionWithProvider(t, s, f, "unleased", "p1")
	all := []string{"question", "jobrunning", "jobhanded", "jobowed", "problems", "ledger", "bgtask", "approval", "running", "taken", "input", "idle"}
	idle(t0+20, all...)
	// Pages: each strictly after the last lease of the one before.
	var paged []string
	var after *store.SessionCursor
	for {
		page := list(t0+20, after, 3)
		for _, l := range page {
			paged = append(paged, l.SessionID)
		}
		if len(page) < 3 {
			break
		}
		last := page[len(page)-1]
		after = &store.SessionCursor{LastActivityAt: last.LastActivityAt, ID: last.SessionID}
	}
	mustEqual(t, "paged", strings.Join(paged, " "), strings.Join(all, " "))

	submit(t, s, newInput("input", 1))
	submit(t, s, newInput("taken", 1))
	// A deferred note alone is no work: "idle" stays idle with one.
	note := newInput("idle", 1)
	note.Kind, note.Note, note.Deferred = store.InputNote, "ledger_merged", true
	submit(t, s, note)
	_, err := s.TakeInputs(ctx, at(fences["taken"], t0+20))
	must(t, err)
	report(t, s, at(fences["running"], t0+20), store.TurnReport{LocalSeq: 1, Phase: store.TurnStarted})
	_, err = s.RequestApproval(ctx, at(fences["approval"], t0+20), store.Approval{ApprovalID: "a1", Tool: "bash", DeviceID: f.device.ID,
		MatchKey: mk("ls"), CreatedAt: t0 + 20, ExpiresAt: t0 + 100})
	must(t, err)
	// A pending question never expires, and does not keep the lease: its
	// answers are an input, which queues the session again.
	_, err = s.RequestApproval(ctx, at(fences["question"], t0+20), newQuestion("q1", "tu-q1", t0+20))
	must(t, err)
	mustEqual(t, "the question is pending", getSessionAt(t, s, "question", t0+21).PendingApprovalCount, uint32(1))
	must(t, s.PatchSessionRecords(ctx, at(fences["bgtask"], t0+20), store.RecordsPatch{BgTasksStarted: []store.BgTask{{TaskID: "t1", Kind: "agent"}}}))
	for i, id := range []string{"ledger", "problems"} {
		l := newSessionLedger(id, f)
		l.State, l.Path = store.SessionLedgerReady, "/snap-"+id
		must(t, s.CreateSessionLedger(ctx, l))
		c := newChangeset("cs-"+id, "session-project", t0)
		c.OpenerWorkerID, c.SessionID = "", id
		must(t, s.CreateLedgerChangeset(ctx, c))
		_, err = s.DiscardLedgerChangeset(ctx, "cs-"+id, "no", true, t0+20+int64(i))
		must(t, err)
	}
	// Problems stop the discard's reset until the agent fixes them: that
	// session is idle.
	got, err := s.GetSessionLedger(ctx, "problems")
	must(t, err)
	stopped := got
	stopped.ProblemCount, stopped.Problems, stopped.UpdatedAt = 1, []store.LedgerProblem{{Path: "x", Message: "y"}}, got.UpdatedAt+1
	ok, err := s.UpdateSessionLedger(ctx, stopped, got.UpdatedAt)
	must(t, err)
	mustEqual(t, "updated", ok, true)
	// Jobs: an ended job whose notice is owed, an ended one handed and not
	// persisted, and one still running (not work yet).
	for i, j := range []struct {
		session string
		state   store.JobState
		handed  bool
	}{{"jobowed", store.JobExited, false}, {"jobhanded", store.JobExited, true}, {"jobrunning", store.JobRunning, false}} {
		call, job := "c-"+j.session, fmt.Sprintf("j%d", i)
		sessionCall(t, s, f, call, j.session, store.CallCompleted)
		_, err = s.PutJob(ctx, store.Job{DeviceID: "d1", BootID: "b1", JobID: job, UserID: "u1", SessionID: j.session, CallID: call, State: j.state, LastSeq: 1})
		must(t, err)
		patch := store.RecordsPatch{Jobs: []store.SessionJob{{CallID: call, JobID: job}}}
		if j.handed {
			patch.HandedJobCalls = []string{call}
		}
		must(t, s.PatchSessionRecords(ctx, at(fences[j.session], t0+20), patch))
	}
	idle(t0+21, "question", "jobrunning", "problems", "idle")

	// An expired approval no longer counts; the running turn's end and a
	// persisted input make their sessions idle again.
	idle(t0+100, "question", "jobrunning", "problems", "approval", "idle")
	report(t, s, at(fences["running"], t0+21), store.TurnReport{LocalSeq: 1, Phase: store.TurnEnded})
	idle(t0+100, "question", "jobrunning", "problems", "approval", "running", "idle")
	// The background task of an earlier epoch is lost, not running: a new
	// epoch's session is idle.
	_, err = s.ReleaseLease(ctx, at(fences["bgtask"], t0+22))
	must(t, err)
	acquire(t, s, "bgtask", "w1", "b1", t0+23)
	idle(t0+100, "question", "jobrunning", "problems", "bgtask", "approval", "running", "idle")
	// The limit and an expired lease.
	mustEqual(t, "limited", len(list(t0+100, nil, 1)), 1)
	// Released, the note is still pending, and the session idle.
	_, err = s.ReleaseLease(ctx, at(fences["idle"], t0+24))
	must(t, err)
	mustEqual(t, "idle after the release", getSession(t, s, "idle").State, store.SessionIdle)
	n, err := s.TakeInputs(ctx, acquire(t, s, "idle", "w1", "b1", t0+25))
	must(t, err)
	mustEqual(t, "the note alone is not taken", len(n), 0)
	idle(t0 + ttl + 30)
	mustIs(t, errOf(s.ListIdleLeases(ctx, "p1", t0, nil, 0)), store.ErrInvalid)
}

// testIdleLeasesTakenHeldNote: a held note taken along with an input
// keeps the session busy until the turn persists it, even once that input
// is persisted: released then, the note's delivery would be unconfirmed.
func testIdleLeasesTakenHeldNote(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	createProvider(t, s, newProvider("p1", "u1", "Prod", "w1"))
	sessionWithProvider(t, s, f, "s1", "p1")
	fence := acquire(t, s, "s1", "w1", "b1", t0)
	listed := func(now int64) int {
		t.Helper()
		ls, err := s.ListIdleLeases(ctx, "p1", now, nil, store.MaxPageSize)
		must(t, err)
		return len(ls)
	}
	note := newInput("s1", 1)
	note.InputID, note.ClientMsgID = "s1-note", "note-1"
	note.Kind, note.Note, note.Deferred = store.InputNote, "ledger_merged", true
	submit(t, s, note)
	mustEqual(t, "held note pending", listed(t0+1), 1)
	in := newInput("s1", 2)
	submit(t, s, in)
	taken, err := s.TakeInputs(ctx, at(fence, t0+3))
	must(t, err)
	mustEqual(t, "taken together", len(taken), 2)
	report(t, s, at(fence, t0+4), store.TurnReport{LocalSeq: 1, Phase: store.TurnStarted})
	report(t, s, at(fence, t0+5), store.TurnReport{LocalSeq: 1, Phase: store.TurnEnded, Persisted: store.TurnPersisted{InputIDs: []string{in.InputID}}})
	mustEqual(t, "state", getSession(t, s, "s1").State, store.SessionIdle)
	mustEqual(t, "held note taken, not persisted", listed(t0+6), 0)
	must(t, s.PatchSessionRecords(ctx, at(fence, t0+7), store.RecordsPatch{Persisted: store.TurnPersisted{InputIDs: []string{"s1-note"}}}))
	mustEqual(t, "held note persisted", listed(t0+8), 1)
}

// testProviderLeaseRelease: the provider recorded on a lease is cleared
// when it loses its holder, and deletion is refused until then.
func testProviderLeaseRelease(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	createProvider(t, s, newProvider("p1", "u1", "Prod", "w1"))
	sessionWithProvider(t, s, f, "s1", "p1")
	fence := acquire(t, s, "s1", "w1", "b1", t0)
	mustIs(t, s.DeleteProvider(ctx, "u1", "p1", t0+1), store.ErrProviderInUse)
	loss, err := s.ReleaseLease(ctx, at(fence, t0+1))
	must(t, err)
	mustEqual(t, "released under", loss.Released.ProviderID, "p1")
	l, err := s.GetLease(ctx, "s1")
	must(t, err)
	mustEqual(t, "cleared", l.ProviderID, "")
	must(t, s.DeleteProvider(ctx, "u1", "p1", t0+2))
	// The session keeps naming the deleted provider; a new holder is
	// refused as unavailable.
	mustEqual(t, "dangling", getSession(t, s, "s1").ProviderID, "p1")
	mustIs(t, errOf(s.AcquireLease(ctx, leaseReq("s1", "w1", "b1", "c1", t0+3))), store.ErrProviderUnavailable)
}

// testSessionProvider: sessions name a provider of their user; changing it
// is refused while a turn runs or after a stop, and a change releases the
// lease with the lease-loss rule.
func testSessionProvider(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	must(t, s.CreateUser(ctx, store.User{ID: "u2", Name: "other", CreatedAt: t0}))
	createProvider(t, s, newProvider("p1", "u1", "One", "w1"))
	createProvider(t, s, newProvider("p2", "u1", "Two", "w1"))
	createProvider(t, s, newProvider("px", "u2", "Foreign"))

	// Creation checks the provider and takes part in the idempotent repeat.
	bad := newSession("s0", f)
	bad.ProviderID = "px"
	mustIs(t, errOf(s.CreateSession(ctx, bad, nil, 10)), store.ErrUnknownProvider)
	sessionWithProvider(t, s, f, "s1", "p1")
	mustEqual(t, "created with provider", getSession(t, s, "s1").ProviderID, "p1")
	repeat := newSession("s1-again", f)
	repeat.ClientRequestID, repeat.ProviderID = "s1", "p2"
	mustIs(t, errOf(s.CreateSession(ctx, repeat, nil, 10)), store.ErrExists)
	repeat.ProviderID = "p1"
	res, err := s.CreateSession(ctx, repeat, nil, 10)
	must(t, err)
	mustEqual(t, "repeat", []any{res.Created, res.Session.ID}, []any{false, "s1"})
	sess := getSession(t, s, "s1")
	mustEqual(t, "matches", []bool{
		sess.MatchesCreate(nil, "", sess.Model, sess.PermissionMode, nil),
		sess.MatchesCreate(nil, "", sess.Model, sess.PermissionMode, ptr("p1")),
		sess.MatchesCreate(nil, "", sess.Model, sess.PermissionMode, ptr("")),
	}, []bool{true, true, false})

	// Unknown, foreign and unchanged providers.
	mustIs(t, errOf(s.SetSessionProvider(ctx, "s1", "px", t0+1)), store.ErrUnknownProvider)
	mustIs(t, errOf(s.SetSessionProvider(ctx, "nope", "p1", t0+1)), store.ErrNotFound)
	same, err := s.SetSessionProvider(ctx, "s1", "p1", t0+1)
	must(t, err)
	mustEqual(t, "same", []any{same.Changed, same.Loss == nil}, []any{false, true})

	// An idle holder loses its lease; pending work queues the session.
	fence := acquire(t, s, "s1", "w1", "b1", t0+2)
	submit(t, s, newInput("s1", 1))
	set, err := s.SetSessionProvider(ctx, "s1", "", t0+3)
	must(t, err)
	if !set.Changed || set.Loss == nil || set.Loss.Released.Epoch != fence.Epoch || set.Loss.Released.ProviderID != "p1" {
		t.Fatalf("change with a lease: %+v", set)
	}
	mustEqual(t, "after change", []any{set.Session.ProviderID, set.Session.State}, []any{"", store.SessionQueued})
	l, err := s.GetLease(ctx, "s1")
	must(t, err)
	mustEqual(t, "no holder", []any{l.WorkerID, l.ProviderID}, []any{"", ""})
	mustIs(t, errOf(s.TakeInputs(ctx, at(fence, t0+4))), store.ErrFenced)

	// A running turn refuses, writing nothing.
	fence = acquire(t, s, "s1", "w1", "b1", t0+5)
	must(t, errOf(s.TakeInputs(ctx, fence)))
	report(t, s, fence, store.TurnReport{LocalSeq: 1, Phase: store.TurnStarted})
	mustIs(t, errOf(s.SetSessionProvider(ctx, "s1", "p2", t0+6)), store.ErrTurnRunning)
	mustEqual(t, "running unchanged", getSession(t, s, "s1").ProviderID, "")
	// A stopped session refuses.
	_, _, _, err = s.StopSession(ctx, "s1", t0+7, "user")
	must(t, err)
	mustIs(t, errOf(s.SetSessionProvider(ctx, "s1", "p2", t0+8)), store.ErrStopped)

	// Listing by provider.
	sessionWithProvider(t, s, f, "s2", "p2")
	got, err := s.ListSessions(ctx, store.SessionFilter{UserID: "u1", ProviderID: "p2", Now: t0, Limit: 10})
	must(t, err)
	mustEqual(t, "by provider", len(got), 1)
	mustEqual(t, "by provider id", got[0].ID, "s2")
}

// testProjectDefaultProvider: a project's default provider must be the
// user's, moves the version like the name, and may dangle after deletion.
func testProjectDefaultProvider(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seedProjects(t, s)
	createProvider(t, s, newProvider("p1", f.alice.ID, "One"))
	createProvider(t, s, newProvider("pb", f.bob.ID, "Bob's"))
	p := newProject("pr", f.alice.ID, "Proj")
	p.DefaultProviderID = "pb"
	mustIs(t, errOf(s.CreateProject(ctx, p)), store.ErrUnknownProvider)
	p.DefaultProviderID = "p1"
	got, err := s.CreateProject(ctx, p)
	must(t, err)
	mustEqual(t, "created", got.DefaultProviderID, "p1")
	mustIs(t, errOf(s.UpdateProject(ctx, f.alice.ID, "pr", store.ProjectPatch{DefaultProviderID: ptr("pb"), UpdatedAt: t0 + 1})), store.ErrUnknownProvider)
	got, err = s.UpdateProject(ctx, f.alice.ID, "pr", store.ProjectPatch{DefaultProviderID: ptr("p1"), UpdatedAt: t0 + 1})
	must(t, err)
	mustEqual(t, "same is no change", got.UpdatedAt, t0)
	mustIs(t, errOf(s.UpdateProject(ctx, f.alice.ID, "pr", store.ProjectPatch{DefaultProviderID: ptr(""), ExpectedUpdatedAt: ptr(t0 - 1), UpdatedAt: t0 + 1})), store.ErrProjectStale)
	got, err = s.UpdateProject(ctx, f.alice.ID, "pr", store.ProjectPatch{DefaultProviderID: ptr(""), ExpectedUpdatedAt: ptr(t0), UpdatedAt: t0})
	must(t, err)
	mustEqual(t, "cleared", []any{got.DefaultProviderID, got.UpdatedAt}, []any{"", t0 + 1})
	got, err = s.UpdateProject(ctx, f.alice.ID, "pr", store.ProjectPatch{DefaultProviderID: ptr("p1"), UpdatedAt: t0 + 5})
	must(t, err)
	must(t, s.DeleteProvider(ctx, f.alice.ID, "p1", t0))
	got, err = s.GetProject(ctx, f.alice.ID, "pr")
	must(t, err)
	mustEqual(t, "dangling", []any{got.DefaultProviderID, got.UpdatedAt}, []any{"p1", t0 + 5})
}

// testProviderConcurrentAcquire: two workers race AcquireLease for two
// sessions of a provider capped at one, on two handles: exactly one wins,
// the other is refused as full, and only one lease exists.
func testProviderConcurrentAcquire(t *testing.T, b Backend) {
	s, dir := open(t, b)
	f := seed(t, s)
	addWorker(t, s, "w2", "u1")
	p := newProvider("p1", "u1", "Prod", "w1", "w2")
	p.MaxSessions = 1
	createProvider(t, s, p)
	other := reopen(t, b, dir)
	for round := range 5 {
		ids := []string{fmt.Sprintf("a%d", round), fmt.Sprintf("b%d", round)}
		for _, id := range ids {
			sessionWithProvider(t, s, f, id, "p1")
		}
		now := t0 + int64(round)*(ttl+10)
		handles := []store.Store{s, other}
		var wg sync.WaitGroup
		errs := make([]error, 2)
		start := make(chan struct{})
		for i, w := range []string{"w1", "w2"} {
			wg.Go(func() {
				<-start
				errs[i] = retryBusy(func() error {
					return within(t, func(ctx context.Context) error {
						return errOf(handles[i].AcquireLease(ctx, leaseReq(ids[i], w, "b-"+w, "c-"+w, now)))
					})
				})
			})
		}
		close(start)
		wg.Wait()
		wins := 0
		for _, err := range errs {
			if err == nil {
				wins++
			} else {
				mustIs(t, err, store.ErrProviderFull)
			}
		}
		mustEqual(t, "winners", wins, 1)
		n, err := s.CountProviderLeases(t.Context(), "p1", now)
		must(t, err)
		mustEqual(t, "leases", n, 1)
	}
}

// testProviderRestartCap: a lease under a provider that ran out before a
// control-plane restart is not extended past the provider's cap (or for a
// deleted provider), while one with room is.
func testProviderRestartCap(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	addWorker(t, s, "w2", "u1")
	capped := newProvider("p1", "u1", "Capped", "w1", "w2")
	capped.MaxSessions = 1
	createProvider(t, s, capped)
	roomy := newProvider("p2", "u1", "Roomy", "w1")
	roomy.MaxSessions = 2
	createProvider(t, s, roomy)
	gone := newProvider("p3", "u1", "Gone", "w1")
	createProvider(t, s, gone)
	for id, p := range map[string]string{"s1": "p1", "s2": "p1", "r1": "p2", "g1": "p3", "local": ""} {
		sessionWithProvider(t, s, f, id, p)
	}
	acquire(t, s, "s1", "w1", "b1", t0) // expires t0+ttl, unswept
	acquire(t, s, "r1", "w1", "b1", t0)
	acquire(t, s, "g1", "w1", "b1", t0)
	acquire(t, s, "local", "w1", "b1", t0)
	// s1's lease ran out: s2 takes the provider's one place.
	acquire(t, s, "s2", "w2", "b2", t0+ttl+1)
	must(t, s.DeleteProvider(ctx, "u1", "p3", t0+ttl+1)) // only an expired lease names it
	restart := t0 + ttl + 2
	n := extendAfterRestart(t, s, restart, restart+ttl, store.MaxPageSize)
	mustEqual(t, "extended", n, 3) // s2 (live), r1 (room), local (no provider)
	for id, want := range map[string]int64{"s1": t0 + ttl, "g1": t0 + ttl, "s2": restart + ttl, "r1": restart + ttl, "local": restart + ttl} {
		l, err := s.GetLease(ctx, id)
		must(t, err)
		mustEqual(t, "expiry of "+id, l.ExpiresAt, want)
	}
	res, err := s.ResumeLeases(ctx, "w1", "b1", "c-new", []store.SessionEpoch{{SessionID: "s1", Epoch: 1}}, restart+1, restart+1+ttl)
	must(t, err)
	mustIs(t, res[0].Err, store.ErrFenced)
	n, err = s.CountProviderLeases(ctx, "p1", restart+1)
	must(t, err)
	mustEqual(t, "at the cap", n, 1)
}

// extendAfterRestart runs ExtendLeasesAfterRestart in pages of limit
// until its cursor ends, checking each page's size, and returns how many
// leases it extended.
func extendAfterRestart(t *testing.T, s store.Store, now, expiresAt int64, limit int) int {
	t.Helper()
	total := 0
	var cursor *store.RestartCursor
	for pages := 1; ; pages++ {
		n, next, err := s.ExtendLeasesAfterRestart(t.Context(), now, expiresAt, cursor, limit)
		must(t, err)
		if n > limit {
			t.Fatalf("page of %d past limit %d", n, limit)
		}
		total += n
		if next == nil {
			return total
		}
		if pages > 1000 {
			t.Fatalf("no end after %d pages", pages)
		}
		cursor = next
	}
}

// testExtendLeasesAfterRestartPaged: run in pages of any size, the
// extension after a restart extends exactly the leases one pass would:
// unexpired ones, expired ones without a provider, and per provider the
// oldest expired ones (by acquisition, then session id) that fit under its
// cap next to its live leases. A page extends at most limit leases, and a
// second run extends nothing.
func testExtendLeasesAfterRestartPaged(t *testing.T, b Backend) {
	for _, limit := range []int{1, 2, 3, 4, store.MaxPageSize} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			s, _ := open(t, b)
			ctx := t.Context()
			f := seed(t, s)
			capped := newProvider("p1", "u1", "Capped", "w1")
			capped.MaxSessions = 3
			createProvider(t, s, capped)
			createProvider(t, s, newProvider("p2", "u1", "Open", "w1"))
			gone := newProvider("p3", "u1", "Gone", "w1")
			createProvider(t, s, gone)
			for id, p := range map[string]string{"e1": "p1", "e2": "p1", "e3": "p1", "live": "p1", "open1": "p2", "open2": "p2",
				"g1": "p3", "x1": "", "x2": "", "released": ""} {
				sessionWithProvider(t, s, f, id, p)
			}
			// e1, then e2 and e3 at the same time, run out; live is
			// acquired after, so p1 has one live lease and room for two of
			// the three: e1 and, by session id, e2.
			acquire(t, s, "e3", "w1", "b1", t0+1)
			acquire(t, s, "e1", "w1", "b1", t0)
			acquire(t, s, "e2", "w1", "b1", t0+1)
			acquire(t, s, "open2", "w1", "b1", t0)
			acquire(t, s, "open1", "w1", "b1", t0)
			acquire(t, s, "g1", "w1", "b1", t0)
			acquire(t, s, "x1", "w1", "b1", t0)
			released := acquire(t, s, "released", "w1", "b1", t0)
			_, err := s.ReleaseLease(ctx, released)
			must(t, err)
			acquire(t, s, "live", "w1", "b1", t0+ttl+5)
			acquire(t, s, "x2", "w1", "b1", t0+ttl+5)
			must(t, s.DeleteProvider(ctx, "u1", "p3", t0+ttl+6))
			restart := t0 + ttl + 10
			mustEqual(t, "extended", extendAfterRestart(t, s, restart, restart+ttl, limit), 7) // e1, e2, live, open1, open2, x1, x2
			for id, want := range map[string]int64{"e1": restart + ttl, "e2": restart + ttl, "e3": t0 + 1 + ttl, "live": restart + ttl,
				"open1": restart + ttl, "open2": restart + ttl, "g1": t0 + ttl, "x1": restart + ttl, "x2": restart + ttl, "released": 0} {
				l, err := s.GetLease(ctx, id)
				must(t, err)
				mustEqual(t, "expiry of "+id, l.ExpiresAt, want)
			}
			n, err := s.CountProviderLeases(ctx, "p1", restart+1)
			must(t, err)
			mustEqual(t, "at the cap", n, 3)
			mustEqual(t, "second run", extendAfterRestart(t, s, restart, restart+ttl, limit), 0)
		})
	}
	s, _ := open(t, b)
	for _, limit := range []int{0, store.MaxPageSize + 1} {
		_, _, err := s.ExtendLeasesAfterRestart(t.Context(), t0, t0+ttl, nil, limit)
		mustIs(t, err, store.ErrInvalid)
	}
}

// testProviderOwnLeaseAndExpired: the cap never counts the session's own
// lease (a fresh re-acquisition by the holder at the cap succeeds), and an
// expired lease does not block DeleteProvider.
func testProviderOwnLeaseAndExpired(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	p := newProvider("p1", "u1", "Capped", "w1")
	p.MaxSessions = 1
	createProvider(t, s, p)
	sessionWithProvider(t, s, f, "s1", "p1")
	first := acquire(t, s, "s1", "w1", "b1", t0)
	req := leaseReq("s1", "w1", "b1", "b1-c2", t0+1)
	req.Fresh = true
	r, err := s.AcquireLease(ctx, req)
	must(t, err)
	if r.Lease.Epoch != first.Epoch+1 || r.Lease.ProviderID != "p1" {
		t.Fatalf("fresh re-acquisition at the cap: %+v", r.Lease)
	}
	mustIs(t, s.DeleteProvider(ctx, "u1", "p1", t0+2), store.ErrProviderInUse)
	must(t, s.DeleteProvider(ctx, "u1", "p1", t0+1+ttl))
}

// testSessionProviderSameIsNoChange: naming the current provider again is
// no change in any state: not refused while running or stopped, nothing
// written, no lease released.
func testSessionProviderSameIsNoChange(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	createProvider(t, s, newProvider("p1", "u1", "One", "w1"))
	sessionWithProvider(t, s, f, "s1", "p1")
	fence := acquire(t, s, "s1", "w1", "b1", t0)
	report(t, s, fence, store.TurnReport{LocalSeq: 1, Phase: store.TurnStarted})
	for _, stop := range []bool{false, true} {
		if stop {
			_, _, _, err := s.StopSession(ctx, "s1", t0+2, "user")
			must(t, err)
		}
		res, err := s.SetSessionProvider(ctx, "s1", "p1", t0+3)
		must(t, err)
		if res.Changed || res.Loss != nil || res.Session.ProviderID != "p1" {
			t.Fatalf("same provider (stopped %v): %+v", stop, res)
		}
		l, err := s.GetLease(ctx, "s1")
		must(t, err)
		mustEqual(t, "lease kept", l.Epoch, fence.Epoch)
		mustEqual(t, "lease holder kept", l.WorkerID, "w1")
	}
	// A provider deleted since, named again, is no change either.
	_, _, _, err := s.StopSession(ctx, "s1", t0+4, "user")
	must(t, err)
	l, err := s.GetLease(ctx, "s1")
	must(t, err)
	_, err = s.ReleaseLease(ctx, store.Fence{SessionID: "s1", Epoch: l.Epoch, WorkerID: "w1", BootID: "b1", ConnID: l.ConnID, Now: t0 + 5})
	must(t, err)
	must(t, s.DeleteProvider(ctx, "u1", "p1", t0+6))
	res, err := s.SetSessionProvider(ctx, "s1", "p1", t0+7)
	must(t, err)
	mustEqual(t, "deleted, same", res.Changed, false)
}
