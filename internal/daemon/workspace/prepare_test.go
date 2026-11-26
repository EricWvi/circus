package workspace_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/daemon/workspace"
)

const projectID = "0196f3c2-aaaa-7bbb-8ccc-0123456789ab"

const (
	created        = circusv1.PrepareWorkspaceResult_ACTION_CREATED
	usedEmpty      = circusv1.PrepareWorkspaceResult_ACTION_USED_EMPTY
	cloned         = circusv1.PrepareWorkspaceResult_ACTION_CLONED
	usedRepository = circusv1.PrepareWorkspaceResult_ACTION_USED_REPOSITORY

	invalidRequest  = circusv1.WorkspaceError_WORKSPACE_ERROR_INVALID_REQUEST
	homeUnknown     = circusv1.WorkspaceError_WORKSPACE_ERROR_HOME_UNKNOWN
	pathConflict    = circusv1.WorkspaceError_WORKSPACE_ERROR_PATH_CONFLICT
	notEmpty        = circusv1.WorkspaceError_WORKSPACE_ERROR_NOT_EMPTY
	remoteMismatch  = circusv1.WorkspaceError_WORKSPACE_ERROR_REMOTE_MISMATCH
	gitUnavailable  = circusv1.WorkspaceError_WORKSPACE_ERROR_GIT_UNAVAILABLE
	cloneFailed     = circusv1.WorkspaceError_WORKSPACE_ERROR_CLONE_FAILED
	noSpace         = circusv1.WorkspaceError_WORKSPACE_ERROR_NO_SPACE
	timedOut        = circusv1.WorkspaceError_WORKSPACE_ERROR_TIMEOUT
	busy            = circusv1.WorkspaceError_WORKSPACE_ERROR_BUSY
	cancelled       = circusv1.WorkspaceError_WORKSPACE_ERROR_CANCELLED
	permissionError = circusv1.WorkspaceError_WORKSPACE_ERROR_PERMISSION_DENIED
	ioError         = circusv1.WorkspaceError_WORKSPACE_ERROR_IO
	refused         = circusv1.WorkspaceError_WORKSPACE_ERROR_REFUSED
)

var request = "~/circus/projects/" + projectID

func (f *fixture) target() string { return filepath.Join(f.projects, projectID) }

// expectClone checks that the target is a checkout of url's commit and that
// nothing else was left beside it.
func (f *fixture) expectClone(dir, url string) {
	f.t.Helper()
	if got := f.git(dir, "remote", "get-url", "origin"); got != url {
		f.t.Fatalf("origin = %q, want %q", got, url)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "README")); err != nil || !strings.HasPrefix(string(data), "hello from") {
		f.t.Fatalf("README = %q, %v", data, err)
	}
	f.expectNoLeftovers(filepath.Dir(dir))
}

func (f *fixture) expectNoLeftovers(dir string) {
	f.t.Helper()
	f.idle()
	for _, name := range entries(f.t, dir) {
		if strings.Contains(name, ".circus-") {
			f.t.Fatalf("leftover %s in %s", name, dir)
		}
	}
}

