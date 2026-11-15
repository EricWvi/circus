package wire_test

import (
	"strings"
	"testing"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/wire"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func TestSessionTranscriptByteLimits(t *testing.T) {
	appendRequest := func(entries ...*circusv1.TranscriptEntry) proto.Message {
		return &circusv1.WorkerUplink{RequestId: 1, Payload: &circusv1.WorkerUplink_Session{Session: &circusv1.SessionRequest{
			SessionId: "session", Epoch: 1, Op: &circusv1.SessionRequest_AppendTranscript{AppendTranscript: &circusv1.AppendTranscript{BatchSeq: 1, Entries: entries}},
		}}}
	}
	entry := &circusv1.TranscriptEntry{Json: strings.Repeat("x", 4<<20)}
	for _, tc := range []struct {
		name string
		m    proto.Message
		kind wire.Kind
		ok   bool
	}{
		{"metadata_sized_append", appendRequest(&circusv1.TranscriptEntry{Json: "opaque, not parsed as JSON"}), wire.WorkerUplink, true},
		{"fragmented_append", appendRequest(entry), wire.WorkerUplink, true},
		{"batch_under_limit", appendRequest(&circusv1.TranscriptEntry{Json: strings.Repeat("x", 2<<20)}, entry), wire.WorkerUplink, true},
		{"batch_over_limit", appendRequest(entry, entry), wire.WorkerUplink, false},
		{"single_oversized_entry", appendRequest(&circusv1.TranscriptEntry{Json: strings.Repeat("x", 9<<20)}), wire.WorkerUplink, true},
		{"large_page", &circusv1.WorkerDownlink{RequestId: 1, Payload: &circusv1.WorkerDownlink_SessionReply{SessionReply: &circusv1.SessionReply{Result: &circusv1.SessionReply_TranscriptPage_{TranscriptPage: &circusv1.SessionReply_TranscriptPage{Entries: []*circusv1.TranscriptEntry{entry}}}}}}, wire.WorkerDownlink, true},
		{"oversized_patch", &circusv1.WorkerUplink{RequestId: 1, Payload: &circusv1.WorkerUplink_Session{Session: &circusv1.SessionRequest{SessionId: "session", Epoch: 1, Op: &circusv1.SessionRequest_PatchState{PatchState: &circusv1.PatchSessionState{ReturnedCallIds: []string{strings.Repeat("x", wire.MaxMetadataBytes)}}}}}}, wire.WorkerUplink, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := proto.Marshal(tc.m)
			if err != nil {
				t.Fatal(err)
			}
			got, err := wire.Decode(tc.kind, data, wire.DefaultMaxMessageBytes)
			if (err == nil) != tc.ok {
				t.Fatalf("Decode error = %v, want success %v", err, tc.ok)
			}
			if tc.ok && !proto.Equal(got, tc.m) {
				t.Fatal("session payload changed during round trip")
			}
		})
	}
}

func TestSessionRepeatedEnumsRejectPackedEncoding(t *testing.T) {
	// Even with packed=false in the schema, protobuf normally accepts packed
	// input. The scanner must reject it so one tag cannot hide excess elements.
	packed := protowire.AppendTag(nil, 1, protowire.BytesType)
	packed = protowire.AppendBytes(packed, []byte{1, 2, 3, 4, 5, 1})
	session := protowire.AppendTag(nil, 18, protowire.BytesType)
	session = protowire.AppendBytes(session, packed)
	envelope := protowire.AppendTag(nil, 15, protowire.BytesType)
	envelope = protowire.AppendBytes(envelope, session)
	if _, err := wire.Decode(wire.WorkerUplink, envelope, wire.DefaultMaxMessageBytes); err == nil || !strings.Contains(err.Error(), "packed repeated enum") {
		t.Fatalf("packed section vector accepted: %v", err)
	}
}

func TestSessionRejectionsAreTerminal(t *testing.T) {
	for _, code := range []circusv1.CallUpdate_Rejected_Code{circusv1.CallUpdate_Rejected_CODE_SESSION_FENCED, circusv1.CallUpdate_Rejected_CODE_SESSION_STOPPED} {
		u := &circusv1.CallUpdate{CallId: "call", Update: &circusv1.CallUpdate_Rejected_{Rejected: &circusv1.CallUpdate_Rejected{Code: code}}}
		if !wire.Terminal(u) {
			t.Errorf("%v must terminate the submission", code)
		}
	}
}

