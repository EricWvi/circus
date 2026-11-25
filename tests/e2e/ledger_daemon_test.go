//go:build e2e && linux

package e2e

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	pb "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/wire"
	"github.com/coder/websocket"
	"github.com/google/uuid"
)

// fakeLedgerBases holds the base commit of each snapshot ledgerAnswer
// prepared or rebased, by snapshot id, as a manifest would.
var fakeLedgerBases sync.Map

// ledgerAnswer answers a session ledger snapshot call the way a daemon
// that keeps no files would: prepared, collected without changes, rebased,
// removed, naming the base it records. It returns nil for any other call.
func ledgerAnswer(c *pb.DeviceCall) *pb.DeviceResult {
	if !strings.HasPrefix(c.GetSessionId(), store.LedgerSnapshotSessionPrefix) {
		return nil
	}
	out := &pb.DeviceResult{CallId: c.GetCallId(), Output: &pb.ToolOutput{}}
	switch t := c.GetTool().(type) {
	case *pb.DeviceCall_PrepareLedger:
		r := &pb.PrepareLedgerResult{Outcome: pb.PrepareLedgerResult_OUTCOME_CHUNK_ACCEPTED}
		if t.PrepareLedger.GetLast() {
			r = &pb.PrepareLedgerResult{Outcome: pb.PrepareLedgerResult_OUTCOME_READY, Path: "/fake/ledger/" + t.PrepareLedger.GetSnapshotId()}
			fakeLedgerBases.Store(t.PrepareLedger.GetSnapshotId(), t.PrepareLedger.GetBaseCommit())
		}
		out.Detail = &pb.DeviceResult_PrepareLedger{PrepareLedger: r}
	case *pb.DeviceCall_CollectLedger:
		base, ok := fakeLedgerBases.Load(t.CollectLedger.GetSnapshotId())
		if !ok {
			out.Detail = &pb.DeviceResult_CollectLedger{CollectLedger: &pb.CollectLedgerResult{Outcome: pb.CollectLedgerResult_OUTCOME_FAILED,
				Error: pb.LedgerSnapshotError_LEDGER_SNAPSHOT_ERROR_ABSENT}}
			break
		}
		out.Detail = &pb.DeviceResult_CollectLedger{CollectLedger: &pb.CollectLedgerResult{Outcome: pb.CollectLedgerResult_OUTCOME_COLLECTED,
			CurrentBaseCommit: base.(string)}}
	case *pb.DeviceCall_RebaseLedger:
		fakeLedgerBases.Store(t.RebaseLedger.GetSnapshotId(), t.RebaseLedger.GetBaseCommit())
		out.Detail = &pb.DeviceResult_RebaseLedger{RebaseLedger: &pb.RebaseLedgerResult{Outcome: pb.RebaseLedgerResult_OUTCOME_REBASED,
			CurrentBaseCommit: t.RebaseLedger.GetBaseCommit()}}
	case *pb.DeviceCall_RemoveLedger:
		out.Detail = &pb.DeviceResult_RemoveLedger{RemoveLedger: &pb.RemoveLedgerResult{Outcome: pb.RemoveLedgerResult_OUTCOME_REMOVED}}
	default:
		return nil
	}
	return out
}

// ledgerOnlyDaemon connects a device's daemon link that keeps sessions'
// ledger snapshots (ledgerAnswer) and answers any other call as an error,
// until the test ends.
func ledgerOnlyDaemon(t *testing.T, h *harness, deviceID, token string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	conn, _, err := wire.DialWebSocket(ctx, "ws"+strings.TrimPrefix(h.base, "http")+"/v1/link/daemon",
		&websocket.DialOptions{HTTPHeader: http.Header{"Authorization": []string{"Bearer " + token}}})
	must(t, err)
	_, opts, err := wire.DialHello(ctx, conn, wire.DaemonUplink, &pb.Hello{
		ProtocolVersion: wire.ProtocolVersion, BootId: uuid.NewString(), Os: "linux",
	}, deviceID, wire.Options{})
	must(t, err)
	link, err := wire.New(conn, wire.DaemonSide, opts)
	must(t, err)
	done := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		_ = link.Close()
		<-done
	})
	go func() {
		defer close(done)
		for {
			m, err := link.Receive(ctx)
			if err != nil {
				if !errors.Is(err, context.Canceled) && ctx.Err() == nil {
					t.Logf("ledger daemon link: %v", err)
				}
				return
			}
			down := m.(*pb.DaemonDownlink)
			var up *pb.DaemonUplink
			switch {
			case down.GetListJobs() != nil:
				up = &pb.DaemonUplink{Payload: &pb.DaemonUplink_JobSnapshot{JobSnapshot: &pb.JobSnapshot{}}}
			case down.GetDispatchCall().GetCall() != nil:
				c := down.GetDispatchCall().GetCall()
				r := ledgerAnswer(c)
				if r == nil {
					r = &pb.DeviceResult{CallId: c.GetCallId(), Output: &pb.ToolOutput{IsError: true,
						Content: []*pb.ContentBlock{{Block: &pb.ContentBlock_Text{Text: "this test device runs no tools"}}}}}
				}
				up = &pb.DaemonUplink{Payload: &pb.DaemonUplink_Result{Result: r}}
			default:
				continue
			}
			if err := link.Send(ctx, up); err != nil {
				return
			}
		}
	}()
}
