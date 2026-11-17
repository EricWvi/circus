package webapi

import (
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/EricWvi/circus/internal/store"
)

// These rules started as a port of web/src/api/validation.ts and are the
// authority; the Web UI runs the same checks first for instant feedback and
// must be kept in step. Rules the Web UI does not have yet: the forbidden
// characters (isForbiddenRune), collapsing "//" and "/./" in POSIX paths,
// refusing the Windows \\?\ and \\.\ prefixes, and the repository URL.

// defaultPathPrefix is where a location without a path resolves on its
// device: defaultPathPrefix + project id.
const defaultPathPrefix = "~/circus/projects/"

// Bounds on request strings beyond the store's.
const (
	// maxDeviceNameBytes bounds a location's device name.
	maxDeviceNameBytes = store.MaxIDBytes
	// maxProjectIDBytes bounds a request's project id.
	maxProjectIDBytes = store.MaxIDBytes
)

// isJSSpace reports whether String.prototype.trim removes r: ECMAScript
// WhiteSpace and LineTerminator. This differs from unicode.IsSpace, which
// also has U+0085 and lacks U+FEFF.
func isJSSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', '\u2028', '\u2029', '\ufeff':
		return true
	}
	return unicode.Is(unicode.Zs, r)
}

// jsTrim trims like String.prototype.trim.
func jsTrim(s string) string { return strings.TrimFunc(s, isJSSpace) }

// isForbiddenRune reports the characters no name, path or repository URL
// may hold (after trimming), because they are invisible or make text
// display differently from what it is:
//   - C0 controls, DEL and C1 controls: U+0000–U+001F, U+007F–U+009F;
//   - zero-width characters: U+200B–U+200D, U+FEFF;
//   - bidirectional marks: U+200E, U+200F (LRM, RLM), U+061C (ALM);
//   - bidirectional embeddings, overrides and isolates: U+202A–U+202E,
//     U+2066–U+2069;
//   - line and paragraph separators: U+2028, U+2029.
func isForbiddenRune(r rune) bool {
	switch {
	case r < 0x20, r >= 0x7f && r <= 0x9f:
		return true
	case r >= 0x200b && r <= 0x200d, r == 0xfeff:
		return true
	case r == 0x200e, r == 0x200f, r == 0x061c:
		return true
	case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
		return true
	case r == 0x2028, r == 0x2029:
		return true
	}
	return false
}

// checkName trims a project name and returns it, or a short reason it is
// refused.
func checkName(raw string) (string, string) {
	name := jsTrim(raw)
	switch {
	case name == "":
		return "", "enter a project name"
	case utf8.RuneCountInString(name) > store.MaxProjectNameRunes:
		return "", "project name is longer than 64 characters"
	case strings.ContainsFunc(name, isForbiddenRune):
		return "", "project name has control or invisible characters"
	}
	return name, ""
}

// isWindows reports whether os (a GOOS value) uses Windows paths. Every
// other value, including an empty one, uses POSIX paths.
func isWindows(os string) bool { return strings.ToLower(os) == "windows" }

