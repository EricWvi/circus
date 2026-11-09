package daemon_test

import (
	"testing"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"google.golang.org/protobuf/proto"
)

func TestDeviceCallRoundTrip(t *testing.T) {
	want := &circusv1.DeviceCall{
		CallId:    "call-1",
		SessionId: "session-1",
		AgentId:   "agent-1",
		TimeoutMs: 1000,
		Tool: &circusv1.DeviceCall_Bash{Bash: &circusv1.BashRequest{
			Command: "pwd",
			Cwd:     "/tmp",
		}},
	}
	data, err := proto.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	got := new(circusv1.DeviceCall)
	if err := proto.Unmarshal(data, got); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(want, got) {
		t.Fatalf("round trip mismatch: want %v, got %v", want, got)
	}
}
