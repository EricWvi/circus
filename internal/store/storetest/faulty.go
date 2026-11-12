package storetest

import (
	"context"
	"errors"
	"sync"

	"github.com/EricWvi/circus/internal/store"
)

// ErrInjected is the default error of an injected write failure.
var ErrInjected = errors.New("storetest: injected write failure")

// ErrCrashed is returned by every operation on a Faulty store once its
// crash point has fired: the process it stands for is dead.
var ErrCrashed = errors.New("storetest: store crashed")

// Faulty wraps a Store and injects faults at write points.
//
// A write point is each call of a mutating Ops method (every method except
// the Get*, List* and LookupCredential reads), inside or outside a Tx, and
// each Tx commit (reached when fn returns nil). Points are numbered from 1
// in the order they happen; Writes reports how many have happened.
//
// A failed point returns its error without reaching the inner store, so the
// failed write leaves nothing behind; a failed commit rolls the transaction
// back. A crash point lets the write (or commit) through, then calls the
// crash hook while the transaction, if any, is still open, and from then on
// every operation, including the rest of the open Tx, returns ErrCrashed;
// the open Tx is rolled back whatever fn returns. Process uses the hook to
// snapshot the files a killed process would leave behind.
//
// Faulty serializes write points (a Tx holds the lock for its whole run),
// so a crash never races another write. Like every Store, it refuses to be
// called with the ctx of its own open Tx (store.CheckOutsideTx), before
// taking that lock, so re-entrant use fails instead of deadlocking. The
// lock honours ctx: a caller still waiting for it when its ctx ends gets the
// ctx's error, as it would waiting for a lock in a real backend.
type Faulty struct {
	faultyOps
	inner store.Store

	sem     chan struct{} // capacity 1: serializes write points and Tx
	state   sync.Mutex    // guards the fields below
	writes  int
	failAt  int
	failErr error
	failAll error
	crashAt int
	onCrash func()
	crashed bool
}

var _ store.Store = (*Faulty)(nil)

// NewFaulty wraps inner. With no faults armed it passes everything through.
func NewFaulty(inner store.Store) *Faulty {
	f := &Faulty{inner: inner, sem: make(chan struct{}, 1)}
	f.faultyOps = faultyOps{f: f, s: inner, top: true}
	return f
}

// Inner returns the wrapped store.
func (f *Faulty) Inner() store.Store { return f.inner }

// FailWrite makes write point Writes()+n fail with err (ErrInjected when
// nil), once. Pass store.ErrBusy to emulate lock contention.
func (f *Faulty) FailWrite(n int, err error) {
	if err == nil {
		err = ErrInjected
	}
	f.state.Lock()
	defer f.state.Unlock()
	f.failAt, f.failErr = f.writes+n, err
}

// FailAllWrites makes every write point fail with err until it is called
// with nil, as when the disk is full.
func (f *Faulty) FailAllWrites(err error) {
	f.state.Lock()
	defer f.state.Unlock()
	f.failAll = err
}

// CrashAfterWrite arms a crash after write point Writes()+n. onCrash, which
// may be nil, runs synchronously at that moment.
func (f *Faulty) CrashAfterWrite(n int, onCrash func()) {
	f.state.Lock()
	defer f.state.Unlock()
	f.crashAt, f.onCrash = f.writes+n, onCrash
}

// Writes returns the number of write points so far.
func (f *Faulty) Writes() int {
	f.state.Lock()
	defer f.state.Unlock()
	return f.writes
}

// Crashed reports whether the crash point has fired.
func (f *Faulty) Crashed() bool {
	f.state.Lock()
	defer f.state.Unlock()
	return f.crashed
}

// Close closes the inner store.
func (f *Faulty) Close() error { return f.inner.Close() }

// lock takes the write lock, or returns ctx's error once ctx ends first.
func (f *Faulty) lock(ctx context.Context) (unlock func(), err error) {
	select {
	case f.sem <- struct{}{}:
		return func() { <-f.sem }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Tx implements store.Store.
func (f *Faulty) Tx(ctx context.Context, fn func(ctx context.Context, tx store.Ops) error) error {
	if err := store.CheckOutsideTx(ctx, f); err != nil {
		return err
	}
	unlock, err := f.lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	if f.Crashed() {
		return ErrCrashed
	}
	ctx, exit := store.EnterTx(ctx, f)
	defer exit()
	crashAtCommit := false
	err = f.inner.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		err := fn(ctx, &faultyOps{f: f, s: tx})
		if f.Crashed() {
			return ErrCrashed
		}
		if err != nil {
			return err
		}
		fail, crash := f.point()
		if fail != nil {
			return fail
		}
		crashAtCommit = crash
		return nil
	})
	if crashAtCommit {
		f.crash()
		return ErrCrashed
	}
	return err
}

