package workspace_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/EricWvi/circus/internal/testutil/diskimage"
)

// macOS has no openat2: the removal walk opens each directory with
// O_NOFOLLOW and refuses one whose st_dev differs from its parent's. A
// disk image mounted in or as the workspace is another device.

func TestRemoveRefusesAVolumeInsideTheWorkspace(t *testing.T) {
	f := newFixture(t)
	volume := filepath.Join(f.target(), "volume")
	diskimage.Mount(t, "HFS+", "8m", volume)
	mustWrite(t, filepath.Join(volume, "keep", "file.txt"), "keep")
	r := f.removeNow(request)
	expectRemoval(t, r, removeFailed, refused)
	if !strings.Contains(r.ErrorDetail, "nothing was removed") {
		t.Errorf("detail %q", r.ErrorDetail)
	}
	if !exists(t, filepath.Join(volume, "keep", "file.txt")) {
		t.Fatal("a file on the volume is gone")
	}
}

func TestRemoveRefusesAWorkspaceThatIsAVolume(t *testing.T) {
	f := newFixture(t)
	diskimage.Mount(t, "HFS+", "8m", f.target())
	mustWrite(t, filepath.Join(f.target(), "file.txt"), "keep")
	r := f.removeNow(request)
	expectRemoval(t, r, removeFailed, refused)
	if !strings.Contains(r.ErrorDetail, "nothing was removed") {
		t.Errorf("detail %q", r.ErrorDetail)
	}
	if !exists(t, filepath.Join(f.target(), "file.txt")) {
		t.Fatal("a file on the volume is gone")
	}
}
