//go:build !linux

package workspace

import "syscall"

// setDeathSignal does nothing: macOS has no parent-death signal. After a
// daemon crash git finishes or fails on its own; the next attempt deletes
// its hidden directory under the lock.
func setDeathSignal(*syscall.SysProcAttr) {}
