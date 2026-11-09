package bashtool

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
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
	e := &environment{options: Options{Home: root, StateDir: link}}
	if _, _, err := e.load(context.Background()); err == nil {
		t.Fatal("accepted symlink state directory")
	}
	e = &environment{options: Options{TempDir: root}}
	if err := os.Symlink(real, e.outputRoot()); err != nil {
		t.Fatal(err)
	}
	if _, err := (&Bash{environment: e}).outputFile("session", "tool-results", "out-*"); err == nil {
		t.Fatal("accepted symlink output root")
	}
}

func TestOutputRootDefaults(t *testing.T) {
	root := unitTempDir(t)
	t.Setenv("XDG_RUNTIME_DIR", root)
	e := &environment{}
	if got := e.outputRoot(); got != filepath.Join(root, "circus") {
		t.Fatal(got)
	}
	t.Setenv("XDG_RUNTIME_DIR", "")
	if got := e.outputRoot(); got != filepath.Join(os.TempDir(), fmtUID()) {
		t.Fatal(got)
	}
}

func fmtUID() string { return "circus-" + strconv.Itoa(os.Getuid()) }

func TestOutputRetention(t *testing.T) {
	t.Parallel()
	e := &environment{options: Options{TempDir: unitTempDir(t)}}
	b := &Bash{environment: e}
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
	out, err := b.outputFile("session", "tool-results", "test-*")
	if err != nil {
		t.Fatal(err)
	}
	_ = out.Close()
	e.releaseOutput(out.Name())
	if _, err := os.Stat(paths[0]); !os.IsNotExist(err) {
		t.Fatalf("ended file retained: %v", err)
	}
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
	out, err = b.outputFile("session", "tool-results", "test-*")
	if err != nil {
		t.Fatal(err)
	}
	_ = out.Close()
	e.releaseOutput(out.Name())
	if _, err := os.Stat(paths[0]); err != nil {
		t.Fatal("swept more than once per hour", err)
	}
	e.lastSweep = time.Now().Add(-2 * time.Hour)
	e.releaseOutput(paths[1])
	out, err = b.outputFile("session", "tool-results", "test-*")
	if err != nil {
		t.Fatal(err)
	}
	_ = out.Close()
	e.releaseOutput(out.Name())
	for _, path := range paths[:2] {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("stale file survived lazy sweep: %s: %v", path, err)
		}
	}
}

func TestStartupFileLifecycle(t *testing.T) {
	t.Parallel()
	root := unitTempDir(t)
	home, state := filepath.Join(root, "home"), filepath.Join(root, "state")
	for _, dir := range []string{home, filepath.Join(state, "snapshot-stale")} {
		if err := privateDir(dir); err != nil {
			t.Fatal(err)
		}
	}
	e := &environment{options: Options{Home: home, StateDir: state, Shell: "/bin/bash"}}
	load := func() string {
		t.Helper()
		snapshot, _, err := e.load(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		e.releaseSnapshot(snapshot)
		return snapshot
	}
	previous := load()
	if _, err := os.Stat(filepath.Join(state, "snapshot-stale")); !os.IsNotExist(err) {
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

func TestPreviewDoesNotSplitAstral(t *testing.T) {
	t.Parallel()
	dir := unitTempDir(t)
	out, err := os.CreateTemp(dir, "preview-*")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = out.Close() }()
	if _, err := out.WriteString(strings.Repeat("a", 1999) + "😀" + strings.Repeat("z", 30000)); err != nil {
		t.Fatal(err)
	}
	body, err := formatOutput(out, &circusv1.BashResult{}, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, strings.Repeat("a", 1999)+"\n...\n") || strings.Contains(body, "😀") || strings.Contains(body, "�") {
		t.Fatal("preview split a surrogate pair")
	}
}

func TestBackgroundLifetimeBounds(t *testing.T) {
	for requested, want := range map[time.Duration]time.Duration{0: 30 * time.Minute, time.Minute: time.Minute, 3 * time.Hour: 2 * time.Hour} {
		if got := backgroundLifetime(requested); got != want {
			t.Fatalf("lifetime %v: %v, want %v", requested, got, want)
		}
	}
}
