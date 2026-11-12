package machineid

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"strings"
	"testing"
)

func want(raw string) string {
	h := sha256.Sum256([]byte("circus-machine-id:" + raw))
	return hex.EncodeToString(h[:])
}

func files(m map[string]string) func(string) ([]byte, error) {
	return func(p string) ([]byte, error) {
		if v, ok := m[p]; ok {
			return []byte(v), nil
		}
		return nil, fs.ErrNotExist
	}
}

func TestSources(t *testing.T) {
	noIoreg := func() ([]byte, error) { return nil, errors.New("no ioreg") }
	host := func() (string, error) { return "box", nil }
	cases := []struct {
		name string
		src  source
		want string
	}{
		{"linux machine-id, trimmed", source{"linux", files(map[string]string{"/etc/machine-id": "abc\n", "/var/lib/dbus/machine-id": "dbus"}), noIoreg, host}, want("abc")},
		{"dbus fallback", source{"linux", files(map[string]string{"/var/lib/dbus/machine-id": " dbus \n"}), noIoreg, host}, want("dbus")},
		{"hostname fallback", source{"linux", files(nil), noIoreg, host}, want("hostname:box")},
		{"empty machine-id falls back", source{"freebsd", files(map[string]string{"/etc/machine-id": "\n"}), noIoreg, host}, want("hostname:box")},
		{"empty machine-id, dbus has one", source{"linux", files(map[string]string{"/etc/machine-id": "", "/var/lib/dbus/machine-id": "dbus\n"}), noIoreg, host}, want("dbus")},
		{"uninitialized, dbus has one", source{"linux", files(map[string]string{"/etc/machine-id": "uninitialized\n", "/var/lib/dbus/machine-id": "dbus"}), noIoreg, host}, want("dbus")},
		{"uninitialized everywhere", source{"linux", files(map[string]string{"/etc/machine-id": "uninitialized", "/var/lib/dbus/machine-id": " "}), noIoreg, host}, want("hostname:box")},
		{"macOS ioreg", source{"darwin", files(map[string]string{"/etc/machine-id": "ignored"}), func() ([]byte, error) {
			return []byte(`  "IOPlatformSerialNumber" = "X"` + "\n" + `  "IOPlatformUUID" = "1234-ABCD"` + "\n"), nil
		}, host}, want("1234-ABCD")},
		{"macOS without ioreg", source{"darwin", files(nil), noIoreg, host}, want("hostname:box")},
	}
	for _, c := range cases {
		got, err := c.src.get()
		if err != nil || got != c.want {
			t.Errorf("%s: %q, %v; want %q", c.name, got, err, c.want)
		}
	}
	none := source{"linux", files(nil), noIoreg, func() (string, error) { return "", errors.New("no host") }}
	if got, err := none.get(); err == nil {
		t.Fatalf("no id and no hostname: %q", got)
	}
}

func TestGetOnThisMachine(t *testing.T) {
	a, err := Get()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := Get()
	if a != b || len(a) != 64 {
		t.Fatalf("Get = %q then %q", a, b)
	}
	if raw, err := os.ReadFile("/etc/machine-id"); err == nil && strings.TrimSpace(string(raw)) != "" && strings.TrimSpace(string(raw)) != "uninitialized" {
		if a != want(strings.TrimSpace(string(raw))) {
			t.Fatal("Get does not derive from /etc/machine-id")
		}
	}
}
