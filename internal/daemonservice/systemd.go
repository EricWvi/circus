package daemonservice

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

const unitName = "circus-daemon.service"

// LinuxTerminalExitStatus marks a daemon that stopped itself and can be
// brought back by an update. systemd treats this exit as successful.
const LinuxTerminalExitStatus = 78

type systemd struct{ config string }

func (s systemd) file() string { return filepath.Join(s.config, "systemd", "user", unitName) }

func (systemd) logs() string { return "journalctl --user -u " + unitName + " -f" }

func unitText(exe string, args, env []string) string {
	var b strings.Builder
	b.WriteString(`[Unit]
Description=circus device daemon

[Service]
Type=simple
Restart=on-failure
RestartSec=5
`)
	fmt.Fprintf(&b, "SuccessExitStatus=%d\nExecStart=", LinuxTerminalExitStatus)
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

func (systemd) definition(data []byte) definition {
	d := definition{env: make(map[string]string)}
	for line := range strings.SplitSeq(string(data), "\n") {
		key, value, _ := strings.Cut(line, "=")
		switch key {
		case "ExecStart":
			value, legacy := strings.CutPrefix(value, ":")
			value, customArg0 := strings.CutPrefix(value, "@")
			d.argv = nil
			for i, word := range unitWord.FindAllString(value, -1) {
				if i == 1 && customArg0 {
					continue
				}
				arg := unitValue(word)
				if i > 0 && !legacy {
					arg = strings.ReplaceAll(arg, "$$", "$")
				}
				d.argv = append(d.argv, arg)
			}
		case "Environment":
			for _, word := range unitWord.FindAllString(value, -1) {
				if key, val, ok := strings.Cut(unitValue(word), "="); ok {
					d.env[key] = val
				}
			}
		}
	}
	return d
}

func (s systemd) install(exe string, args, env []string, previous []byte) error {
	if err := systemdReachable(); err != nil {
		return err
	}
	var wasRunning, wasEnabled bool
	if previous != nil {
		props, err := unitProperties(context.Background())
		if err != nil {
			return err
		}
		wasRunning, wasEnabled = props["ActiveState"] == "active", props["UnitFileState"] == "enabled"
	}
	if err := writePrivate(s.file(), unitText(exe, args, env)); err != nil {
		return err
	}
	enableTried, enabled, restartTried := false, false, false
	err := func() error {
		if _, err := systemctl("daemon-reload"); err != nil {
			return err
		}
		// The manager may use a different XDG_CONFIG_HOME than this shell.
		enableTried = true
		if _, err := systemctl("enable", s.file()); err != nil {
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
		recovery = disableUnit("--now")
	} else if previous != nil && enableTried && !wasEnabled {
		recovery = disableUnit()
	}
	if restoreErr := restoreServiceFile(s.file(), previous); restoreErr != nil {
		return installError(err, errors.Join(recovery, restoreErr))
	}
	_, reloadErr := systemctl("daemon-reload")
	recovery = errors.Join(recovery, reloadErr)
	if reloadErr == nil && previous != nil && restartTried {
		if wasRunning {
			recovery = errors.Join(recovery, s.restart())
		} else {
			recovery = errors.Join(recovery, s.stop())
		}
	}
	return installError(err, recovery)
}

func (s systemd) uninstall() error {
	if err := systemdReachable(); err != nil {
		return err
	}
	if err := disableUnit("--now"); err != nil {
		return err
	}
	if err := restoreServiceFile(s.file(), nil); err != nil {
		return err
	}
	_, err := systemctl("daemon-reload")
	return err
}

func disableUnit(options ...string) error {
	args := append([]string{"disable"}, options...)
	out, err := systemctl(append(args, unitName)...)
	if unitNotFound(out) {
		return nil
	}
	return err
}

func unitNotFound(out string) bool {
	message := strings.ToLower(out)
	return strings.Contains(message, unitName) &&
		(strings.Contains(message, "does not exist") || strings.Contains(message, "not found") || strings.Contains(message, "not loaded"))
}

func (systemd) start() error {
	_, err := systemctl("start", unitName)
	return err
}

func (systemd) stop() error {
	_, err := systemctl("stop", unitName)
	return err
}

func (systemd) restart() error {
	_, err := systemctl("restart", unitName)
	return err
}

func (s systemd) restartIfRunning() (bool, error) {
	props, err := unitProperties(context.Background())
	if err != nil {
		return false, err
	}
	if props["ActiveState"] == "inactive" && exited(props, LinuxTerminalExitStatus) {
		err = s.start()
		return err == nil, err
	}
	if props["ActiveState"] != "active" {
		return false, nil
	}
	// Let systemd check the state again in case the unit stopped since show.
	_, err = systemctl("try-restart", unitName)
	return err == nil, err
}

func exited(props map[string]string, status int) bool {
	code := props["ExecMainCode"]
	return (code == "1" || code == "exited") && props["ExecMainStatus"] == strconv.Itoa(status)
}

func unitProperties(ctx context.Context) (map[string]string, error) {
	out, err := systemctlContext(ctx, "show", unitName, "--property=ActiveState,SubState,MainPID,UnitFileState,ExecMainCode,ExecMainStatus")
	if err != nil && !unitNotFound(out) {
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

func (systemd) status(ctx context.Context) (Status, error) {
	properties, err := unitProperties(ctx)
	active := properties["ActiveState"]
	st := Status{Enabled: properties["UnitFileState"] == "enabled", Running: active == "active", State: active}
	st.StoppedAfterExit = active == "inactive" && (exited(properties, 0) || exited(properties, LinuxTerminalExitStatus))
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
	st.AtBoot, advice = lingerStatus(ctx)
	if advice != "" {
		st.Advice = []string{advice}
	}
	return st, err
}

func systemdReachable() error {
	out, err := systemctl("is-system-running")
	switch out {
	case "offline":
		if err == nil {
			return noSystemd(errors.New(out))
		}
	case "degraded", "starting", "initializing", "maintenance":
		// These states can exit nonzero even though the manager is reachable.
		return nil
	}
	return err
}

func systemctl(args ...string) (string, error) {
	return systemctlContext(context.Background(), args...)
}

func systemctlContext(ctx context.Context, args ...string) (string, error) {
	out, err := runContext(ctx, "systemctl", append([]string{"--user"}, args...)...)
	if err == nil {
		return out, nil
	}
	message := strings.ToLower(out)
	switch {
	case strings.Contains(message, "connect to bus"), strings.Contains(message, "connect to user scope bus"), strings.Contains(message, "xdg_runtime_dir"), strings.Contains(message, "dbus_session_bus_address"):
		advice := fmt.Sprintf("systemctl --user cannot reach the user bus. Log in directly as this user or set XDG_RUNTIME_DIR=/run/user/%d if that session exists. Then rerun `circus daemon install` on its own", os.Getuid())
		return out, fmt.Errorf("%s: %w", systemdAdvice(advice), err)
	case errors.Is(err, exec.ErrNotFound), message == "offline", message == "unknown", strings.Contains(message, "not been booted with systemd"):
		return out, noSystemd(err)
	default:
		return out, err
	}
}

func noSystemd(err error) error {
	advice := "this system has no running systemd user manager. Run `circus daemon` under the system's own service manager"
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

func lingerStatus(ctx context.Context) (bool, string) {
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
		return false, "The circus daemon service stops when you log out. To keep it running, run `sudo loginctl enable-linger " + name + "`."
	default:
		return false, ""
	}
}
