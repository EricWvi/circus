package workerservice

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/EricWvi/circus/internal/selfupdate"
	"github.com/EricWvi/circus/internal/testutil/fakerelease"
)

const (
	v1         = "v1.2.3"
	v2         = "v1.3.0"
	v3         = "v1.4.0"
	testTarget = "linux-amd64"
)

func packageFiles(version string) []fakerelease.File {
	return []fakerelease.File{
		{Name: "circus-worker", Body: "#!/bin/sh\necho " + version + "\n", Mode: 0o755},
		{Name: "package.json", Body: `{"version":"` + version + `"}`},
		{Name: "dist/main.js", Body: "console.log('" + version + "')\n"},
		{Name: "node_modules/dep/index.js", Body: "module.exports = 1\n"},
	}
}

func newStore(t *testing.T) store {
	t.Helper()
	return store{root: filepath.Join(t.TempDir(), "data", "circus", "worker")}
}

func ensureLocked(t *testing.T, s store, rel selfupdate.Release, version string) error {
	t.Helper()
	unlock, err := s.lock(t.Context(), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	return s.ensure(t.Context(), rel, version, testTarget, io.Discard)
}

func entries(t *testing.T, dir string) []string {
	t.Helper()
	list, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range list {
		names = append(names, e.Name())
	}
	return names
}

func TestEnsureDownloadsVerifiesAndUnpacks(t *testing.T) {
	srv := fakerelease.New(t)
	srv.Publish(v1, []string{testTarget, "darwin-arm64"}, packageFiles(v1))
	s := newStore(t)
	if err := ensureLocked(t, s, srv.Release(), v1); err != nil {
		t.Fatal(err)
	}
	want := []string{fakerelease.Path(v1, "latest.json"), fakerelease.Path(v1, fakerelease.PackageName(v1, testTarget))}
	if got := srv.Requests(); !slices.Equal(got, want) {
		t.Errorf("requests = %v, want %v", got, want)
	}
	if got := entries(t, s.root); !slices.Equal(got, []string{lockName, v1}) {
		t.Errorf("store holds %v", got)
	}
	info, err := os.Stat(s.launcher(v1))
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("launcher: %v, %v", info, err)
	}
	if data, err := os.ReadFile(filepath.Join(s.dir(v1), "node_modules", "dep", "index.js")); err != nil || string(data) != "module.exports = 1\n" {
		t.Errorf("dependency: %q, %v", data, err)
	}
	if info, err := os.Stat(filepath.Join(s.dir(v1), "package.json")); err != nil || info.Mode().Perm() != 0o644 {
		t.Errorf("package.json: %v, %v", info, err)
	}
	if err := intact(s.dir(v1)); err != nil {
		t.Errorf("fresh package is not intact: %v", err)
	}

	// Already there and intact: nothing is downloaded, not even the manifest.
	if err := ensureLocked(t, s, srv.Release(), v1); err != nil {
		t.Fatal(err)
	}
	if got := srv.Requests(); len(got) != 0 {
		t.Errorf("an intact package was downloaded again: %v", got)
	}
}

func TestEnsureReplacesDamagedPackage(t *testing.T) {
	for name, damage := range map[string]func(dir string) error{
		"changed file": func(dir string) error {
			return os.WriteFile(filepath.Join(dir, "dist", "main.js"), []byte("changed"), 0o644)
		},
		"missing file":  func(dir string) error { return os.Remove(filepath.Join(dir, "node_modules", "dep", "index.js")) },
		"extra file":    func(dir string) error { return os.WriteFile(filepath.Join(dir, "extra.js"), nil, 0o644) },
		"lost exec bit": func(dir string) error { return os.Chmod(filepath.Join(dir, launcherName), 0o644) },
		"no marker":     func(dir string) error { return os.Remove(filepath.Join(dir, markerName)) },
		"symlink":       func(dir string) error { return os.Symlink("/etc/passwd", filepath.Join(dir, "link")) },
	} {
		t.Run(name, func(t *testing.T) {
			srv := fakerelease.New(t)
			srv.Publish(v1, []string{testTarget}, packageFiles(v1))
			s := newStore(t)
			if err := ensureLocked(t, s, srv.Release(), v1); err != nil {
				t.Fatal(err)
			}
			srv.Requests()
			if err := damage(s.dir(v1)); err != nil {
				t.Fatal(err)
			}
			if intact(s.dir(v1)) == nil {
				t.Fatal("damage went unnoticed")
			}
			if err := ensureLocked(t, s, srv.Release(), v1); err != nil {
				t.Fatal(err)
			}
			if got := srv.Requests(); len(got) != 2 {
				t.Errorf("requests = %v, want the manifest and the package", got)
			}
			if err := intact(s.dir(v1)); err != nil {
				t.Errorf("after a new download: %v", err)
			}
			if got := entries(t, s.root); !slices.Equal(got, []string{lockName, v1}) {
				t.Errorf("store holds %v", got)
			}
		})
	}
}

