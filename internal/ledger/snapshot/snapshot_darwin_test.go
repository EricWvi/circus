package snapshot

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/EricWvi/circus/internal/testutil/diskimage"
	"golang.org/x/sys/unix"
)

// syncFdOnVolume checks syncFd on a file and a directory of dir: whatever
// F_FULLFSYNC does there, the flush succeeds, through fsync when the file
// system does not support F_FULLFSYNC.
func syncFdOnVolume(t *testing.T, dir string) {
	put(t, dir, "file", "data")
	for _, name := range []string{"file", "."} {
		f, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		fd := int(f.Fd())
		_, full := unix.FcntlInt(uintptr(fd), unix.F_FULLFSYNC, 0)
		t.Logf("%s: F_FULLFSYNC = %v", name, full)
		if err := syncFd(fd); err != nil {
			t.Errorf("syncFd(%s) = %v", name, err)
		}
		_ = f.Close()
	}
}

func TestSyncFdFlushesOrFallsBack(t *testing.T) {
	t.Run("APFS", func(t *testing.T) { syncFdOnVolume(t, t.TempDir()) })
	t.Run("FAT", func(t *testing.T) {
		volume := filepath.Join(t.TempDir(), "volume")
		diskimage.Mount(t, "MS-DOS", "16m", volume)
		syncFdOnVolume(t, volume)
	})
}

// An error other than F_FULLFSYNC being unsupported is returned as it is,
// without trying fsync.
func TestSyncFdReportsOtherErrors(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	// A pipe is no vnode: F_FULLFSYNC refuses it with EBADF.
	if err := syncFd(int(w.Fd())); !errors.Is(err, unix.EBADF) {
		t.Errorf("syncFd(pipe) = %v, want EBADF", err)
	}
	f, err := os.Create(filepath.Join(t.TempDir(), "closed"))
	if err != nil {
		t.Fatal(err)
	}
	fd := int(f.Fd())
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := syncFd(fd); !errors.Is(err, unix.EBADF) {
		t.Errorf("syncFd(closed) = %v, want EBADF", err)
	}
}

// renameNoReplace keeps its promise on a FAT volume, whichever of its
// fallbacks the volume needs: msdosfs may lack renameatx_np's RENAME_EXCL
// and has no hard links.
func TestRenameNoReplaceOnFAT(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "volume")
	diskimage.Mount(t, "MS-DOS", "16m", dir)
	put(t, dir, "from", "from")
	put(t, dir, "to", "to")
	d := openDir(t, dir)
	fd := int(d.Fd())
	t.Logf("renameatx_np(RENAME_EXCL) onto an existing name: %v", renameExclusive(fd, "from", fd, "to"))
	if err := renameNoReplace(fd, "from", fd, "to"); !errors.Is(err, unix.EEXIST) {
		t.Fatalf("onto an existing file: %v", err)
	}
	if content(t, filepath.Join(dir, "from")) != "from" || content(t, filepath.Join(dir, "to")) != "to" {
		t.Fatal("a refused rename changed something")
	}
	if err := renameNoReplace(fd, "from", fd, "new"); err != nil {
		t.Fatal(err)
	}
	got := slices.DeleteFunc(names(t, dir), func(name string) bool { return name[0] == '.' })
	if !slices.Equal(got, []string{"new", "to"}) {
		t.Fatalf("after the rename: %v", got)
	}
	if content(t, filepath.Join(dir, "new")) != "from" {
		t.Fatal("the renamed file lost its content")
	}
}

// On APFS the exclusive rename itself refuses an existing name, and an
// unsupported flag is reported as errUnsupported, not as EINVAL.
func TestRenameExclusiveOnAPFS(t *testing.T) {
	dir := t.TempDir()
	put(t, dir, "from", "from")
	put(t, dir, "to", "to")
	d := openDir(t, dir)
	fd := int(d.Fd())
	if err := renameExclusive(fd, "from", fd, "to"); !errors.Is(err, unix.EEXIST) {
		t.Fatalf("onto an existing file: %v", err)
	}
	if err := renameExclusive(fd, "from", fd, "missing/new"); !errors.Is(err, unix.ENOENT) {
		t.Fatalf("into a missing directory: %v", err)
	}
	if err := renameExclusive(fd, "from", fd, "new"); err != nil {
		t.Fatal(err)
	}
}
