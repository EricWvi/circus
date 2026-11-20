package daemonservice

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSystemdVerifyGeneratedUnit(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd unit verification requires Linux")
	}
	analyze, err := exec.LookPath("systemd-analyze")
	if err != nil {
		t.Skip("systemd-analyze is unavailable")
	}
	dir := t.TempDir()
	exe := filepath.Join(dir, `circus test%$`)
	current, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(current, exe); err != nil {
		t.Fatal(err)
	}
	credentials := filepath.Join(dir, `credentials % " \${TOKEN}.json`)
	unit := filepath.Join(dir, unitName)
	data := unitText(exe, []string{"daemon", "--service", "--credentials", credentials}, []string{`PATH=/bin:/with spaces`, `HTTPS_PROXY=http://a%b"$c`})
	writeTestFile(t, unit, data, 0600)
	for _, name := range []string{"basic", "default", "shutdown"} {
		writeTestFile(t, filepath.Join(dir, name+".target"), "[Unit]\nDescription=Verification target\nDefaultDependencies=no\n", 0600)
	}
	// Verify loads units in an isolated in-process manager. It never starts them.
	cmd := exec.CommandContext(t.Context(), analyze, "--user", "verify", unit)
	cmd.Env = append(os.Environ(), "XDG_RUNTIME_DIR="+dir, "SYSTEMD_UNIT_PATH="+dir, "SYSTEMD_LOG_LEVEL=warning")
	if out, err := cmd.CombinedOutput(); err != nil || len(out) != 0 {
		t.Fatalf("verify: %v\n%s", err, out)
	}
	d := (systemd{}).definition([]byte(data))
	if d.program() != exe || d.credentials() != credentials || d.env["HTTPS_PROXY"] != `http://a%b"$c` {
		t.Errorf("unexpected definition: %+v", d)
	}
	if !strings.Contains(data, "SuccessExitStatus=78\n") {
		t.Errorf("unit does not treat the terminal status as successful:\n%s", data)
	}
	if strings.Contains(data, "network-online.target") || strings.Contains(data, "ExecStart=:") {
		t.Errorf("the service includes obsolete directives:\n%s", data)
	}
}

func TestLegacySystemdDefinition(t *testing.T) {
	unit := `[Service]
ExecStart=:"/opt/old binary%%$cash" "daemon" "--credentials" "/other/creds$$.json"
Environment="CIRCUS_HOME=/old%%path"
`
	d := (systemd{}).definition([]byte(unit))
	if d.program() != "/opt/old binary%$cash" || d.credentials() != "/other/creds$$.json" || d.env["CIRCUS_HOME"] != "/old%path" {
		t.Errorf("legacy definition = %+v", d)
	}
}
