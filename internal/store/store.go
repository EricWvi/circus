package store

import "context"

// Store is a handle on the control plane database. It is safe for
// concurrent use. Every Ops method called on the Store directly runs in its
// own transaction.
type Store interface {
	Ops

	// Tx runs fn in one transaction and commits when fn returns nil. Any
	// error from fn, or a panic, rolls back every write fn made. Domain
	// writes and the SetTimer/CancelTimer calls that belong to them go into
	// the same Tx, so a timer never outlives or predates the state it guards.
	//
	// fn gets ctx marked as inside this transaction and must use only tx.
	// While fn runs, calling this Store (an Ops method or a nested Tx) with
	// that ctx, or one derived from it, fails at once with ErrInvalid; on a
	// single-writer backend it would otherwise wait for the transaction fn
	// itself holds. Name the parameter ctx so it shadows the outer one: a
	// call made with an unmarked outer ctx cannot be detected.
	//
	// Transactions are serializable: concurrent Txs, on this handle or any
	// other handle on the same database, have the effect of running one
	// after another, so fn may read a row and write based on what it read
	// (for example read a device's current boot, then dispatch to it). A
	// backend that detects a conflict instead of waiting fails the Tx with
	// ErrBusy and leaves nothing behind.
	//
	// The error is fn's error, or the commit's. A transaction that cannot
	// start or commit because of lock contention returns ErrBusy.
	Tx(ctx context.Context, fn func(ctx context.Context, tx Ops) error) error

	// Close releases the database. Calls after Close fail.
	Close() error
}

