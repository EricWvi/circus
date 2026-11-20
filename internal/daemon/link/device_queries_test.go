package link_test

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/daemon"
	"github.com/EricWvi/circus/internal/daemon/dirlist"
	"github.com/EricWvi/circus/internal/daemon/jobs"
	daemonlink "github.com/EricWvi/circus/internal/daemon/link"
	"github.com/EricWvi/circus/internal/wire"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

type directoryFS struct {
	dirlist.OSFileSystem
	stat  func(string) (fs.FileInfo, error)
	lstat func(string) (fs.FileInfo, error)
	open  func(string) (fs.ReadDirFile, error)
}

func (f directoryFS) Stat(path string) (fs.FileInfo, error) {
	if f.stat != nil {
		return f.stat(path)
	}
	return f.OSFileSystem.Stat(path)
}

func (f directoryFS) Lstat(path string) (fs.FileInfo, error) {
	if f.lstat != nil {
		return f.lstat(path)
	}
	return f.OSFileSystem.Lstat(path)
}

func (f directoryFS) Open(path string) (fs.ReadDirFile, error) {
	if f.open != nil {
		return f.open(path)
	}
	return f.OSFileSystem.Open(path)
}

func sendDeviceQuery(t *testing.T, l *wire.Link, q *circusv1.DeviceQuery) {
	t.Helper()
	send(t, l, &circusv1.DaemonDownlink{Payload: &circusv1.DaemonDownlink_DeviceQuery{DeviceQuery: q}})
}

func directoryQuery(id, path string) *circusv1.DeviceQuery {
	return &circusv1.DeviceQuery{RequestId: id, Query: &circusv1.DeviceQuery_ListDirectory{ListDirectory: &circusv1.ListDirectoryQuery{Path: path}}}
}

func gitRemoteQuery(id, path string) *circusv1.DeviceQuery {
	return &circusv1.DeviceQuery{RequestId: id, Query: &circusv1.DeviceQuery_GitRemote{GitRemote: &circusv1.GitRemoteQuery{Path: path}}}
}

func wantDeviceQueryError(t *testing.T, l *wire.Link, id string, code circusv1.DeviceQueryError_Code) {
	t.Helper()
	reply := receive(t, l).GetDeviceQueryResult()
	if reply == nil || reply.RequestId != id || reply.GetError().GetCode() != code {
		t.Fatalf("reply = %v, want %q with %v", reply, id, code)
	}
}

func TestDeviceQueryReplies(t *testing.T) {
	h := start(t, nil, nil)
	l, _, hello := h.handshake(t)
	if !slices.Contains(hello.Features, wire.FeatureListDirectory) || !slices.Contains(hello.Features, wire.FeatureGitRemote) {
		t.Fatalf("Hello features = %v", hello.Features)
	}
	for _, id := range []string{"", "bad/id", strings.Repeat("a", wire.MaxOrderingKeyBytes+1)} {
		sendDeviceQuery(t, l, directoryQuery(id, ""))
		wantDeviceQueryError(t, l, "", circusv1.DeviceQueryError_CODE_INVALID)
	}
	sendDeviceQuery(t, l, &circusv1.DeviceQuery{RequestId: "missing-kind"})
	wantDeviceQueryError(t, l, "missing-kind", circusv1.DeviceQueryError_CODE_UNSUPPORTED)
	future := &circusv1.DeviceQuery{RequestId: "future"}
	future.ProtoReflect().SetUnknown(protowire.AppendBytes(protowire.AppendTag(nil, 99, protowire.BytesType), nil))
	sendDeviceQuery(t, l, future)
	wantDeviceQueryError(t, l, "future", circusv1.DeviceQueryError_CODE_UNSUPPORTED)

	dir := t.TempDir()
	id := strings.Repeat("a", 128)
	sendDeviceQuery(t, l, directoryQuery(id, dir))
	reply := receive(t, l).GetDeviceQueryResult()
	if reply.GetRequestId() != id || reply.GetListDirectory().GetPath() != dir || reply.GetListDirectory().GetError() != 0 {
		t.Fatalf("valid reply = %v", reply)
	}
}

