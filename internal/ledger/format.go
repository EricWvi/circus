// Package ledger stores project decisions as immutable Git snapshots and
// maintains their derived SQLite index.
package ledger

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/EricWvi/circus/internal/store"
	"gopkg.in/yaml.v3"
)

const (
	MaxDecisionFileBytes = 256 << 10
	MaxNameRunes         = 60
	indexName            = "INDEX.md"
	// Every table cell byte fits in an eight-byte numeric entity.
	maxIndexBytes = 64 + store.MaxLedgerDecisions*(8*(store.MaxLedgerTitleBytes+store.MaxLedgerDirBytes)+32)
)

// Decision is the complete content of a numbered ledger entry.
type Decision struct {
	Dir    string
	Number uint32
	Title  string
	Body   []byte
}

func numberName(number uint32) string {
	return "D" + strconv.FormatUint(uint64(number), 10)
}

// FileName derives the base name from the number and title.
func FileName(number uint32, title string) string {
	var name [MaxNameRunes]rune
	size, end := 0, 0
	lastDash := false
	for _, r := range title {
		if unicode.IsSpace(r) || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || strings.ContainsRune(`/\:*?"<>|`, r) {
			r = '-'
		}
		trim := r == '-' || r == '.'
		if size == 0 && trim || r == '-' && lastDash {
			continue
		}
		if size < len(name) {
			name[size] = r
			size++
		}
		if !trim {
			end = size
			if end == len(name) {
				break
			}
		}
		lastDash = r == '-'
	}
	return numberName(number) + "-" + strings.TrimRight(string(name[:end]), "-.") + ".md"
}

func decisionPath(d Decision) string {
	if d.Dir == "" {
		return FileName(d.Number, d.Title)
	}
	return d.Dir + "/" + FileName(d.Number, d.Title)
}

// appendHeader always emits a double-quoted YAML string. Go's quoted
// string escapes are also YAML escapes, including those for control runes.
func appendHeader(dst []byte, number uint32, title string) []byte {
	dst = append(dst, "---\nnumber: D"...)
	dst = strconv.AppendUint(dst, uint64(number), 10)
	dst = append(dst, "\ntitle: "...)
	dst = strconv.AppendQuote(dst, title)
	return append(dst, "\n---\n\n"...)
}

// Render emits YAML front matter, a blank line, and the original body bytes.
func Render(d Decision) ([]byte, error) {
	if !utf8.ValidString(d.Title) {
		return nil, errors.New("title must be valid UTF-8")
	}
	return append(appendHeader(nil, d.Number, d.Title), d.Body...), nil
}

// Parse reads the two-line front matter without rewriting any byte of the
// body. The returned body aliases data. A file's directory comes from its path.
func Parse(data []byte) (Decision, error) {
	var d Decision
	if bytes.HasPrefix(data, []byte("\ufeff")) {
		return d, errors.New("remove the byte order mark before the front matter")
	}
	open, rest, _ := readLine(data)
	if string(open) != "---" {
		return d, errors.New("missing opening front matter delimiter")
	}
	number, rest, _ := readLine(rest)
	title, rest, _ := readLine(rest)
	close, rest, _ := readLine(rest)
	for _, line := range [][]byte{number, title, close} {
		line := bytes.TrimSpace(line)
		switch {
		case bytes.HasPrefix(line, []byte("...")):
			return d, errors.New("remove the YAML document-end marker from front matter")
		case bytes.HasPrefix(line, []byte("#")):
			return d, errors.New("remove comments from front matter")
		case bytes.HasPrefix(line, []byte("{")) || bytes.HasPrefix(line, []byte("[")):
			return d, errors.New("use two separate lines instead of a YAML flow mapping")
		}
	}
	number, err := frontMatterValue("number", "first", number)
	if err != nil {
		return d, err
	}
	title, err = frontMatterValue("title", "second", title)
	if err != nil {
		return d, err
	}
	for _, raw := range [][]byte{number, title} {
		switch {
		case raw[0] == '!' || raw[0] == '&' || raw[0] == '*':
			return d, errors.New("remove YAML tags, anchors and aliases from front matter")
		case raw[0] == '|' || raw[0] == '>':
			return d, errors.New("replace the YAML block scalar with a single-line string")
		}
	}
	if string(close) != "---" {
		return d, errors.New("expected the closing front matter delimiter after exactly two keys")
	}
	blank, body, ok := readLine(rest)
	if !ok || len(blank) != 0 {
		return d, errors.New("front matter must be followed by a blank line")
	}
	n, err := frontMatterNumber(number)
	if err != nil {
		return d, err
	}
	value, err := frontMatterString("title", title)
	if err != nil {
		return d, err
	}
	return Decision{Number: n, Title: value, Body: body}, nil
}

