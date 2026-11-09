// Package daemon owns device-side execution without retaining conversation state.
// Submit starts an independent call: running calls become completed results,
// retained in memory until ResultTTL. Query then reports lost, while a tombstone
// prevents resubmission until TombstoneTTL after completion. Restart loses all
// records. Duplicate running or retained calls return their existing state.
//
// Call deadlines and cancellation belong to the daemon. Cancelling a Wait only
// detaches that waiter. Shutdown cancels running calls; a tool that ignores its
// deadline is abandoned five seconds later. Result callbacks run after storage,
// outside the dispatcher lock, in completion order, and must return promptly.
//
// Tools may register background jobs with a JobManager passed at construction.
// Jobs belong to sessions and emit running followed by exited or killed events.
// Individual stops, session stops, and shutdown request termination; ended jobs
// remain queryable until retention expires. Output files belong to the tools.
package daemon
