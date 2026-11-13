package wire_test

import (
	"math"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/wire"
	"github.com/EricWvi/circus/internal/wire/wiretest"
)

func TestWelcomeOptionsValidation(t *testing.T) {
	track(t)
	budget := minBudget(t, wire.Options{})
	o, err := wire.OptionsFromWelcome(&circusv1.Welcome{}, wire.Options{Budget: budget, MaxMessageBytes: 100, MaxInFlightMessages: 1})
	if err != nil || o.MaxMessageBytes != wire.DefaultMaxMessageBytes || o.MaxInFlightMessages != 2 || o.Budget != budget || o.MaxConsumerStall != 15*time.Second {
		t.Fatal(o, err)
	}
	o, err = wire.OptionsFromWelcome(&circusv1.Welcome{HeartbeatIntervalMs: 123, HeartbeatTimeoutMs: 456}, wire.Options{})
	if err != nil || o.HeartbeatInterval != 123*time.Millisecond || o.HeartbeatTimeout != 456*time.Millisecond || o.MaxConsumerStall != 456*time.Millisecond {
		t.Fatal(o, err)
	}
	for _, w := range []*circusv1.Welcome{nil, {MaxMessageBytes: math.MaxUint64}, {MaxInFlightBytes: math.MaxUint64}, {MaxInFlightMessages: 3}, {MaxFragmentBytes: 100, MaxMessageBytes: 99}, {HeartbeatIntervalMs: 200, HeartbeatTimeoutMs: 100}} {
		if _, err := wire.OptionsFromWelcome(w, wire.Options{}); err == nil {
			t.Fatalf("invalid Welcome accepted: %+v", w)
		}
	}
	a, b := wiretest.Pipe(wiretest.Options{})
	defer a.Close()
	defer b.Close()
	if l, err := wire.New(a, wire.DaemonSide, wire.Options{MaxReceiveBytes: 1}); err == nil {
		_ = l.Close()
		t.Fatal("undersized local receive budget accepted")
	}
}

// The server writes explicit limits; the client derives the same values without
// relying on its own compiled defaults.
func TestFillWelcomeExplicitRoundTrip(t *testing.T) {
	track(t)
	w := &circusv1.Welcome{PrincipalId: "p"}
	server, err := wire.FillWelcome(w, wire.Options{MaxMessageBytes: 1 << 20, HeartbeatInterval: 2 * time.Second, MaxConsumerStall: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if w.PrincipalId != "p" || w.MaxMessageBytes != 1<<20 || w.MaxFragmentBytes != wire.DefaultMaxFragmentBytes || w.MaxInFlightMessages != 2 || w.MaxInFlightBytes != 2<<20 || w.HeartbeatIntervalMs != 2000 || w.HeartbeatTimeoutMs != 15000 {
		t.Fatalf("Welcome must carry explicit values: %+v", w)
	}
	client, err := wire.OptionsFromWelcome(w, wire.Options{MaxMessageBytes: 999, MaxInFlightBytes: 7})
	if err != nil {
		t.Fatal(err)
	}
	if client.MaxMessageBytes != server.MaxMessageBytes || client.MaxFragmentBytes != server.MaxFragmentBytes || client.MaxInFlightMessages != server.MaxInFlightMessages || client.MaxInFlightBytes != server.MaxInFlightBytes || client.HeartbeatInterval != server.HeartbeatInterval || client.HeartbeatTimeout != server.HeartbeatTimeout {
		t.Fatalf("sides diverge: server=%+v client=%+v", server, client)
	}
	if server.MaxConsumerStall != time.Minute || client.MaxConsumerStall != server.HeartbeatTimeout {
		t.Fatal("local-only options must stay local", server.MaxConsumerStall, client.MaxConsumerStall)
	}
	for _, o := range []wire.Options{{HeartbeatInterval: 1500 * time.Microsecond}, {MaxInFlightMessages: 3}} {
		if _, err := wire.FillWelcome(&circusv1.Welcome{}, o); err == nil {
			t.Fatalf("unrepresentable or invalid options accepted: %+v", o)
		}
	}
	if _, err := wire.FillWelcome(nil, wire.Options{}); err == nil {
		t.Fatal("nil Welcome accepted")
	}
}
