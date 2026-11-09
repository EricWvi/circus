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

// nativeReadSizeLimitBytes is the inclusive raw-byte limit for a full-file Read.
const nativeReadSizeLimitBytes = 262144

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
	// Inferred: any supplied offset or positive limit selects the range path;
	// the full-file guard does not apply, as measured for offset:10, limit:5.
	if r.Offset == nil && r.GetLimit() == 0 && len(data) > nativeReadSizeLimitBytes {
		return output(fmt.Sprintf("File content (%s) exceeds maximum allowed size (256KB). Use offset and limit parameters to read specific portions of the file, or search for specific content instead of reading the whole file. To read it anyway, up to what still fits in your context, call Read again with allow_large: true — only if you genuinely need all of it or the user asked for the whole file.", readSize(len(data))), true), nil
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
	start := uint64(1)
	if r.Offset != nil {
		start = uint64(r.GetOffset())
	}
	first := uint64(0)
	if start > 0 {
		first = start - 1
	}
	var body string
	switch {
	case len(data) == 0:
		body = "<system-reminder>Warning: the file exists but the contents are empty.</system-reminder>"
	case start > uint64(total):
		body = fmt.Sprintf("<system-reminder>Warning: the file exists but is shorter than the provided offset (%d). The file has %d lines.</system-reminder>", start, total)
	default:
		end := uint64(total)
		if r.GetLimit() > 0 && first+uint64(r.GetLimit()) < end {
			end = first + uint64(r.GetLimit())
		}
		var b strings.Builder
		for i := first; i < end; i++ {
			if i > first {
				b.WriteByte('\n')
			}
			fmt.Fprint(&b, start+i-first)
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

func readSize(size int) string {
	// Inferred: binary units rounded to whole KB or one decimal MB reproduce
	// the boundary fixtures and the larger probe documented in the native spec.
	if size >= 1024*1024 {
		return fmt.Sprintf("%.1fMB", float64(size)/(1024*1024))
	}
	return fmt.Sprintf("%.0fKB", float64(size)/1024)
}
