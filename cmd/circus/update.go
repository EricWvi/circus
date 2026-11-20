package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/EricWvi/circus/internal/daemonservice"
	"github.com/EricWvi/circus/internal/selfupdate"
)

var runSelfUpdate = selfupdate.Run

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
	result, err := runSelfUpdate(ctx, version, *force, out)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, result.Message)
	if result.ReplacedPath != "" {
		updateDaemon(result.ReplacedPath, out, errOut)
	}
	return err
}

func updateDaemon(updatedPath string, out, errOut io.Writer) {
	program, installed, err := daemonservice.InstalledProgram()
	if err != nil && !errors.Is(err, daemonservice.ErrUnsupported) {
		fmt.Fprintf(errOut, "warning: could not inspect the circus device daemon service: %v\n", err)
	}
	if !installed {
		return
	}
	if !shouldRestartDaemon(updatedPath, program) {
		switch _, err := os.Stat(program); {
		case program == "":
			fmt.Fprintln(errOut, "The installed circus daemon service has an unknown executable. Reinstall it with `circus daemon install` using the updated binary.")
		case errors.Is(err, os.ErrNotExist):
			fmt.Fprintf(errOut, "The installed circus daemon service points to a removed binary (%q). Reinstall it with `circus daemon install` using the updated binary.\n", program)
		default:
			fmt.Fprintf(errOut, "The installed circus daemon service runs another binary (%q). Run `circus daemon restart` to restart it, or reinstall it with the updated binary.\n", program)
		}
		return
	}
	restarted, err := daemonservice.RestartIfRunning()
	if err != nil {
		fmt.Fprintf(errOut, "warning: could not restart the circus device daemon service: %v\n", err)
	} else if restarted {
		fmt.Fprintln(out, "Restarted the circus device daemon service.")
	} else {
		fmt.Fprintln(out, "the circus daemon service is installed but not running; run `circus daemon start`")
	}
}

func shouldRestartDaemon(updatedPath, program string) bool {
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
