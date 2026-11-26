package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/EricWvi/circus/internal/selfupdate"
	"github.com/EricWvi/circus/internal/userservice"
	"github.com/EricWvi/circus/internal/workerservice"
)

// checkSelfUpdate and runningExecutable are replaced in tests.
var (
	checkSelfUpdate   = selfupdate.Check
	runningExecutable = func() (string, error) {
		exe, err := os.Executable()
		if err != nil {
			return "", err
		}
		return filepath.EvalSymlinks(exe)
	}
)

// updateCommand installs the latest release. Everything it downloads is
// verified before anything changes: the CLI archive and, when the worker
// service runs the circus being replaced, the release's worker package (if
// that fails, nothing is updated). Then it replaces the binary, switches
// the worker service to the new package, and restarts the daemon service
// last, since `circus update` may run inside the daemon, which the restart
// stops. When the CLI is already current but the worker service runs
// another version, it finishes that switch.
func updateCommand(args []string, out, errOut io.Writer) error {
	const usage = "usage: circus update [--force]"
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.Usage = func() {
		fmt.Fprintln(errOut, usage)
		fs.PrintDefaults()
	}
	force := fs.Bool("force", false, "allow installing an older release")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New(usage)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	plan, err := checkSelfUpdate(ctx, version, *force, out)
	if err != nil {
		return err
	}
	if plan.UpToDate {
		fmt.Fprintln(out, plan.Message)
		exe, err := runningExecutable()
		if err != nil {
			return fmt.Errorf("locate this circus: %w", err)
		}
		w, err := prepareWorker(ctx, exe, plan.Version, out, errOut)
		if err != nil || w == nil {
			return err
		}
		if err := switchWorker(w, plan.Version); err != nil {
			return err
		}
		return restartServices(true, false, w.From(), plan.Version, out, errOut)
	}
	w, err := prepareWorker(ctx, plan.Path, plan.Version, out, errOut)
	if err != nil {
		return fmt.Errorf("nothing was updated: %w", err)
	}
	if w != nil {
		defer w.Close()
	}
	result, err := plan.Apply(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintln(out, result.Message)
	// Every service file is in place before any restart is asked for.
	var workerErr error
	switched := false
	if w != nil {
		workerErr = switchWorker(w, result.Version)
		switched = workerErr == nil
	}
	from := ""
	if w != nil {
		from = w.From()
	}
	return errors.Join(workerErr, restartServices(switched, daemonRunsBinary(result.ReplacedPath, errOut), from, result.Version, out, errOut))
}

// restartServices asks the service manager to restart the switched worker
// service and the daemon service, worker first, without waiting: once
// asked, the manager restarts both even if this process is killed by
// either restart (it runs inside the worker or the daemon). Services that
// are not running stay stopped. It does not check that they came up: it
// says to look with `circus worker status` and `circus daemon status`,
// where a worker that stopped for good explains why. A failure for the
// worker is an error; one for the daemon alone a warning, as before; a
// single request that covered both and failed is reported once, naming
// both.
func restartServices(worker, daemon bool, from, version string, out, errOut io.Writer) error {
	var requests []userservice.RestartRequest
	if worker {
		requests = append(requests, userservice.RestartRequest{Service: userservice.Worker, Rewritten: true})
	}
	if daemon {
		requests = append(requests, userservice.RestartRequest{Service: userservice.Daemon})
	}
	if len(requests) == 0 {
		return nil
	}
	results := userservice.RestartIfRunning(requests...)
	if len(results) == 2 && results[0].Err != nil && results[0].Err == results[1].Err {
		return fmt.Errorf("switched the circus worker service from %s to %s, but could not restart the worker and daemon services: %w. Run `circus worker restart` and `circus daemon restart`",
			from, version, results[0].Err)
	}
	var workerErr error
	var check []string
	for i, r := range requests {
		res := results[i]
		switch {
		case r.Service == userservice.Worker && res.Err != nil:
			workerErr = fmt.Errorf("switched the circus worker service from %s to %s, but could not restart it: %w. Run `circus worker restart`", from, version, res.Err)
		case r.Service == userservice.Worker && res.Requested:
			fmt.Fprintf(out, "Switched the circus worker service from %s to %s and asked the service manager to restart it.\n", from, version)
			check = append(check, "`circus worker status`")
		case r.Service == userservice.Worker:
			fmt.Fprintf(out, "Switched the circus worker service from %s to %s; it is not running: run `circus worker start`.\n", from, version)
		case res.Err != nil:
			fmt.Fprintf(errOut, "warning: could not restart the circus device daemon service: %v\n", res.Err)
		case res.Requested:
			fmt.Fprintln(out, "Asked the service manager to restart the circus device daemon service.")
			check = append(check, "`circus daemon status`")
		default:
			fmt.Fprintln(out, "the circus daemon service is installed but not running; run `circus daemon start`")
		}
	}
	switch len(check) {
	case 1:
		fmt.Fprintf(out, "Check that it came up with %s.\n", check[0])
	case 2:
		fmt.Fprintf(out, "Check that they came up with %s.\n", strings.Join(check, " and "))
	}
	return workerErr
}

// prepareWorker gets the worker service ready to move to version when it
// runs the circus at binary and another version: the package is downloaded,
// verified and unpacked, and the store stays locked until the switch. It
// decides on the service file as read under the lock, and returns nil when
// there is nothing to switch.
func prepareWorker(ctx context.Context, binary, version string, out, errOut io.Writer) (*workerservice.Switch, error) {
	installed, ok, err := workerservice.InstalledService()
	if err != nil && !errors.Is(err, userservice.ErrUnsupported) {
		return nil, fmt.Errorf("inspect the circus worker service: %w", err)
	}
	// Without a service running this circus there is nothing to lock for.
	if !ok || installed.Version == version || !runsUpdatedBinary(binary, installed.Circus) {
		if ok && installed.Version != version {
			fmt.Fprintf(errOut, "The installed circus worker service uses another circus (%q), so it stays at %s. Reinstall it with `circus worker install` using the updated binary.\n",
				installed.Circus, installed.Version)
		}
		return nil, nil
	}
	w, err := workerservice.Prepare(ctx, version, workerservice.Options{
		Release: workerRelease(version), Circus: installed.Circus, Out: out,
	})
	if errors.Is(err, userservice.ErrNotInstalled) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get the circus worker %s for the worker service: %w", version, err)
	}
	if locked := w.Installed(); locked.Version == version || !runsUpdatedBinary(binary, locked.Circus) {
		w.Close()
		return nil, nil
	}
	return w, nil
}

