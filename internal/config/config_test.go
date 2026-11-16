package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func loadTest(t *testing.T, body string) (Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(path)
}
func TestLoad(t *testing.T) {
	c, err := loadTest(t, "")
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":8443" || c.OIDC != nil || c.ShutdownTimeout != 0 || c.DB != "" {
		t.Fatalf("defaults = %+v", c)
	}
	c, err = loadTest(t, "listen: 127.0.0.1:1234\ndb: data.db\ndev_insecure: true\nshutdown_timeout: 4s\nworker_connections: 2\ndebug: true\n")
	if err != nil {
		t.Fatal(err)
	}
	if c.Server().ShutdownTimeout != 4*time.Second || c.Server().WorkerConnections != 2 || !c.Debug || c.DB != "data.db" {
		t.Fatalf("config = %+v", c)
	}
	for _, body := range []string{"unknown: true", "oidc:\n  issuer: https://idp\n  client_id: circus", "oidc:\n  unknown: x", "shutdown_timeout: nonsense", "---\n{}\n---\n{}", "public_url: https://host/path", "public_url: https://host/?query=secret", "worker_connections: -1"} {
		if _, err := loadTest(t, body); err == nil {
			t.Errorf("accepted %q", body)
		}
	}
	path := filepath.Join(t.TempDir(), "missing.yaml")
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("missing path: %v", err)
	}
}
func TestOIDCSecretOverride(t *testing.T) {
	t.Setenv("CIRCUS_OIDC_CLIENT_SECRET", "env-secret")
	c, err := loadTest(t, "public_url: https://circus.example.com/\noidc:\n  issuer: https://idp\n  client_id: circus\n  client_secret: file-secret\n  display_name: Authelia\n")
	if err != nil {
		t.Fatal(err)
	}
	if c.OIDC.ClientSecret != "env-secret" || c.OIDC.DisplayName != "Authelia" || c.PublicURL != "https://circus.example.com" {
		t.Fatal("OIDC override failed")
	}
	t.Setenv("CIRCUS_OIDC_CLIENT_SECRET", "")
	c, err = loadTest(t, "public_url: https://circus.example.com\noidc:\n  issuer: https://idp\n  client_id: circus\n  client_secret: file-secret\n")
	if err != nil || c.OIDC.ClientSecret != "file-secret" {
		t.Fatal("empty environment value should leave the file secret unchanged")
	}
}
