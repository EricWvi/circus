package workspace_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/EricWvi/circus/internal/daemon/bashtool"
	"github.com/EricWvi/circus/internal/daemon/workspace"
	"github.com/EricWvi/circus/internal/testutil/leakcheck"
)

// TestBashEnvironment runs git with the real Bash tool's environment: the
// login shell's exports reach git, repository-locating ones do not.
func TestBashEnvironment(t *testing.T) {
	var bash *bashtool.Bash
	f := newFixture(t, func(o *workspace.Options) {
		state := filepath.Join(filepath.Dir(o.Home), "state")
		bash = bashtool.NewBash(o.Jobs, bashtool.Options{Home: o.Home, StateDir: state, TempDir: filepath.Dir(o.Home), Shell: "/bin/bash"})
		o.Environment = bash
	})
	leakcheck.StateDir(t, filepath.Join(f.root, "state"))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := bash.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	profile := "export CIRCUS_FROM_LOGIN=yes\nexport GIT_DIR=/nonexistent GIT_WORK_TREE=/nonexistent GIT_TERMINAL_PROMPT=1\n" +
		"export CIRCUS_TEST_CTL=" + f.ctl + " GIT_CONFIG_NOSYSTEM=1\n"
	mustWrite(t, filepath.Join(f.home, ".bash_profile"), profile)
	url := f.bareRepo("one")
	expectReady(t, f.prepareNow(request, url), cloned)
	data, err := os.ReadFile(filepath.Join(f.ctl, "env"))
	if err != nil {
		t.Fatal(err)
	}
	env := strings.Split(strings.TrimSpace(string(data)), "\n")
	for _, want := range []string{"CIRCUS_FROM_LOGIN=yes", "GIT_TERMINAL_PROMPT=0", "GIT_EDITOR=true", "LC_ALL=C", "HOME=" + f.home} {
		if !slices.Contains(env, want) {
			t.Errorf("environment lacks %s: %v", want, env)
		}
	}
	for _, entry := range env {
		if strings.HasPrefix(entry, "GIT_DIR=") || strings.HasPrefix(entry, "GIT_WORK_TREE=") || entry == "GIT_TERMINAL_PROMPT=1" {
			t.Errorf("environment keeps %s", entry)
		}
	}
}
