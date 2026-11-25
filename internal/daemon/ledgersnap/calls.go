package ledgersnap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"slices"
	"unicode/utf8"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/daemon/workspace"
	"github.com/EricWvi/circus/internal/ledger/snapshot"
	"golang.org/x/sys/unix"
)

type (
	prepareResult = circusv1.PrepareLedgerResult
	collectResult = circusv1.CollectLedgerResult
	rebaseResult  = circusv1.RebaseLedgerResult
	removeResult  = circusv1.RemoveLedgerResult
)

func (s *Service) logFailure(name string, call *circusv1.DeviceCall, f *failure) {
	s.opts.Logger.Info("ledger snapshot call failed", "tool", name, "session_id", call.SessionId, "call_id", call.CallId,
		"error", f.code.String(), "detail", f.detail)
}

// foldsNames is true where the device's file system usually takes names
// differing in case or Unicode normalization for one (macOS). There
// requested paths that would collide are refused up front, naming them;
// elsewhere the last chunk's check that the file system holds exactly the
// requested paths (samePaths) catches a file system that folds anyway.
// Tests replace it.
var foldsNames = runtime.GOOS == "darwin"

// foldKey is the name a case-insensitive, normalization-insensitive file
// system (macOS by default) would see for p (snapshot.FoldKey).
func foldKey(p string) string { return snapshot.FoldKey(p) }

// collision finds two of paths, or of their parent directories, that are
// different names but one name to a case-insensitive or
// normalization-insensitive file system.
func collision(paths []string) *failure {
	seen := make(map[string]string, len(paths))
	for _, p := range paths {
		for i := 0; i <= len(p); i++ {
			if i < len(p) && p[i] != '/' {
				continue
			}
			prefix := p[:i]
			key := foldKey(prefix)
			if other, ok := seen[key]; ok && other != prefix {
				return fail(errInvalidRequest, "paths %q and %q are one name on a case-insensitive or normalizing file system", other, prefix)
			}
			seen[key] = prefix
		}
	}
	return nil
}

// checkFiles validates the files of a chunk or the edits of a rebase:
// valid, distinct paths and contents of valid UTF-8 within the bounds.
func checkFiles(paths []string, contents []string) *failure {
	if len(paths) > maxChunkFiles {
		return fail(errInvalidRequest, "more than %d files", maxChunkFiles)
	}
	seen := make(map[string]bool, len(paths))
	total := 0
	for i, p := range paths {
		if !snapshot.ValidPath(p) {
			return fail(errInvalidRequest, "invalid path %q", p)
		}
		if seen[p] {
			return fail(errInvalidRequest, "path %q appears twice", p)
		}
		seen[p] = true
		c := contents[i]
		if len(c) > snapshot.MaxBytes(p) || !utf8.ValidString(c) {
			return fail(errInvalidRequest, "%s: content must be valid UTF-8 of at most %d bytes", p, snapshot.MaxBytes(p))
		}
		if p == snapshot.IndexPath {
			// The generated index has its own bound.
			continue
		}
		if total += len(c); total > maxChunkBytes {
			return fail(errInvalidRequest, "more than %d bytes of content", maxChunkBytes)
		}
	}
	return nil
}

type knownChange = circusv1.CollectLedgerRequest_KnownChange

// parseKnown validates known changes and maps them by path: at most
// 4096, valid distinct paths, each with a lowercase hex SHA-256 or deleted.
func parseKnown(list []*knownChange) (map[string]*knownChange, *failure) {
	if len(list) > snapshot.MaxChanges {
		return nil, fail(errInvalidRequest, "more than %d known changes", snapshot.MaxChanges)
	}
	known := make(map[string]*knownChange, len(list))
	for _, k := range list {
		if !snapshot.ValidPath(k.Path) {
			return nil, fail(errInvalidRequest, "invalid known path %q", k.Path)
		}
		if known[k.Path] != nil {
			return nil, fail(errInvalidRequest, "known path %q appears twice", k.Path)
		}
		if !k.GetDeleted() && !snapshot.ValidHash(k.GetSha256()) {
			return nil, fail(errInvalidRequest, "known path %q needs a lowercase hex sha256 or deleted", k.Path)
		}
		known[k.Path] = k
	}
	return known, nil
}

