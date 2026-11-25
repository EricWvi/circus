package storetest

import (
	"context"

	"github.com/EricWvi/circus/internal/store"
)

func (o *faultyOps) CreateSessionLedger(ctx context.Context, l store.SessionLedger) error {
	return o.write(ctx, func(s store.Ops) error { return s.CreateSessionLedger(ctx, l) })
}

func (o *faultyOps) GetSessionLedger(ctx context.Context, sessionID string) (v store.SessionLedger, err error) {
	err = o.read(ctx, func(s store.Ops) error { v, err = s.GetSessionLedger(ctx, sessionID); return err })
	return v, err
}

func (o *faultyOps) UpdateSessionLedger(ctx context.Context, l store.SessionLedger, expect int64) (ok bool, err error) {
	err = o.write(ctx, func(s store.Ops) error { ok, err = s.UpdateSessionLedger(ctx, l, expect); return err })
	return ok, err
}

func (o *faultyOps) ListSessionLedgers(ctx context.Context, f store.SessionLedgerFilter) (v []store.SessionLedger, err error) {
	err = o.read(ctx, func(s store.Ops) error { v, err = s.ListSessionLedgers(ctx, f); return err })
	return v, err
}

func (o *faultyOps) ListSessionLedgerStatuses(ctx context.Context, ids []string) (v map[string]store.SessionLedger, err error) {
	err = o.read(ctx, func(s store.Ops) error { v, err = s.ListSessionLedgerStatuses(ctx, ids); return err })
	return v, err
}

func (o *faultyOps) GetOpenSessionChangeset(ctx context.Context, sessionID string) (v store.LedgerChangeset, err error) {
	err = o.read(ctx, func(s store.Ops) error { v, err = s.GetOpenSessionChangeset(ctx, sessionID); return err })
	return v, err
}

func (o *faultyOps) ListLedgerSnapshotRemovals(ctx context.Context, deviceID, after string, limit int) (v []store.LedgerSnapshotRemoval, err error) {
	err = o.read(ctx, func(s store.Ops) error {
		v, err = s.ListLedgerSnapshotRemovals(ctx, deviceID, after, limit)
		return err
	})
	return v, err
}

func (o *faultyOps) DeleteLedgerSnapshotRemoval(ctx context.Context, sessionID string) error {
	return o.write(ctx, func(s store.Ops) error { return s.DeleteLedgerSnapshotRemoval(ctx, sessionID) })
}

func (o *faultyOps) PruneLedgerSnapshotRemovals(ctx context.Context, before int64, limit int) (n int, err error) {
	err = o.write(ctx, func(s store.Ops) error { n, err = s.PruneLedgerSnapshotRemovals(ctx, before, limit); return err })
	return n, err
}

func (o *faultyOps) PruneLedgerCalls(ctx context.Context, limit int) (n int, err error) {
	err = o.write(ctx, func(s store.Ops) error { n, err = s.PruneLedgerCalls(ctx, limit); return err })
	return n, err
}

func (o *faultyOps) GetLedgerSnapshotRemoval(ctx context.Context, sessionID string) (v store.LedgerSnapshotRemoval, err error) {
	err = o.read(ctx, func(s store.Ops) error { v, err = s.GetLedgerSnapshotRemoval(ctx, sessionID); return err })
	return v, err
}

func (o *faultyOps) CountNotesSinceUserInput(ctx context.Context, sessionID string) (n int, err error) {
	err = o.read(ctx, func(s store.Ops) error { n, err = s.CountNotesSinceUserInput(ctx, sessionID); return err })
	return n, err
}

func (o *faultyOps) ListOrphanSessionChangesets(ctx context.Context, after string, limit int) (v []store.LedgerChangeset, err error) {
	err = o.read(ctx, func(s store.Ops) error { v, err = s.ListOrphanSessionChangesets(ctx, after, limit); return err })
	return v, err
}
