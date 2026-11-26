package ledgersnap_test

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/daemon/ledgersnap"
	"github.com/EricWvi/circus/internal/ledger/snapshot"
	"github.com/EricWvi/circus/internal/testutil/diskimage"
)

const invalid = circusv1.LedgerSnapshotError_LEDGER_SNAPSHOT_ERROR_INVALID_REQUEST

// TestRebaseAgentWritesDuringApply: whatever the agent does to an edit's
// file between the last check and the edit (rewrite, delete, create), the
// edit is not applied, the agent's state stays, the rebase is STALE and
// the next collection reports it.
func TestRebaseAgentWritesDuringApply(t *testing.T) {
	for _, tc := range []struct {
		name, path string
		agent      func(t *testing.T, file string)
		collect    string
	}{
		{"rewrites a file to delete", "D?a-new.md", func(t *testing.T, file string) { write(t, file, "new, rewritten") }, "+D?a-new.md:new, rewritten"},
		{"rewrites a file to replace", "D1-x.md", func(t *testing.T, file string) { write(t, file, "one, rewritten") }, "+D1-x.md:one, rewritten"},
		{"deletes a file to replace", "D1-x.md", func(t *testing.T, file string) {
			if err := os.Remove(file); err != nil {
				t.Fatal(err)
			}
		}, "-D1-x.md"},
		{"creates a file to create", "D92-new.md", func(t *testing.T, file string) { write(t, file, "the agent's own") }, "+D92-new.md:the agent's own"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, path, known, edits := renaming(t)
			settle(t, path)
			ledgersnap.SetBeforeEdit(f.svc, func(p string) {
				if p == tc.path {
					tc.agent(t, filepath.Join(path, p))
				}
			})
			r := f.rebase(known, edits)
			ledgersnap.SetBeforeEdit(f.svc, nil)
			if r.Error != stale || !strings.Contains(r.ErrorDetail, tc.path+" changed while the rebase ran") {
				t.Fatalf("rebase: %v", r)
			}
			if m, err := snapshot.ReadManifestFile(f.manifestPath()); err != nil || m.BaseCommit != base1 {
				t.Fatalf("the base moved: %+v %v", m, err)
			}
			if got := changes(f.collect()); !strings.Contains(got, tc.collect) {
				t.Fatalf("collect: %s", got)
			}
			if got := entries(t, filepath.Join(f.ledger, ".staging")); len(got) != 0 {
				t.Fatalf("staging: %v", got)
			}
			// It stopped at the changed file: no later edit was applied.
			if tc.path == "D?a-new.md" {
				if _, err := os.Stat(filepath.Join(path, "D92-new.md")); !os.IsNotExist(err) || read(t, filepath.Join(path, "D1-x.md")) != "one, see D?a" {
					t.Fatalf("edits after the changed file were applied: %v", entries(t, path))
				}
			}
			// The same request again: the agent's state is neither
			// collected nor a target.
			if r := f.rebase(known, edits); r.Error != stale {
				t.Fatalf("re-sent: %v", r)
			}
		})
	}
}

// TestRebaseAfterStaleApplyFinishes: once the agent's write that stopped
// a rebase midway is collected and merged, a rebase on that collection
// finishes and keeps it.
func TestRebaseAfterStaleApplyFinishes(t *testing.T) {
	f, path, known, edits := renaming(t)
	settle(t, path)
	ledgersnap.SetBeforeEdit(f.svc, func(p string) {
		if p == "D1-x.md" {
			write(t, filepath.Join(path, p), "one, agent")
		}
	})
	r := f.rebase(known, edits)
	ledgersnap.SetBeforeEdit(f.svc, nil)
	if r.Error != stale {
		t.Fatalf("rebase: %v", r)
	}
	// The edits before it were applied: they show as changes.
	if got := changes(f.collect()); got != "+D1-x.md:one, agent +D92-new.md:new, see D1" {
		t.Fatalf("collect: %s", got)
	}
	known = []*circusv1.CollectLedgerRequest_KnownChange{knownSha("D1-x.md", "one, agent"), knownSha("D92-new.md", "new, see D1")}
	if r := f.rebase(known, nil); r.Outcome != circusv1.RebaseLedgerResult_OUTCOME_REBASED {
		t.Fatalf("rebase on the new collection: %v", r)
	}
	if read(t, filepath.Join(path, "D1-x.md")) != "one, agent" || changes(f.collect()) != "" {
		t.Fatal("after the rebase")
	}
}

