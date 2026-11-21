package workspace_test

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSameRemote runs the comparison through real git: both sides pass
// `git ls-remote --get-url`, so url.*.insteadOf applies.
func TestSameRemote(t *testing.T) {
	for _, tc := range []struct {
		name, origin, request string
		same                  bool
	}{
		{"identical", "https://example.com/o/r.git", "https://example.com/o/r.git", true},
		{".git suffix and trailing slash", "https://example.com/o/r.git", "https://example.com/o/r/", true},
		{"case of scheme and host", "HTTPS://Example.COM/o/r", "https://example.com/o/r", true},
		{"path case matters", "https://example.com/O/r", "https://example.com/o/r", false},
		{"https userinfo", "https://alice:token@example.com/o/r.git", "https://example.com/o/r", true},
		{"https user only", "https://alice@example.com/o/r", "https://bob@example.com/o/r", true},
		{"default https port", "https://example.com:443/o/r", "https://example.com/o/r", true},
		{"other port", "https://example.com:8443/o/r", "https://example.com/o/r", false},
		{"scp and ssh", "git@github.com:o/r.git", "ssh://git@github.com/o/r", true},
		{"scp and ssh with default port", "git@github.com:o/r", "ssh://git@github.com:22/o/r.git", true},
		{"ssh password", "ssh://git:pw@host/r", "git@host:r", true},
		{"ssh users differ", "ssh://alice@host/r", "bob@host:r", false},
		{"different repository", "git@github.com:o/r", "git@github.com:o/other", false},
		{"http and https differ", "http://example.com/o/r", "https://example.com/o/r", false},
		{"insteadOf", "https://github.com/o/r.git", "gh:o/r", true},
		{"insteadOf on origin", "gh:o/r", "https://github.com/o/r", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			mustWrite(t, filepath.Join(f.home, ".gitconfig"), "[url \"https://github.com/\"]\n\tinsteadOf = gh:\n")
			mustWrite(t, filepath.Join(f.target(), "file"), "x")
			f.git(f.target(), "init", "-q")
			f.git(f.target(), "remote", "add", "origin", tc.origin)
			r := f.prepareNow(request, tc.request)
			if tc.same {
				expectReady(t, r, usedRepository)
			} else {
				expectFailed(t, r, remoteMismatch)
			}
		})
	}
}

func TestSameRemoteLocalPaths(t *testing.T) {
	f := newFixture(t)
	url := f.bareRepo("one")
	bare := url[len("file://"):]
	f.git(f.root, "clone", "-q", bare, f.target())
	expectReady(t, f.prepareNow(request, url), usedRepository)
	if err := os.WriteFile(filepath.Join(f.target(), "untracked"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	expectReady(t, f.prepareNow(request, bare+"/"), usedRepository)
}
