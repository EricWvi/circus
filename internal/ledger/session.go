package ledger

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	snapshotfiles "github.com/EricWvi/circus/internal/ledger/snapshot"
	"github.com/EricWvi/circus/internal/store"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// Sessions' changesets. A session's ledger snapshot is a copy of the
// ledger on its device. Its manifest holds a base commit (the snapshot
// base); the session's next changeset starts from the session's view (a
// main-line commit at first, later the view after its last merge). The
// two differ only after a rebase kept changes the agent made after the
// merge it applied: those changes are then part of the snapshot's base but
// not of the view, so a changeset's content is the snapshot base plus the
// collected changes, compared with the view.

const sessionRefPrefix = "refs/sessions/"

func sessionRef(sessionID string) plumbing.ReferenceName {
	return plumbing.ReferenceName(sessionRefPrefix + sessionID)
}

// SessionAuthor is the author name of a session's commits.
func SessionAuthor(sessionID string) string { return "session " + sessionID }

// ErrKnownChanged refuses a session check whose known changes do not
// match what the changeset holds; the caller collects again without known
// changes.
var ErrKnownChanged = errors.New("ledger: known changes differ from the session's changeset")

// SnapshotChange is one change of a snapshot relative to its base, as a
// collection reports it: a new or changed file's content, a removed base
// file, or (Known) content the caller left out because the session's
// changeset already holds it, identified by its SHA-256.
type SnapshotChange struct {
	Path    string
	Content []byte
	Deleted bool
	Known   bool
	SHA256  string
}

// SessionCheck is a check of a session's changeset with a collection.
type SessionCheck struct {
	ProjectID, SessionID string
	// NewChangesetID is the id a changeset opened by this check gets.
	NewChangesetID string
	// ViewBase is the base of a changeset this check opens.
	ViewBase string
	// SnapshotBase is the commit the changes are relative to.
	SnapshotBase string
	Changes      []SnapshotChange
}

// SessionEntry is one decision (or other file) a session's changeset
// changes, relative to its base.
type SessionEntry struct {
	// Path in the changeset; for a deleted file, its path in the base.
	Path string
	// BasePath is the path in the base when it differs.
	BasePath string
	// Number is "D12", or a temporary "D?a"; empty for a file that is not
	// a decision.
	Number          string
	Title           string
	BaseTitle       string
	Added           bool
	ContentChanged  bool
	Moved, Retitled bool
	Deleted         bool
}

