// Package daemon owns device-side execution without retaining conversation state.
// Submit starts an independent call: running calls become completed results,
// retained in memory until ResultTTL. Query then reports lost, while a tombstone
// prevents resubmission until TombstoneTTL after completion. Restart loses all
// records. Duplicate running or retained calls return their existing state.
//
// Call deadlines and cancellation belong to the daemon. Cancelling a Wait only
// detaches that waiter. Shutdown cancels running calls. A tool that ignores its
// deadline is abandoned five seconds after that deadline; a tool that ignores
// cancellation is independently abandoned five seconds after Cancel or Shutdown
// requests it. Result callbacks run after storage, outside the dispatcher lock,
// in completion order, and must return promptly.
//
// Defaults: a two-minute call timeout, requests clamped to ten minutes,
// thirty-minute result retention, twenty-four-hour tombstones, and
// thirty-minute retention of ended background jobs.
//
// Tools may register background jobs with jobs.Manager passed at construction.
// DefaultTools takes explicit Bash Options; each Bash owns its snapshot and output stores.
// Jobs belong to sessions and emit running followed by exited or killed events.
// Individual stops, session stops, and shutdown request termination; ended jobs
// remain queryable until retention expires. Output files belong to the tools.
package daemon
