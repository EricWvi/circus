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
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/daemon"
)

type fixtureInput struct {
	Path       string  `json:"file_path"`
	Content    string  `json:"content"`
	Old        string  `json:"old_string"`
	New        string  `json:"new_string"`
	ReplaceAll bool    `json:"replace_all"`
	Offset     *uint32 `json:"offset"`
	Limit      uint32  `json:"limit"`
}

type fixture struct {
	Case  string `json:"case"`
	Setup struct {
		Files     map[string]json.RawMessage `json:"files"`
		Dirs      []string                   `json:"dirs"`
		Symlinks  map[string]string          `json:"symlinks"`
		Hardlinks map[string]string          `json:"hardlinks"`
		Modes     map[string]string          `json:"modes"`
		Fifos     []string                   `json:"fifos"`
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
		Result *struct {
			IsError bool   `json:"is_error"`
			Content string `json:"content"`
		} `json:"result"`
	} `json:"steps"`
	FilesAfter map[string]json.RawMessage `json:"files_after"`
	FSAfter    map[string]struct {
		Type        string  `json:"type"`
		LinkTarget  string  `json:"link_target"`
		Mode        string  `json:"mode"`
		SameInodeAs *string `json:"same_inode_as"`
	} `json:"fs_after"`
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
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o666); err != nil {
		t.Fatal(err)
	}
}

func parseMode(t *testing.T, text string) os.FileMode {
	t.Helper()
	bits, err := strconv.ParseUint(text, 8, 12)
	if err != nil || len(text) != 4 {
		t.Fatalf("invalid fixture mode %q: %v", text, err)
	}
	mode := os.FileMode(bits & 0o777)
	for _, special := range []struct {
		bit  uint64
		mode os.FileMode
	}{{0o4000, os.ModeSetuid}, {0o2000, os.ModeSetgid}, {0o1000, os.ModeSticky}} {
		if bits&special.bit != 0 {
			mode |= special.mode
		}
	}
	return mode
}

func verifyFSAfter(t *testing.T, cwd string, f fixture) {
	t.Helper()
	// same_inode_as refers to the first other final path from the setup keys,
	// including keys not explicitly observed in fs_after.
	setupPaths := make(map[string]bool)
	for name := range f.Setup.Files {
		setupPaths[name] = true
	}
	for _, name := range f.Setup.Dirs {
		setupPaths[name] = true
	}
	for name := range f.Setup.Symlinks {
		setupPaths[name] = true
	}
	for name := range f.Setup.Hardlinks {
		setupPaths[name] = true
	}
	for name := range f.Setup.Modes {
		setupPaths[name] = true
	}
	names := make([]string, 0, len(setupPaths))
	for name := range setupPaths {
		names = append(names, name)
	}
	slices.Sort(names)
	final := make(map[string]os.FileInfo)
	for _, name := range names {
		info, err := os.Lstat(filepath.Join(cwd, name))
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		final[name] = info
	}
	for name, want := range f.FSAfter {
		info, err := os.Lstat(filepath.Join(cwd, name))
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		kind := "missing"
		if err == nil {
			switch {
			case info.Mode()&os.ModeSymlink != 0:
				kind = "symlink"
			case info.IsDir():
				kind = "dir"
			case info.Mode().IsRegular():
				kind = "file"
			default:
				t.Fatalf("unsupported file type at %s: %v", name, info.Mode())
			}
		}
		compareText(t, "fs_after/"+name+" type", want.Type, kind)
		if info != nil {
			got := info.Mode() & (os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky)
			if want.Mode == "" || got != parseMode(t, want.Mode) {
				t.Errorf("fs_after/%s mode: expected %s, got %v", name, want.Mode, got)
			}
		} else if want.Mode != "" {
			t.Errorf("fs_after/%s: missing file has expected mode %s", name, want.Mode)
		}
		if kind == "symlink" {
			target, err := os.Readlink(filepath.Join(cwd, name))
			if err != nil {
				t.Fatal(err)
			}
			compareText(t, "fs_after/"+name+" link_target", want.LinkTarget, target)
		}
		same := ""
		if info != nil {
			for _, other := range names {
				if other != name && final[other] != nil && os.SameFile(info, final[other]) {
					same = other
					break
				}
			}
		}
		expected := ""
		if want.SameInodeAs != nil {
			expected = *want.SameInodeAs
		}
		compareText(t, "fs_after/"+name+" same_inode_as (empty means null)", expected, same)
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
					if err := os.MkdirAll(filepath.Join(cwd, dir), 0o777); err != nil {
						t.Fatal(err)
					}
				}
				for name, raw := range f.Setup.Files {
					putFile(t, filepath.Join(cwd, name), fixtureBytes(t, raw))
				}
				for _, name := range f.Setup.Fifos {
					if err := syscall.Mkfifo(filepath.Join(cwd, name), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				for name, target := range f.Setup.Symlinks {
					if err := os.MkdirAll(filepath.Dir(filepath.Join(cwd, name)), 0o777); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(target, filepath.Join(cwd, name)); err != nil {
						t.Fatal(err)
					}
				}
				for name, target := range f.Setup.Hardlinks {
					if err := os.MkdirAll(filepath.Dir(filepath.Join(cwd, name)), 0o777); err != nil {
						t.Fatal(err)
					}
					if err := os.Link(filepath.Join(cwd, target), filepath.Join(cwd, name)); err != nil {
						t.Fatal(err)
					}
				}
				for name, mode := range f.Setup.Modes {
					if err := os.Chmod(filepath.Join(cwd, name), parseMode(t, mode)); err != nil {
						t.Fatal(err)
					}
				}
				w := newWorker(t, cwd)
				for i, step := range f.Steps {
					steps++
					if step.Before != nil {
						putFile(t, step.Before.Write.Path, fixtureBytes(t, step.Before.Write.Content))
					}
					if step.Result == nil {
						// Deliberate deviation: native blocks forever on this FIFO;
						// circus rejects the special file without opening it.
						if f.Case != "read-fifo" {
							t.Fatalf("step %d unexpectedly has no native result", i+1)
						}
						r := w.call(t, step.Tool, step.Input)
						if !r.Output.IsError {
							t.Errorf("step %d should reject the FIFO: %v", i+1, r)
						}
						compareText(t, fmt.Sprintf("step %d special-file error", i+1), "<tool_use_error>This tool cannot read special files. The file is a FIFO.</tool_use_error>", r.Output.Content[0].GetText())
						continue
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
					// files_after records regular files without following symlinks
					// or attempting to read special files.
					if entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
						return nil
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
				verifyFSAfter(t, cwd, f)
			})
		}
	}
	t.Logf("Native fixtures exercised: %d fixtures, %d tool steps (none skipped)", count, steps)
}
