package daemonlock

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

func TestLockOwnerAndRelease(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CIRCUS_HOME", "")
	t.Setenv("XDG_STATE_HOME", dir)
	if got, err := Holder(); err != nil || got != nil {
		t.Fatalf("initial owner = %v, %v", got, err)
	}
	first, err := Acquire(Foreground)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if first != nil {
			_ = first.Close()
		}
	}()
	if data, err := os.ReadFile(filepath.Join(dir, "circus", "daemon", "daemon.lock")); err != nil || string(data) != strconv.Itoa(os.Getpid())+" foreground\n" {
		t.Fatalf("owner record = %q, %v", data, err)
	}
	second, err := Acquire(Service)
	if second != nil {
		_ = second.Close()
		t.Fatal("a second daemon acquired the lock")
	}
	var held *HeldError
	if !errors.As(err, &held) || held.PID != os.Getpid() || held.Mode != Foreground {
		t.Fatalf("conflicting owner = %v", err)
	}
	if owner, err := Holder(); err != nil || owner == nil || *owner != *held {
		t.Fatalf("owner = %v, %v", owner, err)
	}
	t.Setenv("CIRCUS_HOME", filepath.Join(dir, "dev"))
	separate, err := Acquire(Service)
	if err != nil {
		t.Fatalf("development daemon with separate state: %v", err)
	}
	if err := separate.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CIRCUS_HOME", "")
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	first = nil
	if owner, err := Holder(); err != nil || owner != nil {
		t.Fatalf("released owner = %v, %v", owner, err)
	}
	second, err = Acquire(Service)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if owner, err := Holder(); err != nil || owner == nil || owner.Mode != Service {
		t.Fatalf("next owner = %v, %v", owner, err)
	}
}

func TestLockDescriptorIsCloseOnExec(t *testing.T) {
	if path := os.Getenv("CIRCUS_TEST_LOCK_PATH"); path != "" {
		fd, err := strconv.ParseUint(os.Getenv("CIRCUS_TEST_LOCK_FD"), 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		expected, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if file := os.NewFile(uintptr(fd), path); file != nil {
			if actual, err := file.Stat(); err == nil && os.SameFile(actual, expected) {
				t.Fatal("child inherited the daemon lock descriptor")
			}
		}
		return
	}
	t.Setenv("CIRCUS_HOME", t.TempDir())
	lock, err := Acquire(Service)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), exe, "-test.run=^TestLockDescriptorIsCloseOnExec$")
	cmd.Env = append(os.Environ(), "CIRCUS_TEST_LOCK_PATH="+lock.file.Name(), "CIRCUS_TEST_LOCK_FD="+strconv.FormatUint(uint64(lock.file.Fd()), 10))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child: %v\n%s", err, out)
	}
}

func TestLockFollowsSymlinkedStateDirectory(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CIRCUS_HOME", root)
	target := filepath.Join(root, "target")
	for _, dir := range []string{target, filepath.Join(root, "state")} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(target, filepath.Join(root, "state", "daemon")); err != nil {
		t.Fatal(err)
	}
	lock, err := Acquire(Foreground)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if _, err := os.Stat(filepath.Join(target, "daemon.lock")); err != nil {
		t.Fatal(err)
	}
	if owner, err := Holder(); err != nil || owner == nil || owner.Mode != Foreground {
		t.Fatalf("owner = %v, %v", owner, err)
	}
}
