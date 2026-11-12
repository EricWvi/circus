// Package identifier checks the wire grammar for call, session and agent IDs.
package identifier

// Valid accepts 1-128 ASCII bytes in [A-Za-z0-9_-]. Optional IDs may be empty.
func Valid(id string, optional bool) bool {
	if id == "" {
		return optional
	}
	if len(id) > 128 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}