func isASCIILetter(c byte) bool { return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' }

// normalizePath trims whitespace and trailing separators, keeping a bare
// root intact. On POSIX, an absolute path also loses repeated slashes and
// "." segments ("/a//./b" is "/a/b"); ".." is kept, since resolving it
// depends on symbolic links on the device. On Windows, separators become
// backslashes and the drive letter upper case, so "d:/work" and "D:\work"
// are the same location.
func normalizePath(path, os string) string {
	trimmed := jsTrim(path)
	if !isWindows(os) {
		if !strings.HasPrefix(trimmed, "/") {
			return strings.TrimRight(trimmed, "/") // relative: refused later
		}
		var segments []string
		for _, seg := range strings.Split(trimmed, "/") {
			if seg != "" && seg != "." {
				segments = append(segments, seg)
			}
		}
		return "/" + strings.Join(segments, "/")
	}
	w := strings.ReplaceAll(trimmed, "/", `\`)
	if len(w) >= 2 && isASCIILetter(w[0]) && w[1] == ':' {
		w = strings.ToUpper(w[:1]) + w[1:]
	}
	stripped := strings.TrimRight(w, `\`)
	if len(stripped) == 2 && 'A' <= stripped[0] && stripped[0] <= 'Z' && stripped[1] == ':' {
		return stripped + `\`
	}
	return stripped
}

// isAbsolutePath reports whether a path is absolute on a device running os:
// on Windows a drive letter with a separator ("C:\") or a UNC path naming a
// server and a share ("\\server\share"); elsewhere a leading slash.
func isAbsolutePath(path, os string) bool {
	if !isWindows(os) {
		return strings.HasPrefix(path, "/")
	}
	if len(path) >= 3 && isASCIILetter(path[0]) && path[1] == ':' && (path[2] == '\\' || path[2] == '/') {
		return true
	}
	rest, ok := strings.CutPrefix(path, `\\`)
	if !ok {
		return false
	}
	server, share, ok := strings.Cut(rest, `\`)
	return ok && server != "" && share != "" && share[0] != '\\'
}

// checkPath normalizes a location's path for os and returns it, or a short
// reason it is refused. An empty or blank path stays empty: the device's
// default directory. A path that normalizes to nothing (`\\` on Windows) is
// refused, and so are forbidden characters (isForbiddenRune), the Windows
// \\?\ and \\.\ prefixes, and paths over store.MaxPathBytes.
func checkPath(raw, os string) (string, string) {
	trimmed := jsTrim(raw)
	if trimmed == "" {
		return "", ""
	}
	if strings.ContainsFunc(trimmed, isForbiddenRune) {
		return "", "path has control or invisible characters"
	}
	path := normalizePath(trimmed, os)
	switch {
	case isWindows(os) && windowsDevicePath(path):
		return "", `\\?\ and \\.\ paths are not supported`
	case !isAbsolutePath(path, os):
		return "", "path is not absolute on its device"
	case len(path) > store.MaxPathBytes:
		return "", "path is longer than 4096 bytes"
	}
	return path, ""
}

// windowsDevicePath reports a normalized Windows path in the Win32 device
// or long-path namespace (\\?\..., \\.\...), which would otherwise pass as
// a UNC path with the server "?" or ".".
func windowsDevicePath(path string) bool {
	rest, ok := strings.CutPrefix(path, `\\`)
	if !ok {
		return false
	}
	server, _, _ := strings.Cut(rest, `\`)
	return server == "?" || server == "."
}

// checkRepoURL trims a repository URL and returns it, or a short reason it
// is refused. Empty means none. The URL is not rewritten beyond trimming.
// Accepted forms: https://host/path, http://host/path,
// ssh://[user@]host[:port]/path, git://host/path, file:///abs/path, and
// scp-like user@host:path. A password, a query or a fragment is refused,
// and so is anything that could read as a command-line option (a leading
// "-" in the URL, the user or the host).
func checkRepoURL(raw string) (string, string) {
	u := jsTrim(raw)
	switch {
	case u == "":
		return "", ""
	case len(u) > store.MaxRepoURLBytes:
		return "", "repository URL is longer than 2048 bytes"
	case strings.ContainsFunc(u, func(r rune) bool { return unicode.IsSpace(r) || isForbiddenRune(r) }):
		return "", "repository URL has whitespace, control or invisible characters"
	case strings.HasPrefix(u, "-"):
		return "", "repository URL starts with -"
	}
	if !strings.Contains(u, "://") {
		if scpLike(u) {
			return u, ""
		}
		return "", "repository URL is not a supported git URL"
	}
	if !urlLike(u) {
		return "", "repository URL is not a supported git URL"
	}
	return u, ""
}

// plainName reports a user or host name of [A-Za-z0-9._~-] that does not
// start with "-" (which ssh would read as an option).
func plainName(s string) bool {
	if s == "" || s[0] == '-' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(isASCIILetter(c) || '0' <= c && c <= '9' || c == '.' || c == '_' || c == '~' || c == '-') {
			return false
		}
	}
	return true
}

// scpLike reports user@host:path with a non-empty path and a plain user
// and host (plainName).
func scpLike(u string) bool {
	userHost, path, ok := strings.Cut(u, ":")
	if !ok || path == "" {
		return false
	}
	user, host, ok := strings.Cut(userHost, "@")
	return ok && plainName(user) && plainName(host)
}

// sshHost reports an ssh:// host that is a plain name (plainName) or a
// bracketed IPv6 literal without a zone, and a port, if any, of 1 to 65535.
func sshHost(u *url.URL) bool {
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return false
		}
	}
	host := u.Hostname()
	if strings.HasPrefix(u.Host, "[") {
		ip, err := netip.ParseAddr(host)
		return err == nil && ip.Is6() && ip.Zone() == ""
	}
	return plainName(host)
}

// urlLike reports one of the accepted scheme://... forms, with the scheme
// in lower case as git expects it.
func urlLike(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || !strings.HasPrefix(raw, u.Scheme+"://") || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#") {
		return false
	}
	if _, has := u.User.Password(); u.User != nil && has {
		return false
	}
	hasPath := len(u.Path) > 1 && u.Path[0] == '/'
	host := u.Hostname()
	switch u.Scheme {
	case "https", "http", "git":
		return u.User == nil && host != "" && host[0] != '-' && hasPath
	case "ssh":
		return sshHost(u) && hasPath && (u.User == nil || plainName(u.User.Username()))
	case "file":
		return u.Host == "" && u.User == nil && hasPath
	}
	return false
}
