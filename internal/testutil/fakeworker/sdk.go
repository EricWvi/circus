package fakeworker

import (
	"bytes"
	"encoding/json"
	"maps"
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
	payload["parentUuid"] = nil
	if parent != "" {
		payload["parentUuid"] = parent
	}
	return &pb.TranscriptEntry{Uuid: payload["uuid"].(string), Json: jsonString(payload)}
}

func sdkEnvelope(kind, sessionID, id string) object {
	return object{"type": kind, "session_id": sessionID, "uuid": id, "parent_tool_use_id": nil}
}

func sdkUser(sessionID, id string, content any, synthetic bool) sdkMessage {
	m := sdkEnvelope("user", sessionID, id)
	m["message"] = object{"role": "user", "content": content}
	if synthetic {
		m["isSynthetic"] = true
	}
	return sdkMessage{pb.SessionEvent_KIND_USER, m}
}

func sdkAssistant(sessionID, id, messageID, model string, block object) sdkMessage {
	m := sdkEnvelope("assistant", sessionID, id)
	m["message"] = apiMessage(messageID, model, []object{block})
	return sdkMessage{pb.SessionEvent_KIND_ASSISTANT, m}
}

func apiMessage(id, model string, blocks []object) object {
	return object{
		"id": id, "type": "message", "role": "assistant", "model": model, "content": blocks,
		"stop_reason": nil, "stop_details": nil, "stop_sequence": nil,
		"container": nil, "context_management": nil, "diagnostics": nil, "usage": sdkUsage(0, 0),
	}
}

func sdkPartial(sessionID, id string, event object) sdkMessage {
	m := sdkEnvelope("stream_event", sessionID, id)
	m["event"] = event
	return sdkMessage{pb.SessionEvent_KIND_ASSISTANT, m}
}

func sdkUsage(input, output int) object {
	return object{
		"input_tokens": input, "output_tokens": output, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0,
		"cache_creation": object{"ephemeral_1h_input_tokens": 0, "ephemeral_5m_input_tokens": 0},
		"inference_geo":  "", "iterations": []any{}, "output_tokens_details": object{"thinking_tokens": 0},
		"server_tool_use": object{"web_fetch_requests": 0, "web_search_requests": 0},
		"service_tier":    "standard", "speed": "standard",
	}
}

func sdkResult(sessionID, id, text, failure string, duration int64) sdkMessage {
	m := object{
		"type": "result", "uuid": id, "session_id": sessionID, "duration_ms": duration, "duration_api_ms": 0,
		"num_turns": 1, "stop_reason": "end_turn", "total_cost_usd": 0, "usage": sdkUsage(0, 0),
		"modelUsage": object{}, "permission_denials": []any{}, "is_error": false,
		"subtype": "success",
	}
	if failure == "" {
		m["result"] = text
	} else {
		m["subtype"] = "error_during_execution"
		m["is_error"] = true
		m["stop_reason"] = nil
		m["errors"] = []string{failure}
	}
	return sdkMessage{pb.SessionEvent_KIND_RESULT, m}
}

func applyAbortResult(result sdkMessage, reason, text string) {
	if reason == "" {
		reason = "aborted_streaming"
	}
	result.payload["terminal_reason"] = reason
	result.payload["stop_reason"] = nil
	if result.payload["subtype"] == "success" {
		// A tool interrupted by NOW was observed to produce an empty result.
		// Its success subtype and aborted_tools terminal reason are inferred.
		result.payload["result"] = text
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
	result.payload["modelUsage"] = object{model: object{
		"inputTokens": total.Input, "outputTokens": total.Output, "cacheReadInputTokens": 0, "cacheCreationInputTokens": 0,
		"thinkingTokens": total.Thinking, "webSearchRequests": 0, "costUSD": totalCost, "contextWindow": 200000, "maxOutputTokens": 64000,
	}}
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
	return object{"type": "text", "text": text, "citations": nil}
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
