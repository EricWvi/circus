package fakeworker

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	pb "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/google/uuid"
)

func (s *session) script(t *turn, in *input) error {
	text := in.GetText()
	switch in.GetKind() {
	case pb.SessionReply_Inputs_Input_KIND_APPROVAL:
		return s.retryApproval(t, text)
	case pb.SessionReply_Inputs_Input_KIND_NOTE, pb.SessionReply_Inputs_Input_KIND_ANSWER:
		// The control plane's note and the user's answers are answered as
		// text, never run as a script.
		return s.stream(t, quote(text), s.w.opts.StreamDelay, plainText)
	}
	command, arg, _ := strings.Cut(strings.TrimPrefix(text, "/fake "), " ")
	if strings.HasPrefix(text, "/fake ") {
		switch command {
		case "bash", "read", "approve", "job":
			if strings.TrimSpace(arg) == "" {
				return fmt.Errorf("/fake %s requires an argument", command)
			}
			return s.tool(t, command, arg)
		case "ask":
			return s.ask(t)
		case "ledger-path":
			return s.stream(t, "ledger path: "+cmp.Or(s.ledgerPath, "none"), s.w.opts.StreamDelay, plainText)
		case "ledger":
			return s.ledger(t, arg)
		case "parallel":
			if strings.TrimSpace(arg) == "" {
				return errors.New("/fake parallel requires an argument")
			}
			return s.parallel(t, arg)
		case "large":
			n, err := strconv.Atoi(strings.TrimSpace(arg))
			if err != nil || n < 2 || n > 1<<20 {
				return errors.New("/fake large requires a size of 2 to 1048576 bytes")
			}
			return s.largeResult(t, n)
		case "slow":
			duration, err := time.ParseDuration(strings.TrimSpace(arg) + "s")
			if err != nil || duration < 0 {
				return errors.New("/fake slow requires a nonnegative number of seconds")
			}
			return s.stream(t, quote(text), duration/4, plainText)
		case "think":
			return s.stream(t, quote(text), s.w.opts.StreamDelay, thinkFirst)
		case "break":
			return errMirror
		case "error":
			return errScript
		case "crash":
			// The process dies while the model's answer streams.
			return s.stream(t, quote(text), s.w.opts.StreamDelay, crashMidStream)
		case "blip":
			if err := s.disconnect(blip); err != nil {
				return err
			}
			if err := s.serviceHints(t); err != nil {
				return err
			}
		}
	}
	return s.stream(t, quote(text), s.w.opts.StreamDelay, plainText)
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

// apiRequest is one model response the script streams. Claude Code
// publishes a response's stream events and, after each content block's
// last delta, the block as a durable assistant message.
type apiRequest struct {
	id     string
	first  bool
	input  int
	blocks int
}

// begin starts the turn's next response with its message_start. The turn's
// first response, and its first block, name the user message that started
// the turn.
func (s *session) begin(t *turn) *apiRequest {
	r := &apiRequest{id: "msg_" + uuid.NewString(), first: t.requests == 0, input: 24 + t.inputTokens}
	if r.first {
		t.firstInput = r.input
	}
	t.requests++
	t.usedInput += r.input
	t.messageIDs = append(t.messageIDs, r.id)
	m := sdkMessageStart(s.sdkID, uuid.NewString(), r.id, s.model, r.input)
	if r.first {
		m.payload["owed_rows"] = []any{}
		nameUserMessage(m.payload, t.userMessageID)
	}
	s.events = append(s.events, m.event())
	return r
}

// streamEvent is a stream_event of r; thinking marks the events of a
// thinking block that carry the thinking display mode.
func (s *session) streamEvent(r *apiRequest, event object, thinking bool) *pb.PublishEvents_Event {
	m := sdkPartial(s.sdkID, uuid.NewString(), r.id, event)
	if thinking {
		m.payload["thinking_display"] = thinkingDisplay
	}
	return m.event()
}

func (s *session) partial(r *apiRequest, event object, thinking bool) {
	s.events = append(s.events, s.streamEvent(r, event, thinking))
}

// assistant records block as r's next durable assistant message.
func (s *session) assistant(t *turn, r *apiRequest, block object, outputTokens int, aborted bool) error {
	message := sdkAssistant(s.sdkID, uuid.NewString(), r.id, s.model, block)
	message.payload["message"].(object)["usage"] = messageUsage(r.input, outputTokens)
	if r.first && r.blocks == 0 {
		nameUserMessage(message.payload, t.userMessageID)
	}
	r.blocks++
	if aborted {
		message.payload["aborted"] = true
	}
	return s.record(message)
}

func blockStop(index int) object { return object{"type": "content_block_stop", "index": index} }

type streamMode int

const (
	plainText streamMode = iota
	// thinkFirst streams a thinking block before the text.
	thinkFirst
	// crashMidStream crashes the worker after the text's first chunk.
	crashMidStream
)

func (s *session) stream(t *turn, text string, delay time.Duration, mode streamMode) error {
	r := s.begin(t)
	index, thinkingTokens := 0, 0
	if mode == thinkFirst {
		block := thinkingBlock()
		s.partial(r, object{"type": "content_block_start", "index": index, "content_block": object{"type": "thinking", "thinking": "", "signature": ""}}, true)
		s.partial(r, object{"type": "content_block_delta", "index": index, "delta": object{"type": "thinking_delta", "thinking": block["thinking"]}}, true)
		s.partial(r, object{"type": "content_block_delta", "index": index, "delta": object{"type": "signature_delta", "signature": block["signature"]}}, false)
		if err := s.assistant(t, r, block, 1, false); err != nil {
			return err
		}
		s.partial(r, blockStop(index), false)
		thinkingTokens = tokens(block["thinking"].(string))
		index++
	}
	s.partial(r, object{"type": "content_block_start", "index": index, "content_block": textBlock("")}, false)
	runes := []rune(text)
	for i := range 4 {
		end := (i + 1) * len(runes) / 4
		if piece := string(runes[i*len(runes)/4 : end]); piece != "" {
			s.partial(r, textDelta(index, piece), false)
		}
		if err := s.flush(); err != nil {
			return err
		}
		if mode == crashMidStream {
			return s.disconnect(crash)
		}
		if err := s.wait(t, delay); err != nil {
			if isAbort(err) {
				if saveErr := s.finishStream(t, r, index, thinkingTokens, string(runes[:end]), true); saveErr != nil {
					return errors.Join(err, saveErr)
				}
			}
			return err
		}
	}
	return s.finishStream(t, r, index, thinkingTokens, text, false)
}

// finishStream records the text block and ends the response's stream. An
// aborted response's stream ends after the turn's interrupt annotation
// (finishTurn publishes t.tail), as Claude Code's does.
func (s *session) finishStream(t *turn, r *apiRequest, index, thinkingTokens int, text string, aborted bool) error {
	outputTokens := tokens(text) + thinkingTokens
	if text != "" {
		if err := s.assistant(t, r, textBlock(text), max(1, outputTokens), aborted); err != nil {
			return err
		}
	}
	t.outputTokens += outputTokens
	t.thinkingTokens += thinkingTokens
	t.text = text
	var stopReason any = "end_turn"
	if aborted {
		stopReason = nil
		t.abortText = text
	}
	tail := []*pb.PublishEvents_Event{
		s.streamEvent(r, blockStop(index), false),
		s.streamEvent(r, messageDelta(stopReason, r.input, outputTokens, thinkingTokens), false),
	}
	end := sdkPartial(s.sdkID, uuid.NewString(), r.id, object{"type": "message_stop"})
	if aborted {
		end.payload["abandoned_blocks"] = object{"api_message_id": r.id, "from_block_index": index + 1}
	}
	tail = append(tail, end.event())
	if aborted {
		t.tail = tail
		return nil
	}
	s.events = append(s.events, tail...)
	return s.flush()
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
