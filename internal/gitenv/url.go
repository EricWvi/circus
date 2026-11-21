package gitenv

import (
	"net/netip"
	"strconv"
	"strings"
	"unicode"
)

// SanitizeURL removes credentials from a Git remote URL. It only rewrites
// the authority of scheme URLs: passwords are dropped, and HTTP(S) and
// FTP(S) URLs lose all userinfo. SCP-style remotes and local paths are
// returned unchanged. It preserves the spelling of the remote and reports
// false for ambiguous authorities, whose credentials it cannot locate.
func SanitizeURL(raw string) (string, bool) {
	scheme, rest, ok := strings.Cut(raw, "://")
	if !ok {
		return raw, true
	}
	if !validGitURLScheme(scheme) {
		// A :// inside a local path or the path of an scp-style remote
		// does not introduce a URL authority.
		if strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "./") || strings.HasPrefix(raw, "../") || isGitSCPRemote(raw) {
			return raw, true
		}
		return "", false
	}
	if strings.ContainsRune(raw, '\\') {
		return "", false
	}
	pathEnd := strings.IndexAny(rest, "?#")
	if pathEnd < 0 {
		pathEnd = len(rest)
	}
	authority, path, _ := strings.Cut(rest[:pathEnd], "/")
	suffix := rest[len(authority):]
	userinfo, host, hasUserinfo := strings.Cut(authority, "@")
	// A misplaced slash can make credentials look like a host and path.
	if strings.ContainsFunc(authority, unicode.IsSpace) || !hasUserinfo && strings.ContainsRune(path, '@') {
		return "", false
	}
	if authority == "" && strings.EqualFold(scheme, "file") && strings.HasPrefix(suffix, "/") {
		return raw, true
	}
	if !hasUserinfo {
		host = authority
	} else if !validGitURLUserinfo(userinfo) {
		return "", false
	}
	if !validGitURLHost(host) {
		return "", false
	}
	if hasUserinfo {
		switch strings.ToLower(scheme) {
		case "http", "https", "ftp", "ftps":
		default:
			user, _, _ := strings.Cut(userinfo, ":")
			if user != "" {
				host = user + "@" + host
			}
		}
	}
	return scheme + "://" + host + suffix, true
}

func validGitURLScheme(scheme string) bool {
	return scheme != "" && asciiLetter(scheme[0]) && !strings.ContainsFunc(scheme[1:], func(r rune) bool {
		return !(r <= unicode.MaxASCII && asciiLetter(byte(r)) || r >= '0' && r <= '9' || strings.ContainsRune("+.-", r))
	})
}

func asciiLetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

func validGitURLUserinfo(info string) bool {
	for i := 0; i < len(info); i++ {
		c := info[i]
		if c == '%' && i+2 < len(info) {
			if _, err := strconv.ParseUint(info[i+1:i+3], 16, 8); err == nil {
				i += 2
				continue
			}
		}
		if !asciiLetter(c) && !(c >= '0' && c <= '9') && !strings.ContainsRune("-._~!$&'()*+,;=:", rune(c)) {
			return false
		}
	}
	return true
}

func validGitURLHost(host string) bool {
	if strings.HasPrefix(host, "[") {
		literal, tail, ok := strings.Cut(host[1:], "]")
		ip, err := netip.ParseAddr(literal)
		if !ok || err != nil || !ip.Is6() || ip.Zone() != "" {
			return false
		}
		if tail == "" {
			return true
		}
		port, ok := strings.CutPrefix(tail, ":")
		return ok && validGitURLPort(port)
	}
	name, port, hasPort := strings.Cut(host, ":")
	return name != "" && !strings.ContainsFunc(name, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r) && !strings.ContainsRune("._-", r)
	}) && (!hasPort || validGitURLPort(port))
}

func validGitURLPort(port string) bool {
	_, err := strconv.ParseUint(port, 10, 16)
	return err == nil
}

func isGitSCPRemote(raw string) bool {
	user, rest, ok := strings.Cut(raw, "@")
	if !ok || user == "" || strings.ContainsRune(user, ':') || !validGitURLUserinfo(user) {
		return false
	}
	if strings.HasPrefix(rest, "[") {
		literal, _, ok := strings.Cut(rest, "]:")
		return ok && validGitURLHost(literal+"]")
	}
	host, _, ok := strings.Cut(rest, ":")
	return ok && validGitURLHost(host)
}
