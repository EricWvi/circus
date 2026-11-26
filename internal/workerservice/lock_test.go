package workerservice

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/EricWvi/circus/internal/testutil/fakerelease"
)

// waitWriter notices when a command starts waiting for the store's lock.
type waitWriter struct {
	once    sync.Once
	waiting chan struct{}
}

func newWaitWriter() *waitWriter { return &waitWriter{waiting: make(chan struct{})} }

func (w *waitWriter) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte("Waiting for another install or update")) {
		w.once.Do(func() { close(w.waiting) })
	}
	return len(p), nil
}

// holdDownload makes the download of version's package wait for release,
// and reports when it started (the downloader then holds the lock).
func (f fixture) holdDownload(t *testing.T, version string) (started <-chan struct{}, release func()) {
	t.Helper()
	gate, begun := make(chan struct{}), make(chan struct{})
	archive := fakerelease.Archive(t, packageFiles(version))
	var once sync.Once
	f.srv.Handle(fakerelease.Path(version, fakerelease.PackageName(version, testTarget)), func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(begun) })
		<-gate
		_, _ = w.Write(archive)
	})
	var closeOnce sync.Once
	release = func() { closeOnce.Do(func() { close(gate) }) }
	t.Cleanup(release)
	return begun, release
}

// An uninstall that waited for an install removes what the install wrote:
// never a service without its package.
func TestUninstallWaitsForInstall(t *testing.T) {
	f := newFixture(t)
	f.joinWorker(t)
	started, release := f.holdDownload(t, v1)
	installErr := make(chan error, 1)
	go func() { _, err := Install(t.Context(), v1, f.options(io.Discard)); installErr <- err }()
	<-started
	w := newWaitWriter()
	uninstallErr := make(chan error, 1)
	go func() { uninstallErr <- Uninstall(t.Context(), w) }()
	<-w.waiting
	release()
	if err := <-installErr; err != nil {
		t.Fatalf("install: %v", err)
	}
	if err := <-uninstallErr; err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if f.exists(f.unit) || f.exists(f.store().launcher(v1)) {
		t.Errorf("unit exists %t, package exists %t: want neither", f.exists(f.unit), f.exists(f.store().launcher(v1)))
	}
}

// An update that waited for an install reads the service file the install
// wrote, and keeps its settings.
func TestUpdateWaitsForInstallAndKeepsItsSettings(t *testing.T) {
	f := newFixture(t)
	f.joinWorker(t)
	t.Setenv("HTTPS_PROXY", "http://old-proxy")
	if _, err := Install(t.Context(), v1, f.options(io.Discard)); err != nil {
		t.Fatal(err)
	}
	started, release := f.holdDownload(t, v2)
	t.Setenv("HTTPS_PROXY", "http://new-proxy")
	installErr := make(chan error, 1)
	go func() { _, err := Install(t.Context(), v2, f.options(io.Discard)); installErr <- err }()
	<-started
	w := newWaitWriter()
	updateErr := make(chan error, 1)
	go func() { _, err := Update(t.Context(), v2, f.options(w)); updateErr <- err }()
	<-w.waiting
	release()
	if err := <-installErr; err != nil {
		t.Fatal(err)
	}
	if err := <-updateErr; err != nil {
		t.Fatal(err)
	}
	if unit := f.read(t, f.unit); strings.Contains(unit, "old-proxy") || !strings.Contains(unit, "new-proxy") {
		t.Errorf("the update reverted the install's settings:\n%s", unit)
	}
}

// Two installs of different versions run one after the other; the last
// one's version is the only one left.
func TestConcurrentInstalls(t *testing.T) {
	f := newFixture(t)
	f.joinWorker(t)
	started, release := f.holdDownload(t, v1)
	first := make(chan error, 1)
	go func() { _, err := Install(t.Context(), v1, f.options(io.Discard)); first <- err }()
	<-started
	w := newWaitWriter()
	second := make(chan error, 1)
	go func() { _, err := Install(t.Context(), v2, f.options(w)); second <- err }()
	<-w.waiting
	release()
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	if i, _, _ := InstalledService(); i.Version != v2 {
		t.Errorf("the service runs %q", i.Version)
	}
	if got := entries(t, f.store().root); !slices.Equal(got, []string{lockName, v2}) {
		t.Errorf("store holds %v", got)
	}
}

// Waiting for the lock ends with the context (Ctrl-C).
func TestLockWaitEndsWithTheContext(t *testing.T) {
	s := newStore(t)
	unlock, err := s.lock(t.Context(), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithCancel(t.Context())
	w := newWaitWriter()
	done := make(chan error, 1)
	go func() {
		_, err := s.lock(ctx, w)
		done <- err
	}()
	<-w.waiting
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("lock = %v", err)
	}
}

