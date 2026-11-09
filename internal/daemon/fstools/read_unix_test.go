//go:build linux || darwin

package fstools_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/daemon/fstools"
	"google.golang.org/protobuf/proto"
)

func TestReadFIFOReturnsSpecialFileErrorPromptly(t *testing.T) {
	cwd := tempDir(t)
	path := filepath.Join(cwd, "pipe")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	type response struct {
		result *circusv1.DeviceResult
		err    error
	}
	done := make(chan response, 1)
	go func() {
		result, err := fstools.NewRead().Run(context.Background(), &circusv1.DeviceCall{
			Tool: &circusv1.DeviceCall_Read{Read: &circusv1.ReadRequest{Path: path, Cwd: cwd}},
		})
		done <- response{result: result, err: err}
	}()
	select {
	case response := <-done:
		if response.err != nil {
			t.Fatal(response.err)
		}
		want := "<tool_use_error>This tool cannot read special files. The file is a FIFO.</tool_use_error>"
		compareText(t, "FIFO error", want, response.result.Output.Content[0].GetText())
	case <-time.After(2 * time.Second):
		t.Fatal("Read blocked opening a FIFO")
	}
}

func TestRangedReadLargeSparseFileStaysBounded(t *testing.T) {
	cwd := tempDir(t)
	path := filepath.Join(cwd, "sparse.bin")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("first\n"); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Truncate(1 << 30); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	started := time.Now()
	result, err := fstools.NewRead().Run(ctx, &circusv1.DeviceCall{
		Tool: &circusv1.DeviceCall_Read{Read: &circusv1.ReadRequest{
			Path: path, Cwd: cwd, Offset: proto.Uint32(1), Limit: 1,
		}},
	})
	elapsed := time.Since(started)
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed >= 5*time.Second {
		t.Fatalf("ranged sparse Read took %s", elapsed)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 64<<20 {
		t.Fatalf("ranged sparse Read allocated %d bytes", allocated)
	}
	// Sparse holes contain NUL, so the binary-file rejection must remain intact.
	if !result.Output.IsError || !strings.Contains(result.Output.Content[0].GetText(), "cannot read binary files") {
		t.Fatalf("unexpected sparse Read result: %v", result)
	}
}
