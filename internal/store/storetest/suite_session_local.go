package storetest

import (
	"errors"
	"testing"

	"github.com/EricWvi/circus/internal/store"
)

// testSessionLocal: local mode and a derived pin are stored with the
// session, validated, and the client's own request (RequestedWorkerID), not
// the derived pin, decides whether a repeat matches.
func testSessionLocal(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)

	for _, tc := range []struct {
		name   string
		change func(*store.Session)
	}{
		{"local without a pin", func(s *store.Session) { s.Local = true }},
		{"derived pin of a remote session", func(s *store.Session) { s.PinnedWorkerID, s.PinDerived = "w1", true }},
		{"derived pin without a worker", func(s *store.Session) { s.Local, s.PinDerived = true, true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := newSession("bad", f)
			tc.change(&bad)
			mustIs(t, errOf(s.CreateSession(ctx, bad, nil, 8)), store.ErrInvalid)
		})
	}

	derived := newSession("derived", f)
	derived.PinnedWorkerID, derived.Local, derived.PinDerived = "w1", true, true
	created, err := s.CreateSession(ctx, derived, nil, 8)
	must(t, err)
	stored := getSession(t, s, "derived")
	mustEqual(t, "derived stored", []any{created.Created, stored.Local, stored.PinDerived, stored.PinnedWorkerID, stored.RequestedWorkerID()},
		[]any{true, true, true, "w1", ""})
	mustEqual(t, "derived matches no requested worker", stored.MatchesCreate(nil, "", stored.Model, stored.PermissionMode, nil), true)
	mustEqual(t, "derived does not match its derived pin", stored.MatchesCreate(nil, "w1", stored.Model, stored.PermissionMode, nil), false)

	// A repeat compares the requested worker: one that again derives
	// another pin (or none) matches, one naming a worker does not.
	for _, tc := range []struct {
		name  string
		retry func(*store.Session)
		ok    bool
	}{
		{"no worker now", func(s *store.Session) { s.PinnedWorkerID, s.Local, s.PinDerived = "", false, false }, true},
		{"another derived worker", func(s *store.Session) { s.PinnedWorkerID = "w2" }, true},
		{"the derived worker requested", func(s *store.Session) { s.PinDerived = false }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			retry := derived
			retry.ID, retry.ClaudeSessionID = "retry", "claude-retry"
			tc.retry(&retry)
			got, err := s.CreateSession(ctx, retry, nil, 8)
			if !tc.ok {
				mustIs(t, err, store.ErrExists)
				return
			}
			must(t, err)
			mustEqual(t, "repeat", got, store.SessionCreated{Session: stored})
		})
	}

	requested := newSession("requested", f)
	requested.PinnedWorkerID, requested.Local = "w1", true
	must(t, errOf(s.CreateSession(ctx, requested, nil, 8)))
	stored = getSession(t, s, "requested")
	mustEqual(t, "requested stored", []any{stored.Local, stored.PinDerived, stored.PinnedWorkerID, stored.RequestedWorkerID()},
		[]any{true, false, "w1", "w1"})
	remote := newSession("remote", f)
	must(t, errOf(s.CreateSession(ctx, remote, nil, 8)))
	stored = getSession(t, s, "remote")
	mustEqual(t, "remote stored", []any{stored.Local, stored.PinDerived, stored.PinnedWorkerID}, []any{false, false, ""})

	// Lists carry the flags too.
	page, err := s.ListSessions(ctx, store.SessionFilter{Now: t0, UserID: f.user.ID, Limit: 8})
	must(t, err)
	local := map[string]bool{}
	for _, p := range page {
		local[p.ID] = p.Local
	}
	mustEqual(t, "listed local", local, map[string]bool{"derived": true, "requested": true, "remote": false})
}

// testMachineWorker: the user's live worker with an unrevoked credential on
// the machine, the most recently connected first, ties by id.
func testMachineWorker(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	must(t, s.CreateUser(ctx, store.User{ID: "u1", Name: "one", CreatedAt: t0}))
	must(t, s.CreateUser(ctx, store.User{ID: "u2", Name: "two", CreatedAt: t0}))
	add := func(id, user, machine string, connectedAt int64, credential bool) {
		t.Helper()
		must(t, s.CreateIdentity(ctx, store.Identity{Kind: store.KindWorker, ID: id, UserID: user, Name: id, MachineID: machine, CreatedAt: t0}))
		if connectedAt != 0 {
			_, err := s.SetConnected(ctx, store.KindWorker, id, "boot-"+id, connectedAt)
			must(t, err)
		}
		if credential {
			must(t, s.CreateCredential(ctx, store.Credential{ID: "cred-" + id, Kind: store.KindWorker, PrincipalID: id,
				TokenHash: hash(id), CreatedAt: t0}))
		}
	}
	find := func(user, machine string) string {
		t.Helper()
		w, err := s.MachineWorker(ctx, user, machine)
		if errors.Is(err, store.ErrNotFound) {
			return ""
		}
		must(t, err)
		return w.ID
	}
	mustIs(t, errOf(s.MachineWorker(ctx, "", "m1")), store.ErrInvalid)
	mustIs(t, errOf(s.MachineWorker(ctx, "u1", "")), store.ErrInvalid)
	mustEqual(t, "none", find("u1", "m1"), "")

	add("never", "u1", "m1", 0, true)
	mustEqual(t, "a worker never connected", find("u1", "m1"), "never")
	add("b", "u1", "m1", t0+10, true)
	add("a", "u1", "m1", t0+10, true)
	add("old", "u1", "m1", t0+5, true)
	mustEqual(t, "most recent, ties by id", find("u1", "m1"), "a")
	add("newest-no-credential", "u1", "m1", t0+20, false)
	add("other-machine", "u1", "m2", t0+30, true)
	add("other-user", "u2", "m1", t0+30, true)
	mustEqual(t, "credential, machine and user count", find("u1", "m1"), "a")
	mustEqual(t, "other user", find("u2", "m1"), "other-user")

	_, err := s.RevokeCredentials(ctx, store.KindWorker, "a", t0+40)
	must(t, err)
	mustEqual(t, "revoked", find("u1", "m1"), "b")
	_, err = s.DeleteIdentity(ctx, store.KindWorker, "b", t0+40)
	must(t, err)
	mustEqual(t, "deleted", find("u1", "m1"), "old")
	// A worker with a revoked and a live credential still counts.
	must(t, s.CreateCredential(ctx, store.Credential{ID: "cred-a2", Kind: store.KindWorker, PrincipalID: "a", TokenHash: hash("a2"), CreatedAt: t0}))
	mustEqual(t, "a live credential again", find("u1", "m1"), "a")
}
