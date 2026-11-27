package ledger

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/EricWvi/circus/internal/store"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"golang.org/x/sys/unix"
)

const (
	branchPrefix    = "refs/heads/changesets/"
	discardedPrefix = "refs/discarded/"
	// repairAuthor signs archives that a repair finishes.
	repairAuthor = "circus"
)

func branchRef(id string) plumbing.ReferenceName { return plumbing.ReferenceName(branchPrefix + id) }

func discardedRef(id string) plumbing.ReferenceName {
	return plumbing.ReferenceName(discardedPrefix + id)
}

// ValidChangesetID accepts changeset IDs that can name a reference.
func ValidChangesetID(id string) bool { return ValidProjectID(id) && !strings.HasSuffix(id, ".lock") }

// locked is a project held for a changeset operation: its queue, the
// shared root lock and, once the repository exists, main.lock.
type locked struct {
	m          *Manager
	projectID  string
	r          *repository
	unlockMain func()
	release    func()
	// failed holds the changesets whose repair failed, by id.
	failed map[string]error
}

// usable refuses an operation on a changeset whose repair failed.
func (l *locked) usable(id string) error {
	return l.failed[id]
}

func (l *locked) close() {
	if l.unlockMain != nil {
		l.unlockMain()
	}
	if l.r != nil {
		_ = l.r.storage.Close()
	}
	l.release()
}

// lockProject takes the project for a changeset operation. The project
// must exist. Its repository, when there is one, is open with main.lock
// held, and the changesets' unfinished merges and archives are repaired;
// a changeset whose repair fails is left to usable to refuse.
func (m *Manager) lockProject(ctx context.Context, projectID string) (*locked, error) {
	if err := m.CheckProject(ctx, projectID); err != nil {
		return nil, err
	}
	release, err := m.acquire(ctx, projectID)
	if err != nil {
		return nil, err
	}
	unlockRoot, err := m.lockRoot(unix.LOCK_SH)
	if err != nil {
		release()
		return nil, err
	}
	l := &locked{m: m, projectID: projectID, release: func() { unlockRoot(); release() }}
	if _, exists, err := m.st.LedgerProjectName(ctx, projectID); err != nil || !exists {
		l.close()
		return nil, cmpErr(err, store.ErrNotFound)
	}
	if err := l.openRepository(); err != nil {
		l.close()
		return nil, err
	}
	if l.failed, err = m.repair(ctx, l.r, projectID); err != nil {
		l.close()
		return nil, err
	}
	return l, nil
}

// RepairChangesets finishes the project's interrupted merges and archives
// now, as any changeset operation of the project would, and returns the
// changesets whose repair failed, by id, each with its *RepairError.
// Readers call it for a changeset with a merge in progress, which is
// merged or open only once it is repaired.
func (m *Manager) RepairChangesets(ctx context.Context, projectID string) (map[string]error, error) {
	l, err := m.lockProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	defer l.close()
	return l.failed, nil
}

func cmpErr(err, otherwise error) error {
	if err != nil {
		return err
	}
	return otherwise
}

func (l *locked) openRepository() error {
	r, err := l.m.open(l.m.repoPath(l.projectID))
	if err != nil || r == nil {
		return err
	}
	unlock, err := r.lockMain()
	if err != nil {
		_ = r.storage.Close()
		return err
	}
	l.r, l.unlockMain = r, unlock
	return nil
}

// createRepository creates the project's repository, without a main line,
// for a changeset's branch.
func (l *locked) createRepository() error {
	m := l.m
	staging, err := os.MkdirTemp(m.root, initPrefix)
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	r, err := initRepository(staging)
	if err != nil {
		return err
	}
	for _, file := range []string{"HEAD", "config"} {
		if err := syncPath(filepath.Join(staging, file)); err != nil {
			_ = r.storage.Close()
			return err
		}
	}
	_ = r.storage.Close()
	dst := m.repoPath(l.projectID)
	if err := os.Rename(staging, dst); err != nil {
		if errors.Is(err, unix.ENOTEMPTY) || errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%w: ledger repository was created concurrently", store.ErrBusy)
		}
		return err
	}
	for _, dir := range []string{dst, m.root, filepath.Dir(m.root)} {
		if err := syncPath(dir); err != nil {
			return err
		}
	}
	return l.openRepository()
}

// OpenChangeset records a new changeset with the given id, based on the
// main line's current head.
func (m *Manager) OpenChangeset(ctx context.Context, projectID, id, openerWorkerID string) (store.LedgerChangeset, error) {
	if !ValidChangesetID(id) {
		return store.LedgerChangeset{}, fmt.Errorf("%w: invalid changeset id", store.ErrInvalid)
	}
	l, err := m.lockProject(ctx, projectID)
	if err != nil {
		return store.LedgerChangeset{}, err
	}
	defer l.close()
	var base plumbing.Hash
	if l.r != nil {
		if base, err = l.r.head(); err != nil {
			return store.LedgerChangeset{}, err
		}
	}
	now := m.now().UnixMilli()
	c := store.LedgerChangeset{ID: id, ProjectID: projectID, State: store.LedgerChangesetOpen, BaseCommit: commitID(base),
		OpenerWorkerID: openerWorkerID, CreatedAt: now, UpdatedAt: now, Archived: true}
	if err := m.st.CreateLedgerChangeset(ctx, c); err != nil {
		return store.LedgerChangeset{}, err
	}
	return c, nil
}

