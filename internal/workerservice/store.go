package workerservice

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/EricWvi/circus/internal/circushome"
	"github.com/EricWvi/circus/internal/selfupdate"
)

const (
	// launcherName is the launcher at the top of an unpacked package.
	launcherName = "circus-worker"
	// packageTop is the one directory a release package holds.
	packageTop = "circus-worker"
	// markerName holds the digest of an unpacked package's files.
	markerName = ".circus-package-digest"
	lockName   = ".lock"
	// Bounds for unpacking a package.
	unpackedLimit = 1 << 30
	entryLimit    = 100_000
	// trailerLimit bounds what is read after the tar end marker.
	trailerLimit = 1 << 20
)

// Root returns the directory that holds the installed worker packages, one
// directory per version, for this process's environment: see RootFor.
func Root() (string, error) {
	env := map[string]string{}
	for _, key := range []string{circushome.Env, "XDG_DATA_HOME", "HOME"} {
		env[key] = os.Getenv(key)
	}
	return RootFor(env)
}

// RootFor returns the worker package store of the environment env (such as
// a service file's): <data>/worker, where <data> is the worker's data
// directory, DataDir.
func RootFor(env map[string]string) (string, error) {
	data, err := DataDir(env)
	if err != nil {
		return "", err
	}
	return filepath.Join(data, "worker"), nil
}

// DataDir returns the worker's data directory as the worker finds it in
// env: $CIRCUS_HOME, else $XDG_DATA_HOME/circus (when absolute), else
// $HOME/.local/share/circus. It also holds the worker's agents and its
// stop file.
func DataDir(env map[string]string) (string, error) {
	if root := env[circushome.Env]; root != "" {
		return filepath.Abs(root)
	}
	if data := env["XDG_DATA_HOME"]; filepath.IsAbs(data) {
		return filepath.Join(data, "circus"), nil
	}
	home := env["HOME"]
	if home == "" {
		var err error
		if home, err = os.UserHomeDir(); err != nil {
			return "", err
		}
	}
	return filepath.Join(home, ".local", "share", "circus"), nil
}

// store is the directory of installed packages.
type store struct{ root string }

func (s store) dir(version string) string { return filepath.Join(s.root, version) }

func (s store) launcher(version string) string {
	return filepath.Join(s.dir(version), launcherName)
}

// lock takes the store's lock, waiting while another install, update or
// uninstall holds it. Unlock with the returned function.
func (s store) lock(ctx context.Context, out io.Writer) (func(), error) {
	if err := os.MkdirAll(s.root, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(s.root, lockName), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	waiting := false
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			f.Close()
			return nil, fmt.Errorf("lock %s: %w", f.Name(), err)
		}
		if !waiting {
			waiting = true
			fmt.Fprintln(out, "Waiting for another install or update of the circus worker to finish…")
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return func() { f.Close() }, nil
}

// ensure makes p's version present and intact in the store, downloading,
// verifying and unpacking it unless it already is. Call it under the lock.
// Leftovers of an interrupted install are removed first.
func (s store) ensure(ctx context.Context, rel selfupdate.Release, version, target string, out io.Writer) error {
	dir := s.dir(version)
	if err := intact(dir); err == nil {
		fmt.Fprintf(out, "The circus worker %s is already in %s.\n", version, dir)
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(out, "The circus worker %s in %s is damaged (%v); downloading it again.\n", version, dir, err)
	}
	s.removeLeftovers()
	p, err := rel.WorkerPackage(ctx, version, target)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Downloading %s…\n", p.Name)
	archive, err := os.CreateTemp(s.root, ".download-*")
	if err != nil {
		return err
	}
	defer os.Remove(archive.Name())
	defer archive.Close()
	if err := rel.DownloadWorker(ctx, p, archive); err != nil {
		return err
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(s.root, ".unpack-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	if err := unpack(archive, stage); err != nil {
		return fmt.Errorf("unpack %s: %w", p.Name, err)
	}
	if info, err := os.Stat(filepath.Join(stage, launcherName)); err != nil || !info.Mode().IsRegular() || info.Mode()&0o100 == 0 {
		return fmt.Errorf("unpack %s: the package has no executable %s/%s", p.Name, packageTop, launcherName)
	}
	digest, err := treeDigest(stage)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(stage, markerName), []byte(digest+"\n"), 0o644); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// A damaged copy of this version goes aside first; rename can't
	// replace a directory that is not empty.
	if _, err := os.Lstat(dir); err == nil {
		aside, err := os.MkdirTemp(s.root, ".old-*")
		if err != nil {
			return err
		}
		if err := os.Rename(dir, filepath.Join(aside, version)); err != nil {
			return err
		}
		defer os.RemoveAll(aside)
	}
	return os.Rename(stage, dir)
}

// removeLeftovers removes the temporary files and directories an
// interrupted install left. Call it under the lock.
func (s store) removeLeftovers() {
	entries, _ := os.ReadDir(s.root)
	for _, e := range entries {
		if name := e.Name(); strings.HasPrefix(name, ".") && name != lockName {
			_ = os.RemoveAll(filepath.Join(s.root, name))
		}
	}
}

// prune removes every version but those in keep, and any leftovers. Call
// it under the lock, once no service runs another version.
func (s store) prune(keep ...string) error {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		if name := e.Name(); !slices.Contains(keep, name) && name != lockName {
			errs = append(errs, os.RemoveAll(filepath.Join(s.root, name)))
		}
	}
	return errors.Join(errs...)
}

