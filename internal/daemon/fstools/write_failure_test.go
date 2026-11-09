package fstools

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestFailedRenameCleansUpTemporaryFile(t *testing.T) {
	if err := os.MkdirAll(".tmp", 0o777); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(".tmp", "failed-write-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
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