// ReadChangesetBase returns the base's files after the path after, in
// path order, until their encoded size reaches budget; a file larger than
// the budget comes alone. more reports that files follow.
func (m *Manager) ReadChangesetBase(ctx context.Context, c store.LedgerChangeset, after string, budget int) (files []File, more bool, _ error) {
	if c.BaseCommit == "" {
		return emptyFiles(after), false, nil
	}
	if err := m.CheckProject(ctx, c.ProjectID); err != nil {
		return nil, false, err
	}
	release, err := m.acquire(ctx, c.ProjectID)
	if err != nil {
		return nil, false, err
	}
	defer release()
	r, err := m.open(m.repoPath(c.ProjectID))
	if err != nil {
		return nil, false, err
	}
	if r == nil {
		return nil, false, fmt.Errorf("ledger: repository of project %q is missing", c.ProjectID)
	}
	defer r.storage.Close()
	commit, err := r.commit(plumbing.NewHash(c.BaseCommit))
	if err != nil {
		return nil, false, err
	}
	return r.readFiles(ctx, commit.TreeHash, after, budget)
}

// readFiles returns the files of tree after the path after, in path order,
// until their encoded size reaches budget; a file larger than the budget
// comes alone. more reports that files follow.
func (r *repository) readFiles(ctx context.Context, tree plumbing.Hash, after string, budget int) (files []File, more bool, _ error) {
	used := 0
	full := errors.New("page full")
	err := r.walkFiles(ctx, tree, "", func(f fileObject) error {
		if f.path <= after {
			return nil
		}
		if used >= budget {
			return full
		}
		limit := int64(MaxDecisionFileBytes)
		if f.path == indexName {
			limit = maxIndexBytes
		}
		data, err := r.blob(f.hash, limit, nil)
		if err != nil {
			return fmt.Errorf("ledger: read %q: %w", f.path, err)
		}
		size := EncodedBytes(data) + EncodedBytes([]byte(f.path)) + 16
		if used > 0 && used+size > budget {
			return full
		}
		used += size
		files = append(files, File{Path: f.path, Content: data})
		return nil
	})
	if errors.Is(err, full) {
		return files, true, nil
	}
	return files, false, err
}

// readReference reads the snapshot of a main-line commit, with its trees
// when trees is true; cache supplies parsed entries by blob.
func (r *repository) readReference(ctx context.Context, commit plumbing.Hash, cache snapshot, trees bool) (reference, error) {
	if commit.IsZero() {
		return newReference(emptySnapshot()), nil
	}
	c, err := r.commit(commit)
	if err != nil {
		return reference{}, err
	}
	s, err := r.readSnapshot(ctx, c.TreeHash, cache, trees)
	if err != nil {
		return reference{}, fmt.Errorf("ledger: read snapshot %s: %w", commit, err)
	}
	return newReference(s), nil
}

// changesetFiles reads the files of a changeset's branch commit, which
// may hold any file.
func (r *repository) changesetFiles(ctx context.Context, commit plumbing.Hash) (map[string]plumbing.Hash, *object.Commit, error) {
	c, err := r.commit(commit)
	if err != nil {
		return nil, nil, err
	}
	files := make(map[string]plumbing.Hash)
	err = r.walkTree(ctx, c.TreeHash, "", nil, true, func(f fileObject) error {
		if len(files) > store.MaxLedgerDecisions {
			return store.ErrLimit
		}
		files[f.path] = f.hash
		return nil
	})
	return files, c, err
}

// sameTree reports whether two commits hold the same tree, which is how
// a changeset's base is compared with the main line's head: a zero commit
// (an empty ledger) holds no tree. r may be nil when both are zero.
func (r *repository) sameTree(a, b plumbing.Hash) (bool, error) {
	if a == b {
		return true, nil
	}
	if a.IsZero() || b.IsZero() {
		return false, nil
	}
	ca, err := r.commit(a)
	if err != nil {
		return false, err
	}
	cb, err := r.commit(b)
	if err != nil {
		return false, err
	}
	return ca.TreeHash == cb.TreeHash, nil
}

// diff lists the paths where two file maps differ, in path order.
func diff(a, b map[string]plumbing.Hash) []string {
	var out []string
	for p, h := range a {
		if other, ok := b[p]; !ok || other != h {
			out = append(out, p)
		}
	}
	for p := range b {
		if _, ok := a[p]; !ok {
			out = append(out, p)
		}
	}
	slices.Sort(out)
	return out
}

// CheckChangeset replaces an open changeset's content with changes, the
// complete set relative to its base, validates it and compares it with
// the main line. author names the opener on the branch's commit.
func (m *Manager) CheckChangeset(ctx context.Context, id, author string, changes []Change) (CheckResult, error) {
	if err := CheckChanges("changes", changes); err != nil {
		return CheckResult{}, err
	}
	c, err := m.st.GetLedgerChangeset(ctx, id)
	if err != nil {
		return CheckResult{}, err
	}
	l, err := m.lockProject(ctx, c.ProjectID)
	if err != nil {
		return CheckResult{}, err
	}
	defer l.close()
	if err := l.usable(id); err != nil {
		return CheckResult{}, err
	}
	if c, err = m.st.GetLedgerChangeset(ctx, id); err != nil {
		return CheckResult{}, err
	}
	if c.State != store.LedgerChangesetOpen {
		return CheckResult{}, stateError(c)
	}
	out, _, _, err := m.check(ctx, l, c, author, changes)
	return out, err
}

