package fstools

import (
	"bytes"
	"fmt"
	"path/filepath"
	"strings"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
)

// binaryExtensions are the lower-case file extensions Read refuses as binary,
// whatever the content: an empty a.bin and an a.zip holding "abc" are refused,
// while a.txt or an extensionless file with NUL bytes is read as text.
// Inferred: native's list is not published; these are the extensions measured
// as refused. Measured extensions that Read accepts include csv, json, lock,
// log, pkl, svg, ts, rtf, h5, onnx, pt, safetensors, cab, pages and key;
// unmeasured extensions are read as text.
var binaryExtensions = map[string]bool{}

func init() {
	for _, ext := range strings.Fields(`
		bin dat data exe dll so dylib o a obj lib app msi deb rpm apk node wasm rlib
		class jar war ear pyc pyo whl
		zip tar gz tgz bz2 xz z 7z rar iso dmg lz4 zst
		mp3 wav ogg flac aac m4a wma aiff opus
		mp4 mov avi mkv webm wmv flv m4v mpeg mpg
		bmp ico tiff tif heic heif avif psd ai eps sketch fig xd blend 3ds max
		doc docx docm xls xlsx xlsm xlsb ppt pptx pptm odt ods odp epub numbers
		ttf otf woff woff2 eot
		db sqlite sqlite3 mdb idx lockb
		npy npz parquet feather pickle
		swf fla`) {
		binaryExtensions[ext] = true
	}
	// Deviation: native renders these as images or PDF documents. The daemon
	// returns only text, so it refuses them like other binary formats instead of
	// reading their bytes as text.
	for _, ext := range strings.Fields(`png jpg jpeg gif webp pdf`) {
		binaryExtensions[ext] = true
	}
}

// binaryExtension reports whether path has a binary extension, and the
// extension the refusal names. A dotfile such as .bin is refused too, but like
// Node's path.extname its named extension is empty ("a binary  file").
func binaryExtension(path string) (string, bool) {
	ext := strings.ToLower(filepath.Ext(path))
	if ext == "" || !binaryExtensions[ext[1:]] {
		return "", false
	}
	if base := filepath.Base(path); strings.LastIndexByte(base, '.') == 0 {
		return "", true
	}
	return ext, true
}

func binaryReadError(ext string) *circusv1.DeviceResult {
	// The message names the lower-case extension: a.BIN reports ".bin".
	return toolError(fmt.Sprintf("This tool cannot read binary files. The file appears to be a binary %s file. Use a skill for this file type if one is available, or a shell command or script that can read the format.", ext))
}

// nulWindowBytes is the raw prefix searched for NUL bytes: a NUL at byte
// offset 7999 counts, one at offset 8000 does not.
const nulWindowBytes = 8000

func hasNULPrefix(data []byte) bool {
	return bytes.IndexByte(data[:min(len(data), nulWindowBytes)], 0) >= 0
}

const nulReadNotice = "\n\n[This file has NUL bytes in its first 8 KB: binary data, UTF-16/UTF-32 without a byte-order mark, or text with embedded NULs. Edit and Write will refuse it: change it with a shell command that keeps its encoding, or ask the user whether to convert it to UTF-8.]"

const nulError = "File has NUL bytes in its first 8 KB: binary data, UTF-16/UTF-32 without a byte-order mark, or text with embedded NULs. These tools write UTF-8 (or UTF-16LE behind its byte-order mark) and never convert a file's encoding on their own, so nothing was written. If the file is text, make the change with a shell command that reads and writes it in its own encoding (iconv, or PowerShell Get-Content/Set-Content -Encoding), or ask the user whether to convert it to UTF-8 first. To replace the whole file with new UTF-8 text anyway, delete it first, then Write it."

// nulWatcher records whether the first nulWindowBytes written contain NUL.
type nulWatcher struct {
	seen  int
	found bool
}

func (w *nulWatcher) Write(p []byte) (int, error) {
	if w.seen < nulWindowBytes && !w.found {
		w.found = hasNULPrefix(p[:min(len(p), nulWindowBytes-w.seen)])
	}
	w.seen += len(p)
	return len(p), nil
}
