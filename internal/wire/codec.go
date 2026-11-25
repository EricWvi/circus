package wire

import (
	"fmt"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"google.golang.org/protobuf/encoding/protowire"
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
	MaxMetadataBytes        = 64 << 10
	MaxSnapshotBytes        = 8 << 20
	MaxSnapshotJobs         = 4096
	MaxDirectoryResultBytes = 4 << 20
	MaxDirectoryEntries     = 1000
	// MaxLedgerSnapshotFiles caps the files of one ledger snapshot call
	// or result: a project holds at most 4096 decisions.
	MaxLedgerSnapshotFiles = 4096
	MaxDecodeNodes         = 8192
	MaxDecodeFields        = 65536
	// MaxTranscriptBatchBytes bounds the encoded entries of one
	// AppendTranscript batch, unless the batch is a single larger entry.
	MaxTranscriptBatchBytes = 8 << 20
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
	nodes, fields int
	repeated      [len(repeatedLimits)]int
	resultBytes   int
	sessionOp     protoreflect.Name
	appendBytes   int
	appendEntries int
}

func (b decodeBudget) cost(n int) int {
	return int(decodedCost(int64(n), int64(b.nodes), int64(b.fields)))
}
func maxMessageCost(n int) int {
	return int(decodedCost(int64(n), MaxDecodeNodes, MaxDecodeFields))
}

func payloadLimit(name protoreflect.Name, fragmentBytes, messageBytes int) int {
	switch name {
	case "dispatch_call", "submit_call", "result", "call_status", "call_ack", "call_update", "session", "session_reply":
		return messageBytes
	case "job_snapshot":
		return MaxSnapshotBytes
	case "device_query_result":
		return MaxDirectoryResultBytes
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
		// Only members of the payload oneof are payloads; other known envelope
		// fields (the worker link's request_id) are checked by scanMessage.
		if field := descriptor.Fields().ByNumber(protoreflect.FieldNumber(number)); field != nil && field.ContainingOneof() != nil {
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
	if name == "session" {
		// Only transcript appends and event batches may exceed metadata size.
		if budget.sessionOp != "append_transcript" && budget.sessionOp != "publish_events" && len(data) > MaxMetadataBytes {
			return budget, protocol("session metadata size limit")
		}
		if budget.appendBytes > MaxTranscriptBatchBytes && budget.appendEntries != 1 {
			return budget, protocol("transcript batch size limit")
		}
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
			if d.FullName() == "circus.v1.SessionRequest" && field.ContainingOneof() != nil {
				// Exactly one op: protobuf would let a later op replace an
				// earlier one, so an oversized patch could pass as an append.
				if b.sessionOp != "" {
					return protocol("multiple session operations")
				}
				b.sessionOp = field.Name()
			}
			if field.FullName() == "circus.v1.AppendTranscript.entries" {
				b.appendEntries++
				b.appendBytes += n + size
			}
			if field.IsList() {
				// Generated enums are unpacked. Reject packed input too, rather
				// than counting a whole packed vector as one element.
				if field.Kind() == protoreflect.EnumKind && typ != protowire.VarintType {
					return protocol("packed repeated enum is not supported")
				}
				if slot, limit := repeatedLimit(field.FullName()); slot >= 0 {
					b.repeated[slot]++
					if b.repeated[slot] > limit {
						return protocol("repeated field limit: %s", field.FullName())
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
