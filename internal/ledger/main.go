package ledger

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/EricWvi/circus/internal/store"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"golang.org/x/sys/unix"
)

const maxMessageBytes = 60 << 10

// CommitOptions supplies the metadata for a new main-line commit. CodeCommit
// is optional and, when present, must be a lowercase 40-hex commit ID.
type CommitOptions struct {
	Author     string
	Message    string
	CodeCommit string
}

// CommitMain commits the complete set of decisions on top of main. If it
// returns a nonempty ID with an error, Git advanced but durability or the
// index update failed. Reconcile or the next CommitMain can rebuild the index.
func (m *Manager) CommitMain(ctx context.Context, projectID string, decisions []Decision, options CommitOptions) (string, error) {
	message, err := commitMessage(options)
	if err != nil {
		return "", err
	}
	release, err := m.acquire(ctx, projectID)
	if err != nil {
		return "", err
	}
	defer release()

	unlockRoot, err := m.lockRoot(unix.LOCK_SH)
	if err != nil {
		return "", err
	}
	defer unlockRoot()

	_, exists, err := m.st.LedgerProjectName(ctx, projectID)
	if err != nil {
		return "", err
	}
	if !exists {
		return "", store.ErrNotFound
	}
	prepared, candidate, err := prepareSnapshot(ctx, decisions)
	if err != nil {
		return "", err
	}
	dst := m.repoPath(projectID)
	r, err := m.open(dst)
	if err != nil {
		return "", err
	}
	var staging string
	if r == nil {
		staging, err = os.MkdirTemp(m.root, initPrefix)
		if err != nil {
			return "", err
		}
		defer os.RemoveAll(staging)
		r, err = initRepository(staging)
		if err != nil {
			return "", err
		}
		r.faults = &m.faults
	}
	defer r.storage.Close()
	unlockMain, err := r.lockMain()
	if err != nil {
		return "", err
	}
	defer unlockMain()

	parent, err := r.head()
	if err != nil {
		return "", err
	}
	indexed, err := m.st.GetLedgerHead(ctx, projectID)
	if err != nil {
		return "", err
	}
	var previous snapshot
	if !parent.IsZero() {
		commit, err := r.commit(parent)
		if err != nil {
			return "", err
		}
		previous, err = r.readSnapshot(ctx, commit.TreeHash, candidate, true)
		if err != nil {
			return "", err
		}
	}
	if parent.IsZero() && len(prepared) == 0 {
		return "", &ValidationError{Problems: []Problem{{Path: ".", Message: "the first commit must add a decision"}}}
	}
	return m.publish(ctx, projectID, publication{r: r, staging: staging, dst: dst, parent: parent, indexed: indexed,
		previous: previous, prepared: prepared, message: message, options: options})
}

// publication is a new main-line commit prepared under the project's lock
// and main.lock: the complete set of decisions on top of parent, whose
// snapshot is previous.
type publication struct {
	r *repository
	// staging is the directory of a repository created for this commit,
	// renamed to dst once main is published; empty for an existing one.
	staging, dst string
	parent       plumbing.Hash
	indexed      string
	previous     snapshot
	prepared     []preparedDecision
	message      string
	options      CommitOptions
	// beforePublish, when set, runs after the commit object is written
	// and before main moves to it. An error stops the commit.
	beforePublish func(commit plumbing.Hash) error
}

// publish writes the snapshot and the commit, moves main and updates the
// index. If it returns a nonempty ID with an error, Git advanced but
// durability or the index update failed.
func (m *Manager) publish(ctx context.Context, projectID string, p publication) (string, error) {
	r := p.r
	if err := checkRetained(p.previous, p.prepared); err != nil {
		return "", err
	}
	tree, current, err := r.putSnapshot(ctx, p.prepared, p.previous)
	if err != nil {
		return "", err
	}
	author := object.Signature{Name: p.options.Author, Email: "ledger@circus.invalid", When: m.now().UTC().Truncate(time.Second)}
	commit := &object.Commit{Author: author, Committer: author, Message: p.message, TreeHash: tree}
	if !p.parent.IsZero() {
		commit.ParentHashes = []plumbing.Hash{p.parent}
	}
	o := r.storage.NewEncodedObject()
	if err := commit.Encode(o); err != nil {
		return "", err
	}
	commit.Hash, err = r.putObject(o)
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	staging, dst := p.staging, p.dst
	if staging != "" {
		for _, file := range []string{"HEAD", "config"} {
			if err := syncPath(filepath.Join(staging, file)); err != nil {
				return "", err
			}
		}
	}
	if p.beforePublish != nil {
		if err := p.beforePublish(commit.Hash); err != nil {
			return "", err
		}
	}
	published, err := r.setMain(p.parent, commit.Hash)
	if err != nil {
		if published && staging == "" {
			return commit.Hash.String(), fmt.Errorf("ledger: main published but durability check failed: %w", err)
		}
		return "", err
	}
	if staging != "" {
		if err := os.Rename(staging, dst); err != nil {
			if errors.Is(err, unix.ENOTEMPTY) || errors.Is(err, os.ErrExist) {
				return "", fmt.Errorf("%w: ledger repository was created concurrently", store.ErrBusy)
			}
			return "", err
		}
		r.dir = dst
		for _, dir := range []string{dst, m.root, filepath.Dir(m.root)} {
			if err := syncPath(dir); err != nil {
				return commit.Hash.String(), fmt.Errorf("ledger: repository published but durability check failed: %w", err)
			}
		}
	} else if err := syncPath(m.root); err != nil {
		return commit.Hash.String(), fmt.Errorf("ledger: main published but durability check failed: %w", err)
	}

	if p.indexed != commitID(p.parent) {
		if staging != "" {
			// The Git storage still refers to the staging path after rename.
			r, err = m.open(dst)
			if err != nil {
				return commit.Hash.String(), err
			}
			if r == nil {
				return commit.Hash.String(), fmt.Errorf("ledger: published repository disappeared")
			}
			defer r.storage.Close()
		}
		err = m.rebuild(ctx, r, projectID, p.indexed, commit.Hash)
	} else {
		err = m.advance(ctx, projectID, p.indexed, p.previous, current, commit, p.options.CodeCommit)
	}
	if err != nil {
		return commit.Hash.String(), fmt.Errorf("ledger: main advanced but index update failed: %w", err)
	}
	m.setProjectError(projectID, nil)
	return commit.Hash.String(), nil
}

