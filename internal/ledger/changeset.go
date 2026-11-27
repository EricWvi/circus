package ledger

import (
	"bytes"
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	snapshotfiles "github.com/EricWvi/circus/internal/ledger/snapshot"
	"github.com/EricWvi/circus/internal/store"
	"github.com/go-git/go-git/v5/plumbing"
)

// Changeset bounds beyond those of the main line.
const (
	// MaxChangesetBytes bounds the content of the new and changed files of
	// one changeset, and separately of one merge's resolutions.
	MaxChangesetBytes = 8 << 20
	// MaxChanges bounds the changes of one request: every decision and
	// the index moved.
	MaxChanges = 2 * (store.MaxLedgerDecisions + 1)
	// MaxChangePathBytes bounds a changed path, a decision's or not.
	MaxChangePathBytes = store.MaxLedgerPathBytes
	// MaxConflictBytes bounds the encoded contents of the conflicts one
	// check reports. Conflicts beyond it are reported without contents.
	MaxConflictBytes = 8 << 20
	// MaxProblems bounds the problems one check reports.
	MaxProblems = 1000
	// MaxWarnings bounds the warnings one check reports.
	MaxWarnings = 100
	// MaxViolations bounds the violations of one refusal.
	MaxViolations = 100
	// maxProblemBytes bounds one problem's message.
	maxProblemBytes = 512
	// maxSegmentBytes is the longest file or directory name a changeset's
	// local copy can hold on common file systems.
	maxSegmentBytes = 255
)

// Change is one file of a changeset relative to its base: its complete
// new content, or the removal of a base file.
type Change struct {
	Path    string
	Content []byte
	Deleted bool
}

// File is one file of a commit.
type File struct {
	Path    string
	Content []byte
}

// Version is one side of a conflict; Exists is false where the file does
// not exist.
type Version struct {
	Exists  bool
	Content []byte
}

// Conflict is a file the changeset changed that the main line also
// changed after the changeset's base. Omitted conflicts carry no contents
// (see MaxConflictBytes).
type Conflict struct {
	Path                  string
	Base, Changeset, Main Version
	Omitted               bool
}

// CheckResult is the outcome of a check.
type CheckResult struct {
	Changeset store.LedgerChangeset
	Problems  []Problem
	// FastForward reports that the base's tree is the main line head's
	// tree.
	FastForward bool
	MainCommit  string
	Conflicts   []Conflict
	// Warnings do not stop a merge.
	Warnings []Problem
}

// RequestError refuses a malformed request: a field and what is wrong
// with it.
type RequestError struct {
	Field, Message string
}

func (e *RequestError) Error() string { return "ledger: " + e.Field + ": " + e.Message }

// LimitError refuses a changeset beyond its bounds.
type LimitError struct{ Message string }

func (e *LimitError) Error() string { return "ledger: " + e.Message }

// RepairError refuses an operation on a changeset whose interrupted merge
// or archive could not be repaired.
type RepairError struct {
	ID  string
	Err error
}

func (e *RepairError) Error() string {
	return fmt.Sprintf("ledger: changeset %s cannot be used until its interrupted merge or archive is repaired: %v", e.ID, e.Err)
}

func (e *RepairError) Unwrap() error { return e.Err }

// Violation types of a PreconditionError.
const (
	ViolationState    = "CHANGESET_STATE"
	ViolationProblem  = "LEDGER_PROBLEM"
	ViolationConflict = "UNRESOLVED_CONFLICT"
	// ViolationMainMoved refuses reading conflicts against a main-line
	// commit that is no longer the head.
	ViolationMainMoved = "MAIN_LINE_MOVED"
	// ViolationChangesetChanged refuses reading on from a page of a
	// changeset's conflicts after the changeset was checked again.
	ViolationChangesetChanged = "CHANGESET_CHANGED"
)

// Violation is one reason of a PreconditionError, in the shape of a
// google.rpc.PreconditionFailure violation.
type Violation struct {
	Type, Subject, Description string
}

// PreconditionError refuses an operation that the changeset's state or
// content does not allow: it is not open, its validation fails, or a
// conflict is unresolved.
type PreconditionError struct {
	Violations []Violation
}

func (e *PreconditionError) Error() string {
	if len(e.Violations) == 0 {
		return "ledger: changeset precondition failed"
	}
	v := e.Violations[0]
	return fmt.Sprintf("ledger: changeset precondition failed (%d): %s: %s", len(e.Violations), v.Subject, v.Description)
}

