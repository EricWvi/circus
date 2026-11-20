package link

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/clock"
	"github.com/EricWvi/circus/internal/gitenv"
	"github.com/EricWvi/circus/internal/testutil/wait"
)

func gitRemoteClient(executable string) *Client {
	return &Client{opts: Options{GitExecutable: executable, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))},
		gitQueries: make(map[chan struct{}]struct{})}
}

func requireGitFixture(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestReadGitRemote(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git is unavailable: %v", err)
	}
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(root, "gitconfig"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(root))
	gitRun := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), git, append([]string{"-C", dir}, args...)...)
		cmd.Env = gitenv.WithoutRepository(os.Environ())
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
	}
	repo := filepath.Join(root, "repo")
	child := filepath.Join(repo, "subdirectory")
	bare := filepath.Join(root, "bare.git")
	empty := filepath.Join(root, "no-origin")
	noURL := filepath.Join(root, "no-url")
	broken := filepath.Join(root, "broken")
	rewritten := filepath.Join(root, "rewritten")
	file := filepath.Join(root, "file")
	requireGitFixture(t, os.MkdirAll(child, 0700))
	requireGitFixture(t, os.WriteFile(file, nil, 0600))
	const origin = "ssh://git@example.test/team/repo.git"
	gitRun(repo, "init")
	gitRun(repo, "remote", "add", "origin", origin)
	gitRun(root, "init", "--bare", bare)
	gitRun(bare, "remote", "add", "origin", origin)
	gitRun(root, "init", empty)
	gitRun(root, "init", noURL)
	gitRun(noURL, "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*")
	gitRun(root, "init", broken)
	requireGitFixture(t, os.WriteFile(filepath.Join(broken, ".git", "config"), []byte("[core\n"), 0600))
	gitRun(root, "init", rewritten)
	gitRun(rewritten, "config", "url.https://example.test/team/.insteadOf", "short:")
	gitRun(rewritten, "remote", "add", "origin", "short:other.git")
	t.Setenv("GIT_DIR", bare)
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "remote.origin.url")
	t.Setenv("GIT_CONFIG_VALUE_0", "https://example.test/from-environment.git")

	c := gitRemoteClient(git)
	for _, tc := range []struct {
		name, path, url string
		code            circusv1.GitRemoteError
	}{
		{name: "repo", path: repo, url: origin},
		{name: "subdirectory", path: child, url: origin},
		{name: "bare", path: bare, url: origin},
		{name: "insteadOf", path: rewritten, url: "https://example.test/team/other.git"},
		{name: "home subdirectory", path: "~/repo", url: origin},
		{name: "home", path: "~", code: circusv1.GitRemoteError_GIT_REMOTE_ERROR_NOT_REPOSITORY},
		{name: "default home", code: circusv1.GitRemoteError_GIT_REMOTE_ERROR_NOT_REPOSITORY},
		{name: "no origin", path: empty, code: circusv1.GitRemoteError_GIT_REMOTE_ERROR_NO_ORIGIN},
		{name: "origin without URL", path: noURL, code: circusv1.GitRemoteError_GIT_REMOTE_ERROR_NO_ORIGIN},
		{name: "broken config", path: broken, code: circusv1.GitRemoteError_GIT_REMOTE_ERROR_FAILED},
		{name: "missing", path: filepath.Join(root, "missing"), code: circusv1.GitRemoteError_GIT_REMOTE_ERROR_NOT_FOUND},
		{name: "file", path: file, code: circusv1.GitRemoteError_GIT_REMOTE_ERROR_NOT_FOUND},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := c.readGitRemote(t.Context(), tc.path)
			if got.GetError() != tc.code || got.GetUrl() != tc.url {
				t.Fatalf("result = %v, want URL %q and error %v", got, tc.url, tc.code)
			}
		})
	}
}

