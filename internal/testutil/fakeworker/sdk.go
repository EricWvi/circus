package fakeworker

import (
	"bytes"
	"encoding/json"
	"maps"
	"strconv"
	"strings"
	"time"

	pb "github.com/EricWvi/circus/gen/go/circus/v1"
)

// The event payloads use the SDK's stream-json envelopes. The assistant
// message inside its envelope has the Anthropic Messages API shape.
type object map[string]any

type sdkMessage struct {
	kind    pb.SessionEvent_Kind
	payload object
}

func (m sdkMessage) event() *pb.PublishEvents_Event {
	return &pb.PublishEvents_Event{
		Kind: m.kind, Json: jsonString(m.payload), Ephemeral: m.payload["type"] == "stream_event",
	}
}

func (m sdkMessage) transcript(parent string) *pb.TranscriptEntry {
	payload := maps.Clone(m.payload)
	delete(payload, "aborted")
	delete(payload, "circus_note")
	payload["parentUuid"] = nil
	if parent != "" {
		payload["parentUuid"] = parent
	}
	return &pb.TranscriptEntry{Uuid: payload["uuid"].(string), Json: jsonString(payload)}
}

func sdkEnvelope(kind, sessionID, id string) object {
	return object{"type": kind, "session_id": sessionID, "uuid": id, "parent_tool_use_id": nil}
}

// circus_note kinds of worker-written user messages (docs/wire.md).
const (
	noteApprovalRetry           = "approval_retry"
	noteUserInterrupt           = "user_interrupt"
	notePreviousTurnInterrupted = "previous_turn_interrupted"
	noteRecoveredCall           = "recovered_call"
	noteCallNotice              = "call_notice"
	noteJobNotice               = "job_notice"
	noteLostBgtask              = "lost_bgtask"
	noteHistoryMissing          = "history_missing"
	// noteLedgerProblems is the category of a control-plane note (an
	// input of KIND_NOTE) about a ledger collection's problems; such a
	// note keeps the category the control plane gave it.
	noteLedgerProblems = "ledger_problems"
)

// sdkUser builds an SDK user message. A non-empty note marks a message the
// worker wrote for the model rather than the user's own input or a tool
// result: it is synthetic, and the published view event names its kind in
// circus_note (see docs/wire.md); the transcript entry omits circus_note.
// Texts of the notes the real worker writes (worker/src/runner/notes.ts).
const (
	previousTurnInterruptedText = "The previous turn was interrupted before it finished: the process running this session stopped. " +
		"Its last actions may be incomplete; check their effects before relying on them, and do not resume them unless asked."
	historyMissingText = "Part of the conversation history is missing: some earlier messages and tool results of this session could not be saved. " +
		"Do not assume you remember everything that happened before this point; ask the user or check the files when it matters."
)

func sdkUser(sessionID, id string, content any, note string) sdkMessage {
	return sdkUserMessage(sessionID, id, content, note, note != "")
}

// sdkUserMessage is sdkUser for a synthetic message whose note may be
// empty, as a control-plane note without a category: synthetic, without
// circus_note.
func sdkUserMessage(sessionID, id string, content any, note string, synthetic bool) sdkMessage {
	m := sdkEnvelope("user", sessionID, id)
	m["message"] = object{"role": "user", "content": content}
	if synthetic {
		m["isSynthetic"] = true
	}
	if note != "" {
		m["circus_note"] = note
	}
	return sdkMessage{pb.SessionEvent_KIND_USER, m}
}

// sdkAssistant is the durable message of one content block. Claude Code
// publishes it after the block's last delta, before its
// content_block_stop.
func sdkAssistant(sessionID, id, messageID, model string, block object) sdkMessage {
	m := sdkEnvelope("assistant", sessionID, id)
	m["message"] = apiMessage(messageID, model, []object{block})
	m["message"].(object)["context_management"] = nil
	if block["type"] == "thinking" {
		m["thinking_display"] = thinkingDisplay
		m["thinking_duration_ms"] = placeholderMs
	}
	return sdkMessage{pb.SessionEvent_KIND_ASSISTANT, m}
}

