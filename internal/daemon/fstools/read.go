package fstools

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/textutil"
)

// nativeReadSizeLimitBytes is the inclusive raw-byte limit for a full-file Read.
const nativeReadSizeLimitBytes = 262144

// Inferred: 3200000 is the selected-payload boundary measured in fresh native
// sessions with a full context budget; other context budgets were not tested.
const nativeReadRangeLimitBytes = 3200000

const nativeReadRangeSizeError = "The requested line range contains over 3.1MB of text, more than a read can return. Use a smaller limit — or, if a single line is this large, no limit will fit it: search for specific content instead. Even with allow_large, this is more than fits in the context you have left this turn (other large reads in the same turn share that room)."

const nativeReadSizeTextBoundBytes = 10 << 20

const maxFormattedRangeBytes = 2 * nativeReadRangeLimitBytes

const streamContextCheckLines = 4096
const streamContextCheckFragments = 4096
const streamReadBufferSize = 4096

// Read is a stateless file-reading tool.
type Read struct{}

// NewRead returns a stateless Read tool.
func NewRead() *Read { return &Read{} }

// Name returns the device call tool name.
func (*Read) Name() string { return "read" }

func binaryReadError(path string) *circusv1.DeviceResult {
	// Inferred: any NUL byte identifies binary content; the extension is used only in the message.
	return toolError(fmt.Sprintf("This tool cannot read binary files. The file appears to be a binary %s file. Use a skill for this file type if one is available, or a shell command or script that can read the format.", filepath.Ext(path)))
}

func readFailure(path, cwd string, err error) *circusv1.DeviceResult {
	if os.IsNotExist(err) {
		return output(missing(cwd), true)
	}
	var special *specialFileError
	if errors.As(err, &special) {
		return toolError(special.Error())
	}
	// Inferred: uncaptured filesystem errors retain the OS error text.
	return output(err.Error(), true)
}

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
	info, err := inspectFile(path)
	if err != nil {
		return readFailure(path, cwd, err), nil
	}
	if info.IsDir() {
		return readFailure(path, cwd, directoryError(path)), nil
	}

	// A positive limit bypasses the raw-byte guard; offset alone does not.
	guarded := r.GetLimit() == 0
	if guarded && info.Size() > nativeReadSizeLimitBytes {
		return output(readSizeError(info.Size()), true), nil
	}
	f, err := os.Open(path)
	if err != nil {
		return readFailure(path, cwd, err), nil
	}
	defer f.Close()

	var data []byte
	if info.Mode()&os.ModeCharDevice != 0 {
		data, err = readDevice(ctx, f)
	} else {
		ranged := r.Offset != nil || r.GetLimit() > 0
		if ranged {
			var prefix [2]byte
			n, readErr := f.ReadAt(prefix[:], 0)
			if readErr != nil && !errors.Is(readErr, io.EOF) {
				return readFailure(path, cwd, readErr), nil
			}
			if !textutil.HasUTF16LEBOM(prefix[:n]) {
				return streamReadRange(ctx, f, path, info, r, false)
			}
			return streamReadRange(ctx, f, path, info, r, true)
		}
		var reader io.Reader = f
		if guarded {
			reader = io.LimitReader(f, nativeReadSizeLimitBytes+1)
		}
		data, err = io.ReadAll(reader)
	}
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return readFailure(path, cwd, err), nil
	}
	if guarded && len(data) > nativeReadSizeLimitBytes {
		size := info.Size()
		if int64(len(data)) > size {
			size = int64(len(data))
		}
		return output(readSizeError(size), true), nil
	}
	return readComplete(data, info, path, r), nil
}

const invalidUTF8Notice = "\n\n[This file is not valid UTF-8: it uses a legacy encoding such as Windows-1252, Shift-JIS or GBK, or it is binary. Its undecodable bytes show above as U+FFFD. Edit and Write will refuse it: change it with a shell command that keeps its encoding, or ask the user whether to convert it to UTF-8.]"

func readComplete(data []byte, info os.FileInfo, path string, r *circusv1.ReadRequest) *circusv1.DeviceResult {
	decoded := textutil.Decode(data)
	if decoded.Encoding == textutil.EncodingUTF8 && bytes.ContainsRune(data, 0) {
		return binaryReadError(path)
	}
	lines := strings.Split(strings.ReplaceAll(decoded.Text, "\r\n", "\n"), "\n")
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
			if i != uint64(total-1) || lines[i] != "" || decoded.InvalidUTF8 {
				b.WriteByte('\t')
				b.WriteString(lines[i])
			}
		}
		body = b.String()
	}
	if decoded.InvalidUTF8 {
		body += invalidUTF8Notice
	}
	result := output(body, false)
	result.Detail = &circusv1.DeviceResult_Read{Read: &circusv1.ReadResult{
		Sha256: hash(data), MtimeUnixMs: info.ModTime().UnixMilli(), TotalLines: uint32(total),
	}}
	return result
}

