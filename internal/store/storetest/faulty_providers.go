package storetest

import (
	"context"

	"github.com/EricWvi/circus/internal/store"
)

func (o *faultyOps) CreateProvider(ctx context.Context, p store.Provider) (v store.Provider, err error) {
	err = o.write(ctx, func(x store.Ops) error { v, err = x.CreateProvider(ctx, p); return err })
	return
}
func (o *faultyOps) GetProvider(ctx context.Context, userID, providerID string, now int64) (v store.Provider, err error) {
	err = o.read(ctx, func(x store.Ops) error { v, err = x.GetProvider(ctx, userID, providerID, now); return err })
	return
}
func (o *faultyOps) ListProviders(ctx context.Context, userID string, now int64) (v []store.Provider, err error) {
	err = o.read(ctx, func(x store.Ops) error { v, err = x.ListProviders(ctx, userID, now); return err })
	return
}
func (o *faultyOps) UpdateProvider(ctx context.Context, userID string, p store.Provider, expectedUpdatedAt *int64) (v store.Provider, err error) {
	err = o.write(ctx, func(x store.Ops) error { v, err = x.UpdateProvider(ctx, userID, p, expectedUpdatedAt); return err })
	return
}
func (o *faultyOps) DeleteProvider(ctx context.Context, userID, providerID string, now int64) error {
	return o.write(ctx, func(x store.Ops) error { return x.DeleteProvider(ctx, userID, providerID, now) })
}
func (o *faultyOps) CountProviderLeases(ctx context.Context, providerID string, now int64) (v int, err error) {
	err = o.read(ctx, func(x store.Ops) error { v, err = x.CountProviderLeases(ctx, providerID, now); return err })
	return
}
func (o *faultyOps) SetSessionProvider(ctx context.Context, sessionID, providerID string, at int64) (v store.SessionProviderSet, err error) {
	err = o.write(ctx, func(x store.Ops) error { v, err = x.SetSessionProvider(ctx, sessionID, providerID, at); return err })
	return
}
