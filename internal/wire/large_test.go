package wire_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/wire"
	"github.com/EricWvi/circus/internal/wire/wiretest"
	"google.golang.org/protobuf/proto"
)

// Explicit integration run keeps ordinary go test fast. At 4 MiB/s this takes
// over the default 15-second timeout, so reverse-direction heartbeats are
// essential even though fragments themselves keep the receiver alive.
// Run: CIRCUS_WIRE_LARGE_TEST=1 go test -race ./internal/wire -run TestLargeMessage
func TestLargeMessage(t *testing.T) {
	if os.Getenv("CIRCUS_WIRE_LARGE_TEST") != "1" {
		t.Skip("explicit 64 MiB / slow-link integration run")
	}
	for _, toolResult := range []bool{false, true} {
		name := "dispatch"
		if toolResult {
			name = "result"
		}
		t.Run(name, func(t *testing.T) { largeMessage(t, toolResult) })
	}
}

func largeMessage(t *testing.T, toolResult bool) {
	track(t)
	a, b := wiretest.Pipe(wiretest.Options{AtoB: wiretest.Faults{BytesPerSecond: 4 << 20}})
	in, out := wire.DaemonUplink, wire.DaemonDownlink
	var m proto.Message
	if toolResult {
		in, out = out, in
		uplink := result(strings.Repeat("x", wire.DefaultMaxMessageBytes)).(*circusv1.DaemonUplink)
		block := uplink.GetResult().Output.Content[0].Block.(*circusv1.ContentBlock_Text)
		block.Text = block.Text[:len(block.Text)-(proto.Size(uplink)-wire.DefaultMaxMessageBytes)]
		m = uplink
	} else {
		downlink := dispatch("large", strings.Repeat("x", wire.DefaultMaxMessageBytes)).(*circusv1.DaemonDownlink)
		req := downlink.GetDispatchCall().Call.GetWrite()
		req.Content = req.Content[:len(req.Content)-(proto.Size(downlink)-wire.DefaultMaxMessageBytes)]
		m = downlink
	}
	sender := newLink(t, a, in, out, wire.Options{})
	receiver := newLink(t, b, out, in, wire.Options{})
	if proto.Size(m) != wire.DefaultMaxMessageBytes {
		t.Fatalf("message is %d bytes", proto.Size(m))
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	start := time.Now()
	if err := testSend(sender, ctx, m); err != nil {
		t.Fatal(err)
	}
	got, err := receiver.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(got, m) {
		t.Fatal("64 MiB payload mismatch")
	}
	elapsed := time.Since(start)
	if elapsed < 15*time.Second {
		t.Fatalf("rate limit not applied: %s", elapsed)
	}
	if sender.Stats().HeartbeatsRead < 3 || receiver.Stats().HeartbeatsRead < 3 {
		t.Fatalf("missing heartbeats: sender=%+v receiver=%+v", sender.Stats(), receiver.Stats())
	}
	if sender.Err() != nil || receiver.Err() != nil {
		t.Fatal("link died during transfer")
	}
	t.Logf("64 MiB elapsed=%s sender=%+v receiver=%+v", elapsed, sender.Stats(), receiver.Stats())
}
