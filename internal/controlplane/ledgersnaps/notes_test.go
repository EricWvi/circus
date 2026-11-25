package ledgersnaps

import (
	"strings"
	"testing"

	"github.com/EricWvi/circus/internal/store"
)

// TestProblemsText lists the problems with the snapshot's path, and
// explains a conflict file the device restored.
func TestProblemsText(t *testing.T) {
	text := problemsText("/snap", []store.LedgerProblem{{Path: "notes.txt", Message: "unexpected file"}, {Message: "too many files"}})
	if !strings.Contains(text, "/snap") || !strings.Contains(text, "- notes.txt: unexpected file\n") ||
		!strings.Contains(text, "- too many files\n") || strings.Contains(text, "conflict") {
		t.Fatalf("text:\n%s", text)
	}
	text = problemsText("/snap", []store.LedgerProblem{{Path: "core/D1-x.md.conflict-0123456789ab", Message: "unexpected file"}})
	if !strings.Contains(text, "set aside") {
		t.Fatalf("conflict text:\n%s", text)
	}
}

// TestNotesStartWithTheMarker: every ledger note's text starts with the
// marker the system prompt names.
func TestNotesStartWithTheMarker(t *testing.T) {
	for _, text := range []string{
		problemsText("/snap", []store.LedgerProblem{{Path: "notes.txt", Message: "unexpected file"}}),
		mergedText("/snap", "cs-1", mergeNote{numbers: map[string]uint32{"a": 3}}),
		discardedText("/snap", "cs-1", "not now"),
	} {
		if !strings.HasPrefix(text, "[circus ledger] ") {
			t.Fatalf("text without the marker:\n%s", text)
		}
	}
}