func TestPrepareTargetTable(t *testing.T) {
	t.Run("absent without URL creates", func(t *testing.T) {
		f := newFixture(t)
		r := f.prepareNow(request, "")
		expectReady(t, r, created)
		if r.Path != f.target() {
			t.Fatalf("path = %q, want %q", r.Path, f.target())
		}
		if names := entries(t, f.target()); len(names) != 0 {
			t.Fatalf("created directory contains %v", names)
		}
		f.expectNoLeftovers(f.projects)
		// Idempotent: the directory is now the empty workspace.
		expectReady(t, f.prepareNow(request, ""), usedEmpty)
	})
	t.Run("absent with URL clones", func(t *testing.T) {
		f := newFixture(t)
		url := f.bareRepo("one")
		r := f.prepareNow(request, url)
		expectReady(t, r, cloned)
		f.expectClone(f.target(), url)
		// Idempotent: the clone is now a repository of the same remote.
		expectReady(t, f.prepareNow(request, url), usedRepository)
	})
	for _, url := range []string{"", "file:///nonexistent/r.git"} {
		t.Run("file "+url, func(t *testing.T) {
			f := newFixture(t)
			mustWrite(t, f.target(), "mine")
			expectFailed(t, f.prepareNow(request, url), pathConflict)
			if data, _ := os.ReadFile(f.target()); string(data) != "mine" {
				t.Fatalf("file changed to %q", data)
			}
			f.expectNoLeftovers(f.projects)
		})
		t.Run("fifo "+url, func(t *testing.T) {
			f := newFixture(t)
			mustMkdir(t, f.projects)
			if err := syscall.Mkfifo(f.target(), 0o600); err != nil {
				t.Fatal(err)
			}
			expectFailed(t, f.prepareNow(request, url), pathConflict)
		})
		t.Run("dangling symlink "+url, func(t *testing.T) {
			f := newFixture(t)
			mustMkdir(t, f.projects)
			if err := os.Symlink(filepath.Join(f.root, "missing"), f.target()); err != nil {
				t.Fatal(err)
			}
			expectFailed(t, f.prepareNow(request, url), pathConflict)
		})
		t.Run("ancestor is a file "+url, func(t *testing.T) {
			f := newFixture(t)
			mustWrite(t, filepath.Join(f.home, "circus"), "not a directory")
			expectFailed(t, f.prepareNow(request, url), pathConflict)
		})
	}
	t.Run("empty directory without URL is used", func(t *testing.T) {
		f := newFixture(t)
		mustMkdir(t, f.target())
		expectReady(t, f.prepareNow(request, ""), usedEmpty)
	})
	t.Run("empty directory with URL is replaced by a clone", func(t *testing.T) {
		f := newFixture(t)
		url := f.bareRepo("one")
		mustMkdir(t, f.target())
		expectReady(t, f.prepareNow(request, url), cloned)
		f.expectClone(f.target(), url)
	})
	t.Run("symlink to an empty directory", func(t *testing.T) {
		f := newFixture(t)
		url := f.bareRepo("one")
		real := filepath.Join(f.root, "elsewhere", "real")
		mustMkdir(t, real)
		mustMkdir(t, f.projects)
		if err := os.Symlink(real, f.target()); err != nil {
			t.Fatal(err)
		}
		r := f.prepareNow(request, url)
		expectReady(t, r, cloned)
		if r.Path != f.target() {
			t.Fatalf("path = %q, want the unresolved %q", r.Path, f.target())
		}
		if info, err := os.Lstat(f.target()); err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("symlink replaced: %v %v", info, err)
		}
		f.expectClone(real, url)
		f.expectNoLeftovers(f.projects)
	})
	t.Run("repository of the same remote", func(t *testing.T) {
		f := newFixture(t)
		url := f.bareRepo("one")
		f.git(f.root, "clone", "-q", url, f.target())
		expectFailed(t, f.prepareNow(request, ""), notEmpty)
		expectReady(t, f.prepareNow(request, url), usedRepository)
	})
	t.Run("repository of another remote", func(t *testing.T) {
		f := newFixture(t)
		url, other := f.bareRepo("one"), f.bareRepo("other")
		f.git(f.root, "clone", "-q", other, f.target())
		expectFailed(t, f.prepareNow(request, ""), notEmpty)
		r := f.prepareNow(request, url)
		expectFailed(t, r, remoteMismatch)
		if r.ErrorDetail != other {
			t.Fatalf("detail = %q, want the existing origin %q", r.ErrorDetail, other)
		}
		f.expectClone(f.target(), other)
	})
	t.Run("repository without origin", func(t *testing.T) {
		f := newFixture(t)
		url := f.bareRepo("one")
		mustWrite(t, filepath.Join(f.target(), "file"), "x")
		f.git(f.target(), "init", "-q")
		r := f.prepareNow(request, url)
		expectFailed(t, r, remoteMismatch)
		if r.ErrorDetail != "" {
			t.Fatalf("detail = %q, want empty", r.ErrorDetail)
		}
	})
	t.Run("non-empty directory", func(t *testing.T) {
		f := newFixture(t)
		url := f.bareRepo("one")
		mustWrite(t, filepath.Join(f.target(), "notes.txt"), "mine")
		expectFailed(t, f.prepareNow(request, ""), notEmpty)
		expectFailed(t, f.prepareNow(request, url), notEmpty)
		if names := entries(t, f.target()); !slices.Equal(names, []string{"notes.txt"}) {
			t.Fatalf("target now contains %v", names)
		}
	})
	t.Run("subdirectory of an enclosing repository", func(t *testing.T) {
		f := newFixture(t)
		url := f.bareRepo("one")
		// The home itself is a repository (dotfiles) with the same origin.
		f.git(f.home, "init", "-q")
		f.git(f.home, "remote", "add", "origin", url)
		mustWrite(t, filepath.Join(f.target(), "notes.txt"), "mine")
		expectFailed(t, f.prepareNow(request, url), notEmpty)
		other := request + "-empty"
		mustMkdir(t, filepath.Join(f.projects, projectID+"-empty"))
		expectReady(t, f.prepareNow(other, url), cloned)
	})
}

