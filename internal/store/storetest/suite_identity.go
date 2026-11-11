package storetest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/EricWvi/circus/internal/store"
)

func testUsers(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	u := store.User{ID: "u1", Name: "admin", CreatedAt: t0}
	must(t, s.CreateUser(ctx, u))
	got, err := s.GetUser(ctx, "u1")
	must(t, err)
	mustEqual(t, "GetUser", got, u)
	got, err = s.GetUserByName(ctx, "admin")
	must(t, err)
	mustEqual(t, "GetUserByName", got, u)

	mustIs(t, s.CreateUser(ctx, store.User{ID: "u1", Name: "other", CreatedAt: t0}), store.ErrExists)
	mustIs(t, s.CreateUser(ctx, store.User{ID: "u2", Name: "admin", CreatedAt: t0}), store.ErrExists)
	mustIs(t, s.CreateUser(ctx, store.User{ID: "", Name: "x"}), store.ErrInvalid)
	_, err = s.GetUser(ctx, "nope")
	mustIs(t, err, store.ErrNotFound)
	_, err = s.GetUserByName(ctx, "nope")
	mustIs(t, err, store.ErrNotFound)
}

func testIdentities(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	must(t, s.CreateUser(ctx, store.User{ID: "u2", Name: "other", CreatedAt: t0}))

	for _, want := range []store.Identity{f.device, f.worker} {
		k := want.Kind
		got, err := s.GetIdentity(ctx, k, want.ID)
		must(t, err)
		mustEqual(t, label("GetIdentity ", k), got, want)
		got, err = s.GetIdentityByName(ctx, k, "u1", "laptop")
		must(t, err)
		mustEqual(t, label("GetIdentityByName ", k), got, want)

		// Names are unique per user and kind; the other kind and other
		// users may reuse them (device "laptop" and worker "laptop" above).
		dup := want
		dup.ID = want.ID + "-dup"
		mustIs(t, s.CreateIdentity(ctx, dup), store.ErrExists)
		dup.ID = want.ID // same id, new name
		dup.Name = "desk"
		mustIs(t, s.CreateIdentity(ctx, dup), store.ErrExists)
		other := want
		other.ID, other.UserID = want.ID+"-u2", "u2"
		must(t, s.CreateIdentity(ctx, other))

		// A deleted identity frees its name.
		gone := want
		gone.ID, gone.Name, gone.DeletedAt = want.ID+"-gone", "old", t0+1
		must(t, s.CreateIdentity(ctx, gone))
		_, err = s.GetIdentityByName(ctx, k, "u1", "old")
		mustIs(t, err, store.ErrNotFound)
		got, err = s.GetIdentity(ctx, k, gone.ID)
		must(t, err)
		mustEqual(t, "deleted identity by id", got, gone)
		reuse := want
		reuse.ID, reuse.Name = want.ID+"-reuse", "old"
		must(t, s.CreateIdentity(ctx, reuse))

		list, err := s.ListIdentities(ctx, k, "u1")
		must(t, err)
		mustEqual(t, label("ListIdentities ", k), list, []store.Identity{want, reuse})

		missingUser := want
		missingUser.ID, missingUser.UserID = want.ID+"-nouser", "ghost"
		mustIs(t, s.CreateIdentity(ctx, missingUser), store.ErrNotFound)

		// Connection bookkeeping: SetConnected swaps the boot atomically and
		// returns the previous one; SetDisconnected keeps it.
		prev, err := s.SetConnected(ctx, k, want.ID, "boot-1", t0+10)
		must(t, err)
		mustEqual(t, "first prev boot", prev, "")
		must(t, s.SetDisconnected(ctx, k, want.ID, t0+20))
		got, err = s.GetIdentity(ctx, k, want.ID)
		must(t, err)
		mustEqual(t, "boot after disconnect", got.CurrentBootID, "boot-1")
		mustEqual(t, "connected at", got.LastConnectedAt, t0+10)
		mustEqual(t, "disconnected at", got.LastDisconnectedAt, t0+20)
		prev, err = s.SetConnected(ctx, k, want.ID, "boot-2", t0+30)
		must(t, err)
		mustEqual(t, "second prev boot", prev, "boot-1")
		_, err = s.SetConnected(ctx, k, "ghost", "boot-1", t0)
		mustIs(t, err, store.ErrNotFound)
		mustIs(t, s.SetDisconnected(ctx, k, "ghost", t0), store.ErrNotFound)
	}

	// The device's connection did not touch the worker on the same machine.
	d, err := s.GetIdentity(ctx, store.KindDevice, "d1")
	must(t, err)
	w, err := s.GetIdentity(ctx, store.KindWorker, "w1")
	must(t, err)
	mustEqual(t, "shared machine", d.MachineID, w.MachineID)
	_, err = s.GetIdentity(ctx, store.KindWorker, "d1")
	mustIs(t, err, store.ErrNotFound)
	_, err = s.GetIdentity(ctx, "robot", "d1")
	mustIs(t, err, store.ErrInvalid)
}

