package store

import (
	"strings"
	"unicode"
)

// MaxSessionTitleBytes bounds stored session titles. Character validation
// and trimming of explicit titles belong to the service.
const MaxSessionTitleBytes = 1024

// DeriveSessionTitle takes the first nonempty line of a user message,
// collapses whitespace, drops forbidden display characters and limits the
// content to 60 runes, excluding the ellipsis appended on truncation. It
// prefers a word boundary in the second half, including immediately after
// the 60th content rune.
func DeriveSessionTitle(text string) string {
	for line := range strings.SplitSeq(text, "\n") {
		// Normalize whitespace before dropping display controls, so tabs keep
		// separating words. Removing invisible text may expose another run.
		title := strings.Join(strings.Fields(line), " ")
		title = strings.Map(func(r rune) rune {
			if IsForbiddenDisplayRune(r) {
				return -1
			}
			return r
		}, title)
		title = strings.Join(strings.Fields(title), " ")
		if title == "" {
			continue
		}
		runes := []rune(title)
		if len(runes) <= 60 {
			return title
		}
		cut := 60
		for i := 60; i >= 30; i-- {
			if unicode.IsSpace(runes[i]) {
				cut = i
				break
			}
		}
		return string(runes[:cut]) + "…"
	}
	return ""
}
