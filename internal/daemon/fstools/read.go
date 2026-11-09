package fstools

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
)

// nativeReadSizeLimitBytes is the single hook for the unmeasured native Read
// size threshold. Zero disables the limit. TODO(native-threshold): capture the
// threshold and its exact error body before enabling this hook.
const nativeReadSizeLimitBytes = 0

// Read is a stateless file-reading tool.
type Read struct{}

// NewRead returns a stateless Read tool.
func NewRead() *Read { return &Read{} }

// Name returns the device call tool name.
func (*Read) Name() string { return "read" }

// Run reads file text and returns metadata for the whole file.
func (*Read) Run(ctx context.Context, call *circusv1.DeviceCall) (*circusv1.DeviceResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r := call.GetRead()
	path, cwd, err := resolve(r.GetPath(), r.GetCwd())
	if err != nil {
		return nil, err
	}
	data, info, err := load(path)
	if os.IsNotExist(err) {
		return output(missing(cwd), true), nil
	}
	if err != nil {
		// Inferred: uncaptured filesystem errors retain the OS error text.
		return output(err.Error(), true), nil
	}
	if nativeReadSizeLimitBytes > 0 && len(data) > nativeReadSizeLimitBytes {
		return nil, fmt.Errorf("native Read size threshold exceeded")
	}
	// Inferred: any NUL byte identifies binary content; the extension is used only in the message.
	if bytes.ContainsRune(data, 0) {
		return toolError(fmt.Sprintf("This tool cannot read binary files. The file appears to be a binary %s file. Use a skill for this file type if one is available, or a shell command or script that can read the format.", filepath.Ext(path))), nil
	}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	// Inferred: an empty file has zero total lines in structured metadata.
	total := len(lines)
	if len(data) == 0 {
		total = 0
	}
	// TODO(native-threshold): measure explicit offset:0 / limit:0; for now
	// protobuf zero values mean omitted (start at line one, return all lines).
	start := uint64(r.GetOffset())
	if start == 0 {
		start = 1
	}
	var body string
	switch {
	case len(data) == 0:
		body = "<system-reminder>Warning: the file exists but the contents are empty.</system-reminder>"
	case start > uint64(total):
		body = fmt.Sprintf("<system-reminder>Warning: the file exists but is shorter than the provided offset (%d). The file has %d lines.</system-reminder>", start, total)
	default:
		end := uint64(total)
		if r.GetLimit() > 0 && start-1+uint64(r.GetLimit()) < end {
			end = start - 1 + uint64(r.GetLimit())
		}
		var b strings.Builder
		for i := start - 1; i < end; i++ {
			if i > start-1 {
				b.WriteByte('\n')
			}
			fmt.Fprint(&b, i+1)
			if i != uint64(total-1) || lines[i] != "" {
				b.WriteByte('\t')
				b.WriteString(lines[i])
			}
		}
		body = b.String()
	}
	result := output(body, false)
	result.Detail = &circusv1.DeviceResult_Read{Read: &circusv1.ReadResult{
		Sha256: hash(data), MtimeUnixMs: info.ModTime().UnixMilli(), TotalLines: uint32(total),
	}}
	return result, nil
}