func TestGitRemoteInvalidPath(t *testing.T) {
	c := gitRemoteClient("")
	for _, path := range []string{"relative", "~someone", "/nul\x00", "/bad\xff", "/" + strings.Repeat("a", 4096)} {
		if got := c.readGitRemote(t.Context(), path); got.GetError() != circusv1.GitRemoteError_GIT_REMOTE_ERROR_INVALID_PATH {
			t.Errorf("path %q: result = %v", path, got)
		}
	}
	t.Setenv("HOME", "")
	for _, path := range []string{"", "~", "~/repo"} {
		if got := c.readGitRemote(t.Context(), path); got.GetError() != circusv1.GitRemoteError_GIT_REMOTE_ERROR_INVALID_PATH {
			t.Errorf("path %q with unknown home: result = %v", path, got)
		}
	}
}

func TestGitRemoteUnavailable(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if got := gitRemoteClient("").readGitRemote(t.Context(), t.TempDir()); got.GetError() != circusv1.GitRemoteError_GIT_REMOTE_ERROR_GIT_UNAVAILABLE {
		t.Fatalf("result = %v, want git unavailable", got)
	}
}

func TestGitRemoteCommandOutput(t *testing.T) {
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	const script = `#!/bin/sh
if [ "$GIT_TERMINAL_PROMPT:$GIT_OPTIONAL_LOCKS:$GCM_INTERACTIVE:$LC_ALL" != 0:0:never:C ] ||
   [ "$GIT_QUERY_TEST_ENV" != inherited ] || read -r unexpected; then
    printf 'invalid process setup\n' >&2
    exit 1
fi
case "$3" in
    rev-parse)
        if [ "$GIT_TEST_OUTPUT" = unsafe ]; then
            printf 'fatal: detected dubious ownership in repository\n' >&2
            exit 128
        fi
        if [ "$GIT_TEST_OUTPUT" = wrong-status ]; then
            printf 'fatal: not a git repository\n' >&2
            exit 1
        fi
        [ "$4" = --git-dir ] && [ "$#" = 4 ]
        exit
        ;;
    remote) [ "$4" = get-url ] && [ "$5" = origin ] && [ "$#" = 5 ] || exit 1 ;;
    *) exit 1 ;;
esac
case "$GIT_TEST_OUTPUT" in
    trimmed) printf '\t https://example.test/repo.git  \n' ;;
    empty) printf ' \t\n' ;;
    origin) printf 'origin\n' ;;
    long) printf '%02049d\n' 0 ;;
    truncated) printf '%70000shttps://example.test/repo.git\n' '' ;;
    invalid) printf '\377\n' ;;
    esc) printf 'https://example.test/\033repo.git\n' ;;
    newline) printf 'https://example.test/repo\n.git\n' ;;
    del) printf 'https://example.test/\177repo.git\n' ;;
    c1) printf 'https://example.test/repo.git\302\205\n' ;;
    https-token) printf 'https://user:token@example.test/repo.git\n' ;;
    https-user) printf 'HTTPS://user@example.test/repo.git\n' ;;
    http-token) printf '%s\n' 'HtTp://user:p%40ss@example.test:8080/repo.git' ;;
    ftp-token) printf 'ftp://token@example.test/repo.git\n' ;;
    ftps-password) printf 'FtPs://user:token@example.test/repo.git\n' ;;
    ssh-user) printf 'ssh://git@example.test/repo.git\n' ;;
    ssh-password) printf 'ssh://git:password@[::1]:22/repo.git\n' ;;
    scp) printf 'git@example.test:path/repo.git\n' ;;
    scp-absolute) printf 'git@example.test://path/repo.git\n' ;;
    scp-at-in-path) printf 'git@example.test:path/@scope/repo.git\n' ;;
    file) printf 'file:///path/repo.git\n' ;;
    file-space) printf 'file:///Users/me/My Repos/x.git\n' ;;
    https-space) printf 'https://example.test/My Repos/x.git\n' ;;
    authority-space) printf 'https://user name@example.test/repo.git\n' ;;
    at-after-query) printf 'https://example.test/repo.git?x=@y\n' ;;
    at-after-fragment) printf 'https://example.test/repo.git#@fragment\n' ;;
    authority-user-at-in-path) printf 'https://user:token@example.test/@scope/repo.git\n' ;;
    misplaced-password) printf 'https://user:1234/SECRET@host/repo.git?x=1\n' ;;
    misplaced-user) printf 'https://user/name@host/repo.git#fragment\n' ;;
    malformed) printf 'https://user:secret@@example.test/repo.git\n' ;;
    malformed-host) printf 'https://user:secret@[::1/repo.git\n' ;;
    malformed-separator) printf '%s\n' 'https://example.test\@user:secret/repo.git' ;;
    noisy) printf '%070000d\n' 0 >&2; printf 'https://example.test/repo.git\n' ;;
    remote-no-origin) exit 2 ;;
    legacy-no-origin) printf "fatal: No such remote 'origin'\n" >&2; exit 128 ;;
    wrong-no-origin-status) printf "fatal: No such remote 'origin'\n" >&2; exit 1 ;;
    remote-failure) printf '%070000d\n' 0 >&2; exit 128 ;;
esac
`
	requireGitFixture(t, os.WriteFile(filepath.Join(bin, "fake-git"), []byte(script), 0700))
	// Override conflicting inherited values and keep unrelated settings.
	for _, name := range []string{"GIT_TERMINAL_PROMPT", "GIT_OPTIONAL_LOCKS", "GCM_INTERACTIVE", "LC_ALL"} {
		t.Setenv(name, "inherited")
	}
	t.Setenv("GIT_QUERY_TEST_ENV", "inherited")
	c := gitRemoteClient("fake-git")
	path := t.TempDir()
	for _, tc := range []struct {
		name, url string
		code      circusv1.GitRemoteError
	}{
		{name: "trimmed", url: "https://example.test/repo.git"},
		{name: "empty", code: circusv1.GitRemoteError_GIT_REMOTE_ERROR_NO_ORIGIN},
		{name: "origin", code: circusv1.GitRemoteError_GIT_REMOTE_ERROR_NO_ORIGIN},
		{name: "long", code: circusv1.GitRemoteError_GIT_REMOTE_ERROR_FAILED},
		{name: "truncated", code: circusv1.GitRemoteError_GIT_REMOTE_ERROR_FAILED},
		{name: "invalid", code: circusv1.GitRemoteError_GIT_REMOTE_ERROR_FAILED},
		{name: "esc", code: circusv1.GitRemoteError_GIT_REMOTE_ERROR_FAILED},
		{name: "newline", code: circusv1.GitRemoteError_GIT_REMOTE_ERROR_FAILED},
		{name: "del", code: circusv1.GitRemoteError_GIT_REMOTE_ERROR_FAILED},
		{name: "c1", code: circusv1.GitRemoteError_GIT_REMOTE_ERROR_FAILED},
		{name: "https-token", url: "https://example.test/repo.git"},
		{name: "https-user", url: "HTTPS://example.test/repo.git"},
		{name: "http-token", url: "HtTp://example.test:8080/repo.git"},
		{name: "ftp-token", url: "ftp://example.test/repo.git"},
		{name: "ftps-password", url: "FtPs://example.test/repo.git"},
		{name: "ssh-user", url: "ssh://git@example.test/repo.git"},
		{name: "ssh-password", url: "ssh://git@[::1]:22/repo.git"},
		{name: "scp", url: "git@example.test:path/repo.git"},
		{name: "scp-absolute", url: "git@example.test://path/repo.git"},
		{name: "scp-at-in-path", url: "git@example.test:path/@scope/repo.git"},
		{name: "file", url: "file:///path/repo.git"},
		{name: "file-space", url: "file:///Users/me/My Repos/x.git"},
		{name: "https-space", url: "https://example.test/My Repos/x.git"},
		{name: "authority-space", code: circusv1.GitRemoteError_GIT_REMOTE_ERROR_FAILED},
		{name: "at-after-query", url: "https://example.test/repo.git?x=@y"},
		{name: "at-after-fragment", url: "https://example.test/repo.git#@fragment"},
		{name: "authority-user-at-in-path", url: "https://example.test/@scope/repo.git"},
		{name: "misplaced-password", code: circusv1.GitRemoteError_GIT_REMOTE_ERROR_FAILED},
		{name: "misplaced-user", code: circusv1.GitRemoteError_GIT_REMOTE_ERROR_FAILED},
		{name: "malformed", code: circusv1.GitRemoteError_GIT_REMOTE_ERROR_FAILED},
		{name: "malformed-host", code: circusv1.GitRemoteError_GIT_REMOTE_ERROR_FAILED},
		{name: "malformed-separator", code: circusv1.GitRemoteError_GIT_REMOTE_ERROR_FAILED},
		{name: "unsafe", code: circusv1.GitRemoteError_GIT_REMOTE_ERROR_FAILED},
		{name: "wrong-status", code: circusv1.GitRemoteError_GIT_REMOTE_ERROR_FAILED},
		{name: "noisy", url: "https://example.test/repo.git"},
		{name: "remote-no-origin", code: circusv1.GitRemoteError_GIT_REMOTE_ERROR_NO_ORIGIN},
		{name: "legacy-no-origin", code: circusv1.GitRemoteError_GIT_REMOTE_ERROR_NO_ORIGIN},
		{name: "wrong-no-origin-status", code: circusv1.GitRemoteError_GIT_REMOTE_ERROR_FAILED},
		{name: "remote-failure", code: circusv1.GitRemoteError_GIT_REMOTE_ERROR_FAILED},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GIT_TEST_OUTPUT", tc.name)
			var logs bytes.Buffer
			c.opts.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
			got := c.readGitRemote(t.Context(), path)
			if got.GetError() != tc.code || got.GetUrl() != tc.url {
				t.Fatalf("result = %v, want URL %q and error %v", got, tc.url, tc.code)
			}
			if tc.name == "remote-failure" {
				var logged struct{ Stderr string }
				requireGitFixture(t, json.Unmarshal(logs.Bytes(), &logged))
				if logged.Stderr != strings.Repeat("0", 1024) {
					t.Fatalf("logged stderr is %d bytes, want the first 1024", len(logged.Stderr))
				}
			}
		})
	}
}

