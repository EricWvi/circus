package fstools

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
)

const currentContext = " (file state is current in your context — no need to Read it back)"
const modifiedContext = " (note: the file had been modified on disk since you last read it — the edit applied cleanly, but the file contains other changes not in your context. Read it before edits that depend on surrounding content.)"
const modifiedError = "File has been modified since read, either by the user or by a linter. Read it again before attempting to write it."

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
	return path, cwd, nil
}

func missing(cwd string) string {
	return fmt.Sprintf("File does not exist. Note: your current working directory is %s.", cwd)
}

func changed(expected *string, data []byte) bool {
	return expected != nil && *expected != hash(data)
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

// writeFile replaces the directory entry with a synced temporary sibling,
// breaking hard links while preserving existing permission bits. New-file and
// parent-directory modes use the kernel's umask without changing process state.
func writeFile(ctx context.Context, path string, data []byte, info os.FileInfo) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return err
	}
	var f *os.File
	for {
		name := filepath.Join(filepath.Dir(path), ".circus-write-"+rand.Text())
		var err error
		f, err = os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
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
	if _, err := f.Write(data); err != nil {
		return err
	}
	if info != nil {
		mode := info.Mode() & (os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky)
		if err := f.Chmod(mode); err != nil {
			return err
		}
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

// load reads the bytes and metadata through the same open file.
func load(path string) ([]byte, os.FileInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	if info.IsDir() {
		return nil, info, fmt.Errorf("EISDIR: illegal operation on a directory, read '%s'", path)
	}
	data, err := io.ReadAll(f)
	return data, info, err
}
