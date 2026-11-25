package store

import (
	"context"
	"strings"
	"unicode/utf8"
)

// SessionLedgerState is where a session's ledger snapshot is in its life.
type SessionLedgerState string

const (
	// SessionLedgerPending waits for the device to connect.
	SessionLedgerPending   SessionLedgerState = "pending"
	SessionLedgerPreparing SessionLedgerState = "preparing"
	SessionLedgerReady     SessionLedgerState = "ready"
	// SessionLedgerFailed: the session runs without a ledger.
	SessionLedgerFailed SessionLedgerState = "failed"
)

// Valid reports whether s is a known state.
func (s SessionLedgerState) Valid() bool {
	return s == SessionLedgerPending || s == SessionLedgerPreparing || s == SessionLedgerReady || s == SessionLedgerFailed
}

// Unprepared reports whether a snapshot in state s waits for or runs its
// preparation.
func (s SessionLedgerState) Unprepared() bool {
	return s == SessionLedgerPending || s == SessionLedgerPreparing
}

// SessionLedgerWork is what a snapshot owes after its changeset left the
// open state.
type SessionLedgerWork string

const (
	SessionLedgerNoWork SessionLedgerWork = ""
	// SessionLedgerRebase: the changeset was merged; the snapshot's new
	// decisions are still to be renamed (DeviceCall rebase_ledger).
	SessionLedgerRebase SessionLedgerWork = "rebase"
	// SessionLedgerReset: the changeset was discarded; the snapshot is
	// still to be written again from its view (prepare_ledger with reset).
	SessionLedgerReset SessionLedgerWork = "reset"
)

// Session ledger bounds.
const (
	// MaxSessionLedgerProblems bounds the problems and the warnings a
	// session ledger keeps from its last collection.
	MaxSessionLedgerProblems = 256
	// MaxSessionLedgerKnown bounds the known changes of a snapshot.
	MaxSessionLedgerKnown = 4096
	// MaxSessionLedgerPathBytes bounds a known or problem path.
	MaxSessionLedgerPathBytes = 4096
	// MaxSessionLedgerMessageBytes bounds a problem's message and an
	// error detail.
	MaxSessionLedgerMessageBytes = 1024
	// MaxInputNoteBytes bounds a note input's category.
	MaxInputNoteBytes = 64
	// LedgerRemovalRetention is how long a snapshot removal waits for its
	// device before it is dropped, in milliseconds.
	LedgerRemovalRetention = 30 * 24 * 60 * 60 * 1000
)

// LedgerKnownChange is one change of a snapshot relative to its base, as
// the control plane holds it: the SHA-256 (lowercase hex) of the content,
// or a deletion.
type LedgerKnownChange struct {
	Path    string `json:"path"`
	SHA256  string `json:"sha256,omitempty"`
	Deleted bool   `json:"deleted,omitempty"`
}

// LedgerProblem is one problem or warning of a collection; an empty Path
// concerns the whole snapshot.
type LedgerProblem struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

// SessionLedger is the state of a session's ledger snapshot.
type SessionLedger struct {
	SessionID, UserID, ProjectID, DeviceID string
	State                                  SessionLedgerState
	// Path is the snapshot's absolute path on the device, once ready.
	Path string
	// ErrorCode is a circusv1.LedgerSnapshotError, 0 when none.
	ErrorCode   int32
	ErrorDetail string
	// BaseCommit is the commit the snapshot's manifest holds, empty for
	// an empty ledger; ViewCommit is the base of the session's next
	// changeset. They differ only after a rebase kept changes the agent
	// made after the merge it applied.
	BaseCommit, ViewCommit string
	// CollectedAt is the time of the last successful collection, 0 before.
	CollectedAt int64
	// DueAt is the latest turn end no successful collection has seen; 0
	// when no collection is due.
	DueAt int64
	// Known is the last collection's complete change set relative to
	// BaseCommit, in path order.
	Known []LedgerKnownChange
	// Problems and Warnings of the last collection, bounded; ProblemCount
	// counts all problems; ChangeCount the open changeset's entries.
	Problems, Warnings []LedgerProblem
	ChangeCount        uint32
	ProblemCount       uint32
	// NotedHash identifies the last problem set the agent was told of.
	NotedHash string
	// Work owed after a merge or a discard of PendingChangeset, by a ready
	// snapshot only; the discard's reason in PendingReason.
	Pending          SessionLedgerWork
	PendingChangeset string
	PendingReason    string
	// RebaseTarget is the base commit of the last rebase sent whose
	// outcome is not known yet: the device's manifest may record it.
	RebaseTarget string
	// Attempts counts consecutive failed attempts of the current step;
	// RetryAt is when the next may start (0: at once).
	Attempts, RetryAt    int64
	CreatedAt, UpdatedAt int64
	// ChangesetID is the session's open changeset, read with the row.
	ChangesetID string
}

// HoldsTurns reports whether the snapshot holds the session's turns: while
// it waits for its device or is prepared, and while a merge or a discard of
// its changeset is still to be applied to it, unless problems of the
// snapshot stop that until the agent fixes them.
func (l SessionLedger) HoldsTurns() bool {
	return l.State.Unprepared() || l.State == SessionLedgerReady && l.Pending != SessionLedgerNoWork && l.ProblemCount == 0
}

