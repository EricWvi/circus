package wire_test

import (
	"strings"
	"testing"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/wire"
	"github.com/EricWvi/circus/internal/wirepolicy"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func TestEnvelopeSchemaAndPolicyCoverage(t *testing.T) {
	// codec.go charges 256 bytes/node and 64 bytes/field. Packed scalars and
	// maps require element-aware costs; update those constants/scan before adding
	// either. New repeated fields need an explicit element limit as well.
	seen := map[protoreflect.FullName]bool{}
	repeated := map[protoreflect.FullName]bool{"circus.v1.JobSnapshot.jobs": true, "circus.v1.ToolOutput.content": true, "circus.v1.BashRequest.working_dirs": true, "circus.v1.CallUpdate.DeviceOffline.online_devices": true}
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
			if f.IsList() && !repeated[f.FullName()] {
				t.Errorf("add scanner limit for %s", f.FullName())
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
			r.Set(f, protoreflect.ValueOfMessage(body))
			class, key, err := wirepolicy.Classify(m)
			expected := wire.Data
			switch f.Name() {
			case "heartbeat", "go_away", "call_ack":
				expected = wire.Control
			case "list_jobs", "stop_session_jobs":
				expected = wire.Barrier
			}
			if class != expected {
				t.Errorf("class for %s: %v", f.FullName(), class)
			}
			switch f.Name() {
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
	if _, _, err := wirepolicy.Classify(&circusv1.DeviceResult{}); err == nil {
		t.Fatal("non-envelope classified")
	}
}

func TestWorkerJobOrderingIdentity(t *testing.T) {
	event := func(device, boot string) proto.Message {
		return &circusv1.WorkerDownlink{Payload: &circusv1.WorkerDownlink_JobEvent{JobEvent: &circusv1.WorkerJobEvent{Device: device, BootId: boot, Event: &circusv1.JobEvent{}}}}
	}
	_, first, err := wirepolicy.Classify(event(strings.Repeat("d", 2048), "boot1"))
	if err != nil || len(first) > wire.MaxOrderingKeyBytes {
		t.Fatal("unbounded job key", len(first), err)
	}
	_, again, _ := wirepolicy.Classify(event(strings.Repeat("d", 2048), "boot1"))
	_, nextBoot, _ := wirepolicy.Classify(event(strings.Repeat("d", 2048), "boot2"))
	_, otherDevice, _ := wirepolicy.Classify(event("other", "boot1"))
	if first != again || first == nextBoot || first == otherDevice {
		t.Fatal("worker jobs lost device/boot ordering identity")
	}
}
