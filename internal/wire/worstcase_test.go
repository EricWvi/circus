package wire_test

import (
	"math"
	"strings"
	"testing"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/wire"
	"google.golang.org/protobuf/proto"
)

// String caps from docs/wire.md section 7 ("Field caps"), copied literally.
const (
	idCap          = 128
	toolCap        = 256
	textCap        = 1024
	pathCap        = 4 << 10
	sha256HexBytes = 64
)

func capped(n int) string { return strings.Repeat("x", n) }

// worstState fills every SessionState field and repeated field to its cap.
// calls are included only when withCalls is set (Lease carries them in
// Recovery instead).
func worstState(withCalls bool) *circusv1.SessionState {
	s := &circusv1.SessionState{Stopped: true, InterruptTurnNo: math.MaxUint64}
	for range 256 {
		s.Cwds = append(s.Cwds, &circusv1.PatchSessionState_Cwd{AgentId: capped(idCap), Device: capped(idCap), Cwd: capped(pathCap)})
	}
	for range 4096 {
		s.Files = append(s.Files, &circusv1.PatchSessionState_FileRecord{Device: capped(idCap), Path: capped(pathCap), Sha256: capped(sha256HexBytes), MtimeUnixMs: -1})
	}
	for range 1024 {
		s.Jobs = append(s.Jobs, &circusv1.SessionState_Job{
			Key:    &circusv1.JobKey{DeviceId: capped(idCap), BootId: capped(idCap), JobId: capped(idCap)},
			CallId: capped(idCap), ToolUseId: capped(idCap), Description: capped(textCap), OutputPath: capped(pathCap),
			State: circusv1.JobEvent_STATE_LOST, ExitCode: -1, Device: capped(idCap), Handed: true,
		})
	}
	if withCalls {
		s.Calls = worstCalls()
	}
	for range 32 {
		s.Grants = append(s.Grants, &circusv1.SessionState_Grant{Tool: capped(toolCap), Device: capped(idCap), MatchKey: capped(sha256HexBytes), ExpiresAtUnixMs: -1})
	}
	return s
}

func worstCalls() []*circusv1.SessionState_Call {
	var calls []*circusv1.SessionState_Call
	for range 1040 {
		calls = append(calls, &circusv1.SessionState_Call{
			CallId: capped(idCap), ToolUseId: capped(idCap), Device: capped(idCap), Returned: true, Tool: capped(toolCap),
			Outcome: circusv1.CallOutcome_CALL_OUTCOME_REJECTED, ExitCode: proto.Int32(-1), NoticeOwed: true,
			RejectedCode: circusv1.CallUpdate_Rejected_CODE_SESSION_STOPPED,
		})
	}
	return calls
}

func reply(r *circusv1.SessionReply) *circusv1.WorkerDownlink {
	return &circusv1.WorkerDownlink{RequestId: math.MaxUint64, Payload: &circusv1.WorkerDownlink_SessionReply{SessionReply: r}}
}

// The worst-case sizes stated in docs/wire.md must hold, and those replies
// must stay within M and the structural limits so the server can send them.
// The Lease here is built by hand with state.calls empty. When the server
// assembles Lease replies, build this case through that assembler instead,
// so that a Lease which also fills state.calls (over 8192 nodes at the caps)
// fails here rather than closing worker links.
func TestWorstCaseSessionReplies(t *testing.T) {
	recovery := &circusv1.Recovery{Calls: worstCalls(), PreviousTurnInterrupted: true}
	for range 64 {
		recovery.LostBgtasks = append(recovery.LostBgtasks, &circusv1.PatchSessionState_BackgroundTask{TaskId: capped(idCap), ToolUseId: capped(idCap), Kind: capped(idCap), Description: capped(textCap)})
	}
	config := &circusv1.SessionConfig{DefaultDevice: capped(idCap), Model: capped(idCap), PermissionMode: capped(idCap)}
	for range 256 {
		config.Cwds = append(config.Cwds, &circusv1.SessionConfig_Cwd{Device: capped(idCap), Cwd: capped(pathCap)})
	}
	keys := &circusv1.SessionReply_TranscriptKeys{}
	for range 1024 {
		keys.Subpaths = append(keys.Subpaths, capped(textCap))
	}
	for _, tc := range []struct {
		name  string
		m     *circusv1.WorkerDownlink
		bound int // docs/wire.md, section 7 ("Worst-case replies")
	}{
		{"lease", reply(&circusv1.SessionReply{Result: &circusv1.SessionReply_Lease_{Lease: &circusv1.SessionReply_Lease{
			Epoch: math.MaxUint64, ClaudeSessionId: capped(idCap), HasTranscript: true, Reattached: true,
			State: worstState(false), Recovery: recovery, Config: config,
		}}}), 51 << 19}, // 25.5 MiB
		{"session_state", reply(&circusv1.SessionReply{Result: &circusv1.SessionReply_State{State: &circusv1.SessionReply_SessionStateReply{State: worstState(true)}}}), 49 << 19}, // 24.5 MiB
		{"transcript_keys", reply(&circusv1.SessionReply{Result: &circusv1.SessionReply_TranscriptKeys_{TranscriptKeys: keys}}), 1028 << 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := proto.Marshal(tc.m)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("%s: %d bytes", tc.name, len(data))
			if len(data) > tc.bound {
				t.Fatalf("%d bytes, wire.md states at most %d", len(data), tc.bound)
			}
			if _, err := wire.Charge(tc.m, wire.DefaultMaxMessageBytes); err != nil {
				t.Fatalf("Charge: %v", err)
			}
			if _, err := wire.Decode(wire.WorkerDownlink, data, wire.DefaultMaxMessageBytes); err != nil {
				t.Fatalf("Decode: %v", err)
			}
		})
	}
}
