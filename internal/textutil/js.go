package textutil

import (
	"fmt"
	"strings"
	"unicode"
)

// IsJSSpace reports whether r is whitespace for JavaScript's String.prototype.trim
// and the regular-expression class \s: Unicode space separators, the line and
// paragraph separators, the ASCII controls TAB through CR, and U+FEFF. Unlike
// unicode.IsSpace it excludes NEL (U+0085).
func IsJSSpace(r rune) bool {
	if r == '\u0085' {
		return false
	}
	return r == '\ufeff' || unicode.IsSpace(r)
}

// TrimJS removes leading and trailing JavaScript whitespace, like String.prototype.trim.
func TrimJS(s string) string { return strings.TrimFunc(s, IsJSSpace) }

// TrimJSRight removes trailing JavaScript whitespace, like String.prototype.trimEnd.
func TrimJSRight(s string) string { return strings.TrimRightFunc(s, IsJSSpace) }

// StripLeadingBlankLines removes leading lines that hold only JavaScript
// whitespace, through the last LF before the first other character. Leading
// whitespace of that first non-blank line is kept.
func StripLeadingBlankLines(s string) string {
	first := strings.IndexFunc(s, func(r rune) bool { return !IsJSSpace(r) })
	if first < 0 {
		first = len(s)
	}
	if newline := strings.LastIndexByte(s[:first], '\n'); newline >= 0 {
		return s[newline+1:]
	}
	return s
}

// StripTrailingLineWhitespace removes trailing JavaScript whitespace from every
// line. CRLF, LF and lone CR all end a line and are kept unchanged.
func StripTrailingLineWhitespace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for len(s) > 0 {
		end := strings.IndexAny(s, "\r\n")
		if end < 0 {
			b.WriteString(TrimJSRight(s))
			break
		}
		b.WriteString(TrimJSRight(s[:end]))
		separator := 1
		if s[end] == '\r' && end+1 < len(s) && s[end+1] == '\n' {
			separator = 2
		}
		b.WriteString(s[end : end+separator])
		s = s[end+separator:]
	}
	return b.String()
}

// FormatSize renders a byte count in binary units with at most one decimal,
// rounding half up like JavaScript's toFixed(1) and dropping a trailing ".0":
// 32768 bytes is "32KB", 30976 bytes "30.3KB", 1048576 bytes "1MB".
func FormatSize(size int64) string {
	if size < 1024 {
		// Inferred: sizes below one KB are plain byte counts; none was captured.
		return fmt.Sprintf("%d bytes", size)
	}
	unit, suffix := int64(1024), "KB"
	switch {
	case size >= 1<<30:
		// Inferred: the GB unit follows the measured KB and MB pattern.
		unit, suffix = 1<<30, "GB"
	case size >= 1<<20:
		unit, suffix = 1<<20, "MB"
	}
	// Integer arithmetic keeps exact halves exact, so they always round up.
	tenths := (size*10 + unit/2) / unit
	if tenths%10 == 0 {
		return fmt.Sprintf("%d%s", tenths/10, suffix)
	}
	return fmt.Sprintf("%d.%d%s", tenths/10, tenths%10, suffix)
}
