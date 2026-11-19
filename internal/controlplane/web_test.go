package controlplane

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func TestWebHandler(t *testing.T) {
	const index = `<html><head><meta name="circus-backend" content="server"></head><body>circus</body></html>`
	const script = "console.log('circus')"
	const icon = "<svg/>"
	files := fstest.MapFS{
		"index.html":           {Data: []byte(index)},
		"assets/app-abc123.js": {Data: []byte(script)},
		"favicon.svg":          {Data: []byte(icon)},
		"subdir":               {Mode: fs.ModeDir},
	}
	h, err := newWebHandler(files)
	if err != nil {
		t.Fatal(err)
	}

	const missing = "404 page not found\n"
	const immutable = "public, max-age=31536000, immutable"
	for _, tc := range []struct {
		method, path, body string
		status             int
		media, cache       string
		frame              bool
	}{
		{"GET", "/", index, 200, "text/html", "no-cache", true},
		{"GET", "/index.html", index, 200, "text/html", "no-cache", true},
		{"GET", "/p/x/s/y", index, 200, "text/html", "no-cache", true},
		{"GET", "/devices", index, 200, "text/html", "no-cache", true},
		{"GET", "/login", index, 200, "text/html", "no-cache", true},
		{"GET", "/setup?token=abc", index, 200, "text/html", "no-cache", true},
		{"GET", "/assets/app-abc123.js", script, 200, "/javascript", immutable, false},
		{"GET", "/favicon.svg", icon, 200, "image/svg+xml", "no-cache", false},
		{"GET", "/assets/missing.js", missing, 404, "text/plain", "", false},
		{"GET", "/assets/missing", missing, 404, "text/plain", "", false},
		{"GET", "/missing.png", missing, 404, "text/plain", "", false},
		{"GET", "/circus.v1.Nope/Method", missing, 404, "text/plain", "", false},
		{"GET", "/v1/nope", missing, 404, "text/plain", "", false},
		{"GET", "/auth/nope", missing, 404, "text/plain", "", false},
		{"POST", "/circus.v1.X/Y", missing, 404, "text/plain", "", false},
		{"POST", "/v1/x", missing, 404, "text/plain", "", false},
		{"OPTIONS", "/auth/x", missing, 404, "text/plain", "", false},
		{"POST", "/p/x", "Method Not Allowed\n", 405, "text/plain", "", false},
		{"HEAD", "/", "", 200, "text/html", "no-cache", true},
		{"GET", "/../../etc/passwd", index, 200, "text/html", "no-cache", true},
		{"GET", "/subdir/", index, 200, "text/html", "no-cache", true},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
			got := w.Result()
			defer got.Body.Close()
			if got.StatusCode != tc.status || w.Body.String() != tc.body {
				t.Fatalf("response = %d %q, want %d %q", got.StatusCode, w.Body.String(), tc.status, tc.body)
			}
			media, _, _ := strings.Cut(got.Header.Get("Content-Type"), ";")
			if media != tc.media && !(tc.media == "/javascript" && strings.HasSuffix(media, tc.media)) {
				t.Errorf("Content-Type = %q, want %q", media, tc.media)
			}
			headers := map[string]string{
				"X-Content-Type-Options":  "nosniff",
				"Cache-Control":           tc.cache,
				"X-Frame-Options":         "",
				"Content-Security-Policy": "",
				"Allow":                   "",
			}
			if tc.frame {
				headers["X-Frame-Options"] = "DENY"
				headers["Content-Security-Policy"] = "frame-ancestors 'none'"
			}
			if tc.status == http.StatusMethodNotAllowed {
				headers["Allow"] = "GET, HEAD"
			}
			for header, want := range headers {
				if value := got.Header.Get(header); value != want {
					t.Errorf("%s = %q, want %q", header, value, want)
				}
			}
		})
	}

	for _, tc := range []struct{ path, body string }{
		{"/index.html", index},
		{"/devices", index},
		{"/assets/app-abc123.js", script},
	} {
		t.Run("revalidate "+tc.path, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, tc.path, nil)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			want := fmt.Sprintf(`"%x"`, sha256.Sum256([]byte(tc.body)))
			if got := w.Header().Get("ETag"); got != want {
				t.Fatalf("ETag = %q, want %q", got, want)
			}
			r.Header.Set("If-None-Match", want)
			w = httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusNotModified || w.Body.Len() != 0 || w.Header().Get("ETag") != want {
				t.Errorf("conditional response = %d %q %v", w.Code, w.Body, w.Header())
			}
		})
	}
}

func TestWebHandlerRejectsMockBuild(t *testing.T) {
	files := fstest.MapFS{"index.html": {Data: []byte("<html>mock build</html>")}}
	_, err := newWebHandler(files)
	const want = "web UI: index.html was not built for the control plane (build with VITE_CIRCUS_BACKEND=server)"
	if err == nil || err.Error() != want {
		t.Fatalf("newWebHandler error = %v, want %q", err, want)
	}
}

func TestWebHandlerRequiresIndex(t *testing.T) {
	for _, files := range []fstest.MapFS{{}, {"index.html": {Mode: fs.ModeDir}}} {
		if _, err := newWebHandler(files); err == nil || !strings.Contains(err.Error(), "no regular index.html") {
			t.Fatalf("newWebHandler(%v): %v", files, err)
		}
	}
}
