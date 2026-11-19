// Package store defines the control plane's durable state: domain types and
// the Store interface that every storage backend implements.
//
// The store keeps metadata only. Calls carry who, when, which device, which
// tool, the request hash and size, the outcome and timings; request content
// and tool output never reach it.
//
// Conventions shared by every backend:
//
//   - Times are int64 Unix milliseconds supplied by the caller, and
//     durations (the timer lease) are int64 milliseconds; the store has no
//     clock. Zero means "not set" and is stored as NULL.
//   - IDs are opaque strings chosen by the caller (UUIDv7 for rows the control
//     plane creates, daemon-chosen ids for calls and jobs). The only value the
//     store generates is a timer claim token.
//   - Token columns hold SHA-256 hashes; plaintext tokens never reach the
//     store.
//   - Errors are this package's Err* sentinels (ErrNotFound, ErrExists,
//     ErrTokenUnusable, ErrInvalidTransition, ErrInvalid, ErrBusy, and for
//     sessions ErrFenced, ErrLeaseHeld, ErrTranscriptFull, ErrLimit,
//     ErrStopped, ErrBroken, ErrSessionActive, ErrTooManyCalls), possibly
//     wrapped; a fenced operation returns *FencedError, which matches
//     ErrFenced. No driver type or SQL dialect crosses the interface.
//
// Sessions (SessionOps) add leases with a monotonic epoch, bound to one
// connection: every worker operation carries a Fence checked in its own
// transaction, and every session table and string has a bound the store
// enforces. The rules are on
// SessionOps.
//
// Identities are per machine × role: a device (daemon role) and
// a worker are separate identities in tables of the same shape, both linked to
// the machine they run on by MachineID, each with its own credentials, so one
// role can be revoked without the other.
//
// Web users own their machine identities. UserIdentity binds OIDC
// issuer/subject pairs; WebSession contains only a cookie token hash, with
// caller-supplied idle and maximum deadlines.
//
// Projects (ProjectOps) belong to one user and hold an optional repository
// URL and 0 to 16 locations, each a device id and a path (empty for the
// device's default directory, at most one per device). Names are unique per user under simple case
// folding (ProjectNameKey). A location references a device of the same user
// by id; reads return the device's name.
//
// Package storetest holds the contract suite every implementation must pass
// plus fault-injection and crash-restart harnesses; package sqlite is the
// SQLite backend.
//
// The suite's crash point emulates a process crash (kill -9): the restarted
// store sees the files as the dead process left them. It does not emulate
// power loss, which can also drop writes the OS had not yet flushed; that
// case is covered only indirectly, by the SQLite test that proves
// synchronous=FULL is in effect, so every commit is on disk before Tx
// returns.
package store
