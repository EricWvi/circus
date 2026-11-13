package bashtool

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/EricWvi/circus/internal/testutil/leakcheck"
	"github.com/EricWvi/circus/internal/testutil/wait"
)

func unitTempDir(t *testing.T) string {
	t.Helper()
	base, err := filepath.Abs("../../../.tmp")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(base, 0700); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(base, "bash-unit-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	// Runs before the removal above and after cleanups registered later.
	leakcheck.Track(t, dir)
	return dir
}

func TestPrivateDirectories(t *testing.T) {
	t.Parallel()
	root := unitTempDir(t)
	real := filepath.Join(root, "real")
	if err := os.Mkdir(real, 0755); err != nil {
		t.Fatal(err)
	}
	if err := privateDir(real); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(real)
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("private mode: %v %v", info, err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if err := privateDir(link); err == nil {
		t.Fatal("accepted symlink")
	}
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := privateDir(file); err == nil {
		t.Fatal("accepted regular file")
	}
	if os.Getuid() != 0 {
		if err := privateDir(string(filepath.Separator)); err == nil {
			t.Fatal("accepted root-owned directory")
		}
	}
	if os.Getuid() == 0 {
		foreign := filepath.Join(root, "foreign")
		if err := os.Mkdir(foreign, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(foreign, 65534, 65534); err != nil {
			t.Fatal(err)
		}
		if err := privateDir(foreign); err == nil {
			t.Fatal("accepted foreign-owned directory")
		}
		info, err := os.Stat(foreign)
		if err != nil || info.Mode().Perm() != 0755 {
			t.Fatal("changed foreign directory permissions")
		}
	}
	e := &snapshotStore{options: snapshotOptions{Home: root, StateDir: link}}
	if _, _, _, err := e.load(context.Background()); err == nil {
		t.Fatal("accepted symlink state directory")
	}
	o := &outputStore{options: outputOptions{TempDir: root}}
	if err := os.Symlink(real, o.outputRoot()); err != nil {
		t.Fatal(err)
	}
	if _, err := o.outputFile("session", "tool-results", "out-*"); err == nil {
		t.Fatal("accepted symlink output root")
	}
}

func TestOutputRootDefaults(t *testing.T) {
	root := unitTempDir(t)
	home := filepath.Join(root, "home")
	t.Setenv("HOME", home)
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(root, "runtime"))
	e := &outputStore{}
	for _, cache := range []string{"", "relative-cache", filepath.Join(root, "xdg-cache")} {
		t.Setenv("XDG_CACHE_HOME", cache)
		want := filepath.Join(home, ".cache", "circus", "output")
		if filepath.IsAbs(cache) {
			want = filepath.Join(cache, "circus", "output")
		}
		if got := e.outputRoot(); got != want {
			t.Fatalf("XDG_CACHE_HOME=%q: got %s, want %s", cache, got, want)
		}
	}
	e.options.TempDir = root
	if got := e.outputRoot(); got != filepath.Join(root, fmtUID()) {
		t.Fatal("test override ignored", got)
	}
}

func TestCacheOutputDirectoriesArePrivate(t *testing.T) {
	root := unitTempDir(t)
	cache := filepath.Join(root, "cache")
	t.Setenv("XDG_CACHE_HOME", cache)
	e := &outputStore{options: outputOptions{Home: root}}
	b := &Bash{outputs: e}
	if err := os.MkdirAll(cache, 0700); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(root, "real")
	if err := os.Mkdir(real, 0700); err != nil {
		t.Fatal(err)
	}
	owned := filepath.Join(cache, "circus")
	if err := os.Symlink(real, owned); err != nil {
		t.Fatal(err)
	}
	if _, err := b.outputs.outputFile("session", "tasks", "out-*"); err == nil {
		t.Fatal("accepted a symlink at the owned cache parent")
	}
	if err := os.Remove(owned); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(owned, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, e.outputRoot()); err != nil {
		t.Fatal(err)
	}
	if _, err := b.outputs.outputFile("session", "tasks", "out-*"); err == nil {
		t.Fatal("accepted a symlink at the output root")
	}
	if err := os.Remove(e.outputRoot()); err != nil {
		t.Fatal(err)
	}
	out, err := b.outputs.outputFile("session", "tasks", "out-*")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	defer e.releaseOutput(out.Name())
	for _, path := range []string{owned, e.outputRoot(), filepath.Dir(filepath.Dir(out.Name())), filepath.Dir(out.Name())} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0700 {
			t.Fatalf("directory mode: %s: %v %v", path, info, err)
		}
	}
	if info, err := out.Stat(); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("file mode: %v %v", info, err)
	}
}

func fmtUID() string { return "circus-" + strconv.Itoa(os.Getuid()) }

