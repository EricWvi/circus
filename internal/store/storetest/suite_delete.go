package storetest

import (
	"context"
	"testing"

	"github.com/EricWvi/circus/internal/store"
)

// testDeleteIdentity: a soft delete frees the name, keeps the row readable by
// id, leaves the other role on the machine and the credentials alone, and is
// idempotent.
func testDeleteIdentity(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seed(t, s)
	must(t, s.CreateCredential(ctx, store.Credential{ID: "c-d", Kind: store.KindDevice, PrincipalID: "d1", TokenHash: hash("dev"), CreatedAt: t0}))

	ok, err := s.DeleteIdentity(ctx, store.KindDevice, "d1", t0+7)
	must(t, err)
	mustEqual(t, "first delete", ok, true)
	ok, err = s.DeleteIdentity(ctx, store.KindDevice, "d1", t0+8)
	must(t, err)
	mustEqual(t, "second delete", ok, false)

	got, err := s.GetIdentity(ctx, store.KindDevice, "d1")
	must(t, err)
	want := f.device
	want.DeletedAt = t0 + 7
	mustEqual(t, "deleted identity by id", got, want)
	_, err = s.GetIdentityByName(ctx, store.KindDevice, "u1", "laptop")
	mustIs(t, err, store.ErrNotFound)
	list, err := s.ListIdentities(ctx, store.KindDevice, "u1")
	must(t, err)
	mustEqual(t, "live devices", len(list), 0)

	// The name is free again; the worker of the same name is untouched.
	again := f.device
	again.ID = "d2"
	must(t, s.CreateIdentity(ctx, again))
	w, err := s.GetIdentity(ctx, store.KindWorker, "w1")
	must(t, err)
	mustEqual(t, "worker after device delete", w, f.worker)

	// Credentials are a separate layer: deleting does not revoke.
	c, err := s.LookupCredential(ctx, hash("dev"))
	must(t, err)
	mustEqual(t, "credential after delete", c.Revoked(), false)

	// Nothing to delete, or nothing valid to delete with.
	_, err = s.DeleteIdentity(ctx, store.KindDevice, "ghost", t0)
	mustIs(t, err, store.ErrNotFound)
	_, err = s.DeleteIdentity(ctx, store.KindWorker, "d2", t0)
	mustIs(t, err, store.ErrNotFound)
	_, err = s.DeleteIdentity(ctx, "robot", "d2", t0)
	mustIs(t, err, store.ErrInvalid)
	_, err = s.DeleteIdentity(ctx, store.KindDevice, "d2", 0)
	mustIs(t, err, store.ErrInvalid)

	// Inside a Tx, with the revocation, as `revoke` does it.
	must(t, s.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		if _, err := tx.RevokeCredentials(ctx, store.KindWorker, "w1", t0+9); err != nil {
			return err
		}
		_, err := tx.DeleteIdentity(ctx, store.KindWorker, "w1", t0+9)
		return err
	}))
	w, err = s.GetIdentity(ctx, store.KindWorker, "w1")
	must(t, err)
	mustEqual(t, "worker deleted in Tx", w.DeletedAt, t0+9)
}
