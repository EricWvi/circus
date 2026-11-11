package wire

import (
	"bytes"
	"encoding/binary"
	"google.golang.org/protobuf/reflect/protoreflect"
	"runtime"
	"strings"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"google.golang.org/protobuf/proto"
)

// referenceAssembly deliberately uses independent strings/counters instead of
// the production capacity-based buffers. Inputs and retained data are bounded.
type referencePartial struct {
	value        string
	total, index int
}
type referenceAssembly struct {
	high    uint64
	pending map[uint64]referencePartial
}

func (r *referenceAssembly) add(f *circusv1.Fragment) (string, bool) {
	if f.MessageId == 0 || len(f.Data) < 1 || len(f.Data) > 64 {
		return "", false
	}
	p, exists := r.pending[f.MessageId]
	if !exists {
		if f.Index != 0 || f.MessageId <= r.high || f.TotalSize < 1 || f.TotalSize > 512 {
			return "", false
		}
		used := 0
		for _, v := range r.pending {
			used += v.total
		}
		if len(r.pending) >= 8 || int(f.TotalSize)+used > 1024 {
			return "", false
		}
		p = referencePartial{total: int(f.TotalSize)}
	} else if f.TotalSize != 0 {
		return "", false
	}
	if int(f.Index) != p.index || len(p.value)+len(f.Data) > p.total {
		return "", false
	}
	value := p.value + string(f.Data)
	if f.Last != (len(value) == p.total) {
		return "", false
	}
	if !exists {
		r.high = f.MessageId
	}
	if f.Last {
		delete(r.pending, f.MessageId)
		return value, true
	}
	p.value = value
	p.index++
	r.pending[f.MessageId] = p
	return "", true
}

func FuzzReassembly(f *testing.F) {
	// Record: id, index, last flag, uint16 total, uint8 data length, data.
	f.Add([]byte{1, 0, 0, 6, 0, 3, 'a', 'b', 'c', 1, 1, 1, 0, 0, 3, 'd', 'e', 'f'})
	f.Add([]byte{1, 0, 0, 4, 0, 1, 'a', 2, 0, 1, 2, 0, 2, 'b', 'c', 1, 1, 1, 0, 0, 3, 'd', 'e', 'f'})
	f.Add([]byte{1, 0, 1, 1, 0, 1, 'x', 1, 0, 1, 1, 0, 1, 'x'})
	f.Fuzz(func(t *testing.T, input []byte) {
		if len(input) > 4096 {
			return
		}
		o, _ := (Options{MaxFragmentBytes: 64, MaxMessageBytes: 512, MaxInFlightBytes: 1024, MaxInFlightMessages: 2}).defaults()
		o.MaxInFlightMessages = 8 // Exercise reassembler counts beyond the link scheduling cap.
		r := newReassembler(o)
		ref := referenceAssembly{pending: make(map[uint64]referencePartial)}
		for n := 0; len(input) >= 6 && n < 128; n++ {
			size := int(input[5])
			if size > len(input)-6 {
				size = len(input) - 6
			}
			fragment := &circusv1.Fragment{MessageId: uint64(input[0]), Index: uint32(input[1]), Last: input[2]&1 != 0, TotalSize: uint64(binary.LittleEndian.Uint16(input[3:5])), Data: input[6 : 6+size]}
			input = input[6+size:]
			want, accepted := ref.add(fragment)
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			got, err := r.add(fragment, time.Unix(int64(n), 0))
			runtime.ReadMemStats(&after)
			// Noise includes leakcheck's concurrent process sampler. 512 KiB is far
			// above this target's 1 KiB payload budget, but detects large allocation bugs.
			if after.TotalAlloc-before.TotalAlloc > 512<<10 {
				t.Fatal("actual reassembly allocation exceeded bound")
			}
			if r.allocated > r.bytes || r.bytes > 1024 {
				t.Fatal("actual block/reservation bound")
			}
			if accepted != (err == nil) {
				t.Fatalf("rejection mismatch: %+v, error %v", fragment, err)
			}
			if err != nil {
				break
			}
			if !bytes.Equal(got, []byte(want)) {
				t.Fatalf("content mismatch")
			}
			if r.bytes > 1024 || r.bytes < 0 || len(r.messages) > 8 {
				t.Fatal("memory limit violated")
			}
			used := 0
			for _, p := range ref.pending {
				used += p.total
			}
			if used != r.bytes || len(ref.pending) != len(r.messages) {
				t.Fatal("reservation/reference mismatch")
			}
		}
	})
}