// prepareTool is prepare_ledger.
type prepareTool struct{ s *Service }

func (*prepareTool) Name() string { return "prepare_ledger" }

func (t *prepareTool) Run(ctx context.Context, dc *circusv1.DeviceCall) (*circusv1.DeviceResult, error) {
	req := dc.GetPrepareLedger()
	if req == nil {
		return nil, errors.New("prepare_ledger request is required")
	}
	r, f := t.s.prepare(ctx, req)
	var text string
	switch {
	case f != nil:
		t.s.logFailure(t.Name(), dc, f)
		r = &prepareResult{Outcome: circusv1.PrepareLedgerResult_OUTCOME_FAILED, Error: f.code, ErrorDetail: f.detail}
		text = fmt.Sprintf("Couldn't prepare the ledger snapshot: %s", codeText(f.code))
	case r.Outcome == circusv1.PrepareLedgerResult_OUTCOME_READY:
		text = "Ledger snapshot ready at " + r.Path
	default:
		text = fmt.Sprintf("Staged chunk %d of the ledger snapshot", req.ChunkIndex)
	}
	return &circusv1.DeviceResult{Output: textResult(text, f != nil), Detail: &circusv1.DeviceResult_PrepareLedger{PrepareLedger: r}}, nil
}

func (s *Service) prepare(ctx context.Context, req *circusv1.PrepareLedgerRequest) (*prepareResult, *failure) {
	if !snapshot.ValidCommit(req.BaseCommit) {
		return nil, fail(errInvalidRequest, "base_commit must be a full lowercase hex commit id")
	}
	paths, contents := make([]string, len(req.Files)), make([]string, len(req.Files))
	for i, f := range req.Files {
		paths[i], contents[i] = f.GetPath(), f.GetContent()
	}
	if f := checkFiles(paths, contents); f != nil {
		return nil, f
	}
	if foldsNames {
		if f := collision(paths); f != nil {
			return nil, f
		}
	}
	if req.Last && req.TotalFiles > snapshot.MaxEntries {
		return nil, fail(errInvalidRequest, "more than %d files", snapshot.MaxEntries)
	}
	c, f := s.begin(ctx, req.SnapshotId, true)
	if f != nil {
		return nil, f
	}
	defer c.end()
	ready := &prepareResult{Outcome: circusv1.PrepareLedgerResult_OUTCOME_READY, Path: c.path}

	kind, err := c.snapshotKind()
	if err != nil {
		return nil, fsFailure("examine the snapshot", err)
	}
	if kind == snapshotOther && !req.Reset_ {
		return nil, fail(errPathConflict, "%s is a symlink or not a directory", c.path)
	}
	if kind == snapshotDir {
		m, err := c.readManifest()
		switch {
		case errors.Is(err, snapshot.ErrNoManifest):
			// Left by an interrupted preparation or removal: replaced.
		case err != nil:
			return nil, fsFailure("read the manifest", err)
		case m.BaseCommit == req.BaseCommit && !req.Reset_:
			if staging, err := c.openDir(stagingDir, false); err == nil && staging != nil {
				_, _ = removeStaging(ctx, staging, c.id, true)
				_ = staging.Close()
			}
			return ready, nil
		case !req.Reset_:
			return nil, fail(errInvalidRequest, "the snapshot holds base %s; reset replaces it", m.BaseCommit)
		}
	}

	staging, err := c.openDir(stagingDir, true)
	if err != nil {
		return nil, fsFailure("open the staging directory", err)
	}
	defer staging.Close()
	restart := fmt.Sprintf("chunk %d without the chunks before it; start again from chunk 0", req.ChunkIndex)
	if req.ChunkIndex == 0 {
		if _, err := removeStaging(ctx, staging, c.id, false); err != nil {
			return nil, fsFailure("remove earlier staging", err)
		}
		if err := unix.Mkdirat(int(staging.Fd()), c.id, 0o700); err != nil {
			return nil, fsFailure("create the staging directory", err)
		}
		if err := staging.Sync(); err != nil {
			return nil, fsFailure("sync the staging directory", err)
		}
	} else {
		st, err := readChunkState(staging, c.id)
		switch {
		case err != nil:
			return nil, fail(errInvalidRequest, "%s", restart)
		case st.BaseCommit != req.BaseCommit:
			return nil, fail(errInvalidRequest, "the staged chunks are of base %s", st.BaseCommit)
		case req.ChunkIndex != st.Chunk && req.ChunkIndex != st.Chunk+1:
			return nil, fail(errInvalidRequest, "expected chunk %d (or %d again), got %d", st.Chunk+1, st.Chunk, req.ChunkIndex)
		}
	}
	fd, err := unix.Openat(int(staging.Fd()), c.id, dirFlags, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil, fail(errInvalidRequest, "%s", restart)
	}
	if err != nil {
		return nil, fsFailure("open the staging directory", err)
	}
	dir := os.NewFile(uintptr(fd), c.id)
	defer dir.Close()
	w := snapshot.NewWriter(dir)
	w.Sync = true
	for i, p := range paths {
		if err := w.Add(p, contents[i]); err != nil {
			return nil, fsFailure("write "+p, err)
		}
	}
	if !req.Last {
		if err := writeChunkPaths(staging, c.id, req.ChunkIndex, paths); err != nil {
			return nil, fsFailure("record the chunk", err)
		}
		if err := writeChunkState(staging, c.id, chunkState{BaseCommit: req.BaseCommit, Chunk: req.ChunkIndex}); err != nil {
			return nil, fsFailure("record the chunk", err)
		}
		return &prepareResult{Outcome: circusv1.PrepareLedgerResult_OUTCOME_CHUNK_ACCEPTED}, nil
	}

	requested, f := c.requestedPaths(staging, req.ChunkIndex, paths, req.TotalFiles)
	if f != nil {
		return nil, f
	}
	m, problems, err := snapshot.Rebuild(ctx, dir, snapshot.Clock{Dir: staging, Name: clockName(c.id)}, req.BaseCommit)
	if err != nil {
		return nil, fsFailure("read the staged files", err)
	}
	if len(problems) > 0 {
		return nil, fsFailure("read the staged files", errors.New(problems[0].Path+": "+problems[0].Message))
	}
	// The file system may have merged names the checks above did not
	// know as one (another file system's rules): the base must hold
	// exactly the requested paths.
	if f := samePaths(m, requested); f != nil {
		return nil, f
	}
	if f := c.install(ctx, staging, m); f != nil {
		return nil, f
	}
	return ready, nil
}