func TestEnsureFailuresLeaveNothing(t *testing.T) {
	pkg := fakerelease.Path(v1, fakerelease.PackageName(v1, testTarget))
	for _, tc := range []struct {
		name    string
		prepare func(srv *fakerelease.Server)
		wantErr string
	}{
		{"digest mismatch", func(srv *fakerelease.Server) {
			srv.Set(pkg, fakerelease.Archive(t, packageFiles("other")))
		}, "SHA-256 mismatch for circus-worker-v1.2.3-linux-amd64.tar.gz"},
		{"partial download", func(srv *fakerelease.Server) {
			srv.Handle(pkg, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Length", "100000")
				_, _ = w.Write([]byte("partial"))
			})
		}, "unexpected EOF"},
		{"package missing", func(srv *fakerelease.Server) {
			srv.Handle(pkg, http.NotFound)
		}, "HTTP status 404"},
		{"no package for this platform", func(srv *fakerelease.Server) {
			srv.Publish(v1, []string{"darwin-arm64"}, packageFiles(v1))
		}, "no circus-worker release for this platform (linux-amd64)"},
		{"manifest of another version", func(srv *fakerelease.Server) {
			srv.Set(fakerelease.Path(v1, "latest.json"), []byte(`{"version":"v9.9.9"}`))
		}, `names version "v9.9.9"`},
		{"no release", func(srv *fakerelease.Server) {
			srv.Handle(fakerelease.Path(v1, "latest.json"), http.NotFound)
		}, "download the v1.2.3 release manifest: HTTP status 404"},
		{"no launcher", func(srv *fakerelease.Server) {
			srv.Publish(v1, []string{testTarget}, packageFiles(v1)[1:])
		}, "no executable circus-worker/circus-worker"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := fakerelease.New(t)
			srv.Publish(v1, []string{testTarget}, packageFiles(v1))
			tc.prepare(srv)
			s := newStore(t)
			err := ensureLocked(t, s, srv.Release(), v1)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("ensure = %v, want %q", err, tc.wantErr)
			}
			if got := entries(t, s.root); !slices.Equal(got, []string{lockName}) {
				t.Errorf("a failed install left %v", got)
			}
		})
	}
}