func sessionUplink(op *circusv1.SessionRequest) *circusv1.WorkerUplink {
	op.SessionId, op.Epoch = "session", 1
	return &circusv1.WorkerUplink{RequestId: 1, Payload: &circusv1.WorkerUplink_Session{Session: op}}
}

// PublishEvents is, with AppendTranscript, the only session op allowed past
// 64 KiB; any other op of the same size closes the link.
func TestSessionPublishEventsExemptFromMetadataLimit(t *testing.T) {
	big := strings.Repeat("x", 2*wire.MaxMetadataBytes)
	for _, tc := range []struct {
		name string
		op   *circusv1.SessionRequest
		ok   bool
	}{
		{"publish_events", &circusv1.SessionRequest{Op: &circusv1.SessionRequest_PublishEvents{PublishEvents: &circusv1.PublishEvents{BatchSeq: 1, Events: []*circusv1.PublishEvents_Event{{Kind: circusv1.SessionEvent_KIND_ASSISTANT, Json: big}}}}}, true},
		{"request_approval", &circusv1.SessionRequest{Op: &circusv1.SessionRequest_RequestApproval{RequestApproval: &circusv1.RequestApproval{Display: big}}}, false},
		{"transcript_repaired_unknown_field", &circusv1.SessionRequest{Op: &circusv1.SessionRequest_TranscriptRepaired{TranscriptRepaired: &circusv1.TranscriptRepaired{}}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := proto.Marshal(sessionUplink(tc.op))
			if err != nil {
				t.Fatal(err)
			}
			if tc.name == "transcript_repaired_unknown_field" {
				// An unknown field cannot carry a non-append op past 64 KiB.
				data = protowire.AppendTag(data, 99, protowire.BytesType)
				data = protowire.AppendString(data, big)
			}
			if _, err := wire.Decode(wire.WorkerUplink, data, wire.DefaultMaxMessageBytes); (err == nil) != tc.ok {
				t.Fatalf("Decode error = %v, want success %v", err, tc.ok)
			}
		})
	}
}

func TestNudgeReasonsRejectPackedEncoding(t *testing.T) {
	packed := protowire.AppendTag(nil, 1, protowire.BytesType)
	packed = protowire.AppendString(packed, "session")
	packed = protowire.AppendTag(packed, 2, protowire.BytesType)
	packed = protowire.AppendBytes(packed, []byte{1, 2, 3, 4, 5, 1})
	nudge := protowire.AppendTag(nil, 2, protowire.BytesType)
	nudge = protowire.AppendBytes(nudge, packed)
	envelope := protowire.AppendTag(nil, 13, protowire.BytesType)
	envelope = protowire.AppendBytes(envelope, nudge)
	envelope = protowire.AppendTag(envelope, 20, protowire.VarintType)
	envelope = protowire.AppendVarint(envelope, 1)
	if _, err := wire.Decode(wire.WorkerDownlink, envelope, wire.DefaultMaxMessageBytes); err == nil || !strings.Contains(err.Error(), "packed repeated enum") {
		t.Fatalf("packed Nudge reasons accepted: %v", err)
	}
}

