package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/credfile"
	"github.com/EricWvi/circus/internal/daemon"
	"github.com/EricWvi/circus/internal/testutil/leakcheck"
	"google.golang.org/protobuf/encoding/protojson"
)

type harnessTool struct {
	run func(context.Context, *circusv1.DeviceCall) (*circusv1.DeviceResult, error)
}

func (h harnessTool) Name() string { return "read" }
func (h harnessTool) Run(ctx context.Context, call *circusv1.DeviceCall) (*circusv1.DeviceResult, error) {
	return h.run(ctx, call)
}

func await[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(2 * time.Second):
		t.Fatal("timed out")
		var zero T
		return zero
	}
}

func decodeStatus(t *testing.T, line string) *circusv1.CallStatus {
	t.Helper()
	s := new(circusv1.CallStatus)
	if err := protojson.Unmarshal([]byte(line), s); err != nil {
		t.Fatalf("invalid output %q: %v", line, err)
	}
	return s
}

func TestDaemonCallStreamsInCompletionOrder(t *testing.T) {
	input := strings.NewReader("{\"sessionId\":\"session\",\"callId\":\"slow\",\"read\":{\"path\":\"/a\"}}\n{\"sessionId\":\"session\",\"callId\":\"fast\",\"read\":{\"path\":\"/b\"}}\n")
	release := make(chan struct{})
	started := make(chan string, 2)
	tool := harnessTool{run: func(_ context.Context, call *circusv1.DeviceCall) (*circusv1.DeviceResult, error) {
		started <- call.CallId
		if call.CallId == "slow" {
			<-release
		}
		return &circusv1.DeviceResult{Output: &circusv1.ToolOutput{}}, nil
	}}
	reader, writer := io.Pipe()
	t.Cleanup(func() { _ = reader.Close(); _ = writer.Close() })
	finished := make(chan error, 1)
	tools := leakcheck.AuditTools(t, nil, []daemon.Tool{tool})
	go func() { finished <- daemonCall(input, writer, tools, true); _ = writer.Close() }()
	lines := make(chan string, 2)
	go func() {
		scanner := bufio.NewScanner(reader)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
		close(lines)
	}()
	await(t, started)
	await(t, started)
	first := decodeStatus(t, await(t, lines))
	if first.CallId != "fast" || first.State != circusv1.CallStatus_STATE_COMPLETED || first.Result.CallId != "fast" {
		t.Fatalf("first output: %v", first)
	}
	close(release)
	second := decodeStatus(t, await(t, lines))
	if second.CallId != "slow" || second.State != circusv1.CallStatus_STATE_COMPLETED || second.Result.CallId != "slow" {
		t.Fatalf("second output: %v", second)
	}
	if err := await(t, finished); err != nil {
		t.Fatal(err)
	}
	if _, ok := <-lines; ok {
		t.Fatal("extra output")
	}
}

func TestDaemonCallRetriesAndImmediateStatus(t *testing.T) {
	input := "{\"sessionId\":\"session\",\"callId\":\"same\",\"read\":{}}\n{\"sessionId\":\"session\",\"callId\":\"same\",\"read\":{}}\n"
	for _, wait := range []bool{true, false} {
		var runs atomic.Int32
		tool := harnessTool{run: func(ctx context.Context, _ *circusv1.DeviceCall) (*circusv1.DeviceResult, error) {
			runs.Add(1)
			if !wait {
				<-ctx.Done()
			}
			return &circusv1.DeviceResult{}, nil
		}}
		var out bytes.Buffer
		if err := daemonCall(strings.NewReader(input), &out, leakcheck.AuditTools(t, nil, []daemon.Tool{tool}), wait); err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimSpace(out.String()), "\n")
		if len(lines) != 2 {
			t.Fatalf("output: %s", out.String())
		}
		state := circusv1.CallStatus_STATE_RUNNING
		if wait {
			state = circusv1.CallStatus_STATE_COMPLETED
		}
		for _, line := range lines {
			if s := decodeStatus(t, line); s.State != state {
				t.Fatalf("wait %v: %v", wait, s)
			}
		}
		if runs.Load() != 1 {
			t.Fatalf("wait %v ran %d times", wait, runs.Load())
		}
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("stdout failed") }

