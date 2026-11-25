package ledgersnap_test

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/daemon"
	"github.com/EricWvi/circus/internal/daemon/ledgersnap"
	"github.com/EricWvi/circus/internal/daemon/workspace"
	"github.com/EricWvi/circus/internal/ledger/snapshot"
	"github.com/EricWvi/circus/internal/testutil/leakcheck"
)

func TestMain(m *testing.M) { os.Exit(leakcheck.Run(m, leakcheck.Config{})) }

const id = "0196f3c2-session"

var (
	base1 = strings.Repeat("1", 40)
	base2 = strings.Repeat("2", 40)
)

// fixture is a dispatcher with the four ledger tools on a private ledger
// directory.
type fixture struct {
	t      *testing.T
	root   string
	ledger string
	svc    *ledgersnap.Service
	d      *daemon.Dispatcher
	calls  int
}

func newFixture(t *testing.T, opts ledgersnap.Options) *fixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	leakcheck.Track(t, root)
	f := &fixture{t: t, root: root, ledger: filepath.Join(root, "state", "circus", "ledger")}
	if opts.Root == "" && opts.Home == "" {
		opts.Root = f.ledger
	}
	opts.Logger = slog.New(slog.DiscardHandler)
	f.svc = ledgersnap.New(opts)
	var tools []daemon.Tool
	for _, tool := range f.svc.Tools() {
		tools = append(tools, tool)
	}
	f.d, err = daemon.NewDispatcher(daemon.Options{Tools: leakcheck.AuditTools(t, nil, tools)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := f.d.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	return f
}

// run sends req through the dispatcher and checks the result's shape: one
// line of output, is_error exactly for FAILED, and a bounded detail.
func (f *fixture) run(req any) *circusv1.DeviceResult {
	f.t.Helper()
	f.calls++
	call := &circusv1.DeviceCall{CallId: fmt.Sprintf("call-%d", f.calls), SessionId: "ledger-" + id}
	switch r := req.(type) {
	case *circusv1.PrepareLedgerRequest:
		call.Tool = &circusv1.DeviceCall_PrepareLedger{PrepareLedger: r}
	case *circusv1.CollectLedgerRequest:
		call.Tool = &circusv1.DeviceCall_CollectLedger{CollectLedger: r}
	case *circusv1.RebaseLedgerRequest:
		call.Tool = &circusv1.DeviceCall_RebaseLedger{RebaseLedger: r}
	case *circusv1.RemoveLedgerRequest:
		call.Tool = &circusv1.DeviceCall_RemoveLedger{RemoveLedger: r}
	}
	if _, err := f.d.Submit(call); err != nil {
		f.t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	status, err := f.d.Wait(ctx, call.CallId)
	if err != nil || status.State != circusv1.CallStatus_STATE_COMPLETED || status.Result.Fault != nil {
		f.t.Fatalf("call %s: %v %v", call.CallId, status, err)
	}
	r := status.Result
	blocks := r.GetOutput().GetContent()
	if len(blocks) != 1 || strings.Contains(blocks[0].GetText(), "\n") || blocks[0].GetText() == "" {
		f.t.Fatalf("output %v", blocks)
	}
	var failed bool
	var detail string
	switch d := r.Detail.(type) {
	case *circusv1.DeviceResult_PrepareLedger:
		failed, detail = d.PrepareLedger.Outcome == circusv1.PrepareLedgerResult_OUTCOME_FAILED, d.PrepareLedger.ErrorDetail
	case *circusv1.DeviceResult_CollectLedger:
		failed, detail = d.CollectLedger.Outcome == circusv1.CollectLedgerResult_OUTCOME_FAILED, d.CollectLedger.ErrorDetail
	case *circusv1.DeviceResult_RebaseLedger:
		failed, detail = d.RebaseLedger.Outcome == circusv1.RebaseLedgerResult_OUTCOME_FAILED, d.RebaseLedger.ErrorDetail
	case *circusv1.DeviceResult_RemoveLedger:
		failed, detail = d.RemoveLedger.Outcome == circusv1.RemoveLedgerResult_OUTCOME_FAILED, d.RemoveLedger.ErrorDetail
	default:
		f.t.Fatalf("detail %T", r.Detail)
	}
	if failed != r.Output.IsError || len(detail) > 1024 {
		f.t.Fatalf("is_error %v for %v (%d bytes of detail)", r.Output.IsError, r.Detail, len(detail))
	}
	return r
}

func files(kv ...string) []*circusv1.SnapshotFile {
	var out []*circusv1.SnapshotFile
	for i := 0; i < len(kv); i += 2 {
		out = append(out, &circusv1.SnapshotFile{Path: kv[i], Content: kv[i+1]})
	}
	return out
}

func (f *fixture) prepare(req *circusv1.PrepareLedgerRequest) *circusv1.PrepareLedgerResult {
	f.t.Helper()
	if req.SnapshotId == "" {
		req.SnapshotId = id
	}
	if req.BaseCommit == "" {
		req.BaseCommit = base1
	}
	return f.run(req).GetPrepareLedger()
}

// ready prepares the snapshot from one chunk and returns its path.
func (f *fixture) ready(base string, kv ...string) string {
	f.t.Helper()
	r := f.prepare(&circusv1.PrepareLedgerRequest{BaseCommit: base, Files: files(kv...), Last: true, TotalFiles: uint32(len(kv) / 2)})
	if r.Outcome != circusv1.PrepareLedgerResult_OUTCOME_READY {
		f.t.Fatalf("prepare: %v", r)
	}
	return r.Path
}

func (f *fixture) collect(known ...*circusv1.CollectLedgerRequest_KnownChange) *circusv1.CollectLedgerResult {
	f.t.Helper()
	return f.run(&circusv1.CollectLedgerRequest{SnapshotId: id, Known: known}).GetCollectLedger()
}

func (f *fixture) manifestPath() string { return filepath.Join(f.ledger, ".manifests", id+".json") }

// settle waits for the file system clock to move past the files just
// written, so the edits that follow change their modification times.
func settle(t *testing.T, dir string) {
	t.Helper()
	probe := filepath.Join(dir, ".settle")
	stamp := func() time.Time {
		if err := os.WriteFile(probe, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		st, err := os.Stat(probe)
		if err != nil {
			t.Fatal(err)
		}
		_ = os.Remove(probe)
		return st.ModTime()
	}
	start := stamp()
	for deadline := time.Now().Add(5 * time.Second); !stamp().After(start); {
		if time.Now().After(deadline) {
			t.Fatal("the file system clock did not move")
		}
		time.Sleep(time.Millisecond)
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func changes(r *circusv1.CollectLedgerResult) string {
	var parts []string
	for _, c := range r.Changes {
		switch {
		case c.GetDeleted():
			parts = append(parts, "-"+c.Path)
		case c.GetKnown():
			parts = append(parts, "="+c.Path)
		default:
			parts = append(parts, "+"+c.Path+":"+c.GetContent())
		}
	}
	for _, p := range r.Reverted {
		parts = append(parts, "^"+p)
	}
	for _, p := range r.Problems {
		parts = append(parts, "!"+p.Path)
	}
	return strings.Join(parts, " ")
}

func entries(t *testing.T, dir string) []string {
	t.Helper()
	list, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var names []string
	for _, e := range list {
		names = append(names, e.Name())
	}
	return names
}

func TestPrepareInChunks(t *testing.T) {
	f := newFixture(t, ledgersnap.Options{})
	accepted := circusv1.PrepareLedgerResult_OUTCOME_CHUNK_ACCEPTED
	invalid := circusv1.LedgerSnapshotError_LEDGER_SNAPSHOT_ERROR_INVALID_REQUEST
	// A chunk other than 0 needs its predecessors.
	if r := f.prepare(&circusv1.PrepareLedgerRequest{ChunkIndex: 1, Files: files("a.md", "a")}); r.Error != invalid {
		t.Fatalf("chunk 1 first: %v", r)
	}
	for _, step := range []struct {
		chunk uint32
		kv    []string
		want  circusv1.PrepareLedgerResult_Outcome
	}{
		{0, []string{"a.md", "a", "core/b.md", "b"}, accepted},
		{1, []string{"core/c.md", "c"}, accepted},
		// A repeated chunk is accepted again.
		{1, []string{"core/c.md", "c"}, accepted},
		{0, []string{"a.md", "a", "core/b.md", "b"}, accepted},
		{1, []string{"core/c.md", "c"}, accepted},
	} {
		if r := f.prepare(&circusv1.PrepareLedgerRequest{ChunkIndex: step.chunk, Files: files(step.kv...)}); r.Outcome != step.want || r.Path != "" {
			t.Fatalf("chunk %d: %v", step.chunk, r)
		}
	}
	// Skipping a chunk, or another base, restarts nothing.
	if r := f.prepare(&circusv1.PrepareLedgerRequest{ChunkIndex: 3, Files: files("d.md", "d")}); r.Error != invalid || !strings.Contains(r.ErrorDetail, "expected chunk 2") {
		t.Fatalf("chunk 3: %v", r)
	}
	if r := f.prepare(&circusv1.PrepareLedgerRequest{ChunkIndex: 2, BaseCommit: base2, Files: files("d.md", "d")}); r.Error != invalid {
		t.Fatalf("other base: %v", r)
	}
	// The last chunk checks the total.
	if r := f.prepare(&circusv1.PrepareLedgerRequest{ChunkIndex: 2, Files: files("d.md", "d"), Last: true, TotalFiles: 5}); r.Error != invalid || !strings.Contains(r.ErrorDetail, "total_files") {
		t.Fatalf("wrong total: %v", r)
	}
	if _, err := os.Stat(filepath.Join(f.ledger, id)); !os.IsNotExist(err) {
		t.Fatalf("snapshot before the last chunk: %v", err)
	}
	r := f.prepare(&circusv1.PrepareLedgerRequest{ChunkIndex: 2, Files: files("d.md", "d"), Last: true, TotalFiles: 4})
	want := filepath.Join(f.ledger, id)
	if r.Outcome != circusv1.PrepareLedgerResult_OUTCOME_READY || r.Path != want {
		t.Fatalf("last chunk: %v", r)
	}
	if read(t, filepath.Join(want, "core", "c.md")) != "c" || read(t, filepath.Join(want, "d.md")) != "d" {
		t.Fatal("snapshot content")
	}
	m, err := snapshot.ReadManifestFile(f.manifestPath())
	if err != nil || m.BaseCommit != base1 || len(m.Files) != 4 || m.Lookup("a.md").SHA256 != snapshot.Hash("a") {
		t.Fatalf("manifest: %+v %v", m, err)
	}
	if got := entries(t, filepath.Join(f.ledger, ".staging")); len(got) != 0 {
		t.Fatalf("staging left: %v", got)
	}
	if got := entries(t, f.ledger); !slices.Equal(got, []string{".manifests", ".staging", id}) {
		t.Fatalf("ledger directory: %v", got)
	}
	if r := f.collect(); changes(r) != "" || r.Outcome != circusv1.CollectLedgerResult_OUTCOME_COLLECTED {
		t.Fatalf("fresh snapshot: %v", r)
	}

	// The same path twice over two chunks is not the promised total.
	other := "other-session"
	for i, kv := range [][]string{{"x.md", "1"}, {"x.md", "2"}} {
		r := f.prepare(&circusv1.PrepareLedgerRequest{SnapshotId: other, ChunkIndex: uint32(i), Files: files(kv...), Last: i == 1, TotalFiles: 2})
		if i == 1 && r.Error != invalid {
			t.Fatalf("duplicate over chunks: %v", r)
		}
	}
	for _, bad := range []*circusv1.PrepareLedgerRequest{
		{SnapshotId: "../x"}, {SnapshotId: "cli"}, {SnapshotId: "CLI"}, {SnapshotId: strings.Repeat("a", 129)},
		{BaseCommit: "HEAD"}, {Files: files("../x", "x")}, {Files: files("a.md", "1", "a.md", "2")},
		{Files: files("big.md", strings.Repeat("x", snapshot.MaxFileBytes+1))},
	} {
		if r := f.prepare(bad); r.Error != invalid {
			t.Errorf("%v: %v", bad, r)
		}
	}
}

func TestPrepareIdempotentAndReset(t *testing.T) {
	f := newFixture(t, ledgersnap.Options{})
	path := f.ready(base1, "a.md", "a", "b.md", "b")
	settle(t, path)
	write(t, filepath.Join(path, "a.md"), "edited")
	// Ready with the same base: nothing is touched.
	if got := f.ready(base1, "a.md", "a", "b.md", "b"); got != path || read(t, filepath.Join(path, "a.md")) != "edited" {
		t.Fatalf("same base: %s %q", got, read(t, filepath.Join(path, "a.md")))
	}
	// Another base needs reset.
	r := f.prepare(&circusv1.PrepareLedgerRequest{BaseCommit: base2, Files: files("c.md", "c"), Last: true, TotalFiles: 1})
	if r.Error != circusv1.LedgerSnapshotError_LEDGER_SNAPSHOT_ERROR_INVALID_REQUEST || !strings.Contains(r.ErrorDetail, "reset") {
		t.Fatalf("other base without reset: %v", r)
	}
	// Reset replaces it: the edit is gone, the old directory too.
	outside := t.TempDir()
	write(t, filepath.Join(outside, "keep.md"), "keep")
	if err := os.Symlink(outside, filepath.Join(path, "link")); err != nil {
		t.Fatal(err)
	}
	r = f.prepare(&circusv1.PrepareLedgerRequest{BaseCommit: base1, Files: files("a.md", "a", "b.md", "b"), Last: true, TotalFiles: 2, Reset_: true})
	if r.Outcome != circusv1.PrepareLedgerResult_OUTCOME_READY || read(t, filepath.Join(path, "a.md")) != "a" {
		t.Fatalf("reset: %v", r)
	}
	if read(t, filepath.Join(outside, "keep.md")) != "keep" {
		t.Fatal("reset followed a symlink")
	}
	if got := entries(t, path); !slices.Equal(got, []string{"a.md", "b.md"}) {
		t.Fatalf("after reset: %v", got)
	}
	if got := entries(t, filepath.Join(f.ledger, ".staging")); len(got) != 0 {
		t.Fatalf("staging left: %v", got)
	}
	// A directory left without a manifest (an interrupted preparation or
	// removal) is no snapshot: ABSENT, and replaced without reset.
	if err := os.Remove(f.manifestPath()); err != nil {
		t.Fatal(err)
	}
	if r := f.collect(); r.Error != circusv1.LedgerSnapshotError_LEDGER_SNAPSHOT_ERROR_ABSENT {
		t.Fatalf("without manifest: %v", r)
	}
	if got := f.ready(base2, "c.md", "c"); got != path || !slices.Equal(entries(t, path), []string{"c.md"}) {
		t.Fatalf("replaced: %v", entries(t, path))
	}
	// Something not a directory at the snapshot's path is in the way, and
	// only reset removes it, without following it.
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	r = f.prepare(&circusv1.PrepareLedgerRequest{Files: files("a.md", "a"), Last: true, TotalFiles: 1})
	if r.Error != circusv1.LedgerSnapshotError_LEDGER_SNAPSHOT_ERROR_PATH_CONFLICT {
		t.Fatalf("symlink in the way: %v", r)
	}
	if r := f.collect(); r.Error != circusv1.LedgerSnapshotError_LEDGER_SNAPSHOT_ERROR_PATH_CONFLICT {
		t.Fatalf("collect through a symlink: %v", r)
	}
	r = f.prepare(&circusv1.PrepareLedgerRequest{Files: files("a.md", "a"), Last: true, TotalFiles: 1, Reset_: true})
	if r.Outcome != circusv1.PrepareLedgerResult_OUTCOME_READY || read(t, filepath.Join(outside, "keep.md")) != "keep" {
		t.Fatalf("reset over a symlink: %v", r)
	}
}

func TestCollect(t *testing.T) {
	f := newFixture(t, ledgersnap.Options{})
	if r := f.collect(); r.Error != circusv1.LedgerSnapshotError_LEDGER_SNAPSHOT_ERROR_ABSENT || r.CurrentBaseCommit != "" {
		t.Fatalf("no ledger directory: %v", r)
	}
	path := f.ready(base1, "a.md", "a", "b.md", "b", "c.md", "c", "d.md", "d")
	settle(t, path)
	write(t, filepath.Join(path, "a.md"), "a2")
	write(t, filepath.Join(path, "b.md"), "b2")
	write(t, filepath.Join(path, "new.md"), "n")
	if err := os.Remove(filepath.Join(path, "c.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(path, "link.md")); err != nil {
		t.Fatal(err)
	}
	r := f.collect()
	if got := changes(r); got != "+a.md:a2 +b.md:b2 -c.md +new.md:n !link.md" || r.CurrentBaseCommit != base1 {
		t.Fatalf("first collect: %s on %q", got, r.CurrentBaseCommit)
	}
	if c := r.Changes[0]; c.Sha256 != snapshot.Hash("a2") || c.Size != 2 {
		t.Fatalf("change: %v", c)
	}
	known := func(p, content string) *circusv1.CollectLedgerRequest_KnownChange {
		return &circusv1.CollectLedgerRequest_KnownChange{Path: p, Change: &circusv1.CollectLedgerRequest_KnownChange_Sha256{Sha256: snapshot.Hash(content)}}
	}
	deleted := func(p string) *circusv1.CollectLedgerRequest_KnownChange {
		return &circusv1.CollectLedgerRequest_KnownChange{Path: p, Change: &circusv1.CollectLedgerRequest_KnownChange_Deleted{Deleted: true}}
	}
	// What the control plane holds comes back without content; what
	// differs comes with it; d.md, changed before and now back, is
	// reverted; link.md is a problem, neither changed nor reverted.
	r = f.collect(known("a.md", "a2"), known("b.md", "old"), deleted("c.md"), known("d.md", "d2"), known("link.md", "x"))
	if got := changes(r); got != "=a.md +b.md:b2 =c.md +new.md:n ^d.md !link.md" {
		t.Fatalf("with known: %s", got)
	}
	if c := r.Changes[0]; c.Sha256 != snapshot.Hash("a2") || c.Size != 2 {
		t.Fatalf("known change: %v", c)
	}
	for _, bad := range [][]*circusv1.CollectLedgerRequest_KnownChange{
		{known("../x", "x")}, {known("a.md", "x"), known("a.md", "y")}, {{Path: "a.md"}},
		{{Path: "a.md", Change: &circusv1.CollectLedgerRequest_KnownChange_Sha256{Sha256: "ABC"}}},
	} {
		if r := f.collect(bad...); r.Error != circusv1.LedgerSnapshotError_LEDGER_SNAPSHOT_ERROR_INVALID_REQUEST {
			t.Errorf("%v: %v", bad, r)
		}
	}
	// More than 8 MiB of changed content: paths and sizes, no content.
	for i := range 33 {
		write(t, filepath.Join(path, fmt.Sprintf("big%02d.md", i)), strings.Repeat("x", snapshot.MaxFileBytes))
	}
	r = f.collect()
	if !r.TooLarge || len(r.Changes) != 37 || r.CurrentBaseCommit != base1 {
		t.Fatalf("too large: %v, %d changes", r.TooLarge, len(r.Changes))
	}
	for _, c := range r.Changes {
		if !c.GetDeleted() && (!c.GetKnown() || c.Sha256 == "" || c.Size == 0) {
			t.Fatalf("too large change: %v", c)
		}
	}
}

// TestCollectProblemCap: at most 1024 problems are sent.
func TestCollectProblemCap(t *testing.T) {
	f := newFixture(t, ledgersnap.Options{})
	path := f.ready(base1)
	for i := range 1100 {
		if err := os.Symlink("x", filepath.Join(path, fmt.Sprintf("l%04d", i))); err != nil {
			t.Fatal(err)
		}
	}
	if r := f.collect(); len(r.Problems) != 1024 || r.Outcome != circusv1.CollectLedgerResult_OUTCOME_COLLECTED {
		t.Fatalf("%d problems", len(r.Problems))
	}
}

func rebaseEdit(p, expected string, content *string) *circusv1.RebaseLedgerRequest_Edit {
	e := &circusv1.RebaseLedgerRequest_Edit{Path: p}
	if expected != "" {
		e.ExpectedSha256 = snapshot.Hash(expected)
	}
	if content == nil {
		e.Change = &circusv1.RebaseLedgerRequest_Edit_Deleted{Deleted: true}
	} else {
		e.Change = &circusv1.RebaseLedgerRequest_Edit_Content{Content: *content}
	}
	return e
}

func ptr(s string) *string { return &s }

func knownSha(p, content string) *circusv1.CollectLedgerRequest_KnownChange {
	return &circusv1.CollectLedgerRequest_KnownChange{Path: p, Change: &circusv1.CollectLedgerRequest_KnownChange_Sha256{Sha256: snapshot.Hash(content)}}
}

func knownDeleted(p string) *circusv1.CollectLedgerRequest_KnownChange {
	return &circusv1.CollectLedgerRequest_KnownChange{Path: p, Change: &circusv1.CollectLedgerRequest_KnownChange_Deleted{Deleted: true}}
}

func (f *fixture) rebase(known []*circusv1.CollectLedgerRequest_KnownChange, edits []*circusv1.RebaseLedgerRequest_Edit) *circusv1.RebaseLedgerResult {
	f.t.Helper()
	return f.run(&circusv1.RebaseLedgerRequest{SnapshotId: id, BaseCommit: base2, Edits: edits, Known: known}).GetRebaseLedger()
}

// snapshotState is everything a rebase may change: the files and the
// manifest.
func (f *fixture) snapshotState(path string) string {
	f.t.Helper()
	var b strings.Builder
	b.WriteString(read(f.t, f.manifestPath()))
	err := filepath.WalkDir(path, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		fmt.Fprintf(&b, "%s %v ", p, d.Type())
		if d.Type().IsRegular() {
			b.WriteString(read(f.t, p))
		}
		return nil
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return b.String()
}

const stale = circusv1.LedgerSnapshotError_LEDGER_SNAPSHOT_ERROR_STALE

// renaming is a session's snapshot after its turn: base D1 and D2, the
// agent added D?a and referred to it from D1. collected is what the
// control plane collected and merged, renamed the edits that make it the
// merged view.
func renaming(t *testing.T) (*fixture, string, []*circusv1.CollectLedgerRequest_KnownChange, []*circusv1.RebaseLedgerRequest_Edit) {
	f := newFixture(t, ledgersnap.Options{})
	path := f.ready(base1, "D1-x.md", "one", "D2-y.md", "two")
	settle(t, path)
	write(t, filepath.Join(path, "D?a-new.md"), "new, see D1")
	write(t, filepath.Join(path, "D1-x.md"), "one, see D?a")
	if got := changes(f.collect()); got != "+D1-x.md:one, see D?a +D?a-new.md:new, see D1" {
		t.Fatalf("collected: %s", got)
	}
	known := []*circusv1.CollectLedgerRequest_KnownChange{knownSha("D1-x.md", "one, see D?a"), knownSha("D?a-new.md", "new, see D1")}
	edits := []*circusv1.RebaseLedgerRequest_Edit{
		rebaseEdit("D?a-new.md", "new, see D1", nil),
		rebaseEdit("D92-new.md", "", ptr("new, see D1")),
		rebaseEdit("D1-x.md", "one, see D?a", ptr("one, see D92")),
	}
	return f, path, known, edits
}

func TestRebase(t *testing.T) {
	f, path, known, edits := renaming(t)
	before := f.snapshotState(path)
	// An edit that does not find what it expects: nothing changes.
	wrong := slices.Clone(edits)
	wrong[2] = rebaseEdit("D1-x.md", "something else", ptr("one, see D92"))
	if r := f.rebase(known, wrong); r.Error != stale || !strings.Contains(r.ErrorDetail, "D1-x.md") || !strings.Contains(r.ErrorDetail, "collect again") ||
		r.CurrentBaseCommit != base1 {
		t.Fatalf("stale edit: %v", r)
	}
	// Known changes that are not the snapshot's: nothing changes.
	if r := f.rebase(known[1:], edits); r.Error != stale || !strings.Contains(r.ErrorDetail, "D1-x.md") {
		t.Fatalf("incomplete known: %v", r)
	}
	if f.snapshotState(path) != before {
		t.Fatal("a stale rebase changed something")
	}
	if r := f.rebase(known, edits); r.Outcome != circusv1.RebaseLedgerResult_OUTCOME_REBASED || r.CurrentBaseCommit != base2 {
		t.Fatalf("rebase: %v", r)
	}
	if !slices.Equal(entries(t, path), []string{"D1-x.md", "D2-y.md", "D92-new.md"}) || read(t, filepath.Join(path, "D1-x.md")) != "one, see D92" {
		t.Fatalf("after rebase: %v", entries(t, path))
	}
	if r := f.collect(); changes(r) != "" || r.CurrentBaseCommit != base2 {
		t.Fatalf("collect after rebase: %s on %q", changes(r), r.CurrentBaseCommit)
	}
	if m, err := snapshot.ReadManifestFile(f.manifestPath()); err != nil || m.BaseCommit != base2 || len(m.Files) != 3 {
		t.Fatalf("manifest: %+v %v", m, err)
	}
	// Sent again, as after a lost result: the base holds every target and
	// every known change, so it is accepted and changes nothing.
	// The manifest is written again (a new time); the files stay.
	filesOnly := func() string {
		_, rest, _ := strings.Cut(f.snapshotState(path), "\n")
		return rest
	}
	after := filesOnly()
	if r := f.rebase(known, edits); r.Outcome != circusv1.RebaseLedgerResult_OUTCOME_REBASED {
		t.Fatalf("again: %v", r)
	}
	if filesOnly() != after || changes(f.collect()) != "" {
		t.Fatal("the repeated rebase changed the files")
	}
	// Sent again after the agent changed the snapshot: STALE, on the
	// request's base already.
	write(t, filepath.Join(path, "D2-y.md"), "two, later")
	if r := f.rebase(known, edits); r.Error != stale || r.CurrentBaseCommit != base2 {
		t.Fatalf("again after a change: %v", r)
	}
	if got := entries(t, filepath.Join(f.ledger, ".staging")); len(got) != 0 {
		t.Fatalf("staging left: %v", got)
	}
}

// TestRebaseRefusesLaterAgentEdits: a change the agent made after the
// collection the merge used, of any kind, makes the rebase STALE with
// nothing changed, and is collected next.
func TestRebaseRefusesLaterAgentEdits(t *testing.T) {
	for _, tc := range []struct {
		name    string
		edit    func(t *testing.T, path string)
		collect string
	}{
		{"changed", func(t *testing.T, path string) { write(t, filepath.Join(path, "D2-y.md"), "two, edited") }, "+D2-y.md:two, edited"},
		{"new", func(t *testing.T, path string) { write(t, filepath.Join(path, "notes.md"), "later") }, "+notes.md:later"},
		{"deleted", func(t *testing.T, path string) {
			if err := os.Remove(filepath.Join(path, "D2-y.md")); err != nil {
				t.Fatal(err)
			}
		}, "-D2-y.md"},
		{"collected file changed again", func(t *testing.T, path string) { write(t, filepath.Join(path, "D?a-new.md"), "newer") }, "+D?a-new.md:newer"},
		{"symlink", func(t *testing.T, path string) {
			if err := os.Symlink("D2-y.md", filepath.Join(path, "link.md")); err != nil {
				t.Fatal(err)
			}
		}, "!link.md"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, path, known, edits := renaming(t)
			settle(t, path)
			tc.edit(t, path)
			before := f.snapshotState(path)
			r := f.rebase(known, edits)
			if r.Error != stale || !strings.Contains(r.ErrorDetail, "collect again") || !strings.Contains(r.ErrorDetail, "Nothing was changed") {
				t.Fatalf("rebase: %v", r)
			}
			if f.snapshotState(path) != before {
				t.Fatal("a stale rebase changed something")
			}
			if got := changes(f.collect(known...)); !strings.Contains(got, tc.collect) {
				t.Fatalf("collect after the refusal: %s", got)
			}
		})
	}
}

// TestRebaseInterrupted: a rebase that applied some of its edits and died
// before its manifest loses nothing; the same request finishes it, unless
// the agent changed something meanwhile.
func TestRebaseInterrupted(t *testing.T) {
	for _, tc := range []struct {
		name string
		// done applies what the interrupted attempt did.
		done func(t *testing.T, path string)
	}{
		{"after writing", func(t *testing.T, path string) { write(t, filepath.Join(path, "D92-new.md"), "new, see D1") }},
		{"after deleting", func(t *testing.T, path string) {
			if err := os.Remove(filepath.Join(path, "D?a-new.md")); err != nil {
				t.Fatal(err)
			}
		}},
		{"after all edits", func(t *testing.T, path string) {
			write(t, filepath.Join(path, "D92-new.md"), "new, see D1")
			write(t, filepath.Join(path, "D1-x.md"), "one, see D92")
			if err := os.Remove(filepath.Join(path, "D?a-new.md")); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, path, known, edits := renaming(t)
			settle(t, path)
			tc.done(t, path)
			// Meanwhile the agent wrote a file: refused, nothing changed.
			write(t, filepath.Join(path, "notes.md"), "agent's later change")
			before := f.snapshotState(path)
			if r := f.rebase(known, edits); r.Error != stale || !strings.Contains(r.ErrorDetail, "notes.md") {
				t.Fatalf("with a later change: %v", r)
			}
			if f.snapshotState(path) != before {
				t.Fatal("a stale rebase changed something")
			}
			if err := os.Remove(filepath.Join(path, "notes.md")); err != nil {
				t.Fatal(err)
			}
			if r := f.rebase(known, edits); r.Outcome != circusv1.RebaseLedgerResult_OUTCOME_REBASED {
				t.Fatalf("resent: %v", r)
			}
			if !slices.Equal(entries(t, path), []string{"D1-x.md", "D2-y.md", "D92-new.md"}) || read(t, filepath.Join(path, "D1-x.md")) != "one, see D92" ||
				changes(f.collect()) != "" {
				t.Fatalf("after the resent rebase: %v", entries(t, path))
			}
		})
	}
}

// TestRebaseRacingAgent: a file the agent writes while the edits are being
// applied is caught by the check of the rebuilt base: the edits stay, the
// base stays, and the agent's file is collected.
func TestRebaseRacingAgent(t *testing.T) {
	f, path, known, edits := renaming(t)
	ledgersnap.SetAfterApply(f.svc, func() { write(t, filepath.Join(path, "notes.md"), "written during the rebase") })
	r := f.rebase(known, edits)
	ledgersnap.SetAfterApply(f.svc, nil)
	if r.Error != stale || !strings.Contains(r.ErrorDetail, "notes.md") || !strings.Contains(r.ErrorDetail, "changed while") {
		t.Fatalf("racing rebase: %v", r)
	}
	if m, err := snapshot.ReadManifestFile(f.manifestPath()); err != nil || m.BaseCommit != base1 {
		t.Fatalf("the base moved: %+v %v", m, err)
	}
	if got := changes(f.collect()); got != "+D1-x.md:one, see D92 +D92-new.md:new, see D1 +notes.md:written during the rebase" {
		t.Fatalf("collect after the race: %s", got)
	}
	// Collected and merged again, the rebase goes through and keeps it.
	known = []*circusv1.CollectLedgerRequest_KnownChange{knownSha("D1-x.md", "one, see D92"), knownSha("D92-new.md", "new, see D1"),
		knownSha("notes.md", "written during the rebase")}
	if r := f.rebase(known, nil); r.Outcome != circusv1.RebaseLedgerResult_OUTCOME_REBASED {
		t.Fatalf("rebase after collecting again: %v", r)
	}
	if read(t, filepath.Join(path, "notes.md")) != "written during the rebase" || changes(f.collect()) != "" {
		t.Fatal("after the second rebase")
	}
}

func TestRebaseRefusals(t *testing.T) {
	f := newFixture(t, ledgersnap.Options{})
	edit := []*circusv1.RebaseLedgerRequest_Edit{rebaseEdit("a.md", "", ptr("a"))}
	if r := f.rebase(nil, edit); r.Error != circusv1.LedgerSnapshotError_LEDGER_SNAPSHOT_ERROR_ABSENT {
		t.Fatalf("absent: %v", r)
	}
	path := f.ready(base1, "a.md", "a")
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(path, "out")); err != nil {
		t.Fatal(err)
	}
	invalid := circusv1.LedgerSnapshotError_LEDGER_SNAPSHOT_ERROR_INVALID_REQUEST
	for _, bad := range []*circusv1.RebaseLedgerRequest{
		{SnapshotId: id, BaseCommit: "main", Edits: edit},
		{SnapshotId: id, BaseCommit: base2, Edits: []*circusv1.RebaseLedgerRequest_Edit{{Path: "a.md"}}},
		{SnapshotId: id, BaseCommit: base2, Edits: []*circusv1.RebaseLedgerRequest_Edit{{Path: "a.md", ExpectedSha256: "x", Change: &circusv1.RebaseLedgerRequest_Edit_Deleted{Deleted: true}}}},
		{SnapshotId: id, BaseCommit: base2, Edits: []*circusv1.RebaseLedgerRequest_Edit{rebaseEdit("../a.md", "", ptr("a"))}},
		{SnapshotId: id, BaseCommit: base2, Known: []*circusv1.CollectLedgerRequest_KnownChange{knownDeleted("a.md"), knownDeleted("a.md")}},
		{SnapshotId: id, BaseCommit: base2, Known: []*circusv1.CollectLedgerRequest_KnownChange{{Path: "a.md"}}},
	} {
		if r := f.run(bad).GetRebaseLedger(); r.Error != invalid {
			t.Errorf("%v: %v", bad, r)
		}
	}
	// A symlink in the snapshot is a problem a collection reports: STALE,
	// and nothing is written through it.
	r := f.rebase(nil, []*circusv1.RebaseLedgerRequest_Edit{rebaseEdit("out/x.md", "", ptr("x"))})
	if r.Error != stale || !strings.Contains(r.ErrorDetail, "out: a symbolic link") || len(entries(t, outside)) != 0 {
		t.Fatalf("through a symlink: %v", r)
	}
}

func TestRemove(t *testing.T) {
	f := newFixture(t, ledgersnap.Options{})
	removed, absent := circusv1.RemoveLedgerResult_OUTCOME_REMOVED, circusv1.RemoveLedgerResult_OUTCOME_ABSENT
	if r := f.run(&circusv1.RemoveLedgerRequest{SnapshotId: id}).GetRemoveLedger(); r.Outcome != absent {
		t.Fatalf("no ledger directory: %v", r)
	}
	path := f.ready(base1, "a.md", "a", "core/b.md", "b")
	outside := t.TempDir()
	write(t, filepath.Join(outside, "keep.md"), "keep")
	if err := os.Symlink(outside, filepath.Join(path, "core", "out")); err != nil {
		t.Fatal(err)
	}
	// A staging of the same id goes too.
	if r := f.prepare(&circusv1.PrepareLedgerRequest{Files: files("x.md", "x"), Reset_: true}); r.Outcome != circusv1.PrepareLedgerResult_OUTCOME_CHUNK_ACCEPTED {
		t.Fatalf("stage: %v", r)
	}
	if r := f.run(&circusv1.RemoveLedgerRequest{SnapshotId: id}).GetRemoveLedger(); r.Outcome != removed {
		t.Fatalf("remove: %v", r)
	}
	if read(t, filepath.Join(outside, "keep.md")) != "keep" {
		t.Fatal("removal followed a symlink")
	}
	for _, dir := range []string{f.ledger, filepath.Join(f.ledger, ".manifests"), filepath.Join(f.ledger, ".staging")} {
		for _, name := range entries(t, dir) {
			if strings.HasPrefix(name, id) || strings.HasPrefix(name, "."+id) {
				t.Fatalf("left in %s: %s", dir, name)
			}
		}
	}
	if r := f.run(&circusv1.RemoveLedgerRequest{SnapshotId: id}).GetRemoveLedger(); r.Outcome != absent {
		t.Fatalf("again: %v", r)
	}
	// A symlink at the snapshot's path is refused; nothing is removed.
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	if r := f.run(&circusv1.RemoveLedgerRequest{SnapshotId: id}).GetRemoveLedger(); r.Error != circusv1.LedgerSnapshotError_LEDGER_SNAPSHOT_ERROR_PATH_CONFLICT {
		t.Fatalf("symlink: %v", r)
	}
	if _, err := os.Lstat(path); err != nil || read(t, filepath.Join(outside, "keep.md")) != "keep" {
		t.Fatal("the symlink or its target was touched")
	}
	// Ids that would name anything else are refused before any lookup.
	for _, bad := range []string{"", "..", "../x", "a/b", ".manifests", "cli", "Cli", strings.Repeat("a", 129)} {
		if r := f.run(&circusv1.RemoveLedgerRequest{SnapshotId: bad}).GetRemoveLedger(); r.Error != circusv1.LedgerSnapshotError_LEDGER_SNAPSHOT_ERROR_INVALID_REQUEST {
			t.Errorf("%q: %v", bad, r)
		}
	}
}

func TestBusy(t *testing.T) {
	f := newFixture(t, ledgersnap.Options{})
	f.ready(base1, "a.md", "a")
	busy := circusv1.LedgerSnapshotError_LEDGER_SNAPSHOT_ERROR_BUSY
	root, err := os.Open(f.ledger)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	lock, err := workspace.LockBeside(root, id)
	if err != nil {
		t.Fatal(err)
	}
	if r := f.collect(); r.Error != busy {
		t.Fatalf("locked snapshot: %v", r)
	}
	if r := f.run(&circusv1.RemoveLedgerRequest{SnapshotId: id}).GetRemoveLedger(); r.Error != busy {
		t.Fatalf("remove of a locked snapshot: %v", r)
	}
	lock.Release()
	if r := f.collect(); r.Outcome != circusv1.CollectLedgerResult_OUTCOME_COLLECTED {
		t.Fatalf("after release: %v", r)
	}
	// Two calls at once, not more: pinned by value.
	if ledgersnap.MaxCalls != 2 {
		t.Fatalf("MaxCalls = %d", ledgersnap.MaxCalls)
	}
	release := ledgersnap.HoldSlots(f.svc, 1)
	if r := f.collect(); r.Outcome != circusv1.CollectLedgerResult_OUTCOME_COLLECTED {
		t.Fatalf("one slot left: %v", r)
	}
	release()
	release = ledgersnap.HoldSlots(f.svc, 2)
	if r := f.collect(); r.Error != busy {
		t.Fatalf("no slot: %v", r)
	}
	release()
}

func TestStagingSweep(t *testing.T) {
	f := newFixture(t, ledgersnap.Options{})
	for _, sid := range []string{"stale", "fresh"} {
		if r := f.prepare(&circusv1.PrepareLedgerRequest{SnapshotId: sid, Files: files("a.md", "a")}); r.Outcome != circusv1.PrepareLedgerResult_OUTCOME_CHUNK_ACCEPTED {
			t.Fatalf("stage %s: %v", sid, r)
		}
	}
	staging := filepath.Join(f.ledger, ".staging")
	old := time.Now().Add(-ledgersnap.StagingTTL - time.Minute)
	for _, name := range []string{"stale", "stale.state", "stale.paths.0"} {
		if err := os.Chtimes(filepath.Join(staging, name), old, old); err != nil {
			t.Fatal(err)
		}
	}
	f.collect()
	if got := entries(t, staging); !slices.Equal(got, []string{"fresh", "fresh.paths.0", "fresh.state"}) {
		t.Fatalf("after the sweep: %v", got)
	}
	// The stale chunks are gone: its next chunk must start over.
	if r := f.prepare(&circusv1.PrepareLedgerRequest{SnapshotId: "stale", ChunkIndex: 1, Files: files("b.md", "b")}); r.Error != circusv1.LedgerSnapshotError_LEDGER_SNAPSHOT_ERROR_INVALID_REQUEST {
		t.Fatalf("after the sweep: %v", r)
	}
	if r := f.run(&circusv1.RemoveLedgerRequest{SnapshotId: "fresh"}).GetRemoveLedger(); r.Outcome != circusv1.RemoveLedgerResult_OUTCOME_REMOVED {
		t.Fatalf("remove staging: %v", r)
	}
}

// TestStateDirectoryLayout: without an override the snapshots live in
// <CIRCUS_HOME>/state/ledger, else $XDG_STATE_HOME/circus/ledger; a
// symlinked ancestor is fine, a symlinked ledger directory is not.
func TestStateDirectoryLayout(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	leakcheck.Track(t, dir)
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "xdg"))
	t.Setenv("CIRCUS_HOME", filepath.Join(dir, "home"))
	f := newFixture(t, ledgersnap.Options{Home: filepath.Join(dir, "user")})
	if got, want := f.ready(base1, "a.md", "a"), filepath.Join(dir, "home", "state", "ledger", id); got != want {
		t.Fatalf("with CIRCUS_HOME: %s, want %s", got, want)
	}
	if err := os.Setenv("CIRCUS_HOME", ""); err != nil {
		t.Fatal(err)
	}
	if got, want := f.ready(base1, "a.md", "a"), filepath.Join(dir, "xdg", "circus", "ledger", id); got != want {
		t.Fatalf("with XDG_STATE_HOME: %s, want %s", got, want)
	}
	t.Setenv("XDG_STATE_HOME", "relative")
	if got, want := f.ready(base1, "a.md", "a"), filepath.Join(dir, "user", ".local", "state", "circus", "ledger", id); got != want {
		t.Fatalf("default: %s, want %s", got, want)
	}
	// A symlink above the ledger directory is followed; at it, refused.
	target := filepath.Join(dir, "target")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "linked")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "linked"))
	if got := f.ready(base1, "a.md", "a"); got != filepath.Join(dir, "linked", "circus", "ledger", id) {
		t.Fatalf("symlinked ancestor: %s", got)
	}
	if read(t, filepath.Join(target, "circus", "ledger", id, "a.md")) != "a" {
		t.Fatal("snapshot not in the symlink's target")
	}
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "x2"))
	if err := os.MkdirAll(filepath.Join(dir, "x2", "circus"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(target, "circus", "ledger"), filepath.Join(dir, "x2", "circus", "ledger")); err != nil {
		t.Fatal(err)
	}
	if r := f.collect(); r.Error != circusv1.LedgerSnapshotError_LEDGER_SNAPSHOT_ERROR_PATH_CONFLICT {
		t.Fatalf("collect in a symlinked ledger directory: %v", r)
	}
	if r := f.prepare(&circusv1.PrepareLedgerRequest{Files: files("a.md", "a"), Last: true, TotalFiles: 1}); r.Error != circusv1.LedgerSnapshotError_LEDGER_SNAPSHOT_ERROR_PATH_CONFLICT {
		t.Fatalf("prepare in a symlinked ledger directory: %v", r)
	}
	for _, state := range []string{filepath.Join(dir, "home", "state"), filepath.Join(dir, "xdg"), filepath.Join(dir, "user")} {
		_ = os.RemoveAll(state)
	}
}

