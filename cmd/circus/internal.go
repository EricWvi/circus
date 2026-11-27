package main

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"path/filepath"

	"github.com/EricWvi/circus/internal/credfile"
	"github.com/EricWvi/circus/internal/execguard"
	"github.com/EricWvi/circus/internal/userservice"
	"github.com/EricWvi/circus/internal/workerservice"
)

// internalCommand runs `circus internal <helper>`, the hidden helpers other
// circus components start, and returns its exit code. The worker starts the
// Claude Code CLI through `exec-guard`, which supervises the CLI and its tools.
func internalCommand(args []string, stderr io.Writer) int {
	if len(args) > 0 && args[0] == workerRejectedCommand {
		return workerRejected(args[1:], stderr)
	}
	if len(args) == 0 || args[0] != "exec-guard" {
		fmt.Fprintln(stderr, execguard.Usage)
		return execguard.ExitUsage
	}
	return execguard.Run(args[1:], stderr)
}

// workerRejectedCommand is `circus internal worker-rejected --credentials
// <path> --principal <id> --token-sha256 <hex> (--pid <pid> |
// --credential-only)`: the worker starts it when the control plane proved
// it revoked the worker's own identity (it checks the revoked id first).
// Running as the installed service (--pid, the worker process), it removes
// the worker service and the credential file (workerservice.RemoveRejected);
// run in the foreground (--credential-only), it removes only the credential
// file and touches no service (userservice.RemoveRejectedCredential). Either
// way only while the file still holds that credential, named by its
// principal and its token's SHA-256 (never the token, which a command line
// would show); run in the foreground, also only while no installed worker
// service reads the file, else it leaves the file to the service. It
// reports on stderr, which goes to the worker's log, and exits 0 when it
// removed them, 1 when it kept them, 2 on failure or bad usage, and, with
// --credential-only, 3 when the file was already gone.
const workerRejectedCommand = "worker-rejected"

func workerRejected(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("internal "+workerRejectedCommand, flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("credentials", "", "the revoked worker credential file")
	principal := fs.String("principal", "", "the revoked identity")
	tokenHash := fs.String("token-sha256", "", "the SHA-256 of the revoked token, in hex")
	pid := fs.Int("pid", 0, "the worker process, the installed service's own")
	credentialOnly := fs.Bool("credential-only", false, "remove only the credential file: the worker runs in the foreground")
	usage := func() int {
		fmt.Fprintln(stderr, "usage: circus internal "+workerRejectedCommand+" --credentials <absolute path> --principal <id> --token-sha256 <hex> (--pid <pid> | --credential-only)")
		return 2
	}
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || !filepath.IsAbs(*path) || *principal == "" {
		return usage()
	}
	pidGiven := false
	fs.Visit(func(f *flag.Flag) { pidGiven = pidGiven || f.Name == "pid" })
	if *credentialOnly && pidGiven || !*credentialOnly && *pid <= 0 {
		return usage()
	}
	rejected := userservice.Rejected{PrincipalID: *principal}
	b, err := hex.DecodeString(*tokenHash)
	if err != nil || len(b) != len(rejected.TokenSHA256) {
		return usage()
	}
	copy(rejected.TokenSHA256[:], b)
	if *credentialOnly {
		return workerRejectedCredential(*path, rejected, stderr)
	}
	removed, err := workerservice.RemoveRejected(context.Background(), *path, rejected, *pid, stderr)
	switch {
	case err != nil:
		fmt.Fprintf(stderr, "circus: could not fully remove the worker service and its credential %s after the control plane revoked it: %v\n", *path, err)
		return 2
	case removed:
		fmt.Fprintf(stderr, "circus: the control plane revoked the worker credential %s, so the worker service and the credential file were removed; join again with a new link\n", *path)
		return 0
	default:
		fmt.Fprintf(stderr, "circus: the installed worker service does not read %s, or another join replaced it; the service and the file are kept\n", *path)
		return 1
	}
}

// workerRejectedCredential removes the revoked credential file at path of
// a worker run in the foreground, and only that file, unless the installed
// worker service reads it.
func workerRejectedCredential(path string, rejected userservice.Rejected, stderr io.Writer) int {
	outcome, err := userservice.Worker.RemoveRejectedCredential(path, rejected)
	reportRevokedCredential(credfile.RoleWorker, path, outcome, err, stderr)
	switch {
	case err != nil:
		return 2
	case outcome == userservice.CredentialRemoved:
		return 0
	case outcome == userservice.CredentialGone:
		return workerRejectedGone
	default:
		return 1
	}
}

// workerRejectedGone is worker-rejected --credential-only's exit status
// when the credential file is already gone.
const workerRejectedGone = 3

// isExecGuard reports whether the process command line runs the exec-guard.
func isExecGuard(args []string) bool {
	return len(args) > 2 && args[1] == "internal" && args[2] == "exec-guard"
}
