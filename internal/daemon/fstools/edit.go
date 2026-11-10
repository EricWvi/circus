package fstools

import (
	"context"
	"fmt"
	"os"
	"strings"
	"unicode/utf8"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/textutil"
)

// Edit is a stateless file-editing tool.
type Edit struct{}

// NewEdit returns a stateless Edit tool.
func NewEdit() *Edit { return &Edit{} }

// Name returns the device call tool name.
func (*Edit) Name() string { return "edit" }

// Run replaces matching file text and returns its resulting hash.
func (*Edit) Run(ctx context.Context, call *circusv1.DeviceCall) (*circusv1.DeviceResult, error) {
	r := call.GetEdit()
	if r.GetOldString() == r.GetNewString() {
		return toolError("No changes to make: old_string and new_string are exactly the same."), nil
	}
	path, cwd, err := resolve(r.GetPath(), r.GetCwd())
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
	if created && r.GetOldString() != "" {
		return toolError(missing(cwd)), nil
	}
	if err != nil && !created {
		// Inferred: uncaptured Edit filesystem errors use tool-use-error with OS text.
		return toolError(err.Error()), nil
	}
	decoded := textutil.Decode(data)
	// Inferred: Edit refuses malformed UTF-16LE because re-encoding would
	// rewrite invalid code units; native captured valid UTF-16 and invalid UTF-8.
	if decoded.InvalidUTF8 || decoded.InvalidUTF16 {
		return toolError(encodingError), nil
	}
	modified := changed(r.ExpectedSha256, data)
	old := strings.ReplaceAll(r.GetOldString(), "\r\n", "\n")
	replacement := strings.ReplaceAll(r.GetNewString(), "\r\n", "\n")
	shownOld := textutil.NodeUTF8([]byte(r.GetOldString()))
	text := strings.ReplaceAll(decoded.Text, "\r\n", "\n")
	eol := "\n"
	if i := strings.Index(decoded.Text, "\n"); i > 0 && decoded.Text[i-1] == '\r' {
		eol = "\r\n"
	}
	// Inferred: mixed-EOL files are normalized to LF for matching, then the
	// complete result uses the file's first line ending.
	var content string
	var resultSize int64
	if old == "" {
		if len(data) != 0 {
			return toolError("Cannot create new file - file already exists."), nil
		}
		if modified {
			return toolError(modifiedError), nil
		}
		content = replacement
		resultSize = int64(len(replacement))
	} else {
		var count int
		content, count, resultSize, err = replaceText(ctx, text, old, replacement, r.GetReplaceAll())
		if err != nil {
			return nil, err
		}
		if count == 0 {
			if modified {
				return toolError(modifiedError), nil
			}
			return toolError("String to replace not found in file.\nString: " + shownOld), nil
		}
		if count > 1 && !r.GetReplaceAll() {
			return toolError(fmt.Sprintf("Found %d matches of the string to replace, but replace_all is false. To replace all occurrences, set replace_all to true. To replace only one occurrence, please provide more context to uniquely identify the instance.\nString: %s", count, shownOld)), nil
		}
	}
	// Inferred: reject an expanded result before allocating it, using the same
	// whole-file limit as the input. With replace_all=false only one match counts.
	if resultSize > wholeFileSizeLimitBytes || editDiskSize(content, eol, decoded.Encoding) > wholeFileSizeLimitBytes {
		return toolError(wholeFileSizeError), nil
	}
	if eol == "\r\n" {
		content = strings.ReplaceAll(content, "\n", eol)
	}
	raw := decoded.Encode(content)
	if err := writeFile(ctx, path, raw, info); err != nil {
		return toolError(err.Error()), nil
	}
	body := fmt.Sprintf("The file %s has been updated successfully.", r.GetPath())
	if r.GetReplaceAll() {
		body = fmt.Sprintf("The file %s has been updated. All occurrences were successfully replaced.", r.GetPath())
	}
	switch {
	case modified:
		body += modifiedContext
	case r.ExpectedSha256 != nil || created:
		body += currentContext
	}
	result := output(body, false)
	result.Detail = &circusv1.DeviceResult_Edit{Edit: &circusv1.EditResult{
		Sha256: hash(raw), ModifiedSinceRead: modified, Created: created,
	}}
	return result, nil
}

func editDiskSize(content, eol string, encoding textutil.Encoding) int64 {
	var size int64
	if encoding == textutil.EncodingUTF16LE {
		size = 2 // byte-order mark
	}
	for _, r := range content {
		if encoding == textutil.EncodingUTF16LE {
			if r > 0xffff {
				size += 4
			} else {
				size += 2
			}
			if eol == "\r\n" && r == '\n' {
				size += 2
			}
			continue
		}
		size += int64(utf8.RuneLen(r))
		if eol == "\r\n" && r == '\n' {
			size++
		}
	}
	return size
}

