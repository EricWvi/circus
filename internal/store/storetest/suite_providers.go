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
		BaseURL: "https://api.example.com", OpusModel: "opus-x", KeyLast4: "abcd", Secrets: []byte("sealed-" + id),
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
	n, err := s.ExtendLeasesAfterRestart(ctx, restart, restart+ttl)
	must(t, err)
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
