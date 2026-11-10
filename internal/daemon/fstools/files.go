package fstools

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
)

const currentContext = " (file state is current in your context — no need to Read it back)"
const modifiedContext = " (note: the file had been modified on disk since you last read it — the edit applied cleanly, but the file contains other changes not in your context. Read it before edits that depend on surrounding content.)"
const modifiedError = "File has been modified since read, either by the user or by a linter. Read it again before attempting to write it."
const encodingError = "File is not valid UTF-8: it uses a legacy encoding such as Windows-1252, Shift-JIS or GBK, or it is binary. These tools write UTF-8 (or UTF-16LE behind its byte-order mark) and never convert a file's encoding on their own, so nothing was written. If the file is text, make the change with a shell command that reads and writes it in its own encoding (iconv, or PowerShell Get-Content/Set-Content -Encoding), or ask the user whether to convert it to UTF-8 first. To replace the whole file with new UTF-8 text anyway, delete it first, then Write it."

// Deviation: native Read blocks indefinitely on a FIFO (see
// tests/parity/fixtures/native/Read/read-fifo.json), so circus rejects
// special files up front instead. The kind names below are circus's own.
const specialFileErrorFormat = "This tool cannot read special files. The file is a %s."

const wholeFileSizeLimitBytes = 256 << 20

// Inferred: Edit and Write reject files over 256 MiB instead of reading them.
const wholeFileSizeError = "This tool cannot modify files larger than 256 MiB."

func output(body string, failed bool) *circusv1.DeviceResult {
	return &circusv1.DeviceResult{Output: &circusv1.ToolOutput{
		IsError: failed,
		Content: []*circusv1.ContentBlock{{Block: &circusv1.ContentBlock_Text{Text: body}}},
	}}
}

func toolError(body string) *circusv1.DeviceResult {
	return output("<tool_use_error>"+body+"</tool_use_error>", true)
}

func hash(data []byte) string { return fmt.Sprintf("%x", sha256.Sum256(data)) }

func resolve(path, cwd string) (string, string, error) {
	if cwd == "" {
		// Inferred: callers omitting cwd use the daemon process's working directory.
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return "", "", err
		}
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", "", err
	}
	return filepath.Clean(absolute), cwd, nil
}

func missing(cwd string) string {
	return fmt.Sprintf("File does not exist. Note: your current working directory is %s.", cwd)
}

func changed(expected *string, data []byte) bool {
	return expected != nil && *expected != hash(data)
}

type pathMutex struct {
	mu   sync.Mutex
	refs int
}

var pathMutexes = struct {
	sync.Mutex
	paths map[string]*pathMutex
}{paths: make(map[string]*pathMutex)}

// lockPath serializes complete read-modify-rename cycles for one cleaned,
// absolute path. Refcounts let idle entries be removed from the map.
func lockPath(path string) func() {
	pathMutexes.Lock()
	entry := pathMutexes.paths[path]
	if entry == nil {
		entry = &pathMutex{}
		pathMutexes.paths[path] = entry
	}
	entry.refs++
	pathMutexes.Unlock()

	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		pathMutexes.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(pathMutexes.paths, path)
		}
		pathMutexes.Unlock()
	}
}

// symlinkRejection uses Lstat so dangling links are refused as well.
func symlinkRejection(path string) (*circusv1.DeviceResult, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return nil, nil
	}
	target, err := os.Readlink(path)
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(path), target)
	}
	return output(fmt.Sprintf("Refusing to write %s: it is a symbolic link. Write to the link's target path instead: %s.", path, target), true), nil
}

type specialFileError struct {
	kind string
}

func (e *specialFileError) Error() string {
	return fmt.Sprintf(specialFileErrorFormat, e.kind)
}

func specialFileKind(mode os.FileMode) string {
	switch {
	case mode&os.ModeNamedPipe != 0:
		return "FIFO"
	case mode&os.ModeSocket != 0:
		return "socket"
	case mode&os.ModeCharDevice != 0:
		return "character device"
	case mode&os.ModeDevice != 0:
		return "device"
	default:
		return "irregular file"
	}
}

// inspectFile stats a path before opening it and admits regular files,
// directories and character devices. Stat deliberately follows symlinks for
// Read compatibility. Edit and Write reject character devices in load.
func inspectFile(path string) (os.FileInfo, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() && !info.IsDir() && info.Mode()&os.ModeCharDevice == 0 {
		return info, &specialFileError{kind: specialFileKind(info.Mode())}
	}
	return info, nil
}

func directoryError(path string) error {
	return fmt.Errorf("EISDIR: illegal operation on a directory, read '%s'", path)
}

// makeParentDirectories creates missing directories with native's explicit
// 0755 mode, independent of the process umask. Existing directories are left
// unchanged.
func makeParentDirectories(dir string) error {
	var missing []string
	for current := dir; ; current = filepath.Dir(current) {
		info, err := os.Stat(current)
		if err == nil {
			if !info.IsDir() {
				return fmt.Errorf("mkdir %s: not a directory", current)
			}
			break
		}
		if !os.IsNotExist(err) {
			return err
		}
		missing = append(missing, current)
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
	}
	for i := len(missing) - 1; i >= 0; i-- {
		path := missing[i]
		if err := os.Mkdir(path, 0o755); err != nil {
			if !os.IsExist(err) {
				return err
			}
			info, statErr := os.Stat(path)
			if statErr != nil {
				return statErr
			}
			if !info.IsDir() {
				return fmt.Errorf("mkdir %s: not a directory", path)
			}
			continue
		}
		if err := os.Chmod(path, 0o755); err != nil {
			return err
		}
	}
	return nil
}

// writeFile replaces the directory entry with a synced temporary sibling,
// breaking hard links while preserving existing permission bits. Temporary
// content is written only after its final mode has been applied.
func writeFile(ctx context.Context, path string, data []byte, info os.FileInfo) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := makeParentDirectories(filepath.Dir(path)); err != nil {
		return err
	}
	var f *os.File
	for {
		name := filepath.Join(filepath.Dir(path), ".circus-write-"+rand.Text())
		var err error
		f, err = os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if os.IsExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		break
	}
	defer os.Remove(f.Name())
	defer f.Close()
	mode := os.FileMode(0o644)
	if info != nil {
		mode = info.Mode() & (os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky)
	}
	if err := f.Chmod(mode); err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// load reads the whole file for Edit or Write, with a hard allocation limit.
func load(path string) ([]byte, os.FileInfo, error) {
	info, err := inspectFile(path)
	if err != nil {
		return nil, info, err
	}
	if info.IsDir() {
		return nil, info, directoryError(path)
	}
	if !info.Mode().IsRegular() {
		return nil, info, &specialFileError{kind: specialFileKind(info.Mode())}
	}
	if info.Size() > wholeFileSizeLimitBytes {
		return nil, info, fmt.Errorf("%s", wholeFileSizeError)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, info, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, wholeFileSizeLimitBytes+1))
	if err != nil {
		return nil, info, err
	}
	if len(data) > wholeFileSizeLimitBytes {
		return nil, info, fmt.Errorf("%s", wholeFileSizeError)
	}
	return data, info, nil
}
