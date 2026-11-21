package workspace

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"golang.org/x/sys/unix"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func openDir(t *testing.T, path string) *os.File {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	dir, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dir.Close() })
	return dir
}

// homeWithWorkspace returns a home holding ~/circus/projects/p.
func homeWithWorkspace(t *testing.T) (home, workspace string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home = filepath.Join(root, "home")
	workspace = filepath.Join(home, "circus", "projects", "p")
	write(t, filepath.Join(workspace, "a", "file"), "x")
	write(t, filepath.Join(workspace, "b"), "x")
	return home, workspace
}

func runRemove(t *testing.T, r *Remove, ctx context.Context) *circusv1.RemoveWorkspaceResult {
	t.Helper()
	return r.run(ctx, &circusv1.RemoveWorkspaceRequest{Path: "~/circus/projects/p"})
}

func TestOpenChildDirRefusesSymlinks(t *testing.T) {
	root := t.TempDir()
	parent := openDir(t, root)
	if err := os.Mkdir(filepath.Join(root, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	// What the walk sees when a directory is swapped for a symlink after
	// its stat: the open itself must refuse it.
	for _, strict := range []bool{true, false} {
		fd, err := openChildDir(int(parent.Fd()), "link", strict)
		if !errors.Is(err, unix.ELOOP) {
			if err == nil {
				_ = unix.Close(fd)
			}
			t.Fatalf("strict=%v: opening a symlink = %v, want ELOOP", strict, err)
		}
		fd, err = openChildDir(int(parent.Fd()), "real", strict)
		if err != nil {
			t.Fatal(err)
		}
		_ = unix.Close(fd)
	}
	// The fallback used without openat2. With O_DIRECTORY, Linux reports
	// a symlink as ENOTDIR; the walk treats both alike.
	if fd, err := openSameDevice(int(parent.Fd()), "link"); !errors.Is(err, unix.ELOOP) && !errors.Is(err, unix.ENOTDIR) {
		if err == nil {
			_ = unix.Close(fd)
		}
		t.Fatalf("fallback opening a symlink = %v, want ELOOP or ENOTDIR", err)
	}
}

// TestLockRechecksIdentity: a locker that opened the file before its
// holder removed it must not keep the lock on the removed file, or two
// holders would coexist.
func TestLockRechecksIdentity(t *testing.T) {
	dir := openDir(t, t.TempDir())
	first, err := lockIn(dir, "w")
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	beforeFlock = func() { once.Do(first.release) }
	t.Cleanup(func() { beforeFlock = nil })
	second, err := lockIn(dir, "w")
	beforeFlock = nil
	if err != nil {
		t.Fatalf("second locker: %v", err)
	}
	defer second.release()
	if third, err := lockIn(dir, "w"); !errors.Is(err, errBusy) {
		if err == nil {
			third.release()
		}
		t.Fatalf("third locker = %v, want busy while the second holds the lock", err)
	}
}

// TestLockReleaseUnlocks: a child sharing the holder's open file
// description (a fork before exec) must not keep the lock after release.
func TestLockReleaseUnlocks(t *testing.T) {
	dir := openDir(t, t.TempDir())
	first, err := lockIn(dir, "w")
	if err != nil {
		t.Fatal(err)
	}
	shared, err := unix.Dup(first.fd)
	if err != nil {
		t.Fatal(err)
	}
	file := os.NewFile(uintptr(shared), "shared-lock")
	child := exec.Command("sleep", "30")
	child.ExtraFiles = []*os.File{file}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	t.Cleanup(func() {
		_ = child.Process.Kill()
		_ = child.Wait()
	})
	// The second locker opens the file before the holder releases it.
	var once sync.Once
	beforeFlock = func() { once.Do(first.release) }
	t.Cleanup(func() { beforeFlock = nil })
	second, err := lockIn(dir, "w")
	beforeFlock = nil
	if err != nil {
		t.Fatalf("second locker after release: %v", err)
	}
	second.release()
}

func TestRemoveRetriesWhileWritersAdd(t *testing.T) {
	t.Run("a few new entries", func(t *testing.T) {
		home, workspace := homeWithWorkspace(t)
		r := NewRemove(Options{Home: home})
		added := 0
		r.afterRead = func(dir string) {
			if dir == "p" && added < maxRefills {
				added++
				write(t, filepath.Join(workspace, "new"+string(rune('0'+added))), "x")
			}
		}
		if result := runRemove(t, r, context.Background()); result.Outcome != circusv1.RemoveWorkspaceResult_OUTCOME_REMOVED {
			t.Fatalf("removal = %v", result)
		}
	})
	t.Run("a writer that never stops", func(t *testing.T) {
		home, workspace := homeWithWorkspace(t)
		r := NewRemove(Options{Home: home})
		added := 0
		r.afterRead = func(dir string) {
			if dir == "p" {
				added++
				write(t, filepath.Join(workspace, "new"+string(rune('a'+added))), "x")
			}
		}
		result := runRemove(t, r, context.Background())
		if result.Error != errIO || !strings.Contains(result.ErrorDetail, "new entries kept appearing") || !strings.Contains(result.ErrorDetail, "the rest remains in "+workspace) {
			t.Fatalf("removal = %v", result)
		}
		if _, err := os.Stat(workspace); err != nil {
			t.Fatal(err)
		}
	})
}

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

func TestRemoveRestoresModes(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	t.Run("permission denied below", func(t *testing.T) {
		home, workspace := homeWithWorkspace(t)
		readOnly := filepath.Join(workspace, "ro")
		write(t, filepath.Join(readOnly, "locked", "file"), "x")
		locked := filepath.Join(readOnly, "locked")
		if err := os.Chmod(locked, 0); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(readOnly, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(readOnly, 0o755); _ = os.Chmod(locked, 0o755) })
		result := runRemove(t, NewRemove(Options{Home: home}), context.Background())
		if result.Error != errPermissionDenied || !strings.Contains(result.ErrorDetail, "the rest remains in") {
			t.Fatalf("removal = %v", result)
		}
		if got := mode(t, readOnly); got != 0o555 {
			t.Fatalf("read-only directory left with mode %v", got)
		}
	})
	t.Run("stopped between two batches", func(t *testing.T) {
		home, workspace := homeWithWorkspace(t)
		readOnly := filepath.Join(workspace, "ro")
		for i := range readBatch + 5 {
			write(t, filepath.Join(readOnly, "f"+strconv.Itoa(i)), "")
		}
		if err := os.Chmod(readOnly, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(readOnly, 0o755) })
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		r := NewRemove(Options{Home: home})
		rounds := 0
		r.beforeRound = func(dir string) {
			// Cancelled after the first full batch of "ro" is gone, before
			// its second round reopens it.
			if dir == "ro" {
				if rounds++; rounds == 2 {
					cancel()
				}
			}
		}
		result := runRemove(t, r, ctx)
		if result.Error != errIO || !strings.Contains(result.ErrorDetail, "cancelled") {
			t.Fatalf("removal = %v", result)
		}
		if got := mode(t, readOnly); got != 0o500 {
			t.Fatalf("read-only directory left with mode %v", got)
		}
	})
	t.Run("stopped by the deadline", func(t *testing.T) {
		home, workspace := homeWithWorkspace(t)
		readOnly := filepath.Join(workspace, "ro")
		write(t, filepath.Join(readOnly, "file"), "x")
		if err := os.Chmod(readOnly, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(readOnly, 0o755) })
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		r := NewRemove(Options{Home: home})
		r.afterRead = func(dir string) {
			if dir == "ro" {
				cancel()
			}
		}
		result := runRemove(t, r, ctx)
		if result.Error != errIO || !strings.Contains(result.ErrorDetail, "cancelled") {
			t.Fatalf("removal = %v", result)
		}
		if got := mode(t, readOnly); got != 0o500 {
			t.Fatalf("read-only directory left with mode %v", got)
		}
	})
}

func TestStartErrors(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	if _, err := startGit(filepath.Join(t.TempDir(), "no-git"), nil, t.TempDir(), false, "version"); !errors.Is(err, errGitUnavailable) {
		t.Fatalf("missing executable = %v", err)
	}
	notExecutable := filepath.Join(t.TempDir(), "git")
	write(t, notExecutable, "")
	if _, err := startGit(notExecutable, nil, t.TempDir(), false, "version"); !errors.Is(err, errGitUnavailable) {
		t.Fatalf("non-executable git = %v", err)
	}
	// The parent vanished between the lock and the clone.
	_, err = startGit(git, nil, filepath.Join(t.TempDir(), "gone"), false, "version")
	if err == nil || errors.Is(err, errGitUnavailable) || fsErrorCode(err) != errIO {
		t.Fatalf("missing working directory = %v (%v)", err, fsErrorCode(err))
	}
}
