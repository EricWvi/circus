package fstools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/textutil"
)

// Write is a stateless file-writing tool.
type Write struct{}

// NewWrite returns a stateless Write tool.
func NewWrite() *Write { return &Write{} }

// Name returns the device call tool name.
func (*Write) Name() string { return "write" }

// Run writes the complete file content and returns its resulting hash.
func (*Write) Run(ctx context.Context, call *circusv1.DeviceCall) (*circusv1.DeviceResult, error) {
	r := call.GetWrite()
	path, _, err := resolve(r.GetPath(), r.GetCwd())
	if err != nil {
		return nil, err
	}
	unlock := lockPath(path)
	defer unlock()
	if rejection, err := symlinkRejection(path); err != nil {
		return toolError(err.Error()), nil
	} else if rejection != nil {
		return rejection, nil
	}
	data, info, err := load(path)
	created := os.IsNotExist(err)
	if err != nil && !created {
		// Inferred: uncaptured Write filesystem errors use tool-use-error with OS text.
		return toolError(err.Error()), nil
	}
	decoded := textutil.Decode(data)
	// NUL bytes are refused before malformed UTF-8, with the same unwrapped body style.
	// The NUL window counts raw bytes, a UTF-8 byte-order mark included.
	if decoded.Encoding != textutil.EncodingUTF16LE && hasNULPrefix(data) {
		return output(nulError, true), nil
	}
	// Inferred: Write refuses malformed UTF-16LE because re-encoding would
	// rewrite invalid code units; native captured valid UTF-16 and invalid UTF-8.
	if decoded.InvalidUTF8 || decoded.InvalidUTF16 {
		return output(encodingError, true), nil
	}
	if changed(r.ExpectedSha256, data) {
		return toolError(modifiedError), nil
	}
	content := writeContent(decoded, stripForWrite(r.GetPath(), r.GetContent()))
	if err := writeFile(ctx, path, content, info); err != nil {
		return toolError(err.Error()), nil
	}
	body := fmt.Sprintf("The file %s has been updated successfully.%s", r.GetPath(), currentContext)
	// Replacing an existing empty file reads as creation too; a BOM-only
	// UTF-16LE or UTF-8 file is not empty and is updated.
	if created || len(data) == 0 {
		body = fmt.Sprintf("File created successfully at: %s%s", r.GetPath(), currentContext)
	}
	result := output(body, false)
	result.Detail = &circusv1.DeviceResult_Write{Write: &circusv1.WriteResult{Sha256: hash(content), Created: created}}
	return result, nil
}

// writeContent encodes Write content in the existing file's encoding. A UTF-8
// byte-order mark is kept: it precedes non-empty content unless the content
// already starts with one, and empty content leaves an empty file.
func writeContent(decoded textutil.Decoded, text string) []byte {
	if decoded.Encoding == textutil.EncodingUTF8BOM && (text == "" || strings.HasPrefix(text, textutil.UTF8BOM)) {
		return []byte(text)
	}
	return decoded.Encode(text)
}

// stripForWrite removes trailing whitespace from every line of Write content
// and Edit replacement text, except in Markdown (.md or .mdx, any letter case),
// where trailing spaces are line breaks. Other extensions, including
// .markdown, are stripped.
func stripForWrite(path, text string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".md", ".mdx":
		return text
	}
	return textutil.StripTrailingLineWhitespace(text)
}
