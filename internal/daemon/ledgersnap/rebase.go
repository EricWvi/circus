package ledgersnap

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/ledger/snapshot"
)

type rebaseEdit = circusv1.RebaseLedgerRequest_Edit

// fileState is what a path of a snapshot holds: nothing, or a file with
// the given content hash.
type fileState struct {
	exists bool
	sha256 string
}

func (s fileState) String() string {
	if !s.exists {
		return "absent"
	}
	return s.sha256[:12]
}

func manifestState(m *snapshot.Manifest, p string) fileState {
	if e := m.Lookup(p); e != nil {
		return fileState{true, e.SHA256}
	}
	return fileState{}
}

func targetState(e *rebaseEdit) fileState {
	if e.GetDeleted() {
		return fileState{}
	}
	return fileState{true, snapshot.Hash(e.GetContent())}
}

// rebasePlan is a rebase request against the snapshot's current base.
//
// The control plane merged exactly the changes in known, so before the
// rebase every path must hold its collected state: its known change, or
// for a path without one, its base content. The edits then take the
// collected view to the merged one. A path may also already hold its
// edit's target: that is what a rebase interrupted after some of its
// edits leaves, and what a finished rebase leaves once its manifest is
// written (the base then holds the targets, and the paths of known
// changes without an edit hold those changes). Accepting either state per
// path lets the same request finish or repeat, while any other state (a
// change the agent made after the collection, or anything a collection
// reports as a problem) means the directory is not what was merged, and
// making it the base would lose that change.
type rebasePlan struct {
	base  *snapshot.Manifest
	known map[string]*knownChange
	edits map[string]*rebaseEdit
}

// collected is the state p held in the collection the merge used.
func (p *rebasePlan) collected(path string) fileState {
	k := p.known[path]
	switch {
	case k == nil:
		return manifestState(p.base, path)
	case k.GetDeleted():
		return fileState{}
	}
	return fileState{true, k.GetSha256()}
}

// merged is the state p must hold once the edits are applied.
func (p *rebasePlan) merged(path string) fileState {
	if e := p.edits[path]; e != nil {
		return targetState(e)
	}
	return p.collected(path)
}

// checkCurrent compares the snapshot, as res found it against the base,
// with the collected view, accepting an edit's target on its path. It
// returns what differs.
func (p *rebasePlan) checkCurrent(res *snapshot.Result, bad *differences) {
	for _, pr := range res.Problems {
		bad.add("%s", problemText(pr))
	}
	if !res.Complete || res.TooLarge {
		// With HashesOnly, only too many changes set TooLarge, and that
		// is a problem listed above.
		bad.add("the snapshot could not be compared completely")
		return
	}
	current := make(map[string]fileState, len(res.Changes))
	for _, c := range res.Changes {
		if c.Deleted {
			current[c.Path] = fileState{}
		} else {
			current[c.Path] = fileState{true, c.SHA256}
		}
	}
	paths := slices.Concat(slices.Collect(maps.Keys(current)), slices.Collect(maps.Keys(p.known)), slices.Collect(maps.Keys(p.edits)))
	slices.Sort(paths)
	for _, path := range slices.Compact(paths) {
		have, ok := current[path]
		if !ok {
			have = manifestState(p.base, path)
		}
		if have == p.collected(path) {
			continue
		}
		if e := p.edits[path]; e != nil && have == targetState(e) {
			continue
		}
		bad.add("%s (%s, collected %s)", path, have, p.collected(path))
	}
}

// checkRebuilt compares the rebuilt base with the merged view: it catches
// a change the agent made while the rebase ran.
func (p *rebasePlan) checkRebuilt(m *snapshot.Manifest, problems []snapshot.Problem, bad *differences) {
	for _, pr := range problems {
		bad.add("%s", problemText(pr))
	}
	var paths []string
	for _, e := range p.base.Files {
		paths = append(paths, e.Path)
	}
	for _, e := range m.Files {
		paths = append(paths, e.Path)
	}
	paths = slices.Concat(paths, slices.Collect(maps.Keys(p.known)), slices.Collect(maps.Keys(p.edits)))
	slices.Sort(paths)
	for _, path := range slices.Compact(paths) {
		if have, want := manifestState(m, path), p.merged(path); have != want {
			bad.add("%s (%s, merged %s)", path, have, want)
		}
	}
}

