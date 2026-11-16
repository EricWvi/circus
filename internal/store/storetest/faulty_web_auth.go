package storetest

import (
	"context"

	"github.com/EricWvi/circus/internal/store"
)

func (o *faultyOps) CreateSuperUser(ctx context.Context, u store.User, id *store.UserIdentity) error {
	return o.write(ctx, func(x store.Ops) error { return x.CreateSuperUser(ctx, u, id) })
}
func (o *faultyOps) GetSuperUser(ctx context.Context) (v store.User, err error) {
	err = o.read(ctx, func(x store.Ops) error { v, err = x.GetSuperUser(ctx); return err })
	return
}
func (o *faultyOps) GetUserByEmail(ctx context.Context, email string) (v store.User, err error) {
	err = o.read(ctx, func(x store.Ops) error { v, err = x.GetUserByEmail(ctx, email); return err })
	return
}
func (o *faultyOps) CreateUserIdentity(ctx context.Context, id store.UserIdentity) error {
	return o.write(ctx, func(x store.Ops) error { return x.CreateUserIdentity(ctx, id) })
}
func (o *faultyOps) GetUserIdentity(ctx context.Context, issuer, subject string) (v store.UserIdentity, err error) {
	err = o.read(ctx, func(x store.Ops) error { v, err = x.GetUserIdentity(ctx, issuer, subject); return err })
	return
}
func (o *faultyOps) DeleteUser(ctx context.Context, id string) error {
	return o.write(ctx, func(x store.Ops) error { return x.DeleteUser(ctx, id) })
}
func (o *faultyOps) CreateWebSession(ctx context.Context, v store.WebSession) error {
	return o.write(ctx, func(x store.Ops) error { return x.CreateWebSession(ctx, v) })
}
func (o *faultyOps) UseWebSession(ctx context.Context, hash []byte, at int64) (v store.WebSession, err error) {
	err = o.write(ctx, func(x store.Ops) error { v, err = x.UseWebSession(ctx, hash, at); return err })
	return
}
func (o *faultyOps) DeleteWebSession(ctx context.Context, hash []byte) error {
	return o.write(ctx, func(x store.Ops) error { return x.DeleteWebSession(ctx, hash) })
}

func (o *faultyOps) HasUserIdentityAtIssuer(ctx context.Context, userID, issuer string) (v bool, err error) {
	err = o.read(ctx, func(x store.Ops) error { v, err = x.HasUserIdentityAtIssuer(ctx, userID, issuer); return err })
	return
}
func (o *faultyOps) PurgeWebSessions(ctx context.Context, now int64) (v int, err error) {
	err = o.write(ctx, func(x store.Ops) error { v, err = x.PurgeWebSessions(ctx, now); return err })
	return
}
