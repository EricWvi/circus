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
	"github.com/EricWvi/circus/internal/textutil"
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
	compareText(t, "invalid UTF-8 replacement", "1\ta\uFFFD\n2\tb\n\n[This file is not valid UTF-8: it uses a legacy encoding such as Windows-1252, Shift-JIS or GBK, or it is binary. Its undecodable bytes show above as U+FFFD. Edit and Write will refuse it: change it with a shell command that keeps its encoding, or ask the user whether to convert it to UTF-8.]", result.Output.Content[0].GetText())
	wantHash := fmt.Sprintf("%x", sha256.Sum256(raw))
	if result.GetRead().Sha256 != wantHash {
		t.Fatalf("Read hash was not computed from raw bytes: got %s, want %s", result.GetRead().Sha256, wantHash)
	}
}

func TestUTF16LEBOMOnlyReadMatchesDefaultAndRanged(t *testing.T) {
	cwd := tempDir(t)
	raw := []byte{0xff, 0xfe}
	putFile(t, filepath.Join(cwd, "bom.txt"), raw)
	for _, test := range []struct {
		name  string
		input fixtureInput
	}{
		{name: "default", input: fixtureInput{Path: "bom.txt"}},
		{name: "ranged", input: fixtureInput{Path: "bom.txt", Limit: 1}},
	} {
		t.Run(test.name, func(t *testing.T) {
			w := newWorker(t, cwd)
			result := w.call(t, "Read", test.input)
			if result.Output.IsError {
				t.Fatalf("Read failed: %v", result)
			}
			compareText(t, "BOM-only body", "1", result.Output.Content[0].GetText())
			if result.GetRead().TotalLines != 1 || result.GetRead().Sha256 != fmt.Sprintf("%x", sha256.Sum256(raw)) {
				t.Fatalf("BOM-only metadata: %v", result.GetRead())
			}
		})
	}
}

func TestRangedUTF16LEReadStreamsDecodedText(t *testing.T) {
	cwd := tempDir(t)
	raw := textutil.Decoded{Encoding: textutil.EncodingUTF16LE}.Encode("first\r\nβ😀\n")
	putFile(t, filepath.Join(cwd, "utf16.txt"), raw)
	w := newWorker(t, cwd)
	result := w.call(t, "Read", fixtureInput{Path: "utf16.txt", Offset: proto.Uint32(2), Limit: 1})
	if result.Output.IsError {
		t.Fatalf("ranged UTF-16LE Read failed: %v", result)
	}
	compareText(t, "ranged UTF-16LE body", "2\tβ😀", result.Output.Content[0].GetText())
	if result.GetRead().TotalLines != 3 || result.GetRead().Sha256 != fmt.Sprintf("%x", sha256.Sum256(raw)) {
		t.Fatalf("ranged UTF-16LE metadata: %v", result.GetRead())
	}
}

func TestConcurrentEditsResolveSymlinkedParents(t *testing.T) {
	cwd := tempDir(t)
	if err := os.Mkdir(filepath.Join(cwd, "w"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("w", filepath.Join(cwd, "walias")); err != nil {
		t.Fatal(err)
	}
	realPath := filepath.Join(cwd, "w", "big.txt")
	aliasPath := filepath.Join(cwd, "walias", "big.txt")
	var initial strings.Builder
	for i := 0; i < 20; i++ {
		fmt.Fprintf(&initial, "line-%02d\n", i)
	}
	putFile(t, realPath, []byte(initial.String()))

	const count = 20
	start := make(chan struct{})
	errs := make(chan error, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			path := realPath
			if i%2 == 1 {
				path = aliasPath
			}
			result, err := fstools.NewEdit().Run(context.Background(), &circusv1.DeviceCall{
				Tool: &circusv1.DeviceCall_Edit{Edit: &circusv1.EditRequest{
					Path: path, Cwd: cwd, OldString: fmt.Sprintf("line-%02d", i), NewString: fmt.Sprintf("edited-%02d", i),
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

	data, err := os.ReadFile(realPath)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < count; i++ {
		if !strings.Contains(string(data), fmt.Sprintf("edited-%02d\n", i)) {
			t.Errorf("edit %d through symlinked parent was lost; final content:\n%s", i, data)
		}
	}
}

func TestMalformedUTF16LEReadOnlyAndRefuseWrites(t *testing.T) {
	tests := []struct {
		name string
		raw  []byte
	}{
		{name: "unpaired surrogate", raw: []byte{0xff, 0xfe, 'h', 0, 'i', 0, 0x00, 0xd8, '\n', 0}},
		{name: "trailing odd byte", raw: []byte{0xff, 0xfe, 'h', 0, 'i', 0, '\n', 0, 0x5a}},
	}
	for _, test := range tests {
		for _, tool := range []string{"Edit", "Write"} {
			t.Run(test.name+"/"+tool, func(t *testing.T) {
				cwd := tempDir(t)
				path := filepath.Join(cwd, "a.txt")
				putFile(t, path, test.raw)
				w := newWorker(t, cwd)
				read := w.call(t, "Read", fixtureInput{Path: "a.txt"})
				if read.Output.IsError || !strings.Contains(read.Output.Content[0].GetText(), "\uFFFD") {
					t.Fatalf("Read should preserve replacement-character display: %v", read)
				}
				input := fixtureInput{Path: "a.txt", Old: "hi", New: "HI", Content: "replacement\n"}
				result := w.call(t, tool, input)
				if !result.Output.IsError || !strings.Contains(result.Output.Content[0].GetText(), "File is not valid UTF-8:") {
					t.Fatalf("%s should refuse malformed UTF-16LE: %v", tool, result)
				}
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if string(data) != string(test.raw) {
					t.Fatalf("%s rewrote malformed UTF-16LE: expected %v, got %v", tool, test.raw, data)
				}
			})
		}
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
