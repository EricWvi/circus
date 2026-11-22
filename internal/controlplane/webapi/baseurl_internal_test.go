package webapi

import (
	"strings"
	"testing"
)

// baseURLCases pin checkBaseURL's outcome for each case; the parity
// fixture carries the same inputs to the Web client.
var baseURLCases = []struct {
	raw string
	ok  bool
}{
	{"", true},
	{"https://host/%41%2F", true},
	{"https://host:1", true},
	{"https://host:65535/v1", true},
	{"https://127.0.0.1:8443", true},
	{"http://[::1]:8080/anthropic", true},
	{"HTTPS://api.example.com:8443/v1/", true},
	{"https://api.anthropic.com", true},
	{"http://localhost:4000/", true},
	{"https://gw.example/a/b_c~d-e.f", true},
	{"https://[2001:db8::1]/v1", true},
	{"https://host/" + strings.Repeat("p", 2048-len("https://host/")), true},
	{"https://host/%zz", false},
	{"https://host/%4", false},
	{"https://h%41st", false},
	{"https://host:99999", false},
	{"https://host:0", false},
	{"https://host:x", false},
	{"https://host:", false},
	{"https://[::1", false},
	{"http:/example.com", false},
	{"https:example.com", false},
	{"https:\\\\example.com", false},
	{"https://", false},
	{"https://:443/x", false},
	{"https://u@h", false},
	{"https://h/?q", false},
	{"https://h/#f", false},
	{"https://host:65536", false},
	{"https://host:000001", false},
	{"ftp://host/", false},
	{"//host/", false},
	{"https://h/a b", false},
	{"https://h/a\\b", false},
	{"https://hé/", false},
	{"https://::1/", false},
	{"https://[::1]x/", false},
	{"https://[127.0.0.1]/", false},
	{"https://[fe80::1%25eth0]/", false},
	{"https://h:443:1/", false},
	{"https://h/" + strings.Repeat("p", 2048-len("https://h/")+1), false},
}

func TestCheckBaseURL(t *testing.T) {
	for _, c := range baseURLCases {
		if got := checkBaseURL(c.raw); (got == "") != c.ok {
			t.Errorf("checkBaseURL(%q) = %q, want ok %v", c.raw, got, c.ok)
		}
	}
}

func TestCheckKey(t *testing.T) {
	for k, ok := range map[string]bool{"sk-fake": true, "with space": true, "é": true,
		"line\nbreak": false, "cr\r": false, "tab\t": false, "nul\x00": false, "del\x7f": false, "c1\u0085": false} {
		if got := checkKey(k); (got == "") != ok {
			t.Errorf("checkKey(%q) = %q, want ok %v", k, got, ok)
		}
	}
}
