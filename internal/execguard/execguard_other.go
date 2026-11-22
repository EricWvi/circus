//go:build !linux && !darwin

package execguard

import (
	"io"
	"runtime"
)

func run(_ int, _ []string, stderr io.Writer) int {
	return fail(stderr, ExitUsage, "not supported on %s; only Linux and macOS are", runtime.GOOS)
}
