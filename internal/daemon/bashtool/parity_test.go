package bashtool_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/daemon"
	"github.com/EricWvi/circus/internal/daemon/bashtool"
	"github.com/EricWvi/circus/internal/daemon/fstools"
	"github.com/EricWvi/circus/internal/daemon/jobs"
	"github.com/EricWvi/circus/internal/testutil/leakcheck"
	"sync/atomic"
)

type fixture struct {
	Case  string `json:"case"`
	Setup struct {
		Files     map[string]json.RawMessage `json:"files"`
		HomeFiles map[string]json.RawMessage `json:"home_files"`
		Dirs      []string                   `json:"dirs"`
	} `json:"setup"`
	Steps []struct {
		Tool  string `json:"tool"`
		Input struct {
			Command    string `json:"command"`
			Background bool   `json:"run_in_background"`
			Timeout    uint32 `json:"timeout"`
			TaskID     string `json:"task_id"`
			Path       string `json:"file_path"`
		} `json:"input"`
		Before *struct {
			DelayMS int `json:"delay_ms"`
			Remove  *struct {
				Path string `json:"path"`
			} `json:"remove"`
			Write *struct {
				Path    string          `json:"path"`
				Content json.RawMessage `json:"content"`
			} `json:"write"`
		} `json:"before"`
		Result struct {
			IsError bool   `json:"is_error"`
			Content string `json:"content"`
		} `json:"result"`
	} `json:"steps"`
}

