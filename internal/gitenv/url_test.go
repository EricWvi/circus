package gitenv

import (
	"strings"
	"testing"
)

func TestSanitizeURL(t *testing.T) {
	for raw, want := range map[string]string{
		"https://alice:token@example.com/o/r.git": "https://example.com/o/r.git",
		"https://alice@example.com/o/r":           "https://example.com/o/r",
		"ftp://u:p@example.com/r":                 "ftp://example.com/r",
		"ssh://git:pw@example.com:2222/r":         "ssh://git@example.com:2222/r",
		"ssh://:pw@example.com/r":                 "ssh://example.com/r",
		"git@github.com:o/r.git":                  "git@github.com:o/r.git",
		"file:///srv/r.git":                       "file:///srv/r.git",
		"/srv/a://b":                              "/srv/a://b",
		"https://[::1]:8443/r":                    "https://[::1]:8443/r",
	} {
		if got, ok := SanitizeURL(raw); !ok || got != want {
			t.Errorf("SanitizeURL(%q) = %q, %v; want %q", raw, got, ok, want)
		}
	}
	for _, raw := range []string{"https://us er:pw@example.com/r", "https://example.com\\@evil/r", "weird stuff://x", "https://alice:pw/@example.com/r"} {
		if got, ok := SanitizeURL(raw); ok {
			t.Errorf("SanitizeURL(%q) = %q, want rejection", raw, got)
		}
	}
}

func TestRedactText(t *testing.T) {
	raw := "https://alice:t0k3n@example.com/r.git"
	text := "fatal: unable to access '" + raw + "/': 403\n" +
		"retrying https://bob:p@ss@example.com/r and ssh://carol:pw@host/r and HTTP://dave:x@host/r\n" +
		"plain https://example.com/r and git@host:r stay\n"
	got := RedactText(text, raw, "")
	for _, secret := range []string{"t0k3n", "p@ss", "ss@", "pw", "dave", "bob"} {
		if strings.Contains(got, secret) {
			t.Errorf("redacted text keeps %q: %q", secret, got)
		}
	}
	for _, kept := range []string{"https://example.com/r.git/", "ssh://carol@host/r", "HTTP://host/r", "plain https://example.com/r and git@host:r stay"} {
		if !strings.Contains(got, kept) {
			t.Errorf("redacted text lacks %q: %q", kept, got)
		}
	}
	if got := RedactText("x https://u:p@h/r y", "https://us er:p@h/r"); strings.Contains(got, "u:p") {
		t.Errorf("unparseable URL kept credentials: %q", got)
	}
}

func TestRedactTextQuotesAndQueries(t *testing.T) {
	for text, want := range map[string]string{
		"fatal: unable to access 'https://user:SEC'RET@host/x/': 403": "fatal: unable to access 'https://host/x/': 403",
		`remote "ssh://git:pa"ss@host/r" failed`:                      `remote "ssh://git@host/r" failed`,
		"fetching https://host/r.git?access_token=SECRET now":         "fetching https://host/r.git?[redacted] now",
		"fetching 'https://host/r.git?access_token=SE'CRET#frag' now": "fetching 'https://host/r.git?[redacted]#frag' now",
		"see https://host/r and git@host:r?x":                         "see https://host/r and git@host:r?x",
	} {
		if got := RedactText(text); got != want {
			t.Errorf("RedactText(%q) = %q, want %q", text, got, want)
		}
	}
}
