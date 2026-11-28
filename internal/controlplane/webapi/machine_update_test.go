package webapi_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/controlplane/updates"
	"github.com/EricWvi/circus/internal/controlplane/webapi"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/wire"
	"google.golang.org/protobuf/proto"
)

// executors stands in for the daemon and worker links: which roles take
// UpdateMachine, and what was sent.
type executors struct {
	mu      sync.Mutex
	daemons map[string]bool
	workers map[string]bool
	sent    []string
}

func (e *executors) Supports(id, feature string) (bool, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.daemons[id], e.daemons[id] && feature == wire.FeatureMachineUpdate
}

func (e *executors) UpdateMachine(_ context.Context, id string, _ *circusv1.UpdateMachine) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sent = append(e.sent, id)
	return nil
}

func (e *executors) Updatable(id string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.workers[id]
}

func (e *executors) workerUpdate(id string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.workers[id] {
		return false
	}
	e.sent = append(e.sent, id)
	return true
}

type workerSide struct{ *executors }

func (w workerSide) UpdateMachine(id string, _ *circusv1.UpdateMachine) bool {
	return w.workerUpdate(id)
}

func withUpdates(e *executors, m **updates.Manager) func(*webapi.Config) {
	return func(cfg *webapi.Config) {
		var err error
		*m, err = updates.New(updates.Config{Store: cfg.Store, Daemons: e, Workers: workerSide{e}, Now: cfg.Now,
			Latest: func(context.Context) (string, error) { return "v2.0.0", nil }})
		if err != nil {
			panic(err)
		}
		(*m).Check(context.Background())
		cfg.Updates = *m
	}
}