func TestDeviceQueryTimeoutHoldsSlots(t *testing.T) {
	started := make(chan struct{}, 4)
	release := make(chan struct{})
	h := start(t, func(o *daemonlink.Options) {
		o.DirectoryFS = directoryFS{stat: func(path string) (fs.FileInfo, error) {
			started <- struct{}{}
			<-release
			return os.Stat(path)
		}}
	}, nil)
	defer close(release)
	l, _, _ := h.handshake(t)
	dir := t.TempDir()
	pending := map[string]bool{}
	for i := range 4 {
		id := fmt.Sprintf("query-%d", i)
		sendDeviceQuery(t, l, directoryQuery(id, dir))
		pending[id] = true
		recv(t, started)
	}

	sendDeviceQuery(t, l, gitRemoteQuery("busy", dir))
	wantDeviceQueryError(t, l, "busy", circusv1.DeviceQueryError_CODE_BUSY)
	// A regular downlink still gets through while every filesystem worker is stuck.
	send(t, l, &circusv1.DaemonDownlink{Payload: &circusv1.DaemonDownlink_CallQuery{CallQuery: &circusv1.CallQuery{CallId: "call"}}})
	if status := receive(t, l).GetCallStatus(); status.GetCallId() != "call" {
		t.Fatalf("call status = %v", status)
	}

	h.clk.Advance(5 * time.Second)
	for range 4 {
		reply := receive(t, l).GetDeviceQueryResult()
		listing := reply.GetListDirectory()
		if id := reply.GetRequestId(); !pending[id] || listing.GetError() != circusv1.DirectoryError_DIRECTORY_ERROR_TIMEOUT || listing.GetPath() != dir {
			t.Fatalf("timeout reply = %v", reply)
		} else {
			delete(pending, id)
		}
	}
	sendDeviceQuery(t, l, directoryQuery("still-busy", dir))
	wantDeviceQueryError(t, l, "still-busy", circusv1.DeviceQueryError_CODE_BUSY)
}

var gitProcessModes = []string{"timeout", "disconnect", "shutdown", "pipe"}

func TestGitRemoteProcessGroup(t *testing.T) {
	for _, mode := range gitProcessModes {
		t.Run(mode, func(t *testing.T) {
			testGitRemoteProcessGroup(t, t.TempDir(), mode)
		})
	}
}

func gitProcessIDs(dir string) (leader, child int) {
	data, err := os.ReadFile(filepath.Join(dir, "ready"))
	if err == nil {
		_, _ = fmt.Sscan(string(data), &leader, &child)
	}
	return leader, child
}

