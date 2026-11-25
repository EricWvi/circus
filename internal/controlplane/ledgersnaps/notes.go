package ledgersnaps

import (
	"cmp"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/EricWvi/circus/internal/ledger"
	"github.com/EricWvi/circus/internal/store"
)

// Categories of the notes the agent gets (Inputs.Input.note, published as
// the view event's circus_note).
const (
	noteProblems  = "ledger_problems"
	noteMerged    = "ledger_merged"
	noteDiscarded = "ledger_discarded"
)

// maxNoteListed bounds the problems or files one note lists.
const maxNoteListed = 100

const formatRules = "Decision files are named <dir>/D<number>-<name>.md and start with front matter of exactly two keys, " +
	"`number` and `title`, followed by a blank line; a new decision takes a temporary number D?<letters> " +
	"(one to four lowercase letters) in its front matter and file name. Existing decisions cannot be removed, only moved " +
	"or renamed, and the generated INDEX.md cannot be changed. The ledger holds no other files."

// problemsText lists the problems of a collection for the agent.
func problemsText(snapshotPath string, problems []store.LedgerProblem) string {
	var b strings.Builder
	fmt.Fprintf(&b, "The ledger snapshot at %s has problems that keep its changes from being merged. Fix them in the snapshot's files:\n", snapshotPath)
	for i, p := range problems {
		if i == maxNoteListed {
			fmt.Fprintf(&b, "- and %d more\n", len(problems)-i)
			break
		}
		if p.Path == "" || p.Path == "." {
			fmt.Fprintf(&b, "- %s\n", p.Message)
		} else {
			fmt.Fprintf(&b, "- %s: %s\n", p.Path, p.Message)
		}
	}
	b.WriteString(formatRules)
	if slices.ContainsFunc(problems, func(p store.LedgerProblem) bool { return conflictFile.MatchString(p.Path) }) {
		b.WriteString(" A file named <name>.conflict-<hash> (or recovered.conflict-<hash>) is a version of a file that circus " +
			"set aside while it updated the snapshot and could not put back: keep what you need of it in the decision files and remove it.")
	}
	return b.String()
}

// conflictFile matches the files the device restores beside a path it
// could not put a file back to.
var conflictFile = regexp.MustCompile(`\.conflict-[0-9a-f]{12}$`)

// mergeNote is what the merged note says besides the changeset.
type mergeNote struct {
	numbers map[string]uint32
	// renamed maps each renamed file to its new path; restored lists the
	// merged new decisions put back; problems are what the agent should
	// check.
	renamed  map[string]string
	restored []string
	problems []ledger.Problem
	// found: the rebase was found finished (its answer was lost), and
	// which files it renamed is not known; kept: the snapshot keeps
	// changes the agent made after the collection the merge used.
	found, kept bool
}

// mergedText tells the agent its changes were merged and its new
// decisions renamed.
func mergedText(snapshotPath, changeset string, n mergeNote) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Your ledger changes were merged into the project's ledger (changeset %s).", changeset)
	if len(n.numbers) > 0 {
		letters := slices.SortedFunc(maps.Keys(n.numbers), func(a, c string) int { return cmp.Or(cmp.Compare(len(a), len(c)), strings.Compare(a, c)) })
		parts := make([]string, 0, len(letters))
		for i, l := range letters {
			if i == maxNoteListed {
				parts = append(parts, fmt.Sprintf("and %d more", len(letters)-i))
				break
			}
			parts = append(parts, fmt.Sprintf("D?%s is now D%d", l, n.numbers[l]))
		}
		b.WriteString(" " + strings.Join(parts, "; ") + ".")
	}
	fmt.Fprintf(&b, " The snapshot at %s now holds the merged files", snapshotPath)
	switch {
	case len(n.renamed) > 0:
		b.WriteString("; these files were renamed: " + listed(slices.Sorted(maps.Keys(n.renamed)), func(from string) string { return from + " -> " + n.renamed[from] }) +
			"; references to the temporary numbers were rewritten")
	case n.found && len(n.numbers) > 0:
		b.WriteString("; each new decision's file was renamed to its number and references to the temporary numbers were rewritten")
	}
	b.WriteString(". Use the assigned numbers from now on.")
	if n.kept {
		b.WriteString(" The changes you made after your changes were collected for the merge are kept, " +
			"with the assigned numbers in place of the temporary ones; they go into your next ledger changes.")
	}
	if len(n.restored) > 0 {
		b.WriteString(" You removed these merged decisions after your changes were collected for the merge; the ledger keeps every decision, " +
			"so they were put back: " + listed(n.restored, func(p string) string { return p }) + ".")
	}
	if len(n.problems) > 0 {
		b.WriteString(" Check these files:")
		for i, p := range n.problems {
			if i == maxNoteListed {
				fmt.Fprintf(&b, "\n- and %d more", len(n.problems)-i)
				break
			}
			fmt.Fprintf(&b, "\n- %s: %s", p.Path, p.Message)
		}
	}
	return b.String()
}

// listed joins at most maxNoteListed items, each as format makes it.
func listed(items []string, format func(string) string) string {
	parts := make([]string, 0, min(len(items), maxNoteListed+1))
	for i, it := range items {
		if i == maxNoteListed {
			parts = append(parts, fmt.Sprintf("and %d more", len(items)-i))
			break
		}
		parts = append(parts, format(it))
	}
	return strings.Join(parts, ", ")
}

// discardedText tells the agent its changes were discarded.
func discardedText(snapshotPath, changeset, reason string) string {
	return fmt.Sprintf("Your ledger changes (changeset %s) were discarded: %s. The snapshot at %s was written again from its base; "+
		"the discarded changes are gone from it.", changeset, reason, snapshotPath)
}
