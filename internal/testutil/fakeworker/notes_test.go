package fakeworker

import (
	"os"
	"regexp"
	"strconv"
	"testing"
)

// The fake worker writes the real worker's note texts
// (worker/src/runner/notes.ts), which the Web shows verbatim.
func TestNoteTextsMatchTheWorker(t *testing.T) {
	raw, err := os.ReadFile("../../../worker/src/runner/notes.ts")
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"previousTurnInterruptedText": previousTurnInterruptedText,
		"historyMissingText":          historyMissingText,
	} {
		if got := tsStringConst(t, string(raw), name); got != want {
			t.Errorf("%s:\n  notes.ts: %q\n  fake:     %q", name, got, want)
		}
	}
}

// tsStringConst evaluates `export const name = "..." + "...";`, a
// concatenation of double-quoted string literals.
func tsStringConst(t *testing.T, source, name string) string {
	t.Helper()
	decl := regexp.MustCompile(`(?s)export const ` + name + ` =((?:\s*"(?:[^"\\]|\\.)*"\s*\+?)+);`).FindStringSubmatch(source)
	if decl == nil {
		t.Fatalf("no string constant %s in notes.ts", name)
	}
	text := ""
	for _, literal := range regexp.MustCompile(`"(?:[^"\\]|\\.)*"`).FindAllString(decl[1], -1) {
		s, err := strconv.Unquote(literal)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		text += s
	}
	return text
}