func tempDir(t *testing.T) string {
	t.Helper()
	base, err := filepath.Abs("../../../.tmp")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(base, 0700); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(base, "bashtool-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	// Runs before the removal above and after cleanups registered later.
	leakcheck.Track(t, dir)
	return dir
}

func putFile(t *testing.T, path string, raw json.RawMessage) {
	t.Helper()
	var value string
	var data []byte
	if err := json.Unmarshal(raw, &value); err == nil {
		data = []byte(value)
	} else {
		var binary struct {
			Base64 string `json:"base64"`
		}
		if err = json.Unmarshal(raw, &binary); err != nil {
			t.Fatal(err)
		}
		var err error
		data, err = base64.StdEncoding.DecodeString(binary.Base64)
		if err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, path, data)
}
func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

type worker struct {
	d                                             *daemon.Dispatcher
	jobs                                          *jobs.Manager
	events                                        chan *circusv1.JobEvent
	bash                                          *bashtool.Bash
	options                                       bashtool.Options
	root, cwd, start, jobID, outputPath, snapshot string
	next                                          int
}

func newWorker(t *testing.T) *worker {
	t.Helper()
	return newWorkerWithOptions(t, nil)
}

func newWorkerWithOptions(t *testing.T, configure func(*bashtool.Options)) *worker {
	t.Helper()
	root := tempDir(t)
	w := &worker{root: root, cwd: filepath.Join(root, "project"), events: make(chan *circusv1.JobEvent, 32)}
	w.start = w.cwd
	w.options = bashtool.Options{Home: filepath.Join(root, "home"), StateDir: filepath.Join(root, "state"), TempDir: filepath.Join(root, "tmp"), Shell: "/bin/bash"}
	for _, p := range []string{w.cwd, w.options.Home, w.options.StateDir, w.options.TempDir} {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if configure != nil {
		configure(&w.options)
	}
	leakcheck.OutputRoot(t, filepath.Join(w.options.TempDir, fmt.Sprintf("circus-%d", os.Getuid())))
	leakcheck.StateDir(t, w.options.StateDir)
	w.jobs = jobs.NewManager(jobs.Options{OnEvent: func(e *circusv1.JobEvent) { w.events <- e }})
	manager := w.jobs
	w.bash = bashtool.NewBash(manager, w.options)
	w.d = newAuditedDispatcher(t, w.bash, bashtool.NewTaskStop(manager), fstools.NewRead())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := w.d.Shutdown(ctx); err != nil {
			t.Error(err)
		}
		if err := w.jobs.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	return w
}

// newAuditedDispatcher counts executions per call_id and call durations.
func newAuditedDispatcher(t *testing.T, tools ...daemon.Tool) *daemon.Dispatcher {
	t.Helper()
	d, err := daemon.NewDispatcher(daemon.Options{Tools: leakcheck.AuditTools(t, nil, tools)})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func (w *worker) expand(s string) string {
	return strings.NewReplacer("{{CWD}}", w.start, "{{HOME}}", w.options.Home, "{{TMPDIR}}", w.options.TempDir, "{{SNAPSHOT_PATH}}", w.snapshot, "{{TASK_ID}}", w.jobID, "{{OUTPUT_PATH}}", w.outputPath).Replace(s)
}
func (w *worker) normalize(s string) string {
	for _, p := range []struct{ value, placeholder string }{{w.outputPath, "{{OUTPUT_PATH}}"}, {w.snapshot, "{{SNAPSHOT_PATH}}"}, {w.start, "{{CWD}}"}, {w.options.Home, "{{HOME}}"}, {w.options.TempDir, "{{TMPDIR}}"}, {w.jobID, "{{TASK_ID}}"}} {
		if p.value != "" {
			s = strings.ReplaceAll(s, p.value, p.placeholder)
		}
	}
	return s
}
func (w *worker) call(t *testing.T, call *circusv1.DeviceCall) *circusv1.DeviceResult {
	t.Helper()
	w.next++
	call.CallId = fmt.Sprintf("fixture-%d", w.next)
	if call.SessionId == "" {
		call.SessionId = "fixture-session"
	}
	if _, err := w.d.Submit(call); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	status, err := w.d.Wait(ctx, call.CallId)
	if err != nil {
		t.Fatal(err)
	}
	r := status.GetResult()
	if status.State != circusv1.CallStatus_STATE_COMPLETED || r == nil || len(r.GetOutput().GetContent()) != 1 {
		t.Fatalf("invalid result: %v", status)
	}
	if _, ok := r.Output.Content[0].Block.(*circusv1.ContentBlock_Text); !ok {
		t.Fatalf("expected text: %v", r)
	}
	if b := r.GetBash(); b != nil {
		// Announced output files are the only ones allowed to remain.
		leakcheck.Expect(b.OutputPath)
		w.cwd = b.Cwd
		if b.JobId != "" {
			w.jobID = b.JobId
		}
		if b.OutputPath != "" {
			w.outputPath = b.OutputPath
		}
	}
	paths, err := filepath.Glob(filepath.Join(w.options.StateDir, "instance-*", "snapshot-*", "snapshot.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) > 0 {
		w.snapshot = paths[len(paths)-1]
	}
	return r
}
func (w *worker) bashCall(t *testing.T, command string, background bool, timeout uint32) *circusv1.DeviceResult {
	t.Helper()
	return w.call(t, &circusv1.DeviceCall{TimeoutMs: timeout, Tool: &circusv1.DeviceCall_Bash{Bash: &circusv1.BashRequest{Command: command, Cwd: w.cwd, WorkingDirs: []string{w.start}, RunInBackground: background}}})
}
func (w *worker) waitJob(t *testing.T, id string) *circusv1.JobEvent {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	for {
		select {
		case e := <-w.events:
			if e.JobId == id && e.State != circusv1.JobEvent_STATE_RUNNING {
				return e
			}
		case <-ctx.Done():
			t.Fatal("job did not finish")
			return nil
		}
	}
}

// Strip exactly the three CLI-owned reminder block kinds. Other reminders,
// including Read warnings, remain part of the expected tool body.
var reminderBlock = regexp.MustCompile(`(?s)\n\n<system-reminder>.*?</system-reminder>`)

func stripCLI(s string) string {
	return reminderBlock.ReplaceAllStringFunc(s, func(block string) string {
		if strings.Contains(block, "<total_tokens>") || strings.Contains(block, "# Environment update") || strings.Contains(block, "<task-notification>") {
			return ""
		}
		return block
	})
}

type semanticCheck struct {
	reason string
	check  func(*testing.T, string)
}

func containsAll(t *testing.T, body string, values ...string) {
	t.Helper()
	for _, v := range values {
		if !strings.Contains(body, v) {
			t.Errorf("missing %q in %q", v, body)
		}
	}
}
func absentAll(t *testing.T, body string, values ...string) {
	t.Helper()
	for _, v := range values {
		if strings.Contains(body, v) {
			t.Errorf("unexpected %q in %q", v, body)
		}
	}
}

// Byte-comparison exceptions, kept in one table. The explicit M1 design
// reconstructs the terminal via an interactive collector and captures exports;
// native's SDK-specific exports and internal snapshot text are not the contract.
var fixtureExceptions = map[string]semanticCheck{
	"bash-env/0": {"host exports and SDK-only variables differ", func(t *testing.T, s string) {
		containsAll(t, s, "CLAUDECODE=1\n", "GIT_EDITOR=true\n", "COREPACK_ENABLE_AUTO_PIN=0\n", "HOME={{HOME}}\n", "PWD={{CWD}}\n", "SHELL=/bin/bash\n")
		absentAll(t, s, "CLAUDE_CODE_EXECPATH=", "CLAUDE_CODE_SESSION_ID=", "CLAUDE_CODE_MESSAGING_SOCKET=")
	}},
	"bash-shell-info/0": {"ps arguments, PIDs and shopt inventory depend on host and snapshot", func(t *testing.T, s string) {
		containsAll(t, s, "/bin/bash|/bin/bash|", "/bin/bash -c source ", "eval '", "shopt -u extglob", "shopt -s expand_aliases")
		first := strings.SplitN(s, "\n", 2)[0]
		if strings.Contains(strings.TrimPrefix(first, "/bin/bash|/bin/bash|"), "i") {
			t.Errorf("executing shell is interactive: %s", first)
		}
	}},
	"bash-shell-info/1": {"snapshot implementation omits native binary shims", func(t *testing.T, s string) {
		containsAll(t, s, "# Snapshot file", "unalias -a", "shopt -s expand_aliases", "set +o")
		absentAll(t, s, "CLAUDE_CODE_EXECPATH", "function pkill", "function find", "function grep")
	}},
	"bash-profile-sourcing/0": {"interactive terminal reconstruction preserves profile exports and bashrc PATH", func(t *testing.T, s string) {
		containsAll(t, s, "MARK_BASHRC=1\n", "MARK_BASH_PROFILE=1\n", "MARK_INTERACTIVE=1\n", "local=[1]", "{{HOME}}/bin-bashrc")
		absentAll(t, s, "MARK_BASH_LOGIN=", "MARK_PROFILE=")
		match := regexp.MustCompile(`MARK_BASHRC_FLAGS=([^\n]+)`).FindStringSubmatch(s)
		if len(match) != 2 || !strings.Contains(match[1], "i") {
			t.Errorf("collector should be interactive: %q", s)
		}
	}},
	"bash-profile-sourcing/2": {"snapshot text and options are implementation-specific", func(t *testing.T, s string) {
		containsAll(t, s, "unalias -a", "fn_mark ()", "LOCAL_BASHRC=", "ll_mark=", "source_bash_profile_mark=", "snapshot_flags_mark=")
		absentAll(t, s, "source_bash_login_mark=", "source_profile_mark=", "CLAUDE_CODE_EXECPATH")
	}},
}

func TestNativeParity(t *testing.T) {
	count := 0
	var steps atomic.Int32
	for _, tool := range []string{"Bash", "TaskStop"} {
		paths, err := filepath.Glob(filepath.Join("../../../tests/parity/fixtures/native", tool, "*.json"))
		if err != nil || len(paths) == 0 {
			t.Fatalf("fixtures: %v", err)
		}
		for _, path := range paths {
			count++
			t.Run(tool+"/"+strings.TrimSuffix(filepath.Base(path), ".json"), func(t *testing.T) {
				if filepath.Base(path) != "bash-background-child-foreground.json" {
					t.Parallel()
				}
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var f fixture
				if err = json.Unmarshal(raw, &f); err != nil {
					t.Fatal(err)
				}
				w := newWorkerWithOptions(t, func(o *bashtool.Options) {
					for name, data := range f.Setup.HomeFiles {
						putFile(t, filepath.Join(o.Home, name), data)
					}
				})
				for _, dir := range f.Setup.Dirs {
					if err = os.MkdirAll(filepath.Join(w.start, dir), 0700); err != nil {
						t.Fatal(err)
					}
				}
				for name, data := range f.Setup.Files {
					putFile(t, filepath.Join(w.start, name), data)
				}
				var probeSleeps map[string]string
				for i, step := range f.Steps {
					if f.Case == "bash-amp-child-primary" && step.Before != nil && step.Before.DelayMS == 22000 {
						t.Run("natural-completion-wait", func(t *testing.T) {
							t.Skip("native waits 22 seconds for sleep 20; natural group completion is covered by a short unit test")
						})
						continue
					}
					steps.Add(1)
					if step.Before != nil {
						if step.Before.Write != nil {
							putFile(t, w.expand(step.Before.Write.Path), step.Before.Write.Content)
						}
						if step.Before.Remove != nil {
							if err := os.RemoveAll(w.expand(step.Before.Remove.Path)); err != nil {
								t.Fatal(err)
							}
						}
					}
					var r *circusv1.DeviceResult
					switch step.Tool {
					case "Bash":
						if (f.Case == "bash-background" && step.Input.Command == "sleep 4") || (f.Case == "taskstop-finished" && step.Input.Command == "sleep 2") || (f.Case == "bash-timeout-bg-lifecycle" && step.Input.Command == "sleep 6") {
							w.waitJob(t, w.jobID)
							r = w.bashCall(t, "true", false, step.Input.Timeout)
						} else {
							r = w.bashCall(t, w.expand(step.Input.Command), step.Input.Background, step.Input.Timeout)
						}
					case "TaskStop":
						r = w.call(t, &circusv1.DeviceCall{Tool: &circusv1.DeviceCall_TaskStop{TaskStop: &circusv1.TaskStopRequest{JobId: w.expand(step.Input.TaskID)}}})
					case "Read":
						if info, ok := w.jobs.Get(w.jobID); ok && info.State == circusv1.JobEvent_STATE_RUNNING {
							w.waitJob(t, w.jobID)
						}
						r = w.call(t, &circusv1.DeviceCall{Tool: &circusv1.DeviceCall_Read{Read: &circusv1.ReadRequest{Path: w.expand(step.Input.Path), Cwd: w.cwd}}})
					default:
						t.Fatalf("unsupported tool %s", step.Tool)
					}
					if r.Output.IsError != step.Result.IsError {
						t.Errorf("step %d is_error: got %v, want %v", i, r.Output.IsError, step.Result.IsError)
					}
					if i == 0 && strings.HasPrefix(f.Case, "bash-amp-child-") && f.Case != "bash-amp-child-timeout" {
						probeSleeps = waitOwnedSleeps(t, w.start)
					}
					if f.Case == "bash-amp-child-timeout" && len(ownedSleeps(w.start)) != 0 {
						t.Fatal("timeout left probe children alive")
					}
					got := w.normalize(r.Output.Content[0].GetText())
					if f.Case == "bash-amp-child-primary" && i == 1 {
						got = normalizeSleepTable(t, got, probeSleeps)
					}
					if exception, ok := fixtureExceptions[fmt.Sprintf("%s/%d", f.Case, i)]; ok {
						// The host's system profile can make a snapshot exceed the inline
						// limit; inspect its full persisted bytes for this semantic check.
						if strings.HasPrefix(step.Input.Command, "cat ") && r.GetBash().GetOutputTruncated() {
							data, err := os.ReadFile(r.GetBash().OutputPath)
							if err != nil {
								t.Fatal(err)
							}
							got = w.normalize(string(data))
						}
						exception.check(t, got)
						continue
					}
					want := stripCLI(step.Result.Content)
					if got != want {
						first := 0
						for first < len(got) && first < len(want) && got[first] == want[first] {
							first++
						}
						t.Errorf("step %d differs at byte %d; want len=%d got len=%d\nwant %.2500q\ngot  %.2500q", i, first, len(want), len(got), want, got)
					}
				}
			})
		}
	}
	t.Parallel()
	t.Cleanup(func() {
		if !t.Failed() {
			t.Logf("Passed %d Bash/TaskStop fixture subtests (%d tool steps)", count, steps.Load())
		}
	})
}
