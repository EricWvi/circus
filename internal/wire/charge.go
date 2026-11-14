package wire

import (
	"fmt"
	"strings"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// repeatedLimits caps the element count of every repeated field in the
// schema, matched by full name. The decoder and Charge both use it.
var repeatedLimits = [...]struct {
	name  protoreflect.FullName
	limit int
}{
	{"circus.v1.JobSnapshot.jobs", MaxSnapshotJobs},
	{"circus.v1.ToolOutput.content", 128},
	{"circus.v1.BashRequest.working_dirs", 256},
	{"circus.v1.CallUpdate.DeviceOffline.online_devices", 256},
}

// repeatedLimit returns the slot of a capped repeated field in
// repeatedLimits and its cap, or -1 and MaxDecodeFields for any other field.
func repeatedLimit(name protoreflect.FullName) (slot, limit int) {
	for i, r := range repeatedLimits {
		if r.name == name {
			return i, r.limit
		}
	}
	return -1, MaxDecodeFields
}

// decodedCost is the one estimate of the live memory a decoded message owns:
// retained copies, objects, pointer/string slices and size classes for an
// encoded size n with the given node and field counts. It is deliberately
// conservative; never rely on proto.Size for retained memory.
func decodedCost(n, nodes, fields int64) int64 { return 2*n + 256*nodes + 64*fields + 4096 }

// Charge validates the tree bounds of a message built in process (depth,
// nodes, fields, repeated-field caps, maxBytes) before Size or cloning, and
// returns its decoded cost. Its recursive walk has depth <= 64 and retains no
// message after return. Received data uses the pre-Unmarshal scanner instead.
func Charge(m proto.Message, maxBytes int) (int64, error) {
	if m == nil || !m.ProtoReflect().IsValid() || maxBytes < 1 {
		return 0, fmt.Errorf("charge: invalid message")
	}
	nodes, fields := int64(0), int64(0)
	payload := int64(0)
	var walk func(protoreflect.Message, int) error
	walk = func(msg protoreflect.Message, depth int) error {
		nodes++
		if depth > 64 || nodes > MaxDecodeNodes {
			return fmt.Errorf("charge: message tree too deep or wide")
		}
		payload += int64(len(msg.GetUnknown()))
		var failure error
		msg.Range(func(f protoreflect.FieldDescriptor, v protoreflect.Value) bool {
			count := 1
			if f.IsList() {
				count = v.List().Len()
			}
			fields += int64(count)
			_, limit := repeatedLimit(f.FullName())
			if count > limit || fields > MaxDecodeFields || f.IsMap() {
				failure = fmt.Errorf("charge: repeated field limit: %s", f.FullName())
				return false
			}
			visit := func(v protoreflect.Value) error {
				switch f.Kind() {
				case protoreflect.MessageKind:
					return walk(v.Message(), depth+1)
				case protoreflect.StringKind:
					payload += int64(len(v.String()))
				case protoreflect.BytesKind:
					payload += int64(len(v.Bytes()))
				}
				return nil
			}
			if f.IsList() {
				for i := 0; i < count; i++ {
					if failure = visit(v.List().Get(i)); failure != nil {
						break
					}
				}
			} else {
				failure = visit(v)
			}
			if payload > int64(maxBytes) {
				failure = ErrTooLarge
			}
			return failure == nil
		})
		return failure
	}
	if err := walk(m.ProtoReflect(), 0); err != nil {
		return 0, err
	}
	n := proto.Size(m)
	if n > maxBytes {
		return 0, ErrTooLarge
	}
	return decodedCost(int64(n), nodes, fields), nil
}

// CloneOwned copies a message whose tree has already passed Charge, including
// string backing storage. proto.Clone alone can retain an arbitrarily large
// caller-owned backing string through a tiny substring. The bounded walk owns
// only the cloned protobuf tree and at most 64 stack frames.
func CloneOwned(m proto.Message) proto.Message {
	out := proto.Clone(m)
	var detach func(protoreflect.Message)
	detach = func(msg protoreflect.Message) {
		msg.Range(func(f protoreflect.FieldDescriptor, v protoreflect.Value) bool {
			if f.IsList() {
				list := v.List()
				for i := 0; i < list.Len(); i++ {
					if f.Kind() == protoreflect.StringKind {
						list.Set(i, protoreflect.ValueOfString(strings.Clone(list.Get(i).String())))
					} else if f.Kind() == protoreflect.MessageKind {
						detach(list.Get(i).Message())
					}
				}
			} else if f.Kind() == protoreflect.StringKind {
				msg.Set(f, protoreflect.ValueOfString(strings.Clone(v.String())))
			} else if f.Kind() == protoreflect.MessageKind {
				detach(v.Message())
			}
			return true
		})
	}
	detach(out.ProtoReflect())
	return out
}

// Terminal reports whether a CallUpdate is an actual call outcome. Unknown
// and the operation-scoped NOT_FOUND answer do not settle a call.
func Terminal(u *circusv1.CallUpdate) bool {
	if r := u.GetRejected(); r != nil && r.GetCode() == circusv1.CallUpdate_Rejected_CODE_NOT_FOUND {
		return false
	}
	return u.GetResult() != nil || u.GetOffline() != nil || u.GetLost() != nil || u.GetRejected() != nil || u.GetCancelled() != nil
}
