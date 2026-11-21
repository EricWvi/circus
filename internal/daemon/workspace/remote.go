package workspace

import (
	"strings"
)

var defaultPorts = map[string]string{"http": "80", "https": "443", "ssh": "22", "git": "9418", "ftp": "21", "ftps": "990"}

// sameRemote compares two remote URLs after `git ls-remote --get-url` has
// applied url.*.insteadOf to both.
func sameRemote(a, b string) bool {
	na, nb := normalizeRemote(a), normalizeRemote(b)
	return na != "" && na == nb
}

// normalizeRemote returns a comparison key for a remote URL: surrounding
// whitespace, trailing slashes and one ".git" suffix removed; scheme and
// host lower-cased; passwords dropped, and all userinfo for HTTP(S) and
// FTP(S); default ports dropped; an scp-style "user@host:path" written as
// "ssh://user@host/path"; "file:///path" written as "/path".
func normalizeRemote(raw string) string {
	u := strings.TrimSpace(raw)
	u = strings.TrimRight(u, "/")
	u = strings.TrimSuffix(u, ".git")
	u = strings.TrimRight(u, "/")
	if u == "" {
		return ""
	}
	scheme, rest, isURL := strings.Cut(u, "://")
	if !isURL || !validScheme(scheme) {
		user, host, path, ok := scpParts(u)
		if !ok {
			return u
		}
		scheme, rest = "ssh", user+host+"/"+strings.TrimPrefix(path, "/")
	}
	scheme = strings.ToLower(scheme)
	switch scheme {
	case "git+ssh", "ssh+git":
		scheme = "ssh"
	}
	authority, path := rest, ""
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		authority, path = rest[:i], rest[i:]
	}
	if scheme == "file" && authority == "" {
		return path
	}
	userinfo, host := "", authority
	if i := strings.LastIndexByte(authority, '@'); i >= 0 {
		userinfo, host = authority[:i], authority[i+1:]
	}
	host = strings.ToLower(host)
	if name, port, ok := splitPort(host); ok && defaultPorts[scheme] == port {
		host = name
	}
	switch scheme {
	case "http", "https", "ftp", "ftps":
		userinfo = ""
	default:
		userinfo, _, _ = strings.Cut(userinfo, ":")
	}
	if userinfo != "" {
		host = userinfo + "@" + host
	}
	return scheme + "://" + host + path
}

func validScheme(scheme string) bool {
	if scheme == "" || !asciiLetter(scheme[0]) {
		return false
	}
	for i := 1; i < len(scheme); i++ {
		c := scheme[i]
		if !asciiLetter(c) && !(c >= '0' && c <= '9') && !strings.ContainsRune("+.-", rune(c)) {
			return false
		}
	}
	return true
}

func asciiLetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

// scpParts splits git's scp-like syntax "[user@]host:path", which applies
// when a colon comes before any slash. user keeps its "@".
func scpParts(u string) (user, host, path string, ok bool) {
	colon := strings.IndexByte(u, ':')
	if colon <= 0 || strings.IndexByte(u[:colon], '/') >= 0 {
		return "", "", "", false
	}
	if strings.HasPrefix(u, "[") {
		end := strings.Index(u, "]:")
		if end < 0 {
			return "", "", "", false
		}
		colon = end + 1
	}
	host, path = u[:colon], u[colon+1:]
	if i := strings.LastIndexByte(host, '@'); i >= 0 {
		user, host = host[:i+1], host[i+1:]
	}
	if host == "" {
		return "", "", "", false
	}
	return user, strings.ToLower(host), path, true
}

// splitPort splits "host:port" or "[v6]:port".
func splitPort(host string) (string, string, bool) {
	if strings.HasPrefix(host, "[") {
		end := strings.IndexByte(host, ']')
		if end < 0 || !strings.HasPrefix(host[end+1:], ":") {
			return host, "", false
		}
		return host[:end+1], host[end+2:], true
	}
	if strings.Count(host, ":") != 1 {
		return host, "", false
	}
	name, port, _ := strings.Cut(host, ":")
	return name, port, true
}
