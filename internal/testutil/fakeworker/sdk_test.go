package fakeworker

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"testing"

	pb "github.com/EricWvi/circus/gen/go/circus/v1"
)

var updateSDKGolden = flag.Bool("update-sdk-golden", false, "update the fake worker SDK message fixture")

func TestSDKMessages(t *testing.T) {
	const (
		sessionID = "018f0000-0000-7000-8000-000000000001"
		inputID   = "018f0000-0000-7000-8000-000000000002"
		eventID   = "018f0000-0000-7000-8000-000000000003"
		toolID    = "toolu_fake"
		model     = "claude-sonnet-4-6"
	)
	messages := []sdkMessage{
		sdkUser(sessionID, inputID, "hello", ""),
		sdkPartial(sessionID, eventID, textDelta(0, "You said:")),
		sdkAssistant(sessionID, eventID, "msg_fake", model, textBlock("You said:\n\n> hello")),
		sdkAssistant(sessionID, eventID, "msg_fake", model, thinkingBlock()),
		sdkAssistant(sessionID, eventID, "msg_fake", model, toolUse(toolID, "Bash", object{"command": "echo hi"})),
		sdkUser(sessionID, eventID, []object{toolResult(toolID, []object{{"type": "text", "text": "hi\n"}}, false)}, ""),
		sdkResult(sessionID, eventID, "Bash finished.", "", 20),
		sdkResult(sessionID, eventID, "", "scripted worker error", 20),
	}
	partial := sdkAssistant(sessionID, eventID, "msg_aborted", model, textBlock("You said:"))
	partial.payload["aborted"] = true
	if bytes.Contains([]byte(partial.transcript("").GetJson()), []byte(`"aborted":`)) {
		t.Fatal("the SDK aborted flag belongs to the event envelope")
	}
	interrupt := sdkResult(sessionID, eventID, "", "[ede_diagnostic] result_type=user last_content_type=n/a stop_reason=null", 20)
	now := sdkResult(sessionID, eventID, "", "", 20)
	applyAbortResult(interrupt, "", "")
	applyAbortResult(now, "", "You said:")
	resultUsage(now, model, &turn{usedInput: 25, outputTokens: 3, requests: 1}, &usageTotals{}, 20)
	messages = append(messages, partial,
		sdkUser(sessionID, eventID, []object{{"type": "text", "text": "[Request interrupted by user]"}}, noteUserInterrupt),
		interrupt, now, sdkPermissionDenied(sessionID, eventID, toolID),
		sdkUser(sessionID, eventID, []object{toolResult(toolID, denialText, true)}, ""),
		sdkTaskStarted(sessionID, eventID, "task-1", toolID, "Background", "local_bash"),
		sdkTaskNotification(sessionID, eventID, "task-1", toolID, "completed", "Background finished", "/tmp/task-1.output"),
	)
	type goldenEvent struct {
		KindName  string          `json:"kind"`
		Ephemeral bool            `json:"ephemeral"`
		Payload   json.RawMessage `json:"payload"`
	}
	got := make([]goldenEvent, 0, len(messages))
	for _, m := range messages {
		event := m.event()
		got = append(got, goldenEvent{
			KindName: event.GetKind().String(), Ephemeral: event.GetEphemeral(), Payload: json.RawMessage(event.GetJson()),
		})
	}
	encoded, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	encoded = append(encoded, '\n')
	const path = "testdata/sdk_messages.json"
	if *updateSDKGolden {
		if err := os.WriteFile(path, encoded, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(encoded, want) {
		t.Fatalf("SDK message shapes changed:\n%s", encoded)
	}
}

// TestWorkerEventPayloads exports the payloads the worker writes itself,
// rather than forwards from the SDK, to testdata/worker_events.json. The
// TypeScript worker's tests build the same events from the same inputs and
// compare them with this file, so the two workers cannot drift apart.
func TestWorkerEventPayloads(t *testing.T) {
	const (
		sessionID = "018f0000-0000-7000-8000-000000000001"
		inputID   = "018f0000-0000-7000-8000-000000000002"
		eventID   = "018f0000-0000-7000-8000-000000000003"
		stamp     = "2026-01-02T03:04:05.006Z"
	)
	user := func(id string, content any, note string) object {
		m := sdkUser(sessionID, id, content, note).payload
		m["timestamp"] = stamp
		return m
	}
	interrupt := []object{{"type": "text", "text": "[Request interrupted by user]"}}
	type named struct {
		name    string
		kind    pb.SessionEvent_Kind
		payload object
	}
	events := []named{
		{"turn_started", pb.SessionEvent_KIND_TURN_STARTED, turnStartedPayload(18446744073709551615, "input")},
		{"turn_started_cli", pb.SessionEvent_KIND_TURN_STARTED, turnStartedPayload(2, "cli")},
		{"approval_pending", pb.SessionEvent_KIND_APPROVAL_PENDING, approvalPendingPayload(eventID, "Bash", "box", "toolu_fake", "echo hi", approvalReason)},
		{"error", pb.SessionEvent_KIND_ERROR, errorPayload("scripted worker error")},
		{"gap_detected", pb.SessionEvent_KIND_TRANSCRIPT_GAP_DETECTED, gapDetectedPayload("", []string{eventID, inputID})},
		{"lost_task", pb.SessionEvent_KIND_BGTASK_ENDED, lostTaskPayload(sessionID, eventID,
			&pb.PatchSessionState_BackgroundTask{TaskId: "task-1", ToolUseId: "toolu_fake"}, "Background task task-1 was lost.")},
		{"lost_task_without_tool_use", pb.SessionEvent_KIND_BGTASK_ENDED, lostTaskPayload(sessionID, eventID,
			&pb.PatchSessionState_BackgroundTask{TaskId: "task-1"}, "Background task task-1 was lost.")},
		{"user_input", pb.SessionEvent_KIND_USER, user(inputID, "hello", "")},
		{"note_approval_retry", pb.SessionEvent_KIND_USER, user(inputID, "Retry it now.", noteApprovalRetry)},
		{"note_user_interrupt", pb.SessionEvent_KIND_USER, user(eventID, interrupt, noteUserInterrupt)},
		{"note_previous_turn_interrupted", pb.SessionEvent_KIND_USER, user(eventID, "The previous turn was interrupted.", notePreviousTurnInterrupted)},
		{"note_lost_bgtask", pb.SessionEvent_KIND_USER, user(eventID, "Background task task-1 was lost.", noteLostBgtask)},
		{"note_history_missing", pb.SessionEvent_KIND_USER, user(eventID, "Part of the conversation history is missing.", noteHistoryMissing)},
	}
	type goldenEvent struct {
		Name      string          `json:"name"`
		KindName  string          `json:"kind"`
		Ephemeral bool            `json:"ephemeral"`
		Payload   json.RawMessage `json:"payload"`
	}
	got := make([]goldenEvent, 0, len(events))
	for _, e := range events {
		got = append(got, goldenEvent{Name: e.name, KindName: e.kind.String(), Payload: json.RawMessage(jsonString(e.payload))})
	}
	encoded, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	encoded = append(encoded, '\n')
	const path = "testdata/worker_events.json"
	if *updateSDKGolden {
		if err := os.WriteFile(path, encoded, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(encoded, want) {
		t.Fatalf("worker event payloads changed; regenerate with -update-sdk-golden and run the worker tests:\n%s", encoded)
	}
}