// requestedPaths returns every path the chunks asked for, sorted, after
// checking that they are total distinct paths that do not collide and
// that the snapshot can hold them with their directories.
func (c *call) requestedPaths(staging *os.File, last uint32, lastPaths []string, total uint32) ([]string, *failure) {
	all := slices.Clone(lastPaths)
	for n := range last {
		paths, err := readChunkPaths(staging, c.id, n)
		if err != nil {
			return nil, fail(errInvalidRequest, "the paths of chunk %d are lost; start again from chunk 0", n)
		}
		all = append(all, paths...)
	}
	if uint32(len(all)) != total {
		return nil, fail(errInvalidRequest, "total_files is %d but the chunks sent %d files; start again from chunk 0", total, len(all))
	}
	slices.Sort(all)
	for i := 1; i < len(all); i++ {
		if all[i] == all[i-1] {
			return nil, fail(errInvalidRequest, "path %q is in two chunks; start again from chunk 0", all[i])
		}
	}
	if foldsNames {
		if f := collision(all); f != nil {
			return nil, f
		}
	}
	dirs := map[string]bool{}
	for _, p := range all {
		for i := range len(p) {
			if p[i] == '/' {
				dirs[p[:i]] = true
			}
		}
	}
	if n := len(all) + len(dirs); n > snapshot.MaxEntries {
		return nil, fail(errInvalidRequest, "%d files in %d directories are more than the %d entries a snapshot holds", len(all), len(dirs), snapshot.MaxEntries)
	}
	return all, nil
}

