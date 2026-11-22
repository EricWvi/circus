package main

import (
	"fmt"
	"io"

	"github.com/EricWvi/circus/internal/execguard"
)

// internalCommand runs `circus internal <helper>`, the hidden helpers other
// circus components start, and returns its exit code. The worker starts the
// Claude Code CLI through `exec-guard`, which supervises the CLI and its tools.
func internalCommand(args []string, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "exec-guard" {
		fmt.Fprintln(stderr, execguard.Usage)
		return execguard.ExitUsage
	}
	return execguard.Run(args[1:], stderr)
}

// isExecGuard reports whether the process command line runs the exec-guard.
func isExecGuard(args []string) bool {
	return len(args) > 2 && args[1] == "internal" && args[2] == "exec-guard"
}
