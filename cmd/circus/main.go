package main

import (
	"fmt"
	"os"
)

var version = "dev"

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: circus <version|daemon>")
		os.Exit(1)
	}
	switch os.Args[1] {
	case "version":
		fmt.Println(version)
	case "daemon":
		fmt.Fprintln(os.Stderr, "not implemented yet")
		os.Exit(1)
	default:
		fmt.Fprintln(os.Stderr, "usage: circus <version|daemon>")
		os.Exit(1)
	}
}