func problemText(pr snapshot.Problem) string {
	if pr.Path == "" {
		return pr.Message
	}
	return pr.Path + ": " + pr.Message
}

// differences counts what a check found and keeps the first of them,
// as text bounded by maxDetailBytes while it is built.
type differences struct {
	n    int
	text strings.Builder
}

func (d *differences) add(format string, args ...any) {
	d.n++
	if d.text.Len() >= maxDetailBytes {
		return
	}
	if d.text.Len() > 0 {
		d.text.WriteString("; ")
	}
	fmt.Fprintf(&d.text, format, args...)
}

func (d *differences) String() string { return fmt.Sprintf("%d: %s", d.n, d.text.String()) }

// rebase also returns the base commit the manifest records when it ends,
// empty when there is no manifest.
func (s *Service) rebase(ctx context.Context, req *circusv1.RebaseLedgerRequest) (string, string, *failure) {
	current := ""
	if !snapshot.ValidCommit(req.BaseCommit) {
		return "", current, fail(errInvalidRequest, "base_commit must be a full lowercase hex commit id")
	}
	paths, contents := make([]string, len(req.Edits)), make([]string, len(req.Edits))
	edits := make(map[string]*rebaseEdit, len(req.Edits))
	for i, e := range req.Edits {
		paths[i], contents[i] = e.GetPath(), e.GetContent()
		if e.GetChange() == nil {
			return "", current, fail(errInvalidRequest, "edit of %q has neither content nor deleted", e.GetPath())
		}
		if e.ExpectedSha256 != "" && !snapshot.ValidHash(e.ExpectedSha256) {
			return "", current, fail(errInvalidRequest, "edit of %q: expected_sha256 must be a lowercase hex sha256", e.GetPath())
		}
		edits[e.GetPath()] = e
	}
	if f := checkFiles(paths, contents); f != nil {
		return "", current, f
	}
	if foldsNames {
		// A deletion makes no name: a case-only rename (delete, then
		// write) is fine.
		var written []string
		for _, e := range req.Edits {
			if !e.GetDeleted() {
				written = append(written, e.Path)
			}
		}
		if f := collision(written); f != nil {
			return "", current, f
		}
	}
	known, f := parseKnown(req.Known)
	if f != nil {
		return "", current, f
	}
	c, f := s.begin(ctx, req.SnapshotId, false)
	if f != nil {
		return "", current, f
	}
	if c == nil {
		return "", current, fail(errAbsent, "no ledger snapshots on this device")
	}
	defer c.end()
	dir, base, f := c.openSnapshot()
	if f != nil {
		return "", current, f
	}
	current = base.BaseCommit
	defer dir.Close()
	if f := c.recoverAside(dir); f != nil {
		return "", current, f
	}
	// Known changes and edits together name at most 8192 paths, so more
	// changes than that cannot match.
	res, err := snapshot.Compare(ctx, dir, base, snapshot.Limits{MaxChanges: snapshot.MaxEntries, HashesOnly: true})
	if err != nil {
		return "", current, fsFailure("compare the snapshot", err)
	}
	// A snapshot already on the requested base was rebased by an earlier
	// attempt whose answer was lost. Its edits are never applied again:
	// a change since (even one back to a file's collected content) is the
	// agent's.
	if base.BaseCommit == req.BaseCommit {
		if len(res.Changes) == 0 && len(res.Problems) == 0 && res.Complete {
			return c.path, current, nil
		}
		return "", current, fail(errStale, "the snapshot is already on %s and changed since; collect again. Nothing was changed", req.BaseCommit)
	}
	var alias aliases
	if foldsNames {
		if f := editCollision(base, res, req.Edits); f != nil {
			return "", current, f
		}
		alias = aliased(base, res, req.Edits)
	}
	plan := &rebasePlan{base: base, known: known, edits: edits}

	// Check everything before changing anything: the whole snapshot
	// against the collected view, then each edit against its expected
	// content.
	var bad differences
	if plan.checkCurrent(res, &bad); bad.n > 0 {
		return "", current, fail(errStale, "the snapshot differs from the collected changes; collect again. Nothing was changed. Differences %s", &bad)
	}
	var pending []*rebaseEdit
	for _, e := range req.Edits {
		have, err := alias.readState(dir, e.Path)
		if err != nil {
			return "", current, fsFailure("read "+e.Path, err)
		}
		expected := fileState{e.ExpectedSha256 != "", e.ExpectedSha256}
		switch {
		case have == targetState(e):
			// Applied by an earlier attempt.
		case have == expected:
			pending = append(pending, e)
		default:
			bad.add("%s (%s, expected %s)", e.Path, have, expected)
		}
	}
	if bad.n > 0 {
		return "", current, fail(errStale, "edits do not find what they expect; collect again. Nothing was changed. Differences %s", &bad)
	}

	pending = alias.order(pending)
	// The checks and the ordering may have outlasted the call's deadline:
	// then nothing is changed.
	if err := ctx.Err(); err != nil {
		return "", current, fsFailure("start the edits (nothing was changed)", err)
	}
	staging, err := c.openDir(stagingDir, true)
	if err != nil {
		return "", current, fsFailure("open the staging directory", err)
	}
	defer staging.Close()
	if changed, f := s.apply(c, dir, staging, pending, alias); f != nil || changed != "" {
		if f != nil {
			return "", current, f
		}
		return "", current, fail(errStale, "%s changed while the rebase ran; collect again. The edits before it are applied, the base is unchanged", changed)
	}
	if s.afterApply != nil {
		s.afterApply()
	}
	m, problems, err := snapshot.Rebuild(ctx, dir, snapshot.Clock{Dir: staging, Name: clockName(c.id)}, req.BaseCommit)
	if err != nil {
		return "", current, fsFailure("read the snapshot", err)
	}
	// The rebuilt base must be exactly the merged view: a file the agent
	// wrote while the rebase ran is not made part of it. The edits stay
	// applied and the old base stays, as after an interrupted rebase.
	if plan.checkRebuilt(m, problems, &bad); bad.n > 0 {
		return "", current, fail(errStale, "the snapshot changed while it was rebased; collect again. The edits are applied, the base is unchanged. Differences %s", &bad)
	}
	manifests, err := c.openDir(manifestsDir, true)
	if err != nil {
		return "", current, fsFailure("open the manifests directory", err)
	}
	defer manifests.Close()
	if err := snapshot.WriteManifest(manifests, c.manifestName(), m); err != nil {
		return "", current, fsFailure("write the manifest", err)
	}
	return c.path, req.BaseCommit, nil
}