// TestResultLines: the output line names the outcome and the path.
func TestResultLines(t *testing.T) {
	f := newFixture(t, ledgersnap.Options{})
	r := f.run(&circusv1.PrepareLedgerRequest{SnapshotId: id, BaseCommit: "bad"})
	if !r.Output.IsError || r.Output.Content[0].GetText() != "Couldn't prepare the ledger snapshot: invalid request" {
		t.Fatalf("failed result: %v", r)
	}
	path := f.ready(base1, "a.md", "a")
	r = f.run(&circusv1.CollectLedgerRequest{SnapshotId: id})
	if want := "Collected 0 changes, 0 reverted and 0 problems from " + path; r.Output.IsError || r.Output.Content[0].GetText() != want {
		t.Fatalf("collected result: %v", r)
	}
}

// TestInvalidUTF8Content: content no protobuf encoder would send is still
// refused by the tools themselves.
func TestInvalidUTF8Content(t *testing.T) {
	svc := ledgersnap.New(ledgersnap.Options{Root: filepath.Join(t.TempDir(), "ledger"), Logger: slog.New(slog.DiscardHandler)})
	tools := svc.Tools()
	prepare := &circusv1.DeviceCall{CallId: "c1", SessionId: "ledger-" + id, Tool: &circusv1.DeviceCall_PrepareLedger{PrepareLedger: &circusv1.PrepareLedgerRequest{
		SnapshotId: id, BaseCommit: base1, Files: files("bad.md", "\xff"), Last: true, TotalFiles: 1}}}
	rebase := &circusv1.DeviceCall{CallId: "c2", SessionId: "ledger-" + id, Tool: &circusv1.DeviceCall_RebaseLedger{RebaseLedger: &circusv1.RebaseLedgerRequest{
		SnapshotId: id, BaseCommit: base1, Edits: []*circusv1.RebaseLedgerRequest_Edit{rebaseEdit("bad.md", "", ptr("\xff"))}}}}
	for i, call := range []*circusv1.DeviceCall{prepare, rebase} {
		tool := tools[0]
		if i == 1 {
			tool = tools[2]
		}
		r, err := tool.Run(t.Context(), call)
		if err != nil {
			t.Fatal(err)
		}
		if code := max(r.GetPrepareLedger().GetError(), r.GetRebaseLedger().GetError()); code != circusv1.LedgerSnapshotError_LEDGER_SNAPSHOT_ERROR_INVALID_REQUEST {
			t.Fatalf("%s: %v", tool.Name(), r)
		}
	}
}

