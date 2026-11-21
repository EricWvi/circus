package wire

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pb "github.com/EricWvi/circus/gen/go/circus/v1"
	"google.golang.org/protobuf/proto"
)

const vectorFragmentBytes = 32

type workerVector struct {
	Name   string   `json:"name"`
	Frames []string `json:"frames"`
}

func vectorMessages() (proto.Message, proto.Message) {
	return &pb.WorkerUplink{
			RequestId: 9007199254740993,
			Payload: &pb.WorkerUplink_Session{Session: &pb.SessionRequest{
				SessionId: "s-1", Epoch: 7, Op: &pb.SessionRequest_AppendTranscript{AppendTranscript: &pb.AppendTranscript{
					Subpath: "agent/例", BatchSeq: 1,
					Entries: []*pb.TranscriptEntry{{Uuid: "entry", Json: strings.Repeat("你好!", 20)}},
				}},
			}},
		}, &pb.WorkerDownlink{
			RequestId: 9007199254740995,
			Payload: &pb.WorkerDownlink_SessionReply{SessionReply: &pb.SessionReply{
				Result: &pb.SessionReply_TranscriptPage_{TranscriptPage: &pb.SessionReply_TranscriptPage{
					Entries: []*pb.TranscriptEntry{{Uuid: "entry", Json: strings.Repeat("你好!", 20)}},
					NextSeq: 9007199254740999, Done: true,
				}},
			}},
		}
}

func marshalVector(t *testing.T, msg proto.Message) []byte {
	t.Helper()
	data, err := proto.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestWorkerWireVectors(t *testing.T) {
	up, down := vectorMessages()
	readFixture := func(name string) string {
		return filepath.Join("..", "..", "worker", "test", "vectors", name+".json")
	}
	goFrames := workerVector{Name: "transcript page with interleaved heartbeat"}
	message := marshalVector(t, down)
	for i, start := 0, 0; start < len(message); i, start = i+1, start+vectorFragmentBytes {
		end := min(start+vectorFragmentBytes, len(message))
		f := &pb.Fragment{MessageId: 3, Index: uint32(i), Last: end == len(message), Data: message[start:end]}
		if i == 0 {
			f.TotalSize = uint64(len(message))
		}
		goFrames.Frames = append(goFrames.Frames, hex.EncodeToString(marshalVector(t, wrap(WorkerDownlink, "fragment", f))))
		if i == 0 {
			hb := &pb.Heartbeat{Seq: 1, SentUnixMs: 42}
			goFrames.Frames = append(goFrames.Frames, hex.EncodeToString(marshalVector(t, wrap(WorkerDownlink, "heartbeat", hb))))
		}
	}
	invalid := []workerVector{
		{Name: "duplicate payload", Frames: []string{"1a001a00"}},
		{Name: "packed nudge reasons", Frames: []string{"6a0712050a01731200"}},
		{Name: "invalid UTF-8", Frames: []string{"6a050a030a01ff"}},
		{Name: "unknown group", Frames: []string{"1a002b2c"}},
	}
	for _, v := range invalid {
		frame, err := hex.DecodeString(v.Frames[0])
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Decode(WorkerDownlink, frame, MaxMetadataBytes); err == nil {
			t.Fatalf("Go accepted %s", v.Name)
		}
	}
	generated, err := json.MarshalIndent(append([]workerVector{goFrames}, invalid...), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	generated = append(generated, '\n')
	if os.Getenv("UPDATE_WORKER_WIRE_VECTORS") == "1" {
		if err := os.WriteFile(readFixture("go"), generated, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	onDisk, err := os.ReadFile(readFixture("go"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(onDisk, generated) {
		t.Fatal("Go worker vectors changed; regenerate with UPDATE_WORKER_WIRE_VECTORS=1")
	}

	data, err := os.ReadFile(readFixture("ts"))
	if err != nil {
		t.Fatal(err)
	}
	var ts workerVector
	if err := json.Unmarshal(data, &ts); err != nil {
		t.Fatal(err)
	}
	r := newReassembler(Options{MaxMessageBytes: 1024, MaxFragmentBytes: vectorFragmentBytes,
		MaxInFlightMessages: 1, MaxInFlightBytes: 1024})
	var seen bool
	for _, enc := range ts.Frames {
		frame, err := hex.DecodeString(enc)
		if err != nil {
			t.Fatal(err)
		}
		m, _, err := decode(WorkerUplink, frame, vectorFragmentBytes+FrameOverhead, vectorFragmentBytes)
		if err != nil {
			t.Fatal(err)
		}
		f := m.(*pb.WorkerUplink).GetFragment()
		if f == nil {
			t.Fatal("TS frame lacks Fragment")
		}
		body, err := r.add(f, time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		if body == nil {
			continue
		}
		got, err := Decode(WorkerUplink, body, 1024)
		if err != nil {
			t.Fatal(err)
		}
		if seen || !proto.Equal(got, up) {
			t.Fatalf("TS uplink: got %v, want %v", got, up)
		}
		seen = true
	}
	if !seen || len(r.messages) != 0 {
		t.Fatal("missing complete TS uplink")
	}
}
