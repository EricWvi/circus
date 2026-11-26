// Package userservice runs circus programs as per-user services: a systemd
// user unit on Linux, a launchd agent on macOS. It manages two services, the
// device daemon (Daemon) and the worker (Worker), with the same lifecycle,
// status and rollback rules; what to run is up to the caller.
package userservice

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
	"slices"
	"strings"
	"time"

	"github.com/EricWvi/circus/internal/circushome"
	"github.com/EricWvi/circus/internal/credfile"
)

// Service is one per-user service circus manages.
type Service struct {
	// name is the CLI command (`circus <name> install`) and the last part of
	// the unit name, launchd label and log file name.
	name string
	// noun names the service in messages: "the circus <noun> service".
	noun string
	// description is the systemd unit's Description.
	description string
	// role is the credential the service reads.
	role credfile.Role
	// foreground tells how to run the program without a service manager.
	foreground string
	// killMode is the systemd KillMode, empty for the default (control-group).
	killMode string
	// stopTimeout is how many seconds the manager waits after SIGTERM
	// before it kills the service, 0 for the manager's default.
	stopTimeout int
	// noRestartOnTerminal makes the systemd unit say outright that it never
	// restarts after the terminal status (RestartPreventExitStatus).
	noRestartOnTerminal bool
}

var (
	// Daemon is the device daemon, `circus daemon --service`.
	Daemon = &Service{
		name: "daemon", noun: "daemon", description: "circus device daemon",
		role: credfile.RoleDaemon, foreground: "run `circus daemon`",
	}
	// Worker is the worker, run by the launcher of an installed worker
	// release package. Stopping it signals only the worker, which ends its
	// Claude Code processes itself and releases its leases: 15 s for the
	// sessions, up to 5 s for the releases, then closing its link, so the
	// manager waits 30 s before it kills what is left.
	Worker = &Service{
		name: "worker", noun: "worker", description: "circus worker",
		role: credfile.RoleWorker, foreground: "run the worker", killMode: "mixed",
		stopTimeout: 30, noRestartOnTerminal: true,
	}
)

var services = []*Service{Daemon, Worker}

func serviceNamed(name string) *Service {
	for _, s := range services {
		if s.name == name {
			return s
		}
	}
	return nil
}

func (s *Service) unitName() string { return "circus-" + s.name + ".service" }

func (s *Service) label() string { return "com.github.ericwvi.circus." + s.name }

func (s *Service) title() string { return "circus " + s.noun + " service" }

func (s *Service) installCommand() string { return "circus " + s.name + " install" }

// Status describes the service file and the state reported by its manager.
type Status struct {
	Installed bool
	Enabled   bool
	AtBoot    bool
	Running   bool
	PID       int
	State     string
	LastExit  string
	// StoppedAfterExit is true when the manager reports an inactive service
	// that exited cleanly or stopped itself after a terminal error.
	StoppedAfterExit bool
	// Terminal is true when the service stopped itself with the terminal
	// status, LinuxTerminalExitStatus, and stays stopped until it is
	// started again. It is Linux only: on macOS the terminal status is 0,
	// which a plain kill gives too.
	Terminal bool
	File     string
	Logs     string
	Advice   []string
}

// ErrUnsupported indicates that circus does not manage services on this OS.
var ErrUnsupported = errors.New("unsupported service manager")

// ErrNotInstalled is returned by service controls when the file is absent.
var ErrNotInstalled = errors.New("not installed")

type manager interface {
	file() string
	logs() string
	install(exe string, args, env []string, previous []byte) error
	rewrite(exe string, args, env []string, previous []byte) error
	uninstall() error
	start() error
	stop() error
	restart() error
	restartIfRunning() (bool, error)
	status(context.Context) (Status, error)
	definition(data []byte) Definition
}

// Tests select both service managers without using the host's manager.
var goos = runtime.GOOS

func (s *Service) current() (manager, error) {
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
		return systemd{config: config, svc: s}, nil
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		return launchd{home: home, svc: s}, nil
	default:
		return nil, fmt.Errorf("%w on %s: %s under the system's own service manager", ErrUnsupported, goos, s.foreground)
	}
}

// Install replaces the service file with one that runs exe with args and
// env (KEY=VALUE entries), enables it and (re)starts it. A failure restores
// the previous file and the service's previous state.
func (s *Service) Install(exe string, args, env []string, out io.Writer) error {
	m, err := s.current()
	if err != nil {
		return err
	}
	previous, err := readServiceFile(m)
	if err != nil {
		return err
	}
	if previous != nil {
		s.reportReplacement(out, m.definition(previous), makeDefinition(exe, args, env))
	}
	return m.install(exe, args, env, previous)
}

// Rewrite replaces the installed service file to run exe with args and
// env, without restarting the service: systemd reloads its configuration,
// so the next start or restart uses the new file, and launchd keeps a
// loaded agent's old definition until RestartIfRunning loads the new one. A
// failure restores the previous file.
func (s *Service) Rewrite(exe string, args, env []string) error {
	m, err := s.current()
	if err != nil {
		return err
	}
	previous, err := readServiceFile(m)
	if err != nil {
		return err
	}
	if previous == nil {
		return s.notInstalled()
	}
	return m.rewrite(exe, args, env, previous)
}

// Uninstall stops the service and removes its file if present.
func (s *Service) Uninstall() error {
	err := s.withInstalled(manager.uninstall)
	if errors.Is(err, ErrNotInstalled) {
		return nil
	}
	return err
}

