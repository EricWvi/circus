package bus_test

import (
	"context"
	"errors"
	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/controlplane/bus"
	"strings"
	"testing"
	"unsafe"
)

type receiver func(context.Context, bus.Target, *circusv1.DaemonDownlink) error

func (f receiver) Deliver(ctx context.Context, t bus.Target, m *circusv1.DaemonDownlink) error {
	return f(ctx, t, m)
}
func TestAdmissionAndUnbind(t *testing.T) {
	b, err := bus.New(bus.Limits{MaxInstances: 1, MaxInFlight: 1, MaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	target := bus.Target{InstanceID: "i", DeviceID: "d", BootID: "b", ConnectionID: "c"}
	msg := &circusv1.DaemonDownlink{Payload: &circusv1.DaemonDownlink_CallQuery{CallQuery: &circusv1.CallQuery{CallId: "c"}}}
	var calls int
	reg, err := b.Register("i", receiver(func(ctx context.Context, target bus.Target, m *circusv1.DaemonDownlink) error {
		calls++
		if !errors.Is(b.Send(ctx, target, m), bus.ErrBusy) {
			t.Error("nested send escaped admission")
		}
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err = b.Send(t.Context(), target, msg); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal(calls)
	}
	if err = reg.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(b.Send(t.Context(), target, msg), bus.ErrNoRoute) {
		t.Fatal("unbound delivery")
	}
	if got := b.Stats(); got != (bus.Stats{}) {
		t.Fatal(got)
	}
}

func TestOwnedCloneDetachesStringStorage(t *testing.T) {
	backing := strings.Repeat("x", 1<<20)
	small := backing[:4]
	m := &circusv1.DaemonDownlink{Payload: &circusv1.DaemonDownlink_CallQuery{CallQuery: &circusv1.CallQuery{CallId: small}}}
	if _, err := bus.Charge(m, 1024); err != nil {
		t.Fatal(err)
	}
	clone := bus.CloneOwned(m).(*circusv1.DaemonDownlink)
	got := clone.GetCallQuery().CallId
	if got != small || unsafe.StringData(got) == unsafe.StringData(small) {
		t.Fatal("clone retained caller backing storage")
	}
}
