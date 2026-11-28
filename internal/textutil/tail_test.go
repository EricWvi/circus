package textutil

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTail(t *testing.T) {
	for _, tc := range []struct {
		name, in string
		max      int
		want     string
	}{
		{"plain", "  error: boom\n", 100, "error: boom"},
		{"colors and cursor", "\x1b[31merror\x1b[0m: \x1b[2Kboom", 100, "error: boom"},
		{"osc title", "\x1b]0;title\x07done", 100, "done"},
		{"carriage returns", "50%\r100%\r\nok", 100, "50%\n100%\nok"},
		{"control characters", "a\x00b\x07c\td\x7f", 100, "abc\td"},
		{"invalid UTF-8", "a\xffb", 100, "a�b"},
		{"tail kept", "first line\nlast line", 9, "last line"},
		{"rune boundary", "x" + strings.Repeat("é", 3), 5, "éé"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Tail(tc.in, tc.max)
			if got != tc.want {
				t.Errorf("Tail(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
			}
			if !utf8.ValidString(got) || len(got) > tc.max {
				t.Errorf("Tail(%q, %d) = %q: invalid or too long", tc.in, tc.max, got)
			}
		})
	}
}
