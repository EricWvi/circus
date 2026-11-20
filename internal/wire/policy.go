package wire

import (
	"fmt"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/identifier"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// FeatureListDirectory is the daemon Hello capability for directory queries.
const FeatureListDirectory = "device_query.list_directory"

// classify is the one scheduling table: Send's class and ordering key for
// every envelope payload. Raw handshake and transport-owned Fragment are
// classified too, but must still be sent on the raw Conn or by wire itself.
// Daemon job event keys are boot-wide; session operations share a session key.
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
	case "list_jobs", "stop_session_jobs", "unwatch", "attach", "resume_leases":
		// Unwatch frees a slot the client may reuse at once, so no later
		// operation may overtake it. Attach and ResumeLeases rebind the
		// connection's hints and leases: session operations and calls resent
		// after a reconnect must not overtake them, or they are fenced.
		return Barrier, "", nil
	case "job_event":
		return Data, "jobs/boot", nil
	case "dispatch_call":
		return Data, "call/" + m.(*circusv1.DaemonDownlink).GetDispatchCall().GetCall().GetCallId(), nil
	case "submit_call":
		return Data, "call/" + m.(*circusv1.WorkerUplink).GetSubmitCall().GetCall().GetCallId(), nil
	case "cancel_call", "call_query":
		b := r.Get(f).Message()
		id := b.Descriptor().Fields().ByName("call_id")
		return Data, "call/" + b.Get(id).String(), nil
	case "device_query", "device_query_result":
		b := r.Get(f).Message()
		id := b.Get(b.Descriptor().Fields().ByName("request_id")).String()
		if !identifier.Valid(id, false) {
			return Data, "", nil
		}
		return Data, "device_query/" + id, nil
	case "call_update":
		return Data, "call/" + m.(*circusv1.WorkerDownlink).GetCallUpdate().GetCallId(), nil
	case "session":
		return Data, "session/" + m.(*circusv1.WorkerUplink).GetSession().GetSessionId(), nil
	case "list_devices", "session_reply", "worker_event":
		return Data, "", nil
	case "hello", "welcome", "reject", "fragment", "result", "call_status", "job_snapshot":
		return Data, "", nil
	default:
		return 0, "", fmt.Errorf("wire: unclassified payload %s", f.FullName())
	}
}
