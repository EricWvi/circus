package workspace_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/daemon/workspace"
)

const (
	removed      = circusv1.RemoveWorkspaceResult_OUTCOME_REMOVED
	removeAbsent = circusv1.RemoveWorkspaceResult_OUTCOME_ABSENT
	removeFailed = circusv1.RemoveWorkspaceResult_OUTCOME_FAILED
)

func expectRemoval(t *testing.T, r *circusv1.RemoveWorkspaceResult, outcome circusv1.RemoveWorkspaceResult_Outcome, code circusv1.WorkspaceError) {
	t.Helper()
	if r.Outcome != outcome || r.Error != code {
		t.Fatalf("removal = %v, want %v %v", r, outcome, code)
	}
	if len(r.ErrorDetail) > 1024 {
		t.Fatalf("detail has %d bytes", len(r.ErrorDetail))
	}
}

// outsideTree creates files that no removal may touch and returns a check.
func (f *fixture) outsideTree() (string, func()) {
	f.t.Helper()
	outside := filepath.Join(f.root, "outside")
	mustWrite(f.t, filepath.Join(outside, "precious.txt"), "precious")
	mustWrite(f.t, filepath.Join(outside, "dir", "nested.txt"), "nested")
	return outside, func() {
		f.t.Helper()
		for _, name := range []string{"precious.txt", filepath.Join("dir", "nested.txt")} {
			if _, err := os.Stat(filepath.Join(outside, name)); err != nil {
				f.t.Fatalf("outside file %s is gone: %v", name, err)
			}
		}
	}
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Lstat(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return err == nil
}

func TestRemoveWorkspace(t *testing.T) {
	f := newFixture(t)
	outside, intact := f.outsideTree()
	target := f.target()
	mustWrite(t, filepath.Join(target, "a", "b", "c", "deep.txt"), "x")
	mustWrite(t, filepath.Join(target, ".git", "HEAD"), "ref: refs/heads/main\n")
	// Symlinks inside pointing outside: removed as links, never followed.
	for name, dest := range map[string]string{"link-dir": outside, "link-file": filepath.Join(outside, "precious.txt"), "a/up": "../../../../outside", "dangling": "/nonexistent"} {
		if err := os.Symlink(dest, filepath.Join(target, name)); err != nil {
			t.Fatal(err)
		}
	}
	// A read-only directory, like a Go module cache.
	readOnly := filepath.Join(target, "pkg", "mod")
	mustWrite(t, filepath.Join(readOnly, "file.go"), "package x")
	if err := os.Chmod(readOnly, 0o555); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(target, "fifo"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Another project and a leftover of this one beside it.
	mustWrite(t, filepath.Join(f.projects, "other-project", "keep.txt"), "keep")
	mustWrite(t, filepath.Join(f.projects, "."+projectID+".circus-partial-00000000deadbeef", "x"), "x")

	r := f.removeNow(request)
	expectRemoval(t, r, removed, 0)
	if r.Path != target {
		t.Fatalf("path = %q", r.Path)
	}
	if exists(t, target) {
		t.Fatal("target still exists")
	}
	intact()
	if names := entries(t, f.projects); len(names) != 1 || names[0] != "other-project" {
		t.Fatalf("projects now = %v", names)
	}
	// Nothing there any more.
	expectRemoval(t, f.removeNow(request), removeAbsent, 0)
}

func TestRemoveAbsolutePathAndAbsent(t *testing.T) {
	f := newFixture(t)
	expectRemoval(t, f.removeNow(request), removeAbsent, 0) // no ~/circus at all
	mustMkdir(t, f.projects)
	expectRemoval(t, f.removeNow(request), removeAbsent, 0)
	mustWrite(t, filepath.Join(f.target(), "x"), "x")
	expectRemoval(t, f.removeNow(f.target()), removed, 0)
	if names := entries(t, f.projects); len(names) != 0 {
		t.Fatalf("projects now = %v", names)
	}
}

func TestRemoveRefuses(t *testing.T) {
	f := newFixture(t)
	outside, intact := f.outsideTree()
	mustWrite(t, filepath.Join(f.home, "keep.txt"), "keep")
	mustWrite(t, filepath.Join(f.projects, "p", "nested", "file"), "x")
	mustWrite(t, filepath.Join(f.projects, ".hidden", "file"), "x")
	mustWrite(t, filepath.Join(f.home, "circus", "escape", "file"), "x")
	mustWrite(t, filepath.Join(f.home, "projects2", "x", "file"), "x")
	mustWrite(t, filepath.Join(f.projects, "a-file"), "x")
	if err := os.Symlink(outside, filepath.Join(f.projects, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(f.projects, "p"), filepath.Join(f.projects, "link-inside")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"~/",                                  // the home itself
		"~/circus",                            // too shallow
		"~/circus/projects",                   // the projects directory itself
		"~/circus/projects/.",                 // cleans to the projects directory
		"~/circus/projects/p/nested",          // one level too deep
		"~/circus/projects/p/..",              // the projects directory again
		"~/circus/projects/p/../../escape",    // .. out of the projects directory
		"~/circus/projects/../../projects2/x", // .. out of circus
		"~/../outside",                        // .. out of the home
		"/~/circus/projects/p",                // a literal ~ in an absolute path
		f.root + "/outside",                   // anything else absolute
		"~/circus/projects/.hidden",           // hidden names are the tools' own
		"~/circus/projects/link",              // a symlink out of the projects directory
		"~/circus/projects/link-inside",       // a symlink, even to a workspace
		"~/circus/projects/a-file",            // not a directory
	} {
		r := f.removeNow(path)
		expectRemoval(t, r, removeFailed, refused)
	}
	for _, path := range []string{"", "~", "~other/circus/projects/p", "relative/circus/projects/p"} {
		expectRemoval(t, f.removeNow(path), removeFailed, invalidRequest)
	}
	intact()
	for _, path := range []string{"keep.txt", "circus/projects/p/nested/file", "circus/projects/.hidden/file", "circus/escape/file", "projects2/x/file", "circus/projects/a-file"} {
		if !exists(t, filepath.Join(f.home, path)) {
			t.Fatalf("%s is gone", path)
		}
	}
	if !exists(t, filepath.Join(f.projects, "link")) || !exists(t, filepath.Join(f.projects, "link-inside")) {
		t.Fatal("a refused symlink is gone")
	}
}

func TestRemoveRefusesSymlinkedAncestors(t *testing.T) {
	for _, linked := range []string{"circus", "circus/projects"} {
		t.Run(linked, func(t *testing.T) {
			f := newFixture(t)
			elsewhere := filepath.Join(f.root, "elsewhere")
			// The symlinked directory leads to a real projects/<id> tree.
			real := elsewhere
			if linked == "circus" {
				mustWrite(t, filepath.Join(elsewhere, "projects", projectID, "file"), "x")
			} else {
				mustWrite(t, filepath.Join(elsewhere, projectID, "file"), "x")
			}
			link := filepath.Join(f.home, linked)
			mustMkdir(t, filepath.Dir(link))
			if err := os.Symlink(real, link); err != nil {
				t.Fatal(err)
			}
			expectRemoval(t, f.removeNow(request), removeFailed, refused)
			matches, _ := filepath.Glob(filepath.Join(elsewhere, "*", "file"))
			more, _ := filepath.Glob(filepath.Join(elsewhere, "*", "*", "file"))
			if len(matches)+len(more) != 1 {
				t.Fatal("files behind the symlink were removed")
			}
		})
	}
}

func TestRemoveThroughSymlinkedHome(t *testing.T) {
	f := newFixture(t)
	link := filepath.Join(f.root, "home-link")
	if err := os.Symlink(f.home, link); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(f.target(), "file"), "x")
	r := workspace.NewRemove(workspace.Options{Home: link})
	result, err := r.Run(context.Background(), f.call(&circusv1.RemoveWorkspaceRequest{Path: request}))
	if err != nil {
		t.Fatal(err)
	}
	expectRemoval(t, result.GetRemoveWorkspace(), removed, 0)
	if result.GetRemoveWorkspace().Path != filepath.Join(link, "circus", "projects", projectID) {
		t.Fatalf("path = %q", result.GetRemoveWorkspace().Path)
	}
}

func TestRemoveHomeUnknown(t *testing.T) {
	f := newFixture(t, func(o *workspace.Options) { o.Home = "relative" })
	expectRemoval(t, f.removeNow(request), removeFailed, homeUnknown)
	expectRemoval(t, f.removeNow(filepath.Join(f.root, "x", "circus", "projects", "p")), removeFailed, homeUnknown)
}

func TestRemovePermissionDenied(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	f := newFixture(t)
	locked := filepath.Join(f.target(), "locked")
	mustWrite(t, filepath.Join(locked, "file"), "x")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	r := f.removeNow(request)
	expectRemoval(t, r, removeFailed, permissionError)
	if !strings.Contains(r.ErrorDetail, "locked") {
		t.Fatalf("detail = %q", r.ErrorDetail)
	}
	if !exists(t, f.target()) {
		t.Fatal("target is gone")
	}
}

// expiringContext reports DeadlineExceeded once Err has been asked limit
// times, like a deadline arriving in the middle of a removal.
type expiringContext struct {
	context.Context
	calls, limit int64
	done         chan struct{}
}

func (c *expiringContext) Err() error {
	if atomic.AddInt64(&c.calls, 1) > c.limit {
		return context.DeadlineExceeded
	}
	return nil
}
func (c *expiringContext) Done() <-chan struct{}       { return c.done }
func (c *expiringContext) Deadline() (time.Time, bool) { return time.Time{}, false }

func TestRemoveStopsAtDeadline(t *testing.T) {
	f := newFixture(t)
	for i := range 50 {
		mustWrite(t, filepath.Join(f.target(), "d", string(rune('a'+i%26))+string(rune('a'+i/26)), "file"), "x")
	}
	r := workspace.NewRemove(workspace.Options{Home: f.home})
	ctx := &expiringContext{Context: context.Background(), limit: 30, done: make(chan struct{})}
	result, err := r.Run(ctx, f.call(&circusv1.RemoveWorkspaceRequest{Path: request}))
	if err != nil {
		t.Fatal(err)
	}
	got := result.GetRemoveWorkspace()
	expectRemoval(t, got, removeFailed, ioError)
	if !strings.Contains(got.ErrorDetail, "time ran out") || !strings.Contains(got.ErrorDetail, "the rest remains in "+f.target()) || strings.Contains(got.ErrorDetail, "after removing 0 entries") {
		t.Fatalf("detail = %q", got.ErrorDetail)
	}
	if !exists(t, f.target()) {
		t.Fatal("target is gone")
	}
	if exists(t, filepath.Join(f.projects, "."+projectID+".circus-lock")) {
		t.Fatal("lock file left behind")
	}
	// A later call finishes the job.
	expectRemoval(t, f.removeNow(request), removed, 0)
}

func TestRemoveAfterPrepare(t *testing.T) {
	f := newFixture(t)
	url := f.bareRepo("one")
	expectReady(t, f.prepareNow(request, url), cloned)
	expectRemoval(t, f.removeNow(request), removed, 0)
	if names := entries(t, f.projects); len(names) != 0 {
		t.Fatalf("projects now = %v", names)
	}
	expectReady(t, f.prepareNow(request, url), cloned)
}