func TestDaemonCommandRegistryFlagsAndErrors(t *testing.T) {
	if err := os.MkdirAll(".tmp", 0o755); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(".tmp", "daemon-test-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	isolated, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	// daemonCommand warms the default Bash, which uses these default paths.
	leakcheck.Track(t, isolated)
	leakcheck.StateDir(t, filepath.Join(isolated, "state", "circus", "daemon"))
	leakcheck.OutputRoot(t, filepath.Join(isolated, "cache", "circus", "output"))
	t.Setenv("HOME", isolated)
	t.Setenv("XDG_STATE_HOME", filepath.Join(isolated, "state"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(isolated, "cache"))
	t.Setenv("TMPDIR", isolated)
	path, err := filepath.Abs(filepath.Join(dir, "read.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("alpha\nbeta\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	call, err := protojson.Marshal(&circusv1.DeviceCall{
		CallId: "c1", SessionId: "session",
		Tool: &circusv1.DeviceCall_Read{Read: &circusv1.ReadRequest{Path: path}},
	})
	if err != nil {
		t.Fatal(err)
	}
	input := string(call) + "\n"
	var out, errOut bytes.Buffer
	if err := daemonCommand([]string{"call"}, strings.NewReader(input), &out, &errOut); err != nil {
		t.Fatal(err)
	}
	s := decodeStatus(t, strings.TrimSpace(out.String()))
	if s.State != circusv1.CallStatus_STATE_COMPLETED || s.Result.GetOutput().GetIsError() || len(s.Result.GetOutput().GetContent()) != 1 || s.Result.Output.Content[0].GetText() != "1\talpha\n2\tbeta\n3" {
		t.Fatalf("default registry: %v", s)
	}
	out.Reset()
	unavailableInput := "{\"sessionId\":\"session\",\"callId\":\"c2\",\"taskStop\":{\"jobId\":\"unavailable\"}}\n"
	if err := daemonCall(strings.NewReader(unavailableInput), &out, nil, false); err != nil {
		t.Fatal(err)
	}
	if s := decodeStatus(t, strings.TrimSpace(out.String())); s.State != circusv1.CallStatus_STATE_COMPLETED || !s.Result.GetOutput().GetIsError() || len(s.Result.GetOutput().GetContent()) != 1 || s.Result.Output.Content[0].GetText() != `Error: tool "task_stop" is not available on this device` {
		t.Fatalf("immediate unknown tool: %v", s)
	}
	for _, args := range [][]string{nil, {"unknown"}, {"call", "extra"}, {"call", "-unknown"}} {
		if err := daemonCommand(args, strings.NewReader(""), &out, &errOut); err == nil {
			t.Fatalf("invalid args: %v", args)
		}
	}
	for _, badInput := range []string{"not json\n", "{}\n"} {
		if err := daemonCommand([]string{"call"}, strings.NewReader(badInput), &out, &errOut); err == nil {
			t.Fatalf("invalid input: %s", badInput)
		}
	}
	if err := daemonCommand([]string{"call"}, strings.NewReader(input), failingWriter{}, &errOut); err == nil || err.Error() != "stdout failed" {
		t.Fatalf("stdout error: %v", err)
	}
}

func TestDaemonHelpAndCredentialErrors(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := daemonCommand([]string{"--help"}, strings.NewReader(""), &out, &errOut); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"credentials", "daemon call", "SIGTERM"} {
		if !strings.Contains(errOut.String(), want) {
			t.Fatalf("help missing %s: %s", want, errOut.String())
		}
	}
	path := filepath.Join(t.TempDir(), "missing.json")
	if err := daemonCommand([]string{"--credentials", path}, strings.NewReader(""), &out, &errOut); !errors.Is(err, credfile.ErrNotFound) {
		t.Fatal(err)
	}
}

func TestDaemonDefaultCredentialsAndAuthenticationExit(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		if r.URL.Path != "/v1/link/daemon" || r.Header.Get("Authorization") != "Bearer daemon-secret" {
			t.Errorf("unexpected upgrade: %s %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer server.Close()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	path, err := credfile.Path(credfile.RoleDaemon)
	if err != nil {
		t.Fatal(err)
	}
	if err = credfile.Save(path, credfile.File{Role: credfile.RoleDaemon, ControlPlaneURL: server.URL, PrincipalID: "device", Token: "daemon-secret"}); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	err = daemonCommand(nil, strings.NewReader(""), &out, &errOut)
	if err == nil || !strings.Contains(err.Error(), "HTTP 401") || attempts.Load() != 1 {
		t.Fatalf("attempts=%d error=%v", attempts.Load(), err)
	}
}
