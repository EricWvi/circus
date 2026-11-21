package gitenv

import (
	"regexp"
	"strings"
)

// schemeUserinfo matches the authority prefix of a scheme URL that carries
// userinfo, up to its last "@" before the path. Taking the last "@" also
// covers passwords that contain an unencoded "@", and quotes are allowed
// because userinfo may contain "'" (git quotes URLs with it).
var schemeUserinfo = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*)://([^\s/<>` + "`" + `]*)@`)

// schemeQuery matches the query string of a scheme URL, which can carry
// tokens (?access_token=...), up to whitespace or a fragment. Quotes are
// taken too, since a token may contain one; a closing quote may go with it.
var schemeQuery = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://[^\s?#'"<>` + "`" + `]*)\?[^\s#]*`)

// RedactText removes credentials from free text such as Git's stderr. Every
// occurrence of each raw URL is first replaced by its sanitized form (or by
// "[redacted]" when SanitizeURL cannot parse it); then any remaining
// "scheme://user:password@" loses its password, HTTP(S) and FTP(S) URLs
// lose all userinfo, and the query string of any scheme URL becomes
// "?[redacted]". Text cut off in the middle of a URL can defeat
// the second step, so callers redact before truncating.
func RedactText(text string, rawURLs ...string) string {
	for _, raw := range rawURLs {
		if raw == "" {
			continue
		}
		replacement, ok := SanitizeURL(raw)
		if !ok {
			replacement = "[redacted]"
		}
		if replacement != raw {
			text = strings.ReplaceAll(text, raw, replacement)
		}
	}
	text = schemeUserinfo.ReplaceAllStringFunc(text, func(match string) string {
		parts := schemeUserinfo.FindStringSubmatch(match)
		scheme, userinfo := parts[1], parts[2]
		switch strings.ToLower(scheme) {
		case "http", "https", "ftp", "ftps":
			return scheme + "://"
		}
		user, _, _ := strings.Cut(userinfo, ":")
		if user == "" {
			return scheme + "://"
		}
		return scheme + "://" + user + "@"
	})
	return schemeQuery.ReplaceAllString(text, "${1}?[redacted]")
}
