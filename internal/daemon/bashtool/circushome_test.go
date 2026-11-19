package bashtool

import (
	"path/filepath"
	"testing"
)

func TestCircusHomePaths(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "xdg-state"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "xdg-cache"))
	t.Setenv("CIRCUS_HOME", filepath.Join(root, "circus"))
	state, err := defaultStateDir(filepath.Join(root, "home"))
	if err != nil || state != filepath.Join(root, "circus", "state", "daemon") {
		t.Fatalf("state %q %v", state, err)
	}
	e := &outputStore{}
	if got := e.outputRoot(); got != filepath.Join(root, "circus", "cache", "output") {
		t.Fatalf("output root %q", got)
	}

	t.Setenv("CIRCUS_HOME", "")
	state, err = defaultStateDir(filepath.Join(root, "home"))
	if err != nil || state != filepath.Join(root, "xdg-state", "circus", "daemon") {
		t.Fatalf("XDG state %q %v", state, err)
	}
	if got := e.outputRoot(); got != filepath.Join(root, "xdg-cache", "circus", "output") {
		t.Fatalf("XDG output root %q", got)
	}
}
