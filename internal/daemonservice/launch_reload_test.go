package daemonservice

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == LaunchReloadFlag {
		os.Exit(RunLaunchReload(os.Args[2:], os.Stderr))
	}
	os.Exit(m.Run())
}

func TestDetachedLaunchReloadSurvivesInstallProcessGroup(t *testing.T) {
	const installChild = "CIRCUS_TEST_INSTALL_CHILD"
	if os.Getenv(installChild) == "1" {
		l := launchd{home: os.Getenv("HOME")}
		if err := l.install(fakeExecutable, []string{"daemon", "--service"}, nil, []byte("previous")); err != nil {
			t.Fatal(err)
		}
		return
	}
	f := fakeService(t, "darwin")
	f.response(t, "loaded", "")
	script := strings.Replace(fakeManager, "launchctl:bootout)\n", `launchctl:bootout)
        : > "$dir/bootout-entered"
        n=0
        while [ "$n" -lt 50 ]; do
            n=$((n+1))
            if [ -f "$dir/continue" ]; then break; fi
            /bin/sleep 0.1
        done
        if [ ! -f "$dir/continue" ]; then exit 1; fi
`, 1)
	// Wait for a release from the test while the parent is still in its
	// original process group and the detached child is in bootout.
	writeTestFile(t, filepath.Join(f.bin, "launchctl"), script, 0700)
	t.Cleanup(func() { _ = os.WriteFile(filepath.Join(f.dir, "continue"), nil, 0600) })
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestDetachedLaunchReloadSurvivesInstallProcessGroup$")
	cmd.Env = append(os.Environ(), installChild+"=1", "GORACE=atexit_sleep_ms=0")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	defer func() {
		if !waited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	waitForLaunchTestFile(t, ctx, filepath.Join(f.dir, "bootout-entered"))
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	err = cmd.Wait()
	waited = true
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ProcessState.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
		t.Fatalf("install parent was not killed: %v", err)
	}
	f.response(t, "continue", "")
	calls := filepath.Join(f.dir, "calls")
	for {
		data, err := os.ReadFile(calls)
		if err == nil && strings.Contains(string(data), "launchctl bootstrap "+launchDomain()) {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("detached bootstrap did not run: %s", data)
		case <-time.After(10 * time.Millisecond):
		}
	}
	waitForLaunchTestFile(t, ctx, filepath.Join(f.dir, "loaded"))
	if _, err := os.Stat(filepath.Join(f.home, "Library", "LaunchAgents", label+".plist")); err != nil {
		t.Fatal(err)
	}
}

func waitForLaunchTestFile(t *testing.T, ctx context.Context, path string) {
	t.Helper()
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for %s", path)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestDetachedLaunchReloadTimeoutRestoresOriginalService(t *testing.T) {
	f := fakeService(t, "darwin")
	l := launchd{home: f.home}
	const original = "original plist"
	if err := writePrivate(l.file(), original); err != nil {
		t.Fatal(err)
	}
	f.response(t, "loaded", "")
	script := strings.Replace(fakeManager, `launchctl:bootstrap) : > "$dir/loaded" ;;`, `launchctl:bootstrap)
        : > "$dir/loaded"
        if ! /bin/grep -q 'original plist' "$3"; then
            : > "$dir/replacement-bootstrapped"
            exec /bin/sleep 30
        fi
        ;;
`, 1)
	writeTestFile(t, filepath.Join(f.bin, "launchctl"), script, 0700)
	oldTimeout := launchReloadTimeout
	launchReloadTimeout = time.Second
	t.Cleanup(func() { launchReloadTimeout = oldTimeout })
	err := l.install(fakeExecutable, []string{"daemon", "--service"}, nil, []byte(original))
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "launchd reload timed out") {
		t.Fatalf("install error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "replacement-bootstrapped")); err != nil {
		t.Fatalf("reload did not reach bootstrap before timing out: %v", err)
	}
	if data := readPrivate(t, l.file()); data != original {
		t.Errorf("restored plist = %q", data)
	}
	print := "launchctl print " + launchTarget()
	bootout := "launchctl bootout " + launchTarget()
	bootstrap := "launchctl bootstrap " + launchDomain() + " " + l.file()
	f.checkCalls(t, print, bootout, print, bootstrap, print, bootout, print, bootstrap)
}
