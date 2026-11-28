package controlplane_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/gen/go/circus/v1/circusv1connect"
	"github.com/EricWvi/circus/internal/auth"
	"github.com/EricWvi/circus/internal/controlplane"
	"github.com/EricWvi/circus/internal/credfile"
	"github.com/EricWvi/circus/internal/daemon"
	"github.com/EricWvi/circus/internal/daemon/jobs"
	daemonlink "github.com/EricWvi/circus/internal/daemon/link"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/testutil/wait"
	wc "github.com/EricWvi/circus/internal/testutil/workerclient"
	"github.com/EricWvi/circus/internal/wire"
	"github.com/google/uuid"
)

// TestMachineUpdateThroughServer drives machine updates over the real
// links: only the card of a role online as the service offers it; the
// worker runs the update and its failure shows on both cards, a long
// report keeping its end; a retry is a new attempt and the old one's late
// report is ignored; the worker's reconnect at the new release completes
// it. Then the daemon, below the target while the worker is at it, runs
// the fake circus, reports its failure, and completes the update by
// reconnecting at the new release.
func TestMachineUpdateThroughServer(t *testing.T) {
	h := newServer(t, controlplane.Config{LatestRelease: func(context.Context) (string, error) { return "v2.0.0", nil }}, nil)
	ctx := wait.Ctx(t)
	var token string
	for line := range strings.SplitSeq(h.logs.String(), "\n") {
		var rec struct {
			URL string `json:"url"`
		}
		if json.Unmarshal([]byte(line), &rec) == nil && rec.URL != "" {
			u, _ := url.Parse(rec.URL)
			token = u.Query().Get("token")
		}
	}
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	t.Cleanup(client.CloseIdleConnections)
	setup, err := circusv1connect.NewAuthServiceClient(client, h.base).Setup(ctx, connect.NewRequest(&circusv1.SetupRequest{Token: token, Email: "admin@example.com", Password: "password-123"}))
	if err != nil {
		t.Fatal(err)
	}
	owner := setup.Msg.GetUser().GetUserId()
	enroll := func(kind store.PrincipalKind, name string) auth.Enrollment {
		t.Helper()
		tok, _, err := auth.CreateEnrollToken(ctx, h.st, owner, kind, time.Hour, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		res, err := auth.Enroll(ctx, h.st, auth.EnrollRequest{Token: tok, Kind: kind, Name: name, MachineID: "m1", OS: "linux", Arch: "amd64", ClientVersion: "v1.0.0"}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	device, worker := enroll(store.KindDevice, "box"), enroll(store.KindWorker, "box")
	machines := circusv1connect.NewMachineServiceClient(client, h.base)
	rows := func() (*circusv1.Device, *circusv1.Worker) {
		t.Helper()
		ds, err := machines.ListDevices(ctx, connect.NewRequest(&circusv1.ListDevicesRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		ws, err := machines.ListWorkers(ctx, connect.NewRequest(&circusv1.ListWorkersRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		return ds.Msg.GetDevices()[0], ws.Msg.GetWorkers()[0]
	}
	press := func(req *circusv1.UpdateMachineRequest) *circusv1.MachineUpdate {
		t.Helper()
		res, err := machines.UpdateMachine(ctx, connect.NewRequest(req))
		if err != nil {
			t.Fatal(err)
		}
		return res.Msg.GetUpdate()
	}
	byDevice := &circusv1.UpdateMachineRequest{Identity: &circusv1.UpdateMachineRequest_DeviceId{DeviceId: device.Identity.ID}}
	running, failed := circusv1.MachineUpdateState_MACHINE_UPDATE_STATE_RUNNING, circusv1.MachineUpdateState_MACHINE_UPDATE_STATE_FAILED

	// The worker runs as the service.
	dialWorker := func(version string) (*wc.Client, wc.Events) {
		t.Helper()
		w, err := wc.Dial(ctx, wc.Options{URL: h.base, Token: worker.Token, PrincipalID: worker.Identity.ID, BootID: uuid.Must(uuid.NewV7()).String(),
			ClientVersion: version, Features: []string{wire.FeatureMachineUpdate}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { w.Close() })
		hints, err := w.Attach(ctx, &circusv1.Attach{MaxSessions: 1, Agents: wc.ClaudeInstalled()})
		if err != nil {
			t.Fatal(err)
		}
		return w, hints
	}
	nextUpdate := func(hints wc.Events) *circusv1.UpdateMachine {
		t.Helper()
		for {
			e, err := hints.NextEvent(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if u := e.GetUpdateMachine(); u != nil {
				return u
			}
		}
	}
	byWorker := &circusv1.UpdateMachineRequest{Identity: &circusv1.UpdateMachineRequest_WorkerId{WorkerId: worker.Identity.ID}}
	w, hints := dialWorker("v1.0.0")
	// Only the card of the role online as the service offers it.
	wait.For(t, func() bool { _, w := rows(); return w.GetUpdateVersion() == "v2.0.0" })
	if d, _ := rows(); d.GetUpdateVersion() != "" {
		t.Fatalf("the offline device's card offers %q", d.GetUpdateVersion())
	}
	if _, err := machines.UpdateMachine(ctx, connect.NewRequest(byDevice)); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("pressing the offline device = %v", err)
	}

	if u := press(byWorker); u.GetState() != running || u.GetTargetVersion() != "v2.0.0" {
		t.Fatalf("update = %v", u)
	}
	first := nextUpdate(hints)
	if first.GetVersion() != "v2.0.0" {
		t.Fatalf("command = %v", first)
	}
	report := func(w *wc.Client, id, text string) {
		t.Helper()
		r, err := w.UpdateFailed(ctx, id, text)
		if err != nil {
			t.Fatal(err)
		}
		if reply, err := r.Wait(ctx); err != nil || reply.GetOk() == nil {
			t.Fatalf("UpdateFailed reply %v, %v", reply, err)
		}
	}
	// An invalid report is refused without closing the link.
	if r, err := w.UpdateFailed(ctx, "not an id!", "x"); err != nil {
		t.Fatal(err)
	} else if reply, err := r.Wait(ctx); err != nil || reply.GetRejected().GetCode() != circusv1.SessionReply_Rejected_CODE_INVALID {
		t.Fatalf("invalid report reply %v, %v", reply, err)
	}
	// A report longer than 2048 bytes keeps its end, as on the daemon link.
	report(w, first.GetUpdateId(), strings.Repeat("x", 3000)+"\nerror: permission denied")
	d, wr := rows()
	if d.GetUpdate().GetState() != failed || d.GetUpdate().GetFailure() != circusv1.MachineUpdateFailure_MACHINE_UPDATE_FAILURE_COMMAND ||
		len(wr.GetUpdate().GetError()) != 2048 || !strings.HasSuffix(wr.GetUpdate().GetError(), "\nerror: permission denied") || wr.GetUpdateVersion() != "v2.0.0" {
		t.Fatalf("after the failure: %v / %v", d, wr)
	}

	// Retry from the worker's card: a new attempt; the old one's report is ignored.
	press(byWorker)
	second := nextUpdate(hints)
	if second.GetUpdateId() == first.GetUpdateId() {
		t.Fatal("the retry reused the attempt id")
	}
	report(w, first.GetUpdateId(), "late")
	if d, _ := rows(); d.GetUpdate().GetState() != running {
		t.Fatalf("a stale report changed the update: %v", d.GetUpdate())
	}
	// The restarted worker connects at the new release: done.
	w.Close()
	dialWorker("v2.0.0")
	wait.For(t, func() bool { d, w := rows(); return d.GetUpdate() == nil && w.GetUpdate() == nil })
	if d, w := rows(); d.GetUpdateVersion() != "" || w.GetUpdateVersion() != "" {
		t.Fatalf("offers after the worker's update: %q / %q", d.GetUpdateVersion(), w.GetUpdateVersion())
	}

	// The daemon, still at the old release, runs as the service too: its
	// card offers the update, and it runs it (the worker is at the target).
	dir := t.TempDir()
	marker, fake := filepath.Join(dir, "ran"), filepath.Join(dir, "circus")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\necho \"$1\" >> "+marker+"\necho 'error: no space left' >&2\nexit 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	runDaemon := func(version string) context.CancelFunc {
		t.Helper()
		c, err := daemonlink.New(daemonlink.Options{
			Credentials:   credfile.File{Role: credfile.RoleDaemon, ControlPlaneURL: h.base, PrincipalID: device.Identity.ID, Token: device.Token},
			ClientVersion: version, Service: true, UpdateExecutable: fake, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
			Tools:           func(*jobs.Manager) []daemon.Tool { return nil },
			MachineIDSource: func(context.Context) (string, error) { return "m1", nil },
		})
		if err != nil {
			t.Fatal(err)
		}
		runCtx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); _ = c.Run(runCtx) }()
		stop := func() { cancel(); <-done }
		t.Cleanup(stop)
		return stop
	}
	stop := runDaemon("v1.0.0")
	wait.For(t, func() bool { d, _ := rows(); return d.GetOnline() && d.GetUpdateVersion() == "v2.0.0" })
	press(byDevice)
	wait.For(t, func() bool { d, _ := rows(); return d.GetUpdate().GetState() == failed })
	d, wr = rows()
	if !strings.Contains(d.GetUpdate().GetError(), "exit status 2") || !strings.HasSuffix(wr.GetUpdate().GetError(), "error: no space left") {
		t.Fatalf("daemon failure: %v / %v", d.GetUpdate(), wr.GetUpdate())
	}
	if data, _ := os.ReadFile(marker); string(data) != "update\n" {
		t.Fatalf("the daemon ran %q, want circus update once", data)
	}
	// The restarted daemon at the new release completes even a failed update.
	stop()
	runDaemon("v2.0.0")
	wait.For(t, func() bool { d, w := rows(); return d.GetUpdate() == nil && w.GetUpdate() == nil && d.GetOnline() })
	if d, _ := rows(); d.GetVersion() != "v2.0.0" || d.GetUpdateVersion() != "" {
		t.Fatalf("device after its update: %v", d)
	}
}
