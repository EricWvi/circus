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
	partial := func(event object) sdkMessage { return sdkPartial(sessionID, eventID, "msg_fake", event) }
	first := sdkMessageStart(sessionID, eventID, "msg_fake", model, 25)
	first.payload["owed_rows"] = []any{}
	nameUserMessage(first.payload, inputID)
	text := sdkAssistant(sessionID, eventID, "msg_fake", model, textBlock("You said:\n\n> hello"))
	nameUserMessage(text.payload, inputID)
	thinkingStart := partial(object{"type": "content_block_start", "index": 0, "content_block": object{"type": "thinking", "thinking": "", "signature": ""}})
	thinkingStart.payload["thinking_display"] = thinkingDisplay
	messages := []sdkMessage{
		sdkUser(sessionID, inputID, "hello", ""),
		first,
		sdkMessageStart(sessionID, eventID, "msg_fake", model, 25),
		thinkingStart,
		partial(object{"type": "content_block_start", "index": 0, "content_block": textBlock("")}),
		partial(textDelta(0, "You said:")),
		partial(object{"type": "content_block_start", "index": 0, "content_block": object{"type": "tool_use", "id": toolID, "name": "Bash", "input": object{}}}),
		partial(inputDelta(0, `{"command":"echo hi"}`)),
		partial(blockStop(0)),
		partial(messageDelta("end_turn", 25, 3, 0)),
		partial(messageDelta(nil, 25, 3, 0)),
		partial(object{"type": "message_stop"}),
		text,
		sdkAssistant(sessionID, eventID, "msg_fake", model, thinkingBlock()),
		sdkAssistant(sessionID, eventID, "msg_fake", model, toolUse(toolID, "Bash", object{"command": "echo hi"})),
		sdkToolResult(sessionID, eventID, toolID, "hi", false, toolUseResult("Bash", "", "hi", false), allowedMeta(toolID, false)),
		sdkToolResult(sessionID, eventID, toolID, "hi", false, toolUseResult("Bash", "", "hi", false), allowedMeta(toolID, true)),
		sdkToolResult(sessionID, eventID, toolID, "a\nb\n", false, toolUseResult("Read", "/w/notes.txt", "a\nb\n", false), allowedMeta(toolID, false)),
		sdkToolResult(sessionID, eventID, toolID, "exit 1", true, toolUseResult("Bash", "", "exit 1", true), allowedMeta(toolID, false)),
		sdkInterruptedTool(sessionID, eventID, toolID),
		sdkResult(sessionID, eventID, "Bash finished.", "", 20),
		sdkResult(sessionID, eventID, "", "scripted worker error", 20),
	}
	aborted := sdkAssistant(sessionID, eventID, "msg_aborted", model, textBlock("You said:"))
	aborted.payload["aborted"] = true
	if bytes.Contains([]byte(aborted.transcript("").GetJson()), []byte(`"aborted":`)) {
		t.Fatal("the SDK aborted flag belongs to the event envelope")
	}
	abandoned := sdkPartial(sessionID, eventID, "msg_aborted", object{"type": "message_stop"})
	abandoned.payload["abandoned_blocks"] = object{"api_message_id": "msg_aborted", "from_block_index": 1}
	interrupt := sdkResult(sessionID, eventID, "", abortFailure(abortedStreaming), 20)
	interruptTool := sdkResult(sessionID, eventID, "", abortFailure(abortedTools), 20)
	now := sdkResult(sessionID, eventID, "", "", 20)
	applyAbortResult(interrupt, "", "")
	applyAbortResult(interruptTool, abortedTools, "")
	applyAbortResult(now, "", "You said:")
	s := &session{}
	tr := &turn{usedInput: 25, outputTokens: 3, requests: 1, userMessageID: inputID, messageIDs: []string{"msg_aborted"}, firstInput: 25}
	s.describeResult(now, tr)
	resultUsage(now, model, tr, &usageTotals{}, 20)
	messages = append(messages, aborted, abandoned,
		sdkUser(sessionID, eventID, []object{{"type": "text", "text": "[Request interrupted by user]"}}, noteUserInterrupt),
		sdkUser(sessionID, eventID, []object{{"type": "text", "text": "[Request interrupted by user for tool use]"}}, noteUserInterrupt),
		interrupt, interruptTool, now, sdkPermissionDenied(sessionID, eventID, toolID),
		sdkDenialResult(sessionID, eventID, toolID),
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
	ledgerNote := func(id string, content any, note string) object {
		m := sdkUserMessage(sessionID, id, content, note, false).payload
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
		// A ledger note is not synthetic; its view event names it.
		{"note_ledger_problems", pb.SessionEvent_KIND_USER, ledgerNote(inputID, "[circus ledger] The ledger collection found problems.", noteLedgerProblems)},
		// The user's answers are not synthetic either; the view event names them.
		{"note_question_answer", pb.SessionEvent_KIND_USER, ledgerNote(inputID, `The user answered your questions: "Which color?"="Red". Continue with these answers in mind.`, noteQuestionAnswer)},
		// Held notes, published each with its own event before the input
		// whose message carried them.
		{"note_ledger_merged", pb.SessionEvent_KIND_USER, ledgerNote(eventID, "[circus ledger] Your ledger changes were merged.", "ledger_merged")},
		{"note_ledger_discarded", pb.SessionEvent_KIND_USER, ledgerNote(eventID, "[circus ledger] Your ledger changes were discarded.", "ledger_discarded")},
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