func readDevice(ctx context.Context, f *os.File) ([]byte, error) {
	type readResult struct {
		data []byte
		err  error
	}
	done := make(chan readResult, 1)
	go func() {
		data, err := io.ReadAll(io.LimitReader(f, nativeReadSizeLimitBytes+1))
		select {
		case done <- readResult{data: data, err: err}:
		case <-ctx.Done():
		}
	}()
	select {
	case result := <-done:
		return result.data, result.err
	case <-ctx.Done():
		_ = f.Close()
		return nil, ctx.Err()
	}
}

// streamingUTF8 validates raw bytes without retaining unrequested content.
type streamingUTF8 struct {
	pending []byte
	invalid bool
}

func (v *streamingUTF8) Write(p []byte) (int, error) {
	n := len(p)
	data := p
	if len(v.pending) > 0 {
		data = make([]byte, 0, len(v.pending)+len(p))
		data = append(data, v.pending...)
		data = append(data, p...)
		v.pending = nil
	}
	for len(data) > 0 {
		r, size := utf8.DecodeRune(data)
		if r == utf8.RuneError && size == 1 {
			if !utf8.FullRune(data) {
				v.pending = append(v.pending, data...)
				break
			}
			v.invalid = true
			data = data[1:]
			continue
		}
		data = data[size:]
	}
	return n, nil
}

func (v *streamingUTF8) finish() {
	if len(v.pending) > 0 {
		v.invalid = true
	}
}