// CollectionDue reports whether a turn ended after the last successful
// collection.
func (l SessionLedger) CollectionDue() bool { return l.DueAt > 0 }

// LedgerSnapshotRemoval is the snapshot of a deleted session that is
// still to be removed from its device.
type LedgerSnapshotRemoval struct {
	SessionID, UserID, DeviceID string
	CreatedAt                   int64
}

// SessionLedgerFilter selects session ledgers with work: preparation, a
// due collection, a rebase or a reset. DeviceID, when set, selects one
// device's; After continues after a session id.
type SessionLedgerFilter struct {
	DeviceID string
	After    string
	Limit    int // 1..MaxPageSize
}

// SessionLedgerOps are the operations on session ledger snapshots.
type SessionLedgerOps interface {
	// CreateSessionLedger inserts a session's snapshot row. The session
	// must exist (ErrNotFound); ErrExists if it has one.
	CreateSessionLedger(ctx context.Context, l SessionLedger) error
	// GetSessionLedger returns the session's snapshot with its open
	// changeset, or ErrNotFound.
	GetSessionLedger(ctx context.Context, sessionID string) (SessionLedger, error)
	// UpdateSessionLedger replaces the mutable fields of the session's
	// snapshot if its UpdatedAt is still expect, and reports whether it
	// did. l.UpdatedAt must be later than expect. ErrNotFound if absent.
	UpdateSessionLedger(ctx context.Context, l SessionLedger, expect int64) (bool, error)
	// ListSessionLedgers lists the session ledgers with work, by session
	// id.
	ListSessionLedgers(ctx context.Context, f SessionLedgerFilter) ([]SessionLedger, error)
	// ListSessionLedgerStatuses returns the snapshots of those of the
	// sessions that have one, by session id (at most MaxPageSize ids).
	ListSessionLedgerStatuses(ctx context.Context, sessionIDs []string) (map[string]SessionLedger, error)

	// ListLedgerSnapshotRemovals lists pending removals, of one device
	// when deviceID is set, by session id after after.
	ListLedgerSnapshotRemovals(ctx context.Context, deviceID, after string, limit int) ([]LedgerSnapshotRemoval, error)
	// GetLedgerSnapshotRemoval returns the removal of a deleted session's
	// snapshot, or ErrNotFound.
	GetLedgerSnapshotRemoval(ctx context.Context, sessionID string) (LedgerSnapshotRemoval, error)
	// DeleteLedgerSnapshotRemoval forgets a removal; a missing one is no
	// error.
	DeleteLedgerSnapshotRemoval(ctx context.Context, sessionID string) error
	// PruneLedgerSnapshotRemovals drops at most limit removals created
	// before the given time and returns how many.
	PruneLedgerSnapshotRemovals(ctx context.Context, before int64, limit int) (int, error)
	// CountNotesSinceUserInput counts the session's note inputs created
	// after its newest user input (all of them without one).
	CountNotesSinceUserInput(ctx context.Context, sessionID string) (int, error)
	// ListOrphanSessionChangesets lists at most limit open changesets of
	// sessions that no longer exist, by id after after.
	ListOrphanSessionChangesets(ctx context.Context, after string, limit int) ([]LedgerChangeset, error)
	// PruneLedgerCalls deletes at most limit terminal snapshot device
	// calls (session ids "ledger-*", call ids "lg-*") and returns how
	// many.
	PruneLedgerCalls(ctx context.Context, limit int) (int, error)
}

// LedgerSnapshotSessionPrefix starts the session id of a snapshot's
// device calls: "ledger-<session id>".
const LedgerSnapshotSessionPrefix = "ledger-"

// LedgerSnapshotCallPrefix starts the call id of a snapshot's device
// calls.
const LedgerSnapshotCallPrefix = "lg-"

// CheckLedgerProblems checks problems or warnings to store.
func CheckLedgerProblems(what string, problems []LedgerProblem) error {
	if len(problems) > MaxSessionLedgerProblems {
		return invalid("too many %s", what)
	}
	for _, p := range problems {
		if len(p.Path) > MaxSessionLedgerPathBytes || len(p.Message) > MaxSessionLedgerMessageBytes ||
			!utf8.ValidString(p.Path) || !utf8.ValidString(p.Message) || strings.ContainsRune(p.Path+p.Message, 0) {
			return invalid("invalid %s", what)
		}
	}
	return nil
}

// CheckLedgerKnown checks a snapshot's known changes.
func CheckLedgerKnown(known []LedgerKnownChange) error {
	if len(known) > MaxSessionLedgerKnown {
		return invalid("too many known changes")
	}
	for _, k := range known {
		if k.Path == "" || len(k.Path) > MaxSessionLedgerPathBytes || !utf8.ValidString(k.Path) || strings.ContainsRune(k.Path, 0) ||
			k.Deleted != (k.SHA256 == "") || !k.Deleted && !isHexLen(k.SHA256, 64) {
			return invalid("invalid known change")
		}
	}
	return nil
}

func isHexLen(s string, n int) bool {
	return len(s) == n && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f')
	}) == -1
}
