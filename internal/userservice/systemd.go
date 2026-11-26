package userservice

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// LinuxTerminalExitStatus marks a daemon that stopped itself and can be
// brought back by an update. systemd treats this exit as successful.
const LinuxTerminalExitStatus = 78

type systemd struct {
	config string
	svc    *Service
}

func (s systemd) unit() string { return s.svc.unitName() }

func (s systemd) file() string { return filepath.Join(s.config, "systemd", "user", s.unit()) }

func (s systemd) logs() string { return "journalctl --user -u " + s.unit() + " -f" }

func (s systemd) unitText(exe string, args, env []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `[Unit]
Description=%s

[Service]
Type=simple
Restart=on-failure
RestartSec=5
`, s.svc.description)
	if s.svc.killMode != "" {
		fmt.Fprintf(&b, "KillMode=%s\n", s.svc.killMode)
	}
	if s.svc.stopTimeout > 0 {
		fmt.Fprintf(&b, "TimeoutStopSec=%d\n", s.svc.stopTimeout)
	}
	fmt.Fprintf(&b, "SuccessExitStatus=%d\n", LinuxTerminalExitStatus)
	if s.svc.noRestartOnTerminal {
		fmt.Fprintf(&b, "RestartPreventExitStatus=%d\n", LinuxTerminalExitStatus)
	}
	b.WriteString("ExecStart=")
	if strings.Contains(exe, "$") {
		// The executable path is used literally. Use @ to give systemd a
		// separate argv[0] that can safely go through dollar expansion.
		b.WriteString("@" + unitQuote(exe) + " ")
	}
	b.WriteString(unitArg(exe))
	for _, arg := range args {
		b.WriteString(" " + unitArg(arg))
	}
	b.WriteByte('\n')
	for _, entry := range env {
		b.WriteString("Environment=" + unitQuote(entry) + "\n")
	}
	b.WriteString("\n[Install]\nWantedBy=default.target\n")
	return b.String()
}

var unitEscapes = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%", "\n", `\n`, "\r", `\r`, "\t", `\t`)

func unitQuote(s string) string { return `"` + unitEscapes.Replace(s) + `"` }

func unitArg(s string) string { return unitQuote(strings.ReplaceAll(s, "$", "$$")) }

var unitWord = regexp.MustCompile(`"(?:[^"\\]|\\.)*"|[^"\s]+`)

func unitValue(word string) string {
	if strings.HasPrefix(word, `"`) {
		value, err := strconv.Unquote(word)
		if err != nil {
			return ""
		}
		word = value
	}
	return strings.ReplaceAll(word, "%%", "%")
}

func (systemd) definition(data []byte) Definition {
	d := Definition{Env: make(map[string]string)}
	for line := range strings.SplitSeq(string(data), "\n") {
		key, value, _ := strings.Cut(line, "=")
		switch key {
		case "ExecStart":
			value, legacy := strings.CutPrefix(value, ":")
			value, customArg0 := strings.CutPrefix(value, "@")
			d.Argv = nil
			for i, word := range unitWord.FindAllString(value, -1) {
				if i == 1 && customArg0 {
					continue
				}
				arg := unitValue(word)
				if i > 0 && !legacy {
					arg = strings.ReplaceAll(arg, "$$", "$")
				}
				d.Argv = append(d.Argv, arg)
			}
		case "Environment":
			for _, word := range unitWord.FindAllString(value, -1) {
				if key, val, ok := strings.Cut(unitValue(word), "="); ok {
					d.Env[key] = val
				}
			}
		}
	}
	return d
}

func (s systemd) install(exe string, args, env []string, previous []byte) error {
	if err := s.reachable(); err != nil {
		return err
	}
	var wasRunning, wasEnabled bool
	if previous != nil {
		props, err := s.properties(context.Background())
		if err != nil {
			return err
		}
		wasRunning, wasEnabled = props["ActiveState"] == "active", props["UnitFileState"] == "enabled"
	}
	if err := writePrivate(s.file(), s.unitText(exe, args, env)); err != nil {
		return err
	}
	enableTried, enabled, restartTried := false, false, false
	err := func() error {
		if _, err := s.systemctl("daemon-reload"); err != nil {
			return err
		}
		// The manager may use a different XDG_CONFIG_HOME than this shell.
		enableTried = true
		if _, err := s.systemctl("enable", s.file()); err != nil {
			return err
		}
		enabled, restartTried = true, true
		return s.restart()
	}()
	if err == nil {
		return nil
	}
	var recovery error
	if enabled && previous == nil {
		recovery = s.disable("--now")
	} else if previous != nil && enableTried && !wasEnabled {
		recovery = s.disable()
	}
	if restoreErr := restoreServiceFile(s.file(), previous); restoreErr != nil {
		return s.svc.installError(err, errors.Join(recovery, restoreErr))
	}
	_, reloadErr := s.systemctl("daemon-reload")
	recovery = errors.Join(recovery, reloadErr)
	if reloadErr == nil && previous != nil && restartTried {
		if wasRunning {
			recovery = errors.Join(recovery, s.restart())
		} else {
			recovery = errors.Join(recovery, s.stop())
		}
	}
	return s.svc.installError(err, recovery)
}