// samePaths refuses a rebuilt base whose paths are not exactly the
// requested ones (both sorted), naming one held and one missing.
func samePaths(m *snapshot.Manifest, requested []string) *failure {
	held := make([]string, len(m.Files))
	for i, e := range m.Files {
		held[i] = e.Path
	}
	if slices.Equal(held, requested) {
		return nil
	}
	extra, missing := "", ""
	for _, p := range held {
		if _, ok := slices.BinarySearch(requested, p); !ok {
			extra = p
			break
		}
	}
	for _, p := range requested {
		if _, ok := slices.BinarySearch(held, p); !ok {
			missing = p
			break
		}
	}
	return fail(errInvalidRequest, "the device's file system holds %q but not the requested %q (%d files held, %d requested); start again from chunk 0",
		extra, missing, len(held), len(requested))
}

// install puts the staged directory in place of the snapshot. The old
// manifest goes first, whether or not a directory is there (one removed
// from outside leaves its manifest), and the chunk state with it, so a
// re-sent last chunk starts again; the new manifest is written last. A
// crash in between leaves a directory without a manifest, which is no
// snapshot.
func (c *call) install(ctx context.Context, staging *os.File, m *snapshot.Manifest) *failure {
	manifests, err := c.openDir(manifestsDir, true)
	if err != nil {
		return fsFailure("open the manifests directory", err)
	}
	defer manifests.Close()
	rootFd, stagingFd := int(c.root.Fd()), int(staging.Fd())
	if _, err := snapshot.RemoveManifest(manifests, c.manifestName()); err != nil {
		return fsFailure("remove the old manifest", err)
	}
	if err := unix.Unlinkat(stagingFd, stateName(c.id), 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return fsFailure("remove the chunk state", err)
	}
	if err := unix.Renameat(rootFd, c.id, stagingFd, c.id+".old"); err != nil && !errors.Is(err, unix.ENOENT) {
		return fsFailure("move the old snapshot aside", err)
	}
	if err := unix.Renameat(stagingFd, c.id, rootFd, c.id); err != nil {
		return fsFailure("move the snapshot into place", err)
	}
	if err := errors.Join(c.root.Sync(), staging.Sync()); err != nil {
		return fsFailure("sync the ledger directory", err)
	}
	if err := snapshot.WriteManifest(manifests, c.manifestName(), m); err != nil {
		return fsFailure("write the manifest", err)
	}
	if _, err := removeStaging(ctx, staging, c.id, false); err != nil {
		c.s.opts.Logger.Warn("ledger staging removal failed", "snapshot_id", c.id, "error", err)
	}
	return nil
}

// collectTool is collect_ledger.
type collectTool struct{ s *Service }

func (*collectTool) Name() string { return "collect_ledger" }

func (t *collectTool) Run(ctx context.Context, dc *circusv1.DeviceCall) (*circusv1.DeviceResult, error) {
	req := dc.GetCollectLedger()
	if req == nil {
		return nil, errors.New("collect_ledger request is required")
	}
	r, path, base, f := t.s.collect(ctx, req)
	var text string
	if f != nil {
		t.s.logFailure(t.Name(), dc, f)
		r = &collectResult{Outcome: circusv1.CollectLedgerResult_OUTCOME_FAILED, Error: f.code, ErrorDetail: f.detail}
	}
	r.CurrentBaseCommit = base
	if f != nil {
		text = fmt.Sprintf("Couldn't collect the ledger snapshot: %s", codeText(f.code))
	} else {
		text = fmt.Sprintf("Collected %d changes, %d reverted and %d problems from %s", len(r.Changes), len(r.Reverted), len(r.Problems), path)
		if r.TooLarge {
			text += " (too large to use)"
		}
	}
	return &circusv1.DeviceResult{Output: textResult(text, f != nil), Detail: &circusv1.DeviceResult_CollectLedger{CollectLedger: r}}, nil
}