// Ops is every domain operation. It is implemented by the Store (each call
// is its own transaction) and by the tx passed to Store.Tx.
//
// Methods return ErrInvalid for malformed arguments before reading or
// writing anything.
type Ops interface {
	// CreateUser inserts a user. ErrExists if the id or name is taken.
	CreateUser(ctx context.Context, u User) error
	// GetUser returns the user with this id, or ErrNotFound.
	GetUser(ctx context.Context, id string) (User, error)
	// GetUserByName returns the user with this name, or ErrNotFound.
	GetUserByName(ctx context.Context, name string) (User, error)

	// CreateIdentity inserts a device or worker, by id.Kind. ErrExists if the
	// id is taken or the user already has a live identity of that kind with
	// that name; ErrNotFound if the user does not exist.
	CreateIdentity(ctx context.Context, id Identity) error
	// GetIdentity returns the identity, deleted or not, or ErrNotFound.
	GetIdentity(ctx context.Context, kind PrincipalKind, id string) (Identity, error)
	// GetIdentityByName returns the user's live identity with this name, or
	// ErrNotFound.
	GetIdentityByName(ctx context.Context, kind PrincipalKind, userID, name string) (Identity, error)
	// ListIdentities returns the user's live identities of this kind, by name.
	ListIdentities(ctx context.Context, kind PrincipalKind, userID string) ([]Identity, error)
	// SetConnected records a connection from boot bootID at time at and
	// returns the boot recorded before ("" if never connected), read and
	// replaced atomically. The caller compares the two to detect a daemon
	// restart (outline 3.3, reconnect sync step 1). ErrNotFound if absent.
	SetConnected(ctx context.Context, kind PrincipalKind, id, bootID string, at int64) (prevBootID string, err error)
	// SetDisconnected records the end of the identity's connection at time
	// at. CurrentBootID is kept: a later SetConnected compares against it.
	SetDisconnected(ctx context.Context, kind PrincipalKind, id string, at int64) error
	// DeleteIdentity soft-deletes a live identity: it sets DeletedAt = at,
	// which frees the name for a new identity of that kind, and returns
	// true. It returns false with no change when the identity is already
	// deleted; ErrNotFound if absent. Credentials are not touched; revoke
	// them in the same Tx (RevokeCredentials).
	DeleteIdentity(ctx context.Context, kind PrincipalKind, id string, at int64) (bool, error)

	// CreateCredential inserts a credential. ErrNotFound if the identity it
	// names does not exist; ErrExists if the id or token hash is taken.
	CreateCredential(ctx context.Context, c Credential) error
	// LookupCredential returns the credential with this token hash, or
	// ErrNotFound. Revoked credentials are returned too, so the caller can
	// tell a revoked token (stop reconnecting) from an unknown one; the
	// caller must check Revoked.
	LookupCredential(ctx context.Context, tokenHash []byte) (Credential, error)
	// TouchCredential sets LastUsedAt. ErrNotFound if absent.
	TouchCredential(ctx context.Context, id string, at int64) error
	// RevokeCredentials revokes every unrevoked credential of one identity
	// at time at and returns how many it revoked. Revoking a device leaves
	// the worker on the same machine untouched (D43 item 8).
	RevokeCredentials(ctx context.Context, kind PrincipalKind, principalID string, at int64) (int, error)

	// CreateEnrollToken inserts a one-time token. ErrExists if the id or
	// hash is taken; ErrNotFound if the user does not exist.
	CreateEnrollToken(ctx context.Context, t EnrollToken) error
	// UseEnrollToken marks the token with this hash used, if it is unused
	// and now < ExpiresAt, and returns it. Of any number of concurrent uses
	// exactly one succeeds. Otherwise it returns ErrTokenUnusable, whatever
	// the reason. Run it in the same Tx that creates the identities and
	// credentials, so a failed enrollment does not burn the token.
	UseEnrollToken(ctx context.Context, tokenHash []byte, now int64, usedBy string) (EnrollToken, error)
	// PurgeEnrollTokens deletes unused tokens that expired at or before now
	// and returns how many. Used tokens stay as an audit record.
	PurgeEnrollTokens(ctx context.Context, now int64) (int, error)

	// CreateCall inserts a call (see CheckNewCall). ErrExists if the call id
	// is taken: the caller then reads the row and compares request hashes
	// (outline 4.4 item 5). ErrNotFound if the user is missing or the device
	// does not exist under that user.
	CreateCall(ctx context.Context, c Call) error
	// GetCall returns the call, or ErrNotFound.
	GetCall(ctx context.Context, callID string) (Call, error)
	// ListCalls returns at most f.Limit rows ordered by (submitted_at, call_id).
	ListCalls(ctx context.Context, f CallFilter) ([]Call, error)
	// TransitionCall moves the call to state to and applies patch, only if
	// the call matches expect (see CallExpect). Otherwise it returns false
	// and changes nothing. Of concurrent transitions out of the same state
	// exactly one returns true. Every expected state → to must be an edge of
	// the state machine, else ErrInvalidTransition; patch.DispatchedBootID
	// is required on the transition to dispatched and refused on every
	// other (CheckTransition), else ErrInvalid. The transitions to
	// dispatched and to accepted return false for a call that already has a
	// DispatchedBootID, even one in an expected state (a lost call that had
	// been dispatched); lost → accepted is the resubmission (see CallState).
	// ErrNotFound if the call does not exist.
	//
	// Dispatching: the router reads the device's CurrentBootID in the same
	// Tx (GetIdentity) and passes it as patch.DispatchedBootID. The store
	// does not compare the two; a boot read outside the Tx may already be
	// replaced, and the call would then wait in dispatched for the next
	// boot change or its watchdog instead of being lost at once.
	TransitionCall(ctx context.Context, callID string, expect CallExpect, to CallState, patch CallPatch) (bool, error)
	// PatchCall applies patch without changing the state, only if the call
	// matches expect; it returns false otherwise. patch must set at least
	// one column and never DispatchedBootID (CheckPatch), else ErrInvalid.
	// Example: a cancel for a call that is dispatched, running or unknown
	// records CancelRequestedAt. ErrNotFound if the call does not exist.
	PatchCall(ctx context.Context, callID string, expect CallExpect, patch CallPatch) (bool, error)

	// PutJob inserts the job, or updates it when j.LastSeq is greater than
	// the stored one and the stored job is still running; stale or repeated
	// events, and any event for a job that has ended (exited, killed, lost),
	// return false with no change.
	// Empty SessionID or CallID never overwrite stored values, so a job row
	// created from an event can be completed by a later one (F22).
	PutJob(ctx context.Context, j Job) (bool, error)
	// ListJobs returns at most f.Limit rows ordered by (device_id, boot_id, job_id).
	ListJobs(ctx context.Context, f JobFilter) ([]Job, error)
	// LoseJobs marks every running job of the device whose boot is not
	// keepBootID as lost at time at, and returns how many. It is called when
	// the device connects with a new boot (outline 3.3 step 1).
	LoseJobs(ctx context.Context, deviceID, keepBootID string, at int64) (int, error)
	// LoseCalls moves at most limit (1..MaxPageSize) dispatched, running or unknown calls of the
	// device whose DispatchedBootID is not keepBootID to lost, sets
	// FinishedAt = at and OutcomeReason = reason (a short code, at most
	// MaxCodeBytes), and returns those calls with the state each left,
	// oldest submission first. It is called with LoseJobs, in the same Tx,
	// when the device connects with a new boot (outline 3.3 reconnect sync
	// step 1, F4). Comparing with the new boot rather than the previous one
	// also catches calls dispatched to a boot that was never recorded as
	// current. The caller uses the result to notify the calls' subscribers
	// and to cancel their timers (call_watchdog) in the same Tx.
	LoseCalls(ctx context.Context, deviceID, keepBootID string, at int64, reason string, limit int) ([]LostCall, error)

	// SetTimer creates the (kind, subjectID) timer or moves it to dueAt,
	// clearing any claim and attempt count.
	SetTimer(ctx context.Context, kind, subjectID string, dueAt int64) error
	// CancelTimer deletes the timer, claimed or not. A missing timer is not
	// an error.
	CancelTimer(ctx context.Context, kind, subjectID string) error
	// ClaimDueTimers claims up to limit timers with DueAt <= now whose claim
	// is absent or ended (ClaimUntil <= now), earliest due first. lease is
	// in milliseconds, at least 1. Each claim
	// sets ClaimedBy = instanceID, ClaimUntil = now + lease, increments
	// Attempts and issues a fresh Token. A timer is held by at most one
	// claim at a time. A claimant that dies simply lets the lease run out;
	// any instance then re-claims the timer. Handlers must be idempotent.
	//
	// A handler acts on a claimed timer in one Tx that calls CompleteTimer
	// first and, if it returns false, aborts without writing anything: the
	// timer was cancelled (for example a device_grace timer by the device's
	// reconnect), re-set or re-claimed after this claim, and its effects
	// must not apply. Completing at the end instead would let a cancelled
	// timer's handler write its effects first.
	ClaimDueTimers(ctx context.Context, now int64, instanceID string, lease int64, limit int) ([]Timer, error)
	// CompleteTimer deletes a claimed timer, only while t's claim is still
	// the current one (same Token). It returns false when the timer was
	// cancelled, re-set or re-claimed since: the newer state wins. Call it
	// first in the handler's Tx and abort that Tx on false (see
	// ClaimDueTimers).
	CompleteTimer(ctx context.Context, t Timer) (bool, error)
	// ListTimers returns a bounded page ordered by (due_at, kind, subject_id).
	ListTimers(ctx context.Context, f TimerFilter) ([]Timer, error)
}
