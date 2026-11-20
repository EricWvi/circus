// Package daemonservice manages the circus device daemon as a per-user service.
package daemonservice

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/EricWvi/circus/internal/circushome"
	"github.com/EricWvi/circus/internal/credfile"
	"github.com/EricWvi/circus/internal/daemonlock"
)

// Status describes the service file and the state reported by its manager.
type Status struct {
	Installed bool
	Enabled   bool
	AtBoot    bool
	Running   bool
	PID       int
	State     string
	LastExit  string
	// StoppedAfterExit is true when the manager reports an inactive daemon
	// that exited cleanly or stopped itself after a terminal error.
	StoppedAfterExit bool
	File             string
	Logs             string
	Advice           []string
}

// ErrUnsupported indicates that circus does not manage services on this OS.
var ErrUnsupported = errors.New("unsupported service manager")

// ErrNotInstalled is returned by service controls when the file is absent.
var ErrNotInstalled = errors.New("not installed: run `circus daemon install`")

type manager interface {
	file() string
	logs() string
	install(exe string, args, env []string, previous []byte) error
	uninstall() error
	start() error
	stop() error
	restart() error
	restartIfRunning() (bool, error)
	status(context.Context) (Status, error)
	definition(data []byte) definition
}

// Tests select both service managers without using the host's manager.
var goos = runtime.GOOS

func current() (manager, error) {
	switch goos {
	case "linux":
		config := os.Getenv("XDG_CONFIG_HOME")
		if !filepath.IsAbs(config) {
			home, err := os.UserHomeDir()
			if err != nil {
				return nil, err
			}
			config = filepath.Join(home, ".config")
		}
		return systemd{config: config}, nil
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		return launchd{home: home}, nil
	default:
		return nil, fmt.Errorf("%w on %s: run `circus daemon` under the system's own service manager", ErrUnsupported, goos)
	}
}

// Install validates the daemon credentials, replaces the service file and starts
// the service with the current shell's configuration. An empty credentials
// path uses the same default as the foreground daemon.
func Install(credentials string, out io.Writer) error {
	m, err := current()
	if err != nil {
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
	env, skipped, err := serviceEnvironment()
	if err != nil {
		return err
	}
	for _, name := range skipped {
		fmt.Fprintf(out, "Warning: skipping environment variable %q with control characters or invalid UTF-8\n", name)
	}
	previous, err := readServiceFile(m)
	if err != nil {
		return err
	}
	if previous != nil {
		reportReplacement(out, m.definition(previous), makeDefinition(exe, args, env))
	}
	return m.install(exe, args, env, previous)
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

// Uninstall stops the service and removes its file if present.
func Uninstall() error {
	err := withInstalled(manager.uninstall)
	if errors.Is(err, ErrNotInstalled) {
		return nil
	}
	return err
}

// Start starts the installed service.
func Start() error { return withInstalled(manager.start) }

// Stop stops the installed service for the current session.
func Stop() error { return withInstalled(manager.stop) }

// Restart restarts the installed service.
func Restart() error { return withInstalled(manager.restart) }

// RestartWithCredentials restarts the installed service if it uses the given
// credential path and the current CIRCUS_HOME.
func RestartWithCredentials(path string) (bool, error) {
	m, err := current()
	if errors.Is(err, ErrUnsupported) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	data, err := readServiceFile(m)
	if data == nil || err != nil {
		return false, err
	}
	root, err := circushome.Root()
	if err != nil {
		return false, err
	}
	d := m.definition(data)
	if d.program() == "" || d.env[circushome.Env] != root || filepath.Clean(d.credentials()) != filepath.Clean(path) {
		return false, nil
	}
	err = m.restart()
	return err == nil, err
}

// RestartIfRunning restarts a running service or one that stopped itself.
// A manually stopped service stays stopped.
func RestartIfRunning() (bool, error) {
	var restarted bool
	err := withInstalled(func(m manager) error {
		var err error
		restarted, err = m.restartIfRunning()
		return err
	})
	if errors.Is(err, ErrNotInstalled) {
		return false, nil
	}
	return restarted, err
}

func withInstalled(action func(manager) error) error {
	m, err := current()
	if err != nil {
		return err
	}
	if _, err := os.Stat(m.file()); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrNotInstalled
		}
		return err
	}
	return action(m)
}

