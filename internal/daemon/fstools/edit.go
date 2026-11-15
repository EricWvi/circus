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
	// An empty old_string on a non-empty file fails before any encoding check.
	// Byte-order marks do not count: a UTF-8 or UTF-16LE BOM-only file is empty.
	decoded := textutil.Decode(data)
	if r.GetOldString() == "" && len(decoded.Text) != 0 {
		return toolError("Cannot create new file - file already exists."), nil
	}
	// The NUL window counts raw bytes, a UTF-8 byte-order mark included.
	if decoded.Encoding != textutil.EncodingUTF16LE && hasNULPrefix(data) {
		return toolError(nulError), nil
	}
	// Inferred: Edit refuses malformed UTF-16LE because re-encoding would
	// rewrite invalid code units; native captured valid UTF-16 and invalid UTF-8.
	if decoded.InvalidUTF8 || decoded.InvalidUTF16 {
		return toolError(encodingError), nil
	}
	modified := changed(r.ExpectedSha256, data)
	newString := r.GetNewString()
	if !created {
		// Replacement text for an existing file loses trailing line whitespace
		// like Write content; text creating a missing file is kept verbatim.
		newString = stripForWrite(r.GetPath(), newString)
	}
	// The file is normalized to LF for matching, but old_string and new_string
	// are used as given: an old_string with CRLF does not match, and CRLF in
	// new_string survives in an LF file. The complete result is written with
	// the ending chosen by lineEnding.
	old, replacement := r.GetOldString(), newString
	shownOld := textutil.NodeUTF8([]byte(r.GetOldString()))
	text := strings.ReplaceAll(decoded.Text, "\r\n", "\n")
	// The line-ending window counts a UTF-8 byte-order mark as one code unit,
	// though matching and the written text exclude it; Encode writes it back.
	window := decoded.Text
	if decoded.Encoding == textutil.EncodingUTF8BOM {
		window = textutil.UTF8BOM + window
	}
	eol := lineEnding(window)
	var content string
	var resultSize int64
	if old == "" {
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
		// Every LF or CRLF becomes CRLF, so a CR left before an LF by the
		// normalization (from CR CR LF) is not doubled.
		content = strings.ReplaceAll(strings.ReplaceAll(content, "\r\n", "\n"), "\n", eol)
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

// lineEndingWindowUnits is the decoded prefix, in UTF-16 code units, whose
// line endings decide Edit's output ending: an LF at code unit 4095 counts,
// one at 4096 does not. A CJK character is one unit and an emoji two; a
// UTF-16LE file counts its decoded text without the byte-order mark.
const lineEndingWindowUnits = 4096

// lineEnding returns CRLF when CRLF endings outnumber lone LF endings in the
// first 4096 UTF-16 code units of the decoded text, and LF otherwise, ties
// included.
func lineEnding(text string) string {
	crlf, lf, units := 0, 0, 0
	previous := rune(0)
	for _, r := range text {
		units++
		if r > 0xffff {
			units++
		}
		if units > lineEndingWindowUnits {
			break
		}
		if r == '\n' {
			if previous == '\r' {
				crlf++
			} else {
				lf++
			}
		}
		previous = r
	}
	if crlf > lf {
		return "\r\n"
	}
	return "\n"
}

func editDiskSize(content, eol string, encoding textutil.Encoding) int64 {
	var size int64
	switch encoding {
	case textutil.EncodingUTF16LE:
		size = 2 // byte-order mark
	case textutil.EncodingUTF8BOM:
		size = 3
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

// deletionPattern extends a deleted old_string with its following LF: when
// the replacement is empty, old_string does not end in LF and the content holds
// old_string followed by LF, that longer text is what gets replaced, so a
// deleted line takes its line ending along. Matching and the match count still
// use old_string alone; with replace_all, occurrences without a following LF
// are then left in place.
func deletionPattern(content, old, replacement string) string {
	if replacement == "" && !strings.HasSuffix(old, "\n") && strings.Contains(content, old+"\n") {
		return old + "\n"
	}
	return old
}

func replaceLiteral(content, pattern, replacement string, replaceAll bool) (string, int64) {
	count := 1
	if replaceAll {
		count = strings.Count(content, pattern)
	}
	resultSize := editedSize(len(content), len(pattern), len(replacement), count, true)
	if resultSize > wholeFileSizeLimitBytes {
		return "", resultSize
	}
	if replaceAll {
		return strings.ReplaceAll(content, pattern, replacement), resultSize
	}
	return strings.Replace(content, pattern, replacement, 1), resultSize
}

func replaceText(ctx context.Context, content, old, replacement string, replaceAll bool) (string, int, int64, error) {
	if count := strings.Count(content, old); count > 0 {
		if count > 1 && !replaceAll {
			return "", count, editedSize(len(content), len(old), len(replacement), count, false), nil
		}
		result, resultSize := replaceLiteral(content, deletionPattern(content, old, replacement), replacement, replaceAll)
		return result, count, resultSize, nil
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
	firstStart, firstEnd := 0, 0
	err = scanQuoteMatches(ctx, content, pattern, failure, func(start, end int) {
		count++
		if count == 1 {
			firstStart, firstEnd = start, end
		}
	})
	if err != nil || count == 0 {
		return "", count, int64(len(content)), err
	}
	// The edit uses the file's own text of the first match: only occurrences
	// identical to it count and are replaced, also with replace_all, so another
	// curly variant of the same words neither makes the match ambiguous nor
	// changes. A deletion extends that text by the following LF when the
	// content holds one after it.
	actual := content[firstStart:firstEnd]
	count = strings.Count(content, actual)
	if count > 1 && !replaceAll {
		return "", count, int64(len(content)), nil
	}
	pattern, text := deletionPattern(content, actual, replacement), quoteReplacement(actual, replacement)
	result, resultSize := replaceLiteral(content, pattern, text, replaceAll)
	return result, count, resultSize, nil
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