func editedSize(contentLength, oldLength, replacementLength, count int, replaceAll bool) int64 {
	replacements := 1
	if replaceAll {
		replacements = count
	}
	return int64(contentLength) + int64(replacements)*(int64(replacementLength)-int64(oldLength))
}

func replaceText(ctx context.Context, content, old, replacement string, replaceAll bool) (string, int, int64, error) {
	if count := strings.Count(content, old); count > 0 {
		resultSize := editedSize(len(content), len(old), len(replacement), count, replaceAll)
		if count > 1 && !replaceAll || resultSize > wholeFileSizeLimitBytes {
			return "", count, resultSize, nil
		}
		return strings.ReplaceAll(content, old, replacement), count, resultSize, nil
	}
	if !strings.ContainsAny(old, "\"'“”‘’") {
		return "", 0, 0, nil
	}
	// Inferred: quote tolerance is a fallback after literal matching, limited
	// to straight/curly double and single quotes; no other fuzzy matching is performed.
	pattern := strings.Map(normalizeQuote, old)
	failure, err := quoteFailureTable(ctx, pattern)
	if err != nil {
		return "", 0, 0, err
	}
	count := 0
	resultSize := int64(len(content))
	err = scanQuoteMatches(ctx, content, pattern, failure, func(start, end int) {
		count++
		if !replaceAll && count > 1 {
			return
		}
		matched := content[start:end]
		replacementLength := len(replacement)
		if strings.ContainsAny(matched, "“”") {
			replacementLength += 2 * strings.Count(replacement, "\"")
		}
		if strings.ContainsAny(matched, "‘’") {
			replacementLength += 2 * strings.Count(replacement, "'")
		}
		resultSize += int64(replacementLength - (end - start))
	})
	if err != nil || count == 0 || count > 1 && !replaceAll || resultSize > wholeFileSizeLimitBytes {
		return "", count, resultSize, err
	}

	var b strings.Builder
	b.Grow(int(resultSize))
	last := 0
	err = scanQuoteMatches(ctx, content, pattern, failure, func(start, end int) {
		b.WriteString(content[last:start])
		b.WriteString(quoteReplacement(content[start:end], replacement))
		last = end
	})
	if err != nil {
		return "", count, resultSize, err
	}
	b.WriteString(content[last:])
	return b.String(), count, resultSize, nil
}

func normalizeQuote(r rune) rune {
	switch r {
	case '“', '”':
		return '"'
	case '‘', '’':
		return '\''
	default:
		return r
	}
}

func quoteFailureTable(ctx context.Context, pattern string) ([]int32, error) {
	failure := make([]int32, len(pattern))
	for i, j := 1, 0; i < len(pattern); i++ {
		if i%(64<<10) == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		for j > 0 && pattern[i] != pattern[j] {
			j = int(failure[j-1])
		}
		if pattern[i] == pattern[j] {
			j++
		}
		failure[i] = int32(j)
	}
	return failure, nil
}

// scanQuoteMatches applies KMP to quote-normalized UTF-8 bytes. The start
// offsets ring retains only pattern-sized state, not a rune copy of the file.
func scanQuoteMatches(ctx context.Context, content, pattern string, failure []int32, visit func(start, end int)) error {
	starts := make([]int32, len(pattern))
	matched, normalizedPosition, nextCheck := 0, 0, 64<<10
	consume := func(b byte, offset, end int) {
		starts[normalizedPosition%len(pattern)] = int32(offset)
		normalizedPosition++
		for matched > 0 && pattern[matched] != b {
			matched = int(failure[matched-1])
		}
		if pattern[matched] == b {
			matched++
		}
		if matched == len(pattern) {
			start := int(starts[(normalizedPosition-len(pattern))%len(pattern)])
			visit(start, end)
			// Replacements are non-overlapping, like strings.ReplaceAll.
			matched = 0
		}
	}
	for offset, r := range content {
		if offset >= nextCheck {
			if err := ctx.Err(); err != nil {
				return err
			}
			nextCheck = offset + 64<<10
		}
		normalized := normalizeQuote(r)
		if normalized != r {
			consume(byte(normalized), offset, offset+utf8.RuneLen(r))
			continue
		}
		for i := 0; i < utf8.RuneLen(r); i++ {
			consume(content[offset+i], offset+i, offset+i+1)
		}
	}
	return ctx.Err()
}

func quoteReplacement(matched, replacement string) string {
	next := replacement
	if strings.ContainsAny(matched, "“”") {
		// Inferred: double quotes in replacement text alternate opening
		// and closing curly quotes when the matched text uses curly quotes.
		opening := true
		next = strings.Map(func(r rune) rune {
			if r != '"' {
				return r
			}
			opening = !opening
			if !opening {
				return '“'
			}
			return '”'
		}, next)
	}
	if strings.ContainsAny(matched, "‘’") {
		// Inferred: straight apostrophes become right single quotation
		// marks when the matched text uses either curly single quote.
		next = strings.Map(func(r rune) rune {
			if r == '\'' {
				return '’'
			}
			return r
		}, next)
	}
	return next
}