// A SessionRequest carries exactly one op. Protobuf would let the last op win,
// so a large patch followed by an empty append would pass the size check as
// an append and then be discarded.
func TestSessionRequestRejectsSecondOp(t *testing.T) {
	encode := func(op *circusv1.SessionRequest) []byte {
		data, err := proto.Marshal(op)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	patch := encode(&circusv1.SessionRequest{SessionId: "session", Epoch: 1, Op: &circusv1.SessionRequest_PatchState{PatchState: &circusv1.PatchSessionState{ReturnedCallIds: []string{strings.Repeat("x", 200<<10)}}}})
	appendOp := encode(&circusv1.SessionRequest{Op: &circusv1.SessionRequest_AppendTranscript{AppendTranscript: &circusv1.AppendTranscript{}}})
	takeOp := encode(&circusv1.SessionRequest{SessionId: "session", Epoch: 1, Op: &circusv1.SessionRequest_TakeInputs{TakeInputs: &circusv1.TakeInputs{}}})
	for name, session := range map[string][]byte{
		"patch_then_append": append(patch, appendOp...),
		"same_op_twice":     append(append([]byte{}, takeOp...), takeOp[len(takeOp)-2:]...),
	} {
		t.Run(name, func(t *testing.T) {
			envelope := protowire.AppendTag(nil, 15, protowire.BytesType)
			envelope = protowire.AppendBytes(envelope, session)
			envelope = protowire.AppendTag(envelope, 20, protowire.VarintType)
			envelope = protowire.AppendVarint(envelope, 1)
			if _, err := wire.Decode(wire.WorkerUplink, envelope, wire.DefaultMaxMessageBytes); err == nil || !strings.Contains(err.Error(), "multiple session operations") {
				t.Fatalf("second op accepted: %v", err)
			}
		})
	}
	single := protowire.AppendTag(nil, 15, protowire.BytesType)
	single = protowire.AppendBytes(single, takeOp)
	single = protowire.AppendTag(single, 20, protowire.VarintType)
	single = protowire.AppendVarint(single, 1)
	if _, err := wire.Decode(wire.WorkerUplink, single, wire.DefaultMaxMessageBytes); err != nil {
		t.Fatalf("single op rejected: %v", err)
	}
}

// Attach and ResumeLeases rebind hints and leases to the connection; session
// operations and calls resent after a reconnect must not overtake them.
func TestSessionRebindingPayloadsAreBarriers(t *testing.T) {
	for name, m := range map[string]*circusv1.WorkerUplink{
		"attach":        {RequestId: 1, Payload: &circusv1.WorkerUplink_Attach{Attach: &circusv1.Attach{MaxSessions: 1}}},
		"resume_leases": {RequestId: 1, Payload: &circusv1.WorkerUplink_ResumeLeases{ResumeLeases: &circusv1.ResumeLeases{}}},
	} {
		if class, key, err := wire.Classify(m); err != nil || class != wire.Barrier || key != "" {
			t.Errorf("%s: class %v key %q err %v, want keyless Barrier", name, class, key, err)
		}
	}
	m := &circusv1.WorkerUplink{RequestId: 1, Payload: &circusv1.WorkerUplink_ListDevices{ListDevices: &circusv1.ListDevices{}}}
	if class, key, _ := wire.Classify(m); class != wire.Data || key != "" {
		t.Errorf("list_devices: class %v key %q, want keyless Data", class, key)
	}
}

// Keyless metadata payloads of the worker link are bounded by 64 KiB in both
// directions, unlike session ops and replies.
func TestWorkerMetadataPayloadsLimitedTo64KiB(t *testing.T) {
	big := strings.Repeat("x", wire.MaxMetadataBytes)
	for _, tc := range []struct {
		name string
		kind wire.Kind
		m    proto.Message
	}{
		{"attach", wire.WorkerUplink, &circusv1.WorkerUplink{RequestId: 1, Payload: &circusv1.WorkerUplink_Attach{Attach: &circusv1.Attach{MaxSessions: 1, SdkVersion: big}}}},
		{"resume_leases", wire.WorkerUplink, &circusv1.WorkerUplink{RequestId: 1, Payload: &circusv1.WorkerUplink_ResumeLeases{ResumeLeases: &circusv1.ResumeLeases{Leases: []*circusv1.ResumeLeases_Held{{SessionId: big, Epoch: 1}}}}}},
		{"list_devices", wire.WorkerUplink, &circusv1.WorkerUplink{RequestId: 1, Payload: &circusv1.WorkerUplink_ListDevices{ListDevices: &circusv1.ListDevices{}}}},
		{"worker_event", wire.WorkerDownlink, &circusv1.WorkerDownlink{RequestId: 1, Payload: &circusv1.WorkerDownlink_WorkerEvent{WorkerEvent: &circusv1.WorkerEvent{Event: &circusv1.WorkerEvent_Offer_{Offer: &circusv1.WorkerEvent_Offer{SessionId: big}}}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := proto.Marshal(tc.m)
			if err != nil {
				t.Fatal(err)
			}
			if tc.name == "list_devices" {
				// ListDevices has no fields; pad it with an unknown one.
				data = protowire.AppendTag(data, 99, protowire.BytesType)
				data = protowire.AppendString(data, big)
			}
			if len(data) <= wire.MaxMetadataBytes {
				t.Fatalf("test envelope is only %d bytes", len(data))
			}
			if _, err := wire.Decode(tc.kind, data, wire.DefaultMaxMessageBytes); err == nil {
				t.Fatal("over-64 KiB metadata payload accepted")
			}
		})
	}
}
