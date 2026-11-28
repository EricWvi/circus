package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/EricWvi/circus/internal/store"
)

// ErrNoSuchIdentity reports that no live identity of the user matched.
var ErrNoSuchIdentity = errors.New("no such identity")

// RevokeByName removes the user's live identity of this kind and name
// (compared ignoring ASCII case, as everywhere), as RevokeByID does. It is
// `circus server device revoke` and `circus server worker revoke`.
// ErrNoSuchIdentity if no live identity has that name.
func RevokeByName(ctx context.Context, st store.Store, kind store.PrincipalKind, userID, name string, now time.Time) (store.Identity, int, error) {
	return revoke(ctx, st, kind, now, func(ctx context.Context, tx store.Ops) (store.Identity, error) {
		id, err := tx.GetIdentityByName(ctx, kind, userID, name)
		if errors.Is(err, store.ErrNotFound) {
			return id, fmt.Errorf("%w: no %s named %q", ErrNoSuchIdentity, AdminNoun(kind), name)
		}
		return id, err
	})
}

// RevokeByID removes the user's live identity of this kind with this id:
// in one transaction it revokes every unrevoked credential and soft-deletes
// the identity, which frees its name for a new enrollment. It returns the
// identity as it was before the revocation and how many credentials it
// revoked. The other role on the same machine is untouched. New upgrade
// requests with a revoked credential get ErrRevoked at once; live
// connections learn of it through Authenticator.Valid. ErrNoSuchIdentity
// for another user's, an unknown or a deleted identity. It is the Web UI's
// delete; RevokeByName and a rejoin (Enroll) revoke with the same revokeIn.
func RevokeByID(ctx context.Context, st store.Store, kind store.PrincipalKind, userID, id string, now time.Time) (store.Identity, int, error) {
	return revoke(ctx, st, kind, now, func(ctx context.Context, tx store.Ops) (store.Identity, error) {
		return liveIdentity(ctx, tx, kind, userID, id)
	})
}

func revoke(ctx context.Context, st store.Store, kind store.PrincipalKind, now time.Time,
	find func(context.Context, store.Ops) (store.Identity, error)) (store.Identity, int, error) {
	var id store.Identity
	var n int
	err := st.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		var err error
		if id, err = find(ctx, tx); err != nil {
			return err
		}
		if n, err = revokeIn(ctx, tx, kind, id.ID, now); err != nil {
			return err
		}
		return ForgetMachine(ctx, tx, id.UserID, id.MachineID)
	})
	if err != nil {
		return store.Identity{}, 0, err
	}
	return id, n, nil
}

func revokeIn(ctx context.Context, tx store.Ops, kind store.PrincipalKind, id string, now time.Time) (int, error) {
	at := now.UnixMilli()
	if _, err := tx.DeleteIdentity(ctx, kind, id, at); err != nil {
		return 0, err
	}
	return tx.RevokeCredentials(ctx, kind, id, at)
}

// ForgetMachine deletes the update of the user's machine machineID once no
// live identity is on it, in the transaction that removed the last one, so
// no revocation path leaves it behind. Its deadline timer may stay; it
// finds no update and does nothing. A rejoin (Enroll) calls it after
// creating the replacing identity, so a rejoin on the same machine keeps
// the machine's update.
func ForgetMachine(ctx context.Context, tx store.Ops, userID, machineID string) error {
	// A key the update table cannot hold (an identity stored, not
	// enrolled, with a longer id) has no update.
	if machineID == "" || len(machineID) > store.MaxMachineIDBytes {
		return nil
	}
	for _, kind := range []store.PrincipalKind{store.KindDevice, store.KindWorker} {
		ids, err := tx.ListIdentities(ctx, kind, userID)
		if err != nil {
			return err
		}
		for _, id := range ids {
			if id.MachineID == machineID {
				return nil
			}
		}
	}
	_, err := tx.DeleteMachineUpdate(ctx, userID, machineID, "")
	return err
}