func apiMessage(id, model string, blocks []object) object {
	return object{
		"id": id, "type": "message", "role": "assistant", "model": model, "content": blocks,
		"stop_reason": nil, "stop_sequence": nil, "usage": messageUsage(0, 0),
	}
}

// Claude Code's thinking display mode, on a request's message_start and on
// its thinking block's events.
const thinkingDisplay = "updates"

// placeholderMs stands in for the timings Claude Code measures (ttft_*,
// *_ms): the fake has nothing meaningful to report.
const placeholderMs = 0

// sdkPartial is a stream_event of the API message messageID.
func sdkPartial(sessionID, id, messageID string, event object) sdkMessage {
	m := sdkEnvelope("stream_event", sessionID, id)
	m["api_message_id"] = messageID
	m["event"] = event
	return sdkMessage{pb.SessionEvent_KIND_ASSISTANT, m}
}

// sdkMessageStart starts the stream of the API message messageID.
func sdkMessageStart(sessionID, id, messageID, model string, input int) sdkMessage {
	start := apiMessage(messageID, model, []object{})
	start["usage"] = messageUsage(input, 1)
	m := sdkPartial(sessionID, id, messageID, object{"type": "message_start", "message": start})
	m.payload["thinking_display"] = thinkingDisplay
	m.payload["ttft_ms"] = placeholderMs
	return m
}

// nameUserMessage names the user message that started a turn on the
// turn's first message_start, its first durable block and its result.
func nameUserMessage(payload object, userMessageID string) {
	if userMessageID != "" {
		payload["user_message_uuid"] = userMessageID
		payload["user_message_uuids"] = []string{userMessageID}
	}
}

// messageUsage is the usage of a message_start event and of a durable
// assistant message.
func messageUsage(input, output int) object {
	return object{"input_tokens": input, "output_tokens": output, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0}
}

func sdkUsage(input, output int) object {
	return object{
		"input_tokens": input, "output_tokens": output, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0,
		"cache_creation": object{"ephemeral_1h_input_tokens": 0, "ephemeral_5m_input_tokens": 0}, "fallback_credit": nil,
		"inference_geo": "", "iterations": []any{}, "output_tokens_details": object{"thinking_tokens": 0},
		"server_tool_use": object{"web_fetch_requests": 0, "web_search_requests": 0},
		"service_tier":    "standard", "speed": "standard",
	}
}

// safeguardResults is a placeholder for the auto mode check results
// Claude Code attaches to a message_delta.
func safeguardResults() []any {
	return []any{object{"type": "dangerous_tool_use", "status": object{"type": "available", "tool_uses": object{}}}}
}

// messageDelta is the message_delta of a response that ended with
// stopReason, or of one the user aborted (stopReason nil), which Claude
// Code synthesizes in the API's full shape.
func messageDelta(stopReason any, input, output, thinking int) object {
	if stopReason != nil {
		return object{
			"type":  "message_delta",
			"delta": object{"stop_reason": stopReason, "stop_sequence": nil, "safeguard_results": safeguardResults()},
			"usage": object{"output_tokens": output},
		}
	}
	return object{
		"type": "message_delta", "context_management": nil,
		"delta": object{"stop_reason": nil, "stop_sequence": nil, "stop_details": nil, "container": nil},
		"usage": object{
			"input_tokens": input, "output_tokens": output, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0,
			"fallback_credit": nil, "iterations": []any{}, "output_tokens_details": object{"thinking_tokens": thinking},
			"server_tool_use": object{"web_fetch_requests": 0, "web_search_requests": 0},
		},
	}
}

