package workerservice

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeNode writes a stand-in for Node.js that reports version.
func fakeNode(t *testing.T, dir, name, version string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	script := "#!/bin/sh\n[ \"$1 $2\" = '-p process.versions.node' ] || exit 9\necho " + version + "\n"
	if version == "" {
		script = "#!/bin/sh\nexit 3\n"
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

// useEphemeral makes dirs the only directories whose contents may vanish
// (the tests themselves live in the temporary directory).
func useEphemeral(t *testing.T, dirs ...string) {
	t.Helper()
	old := ephemeralDirs
	t.Cleanup(func() { ephemeralDirs = old })
	ephemeralDirs = func() []string { return dirs }
}

func TestFindNode(t *testing.T) {
	// Resolved, as FindNode resolves what it finds in an ephemeral
	// directory: on macOS the temporary directory is below /var, a link
	// to /private/var.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	useEphemeral(t, filepath.Join(dir, "run"))
	// Homebrew: bin/node links into the versioned Cellar, which an upgrade
	// replaces; the link follows it.
	fakeNode(t, filepath.Join(dir, "opt/homebrew/Cellar/node/24.1.0/bin"), "node", "24.1.0")
	homebrew := filepath.Join(dir, "opt/homebrew/bin/node")
	symlink(t, "../Cellar/node/24.1.0/bin/node", homebrew)
	// nvm: the versioned directory itself is on PATH.
	nvm := fakeNode(t, filepath.Join(dir, "home/.nvm/versions/node/v24.2.0/bin"), "node", "24.2.0")
	// fnm: a per-shell directory of links, removed with the shell.
	installation := filepath.Join(dir, "home/.local/share/fnm/node-versions/v25.0.0/installation")
	fnmReal := fakeNode(t, filepath.Join(installation, "bin"), "node", "25.0.0")
	multishell := filepath.Join(dir, "home/.local/state/fnm_multishells/4242_1764000000000")
	symlink(t, installation, multishell)
	fnm := filepath.Join(multishell, "bin", "node")
	// A link in $XDG_RUNTIME_DIR or the temporary directory.
	runtime := filepath.Join(dir, "run", "node")
	symlink(t, nvm, runtime)
	// A shim of a version manager (asdf, mise) stays: it is stable.
	shim := filepath.Join(dir, "home/.asdf/shims/node")
	if err := os.MkdirAll(filepath.Dir(shim), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(shim, []byte("#!/bin/sh\nexec "+nvm+" \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := fakeNode(t, dir, "node22", "22.11.0")
	broken := fakeNode(t, dir, "broken", "")
	t.Chdir(dir)
	for _, tc := range []struct {
		name, path, circusNode, wantPath, wantVersion, wantErr string
	}{
		{name: "Homebrew on PATH", path: filepath.Dir(homebrew), wantPath: homebrew, wantVersion: "24.1.0"},
		{name: "Homebrew in CIRCUS_NODE", circusNode: homebrew, wantPath: homebrew, wantVersion: "24.1.0"},
		{name: "nvm on PATH", path: filepath.Dir(nvm), wantPath: nvm, wantVersion: "24.2.0"},
		{name: "fnm on PATH", path: filepath.Dir(fnm), wantPath: fnmReal, wantVersion: "25.0.0"},
		{name: "fnm in CIRCUS_NODE", circusNode: fnm, wantPath: fnmReal, wantVersion: "25.0.0"},
		{name: "runtime directory", circusNode: runtime, wantPath: nvm, wantVersion: "24.2.0"},
		{name: "shim", path: filepath.Dir(shim), wantPath: shim, wantVersion: "24.2.0"},
		{name: "relative CIRCUS_NODE", circusNode: "./opt/homebrew/bin/node", wantPath: homebrew, wantVersion: "24.1.0"},
		{name: "CIRCUS_NODE command on PATH", path: filepath.Dir(nvm), circusNode: "node", wantPath: nvm, wantVersion: "24.2.0"},
		{name: "too old", circusNode: old, wantErr: old + ` is version "22.11.0"`},
		{name: "no version", circusNode: broken, wantErr: broken + " did not report its version"},
		{name: "missing", circusNode: filepath.Join(dir, "missing"), wantErr: filepath.Join(dir, "missing") + " not found"},
		{name: "nothing on PATH", path: filepath.Join(dir, "empty"), wantErr: "node not found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PATH", tc.path)
			t.Setenv("CIRCUS_NODE", tc.circusNode)
			node, err := FindNode(t.Context())
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) || !strings.Contains(err.Error(), "needs Node.js 24 or later") ||
					!strings.Contains(err.Error(), "set CIRCUS_NODE") {
					t.Fatalf("FindNode = %+v, %v; want %q", node, err, tc.wantErr)
				}
				return
			}
			if err != nil || node.Path != tc.wantPath || node.Version != tc.wantVersion {
				t.Fatalf("FindNode = %+v, %v; want %s %s", node, err, tc.wantPath, tc.wantVersion)
			}
		})
	}
}
