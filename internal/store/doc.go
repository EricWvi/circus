// Package store defines the control plane's durable state: domain types and
// the Store interface that every storage backend implements.
//
// The store keeps metadata only. Calls carry who, when, which device, which
// tool, the request hash and size, the outcome and timings; request content
// and tool output never reach it (D9 item 3, D11 item 3 as clarified by D43).
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
//     ErrTokenUnusable, ErrInvalidTransition, ErrInvalid, ErrBusy), possibly
//     wrapped. No driver type or SQL dialect crosses the interface.
//
// Identities are per machine × role (D43 item 8): a device (daemon role) and
// a worker are separate identities in tables of the same shape, both linked to
// the machine they run on by MachineID, each with its own credentials, so one
// role can be revoked without the other.
//
// Package storetest holds the contract suite every implementation must pass
// and a fault-injection wrapper; package sqlite is the SQLite backend.
//
// The suite's crash point emulates a process crash (kill -9): the restarted
// store sees the files as the dead process left them. It does not emulate
// power loss, which can also drop writes the OS had not yet flushed; that
// case is covered only indirectly, by the SQLite test that proves
// synchronous=FULL is in effect, so every commit is on disk before Tx
// returns.
package store
