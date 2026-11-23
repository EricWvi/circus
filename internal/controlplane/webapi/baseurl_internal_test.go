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
	// Host names: labels of 1 to 63 bytes, no "-" at either end, and a
	// name ending in a number only as an IPv4 address.
	{"https://a-b.example", true},
	{"https://xn--bcher-kva.example/v1", true},
	{"https://h_x.example", true},
	{"https://~", false},
	{"https://h~x.localhost", false},
	{"https://a~.example", false},
	// At most 253 bytes: 3 labels of 63, one of 61 and their dots.
	{"https://" + strings.Repeat("l", 63) + "." + strings.Repeat("l", 63) + "." + strings.Repeat("l", 63) + "." + strings.Repeat("l", 61), true},
	{"https://" + strings.Repeat("l", 63) + "." + strings.Repeat("l", 63) + "." + strings.Repeat("l", 63) + "." + strings.Repeat("l", 62), false},
	{"https://" + strings.Repeat("l", 63) + "." + strings.Repeat("l", 63) + "." + strings.Repeat("l", 63) + "." + strings.Repeat("l", 63), false},
	{"https://1a.example", true},
	{"https://a.b1", true},
	{"https://8.8.8.8/", true},
	{"https://" + strings.Repeat("l", 63) + ".example", true},
	{"https://.", false},
	{"https://a..b", false},
	{"https://a.", false},
	{"https://.a", false},
	{"https://-", false},
	{"https://-a.example", false},
	{"https://a-.example", false},
	{"https://" + strings.Repeat("l", 64) + ".example", false},
	{"https://999.999.999.999", false},
	{"https://1", false},
	{"https://1.2.3", false},
	{"https://01.2.3.4", false},
	{"https://1.2.3.4.5", false},
	{"https://a.1", false},
	{"https://a.0x1", false},
	{"https://0x7f.0.0.1", false},
	{"https://bücher.example/", false},
	// Path characters that must be percent-encoded.
	{"https://h/%3C%3E%7B%7C%7D%5E%60", true},
	{"https://h/%22%5B%5D", true},
	{"https://h/a\"b", false},
	{"https://h/a[b", false},
	{"https://h/a]b", false},
	{"https://h/a<b", false},
	{"https://h/a>b", false},
	{"https://h/a{b", false},
	{"https://h/a|b", false},
	{"https://h/a}b", false},
	{"https://h/a^b", false},
	{"https://h/a`b", false},
	{"https://[::1]:8080/a{b", false},
}

// keyCases pin checkKey's outcome for each nonempty key; the parity
// fixture carries the same inputs to the Web client.
var keyCases = []struct {
	key string
	ok  bool
}{
	{"sk-fake", true},
	{"sk-ant-api03-AZaz09_-./+=~!", true},
	{"!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~", true},
	{"with space inside", false},
	{"\u00e9t\u00e9", false},
	{"sk-\u00ff", false},
	{"sk-\u6c49", false},
	{"sk-\U0001f600", false},
	{"line\nbreak", false},
	{"cr\r", false},
	{"tab\t", false},
	{"nul\x00", false},
	{"del\x7f", false},
	{"c1\u0085", false},
	{" lead", false},
	{"trail ", false},
	{"\u00a0nbsp", false},
	{"ideographic\u3000", false},
	{"zw\u200bsp", false},
	{"zwj\u200d", false},
	{"\ufeffbom", false},
	{"bom\ufeff", false},
	{"ls\u2028x", false},
	{"ps\u2029x", false},
	{"soft\u00adhyphen", false},
	{"rlo\u202ex", false},
	{"tag\U000e0041", false},
}

func TestCheckBaseURL(t *testing.T) {
	for _, c := range baseURLCases {
		if got := checkBaseURL(c.raw); (got == "") != c.ok {
			t.Errorf("checkBaseURL(%q) = %q, want ok %v", c.raw, got, c.ok)
		}
	}
}

func TestCheckKey(t *testing.T) {
	for _, c := range keyCases {
		if got := checkKey(c.key); (got == "") != c.ok {
			t.Errorf("checkKey(%q) = %q, want ok %v", c.key, got, c.ok)
		}
	}
}
