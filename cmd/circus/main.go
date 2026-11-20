// Command circus is the circus CLI: `circus server` runs the control plane
// and its admin subcommands, `circus join` enrolls a machine, `circus daemon`
// runs and manages the device daemon, `circus daemon call` is its local harness,
// `circus call` is the worker-credential debug client, `circus doctor` checks the stored
// credentials against the control plane, `circus update` installs the latest
// CLI release, and `circus version` prints the build version.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"runtime"

	"github.com/EricWvi/circus/internal/daemonservice"
)

var version = "dev"

const usage = "usage: circus <version|update|daemon|server|join|doctor|call>"

func main() {
	if runtime.GOOS == "darwin" && len(os.Args) > 1 && os.Args[1] == daemonservice.LaunchReloadFlag {
		os.Exit(daemonservice.RunLaunchReload(os.Args[2:], os.Stderr))
	}
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(1)
	}

	switch os.Args[1] {
	case "version":
		fmt.Println(version)
	case "update":
		exitOn(updateCommand(os.Args[2:], os.Stdout, os.Stderr))
	case "daemon":
		if err := daemonCommand(os.Args[2:], os.Stdin, os.Stdout, os.Stderr); err != nil {
			if errors.Is(err, errDaemonServiceStopped) {
				os.Exit(daemonservice.LinuxTerminalExitStatus)
			}
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "call":
		exitOn(callCommand(os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
	case "server":
		exitOn(serverCommand(os.Args[2:], os.Stdout, os.Stderr))
	case "join":
		exitOn(joinCommand(os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
	case "doctor":
		exitOn(doctorCommand(os.Args[2:], os.Stdout, os.Stderr))
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(1)
	}
}

// exitOn prints err and exits 1 when it is not nil. Flag parse errors were
// already printed by the flag set.
func exitOn(err error) {
	if err == nil {
		return
	}
	var outcome callExit
	if errors.As(err, &outcome) {
		if outcome.err != nil {
			fmt.Fprintln(os.Stderr, outcome.err)
		}
		os.Exit(outcome.code)
	}
	if !errors.Is(err, flag.ErrHelp) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
