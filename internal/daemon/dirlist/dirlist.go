// Package dirlist browses the local filesystem for device queries.
package dirlist

import (
	"cmp"
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"unicode/utf8"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
)

const (
	maxRawEntries = 20000
	maxEntries    = 1000
	readBatch     = 256
)

// Resolve returns the lexical path, parent and home without accessing the
// filesystem. Invalid paths have an empty path and parent.
func Resolve(path string) *circusv1.ListDirectoryResult {
	return resolve(path, os.UserHomeDir)
}

// ResolveWith is Resolve with the home taken from lookupHome.
func ResolveWith(path string, lookupHome func() (string, error)) *circusv1.ListDirectoryResult {
	return resolve(path, lookupHome)
}

func resolve(path string, lookupHome func() (string, error)) *circusv1.ListDirectoryResult {
	home, err := lookupHome()
	if err != nil || !filepath.IsAbs(home) || !utf8.ValidString(home) || strings.ContainsRune(home, 0) {
		home = ""
	} else {
		home = filepath.Clean(home)
	}
	result := &circusv1.ListDirectoryResult{Home: home}
	if len(path) > 4096 || strings.ContainsRune(path, 0) || !utf8.ValidString(path) {
		result.Error = circusv1.DirectoryError_DIRECTORY_ERROR_INVALID_PATH
		return result
	}
	switch {
	case path == "" || path == "~":
		path = home
	case strings.HasPrefix(path, "~/"):
		if home != "" {
			path = filepath.Join(home, path[2:])
		}
	}
	if !filepath.IsAbs(path) {
		result.Error = circusv1.DirectoryError_DIRECTORY_ERROR_INVALID_PATH
		return result
	}
	path = filepath.Clean(path)
	parent := filepath.Dir(path)
	if !utf8.ValidString(path) || !utf8.ValidString(parent) {
		return &circusv1.ListDirectoryResult{Error: circusv1.DirectoryError_DIRECTORY_ERROR_INVALID_PATH}
	}
	result.Path = path
	if parent != path {
		result.Parent = parent
	}
	return result
}

// FileSystem supplies the operations used to read a directory and its markers.
type FileSystem interface {
	Stat(string) (fs.FileInfo, error)
	Lstat(string) (fs.FileInfo, error)
	Open(string) (fs.ReadDirFile, error)
}

// OSFileSystem uses paths on the daemon's host.
type OSFileSystem struct{}

func (OSFileSystem) Stat(path string) (fs.FileInfo, error)    { return os.Stat(path) }
func (OSFileSystem) Lstat(path string) (fs.FileInfo, error)   { return os.Lstat(path) }
func (OSFileSystem) Open(path string) (fs.ReadDirFile, error) { return os.Open(path) }

// Lister reads directories and shares a bounded probe pool across queries.
type Lister struct {
	fs         FileSystem
	logger     *slog.Logger
	probeSlots chan struct{}
}

// New constructs the daemon's lister. A nil filesystem uses the host.
func New(files FileSystem, logger *slog.Logger) *Lister {
	if files == nil {
		files = OSFileSystem{}
	}
	return &Lister{fs: files, logger: logger, probeSlots: make(chan struct{}, maxProbes)}
}

// Read lists the requested directory's candidate entries without probing their
// targets or .git markers. Apply Mark before sending the result.
func (l *Lister) Read(ctx context.Context, path string, includeHidden bool) *circusv1.ListDirectoryResult {
	result := Resolve(path)
	if result.Error != circusv1.DirectoryError_DIRECTORY_ERROR_UNSPECIFIED {
		return result
	}
	entries, truncated, err := l.read(ctx, result.Path, includeHidden)
	switch {
	case err == nil:
		result.Entries, result.Truncated = entries, truncated
	case ctx.Err() != nil:
		result.Error = circusv1.DirectoryError_DIRECTORY_ERROR_TIMEOUT
	case errors.Is(err, syscall.ENOTDIR):
		result.Error = circusv1.DirectoryError_DIRECTORY_ERROR_NOT_DIRECTORY
	case errors.Is(err, os.ErrNotExist):
		result.Error = circusv1.DirectoryError_DIRECTORY_ERROR_NOT_FOUND
	case errors.Is(err, os.ErrPermission):
		result.Error = circusv1.DirectoryError_DIRECTORY_ERROR_PERMISSION_DENIED
	default:
		result.Error = circusv1.DirectoryError_DIRECTORY_ERROR_IO
		l.logger.Warn("directory listing failed", "error", err)
	}
	return result
}

func (l *Lister) read(ctx context.Context, path string, includeHidden bool) ([]*circusv1.DirectoryEntry, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	// Stat first so a normal request for a FIFO cannot block in Open.
	info, err := l.fs.Stat(path)
	if err != nil {
		return nil, false, err
	}
	if !info.IsDir() {
		return nil, false, syscall.ENOTDIR
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	dir, err := l.fs.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer dir.Close()

	var dirs []*circusv1.DirectoryEntry
	raw := 0
	for raw < maxRawEntries {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		batch, err := dir.ReadDir(min(readBatch, maxRawEntries-raw))
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, false, err
		}
		raw += len(batch)
		for _, entry := range batch {
			name := entry.Name()
			if !utf8.ValidString(name) || !includeHidden && strings.HasPrefix(name, ".") {
				continue
			}
			symlink := entry.Type()&os.ModeSymlink != 0
			if !symlink && !entry.IsDir() {
				continue
			}
			dirs = append(dirs, &circusv1.DirectoryEntry{Name: name, Symlink: symlink})
		}
		if errors.Is(err, io.EOF) {
			break
		}
	}

	var more bool
	if raw == maxRawEntries {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		extra, err := dir.ReadDir(1)
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, false, err
		}
		more = len(extra) != 0
	}
	slices.SortFunc(dirs, func(a, b *circusv1.DirectoryEntry) int { return cmp.Compare(a.Name, b.Name) })
	return dirs, more, nil
}
