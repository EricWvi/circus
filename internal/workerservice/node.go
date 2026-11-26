package workerservice

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// MinNodeMajor is the oldest Node.js major release the worker runs on.
const MinNodeMajor = 24

// Node is the Node.js the worker service runs with.
type Node struct {
	// Path is the Node.js found, as found: Homebrew's bin/node and system
	// paths are stable links that follow upgrades. Only a path in a
	// per-shell or temporary location (fnm's multishell links,
	// $XDG_RUNTIME_DIR, the temporary directory), which may vanish, is
	// resolved to the executable it links to. A version-pinned path (nvm's)
	// stays as found; `circus worker status` says when it is gone.
	Path    string
	Version string
}

var nodeTimeout = 15 * time.Second

// FindNode finds Node.js as the worker's launcher does, $CIRCUS_NODE when
// set, else node on PATH, and checks that it is MinNodeMajor or later.
func FindNode(ctx context.Context) (Node, error) {
	name := os.Getenv("CIRCUS_NODE")
	if name == "" {
		name = "node"
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return Node{}, nodeRequired(fmt.Sprintf("%s not found", name))
	}
	if path, err = filepath.Abs(path); err != nil {
		return Node{}, err
	}
	if ephemeral(path) {
		real, err := filepath.EvalSymlinks(path)
		if err != nil {
			return Node{}, nodeRequired(fmt.Sprintf("%s: %v", path, err))
		}
		path = real
	}
	node, err := checkNode(ctx, path)
	if err != nil {
		return Node{}, nodeRequired(err.Error())
	}
	return node, nil
}

// ephemeral reports whether path lies in a directory that may vanish: a
// per-shell directory of fnm, $XDG_RUNTIME_DIR or the temporary directory.
func ephemeral(path string) bool {
	if slices.Contains(strings.Split(filepath.ToSlash(path), "/"), "fnm_multishells") {
		return true
	}
	for _, dir := range ephemeralDirs() {
		if dir == "" || !filepath.IsAbs(dir) {
			continue
		}
		for _, d := range []string{dir, resolved(dir)} {
			if rel, err := filepath.Rel(d, path); err == nil && rel != "." && filepath.IsLocal(rel) {
				return true
			}
		}
	}
	return false
}

// ephemeralDirs are the directories whose contents may vanish; tests,
// which live in the temporary directory, replace it.
var ephemeralDirs = func() []string { return []string{os.Getenv("XDG_RUNTIME_DIR"), os.TempDir()} }

func resolved(dir string) string {
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		return real
	}
	return dir
}

// checkNode runs the Node.js at path and checks its version.
func checkNode(ctx context.Context, path string) (Node, error) {
	ctx, cancel := context.WithTimeout(ctx, nodeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "-p", "process.versions.node")
	cmd.WaitDelay = time.Second
	data, err := cmd.Output()
	if err != nil {
		return Node{}, fmt.Errorf("%s did not report its version (%v)", path, err)
	}
	version := strings.TrimSpace(string(data))
	major, _, _ := strings.Cut(version, ".")
	if n, err := strconv.Atoi(major); err != nil || n < MinNodeMajor {
		return Node{}, fmt.Errorf("%s is version %q", path, version)
	}
	return Node{Path: path, Version: version}, nil
}

func nodeRequired(problem string) error {
	return fmt.Errorf("the circus worker needs Node.js %d or later: %s. Install Node.js %d or later, or set CIRCUS_NODE to its path, then run `circus worker install` again",
		MinNodeMajor, problem, MinNodeMajor)
}
