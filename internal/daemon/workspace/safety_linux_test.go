package workspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// withoutOpenat2 simulates a kernel or seccomp profile without a usable
// openat2: the probe reports it unavailable and the call fails with errno.
func withoutOpenat2(t *testing.T, errno unix.Errno) {
	savedCall, savedProbe := openat2, mountCheckAvailable
	openat2 = func(int, string, *unix.OpenHow) (int, error) { return -1, errno }
	mountCheckAvailable = sync.OnceValue(probeOpenat2)
	t.Cleanup(func() { openat2, mountCheckAvailable = savedCall, savedProbe })
}

func TestOpenat2Probe(t *testing.T) {
	if !probeOpenat2() {
		t.Skip("openat2 is unavailable here; the probe's positive case cannot run")
	}
	for _, errno := range []unix.Errno{unix.ENOSYS, unix.EPERM, unix.EACCES} {
		withoutOpenat2(t, errno)
		if mountCheckAvailable() {
			t.Fatalf("probe with %v reports openat2 usable", errno)
		}
	}
}

func TestRemoveRefusesWithoutOpenat2(t *testing.T) {
	// ENOSYS: a kernel before 5.6. EPERM: a seccomp profile blocking it.
	for _, errno := range []unix.Errno{unix.ENOSYS, unix.EPERM} {
		t.Run(errno.Error(), func(t *testing.T) {
			withoutOpenat2(t, errno)
			home, workspace := homeWithWorkspace(t)
			leftover := filepath.Join(filepath.Dir(workspace), ".p.circus-partial-0123456789abcdef")
			write(t, filepath.Join(leftover, "x"), "x")
			result := runRemove(t, NewRemove(Options{Home: home}), context.Background())
			if result.Error != errRefused || !strings.Contains(result.ErrorDetail, "openat2") || !strings.Contains(result.ErrorDetail, "nothing was removed") {
				t.Fatalf("removal = %v", result)
			}
			// Nothing deleted, leftover partial clones included.
			for _, path := range []string{filepath.Join(workspace, "a", "file"), filepath.Join(leftover, "x")} {
				if _, err := os.Stat(path); err != nil {
					t.Fatal(err)
				}
			}
			// Partial clones, which are ours, fall back to comparing devices.
			projects := openDir(t, filepath.Dir(workspace))
			if err := removeLeftovers(context.Background(), projects, "p"); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(leftover); !os.IsNotExist(err) {
				t.Fatalf("fallback cleanup left %v", err)
			}
		})
	}
}

const deathChildEnv = "CIRCUS_TEST_DEATH_CHILD"

// TestDeathSignalChild starts a git stand-in and exits without cleanup,
// like a crashing daemon.
func TestDeathSignalChild(t *testing.T) {
	pidFile := os.Getenv(deathChildEnv)
	if pidFile == "" {
		t.Skip("only runs as the child of TestGitDiesWithDaemon")
	}
	if _, err := startGit("/bin/sh", nil, "/", false, "-c", `echo $$ > "$0.tmp" && mv "$0.tmp" "$0" && exec sleep 1000`, pidFile); err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(pidFile); err == nil {
			os.Exit(0)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestGitDiesWithDaemon(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(t.TempDir(), "pid")
	child := exec.Command(executable, "-test.run=^TestDeathSignalChild$", "-test.count=1")
	child.Env = append(os.Environ(), deathChildEnv+"="+pidFile)
	if out, err := child.CombinedOutput(); err != nil {
		t.Fatalf("child: %v\n%s", err, out)
	}
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	var pid int
	for _, c := range strings.TrimSpace(string(data)) {
		pid = pid*10 + int(c-'0')
	}
	// The orphan is reparented to this test process (a subreaper under
	// leakcheck) or to init; either way it must be dead, not sleeping.
	deadline := time.Now().Add(10 * time.Second)
	for {
		stat, err := os.ReadFile("/proc/" + strings.TrimSpace(string(data)) + "/stat")
		if err != nil {
			break // reaped
		}
		if fields := strings.Fields(string(stat[strings.LastIndexByte(string(stat), ')')+1:])); len(fields) > 0 && fields[0] == "Z" {
			var status syscall.WaitStatus
			_, _ = syscall.Wait4(pid, &status, syscall.WNOHANG, nil)
			if !status.Signaled() && status != 0 {
				t.Fatalf("orphan ended %v", status)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("git stand-in %d outlived its daemon", pid)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
