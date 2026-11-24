package store

import (
	"context"
	"errors"
	"iter"
	"math"
	"strings"
)

const (
	MaxLedgerDecisions    = 4096
	MaxLedgerTitleBytes   = 512
	MaxLedgerDirBytes     = 256
	MaxLedgerPathBytes    = MaxLedgerDirBytes + 1 + 1 + 10 + 1 + 60*4 + len(".md")
	MaxLedgerAuthorBytes  = 512
	MaxLedgerSummaryBytes = 4096
	MaxLedgerHistoryPage  = 100
	LedgerProjectPage     = 256
	// The next number must also fit in the protocol's uint32.
	MaxLedgerNumber = math.MaxUint32 - 1
)

var ErrLedgerCursor = errors.New("store: ledger history cursor not found")

// Changeset bounds.
const (
	// MaxOpenLedgerChangesets open changesets per project at a time.
	MaxOpenLedgerChangesets = 64
	// MaxLedgerDiscardReasonBytes bounds a discard reason.
	MaxLedgerDiscardReasonBytes = 1024
	// MaxLedgerChangesetPage bounds one ListLedgerChangesets page.
	MaxLedgerChangesetPage = 100
	// MaxLedgerRepairs bounds one ListLedgerRepairs answer.
	MaxLedgerRepairs = 256
)

// LedgerChangesetState is where a changeset is in its life.
type LedgerChangesetState string

const (
	LedgerChangesetOpen      LedgerChangesetState = "open"
	LedgerChangesetMerged    LedgerChangesetState = "merged"
	LedgerChangesetDiscarded LedgerChangesetState = "discarded"
)

// Valid reports whether s is a known state.
func (s LedgerChangesetState) Valid() bool {
	return s == LedgerChangesetOpen || s == LedgerChangesetMerged || s == LedgerChangesetDiscarded
}

// LedgerChangeset is a changeset's state. Its content lives in the
// ledger's Git repository.
type LedgerChangeset struct {
	ID, ProjectID string
	State         LedgerChangesetState
	// BaseCommit is the main-line commit it started from, empty for an
	// empty ledger.
	BaseCommit     string
	OpenerWorkerID string
	CreatedAt      int64 // Unix milliseconds
	UpdatedAt      int64
	// Merge describes the merge of a merged changeset, or on an open one a
	// merge in progress (see PrepareLedgerMerge).
	Merge         LedgerMerge
	DiscardReason string
	// Archived is false while a discarded changeset's branch still has to
	// be archived in the repository.
	Archived bool
}

// LedgerMerge is a merge commit and what it recorded.
type LedgerMerge struct {
	// Commit is the main-line commit; Parent is the main head it was made
	// on top of (empty for the first commit).
	Commit, Parent string
	CodeCommit     string
	// Numbers maps each temporary number's letters ("a" for D?a) to the
	// number assigned to it.
	Numbers map[string]uint32
}

// Pending reports whether c is open with a merge in progress.
func (c LedgerChangeset) Pending() bool {
	return c.State == LedgerChangesetOpen && c.Merge.Commit != ""
}

// LedgerChangesetCursor is the exclusive position after the last row of
// a page, in the newest-first order.
type LedgerChangesetCursor struct {
	CreatedAt int64
	ID        string
}

// LedgerChangesetFilter selects a page of a project's changesets.
type LedgerChangesetFilter struct {
	ProjectID string
	// State is empty for every state.
	State LedgerChangesetState
	After *LedgerChangesetCursor
	Limit int // 1..MaxLedgerChangesetPage
}

// IsLedgerCommitID reports whether id is a full lowercase SHA-1 object id.
func IsLedgerCommitID(id string) bool {
	return len(id) == 40 && strings.IndexFunc(id, func(r rune) bool {
		return !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f')
	}) == -1
}

type LedgerIndex struct {
	Head       string
	NextNumber uint32
	Entries    []LedgerEntry
}

type LedgerEntry struct {
	Number           uint32
	Dir, Title, Path string
	Commit           string
	CommittedAt      int64 // Unix milliseconds
}

type LedgerVersion struct {
	Number                                        uint32
	Position                                      int64 // Order on the first-parent line.
	Commit                                        string
	CommittedAt                                   int64 // Unix milliseconds
	Author, Summary, Dir, Path, Title, CodeCommit string
	Added, ContentChanged, Moved, Retitled        bool
}