func FuzzDecode(f *testing.F) {
	for _, k := range []Kind{DaemonUplink, DaemonDownlink, WorkerUplink, WorkerDownlink} {
		data, _ := proto.Marshal(wrap(k, "heartbeat", &circusv1.Heartbeat{Seq: 1}))
		f.Add(uint8(k), uint16(65535), data)
	}
	for _, count := range []int{4096, 4097, 8192} {
		inner := bytes.Repeat([]byte{0x12, 0}, count)
		data := []byte{0x72}
		data = appendReferenceVarint(data, uint64(len(inner)))
		data = append(data, inner...)
		f.Add(uint8(DaemonUplink), uint16(65535), data)
	}
	for _, m := range []proto.Message{
		wrap(DaemonUplink, "call_status", &circusv1.CallStatus{CallId: strings.Repeat("s", (64<<10)+1)}),
		wrap(DaemonUplink, "call_status", &circusv1.CallStatus{CallId: strings.Repeat("s", (64<<10)+1), Result: &circusv1.DeviceResult{}}),
		wrap(DaemonUplink, "call_status", &circusv1.CallStatus{CallId: "c", Result: &circusv1.DeviceResult{Output: &circusv1.ToolOutput{Content: []*circusv1.ContentBlock{{Block: &circusv1.ContentBlock_Text{Text: strings.Repeat("o", 80<<10)}}}}}}),
	} {
		data, _ := proto.Marshal(m)
		f.Add(uint8(DaemonUplink), uint16(65535), data)
	}
	f.Add(uint8(1), uint16(256), []byte{0xff})
	f.Fuzz(func(t *testing.T, kind uint8, limit uint16, data []byte) {
		if len(data) > 256<<10 {
			return
		}
		k := Kind(kind)
		bound := int(limit) * 8
		expected := k.newMessage()
		model := referenceDecode{counts: make(map[protoreflect.FullName]int), messageBytes: bound}
		structural := expected != nil && bound > 0 && len(data) <= bound
		if structural {
			structural = model.walk(expected.ProtoReflect().Descriptor(), data, 0)
		}
		valid := structural
		if valid {
			valid = (proto.UnmarshalOptions{RecursionLimit: 32}).Unmarshal(data, expected) == nil
		}
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		got, err := Decode(k, data, bound)
		runtime.ReadMemStats(&after)
		allocationLimit := uint64(512 << 10) // runtime/sampler noise, not decoded objects
		if structural {
			allocationLimit += uint64(2*len(data) + 256*model.nodes + 64*model.fields + 4096)
		}
		if after.TotalAlloc-before.TotalAlloc > allocationLimit {
			t.Fatalf("actual allocation %d exceeds %d", after.TotalAlloc-before.TotalAlloc, allocationLimit)
		}
		if valid != (err == nil) {
			t.Fatalf("acceptance mismatch kind=%d size=%d bound=%d err=%v", k, len(data), bound, err)
		}
		if valid && !proto.Equal(got, expected) {
			t.Fatal("decoded content mismatch")
		}
	})
}

func appendReferenceVarint(b []byte, v uint64) []byte {
	for v >= 128 {
		b = append(b, byte(v)|128)
		v >>= 7
	}
	return append(b, byte(v))
}

// This reference parses tags and lengths independently (without protowire or
// production scan helpers). The generated decoder is used only after the model
// accepts structure, for UTF-8/type validation and content comparison.
type referenceDecode struct {
	nodes, fields int
	resultBytes   int
	messageBytes  int
	counts        map[protoreflect.FullName]int
}

func referenceVarint(data []byte) (uint64, int) {
	var v uint64
	for i, b := range data {
		if i == 10 || i == 9 && b > 1 {
			return 0, -1
		}
		v |= uint64(b&127) << uint(7*i)
		if b < 128 {
			return v, i + 1
		}
	}
	return 0, -1
}
func (r *referenceDecode) walk(d protoreflect.MessageDescriptor, data []byte, depth int) bool {
	r.nodes++
	if depth >= 32 || r.nodes > 8192 {
		return false
	}
	payloads := 0
	var payloadName string
	fullSize := len(data)
	for len(data) > 0 {
		tag, n := referenceVarint(data)
		if n < 0 || tag>>3 == 0 || tag>>3 > 536870911 {
			return false
		}
		data = data[n:]
		typ := tag & 7
		var value []byte
		switch typ {
		case 0:
			_, n = referenceVarint(data)
			if n < 0 {
				return false
			}
		case 1:
			n = 8
		case 5:
			n = 4
		case 2:
			length, h := referenceVarint(data)
			if h < 0 || length > uint64(len(data)-h) {
				return false
			}
			n = h + int(length)
			value = data[h:n]
		default:
			return false
		}
		if n > len(data) {
			return false
		}
		r.fields++
		if r.fields > 65536 {
			return false
		}
		field := d.Fields().ByNumber(protoreflect.FieldNumber(tag >> 3))
		if field != nil {
			if depth == 0 {
				payloads++
				payloadName = string(field.Name())
				cap := 64 << 10
				switch string(field.Name()) {
				case "dispatch_call", "submit_call", "result", "call_status", "call_ack", "call_update":
					cap = r.messageBytes
				case "job_snapshot":
					cap = 8 << 20
				case "fragment":
					cap = (256 << 10) + 128
				}
				if payloads > 1 || typ != 2 || fullSize > cap {
					return false
				}
			}
			if field.IsList() {
				r.counts[field.FullName()]++
				cap := int(^uint(0) >> 1)
				switch string(field.FullName()) {
				case "circus.v1.JobSnapshot.jobs":
					cap = 4096
				case "circus.v1.ToolOutput.content":
					cap = 128
				case "circus.v1.BashRequest.working_dirs", "circus.v1.CallUpdate.DeviceOffline.online_devices":
					cap = 256
				}
				if r.counts[field.FullName()] > cap {
					return false
				}
			}
			if field.Message() != nil {
				if typ != 2 {
					return false
				}
				if string(field.Message().FullName()) == "circus.v1.JobEvent" && len(value) > 64<<10 {
					return false
				}
				if string(field.Message().FullName()) == "circus.v1.DeviceResult" {
					r.resultBytes += len(value)
				}
				if !r.walk(field.Message(), value, depth+1) {
					return false
				}
			}
		}
		data = data[n:]
	}
	if depth == 0 && (payloadName == "call_status" || payloadName == "call_ack" || payloadName == "call_update") && fullSize-r.resultBytes > 64<<10 {
		return false
	}
	return depth != 0 || payloads == 1
}