// streamReadRange hashes and counts the entire file while retaining only the
// requested lines. ReadSlice fragments prevent an unrequested long line from
// being accumulated. Selected payload is capped independently of file size.
func streamReadRange(ctx context.Context, f *os.File, path string, info os.FileInfo, r *circusv1.ReadRequest, utf16le bool) (*circusv1.DeviceResult, error) {
	start := uint64(1)
	if r.Offset != nil {
		start = uint64(r.GetOffset())
	}
	first := uint64(0)
	if start > 0 {
		first = start - 1
	}
	limit := uint64(r.GetLimit())
	selected := func(index uint64) bool {
		return index >= first && (limit == 0 || index < first+limit)
	}

	hasher := sha256.New()
	validity := &streamingUTF8{}
	var source io.Reader
	if utf16le {
		source = textutil.NewUTF16LEDecoder(io.TeeReader(f, hasher))
	} else {
		source = io.TeeReader(f, io.MultiWriter(hasher, validity))
	}
	reader := bufio.NewReaderSize(source, streamReadBufferSize)
	body := &strings.Builder{}
	var total uint64
	var selectedBytes int64
	sawAny, lastNewline := false, false
	appendLine := func(index uint64, raw []byte, finalEmpty bool) bool {
		var numberBuffer [20]byte
		number := strconv.AppendUint(numberBuffer[:0], start+index-first, 10)
		extra := len(number)
		if body.Len() > 0 {
			extra++
		}
		text := ""
		if !finalEmpty {
			text = textutil.NodeUTF8(raw)
			extra += 1 + len(text)
		} else if validity.invalid {
			extra++
		}
		// Deviation: native behavior for huge numbers of short lines was not
		// captured, so formatted output has this additional defensive bound.
		if body.Len()+extra > maxFormattedRangeBytes {
			return false
		}
		if need := body.Len() + extra; need > body.Cap() {
			// Double up to the bound: append's ~1.25x growth for large buffers
			// would allocate about five times the final size in total. An empty
			// Builder's Grow(n) allocates exactly n.
			grown := &strings.Builder{}
			grown.Grow(min(max(need, 2*body.Cap()), maxFormattedRangeBytes))
			grown.WriteString(body.String())
			body = grown
		}
		if body.Len() > 0 {
			body.WriteByte('\n')
		}
		body.Write(number)
		if !finalEmpty {
			body.WriteByte('\t')
			body.WriteString(text)
		} else if validity.invalid {
			body.WriteByte('\t')
		}
		return true
	}

	for {
		if total%streamContextCheckLines == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		var raw []byte
		pendingCR := false
		hadData, endedNewline, eof := false, false, false
		for fragments := 0; ; fragments++ {
			if fragments > 0 && fragments%streamContextCheckFragments == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			fragment, err := reader.ReadSlice('\n')
			endedNewline = len(fragment) > 0 && fragment[len(fragment)-1] == '\n'
			finalFragment := !errors.Is(err, bufio.ErrBufferFull)
			if selected(total) && pendingCR {
				pendingCR = false
				pendingIsCRLF := finalFragment && endedNewline && len(fragment) == 1
				if !pendingIsCRLF {
					selectedBytes++
					if selectedBytes > nativeReadRangeLimitBytes {
						return output(nativeReadRangeSizeError, true), nil
					}
					raw = append(raw, '\r')
				}
			}
			if selected(total) && !finalFragment && len(fragment) > 0 && fragment[len(fragment)-1] == '\r' {
				// Hold a fragment-final CR until the next fragment distinguishes
				// a CRLF line ending from line content.
				pendingCR = true
				fragment = fragment[:len(fragment)-1]
			}
			if finalFragment && endedNewline {
				fragment = fragment[:len(fragment)-1]
				if len(fragment) > 0 && fragment[len(fragment)-1] == '\r' {
					fragment = fragment[:len(fragment)-1]
				}
			}
			if len(fragment) > 0 {
				hadData = true
				if !utf16le && bytes.IndexByte(fragment, 0) >= 0 {
					return binaryReadError(path), nil
				}
				if selected(total) {
					// Inferred: the cap counts selected line-content bytes before Read
					// numbering (decoded UTF-8 bytes for UTF-16LE). Line numbers, Tabs,
					// separating LFs and line endings do not count.
					selectedBytes += int64(len(fragment))
					if selectedBytes > nativeReadRangeLimitBytes {
						return output(nativeReadRangeSizeError, true), nil
					}
					raw = append(raw, fragment...)
				}
			}
			if errors.Is(err, bufio.ErrBufferFull) {
				continue
			}
			if err != nil && !errors.Is(err, io.EOF) {
				return readFailure(f.Name(), "", err), nil
			}
			eof = errors.Is(err, io.EOF)
			break
		}
		if !hadData && eof {
			break
		}

		index := total
		total++
		sawAny, lastNewline = true, endedNewline
		if selected(index) {
			if !appendLine(index, raw, false) {
				return output(nativeReadRangeSizeError, true), nil
			}
		}
		if eof {
			break
		}
	}
	validity.finish()
	if total == 0 && utf16le && info.Size() > 0 {
		// A BOM-only file decodes to one empty line, matching readComplete.
		total = 1
		if selected(0) && !appendLine(0, nil, true) {
			return output(nativeReadRangeSizeError, true), nil
		}
	}
	if sawAny && lastNewline {
		index := total
		total++
		if selected(index) {
			if !appendLine(index, nil, true) {
				return output(nativeReadRangeSizeError, true), nil
			}
		}
	}

	var text string
	switch {
	case total == 0:
		text = "<system-reminder>Warning: the file exists but the contents are empty.</system-reminder>"
	case start > total:
		text = fmt.Sprintf("<system-reminder>Warning: the file exists but is shorter than the provided offset (%d). The file has %d lines.</system-reminder>", start, total)
	default:
		text = body.String()
	}
	if validity.invalid {
		text += invalidUTF8Notice
	}
	result := output(text, false)
	result.Detail = &circusv1.DeviceResult_Read{Read: &circusv1.ReadResult{
		Sha256: hex.EncodeToString(hasher.Sum(nil)), MtimeUnixMs: info.ModTime().UnixMilli(), TotalLines: uint32(total),
	}}
	return result, nil
}

func readSizeError(size int64) string {
	advice := "To read it anyway, up to what still fits in your context, call Read again with allow_large: true — only if you genuinely need all of it or the user asked for the whole file."
	// Inferred: the 1 MiB probe uses the allow-large advice while the 2 MiB
	// probe uses the context warning; the switch between them was not bisected.
	if size > 1024*1024 && size < nativeReadSizeTextBoundBytes {
		advice = "Even with allow_large, this is more than fits in the context you have left this turn (other large reads in the same turn share that room)."
	}
	return fmt.Sprintf("File content (%s) exceeds maximum allowed size (256KB). Use offset and limit parameters to read specific portions of the file, or search for specific content instead of reading the whole file. %s", readSize(size), advice)
}

func readSize(size int64) string {
	// read-size-text-boundary-real.json reports the real size at 10485759 bytes;
	// read-size-text-boundary-bounded.json reports 512KB at 10485760 bytes.
	if size >= nativeReadSizeTextBoundBytes {
		size = 512 * 1024
	}
	// Inferred: binary units rounded to whole KB or one decimal MB reproduce
	// the boundary fixtures and the larger probe documented in the native spec.
	if size >= 1024*1024 {
		return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(size)/(1024*1024)), ".0") + "MB"
	}
	return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(size)/1024), ".0") + "KB"
}
