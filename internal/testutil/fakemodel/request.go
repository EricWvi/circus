package fakemodel

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

// Outcome is how the server's answer to a request ended.
type Outcome int

const (
	// Pending: the answer is still being written.
	Pending Outcome = iota
	// Completed: the whole answer was written.
	Completed
	// ClientGone: the client went away before the answer ended (an aborted
	// stream, for example after an interrupt or when its process died).
	ClientGone
	// ServerClosed: the server closed before the answer ended.
	ServerClosed
)

func (o Outcome) String() string {
	switch o {
	case Pending:
		return "pending"
	case Completed:
		return "completed"
	case ClientGone:
		return "client gone"
	case ServerClosed:
		return "server closed"
	}
	return "unknown"
}

// Request is one HTTP request the server received, parsed as far as it is a
// Messages API request. Handlers get a pointer they must not keep after
// returning; Server.Requests returns copies.
type Request struct {
	// Seq numbers every request the server received, from 1.
	Seq    int
	Method string
	Path   string
	At     time.Time
	// Main: a request of the main conversation (routed to Options.Main).
	Main bool
	// Messages API fields.
	Model     string
	Stream    bool
	MaxTokens int
	System    string
	Messages  []Message
	Tools     []string
	// Safeguards is the top-level safeguards field auto mode adds, raw.
	Safeguards json.RawMessage
	// Headers holds a few non-credential headers (anthropic-beta,
	// anthropic-version, user-agent, x-app, x-claude-code-session-id).
	Headers map[string]string
	// Body is the raw request body. Body, System, Messages and Tools are
	// dropped from old records when the recording budget is exceeded
	// (Stripped is then set).
	Body     []byte
	Stripped bool
	// Status is the HTTP status answered (0 until known).
	Status  int
	Outcome Outcome
	// ToolUseIDs are the tool_use ids of the answer.
	ToolUseIDs []string

	ctx context.Context
}

// cost is what a record counts against the recording budget: its body
// and, about as much, its parsed content.
func (r *Request) cost() int {
	if r.Stripped {
		return 0
	}
	return 2 * len(r.Body)
}

func (r *Request) strip() {
	r.Body, r.System, r.Messages, r.Tools, r.Safeguards = nil, "", nil, nil, nil
	r.Stripped = true
}

// Context is cancelled when the client goes away or the server closes.
func (r *Request) Context() context.Context {
	if r.ctx == nil {
		return context.Background()
	}
	return r.ctx
}

// Message is one entry of the request's messages.
type Message struct {
	Role    string
	Content []Block
}

// Block is a content block. Text holds the text of text and thinking blocks;
// ID, Name and Input a tool_use; ToolUseID, IsError and Content a
// tool_result (Content raw: a string or an array of blocks).
type Block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
}

// ResultText is the text of a tool_result: its string content, or its text
// blocks joined with newlines.
func (b Block) ResultText() string {
	return contentText(b.Content)
}

// String is the block's readable text: text, thinking, a tool_result's text
// or a tool_use's input.
func (b Block) String() string {
	switch b.Type {
	case "text":
		return b.Text
	case "thinking":
		return b.Thinking
	case "tool_result":
		return b.ResultText()
	case "tool_use":
		return string(b.Input)
	}
	return ""
}

// Text joins the readable text of the message's blocks with newlines.
func (m Message) Text() string {
	parts := make([]string, 0, len(m.Content))
	for _, b := range m.Content {
		if s := b.String(); s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, "\n")
}

// Last is the last message, or a zero Message.
func (r *Request) Last() Message {
	if len(r.Messages) == 0 {
		return Message{}
	}
	return r.Messages[len(r.Messages)-1]
}

// Assistants counts the assistant messages: how many answers of this
// conversation the client replays.
func (r *Request) Assistants() int {
	n := 0
	for _, m := range r.Messages {
		if m.Role == "assistant" {
			n++
		}
	}
	return n
}

// ToolResult finds the tool_result for a tool_use id.
func (r *Request) ToolResult(id string) (Block, bool) {
	for _, m := range r.Messages {
		for _, b := range m.Content {
			if b.Type == "tool_result" && b.ToolUseID == id {
				return b, true
			}
		}
	}
	return Block{}, false
}

