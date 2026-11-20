package wire_test

import (
	"fmt"
	"strings"
	"testing"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/wire"
	"github.com/EricWvi/circus/internal/wire/wiretest"
	"google.golang.org/protobuf/proto"
)

func TestDeviceQueryResultFragmentation(t *testing.T) {
	track(t)
	a, b := wiretest.Pipe(wiretest.Options{})
	opts := wire.Options{DisableHeartbeat: true}
	sender := newLink(t, a, wire.DaemonDownlink, wire.DaemonUplink, opts)
	receiver := newLink(t, b, wire.DaemonUplink, wire.DaemonDownlink, opts)
	listing := &circusv1.ListDirectoryResult{Path: "/projects", Parent: "/"}
	for i := range wire.MaxDirectoryEntries {
		listing.Entries = append(listing.Entries, &circusv1.DirectoryEntry{
			Name: fmt.Sprintf("%04d%s", i, strings.Repeat("x", 251)), Symlink: true, GitRepository: true,
		})
	}
	msg := &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_DeviceQueryResult{DeviceQueryResult: &circusv1.DeviceQueryResult{
		RequestId: "listing", Outcome: &circusv1.DeviceQueryResult_ListDirectory{ListDirectory: listing},
	}}}
	if proto.Size(msg) <= wire.DefaultMaxFragmentBytes {
		t.Fatalf("fixture must exceed one fragment, got %d bytes", proto.Size(msg))
	}
	ctx := contextFor(t)
	if err := sender.Send(ctx, msg); err != nil {
		t.Fatal(err)
	}
	got, err := receiver.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(got, msg) || receiver.Stats().FramesRead < 2 {
		t.Fatal("fragmented directory result was not received intact")
	}
}
