package wire

import (
	"testing"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/dynamicpb"
)

// Every repeatedLimits entry names a real repeated field, and every repeated
// field reachable from an envelope has an entry: a misspelt name would lift
// the cap from both the decoder and Charge.
func TestRepeatedLimitsMatchSchema(t *testing.T) {
	inTable := map[protoreflect.FullName]bool{}
	for _, r := range repeatedLimits {
		d, err := protoregistry.GlobalFiles.FindDescriptorByName(r.name)
		if err != nil {
			t.Errorf("%s: %v", r.name, err)
			continue
		}
		if f, ok := d.(protoreflect.FieldDescriptor); !ok || !f.IsList() {
			t.Errorf("%s is not a repeated field", r.name)
		}
		inTable[r.name] = true
	}
	seen := map[protoreflect.FullName]bool{}
	var walk func(protoreflect.MessageDescriptor)
	walk = func(d protoreflect.MessageDescriptor) {
		if seen[d.FullName()] {
			return
		}
		seen[d.FullName()] = true
		for i := 0; i < d.Fields().Len(); i++ {
			f := d.Fields().Get(i)
			if f.IsList() && !inTable[f.FullName()] {
				t.Errorf("repeated field %s has no entry in repeatedLimits", f.FullName())
			}
			if f.Message() != nil {
				walk(f.Message())
			}
		}
	}
	for _, m := range []proto.Message{&circusv1.DaemonUplink{}, &circusv1.DaemonDownlink{}, &circusv1.WorkerUplink{}, &circusv1.WorkerDownlink{}, &circusv1.ListSessionsResponse{}, &circusv1.ListApprovalsResponse{}} {
		walk(m.ProtoReflect().Descriptor())
	}
}

// Exercise each registered cap at its boundary, using the shortest envelope
// path to the field. This checks both the pre-allocation scanner and Charge.
func TestRepeatedLimitBoundaries(t *testing.T) {
	for _, rule := range repeatedLimits {
		t.Run(string(rule.name), func(t *testing.T) {
			var root proto.Message
			var path []protoreflect.FieldDescriptor
			var kind Kind
			for _, k := range []Kind{DaemonUplink, DaemonDownlink, WorkerUplink, WorkerDownlink} {
				m := k.newMessage()
				if p := pathToField(m.ProtoReflect().Descriptor(), rule.name, map[protoreflect.FullName]bool{}); p != nil {
					root, path, kind = m, p, k
					break
				}
			}
			if root == nil {
				// User-facing RPC responses are not link envelopes.
				d, err := protoregistry.GlobalFiles.FindDescriptorByName(rule.name)
				if err != nil {
					t.Fatal(err)
				}
				f := d.(protoreflect.FieldDescriptor)
				root = dynamicpb.NewMessage(f.ContainingMessage())
				path = []protoreflect.FieldDescriptor{f}
			}
			m := root.ProtoReflect()
			for _, f := range path[:len(path)-1] {
				if f.IsList() {
					m = m.Mutable(f).List().AppendMutable().Message()
				} else {
					m = m.Mutable(f).Message()
				}
			}
			list := m.Mutable(path[len(path)-1]).List()
			for range rule.limit {
				list.Append(list.NewElement())
			}
			if _, err := Charge(root, DefaultMaxMessageBytes); err != nil {
				t.Fatalf("boundary rejected by Charge: %v", err)
			}
			if kind != 0 {
				data, err := proto.Marshal(root)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := Decode(kind, data, DefaultMaxMessageBytes); err != nil {
					t.Fatalf("boundary rejected by Decode: %v", err)
				}
			}
			list.Append(list.NewElement())
			if _, err := Charge(root, DefaultMaxMessageBytes); err == nil {
				t.Fatal("Charge accepted excess elements")
			}
			if kind != 0 {
				data, err := proto.Marshal(root)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := Decode(kind, data, DefaultMaxMessageBytes); err == nil {
					t.Fatal("Decode accepted excess elements")
				}
			}
		})
	}
}

