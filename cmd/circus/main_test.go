package main

import (
	"os"
	"regexp"
	"testing"

	"github.com/EricWvi/circus/internal/testutil/leakcheck"
)

func TestMain(m *testing.M) {
	os.Exit(leakcheck.Run(m, leakcheck.Config{AllowFDs: []leakcheck.Allowance{{
		// daemonCommand warms DefaultTools' Bash, which shares the process-wide
		// environment; by design its instance lock lasts until process exit.
		Match:  regexp.MustCompile(`/daemon-test-[^/]+/state/circus/daemon/instance-[^/]+/\.lock$`),
		Reason: "the process-wide bash environment keeps its instance lock until process exit",
	}}}))
}