// Start starts the installed service.
func (s *Service) Start() error { return s.withInstalled(manager.start) }

// Stop stops the installed service for the current session.
func (s *Service) Stop() error { return s.withInstalled(manager.stop) }

// Restart restarts the installed service.
func (s *Service) Restart() error { return s.withInstalled(manager.restart) }

// RestartWithCredentials restarts the installed service if it reads the
// credential file at path and uses the current CIRCUS_HOME.
func (s *Service) RestartWithCredentials(path string) (bool, error) {
	m, err := s.current()
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
	if d.Program() == "" || d.Env[circushome.Env] != root || filepath.Clean(d.Credentials(s.role)) != filepath.Clean(path) {
		return false, nil
	}
	err = m.restart()
	return err == nil, err
}

// Restart is the outcome of RestartIfRunning for one service.
type Restart struct {
	// Requested is true when the manager was asked to restart the service.
	Requested bool
	// Err is why the request failed. When one request covered several
	// services, each of them holds the same error value, which names them
	// all: the request failing says nothing about which one it was for.
	Err error
}

// sharedError is the failure of one request that covered several
// services, named by their units or labels.
func sharedError(names []string, err error) error {
	return fmt.Errorf("the one request to restart %s failed (it covered all of them): %w", strings.Join(names, " and "), err)
}

// RestartRequest names a service for RestartIfRunning.
type RestartRequest struct {
	Service *Service
	// Rewritten is true when Rewrite changed its service file since the
	// service started: on macOS the agent must then be reloaded from it.
	Rewritten bool
}

// RestartIfRunning asks the service manager to restart each installed
// service of requests that runs, or start one that stopped itself with the
// terminal status, in the order given; a manually stopped service stays
// stopped. It reports the outcome for each, in the same order. It does not
// wait for the restarts, and it asks for all of them before any can begin
// to stop this process, as it does when it runs inside one of the services
// (a Bash tool of the daemon or the worker): systemd gets one
// `systemctl --user start --no-block` for the units that stopped
// themselves, then one `try-restart --no-block` naming every running unit,
// so the jobs exist before anything acts on them; on macOS one detached
// helper in its own session, started once for all of them, reloads an
// agent whose plist was rewritten and restarts any other in place
// (`kickstart -k`).
func RestartIfRunning(requests ...RestartRequest) []Restart {
	results := make([]Restart, len(requests))
	if goos == "darwin" {
		launchdRestartIfRunning(requests, results)
		return results
	}
	systemdRestartIfRunning(requests, results)
	return results
}

func (s *Service) notInstalled() error {
	return fmt.Errorf("the %s is %w: run `%s`", s.title(), ErrNotInstalled, s.installCommand())
}

func (s *Service) withInstalled(action func(manager) error) error {
	m, err := s.current()
	if err != nil {
		return err
	}
	if _, err := os.Stat(m.file()); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s.notInstalled()
		}
		return err
	}
	return action(m)
}

// Get reports the service's current state. It returns the file and log
// locations even when the file is absent or the manager query fails.
func (s *Service) Get() (Status, error) {
	return s.getStatus(context.Background())
}

// GetAfterInstall gives a systemd service time to exit after startup and a
// launchd service time to reach the running state.
func (s *Service) GetAfterInstall() (Status, error) {
	return s.pollInstallStatus(2 * time.Second)
}

// The timeout only decides whether to query again. Each query runs to
// completion: a manager or loginctl query killed at the deadline would report
// a partial status, dropping the running state or the linger advice.
func (s *Service) pollInstallStatus(timeout time.Duration) (Status, error) {
	deadline := time.Now().Add(timeout)
	for {
		st, err := s.getStatus(context.Background())
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

func (s *Service) getStatus(ctx context.Context) (Status, error) {
	m, err := s.current()
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
func (s *Service) Program() string {
	d, _, _ := s.Installed()
	return d.Program()
}

// Installed reads the installed service file: what it runs and with which
// environment, and whether the file exists. It does not need to connect to
// the service manager.
func (s *Service) Installed() (Definition, bool, error) {
	m, err := s.current()
	if err != nil {
		return Definition{}, false, err
	}
	data, err := readServiceFile(m)
	if data == nil || err != nil {
		return Definition{}, false, err
	}
	return m.definition(data), true, nil
}

func readServiceFile(m manager) ([]byte, error) {
	data, err := os.ReadFile(m.file())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return data, err
}

// Linger returns the Linux advice for running the service without a login.
func (s *Service) Linger() string {
	if goos == "linux" {
		_, advice := s.lingerStatus(context.Background())
		return advice
	}
	return ""
}

// Executable returns the path of the running circus for a service file: a
// stable launcher on PATH (such as a symlink) when it points to this
// executable. A temporary `go run` build is refused.
func Executable() (string, error) { return serviceExecutable() }

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
		return "", errors.New("this executable is a temporary `go run` build. To install the service, install circus first and run its install command")
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

func (s *Service) installError(installErr, recovery error) error {
	if recovery == nil {
		return installErr
	}
	return errors.Join(installErr, fmt.Errorf("restore %s: %w", s.title(), recovery))
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
	f, err := os.CreateTemp(dir, ".circus-service-*")
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

// EnvironmentList turns a recorded environment back into sorted KEY=VALUE
// entries for Install or Replace.
func EnvironmentList(env map[string]string) []string {
	list := make([]string, 0, len(env))
	for key, value := range env {
		list = append(list, key+"="+value)
	}
	slices.Sort(list)
	return list
}
