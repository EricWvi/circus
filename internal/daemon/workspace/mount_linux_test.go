package workspace

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"golang.org/x/sys/unix"
)

const mountChildEnv = "CIRCUS_TEST_MOUNT_CHILD"

// TestMountNamespaceChild runs inside the namespace that
// TestRemoveStaysOffBindMounts sets up and prints what removal did.
func TestMountNamespaceChild(t *testing.T) {
	mode := os.Getenv(mountChildEnv)
	if mode == "" {
		t.Skip("only runs inside the mount namespace of TestRemoveStaysOffBindMounts")
	}
	home, name := os.Getenv("CIRCUS_TEST_HOME"), os.Getenv("CIRCUS_TEST_NAME")
	switch mode {
	case "remove":
		r := NewRemove(Options{Home: home})
		result := r.run(context.Background(), &circusv1.RemoveWorkspaceRequest{Path: "~/circus/projects/" + name})
		fmt.Printf("result: %s %s %q\n", result.Outcome, result.Error, result.ErrorDetail)
	case "walk":
		// The walk alone, without removal's mountinfo check.
		projects, err := os.Open(filepath.Join(home, "circus", "projects"))
		if err != nil {
			t.Fatal(err)
		}
		defer projects.Close()
		err = newRemoval(context.Background(), true).removeDir(int(projects.Fd()), name, 0, true)
		fmt.Printf("result: %v\n", err)
	case "fallback":
		// A kernel without openat2: partial clones fall back to st_dev,
		// which a mount of another filesystem (tmpfs here) does change.
		withoutOpenat2(t, unix.ENOSYS)
		inside := filepath.Join(home, "circus", "projects", name, "mnt", "inside.txt")
		if err := os.WriteFile(inside, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		projects, err := os.Open(filepath.Join(home, "circus", "projects"))
		if err != nil {
			t.Fatal(err)
		}
		defer projects.Close()
		err = newRemoval(context.Background(), false).removeDir(int(projects.Fd()), name, 0, true)
		_, statErr := os.Stat(inside)
		fmt.Printf("result: %v; inside: %v\n", err, statErr)
	}
}

// TestRemoveStaysOffBindMounts reproduces same-filesystem bind mounts,
// which share st_dev with their parent, in a user and mount namespace
// made by bwrap: one inside the workspace and the workspace itself.
func TestRemoveStaysOffBindMounts(t *testing.T) {
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		t.Skip("bwrap is not installed; this test needs a user and mount namespace")
	}
	if out, err := exec.Command(bwrap, "--unshare-user", "--dev-bind", "/", "/", "--", "true").CombinedOutput(); err != nil {
		t.Skipf("user namespaces are unavailable here, so bwrap cannot make the mount namespace this test needs "+
			"(disabled by kernel.unprivileged_userns_clone, an AppArmor userns restriction or a container): %v: %s", err, bytes.TrimSpace(out))
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, mode, mountAt, want string
	}{
		{"inside, removal", "remove", "p/mnt", `result: OUTCOME_FAILED WORKSPACE_ERROR_REFUSED "p/mnt is a mount point; nothing was removed"`},
		{"inside, walk", "walk", "p/mnt", "result: open p/mnt: is a mount point; nothing below it was removed"},
		{"workspace, removal", "remove", "p", `result: OUTCOME_FAILED WORKSPACE_ERROR_REFUSED "open p: is a mount point; nothing below it was removed; nothing was removed"`},
		{"workspace, walk", "walk", "p", "result: open p: is a mount point; nothing below it was removed"},
		{"other filesystem, st_dev fallback", "fallback", "tmpfs:p/mnt", "result: open p/mnt: is a mount point; nothing below it was removed; inside: <nil>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			mountAt, tmpfs := strings.CutPrefix(tc.mountAt, "tmpfs:")
			home := filepath.Join(root, "home")
			projects := filepath.Join(home, "circus", "projects")
			outside := filepath.Join(root, "outside")
			files := []string{filepath.Join(outside, "precious.txt"), filepath.Join(outside, "sub", "n.txt")}
			// The mount's siblings: removal refuses before deleting
			// anything, so they stay. The walk alone deletes entries in
			// directory order until it reaches the mount, so whether they
			// stay depends on the file system's order (tmpfs lists the
			// mount first, ext4 by hash); only the mount's files must.
			siblings := []string{filepath.Join(projects, "p", "keep.txt"), filepath.Join(projects, "p", "sub", "keep.txt")}
			if mountAt != "p/mnt" {
				siblings = nil
			}
			for _, file := range append(slices.Clone(files), siblings...) {
				if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			target := filepath.Join(projects, filepath.FromSlash(mountAt))
			if err := os.MkdirAll(target, 0o755); err != nil {
				t.Fatal(err)
			}
			var st, parent unix.Stat_t
			if unix.Stat(outside, &st) != nil || unix.Stat(filepath.Dir(target), &parent) != nil || st.Dev != parent.Dev {
				t.Fatal("the bind source must be on the workspace's filesystem for this test")
			}
			mount := []string{"--bind", outside, target}
			if tmpfs {
				mount = []string{"--tmpfs", target}
			}
			args := append(append([]string{"--unshare-user", "--die-with-parent", "--dev-bind", "/", "/"}, mount...), "--",
				executable, "-test.run=^TestMountNamespaceChild$", "-test.count=1")
			cmd := exec.Command(bwrap, args...)
			cmd.Env = append(os.Environ(), mountChildEnv+"="+tc.mode, "CIRCUS_TEST_HOME="+home, "CIRCUS_TEST_NAME=p")
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("child: %v\n%s", err, out)
			}
			if !strings.Contains(string(out), tc.want+"\n") {
				t.Fatalf("child output lacks %q:\n%s", tc.want, out)
			}
			if tc.mode == "remove" {
				files = append(files, siblings...)
			}
			for _, file := range files {
				if _, err := os.Stat(file); err != nil {
					t.Errorf("%s was removed: %v", file, err)
				}
			}
		})
	}
}