// Get reports the service's current state. It returns the file and log
// locations even when the file is absent or the manager query fails.
func Get() (Status, error) {
	return getStatus(context.Background())
}

// GetAfterInstall gives a systemd service time to exit after startup and a
// launchd service time to reach the running state.
func GetAfterInstall() (Status, error) {
	return pollInstallStatus(2 * time.Second)
}

// The timeout only decides whether to query again. Each query runs to
// completion: a manager or loginctl query killed at the deadline would report
// a partial status, dropping the running state or the linger advice.
func pollInstallStatus(timeout time.Duration) (Status, error) {
	deadline := time.Now().Add(timeout)
	for {
		st, err := getStatus(context.Background())
		if goos == "darwin" && err == nil && st.Running {
			return st, nil
		}
		wait := time.Until(deadline)
		if wait <= 0 {
			return st, err
		}
		time.Sleep(min(wait, 100*time.Millisecond))
	}
}

func getStatus(ctx context.Context) (Status, error) {
	m, err := current()
	if err != nil {
		return Status{}, err
	}
	st := Status{File: m.file(), Logs: m.logs()}
	if _, err := os.Stat(st.File); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return st, nil
		}
		return st, err
	}
	status, err := m.status(ctx)
	status.Installed, status.File, status.Logs = true, st.File, st.Logs
	return status, err
}

// Program returns the executable recorded in the installed service file, or
// an empty string if that information is unavailable.
func Program() string {
	program, _, _ := InstalledProgram()
	return program
}

// InstalledProgram reports the executable and whether the service file exists.
// It does not need to connect to the service manager.
func InstalledProgram() (string, bool, error) {
	m, err := current()
	if err != nil {
		return "", false, err
	}
	data, err := readServiceFile(m)
	if data == nil || err != nil {
		return "", false, err
	}
	return m.definition(data).program(), true, nil
}

func readServiceFile(m manager) ([]byte, error) {
	data, err := os.ReadFile(m.file())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return data, err
}

// Linger returns the Linux advice for running the service without a login.
func Linger() string {
	if goos == "linux" {
		_, advice := lingerStatus(context.Background())
		return advice
	}
	return ""
}

// serviceExecutable is replaceable in tests, which run from a temporary build.
var serviceExecutable = func() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate circus executable: %w", err)
	}
	real := exe
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		real = resolved
	}
	return pick(real, exe)
}

var goBuildPath = regexp.MustCompile(`(?:^|/)go-build[0-9]+(?:/|$)`)

func pick(real, exe string) (string, error) {
	if goBuildPath.MatchString(real) {
		return "", errors.New("this executable is a temporary `go run` build. To install the service, install circus first and run `circus daemon install`")
	}
	info, err := os.Stat(real)
	if err != nil {
		return "", fmt.Errorf("locate circus executable: %w", err)
	}
	// Keep a stable launcher (such as a symlink on PATH) if it points to us.
	if path, err := exec.LookPath("circus"); err == nil {
		if path, err = filepath.Abs(path); err == nil && matchesFile(path, info) {
			return path, nil
		}
	}
	if filepath.IsAbs(exe) && matchesFile(exe, info) {
		return exe, nil
	}
	return real, nil
}

func matchesFile(path string, info os.FileInfo) bool {
	other, err := os.Stat(path)
	return err == nil && os.SameFile(info, other)
}

func run(name string, args ...string) (string, error) {
	return runContext(context.Background(), name, args...)
}

func runContext(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if _, bounded := ctx.Deadline(); bounded {
		cmd.WaitDelay = 100 * time.Millisecond
	}
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	data, err := cmd.CombinedOutput()
	out := strings.TrimSpace(string(data))
	if err != nil {
		if out != "" {
			return out, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, out)
		}
		return out, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return out, nil
}

func restoreServiceFile(path string, previous []byte) error {
	if previous != nil {
		return writePrivate(path, string(previous))
	}
	if err := os.Remove(path); !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func installError(installErr, recovery error) error {
	if recovery == nil {
		return installErr
	}
	return errors.Join(installErr, fmt.Errorf("restore circus daemon service: %w", recovery))
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// writePrivate replaces the configuration atomically. Proxy settings in it
// can contain credentials, so the temporary file is also private.
func writePrivate(path, content string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".circus-daemon-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err := f.Chmod(0o600); err != nil {
		return err
	}
	if _, err := f.WriteString(content); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
