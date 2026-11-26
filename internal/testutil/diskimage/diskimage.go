// Package diskimage mounts a fresh disk image for a test on macOS, a file
// system of its own (its own st_dev) that an unprivileged user can attach
// and detach.
package diskimage

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// Mount creates an empty disk image of size (such as "16m") with file
// system fs (an `hdiutil create -fs` name, such as "HFS+" or "MS-DOS"),
// attaches it at mountpoint, which must be an empty directory or absent,
// and detaches it when the test ends. It fails the test on any other
// system than macOS.
func Mount(t *testing.T, fs, size, mountpoint string) {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Fatalf("disk images are mounted with hdiutil, on macOS only")
	}
	image := filepath.Join(t.TempDir(), "volume.dmg")
	run(t, "hdiutil", "create", "-quiet", "-size", size, "-fs", fs, "-volname", "CIRCUSTEST", "-layout", "NONE", image)
	if err := os.MkdirAll(mountpoint, 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, "hdiutil", "attach", "-quiet", "-nobrowse", "-noverify", "-noautoopen", "-mountpoint", mountpoint, image)
	t.Cleanup(func() {
		if out, err := exec.Command("hdiutil", "detach", "-force", mountpoint).CombinedOutput(); err != nil {
			t.Errorf("hdiutil detach %s: %v\n%s", mountpoint, err, out)
		}
	})
}

func run(t *testing.T, name string, args ...string) {
	t.Helper()
	if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
}
