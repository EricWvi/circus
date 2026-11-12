package gateway_test

import (
	"github.com/EricWvi/circus/internal/controlplane/bus"
	"github.com/EricWvi/circus/internal/controlplane/gateway"
	"testing"
)

func TestRegistryReplacementFencesStaleClose(t *testing.T) {
	r, err := gateway.NewRegistry(1)
	if err != nil {
		t.Fatal(err)
	}
	a := gateway.Connection{Target: bus.Target{InstanceID: "i", DeviceID: "d", BootID: "b", ConnectionID: "a"}, UserID: "u", DeviceName: "desk"}
	b := a
	b.Target.ConnectionID = "b"
	if _, err = r.Register(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	prev, err := r.Register(t.Context(), b)
	if err != nil || prev == nil || prev.Target != a.Target {
		t.Fatal(prev, err)
	}
	if changed, err := r.Unregister(t.Context(), a); changed || err != nil {
		t.Fatal(changed, err)
	}
	if changed, err := r.Activate(t.Context(), b); !changed || err != nil {
		t.Fatal(changed, err)
	}
	got, ok, err := r.Lookup(t.Context(), "d")
	if err != nil || !ok || !got.Ready {
		t.Fatal(got, ok, err)
	}
	other, err := r.ListOnline(t.Context(), "other")
	if err != nil || len(other) != 0 {
		t.Fatal(other, err)
	}
	r.Unregister(t.Context(), b)
	if r.Stats().Connections != 0 {
		t.Fatal(r.Stats())
	}
}
