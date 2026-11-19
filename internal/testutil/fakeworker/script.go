package fakeworker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	pb "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/google/uuid"
)

func (s *session) script(t *turn, in *input) error {
	if in.GetKind() == pb.SessionReply_Inputs_Input_KIND_APPROVAL {
		return s.retryApproval(t, in.GetText())
	}
	text := in.GetText()
	command, arg, _ := strings.Cut(strings.TrimPrefix(text, "/fake "), " ")
	if strings.HasPrefix(text, "/fake ") {
		switch command {
		case "bash", "read", "approve", "job":
			if strings.TrimSpace(arg) == "" {
				return fmt.Errorf("/fake %s requires an argument", command)
			}
			return s.tool(t, command, arg)
		case "slow":
			duration, err := time.ParseDuration(strings.TrimSpace(arg) + "s")
			if err != nil || duration < 0 {
				return errors.New("/fake slow requires a nonnegative number of seconds")
			}
			return s.stream(t, quote(text), duration/4, false)
		case "think":
			return s.stream(t, quote(text), s.w.opts.StreamDelay, true)
		case "break":
			return errMirror
		case "error":
			return errScript
		case "crash":
			return s.disconnect(crash)
		case "blip":
			if err := s.disconnect(blip); err != nil {
				return err
			}
			if err := s.serviceHints(t); err != nil {
				return err
			}
		}
	}
	return s.stream(t, quote(text), s.w.opts.StreamDelay, false)
}

func quote(text string) string {
	return "You said:\n\n> " + strings.ReplaceAll(text, "\n", "\n> ")
}

func (s *session) disconnect(mode outage) error {
	for {
		link, err := s.w.connection(s.ctx)
		if err != nil {
			return err
		}
		if s.w.disrupt(link, mode) {
			break
		}
	}
	if mode == crash {
		return errCrash
	}
	_, err := s.w.connection(s.ctx)
	return err
}

func (s *session) assistant(id string, block object, inputTokens, outputTokens int, aborted bool) error {
	message := sdkAssistant(s.sdkID, uuid.NewString(), id, s.model, block)
	message.payload["message"].(object)["usage"] = sdkUsage(inputTokens, outputTokens)
	if aborted {
		message.payload["aborted"] = true
	}
	return s.record(message)
}

func (s *session) partial(event object) {
	message := sdkPartial(s.sdkID, uuid.NewString(), event)
	s.events = append(s.events, message.event())
}

func (s *session) stream(t *turn, text string, delay time.Duration, think bool) error {
	messageID := "msg_" + uuid.NewString()
	inputTokens := 24 + t.inputTokens
	t.requests++
	t.usedInput += inputTokens
	start := apiMessage(messageID, s.model, []object{})
	start["usage"] = sdkUsage(inputTokens, 1)
	s.partial(object{"type": "message_start", "message": start})
	index, thinkingTokens := 0, 0
	if think {
		block := thinkingBlock()
		s.partial(object{"type": "content_block_start", "index": index, "content_block": object{"type": "thinking", "thinking": "", "signature": ""}})
		s.partial(object{"type": "content_block_delta", "index": index, "delta": object{"type": "thinking_delta", "thinking": block["thinking"]}})
		s.partial(object{"type": "content_block_delta", "index": index, "delta": object{"type": "signature_delta", "signature": block["signature"]}})
		s.partial(object{"type": "content_block_stop", "index": index})
		if err := s.assistant(messageID, block, inputTokens, 1, false); err != nil {
			return err
		}
		thinkingTokens = tokens(block["thinking"].(string))
		index++
	}
	s.partial(object{"type": "content_block_start", "index": index, "content_block": textBlock("")})
	runes := []rune(text)
	for i := range 4 {
		end := (i + 1) * len(runes) / 4
		if piece := string(runes[i*len(runes)/4 : end]); piece != "" {
			s.partial(textDelta(index, piece))
		}
		if err := s.flush(); err != nil {
			return err
		}
		if err := s.wait(t, delay); err != nil {
			if isAbort(err) {
				if saveErr := s.finishStream(t, messageID, index, inputTokens, thinkingTokens, string(runes[:end]), true); saveErr != nil {
					return errors.Join(err, saveErr)
				}
			}
			return err
		}
	}
	return s.finishStream(t, messageID, index, inputTokens, thinkingTokens, text, false)
}

func (s *session) finishStream(t *turn, id string, index, inputTokens, thinkingTokens int, text string, aborted bool) error {
	s.partial(object{"type": "content_block_stop", "index": index})
	var stopReason any = "end_turn"
	if aborted {
		stopReason = nil
	}
	outputTokens := tokens(text) + thinkingTokens
	s.partial(object{
		"type": "message_delta", "context_management": nil,
		"delta": object{"stop_reason": stopReason, "stop_sequence": nil, "stop_details": nil, "container": nil},
		"usage": object{
			"input_tokens": inputTokens, "output_tokens": outputTokens,
			"cache_creation_input_tokens": 0, "cache_read_input_tokens": 0, "server_tool_use": nil,
			"iterations": nil, "output_tokens_details": object{"thinking_tokens": thinkingTokens},
		},
	})
	end := sdkPartial(s.sdkID, uuid.NewString(), object{"type": "message_stop"})
	if aborted {
		end.payload["abandoned_blocks"] = object{"api_message_id": id, "from_block_index": index + 1}
		t.abortText = text
	}
	s.events = append(s.events, end.event())
	if err := s.flush(); err != nil {
		return err
	}
	if text != "" {
		if err := s.assistant(id, textBlock(text), inputTokens, max(1, outputTokens), aborted); err != nil {
			return err
		}
	}
	t.outputTokens += outputTokens
	t.thinkingTokens += thinkingTokens
	t.text = text
	return nil
}

func tokens(text string) int { return (len(text) + 3) / 4 }

func (s *session) wait(t *turn, delay time.Duration) error {
	timer := s.w.opts.Clock.After(delay)
	for {
		if err := s.serviceHints(t); err != nil {
			return err
		}
		select {
		case <-timer:
			return s.serviceHints(t)
		case <-s.wake:
		case <-s.ctx.Done():
			return context.Cause(s.ctx)
		}
	}
}
