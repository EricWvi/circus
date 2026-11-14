package wire_test

import (
	"bytes"
	"runtime"
	"strings"
	"testing"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/wire"
	"github.com/EricWvi/circus/internal/wire/wiretest"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func snapshotAttack(count int) []byte {
	body := bytes.Repeat([]byte{0x12, 0}, count)
	data := protowire.AppendTag(nil, 14, protowire.BytesType)
	return protowire.AppendBytes(data, body)
}

// Reject the packet BEFORE the generated decoder allocates jobs.
func TestDecodeAmplification(t *testing.T) {
	track(t)
	for _, count := range []int{wire.MaxSnapshotJobs + 1, 8192, 1 << 20, 4 << 20} {
		data := snapshotAttack(count)
		_, _ = wire.Decode(wire.DaemonUplink, data, wire.DefaultMaxMessageBytes)
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		m, err := wire.Decode(wire.DaemonUplink, data, wire.DefaultMaxMessageBytes)
		runtime.ReadMemStats(&after)
		runtime.KeepAlive(m)
		if err == nil {
			t.Fatalf("accepted %d snapshot entries", count)
		}
		if n := after.TotalAlloc - before.TotalAlloc; n > 512<<10 {
			t.Fatalf("rejected packet allocated %d bytes", n)
		}
	}
	// Exercise the exact repeated-element boundary within the byte limit.
	data := snapshotAttack(wire.MaxSnapshotJobs)
	if _, err := wire.Decode(wire.DaemonUplink, data, wire.DefaultMaxMessageBytes); err != nil {
		t.Fatal(err)
	}
}
func TestPayloadByteLimits(t *testing.T) {
	track(t)
	m := &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_JobEvent{JobEvent: &circusv1.JobEvent{OutputPath: strings.Repeat("x", wire.MaxMetadataBytes+1)}}}
	b, _ := proto.Marshal(m)
	if _, err := wire.Decode(wire.DaemonUplink, b, wire.DefaultMaxMessageBytes); err == nil {
		t.Fatal("metadata budget ignored")
	}
	// The negotiated envelope cap still applies to tool output, independently
	// of its node/field budgets. Use a small negotiation to keep this test cheap.
	b, _ = proto.Marshal(result(strings.Repeat("x", 1<<20)))
	if _, err := wire.Decode(wire.DaemonUplink, b, 1<<20); err == nil {
		t.Fatal("negotiated result budget ignored")
	}
}

func TestResultCarriersUseNegotiatedLimit(t *testing.T) {
	// Exercise every wrapper that can return retained tool output. In particular,
	// an accepted duplicate ack can carry a completed CallStatus with its result.
	output := result(strings.Repeat("x", 3<<20)).(*circusv1.DaemonUplink).GetResult()
	status := &circusv1.CallStatus{CallId: "c", State: circusv1.CallStatus_STATE_COMPLETED, Result: output}
	cases := []struct {
		name    string
		side    wire.Side
		message proto.Message
	}{
		{"result", wire.DaemonSide, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_Result{Result: output}}},
		{"status", wire.DaemonSide, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_CallStatus{CallStatus: status}}},
		{"ack", wire.DaemonSide, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_CallAck{CallAck: &circusv1.CallAck{CallId: "c", Outcome: &circusv1.CallAck_Status{Status: status}}}}},
		{"worker", wire.WorkerControlSide, &circusv1.WorkerDownlink{Payload: &circusv1.WorkerDownlink_CallUpdate{CallUpdate: &circusv1.CallUpdate{CallId: "c", Update: &circusv1.CallUpdate_Result{Result: output}}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			track(t)
			a, b := wiretest.Pipe(wiretest.Options{})
			sender, err := wire.New(a, tc.side, wire.Options{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = sender.Close() })
			peer := wire.DaemonControlSide
			if tc.side == wire.WorkerControlSide {
				peer = wire.WorkerSide
			}
			receiver, err := wire.New(b, peer, wire.Options{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = receiver.Close() })
			if err := sender.SendAs(contextFor(t), tc.message, wire.Data, "c"); err != nil {
				t.Fatal(err)
			}
			if got := receive(t, receiver); !proto.Equal(got, tc.message) {
				t.Fatal("result wrapper mismatch")
			}
		})
	}
}

func TestStatusOnlyPayloadsStaySmall(t *testing.T) {
	track(t)
	text := strings.Repeat("x", wire.MaxMetadataBytes+1)
	cases := []struct {
		kind    wire.Kind
		message proto.Message
	}{
		{wire.DaemonUplink, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_CallStatus{CallStatus: &circusv1.CallStatus{CallId: text}}}},
		{wire.DaemonUplink, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_CallStatus{CallStatus: &circusv1.CallStatus{CallId: text, Result: &circusv1.DeviceResult{}}}}},
		{wire.DaemonUplink, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_CallAck{CallAck: &circusv1.CallAck{Outcome: &circusv1.CallAck_Rejection{Rejection: &circusv1.Rejection{Message: text}}}}}},
		{wire.WorkerDownlink, &circusv1.WorkerDownlink{Payload: &circusv1.WorkerDownlink_CallUpdate{CallUpdate: &circusv1.CallUpdate{Update: &circusv1.CallUpdate_Unknown_{Unknown: &circusv1.CallUpdate_Unknown{Reason: text}}}}}},
	}
	for _, tc := range cases {
		data, err := proto.Marshal(tc.message)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := wire.Decode(tc.kind, data, wire.DefaultMaxMessageBytes); err == nil {
			t.Fatal("status-only payload exceeded metadata cap")
		}
	}
}
func TestScannerRejectsAttackShapes(t *testing.T) {
	track(t)
	empty := []byte{0x12, 0}
	nodeAttack := envField(11, bytes.Repeat([]byte{0x1a, 0}, wire.MaxDecodeNodes)) // repeated singular fault messages
	eventAttack := envField(14, envField(2, envField(5, bytes.Repeat([]byte{'x'}, wire.MaxMetadataBytes+1))))
	m := result(strings.Repeat("o", 40<<10)).(*circusv1.DaemonUplink).GetResult()
	padded := &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_CallStatus{CallStatus: &circusv1.CallStatus{CallId: strings.Repeat("m", wire.MaxMetadataBytes+1), Result: m}}}
	paddedBytes, _ := proto.Marshal(padded)
	for name, data := range map[string][]byte{"duplicate-payload": append(append([]byte{}, empty...), empty...), "nodes": nodeAttack, "nested-event": eventAttack, "result-exemption": paddedBytes} {
		t.Run(name, func(t *testing.T) {
			if _, err := wire.Decode(wire.DaemonUplink, data, wire.DefaultMaxMessageBytes); err == nil {
				t.Fatal("scanner accepted", name)
			}
		})
	}
}
