package fstools_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/daemon/fstools"
	"github.com/EricWvi/circus/internal/textutil"
)

func editCall(path, old, replacement string, replaceAll bool) *circusv1.DeviceCall {
	return &circusv1.DeviceCall{Tool: &circusv1.DeviceCall_Edit{Edit: &circusv1.EditRequest{
		Path: path, OldString: old, NewString: replacement, ReplaceAll: replaceAll,
	}}}
}

func writeRepeatedByte(t *testing.T, path string, size int) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	chunk := bytes.Repeat([]byte{'a'}, 1<<20)
	for remaining := size; remaining > 0; {
		n := min(remaining, len(chunk))
		if _, err := f.Write(chunk[:n]); err != nil {
			_ = f.Close()
			t.Fatal(err)
		}
		remaining -= n
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func writeUTF16EditFile(t *testing.T, path string, size int) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	prefix := []byte{0xff, 0xfe, 'n', 0, 'e', 0, 'e', 0, 'd', 0, 'l', 0, 'e', 0}
	if _, err := f.Write(prefix); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	chunk := bytes.Repeat([]byte{'a', 0}, 1<<19)
	for remaining := size - len(prefix); remaining > 0; {
		n := min(remaining, len(chunk))
		if _, err := f.Write(chunk[:n]); err != nil {
			_ = f.Close()
			t.Fatal(err)
		}
		remaining -= n
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestEditReplaceAllResultSizeLimit(t *testing.T) {
	path := filepath.Join(tempDir(t), "expand.txt")
	original := strings.Repeat("a", 1<<20)
	putFile(t, path, []byte(original))
	result, err := fstools.NewEdit().Run(context.Background(), editCall(path, "a", strings.Repeat("b", 4<<10), true))
	if err != nil {
		t.Fatal(err)
	}
	if !result.GetOutput().GetIsError() || !strings.Contains(result.GetOutput().GetContent()[0].GetText(), "cannot modify files larger than 256 MiB") {
		t.Fatalf("expanded result was not rejected: %v", result)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != original {
		t.Fatal("rejected edit modified the file")
	}
}

func TestEditEncodedResultSizeLimit(t *testing.T) {
	if path := os.Getenv("CIRCUS_EDIT_SIZE_FILE"); path != "" {
		original, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		result, err := fstools.NewEdit().Run(context.Background(), editCall(path, "a", strings.Repeat("\n", 4096), true))
		if err != nil {
			t.Fatal(err)
		}
		if !result.GetOutput().GetIsError() || !strings.Contains(result.GetOutput().GetContent()[0].GetText(), "cannot modify files larger than 256 MiB") {
			t.Fatalf("expanded on-disk result was not rejected: %v", result)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(data, original) {
			t.Fatal("rejected edit modified the file")
		}
		return
	}

	for _, test := range []struct {
		name     string
		encoding textutil.Encoding
	}{
		{name: "UTF-8 CRLF", encoding: textutil.EncodingUTF8},
		{name: "UTF-16LE CRLF", encoding: textutil.EncodingUTF16LE},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(tempDir(t), "expand.txt")
			originalText := "\r\n" + strings.Repeat("a", 65535)
			original := textutil.Decoded{Encoding: test.encoding}.Encode(originalText)
			putFile(t, path, original)
			binary, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, binary, "-test.run=^TestEditEncodedResultSizeLimit$")
			cmd.Env = append(os.Environ(), "CIRCUS_EDIT_SIZE_FILE="+path)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("size-check subprocess failed: %v\n%s", err, output)
			}
		})
	}
}

