package link_test

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	daemonlink "github.com/EricWvi/circus/internal/daemon/link"
	"github.com/EricWvi/circus/internal/wire"
)

// fakeCircus writes an executable script standing in for circus and
// returns its path; body runs with $1 = "update".
func fakeCircus(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "circus")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func startUpdater(t *testing.T, exe string, timeout time.Duration) (*harness, *wire.Link) {
	t.Helper()
	h := start(t, func(o *daemonlink.Options) {
		o.Service, o.UpdateExecutable, o.UpdateTimeout = true, exe, timeout
	}, nil)
	l, _, hello := h.handshake(t)
	if !slices.Contains(hello.GetFeatures(), wire.FeatureMachineUpdate) {
		t.Fatalf("features = %q, want %q", hello.GetFeatures(), wire.FeatureMachineUpdate)
	}
	return h, l
}

func sendUpdate(t *testing.T, l *wire.Link, id string) {
	t.Helper()
	send(t, l, &circusv1.DaemonDownlink{Payload: &circusv1.DaemonDownlink_UpdateMachine{UpdateMachine: &circusv1.UpdateMachine{UpdateId: id, Version: "v9.9.9"}}})
}

// nextUplink returns the next uplink that is not a heartbeat.
func nextUplink(t *testing.T, l *wire.Link) *circusv1.DaemonUplink {
	t.Helper()
	for {
		if m := receive(t, l); m.GetHeartbeat() == nil {
			return m
		}
	}
}

// settled proves the commands sent so far were handled (a device query
// sent after them is answered, and nothing else came first), then waits
// for an update they started to end; it returns nothing reported before.
func settled(t *testing.T, h *harness, l *wire.Link) {
	t.Helper()
	sendDeviceQuery(t, l, directoryQuery("after", t.TempDir()))
	if m := nextUplink(t, l); m.GetDeviceQueryResult().GetRequestId() != "after" {
		t.Fatalf("uplink = %v, want only the query's answer", m)
	}
	eventually(t, func() bool { return !h.client.Stats().Updating })
}

// gone waits until the process pid no longer exists (killed and reaped).
func gone(t *testing.T, pidfile string) {
	t.Helper()
	data, err := os.ReadFile(pidfile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return syscall.Kill(pid, 0) == syscall.ESRCH })
}

func TestUpdateMachineSuccessReportsNothing(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	t.Setenv("HTTPS_PROXY", "http://proxy.test:3128")
	exe := fakeCircus(t, `printf '%s %s\n' "$*" "$HTTPS_PROXY" > `+marker)
	h, l := startUpdater(t, exe, time.Minute)
	sendUpdate(t, l, "u1")
	eventually(t, func() bool { _, err := os.Stat(marker); return err == nil })
	settled(t, h, l)
	settled(t, h, l) // nothing reported after the end either
	got, err := os.ReadFile(marker)
	if err != nil || string(got) != "update http://proxy.test:3128\n" {
		t.Errorf("fake circus saw %q, %v: want the update command and the daemon's environment", got, err)
	}
}

func TestUpdateMachineFailureIsReportedBounded(t *testing.T) {
	// Over 8 KiB of colored output, then the line that says why.
	exe := fakeCircus(t, `i=0; while [ $i -lt 400 ]; do printf '\033[31mnoise line %d\033[0m\r\n' $i; i=$((i+1)); done
printf 'bell\007 and nul-free\n' >&2
echo 'error: download latest.json: HTTP status 502' >&2
exit 3`)
	h, l := startUpdater(t, exe, time.Minute)
	sendUpdate(t, l, "u1")
	f := nextUplink(t, l).GetUpdateFailed()
	if f == nil || f.GetUpdateId() != "u1" {
		t.Fatalf("uplink = %v, want UpdateFailed for u1", f)
	}
	e := f.GetError()
	if len(e) > 2048 || !utf8.ValidString(e) || strings.ContainsAny(e, "\x1b\r\x07") {
		t.Errorf("error is not bounded and clean (%d bytes): %q", len(e), e)
	}
	if !strings.HasPrefix(e, "circus update failed: exit status 3\n") || !strings.HasSuffix(e, "error: download latest.json: HTTP status 502") {
		t.Errorf("error = %q", e)
	}
	settled(t, h, l)
}