// rewrite writes the new unit and reloads systemd's configuration. A
// failure restores the previous unit.
func (s systemd) rewrite(exe string, args, env []string, previous []byte) error {
	if err := s.reachable(); err != nil {
		return err
	}
	if err := writePrivate(s.file(), s.unitText(exe, args, env)); err != nil {
		return err
	}
	_, err := s.systemctl("daemon-reload")
	if err == nil {
		return nil
	}
	if restoreErr := restoreServiceFile(s.file(), previous); restoreErr != nil {
		return s.svc.installError(err, restoreErr)
	}
	_, recovery := s.systemctl("daemon-reload")
	return s.svc.installError(err, recovery)
}

func (s systemd) uninstall() error {
	if err := s.reachable(); err != nil {
		return err
	}
	if err := s.disable("--now"); err != nil {
		return err
	}
	if err := restoreServiceFile(s.file(), nil); err != nil {
		return err
	}
	_, err := s.systemctl("daemon-reload")
	return err
}

func (s systemd) disable(options ...string) error {
	args := append([]string{"disable"}, options...)
	out, err := s.systemctl(append(args, s.unit())...)
	if s.notFound(out) {
		return nil
	}
	return err
}

func (s systemd) notFound(out string) bool {
	message := strings.ToLower(out)
	return strings.Contains(message, s.unit()) &&
		(strings.Contains(message, "does not exist") || strings.Contains(message, "not found") || strings.Contains(message, "not loaded"))
}

func (s systemd) start() error {
	_, err := s.systemctl("start", s.unit())
	return err
}

func (s systemd) stop() error {
	_, err := s.systemctl("stop", s.unit())
	return err
}

func (s systemd) restart() error {
	_, err := s.systemctl("restart", s.unit())
	return err
}

// restartIfRunning restarts a running unit, or starts one that stopped
// itself, without waiting for the job.
func (s systemd) restartIfRunning() (bool, error) {
	results := make([]Restart, 1)
	systemdRestartIfRunning([]RestartRequest{{Service: s.svc}}, results)
	return results[0].Requested, results[0].Err
}

// systemdRestartIfRunning reads every installed unit's state first, then
// asks for the jobs with as few calls as possible: one `start --no-block`
// naming the units that stopped themselves (starting them cannot stop this
// process), then one `try-restart --no-block` naming the running ones in
// the order given. systemctl sends one request per unit, but in a single
// run without waiting for any job, so the gap a restart that kills this
// process has to keep a later unit from being asked is one D-Bus round
// trip, not a separate process.
func systemdRestartIfRunning(requests []RestartRequest, results []Restart) {
	var units = map[string][]int{}
	var manager systemd
	for i, r := range requests {
		m, err := r.Service.current()
		if err == nil {
			if _, statErr := os.Stat(m.file()); statErr != nil {
				if !errors.Is(statErr, os.ErrNotExist) {
					results[i].Err = fmt.Errorf("restart the %s: %w", r.Service.title(), statErr)
				}
				continue
			}
		}
		var props map[string]string
		if err == nil {
			manager = m.(systemd)
			props, err = manager.properties(context.Background())
		}
		if err != nil {
			results[i].Err = fmt.Errorf("restart the %s: %w", r.Service.title(), err)
			continue
		}
		switch {
		case props["ActiveState"] == "inactive" && exited(props, LinuxTerminalExitStatus):
			units["start"] = append(units["start"], i)
		case props["ActiveState"] == "active":
			units["try-restart"] = append(units["try-restart"], i)
		}
	}
	for _, verb := range []string{"start", "try-restart"} {
		if len(units[verb]) == 0 {
			continue
		}
		args := []string{verb, "--no-block"}
		for _, i := range units[verb] {
			args = append(args, requests[i].Service.unitName())
		}
		// Let systemd check the state again in case a unit stopped since show.
		_, err := manager.systemctl(args...)
		if err != nil && len(units[verb]) > 1 {
			err = sharedError(args[2:], err)
		}
		for _, i := range units[verb] {
			if err != nil && len(units[verb]) > 1 {
				results[i].Err = err
			} else if err != nil {
				results[i].Err = fmt.Errorf("restart the %s: %w", requests[i].Service.title(), err)
			} else {
				results[i].Requested = true
			}
		}
	}
}

