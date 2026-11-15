package fstools

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/EricWvi/circus/internal/testutil/leakcheck"
)

func TestFailedRenameCleansUpTemporaryFile(t *testing.T) {
	// Scratch lives under the repository's .tmp like the other tests, never
	// inside the package directory, so the suite also runs on a read-only tree.
	base, err := filepath.Abs("../../../.tmp")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(base, 0o777); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(base, "failed-write-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	leakcheck.Track(t, dir)
	// A directory cannot be replaced by the temporary regular file, so the
	// rename fails after the temporary file has been written and synced.
	target := filepath.Join(dir, "target")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFile(context.Background(), target, []byte("replacement"), before); err == nil {
		t.Fatal("expected rename over directory to fail")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "target" {
		t.Fatalf("temporary file left behind: %v", entries)
	}
	after, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) || !after.IsDir() {
		t.Fatal("failed write changed the original target")
	}
}
