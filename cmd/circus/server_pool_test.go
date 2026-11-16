package main

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/EricWvi/circus/internal/wire"
)

// Pool configuration must reach the gateway's shared wire.Budget.
func TestServerPoolConfig(t *testing.T) {
	minSend, minReceive, err := wire.MinBudget(wire.Options{})
	if err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(t.TempDir(), "circus.db")
	t.Run("below minimum names keys and minimum", func(t *testing.T) {
		path := writeServerTestConfig(t, fmt.Sprintf("db: %q\nlisten: 127.0.0.1:0\ndev_insecure: true\nsend_pool_bytes: 1024\nreceive_pool_bytes: 2048\n", db))
		err := serverCommand([]string{"--config", path}, io.Discard, io.Discard)
		if err == nil {
			t.Fatal("accepted undersized pools")
		}
		for _, want := range []string{fmt.Sprintf("send_pool_bytes 1024 is below the minimum %d", 2*minSend), fmt.Sprintf("receive_pool_bytes 2048 is below the minimum %d", 2*minReceive), "twice wire.MinBudget"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%q lacks %q", err, want)
			}
		}
	})
	t.Run("exact minimum accepted", func(t *testing.T) {
		path := writeServerTestConfig(t, fmt.Sprintf("db: %q\nlisten: 192.0.2.1:8443\nbehind_proxy: true\nsend_pool_bytes: %d\nreceive_pool_bytes: %d\n", db, 2*minSend, 2*minReceive))
		err := serverCommand([]string{"--config", path}, io.Discard, io.Discard)
		if err == nil {
			t.Fatal("unexpectedly bound TEST-NET address")
		}
		if strings.Contains(err.Error(), "pool") || strings.Contains(err.Error(), "MinBudget") {
			t.Fatalf("rejected valid pools: %v", err)
		}
	})
}
func TestServerWorkerConnectionsConfig(t *testing.T) {
	path := writeServerTestConfig(t, "dev_insecure: true\nlisten: 127.0.0.1:0\nworker_connections: -1\n")
	err := serverCommand([]string{"--config", path}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "worker_connections") {
		t.Fatal(err)
	}
	if !strings.Contains(serverUsage, "worker_connections") {
		t.Fatal("missing config usage")
	}
}