// TestRebaseApplyWindow: an agent writing a file while many edits are
// being applied never loses that write: either the rebase stops at it, or
// the rebase finished first and the write is a change against the new base.
func TestRebaseApplyWindow(t *testing.T) {
	f := newFixture(t, ledgersnap.Options{})
	path := f.ready(base1, "D1-x.md", "one")
	settle(t, path)
	write(t, filepath.Join(path, "D?a-new.md"), "new v1")
	known := []*circusv1.CollectLedgerRequest_KnownChange{knownSha("D?a-new.md", "new v1")}
	var edits []*circusv1.RebaseLedgerRequest_Edit
	for i := range 3000 {
		edits = append(edits, rebaseEdit(fmt.Sprintf("gen/x%04d.md", i), "", ptr("g")))
	}
	edits = append(edits, rebaseEdit("D?a-new.md", "new v1", nil), rebaseEdit("D92-new.md", "", ptr("new v1")))
	const agent = "new v2 written by the agent during the rebase"
	done := make(chan struct{})
	go func() {
		defer close(done)
		first := filepath.Join(path, edits[0].Path)
		for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); {
			if _, err := os.Stat(first); err == nil {
				write(t, filepath.Join(path, "D?a-new.md"), agent)
				return
			}
		}
		t.Error("the first edit never appeared")
	}()
	r := f.rebase(known, edits)
	<-done
	if got := read(t, filepath.Join(path, "D?a-new.md")); got != agent {
		t.Fatalf("the agent's write is gone (rebase %v): %q", r, got)
	}
	if got := changes(f.collect()); !strings.Contains(got, "+D?a-new.md:"+agent) {
		t.Fatalf("the agent's write is not collected (rebase %v): %.200s", r, got)
	}
}

// TestRebaseOnItsOwnBaseIsDone: a rebase whose base the snapshot already
// has never applies its edits again: with no change since it is REBASED,
// otherwise STALE, even for a change back to a file's collected content.
func TestRebaseOnItsOwnBaseIsDone(t *testing.T) {
	f, path, known, edits := renaming(t)
	if r := f.rebase(known, edits); r.Outcome != circusv1.RebaseLedgerResult_OUTCOME_REBASED {
		t.Fatal(r)
	}
	if r := f.rebase(known, edits); r.Outcome != circusv1.RebaseLedgerResult_OUTCOME_REBASED {
		t.Fatalf("again without changes: %v", r)
	}
	settle(t, path)
	write(t, filepath.Join(path, "D1-x.md"), "one, see D?a")
	if r := f.rebase(known, edits); r.Error != stale || !strings.Contains(r.ErrorDetail, "already on") {
		t.Fatalf("again after a change: %v", r)
	}
	if read(t, filepath.Join(path, "D1-x.md")) != "one, see D?a" || changes(f.collect()) != "+D1-x.md:one, see D?a" {
		t.Fatal("the agent's change was overwritten")
	}
}

// TestPrepareRemovesOrphanedManifest: a manifest whose directory was
// removed from outside goes before anything is installed, so a new
// directory is never described by it, even when the new manifest cannot
// be written; the sweep removes such manifests too.
func TestPrepareRemovesOrphanedManifest(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into read-only directories")
	}
	f := newFixture(t, ledgersnap.Options{})
	path := f.ready(base1, "A.md", "one")
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	manifests := filepath.Join(f.ledger, ".manifests")
	if err := os.Chmod(manifests, 0o500); err != nil {
		t.Fatal(err)
	}
	r := f.prepare(&circusv1.PrepareLedgerRequest{BaseCommit: base2, Reset_: true, Files: files("B.md", "two"), Last: true, TotalFiles: 1})
	if err := os.Chmod(manifests, 0o700); err != nil {
		t.Fatal(err)
	}
	if r.Error != circusv1.LedgerSnapshotError_LEDGER_SNAPSHOT_ERROR_PERMISSION_DENIED || !strings.Contains(r.ErrorDetail, "remove the old manifest") {
		t.Fatalf("prepare: %v", r)
	}
	if c := f.collect(); c.Error != circusv1.LedgerSnapshotError_LEDGER_SNAPSHOT_ERROR_ABSENT {
		t.Fatalf("collect after the failed prepare: %v", c)
	}
	// Another id's call removes the orphaned manifest.
	f.run(&circusv1.CollectLedgerRequest{SnapshotId: "other"})
	if got := entries(t, manifests); len(got) != 0 {
		t.Fatalf("manifests after the sweep: %v", got)
	}
	if f.ready(base2, "B.md", "two") != path || changes(f.collect()) != "" {
		t.Fatal("prepare after the sweep")
	}
}

