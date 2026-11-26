package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/EricWvi/circus/internal/testutil/diskimage"
)

// A volume mounted in the workspace once the walk has read its first names
// is reached only after those entries were removed: the walk stops at the
// volume, its files stay, and the result says what was removed and what
// remains.
func TestRemoveStopsAtAVolumeAfterOtherEntries(t *testing.T) {
	home, workspace := homeWithWorkspace(t)
	volume := filepath.Join(workspace, "volume")
	r := NewRemove(Options{Home: home})
	mounted := false
	r.afterRead = func(dir string) {
		if dir == "p" && !mounted {
			mounted = true
			diskimage.Mount(t, "HFS+", "8m", volume)
			write(t, filepath.Join(volume, "file.txt"), "keep")
		}
	}
	result := runRemove(t, r, context.Background())
	if !mounted {
		t.Fatal("the walk read no names of the workspace")
	}
	if result.Error != errIO || !strings.Contains(result.ErrorDetail, "is a mount point") ||
		!strings.Contains(result.ErrorDetail, "removed 3 entries before that; the rest remains in "+workspace) {
		t.Fatalf("removal = %v", result)
	}
	for _, gone := range []string{"a", "b"} {
		if _, err := os.Lstat(filepath.Join(workspace, gone)); !os.IsNotExist(err) {
			t.Errorf("%s was not removed: %v", gone, err)
		}
	}
	if _, err := os.Stat(filepath.Join(volume, "file.txt")); err != nil {
		t.Fatalf("a file on the volume is gone: %v", err)
	}
}
