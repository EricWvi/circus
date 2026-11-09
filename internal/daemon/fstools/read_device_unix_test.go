//go:build linux || darwin

package fstools_test

import (
	"strings"
	"testing"
)

func TestReadLiteralReplacementCharacterHasNoEncodingNotice(t *testing.T) {
	cwd := tempDir(t)
	putFile(t, cwd+"/a.txt", []byte("before\uFFFDafter\n"))
	w := newWorker(t, cwd)
	r := w.call(t, "Read", fixtureInput{Path: "a.txt"})
	if r.Output.IsError {
		t.Fatalf("literal U+FFFD read failed: %v", r)
	}
	compareText(t, "literal U+FFFD read", "1\tbefore\uFFFDafter\n2", r.Output.Content[0].GetText())
}

func TestReadZeroDeviceUsesSizeGuard(t *testing.T) {
	w := newWorker(t, tempDir(t))
	r := w.call(t, "Read", fixtureInput{Path: "/dev/zero"})
	if !r.Output.IsError || !strings.HasPrefix(r.Output.Content[0].GetText(), "File content (256KB) exceeds maximum allowed size (256KB).") {
		t.Fatalf("/dev/zero did not hit the size guard: %v", r)
	}
}