// collect also returns the base commit the manifest records, empty when
// there is no manifest.
func (s *Service) collect(ctx context.Context, req *circusv1.CollectLedgerRequest) (*collectResult, string, string, *failure) {
	base := ""
	known, f := parseKnown(req.Known)
	if f != nil {
		return nil, "", base, f
	}
	c, f := s.begin(ctx, req.SnapshotId, false)
	if f != nil {
		return nil, "", base, f
	}
	if c == nil {
		return nil, "", base, fail(errAbsent, "no ledger snapshots on this device")
	}
	defer c.end()
	dir, m, f := c.openSnapshot()
	if f != nil {
		return nil, "", base, f
	}
	base = m.BaseCommit
	defer dir.Close()
	if f := c.recoverAside(dir); f != nil {
		return nil, "", base, f
	}
	res, err := snapshot.Compare(ctx, dir, m, snapshot.Limits{})
	if err != nil {
		return nil, "", base, fsFailure("compare the snapshot", err)
	}
	out := &collectResult{Outcome: circusv1.CollectLedgerResult_OUTCOME_COLLECTED, TooLarge: res.TooLarge}
	changed := make(map[string]bool, len(res.Changes))
	for _, ch := range res.Changes {
		changed[ch.Path] = true
		k := known[ch.Path]
		switch {
		case ch.Deleted && k != nil && k.GetDeleted():
			out.Changes = append(out.Changes, &circusv1.CollectLedgerResult_Change{Path: ch.Path, Change: &circusv1.CollectLedgerResult_Change_Known{Known: true}})
		case ch.Deleted:
			out.Changes = append(out.Changes, &circusv1.CollectLedgerResult_Change{Path: ch.Path, Change: &circusv1.CollectLedgerResult_Change_Deleted{Deleted: true}})
		case res.TooLarge || k != nil && !k.GetDeleted() && k.GetSha256() == ch.SHA256:
			out.Changes = append(out.Changes, &circusv1.CollectLedgerResult_Change{Path: ch.Path, Change: &circusv1.CollectLedgerResult_Change_Known{Known: true},
				Sha256: ch.SHA256, Size: uint64(ch.Size)})
		default:
			out.Changes = append(out.Changes, &circusv1.CollectLedgerResult_Change{Path: ch.Path, Change: &circusv1.CollectLedgerResult_Change_Content{Content: ch.Content},
				Sha256: ch.SHA256, Size: uint64(ch.Size)})
		}
	}
	if res.Complete && !(res.TooLarge && len(res.Changes) == 0) {
		for _, k := range req.Known {
			if !changed[k.Path] && !res.Unknown(k.Path) {
				out.Reverted = append(out.Reverted, k.Path)
			}
		}
	}
	for _, p := range res.Problems {
		if len(out.Problems) == maxProblems {
			break
		}
		out.Problems = append(out.Problems, &circusv1.CollectLedgerResult_Problem{Path: p.Path, Message: clip(p.Message)})
	}
	return out, c.path, base, nil
}

// rebaseTool is rebase_ledger.
type rebaseTool struct{ s *Service }

func (*rebaseTool) Name() string { return "rebase_ledger" }

