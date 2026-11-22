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