// intact checks that dir holds a complete unpacked package whose files
// are unchanged since it was unpacked. A missing dir is fs.ErrNotExist.
func intact(dir string) error {
	if _, err := os.Lstat(dir); err != nil {
		return err
	}
	data, err := os.ReadFile(filepath.Join(dir, markerName))
	if errors.Is(err, fs.ErrNotExist) {
		return errors.New("no record of its files")
	}
	if err != nil {
		return err
	}
	digest, err := treeDigest(dir)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(data)) != digest {
		return errors.New("its files changed")
	}
	return nil
}

// treeDigest hashes the names, kinds, executable bits and contents of the
// files under dir, except the marker at its top. Anything but directories
// and regular files fails it.
func treeDigest(dir string) (string, error) {
	sum := sha256.New()
	err := filepath.WalkDir(dir, func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, name)
		if err != nil || rel == "." || rel == markerName {
			return err
		}
		rel = filepath.ToSlash(rel)
		switch {
		case d.IsDir():
			fmt.Fprintf(sum, "d %q\n", rel)
		case d.Type().IsRegular():
			info, err := d.Info()
			if err != nil {
				return err
			}
			hash, err := fileDigest(name)
			if err != nil {
				return err
			}
			fmt.Fprintf(sum, "f %q %t %d %s\n", rel, info.Mode()&0o100 != 0, info.Size(), hash)
		default:
			return fmt.Errorf("unexpected %s %s", d.Type(), rel)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

func fileDigest(name string) (string, error) {
	f, err := os.Open(name)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

// unpack writes the release package in r into dst, without its top
// directory. It accepts directories and regular files under circus-worker/
// only, at most entryLimit of them and unpackedLimit bytes.
func unpack(r io.Reader, dst string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var total int64
	for entries := 0; ; entries++ {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if entries >= entryLimit {
			return fmt.Errorf("more than %d entries", entryLimit)
		}
		name := path.Clean(h.Name)
		rel, ok := strings.CutPrefix(name, packageTop+"/")
		if name == packageTop && h.Typeflag == tar.TypeDir {
			continue
		}
		if !ok || !filepath.IsLocal(rel) {
			return fmt.Errorf("entry %q is outside %s/", h.Name, packageTop)
		}
		target := filepath.Join(dst, filepath.FromSlash(rel))
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if total += h.Size; h.Size < 0 || total > unpackedLimit {
				return fmt.Errorf("larger than %d MiB unpacked", unpackedLimit>>20)
			}
			if err := writeFile(target, h, tr); err != nil {
				return err
			}
		default:
			return fmt.Errorf("entry %q is not a regular file or directory", h.Name)
		}
	}
	// Read through the gzip trailer to check the archive's integrity. tar
	// pads the archive to its record size; anything much longer is not a
	// package.
	n, err := io.CopyN(io.Discard, gz, trailerLimit+1)
	if err == io.EOF {
		return nil
	}
	if err == nil && n > trailerLimit {
		return fmt.Errorf("more than %d KiB after the end of the archive", trailerLimit>>10)
	}
	return err
}

func writeFile(target string, h *tar.Header, r io.Reader) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if h.Mode&0o111 != 0 {
		mode = 0o755
	}
	// O_EXCL refuses an entry that comes twice.
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(r, h.Size))
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil && n != h.Size {
		err = io.ErrUnexpectedEOF
	}
	return err
}
