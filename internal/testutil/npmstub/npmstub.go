// Package npmstub is a local npm registry for tests: it serves the Claude
// Code platform package tarball the TypeScript worker installs (the
// worker's CIRCUS_NPM_REGISTRY points at it), so no test downloads from the
// network.
//
// The tarball is the one package-lock.json pins for this platform
// (@anthropic-ai/claude-agent-sdk-<os>-<arch>[-musl]): taken from the npm
// cache (content-v2, addressed by the lockfile's sha512 integrity), else
// from a copy kept under the repository's .tmp/, else downloaded once from
// the lockfile's resolved URL into that copy. Every source is checked
// against the integrity before it is served.
package npmstub

import (
	"bytes"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
)

// Package is one platform package of the lockfile.
type Package struct {
	// Name is the npm package name.
	Name string
	// Path is the tarball's path on the registry (the resolved URL's).
	Path string
	// Resolved is the lockfile's tarball URL.
	Resolved string
	// Integrity is the lockfile's "sha512-<base64>".
	Integrity string
}

// sum is the integrity's digest.
func (p Package) sum() ([]byte, error) {
	b64, ok := strings.CutPrefix(p.Integrity, "sha512-")
	if !ok {
		return nil, fmt.Errorf("npmstub: %s: integrity is not sha512", p.Name)
	}
	sum, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(sum) != sha512.Size {
		return nil, fmt.Errorf("npmstub: %s: malformed integrity", p.Name)
	}
	return sum, nil
}

// PlatformName is the platform package this machine's worker installs.
func PlatformName() (string, error) {
	arch := map[string]string{"amd64": "x64", "arm64": "arm64"}[runtime.GOARCH]
	if arch == "" || (runtime.GOOS != "linux" && runtime.GOOS != "darwin") {
		return "", fmt.Errorf("npmstub: no Claude Code package for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	name := "@anthropic-ai/claude-agent-sdk-" + runtime.GOOS + "-" + arch
	if musl, _ := filepath.Glob("/lib/ld-musl-*"); runtime.GOOS == "linux" && len(musl) > 0 {
		name += "-musl"
	}
	return name, nil
}

// Lookup reads package name from the lockfile at lockPath.
func Lookup(lockPath, name string) (Package, error) {
	data, err := os.ReadFile(lockPath)
	if err != nil {
		return Package{}, err
	}
	var lock struct {
		Packages map[string]struct {
			Resolved  string `json:"resolved"`
			Integrity string `json:"integrity"`
		} `json:"packages"`
	}
	if err := json.Unmarshal(data, &lock); err != nil {
		return Package{}, fmt.Errorf("npmstub: %s: %w", lockPath, err)
	}
	entry, ok := lock.Packages["node_modules/"+name]
	if !ok {
		return Package{}, fmt.Errorf("npmstub: %s is not in %s", name, lockPath)
	}
	u, err := url.Parse(entry.Resolved)
	if err != nil || u.Path == "" {
		return Package{}, fmt.Errorf("npmstub: %s: bad resolved URL %q", name, entry.Resolved)
	}
	p := Package{Name: name, Path: u.EscapedPath(), Resolved: entry.Resolved, Integrity: entry.Integrity}
	if _, err := p.sum(); err != nil {
		return Package{}, err
	}
	return p, nil
}

// Tarball returns the path of a verified copy of p's tarball: the npm
// cache's file, else <cacheDir>/<sha512 hex>.tgz, downloading it there
// first when absent. Concurrent callers share one download (an flock on
// cacheDir).
func Tarball(p Package, cacheDir string) (string, error) {
	sum, err := p.sum()
	if err != nil {
		return "", err
	}
	digest := hex.EncodeToString(sum)
	if dir := npmCache(); dir != "" {
		path := filepath.Join(dir, "_cacache", "content-v2", "sha512", digest[:2], digest[2:4], digest[4:])
		if verify(path, sum) == nil {
			return path, nil
		}
	}
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return "", err
	}
	lock, err := os.OpenFile(filepath.Join(cacheDir, ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return "", err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return "", err
	}
	path := filepath.Join(cacheDir, digest+".tgz")
	if verify(path, sum) == nil {
		return path, nil
	}
	if err := download(p.Resolved, path, sum); err != nil {
		return "", fmt.Errorf("npmstub: %s: %w", p.Name, err)
	}
	return path, nil
}

// npmCache is npm's cache directory: `npm config get cache`, else ~/.npm.
func npmCache() string {
	if out, err := exec.Command("npm", "config", "get", "cache").Output(); err == nil {
		if dir := strings.TrimSpace(string(out)); filepath.IsAbs(dir) {
			return dir
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".npm")
	}
	return ""
}

func verify(path string, sum []byte) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha512.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if !bytes.Equal(h.Sum(nil), sum) {
		return errors.New("digest mismatch")
	}
	return nil
}

func download(from, path string, sum []byte) error {
	resp, err := http.Get(from)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", from, resp.Status)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".download-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	h := sha512.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), resp.Body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if !bytes.Equal(h.Sum(nil), sum) {
		return fmt.Errorf("GET %s: digest mismatch", from)
	}
	return os.Rename(tmp.Name(), path)
}

// Server is the stub registry.
type Server struct {
	*httptest.Server
	pkg     Package
	file    string
	corrupt atomic.Bool
	gets    atomic.Int64
}

// Start serves the tarball at file under p.Path; every other path is 404.
func Start(p Package, file string) *Server {
	s := &Server{pkg: p, file: file}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

// Corrupt makes the stub serve the tarball with its last byte changed
// (same length, another digest), or the real one again.
func (s *Server) Corrupt(on bool) { s.corrupt.Store(on) }

// Gets counts the tarball requests served.
func (s *Server) Gets() int64 { return s.gets.Load() }

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet || r.URL.EscapedPath() != s.pkg.Path {
		http.NotFound(w, r)
		return
	}
	s.gets.Add(1)
	if !s.corrupt.Load() {
		http.ServeFile(w, r, s.file)
		return
	}
	data, err := os.ReadFile(s.file)
	if err != nil || len(data) == 0 {
		http.Error(w, "unreadable tarball", http.StatusInternalServerError)
		return
	}
	data[len(data)-1] ^= 0xff
	w.Header().Set("Content-Length", fmt.Sprint(len(data)))
	_, _ = w.Write(data)
}
