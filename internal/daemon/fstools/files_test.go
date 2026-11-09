package fstools_test

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func digest(content string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(content))) }

func TestRelativePathsAndResultMetadata(t *testing.T) {
	cwd := tempDir(t)
	w := newWorker(t, cwd)
	path := "nested/a.txt"
	content := "alpha\r\nbeta\r\n"
	r := w.call(t, "Write", fixtureInput{Path: path, Content: content})
	if r.Output.IsError || r.GetWrite() == nil || !r.GetWrite().Created || r.GetWrite().Sha256 != digest(content) {
		t.Fatalf("write create metadata: %v", r)
	}
	r = w.call(t, "Read", fixtureInput{Path: path, Offset: 2, Limit: 1})
	info, err := os.Stat(filepath.Join(cwd, path))
	if err != nil {
		t.Fatal(err)
	}
	if r.Output.IsError || r.GetRead() == nil || r.GetRead().Sha256 != digest(content) || r.GetRead().TotalLines != 3 || r.GetRead().MtimeUnixMs != info.ModTime().UnixMilli() {
		t.Fatalf("whole-file metadata after ranged read: %v", r)
	}
	// Absolute and relative paths share the worker's read-state entry.
	putFile(t, filepath.Join(cwd, path), []byte(content+"external\r\n"))
	r = w.call(t, "Edit", fixtureInput{Path: filepath.Join(cwd, path), Old: "alpha\nbeta", New: "ALPHA\nBETA"})
	want := "ALPHA\r\nBETA\r\nexternal\r\n"
	if r.Output.IsError || r.GetEdit() == nil || r.GetEdit().Created || !r.GetEdit().ModifiedSinceRead || r.GetEdit().Sha256 != digest(want) {
		t.Fatalf("modified edit metadata: %v", r)
	}
	data, err := os.ReadFile(filepath.Join(cwd, path))
	if err != nil {
		t.Fatal(err)
	}
	compareText(t, "relative CRLF edit", want, string(data))
	r = w.call(t, "Write", fixtureInput{Path: "./" + path, Content: "stale overwrite"})
	if !r.Output.IsError {
		t.Fatalf("worker did not share read state between path spellings: %v", r)
	}
	// A new read advances the expected hash, permitting an ordinary overwrite.
	w.call(t, "Read", fixtureInput{Path: path})
	r = w.call(t, "Write", fixtureInput{Path: path, Content: "current"})
	if r.Output.IsError || r.GetWrite() == nil || r.GetWrite().Created || r.GetWrite().Sha256 != digest("current") {
		t.Fatalf("write update metadata: %v", r)
	}
}

func TestEditCreationAndEmptyReadMetadata(t *testing.T) {
	cwd := tempDir(t)
	w := newWorker(t, cwd)
	r := w.call(t, "Edit", fixtureInput{Path: "new/file", New: "created"})
	if r.Output.IsError || r.GetEdit() == nil || !r.GetEdit().Created || r.GetEdit().ModifiedSinceRead || r.GetEdit().Sha256 != digest("created") {
		t.Fatalf("edit create metadata: %v", r)
	}
	r = w.call(t, "Edit", fixtureInput{Path: "new/file", Old: "created", New: ""})
	if r.Output.IsError || r.GetEdit() == nil || r.GetEdit().Created || r.GetEdit().ModifiedSinceRead || r.GetEdit().Sha256 != digest("") {
		t.Fatalf("edit update metadata: %v", r)
	}
	r = w.call(t, "Read", fixtureInput{Path: "new/file"})
	if r.Output.IsError || r.GetRead() == nil || r.GetRead().TotalLines != 0 || r.GetRead().Sha256 != digest("") {
		t.Fatalf("empty read metadata: %v", r)
	}
}
