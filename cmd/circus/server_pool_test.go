package main

import (
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/EricWvi/circus/internal/wire"
)

// TestServerPoolFlags covers the --send-pool-bytes and --receive-pool-bytes
// wiring of circus server: the values reach the gateway's shared wire.Budget,
// pools below twice wire.MinBudget are rejected with the flag name and the
// minimum, and the exact minimum is accepted (ConfigureLinks then passes and
// the command fails only on the deliberately unassignable listen address).
func TestServerPoolFlags(t *testing.T) {
	// The same defaults serverCommand passes with no --max-message-bytes.
	minSend, minReceive, err := wire.MinBudget(wire.Options{})
	if err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(t.TempDir(), "circus.db")

	t.Run("below minimum rejected naming flag and minimum", func(t *testing.T) {
		err := serverCommand([]string{"--dev-insecure", "--listen", "127.0.0.1:0", "--db", db,
			"--send-pool-bytes", "1024", "--receive-pool-bytes", "2048"},
			io.Discard, io.Discard)
		if err == nil {
			t.Fatal("serverCommand accepted undersized pools")
		}
		msg := err.Error()
		for _, want := range []string{
			fmt.Sprintf("--send-pool-bytes 1024 is below the minimum %d", 2*minSend),
			fmt.Sprintf("--receive-pool-bytes 2048 is below the minimum %d", 2*minReceive),
			"twice wire.MinBudget",
		} {
			if !strings.Contains(msg, want) {
				t.Errorf("error %q does not contain %q", msg, want)
			}
		}
	})

	t.Run("exact minimum accepted", func(t *testing.T) {
		// 192.0.2.1 is TEST-NET-1, never assigned to an interface, so the
		// bind fails after ConfigureLinks: a pool error would surface here
		// instead of the listen error.
		err := serverCommand([]string{"--behind-proxy", "--listen", "192.0.2.1:8443", "--db", db,
			"--send-pool-bytes", strconv.Itoa(2 * minSend),
			"--receive-pool-bytes", strconv.Itoa(2 * minReceive)},
			io.Discard, io.Discard)
		if err == nil {
			t.Fatal("serverCommand unexpectedly succeeded")
		}
		if msg := err.Error(); strings.Contains(msg, "pool") || strings.Contains(msg, "MinBudget") {
			t.Fatalf("valid pools were rejected: %v", err)
		}
	})
}