// editCollision refuses an edit that writes a path whose name, or a
// directory on its way, a folding file system would take for another
// name already in the snapshot: there the file would land under the other
// name, and the rebuilt base would never match. Directories count as long
// as any file named them, as deletions leave directories in place; files
// an edit deletes do not count.
func editCollision(base *snapshot.Manifest, res *snapshot.Result, edits []*rebaseEdit) *failure {
	deleted := map[string]bool{}
	for _, e := range edits {
		if e.GetDeleted() {
			deleted[e.Path] = true
		}
	}
	current := map[string]bool{}
	for _, e := range base.Files {
		current[e.Path] = true
	}
	for _, c := range res.Changes {
		current[c.Path] = !c.Deleted
	}
	seen := map[string]string{}
	for p, exists := range current {
		for i := range len(p) {
			if p[i] == '/' {
				seen[foldKey(p[:i])] = p[:i]
			}
		}
		if exists && !deleted[p] {
			seen[foldKey(p)] = p
		}
	}
	for _, e := range edits {
		if e.GetDeleted() {
			continue
		}
		p := e.Path
		for i := 0; i <= len(p); i++ {
			if i < len(p) && p[i] != '/' {
				continue
			}
			if other, ok := seen[foldKey(p[:i])]; ok && other != p[:i] {
				return fail(errInvalidRequest, "edit path %q and %q in the snapshot are one name on this device's file system", p[:i], other)
			}
		}
	}
	return nil
}

// aliases are, where names fold, the fold keys (foldKey) that more than
// one spelling among the snapshot's files, the scan's changes and the
// edits share: a case-only rename (A.md to a.md) is the one such request
// editCollision lets through. A path of such a key may open the file of
// another spelling. It is nil where names do not fold.
type aliases map[string]bool