func stateError(c store.LedgerChangeset) error {
	return &PreconditionError{Violations: []Violation{{Type: ViolationState, Subject: c.ID, Description: "the changeset is " + string(c.State)}}}
}

// violations turns problems and unresolved conflicts into a bounded
// PreconditionError.
func violations(problems []Problem, unresolved []string) error {
	var out []Violation
	for _, p := range problems {
		out = append(out, Violation{Type: ViolationProblem, Subject: p.Path, Description: p.Message})
	}
	for _, p := range unresolved {
		out = append(out, Violation{Type: ViolationConflict, Subject: p,
			Description: "the main line changed this file after the changeset's base; give its final content in resolved"})
	}
	if len(out) == 0 {
		return nil
	}
	if len(out) > MaxViolations {
		more := len(out) - MaxViolations + 1
		out = append(out[:MaxViolations-1], Violation{Type: ViolationProblem, Subject: ".", Description: fmt.Sprintf("%d more not shown", more)})
	}
	return &PreconditionError{Violations: out}
}

// boundProblems keeps at most MaxProblems problems with bounded messages.
func boundProblems(problems []Problem) []Problem {
	for i := range problems {
		problems[i].Message = truncate(problems[i].Message, maxProblemBytes)
	}
	if len(problems) > MaxProblems {
		more := len(problems) - MaxProblems + 1
		problems = append(problems[:MaxProblems-1], Problem{Path: ".", Message: fmt.Sprintf("%d more problems not shown", more)})
	}
	return problems
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

// EncodedBytes is the size of b as a JSON string, which bounds its size
// in the binary encoding too: two bytes for a quote or a backslash, up to
// six for another control character.
func EncodedBytes(b []byte) int {
	n := len(b) + 2
	for _, c := range b {
		switch {
		case c == '"' || c == '\\' || c == '\b' || c == '\f' || c == '\n' || c == '\r' || c == '\t':
			n++
		case c < ' ':
			n += 5
		}
	}
	return n
}

// checkChangePath reports what makes p unusable as a changed path, or "".
// Whether it may hold a decision is validation's business; this only
// keeps paths a Git tree and a local directory can hold.
func checkChangePath(p string) string {
	switch {
	case p == "":
		return "path is empty"
	case len(p) > MaxChangePathBytes:
		return fmt.Sprintf("path exceeds %d bytes", MaxChangePathBytes)
	case !utf8.ValidString(p):
		return "path must be valid UTF-8"
	case strings.ContainsFunc(p, unicode.IsControl):
		return "path must not contain control characters"
	}
	for segment := range strings.SplitSeq(p, "/") {
		switch {
		case segment == "" || segment == "." || segment == "..":
			return "path must be relative and '/'-separated, without empty, '.' or '..' segments"
		case len(segment) > maxSegmentBytes:
			return fmt.Sprintf("a path segment exceeds %d bytes", maxSegmentBytes)
		}
	}
	return ""
}

// CheckChanges checks the shape of a request's changes: well-formed
// unique paths, their number and the size of their content. field names
// the request field, such as "changes".
func CheckChanges(field string, changes []Change) error {
	if len(changes) > MaxChanges {
		return &LimitError{Message: fmt.Sprintf("a request holds at most %d changes", MaxChanges)}
	}
	seen := make(map[string]struct{}, len(changes))
	total := 0
	for i, c := range changes {
		name := fmt.Sprintf("%s[%d].path", field, i)
		if msg := checkChangePath(c.Path); msg != "" {
			return &RequestError{Field: name, Message: msg}
		}
		if _, ok := seen[c.Path]; ok {
			return &RequestError{Field: name, Message: "path occurs twice"}
		}
		seen[c.Path] = struct{}{}
		if c.Deleted && len(c.Content) != 0 {
			return &RequestError{Field: fmt.Sprintf("%s[%d]", field, i), Message: "a deleted file has no content"}
		}
		total += len(c.Content)
	}
	if total > MaxChangesetBytes {
		return &LimitError{Message: fmt.Sprintf("a changeset holds at most %d bytes of changed content", MaxChangesetBytes)}
	}
	return nil
}

// reference is the snapshot a changeset is validated against: its base
// for a check, the main line's head for a merge.
type reference struct {
	snapshot
	byPath map[string]snapshotEntry
	// files maps every file, the index included, to its blob.
	files map[string]plumbing.Hash
}

func newReference(s snapshot) reference {
	ref := reference{snapshot: s, byPath: make(map[string]snapshotEntry, len(s.entries)), files: make(map[string]plumbing.Hash, len(s.entries)+1)}
	for _, e := range s.entries {
		ref.byPath[e.Path] = e
		ref.files[e.Path] = e.blob
	}
	if !s.index.IsZero() {
		ref.files[indexName] = s.index
	}
	return ref
}

// change is an effective change: content that differs from the
// reference's file at the path, or the removal of one of its files.
type change struct {
	path    string
	content []byte
	deleted bool
}

// validateChanges checks the changes against the reference and returns
// the problems and the temporary numbers' letters with their paths.
func validateChanges(ref reference, changes []change) ([]Problem, map[string]string) {
	var v validation
	changes = slices.Clone(changes)
	slices.SortFunc(changes, func(a, b change) int { return strings.Compare(a.path, b.path) })
	removed := make(map[uint32]string)
	for _, c := range changes {
		switch {
		case !c.deleted:
		case c.path == indexName:
			v.add(c.path, "INDEX.md is generated and cannot be changed")
		default:
			if e, ok := ref.byPath[c.path]; ok {
				removed[e.Number] = c.path
			}
		}
	}
	present := make(map[uint32]string)
	temporary := make(map[string]string)
	for _, c := range changes {
		if c.deleted {
			continue
		}
		p := c.path
		if p == indexName {
			v.add(p, "INDEX.md is generated and cannot be changed")
			continue
		}
		if !decisionFile(p) {
			v.checkDir(p, fileDir(p))
			v.add(p, "unexpected file: the ledger holds only decision files named <dir>/D<n>-<name>.md")
			continue
		}
		if len(c.content) > MaxDecisionFileBytes {
			v.add(p, "decision file exceeds 256 KiB")
		}
		d, letters, err := parse(c.content, true)
		d.Dir = fileDir(p)
		v.checkDir(p, d.Dir)
		if err != nil {
			v.add(p, err.Error())
			continue
		}
		v.checkContent(p, d)
		old, existing := ref.byPath[p]
		if letters != "" {
			if existing {
				v.add(p, "the number of an existing decision cannot change; it is "+numberName(old.Number))
			}
			if want := joinPath(d.Dir, fileName(temporaryPrefix+letters, d.Title)); p != want {
				v.add(p, "path does not match the number, title and directory; expected "+want)
			}
			if other, ok := temporary[letters]; ok {
				v.add(p, "temporary number "+temporaryPrefix+letters+" also occurs at "+other)
			} else {
				temporary[letters] = p
			}
			continue
		}
		if d.Number > store.MaxLedgerNumber {
			v.add(p, "number leaves no representable next number")
		}
		if want := decisionPath(d); p != want {
			v.add(p, "path does not match the number, title and directory; expected "+want)
		}
		switch e, exists := ref.entries[d.Number]; {
		case existing && old.Number != d.Number:
			v.add(p, "the number of an existing decision cannot change; it is "+numberName(old.Number))
		case existing:
		case !exists:
			v.add(p, "a new decision takes a temporary number D?<letters>; numbers are assigned when the changeset is merged")
		case removed[d.Number] == "":
			v.add(p, "number also occurs at "+e.Path)
		}
		if other, ok := present[d.Number]; ok {
			v.add(p, "number also occurs at "+other)
		} else {
			present[d.Number] = p
		}
	}
	for _, number := range slices.Sorted(maps.Keys(removed)) {
		if _, ok := present[number]; !ok {
			v.add(removed[number], "existing decisions cannot be removed; to move or rename one, add it at its new path with the same number")
		}
	}
	if len(ref.entries)+len(temporary) > store.MaxLedgerDecisions {
		v.add(".", "ledger exceeds 4096 decisions")
	}
	v.problems = append(v.problems, foldCollisions(ref, changes)...)
	return v.problems, temporary
}

// foldCollisions reports each changed file whose path, or a directory on
// it, is one name with another path of the resulting ledger on a file
// system that ignores case and Unicode normalization (macOS by default),
// as snapshotfiles.FoldKey compares names: such a ledger could not be written
// to a session's snapshot there.
func foldCollisions(ref reference, changes []change) []Problem {
	files := make(map[string]bool, len(ref.files)+len(changes))
	for p := range ref.files {
		files[p] = true
	}
	for _, c := range changes {
		files[c.path] = !c.deleted
	}
	// Every name of the result, files and their directories, by key.
	names := make(map[string]map[string]bool)
	for p, exists := range files {
		if !exists {
			continue
		}
		for i := 0; i <= len(p); i++ {
			if i < len(p) && p[i] != '/' {
				continue
			}
			key := snapshotfiles.FoldKey(p[:i])
			if names[key] == nil {
				names[key] = make(map[string]bool, 1)
			}
			names[key][p[:i]] = true
		}
	}
	var out []Problem
	for _, c := range changes {
		if c.deleted {
			continue
		}
		p := c.path
		for i := 0; i <= len(p); i++ {
			if i < len(p) && p[i] != '/' {
				continue
			}
			var others []string
			for name := range names[snapshotfiles.FoldKey(p[:i])] {
				if name != p[:i] {
					others = append(others, name)
				}
			}
			if len(others) > 0 {
				slices.Sort(others)
				out = append(out, Problem{Path: p, Message: fmt.Sprintf("%q and %q differ only in case or Unicode normalization, "+
					"so they are one name on macOS; rename one of them", p[:i], others[0])})
				break
			}
		}
	}
	return out
}

// checkContent is checkDecision without the number and the path, which
// a changeset's validation checks itself.
func (v *validation) checkContent(p string, d Decision) {
	if d.Title == "" || len(d.Title) > store.MaxLedgerTitleBytes || !utf8.ValidString(d.Title) || strings.ContainsAny(d.Title, "\r\n\u0085  ") || strings.ContainsFunc(d.Title, invalidTitleControl) {
		v.add(p, "title must be a nonempty, single UTF-8 line of at most 512 bytes without control characters")
	}
	if !utf8.Valid(d.Body) {
		v.add(p, "body must be valid UTF-8")
	}
}

func joinPath(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

// assignNumbers gives the temporary numbers consecutive numbers from
// next, in the order a, b, …, z, aa, ab, …: shorter letters first, then
// alphabetically.
func assignNumbers(temporary map[string]string, next uint32) (map[string]uint32, error) {
	letters := slices.SortedFunc(maps.Keys(temporary), func(a, b string) int {
		return cmp.Or(cmp.Compare(len(a), len(b)), strings.Compare(a, b))
	})
	if uint64(next)+uint64(len(letters)) > uint64(store.MaxLedgerNumber)+1 {
		return nil, &PreconditionError{Violations: []Violation{{Type: ViolationProblem, Subject: ".", Description: "no numbers are left for the new decisions"}}}
	}
	out := make(map[string]uint32, len(letters))
	for i, l := range letters {
		out[l] = next + uint32(i)
	}
	return out, nil
}

func wordByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_'
}

// temporaryReferences calls visit for each reference to a temporary
// number in content: "D?" and one to four lowercase letters, with no
// letter, digit or underscore right before or after it. visit gets the
// reference's bounds and letters.
//
// Occurrences inside Markdown code are not references, so a decision can
// show the notation: inline code spans (a run of n backticks up to the
// next run of exactly n in the same paragraph; an unmatched run is a
// literal backtick) and fenced code blocks (a line of three or more
// backticks or tildes indented by at most three spaces, up to a line of
// at least as many of the same character, or the end of the file).
// Paragraphs end at blank lines and fences. Indented code blocks are read
// as text: inside the lists decisions use, indentation is ambiguous. The
// front matter's number line has no backtick before it, so the number in
// it is always a reference.
func temporaryReferences(content []byte, visit func(start, end int, letters string)) {
	paragraph := 0
	var fence byte
	fenceLen := 0
	for pos := 0; pos < len(content); {
		next := len(content)
		if i := bytes.IndexByte(content[pos:], '\n'); i >= 0 {
			next = pos + i + 1
		}
		line := content[pos:next]
		switch {
		case fence != 0:
			if c, n, rest := fenceLine(line); c == fence && n >= fenceLen && blank(rest) {
				fence = 0
			}
			paragraph = next
		case blank(line):
			inlineReferences(content, paragraph, pos, visit)
			paragraph = next
		default:
			// A backtick fence's info string has no backtick; such a line
			// is inline code instead.
			if c, n, rest := fenceLine(line); c == '~' || c == '`' && bytes.IndexByte(rest, '`') < 0 {
				inlineReferences(content, paragraph, pos, visit)
				fence, fenceLen = c, n
				paragraph = next
			}
		}
		pos = next
	}
	if fence == 0 {
		inlineReferences(content, paragraph, len(content), visit)
	}
}

func blank(line []byte) bool { return len(bytes.Trim(line, " \t\r\n")) == 0 }

// fenceLine returns the fence character and length of a line that starts,
// after at most three spaces, with three or more backticks or tildes, and
// the rest of the line; c is 0 for any other line.
func fenceLine(line []byte) (c byte, n int, rest []byte) {
	i := 0
	for i < 3 && i < len(line) && line[i] == ' ' {
		i++
	}
	if i == len(line) || line[i] != '`' && line[i] != '~' {
		return 0, 0, nil
	}
	end := i
	for end < len(line) && line[end] == line[i] {
		end++
	}
	if end-i < 3 {
		return 0, 0, nil
	}
	return line[i], end - i, line[end:]
}

// inlineReferences visits the references in the paragraph
// content[from:to] outside its code spans.
func inlineReferences(content []byte, from, to int, visit func(start, end int, letters string)) {
	text := from
	// unmatched holds, by run length, a position after which no run of
	// that length follows, so a paragraph of unmatched runs is not
	// searched again and again.
	var unmatched map[int]int
	for i := from; i < to; {
		switch {
		case content[i] == '\\' && i+1 < to && (content[i+1] == '`' || content[i+1] == '\\'):
			// An escaped backtick is literal, and so is an escaped
			// backslash, which escapes nothing.
			i += 2
			continue
		case content[i] != '`':
			i++
			continue
		}
		open := backtickRunEnd(content, i, to)
		n := open - i
		closing := -1
		if after, ok := unmatched[n]; !ok || open < after {
			for j := open; j < to; {
				if content[j] != '`' {
					j++
					continue
				}
				k := backtickRunEnd(content, j, to)
				if k-j == n {
					closing = j
					break
				}
				j = k
			}
			if closing < 0 {
				if unmatched == nil {
					unmatched = make(map[int]int)
				}
				unmatched[n] = open
			}
		}
		if closing < 0 {
			i = open
			continue
		}
		plainReferences(content, text, i, visit)
		i = closing + n
		text = i
	}
	plainReferences(content, text, to, visit)
}

// backtickRunEnd returns the end of the run of backticks at content[i].
func backtickRunEnd(content []byte, i, to int) int {
	for i < to && content[i] == '`' {
		i++
	}
	return i
}

// plainReferences visits the references in content[from:to], text without
// code. The bytes around it are backticks, line ends or nothing.
func plainReferences(content []byte, from, to int, visit func(start, end int, letters string)) {
	for i := from; i+2 < to; i++ {
		if content[i] != 'D' || content[i+1] != '?' || i > 0 && wordByte(content[i-1]) {
			continue
		}
		end := i + 2
		for end < to && content[end] >= 'a' && content[end] <= 'z' {
			end++
		}
		if end < len(content) && wordByte(content[end]) || !store.ValidTemporaryLetters(string(content[i+2:end])) {
			continue
		}
		visit(i, end, string(content[i+2:end]))
		i = end - 1
	}
}

// rewriteTemporary replaces each reference to a temporary number with its
// assigned number. One whose letters are not assigned is kept, as text
// about the notation is, and so is the notation inside Markdown code,
// which is no reference.
func rewriteTemporary(content []byte, numbers map[string]uint32) []byte {
	var out []byte
	last := 0
	temporaryReferences(content, func(start, end int, letters string) {
		n, ok := numbers[letters]
		if !ok {
			return
		}
		out = append(out, content[last:start]...)
		out = append(out, 'D')
		out = strconv.AppendUint(out, uint64(n), 10)
		last = end
	})
	if out == nil {
		return content
	}
	return append(out, content[last:]...)
}

// unknownReferences warns of references in the changed files to
// temporary numbers that no new decision of the changeset has: a merge
// keeps them as they are, which a typo would leave on the main line.
// The notation inside Markdown code is no reference and no warning.
func unknownReferences(changes []change, temporary map[string]string) []Problem {
	var out []Problem
	for _, ch := range changes {
		seen := make(map[string]bool)
		temporaryReferences(ch.content, func(_, _ int, letters string) {
			if _, ok := temporary[letters]; ok || seen[letters] {
				return
			}
			seen[letters] = true
			out = append(out, Problem{Path: ch.path, Message: "refers to " + temporaryPrefix + letters +
				", which no new decision of this changeset has; a merge keeps it as it is"})
		})
	}
	slices.SortStableFunc(out, func(a, b Problem) int { return strings.Compare(a.Path, b.Path) })
	if len(out) > MaxWarnings {
		more := len(out) - MaxWarnings + 1
		out = append(out[:MaxWarnings-1], Problem{Path: ".", Message: fmt.Sprintf("%d more warnings not shown", more)})
	}
	return out
}
