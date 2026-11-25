package ledger

import (
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
func temporaryReferences(content []byte, visit func(start, end int, letters string)) {
	for i := 0; i+2 < len(content); i++ {
		if content[i] != 'D' || content[i+1] != '?' || i > 0 && wordByte(content[i-1]) {
			continue
		}
		end := i + 2
		for end < len(content) && content[end] >= 'a' && content[end] <= 'z' {
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
// about the notation is.
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