func TestEnsureRemovesLeftovers(t *testing.T) {
	srv := fakerelease.New(t)
	srv.Publish(v1, []string{testTarget}, packageFiles(v1))
	s := newStore(t)
	// An install killed while unpacking or downloading.
	for _, dir := range []string{".unpack-123/dist", ".old-456/" + v1} {
		if err := os.MkdirAll(filepath.Join(s.root, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(s.root, ".download-789"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ensureLocked(t, s, srv.Release(), v1); err != nil {
		t.Fatal(err)
	}
	if got := entries(t, s.root); !slices.Equal(got, []string{lockName, v1}) {
		t.Errorf("store holds %v", got)
	}
}

func TestPruneKeepsOneVersion(t *testing.T) {
	srv := fakerelease.New(t)
	srv.Publish(v1, []string{testTarget}, packageFiles(v1))
	srv.Publish(v2, []string{testTarget}, packageFiles(v2))
	s := newStore(t)
	for _, v := range []string{v1, v2} {
		if err := ensureLocked(t, s, srv.Release(), v); err != nil {
			t.Fatal(err)
		}
	}
	if got := entries(t, s.root); !slices.Equal(got, []string{lockName, v1, v2}) {
		t.Fatalf("store holds %v", got)
	}
	if err := s.prune(v2); err != nil {
		t.Fatal(err)
	}
	if got := entries(t, s.root); !slices.Equal(got, []string{lockName, v2}) {
		t.Errorf("after prune: %v", got)
	}
}

func TestUnpackRejectsUnsafeArchives(t *testing.T) {
	launcher := fakerelease.File{Name: "circus-worker", Body: "#!/bin/sh\n", Mode: 0o755}
	for _, tc := range []struct {
		name  string
		files []fakerelease.File
		want  string
	}{
		{"symlink", []fakerelease.File{launcher, {Name: "link", Type: tar.TypeSymlink, Linkname: "/etc/passwd"}}, "not a regular file or directory"},
		{"hard link", []fakerelease.File{launcher, {Name: "link", Type: tar.TypeLink, Linkname: "circus-worker/circus-worker"}}, "not a regular file or directory"},
		{"parent directory", []fakerelease.File{launcher, {Name: "../escape", Body: "x", Raw: true}}, "outside circus-worker/"},
		{"climbing out", []fakerelease.File{launcher, {Name: "circus-worker/../../escape", Body: "x", Raw: true}}, "outside circus-worker/"},
		{"absolute", []fakerelease.File{launcher, {Name: "/tmp/escape", Body: "x", Raw: true}}, "outside circus-worker/"},
		{"another top directory", []fakerelease.File{launcher, {Name: "circus-workers/x", Body: "x", Raw: true}}, "outside circus-worker/"},
		{"duplicate", []fakerelease.File{launcher, launcher}, "file exists"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dst := t.TempDir()
			archive := fakerelease.Archive(t, tc.files)
			err := unpack(strings.NewReader(string(archive)), dst)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("unpack = %v, want %q", err, tc.want)
			}
			if _, err := os.Lstat(filepath.Join(filepath.Dir(dst), "escape")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("an entry escaped: %v", err)
			}
		})
	}
	t.Run("trailing garbage", func(t *testing.T) {
		archive := append(fakerelease.Archive(t, []fakerelease.File{launcher}), "garbage"...)
		if err := unpack(strings.NewReader(string(archive)), t.TempDir()); err == nil {
			t.Fatal("unpacked an archive with trailing garbage")
		}
	})
}

// gzipTar writes a gzipped tar with write, leaving the tar unclosed when
// write returns false.
func gzipTar(t *testing.T, write func(tw *tar.Writer) bool, after []byte) []byte {
	t.Helper()
	var data bytes.Buffer
	gz := gzip.NewWriter(&data)
	tw := tar.NewWriter(gz)
	if write(tw) {
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
	} else if err := tw.Flush(); err != nil && !strings.Contains(err.Error(), "missed writing") {
		t.Fatal(err)
	}
	if _, err := gz.Write(after); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func TestUnpackBounds(t *testing.T) {
	t.Run("too many entries", func(t *testing.T) {
		archive := gzipTar(t, func(tw *tar.Writer) bool {
			for i := range entryLimit + 1 {
				if err := tw.WriteHeader(&tar.Header{Name: fmt.Sprintf("circus-worker/d%d/", i), Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
					t.Fatal(err)
				}
			}
			return true
		}, nil)
		if err := unpack(bytes.NewReader(archive), t.TempDir()); err == nil || !strings.Contains(err.Error(), "more than 100000 entries") {
			t.Fatalf("unpack = %v", err)
		}
	})
	t.Run("too large unpacked", func(t *testing.T) {
		// The header's size alone fails it; nothing that large is written.
		archive := gzipTar(t, func(tw *tar.Writer) bool {
			if err := tw.WriteHeader(&tar.Header{Name: "circus-worker/big", Typeflag: tar.TypeReg, Mode: 0o644, Size: unpackedLimit + 1}); err != nil {
				t.Fatal(err)
			}
			return false
		}, nil)
		dst := t.TempDir()
		if err := unpack(bytes.NewReader(archive), dst); err == nil || !strings.Contains(err.Error(), "larger than 1024 MiB unpacked") {
			t.Fatalf("unpack = %v", err)
		}
		if _, err := os.Stat(filepath.Join(dst, "big")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the oversized file was written: %v", err)
		}
	})
	launcher := fakerelease.File{Name: "circus-worker", Body: "#!/bin/sh\n", Mode: 0o755}
	t.Run("long data after the archive", func(t *testing.T) {
		archive := gzipTar(t, func(tw *tar.Writer) bool {
			if err := tw.WriteHeader(&tar.Header{Name: "circus-worker/x", Typeflag: tar.TypeReg, Mode: 0o644}); err != nil {
				t.Fatal(err)
			}
			return true
		}, make([]byte, trailerLimit+1))
		if err := unpack(bytes.NewReader(archive), t.TempDir()); err == nil || !strings.Contains(err.Error(), "after the end of the archive") {
			t.Fatalf("unpack = %v", err)
		}
	})
	t.Run("tar padding after the archive", func(t *testing.T) {
		archive := gzipTar(t, func(tw *tar.Writer) bool {
			if err := tw.WriteHeader(&tar.Header{Name: "circus-worker/x", Typeflag: tar.TypeReg, Mode: 0o644}); err != nil {
				t.Fatal(err)
			}
			return true
		}, make([]byte, 9*1024))
		if err := unpack(bytes.NewReader(archive), t.TempDir()); err != nil {
			t.Fatalf("unpack = %v", err)
		}
	})
	t.Run("modes are normalised", func(t *testing.T) {
		dst := t.TempDir()
		archive := fakerelease.Archive(t, []fakerelease.File{
			launcher,
			{Name: "setuid", Body: "x", Mode: 0o4755},
			{Name: "writable", Body: "x", Mode: 0o666},
		})
		if err := unpack(bytes.NewReader(archive), dst); err != nil {
			t.Fatal(err)
		}
		for name, want := range map[string]os.FileMode{"setuid": 0o755, "writable": 0o644} {
			info, err := os.Stat(filepath.Join(dst, name))
			if err != nil || info.Mode() != want {
				t.Errorf("%s mode = %v, %v; want %v", name, info.Mode(), err, want)
			}
		}
	})
}
