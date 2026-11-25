//go:build !linux && !darwin

package snapshot

// renameExclusive has no exclusive rename to call on this platform.
func renameExclusive(int, string, int, string) error { return errUnsupported }
