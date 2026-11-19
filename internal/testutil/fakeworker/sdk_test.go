package fakeworker

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"testing"
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
		sdkUser(sessionID, inputID, "hello", false),
		sdkPartial(sessionID, eventID, textDelta(0, "You said:")),
		sdkAssistant(sessionID, eventID, "msg_fake", model, textBlock("You said:\n\n> hello")),
		sdkAssistant(sessionID, eventID, "msg_fake", model, thinkingBlock()),
		sdkAssistant(sessionID, eventID, "msg_fake", model, toolUse(toolID, "Bash", object{"command": "echo hi"})),
		sdkUser(sessionID, eventID, []object{toolResult(toolID, []object{{"type": "text", "text": "hi\n"}}, false)}, false),
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
		sdkUser(sessionID, eventID, []object{{"type": "text", "text": "[Request interrupted by user]"}}, true),
		interrupt, now, sdkPermissionDenied(sessionID, eventID, toolID),
		sdkUser(sessionID, eventID, []object{toolResult(toolID, denialText, true)}, false),
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
