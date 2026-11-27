package snapshot

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExporterRefusesPaths(t *testing.T) {
	for name, paths := range map[string][]string{
		"absolute":             {"/etc/passwd"},
		"parent":               {"../escape.md"},
		"inner parent":         {"a/../../escape.md"},
		"dot":                  {"./a.md"},
		"empty segment":        {"a//b.md"},
		"invalid UTF-8":        {"a\xff.md"},
		"control":              {"a\x01.md"},
		"duplicate":            {"a.md", "a.md"},
		"out of order":         {"b.md", "a.md"},
		"folded files":         {"A.md", "a.md"},
		"folded directories":   {"Core/a.md", "core/b.md"},
		"folded deeper":        {"x/Core/a.md", "x/core/b.md"},
		"normalization":        {"café/a.md", "café/b.md"},
		"file then directory":  {"a", "a/b.md"},
		"directory then file":  {"a/b.md", "a/b.md/c.md"},
		"folded file then dir": {"A", "a/b.md"},
		"folded dir then file": {"A/b.md", "a"},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			e := NewExporter(openDir(t, root))
			var err error
			for _, p := range paths {
				if err = e.Add(p, "x"); err != nil {
					break
				}
			}
			if !errors.Is(err, ErrInvalidPath) {
				t.Fatalf("Add(%q) = %v", paths, err)
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(root), "escape.md")); err == nil {
				t.Fatal("wrote outside the directory")
			}
		})
	}
	e := NewExporter(openDir(t, t.TempDir()))
	if err := e.Add("a.md", strings.Repeat("x", MaxFileBytes+1)); err == nil {
		t.Fatal("a file over the bound was written")
	}
	if err := e.Add("b.md", "\xff"); err == nil {
		t.Fatal("invalid UTF-8 content was written")
	}
}

func TestExporterWrites(t *testing.T) {
	root := t.TempDir()
	e := NewExporter(openDir(t, root))
	files := map[string]string{"D0-A.md": "a", "INDEX.md": "index", "core/D1-B.md": "b", "core/net/D2-C.md": "c", "x/D3-D.md": "d"}
	for _, p := range []string{"D0-A.md", "INDEX.md", "core/D1-B.md", "core/net/D2-C.md", "x/D3-D.md"} {
		if err := e.Add(p, files[p]); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.Sync(); err != nil {
		t.Fatal(err)
	}
	for p, want := range files {
		if data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(p))); err != nil || string(data) != want {
			t.Fatalf("%s = %q, %v", p, data, err)
		}
	}
	// A file never replaces anything, not even one written meanwhile.
	put(t, root, "y.md", "theirs")
	if err := e.Add("y.md", "ours"); err == nil {
		t.Fatal("an existing file was replaced")
	}
}
