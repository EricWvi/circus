package wire

import (
	"fmt"
	"google.golang.org/protobuf/encoding/protowire"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Kind selects the role and direction of a serialized envelope.
type Kind uint8

const (
	DaemonUplink Kind = iota + 1
	DaemonDownlink
	WorkerUplink
	WorkerDownlink
)

func (k Kind) newMessage() proto.Message {
	switch k {
	case DaemonUplink:
		return &circusv1.DaemonUplink{}
	case DaemonDownlink:
		return &circusv1.DaemonDownlink{}
	case WorkerUplink:
		return &circusv1.WorkerUplink{}
	case WorkerDownlink:
		return &circusv1.WorkerDownlink{}
	default:
		return nil
	}
}

// Transport resource limits, independent of application ID validation.
const (
	MaxMetadataBytes = 64 << 10
	MaxSnapshotBytes = 8 << 20
	MaxSnapshotJobs  = 4096
	MaxDecodeNodes   = 8192
	MaxDecodeFields  = 65536
)

// Decode is for a bounded raw handshake or envelope. It scans wire structure
// before allocating generated messages. Unknown non-group fields are retained;
// duplicate envelope payloads, groups and excessive structure are rejected.
// Link applies its negotiated fragment limit; raw Decode uses the default.
func Decode(k Kind, data []byte, maxBytes int) (proto.Message, error) {
	m, _, err := decode(k, data, maxBytes, DefaultMaxFragmentBytes)
	return m, err
}

type decodeBudget struct {
	nodes, fields, jobs, blocks, dirs, devices int
	resultBytes                                int
}

func (b decodeBudget) cost(n int) int {
	// Retained copies, objects, pointer/string slices and size classes. Temporary
	// decoder slice growth is accounted separately in the package memory model.
	// Deliberately conservative; never rely on proto.Size for retained memory.
	return 2*n + 256*b.nodes + 64*b.fields + 4096
}
func maxMessageCost(n int) int { return 2*n + 256*MaxDecodeNodes + 64*MaxDecodeFields + 4096 }

func payloadLimit(name protoreflect.Name, fragmentBytes, messageBytes int) int {
	switch name {
	case "dispatch_call", "submit_call", "result", "call_status", "call_ack", "call_update":
		return messageBytes
	case "job_snapshot":
		return MaxSnapshotBytes
	case "fragment":
		return fragmentBytes + FrameOverhead
	default:
		return MaxMetadataBytes
	}
}
func scan(k Kind, data []byte, maxBytes, fragmentBytes int) (decodeBudget, error) {
	var budget decodeBudget
	m := k.newMessage()
	if m == nil || maxBytes <= 0 || len(data) > maxBytes {
		return budget, protocol("invalid kind or envelope size")
	}
	descriptor := m.ProtoReflect().Descriptor()
	payloads := 0
	var name protoreflect.Name
	rest := data
	for len(rest) > 0 {
		number, typ, n := protowire.ConsumeTag(rest)
		if n < 0 {
			return budget, protocol("invalid envelope tag")
		}
		rest = rest[n:]
		length := protowire.ConsumeFieldValue(number, typ, rest)
		if length < 0 {
			return budget, protocol("invalid envelope field")
		}
		if field := descriptor.Fields().ByNumber(protoreflect.FieldNumber(number)); field != nil {
			name = field.Name()
			payloads++
			if payloads > 1 || typ != protowire.BytesType || len(data) > payloadLimit(field.Name(), fragmentBytes, maxBytes) {
				return budget, protocol("payload size or multiplicity limit")
			}
		}
		rest = rest[length:]
	}
	if payloads != 1 {
		return budget, protocol("missing envelope payload")
	}
	if err := scanMessage(descriptor, data, 0, &budget); err != nil {
		return budget, err
	}
	switch name {
	case "call_status", "call_ack", "call_update":
		// Completed statuses, duplicate acks and worker updates may contain a
		// full DeviceResult. Only that embedded output is exempt from the tight
		// metadata allowance; presence of an empty result cannot bypass it.
		if len(data)-budget.resultBytes > MaxMetadataBytes {
			return budget, protocol("status metadata size limit")
		}
	}
	return budget, nil
}
func scanMessage(d protoreflect.MessageDescriptor, data []byte, depth int, b *decodeBudget) error {
	b.nodes++
	if depth >= 32 || b.nodes > MaxDecodeNodes {
		return protocol("message element limit")
	}
	for len(data) > 0 {
		number, typ, n := protowire.ConsumeTag(data)
		if n < 0 || number <= 0 || typ == protowire.StartGroupType || typ == protowire.EndGroupType {
			return protocol("invalid protobuf tag or group")
		}
		data = data[n:]
		b.fields++
		if b.fields > MaxDecodeFields {
			return protocol("field count limit")
		}
		field := d.Fields().ByNumber(protoreflect.FieldNumber(number))
		size := protowire.ConsumeFieldValue(number, typ, data)
		if size < 0 {
			return protocol("malformed protobuf field")
		}
		if field != nil {
			if field.IsList() {
				switch field.FullName() {
				case "circus.v1.JobSnapshot.jobs":
					b.jobs++
					if b.jobs > MaxSnapshotJobs {
						return protocol("snapshot job limit")
					}
				case "circus.v1.ToolOutput.content":
					b.blocks++
					if b.blocks > 128 {
						return protocol("content block limit")
					}
				case "circus.v1.BashRequest.working_dirs":
					b.dirs++
					if b.dirs > 256 {
						return protocol("working directory limit")
					}
				case "circus.v1.CallUpdate.DeviceOffline.online_devices":
					b.devices++
					if b.devices > 256 {
						return protocol("online device limit")
					}
				}
			}
			if field.Message() != nil {
				if typ != protowire.BytesType {
					return protocol("invalid message wire type")
				}
				inner, n := protowire.ConsumeBytes(data)
				if n < 0 {
					return protocol("malformed message")
				}
				if field.Message().FullName() == "circus.v1.JobEvent" && len(inner) > MaxMetadataBytes {
					return protocol("job event size limit")
				}
				if field.Message().FullName() == "circus.v1.DeviceResult" {
					b.resultBytes += len(inner)
				}
				if err := scanMessage(field.Message(), inner, depth+1, b); err != nil {
					return err
				}
			}
		}
		data = data[size:]
	}
	return nil
}
func decode(k Kind, data []byte, maxBytes, fragmentBytes int) (proto.Message, int, error) {
	b, err := scan(k, data, maxBytes, fragmentBytes)
	if err != nil {
		return nil, 0, err
	}
	m := k.newMessage()
	if err := (proto.UnmarshalOptions{RecursionLimit: 32}).Unmarshal(data, m); err != nil {
		return nil, 0, protocol("malformed protobuf: %v", err)
	}
	return m, b.cost(len(data)), nil
}

func payload(m proto.Message) protoreflect.FieldDescriptor {
	if m == nil || !m.ProtoReflect().IsValid() {
		return nil
	}
	r := m.ProtoReflect()
	o := r.Descriptor().Oneofs().ByName("payload")
	if o == nil {
		return nil
	}
	return r.WhichOneof(o)
}

func body(m proto.Message) proto.Message {
	f := payload(m)
	if f == nil {
		return nil
	}
	return m.ProtoReflect().Get(f).Message().Interface()
}

func wrap(k Kind, name protoreflect.Name, b proto.Message) proto.Message {
	m := k.newMessage()
	r := m.ProtoReflect()
	f := r.Descriptor().Fields().ByName(name)
	r.Set(f, protoreflect.ValueOfMessage(b.ProtoReflect()))
	return m
}

// ProtocolError indicates malformed framing; the connection cannot be reused.
type ProtocolError struct{ Reason string }

func (e *ProtocolError) Error() string { return "wire protocol: " + e.Reason }
func protocol(format string, args ...any) error {
	return &ProtocolError{Reason: fmt.Sprintf(format, args...)}
}
