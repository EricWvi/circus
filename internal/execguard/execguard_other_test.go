//go:build !linux && !darwin

package execguard

import (
	"bytes"
	"testing"
)

func TestUnsupportedPlatform(t *testing.T) {
	var stderr bytes.Buffer
	if code := Run([]string{"--parent", "100", "--", "true"}, &stderr); code != ExitUsage {
		t.Fatalf("exit %d, want %d; stderr %q", code, ExitUsage, stderr.String())
	}
}
