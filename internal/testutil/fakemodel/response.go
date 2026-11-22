package fakemodel

import (
	"sync"
)

// Response is a scripted answer to one request.
type Response struct {
	// Thinking, when set, is streamed as a thinking block first.
	Thinking string
	// Text, when set, is streamed as a text block (after Thinking).
	Text string
	// ToolUses are streamed as tool_use blocks after the text, in order:
	// several make parallel tool uses.
	ToolUses []ToolUse
	// StopReason defaults to tool_use when there are tool uses, else end_turn.
	StopReason string
	// Flagged maps tool_use ids to the explanation of a flagged verdict.
	// On a request that carries safeguards (auto mode), every tool use of
	// the answer gets a verdict in message_delta.delta.safeguard_results:
	// flagged for the ids here, not_flagged for the others.
	Flagged map[string]string
	// Hold, when set, pauses the stream after the first delta of the first
	// content block until the gate is released, the client goes away or
	// the server closes.
	Hold *Gate
	// Chunks is the number of deltas a text is split into (default 3).
	Chunks int
	// Status, when not zero, answers with this HTTP status and an error
	// body of ErrorType (default api_error) and Error instead.
	Status    int
	ErrorType string
	Error     string
	// ShouldRetry sets the x-should-retry header of an error status; by
	// default true for 408, 409, 429 and 5xx (Claude Code then retries).
	// A retryable error also carries retry-after: RetryAfterSeconds (default
	// 0, so a scripted 429 or 529 does not make the client back off long).
	ShouldRetry       *bool
	RetryAfterSeconds int
	// StreamError, when set, ends the stream after the first delta of the
	// first content block with an "event: error" of this type (for example
	// overloaded_error) and StreamErrorMessage, as the API does when a
	// streamed answer fails midway.
	StreamError        string
	StreamErrorMessage string
	// InputTokens and OutputTokens are the reported usage (defaults 25 and 10).
	InputTokens, OutputTokens int
}

// ToolUse is one tool_use block of an answer. An empty ID is generated.
// Input is marshaled to JSON (nil is {}).
type ToolUse struct {
	ID    string
	Name  string
	Input any
}

// Text answers with a text block.
func Text(text string) *Response { return &Response{Text: text} }

// Tools answers with tool_use blocks.
func Tools(uses ...ToolUse) *Response { return &Response{ToolUses: uses} }

// Use builds a ToolUse.
func Use(id, name string, input any) ToolUse { return ToolUse{ID: id, Name: name, Input: input} }

// Bash builds a Bash tool use.
func Bash(id, command string) ToolUse {
	return ToolUse{ID: id, Name: "Bash", Input: map[string]any{"command": command, "description": "Run " + clip(command, 40)}}
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// Gate pauses a stream (Response.Hold) until released.
type Gate struct {
	reachOnce, releaseOnce sync.Once
	reached, released      chan struct{}
}

// NewGate returns a closed-off gate.
func NewGate() *Gate {
	return &Gate{reached: make(chan struct{}), released: make(chan struct{})}
}

// Reached is closed once a stream is paused at the gate.
func (g *Gate) Reached() <-chan struct{} { return g.reached }

// Release lets the paused stream (and any later one) continue.
func (g *Gate) Release() { g.releaseOnce.Do(func() { close(g.released) }) }

func (g *Gate) reach() { g.reachOnce.Do(func() { close(g.reached) }) }
