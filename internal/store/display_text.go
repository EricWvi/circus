package store

// IsForbiddenDisplayRune reports invisible characters and characters that
// change how text is displayed. Web validation rejects these in trimmed
// project names, paths and repository URLs. Title derivation removes them
// from session titles:
//   - C0 controls, DEL and C1 controls: U+0000–U+001F, U+007F–U+009F;
//   - zero-width characters: U+200B–U+200D, U+FEFF;
//   - bidirectional marks: U+200E, U+200F (LRM, RLM), U+061C (ALM);
//   - bidirectional embeddings, overrides and isolates: U+202A–U+202E,
//     U+2066–U+2069;
//   - line and paragraph separators: U+2028, U+2029.
func IsForbiddenDisplayRune(r rune) bool {
	switch {
	case r < 0x20, r >= 0x7f && r <= 0x9f:
		return true
	case r >= 0x200b && r <= 0x200d, r == 0xfeff:
		return true
	case r == 0x200e, r == 0x200f, r == 0x061c:
		return true
	case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
		return true
	case r == 0x2028, r == 0x2029:
		return true
	}
	return false
}
