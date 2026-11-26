package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/EricWvi/circus/internal/selfupdate"
	"github.com/EricWvi/circus/internal/userservice"
	"github.com/EricWvi/circus/internal/workerservice"
)

const workerUsage = `usage: circus worker <install|uninstall|start|stop|restart|status>

Run the circus worker as a service of this user (a systemd user unit on
Linux, a launchd agent on macOS). install downloads the worker release
package of this circus's version, checks it against the release manifest,
and installs and starts the service with this shell's settings; run it
again to change them.`

// workerRelease is where `circus worker install` and `circus update` get
// worker packages; tests serve a fake release.
var workerRelease = selfupdate.DefaultRelease

// workerExecutable is the circus the worker service runs exec-guard from.
var workerExecutable = userservice.Executable

func workerCommand(args []string, out, errOut io.Writer) error {
	flags := flag.NewFlagSet("worker", flag.ContinueOnError)
	flags.SetOutput(errOut)
	flags.Usage = func() { fmt.Fprintln(errOut, workerUsage) }
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New(workerUsage)
	}
	switch flags.Arg(0) {
	case "install":
		return workerInstall(out, errOut)
	case "uninstall":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return workerAction(out, func() error { return workerservice.Uninstall(ctx, out) }, "Removed")
	case "start":
		return workerAction(out, userservice.Worker.Start, "Started")
	case "stop":
		return workerAction(out, userservice.Worker.Stop, "Stopped")
	case "restart":
		return workerAction(out, userservice.Worker.Restart, "Restarted")
	case "status":
		return workerStatus(out)
	default:
		return errors.New(workerUsage)
	}
}

func workerInstall(out, errOut io.Writer) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	circus, err := workerExecutable()
	if err != nil && selfupdate.IsRelease(version) {
		return err
	}
	node, err := workerservice.Install(ctx, version, workerservice.Options{
		Release: workerRelease(version), Circus: circus, Out: out,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Node.js %s: %s\n", node.Version, node.Path)
	st, err := userservice.Worker.GetAfterInstall()
	if st.Running && err == nil {
		fmt.Fprintln(out, "✓ Installed and started the circus worker service.")
	} else {
		fmt.Fprintln(out, "✓ Installed the circus worker service.")
		if err != nil {
			fmt.Fprintf(errOut, "WARNING: Could not confirm that the circus worker service is running: %v. See the logs: %s\n", err, st.Logs)
		} else {
			fmt.Fprintf(errOut, "WARNING: The circus worker service is not running. See the logs: %s\n", st.Logs)
		}
	}
	for _, warning := range st.Advice {
		fmt.Fprintln(errOut, "WARNING:", warning)
	}
	i, _, _ := workerservice.InstalledService()
	printWorkerLocation(out, st, i)
	if proxies := workerservice.RecordedProxies(i.Definition); len(proxies) > 0 {
		fmt.Fprintf(out, "The service file records %s as set in this shell, credentials included; it is readable by you only.\n", strings.Join(proxies, ", "))
	}
	return nil
}

func workerAction(out io.Writer, action func() error, verb string) error {
	if err := action(); err != nil {
		return err
	}
	_, err := fmt.Fprintln(out, "✓", verb, "the circus worker service.")
	return err
}

func workerStatus(out io.Writer) error {
	st, err := userservice.Worker.Get()
	if err != nil {
		return err
	}
	if !st.Installed {
		fmt.Fprintln(out, "The circus worker service is not installed. Run `circus worker install`.")
		return nil
	}
	i, _, err := workerservice.InstalledService()
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "The circus worker service is installed and %s.\n", runState(st))
	printServiceState(out, st)
	printWorkerLocation(out, st, i)
	// The worker (or its launcher) records why it stopped for good; the
	// exit status alone can't tell (on macOS it is 0, as for a plain kill).
	if reason, ok := workerservice.StopReason(i); ok && !st.Running {
		fmt.Fprintln(out, "It stopped itself and stays stopped.", reason)
	} else if st.Terminal {
		fmt.Fprintf(out, "It stopped itself and stays stopped. See the logs for why: %s\n", st.Logs)
	}
	if workerservice.NodeGone(i) {
		fmt.Fprintf(out, "The Node.js it was installed with (%s) is gone. Run `circus worker install` to record the one on your PATH.\n", i.Node)
	}
	printServiceAdvice(out, st)
	return nil
}

func printWorkerLocation(out io.Writer, st userservice.Status, i workerservice.Installed) {
	if i.Version != "" {
		fmt.Fprintln(out, "  Version   ", i.Version)
		fmt.Fprintln(out, "  Package   ", i.Root+string(os.PathSeparator)+i.Version)
	} else if program := i.Definition.Program(); program != "" {
		fmt.Fprintln(out, "  Program   ", program)
	}
	if i.Node != "" {
		fmt.Fprintln(out, "  Node.js   ", i.Node)
	}
	printDaemonLocation(out, st)
}
