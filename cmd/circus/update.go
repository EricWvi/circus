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

	"github.com/EricWvi/circus/internal/selfupdate"
)

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
	message, err := selfupdate.Run(ctx, version, *force, out)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, message)
	return err
}
