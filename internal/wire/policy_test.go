package wire_test

import (
	"testing"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/wire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func TestEnvelopeSchemaAndPolicyCoverage(t *testing.T) {
	// codec.go charges 256 bytes/node and 64 bytes/field. Packed scalars and
	// maps require element-aware costs; update those constants/scan before adding
	// either. Repeated-limit coverage is checked in TestRepeatedLimitsMatchSchema.
	seen := map[protoreflect.FullName]bool{}
	var walk func(protoreflect.MessageDescriptor)
	walk = func(d protoreflect.MessageDescriptor) {
		if seen[d.FullName()] {
			return
		}
		seen[d.FullName()] = true
		for i := 0; i < d.Fields().Len(); i++ {
			f := d.Fields().Get(i)
			if f.IsMap() || f.IsPacked() {
				t.Errorf("update scanner cost for %s", f.FullName())
			}
			if f.Message() != nil {
				walk(f.Message())
			}
		}
	}
	for _, m := range []proto.Message{&circusv1.DaemonUplink{}, &circusv1.DaemonDownlink{}, &circusv1.WorkerUplink{}, &circusv1.WorkerDownlink{}} {
		r := m.ProtoReflect()
		walk(r.Descriptor())
		fields := r.Descriptor().Fields()
		for i := 0; i < fields.Len(); i++ {
			f := fields.Get(i)
			if f.ContainingOneof() == nil {
				continue // envelope metadata (request_id), not a payload
			}
			body := r.NewField(f).Message()
			// A distinct, nonempty call ID per payload catches a constant key.
			callID := string(r.Descriptor().Name()) + "/" + string(f.Name())
			holder := body
			if inner := body.Descriptor().Fields().ByName("call"); inner != nil && inner.Message() != nil {
				holder = body.Mutable(inner).Message()
			}
			if id := holder.Descriptor().Fields().ByName("call_id"); id != nil {
				holder.Set(id, protoreflect.ValueOfString(callID))
			}
			if id := body.Descriptor().Fields().ByName("session_id"); f.Name() == "session" && id != nil {
				body.Set(id, protoreflect.ValueOfString(callID))
			}
			queryID := string(f.Name()) + "-test"
			if f.Name() == "device_query" || f.Name() == "device_query_result" {
				body.Set(body.Descriptor().Fields().ByName("request_id"), protoreflect.ValueOfString(queryID))
			}
			r.Set(f, protoreflect.ValueOfMessage(body))
			class, key, err := wire.Classify(m)
			expected := wire.Data
			switch f.Name() {
			case "heartbeat", "go_away", "call_ack":
				expected = wire.Control
			case "list_jobs", "stop_session_jobs", "unwatch", "attach", "resume_leases":
				expected = wire.Barrier
			}
			if class != expected {
				t.Errorf("class for %s: %v", f.FullName(), class)
			}
			switch f.Name() {
			case "device_query", "device_query_result":
				if key != "device_query/"+queryID {
					t.Errorf("device query ordering key for %s: %q", f.FullName(), key)
				}
			case "session":
				if key != "session/"+callID {
					t.Errorf("session ordering key for %s: %q", f.FullName(), key)
				}
			case "attach", "resume_leases", "list_devices", "session_reply", "worker_event":
				if key != "" {
					t.Errorf("unexpected ordering key for %s: %q", f.FullName(), key)
				}
			case "dispatch_call", "submit_call", "cancel_call", "call_query", "call_update":
				if key != "call/"+callID {
					t.Errorf("call ordering key for %s: %q, want %q", f.FullName(), key, "call/"+callID)
				}
			}
			if err != nil {
				t.Errorf("%s: %v", f.FullName(), err)
			}
		}
	}
	if _, _, err := wire.Classify(&circusv1.DeviceResult{}); err == nil {
		t.Fatal("non-envelope classified")
	}
}