// point records one write point and reports whether it fails or crashes.
func (f *Faulty) point() (fail error, crash bool) {
	f.state.Lock()
	defer f.state.Unlock()
	f.writes++
	switch {
	case f.failAll != nil:
		return f.failAll, false
	case f.writes == f.failAt:
		return f.failErr, false
	}
	return nil, f.writes == f.crashAt
}

func (f *Faulty) crash() {
	f.state.Lock()
	hook := f.onCrash
	f.state.Unlock()
	if hook != nil {
		hook()
	}
	f.state.Lock()
	f.crashed = true
	f.state.Unlock()
}

// faultyOps wraps the Ops of the inner store (top) or of an inner tx.
type faultyOps struct {
	f   *Faulty
	s   store.Ops
	top bool
}

// enter rejects a top-level call made with the ctx of this Faulty's own
// open Tx.
func (o *faultyOps) enter(ctx context.Context) error {
	if o.top {
		return store.CheckOutsideTx(ctx, o.f)
	}
	return nil
}

func (o *faultyOps) read(ctx context.Context, do func(store.Ops) error) error {
	if err := o.enter(ctx); err != nil {
		return err
	}
	if o.f.Crashed() {
		return ErrCrashed
	}
	return do(o.s)
}

func (o *faultyOps) write(ctx context.Context, do func(store.Ops) error) error {
	if err := o.enter(ctx); err != nil {
		return err
	}
	if o.top {
		unlock, err := o.f.lock(ctx)
		if err != nil {
			return err
		}
		defer unlock()
	}
	if o.f.Crashed() {
		return ErrCrashed
	}
	fail, crash := o.f.point()
	if fail != nil {
		return fail
	}
	err := do(o.s)
	if crash {
		o.f.crash()
		return ErrCrashed
	}
	return err
}

func (o *faultyOps) CreateUser(ctx context.Context, u store.User) error {
	return o.write(ctx, func(s store.Ops) error { return s.CreateUser(ctx, u) })
}

func (o *faultyOps) GetUser(ctx context.Context, id string) (u store.User, err error) {
	err = o.read(ctx, func(s store.Ops) error { u, err = s.GetUser(ctx, id); return err })
	return u, err
}

func (o *faultyOps) GetUserByName(ctx context.Context, name string) (u store.User, err error) {
	err = o.read(ctx, func(s store.Ops) error { u, err = s.GetUserByName(ctx, name); return err })
	return u, err
}

func (o *faultyOps) CreateIdentity(ctx context.Context, id store.Identity) error {
	return o.write(ctx, func(s store.Ops) error { return s.CreateIdentity(ctx, id) })
}

func (o *faultyOps) GetIdentity(ctx context.Context, kind store.PrincipalKind, id string) (v store.Identity, err error) {
	err = o.read(ctx, func(s store.Ops) error { v, err = s.GetIdentity(ctx, kind, id); return err })
	return v, err
}

func (o *faultyOps) GetIdentityByName(ctx context.Context, kind store.PrincipalKind, userID, name string) (v store.Identity, err error) {
	err = o.read(ctx, func(s store.Ops) error { v, err = s.GetIdentityByName(ctx, kind, userID, name); return err })
	return v, err
}

func (o *faultyOps) ListIdentities(ctx context.Context, kind store.PrincipalKind, userID string) (v []store.Identity, err error) {
	err = o.read(ctx, func(s store.Ops) error { v, err = s.ListIdentities(ctx, kind, userID); return err })
	return v, err
}

func (o *faultyOps) SetConnected(ctx context.Context, kind store.PrincipalKind, id, bootID string, at int64) (prev string, err error) {
	err = o.write(ctx, func(s store.Ops) error { prev, err = s.SetConnected(ctx, kind, id, bootID, at); return err })
	return prev, err
}

func (o *faultyOps) SetDisconnected(ctx context.Context, kind store.PrincipalKind, id string, at int64) error {
	return o.write(ctx, func(s store.Ops) error { return s.SetDisconnected(ctx, kind, id, at) })
}

func (o *faultyOps) DeleteIdentity(ctx context.Context, kind store.PrincipalKind, id string, at int64) (ok bool, err error) {
	err = o.write(ctx, func(s store.Ops) error { ok, err = s.DeleteIdentity(ctx, kind, id, at); return err })
	return ok, err
}

func (o *faultyOps) CreateCredential(ctx context.Context, c store.Credential) error {
	return o.write(ctx, func(s store.Ops) error { return s.CreateCredential(ctx, c) })
}

func (o *faultyOps) LookupCredential(ctx context.Context, tokenHash []byte) (c store.Credential, err error) {
	err = o.read(ctx, func(s store.Ops) error { c, err = s.LookupCredential(ctx, tokenHash); return err })
	return c, err
}