func (t *rebaseTool) Run(ctx context.Context, dc *circusv1.DeviceCall) (*circusv1.DeviceResult, error) {
	req := dc.GetRebaseLedger()
	if req == nil {
		return nil, errors.New("rebase_ledger request is required")
	}
	path, base, f := t.s.rebase(ctx, req)
	r := &rebaseResult{Outcome: circusv1.RebaseLedgerResult_OUTCOME_REBASED, CurrentBaseCommit: base}
	text := fmt.Sprintf("Rebased the ledger snapshot at %s on %s", path, req.BaseCommit)
	if f != nil {
		t.s.logFailure(t.Name(), dc, f)
		r = &rebaseResult{Outcome: circusv1.RebaseLedgerResult_OUTCOME_FAILED, Error: f.code, ErrorDetail: f.detail, CurrentBaseCommit: base}
		text = fmt.Sprintf("Couldn't rebase the ledger snapshot: %s", codeText(f.code))
	}
	return &circusv1.DeviceResult{Output: textResult(text, f != nil), Detail: &circusv1.DeviceResult_RebaseLedger{RebaseLedger: r}}, nil
}

// removeTool is remove_ledger.
type removeTool struct{ s *Service }

func (*removeTool) Name() string { return "remove_ledger" }

func (t *removeTool) Run(ctx context.Context, dc *circusv1.DeviceCall) (*circusv1.DeviceResult, error) {
	req := dc.GetRemoveLedger()
	if req == nil {
		return nil, errors.New("remove_ledger request is required")
	}
	removed, path, f := t.s.remove(ctx, req)
	var r *removeResult
	var text string
	switch {
	case f != nil:
		t.s.logFailure(t.Name(), dc, f)
		r = &removeResult{Outcome: circusv1.RemoveLedgerResult_OUTCOME_FAILED, Error: f.code, ErrorDetail: f.detail}
		text = fmt.Sprintf("Couldn't remove the ledger snapshot: %s", codeText(f.code))
	case removed:
		r = &removeResult{Outcome: circusv1.RemoveLedgerResult_OUTCOME_REMOVED}
		text = "Removed the ledger snapshot at " + path
	default:
		r = &removeResult{Outcome: circusv1.RemoveLedgerResult_OUTCOME_ABSENT}
		text = "No ledger snapshot to remove"
		if path != "" {
			text += " at " + path
		}
	}
	return &circusv1.DeviceResult{Output: textResult(text, f != nil), Detail: &circusv1.DeviceResult_RemoveLedger{RemoveLedger: r}}, nil
}

func (s *Service) remove(ctx context.Context, req *circusv1.RemoveLedgerRequest) (bool, string, *failure) {
	c, f := s.begin(ctx, req.SnapshotId, false)
	if f != nil || c == nil {
		return false, "", f
	}
	defer c.end()
	kind, err := c.snapshotKind()
	if err != nil {
		return false, c.path, fsFailure("examine the snapshot", err)
	}
	if kind == snapshotOther {
		return false, c.path, fail(errPathConflict, "%s is a symlink or not a directory; nothing was removed", c.path)
	}
	removed := false
	// The manifest goes first: a directory left without one is no snapshot.
	manifests, err := c.openDir(manifestsDir, false)
	if err != nil {
		return false, c.path, fsFailure("open the manifests directory", err)
	}
	if manifests != nil {
		removed, err = snapshot.RemoveManifest(manifests, c.manifestName())
		_ = manifests.Close()
		if err != nil {
			return false, c.path, fsFailure("remove the manifest", err)
		}
	}
	if kind == snapshotDir {
		if err := workspace.RemoveTree(ctx, int(c.root.Fd()), c.id); err != nil {
			if workspace.IsNotRemovable(err) {
				return false, c.path, fail(errPathConflict, "%s is a symlink or not a directory", c.path)
			}
			return false, c.path, fsFailure("remove the snapshot", err)
		}
		removed = true
	}
	staging, err := c.openDir(stagingDir, false)
	if err != nil {
		return false, c.path, fsFailure("open the staging directory", err)
	}
	if staging != nil {
		stagedRemoved, err := removeStaging(ctx, staging, c.id, false)
		_ = staging.Close()
		if err != nil {
			return false, c.path, fsFailure("remove the staging", err)
		}
		removed = removed || stagedRemoved
	}
	if err := c.root.Sync(); err != nil {
		return false, c.path, fsFailure("sync the ledger directory", err)
	}
	return removed, c.path, nil
}
