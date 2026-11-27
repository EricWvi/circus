package userservice

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

type launchd struct {
	home string
	svc  *Service
}

func (l launchd) label() string { return l.svc.label() }

func (l launchd) file() string {
	return filepath.Join(l.home, "Library", "LaunchAgents", l.label()+".plist")
}

func (l launchd) logFile() string {
	return filepath.Join(l.home, "Library", "Logs", "circus", l.svc.name+".log")
}

func (l launchd) logs() string { return "tail -f " + shellQuote(l.logFile()) }

func launchDomain() string { return "gui/" + strconv.Itoa(os.Getuid()) }

func (l launchd) target() string { return launchDomain() + "/" + l.label() }

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
	writeString(l.label())
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
`)
	if l.svc.stopTimeout > 0 {
		fmt.Fprintf(&b, "  <key>ExitTimeOut</key>\n  <integer>%d</integer>\n", l.svc.stopTimeout)
	}
	b.WriteString(`  <key>ProcessType</key>
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

func (launchd) definition(data []byte) Definition {
	var plist struct {
		Dict plistElement `xml:"dict"`
	}
	d := Definition{Env: make(map[string]string)}
	if err := xml.Unmarshal(data, &plist); err != nil {
		return d
	}
	for i := 0; i+1 < len(plist.Dict.Children); i += 2 {
		key, value := plist.Dict.Children[i], plist.Dict.Children[i+1]
		switch key.Text {
		case "ProgramArguments":
			for _, arg := range value.Children {
				d.Argv = append(d.Argv, arg.Text)
			}
		case "EnvironmentVariables":
			for j := 0; j+1 < len(value.Children); j += 2 {
				d.Env[value.Children[j].Text] = value.Children[j+1].Text
			}
		}
	}
	return d
}

func (l launchd) install(exe string, args, env []string, previous []byte) error {
	return l.write(exe, args, env, previous)
}

// rewrite only writes the plist: launchd keeps running a loaded agent's
// old definition until it is booted out, which RestartIfRunning does.
func (l launchd) rewrite(exe string, args, env []string, previous []byte) error {
	if err := os.MkdirAll(filepath.Dir(l.logFile()), 0700); err != nil {
		return err
	}
	return writePrivate(l.file(), l.plist(exe, args, env))
}

// launchdRestartIfRunning restarts every installed and loaded agent of
// requests, in order, from one detached helper started once for all of
// them: an agent whose plist was rewritten is reloaded from it, any other
// is restarted in place with `kickstart -k` and stays loaded. A failure of
// the helper is reported for every agent it was to restart.
func launchdRestartIfRunning(requests []RestartRequest, results []Restart) {
	home, err := os.UserHomeDir()
	if err != nil {
		for i := range results {
			results[i].Err = err
		}
		return
	}
	var steps []launchStep
	var reloaded []int
	for i, r := range requests {
		s := r.Service
		l := launchd{home: home, svc: s}
		if _, err := os.Stat(l.file()); err != nil {
			continue
		}
		_, loaded, err := l.print(context.Background())
		if err != nil {
			results[i].Err = fmt.Errorf("restart the %s: %w", s.title(), err)
			continue
		}
		if loaded {
			results[i].Requested = true
			kind := stepKickstart
			if r.Rewritten {
				kind = stepReload
			}
			steps = append(steps, launchStep{svc: s, kind: kind})
			reloaded = append(reloaded, i)
		}
	}
	if len(steps) == 0 {
		return
	}
	if _, err := reloadDetached(home, steps); err != nil {
		if len(steps) > 1 {
			var labels []string
			for _, step := range steps {
				labels = append(labels, step.svc.label())
			}
			err = sharedError(labels, err)
		}
		for _, i := range reloaded {
			results[i] = Restart{Err: err}
		}
	}
}

// write writes the plist and (re)loads it. A failure restores the
// previous plist and whether it was loaded.
func (l launchd) write(exe string, args, env []string, previous []byte) error {
	_, wasLoaded, err := l.print(context.Background())
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(l.logFile()), 0700); err != nil {
		return err
	}
	if err := writePrivate(l.file(), l.plist(exe, args, env)); err != nil {
		return err
	}
	bootstrapTried, err := reloadDetached(l.home, []launchStep{loadStep(l.svc, wasLoaded)})
	if err == nil {
		return nil
	}
	restoreErr := restoreServiceFile(l.file(), previous)
	if restoreErr == nil && (wasLoaded || bootstrapTried) {
		restoreErr = l.restoreAgent(wasLoaded && previous != nil, bootstrapTried)
	}
	return l.svc.installError(err, restoreErr)
}

