package fakemodel

import (
	"encoding/json"
	"errors"
	"io"
	"unicode/utf8"
)

// fakeSignature is the signature of thinking blocks.
const fakeSignature = "ZmFrZS10aGlua2luZy1zaWduYXR1cmU="

// sseWriter writes server-sent events and flushes each one.
type sseWriter struct {
	w     io.Writer
	flush func()
}

// event writes one event: "event: <type>\ndata: <json>\n\n". data must
// carry the same "type".
func (s *sseWriter) event(kind string, data map[string]any) error {
	data["type"] = kind
	payload, err := json.Marshal(data)
	if err != nil {
		return err
	}
	buf := make([]byte, 0, len(kind)+len(payload)+16)
	buf = append(buf, "event: "...)
	buf = append(buf, kind...)
	buf = append(buf, "\ndata: "...)
	buf = append(buf, payload...)
	buf = append(buf, "\n\n"...)
	if _, err := s.w.Write(buf); err != nil {
		return err
	}
	if s.flush != nil {
		s.flush()
	}
	return nil
}

// writeStream streams msg as Messages API events. hold runs once, after the
// first delta of the first content block; an error from it (or from a
// write) ends the stream there.
func writeStream(s *sseWriter, msg message, res *Response, verdicts any, hold func() error) error {
	start := msg.final()
	start["content"] = []any{}
	start["stop_reason"] = nil
	start["usage"] = msg.usage(1)
	if err := s.event("message_start", map[string]any{"message": start}); err != nil {
		return err
	}
	if err := s.event("ping", map[string]any{}); err != nil {
		return err
	}
	held := false
	pause := func() error {
		if held {
			return nil
		}
		held = true
		if err := hold(); err != nil {
			return err
		}
		if res.StreamError != "" {
			message := res.StreamErrorMessage
			if message == "" {
				message = "fake stream error"
			}
			if err := s.event("error", map[string]any{"error": map[string]any{"type": res.StreamError, "message": message}}); err != nil {
				return err
			}
			return errStreamFailed
		}
		return nil
	}
	chunks := res.Chunks
	if chunks <= 0 {
		chunks = 3
	}
	for i, b := range msg.Content {
		var first map[string]any
		var deltas []map[string]any
		switch b.Type {
		case "thinking":
			first = map[string]any{"type": "thinking", "thinking": "", "signature": ""}
			for _, part := range split(b.Text, chunks) {
				deltas = append(deltas, map[string]any{"type": "thinking_delta", "thinking": part})
			}
			deltas = append(deltas, map[string]any{"type": "signature_delta", "signature": fakeSignature})
		case "tool_use":
			first = map[string]any{"type": "tool_use", "id": b.ID, "name": b.Name, "input": map[string]any{}}
			for _, part := range split(string(b.Input), 2) {
				deltas = append(deltas, map[string]any{"type": "input_json_delta", "partial_json": part})
			}
		default:
			first = map[string]any{"type": "text", "text": ""}
			for _, part := range split(b.Text, chunks) {
				deltas = append(deltas, map[string]any{"type": "text_delta", "text": part})
			}
		}
		if err := s.event("content_block_start", map[string]any{"index": i, "content_block": first}); err != nil {
			return err
		}
		for _, d := range deltas {
			if err := s.event("content_block_delta", map[string]any{"index": i, "delta": d}); err != nil {
				return err
			}
			if err := pause(); err != nil {
				return err
			}
		}
		if err := s.event("content_block_stop", map[string]any{"index": i}); err != nil {
			return err
		}
	}
	if err := pause(); err != nil {
		return err
	}
	delta := map[string]any{"stop_reason": msg.StopReason, "stop_sequence": nil}
	if verdicts != nil {
		delta["safeguard_results"] = verdicts
	}
	if err := s.event("message_delta", map[string]any{"delta": delta, "usage": map[string]any{"output_tokens": msg.Out}}); err != nil {
		return err
	}
	return s.event("message_stop", map[string]any{})
}

// errStreamFailed ends a stream after its scripted error event.
var errStreamFailed = errors.New("fakemodel: scripted stream error")

// split cuts s into at most n non-empty parts on rune boundaries (one
// empty part for an empty s).
func split(s string, n int) []string {
	if s == "" {
		return []string{""}
	}
	runes := utf8.RuneCountInString(s)
	if n > runes {
		n = runes
	}
	parts := make([]string, 0, n)
	size := (runes + n - 1) / n
	for len(s) > 0 {
		end, count := 0, 0
		for end < len(s) && count < size {
			_, w := utf8.DecodeRuneInString(s[end:])
			end += w
			count++
		}
		parts = append(parts, s[:end])
		s = s[end:]
	}
	return parts
}