func aliased(base *snapshot.Manifest, res *snapshot.Result, edits []*rebaseEdit) aliases {
	spellings := map[string]string{}
	alias := aliases{}
	add := func(p string) {
		key := foldKey(p)
		if other, ok := spellings[key]; ok && other != p {
			alias[key] = true
		}
		spellings[key] = p
	}
	for _, e := range base.Files {
		add(e.Path)
	}
	for _, c := range res.Changes {
		add(c.Path)
	}
	for _, e := range edits {
		add(e.Path)
	}
	return alias
}

// readState reads what p holds now. A path with aliases holds a file only
// while its directory lists it under exactly its spelling, checked at the
// time of the read: otherwise what the file system reaches through it,
// whatever its size, is another spelling's file, and p is absent.
func (alias aliases) readState(dir *os.File, p string) (fileState, error) {
	if alias != nil && alias[foldKey(p)] {
		listed, err := snapshot.Listed(dir, p)
		if err != nil || !listed {
			return fileState{}, err
		}
	}
	st, err := snapshot.ReadState(dir, p)
	return fileState{st.Exists, st.SHA256}, err
}

// order moves the deletion of a path with aliases to just before the first
// write of another spelling of it, so that a case-only rename frees the old
// name before the new one is written; everything else keeps the request's
// order, so a failing edit stops the rebase before any edit after it. Each
// fold key is computed once and the deletions are indexed by it.
func (alias aliases) order(pending []*rebaseEdit) []*rebaseEdit {
	if len(alias) == 0 {
		return pending
	}
	keys := make([]string, len(pending))
	deletes := map[string][]int{}
	for i, e := range pending {
		keys[i] = foldKey(e.Path)
		if e.GetDeleted() && alias[keys[i]] {
			deletes[keys[i]] = append(deletes[keys[i]], i)
		}
	}
	ordered := make([]*rebaseEdit, 0, len(pending))
	placed := make([]bool, len(pending))
	for i, e := range pending {
		if placed[i] {
			continue
		}
		if !e.GetDeleted() && alias[keys[i]] {
			for _, d := range deletes[keys[i]] {
				if !placed[d] && pending[d].Path != e.Path {
					placed[d] = true
					ordered = append(ordered, pending[d])
				}
			}
			delete(deletes, keys[i])
		}
		placed[i] = true
		ordered = append(ordered, e)
	}
	return ordered
}

// apply applies the pending edits in order, each only if its file still
// holds the expected content right before (checked again, then enforced
// by snapshot.Applier, which never discards or replaces anything else).
// It stops at the first file the agent changed meanwhile and returns its
// path; that file stays as the agent left it, the edits before it stay
// applied and the old manifest stays. The next collection then reports the
// agent's write (and the applied edits, as changes against the old base);
// the same request sent again finds that path neither collected nor its
// target and is STALE, and a rebase built from the new collection
// finishes. Directories are synced once, after the renames.
func (s *Service) apply(c *call, dir, staging *os.File, pending []*rebaseEdit, alias aliases) (string, *failure) {
	ap := snapshot.NewApplier(dir, staging, tmpName(c.id), asideName(c.id))
	changed := ""
	var applyErr error
	for _, e := range pending {
		have, err := alias.readState(dir, e.Path)
		if err != nil {
			applyErr = fmt.Errorf("read %s: %w", e.Path, err)
			break
		}
		if have == targetState(e) {
			continue
		}
		if have != (fileState{e.ExpectedSha256 != "", e.ExpectedSha256}) {
			changed = e.Path
			break
		}
		if s.beforeEdit != nil {
			s.beforeEdit(e.Path)
		}
		if e.GetDeleted() {
			err = ap.Delete(e.Path, e.ExpectedSha256)
		} else {
			err = ap.Replace(e.Path, e.ExpectedSha256, e.GetContent())
		}
		if errors.Is(err, snapshot.ErrChanged) {
			changed = e.Path
			break
		}
		if err != nil {
			applyErr = fmt.Errorf("apply the edit of %s: %w", e.Path, err)
			break
		}
	}
	if err := errors.Join(applyErr, ap.Sync()); err != nil {
		return "", fsFailure("apply the edits", err)
	}
	return changed, nil
}
