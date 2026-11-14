package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/EricWvi/circus/internal/store"
)

// ErrNoSuchIdentity reports that RevokeByName found no live identity.
var ErrNoSuchIdentity = errors.New("no such identity")

// RevokeByName removes the user's live identity of this kind and name: in
// one transaction it revokes every unrevoked credential and soft-deletes the
// identity, which frees its name for a new enrollment. It returns the
// identity as it was before the revocation and how many credentials it
// revoked. The other role on the same machine is untouched. New upgrade
// requests with a revoked credential get ErrRevoked at once; live
// connections learn of it through Authenticator.Valid. ErrNoSuchIdentity if
// no live identity has that name.
func RevokeByName(ctx context.Context, st store.Store, kind store.PrincipalKind, userID, name string, now time.Time) (store.Identity, int, error) {
	var id store.Identity
	var n int
	err := st.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		var err error
		id, err = tx.GetIdentityByName(ctx, kind, userID, name)
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("%w: no %s named %q", ErrNoSuchIdentity, AdminNoun(kind), name)
		}
		if err != nil {
			return err
		}
		n, err = revokeIn(ctx, tx, kind, id.ID, now)
		return err
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
