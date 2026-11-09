package fstools

import (
	"context"
	"fmt"
	"os"
	"strings"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
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
	modified := changed(r.ExpectedSha256, data)
	old, replacement := r.GetOldString(), r.GetNewString()
	if strings.Contains(string(data), "\r\n") {
		// Inferred: for multiline edits in a CRLF file, normalize only the
		// supplied strings to CRLF; bytes outside the replacement stay intact.
		old = strings.ReplaceAll(strings.ReplaceAll(old, "\r\n", "\n"), "\n", "\r\n")
		replacement = strings.ReplaceAll(strings.ReplaceAll(replacement, "\r\n", "\n"), "\n", "\r\n")
	}
	var content string
	if old == "" {
		if len(data) != 0 {
			return toolError("Cannot create new file - file already exists."), nil
		}
		if modified {
			return toolError(modifiedError), nil
		}
		content = replacement
	} else {
		var count int
		content, count = replaceText(string(data), old, replacement)
		if count == 0 {
			if modified {
				return toolError(modifiedError), nil
			}
			return toolError("String to replace not found in file.\nString: " + r.GetOldString()), nil
		}
		if count > 1 && !r.GetReplaceAll() {
			return toolError(fmt.Sprintf("Found %d matches of the string to replace, but replace_all is false. To replace all occurrences, set replace_all to true. To replace only one occurrence, please provide more context to uniquely identify the instance.\nString: %s", count, r.GetOldString())), nil
		}
	}
	if err := writeFile(ctx, path, []byte(content), info); err != nil {
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
		Sha256: hash([]byte(content)), ModifiedSinceRead: modified, Created: created,
	}}
	return result, nil
}

func replaceText(content, old, replacement string) (string, int) {
	if count := strings.Count(content, old); count > 0 {
		return strings.ReplaceAll(content, old, replacement), count
	}
	// Inferred: quote tolerance is a fallback after literal matching, limited
	// to straight/curly double quotes; no other fuzzy matching is performed.
	text, target := []rune(content), []rune(old)
	normalize := func(r rune) rune {
		if r == '“' || r == '”' {
			return '"'
		}
		return r
	}
	var b strings.Builder
	count, last := 0, 0
	for i := 0; i+len(target) <= len(text); {
		match := true
		for j := range target {
			if normalize(text[i+j]) != normalize(target[j]) {
				match = false
				break
			}
		}
		if !match {
			i++
			continue
		}
		b.WriteString(string(text[last:i]))
		next := replacement
		if strings.ContainsAny(string(text[i:i+len(target)]), "“”") {
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
		b.WriteString(next)
		i += len(target)
		last = i
		count++
	}
	b.WriteString(string(text[last:]))
	return b.String(), count
}