// A service file that runs anything but a package of the current store is
// never used to find directories to remove or write.
func TestForeignServiceFileIsRefused(t *testing.T) {
	for _, tc := range []struct{ name, launcher string }{
		{"a directory of the user's", "projects/v1.2.3/circus-worker"},
		{"another data root", "other-home/worker/v1.2.3/circus-worker"},
		{"not a package", "bin/circus-worker"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.joinWorker(t)
			launcher := filepath.Join(f.dir, tc.launcher)
			victim := filepath.Dir(filepath.Dir(launcher))
			if err := os.MkdirAll(filepath.Dir(launcher), 0o700); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{launcher, filepath.Join(victim, "thesis.tex")} {
				if err := os.WriteFile(name, []byte("precious"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.MkdirAll(filepath.Dir(f.unit), 0o700); err != nil {
				t.Fatal(err)
			}
			unit := "[Service]\nExecStart=\"" + launcher + "\"\n"
			if err := os.WriteFile(f.unit, []byte(unit), 0o600); err != nil {
				t.Fatal(err)
			}
			want := "is not a worker package in "
			if err := Uninstall(t.Context(), io.Discard); err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("uninstall = %v", err)
			}
			if _, err := Update(t.Context(), v2, f.options(io.Discard)); err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("update = %v", err)
			}
			if f.read(t, f.unit) != unit || f.read(t, filepath.Join(victim, "thesis.tex")) != "precious" || !f.exists(launcher) {
				t.Error("a refused command changed something")
			}
			if got := f.srv.Requests(); len(got) != 0 {
				t.Errorf("a refused update downloaded %v", got)
			}
			// install replaces the service file, and leaves the other directory alone.
			if _, err := Install(t.Context(), v1, f.options(io.Discard)); err != nil {
				t.Fatal(err)
			}
			if !f.exists(filepath.Join(victim, "thesis.tex")) || !f.exists(launcher) {
				t.Error("install touched the other directory")
			}
		})
	}
}

// A failed download installs no service file.
func TestInstallWritesNoServiceWithoutThePackage(t *testing.T) {
	f := newFixture(t)
	f.joinWorker(t)
	f.srv.Set(fakerelease.Path(v1, fakerelease.PackageName(v1, testTarget)), []byte("corrupt"))
	if _, err := Install(t.Context(), v1, f.options(io.Discard)); err == nil || !strings.Contains(err.Error(), "SHA-256 mismatch") {
		t.Fatalf("install = %v", err)
	}
	if f.exists(f.unit) || len(f.calls(t)) != 0 {
		t.Error("a failed download wrote or started the service")
	}
}

// Every setting and proxy variable is recorded, and a changed Node.js is
// reported on reinstall.
func TestInstallRecordsEverySetting(t *testing.T) {
	f := newFixture(t)
	f.joinWorker(t)
	// Spelled out, so a variable dropped from Settings or ProxyVariables
	// fails here.
	recorded := []string{
		"CIRCUS_WORKER_MAX_SESSIONS", "CIRCUS_WORKER_LOG_LEVEL", "CIRCUS_WORKER_IDLE_SECONDS",
		"CIRCUS_NPM_REGISTRY", "CLAUDE_CONFIG_DIR", "NODE_EXTRA_CA_CERTS", "NODE_USE_SYSTEM_CA",
		"HTTPS_PROXY", "HTTP_PROXY", "ALL_PROXY", "NO_PROXY", "https_proxy", "http_proxy", "all_proxy", "no_proxy",
	}
	for _, name := range recorded {
		t.Setenv(name, "value-of-"+name)
	}
	t.Setenv("NODE_OPTIONS", "--use-system-ca")
	if _, err := Install(t.Context(), v1, f.options(io.Discard)); err != nil {
		t.Fatal(err)
	}
	unit := f.read(t, f.unit)
	for _, name := range recorded {
		if want := `Environment="` + name + "=value-of-" + name + `"`; !strings.Contains(unit, want) {
			t.Errorf("unit lacks %s", want)
		}
	}
	if strings.Contains(unit, "NODE_OPTIONS") {
		t.Error("the unit records NODE_OPTIONS")
	}
	other := fakeNode(t, f.dir, "other-node", "25.1.0")
	t.Setenv("CIRCUS_NODE", other)
	var out bytes.Buffer
	if _, err := Install(t.Context(), v1, f.options(&out)); err != nil {
		t.Fatal(err)
	}
	if want := `CIRCUS_NODE "` + f.node + `" → "` + other + `"`; !strings.Contains(out.String(), want) {
		t.Errorf("reinstall output lacks %s:\n%s", want, out.String())
	}
}

// circus update works on the worker installation its service file records,
// whatever data directory the calling shell names.
func TestUpdateUsesTheRecordedDataDirectory(t *testing.T) {
	f := newFixture(t)
	f.joinWorker(t)
	if _, err := Install(t.Context(), v1, f.options(io.Discard)); err != nil {
		t.Fatal(err)
	}
	elsewhere := filepath.Join(f.dir, "elsewhere")
	t.Setenv("XDG_DATA_HOME", elsewhere)
	t.Setenv("CIRCUS_HOME", filepath.Join(f.dir, "another-home"))
	if _, err := Update(t.Context(), v2, f.options(io.Discard)); err != nil {
		t.Fatalf("update from another data directory: %v", err)
	}
	if i, _, _ := InstalledService(); i.Version != v2 || i.Root != f.store().root {
		t.Errorf("the service runs %s from %s", i.Version, i.Root)
	}
	if f.exists(elsewhere) || f.exists(filepath.Join(f.dir, "another-home")) {
		t.Error("the update wrote into the calling shell's data directory")
	}
	if err := Uninstall(t.Context(), io.Discard); err != nil {
		t.Fatal(err)
	}
	if got := entries(t, f.store().root); !slices.Equal(got, []string{lockName}) {
		t.Errorf("uninstall left %v", got)
	}
}