// frontMatterValue checks the "key: value" spelling the renderer writes
// and names exactly what differs from it.
func frontMatterValue(key, ordinal string, line []byte) ([]byte, error) {
	rest, ok := bytes.CutPrefix(line, []byte(key+":"))
	if !ok {
		return nil, fmt.Errorf("the %s front matter line must start with %q: front matter holds exactly number and title, in that order, with unquoted keys", ordinal, key+": ")
	}
	value := bytes.TrimLeft(rest, " \t")
	switch {
	case len(value) == 0:
		return nil, fmt.Errorf("%s has no value", key)
	case rest[0] == '\t':
		return nil, fmt.Errorf("use one space, not a tab, after %q", key+":")
	case rest[0] != ' ':
		return nil, fmt.Errorf("put one space after %q", key+":")
	case len(rest)-len(value) > 1:
		return nil, fmt.Errorf("use exactly one space after %q", key+":")
	case value[len(value)-1] == ' ' || value[len(value)-1] == '\t':
		return nil, fmt.Errorf("remove the whitespace after the %s value", key)
	}
	return value, nil
}

func frontMatterNumber(raw []byte) (uint32, error) {
	value := string(raw)
	// The renderer emits the decimal number directly.
	if len(raw) < 2 || raw[0] != 'D' || bytes.IndexFunc(raw[1:], func(r rune) bool { return r < '0' || r > '9' }) != -1 {
		var err error
		value, err = frontMatterString("number", raw)
		if err != nil {
			return 0, err
		}
	}
	n, err := strconv.ParseUint(strings.TrimPrefix(value, "D"), 10, 32)
	if err != nil || value != numberName(uint32(n)) {
		return 0, errors.New("number must be D followed by its decimal value")
	}
	return uint32(n), nil
}

func frontMatterString(key string, raw []byte) (string, error) {
	value := string(raw)
	if raw[0] == '"' {
		if unquoted, err := strconv.Unquote(value); err == nil && utf8.ValidString(unquoted) && strconv.Quote(unquoted) == value {
			return unquoted, nil
		}
	}
	if len(bytes.TrimSpace(raw)) == 0 || raw[0] == ' ' || raw[0] == '\t' {
		return "", errors.New("use exactly one space after each front matter key")
	}
	// Decode the alternate plain or quoted spelling in mapping context.
	var doc yaml.Node
	if err := yaml.Unmarshal(fmt.Appendf(nil, "%s: %s\n", key, raw), &doc); err != nil {
		return "", fmt.Errorf("invalid %s string in front matter: %w", key, err)
	}
	if len(doc.Content) != 1 || len(doc.Content[0].Content) != 2 {
		return "", errors.New("front matter keys and values must be YAML strings")
	}
	mapping := doc.Content[0]
	node := mapping.Content[1]
	if mapping.HeadComment != "" || mapping.LineComment != "" || mapping.FootComment != "" ||
		node.HeadComment != "" || node.LineComment != "" || node.FootComment != "" || mapping.Content[0].LineComment != "" {
		return "", errors.New("remove comments from front matter")
	}
	if node.Style&yaml.TaggedStyle != 0 || node.Anchor != "" || node.Kind == yaml.AliasNode {
		return "", errors.New("remove YAML tags, anchors and aliases from front matter")
	}
	if node.Kind != yaml.ScalarNode || node.ShortTag() != "!!str" {
		return "", errors.New("front matter keys and values must be YAML strings")
	}
	return node.Value, nil
}

func readLine(data []byte) (line, rest []byte, ok bool) {
	line, rest, ok = bytes.Cut(data, []byte("\n"))
	return bytes.TrimSuffix(line, []byte("\r")), rest, ok
}

// RenderIndex returns the generated root table, ordered by number.
func RenderIndex(decisions []Decision) []byte {
	entries := make([]store.LedgerEntry, 0, len(decisions))
	for _, d := range decisions {
		entries = append(entries, store.LedgerEntry{Number: d.Number, Title: d.Title, Dir: d.Dir})
	}
	return renderIndex(entries)
}

func renderIndex(entries []store.LedgerEntry) []byte {
	slices.SortFunc(entries, func(a, b store.LedgerEntry) int {
		return cmp.Compare(a.Number, b.Number)
	})
	b := []byte("| Number | Directory | Title |\n| --- | --- | --- |\n")
	for _, e := range entries {
		b = fmt.Appendf(b, "| %s | %s | %s |\n", numberName(e.Number), tableCell(e.Dir), tableCell(e.Title))
	}
	return b
}