// ToolUse finds a replayed tool_use by id.
func (r *Request) ToolUse(id string) (Block, bool) {
	for _, m := range r.Messages {
		for _, b := range m.Content {
			if b.Type == "tool_use" && b.ID == id {
				return b, true
			}
		}
	}
	return Block{}, false
}

// Text is the readable text of every message, one message per line group.
func (r *Request) Text() string {
	parts := make([]string, 0, len(r.Messages))
	for _, m := range r.Messages {
		parts = append(parts, m.Text())
	}
	return strings.Join(parts, "\n")
}

// Contains reports whether any message's readable text contains s.
func (r *Request) Contains(s string) bool {
	for _, m := range r.Messages {
		for _, b := range m.Content {
			if strings.Contains(b.String(), s) {
				return true
			}
		}
	}
	return false
}

// Count counts the messages whose readable text contains s.
func (r *Request) Count(s string) int {
	n := 0
	for _, m := range r.Messages {
		for _, b := range m.Content {
			n += strings.Count(b.String(), s)
		}
	}
	return n
}

const (
	midTurnPrefix = "The user sent a new message while you were working:\n"
	midTurnEnd    = "\n\nThis is how Claude Code surfaces messages the user sends mid-turn"
)

// MidTurnMessages are the user messages Claude Code merged into the running
// turn ("The user sent a new message while you were working", in a system
// message or a <system-reminder> block), in order.
func (r *Request) MidTurnMessages() []string {
	var out []string
	for _, m := range r.Messages {
		for _, b := range m.Content {
			out = append(out, between(b.String(), midTurnPrefix, midTurnEnd)...)
		}
	}
	return out
}

// Notifications are the <task-notification> blocks in the messages, whole.
func (r *Request) Notifications() []string {
	var out []string
	for _, m := range r.Messages {
		for _, b := range m.Content {
			for _, body := range between(b.String(), "<task-notification>", "</task-notification>") {
				out = append(out, "<task-notification>"+body+"</task-notification>")
			}
		}
	}
	return out
}

// between returns every text found between open and close, in order.
func between(s, open, close string) []string {
	var out []string
	for {
		i := strings.Index(s, open)
		if i < 0 {
			return out
		}
		s = s[i+len(open):]
		j := strings.Index(s, close)
		if j < 0 {
			return out
		}
		out = append(out, s[:j])
		s = s[j+len(close):]
	}
}

func contentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []Block
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	parts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		if s := b.String(); s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, "\n")
}

// wireRequest is the part of a Messages API request body the server reads.
type wireRequest struct {
	Model      string                  `json:"model"`
	Stream     bool                    `json:"stream"`
	MaxTokens  int                     `json:"max_tokens"`
	System     json.RawMessage         `json:"system"`
	Messages   []wireMessage           `json:"messages"`
	Tools      []struct{ Name string } `json:"tools"`
	Safeguards json.RawMessage         `json:"safeguards"`
}

type wireMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

func (r *Request) parse(body []byte) {
	var w wireRequest
	if json.Unmarshal(body, &w) != nil {
		return
	}
	r.Model, r.Stream, r.MaxTokens = w.Model, w.Stream, w.MaxTokens
	r.System = contentText(w.System)
	if len(w.Safeguards) > 0 && string(w.Safeguards) != "null" {
		r.Safeguards = w.Safeguards
	}
	for _, t := range w.Tools {
		r.Tools = append(r.Tools, t.Name)
	}
	for _, m := range w.Messages {
		msg := Message{Role: m.Role}
		var s string
		if json.Unmarshal(m.Content, &s) == nil {
			msg.Content = []Block{{Type: "text", Text: s}}
		} else {
			_ = json.Unmarshal(m.Content, &msg.Content)
		}
		r.Messages = append(r.Messages, msg)
	}
}

// HasTool reports whether the request offers a tool of this name.
func (r *Request) HasTool(name string) bool {
	for _, t := range r.Tools {
		if t == name {
			return true
		}
	}
	return false
}
