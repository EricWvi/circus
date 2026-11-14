package wire

import (
	"crypto/sha256"
	"fmt"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// classify is the one scheduling table: Send's class and ordering key for
// every envelope payload. Raw handshake and transport-owned Fragment are
// classified too, but must still be sent on the raw Conn or by wire itself. Job event keys are boot-wide; worker
// events include device and boot because one worker link multiplexes both.
// Call IDs are scoped to the link's principal; keys are not reused across links.
func classify(m proto.Message) (Class, string, error) {
	if m == nil || !m.ProtoReflect().IsValid() {
		return 0, "", fmt.Errorf("wire: missing envelope")
	}
	var name protoreflect.Name
	switch m.(type) {
	case *circusv1.DaemonUplink, *circusv1.DaemonDownlink, *circusv1.WorkerUplink, *circusv1.WorkerDownlink:
	default:
		return 0, "", fmt.Errorf("wire: unsupported envelope")
	}
	r := m.ProtoReflect()
	f := r.WhichOneof(r.Descriptor().Oneofs().ByName("payload"))
	if f == nil {
		return 0, "", fmt.Errorf("wire: missing payload")
	}
	name = f.Name()
	switch name {
	case "heartbeat", "go_away", "call_ack":
		return Control, "", nil
	case "list_jobs", "stop_session_jobs":
		return Barrier, "", nil
	case "job_event":
		if e, ok := m.(*circusv1.WorkerDownlink); ok {
			j := e.GetJobEvent()
			// A device alias has no protocol length cap of its own. Hash the
			// length-delimited identity so legal metadata cannot exceed wire key limits.
			identity := fmt.Sprintf("%d:%s/%s", len(j.GetDevice()), j.GetDevice(), j.GetBootId())
			return Data, fmt.Sprintf("jobs/%x", sha256.Sum256([]byte(identity))), nil
		}
		return Data, "jobs/boot", nil
	case "dispatch_call":
		return Data, "call/" + m.(*circusv1.DaemonDownlink).GetDispatchCall().GetCall().GetCallId(), nil
	case "submit_call":
		return Data, "call/" + m.(*circusv1.WorkerUplink).GetSubmitCall().GetCall().GetCallId(), nil
	case "cancel_call", "call_query":
		b := r.Get(f).Message()
		id := b.Descriptor().Fields().ByName("call_id")
		return Data, "call/" + b.Get(id).String(), nil
	case "call_update":
		return Data, "call/" + m.(*circusv1.WorkerDownlink).GetCallUpdate().GetCallId(), nil
	case "hello", "welcome", "reject", "fragment", "result", "call_status", "job_snapshot":
		return Data, "", nil
	default:
		return 0, "", fmt.Errorf("wire: unclassified payload %s", f.FullName())
	}
}
