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
