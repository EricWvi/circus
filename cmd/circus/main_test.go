package main

import (
	"os"
	"testing"

	"github.com/EricWvi/circus/internal/testutil/leakcheck"
)

func TestMain(m *testing.M) {
	if os.Getenv("CIRCUS_TEST_RUN_DAEMON_CLI") == "1" {
		// The subprocess runs the CLI with the parent's isolated environment.
		os.Exit(m.Run())
	}
	os.Exit(leakcheck.Run(m, leakcheck.Config{}))
}