func TestGitRemoteGroupCleanupDeadline(t *testing.T) {
	clk := clock.New()
	checked := make(chan struct{}, 1)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	done := make(chan error, 1)
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	go func() {
		done <- waitGitGroup(42, "/repo", logger, clk, func(pid int, sig syscall.Signal) error {
			if pid != -42 || sig != 0 {
				t.Errorf("group check = (%d, %v)", pid, sig)
			}
			select {
			case <-release:
				return syscall.ESRCH
			default:
			}
			select {
			case checked <- struct{}{}:
			default:
			}
			return nil
		})
	}()
	wait.Recv(t, checked)
	wait.For(t, func() bool { return clk.Waiters() == 2 })
	clk.Advance(999 * time.Millisecond)
	wait.Recv(t, checked)
	wait.For(t, func() bool { return clk.Waiters() == 2 })
	select {
	case err := <-done:
		t.Fatalf("cleanup returned before the bound: %v", err)
	default:
	}
	clk.Advance(time.Millisecond)
	requireGitFixture(t, wait.Recv(t, done))
	var warning struct {
		Level string
		PGID  int
		Path  string
	}
	requireGitFixture(t, json.Unmarshal(logs.Bytes(), &warning))
	if warning.Level != "WARN" || warning.PGID != 42 || warning.Path != "/repo" {
		t.Fatalf("unexpected cleanup warning: %+v", warning)
	}
}