func subagentStats() object {
	return object{
		"by_type": object{}, "completed": 0, "failed": 0, "killed": object{"parent": 0, "system": 0, "user": 0},
		"max_depth": 0, "refused": object{"budget": 0, "concurrency_limit": 0, "depth_limit": 0},
		"requested": object{"background": 0, "foreground": 0, "unset": 0}, "spawned": 0, "spawned_by_subagents": 0,
		"started_in_background": 0,
	}
}

// sdkResult is a turn's result. The keys a successful turn's result has in
// addition to a failed one's are the first request's timings.
func sdkResult(sessionID, id, text, failure string, duration int64) sdkMessage {
	m := object{
		"type": "result", "uuid": id, "session_id": sessionID, "duration_ms": duration, "duration_api_ms": 0,
		"num_turns": 1, "stop_reason": "end_turn", "total_cost_usd": 0, "usage": sdkUsage(0, 0),
		"modelUsage": object{}, "model_usage_since_last_result": object{}, "permission_denials": []any{}, "is_error": false,
		"subtype": "success", "terminal_reason": "completed", "begun_message_ids": []any{}, "result_index": 0,
		"fast_mode_state": "off", "fast_mode_disabled_reason": "sdk_opt_in_required", "queued_turn_count": 0,
		"safety_stops": 0, "subagent_stats": subagentStats(),
	}
	if failure == "" {
		m["result"] = text
		m["api_error_status"] = nil
		m["first_request_input_tokens"] = 0
		m["first_request_cache_read_input_tokens"] = 0
		for _, key := range []string{"first_content_frame_ms", "request_sent_wall_ms", "time_to_request_ms", "ttft_ms", "ttft_stream_ms"} {
			m[key] = placeholderMs
		}
	} else {
		m["subtype"] = "error_during_execution"
		m["is_error"] = true
		m["stop_reason"] = nil
		m["errors"] = []string{failure}
		// Only an aborted turn has been observed to end in error_during_execution.
		delete(m, "terminal_reason")
	}
	return sdkMessage{pb.SessionEvent_KIND_RESULT, m}
}

// Terminal reasons of a turn the user aborted.
const (
	abortedStreaming = "aborted_streaming"
	abortedTools     = "aborted_tools"
)

// abortFailure is the error Claude Code reports for an Interrupt: the stop
// reason is the aborted response's ("tool_use" when a tool was running).
func abortFailure(reason string) string {
	stop := "null"
	if reason == abortedTools {
		stop = "tool_use"
	}
	return "[ede_diagnostic] result_type=user last_content_type=n/a stop_reason=" + stop
}

func applyAbortResult(result sdkMessage, reason, text string) {
	if reason == "" {
		reason = abortedStreaming
	}
	result.payload["terminal_reason"] = reason
	result.payload["stop_reason"] = nil
	if reason == abortedTools {
		result.payload["stop_reason"] = "tool_use"
	}
	if result.payload["subtype"] == "success" {
		// A tool interrupted by NOW was observed to produce an empty result.
		// Its success subtype and aborted_tools terminal reason are inferred.
		result.payload["result"] = text
	}
}

// modelUsage is one model's entry of a result's modelUsage.
func modelUsage(model string, input, output, thinking int, cost float64) object {
	return object{
		"inputTokens": input, "outputTokens": output, "cacheReadInputTokens": 0, "cacheCreationInputTokens": 0,
		"cacheCreationEphemeral1hInputTokens": 0, "cacheCreationEphemeral5mInputTokens": 0, "canonicalModel": model,
		"thinkingTokens": thinking, "webSearchRequests": 0, "costUSD": cost, "costBasis": "list", "provider": "firstParty",
		"contextWindow": 200000, "maxOutputTokens": 64000,
	}
}

// describeResult fills in what the result says about its turn: the API
// messages it began, the user message that started it and its index among
// the results of the session's process.
func (s *session) describeResult(result sdkMessage, t *turn) {
	result.payload["begun_message_ids"] = append([]string{}, t.messageIDs...)
	result.payload["result_index"] = s.results
	s.results++
	nameUserMessage(result.payload, t.userMessageID)
	if _, ok := result.payload["first_request_input_tokens"]; ok {
		result.payload["first_request_input_tokens"] = t.firstInput
	}
}

