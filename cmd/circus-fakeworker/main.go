//go:build dev

// circus-fakeworker is a development command for the control plane and Web UI.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/EricWvi/circus/internal/credfile"
	"github.com/EricWvi/circus/internal/testutil/fakeworker"
)

func main() {
	if err := run(); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	var o fakeworker.Options
	credentialPath := flag.String("credential", "", "worker credential file (default: the circus join worker credential)")
	flag.StringVar(&o.URL, "url", "", "control plane URL override")
	flag.StringVar(&o.Name, "name", "fakeworker", "worker name in logs")
	maxSessions := flag.Uint("max-sessions", 4, "maximum concurrent sessions (1..64)")
	flag.DurationVar(&o.IdleRelease, "idle-release", 5*time.Minute, "idle time before releasing a session lease")
	flag.DurationVar(&o.StreamDelay, "stream-delay", 100*time.Millisecond, "delay between streamed chunks")
	flag.Parse()
	if flag.NArg() != 0 || *maxSessions < 1 || *maxSessions > 64 {
		flag.Usage()
		return errors.New("invalid fake worker arguments")
	}
	o.MaxSessions = uint32(*maxSessions)
	o.Logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	var err error
	if *credentialPath == "" {
		*credentialPath, err = credfile.Path(credfile.RoleWorker)
		if err != nil {
			return err
		}
	}
	o.Credential, err = credfile.Load(*credentialPath)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return fakeworker.Run(ctx, o)
}
