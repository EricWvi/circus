package daemonservice

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestServiceEnvironment(t *testing.T) {
	f := fakeService(t, "linux")
	t.Chdir(f.dir)
	settings := map[string]string{
		"CIRCUS_HOME": "relative/config", "LANG": "en_US.UTF-8", "LC_CTYPE": "C.UTF-8", "LC_NUMERIC": "C",
		"SHELL": "/opt/bin/bash", "USER": "example", "HTTPS_PROXY": "https://example.test?a=b",
		"no_proxy": "", "XDG_STATE_HOME": "/x/state", "XDG_CACHE_HOME": "/x/cache", "XDG_DATA_HOME": "/x/data",
		"LOGNAME": "other", "TMPDIR": "/other", "UNRELATED_TOKEN": "test-value",
		"SSL_CERT_FILE": "bad\xff", "SSL_CERT_DIR": "/my/certs", "LC_BAD": "bad\nvalue", "HTTP_PROXY": "bad\x1bvalue",
	}
	for key, value := range settings {
		t.Setenv(key, value)
	}
	t.Setenv("ALL_PROXY", "")
	if err := os.Unsetenv("ALL_PROXY"); err != nil {
		t.Fatal(err)
	}
	env, skipped, err := serviceEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(skipped, []string{"HTTP_PROXY", "LC_BAD", "SSL_CERT_FILE"}) {
		t.Errorf("skipped environment = %v", skipped)
	}
	if !slices.IsSorted(env) {
		t.Errorf("environment order is unstable: %v", env)
	}
	values := make(map[string]string)
	for _, entry := range env {
		key, val, _ := strings.Cut(entry, "=")
		if _, exists := values[key]; exists {
			t.Errorf("duplicate environment setting %s", key)
		}
		values[key] = val
	}
	for key, want := range map[string]string{
		"PATH": f.bin, "CIRCUS_HOME": filepath.Join(f.dir, "relative", "config"),
		"LANG": "en_US.UTF-8", "LC_CTYPE": "C.UTF-8", "LC_NUMERIC": "C", "HOME": f.home,
		"SSL_CERT_DIR": "/my/certs", "USER": "example", "SHELL": "/opt/bin/bash", "HTTPS_PROXY": "https://example.test?a=b", "no_proxy": "",
		"XDG_CONFIG_HOME": f.config, "XDG_STATE_HOME": "/x/state", "XDG_CACHE_HOME": "/x/cache", "XDG_DATA_HOME": "/x/data",
	} {
		if got, exists := values[key]; !exists || got != want {
			t.Errorf("%s = %q (set: %t), want %q", key, got, exists, want)
		}
	}
	for _, key := range []string{"ALL_PROXY", "HTTP_PROXY", "LC_BAD", "SSL_CERT_FILE", "LOGNAME", "TMPDIR", "UNRELATED_TOKEN"} {
		if _, exists := values[key]; exists {
			t.Errorf("unexpected %s in service environment", key)
		}
	}
}

func TestPickExecutable(t *testing.T) {
	f := fakeService(t, "linux")
	real := filepath.Join(f.dir, "circus-real")
	writeTestFile(t, real, "binary", 0o700)
	link := filepath.Join(f.bin, "circus")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if got, err := pick(real, real); err != nil || got != link {
		t.Errorf("pick = %q, %v; want the PATH symlink %q", got, err, link)
	}
	prefix := filepath.Join(f.dir, "go-builds")
	if err := os.Mkdir(prefix, 0700); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(prefix, "another-circus")
	writeTestFile(t, other, "another binary", 0o700)
	if got, err := pick(other, other); err != nil || got != other {
		t.Errorf("pick = %q, %v; want %q", got, err, other)
	}
	temporary := filepath.Join(f.dir, "go-build123", "exe", "circus")
	if _, err := pick(temporary, temporary); err == nil || !strings.Contains(err.Error(), "install circus first") {
		t.Errorf("go run executable: %v", err)
	}
}