func TestPrepareInvalidRequests(t *testing.T) {
	f := newFixture(t)
	for _, tc := range []struct{ path, url string }{
		{"", ""}, {"~", ""}, {"relative/path", ""}, {"/", ""}, {"~/x\x00y", ""}, {"~/" + strings.Repeat("a", 4100), ""},
		{request, "-uupload-pack=evil"}, {request, "https://example.com/" + strings.Repeat("a", 2048)}, {request, "https://example.com/a\nb"},
		{request, "https://us er:pw@example.com/r"},
	} {
		r := f.prepareNow(tc.path, tc.url)
		expectFailed(t, r, invalidRequest)
	}
	if _, err := os.Stat(f.projects); !os.IsNotExist(err) {
		t.Fatalf("invalid requests touched the projects directory: %v", err)
	}
}

func TestPrepareHomeUnknown(t *testing.T) {
	f := newFixture(t, func(o *workspace.Options) { o.Home = "relative/home" })
	expectFailed(t, f.prepareNow(request, ""), homeUnknown)
	// An absolute path does not need the home.
	expectReady(t, f.prepareNow(filepath.Join(f.root, "abs", "ws"), ""), created)
}

func TestPrepareGitUnavailable(t *testing.T) {
	f := newFixture(t, func(o *workspace.Options) { o.Git = filepath.Join(o.Home, "no-such-git") })
	expectFailed(t, f.prepareNow(request, "file:///nonexistent/r.git"), gitUnavailable)
	f.expectNoLeftovers(f.projects)
	// Without a URL git is not needed.
	expectReady(t, f.prepareNow(request, ""), created)
	// git is looked up on the environment's PATH, not the daemon's.
	g := newFixture(t, func(o *workspace.Options) {
		o.Git = ""
		o.Environment = staticEnv{[]string{"PATH=" + filepath.Join(o.Home, "empty-bin"), "HOME=" + o.Home}}
	})
	expectFailed(t, g.prepareNow(request, "file:///nonexistent/r.git"), gitUnavailable)
}

func TestPrepareCloneFailure(t *testing.T) {
	f := newFixture(t)
	r := f.prepareNow(request, "file://"+filepath.Join(f.root, "missing.git"))
	expectFailed(t, r, cloneFailed)
	if !strings.Contains(r.ErrorDetail, "does not appear to be a git repository") && !strings.Contains(r.ErrorDetail, "fatal:") {
		t.Fatalf("detail = %q", r.ErrorDetail)
	}
	if len(r.ErrorDetail) > 1024 {
		t.Fatalf("detail has %d bytes", len(r.ErrorDetail))
	}
	if _, err := os.Lstat(f.target()); !os.IsNotExist(err) {
		t.Fatalf("failed clone left the target: %v", err)
	}
	f.expectNoLeftovers(f.projects)
}

func TestPrepareNoSpace(t *testing.T) {
	f := newFixture(t)
	mustWrite(t, filepath.Join(f.ctl, "nospace"), "")
	r := f.prepareNow(request, "file:///any/r.git")
	expectFailed(t, r, noSpace)
	if !strings.Contains(r.ErrorDetail, "No space left on device") {
		t.Fatalf("detail = %q", r.ErrorDetail)
	}
}