func testGitRemoteProcessGroup(t *testing.T, dir, mode string) {
	t.Helper()
	sleep, err := exec.LookPath("sleep")
	requireDirectoryFixture(t, err)
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	t.Setenv("FAKE_GIT_SLEEP", sleep)
	const script = `#!/bin/sh
if [ "$3" = rev-parse ]; then
    printf '.git\n'
    exit
fi
trap 'exit 0' USR1
"$FAKE_GIT_SLEEP" 3600 &
printf '%s %s\n' "$$" "$!" > "$2/ready"
wait
`
	requireDirectoryFixture(t, os.WriteFile(filepath.Join(bin, "fake-git"), []byte(script), 0700))
	h := start(t, func(o *daemonlink.Options) { o.GitExecutable = "fake-git" }, nil)
	l, _, _ := h.handshake(t)
	reaped := false
	t.Cleanup(func() {
		h.cancel()
		if group, _ := gitProcessIDs(dir); group > 0 && !reaped {
			_ = syscall.Kill(-group, syscall.SIGKILL)
		}
	})
	sendDeviceQuery(t, l, gitRemoteQuery("git-remote", dir))
	var group, child int
	eventually(t, func() bool {
		group, child = gitProcessIDs(dir)
		return group > 0 && child > 0
	})
	if pgid, err := syscall.Getpgid(child); err != nil || pgid != group {
		t.Fatalf("child group = %d, %v, want %d", pgid, err, group)
	}
	if got := h.client.DeviceQuerySlotsInUseForTest(); got != 1 {
		t.Fatalf("slots in use = %d, want 1", got)
	}
	wantReply := func(code circusv1.GitRemoteError) {
		t.Helper()
		reply := receive(t, l).GetDeviceQueryResult()
		if reply.GetRequestId() != "git-remote" || reply.GetGitRemote().GetError() != code {
			t.Fatalf("git remote reply = %v, want %v", reply, code)
		}
	}
	switch mode {
	case "timeout":
		h.clk.Advance(5 * time.Second)
		wantReply(circusv1.GitRemoteError_GIT_REMOTE_ERROR_TIMEOUT)
	case "disconnect":
		requireDirectoryFixture(t, l.Close())
	case "shutdown":
		h.cancel()
	case "pipe":
		// Let git exit, leaving stdout open in its child. WaitDelay must
		// unblock Wait and let the daemon reach group cleanup.
		requireDirectoryFixture(t, syscall.Kill(group, syscall.SIGUSR1))
		wantReply(circusv1.GitRemoteError_GIT_REMOTE_ERROR_FAILED)
	}
	eventually(t, func() bool { return h.client.DeviceQuerySlotsInUseForTest() == 0 })
	if err := syscall.Kill(-group, 0); err != syscall.ESRCH {
		t.Fatalf("group %d still exists after releasing the slot: %v", group, err)
	}
	reaped = true
}

func TestDeviceQueryShutdown(t *testing.T) {
	queryStarted := make(chan struct{}, 1)
	callCanceled := make(chan struct{}, 1)
	releaseCall := make(chan struct{})
	releaseRead := make(chan struct{})
	h := start(t, func(o *daemonlink.Options) {
		o.DirectoryFS = directoryFS{stat: func(path string) (fs.FileInfo, error) {
			queryStarted <- struct{}{}
			<-releaseRead
			return os.Stat(path)
		}}
		o.Tools = func(*jobs.Manager) []daemon.Tool {
			return []daemon.Tool{tool{func(ctx context.Context, _ *circusv1.DeviceCall) (*circusv1.DeviceResult, error) {
				<-ctx.Done()
				callCanceled <- struct{}{}
				<-releaseCall
				return result("done"), nil
			}}}
		}
	}, nil)
	defer close(releaseCall)
	defer close(releaseRead)
	l, _, hello := h.handshake(t)
	dispatch(t, l, hello.BootId, call("running"))
	ack(t, l)
	sendDeviceQuery(t, l, directoryQuery("running-query", ""))
	recv(t, queryStarted)
	h.cancel()
	recv(t, callCanceled)
	wantDeviceQueryError(t, l, "running-query", circusv1.DeviceQueryError_CODE_SHUTTING_DOWN)
	sendDeviceQuery(t, l, directoryQuery("new-query", ""))
	wantDeviceQueryError(t, l, "new-query", circusv1.DeviceQueryError_CODE_SHUTTING_DOWN)
}

