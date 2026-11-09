//go:build linux || darwin

package fstools_test

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestMain(m *testing.M) {
	original := syscall.Umask(0o022)
	code := m.Run()
	syscall.Umask(original)
	os.Exit(code)
}

func TestNewFileModesIgnoreUmask(t *testing.T) {
	// Native uses 0644 files and 0755 directories even under a private umask.
	original := syscall.Umask(0o077)
	defer syscall.Umask(original)
	for _, tool := range []string{"Write", "Edit"} {
		t.Run(tool, func(t *testing.T) {
			cwd := tempDir(t)
			w := newWorker(t, cwd)
			r := w.call(t, tool, fixtureInput{Path: "private/file", Content: "created", New: "created"})
			if r.Output.IsError {
				t.Fatal(r)
			}
			for _, path := range []string{"private", "private/file"} {
				info, err := os.Stat(filepath.Join(cwd, path))
				if err != nil {
					t.Fatal(err)
				}
				want := os.FileMode(0o644)
				if info.IsDir() {
					want = 0o755
				}
				if info.Mode().Perm() != want {
					t.Errorf("%s mode: got %#o, want %#o", path, info.Mode().Perm(), want)
				}
			}
		})
	}
}