func TestPrepareEnvironment(t *testing.T) {
	f := newFixture(t)
	url := f.bareRepo("one")
	expectReady(t, f.prepareNow(request, url), cloned)
	data, err := os.ReadFile(filepath.Join(f.ctl, "env"))
	if err != nil {
		t.Fatal(err)
	}
	env := strings.Split(strings.TrimSpace(string(data)), "\n")
	for _, want := range []string{"GIT_TERMINAL_PROMPT=0", "GIT_EDITOR=true", "LC_ALL=C", "GCM_INTERACTIVE=never", "HOME=" + f.home,
		"GIT_ASKPASS=", "SSH_ASKPASS_REQUIRE=never", "SSH_AUTH_SOCK=/run/agent.sock"} {
		if !slices.Contains(env, want) {
			t.Errorf("environment lacks %s: %v", want, env)
		}
	}
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		switch {
		case name == "GIT_DIR", name == "GIT_WORK_TREE", entry == "GIT_TERMINAL_PROMPT=1", strings.HasPrefix(entry, "LC_ALL=fr"),
			strings.HasPrefix(name, "GIT_TRACE"), name == "GIT_CURL_VERBOSE", name == "SSH_ASKPASS", name == "DISPLAY",
			name == "WAYLAND_DISPLAY", entry == "SSH_ASKPASS_REQUIRE=force", strings.HasPrefix(entry, "GIT_ASKPASS=/"):
			t.Errorf("environment keeps %s", entry)
		}
	}
}

func TestPrepareLockBusy(t *testing.T) {
	f := newFixture(t)
	mustMkdir(t, f.projects)
	lock, err := os.OpenFile(filepath.Join(f.projects, "."+projectID+".circus-lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	expectFailed(t, f.prepareNow(request, ""), busy)
	if r := f.removeNow(request); r.Error != busy {
		t.Fatalf("removal under the lock = %v", r)
	}
	if _, err := os.Lstat(f.target()); !os.IsNotExist(err) {
		t.Fatalf("busy preparation touched the target: %v", err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	expectReady(t, f.prepareNow(request, ""), created)
	// The holder's file was removed on release; the next one is fresh.
	f.expectNoLeftovers(f.projects)
}

func TestPrepareRemovesLeftovers(t *testing.T) {
	f := newFixture(t)
	outside := filepath.Join(f.root, "outside")
	mustWrite(t, filepath.Join(outside, "keep"), "keep")
	stale := filepath.Join(f.projects, "."+projectID+".circus-partial-0123456789abcdef")
	mustWrite(t, filepath.Join(stale, ".git", "objects", "pack", "x.pack"), "partial")
	if err := os.Symlink(outside, filepath.Join(stale, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(f.projects, "."+projectID+".circus-partial-fedcba9876543210")); err != nil {
		t.Fatal(err)
	}
	keep := []string{
		"." + projectID + "x.circus-partial-0123456789abcdef", // another workspace's leftover
		"." + projectID + ".circus-partial-",                  // no suffix
		"." + projectID + ".circus-partial-0123456789ABCDE0",  // not lowercase hex (and, on a case-insensitive file system, not the stale leftover's name)
		"." + projectID + ".circus-partial-0123456789abcde",   // 15 digits
		"." + projectID + ".circus-partial-0123456789abcdef0", // 17 digits
		// A workspace named "<id>.circus-partial-x": its lock and its
		// partial clone share the prefix but are not this pattern.
		"." + projectID + ".circus-partial-x.circus-lock",
		"." + projectID + ".circus-partial-x.circus-partial-0123456789abcdef",
		"other",
	}
	for _, name := range keep {
		mustWrite(t, filepath.Join(f.projects, name), "keep")
	}
	expectReady(t, f.prepareNow(request, ""), created)
	names := entries(t, f.projects)
	want := append([]string{projectID}, keep...)
	slices.Sort(names)
	slices.Sort(want)
	if !slices.Equal(names, want) {
		t.Fatalf("projects = %v, want %v", names, want)
	}
	if data, err := os.ReadFile(filepath.Join(outside, "keep")); err != nil || string(data) != "keep" {
		t.Fatalf("symlink target damaged: %q %v", data, err)
	}
}