func requireDirectoryFixture(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func wantDirectoryEntries(t *testing.T, l *wire.Link, id string, entries ...*circusv1.DirectoryEntry) {
	t.Helper()
	reply := receive(t, l).GetDeviceQueryResult()
	dir := reply.GetListDirectory()
	if reply.GetRequestId() != id || dir == nil || dir.Error != 0 || dir.Truncated || !slices.EqualFunc(dir.Entries, entries, func(a, b *circusv1.DirectoryEntry) bool {
		return proto.Equal(a, b)
	}) {
		t.Fatalf("reply = %v, want %q with entries %v", reply, id, entries)
	}
}

func TestDeviceQueryMarkerSubBudget(t *testing.T) {
	path := t.TempDir()
	requireDirectoryFixture(t, os.Mkdir(filepath.Join(path, "a-repo"), 0700))
	requireDirectoryFixture(t, os.Mkdir(filepath.Join(path, "a-repo", ".git"), 0700))
	requireDirectoryFixture(t, os.WriteFile(filepath.Join(path, "file"), nil, 0600))
	link := filepath.Join(path, "z-link")
	err := os.Symlink("file", link)
	if err != nil && runtime.GOOS == "windows" {
		t.Skipf("creating symlinks: %v", err)
	}
	requireDirectoryFixture(t, err)

	readStarted := make(chan struct{}, 1)
	probeStarted := make(chan struct{}, 1)
	readRelease := make(chan struct{})
	probeRelease := make(chan struct{})
	h := start(t, func(o *daemonlink.Options) {
		o.DirectoryFS = directoryFS{
			open: func(path string) (fs.ReadDirFile, error) {
				readStarted <- struct{}{}
				<-readRelease
				return os.Open(path)
			},
			stat: func(path string) (fs.FileInfo, error) {
				if path == link {
					probeStarted <- struct{}{}
					<-probeRelease
				}
				return os.Stat(path)
			},
		}
	}, nil)
	defer close(probeRelease)
	releaseRead := sync.OnceFunc(func() { close(readRelease) })
	defer releaseRead()

	l, _, _ := h.handshake(t)
	sendDeviceQuery(t, l, directoryQuery("markers", path))
	recv(t, readStarted)
	h.clk.Advance(time.Second)
	releaseRead()
	recv(t, probeStarted)
	h.clk.Advance(time.Second)
	wantDirectoryEntries(t, l, "markers",
		&circusv1.DirectoryEntry{Name: "a-repo", GitRepository: true},
		&circusv1.DirectoryEntry{Name: "z-link", Symlink: true})
}

func TestDeviceQueryProbePoolExhaustion(t *testing.T) {
	blocked := t.TempDir()
	requireDirectoryFixture(t, os.Mkdir(filepath.Join(blocked, "child"), 0700))
	blockedMarker := filepath.Join(blocked, "child", ".git")
	healthy := make([]string, 6)
	for i := range healthy {
		healthy[i] = t.TempDir()
		requireDirectoryFixture(t, os.MkdirAll(filepath.Join(healthy[i], "repo", ".git"), 0700))
	}
	probeStarted := make(chan struct{}, 1)
	probeRelease := make(chan struct{})
	h := start(t, func(o *daemonlink.Options) {
		o.DirectoryFS = directoryFS{lstat: func(path string) (fs.FileInfo, error) {
			if path == blockedMarker {
				probeStarted <- struct{}{}
				<-probeRelease
			}
			return os.Lstat(path)
		}}
	}, nil)
	defer close(probeRelease)

	l, _, _ := h.handshake(t)
	checkHealthy := func(marked bool) {
		t.Helper()
		for i, path := range healthy {
			id := fmt.Sprintf("healthy-%t-%d", marked, i)
			sendDeviceQuery(t, l, directoryQuery(id, path))
			wantDirectoryEntries(t, l, id, &circusv1.DirectoryEntry{Name: "repo", GitRepository: marked})
		}
	}
	for i := range 16 {
		id := fmt.Sprintf("blocked-%d", i)
		sendDeviceQuery(t, l, directoryQuery(id, blocked))
		recv(t, probeStarted)
		h.clk.Advance(2 * time.Second)
		wantDirectoryEntries(t, l, id, &circusv1.DirectoryEntry{Name: "child"})
		if i == 0 {
			checkHealthy(true)
		}
	}
	// All 16 probe slots are held, yet directory queries still reply immediately.
	checkHealthy(false)
}
