package store

import (
	"context"
	"sync/atomic"
)

// txKey marks a context as belonging to an open transaction of one store
// handle (owner). Keys compare by owner, so marks of nested wrappers, such
// as storetest.Faulty around a backend, coexist in one context.
type txKey struct{ owner any }

// EnterTx returns ctx marked as belonging to an open transaction of owner,
// and the function that ends the mark. Backends call it in Tx and pass the
// marked context to fn; while the mark is open, CheckOutsideTx(ctx, owner)
// fails for that context and every context derived from it. After exit the
// context is ordinary again, so a context that outlives its Tx stays usable.
func EnterTx(ctx context.Context, owner any) (context.Context, func()) {
	open := new(atomic.Bool)
	open.Store(true)
	return context.WithValue(ctx, txKey{owner}, open), func() { open.Store(false) }
}

// CheckOutsideTx returns ErrInvalid when ctx belongs to an open transaction
// of owner. Backends call it at the start of Tx and of every Ops method
// called on the store handle itself, so re-entrant use from inside fn fails
// at once instead of waiting for the transaction fn itself holds.
func CheckOutsideTx(ctx context.Context, owner any) error {
	if open, ok := ctx.Value(txKey{owner}).(*atomic.Bool); ok && open.Load() {
		return invalid("store handle called from inside its own open Tx; inside fn, use the tx fn is given")
	}
	return nil
}