// check is CheckChangeset of the open changeset c under the project's
// lock. It also returns the effective changes and the base's reference.
func (m *Manager) check(ctx context.Context, l *locked, c store.LedgerChangeset, author string, changes []Change) (CheckResult, []change, reference, error) {
	id := c.ID
	base := plumbing.NewHash(c.BaseCommit)
	var baseRef reference
	var err error
	if c.BaseCommit != "" {
		if l.r == nil {
			return CheckResult{}, nil, reference{}, fmt.Errorf("ledger: repository of project %q is missing", c.ProjectID)
		}
		if baseRef, err = l.r.readReference(ctx, base, snapshot{}, true); err != nil {
			return CheckResult{}, nil, reference{}, err
		}
	} else {
		baseRef = newReference(emptySnapshot())
	}

	// Apply the changes to the base's files.
	files := maps.Clone(baseRef.files)
	var effective []change
	blobs := make(map[string]plumbing.Hash)
	for i, ch := range changes {
		old, exists := baseRef.files[ch.Path]
		if ch.Deleted {
			if !exists {
				return CheckResult{}, nil, reference{}, &RequestError{Field: fmt.Sprintf("changes[%d].path", i), Message: "the base has no such file to remove"}
			}
			delete(files, ch.Path)
			effective = append(effective, change{path: ch.Path, deleted: true})
			continue
		}
		hash := plumbing.ComputeHash(plumbing.BlobObject, ch.Content)
		if exists && hash == old {
			continue
		}
		files[ch.Path] = hash
		blobs[ch.Path] = hash
		effective = append(effective, change{path: ch.Path, content: ch.Content})
	}
	if len(files) > store.MaxLedgerDecisions+1 {
		return CheckResult{}, nil, reference{}, &LimitError{Message: "a changeset holds at most 4096 decisions"}
	}
	for p := range files {
		for dir := fileDir(p); dir != ""; dir = fileDir(dir) {
			if _, ok := files[dir]; ok {
				return CheckResult{}, nil, reference{}, &RequestError{Field: "changes", Message: fmt.Sprintf("%q is a file and a directory of %q", dir, p)}
			}
		}
	}

	if len(effective) == 0 {
		if l.r != nil {
			if err := l.r.dropBranch(id); err != nil {
				return CheckResult{}, nil, reference{}, err
			}
		}
	} else {
		if l.r == nil {
			if err := l.createRepository(); err != nil {
				return CheckResult{}, nil, reference{}, err
			}
		}
		if err := m.writeBranch(ctx, l.r, c, author, baseRef, files, effective, blobs); err != nil {
			return CheckResult{}, nil, reference{}, err
		}
	}

	problems, temporary := validateChanges(baseRef, effective)
	out := CheckResult{Problems: boundProblems(problems), Warnings: unknownReferences(effective, temporary)}
	var head plumbing.Hash
	if l.r != nil {
		if head, err = l.r.head(); err != nil {
			return CheckResult{}, nil, reference{}, err
		}
	}
	out.MainCommit = commitID(head)
	if out.FastForward, err = l.r.sameTree(base, head); err != nil {
		return CheckResult{}, nil, reference{}, err
	}
	if !out.FastForward {
		mainRef, err := l.r.readReference(ctx, head, baseRef.snapshot, false)
		if err != nil {
			return CheckResult{}, nil, reference{}, err
		}
		if out.Conflicts, err = l.r.conflicts(ctx, baseRef, mainRef, effective); err != nil {
			return CheckResult{}, nil, reference{}, err
		}
	}
	if _, err := m.st.TouchLedgerChangeset(ctx, id, m.now().UnixMilli()); err != nil {
		return CheckResult{}, nil, reference{}, err
	}
	if out.Changeset, err = m.st.GetLedgerChangeset(ctx, id); err != nil {
		return CheckResult{}, nil, reference{}, err
	}
	return out, effective, baseRef, nil
}