func TestOutputRetention(t *testing.T) {
	t.Parallel()
	e := &outputStore{options: outputOptions{TempDir: unitTempDir(t)}}
	b := &Bash{outputs: e}
	dir := filepath.Join(e.outputRoot(), "session", "tasks")
	if err := privateDir(dir); err != nil {
		t.Fatal(err)
	}
	paths := []string{filepath.Join(dir, "ended"), filepath.Join(dir, "running"), filepath.Join(dir, "recent")}
	for _, path := range paths {
		if err := os.WriteFile(path, nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-25 * time.Hour)
	for _, path := range paths[:2] {
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	e.active = map[string]bool{paths[1]: true}
	out, err := b.outputs.outputFile("session", "tool-results", "test-*")
	if err != nil {
		t.Fatal(err)
	}
	_ = out.Close()
	e.releaseOutput(out.Name())
	await(t, func() bool { _, err := os.Stat(paths[0]); return os.IsNotExist(err) })
	for _, path := range paths[1:] {
		if _, err := os.Stat(path); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(paths[0], nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(paths[0], old, old); err != nil {
		t.Fatal(err)
	}
	out, err = b.outputs.outputFile("session", "tool-results", "test-*")
	if err != nil {
		t.Fatal(err)
	}
	_ = out.Close()
	e.releaseOutput(out.Name())
	if _, err := os.Stat(paths[0]); err != nil {
		t.Fatal("swept more than once per hour", err)
	}
	e.mu.Lock()
	e.lastSweep = time.Now().Add(-2 * time.Hour)
	e.mu.Unlock()
	e.releaseOutput(paths[1])
	out, err = b.outputs.outputFile("session", "tool-results", "test-*")
	if err != nil {
		t.Fatal(err)
	}
	_ = out.Close()
	e.releaseOutput(out.Name())
	for _, path := range paths[:2] {
		await(t, func() bool { _, err := os.Stat(path); return os.IsNotExist(err) })
	}
}

func TestStartupFileLifecycle(t *testing.T) {
	t.Parallel()
	root := unitTempDir(t)
	home, state := filepath.Join(root, "home"), filepath.Join(root, "state")
	for _, dir := range []string{home, filepath.Join(state, "instance-stale"), filepath.Join(state, "snapshot-legacy")} {
		if err := privateDir(dir); err != nil {
			t.Fatal(err)
		}
	}
	e := &snapshotStore{options: snapshotOptions{Home: home, StateDir: state, Shell: "/bin/bash"}}
	t.Cleanup(e.releaseInstance)
	load := func() string {
		t.Helper()
		snapshot, _, snapshotLease, err := e.load(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		snapshotLease.release()
		return snapshot
	}
	previous := load()
	if _, err := os.Stat(filepath.Join(state, "snapshot-legacy")); !os.IsNotExist(err) {
		t.Fatal("legacy snapshot retained", err)
	}
	if _, err := os.Stat(filepath.Join(state, "instance-stale")); !os.IsNotExist(err) {
		t.Fatal("stale snapshot retained")
	}
	for _, name := range []string{".bashrc", ".bash_profile", ".bash_login", ".profile"} {
		path := filepath.Join(home, name)
		for _, action := range []string{"appear", "mtime", "disappear"} {
			switch action {
			case "appear":
				if err := os.WriteFile(path, []byte("true\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "mtime":
				next := time.Now().Add(time.Hour)
				if err := os.Chtimes(path, next, next); err != nil {
					t.Fatal(err)
				}
			case "disappear":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			current := load()
			if current == previous {
				t.Fatalf("%s %s did not rebuild", name, action)
			}
			if _, err := os.Stat(filepath.Dir(previous)); !os.IsNotExist(err) {
				t.Fatalf("previous snapshot directory retained: %v", err)
			}
			if reused := load(); reused != current {
				t.Fatal("unchanged startup files rebuilt the snapshot")
			}
			previous = current
		}
	}
}

func TestBackgroundLifetimeBounds(t *testing.T) {
	for requested, want := range map[time.Duration]time.Duration{0: 30 * time.Minute, time.Minute: time.Minute, 3 * time.Hour: 2 * time.Hour} {
		if got := backgroundLifetime(requested); got != want {
			t.Fatalf("lifetime %v: %v, want %v", requested, got, want)
		}
	}
}

func TestShutdownReleasesInstance(t *testing.T) {
	t.Parallel()
	b, root, events := reviewBash(t, 0)
	job := reviewRun(t, b, root, `while [ ! -f release ]; do sleep 0.02; done`, true)
	if job.GetBash().JobId == "" {
		t.Fatal(job)
	}
	b.snapshots.mu.Lock()
	dir, lock := b.snapshots.instanceDir, b.snapshots.instanceLock
	b.snapshots.mu.Unlock()
	if err := b.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	// A visible job still sources and writes files in the instance.
	if _, err := os.Stat(dir); err != nil {
		t.Fatal("instance released while a background job uses it", err)
	}
	if _, err := lock.Stat(); err != nil {
		t.Fatal("instance lock closed while a background job uses it", err)
	}
	if err := os.WriteFile(filepath.Join(root, "release"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	reviewEvent(t, events, true)
	await(t, func() bool { _, err := os.Stat(dir); return os.IsNotExist(err) })
	if _, err := lock.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatal("instance lock still open after release", err)
	}
	// The snapshot store stays usable: the next load locks a new instance.
	t.Cleanup(b.snapshots.releaseInstance)
	snapshot, _, snapshotLease, err := b.snapshots.load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	snapshotLease.release()
	if next := filepath.Dir(filepath.Dir(snapshot)); next == dir || filepath.Dir(next) != filepath.Dir(dir) {
		t.Fatalf("reload used %s after releasing %s", next, dir)
	}
}

func await(t *testing.T, ready func() bool) {
	t.Helper()
	wait.For(t, ready)
}