func pathToField(d protoreflect.MessageDescriptor, target protoreflect.FullName, seen map[protoreflect.FullName]bool) []protoreflect.FieldDescriptor {
	if seen[d.FullName()] {
		return nil
	}
	seen[d.FullName()] = true
	defer delete(seen, d.FullName())
	var best []protoreflect.FieldDescriptor
	for i := 0; i < d.Fields().Len(); i++ {
		f := d.Fields().Get(i)
		if f.FullName() == target {
			return []protoreflect.FieldDescriptor{f}
		}
		if f.Message() != nil {
			if path := pathToField(f.Message(), target, seen); path != nil && (best == nil || len(path)+1 < len(best)) {
				best = append([]protoreflect.FieldDescriptor{f}, path...)
			}
		}
	}
	return best
}

// The caps are copied literally from docs/wire.md section 5 ("Structure
// limits"), so changing a value in repeatedLimits without the document, or
// the reverse, fails here. Do not derive these numbers from the table.
func TestRepeatedLimitsMatchWireDoc(t *testing.T) {
	want := map[protoreflect.FullName]int{
		"circus.v1.JobSnapshot.jobs":                        4096,
		"circus.v1.ToolOutput.content":                      128,
		"circus.v1.BashRequest.working_dirs":                256,
		"circus.v1.CallUpdate.DeviceOffline.online_devices": 256,
		"circus.v1.WorkerEvent.Nudge.reasons":               5,
		"circus.v1.GetSessionState.sections":                5,
		"circus.v1.SessionConfig.cwds":                      256,
		"circus.v1.PatchSessionState.cwds":                  256,
		"circus.v1.SessionState.cwds":                       256,
		"circus.v1.PatchSessionState.files":                 4096,
		"circus.v1.SessionState.files":                      4096,
		"circus.v1.AppendTranscript.entries":                8189,
		"circus.v1.SessionReply.TranscriptPage.entries":     8189,
		"circus.v1.ReportTurn.Persisted.input_ids":          32,
		"circus.v1.SessionReply.Inputs.inputs":              32,
		"circus.v1.ReportTurn.Persisted.call_ids":           1024,
		"circus.v1.ReportTurn.Persisted.jobs":               1024,
		"circus.v1.ReportTurn.Persisted.late_call_ids":      16,
		"circus.v1.PatchSessionState.awaiting_call_ids":     16,
		"circus.v1.PatchSessionState.returned_call_ids":     1024,
		"circus.v1.PatchSessionState.handed_jobs":           1024,
		"circus.v1.PatchSessionState.bgtasks_started":       64,
		"circus.v1.PatchSessionState.bgtasks_ended":         64,
		"circus.v1.SessionState.jobs":                       1024,
		"circus.v1.SessionState.calls":                      1040,
		"circus.v1.Recovery.calls":                          1040,
		"circus.v1.SessionState.grants":                     32,
		"circus.v1.Recovery.lost_bgtasks":                   64,
		"circus.v1.PublishEvents.events":                    128,
		"circus.v1.ResumeLeases.leases":                     64,
		"circus.v1.SessionReply.Resumed.leases":             64,
		"circus.v1.SessionReply.TranscriptKeys.subpaths":    1024,
		"circus.v1.SessionReply.Devices.devices":            256,
		"circus.v1.ListSessionsResponse.sessions":           1000,
		"circus.v1.ListApprovalsResponse.approvals":         32,
	}
	got := map[protoreflect.FullName]int{}
	for _, r := range repeatedLimits {
		got[r.name] = r.limit
	}
	for name, limit := range want {
		if got[name] != limit {
			t.Errorf("%s: cap %d, wire.md says %d", name, got[name], limit)
		}
	}
	for name, limit := range got {
		if _, ok := want[name]; !ok {
			t.Errorf("%s: cap %d is not in wire.md", name, limit)
		}
	}
}