// TestResendLastChunkAfterSwap: a last chunk that failed after the swap
// (here at the manifest) sent again starts the preparation over.
func TestResendLastChunkAfterSwap(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into read-only directories")
	}
	f := newFixture(t, ledgersnap.Options{})
	if r := f.prepare(&circusv1.PrepareLedgerRequest{Files: files("A.md", "one")}); r.Outcome != circusv1.PrepareLedgerResult_OUTCOME_CHUNK_ACCEPTED {
		t.Fatal(r)
	}
	manifests := filepath.Join(f.ledger, ".manifests")
	if err := os.MkdirAll(manifests, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(manifests, 0o500); err != nil {
		t.Fatal(err)
	}
	last := func() *circusv1.PrepareLedgerResult {
		return f.prepare(&circusv1.PrepareLedgerRequest{ChunkIndex: 1, Files: files("B.md", "two"), Last: true, TotalFiles: 2})
	}
	r := last()
	if err := os.Chmod(manifests, 0o700); err != nil {
		t.Fatal(err)
	}
	if r.Error != circusv1.LedgerSnapshotError_LEDGER_SNAPSHOT_ERROR_PERMISSION_DENIED || !strings.Contains(r.ErrorDetail, "write the manifest") {
		t.Fatalf("last chunk: %v", r)
	}
	// The chunk state went before the swap.
	if got := entries(t, filepath.Join(f.ledger, ".staging")); slices.Contains(got, id+".state") {
		t.Fatalf("staging after the swap: %v", got)
	}
	if r := last(); r.Error != invalid || !strings.Contains(r.ErrorDetail, "start again from chunk 0") {
		t.Fatalf("re-sent last chunk: %v", r)
	}
	if r := f.prepare(&circusv1.PrepareLedgerRequest{Files: files("A.md", "one")}); r.Outcome != circusv1.PrepareLedgerResult_OUTCOME_CHUNK_ACCEPTED {
		t.Fatal(r)
	}
	if r := last(); r.Outcome != circusv1.PrepareLedgerResult_OUTCOME_READY || changes(f.collect()) != "" {
		t.Fatalf("started over: %v", r)
	}
}

// TestPathCollisions: names a case-insensitive or normalizing file system
// would merge are refused, within a chunk, across chunks and among a
// rebase's edits; and the base holds exactly the requested paths.
func TestPathCollisions(t *testing.T) {
	ledgersnap.SetFoldsNames(t, true)
	f := newFixture(t, ledgersnap.Options{})
	for _, kv := range [][]string{
		{"Arch/D1-x.md", "1", "arch/D2-y.md", "2"},
		{"café.md", "1", "café.md", "2"},
		{"D1.md", "1", "d1.md/x.md", "2"},
	} {
		r := f.prepare(&circusv1.PrepareLedgerRequest{Files: files(kv...), Last: true, TotalFiles: 2})
		if r.Error != invalid || !strings.Contains(r.ErrorDetail, "one name") {
			t.Errorf("%q: %v", kv, r)
		}
	}
	if r := f.prepare(&circusv1.PrepareLedgerRequest{Files: files("A.md", "1")}); r.Outcome != circusv1.PrepareLedgerResult_OUTCOME_CHUNK_ACCEPTED {
		t.Fatal(r)
	}
	if r := f.prepare(&circusv1.PrepareLedgerRequest{ChunkIndex: 1, Files: files("a.md", "2"), Last: true, TotalFiles: 2}); r.Error != invalid || !strings.Contains(r.ErrorDetail, `"A.md" and "a.md"`) {
		t.Fatalf("across chunks: %v", r)
	}
	// The file system held another name than requested (as a folding
	// file system would): refused.
	if r := f.prepare(&circusv1.PrepareLedgerRequest{Files: files("A.md", "1")}); r.Outcome != circusv1.PrepareLedgerResult_OUTCOME_CHUNK_ACCEPTED {
		t.Fatal(r)
	}
	staged := filepath.Join(f.ledger, ".staging", id)
	if err := os.Rename(filepath.Join(staged, "A.md"), filepath.Join(staged, "a.md")); err != nil {
		t.Fatal(err)
	}
	if r := f.prepare(&circusv1.PrepareLedgerRequest{ChunkIndex: 1, Files: files("B.md", "2"), Last: true, TotalFiles: 2}); r.Error != invalid || !strings.Contains(r.ErrorDetail, `holds "a.md"`) {
		t.Fatalf("merged by the file system: %v", r)
	}
	f.ready(base1, "a.md", "1")
	r := f.rebase(nil, []*circusv1.RebaseLedgerRequest_Edit{rebaseEdit("X.md", "", ptr("x")), rebaseEdit("x.md", "", ptr("y"))})
	if r.Error != invalid || !strings.Contains(r.ErrorDetail, "one name") {
		t.Fatalf("rebase: %v", r)
	}
	// An edit against a name already in the snapshot: refused, unless the
	// edit deletes that file.
	pr := f.prepare(&circusv1.PrepareLedgerRequest{BaseCommit: base2, Reset_: true, Files: files("docs/D1.md", "1", "A.md", "a"), Last: true, TotalFiles: 2})
	if pr.Outcome != circusv1.PrepareLedgerResult_OUTCOME_READY {
		t.Fatal(pr)
	}
	path := pr.Path
	r = f.run(&circusv1.RebaseLedgerRequest{SnapshotId: id, BaseCommit: strings.Repeat("4", 40),
		Edits: []*circusv1.RebaseLedgerRequest_Edit{rebaseEdit("Docs/D92.md", "", ptr("x"))}}).GetRebaseLedger()
	if r.Error != invalid || !strings.Contains(r.ErrorDetail, `"Docs" and "docs"`) {
		t.Fatalf("rebase against an existing name: %v", r)
	}
	settle(t, path)
	write(t, filepath.Join(path, "notes.md"), "n")
	known := []*circusv1.CollectLedgerRequest_KnownChange{knownSha("notes.md", "n")}
	r = f.run(&circusv1.RebaseLedgerRequest{SnapshotId: id, BaseCommit: strings.Repeat("3", 40), Known: known,
		Edits: []*circusv1.RebaseLedgerRequest_Edit{rebaseEdit("A.md", "a", nil), rebaseEdit("a.md", "", ptr("a"))}}).GetRebaseLedger()
	if r.Outcome != circusv1.RebaseLedgerResult_OUTCOME_REBASED {
		t.Fatalf("case-only rename: %v", r)
	}
}

