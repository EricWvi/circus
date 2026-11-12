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
		fmt.Fprintln(os.Stderr, "usage: circus <version|daemon|server|join|doctor>")
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
	case "server":
		exitOn(serverCommand(os.Args[2:], os.Stdout, os.Stderr))
	case "join":
		exitOn(joinCommand(os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
	case "doctor":
		exitOn(doctorCommand(os.Args[2:], os.Stdout, os.Stderr))
	default:
		fmt.Fprintln(os.Stderr, "usage: circus <version|daemon|server|join|doctor>")
		os.Exit(1)
	}
}

// exitOn prints err and exits 1 when it is not nil. Flag parse errors were
// already printed by the flag set.
func exitOn(err error) {
	if err == nil {
		return
	}
	if !errors.Is(err, flag.ErrHelp) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
