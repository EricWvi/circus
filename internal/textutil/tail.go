package textutil

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ansiEscape matches terminal escape sequences: CSI (colors, cursor
// movement), OSC (titles, links; ended by BEL or ST) and two-byte escapes.
var ansiEscape = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)?|\x1b[@-_]`)

// Tail makes command output safe to store and show as text and returns its
// last max bytes: valid UTF-8, terminal escape sequences removed, CR and
// CRLF turned into newlines, every other control character except newline
// and tab dropped, surrounding white space trimmed, cut at a character
// boundary.
func Tail(s string, max int) string {
	s = strings.ToValidUTF8(s, "�")
	s = ansiEscape.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || !unicode.IsControl(r) {
			return r
		}
		return -1
	}, s)
	s = strings.TrimSpace(s)
	if max >= 0 && len(s) > max {
		s = s[len(s)-max:]
		for len(s) > 0 && !utf8.RuneStart(s[0]) {
			s = s[1:]
		}
		s = strings.TrimSpace(s)
	}
	return s
}
