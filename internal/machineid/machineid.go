// Package machineid derives the machine id a machine reports when it enrolls
// (EnrollRequest.machine_id) and, later, in the daemon's Hello. Both roles on
// one machine must report the same value, so the store can link a machine's
// device and worker identities.
//
// The id is the hex SHA-256 of the OS machine id (/etc/machine-id, then
// /var/lib/dbus/machine-id on Linux and other Unix systems, skipping an
// empty file or systemd's "uninitialized" placeholder; IOPlatformUUID
// from ioreg on macOS) under a circus-specific prefix, so the raw id never
// leaves the machine. Without an OS id (some containers) it falls back to the
// hostname.
package machineid

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
)

// Get returns this machine's id. It fails only when the machine has neither
// an OS machine id nor a hostname.
func Get() (string, error) { return osSource().get() }

// source holds the OS accessors Get uses; tests replace them.
type source struct {
	goos     string
	readFile func(string) ([]byte, error)
	ioreg    func() ([]byte, error)
	hostname func() (string, error)
}

func osSource() source {
	return source{
		goos:     runtime.GOOS,
		readFile: os.ReadFile,
		ioreg: func() ([]byte, error) {
			return exec.Command("ioreg", "-rd1", "-c", "IOPlatformExpertDevice").Output()
		},
		hostname: os.Hostname,
	}
}

var ioregUUID = regexp.MustCompile(`"IOPlatformUUID" = "([^"]+)"`)

func (s source) get() (string, error) {
	var raw string
	switch s.goos {
	case "darwin":
		b, err := s.ioreg()
		if m := ioregUUID.FindSubmatch(b); err == nil && m != nil {
			raw = string(m[1])
		}
	default:
		for _, p := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
			b, err := s.readFile(p)
			if err != nil {
				continue
			}
			// An empty file (some images) or systemd's first-boot
			// placeholder is not an id: try the next source.
			if v := strings.TrimSpace(string(b)); v != "" && v != "uninitialized" {
				raw = v
				break
			}
		}
	}
	if raw == "" {
		host, err := s.hostname()
		if err != nil || host == "" {
			return "", errors.New("machineid: no OS machine id and no hostname")
		}
		raw = "hostname:" + host
	}
	return hash(raw), nil
}

func hash(raw string) string {
	h := sha256.Sum256([]byte("circus-machine-id:" + raw))
	return hex.EncodeToString(h[:])
}