// TestPathCollisionsWhereNamesDoNotFold: where the file system keeps
// names apart, names that differ only in case or normalization are
// prepared like any others.
func TestPathCollisionsWhereNamesDoNotFold(t *testing.T) {
	ledgersnap.SetFoldsNames(t, false)
	opts := ledgersnap.Options{}
	if runtime.GOOS == "darwin" {
		// macOS's own volume folds names: use one that keeps them apart.
		volume := filepath.Join(t.TempDir(), "volume")
		diskimage.Mount(t, "Case-sensitive HFS+", "16m", volume)
		opts.Root = filepath.Join(volume, "ledger")
	}
	f := newFixture(t, opts)
	path := f.ready(base1, "Docs/D1.md", "1", "docs/D2.md", "2", "Stra\u00dfe.md", "3", "Strasse.md", "4")
	if got := entries(t, path); !slices.Equal(got, []string{"Docs", "Strasse.md", "Stra\u00dfe.md", "docs"}) {
		t.Fatalf("snapshot: %v", got)
	}
}

// TestPrepareEntryBounds: 8192 files fit; 8193 are refused up front, and
// files whose directories take the snapshot past 8192 entries are refused
// at the last chunk, as INVALID_REQUEST.
func TestPrepareEntryBounds(t *testing.T) {
	f := newFixture(t, ledgersnap.Options{})
	chunks := func(sid string, name func(int) string, n int) *circusv1.PrepareLedgerResult {
		var r *circusv1.PrepareLedgerResult
		for c := 0; c*4096 < n; c++ {
			var kv []string
			for i := c * 4096; i < min(n, (c+1)*4096); i++ {
				kv = append(kv, name(i), "")
			}
			r = f.prepare(&circusv1.PrepareLedgerRequest{SnapshotId: sid, ChunkIndex: uint32(c), Files: files(kv...), Last: (c+1)*4096 >= n, TotalFiles: uint32(n)})
		}
		return r
	}
	if r := chunks("flat", func(i int) string { return fmt.Sprintf("f%05d.md", i) }, 8192); r.Outcome != circusv1.PrepareLedgerResult_OUTCOME_READY {
		t.Fatalf("8192 files: %v", r)
	}
	if r := f.prepare(&circusv1.PrepareLedgerRequest{SnapshotId: "over", Files: files("a.md", ""), Last: true, TotalFiles: 8193}); r.Error != invalid {
		t.Fatalf("8193 files: %v", r)
	}
	r := chunks("dirs", func(i int) string { return fmt.Sprintf("d%d/f%05d.md", i%2, i) }, 8192)
	if r.Error != invalid || !strings.Contains(r.ErrorDetail, "8192 files in 2 directories") {
		t.Fatalf("8192 files in 2 directories: %v", r)
	}
}

