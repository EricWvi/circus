// Package wirepolicy defines the shared M2 scheduling table for link callers.
// It supplies no routing, sessions, persistence or daemon execution behavior.
package wirepolicy

import (
	"crypto/sha256"
	"fmt"
	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/wire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Classify supplies wire.Send's class and key for every envelope payload. Raw
// handshake and transport-owned Fragment are classified too, but must still be
// sent on the raw Conn or by wire itself. Job event keys are boot-wide; worker
// events include device and boot because one worker link multiplexes both.
// Call IDs are scoped to the link's principal; keys are not reused across links.
func Classify(m proto.Message) (wire.Class, string, error) {
	if m == nil || !m.ProtoReflect().IsValid() {
		return 0, "", fmt.Errorf("wirepolicy: missing envelope")
	}
	var name protoreflect.Name
	switch m.(type) {
	case *circusv1.DaemonUplink, *circusv1.DaemonDownlink, *circusv1.WorkerUplink, *circusv1.WorkerDownlink:
	default:
		return 0, "", fmt.Errorf("wirepolicy: unsupported envelope")
	}
	r := m.ProtoReflect()
	f := r.WhichOneof(r.Descriptor().Oneofs().ByName("payload"))
	if f == nil {
		return 0, "", fmt.Errorf("wirepolicy: missing payload")
	}
	name = f.Name()
	switch name {
	case "heartbeat", "go_away", "call_ack":
		return wire.Control, "", nil
	case "list_jobs", "stop_session_jobs":
		return wire.Barrier, "", nil
	case "job_event":
		if e, ok := m.(*circusv1.WorkerDownlink); ok {
			j := e.GetJobEvent()
			// A device alias has no protocol length cap of its own. Hash the
			// length-delimited identity so legal metadata cannot exceed wire key limits.
			identity := fmt.Sprintf("%d:%s/%s", len(j.GetDevice()), j.GetDevice(), j.GetBootId())
			return wire.Data, fmt.Sprintf("jobs/%x", sha256.Sum256([]byte(identity))), nil
		}
		return wire.Data, "jobs/boot", nil
	case "dispatch_call":
		return wire.Data, "call/" + m.(*circusv1.DaemonDownlink).GetDispatchCall().GetCall().GetCallId(), nil
	case "submit_call":
		return wire.Data, "call/" + m.(*circusv1.WorkerUplink).GetSubmitCall().GetCall().GetCallId(), nil
	case "cancel_call", "call_query":
		b := r.Get(f).Message()
		id := b.Descriptor().Fields().ByName("call_id")
		return wire.Data, "call/" + b.Get(id).String(), nil
	case "call_update":
		return wire.Data, "call/" + m.(*circusv1.WorkerDownlink).GetCallUpdate().GetCallId(), nil
	case "hello", "welcome", "reject", "fragment", "result", "call_status", "job_snapshot":
		return wire.Data, "", nil
	default:
		return 0, "", fmt.Errorf("wirepolicy: unclassified payload %s", f.FullName())
	}
}
