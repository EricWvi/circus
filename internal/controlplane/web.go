package controlplane

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

const (
	webIndex         = "index.html"
	webBackendMarker = `<meta name="circus-backend" content="server">`
)

type webHandler struct {
	ui    fs.FS
	etags map[string]string
}

// newWebHandler serves files from ui and falls back to index.html
// for SPA routes. API paths and missing assets or file names return 404.
func newWebHandler(ui fs.FS) (http.Handler, error) {
	h := &webHandler{ui: ui, etags: make(map[string]string)}
	err := fs.WalkDir(ui, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		data, err := fs.ReadFile(ui, name)
		if err != nil {
			return err
		}
		if name == webIndex && !bytes.Contains(data, []byte(webBackendMarker)) {
			return errors.New("web UI: index.html was not built for the control plane (build with VITE_CIRCUS_BACKEND=server)")
		}
		h.etags[name] = fmt.Sprintf(`"%x"`, sha256.Sum256(data))
		return nil
	})
	if err != nil {
		return nil, err
	}
	if h.etags[webIndex] == "" {
		return nil, errors.New("web UI: no regular index.html")
	}
	return h, nil
}

func (h *webHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	cleaned := path.Clean(r.URL.Path)
	if strings.HasPrefix(cleaned, "/circus.v1.") || strings.HasPrefix(cleaned, "/auth/") || strings.HasPrefix(cleaned, "/v1/") {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	name := strings.TrimPrefix(cleaned, "/")
	if name == "" {
		name = webIndex
	}
	if !fs.ValidPath(name) {
		http.NotFound(w, r)
		return
	}

	if h.serveFile(w, r, name) {
		return
	}
	if !strings.HasPrefix(name, "assets/") && path.Ext(name) == "" && h.serveFile(w, r, webIndex) {
		return
	}
	http.NotFound(w, r)
}

func (h *webHandler) serveFile(w http.ResponseWriter, r *http.Request, name string) bool {
	etag, ok := h.etags[name]
	if !ok {
		return false
	}
	file, err := h.ui.Open(name)
	if err != nil {
		return false
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	content, ok := file.(io.ReadSeeker)
	if !ok {
		data, err := io.ReadAll(file)
		if err != nil {
			return false
		}
		content = bytes.NewReader(data)
	}

	cache := "no-cache"
	if strings.HasPrefix(name, "assets/") {
		cache = "public, max-age=31536000, immutable"
	}
	w.Header().Set("Cache-Control", cache)
	w.Header().Set("ETag", etag)
	if name == webIndex {
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
	}
	http.ServeContent(w, r, name, info.ModTime(), content)
	return true
}
