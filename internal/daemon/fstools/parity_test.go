package fstools_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/daemon"
)

type fixtureInput struct {
	Path       string `json:"file_path"`
	Content    string `json:"content"`
	Old        string `json:"old_string"`
	New        string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all"`
	Offset     uint32 `json:"offset"`
	Limit      uint32 `json:"limit"`
}

type fixture struct {
	Case  string `json:"case"`
	Setup struct {
		Files map[string]json.RawMessage `json:"files"`
		Dirs  []string                   `json:"dirs"`
	} `json:"setup"`
	Steps []struct {
		Tool   string       `json:"tool"`
		Input  fixtureInput `json:"input"`
		Before *struct {
			Write struct {
				Path    string          `json:"path"`
				Content json.RawMessage `json:"content"`
			} `json:"write"`
		} `json:"before"`
		Result struct {
			IsError bool   `json:"is_error"`
			Content string `json:"content"`
		} `json:"result"`
	} `json:"steps"`
	FilesAfter map[string]json.RawMessage `json:"files_after"`
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("cannot locate repository go.mod")
		}
		dir = parent
	}
}

func tempDir(t *testing.T) string {
	t.Helper()
	base := filepath.Join(repoRoot(t), ".tmp")
	if err := os.MkdirAll(base, 0755); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(base, "fstools-test-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	return dir
}

func fixtureBytes(t *testing.T, raw json.RawMessage) []byte {
	t.Helper()
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return []byte(text)
	}
	var binary struct {
		Base64 *string `json:"base64"`
	}
	if err := json.Unmarshal(raw, &binary); err != nil || binary.Base64 == nil {
		t.Fatalf("invalid file content: %s", raw)
	}
	data, err := base64.StdEncoding.DecodeString(*binary.Base64)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func putFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

// simulatedWorker owns session read state; the production dispatcher and tools
// never retain it. Paths are resolved before using them as read-state keys.
type simulatedWorker struct {
	d     *daemon.Dispatcher
	cwd   string
	reads map[string]string
	next  int
}

func newWorker(t *testing.T, cwd string) *simulatedWorker {
	t.Helper()
	d, err := daemon.NewDispatcher(daemon.Options{Tools: daemon.DefaultTools(nil)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := d.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	return &simulatedWorker{d: d, cwd: cwd, reads: make(map[string]string)}
}

func (w *simulatedWorker) call(t *testing.T, tool string, input fixtureInput) *circusv1.DeviceResult {
	t.Helper()
	path := input.Path
	if !filepath.IsAbs(path) {
		path = filepath.Join(w.cwd, path)
	}
	var expected *string
	if sha, ok := w.reads[filepath.Clean(path)]; ok {
		expected = &sha
	}
	w.next++
	call := &circusv1.DeviceCall{CallId: fmt.Sprintf("fixture-%d", w.next), SessionId: "fixture-session"}
	switch tool {
	case "Read":
		call.Tool = &circusv1.DeviceCall_Read{Read: &circusv1.ReadRequest{Path: input.Path, Cwd: w.cwd, Offset: input.Offset, Limit: input.Limit}}
	case "Write":
		call.Tool = &circusv1.DeviceCall_Write{Write: &circusv1.WriteRequest{Path: input.Path, Cwd: w.cwd, Content: input.Content, ExpectedSha256: expected}}
	case "Edit":
		call.Tool = &circusv1.DeviceCall_Edit{Edit: &circusv1.EditRequest{Path: input.Path, Cwd: w.cwd, OldString: input.Old, NewString: input.New, ReplaceAll: input.ReplaceAll, ExpectedSha256: expected}}
	default:
		t.Fatalf("unsupported fixture tool %q", tool)
	}
	if _, err := w.d.Submit(call); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	status, err := w.d.Wait(ctx, call.CallId)
	if err != nil {
		t.Fatal(err)
	}
	if status.GetState() != circusv1.CallStatus_STATE_COMPLETED || status.GetResult() == nil {
		t.Fatalf("call did not complete: %v", status)
	}
	result := status.Result
	if result.GetCallId() != call.CallId || len(result.GetOutput().GetContent()) != 1 {
		t.Fatalf("invalid result envelope: %v", result)
	}
	if _, ok := result.Output.Content[0].Block.(*circusv1.ContentBlock_Text); !ok {
		t.Fatalf("expected one text block: %v", result)
	}
	if !result.Output.IsError && result.GetRead() != nil {
		w.reads[filepath.Clean(path)] = result.GetRead().Sha256
	}
	return result
}

var tokenSuffix = regexp.MustCompile(`\n\n<system-reminder>\n<total_tokens>[0-9]+ tokens left</total_tokens>\n</system-reminder>$`)

func compareText(t *testing.T, label, want, got string) {
	t.Helper()
	if want == got {
		return
	}
	// Quoted strings make tabs, CRLF and trailing newlines visible. The first
	// differing byte locates small errors even in the long-file fixtures.
	i := 0
	for i < len(want) && i < len(got) && want[i] == got[i] {
		i++
	}
	t.Errorf("%s differs at byte %d\nexpected: %q\n     got: %q", label, i, want, got)
}

func TestNativeParity(t *testing.T) {
	root := filepath.Join(repoRoot(t), "tests", "parity", "fixtures", "native")
	count, steps := 0, 0
	for _, tool := range []string{"Read", "Write", "Edit"} {
		paths, err := filepath.Glob(filepath.Join(root, tool, "*.json"))
		if err != nil || len(paths) == 0 {
			t.Fatalf("no %s fixtures: %v", tool, err)
		}
		for _, path := range paths {
			count++
			t.Run(tool+"/"+strings.TrimSuffix(filepath.Base(path), ".json"), func(t *testing.T) {
				cwd := tempDir(t)
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				// Substitute inside JSON strings so escaping remains valid on every path.
				encodedCwd, _ := json.Marshal(cwd)
				raw = []byte(strings.ReplaceAll(string(raw), "{{CWD}}", string(encodedCwd[1:len(encodedCwd)-1])))
				var f fixture
				if err := json.Unmarshal(raw, &f); err != nil {
					t.Fatal(err)
				}
				for _, dir := range f.Setup.Dirs {
					if err := os.MkdirAll(filepath.Join(cwd, dir), 0755); err != nil {
						t.Fatal(err)
					}
				}
				for name, raw := range f.Setup.Files {
					putFile(t, filepath.Join(cwd, name), fixtureBytes(t, raw))
				}
				w := newWorker(t, cwd)
				for i, step := range f.Steps {
					steps++
					if step.Before != nil {
						putFile(t, step.Before.Write.Path, fixtureBytes(t, step.Before.Write.Content))
					}
					r := w.call(t, step.Tool, step.Input)
					if r.Output.IsError != step.Result.IsError {
						t.Errorf("step %d is_error: expected %v, got %v", i+1, step.Result.IsError, r.Output.IsError)
					}
					if !tokenSuffix.MatchString(step.Result.Content) {
						t.Fatalf("step %d has no exact CLI token suffix", i+1)
					}
					want := tokenSuffix.ReplaceAllString(step.Result.Content, "")
					compareText(t, fmt.Sprintf("step %d body", i+1), want, r.Output.Content[0].GetText())
				}
				seen := make(map[string]bool)
				err = filepath.WalkDir(cwd, func(path string, entry fs.DirEntry, err error) error {
					if err != nil || entry.IsDir() {
						return err
					}
					name, err := filepath.Rel(cwd, path)
					if err != nil {
						return err
					}
					name = filepath.ToSlash(name)
					expected, ok := f.FilesAfter[name]
					if !ok {
						t.Errorf("unexpected file after steps: %s", name)
						return nil
					}
					seen[name] = true
					got, err := os.ReadFile(path)
					if err != nil {
						return err
					}
					compareText(t, "files_after/"+name, string(fixtureBytes(t, expected)), string(got))
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				for name := range f.FilesAfter {
					if !seen[name] {
						t.Errorf("missing file after steps: %s", name)
					}
				}
			})
		}
	}
	t.Logf("Native fixtures exercised: %d fixtures, %d tool steps (none skipped)", count, steps)
}
