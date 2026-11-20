package daemonservice

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const label = "com.github.ericwvi.circus.daemon"

type launchd struct{ home string }

func (l launchd) file() string {
	return filepath.Join(l.home, "Library", "LaunchAgents", label+".plist")
}

func (l launchd) logFile() string {
	return filepath.Join(l.home, "Library", "Logs", "circus", "daemon.log")
}

func (l launchd) logs() string { return "tail -f " + shellQuote(l.logFile()) }

func launchDomain() string { return "gui/" + strconv.Itoa(os.Getuid()) }

func launchTarget() string { return launchDomain() + "/" + label }

func (l launchd) plist(exe string, args, env []string) string {
	var b strings.Builder
	writeString := func(value string) {
		b.WriteString("<string>" + html.EscapeString(value) + "</string>\n")
	}
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  `)
	writeString(label)
	b.WriteString("  <key>ProgramArguments</key>\n  <array>\n")
	for _, arg := range append([]string{exe}, args...) {
		b.WriteString("    ")
		writeString(arg)
	}
	b.WriteString(`  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <dict>
    <key>SuccessfulExit</key>
    <false/>
  </dict>
  <key>ThrottleInterval</key>
  <integer>5</integer>
  <key>ProcessType</key>
  <string>Standard</string>
  <key>StandardOutPath</key>
  `)
	writeString(l.logFile())
	b.WriteString("  <key>StandardErrorPath</key>\n  ")
	writeString(l.logFile())
	if len(env) > 0 {
		b.WriteString("  <key>EnvironmentVariables</key>\n  <dict>\n")
		for _, entry := range env {
			key, value, _ := strings.Cut(entry, "=")
			b.WriteString("    <key>" + html.EscapeString(key) + "</key>\n    ")
			writeString(value)
		}
		b.WriteString("  </dict>\n")
	}
	b.WriteString("</dict>\n</plist>\n")
	return b.String()
}

type plistElement struct {
	Text     string         `xml:",chardata"`
	Children []plistElement `xml:",any"`
}

func (launchd) definition(data []byte) definition {
	var plist struct {
		Dict plistElement `xml:"dict"`
	}
	d := definition{env: make(map[string]string)}
	if err := xml.Unmarshal(data, &plist); err != nil {
		return d
	}
	for i := 0; i+1 < len(plist.Dict.Children); i += 2 {
		key, value := plist.Dict.Children[i], plist.Dict.Children[i+1]
		switch key.Text {
		case "ProgramArguments":
			for _, arg := range value.Children {
				d.argv = append(d.argv, arg.Text)
			}
		case "EnvironmentVariables":
			for j := 0; j+1 < len(value.Children); j += 2 {
				d.env[value.Children[j].Text] = value.Children[j+1].Text
			}
		}
	}
	return d
}

func (l launchd) install(exe string, args, env []string, previous []byte) error {
	_, wasLoaded, err := launchPrint(context.Background())
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(l.logFile()), 0700); err != nil {
		return err
	}
	if err := writePrivate(l.file(), l.plist(exe, args, env)); err != nil {
		return err
	}
	bootstrapTried, err := l.reloadDetached(wasLoaded)
	if err == nil {
		return nil
	}
	restoreErr := restoreServiceFile(l.file(), previous)
	if restoreErr == nil && (wasLoaded || bootstrapTried) {
		restoreErr = l.restoreAgent(wasLoaded && previous != nil, bootstrapTried)
	}
	return installError(err, restoreErr)
}

// restoreAgent clears any partially loaded replacement and reloads the old
// plist only if it was loaded before installation.
func (l launchd) restoreAgent(wasLoaded, bootstrapTried bool) error {
	_, loaded, err := launchPrint(context.Background())
	if errors.Is(err, errNoGUIDomain) {
		return nil
	}
	if err != nil {
		return err
	}
	if loaded && bootstrapTried {
		if err := bootout(); err != nil {
			return err
		}
		if err := waitForUnload(); err != nil {
			return err
		}
		loaded = false
	}
	if wasLoaded && !loaded {
		return l.bootstrap()
	}
	return nil
}

func (l launchd) uninstall() error {
	if err := l.stop(); err != nil {
		return err
	}
	return restoreServiceFile(l.file(), nil)
}

func (l launchd) bootstrap() error {
	out, err := run("launchctl", "bootstrap", launchDomain(), l.file())
	if err == nil {
		return nil
	}
	if noGUIDomain(out) {
		return guiDomainError(err)
	}
	// Some launchctl versions only report a generic I/O error on bootstrap.
	domain, domainErr := run("launchctl", "print", launchDomain())
	if domainErr != nil && noGUIDomain(domain) {
		return guiDomainError(err)
	}
	return err
}

func (l launchd) start() error {
	_, loaded, err := launchPrint(context.Background())
	if err != nil {
		return err
	}
	if !loaded {
		return l.bootstrap()
	}
	_, err = run("launchctl", "kickstart", launchTarget())
	return err
}

func bootout() error {
	_, err := run("launchctl", "bootout", launchTarget())
	return err
}

func (launchd) stop() error {
	_, loaded, err := launchPrint(context.Background())
	if errors.Is(err, errNoGUIDomain) {
		return nil
	}
	if err != nil || !loaded {
		return err
	}
	return bootout()
}

func (l launchd) restart() error {
	restarted, err := l.restartIfRunning()
	if err != nil || restarted {
		return err
	}
	return l.bootstrap()
}

func (launchd) restartIfRunning() (bool, error) {
	_, loaded, err := launchPrint(context.Background())
	if err != nil || !loaded {
		return false, err
	}
	// launchd owns the whole restart, so stopping the old daemon cannot
	// interrupt the operation even if the daemon started this command.
	_, err = run("launchctl", "kickstart", "-k", launchTarget())
	return err == nil, err
}

const unloadTimeout = 25 * time.Second

func waitForUnload() error {
	ctx, cancel := context.WithTimeout(context.Background(), unloadTimeout)
	defer cancel()
	poll := time.NewTicker(100 * time.Millisecond)
	defer poll.Stop()
	for {
		_, loaded, err := launchPrint(ctx)
		if ctx.Err() != nil {
			return fmt.Errorf("wait for launchd to unload %s: %w", label, ctx.Err())
		}
		if err != nil || !loaded {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for launchd to unload %s: %w", label, ctx.Err())
		case <-poll.C:
		}
	}
}

var errNoGUIDomain = errors.New("launchd GUI domain unavailable")

func noGUIDomain(out string) bool {
	message := strings.ToLower(out)
	return strings.Contains(message, "could not find domain") ||
		strings.Contains(message, "could not find specified domain") ||
		strings.Contains(message, "domain does not exist")
}

func guiDomainError(err error) error {
	return fmt.Errorf("%w. Log in to the macOS desktop as this user. Then rerun `circus daemon install` on its own: %v", errNoGUIDomain, err)
}

func launchPrint(ctx context.Context) (string, bool, error) {
	out, err := runContext(ctx, "launchctl", "print", launchTarget())
	if err != nil {
		if noGUIDomain(out) {
			return "", false, guiDomainError(err)
		}
		if strings.Contains(strings.ToLower(out), "could not find service") {
			return "", false, nil
		}
		return "", false, err
	}
	return out, true, nil
}

func launchEnabled(ctx context.Context) (bool, error) {
	out, err := runContext(ctx, "launchctl", "print-disabled", launchDomain())
	if err != nil {
		if noGUIDomain(out) {
			// The plist is still eligible to load when the user logs in.
			return true, nil
		}
		return false, err
	}
	for line := range strings.SplitSeq(out, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=>")
		if ok && strings.Trim(strings.TrimSpace(key), `"`) == label {
			value = strings.TrimSpace(value)
			return value != "disabled" && value != "true", nil
		}
	}
	return true, nil
}

func (launchd) status(ctx context.Context) (Status, error) {
	out, loaded, err := launchPrint(ctx)
	if err != nil && !errors.Is(err, errNoGUIDomain) {
		return Status{}, err
	}
	enabled, err := launchEnabled(ctx)
	if err != nil {
		return Status{}, err
	}
	st := Status{Enabled: enabled}
	if !loaded {
		st.State = "not loaded"
		return st, nil
	}
	for line := range strings.SplitSeq(out, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), " = ")
		if !ok {
			continue
		}
		switch key {
		case "state":
			if st.State == "" {
				st.State = value
			}
		case "pid":
			if pid, err := strconv.Atoi(value); err == nil && pid > 0 {
				st.PID = pid
			}
		case "last exit code":
			if !strings.HasPrefix(value, "(never") {
				st.LastExit = value
			}
		}
	}
	st.Running = st.State == "running"
	if !st.Running && st.PID == 0 && st.LastExit != "" {
		var code int
		_, err := fmt.Sscanf(st.LastExit, "%d", &code)
		st.StoppedAfterExit = err == nil && (code == 0 || code == LinuxTerminalExitStatus)
	}
	return st, nil
}
