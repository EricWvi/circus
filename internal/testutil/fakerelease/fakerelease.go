// Package fakerelease serves circus releases over local HTTPS for tests:
// each release's latest.json and its circus-worker packages, laid out as on
// GitHub (<base>download/<tag>/<name>).
package fakerelease

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/EricWvi/circus/internal/selfupdate"
)

// File is one entry of a package, under circus-worker/ unless Raw.
type File struct {
	Name string
	// Raw takes Name as the whole entry name.
	Raw  bool
	Body string
	Mode int64
	// Type is tar.TypeReg when zero.
	Type     byte
	Linkname string
}

// Server serves releases. Its methods are safe for concurrent use.
type Server struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	files    map[string][]byte
	requests []string
	// override serves a path instead of the files.
	override map[string]http.HandlerFunc
}

// New starts a server, closed when the test ends.
func New(t *testing.T) *Server {
	s := &Server{t: t, files: make(map[string][]byte), override: make(map[string]http.HandlerFunc)}
	s.srv = httptest.NewTLSServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.requests = append(s.requests, r.URL.Path)
	handler := s.override[r.URL.Path]
	data, ok := s.files[r.URL.Path]
	s.mu.Unlock()
	switch {
	case handler != nil:
		handler(w, r)
	case ok:
		_, _ = w.Write(data)
	default:
		http.NotFound(w, r)
	}
}

// Base is the release base URL, ending in "/releases/".
func (s *Server) Base() string { return s.srv.URL + "/releases/" }

// Release reads this server's releases.
func (s *Server) Release() selfupdate.Release {
	return selfupdate.Release{BaseURL: s.Base(), Client: s.srv.Client(), Current: "test"}
}

// Path is the URL path of a release file.
func Path(version, name string) string { return "/releases/download/" + version + "/" + name }

// PackageName is the worker package's file name.
func PackageName(version, target string) string {
	return "circus-worker-" + version + "-" + target + ".tar.gz"
}

// Publish serves release version with a worker package for each target,
// holding files; the manifest lists their digests. It returns the archive.
func (s *Server) Publish(version string, targets []string, files []File) []byte {
	archive := Archive(s.t, files)
	s.PublishArchive(version, targets, archive)
	return archive
}

// PublishArchive serves release version with archive as its worker package
// for each target, listed in the manifest with archive's digest.
func (s *Server) PublishArchive(version string, targets []string, archive []byte) {
	sum := sha256.Sum256(archive)
	var assets []map[string]string
	for _, target := range targets {
		name := PackageName(version, target)
		assets = append(assets, map[string]string{
			"target": target, "name": name, "url": s.srv.URL + Path(version, name), "sha256": hex.EncodeToString(sum[:]),
		})
		s.Set(Path(version, name), archive)
	}
	manifest, err := json.Marshal(map[string]any{"version": version, "assets": []any{}, "worker_assets": assets})
	if err != nil {
		s.t.Fatal(err)
	}
	s.Set(Path(version, "latest.json"), manifest)
}

// Set serves data at path.
func (s *Server) Set(path string, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.files[path] = data
}

// Handle serves path with h instead of a file.
func (s *Server) Handle(path string, h http.HandlerFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.override[path] = h
}

// Requests returns the paths requested so far and forgets them.
func (s *Server) Requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := slices.Clone(s.requests)
	s.requests = nil
	return r
}

// Archive builds a worker package: a gzipped tar of circus-worker/ holding
// files. Directories are added for every parent.
func Archive(t *testing.T, files []File) []byte {
	t.Helper()
	var data bytes.Buffer
	gz := gzip.NewWriter(&data)
	tw := tar.NewWriter(gz)
	dirs := map[string]bool{}
	addDir := func(name string) {
		if dirs[name] {
			return
		}
		dirs[name] = true
		if err := tw.WriteHeader(&tar.Header{Name: name + "/", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
			t.Fatal(err)
		}
	}
	addDir("circus-worker")
	for _, f := range files {
		name := f.Name
		if !f.Raw {
			name = "circus-worker/" + name
		}
		if parts := strings.Split(name, "/"); len(parts) > 2 && parts[0] == "circus-worker" {
			for i := 2; i < len(parts); i++ {
				addDir(strings.Join(parts[:i], "/"))
			}
		}
		kind := f.Type
		if kind == 0 {
			kind = tar.TypeReg
		}
		mode := f.Mode
		if mode == 0 {
			mode = 0o644
		}
		h := &tar.Header{Name: name, Typeflag: kind, Mode: mode, Linkname: f.Linkname}
		if kind == tar.TypeReg {
			h.Size = int64(len(f.Body))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if kind == tar.TypeReg {
			if _, err := tw.Write([]byte(f.Body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}
