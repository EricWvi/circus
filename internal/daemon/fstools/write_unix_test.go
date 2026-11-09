//go:build linux || darwin

package fstools_test

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestWritesPreserveLinks(t *testing.T) {
	for _, tool := range []string{"Write", "Edit"} {
		for _, kind := range []string{"symlink", "hardlink"} {
			t.Run(tool+"/"+kind, func(t *testing.T) {
				cwd := tempDir(t)
				target, link := filepath.Join(cwd, "target"), filepath.Join(cwd, "link")
				putFile(t, target, []byte("original content"))
				if err := os.Chmod(target, 0o751); err != nil {
					t.Fatal(err)
				}
				var err error
				if kind == "symlink" {
					err = os.Symlink("target", link)
				} else {
					err = os.Link(target, link)
				}
				if err != nil {
					t.Fatal(err)
				}
				before, err := os.Stat(target)
				if err != nil {
					t.Fatal(err)
				}
				linkBefore, err := os.Lstat(link)
				if err != nil {
					t.Fatal(err)
				}
				w := newWorker(t, cwd)
				r := w.call(t, tool, fixtureInput{Path: "link", Content: "updated", Old: "original content", New: "updated"})
				if r.Output.IsError {
					t.Fatal(r)
				}
				for _, path := range []string{target, link} {
					after, err := os.Stat(path)
					if err != nil {
						t.Fatal(err)
					}
					if !os.SameFile(before, after) || after.Mode() != before.Mode() {
						t.Fatalf("%s inode or mode changed: before=%v after=%v", path, before, after)
					}
					ownerBefore, ownerAfter := before.Sys().(*syscall.Stat_t), after.Sys().(*syscall.Stat_t)
					if ownerBefore.Uid != ownerAfter.Uid || ownerBefore.Gid != ownerAfter.Gid {
						t.Fatalf("%s owner changed", path)
					}
					data, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					compareText(t, path, "updated", string(data))
				}
				linkAfter, err := os.Lstat(link)
				if err != nil {
					t.Fatal(err)
				}
				if !os.SameFile(linkBefore, linkAfter) {
					t.Fatal("link inode changed")
				}
				if kind == "symlink" {
					if destination, err := os.Readlink(link); err != nil || destination != "target" {
						t.Fatalf("symlink changed: %q, %v", destination, err)
					}
				}
			})
		}
	}
}

func TestNewFileModesRespectUmask(t *testing.T) {
	// Umask is process-wide. Read and restore it before starting any dispatcher;
	// this test and the other file-writing tests must not run in parallel.
	mask := syscall.Umask(0)
	syscall.Umask(mask)
	for _, tool := range []string{"Write", "Edit"} {
		t.Run(tool, func(t *testing.T) {
			cwd := tempDir(t)
			w := newWorker(t, cwd)
			r := w.call(t, tool, fixtureInput{Path: "parent/nested/file", Content: "created", New: "created"})
			if r.Output.IsError {
				t.Fatal(r)
			}
			for _, path := range []string{"parent", "parent/nested", "parent/nested/file"} {
				info, err := os.Stat(filepath.Join(cwd, path))
				if err != nil {
					t.Fatal(err)
				}
				mode := os.FileMode(0o666 &^ mask)
				if info.IsDir() {
					mode = os.FileMode(0o777 &^ mask)
				}
				if info.Mode().Perm() != mode {
					t.Errorf("%s mode = %#o, want %#o (umask %#o)", path, info.Mode().Perm(), mode, mask)
				}
			}
		})
	}
}