func exited(props map[string]string, status int) bool {
	code := props["ExecMainCode"]
	return (code == "1" || code == "exited") && props["ExecMainStatus"] == strconv.Itoa(status)
}

func (s systemd) properties(ctx context.Context) (map[string]string, error) {
	out, err := s.systemctlContext(ctx, "show", s.unit(), "--property=ActiveState,SubState,MainPID,UnitFileState,ExecMainCode,ExecMainStatus")
	if err != nil && !s.notFound(out) {
		return nil, err
	}
	properties := make(map[string]string)
	for line := range strings.SplitSeq(out, "\n") {
		if key, value, ok := strings.Cut(line, "="); ok {
			properties[key] = value
		}
	}
	return properties, nil
}

func (s systemd) status(ctx context.Context) (Status, error) {
	properties, err := s.properties(ctx)
	active := properties["ActiveState"]
	st := Status{Enabled: properties["UnitFileState"] == "enabled", Running: active == "active", State: active}
	st.StoppedAfterExit = active == "inactive" && (exited(properties, 0) || exited(properties, LinuxTerminalExitStatus))
	st.Terminal = active == "inactive" && exited(properties, LinuxTerminalExitStatus)
	if sub := properties["SubState"]; sub != "" && sub != active {
		st.State = sub
		if active != "" {
			st.State = active + " (" + sub + ")"
		}
	}
	if pid, err := strconv.Atoi(properties["MainPID"]); err == nil && pid > 0 {
		st.PID = pid
	}
	if exit := properties["ExecMainStatus"]; exit != "" {
		switch properties["ExecMainCode"] {
		case "1", "exited":
			st.LastExit = "exit status " + exit
		case "2", "3", "killed", "dumped":
			st.LastExit = "signal " + exit
		}
	}
	var advice string
	st.AtBoot, advice = s.svc.lingerStatus(ctx)
	if advice != "" {
		st.Advice = []string{advice}
	}
	return st, err
}

func (s systemd) reachable() error {
	out, err := s.systemctl("is-system-running")
	switch out {
	case "offline":
		if err == nil {
			return s.noSystemd(errors.New(out))
		}
	case "degraded", "starting", "initializing", "maintenance":
		// These states can exit nonzero even though the manager is reachable.
		return nil
	}
	return err
}

func (s systemd) systemctl(args ...string) (string, error) {
	return s.systemctlContext(context.Background(), args...)
}

func (s systemd) systemctlContext(ctx context.Context, args ...string) (string, error) {
	out, err := runContext(ctx, "systemctl", append([]string{"--user"}, args...)...)
	if err == nil {
		return out, nil
	}
	message := strings.ToLower(out)
	switch {
	case strings.Contains(message, "connect to bus"), strings.Contains(message, "connect to user scope bus"), strings.Contains(message, "xdg_runtime_dir"), strings.Contains(message, "dbus_session_bus_address"):
		advice := fmt.Sprintf("systemctl --user cannot reach the user bus. Log in directly as this user or set XDG_RUNTIME_DIR=/run/user/%d if that session exists. Then rerun `%s` on its own", os.Getuid(), s.svc.installCommand())
		return out, fmt.Errorf("%s: %w", systemdAdvice(advice), err)
	case errors.Is(err, exec.ErrNotFound), message == "offline", message == "unknown", strings.Contains(message, "not been booted with systemd"):
		return out, s.noSystemd(err)
	default:
		return out, err
	}
}

func (s systemd) noSystemd(err error) error {
	advice := "this system has no running systemd user manager. " + capitalize(s.svc.foreground) + " under the system's own service manager"
	return fmt.Errorf("%s: %w", systemdAdvice(advice), err)
}

func systemdAdvice(advice string) string {
	if isWSL() {
		advice += ". On WSL, set systemd=true in the [boot] section of /etc/wsl.conf, then run `wsl --shutdown` from Windows"
	}
	return advice
}

var isWSL = func() bool {
	data, _ := os.ReadFile("/proc/version")
	return strings.Contains(strings.ToLower(string(data)), "microsoft")
}

func (s *Service) lingerStatus(ctx context.Context) (bool, string) {
	name := os.Getenv("USER")
	if u, err := user.Current(); err == nil && u.Username != "" {
		name = u.Username
	}
	if name == "" {
		return false, ""
	}
	out, err := runContext(ctx, "loginctl", "show-user", name, "--property=Linger", "--value")
	if err != nil {
		return false, ""
	}
	switch out {
	case "yes":
		return true, ""
	case "no":
		if strings.ContainsAny(name, " \\'\"$`;&|<>()") {
			name = shellQuote(name)
		}
		return false, "The " + s.title() + " stops when you log out. To keep it running, run `sudo loginctl enable-linger " + name + "`."
	default:
		return false, ""
	}
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