// switchWorker points the worker service at the prepared package.
func switchWorker(w *workerservice.Switch, version string) error {
	from := w.From()
	if err := w.Apply(); err != nil {
		return fmt.Errorf("could not switch the circus worker service to %s: %w. It still runs %s; run `circus update` again to retry", version, err, from)
	}
	return nil
}

// daemonRunsBinary reports whether the installed daemon service runs the
// updated binary, and says why not when it is installed but doesn't.
func daemonRunsBinary(updatedPath string, errOut io.Writer) bool {
	d, installed, err := userservice.Daemon.Installed()
	program := d.Program()
	if err != nil && !errors.Is(err, userservice.ErrUnsupported) {
		fmt.Fprintf(errOut, "warning: could not inspect the circus device daemon service: %v\n", err)
	}
	if !installed {
		return false
	}
	if !runsUpdatedBinary(updatedPath, program) {
		switch _, err := os.Stat(program); {
		case program == "":
			fmt.Fprintln(errOut, "The installed circus daemon service has an unknown executable. Reinstall it with `circus daemon install` using the updated binary.")
		case errors.Is(err, os.ErrNotExist):
			fmt.Fprintf(errOut, "The installed circus daemon service points to a removed binary (%q). Reinstall it with `circus daemon install` using the updated binary.\n", program)
		default:
			fmt.Fprintf(errOut, "The installed circus daemon service runs another binary (%q). Run `circus daemon restart` to restart it, or reinstall it with the updated binary.\n", program)
		}
		return false
	}
	return true
}

func runsUpdatedBinary(updatedPath, program string) bool {
	if updatedPath == "" || program == "" {
		return false
	}
	updated, err := os.Stat(updatedPath)
	if err != nil {
		return false
	}
	installed, err := os.Stat(program)
	return err == nil && os.SameFile(updated, installed)
}
