package dbtemplate

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// The template is built once and each seed is an independent copy.
func TestSeedCopiesOnce(t *testing.T) {
	var builds atomic.Int32
	tp := New(func(path string) error {
		builds.Add(1)
		return os.WriteFile(path, []byte("schema"), 0o600)
	})
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	tp.Seed(t, a)
	tp.Seed(t, b)
	if err := os.WriteFile(a, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(b); err != nil || string(got) != "schema" {
		t.Fatalf("second copy = %q, %v", got, err)
	}
	if n := builds.Load(); n != 1 {
		t.Fatalf("built %d times, want once", n)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("directory holds %d entries, want the two copies", len(entries))
	}
}

// Seed never replaces an existing file.
func TestSeedRefusesExistingFile(t *testing.T) {
	tp := New(func(path string) error { return os.WriteFile(path, []byte("schema"), 0o600) })
	path := filepath.Join(t.TempDir(), "db")
	if err := os.WriteFile(path, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := tp.seed(path); !errors.Is(err, os.ErrExist) {
		t.Fatalf("seed over an existing file = %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "mine" {
		t.Fatalf("existing file now holds %q", got)
	}
}

// A build that leaves more than the database file, such as an unmerged WAL,
// or an empty file, or that fails, is refused.
func TestBuildChecks(t *testing.T) {
	for name, build := range map[string]func(string) error{
		"extra file": func(path string) error {
			return errors.Join(os.WriteFile(path, []byte("schema"), 0o600), os.WriteFile(path+"-wal", []byte("frames"), 0o600))
		},
		"empty file": func(path string) error { return os.WriteFile(path, nil, 0o600) },
		"failure":    func(string) error { return errors.New("boom") },
	} {
		t.Run(name, func(t *testing.T) {
			tp := New(build)
			err := tp.seed(filepath.Join(t.TempDir(), "db"))
			if err == nil || !strings.Contains(err.Error(), "dbtemplate") {
				t.Fatalf("seed = %v", err)
			}
		})
	}
}
