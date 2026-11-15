package textutil

import "testing"

func TestTrimJS(t *testing.T) {
	for in, want := range map[string]string{
		" \tx\r\n":                  "x",
		"\u00a0\u3000x\ufeff\u2028": "x",
		"\u0085x\u0085":             "\u0085x\u0085",
		"\u200bx":                   "\u200bx",
		"\v\fx\f\v":                 "x",
		" \n ":                      "",
	} {
		if got := TrimJS(in); got != want {
			t.Errorf("TrimJS(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStripLeadingBlankLines(t *testing.T) {
	for in, want := range map[string]string{
		"\n\nx":        "x",
		"  \n\t\n  x ": "  x ",
		"  x":          "  x",
		" \n ":         " ",
		"":             "",
	} {
		if got := StripLeadingBlankLines(in); got != want {
			t.Errorf("StripLeadingBlankLines(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStripTrailingLineWhitespace(t *testing.T) {
	for in, want := range map[string]string{
		"a \rb\t\nc \r\nd\t": "a\rb\nc\r\nd",
		"  \n\t\n \r\n":      "\n\n\r\n",
		"a\u00a0\nb\u0085":   "a\nb\u0085",
		"x \r":               "x\r",
		"":                   "",
	} {
		if got := StripTrailingLineWhitespace(in); got != want {
			t.Errorf("StripTrailingLineWhitespace(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFormatSize(t *testing.T) {
	for size, want := range map[int64]string{
		30720: "30KB", 30771: "30KB", 30772: "30.1KB", 30976: "30.3KB", 32768: "32KB",
		262400: "256.3KB", 588895: "575.1KB", 1048575: "1024KB", 1048576: "1MB", 1572864: "1.5MB",
		1 << 30: "1GB", 1000: "1000 bytes",
	} {
		if got := FormatSize(size); got != want {
			t.Errorf("FormatSize(%d) = %q, want %q", size, got, want)
		}
	}
}