func TestEditUnmatchedMemoryBound(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping 250 MiB memory regression in short mode")
	}
	const fileSize = 250 << 20
	if path := os.Getenv("CIRCUS_EDIT_MEMORY_FILE"); path != "" {
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		result, err := fstools.NewEdit().Run(context.Background(), editCall(path, "\"not-present", "replacement", false))
		runtime.ReadMemStats(&after)
		if err != nil {
			t.Fatal(err)
		}
		if !result.GetOutput().GetIsError() || !strings.Contains(result.GetOutput().GetContent()[0].GetText(), "String to replace not found") {
			t.Fatalf("unexpected unmatched edit result: %v", result)
		}
		growth := after.TotalAlloc - before.TotalAlloc
		// Leave headroom for race-detector allocation accounting while still
		// rejecting the old whole-file rune conversion and builder copies.
		if growth > 5*fileSize {
			t.Fatalf("unmatched edit allocated %d bytes for a %d-byte file", growth, fileSize)
		}
		t.Logf("250 MiB unmatched edit: allocated %d bytes", growth)
		return
	}

	path := filepath.Join(tempDir(t), "large.txt")
	writeRepeatedByte(t, path, fileSize)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "-test.run=^TestEditUnmatchedMemoryBound$", "-test.v")
	cmd.Env = append(os.Environ(), "CIRCUS_EDIT_MEMORY_FILE="+path)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("memory measurement subprocess failed: %v\n%s", err, output)
	} else {
		t.Logf("memory measurement subprocess:\n%s", output)
	}
}

func TestEditUTF16MemoryBound(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping 200 MiB UTF-16 memory regression in short mode")
	}
	const fileSize = 200 << 20
	if path := os.Getenv("CIRCUS_EDIT_UTF16_MEMORY_FILE"); path != "" {
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		result, err := fstools.NewEdit().Run(context.Background(), editCall(path, "needle", "replacement", false))
		runtime.ReadMemStats(&after)
		if err != nil {
			t.Fatal(err)
		}
		if result.GetOutput().GetIsError() {
			t.Fatalf("UTF-16 edit failed: %v", result)
		}
		growth := after.TotalAlloc - before.TotalAlloc
		if growth > 6*fileSize {
			t.Fatalf("UTF-16 edit allocated %d bytes for a %d-byte file", growth, fileSize)
		}
		t.Logf("200 MiB UTF-16 edit: allocated %d bytes", growth)
		return
	}

	path := filepath.Join(tempDir(t), "large-utf16.txt")
	writeUTF16EditFile(t, path, fileSize)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "-test.run=^TestEditUTF16MemoryBound$", "-test.v")
	cmd.Env = append(os.Environ(), "CIRCUS_EDIT_UTF16_MEMORY_FILE="+path)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("UTF-16 memory measurement subprocess failed: %v\n%s", err, output)
	} else {
		t.Logf("UTF-16 memory measurement subprocess:\n%s", output)
	}
}

func TestEditUnmatchedLongOldStringIsPromptAndReleasesPath(t *testing.T) {
	path := filepath.Join(tempDir(t), "long.txt")
	writeRepeatedByte(t, path, 1<<20)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	started := time.Now()
	result, err := fstools.NewEdit().Run(ctx, editCall(path, strings.Repeat("a", 20000)+"b", "replacement", false))
	if err != nil {
		t.Fatal(err)
	}
	if !result.GetOutput().GetIsError() || !strings.Contains(result.GetOutput().GetContent()[0].GetText(), "String to replace not found") {
		t.Fatalf("unexpected unmatched edit result: %v", result)
	}
	if elapsed := time.Since(started); elapsed >= 2*time.Second {
		t.Fatalf("unmatched edit took %s", elapsed)
	}

	writeCtx, stopWrite := context.WithTimeout(context.Background(), 2*time.Second)
	defer stopWrite()
	writeResult, err := fstools.NewWrite().Run(writeCtx, &circusv1.DeviceCall{Tool: &circusv1.DeviceCall_Write{Write: &circusv1.WriteRequest{
		Path: path, Content: "written",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if writeResult.GetOutput().GetIsError() {
		t.Fatalf("following Write was blocked or failed: %v", writeResult)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "written" {
		t.Fatalf("following Write did not replace the file: %q", data)
	}
}