func testCredentials(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	seed(t, s)
	dc := store.Credential{ID: "c-d", Kind: store.KindDevice, PrincipalID: "d1", TokenHash: hash("dev"), CreatedAt: t0}
	wc := store.Credential{ID: "c-w", Kind: store.KindWorker, PrincipalID: "w1", TokenHash: hash("wrk"), CreatedAt: t0}
	must(t, s.CreateCredential(ctx, dc))
	must(t, s.CreateCredential(ctx, wc))

	got, err := s.LookupCredential(ctx, hash("dev"))
	must(t, err)
	mustEqual(t, "lookup", got, dc)
	_, err = s.LookupCredential(ctx, hash("nope"))
	mustIs(t, err, store.ErrNotFound)

	mustIs(t, s.CreateCredential(ctx, store.Credential{ID: "c-x", Kind: store.KindDevice, PrincipalID: "d1", TokenHash: hash("dev")}), store.ErrExists)
	// The principal must exist as that kind.
	mustIs(t, s.CreateCredential(ctx, store.Credential{ID: "c-y", Kind: store.KindWorker, PrincipalID: "d1", TokenHash: hash("y")}), store.ErrNotFound)
	mustIs(t, s.CreateCredential(ctx, store.Credential{ID: "c-z", Kind: "robot", PrincipalID: "d1", TokenHash: hash("z")}), store.ErrInvalid)

	must(t, s.TouchCredential(ctx, "c-d", t0+5))
	mustIs(t, s.TouchCredential(ctx, "ghost", t0+5), store.ErrNotFound)

	// Revoking the device role leaves the worker role on the same machine.
	n, err := s.RevokeCredentials(ctx, store.KindDevice, "d1", t0+9)
	must(t, err)
	mustEqual(t, "revoked", n, 1)
	n, err = s.RevokeCredentials(ctx, store.KindDevice, "d1", t0+10)
	must(t, err)
	mustEqual(t, "revoked again", n, 0)
	got, err = s.LookupCredential(ctx, hash("dev"))
	must(t, err)
	want := dc
	want.LastUsedAt, want.RevokedAt = t0+5, t0+9
	mustEqual(t, "revoked credential", got, want)
	if !got.Revoked() {
		t.Fatal("Revoked() = false after revocation")
	}
	got, err = s.LookupCredential(ctx, hash("wrk"))
	must(t, err)
	if got.Revoked() {
		t.Fatal("worker credential revoked with the device")
	}
}

