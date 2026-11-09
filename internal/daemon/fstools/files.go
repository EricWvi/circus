package fstools

import (
	"context"
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

// writeFile follows existing symlinks and overwrites the target in place, keeping
// its inode and hard links. Creation is exclusive so an intervening file is not
// accidentally overwritten.
func writeFile(ctx context.Context, path string, data []byte, created bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	flags, mode := os.O_WRONLY|os.O_TRUNC, os.FileMode(0)
	if created {
		// Inferred: Node fs defaults
		if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
			return err
		}
		flags, mode = os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666
	} else {
		var err error
		path, err = filepath.EvalSymlinks(path)
		if err != nil {
			return err
		}
	}
	f, err := os.OpenFile(path, flags, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
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
