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
}