// writeBranch stores the changeset's files as the commit of its branch,
// unless the branch already holds them.
func (m *Manager) writeBranch(ctx context.Context, r *repository, c store.LedgerChangeset, author string, baseRef reference,
	files map[string]plumbing.Hash, effective []change, blobs map[string]plumbing.Hash) error {
	for _, ch := range effective {
		if !ch.deleted {
			if err := r.storeObject(plumbing.BlobObject, blobs[ch.path], ch.content); err != nil {
				return err
			}
		}
	}
	if files[indexName] == emptyIndexHash {
		// A changeset of the empty ledger keeps its index, which no
		// commit stored.
		if err := r.storeObject(plumbing.BlobObject, emptyIndexHash, emptyIndex); err != nil {
			return err
		}
	}
	reusable := maps.Clone(baseRef.trees)
	for _, ch := range effective {
		delete(reusable, "")
		for dir := fileDir(ch.path); dir != ""; dir = fileDir(dir) {
			delete(reusable, dir+"/")
		}
	}
	list := make([]fileObject, 0, len(files))
	for p, h := range files {
		list = append(list, fileObject{path: p, hash: h})
	}
	sortedFiles(list)
	tree, err := r.writeTree(ctx, list, 0, reusable)
	if err != nil {
		return err
	}
	name := branchRef(c.ID)
	current, err := r.readRef(name)
	if err != nil {
		return err
	}
	base := plumbing.NewHash(c.BaseCommit)
	if !current.IsZero() {
		old, err := r.commit(current)
		if err == nil && old.TreeHash == tree && slices.Equal(old.ParentHashes, parents(base)) {
			return nil
		}
	}
	sig := object.Signature{Name: signatureName(author), Email: "ledger@circus.invalid", When: m.now().UTC().Truncate(time.Second)}
	commit := &object.Commit{Author: sig, Committer: sig, Message: "Changeset " + c.ID + "\n", TreeHash: tree, ParentHashes: parents(base)}
	o := r.storage.NewEncodedObject()
	if err := commit.Encode(o); err != nil {
		return err
	}
	hash, err := r.putObject(o)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err = r.setRef(name, current, hash)
	return err
}

func parents(base plumbing.Hash) []plumbing.Hash {
	if base.IsZero() {
		return nil
	}
	return []plumbing.Hash{base}
}

// signatureName keeps a name Git can record as an author.
func signatureName(name string) string {
	if _, err := commitMessage(CommitOptions{Author: name, Message: "x"}); err != nil {
		return repairAuthor
	}
	return name
}

// dropBranch removes a changeset's branch: it has no changes.
func (r *repository) dropBranch(id string) error {
	current, err := r.readRef(branchRef(id))
	if err != nil || current.IsZero() {
		return err
	}
	_, err = r.setRef(branchRef(id), current, plumbing.ZeroHash)
	return err
}