func resultUsage(result sdkMessage, model string, t *turn, total *usageTotals, duration int64) object {
	input := max(t.usedInput, t.inputTokens)
	output := t.outputTokens
	cost := float64(3*input+15*output) / 1e6
	total.Input += input
	total.Output += output
	total.Thinking += t.thinkingTokens
	totalCost := float64(3*total.Input+15*total.Output) / 1e6
	usage := sdkUsage(input, output)
	usage["output_tokens_details"] = object{"thinking_tokens": t.thinkingTokens}
	result.payload["usage"] = usage
	result.payload["duration_api_ms"] = min(duration, int64(30+t.requests*120))
	result.payload["num_turns"] = max(1, t.requests)
	result.payload["total_cost_usd"] = totalCost
	result.payload["modelUsage"] = object{model: modelUsage(model, total.Input, total.Output, total.Thinking, totalCost)}
	result.payload["model_usage_since_last_result"] = object{model: modelUsage(model, 0, 0, 0, 0)}
	if len(t.denials) != 0 {
		result.payload["permission_denials"] = t.denials
	}
	return object{"input_tokens": input, "output_tokens": output, "total_cost_usd": cost, "duration_ms": duration}
}

func (s *session) stamp(m sdkMessage) sdkMessage {
	m.payload["timestamp"] = s.w.opts.Clock.Now().UTC().Format(time.RFC3339Nano)
	return m
}

func textBlock(text string) object {
	return object{"type": "text", "text": text}
}

func inputDelta(index int, partial string) object {
	return object{"type": "content_block_delta", "index": index, "delta": object{"type": "input_json_delta", "partial_json": partial}}
}

func textDelta(index int, text string) object {
	return object{"type": "content_block_delta", "index": index, "delta": object{"type": "text_delta", "text": text}}
}

func thinkingBlock() object {
	return object{"type": "thinking", "thinking": "Planning a scripted response.", "signature": "fake-signature"}
}

func toolUse(id, name string, input object) object {
	return object{"type": "tool_use", "id": id, "name": name, "input": input}
}

// sdkToolResult is the user message that carries a tool's result:
// useResult is the tool's own result (tool_use_result) and meta how the
// call was let run or why it did not run (tool_result_meta).
func sdkToolResult(sessionID, id, toolID string, content any, isError bool, useResult any, meta object) sdkMessage {
	m := sdkUser(sessionID, id, []object{toolResult(toolID, content, isError)}, "")
	m.payload["tool_use_result"] = useResult
	m.payload["tool_result_meta"] = []object{meta}
	return m
}

// allowedMeta is the tool_result_meta of a call auto mode's classifier
// allowed, or the worker's PreToolUse hook (an approved retry).
func allowedMeta(toolID string, hook bool) object {
	decision := object{"decision": "accept", "reason_type": "classifier", "source": "config"}
	if hook {
		decision = object{"decision": "accept", "reason_type": "hook", "source": "hook"}
	}
	return object{"id": toolID, "permission_decision": decision}
}

// interruptedToolText is the tool_result of a call the user interrupted, as
// the real worker publishes it (captured by the e2e tests).
const interruptedToolText = "The user doesn't want to proceed with this tool use. The tool use was rejected " +
	"(eg. if it was a file edit, the new_string was NOT written to the file). STOP what you are doing and wait for the user to tell you how to proceed. " +
	"Correction to the text above: this call was stopped while running, not refused before it ran. " +
	"It may have run in part or in full, and its changes may have been written. No permission prompt was involved. " +
	"Work it started elsewhere (a remote command, a background process) may still be running, so check before starting the same work again. " +
	"The instruction to stop and wait still stands."