// SessionCheckResult is a session check's outcome. Changeset is zero when
// the session has no open changeset (the collection found no changes and
// none was open).
type SessionCheckResult struct {
	CheckResult
	Opened  bool
	Entries []SessionEntry
	// Known is the collection's complete change set relative to the
	// snapshot base, as hashes, in path order.
	Known []store.LedgerKnownChange
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// snapshotFiles applies changes to the files of a snapshot base: it
// returns the complete files with the content of each changed one. Known
// changes take their content from source (the files of the changeset's
// last check), whose content must have the given hash.
func (r *repository) snapshotFiles(base map[string]plumbing.Hash, source map[string]plumbing.Hash, changes []SnapshotChange) (map[string]plumbing.Hash, map[string][]byte, error) {
	files := maps.Clone(base)
	contents := make(map[string][]byte, len(changes))
	seen := make(map[string]bool, len(changes))
	for i, ch := range changes {
		if seen[ch.Path] {
			return nil, nil, &RequestError{Field: fmt.Sprintf("changes[%d].path", i), Message: "path occurs twice"}
		}
		seen[ch.Path] = true
		switch {
		case ch.Deleted:
			delete(files, ch.Path)
		case ch.Known:
			hash, ok := source[ch.Path]
			if !ok || r == nil {
				return nil, nil, ErrKnownChanged
			}
			data, err := r.blob(hash, MaxChangesetBytes, nil)
			if err != nil {
				return nil, nil, err
			}
			if sha256Hex(data) != ch.SHA256 {
				return nil, nil, ErrKnownChanged
			}
			files[ch.Path], contents[ch.Path] = hash, data
		default:
			files[ch.Path] = plumbing.ComputeHash(plumbing.BlobObject, ch.Content)
			contents[ch.Path] = ch.Content
		}
	}
	return files, contents, nil
}

// relative lists the changes that take base's files to files, reading
// from the repository what contents does not hold.
func (r *repository) relative(base, files map[string]plumbing.Hash, contents map[string][]byte) ([]Change, error) {
	var out []Change
	total := 0
	for _, p := range diff(base, files) {
		hash, ok := files[p]
		if !ok {
			out = append(out, Change{Path: p, Deleted: true})
			continue
		}
		data, ok := contents[p]
		if !ok {
			var err error
			if data, err = r.blob(hash, MaxChangesetBytes, nil); err != nil {
				return nil, fmt.Errorf("ledger: read %q: %w", p, err)
			}
		}
		if total += len(data); total > MaxChangesetBytes {
			return nil, &LimitError{Message: fmt.Sprintf("a changeset holds at most %d bytes of changed content", MaxChangesetBytes)}
		}
		out = append(out, Change{Path: p, Content: data})
	}
	return out, nil
}

// knownOf lists the changes from base to files as hashes.
func (r *repository) knownOf(base, files map[string]plumbing.Hash, contents map[string][]byte) ([]store.LedgerKnownChange, error) {
	var out []store.LedgerKnownChange
	for _, p := range diff(base, files) {
		hash, ok := files[p]
		if !ok {
			out = append(out, store.LedgerKnownChange{Path: p, Deleted: true})
			continue
		}
		data, ok := contents[p]
		if !ok {
			var err error
			if data, err = r.blob(hash, MaxChangesetBytes, nil); err != nil {
				return nil, fmt.Errorf("ledger: read %q: %w", p, err)
			}
		}
		out = append(out, store.LedgerKnownChange{Path: p, SHA256: sha256Hex(data)})
	}
	return out, nil
}

func checkCommitID(field, id string) error {
	if id != "" && !store.IsLedgerCommitID(id) {
		return &RequestError{Field: field, Message: "commit must be 40 lowercase hexadecimal characters"}
	}
	return nil
}

// CheckSessionChangeset checks the session's open changeset with a
// collection of its snapshot. With no open changeset it opens one, based
// on ViewBase and with no opener, when the collection differs from the
// view; otherwise it changes nothing and returns no changeset. A known
// change whose content the changeset does not hold is ErrKnownChanged.
func (m *Manager) CheckSessionChangeset(ctx context.Context, sc SessionCheck) (SessionCheckResult, error) {
	if err := errors.Join(checkCommitID("view_base", sc.ViewBase), checkCommitID("snapshot_base", sc.SnapshotBase)); err != nil {
		return SessionCheckResult{}, err
	}
	if err := store.CheckID("session id", sc.SessionID); err != nil {
		return SessionCheckResult{}, err
	}
	if len(sc.Changes) > MaxChanges {
		return SessionCheckResult{}, &LimitError{Message: fmt.Sprintf("a snapshot holds at most %d changes", MaxChanges)}
	}
	l, err := m.lockProject(ctx, sc.ProjectID)
	if err != nil {
		return SessionCheckResult{}, err
	}
	defer l.close()
	c, err := m.st.GetOpenSessionChangeset(ctx, sc.SessionID)
	open := err == nil
	switch {
	case errors.Is(err, store.ErrNotFound):
		c = store.LedgerChangeset{BaseCommit: sc.ViewBase}
	case err != nil:
		return SessionCheckResult{}, err
	case c.ProjectID != sc.ProjectID:
		return SessionCheckResult{}, fmt.Errorf("%w: the session's changeset is of another project", store.ErrInvalid)
	}
	if open {
		if err := l.usable(c.ID); err != nil {
			return SessionCheckResult{}, err
		}
	}
	r := l.r
	if r == nil && (c.BaseCommit != "" || sc.SnapshotBase != "") {
		return SessionCheckResult{}, fmt.Errorf("ledger: repository of project %q is missing", sc.ProjectID)
	}
	var snapBase, viewFiles, source map[string]plumbing.Hash
	if r != nil {
		if snapBase, err = r.commitFiles(ctx, plumbing.NewHash(sc.SnapshotBase), true); err != nil {
			return SessionCheckResult{}, err
		}
		viewFiles = snapBase
		if c.BaseCommit != sc.SnapshotBase {
			if viewFiles, err = r.commitFiles(ctx, plumbing.NewHash(c.BaseCommit), false); err != nil {
				return SessionCheckResult{}, err
			}
		}
		source = viewFiles
		if open {
			branch, err := r.readRef(branchRef(c.ID))
			if err != nil {
				return SessionCheckResult{}, err
			}
			if !branch.IsZero() {
				if source, err = r.commitFiles(ctx, branch, true); err != nil {
					return SessionCheckResult{}, err
				}
			}
		}
	} else {
		// The empty ledger, without a repository: its index alone.
		empty := map[string]plumbing.Hash{indexName: emptyIndexHash}
		snapBase, viewFiles, source = empty, empty, empty
	}
	files, contents, err := r.snapshotFiles(snapBase, source, sc.Changes)
	if err != nil {
		return SessionCheckResult{}, err
	}
	known, err := r.knownOf(snapBase, files, contents)
	if err != nil {
		return SessionCheckResult{}, err
	}
	changes, err := r.relative(viewFiles, files, contents)
	if err != nil {
		return SessionCheckResult{}, err
	}
	if err := CheckChanges("changes", changes); err != nil {
		return SessionCheckResult{}, err
	}
	out := SessionCheckResult{Known: known}
	if !open {
		if len(changes) == 0 {
			return out, nil
		}
		if !ValidChangesetID(sc.NewChangesetID) {
			return SessionCheckResult{}, fmt.Errorf("%w: invalid changeset id", store.ErrInvalid)
		}
		now := m.now().UnixMilli()
		c = store.LedgerChangeset{ID: sc.NewChangesetID, ProjectID: sc.ProjectID, State: store.LedgerChangesetOpen,
			BaseCommit: sc.ViewBase, CreatedAt: now, UpdatedAt: now, Archived: true, SessionID: sc.SessionID}
		if err := m.st.CreateLedgerChangeset(ctx, c); err != nil {
			return SessionCheckResult{}, err
		}
		out.Opened = true
	}
	res, effective, baseRef, err := m.check(ctx, l, c, SessionAuthor(sc.SessionID), changes)
	if err != nil {
		return SessionCheckResult{}, err
	}
	assigned, err := m.mergedLetters(ctx, sc.ProjectID, sc.SessionID)
	if err != nil {
		return SessionCheckResult{}, err
	}
	res.Problems, res.Warnings = staleReferences(effective, assigned, res.Problems, res.Warnings)
	out.CheckResult = res
	out.Entries = sessionEntries(baseRef, parseChanges(effective))
	return out, nil
}

// parsedChange is what an entry needs of an effective change: its path,
// and for a file its front matter and body hash.
type parsedChange struct {
	path     string
	deleted  bool
	ok       bool
	number   string
	perm     bool
	value    uint32
	title    string
	bodyHash [sha256.Size]byte
}

func parseChange(ch change) parsedChange {
	p := parsedChange{path: ch.path, deleted: ch.deleted}
	if ch.deleted || !decisionFile(ch.path) {
		return p
	}
	d, letters, err := parse(ch.content, true)
	if err != nil {
		return p
	}
	p.ok, p.title, p.bodyHash = true, d.Title, sha256.Sum256(d.Body)
	if letters != "" {
		p.number = temporaryPrefix + letters
	} else {
		p.number, p.perm, p.value = numberName(d.Number), true, d.Number
	}
	return p
}

func parseChanges(effective []change) []parsedChange {
	out := make([]parsedChange, 0, len(effective))
	for _, ch := range effective {
		out = append(out, parseChange(ch))
	}
	return out
}

// sessionEntries describes effective changes relative to the base as one
// entry per decision: a removed decision added again at another path with
// the same number is one moved (or retitled) entry.
func sessionEntries(baseRef reference, changes []parsedChange) []SessionEntry {
	removed := make(map[uint32]string)
	for _, ch := range changes {
		if e, ok := baseRef.byPath[ch.path]; ok && ch.deleted {
			removed[e.Number] = ch.path
		}
	}
	moved := make(map[string]bool)
	var out []SessionEntry
	for _, p := range changes {
		if p.deleted {
			continue
		}
		entry := SessionEntry{Path: p.path, Number: p.number, Title: p.title}
		if old, ok := baseRef.byPath[p.path]; ok {
			if !p.ok {
				// A decision file that no longer parses.
				entry.Number = numberName(old.Number)
			}
			entry.ContentChanged = !p.ok || p.bodyHash != old.bodyHash
			if p.ok && p.title != old.Title {
				entry.Retitled, entry.BaseTitle = true, old.Title
			}
			if !entry.ContentChanged && !entry.Retitled {
				// Same number, title and body in another spelling.
				entry.ContentChanged = true
			}
		} else if from, ok := removed[p.value]; p.perm && ok && !moved[from] {
			old := baseRef.byPath[from]
			moved[from] = true
			entry.BasePath = from
			entry.Moved = old.Dir != fileDir(p.path)
			entry.ContentChanged = p.bodyHash != old.bodyHash
			if p.title != old.Title {
				entry.Retitled, entry.BaseTitle = true, old.Title
			}
		} else if _, inBase := baseRef.files[p.path]; inBase {
			// The generated index, or another file that is not a decision.
			entry.ContentChanged = true
		} else {
			entry.Added = true
		}
		out = append(out, entry)
	}
	for _, ch := range changes {
		if !ch.deleted || moved[ch.path] {
			continue
		}
		entry := SessionEntry{Path: ch.path, Deleted: true}
		if old, ok := baseRef.byPath[ch.path]; ok {
			entry.Number, entry.Title = numberName(old.Number), old.Title
		}
		out = append(out, entry)
	}
	slices.SortFunc(out, func(a, b SessionEntry) int { return strings.Compare(a.Path, b.Path) })
	if len(out) > store.MaxLedgerDecisions {
		out = out[:store.MaxLedgerDecisions]
	}
	return out
}

// SessionChangeset is a session's changeset as GetSessionLedger shows it.
type SessionChangeset struct {
	Changeset store.LedgerChangeset
	Entries   []SessionEntry
	// FastForward reports that the base's tree is the main line head's.
	FastForward bool
}

// readSession reads a changeset's base reference and the changes of its
// last check, parsed one file at a time, with the project's queue held.
// files maps the branch's files to their blobs.
func (m *Manager) readSession(ctx context.Context, id string, visit func(r *repository, c store.LedgerChangeset, baseRef reference,
	changes []parsedChange, files map[string]plumbing.Hash, head plumbing.Hash) error) error {
	c, err := m.st.GetLedgerChangeset(ctx, id)
	if err != nil {
		return err
	}
	if err := m.CheckProject(ctx, c.ProjectID); err != nil {
		return err
	}
	release, err := m.acquire(ctx, c.ProjectID)
	if err != nil {
		return err
	}
	defer release()
	r, err := m.open(m.repoPath(c.ProjectID))
	if err != nil {
		return err
	}
	if r == nil {
		return visit(nil, c, newReference(emptySnapshot()), nil, nil, plumbing.ZeroHash)
	}
	defer r.storage.Close()
	baseRef, err := r.readReference(ctx, plumbing.NewHash(c.BaseCommit), snapshot{}, false)
	if err != nil {
		return err
	}
	head, err := r.head()
	if err != nil {
		return err
	}
	branch, err := r.readRef(branchRef(id))
	if err != nil || branch.IsZero() {
		if err == nil {
			err = visit(r, c, baseRef, nil, nil, head)
		}
		return err
	}
	files, err := r.commitFiles(ctx, branch, true)
	if err != nil {
		return err
	}
	var changes []parsedChange
	var buffer []byte
	for _, p := range diff(baseRef.files, files) {
		hash, ok := files[p]
		if !ok {
			changes = append(changes, parsedChange{path: p, deleted: true})
			continue
		}
		data, err := r.blob(hash, MaxChangesetBytes, buffer)
		if err != nil {
			return fmt.Errorf("ledger: read %q of changeset %s: %w", p, id, err)
		}
		buffer = data
		changes = append(changes, parseChange(change{path: p, content: data}))
	}
	return visit(r, c, baseRef, changes, files, head)
}

// ReadSessionChangeset returns a changeset's entries, from its last
// check, and whether it fast-forwards now.
func (m *Manager) ReadSessionChangeset(ctx context.Context, id string) (SessionChangeset, error) {
	var out SessionChangeset
	err := m.readSession(ctx, id, func(r *repository, c store.LedgerChangeset, baseRef reference, changes []parsedChange,
		_ map[string]plumbing.Hash, head plumbing.Hash) error {
		out.Changeset = c
		out.Entries = sessionEntries(baseRef, changes)
		var err error
		out.FastForward, err = r.sameTree(plumbing.NewHash(c.BaseCommit), head)
		return err
	})
	return out, err
}

// SessionDiff is one entry's file in a changeset's base and in the
// changeset.
type SessionDiff struct {
	Base, Changeset Version
}

// ReadSessionDiff returns the file at path (an entry's path) of the
// changeset's last check next to its version in the base (at the entry's
// base path when moved). ErrNotFound when the changeset does not change
// that path.
func (m *Manager) ReadSessionDiff(ctx context.Context, id, p string) (SessionDiff, error) {
	var out SessionDiff
	err := m.readSession(ctx, id, func(r *repository, c store.LedgerChangeset, baseRef reference, changes []parsedChange,
		files map[string]plumbing.Hash, _ plumbing.Hash) error {
		var entry *SessionEntry
		entries := sessionEntries(baseRef, changes)
		for i := range entries {
			if entries[i].Path == p {
				entry = &entries[i]
			}
		}
		if entry == nil {
			return store.ErrNotFound
		}
		basePath := cmp.Or(entry.BasePath, entry.Path)
		if hash, ok := baseRef.files[basePath]; ok {
			data, err := r.mainFile(basePath, hash, true)
			if err != nil {
				return err
			}
			out.Base = Version{Exists: true, Content: data}
		}
		if hash, ok := files[p]; ok && !entry.Deleted {
			data, err := r.blob(hash, MaxChangesetBytes, nil)
			if err != nil {
				return fmt.Errorf("ledger: read %q of changeset %s: %w", p, id, err)
			}
			out.Changeset = Version{Exists: true, Content: data}
		}
		return nil
	})
	return out, err
}

// SnapshotEdit is one change rebase_ledger applies to a snapshot:
// expected is the SHA-256 the file must have, empty when it must not
// exist.
type SnapshotEdit struct {
	Path           string
	ExpectedSHA256 string
	Content        []byte
	Deleted        bool
}

// TargetSHA256 is the SHA-256 of the edit's target, empty for a deletion.
func (e SnapshotEdit) TargetSHA256() string {
	if e.Deleted {
		return ""
	}
	return sha256Hex(e.Content)
}

// RebasePlan is the rebase of a session's snapshot after the merge of its
// changeset: what the snapshot holds then, and the edits that take it
// there from what it holds now.
type RebasePlan struct {
	// View is the session's view after the merge, the base of its next
	// changeset: the changeset's base with its merged files.
	View string
	// Target is the commit whose files the rebase installs: the view,
	// with the agent's changes since the collection the merge used,
	// renumbered as the merge would have; the view itself without such
	// changes. Its parent is the view.
	Target string
	// Known is the collection relative to the snapshot base.
	Known []store.LedgerKnownChange
	// Edits take the snapshot from what it holds now to Target, each
	// expecting the file's current content, in path order.
	Edits []SnapshotEdit
	// Renamed maps the path of each new decision in the snapshot to its
	// path after the rebase.
	Renamed map[string]string
	// Numbers maps the letters of each temporary number to the number the
	// merge assigned.
	Numbers map[string]uint32
	// Restored lists the merged new decisions the agent removed after the
	// collection the merge used, put back at their merged paths: the
	// ledger keeps every decision.
	Restored []string
	// Problems are what the agent should check: a new decision that
	// reused the temporary number of a merged one, given fresh letters.
	Problems []Problem
}

// RebaseInput is a collection of a session's snapshot made for the rebase
// after the merge of its changeset ChangesetID: Changes relative to
// SnapshotBase, whose known changes take their content from the
// changeset's files (the collection the merge used).
type RebaseInput struct {
	ChangesetID, SnapshotBase string
	Changes                   []SnapshotChange
}

// renamedPath is the path a new decision's file gets when its temporary
// number is assigned; p itself for any other file.
func renamedPath(p string, numbers map[string]uint32) string {
	dir, name := fileDir(p), path.Base(p)
	rest, ok := strings.CutPrefix(name, temporaryPrefix)
	if !ok {
		return p
	}
	letters, tail, ok := strings.Cut(rest, "-")
	n, assigned := numbers[letters]
	if !ok || !assigned || !strings.HasSuffix(tail, ".md") {
		return p
	}
	return joinPath(dir, "D"+strconv.FormatUint(uint64(n), 10)+"-"+tail)
}

// renumber is what a merge with numbers makes of a new decision's file at
// p: its temporary number and references replaced, its front matter
// written again and its name derived again, at its directory. Any other
// file keeps its path, with its references replaced.
func renumber(p string, data []byte, numbers map[string]uint32) (string, []byte, bool) {
	rewritten := rewriteTemporary(data, numbers)
	if !decisionFile(p) {
		return p, rewritten, false
	}
	if _, letters, err := parse(data, true); err != nil || letters == "" {
		return p, rewritten, false
	} else if _, assigned := numbers[letters]; !assigned {
		return p, rewritten, false
	}
	d, err := Parse(rewritten)
	if err != nil {
		return p, rewritten, false
	}
	d.Dir = fileDir(p)
	return decisionPath(d), append(appendHeader(nil, d.Number, d.Title), d.Body...), true
}

// lockMergedSession locks the project of the merged session changeset id
// and reads it again under the lock.
func (m *Manager) lockMergedSession(ctx context.Context, id string) (*locked, store.LedgerChangeset, error) {
	c, err := m.st.GetLedgerChangeset(ctx, id)
	if err != nil {
		return nil, c, err
	}
	l, err := m.lockProject(ctx, c.ProjectID)
	if err != nil {
		return nil, c, err
	}
	if err := l.usable(c.ID); err != nil {
		l.close()
		return nil, c, err
	}
	if c, err = m.st.GetLedgerChangeset(ctx, c.ID); err != nil {
		l.close()
		return nil, c, err
	}
	if c.State != store.LedgerChangesetMerged || c.SessionID == "" || l.r == nil {
		l.close()
		return nil, c, stateError(c)
	}
	return l, c, nil
}

// sessionViewState is a session's view after the merge of its changeset.
type sessionViewState struct {
	commit plumbing.Hash
	when   time.Time
	// files are the view's; branchFiles the changeset's (the collection
	// the merge used).
	files, branchFiles map[string]plumbing.Hash
	// renamed maps each new decision's path in the changeset to its path
	// in the view.
	renamed map[string]string
}

// sessionView builds the view of merged session changeset c.
func (m *Manager) sessionView(ctx context.Context, r *repository, c store.LedgerChangeset) (sessionViewState, error) {
	var v sessionViewState
	branch, err := r.readRef(branchRef(c.ID))
	if err != nil {
		return v, err
	}
	merge, err := r.commit(plumbing.NewHash(c.Merge.Commit))
	if err != nil {
		return v, err
	}
	baseRef, err := r.readReference(ctx, plumbing.NewHash(c.BaseCommit), snapshot{}, false)
	if err != nil {
		return v, err
	}
	mergeRef, err := r.readReference(ctx, merge.Hash, baseRef.snapshot, false)
	if err != nil {
		return v, err
	}
	if v.branchFiles, err = r.commitFiles(ctx, branch, true); err != nil {
		return v, err
	}
	numbers := c.Merge.Numbers
	view := maps.Clone(baseRef.files)
	delete(view, indexName)
	v.renamed = make(map[string]string)
	entries := make(map[string]store.LedgerEntry)
	for p, e := range baseRef.byPath {
		entries[p] = e.LedgerEntry
	}
	for _, p := range diff(baseRef.files, v.branchFiles) {
		if p == indexName {
			continue
		}
		delete(view, p)
		delete(entries, p)
		target := renamedPath(p, numbers)
		if target != p {
			v.renamed[p] = target
			delete(view, target)
			delete(entries, target)
		}
		if hash, ok := mergeRef.files[target]; ok {
			view[target] = hash
			entries[target] = mergeRef.byPath[target].LedgerEntry
		}
	}
	list := make([]store.LedgerEntry, 0, len(entries))
	for _, e := range entries {
		list = append(list, e)
	}
	index, err := r.putBlob(renderIndex(list))
	if err != nil {
		return v, err
	}
	view[indexName] = index
	v.files, v.when = view, merge.Committer.When
	v.commit, err = m.putSessionCommit(ctx, r, c.SessionID, view, merge.Hash, v.when,
		"Session view after merging changeset "+c.ID)
	return v, err
}

// SessionView is the view of a session after the merge of its changeset
// id: the base of its next changeset, as PlanSessionRebase makes it.
func (m *Manager) SessionView(ctx context.Context, id string) (string, error) {
	l, c, err := m.lockMergedSession(ctx, id)
	if err != nil {
		return "", err
	}
	defer l.close()
	v, err := m.sessionView(ctx, l.r, c)
	if err != nil {
		return "", err
	}
	return v.commit.String(), nil
}

// PlanSessionRebase plans the rebase of a session's snapshot after the
// merge of its changeset, from a collection of what it holds now.
//
// The view is the changeset's base with each path the changeset changed
// as the merge left it (new decisions renamed, references and conflict
// resolutions as merged), and its generated index. It is a commit whose
// parent is the merge commit, with the merge's time, so building it again
// gives the same commit; refs/sessions/<id> keeps it reachable.
//
// The target starts from the view. A file the agent changed after the
// collection the merge used (the snapshot holds neither that collection's
// content nor its absence) replaces the merged version: a new decision
// whose temporary number the merge assigned becomes that number at its
// current directory and title, written as the merge writes it, so the
// agent's later move, retitle or revision of it is a change of D<n> in the
// next changeset; other files keep their paths with their references
// replaced. A file the agent removed is removed, except a merged new
// decision, which the ledger keeps: it is put back (Restored).
//
// A file is taken for a merged new decision only when that is plausible:
// it is at the decision's path in the collection, or that path is gone
// and it is the only file with the decision's temporary number. Any other
// file with that number is another new decision that reused the letters:
// it gets fresh letters, and the agent is asked to check it (Problems).
func (m *Manager) PlanSessionRebase(ctx context.Context, in RebaseInput) (RebasePlan, error) {
	if err := checkCommitID("snapshot_base", in.SnapshotBase); err != nil {
		return RebasePlan{}, err
	}
	l, c, err := m.lockMergedSession(ctx, in.ChangesetID)
	if err != nil {
		return RebasePlan{}, err
	}
	defer l.close()
	r := l.r
	v, err := m.sessionView(ctx, r, c)
	if err != nil {
		return RebasePlan{}, err
	}
	branchFiles, view, viewRenamed, numbers := v.branchFiles, v.files, v.renamed, c.Merge.Numbers
	plan := RebasePlan{View: v.commit.String(), Renamed: make(map[string]string), Numbers: numbers}

	// What the snapshot holds now, and what it is to hold.
	snapBase, err := r.commitFiles(ctx, plumbing.NewHash(in.SnapshotBase), true)
	if err != nil {
		return RebasePlan{}, err
	}
	files, contents, err := r.snapshotFiles(snapBase, branchFiles, in.Changes)
	if err != nil {
		return RebasePlan{}, err
	}
	read := func(p string, hash plumbing.Hash) ([]byte, error) {
		if data, ok := contents[p]; ok {
			return data, nil
		}
		data, err := r.blob(hash, int64(max(MaxChangesetBytes, maxIndexBytes)), nil)
		if err != nil {
			return nil, fmt.Errorf("ledger: read %q: %w", p, err)
		}
		return data, nil
	}
	target := maps.Clone(view)
	written := make(map[string][]byte)
	later := diff(branchFiles, files)
	laterData := make(map[string][]byte)
	for _, p := range later {
		if hash, ok := files[p]; ok {
			data, err := read(p, hash)
			if err != nil {
				return RebasePlan{}, err
			}
			laterData[p] = data
		}
	}
	id := identify(files, branchFiles, viewRenamed, laterData, numbers)
	for _, p := range later {
		if _, merged := branchFiles[p]; merged {
			delete(target, p)
			if q, ok := viewRenamed[p]; ok && !id.restore[q] {
				delete(target, q)
			}
		}
	}
	plan.Restored = slices.Sorted(maps.Keys(id.restore))
	moved := make(map[string]string)
	for _, p := range later {
		data, ok := laterData[p]
		if !ok {
			continue
		}
		var q string
		var content []byte
		var renumbered bool
		if letters, ok := id.fresh[p]; ok {
			q, content = id.freshFile(p, data)
			renumbered = true
			plan.Problems = append(plan.Problems, Problem{Path: q, Message: fmt.Sprintf(
				"this new decision reused the temporary number %s%s of %s, which the merge numbered %s; it is %s%s now. "+
					"If it was meant as a change of %s, make that change in %s and remove this file",
				temporaryPrefix, id.reused[p], id.merged[id.reused[p]], numberName(numbers[id.reused[p]]), temporaryPrefix, letters,
				numberName(numbers[id.reused[p]]), viewRenamed[id.merged[id.reused[p]]])})
		} else {
			q, content, renumbered = renumber(p, data, numbers)
		}
		if _, taken := written[q]; taken {
			// Another file the agent changed took the name: this one
			// stays as it is, for the agent to sort out.
			q, content, renumbered = p, data, false
		}
		target[q] = plumbing.ComputeHash(plumbing.BlobObject, content)
		written[q] = content
		if renumbered {
			moved[p] = q
		}
	}
	for p := range files {
		if q, ok := moved[p]; ok {
			plan.Renamed[p] = q
		} else if q, ok := viewRenamed[p]; ok && files[p] == branchFiles[p] {
			plan.Renamed[p] = q
		}
	}

	// The edits, each expecting what the snapshot holds now.
	for _, p := range diff(files, target) {
		e := SnapshotEdit{Path: p}
		if hash, ok := files[p]; ok {
			data, err := read(p, hash)
			if err != nil {
				return RebasePlan{}, err
			}
			e.ExpectedSHA256 = sha256Hex(data)
		}
		hash, ok := target[p]
		switch {
		case !ok:
			e.Deleted = true
		case written[p] != nil:
			e.Content = written[p]
		default:
			data, err := r.mainFile(p, hash, true)
			if err != nil {
				return RebasePlan{}, err
			}
			e.Content = data
		}
		plan.Edits = append(plan.Edits, e)
	}
	plan.Target = plan.View
	if len(diff(view, target)) > 0 {
		for p, data := range written {
			if _, err := r.putBlob(data); err != nil {
				return RebasePlan{}, fmt.Errorf("ledger: store %q: %w", p, err)
			}
		}
		commit, err := m.putSessionCommit(ctx, r, c.SessionID, target, v.commit, v.when,
			"Session snapshot with the agent's changes after the merge of changeset "+c.ID)
		if err != nil {
			return RebasePlan{}, err
		}
		plan.Target = commit.String()
	}
	if plan.Known, err = r.knownOf(snapBase, files, contents); err != nil {
		return RebasePlan{}, err
	}
	return plan, nil
}

// identity is how the files the agent changed after the collection a merge
// used relate to the merged new decisions.
type identity struct {
	// merged maps the letters of each merged new decision to its path in
	// the collection.
	merged map[string]string
	// fresh maps a file that reused a merged decision's letters to its
	// fresh letters, and reused to the letters it reused.
	fresh, reused map[string]string
	// restore holds the view paths of merged new decisions to put back.
	restore map[string]bool
	numbers map[string]uint32
}

// identify decides which file stands for each merged new decision: the
// file at its collected path; or, with that path gone, the only changed
// file that carries its letters. Other files carrying them get fresh
// letters; a decision none stands for is restored at its view path.
func identify(files, branchFiles map[string]plumbing.Hash, viewRenamed map[string]string, laterData map[string][]byte,
	numbers map[string]uint32) identity {
	id := identity{merged: make(map[string]string), fresh: make(map[string]string), reused: make(map[string]string),
		restore: make(map[string]bool), numbers: numbers}
	used := make(map[string]bool)
	for letters := range numbers {
		used[letters] = true
	}
	for p := range viewRenamed {
		if _, ok := branchFiles[p]; !ok {
			continue
		}
		if letters, ok := nameLetters(p); ok {
			id.merged[letters] = p
		}
	}
	carriers := make(map[string][]string)
	for _, p := range slices.Sorted(maps.Keys(laterData)) {
		if letters, ok := nameLetters(p); ok {
			used[letters] = true
		}
		if !decisionFile(p) {
			continue
		}
		_, letters, err := parse(laterData[p], true)
		if err != nil || letters == "" {
			continue
		}
		used[letters] = true
		if _, ok := id.merged[letters]; ok {
			carriers[letters] = append(carriers[letters], p)
		}
	}
	for letters, p := range id.merged {
		var others []string
		for _, c := range carriers[letters] {
			if c != p {
				others = append(others, c)
			}
		}
		_, kept := files[p]
		switch {
		case kept:
		case len(others) == 1:
			// Moved or retitled.
			others = nil
		default:
			id.restore[viewRenamed[p]] = true
		}
		for _, c := range others {
			id.fresh[c], id.reused[c] = "", letters
		}
	}
	for _, p := range slices.Sorted(maps.Keys(id.fresh)) {
		id.fresh[p] = freeLetters(used)
		used[id.fresh[p]] = true
	}
	return id
}

// nameLetters returns the temporary letters in p's file name.
func nameLetters(p string) (string, bool) {
	rest, ok := strings.CutPrefix(path.Base(p), temporaryPrefix)
	if !ok {
		return "", false
	}
	letters, _, ok := strings.Cut(rest, "-")
	return letters, ok && letters != ""
}

// freeLetters returns the first letters, in assignment order (a, …, z,
// aa, …), not in used.
func freeLetters(used map[string]bool) string {
	for n := 1; ; n++ {
		buf := make([]byte, n)
		for i := range buf {
			buf[i] = 'a'
		}
		for {
			if s := string(buf); !used[s] {
				return s
			}
			i := n - 1
			for i >= 0 && buf[i] == 'z' {
				buf[i] = 'a'
				i--
			}
			if i < 0 {
				break
			}
			buf[i]++
		}
	}
}

// freshFile is the file the agent wrote at p, data, as a new decision
// under its fresh letters: at its directory with the name the letters and
// its title make, its references to other merged decisions replaced, and
// those to the letters it reused left for the agent to check.
func (id identity) freshFile(p string, data []byte) (string, []byte) {
	letters := id.fresh[p]
	others := maps.Clone(id.numbers)
	delete(others, id.reused[p])
	d, _, err := parse(data, true)
	if err != nil {
		return p, data
	}
	header := "---\nnumber: " + temporaryPrefix + letters + "\ntitle: " + strconv.Quote(d.Title) + "\n---\n\n"
	return joinPath(fileDir(p), fileName(temporaryPrefix+letters, d.Title)), append([]byte(header), rewriteTemporary(d.Body, others)...)
}

// mergedLetters returns the temporary letters the merges of the session's
// changesets assigned numbers to.
func (m *Manager) mergedLetters(ctx context.Context, projectID, sessionID string) (map[string]bool, error) {
	out := make(map[string]bool)
	f := store.LedgerChangesetFilter{ProjectID: projectID, State: store.LedgerChangesetMerged, SessionID: sessionID, Limit: store.MaxLedgerChangesetPage}
	for {
		page, more, err := m.st.ListLedgerChangesets(ctx, f)
		if err != nil {
			return nil, err
		}
		for _, c := range page {
			for letters := range c.Merge.Numbers {
				out[letters] = true
			}
		}
		if !more || len(page) == 0 {
			return out, nil
		}
		last := page[len(page)-1]
		f.After = &store.LedgerChangesetCursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
}

// staleReferences turns the warnings of references to temporary numbers
// that an earlier merge of the session assigned, and that no new decision
// of this changeset has, into problems: the rebase after that merge
// replaced such references everywhere but in a new decision that reused
// the letters (given fresh ones), where the agent must decide what they
// mean before the changeset can be merged.
func staleReferences(effective []change, assigned map[string]bool, problems, warnings []Problem) ([]Problem, []Problem) {
	if len(assigned) == 0 {
		return problems, warnings
	}
	carried := make(map[string]bool)
	for _, ch := range effective {
		if ch.deleted || !decisionFile(ch.path) {
			continue
		}
		if _, letters, err := parse(ch.content, true); err == nil && letters != "" {
			carried[letters] = true
		}
	}
	stale := make(map[string]bool)
	for _, ch := range effective {
		if ch.deleted {
			continue
		}
		seen := make(map[string]bool)
		temporaryReferences(ch.content, func(_, _ int, letters string) {
			if !assigned[letters] || carried[letters] || seen[letters] {
				return
			}
			seen[letters] = true
			stale[ch.path+"\x00"+letters] = true
			problems = append(problems, Problem{Path: ch.path, Message: "refers to " + temporaryPrefix + letters +
				", which no decision of this changeset has; check it"})
		})
	}
	if len(stale) == 0 {
		return problems, warnings
	}
	kept := warnings[:0:0]
	for _, w := range warnings {
		letters, ok := strings.CutPrefix(w.Message, "refers to "+temporaryPrefix)
		if ok {
			letters, _, _ = strings.Cut(letters, ",")
			if stale[w.Path+"\x00"+letters] {
				continue
			}
		}
		kept = append(kept, w)
	}
	slices.SortStableFunc(problems, func(a, b Problem) int { return strings.Compare(a.Path, b.Path) })
	return boundProblems(problems), kept
}

// HasCommit reports whether the project's ledger holds commit.
func (m *Manager) HasCommit(ctx context.Context, projectID, commit string) (bool, error) {
	if !store.IsLedgerCommitID(commit) {
		return false, nil
	}
	if err := m.CheckProject(ctx, projectID); err != nil {
		return false, err
	}
	release, err := m.acquire(ctx, projectID)
	if err != nil {
		return false, err
	}
	defer release()
	r, err := m.open(m.repoPath(projectID))
	if err != nil || r == nil {
		return false, err
	}
	defer r.storage.Close()
	if _, err := r.commit(plumbing.NewHash(commit)); err != nil {
		if errors.Is(err, plumbing.ErrObjectNotFound) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// putSessionCommit writes files as a commit with parent and time and
// points the session's reference at it.
func (m *Manager) putSessionCommit(ctx context.Context, r *repository, sessionID string, files map[string]plumbing.Hash,
	parent plumbing.Hash, when time.Time, message string) (plumbing.Hash, error) {
	list := make([]fileObject, 0, len(files))
	for p, h := range files {
		list = append(list, fileObject{path: p, hash: h})
	}
	sortedFiles(list)
	tree, err := r.writeTree(ctx, list, 0, nil)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	sig := object.Signature{Name: repairAuthor, Email: "ledger@circus.invalid", When: when.UTC().Truncate(time.Second)}
	commit := &object.Commit{Author: sig, Committer: sig, Message: message + "\n", TreeHash: tree, ParentHashes: parents(parent)}
	o := r.storage.NewEncodedObject()
	if err := commit.Encode(o); err != nil {
		return plumbing.ZeroHash, err
	}
	hash, err := r.putObject(o)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	name := sessionRef(sessionID)
	current, err := r.readRef(name)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	if current != hash {
		if _, err := r.setRef(name, current, hash); err != nil {
			return plumbing.ZeroHash, err
		}
	}
	return hash, nil
}

// RemoveSession removes what the ledger keeps of a deleted session: its
// view's reference.
func (m *Manager) RemoveSession(ctx context.Context, projectID, sessionID string) error {
	if err := store.CheckID("session id", sessionID); err != nil {
		return err
	}
	l, err := m.lockProject(ctx, projectID)
	if err != nil {
		return err
	}
	defer l.close()
	if l.r == nil {
		return nil
	}
	current, err := l.r.readRef(sessionRef(sessionID))
	if err != nil || current.IsZero() {
		return err
	}
	_, err = l.r.setRef(sessionRef(sessionID), current, plumbing.ZeroHash)
	return err
}

// emptyFiles returns the files of the empty ledger after the path after:
// its index.
func emptyFiles(after string) []File {
	if indexName <= after {
		return nil
	}
	return []File{{Path: indexName, Content: slices.Clone(emptyIndex)}}
}

// SnapshotPage is one chunk of a commit's files for prepare_ledger.
type SnapshotPage struct {
	Files []File
	More  bool
}

// ReadSnapshotFiles returns the files of commit (empty: the empty
// ledger, whose only file is its index) after the path after, in path
// order, up to maxBytes of content and maxFiles files; a file larger than
// maxBytes (only the generated index, up to its own bound) comes alone.
func (m *Manager) ReadSnapshotFiles(ctx context.Context, projectID, commit, after string, maxBytes, maxFiles int) (SnapshotPage, error) {
	if err := checkCommitID("commit", commit); err != nil {
		return SnapshotPage{}, err
	}
	if commit == "" {
		return SnapshotPage{Files: emptyFiles(after)}, nil
	}
	if err := m.CheckProject(ctx, projectID); err != nil {
		return SnapshotPage{}, err
	}
	release, err := m.acquire(ctx, projectID)
	if err != nil {
		return SnapshotPage{}, err
	}
	defer release()
	r, err := m.open(m.repoPath(projectID))
	if err != nil {
		return SnapshotPage{}, err
	}
	if r == nil {
		return SnapshotPage{}, fmt.Errorf("ledger: repository of project %q is missing", projectID)
	}
	defer r.storage.Close()
	c, err := r.commit(plumbing.NewHash(commit))
	if err != nil {
		return SnapshotPage{}, err
	}
	var out SnapshotPage
	used := 0
	full := errors.New("page full")
	err = r.walkTree(ctx, c.TreeHash, "", nil, true, func(f fileObject) error {
		if f.path <= after {
			return nil
		}
		if len(out.Files) >= maxFiles || used >= maxBytes {
			return full
		}
		limit := maxBytes
		if f.path == indexName {
			// The generated index may exceed a page; it comes alone.
			limit = max(limit, snapshotfiles.MaxIndexBytes)
		}
		data, err := r.blob(f.hash, int64(limit), nil)
		if err != nil {
			return fmt.Errorf("ledger: read %q: %w", f.path, err)
		}
		if used > 0 && used+len(data) > maxBytes {
			return full
		}
		used += len(data)
		out.Files = append(out.Files, File{Path: f.path, Content: data})
		return nil
	})
	if errors.Is(err, full) {
		out.More = true
		err = nil
	}
	return out, err
}
