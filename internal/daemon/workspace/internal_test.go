package workspace

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestNormalizeRemote(t *testing.T) {
	for raw, want := range map[string]string{
		"  https://Example.COM/o/r.git/ \n": "https://example.com/o/r",
		"https://u:p@example.com:443/o/r":   "https://example.com/o/r",
		"ftp://u:p@example.com:21/o/r":      "ftp://example.com/o/r",
		"ssh://git:pw@HOST:22/o/r.git":      "ssh://git@host/o/r",
		"git+ssh://git@host/o/r":            "ssh://git@host/o/r",
		"git@Host:o/r.git":                  "ssh://git@host/o/r",
		"host:/srv/r":                       "ssh://host/srv/r",
		"git://host:9418/r":                 "git://host/r",
		"https://[::1]:443/r":               "https://[::1]/r",
		"https://[::1]:8443/r":              "https://[::1]:8443/r",
		"file:///srv/r.git":                 "/srv/r",
		"/srv/r.git/":                       "/srv/r",
		"./relative/r":                      "./relative/r",
		"":                                  "",
		"   ":                               "",
		"ssh://git@host/o/r.git.git":        "ssh://git@host/o/r.git",
	} {
		if got := normalizeRemote(raw); got != want {
			t.Errorf("normalizeRemote(%q) = %q, want %q", raw, got, want)
		}
	}
	if sameRemote("", "") {
		t.Error("empty remotes compare equal")
	}
}

func TestStderrDetail(t *testing.T) {
	url := "https://alice:tok@example.com/r.git"
	t.Run("redacts the raw URL and any userinfo", func(t *testing.T) {
		got := stderrDetail([]byte("Cloning into 'x'...\nfatal: "+url+" failed\nremote: see ssh://bob:pw2@h/r and http://carol:pw3@h/r\n"), false, url)
		for _, secret := range []string{"tok", "pw2", "pw3", "carol"} {
			if strings.Contains(got, secret) {
				t.Fatalf("detail keeps %q: %q", secret, got)
			}
		}
		if !strings.Contains(got, "https://example.com/r.git") || !strings.Contains(got, "ssh://bob@h/r") {
			t.Fatalf("detail = %q", got)
		}
	})
	t.Run("drops a partial first line after overflow", func(t *testing.T) {
		got := stderrDetail([]byte("tok@example.com/r.git cut in half\nfatal: last\n"), true, url)
		if got != "fatal: last" {
			t.Fatalf("detail = %q", got)
		}
	})
	t.Run("folds progress and keeps the tail", func(t *testing.T) {
		var b strings.Builder
		for i := range 40 {
			b.WriteString("line ")
			b.WriteString(strings.Repeat("x", i))
			b.WriteString("\n")
		}
		b.WriteString("Receiving objects: 1%\rReceiving objects: 99%\r\n")
		got := stderrDetail([]byte(b.String()), false, "")
		lines := strings.Split(got, "\n")
		if len(lines) != maxDetailLines || lines[len(lines)-1] != "Receiving objects: 99%" {
			t.Fatalf("detail = %q", got)
		}
	})
	t.Run("bounded valid UTF-8", func(t *testing.T) {
		got := stderrDetail([]byte(strings.Repeat("é", 2000)+"\xff\n"), false, "")
		if len(got) > maxDetailBytes || !utf8.ValidString(got) {
			t.Fatalf("detail has %d bytes, valid %v", len(got), utf8.ValidString(got))
		}
	})
}

func TestCaptureTail(t *testing.T) {
	c := &capture{tail: true}
	for range 100 {
		_, _ = c.Write([]byte(strings.Repeat("a", 1000) + "\n"))
	}
	_, _ = c.Write([]byte("end"))
	data, dropped := c.bytes()
	if !dropped || len(data) != maxCapture || !strings.HasSuffix(string(data), "\nend") {
		t.Fatalf("tail has %d bytes, dropped %v", len(data), dropped)
	}
	head := &capture{}
	_, _ = head.Write(make([]byte, maxCapture+10))
	if data, dropped := head.bytes(); !dropped || len(data) != maxCapture {
		t.Fatalf("head has %d bytes, dropped %v", len(data), dropped)
	}
}