func (c *client) updateMachine(t *testing.T, req *circusv1.UpdateMachineRequest) (*circusv1.MachineUpdate, error) {
	t.Helper()
	res, err := c.machines.UpdateMachine(t.Context(), connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return res.Msg.GetUpdate(), nil
}

func byDevice(id string) *circusv1.UpdateMachineRequest {
	return &circusv1.UpdateMachineRequest{Identity: &circusv1.UpdateMachineRequest_DeviceId{DeviceId: id}}
}

func byWorker(id string) *circusv1.UpdateMachineRequest {
	return &circusv1.UpdateMachineRequest{Identity: &circusv1.UpdateMachineRequest_WorkerId{WorkerId: id}}
}

// rows returns the device's and the worker's rows as the lists show them.
func (c *client) rows(t *testing.T, device, worker string) (*circusv1.Device, *circusv1.Worker) {
	t.Helper()
	ds, err := c.machines.ListDevices(t.Context(), connect.NewRequest(&circusv1.ListDevicesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	ws, err := c.machines.ListWorkers(t.Context(), connect.NewRequest(&circusv1.ListWorkersRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	var d *circusv1.Device
	var w *circusv1.Worker
	for _, x := range ds.Msg.GetDevices() {
		if x.GetDeviceId() == device {
			d = x
		}
	}
	for _, x := range ws.Msg.GetWorkers() {
		if x.GetWorkerId() == worker {
			w = x
		}
	}
	return d, w
}

// TestUpdateMachine: the offer is on the card of a role online as the
// service with an older release; pressing it starts the machine's one
// update, which both cards show, failure reason included; the refusals
// follow the proto, with the reason as the message; the update goes with
// the machine's last identity.
func TestUpdateMachine(t *testing.T) {
	e := &executors{daemons: map[string]bool{}, workers: map[string]bool{}}
	var m *updates.Manager
	h, owner := newHarness(t, withUpdates(e, &m))
	ctx := t.Context()
	for _, id := range []store.Identity{
		{Kind: store.KindDevice, ID: "d1", UserID: h.owner.ID, Name: "laptop", MachineID: "m1", ClientVersion: "v1.0.0", CreatedAt: 1},
		{Kind: store.KindWorker, ID: "w1", UserID: h.owner.ID, Name: "laptop", MachineID: "m1", ClientVersion: "v1.0.0", CreatedAt: 1},
		{Kind: store.KindDevice, ID: "d2", UserID: h.owner.ID, Name: "dev", MachineID: "m2", ClientVersion: "dev", CreatedAt: 1},
	} {
		if err := h.st.CreateIdentity(ctx, id); err != nil {
			t.Fatal(err)
		}
	}

	// Nothing online: no offer, and pressing is refused.
	d, w := owner.rows(t, "d1", "w1")
	if d.GetUpdateVersion() != "" || w.GetUpdateVersion() != "" || d.GetUpdate() != nil {
		t.Fatalf("offer without an executor: %v / %v", d, w)
	}
	_, err := owner.updateMachine(t, byDevice("d1"))
	wantCode(t, err, connect.CodeFailedPrecondition)

	// The worker runs as the service: only its card offers the update.
	e.workers["w1"], e.daemons["d2"] = true, true
	d, w = owner.rows(t, "d1", "w1")
	if d.GetUpdateVersion() != "" || w.GetUpdateVersion() != "v2.0.0" {
		t.Fatalf("offers %q / %q, want the worker's card only", d.GetUpdateVersion(), w.GetUpdateVersion())
	}
	_, err = owner.updateMachine(t, byDevice("d1"))
	wantCode(t, err, connect.CodeFailedPrecondition)
	var refusal *connect.Error
	if !errors.As(err, &refusal) || refusal.Message() != "the device is not online as the installed service" {
		t.Fatalf("refusal = %v", err)
	}
	dev, _ := owner.rows(t, "d2", "")
	if dev.GetUpdateVersion() != "" {
		t.Fatalf("a development build is offered %q", dev.GetUpdateVersion())
	}
	_, err = owner.updateMachine(t, byDevice("d2"))
	wantCode(t, err, connect.CodeFailedPrecondition)

	// Refusals.
	_, err = owner.updateMachine(t, &circusv1.UpdateMachineRequest{})
	wantField(t, err, connect.CodeInvalidArgument, "identity")
	_, err = owner.updateMachine(t, byWorker(""))
	wantField(t, err, connect.CodeInvalidArgument, "worker_id")
	_, err = owner.updateMachine(t, byWorker("missing"))
	wantCode(t, err, connect.CodeNotFound)
	stranger := h.signIn(t, "stranger", "stranger@example.com")
	_, err = stranger.updateMachine(t, byDevice("d1"))
	wantCode(t, err, connect.CodeNotFound)
	_, err = h.browser(t).updateMachine(t, byDevice("d1"))
	wantCode(t, err, connect.CodeUnauthenticated)
	if len(e.sent) != 0 {
		t.Fatalf("refusals sent %v", e.sent)
	}

	// Pressing the worker's card updates the machine through it.
	u, err := owner.updateMachine(t, byWorker("w1"))
	if err != nil {
		t.Fatal(err)
	}
	now := h.clock.Load()
	want := &circusv1.MachineUpdate{TargetVersion: "v2.0.0", State: circusv1.MachineUpdateState_MACHINE_UPDATE_STATE_RUNNING,
		StartedUnixMs: now, DeadlineUnixMs: now + (5 * time.Minute).Milliseconds()}
	if !proto.Equal(u, want) || len(e.sent) != 1 || e.sent[0] != "w1" {
		t.Fatalf("update %v, sent %v\nwant %v", u, e.sent, want)
	}
	d, w = owner.rows(t, "d1", "w1")
	if !proto.Equal(d.GetUpdate(), want) || !proto.Equal(w.GetUpdate(), want) || d.GetUpdateVersion() != "" || w.GetUpdateVersion() != "" {
		t.Fatalf("cards: %v / %v", d, w)
	}
	// Pressing again returns the same update.
	again, err := owner.updateMachine(t, byWorker("w1"))
	if err != nil || !proto.Equal(again, want) || len(e.sent) != 1 {
		t.Fatalf("again = %v, %v; sent %v", again, err, e.sent)
	}
	// The worker's failure report shows on both cards, with its reason.
	update, err := h.st.GetMachineUpdate(ctx, h.owner.ID, "m1")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Failed(ctx, store.KindWorker, "w1", update.UpdateID, "error: disk full"); err != nil {
		t.Fatal(err)
	}
	failed := proto.Clone(want).(*circusv1.MachineUpdate)
	failed.State, failed.Failure, failed.Error = circusv1.MachineUpdateState_MACHINE_UPDATE_STATE_FAILED,
		circusv1.MachineUpdateFailure_MACHINE_UPDATE_FAILURE_COMMAND, "error: disk full"
	d, w = owner.rows(t, "d1", "w1")
	if !proto.Equal(d.GetUpdate(), failed) || !proto.Equal(w.GetUpdate(), failed) || w.GetUpdateVersion() != "v2.0.0" || d.GetUpdateVersion() != "" {
		t.Fatalf("cards after the failure: %v / %v", d, w)
	}

	// Deleting one identity keeps the update; deleting the last removes it.
	if _, err := owner.machines.DeleteDevice(ctx, connect.NewRequest(&circusv1.DeleteDeviceRequest{DeviceId: "d1"})); err != nil {
		t.Fatal(err)
	}
	if _, w = owner.rows(t, "", "w1"); !proto.Equal(w.GetUpdate(), failed) {
		t.Fatalf("worker card after deleting the device: %v", w)
	}
	if _, err := owner.machines.DeleteWorker(ctx, connect.NewRequest(&circusv1.DeleteWorkerRequest{WorkerId: "w1"})); err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.GetMachineUpdate(ctx, h.owner.ID, "m1"); err != store.ErrNotFound {
		t.Fatalf("update after deleting the machine's identities: %v", err)
	}
}
