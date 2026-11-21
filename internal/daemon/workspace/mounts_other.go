//go:build !linux

package workspace

// mountBelow is not implemented outside Linux; the walk refuses to cross
// onto another device by itself.
func mountBelow(string) string { return "" }