// TestStaleDetailClipped: a STALE answer listing many differences stays
// within 1024 bytes and still counts them all.
func TestStaleDetailClipped(t *testing.T) {
	f := newFixture(t, ledgersnap.Options{})
	f.ready(base1, "a.md", "a")
	var edits []*circusv1.RebaseLedgerRequest_Edit
	for i := range 300 {
		edits = append(edits, rebaseEdit(fmt.Sprintf("%s%03d.md", strings.Repeat("long-name-", 10), i), "missing", ptr("x")))
	}
	r := f.rebase(nil, edits)
	if r.Error != stale || len(r.ErrorDetail) > 1024 || !strings.Contains(r.ErrorDetail, "Differences 300: ") {
		t.Fatalf("detail of %d bytes: %.120s", len(r.ErrorDetail), r.ErrorDetail)
	}
}

// TestRecoverAside: a file a killed rebase left aside goes back to its
// path before a collection, so an agent's write it held is collected.
func TestRecoverAside(t *testing.T) {
	f := newFixture(t, ledgersnap.Options{})
	path := f.ready(base1, "a.md", "a")
	staging := filepath.Join(f.ledger, ".staging")
	if err := os.Remove(filepath.Join(path, "a.md")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(staging, id+".aside"), "the agent's write")
	write(t, filepath.Join(staging, id+".aside.path"), `{"path":"a.md","expected":"`+snapshot.Hash("a")+`"}`)
	// A sweep for another id leaves it alone, however old.
	old := time.Now().Add(-2 * ledgersnap.StagingTTL)
	for _, name := range []string{id + ".aside", id + ".aside.path"} {
		if err := os.Chtimes(filepath.Join(staging, name), old, old); err != nil {
			t.Fatal(err)
		}
	}
	f.run(&circusv1.CollectLedgerRequest{SnapshotId: "other"})
	if got := entries(t, staging); !slices.Equal(got, []string{id + ".aside", id + ".aside.path"}) {
		t.Fatalf("staging after another id's sweep: %v", got)
	}
	if got := changes(f.collect()); got != "+a.md:the agent's write" {
		t.Fatalf("collect: %s", got)
	}
	if got := entries(t, staging); len(got) != 0 {
		t.Fatalf("staging after recovery: %v", got)
	}
}

// TestRebaseRecoversAside: a rebase recovers what a killed rebase left
// aside before it checks anything, so the agent's write it held is back in
// the snapshot, and is not the collected state.
func TestRebaseRecoversAside(t *testing.T) {
	f, path, known, edits := renaming(t)
	staging := filepath.Join(f.ledger, ".staging")
	if err := os.Remove(filepath.Join(path, "D1-x.md")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(staging, id+".aside"), "one, rewritten by the agent")
	write(t, filepath.Join(staging, id+".aside.path"), `{"path":"D1-x.md","expected":"`+snapshot.Hash("one, see D?a")+`"}`)
	if r := f.rebase(known, edits); r.Error != stale || !strings.Contains(r.ErrorDetail, "D1-x.md") {
		t.Fatalf("rebase: %v", r)
	}
	if got := read(t, filepath.Join(path, "D1-x.md")); got != "one, rewritten by the agent" {
		t.Fatalf("D1-x.md after the rebase: %q", got)
	}
}

// TestMalformedJournal: a journal that cannot be parsed does not block
// collections: the file it guarded is restored at the snapshot's top
// under a conflict name, and collected.
func TestMalformedJournal(t *testing.T) {
	f := newFixture(t, ledgersnap.Options{})
	f.ready(base1, "a.md", "a")
	staging := filepath.Join(f.ledger, ".staging")
	write(t, filepath.Join(staging, id+".aside"), "kept")
	write(t, filepath.Join(staging, id+".aside.path"), "\x00garbage")
	got := changes(f.collect())
	if !strings.HasPrefix(got, "+recovered.conflict-") || !strings.HasSuffix(got, ":kept") {
		t.Fatalf("collect: %s", got)
	}
	if got := entries(t, staging); len(got) != 0 {
		t.Fatalf("staging: %v", got)
	}
}

// TestLeftoverClockProbe: a clock probe a killed call left in staging does
// not stop the next rebase, and is gone afterwards.
func TestLeftoverClockProbe(t *testing.T) {
	f, _, known, edits := renaming(t)
	staging := filepath.Join(f.ledger, ".staging")
	if err := os.MkdirAll(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(staging, id+".clock"), "")
	if r := f.rebase(known, edits); r.Outcome != circusv1.RebaseLedgerResult_OUTCOME_REBASED {
		t.Fatalf("rebase: %v", r)
	}
	if got := entries(t, staging); len(got) != 0 {
		t.Fatalf("staging: %v", got)
	}
}