// restoreAgent clears any partially loaded replacement and reloads the old
// plist only if it was loaded before installation.
func (l launchd) restoreAgent(wasLoaded, bootstrapTried bool) error {
	_, loaded, err := l.print(context.Background())
	if errors.Is(err, errNoGUIDomain) {
		return nil
	}
	if err != nil {
		return err
	}
	if loaded && bootstrapTried {
		if err := l.bootout(); err != nil {
			return err
		}
		if err := l.waitForUnload(); err != nil {
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
		return l.guiDomainError(err)
	}
	// Some launchctl versions only report a generic I/O error on bootstrap.
	domain, domainErr := run("launchctl", "print", launchDomain())
	if domainErr != nil && noGUIDomain(domain) {
		return l.guiDomainError(err)
	}
	return err
}

func (l launchd) start() error {
	_, loaded, err := l.print(context.Background())
	if err != nil {
		return err
	}
	if !loaded {
		return l.bootstrap()
	}
	_, err = run("launchctl", "kickstart", l.target())
	return err
}

func (l launchd) bootout() error {
	_, err := run("launchctl", "bootout", l.target())
	return err
}

// stop boots the agent out and waits (up to unloadTimeout) until launchd
// no longer has it loaded. `launchctl bootout` can return while the agent's
// process is still exiting (launchd shows it "SIGTERMed" until then), so
// without the wait `stop` and `uninstall` could report success with the
// agent still loaded and its process still running.
func (l launchd) stop() error {
	_, loaded, err := l.print(context.Background())
	if errors.Is(err, errNoGUIDomain) {
		return nil
	}
	if err != nil || !loaded {
		return err
	}
	if err := l.bootout(); err != nil {
		return err
	}
	return l.waitForUnload()
}

func (l launchd) restart() error {
	restarted, err := l.restartIfRunning()
	if err != nil || restarted {
		return err
	}
	return l.bootstrap()
}

func (l launchd) restartIfRunning() (bool, error) {
	_, loaded, err := l.print(context.Background())
	if err != nil || !loaded {
		return false, err
	}
	// launchd owns the whole restart, so stopping the old daemon cannot
	// interrupt the operation even if the daemon started this command.
	_, err = run("launchctl", "kickstart", "-k", l.target())
	return err == nil, err
}

// unloadTimeout bounds the wait for a booted-out agent to unload: longer
// than the largest ExitTimeOut a circus plist sets (the worker's graceful
// stop), with a margin.
const unloadTimeout = 45 * time.Second

func (l launchd) waitForUnload() error {
	ctx, cancel := context.WithTimeout(context.Background(), unloadTimeout)
	defer cancel()
	poll := time.NewTicker(100 * time.Millisecond)
	defer poll.Stop()
	for {
		_, loaded, err := l.print(ctx)
		if ctx.Err() != nil {
			return fmt.Errorf("wait for launchd to unload %s: %w", l.label(), ctx.Err())
		}
		if err != nil || !loaded {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for launchd to unload %s: %w", l.label(), ctx.Err())
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

func (l launchd) guiDomainError(err error) error {
	return fmt.Errorf("%w. Log in to the macOS desktop as this user. Then rerun `%s` on its own: %v", errNoGUIDomain, l.svc.installCommand(), err)
}

func (l launchd) print(ctx context.Context) (string, bool, error) {
	out, err := runContext(ctx, "launchctl", "print", l.target())
	if err != nil {
		if noGUIDomain(out) {
			return "", false, l.guiDomainError(err)
		}
		if strings.Contains(strings.ToLower(out), "could not find service") {
			return "", false, nil
		}
		return "", false, err
	}
	return out, true, nil
}

func (l launchd) enabled(ctx context.Context) (bool, error) {
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
		if ok && strings.Trim(strings.TrimSpace(key), `"`) == l.label() {
			value = strings.TrimSpace(value)
			return value != "disabled" && value != "true", nil
		}
	}
	return true, nil
}

func (l launchd) status(ctx context.Context) (Status, error) {
	out, loaded, err := l.print(ctx)
	if err != nil && !errors.Is(err, errNoGUIDomain) {
		return Status{}, err
	}
	enabled, err := l.enabled(ctx)
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
		// Terminal stays false: on macOS a service stops itself for good by
		// exiting 0, which a plain kill does as well.
	}
	return st, nil
}
