// Command circus is the circus CLI: `circus server` runs the control plane
// and its admin subcommands, `circus join` enrolls a machine, `circus daemon`
// runs the device daemon (and its local `daemon call` harness), `circus call`
// is the worker-credential debug client, `circus doctor` checks the stored
// credentials against the control plane, and `circus version` prints the
// build version.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
)

var version = "dev"

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: circus <version|daemon|server|join|doctor|call>")
		os.Exit(1)
	}

	switch os.Args[1] {
	case "version":
		fmt.Println(version)
	case "daemon":
		if err := daemonCommand(os.Args[2:], os.Stdin, os.Stdout, os.Stderr); err != nil {
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
		fmt.Fprintln(os.Stderr, "usage: circus <version|daemon|server|join|doctor|call>")
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
