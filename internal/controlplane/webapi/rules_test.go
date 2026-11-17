package webapi

import (
	"strings"
	"testing"
)

// The Web UI runs a port of these rules. Every input below is also in
// webParityInputs (parity_test.go), whose recorded results the Web's parity
// test checks its port against; add new cases there too.

func TestNormalizePath(t *testing.T) {
	for _, tc := range []struct{ os, in, want string }{
		// POSIX: trailing slashes go, a bare root stays.
		{"linux", "/src/a/", "/src/a"},
		{"linux", "/src/a", "/src/a"},
		{"darwin", " /a ", "/a"},
		{"linux", "/", "/"},
		{"linux", "///", "/"},
		{"linux", "/a//", "/a"},
		{"linux", "src/relative", "src/relative"},
		{"linux", "", ""},
		{"linux", "   ", ""},
		{"linux", `C:\src`, `C:\src`},
		{"", "/x/", "/x"},
		{"freebsd", "/x/", "/x"},
		// JavaScript's trim: NBSP, ideographic space and BOM go; NEL stays.
		{"linux", "\u00a0/a\u3000", "/a"},
		{"linux", "\ufeff/a", "/a"},
		{"linux", "\u0085/a", "\u0085/a"},
		// Windows: backslashes, an upper-case drive letter, no trailing
		// separator except after a bare drive.
		{"windows", "c:/src/app", `C:\src\app`},
		{"windows", `D:\work`, `D:\work`},
		{"windows", "d:/work/", `D:\work`},
		{"windows", `D:\src`, `D:\src`},
		{"windows", "C:", `C:\`},
		{"windows", `c:\`, `C:\`},
		{"windows", "c://", `C:\`},
		{"windows", "//server/share/", `\\server\share`},
		{"windows", `\\server\share\dir\`, `\\server\share\dir`},
		{"windows", "/not/windows", `\not\windows`},
		{"Windows", "e:/x", `E:\x`},
		{"windows", "1:/x", `1:\x`},
	} {
		if got := normalizePath(tc.in, tc.os); got != tc.want {
			t.Errorf("normalizePath(%q, %q) = %q, want %q", tc.in, tc.os, got, tc.want)
		}
	}
}

func TestCheckPath(t *testing.T) {
	ok := []struct{ os, in, want string }{
		{"linux", "/src", "/src"},
		{"linux", "/src/a/", "/src/a"},
		{"darwin", "/Users/me", "/Users/me"},
		{"linux", "/", "/"},
		{"windows", `D:\src`, `D:\src`},
		{"windows", `D:\work`, `D:\work`},
		{"windows", "c:/src/app", `C:\src\app`},
		{"windows", "C:", `C:\`},
		{"windows", `\\server\share`, `\\server\share`},
		{"windows", "//server/share/x", `\\server\share\x`},
		{"linux", "/" + strings.Repeat("x", 4095), "/" + strings.Repeat("x", 4095)},
		// Empty or blank: the device's default directory.
		{"linux", "", ""},
		{"linux", "  ", ""},
		{"windows", "", ""},
		{"windows", "\u3000", ""},
	}
	for _, tc := range ok {
		got, problem := checkPath(tc.in, tc.os)
		if problem != "" || got != tc.want {
			t.Errorf("checkPath(%q, %q) = %q, %q; want %q", tc.in, tc.os, got, problem, tc.want)
		}
	}
	bad := []struct{ os, in string }{
		{"linux", "src"},
		{"linux", "src/relative"},
		{"linux", "~/src"},
		{"linux", "\u0085/a"},
		{"windows", "/src"},
		{"windows", "/not/windows"},
		{"windows", `src\app`},
		{"windows", "C:src"},
		{"windows", `\\server`},
		{"windows", `\\server\`},
		{"windows", `\\\server\share`},
		{"windows", `\\`},
		{"windows", `\\\`},
		// Deliberately stricter than the Web UI: NUL and over-long paths.
		{"linux", "/a\x00b"},
		{"linux", "/" + strings.Repeat("x", 4096)},
	}
	for _, tc := range bad {
		if got, problem := checkPath(tc.in, tc.os); problem == "" {
			t.Errorf("checkPath(%q, %q) = %q, want a refusal", tc.in, tc.os, got)
		}
	}
}

func TestCheckName(t *testing.T) {
	ok := []struct{ in, want string }{
		{"  web  ", "web"},
		{"circus", "circus"},
		{strings.Repeat("x", 64), strings.Repeat("x", 64)},
		// Counted in code points: 64 four-byte characters are fine.
		{strings.Repeat("😀", 64), strings.Repeat("😀", 64)},
		{"\u3000名前\ufeff", "名前"},
		// Trimmed at the ends, like the Web UI.
		{"\ufeffweb\u00a0", "web"},
		{"a b", "a b"},
	}
	for _, tc := range ok {
		got, problem := checkName(tc.in)
		if problem != "" || got != tc.want {
			t.Errorf("checkName(%q) = %q, %q; want %q", tc.in, got, problem, tc.want)
		}
	}
	for _, in := range []string{"", "   ", "\u00a0\u2028", strings.Repeat("x", 65), strings.Repeat("😀", 65), "a\tb", "a\x00", "a\x7fb", "a\nb",
		"a\u0085b", "a\u009fb", "a\u200bb", "a\u200db", "a\ufeffb", "a\u202eb", "a\u202ab", "a\u2066b", "a\u2069b",
		"a\u200eb", "a\u200fb", "a\u061cb", "a\u2028b", "a\u2029b"} {
		if got, problem := checkName(in); problem == "" {
			t.Errorf("checkName(%q) = %q, want a refusal", in, got)
		}
	}
}

func TestCheckRepoURL(t *testing.T) {
	ok := []string{
		"https://github.com/owner/repo.git",
		"https://github.com/owner/repo",
		"http://git.example.com/x",
		"https://[::1]:8080/x.git",
		"ssh://git@github.com/owner/repo.git",
		"ssh://github.com/owner/repo.git",
		"ssh://git@host:2222/srv/repo.git",
		"git://host.example/repo.git",
		"file:///srv/git/repo.git",
		"git@github.com:owner/repo.git",
		"git@host:repo",
		"user@10.0.0.1:/abs/repo.git",
		"https://Host/Path",
	}
	for _, in := range ok {
		if got, problem := checkRepoURL(in); problem != "" || got != in {
			t.Errorf("checkRepoURL(%q) = %q, %q", in, got, problem)
		}
	}
	// Trimmed, otherwise kept as given.
	if got, problem := checkRepoURL("  git@github.com:Owner/Repo.git\n"); problem != "" || got != "git@github.com:Owner/Repo.git" {
		t.Errorf("trim = %q, %q", got, problem)
	}
	if got, problem := checkRepoURL(" \t "); problem != "" || got != "" {
		t.Errorf("blank = %q, %q", got, problem)
	}
	bad := []string{
		"github.com/owner/repo",
		"owner/repo",
		"/srv/git/repo.git",
		"host:repo",
		"@host:repo",
		"git@:repo",
		"git@host:",
		"a/b@host:repo",
		"-oProxyCommand=x@host:repo",
		"git@-host:repo",
		"--upload-pack=touch /tmp/x",
		"https://",
		"https://host",
		"https://host/",
		"https:///path",
		"https://user@host/x",
		"https://user:secret@host/x",
		"ssh://user:secret@host/x",
		"ssh://-oProxyCommand=x/repo",
		"ssh://-user@host/repo",
		"ssh://host:port/x",
		"https://host/x?ref=main",
		"https://host/x#frag",
		"https://host/x#",
		"ftp://host/x",
		"HTTPS://host/x",
		"ext::nc%20host",
		"file://host/x",
		"file:///",
		"file:relative",
		"ext::sh -c touch% /tmp/pwned",
		"https://host/a b",
		"https://host/a\tb",
		"https://host/a\u0085b",
		"https://host/a\x7f",
		"https://host/a\u00a0b",
		"https://host/" + strings.Repeat("x", 2048),
	}
	for _, in := range bad {
		if got, problem := checkRepoURL(in); problem == "" {
			t.Errorf("checkRepoURL(%q) = %q, want a refusal", in, got)
		}
	}
}

func TestNormalizePathCollapses(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"/a//b", "/a/b"},
		{"/a/./b/", "/a/b"},
		{"//a", "/a"},
		{"/./", "/"},
		{"/.", "/"},
		{"/a/.", "/a"},
		{"/a/../b", "/a/../b"},
		{"/a/.b/..c", "/a/.b/..c"},
		{"/a/./../b//", "/a/../b"},
		{"rel//x/", "rel//x"},
	} {
		if got := normalizePath(tc.in, "linux"); got != tc.want {
			t.Errorf("normalizePath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	// Windows separators are not collapsed; "C:" keeps its root.
	if got := normalizePath("c:", "windows"); got != `C:\` {
		t.Errorf("C: = %q", got)
	}
}

func TestCheckPathRefusesSpecialForms(t *testing.T) {
	for _, tc := range []struct{ os, in string }{
		{"windows", `\\?\C:\x`},
		{"windows", `\\?\UNC\server\share`},
		{"windows", `\\.\PhysicalDrive0`},
		{"windows", "//?/C:/x"},
		{"windows", `\\.\`},
		{"windows", `\\?`},
		{"linux", "/a\u0085b"},
		{"linux", "/a\u200bb"},
		{"linux", "/a\u202eb"},
		{"linux", "/a\ufeffb"},
		{"linux", "/a\x01"},
		{"windows", "C:\\a\u2067b"},
		{"linux", "/a\u200eb"},
		{"linux", "/a\u200fb"},
		{"linux", "/a\u061cb"},
		{"linux", "/a\u2028b"},
		{"windows", "C:\\a\u2029b"},
	} {
		if got, problem := checkPath(tc.in, tc.os); problem == "" {
			t.Errorf("checkPath(%q, %q) = %q, want a refusal", tc.in, tc.os, got)
		}
	}
	// A server named like the prefixes but longer is an ordinary UNC path.
	if got, problem := checkPath(`\\?x\share`, "windows"); problem != "" || got != `\\?x\share` {
		t.Errorf("UNC = %q, %q", got, problem)
	}
}

func TestCheckRepoURLNames(t *testing.T) {
	ok := []string{
		"git@github.com:owner/repo.git",
		"a.b_c~d-e@host-1.example:x",
		"ssh://git@[2001:db8::1]:22/repo.git",
		"ssh://[::1]/repo.git",
		"ssh://deploy_user@git.example.com:65535/r",
	}
	for _, in := range ok {
		if got, problem := checkRepoURL(in); problem != "" || got != in {
			t.Errorf("checkRepoURL(%q) = %q, %q", in, got, problem)
		}
	}
	bad := []string{
		"git@[::1]:repo",
		"git@host!:repo",
		"g%74@host:repo",
		"us+er@host:repo",
		"git@ho$t:repo",
		"git@host,evil:repo",
		"ssh://git@ho%73t/repo",
		"ssh://us+er@host/repo",
		"ssh://git@[fe80::1%25eth0]/repo",
		"ssh://git@[1.2.3.4]/repo",
		"ssh://git@host:0/repo",
		"ssh://git@host:65536/repo",
		"ssh://git@-host/repo",
		"https://host/a\u200bb",
		"https://host/a\u202eb",
		"https://host/a\ufeffb",
		"https://host/a\u0085b",
		"https://host/a\u200eb",
		"https://host/a\u200fb",
		"git@host:a\u061cb",
		"https://host/a\u2028b",
		"ssh://git@host/a\u2029b",
	}
	for _, in := range bad {
		if got, problem := checkRepoURL(in); problem == "" {
			t.Errorf("checkRepoURL(%q) = %q, want a refusal", in, got)
		}
	}
}

func TestSeparatorsTrimmedAtEnds(t *testing.T) {
	if got, problem := checkName("\u2028web\u2029"); problem != "" || got != "web" {
		t.Errorf("name = %q, %q", got, problem)
	}
	if got, problem := checkPath("\u2029/a\u2028", "linux"); problem != "" || got != "/a" {
		t.Errorf("path = %q, %q", got, problem)
	}
}