func checkRetained(previous snapshot, entries []preparedDecision) error {
	numbers := make(map[uint32]bool, len(entries))
	for _, e := range entries {
		numbers[e.Number] = true
	}
	var v validation
	for _, old := range previous.entries {
		if !numbers[old.Number] {
			v.add(old.Path, "existing decisions cannot be removed")
		}
	}
	if len(v.problems) > 0 {
		return &ValidationError{Problems: v.problems}
	}
	return nil
}

func commitMessage(o CommitOptions) (string, error) {
	if len(o.Author) == 0 || len(o.Author) > store.MaxLedgerAuthorBytes || strings.TrimSpace(o.Author) != o.Author ||
		!utf8.ValidString(o.Author) || strings.ContainsAny(o.Author, "<>") || strings.ContainsFunc(o.Author, unicode.IsControl) {
		return "", fmt.Errorf("%w: invalid ledger author", store.ErrInvalid)
	}
	if len(o.Message) > maxMessageBytes || len(summary(o.Message)) > store.MaxLedgerSummaryBytes || !utf8.ValidString(o.Message) ||
		strings.ContainsRune(o.Message, 0) || strings.TrimSpace(summary(o.Message)) == "" {
		return "", fmt.Errorf("%w: invalid ledger commit message", store.ErrInvalid)
	}
	if o.CodeCommit != "" && !store.IsLedgerCommitID(o.CodeCommit) {
		return "", fmt.Errorf("%w: invalid ledger code commit", store.ErrInvalid)
	}
	if codeTrailer(o.Message) != "" {
		return "", fmt.Errorf("%w: supply the Code-Commit trailer through CommitOptions", store.ErrInvalid)
	}
	msg := strings.TrimRight(o.Message, "\r\n")
	if o.CodeCommit != "" {
		msg += "\n\nCode-Commit: " + o.CodeCommit
	}
	return msg + "\n", nil
}

func summary(message string) string {
	line, _, _ := strings.Cut(message, "\n")
	return strings.TrimSuffix(line, "\r")
}

func codeTrailer(message string) string {
	// The final paragraph is the trailer block. CRLF is permitted in the
	// supplied message, and older commits can have additional trailers.
	message = strings.ReplaceAll(message, "\r\n", "\n")
	message = strings.TrimRight(message, "\n")
	i := strings.LastIndex(message, "\n\n")
	if i < 0 {
		return ""
	}
	var code string
	for line := range strings.SplitSeq(message[i+2:], "\n") {
		key, value, ok := strings.Cut(line, ": ")
		if !ok || strings.ContainsAny(key, " \t") {
			return ""
		}
		if key == "Code-Commit" && store.IsLedgerCommitID(value) {
			code = value
		}
	}
	return code
}

func version(commit *object.Commit, code string, current, previous snapshotEntry, existed bool) store.LedgerVersion {
	v := store.LedgerVersion{
		Number: current.Number, Commit: commit.Hash.String(), CommittedAt: commit.Committer.When.UnixMilli(),
		Author: commit.Author.Name, Summary: summary(commit.Message), Dir: current.Dir, Path: current.Path, Title: current.Title,
		CodeCommit: code, Added: !existed,
	}
	if existed {
		v.ContentChanged = current.bodyHash != previous.bodyHash
		v.Moved = current.Dir != previous.Dir
		v.Retitled = current.Title != previous.Title
	}
	return v
}

func changed(v store.LedgerVersion) bool {
	return v.Added || v.ContentChanged || v.Moved || v.Retitled
}

func (m *Manager) advance(ctx context.Context, projectID, indexed string, previous, current snapshot, commit *object.Commit, code string) error {
	var entries []store.LedgerEntry
	var versions []store.LedgerVersion
	for number, entry := range current.entries {
		old, ok := previous.entries[number]
		if ok && entry.Path == old.Path && entry.blob == old.blob {
			continue
		}
		v := version(commit, code, entry, old, ok)
		if changed(v) {
			e := entry.LedgerEntry
			e.Commit, e.CommittedAt = v.Commit, v.CommittedAt
			entries = append(entries, e)
			versions = append(versions, v)
		}
	}
	return m.st.AdvanceLedgerIndex(ctx, projectID, indexed, commit.Hash.String(), entries, versions)
}