func tableCell(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '|' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case unicode.IsControl(r) || strings.ContainsRune("<>&\u2028\u2029", r):
			fmt.Fprintf(&b, "&#%d;", r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Problem identifies one validation failure and the file or directory it
// concerns. A path of "." refers to the whole ledger.
type Problem struct {
	Path    string
	Message string
}

// ValidationError carries all problems discovered in the submitted tree.
type ValidationError struct {
	Problems []Problem
}

func (e *ValidationError) Error() string {
	if len(e.Problems) == 0 {
		return "ledger validation failed"
	}
	first := e.Problems[0]
	return fmt.Sprintf("ledger validation failed (%d problems): %s: %s", len(e.Problems), first.Path, first.Message)
}

type validation struct {
	problems []Problem
	seen     map[uint32]string
	count    int
}

func (v *validation) add(p, message string) {
	v.problems = append(v.problems, Problem{Path: p, Message: message})
}

func (v *validation) checkDir(p, dir string) {
	if dir == "" {
		return
	}
	if len(dir) > store.MaxLedgerDirBytes {
		v.add(p, "directory exceeds 256 bytes")
	}
	for segment := range strings.SplitSeq(dir, "/") {
		if segment == "" || strings.HasPrefix(segment, ".") || len(segment) > 64 || strings.ContainsFunc(segment, invalidDirRune) {
			v.add(p, "directory segments must be 1 to 64 bytes, use only letters, digits, marks, hyphens, underscores and dots, and start without a dot")
			break
		}
	}
}

func invalidDirRune(r rune) bool {
	return !(unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsMark(r) || r == '-' || r == '_' || r == '.')
}

// Line breaks have their own validation, including the three Unicode
// line separators and the two ASCII newlines.
func invalidTitleControl(r rune) bool {
	return unicode.IsControl(r) && !strings.ContainsRune("\r\n\u0085", r)
}

func (v *validation) checkDecision(p string, d Decision) {
	v.checkDir(p, d.Dir)
	if d.Title == "" || len(d.Title) > store.MaxLedgerTitleBytes || !utf8.ValidString(d.Title) || strings.ContainsAny(d.Title, "\r\n\u0085\u2028\u2029") || strings.ContainsFunc(d.Title, invalidTitleControl) {
		v.add(p, "title must be a nonempty, single UTF-8 line of at most 512 bytes without control characters")
	}
	if d.Number > store.MaxLedgerNumber {
		v.add(p, "number leaves no representable next number")
	}
	if !utf8.Valid(d.Body) {
		v.add(p, "body must be valid UTF-8")
	}
	v.checkIdentity(p, d)
}

func (v *validation) checkIdentity(p string, d Decision) {
	if p != decisionPath(d) {
		v.add(p, "path does not match the number, title and directory")
	}
	if v.seen == nil {
		v.seen = make(map[uint32]string)
	}
	if old, ok := v.seen[d.Number]; ok {
		v.add(p, "number also occurs at "+old)
	} else {
		v.seen[d.Number] = p
	}
}

func (v *validation) checkCount() {
	if v.count > store.MaxLedgerDecisions {
		v.add(".", "ledger exceeds 4096 decisions")
	}
}

func fileDir(p string) string {
	i := strings.LastIndexByte(p, '/')
	if i < 0 {
		return ""
	}
	return p[:i]
}

func decisionFile(p string) bool {
	name := path.Base(p)
	return strings.HasPrefix(name, "D") && strings.HasSuffix(name, ".md")
}

// ValidateTree checks a complete flat map of relative file paths to bytes,
// including the generated table. It reports all path-specific problems.
func ValidateTree(files map[string][]byte) []Problem {
	var v validation
	entries := make([]store.LedgerEntry, 0, min(len(files), store.MaxLedgerDecisions))
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	for _, p := range paths {
		if p == indexName {
			continue
		}
		for dir := fileDir(p); dir != ""; dir = fileDir(dir) {
			if _, exists := files[dir]; exists {
				v.add(p, "directory conflicts with file "+dir)
			}
		}
		data := files[p]
		d, ok := v.parseFile(p, data)
		if ok && len(entries) < store.MaxLedgerDecisions {
			entries = append(entries, entryFor(p, d))
		}
	}
	v.checkCount()
	v.checkIndex(files[indexName], entries)
	return v.problems
}

func (v *validation) parseFile(p string, data []byte) (Decision, bool) {
	if !decisionFile(p) {
		v.checkDir(p, fileDir(p))
		v.add(p, "unexpected file")
		return Decision{}, false
	}
	v.count++
	if len(data) > MaxDecisionFileBytes {
		v.add(p, "decision file exceeds 256 KiB")
	}
	d, err := Parse(data)
	if err != nil {
		v.checkDir(p, fileDir(p))
		v.add(p, err.Error())
		return Decision{}, false
	}
	d.Dir = fileDir(p)
	v.checkDecision(p, d)
	return d, true
}

func entryFor(p string, d Decision) store.LedgerEntry {
	return store.LedgerEntry{Number: d.Number, Dir: d.Dir, Title: d.Title, Path: p}
}

func (v *validation) checkIndex(index []byte, entries []store.LedgerEntry) {
	if !bytes.Equal(index, renderIndex(entries)) {
		v.add(indexName, "missing or does not match the generated table")
	}
}