// conflicts lists the changed paths that the main line also changed after
// the base, with their three versions while they fit MaxConflictBytes.
func (r *repository) conflicts(ctx context.Context, baseRef, mainRef reference, effective []change) ([]Conflict, error) {
	var out []Conflict
	used := 0
	effective = slices.Clone(effective)
	slices.SortFunc(effective, func(a, b change) int { return strings.Compare(a.path, b.path) })
	for _, ch := range effective {
		baseHash, inBase := baseRef.files[ch.path]
		mainHash, inMain := mainRef.files[ch.path]
		if inBase == inMain && baseHash == mainHash {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		cf := Conflict{Path: ch.path, Base: Version{Exists: inBase}, Changeset: Version{Exists: !ch.deleted}, Main: Version{Exists: inMain}, Omitted: true}
		if used < MaxConflictBytes {
			base, err := r.mainFile(ch.path, baseHash, inBase)
			if err != nil {
				return nil, err
			}
			main, err := r.mainFile(ch.path, mainHash, inMain)
			if err != nil {
				return nil, err
			}
			if size := EncodedBytes(base) + EncodedBytes(ch.content) + EncodedBytes(main); used+size <= MaxConflictBytes {
				cf.Base.Content, cf.Changeset.Content, cf.Main.Content, cf.Omitted = base, ch.content, main, false
				used += size
			} else {
				// Later conflicts are reported without contents too, so
				// the ones shown come first in path order.
				used = MaxConflictBytes
			}
		}
		out = append(out, cf)
	}
	return out, nil
}

// ConflictPage is one page of ReadChangesetConflicts.
type ConflictPage struct {
	Conflicts []Conflict
	// More reports that conflicts follow.
	More bool
	// Branch is the changeset's branch commit the page was read from; the
	// next page must be read from the same one.
	Branch string
}

// ReadChangesetConflicts returns the conflicts of the changeset's branch,
// its last check, against the main line at mainCommit, which must still be
// the head: those after the path after, in path order, with all their
// contents, until their encoded size reaches budget. A conflict larger
// than the budget comes alone. branch, when not empty, is the branch
// commit of the previous page, which must still be the branch's.
//
// A page reads the trees of the base, the main line and the branch to
// find the conflicts, and then only the blobs of its own conflicts, so a
// check whose conflicts take many pages reads each conflict's contents
// once.
func (m *Manager) ReadChangesetConflicts(ctx context.Context, id, mainCommit, branch, after string, budget int) (ConflictPage, error) {
	if mainCommit != "" && !store.IsLedgerCommitID(mainCommit) {
		return ConflictPage{}, &RequestError{Field: "main_commit", Message: "main commit must be 40 lowercase hexadecimal characters"}
	}
	c, err := m.st.GetLedgerChangeset(ctx, id)
	if err != nil {
		return ConflictPage{}, err
	}
	l, err := m.lockProject(ctx, c.ProjectID)
	if err != nil {
		return ConflictPage{}, err
	}
	defer l.close()
	if err := l.usable(id); err != nil {
		return ConflictPage{}, err
	}
	if c, err = m.st.GetLedgerChangeset(ctx, id); err != nil {
		return ConflictPage{}, err
	}
	if c.State != store.LedgerChangesetOpen {
		return ConflictPage{}, stateError(c)
	}
	r := l.r
	var head, current plumbing.Hash
	if r != nil {
		if head, err = r.head(); err != nil {
			return ConflictPage{}, err
		}
		if current, err = r.readRef(branchRef(id)); err != nil {
			return ConflictPage{}, err
		}
	}
	if commitID(head) != mainCommit {
		return ConflictPage{}, &PreconditionError{Violations: []Violation{{Type: ViolationMainMoved, Subject: commitID(head),
			Description: "the main line moved after the check; check again"}}}
	}
	if branch != "" && commitID(current) != branch {
		return ConflictPage{}, &PreconditionError{Violations: []Violation{{Type: ViolationChangesetChanged, Subject: id,
			Description: "the changeset was checked again while its conflicts were read; check again"}}}
	}
	page := ConflictPage{Branch: commitID(current)}
	if current.IsZero() {
		return page, nil
	}
	if same, err := r.sameTree(plumbing.NewHash(c.BaseCommit), head); err != nil || same {
		return page, err
	}
	baseFiles, err := r.commitFiles(ctx, plumbing.NewHash(c.BaseCommit), false)
	if err != nil {
		return ConflictPage{}, err
	}
	mainFiles, err := r.commitFiles(ctx, head, false)
	if err != nil {
		return ConflictPage{}, err
	}
	files, err := r.commitFiles(ctx, current, true)
	if err != nil {
		return ConflictPage{}, err
	}
	used := 0
	for _, p := range diff(baseFiles, files) {
		baseHash, inBase := baseFiles[p]
		mainHash, inMain := mainFiles[p]
		if p <= after || inBase == inMain && baseHash == mainHash {
			continue
		}
		if err := ctx.Err(); err != nil {
			return ConflictPage{}, err
		}
		if used >= budget {
			page.More = true
			return page, nil
		}
		hash, inBranch := files[p]
		cf := Conflict{Path: p, Base: Version{Exists: inBase}, Changeset: Version{Exists: inBranch}, Main: Version{Exists: inMain}}
		if cf.Base.Content, err = r.mainFile(p, baseHash, inBase); err != nil {
			return ConflictPage{}, err
		}
		if cf.Main.Content, err = r.mainFile(p, mainHash, inMain); err != nil {
			return ConflictPage{}, err
		}
		if inBranch {
			if cf.Changeset.Content, err = r.blob(hash, MaxChangesetBytes, nil); err != nil {
				return ConflictPage{}, fmt.Errorf("ledger: read %q of changeset %s: %w", p, id, err)
			}
		}
		size := EncodedBytes(cf.Base.Content) + EncodedBytes(cf.Changeset.Content) + EncodedBytes(cf.Main.Content) + EncodedBytes([]byte(p)) + 32
		if used > 0 && used+size > budget {
			page.More = true
			return page, nil
		}
		used += size
		page.Conflicts = append(page.Conflicts, cf)
	}
	return page, nil
}

// commitFiles maps the files of a commit to their blobs, reading only its
// trees; a zero commit (the empty ledger) has only the empty index.
// lenient accepts the files of a changeset's branch, which the main line
// could not hold.
func (r *repository) commitFiles(ctx context.Context, commit plumbing.Hash, lenient bool) (map[string]plumbing.Hash, error) {
	files := make(map[string]plumbing.Hash)
	if commit.IsZero() {
		files[indexName] = emptyIndexHash
		return files, nil
	}
	c, err := r.commit(commit)
	if err != nil {
		return nil, err
	}
	err = r.walkTree(ctx, c.TreeHash, "", nil, lenient, func(f fileObject) error {
		if len(files) > store.MaxLedgerDecisions {
			return store.ErrLimit
		}
		files[f.path] = f.hash
		return nil
	})
	return files, err
}

// mainFile reads a file of a main-line commit.
func (r *repository) mainFile(p string, hash plumbing.Hash, exists bool) ([]byte, error) {
	if !exists {
		return nil, nil
	}
	limit := int64(MaxDecisionFileBytes)
	if p == indexName {
		limit = maxIndexBytes
	}
	data, err := r.blob(hash, limit, nil)
	if err != nil {
		return nil, fmt.Errorf("ledger: read %q: %w", p, err)
	}
	return data, nil
}

// MergeOptions configures a merge.
type MergeOptions struct {
	// Author names the merging worker on the main-line commit.
	Author     string
	CodeCommit string
	// Resolved gives the final content of conflicting files.
	Resolved []Change
	// Authorize is called under the project's lock with whether the merge
	// fast-forwards; its error refuses the merge.
	Authorize func(ctx context.Context, fastForward bool) error
}

// MergeChangeset merges an open changeset into the main line, or returns
// a merged one as it is.
func (m *Manager) MergeChangeset(ctx context.Context, id string, o MergeOptions) (store.LedgerChangeset, error) {
	if err := CheckChanges("resolved", o.Resolved); err != nil {
		return store.LedgerChangeset{}, err
	}
	if o.CodeCommit != "" && !store.IsLedgerCommitID(o.CodeCommit) {
		return store.LedgerChangeset{}, &RequestError{Field: "code_commit", Message: "code commit must be 40 lowercase hexadecimal characters"}
	}
	c, err := m.st.GetLedgerChangeset(ctx, id)
	if err != nil {
		return store.LedgerChangeset{}, err
	}
	l, err := m.lockProject(ctx, c.ProjectID)
	if err != nil {
		return store.LedgerChangeset{}, err
	}
	defer l.close()
	if err := l.usable(id); err != nil {
		return store.LedgerChangeset{}, err
	}
	if c, err = m.st.GetLedgerChangeset(ctx, id); err != nil {
		return store.LedgerChangeset{}, err
	}
	switch c.State {
	case store.LedgerChangesetMerged:
		return c, nil
	case store.LedgerChangesetDiscarded:
		return store.LedgerChangeset{}, stateError(c)
	}
	r := l.r
	var branch plumbing.Hash
	if r != nil {
		if branch, err = r.readRef(branchRef(id)); err != nil {
			return store.LedgerChangeset{}, err
		}
	}
	if branch.IsZero() {
		return store.LedgerChangeset{}, &PreconditionError{Violations: []Violation{{Type: ViolationProblem, Subject: ".",
			Description: "the changeset has no changes; check it with its changes first"}}}
	}
	head, err := r.head()
	if err != nil {
		return store.LedgerChangeset{}, err
	}
	fastForward, err := r.sameTree(plumbing.NewHash(c.BaseCommit), head)
	if err != nil {
		return store.LedgerChangeset{}, err
	}
	if o.Authorize != nil {
		if err := o.Authorize(ctx, fastForward); err != nil {
			return store.LedgerChangeset{}, err
		}
	}
	base := plumbing.NewHash(c.BaseCommit)
	// The main line's snapshot keeps its trees for the new commit.
	baseRef, err := r.readReference(ctx, base, snapshot{}, fastForward)
	if err != nil {
		return store.LedgerChangeset{}, err
	}
	mainRef := baseRef
	if !fastForward {
		if mainRef, err = r.readReference(ctx, head, baseRef.snapshot, true); err != nil {
			return store.LedgerChangeset{}, err
		}
	}
	files, branchCommit, err := r.changesetFiles(ctx, branch)
	if err != nil {
		return store.LedgerChangeset{}, err
	}
	if !slices.Equal(branchCommit.ParentHashes, parents(base)) {
		return store.LedgerChangeset{}, fmt.Errorf("ledger: branch of changeset %s does not start from its base", id)
	}

	// The final changes relative to the main line: the changeset's, with
	// conflicting files replaced by their resolutions.
	resolved := make(map[string]Change, len(o.Resolved))
	if !fastForward {
		for _, ch := range o.Resolved {
			resolved[ch.Path] = ch
		}
	}
	var final []change
	var problems []Problem
	var unresolved []string
	budget := MaxChangesetBytes
	for _, p := range diff(baseRef.files, files) {
		baseHash, inBase := baseRef.files[p]
		mainHash, inMain := mainRef.files[p]
		conflict := inBase != inMain || baseHash != mainHash
		var ch change
		if conflict {
			res, ok := resolved[p]
			if !ok {
				unresolved = append(unresolved, p)
				continue
			}
			delete(resolved, p)
			ch = change{path: p, content: res.Content, deleted: res.Deleted}
		} else if hash, ok := files[p]; ok {
			data, err := r.blob(hash, int64(budget), nil)
			if err != nil {
				return store.LedgerChangeset{}, fmt.Errorf("ledger: read %q of changeset %s: %w", p, id, err)
			}
			budget -= len(data)
			ch = change{path: p, content: data}
		} else {
			ch = change{path: p, deleted: true}
		}
		switch {
		case ch.deleted && !inMain:
			continue
		case !ch.deleted && inMain && plumbing.ComputeHash(plumbing.BlobObject, ch.content) == mainHash:
			continue
		}
		final = append(final, ch)
	}
	for _, p := range slices.Sorted(maps.Keys(resolved)) {
		problems = append(problems, Problem{Path: p, Message: "is not a conflicting file; resolved holds only conflicting files"})
	}
	validation, temporary := validateChanges(mainRef, final)
	problems = append(problems, validation...)
	if err := violations(boundProblems(problems), unresolved); err != nil {
		return store.LedgerChangeset{}, err
	}

	next, err := m.st.GetLedgerNextNumber(ctx, c.ProjectID)
	if err != nil {
		return store.LedgerChangeset{}, err
	}
	for n := range mainRef.entries {
		next = max(next, n+1)
	}
	numbers, err := assignNumbers(temporary, next)
	if err != nil {
		return store.LedgerChangeset{}, err
	}
	prepared, err := mergedDecisions(ctx, mainRef, final, numbers)
	if err != nil {
		return store.LedgerChangeset{}, err
	}
	if unchanged(mainRef, prepared) {
		// As for a changeset without changes: the commit would change
		// nothing, as when every conflict is resolved to the main line's
		// version.
		return store.LedgerChangeset{}, &PreconditionError{Violations: []Violation{{Type: ViolationProblem, Subject: ".",
			Description: "nothing to merge: the main line already holds the changeset's final content; discard it"}}}
	}
	message, err := commitMessage(CommitOptions{Author: signatureName(o.Author), Message: "Merge changeset " + id, CodeCommit: o.CodeCommit})
	if err != nil {
		return store.LedgerChangeset{}, err
	}
	indexed, err := m.st.GetLedgerHead(ctx, c.ProjectID)
	if err != nil {
		return store.LedgerChangeset{}, err
	}
	merge := store.LedgerMerge{Parent: commitID(head), CodeCommit: o.CodeCommit, Numbers: numbers}
	published, err := m.publish(ctx, c.ProjectID, publication{r: r, parent: head, indexed: indexed, previous: mainRef.snapshot,
		prepared: prepared, message: message, options: CommitOptions{Author: signatureName(o.Author), CodeCommit: o.CodeCommit},
		beforePublish: func(commit plumbing.Hash) error {
			merge.Commit = commit.String()
			ok, err := m.st.PrepareLedgerMerge(ctx, id, merge, m.now().UnixMilli())
			if err == nil && !ok {
				err = fmt.Errorf("%w: changeset %s changed during its merge", store.ErrBusy, id)
			}
			return err
		}})
	if published == "" {
		if err != nil && merge.Commit != "" {
			// Main did not move: forget the merge, or leave it to a repair.
			_, _ = m.st.ClearLedgerMerge(context.WithoutCancel(ctx), id, merge.Commit)
		}
		return store.LedgerChangeset{}, err
	}
	if err != nil {
		// Main moved but its durability or the index is in doubt: readers
		// rebuild the index, and the merge stands.
		m.markStale(c.ProjectID, err)
		m.logger.WarnContext(ctx, "ledger merge published with an error", "project_id", c.ProjectID, "changeset_id", id, "error", err)
	}
	if _, err := m.st.FinishLedgerMerge(context.WithoutCancel(ctx), id, published, m.now().UnixMilli()); err != nil {
		return store.LedgerChangeset{}, err
	}
	return m.st.GetLedgerChangeset(context.WithoutCancel(ctx), id)
}

// mergedDecisions prepares the main line's decisions after a merge: the
// unchanged ones kept, the changed ones with their temporary numbers
// replaced and their paths derived again.
func mergedDecisions(ctx context.Context, mainRef reference, final []change, numbers map[string]uint32) ([]preparedDecision, error) {
	touched := make(map[string]bool, len(final))
	for _, ch := range final {
		touched[ch.path] = true
	}
	kept := make([]snapshotEntry, 0, len(mainRef.entries))
	for _, e := range mainRef.entries {
		if !touched[e.Path] {
			kept = append(kept, e)
		}
	}
	slices.SortFunc(kept, func(a, b snapshotEntry) int { return strings.Compare(a.Path, b.Path) })
	var decisions []Decision
	var v validation
	for _, ch := range final {
		if ch.deleted {
			continue
		}
		content := rewriteTemporary(ch.content, numbers)
		if len(content) > MaxDecisionFileBytes {
			v.add(ch.path, "decision file exceeds 256 KiB after its temporary numbers were replaced")
			continue
		}
		d, err := Parse(content)
		if err != nil {
			v.add(ch.path, "after its temporary numbers were replaced: "+err.Error())
			continue
		}
		d.Dir = fileDir(ch.path)
		decisions = append(decisions, d)
	}
	if len(v.problems) > 0 {
		return nil, violations(boundProblems(v.problems), nil)
	}
	prepared, _, err := prepareDecisions(ctx, kept, decisions)
	var invalid *ValidationError
	if errors.As(err, &invalid) {
		return nil, violations(boundProblems(invalid.Problems), nil)
	}
	return prepared, err
}

// unchanged reports whether the merged decisions are the main line's:
// the same numbers at the same paths with the same titles and bodies.
func unchanged(mainRef reference, prepared []preparedDecision) bool {
	if len(prepared) != len(mainRef.entries) {
		return false
	}
	for _, e := range prepared {
		old, ok := mainRef.entries[e.Number]
		if !ok || old.Path != e.Path || old.Title != e.Title {
			return false
		}
		if !e.kept && old.blob != e.blob && old.bodyHash != sha256.Sum256(e.body) {
			return false
		}
	}
	return true
}

// markStale makes the next read check the project and rebuild its index,
// without refusing readers meanwhile.
func (m *Manager) markStale(projectID string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.broken == nil {
		m.broken = make(map[string]*projectFault)
	}
	if m.broken[projectID] == nil {
		m.broken[projectID] = &projectFault{err: fmt.Errorf("ledger project %q: %w", projectID, err), delay: minProjectRetry, stale: true}
	}
}

// DiscardChangeset discards an open changeset with a reason and archives
// its branch; author names the discarding worker on the archive. A
// discarded changeset is returned as it is.
func (m *Manager) DiscardChangeset(ctx context.Context, id, reason, author string) (store.LedgerChangeset, error) {
	c, err := m.st.GetLedgerChangeset(ctx, id)
	if err != nil {
		return store.LedgerChangeset{}, err
	}
	l, err := m.lockProject(ctx, c.ProjectID)
	if err != nil {
		return store.LedgerChangeset{}, err
	}
	defer l.close()
	if err := l.usable(id); err != nil {
		return store.LedgerChangeset{}, err
	}
	if c, err = m.st.GetLedgerChangeset(ctx, id); err != nil {
		return store.LedgerChangeset{}, err
	}
	switch c.State {
	case store.LedgerChangesetMerged:
		return store.LedgerChangeset{}, stateError(c)
	case store.LedgerChangesetDiscarded:
		return c, nil
	}
	archived := l.r == nil
	ok, err := m.st.DiscardLedgerChangeset(ctx, id, reason, archived, m.now().UnixMilli())
	if err != nil {
		return store.LedgerChangeset{}, err
	}
	if !ok {
		return store.LedgerChangeset{}, fmt.Errorf("%w: changeset %s changed during its discard", store.ErrBusy, id)
	}
	if c, err = m.st.GetLedgerChangeset(ctx, id); err != nil {
		return store.LedgerChangeset{}, err
	}
	if !archived {
		// A failure leaves the archive to a repair; the discard stands.
		if err := m.archive(ctx, l.r, c, author); err != nil {
			m.logger.WarnContext(ctx, "ledger changeset archive failed", "project_id", c.ProjectID, "changeset_id", id, "error", err)
			return c, nil
		}
		c.Archived = true
	}
	return c, nil
}

// archive moves a discarded changeset's branch to refs/discarded/<id>,
// on a commit whose message holds the reason, and records it.
func (m *Manager) archive(ctx context.Context, r *repository, c store.LedgerChangeset, author string) error {
	branch, err := r.readRef(branchRef(c.ID))
	if err != nil {
		return err
	}
	target := branch
	if target.IsZero() {
		target = plumbing.NewHash(c.BaseCommit)
	}
	existing, err := r.readRef(discardedRef(c.ID))
	if err != nil {
		return err
	}
	if existing.IsZero() && !target.IsZero() {
		t, err := r.commit(target)
		if err != nil {
			return err
		}
		sig := object.Signature{Name: signatureName(author), Email: "ledger@circus.invalid", When: m.now().UTC().Truncate(time.Second)}
		commit := &object.Commit{Author: sig, Committer: sig, TreeHash: t.TreeHash, ParentHashes: []plumbing.Hash{target},
			Message: "Discard changeset " + c.ID + "\n\n" + strings.TrimRight(c.DiscardReason, "\n") + "\n"}
		o := r.storage.NewEncodedObject()
		if err := commit.Encode(o); err != nil {
			return err
		}
		hash, err := r.putObject(o)
		if err != nil {
			return err
		}
		if _, err := r.setRef(discardedRef(c.ID), plumbing.ZeroHash, hash); err != nil {
			return err
		}
	}
	if !branch.IsZero() {
		if _, err := r.setRef(branchRef(c.ID), branch, plumbing.ZeroHash); err != nil {
			return err
		}
	}
	return m.st.ArchiveLedgerChangeset(ctx, c.ID)
}

// repair finishes the project's interrupted merges and archives. The
// caller holds the project with main.lock, or the exclusive startup lock;
// r is nil when the project has no repository. A changeset whose repair
// fails, as when one of its objects is damaged, is logged and returned in
// failed: operations on it are refused, while the project's other
// changesets stay usable. An interrupted merge cannot break them: the
// main line stays the truth for later merges, and the merge is finished
// or forgotten by where its commit is once its repair succeeds.
func (m *Manager) repair(ctx context.Context, r *repository, projectID string) (failed map[string]error, _ error) {
	repairs, err := m.st.ListLedgerRepairs(ctx, projectID)
	if err != nil {
		return nil, err
	}
	for _, c := range repairs {
		var err error
		switch {
		case c.Pending():
			err = m.finishMerge(ctx, r, c)
		case !c.Archived && r == nil:
			err = m.st.ArchiveLedgerChangeset(ctx, c.ID)
		case !c.Archived:
			err = m.archive(ctx, r, c, repairAuthor)
		}
		if err == nil {
			continue
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		m.logger.ErrorContext(ctx, "ledger changeset repair failed", "project_id", projectID, "changeset_id", c.ID, "error", err)
		if failed == nil {
			failed = make(map[string]error)
		}
		failed[c.ID] = &RepairError{ID: c.ID, Err: err}
	}
	return failed, nil
}

// finishMerge records an interrupted merge as merged when its commit is
// on the main line, and forgets it otherwise.
func (m *Manager) finishMerge(ctx context.Context, r *repository, c store.LedgerChangeset) error {
	onMain := false
	if r != nil {
		head, err := r.head()
		if err != nil {
			return err
		}
		target, parent := plumbing.NewHash(c.Merge.Commit), plumbing.NewHash(c.Merge.Parent)
		for hash := head; !hash.IsZero() && hash != parent; {
			if hash == target {
				onMain = true
				break
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			commit, err := r.commit(hash)
			if err != nil {
				return err
			}
			if len(commit.ParentHashes) == 0 {
				break
			}
			hash = commit.ParentHashes[0]
		}
	}
	var err error
	if onMain {
		_, err = m.st.FinishLedgerMerge(ctx, c.ID, c.Merge.Commit, m.now().UnixMilli())
		m.logger.InfoContext(ctx, "finished interrupted ledger merge", "project_id", c.ProjectID, "changeset_id", c.ID, "commit", c.Merge.Commit)
	} else {
		_, err = m.st.ClearLedgerMerge(ctx, c.ID, c.Merge.Commit)
	}
	return err
}
