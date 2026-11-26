package npmstub

import (
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// TestStub: a package is looked up in a lockfile, its verified copy found
// in the cache directory, and served at its registry path only; a corrupt
// copy is refused, and Corrupt serves other bytes of the same length.
func TestStub(t *testing.T) {
	dir := t.TempDir()
	body := []byte("not really a tarball")
	sum := sha512.Sum512(body)
	integrity := "sha512-" + base64.StdEncoding.EncodeToString(sum[:])
	lock := `{"packages":{"node_modules/@scope/pkg":{"resolved":"https://registry.example/@scope/pkg/-/pkg-1.0.0.tgz","integrity":"` + integrity + `"}}}`
	must(t, os.WriteFile(filepath.Join(dir, "package-lock.json"), []byte(lock), 0o600))
	p, err := Lookup(filepath.Join(dir, "package-lock.json"), "@scope/pkg")
	must(t, err)
	if p.Path != "/@scope/pkg/-/pkg-1.0.0.tgz" {
		t.Fatalf("path %q", p.Path)
	}
	if _, err := Lookup(filepath.Join(dir, "package-lock.json"), "@scope/other"); err == nil {
		t.Fatal("found a missing package")
	}
	cache := filepath.Join(dir, "cache")
	must(t, os.MkdirAll(cache, 0o700))
	copyPath := filepath.Join(cache, hex.EncodeToString(sum[:])+".tgz")
	must(t, os.WriteFile(copyPath, body, 0o600))
	file, err := Tarball(p, cache)
	must(t, err)
	if file != copyPath {
		t.Fatalf("tarball %q", file)
	}

	s := Start(p, file)
	defer s.Close()
	get := func(path string) (int, []byte) {
		t.Helper()
		resp, err := http.Get(s.URL + path)
		must(t, err)
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		must(t, err)
		return resp.StatusCode, data
	}
	if code, data := get(p.Path); code != http.StatusOK || string(data) != string(body) {
		t.Fatalf("GET = %d %q", code, data)
	}
	if code, _ := get("/@scope/pkg"); code != http.StatusNotFound {
		t.Fatalf("other path = %d", code)
	}
	s.Corrupt(true)
	if code, data := get(p.Path); code != http.StatusOK || len(data) != len(body) || string(data) == string(body) {
		t.Fatalf("corrupt GET = %d %q", code, data)
	}
	if s.Gets() != 2 {
		t.Fatalf("gets %d", s.Gets())
	}

	// A copy that does not match is not used (and the download fails
	// against the unreachable registry).
	must(t, os.WriteFile(copyPath, []byte("tampered"), 0o600))
	p.Resolved = s.URL + "/missing.tgz"
	if _, err := Tarball(p, cache); err == nil {
		t.Fatal("a tampered copy was accepted")
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