// TestGeneratedIndex: the generated index at the top may exceed the
// per-file limit, besides a chunk's 8 MiB of other content, and is
// collected and rebased like any file; a larger INDEX.md elsewhere is
// refused.
func TestGeneratedIndex(t *testing.T) {
	f := newFixture(t, ledgersnap.Options{})
	index := strings.Repeat("| D1 | dir | title |\n", snapshot.MaxFileBytes/10)
	var kv []string
	for i := range 33 {
		kv = append(kv, fmt.Sprintf("d/D%d-x.md", i), strings.Repeat("y", snapshot.MaxFileBytes))
	}
	if r := f.prepare(&circusv1.PrepareLedgerRequest{Files: files(append(kv, "sub/INDEX.md", index)...), Last: true, TotalFiles: 34}); r.Outcome != circusv1.PrepareLedgerResult_OUTCOME_FAILED {
		t.Fatalf("a large INDEX.md below the top: %v", r)
	}
	path := f.ready(base1, append(kv[:62], "INDEX.md", index)...)
	if got := changes(f.collect()); got != "" {
		t.Fatalf("collect: %s", got)
	}
	newIndex := index + "| D2 | dir | more |\n"
	edit := rebaseEdit("INDEX.md", index, &newIndex)
	if r := f.rebase(nil, []*circusv1.RebaseLedgerRequest_Edit{edit}); r.Outcome != circusv1.RebaseLedgerResult_OUTCOME_REBASED {
		t.Fatalf("rebase: %v", r)
	}
	if read(t, filepath.Join(path, "INDEX.md")) != newIndex {
		t.Fatal("index not rebased")
	}
}