func (o *faultyOps) TouchCredential(ctx context.Context, id string, at int64) error {
	return o.write(ctx, func(s store.Ops) error { return s.TouchCredential(ctx, id, at) })
}

func (o *faultyOps) RevokeCredentials(ctx context.Context, kind store.PrincipalKind, principalID string, at int64) (n int, err error) {
	err = o.write(ctx, func(s store.Ops) error { n, err = s.RevokeCredentials(ctx, kind, principalID, at); return err })
	return n, err
}

func (o *faultyOps) CreateEnrollToken(ctx context.Context, t store.EnrollToken) error {
	return o.write(ctx, func(s store.Ops) error { return s.CreateEnrollToken(ctx, t) })
}

func (o *faultyOps) UseEnrollToken(ctx context.Context, tokenHash []byte, now int64, usedBy string) (t store.EnrollToken, err error) {
	err = o.write(ctx, func(s store.Ops) error { t, err = s.UseEnrollToken(ctx, tokenHash, now, usedBy); return err })
	return t, err
}

func (o *faultyOps) PurgeEnrollTokens(ctx context.Context, now int64) (n int, err error) {
	err = o.write(ctx, func(s store.Ops) error { n, err = s.PurgeEnrollTokens(ctx, now); return err })
	return n, err
}

func (o *faultyOps) CreateCall(ctx context.Context, c store.Call) error {
	return o.write(ctx, func(s store.Ops) error { return s.CreateCall(ctx, c) })
}

func (o *faultyOps) GetCall(ctx context.Context, callID string) (c store.Call, err error) {
	err = o.read(ctx, func(s store.Ops) error { c, err = s.GetCall(ctx, callID); return err })
	return c, err
}

func (o *faultyOps) ListCalls(ctx context.Context, f store.CallFilter) (v []store.Call, err error) {
	err = o.read(ctx, func(s store.Ops) error { v, err = s.ListCalls(ctx, f); return err })
	return v, err
}

func (o *faultyOps) TransitionCall(ctx context.Context, callID string, expect store.CallExpect, to store.CallState, patch store.CallPatch) (ok bool, err error) {
	err = o.write(ctx, func(s store.Ops) error { ok, err = s.TransitionCall(ctx, callID, expect, to, patch); return err })
	return ok, err
}

func (o *faultyOps) PatchCall(ctx context.Context, callID string, expect store.CallExpect, patch store.CallPatch) (ok bool, err error) {
	err = o.write(ctx, func(s store.Ops) error { ok, err = s.PatchCall(ctx, callID, expect, patch); return err })
	return ok, err
}

func (o *faultyOps) PutJob(ctx context.Context, j store.Job) (ok bool, err error) {
	err = o.write(ctx, func(s store.Ops) error { ok, err = s.PutJob(ctx, j); return err })
	return ok, err
}

func (o *faultyOps) ListJobs(ctx context.Context, f store.JobFilter) (v []store.Job, err error) {
	err = o.read(ctx, func(s store.Ops) error { v, err = s.ListJobs(ctx, f); return err })
	return v, err
}

func (o *faultyOps) LoseJobs(ctx context.Context, deviceID, keepBootID string, at int64) (n int, err error) {
	err = o.write(ctx, func(s store.Ops) error { n, err = s.LoseJobs(ctx, deviceID, keepBootID, at); return err })
	return n, err
}

func (o *faultyOps) LoseCalls(ctx context.Context, deviceID, keepBootID string, at int64, reason string) (v []store.LostCall, err error) {
	err = o.write(ctx, func(s store.Ops) error { v, err = s.LoseCalls(ctx, deviceID, keepBootID, at, reason); return err })
	return v, err
}

func (o *faultyOps) SetTimer(ctx context.Context, kind, subjectID string, dueAt int64) error {
	return o.write(ctx, func(s store.Ops) error { return s.SetTimer(ctx, kind, subjectID, dueAt) })
}

func (o *faultyOps) CancelTimer(ctx context.Context, kind, subjectID string) error {
	return o.write(ctx, func(s store.Ops) error { return s.CancelTimer(ctx, kind, subjectID) })
}

func (o *faultyOps) ClaimDueTimers(ctx context.Context, now int64, instanceID string, lease int64, limit int) (v []store.Timer, err error) {
	err = o.write(ctx, func(s store.Ops) error { v, err = s.ClaimDueTimers(ctx, now, instanceID, lease, limit); return err })
	return v, err
}

func (o *faultyOps) CompleteTimer(ctx context.Context, t store.Timer) (ok bool, err error) {
	err = o.write(ctx, func(s store.Ops) error { ok, err = s.CompleteTimer(ctx, t); return err })
	return ok, err
}

func (o *faultyOps) ListTimers(ctx context.Context) (v []store.Timer, err error) {
	err = o.read(ctx, func(s store.Ops) error { v, err = s.ListTimers(ctx); return err })
	return v, err
}
