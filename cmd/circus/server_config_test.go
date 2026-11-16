package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeServerTestConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestServerRequiresConfigAndRejectsOldFlags(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.yaml")
	err := serverCommand([]string{"--config", path}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("missing config: %v", err)
	}
	for _, flag := range []string{"--db", "--listen", "--tls-cert", "--tls-key", "--behind-proxy", "--dev-insecure", "--public-url", "--shutdown-timeout", "--timer-poll-interval", "--max-message-bytes", "--device-grace", "--heartbeat-interval", "--heartbeat-timeout", "--send-pool-bytes", "--receive-pool-bytes", "--worker-connections"} {
		err := serverCommand([]string{flag}, io.Discard, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "flag provided but not defined") {
			t.Errorf("accepted old flag %s: %v", flag, err)
		}
	}
}
