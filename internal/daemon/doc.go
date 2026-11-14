// Package daemon owns device-side execution without retaining conversation state.
// Submit starts an independent call: running calls become completed results,
// retained in memory until ResultTTL. Query then reports lost, while a tombstone
// prevents resubmission until TombstoneTTL after completion. Restart loses all
// records. Duplicate running or retained calls return their existing state,
// including during shutdown and regardless of dispatch age. An unseen call is
// expired when its Welcome-anchored age exceeds MaxDispatchAge (12 hours by
// default), leaving no record; a fresh unseen call gets SHUTTING_DOWN once
// shutdown begins. MaxDispatchAge plus the link handshake timeout must remain
// below TombstoneTTL.
//
// Call deadlines and cancellation belong to the daemon. Cancelling a Wait only
// detaches that waiter. Shutdown cancels running calls. A tool that ignores its
// deadline is abandoned five seconds after that deadline; a tool that ignores
// cancellation is independently abandoned five seconds after Cancel or Shutdown
// requests it. Result callbacks run after storage, outside the dispatcher lock,
// in completion order, and must return promptly. Calls validate required call_id
// and session_id, and optional agent_id, against [A-Za-z0-9_-], at most 128 bytes,
// before hashing or execution. ErrInvalidCall distinguishes invalid requests.
// Daemon failures carry a fixed-code fault; tool-reported errors do not.
//
// Result callback backlog is capped at 128 MiB of serialized data and 4096
// entries by default, separate from retained authoritative results. Overflow
// drops the notification and invokes the nonblocking OnOverflow hook; link
// clients disconnect and recover by Query. One callback additionally owns one
// encoded result and its decoded protobuf while running. Queue slice overhead
// is at most about 256 KiB (4096 entries with growth). Retained calls/results
// themselves remain TTL-based, without a count cap; this is not a process RSS
// bound. See daemon/link for the independently bounded outbound queue.
//
// Defaults: a two-minute call timeout, requests clamped to ten minutes,
// thirty-minute result retention, a twelve-hour maximum dispatch age,
// twenty-four-hour tombstones, and thirty-minute retention of ended background
// jobs.
//
// Tools may register background jobs with jobs.Manager passed at construction.
// DefaultTools takes explicit Bash Options; each Bash owns its snapshot and output stores.
// Jobs belong to sessions and creating calls, and emit running followed by exited
// or killed events with a boot-wide sequence allocated under the manager lock.
// Snapshot atomically captures that high-water mark and retained job states;
// oversize sets return a RUNNING-first bounded prefix, mark Truncated, and
// return jobs.ErrSnapshotLimit. Omissions from truncated snapshots imply nothing.
// RequestStopSession never waits for Job.Stop; each running job gets at most one
// stop goroutine.
// Individual stops, session stops, and shutdown request termination; ended jobs
// remain queryable until retention expires. Output files belong to the tools.
package daemon