func sdkInterruptedTool(sessionID, id, toolID string) sdkMessage {
	return sdkToolResult(sessionID, id, toolID, interruptedToolText, true, "User rejected tool use",
		object{"id": toolID, "non_execution_kind": "user-rejected"})
}

// toolUseResult is a tool's tool_use_result for its text output.
func toolUseResult(name, path, text string, failed bool) any {
	switch {
	case failed:
		return "Error: " + text
	case name == "Read":
		lines := strings.Count(text, "\n")
		if text != "" && !strings.HasSuffix(text, "\n") {
			lines++
		}
		return object{"type": "text", "file": object{
			"filePath": path, "content": text, "numLines": lines, "startLine": 1, "totalLines": lines,
		}}
	}
	return object{"stdout": text, "stderr": "", "interrupted": false, "isImage": false, "noOutputExpected": false}
}

// resultContent is a tool_result's content: a string for text output, the
// blocks when there is an image.
func resultContent(blocks []object) any {
	texts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		if b["type"] != "text" {
			return blocks
		}
		texts = append(texts, b["text"].(string))
	}
	return strings.Join(texts, "\n")
}

func toolResult(id string, content any, isError bool) object {
	return object{"type": "tool_result", "tool_use_id": id, "content": content, "is_error": isError}
}

func jsonString(payload any) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	// Only concrete JSON values are used to build the scripted messages.
	if err := enc.Encode(payload); err != nil {
		panic(err)
	}
	return string(bytes.TrimSuffix(b.Bytes(), []byte{'\n'}))
}

// The payloads below are the events the worker writes itself rather than
// forwards from the SDK. The TypeScript worker builds the same shapes; the
// golden test exports them to testdata/worker_events.json for its tests.

func turnStartedPayload(turnNo uint64, origin string) object {
	return object{"turn_no": strconv.FormatUint(turnNo, 10), "origin": origin}
}

func approvalPendingPayload(approvalID, tool, device, toolUseID, display, reason string) object {
	return object{
		"approval_id": approvalID, "tool": tool, "device": device,
		"tool_use_id": toolUseID, "display": display, "reason": reason,
	}
}

func errorPayload(message string) object { return object{"message": message} }

func gapDetectedPayload(subpath string, unknownParents []string) object {
	return object{"subpath": subpath, "unknown_parents": unknownParents}
}

// sdkTaskStarted is the CLI's task_started message for a background task.
func sdkTaskStarted(sessionID, id, taskID, toolUseID, description, taskType string) sdkMessage {
	m := object{
		"type": "system", "subtype": "task_started", "uuid": id, "session_id": sessionID,
		"task_id": taskID, "description": description, "task_type": taskType, "is_backgrounded": true,
	}
	if toolUseID != "" {
		m["tool_use_id"] = toolUseID
	}
	return sdkMessage{pb.SessionEvent_KIND_BGTASK_STARTED, m}
}

// sdkTaskNotification is the CLI's task_notification message for an ended task.
func sdkTaskNotification(sessionID, id, taskID, toolUseID, status, summary, outputFile string) sdkMessage {
	m := object{
		"type": "system", "subtype": "task_notification", "uuid": id, "session_id": sessionID,
		"task_id": taskID, "status": status, "summary": summary, "output_file": outputFile,
	}
	if toolUseID != "" {
		m["tool_use_id"] = toolUseID
	}
	return sdkMessage{pb.SessionEvent_KIND_BGTASK_ENDED, m}
}

// lostTaskPayload simulates the CLI's own task_notification for an SDK
// background task orphaned with its worker.
func lostTaskPayload(sessionID, id string, task *pb.PatchSessionState_BackgroundTask, summary string) object {
	payload := sdkTaskNotification(sessionID, id, task.GetTaskId(), task.GetToolUseId(), "stopped", summary, "").payload
	payload["reason"] = "worker_restart"
	return payload
}
