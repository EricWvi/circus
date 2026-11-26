package userservice

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"

	"github.com/EricWvi/circus/internal/credfile"
	"github.com/EricWvi/circus/internal/daemonlock"
)

// InstallDaemon validates the daemon credentials, replaces the daemon's
// service file and starts the service with the current shell's
// configuration. An empty credentials path uses the same default as the
// foreground daemon.
func InstallDaemon(credentials string, out io.Writer) error {
	if _, err := Daemon.current(); err != nil {
		return err
	}
	holder, err := daemonlock.Holder()
	if err != nil {
		return fmt.Errorf("check daemon lock: %w", err)
	}
	if holder != nil {
		switch holder.Mode {
		case daemonlock.Foreground:
			return fmt.Errorf("stop the foreground `circus daemon` (pid %d) first", holder.PID)
		case daemonlock.Service:
		default:
			return holder
		}
	}
	exe, err := serviceExecutable()
	if err != nil {
		return err
	}
	args, err := daemonArgs(credentials)
	if err != nil {
		return err
	}
	env, skipped, err := Environment()
	if err != nil {
		return err
	}
	WarnSkipped(out, skipped)
	return Daemon.Install(exe, args, env, out)
}

// WarnSkipped tells which variables Environment left out.
func WarnSkipped(out io.Writer, skipped []string) {
	for _, name := range skipped {
		fmt.Fprintf(out, "Warning: skipping environment variable %q with control characters or invalid UTF-8\n", name)
	}
}

func daemonArgs(credentials string) ([]string, error) {
	var path string
	var err error
	if credentials == "" {
		path, err = credfile.Path(credfile.RoleDaemon)
	} else {
		path, err = filepath.Abs(credentials)
	}
	if err != nil {
		return nil, fmt.Errorf("resolve daemon credentials: %w", err)
	}
	file, err := credfile.Load(path)
	if errors.Is(err, credfile.ErrNotFound) {
		return nil, fmt.Errorf("load daemon credentials at %q: %w", path, err)
	}
	if err != nil {
		return nil, fmt.Errorf("load daemon credentials at %q: %w. Run `circus join` to enroll this device", path, err)
	}
	if file.Role != credfile.RoleDaemon {
		return nil, fmt.Errorf("%q contains %s credentials. Run `circus join` to enroll this device as a daemon", path, file.Role)
	}
	args := []string{"daemon", "--service"}
	if credentials != "" {
		args = append(args, "--credentials", path)
	}
	return args, nil
}