type LedgerOps interface {
	// LedgerProjectName reads the project's name, without loading its
	// locations, and reports whether the project exists.
	LedgerProjectName(ctx context.Context, projectID string) (string, bool, error)
	// ListLedgerProjects lists at most LedgerProjectPage projects with a nonempty indexed head
	// in ascending order, strictly after after (empty starts the scan).
	ListLedgerProjects(ctx context.Context, after string) ([]string, error)
	// GetLedgerHead returns an empty head for a project with no index.
	GetLedgerHead(ctx context.Context, projectID string) (string, error)
	// GetLedgerIndex reads the head, counter and directory/name-ordered
	// entries from one snapshot. Missing index rows give an empty ledger.
	GetLedgerIndex(ctx context.Context, projectID string) (LedgerIndex, error)
	// ReplaceLedgerIndex installs a prepared snapshot and version stream
	// if the indexed head still matches expected; a head mismatch returns
	// ErrBusy. An empty head clears entries and versions and preserves the
	// counter.
	//
	// When retainPrefix is true, expected is a verified first-parent
	// ancestor of head, entries contain changes since expected, and
	// versions contain only the prepared suffix, at most
	// MaxLedgerDecisions rows. It runs in one transaction.
	//
	// Otherwise it replaces all entries and versions without holding the
	// store for a time proportional to the history: outside a transaction,
	// it stages the versions in transactions of a bounded number of rows,
	// and readers keep the previous complete index until a final
	// transaction checks expected again and switches them to the new one.
	// Rows left by an install that failed or crashed are invisible and are
	// deleted by the next replacement. Replacements of one project must not
	// run concurrently; a newer one makes an older one fail with ErrBusy.
	//
	// The stream may read prepared data
	// but must not perform repository I/O or call the store.
	ReplaceLedgerIndex(ctx context.Context, projectID, expected, head string, retainPrefix bool, entries []LedgerEntry, versions iter.Seq2[LedgerVersion, error]) error
	// AdvanceLedgerIndex moves the indexed head if it matches parent,
	// upserts the changed entries and appends the new versions. It sets
	// their positions above the preceding versions. A head mismatch
	// returns ErrBusy.
	AdvanceLedgerIndex(ctx context.Context, projectID, parent, head string, entries []LedgerEntry, versions []LedgerVersion) error
	// ListLedgerHistory reads at most limit versions in reverse main-line
	// order, after the named commit (empty starts at the head). An unknown
	// cursor for this project and number returns ErrLedgerCursor. A number
	// with no history and no cursor gives an empty page.
	ListLedgerHistory(ctx context.Context, projectID string, number uint32, after string, limit int) ([]LedgerVersion, bool, error)
	// GetLedgerNextNumber returns the number the next new decision gets
	// according to the index; 0 for a project with no index.
	GetLedgerNextNumber(ctx context.Context, projectID string) (uint32, error)

	// CreateLedgerChangeset inserts an open changeset with no merge.
	// ErrNotFound if the project does not exist; ErrExists if the id is
	// taken; ErrLimit if the project already has MaxOpenLedgerChangesets
	// open changesets, checked atomically with the insert.
	CreateLedgerChangeset(ctx context.Context, c LedgerChangeset) error
	// GetLedgerChangeset returns the changeset, or ErrNotFound.
	GetLedgerChangeset(ctx context.Context, id string) (LedgerChangeset, error)
	// ListLedgerChangesets returns one page of a project's changesets,
	// newest first (by creation time, then id), and whether more follow.
	ListLedgerChangesets(ctx context.Context, f LedgerChangesetFilter) ([]LedgerChangeset, bool, error)
	// ListLedgerRepairs returns at most MaxLedgerRepairs changesets of the
	// project that have a merge in progress or a branch still to archive.
	ListLedgerRepairs(ctx context.Context, projectID string) ([]LedgerChangeset, error)
	// TouchLedgerChangeset sets UpdatedAt of an open changeset and reports
	// whether it was open. ErrNotFound if absent.
	TouchLedgerChangeset(ctx context.Context, id string, at int64) (bool, error)
	// PrepareLedgerMerge records a merge in progress on an open changeset
	// without one, before the merge commit is published, and reports
	// whether it did. ErrNotFound if absent.
	PrepareLedgerMerge(ctx context.Context, id string, m LedgerMerge, at int64) (bool, error)
	// ClearLedgerMerge forgets the merge in progress with this commit, one
	// that never reached the main line, and reports whether it did.
	ClearLedgerMerge(ctx context.Context, id, commit string) (bool, error)
	// FinishLedgerMerge marks the changeset merged by the merge in progress
	// with this commit and reports whether it did.
	FinishLedgerMerge(ctx context.Context, id, commit string, at int64) (bool, error)
	// DiscardLedgerChangeset marks an open changeset without a merge in
	// progress discarded with reason, archived or with its branch still to
	// archive, and reports whether it did.
	DiscardLedgerChangeset(ctx context.Context, id, reason string, archived bool, at int64) (bool, error)
	// ArchiveLedgerChangeset records that a discarded changeset's branch
	// was archived. ErrNotFound if absent.
	ArchiveLedgerChangeset(ctx context.Context, id string) error
	// PurgeOrphanLedgerChangesets deletes the changesets of deleted
	// projects, which a deletion interrupted by a crash leaves behind, in
	// transactions of a bounded number of rows.
	PurgeOrphanLedgerChangesets(ctx context.Context) error
}

// ValidTemporaryLetters reports whether s is the letters of a temporary
// decision number: one to four lowercase ASCII letters.
func ValidTemporaryLetters(s string) bool {
	if len(s) == 0 || len(s) > 4 {
		return false
	}
	for i := range len(s) {
		if s[i] < 'a' || s[i] > 'z' {
			return false
		}
	}
	return true
}
