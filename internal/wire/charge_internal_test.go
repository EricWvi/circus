package wire

import (
	"testing"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
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
	for _, m := range []proto.Message{&circusv1.DaemonUplink{}, &circusv1.DaemonDownlink{}, &circusv1.WorkerUplink{}, &circusv1.WorkerDownlink{}} {
		walk(m.ProtoReflect().Descriptor())
	}
}