func TestUpdateMachineTimeoutKillsProcessGroup(t *testing.T) {
	pidfile := filepath.Join(t.TempDir(), "pid")
	exe := fakeCircus(t, `sleep 60 & echo $! > `+pidfile+`.tmp; mv `+pidfile+`.tmp `+pidfile+`
echo started
sleep 60`)
	h, l := startUpdater(t, exe, 500*time.Millisecond)
	sendUpdate(t, l, "u1")
	f := nextUplink(t, l).GetUpdateFailed()
	if f == nil || f.GetUpdateId() != "u1" || !strings.HasPrefix(f.GetError(), "circus update did not finish within 1s and was stopped") ||
		!strings.HasSuffix(f.GetError(), "started") {
		t.Fatalf("uplink = %v, want a timeout for u1 with the output", f)
	}
	// The background sleep was in the command's process group: killed,
	// and reaped by this test process (a subreaper, as PID 1 would be).
	gone(t, pidfile)
	settled(t, h, l)
}

// What the command leaves in its group when it exits goes too, whether it
// failed or succeeded.
func TestUpdateMachineKillsWhatTheCommandLeft(t *testing.T) {
	for _, status := range []string{"0", "1"} {
		t.Run("exit "+status, func(t *testing.T) {
			pidfile := filepath.Join(t.TempDir(), "pid")
			exe := fakeCircus(t, `sleep 60 & echo $! > `+pidfile+`.tmp; mv `+pidfile+`.tmp `+pidfile+`
exit `+status)
			h, l := startUpdater(t, exe, time.Minute)
			sendUpdate(t, l, "u1")
			if status != "0" {
				if f := nextUplink(t, l).GetUpdateFailed(); f.GetUpdateId() != "u1" || !strings.HasPrefix(f.GetError(), "circus update failed: exit status 1") {
					t.Fatalf("UpdateFailed = %v", f)
				}
			}
			settled(t, h, l)
			settled(t, h, l) // nothing reported after the end either
			gone(t, pidfile)
		})
	}
}

func TestUpdateMachineOneAtATime(t *testing.T) {
	dir := t.TempDir()
	runs, release := filepath.Join(dir, "runs"), filepath.Join(dir, "release")
	exe := fakeCircus(t, `echo run >> `+runs+`
while [ ! -e `+release+` ]; do sleep 0.02; done
exit 1`)
	h, l := startUpdater(t, exe, time.Minute)
	sendUpdate(t, l, "first")
	eventually(t, func() bool { _, err := os.Stat(runs); return err == nil })
	sendUpdate(t, l, "second")
	// The second command was handled (ignored) before this query.
	sendDeviceQuery(t, l, directoryQuery("between", dir))
	if m := nextUplink(t, l); m.GetDeviceQueryResult().GetRequestId() != "between" {
		t.Fatalf("uplink = %v", m)
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if f := nextUplink(t, l).GetUpdateFailed(); f.GetUpdateId() != "first" {
		t.Fatalf("UpdateFailed = %v, want the first attempt's", f)
	}
	settled(t, h, l)
	if data, _ := os.ReadFile(runs); string(data) != "run\n" {
		t.Errorf("circus update ran %q, want once", data)
	}
}

// A daemon that does not run as the installed service neither advertises
// the feature nor runs the command.
func TestUpdateMachineOnlyAsService(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	exe := fakeCircus(t, `touch `+marker)
	h := start(t, func(o *daemonlink.Options) { o.UpdateExecutable = exe }, nil)
	l, _, hello := h.handshake(t)
	if slices.Contains(hello.GetFeatures(), wire.FeatureMachineUpdate) {
		t.Fatalf("features = %q without Service", hello.GetFeatures())
	}
	sendUpdate(t, l, "u1")
	settled(t, h, l)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("circus update ran on a foreground daemon: %v", err)
	}
}

func TestUpdateMachineMissingExecutable(t *testing.T) {
	h, l := startUpdater(t, filepath.Join(t.TempDir(), "missing"), time.Minute)
	sendUpdate(t, l, "u1")
	f := nextUplink(t, l).GetUpdateFailed()
	if f.GetUpdateId() != "u1" || !strings.HasPrefix(f.GetError(), "could not run circus update: ") {
		t.Fatalf("UpdateFailed = %v", f)
	}
	settled(t, h, l)
}
