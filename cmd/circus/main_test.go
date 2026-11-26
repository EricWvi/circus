package main

import (
	"os"
	"testing"

	"github.com/EricWvi/circus/internal/testutil/leakcheck"
	"github.com/EricWvi/circus/internal/userservice"
)

func TestMain(m *testing.M) {
	// On macOS, restarting services from `circus update` (updateCommand)
	// starts a detached launchd helper from this process's own executable,
	// as main does for the circus binary: here, the test binary.
	if len(os.Args) > 1 && os.Args[1] == userservice.LaunchReloadFlag {
		os.Exit(userservice.RunLaunchReload(os.Args[2:], os.Stderr))
	}
	if os.Getenv("CIRCUS_TEST_RUN_DAEMON_CLI") == "1" {
		// The subprocess runs the CLI with the parent's isolated environment.
		os.Exit(m.Run())
	}
	os.Exit(leakcheck.Run(m, leakcheck.Config{}))
}