func testEnrollTokens(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	seed(t, s)
	tok := store.EnrollToken{
		ID: "e1", UserID: "u1", TokenHash: hash("e1"), CreatedAt: t0, ExpiresAt: t0 + 3600_000,
		Kinds: []store.PrincipalKind{store.KindWorker, store.KindDevice},
	}
	must(t, s.CreateEnrollToken(ctx, tok))
	mustIs(t, s.CreateEnrollToken(ctx, tok), store.ErrExists)
	bad := tok
	bad.ID, bad.TokenHash = "e-bad", hash("bad")
	for _, kinds := range [][]store.PrincipalKind{nil, {"robot"}, {store.KindDevice, store.KindDevice}} {
		bad.Kinds = kinds
		mustIs(t, s.CreateEnrollToken(ctx, bad), store.ErrInvalid)
	}
	bad.Kinds, bad.UserID = []store.PrincipalKind{store.KindDevice}, "ghost"
	mustIs(t, s.CreateEnrollToken(ctx, bad), store.ErrNotFound)

	// A failed enrollment transaction does not burn the token.
	errAbort := errors.New("abort")
	mustIs(t, s.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		if _, err := tx.UseEnrollToken(ctx, hash("e1"), t0+1, "m1"); err != nil {
			return err
		}
		return errAbort
	}), errAbort)

	used, err := s.UseEnrollToken(ctx, hash("e1"), t0+2, "m1")
	must(t, err)
	want := tok
	want.Kinds = []store.PrincipalKind{store.KindDevice, store.KindWorker}
	want.UsedAt, want.UsedBy = t0+2, "m1"
	mustEqual(t, "used token", used, want)

	_, err = s.UseEnrollToken(ctx, hash("e1"), t0+3, "m2")
	mustIs(t, err, store.ErrTokenUnusable)
	_, err = s.UseEnrollToken(ctx, hash("unknown"), t0+3, "m2")
	mustIs(t, err, store.ErrTokenUnusable)

	// Expiry is exclusive: usable strictly before ExpiresAt.
	exp := store.EnrollToken{ID: "e2", UserID: "u1", TokenHash: hash("e2"), Kinds: []store.PrincipalKind{store.KindDevice}, CreatedAt: t0, ExpiresAt: t0 + 100}
	must(t, s.CreateEnrollToken(ctx, exp))
	_, err = s.UseEnrollToken(ctx, hash("e2"), t0+100, "m1")
	mustIs(t, err, store.ErrTokenUnusable)

	// Purge removes unused expired tokens only: e2 goes; e1 (used) and e3
	// (unexpired) stay.
	must(t, s.CreateEnrollToken(ctx, store.EnrollToken{ID: "e3", UserID: "u1", TokenHash: hash("e3"), Kinds: []store.PrincipalKind{store.KindWorker}, CreatedAt: t0, ExpiresAt: t0 + 10_000_000}))
	n, err := s.PurgeEnrollTokens(ctx, t0+3600_000)
	must(t, err)
	mustEqual(t, "purged", n, 1)
	mustIs(t, s.CreateEnrollToken(ctx, exp), nil) // e2's id and hash are free again
	mustIs(t, s.CreateEnrollToken(ctx, tok), store.ErrExists)
	_, err = s.UseEnrollToken(ctx, hash("e3"), t0+3600_000, "m3")
	must(t, err)
}

// testEnrollTokenConcurrentUse races whole enrollments (use the token,
// create the identity and its credential, in one Tx) from two handles on the
// same database. Exactly one enrollment may succeed.
func testEnrollTokenConcurrentUse(t *testing.T, b Backend) {
	s1, dir := open(t, b)
	s2 := reopen(t, b, dir)
	ctx := t.Context()
	seed(t, s1)
	must(t, s1.CreateEnrollToken(ctx, store.EnrollToken{
		ID: "e1", UserID: "u1", TokenHash: hash("e1"), Kinds: []store.PrincipalKind{store.KindDevice},
		CreatedAt: t0, ExpiresAt: t0 + 3600_000,
	}))

	const racers = 16
	var wg sync.WaitGroup
	errs := make([]error, racers)
	for i := range racers {
		wg.Go(func() {
			s := s1
			if i%2 == 1 {
				s = s2
			}
			errs[i] = s.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
				tok, err := tx.UseEnrollToken(ctx, hash("e1"), t0+1, fmt.Sprintf("m-%d", i))
				if err != nil {
					return err
				}
				id := fmt.Sprintf("dev-%d", i)
				if err := tx.CreateIdentity(ctx, store.Identity{
					Kind: store.KindDevice, ID: id, UserID: tok.UserID, Name: id, MachineID: fmt.Sprintf("m-%d", i), CreatedAt: t0 + 1,
				}); err != nil {
					return err
				}
				return tx.CreateCredential(ctx, store.Credential{
					ID: "cred-" + id, Kind: store.KindDevice, PrincipalID: id, TokenHash: hash(id), CreatedAt: t0 + 1,
				})
			})
		})
	}
	wg.Wait()
	winners := 0
	for i, err := range errs {
		switch {
		case err == nil:
			winners++
		case !errors.Is(err, store.ErrTokenUnusable):
			t.Errorf("racer %d: %v", i, err)
		}
	}
	mustEqual(t, "winners", winners, 1)
	devices, err := s1.ListIdentities(ctx, store.KindDevice, "u1")
	must(t, err)
	mustEqual(t, "devices (seed + winner)", len(devices), 2)
}
