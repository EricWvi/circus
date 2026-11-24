package storetest

import (
	"context"
	"iter"

	"github.com/EricWvi/circus/internal/store"
)

func (o *faultyOps) LedgerProjectName(ctx context.Context, projectID string) (name string, found bool, err error) {
	err = o.read(ctx, func(x store.Ops) error { name, found, err = x.LedgerProjectName(ctx, projectID); return err })
	return
}

func (o *faultyOps) ListLedgerProjects(ctx context.Context, after string) (v []string, err error) {
	err = o.read(ctx, func(x store.Ops) error { v, err = x.ListLedgerProjects(ctx, after); return err })
	return
}

func (o *faultyOps) GetLedgerHead(ctx context.Context, projectID string) (v string, err error) {
	err = o.read(ctx, func(x store.Ops) error { v, err = x.GetLedgerHead(ctx, projectID); return err })
	return
}

func (o *faultyOps) GetLedgerIndex(ctx context.Context, projectID string) (v store.LedgerIndex, err error) {
	err = o.read(ctx, func(x store.Ops) error { v, err = x.GetLedgerIndex(ctx, projectID); return err })
	return
}

// ReplaceLedgerIndex is one write point even when it runs several bounded
// transactions, and Faulty holds its lock for the whole call, so it cannot
// crash between batches. The sqlite package's own tests cover those
// crashes with a hook between its transactions.
func (o *faultyOps) ReplaceLedgerIndex(ctx context.Context, projectID, expected, head string, retainPrefix bool, entries []store.LedgerEntry, versions iter.Seq2[store.LedgerVersion, error]) error {
	return o.write(ctx, func(x store.Ops) error {
		return x.ReplaceLedgerIndex(ctx, projectID, expected, head, retainPrefix, entries, versions)
	})
}

func (o *faultyOps) AdvanceLedgerIndex(ctx context.Context, projectID, parent, head string, entries []store.LedgerEntry, versions []store.LedgerVersion) error {
	return o.write(ctx, func(x store.Ops) error { return x.AdvanceLedgerIndex(ctx, projectID, parent, head, entries, versions) })
}

func (o *faultyOps) ListLedgerHistory(ctx context.Context, projectID string, number uint32, after string, limit int) (v []store.LedgerVersion, more bool, err error) {
	err = o.read(ctx, func(x store.Ops) error {
		v, more, err = x.ListLedgerHistory(ctx, projectID, number, after, limit)
		return err
	})
	return
}

func (o *faultyOps) GetLedgerNextNumber(ctx context.Context, projectID string) (v uint32, err error) {
	err = o.read(ctx, func(x store.Ops) error { v, err = x.GetLedgerNextNumber(ctx, projectID); return err })
	return
}

func (o *faultyOps) CreateLedgerChangeset(ctx context.Context, c store.LedgerChangeset) error {
	return o.write(ctx, func(x store.Ops) error { return x.CreateLedgerChangeset(ctx, c) })
}

func (o *faultyOps) GetLedgerChangeset(ctx context.Context, id string) (v store.LedgerChangeset, err error) {
	err = o.read(ctx, func(x store.Ops) error { v, err = x.GetLedgerChangeset(ctx, id); return err })
	return
}

func (o *faultyOps) ListLedgerChangesets(ctx context.Context, f store.LedgerChangesetFilter) (v []store.LedgerChangeset, more bool, err error) {
	err = o.read(ctx, func(x store.Ops) error { v, more, err = x.ListLedgerChangesets(ctx, f); return err })
	return
}

func (o *faultyOps) ListLedgerRepairs(ctx context.Context, projectID string) (v []store.LedgerChangeset, err error) {
	err = o.read(ctx, func(x store.Ops) error { v, err = x.ListLedgerRepairs(ctx, projectID); return err })
	return
}

func (o *faultyOps) TouchLedgerChangeset(ctx context.Context, id string, at int64) (ok bool, err error) {
	err = o.write(ctx, func(x store.Ops) error { ok, err = x.TouchLedgerChangeset(ctx, id, at); return err })
	return
}

func (o *faultyOps) PrepareLedgerMerge(ctx context.Context, id string, m store.LedgerMerge, at int64) (ok bool, err error) {
	err = o.write(ctx, func(x store.Ops) error { ok, err = x.PrepareLedgerMerge(ctx, id, m, at); return err })
	return
}

func (o *faultyOps) ClearLedgerMerge(ctx context.Context, id, commit string) (ok bool, err error) {
	err = o.write(ctx, func(x store.Ops) error { ok, err = x.ClearLedgerMerge(ctx, id, commit); return err })
	return
}

func (o *faultyOps) FinishLedgerMerge(ctx context.Context, id, commit string, at int64) (ok bool, err error) {
	err = o.write(ctx, func(x store.Ops) error { ok, err = x.FinishLedgerMerge(ctx, id, commit, at); return err })
	return
}

func (o *faultyOps) DiscardLedgerChangeset(ctx context.Context, id, reason string, archived bool, at int64) (ok bool, err error) {
	err = o.write(ctx, func(x store.Ops) error { ok, err = x.DiscardLedgerChangeset(ctx, id, reason, archived, at); return err })
	return
}

func (o *faultyOps) ArchiveLedgerChangeset(ctx context.Context, id string) error {
	return o.write(ctx, func(x store.Ops) error { return x.ArchiveLedgerChangeset(ctx, id) })
}

// PurgeOrphanLedgerChangesets is one write point, like ReplaceLedgerIndex.
func (o *faultyOps) PurgeOrphanLedgerChangesets(ctx context.Context) error {
	return o.write(ctx, func(x store.Ops) error { return x.PurgeOrphanLedgerChangesets(ctx) })
}
