package fstools_test

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/daemon/fstools"
	"google.golang.org/protobuf/proto"
)

func TestReadInvalidUTF8ProducesSerializableText(t *testing.T) {
	cwd := tempDir(t)
	raw := []byte{'a', 0xe2, 0x82, '\n', 'b'}
	putFile(t, filepath.Join(cwd, "invalid.txt"), raw)
	w := newWorker(t, cwd)
	result := w.call(t, "Read", fixtureInput{Path: "invalid.txt"})
	if result.Output.IsError {
		t.Fatalf("Read failed: %v", result)
	}
	if _, err := proto.Marshal(result); err != nil {
		t.Fatalf("result does not marshal as protobuf: %v", err)
	}
	compareText(t, "invalid UTF-8 replacement", "1\ta\uFFFD\n2\tb", result.Output.Content[0].GetText())
	wantHash := fmt.Sprintf("%x", sha256.Sum256(raw))
	if result.GetRead().Sha256 != wantHash {
		t.Fatalf("Read hash was not computed from raw bytes: got %s, want %s", result.GetRead().Sha256, wantHash)
	}
}

func TestConcurrentEditsDoNotLoseUpdates(t *testing.T) {
	cwd := tempDir(t)
	path := filepath.Join(cwd, "concurrent.txt")
	var initial strings.Builder
	for i := 0; i < 20; i++ {
		fmt.Fprintf(&initial, "line-%02d\n", i)
	}
	putFile(t, path, []byte(initial.String()))

	const count = 20
	start := make(chan struct{})
	errs := make(chan error, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			old := fmt.Sprintf("line-%02d", i)
			result, err := fstools.NewEdit().Run(context.Background(), &circusv1.DeviceCall{
				Tool: &circusv1.DeviceCall_Edit{Edit: &circusv1.EditRequest{
					Path: path, Cwd: cwd, OldString: old, NewString: fmt.Sprintf("edited-%02d", i),
				}},
			})
			if err != nil {
				errs <- err
				return
			}
			if result.Output.IsError {
				errs <- fmt.Errorf("edit %d failed: %s", i, result.Output.Content[0].GetText())
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for i := 0; i < count; i++ {
		if !strings.Contains(text, fmt.Sprintf("edited-%02d\n", i)) {
			t.Errorf("edit %d was lost; final content:\n%s", i, text)
		}
	}
}
