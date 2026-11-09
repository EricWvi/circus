package main

import (
	"fmt"
	"os"
)

var version = "dev"

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: circus <version|daemon>")
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
	default:
		fmt.Fprintln(os.Stderr, "usage: circus <version|daemon>")
		os.Exit(1)
	}
}
