// Package fakemodel is an in-process fake of the Anthropic Messages API,
// enough for the Claude Code CLI: streamed answers (thinking, text,
// parallel tool uses with input JSON deltas), auto mode's server-side check
// (safeguards / safeguard_results), count_tokens, and harmless answers to
// whatever else the CLI asks for. Tests script the main conversation with
// a Go closure that sees each request and returns the next answer; other
// requests (title generation, subagents, ...) get a short text unless a
// side handler is set. Every request is recorded, within a memory budget.
package fakemodel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Handler answers one request. It may block (for example until the test
// lets it answer); it should then also watch r.Context(). A nil answer is
// an empty end_turn message.
type Handler func(r *Request) *Response

// Options configure a Server. The zero value answers every request with a
// short text.
type Options struct {
	// Main answers requests of the main conversation.
	Main Handler
	// Side answers the other Messages requests; by default a short text.
	Side Handler
	// IsMain tells the main conversation's requests from others; by default
	// IsMainConversation.
	IsMain func(*Request) bool
	// RejectSafeguards answers 400 invalid_request_error naming safeguards
	// to every request that carries them (a proxy that cannot pass auto
	// mode's check through).
	RejectSafeguards bool
	// MaxRecords bounds the recorded requests (default 512); the oldest go.
	MaxRecords int
	// MaxRecordBytes bounds the recorded request bodies and their parsed
	// content, counted as twice the body (default 64 MiB): the oldest
	// records lose them first (keeping their metadata).
	MaxRecordBytes int
	// MaxBodyBytes bounds one request body (default 32 MiB); a larger one
	// is answered 413. Each request in flight buffers its whole body, so
	// concurrent requests are not bounded together: fine for tests, whose
	// clients send a few at a time.
	MaxBodyBytes int64
	// Logf, when set, logs each request on one line.
	Logf func(format string, args ...any)
}

const (
	defaultMaxRecords     = 512
	defaultMaxRecordBytes = 64 << 20
	defaultMaxBodyBytes   = 32 << 20
	// SideText is the default answer to side requests.
	SideText = "Fake reply"
)

// IsMainConversation is the default routing: a Messages request of a
// Claude Code session (it names its session in x-claude-code-session-id)
// that offers the built-in tools (Bash among them) and is not a
// subagent's. It does not depend on the wording of the main system prompt;
// tests that need more route by their own markers (Options.IsMain).
func IsMainConversation(r *Request) bool {
	return r.Path == "/v1/messages" && r.HasTool("Bash") && r.Headers["x-claude-code-session-id"] != "" &&
		!strings.Contains(r.System, "You are an agent for Claude Code")
}

// Server is a running fake.
type Server struct {
	opts     Options
	listener net.Listener
	http     *http.Server
	url      string
	closing  chan struct{}
	closed   sync.Once
	serving  chan struct{}

	mu      sync.Mutex
	seq     int
	records []*Request
	bytes   int
	dropped int
	wake    chan struct{}
}

// Start listens on a loopback port and serves until Close.
func Start(opts Options) (*Server, error) {
	if opts.MaxRecords <= 0 {
		opts.MaxRecords = defaultMaxRecords
	}
	if opts.MaxRecordBytes <= 0 {
		opts.MaxRecordBytes = defaultMaxRecordBytes
	}
	if opts.MaxBodyBytes <= 0 {
		opts.MaxBodyBytes = defaultMaxBodyBytes
	}
	if opts.IsMain == nil {
		opts.IsMain = IsMainConversation
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &Server{opts: opts, listener: l, url: "http://" + l.Addr().String(), closing: make(chan struct{}), serving: make(chan struct{}), wake: make(chan struct{})}
	s.http = &http.Server{Handler: s, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		defer close(s.serving)
		_ = s.http.Serve(l)
	}()
	return s, nil
}

// URL is the base URL for ANTHROPIC_BASE_URL.
func (s *Server) URL() string { return s.url }

// Close stops the server: open streams end at once.
func (s *Server) Close() error {
	s.closed.Do(func() { close(s.closing) })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := s.http.Shutdown(ctx)
	if err != nil {
		err = s.http.Close()
	}
	<-s.serving
	return err
}

// Requests returns copies of the recorded requests, oldest first.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Request, len(s.records))
	for i, r := range s.records {
		out[i] = *r
		out[i].ctx = nil
	}
	return out
}

// MainRequests returns copies of the recorded main-conversation requests.
func (s *Server) MainRequests() []Request {
	var out []Request
	for _, r := range s.Requests() {
		if r.Main {
			out = append(out, r)
		}
	}
	return out
}

// Dropped counts the records evicted by the bounds.
func (s *Server) Dropped() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dropped
}

// Wait blocks until a recorded request satisfies match (checked on every
// new or updated record) or ctx ends, and returns a copy of it. match runs
// with the server's lock held: it must not call the Server's methods (that
// deadlocks) and should be quick. Records stripped by the recording budget
// (Request.Stripped) have no content left for match to see.
func (s *Server) Wait(ctx context.Context, match func(*Request) bool) (Request, error) {
	for {
		s.mu.Lock()
		for _, r := range s.records {
			if match(r) {
				c := *r
				c.ctx = nil
				s.mu.Unlock()
				return c, nil
			}
		}
		wake := s.wake
		s.mu.Unlock()
		select {
		case <-wake:
		case <-ctx.Done():
			return Request{}, ctx.Err()
		}
	}
}

func (s *Server) notifyLocked() {
	close(s.wake)
	s.wake = make(chan struct{})
}

func (s *Server) record(r *Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	r.Seq = s.seq
	s.records = append(s.records, r)
	s.bytes += r.cost()
	for len(s.records) > s.opts.MaxRecords {
		s.bytes -= s.records[0].cost()
		s.records[0] = nil
		s.records = s.records[1:]
		s.dropped++
	}
	for i := 0; s.bytes > s.opts.MaxRecordBytes && i < len(s.records); i++ {
		s.bytes -= s.records[i].cost()
		s.records[i].strip()
	}
	s.notifyLocked()
}

func (s *Server) finish(r *Request, status int, outcome Outcome, toolUses []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if status != 0 {
		r.Status = status
	}
	r.Outcome = outcome
	if toolUses != nil {
		r.ToolUseIDs = toolUses
	}
	s.notifyLocked()
}

var keptHeaders = []string{"anthropic-beta", "anthropic-version", "user-agent", "x-app", "x-claude-code-session-id"}

// ServeHTTP answers the Messages API endpoints the CLI uses.
func (s *Server) ServeHTTP(w http.ResponseWriter, hr *http.Request) {
	ctx, cancel := context.WithCancel(hr.Context())
	defer cancel()
	go func() {
		select {
		case <-s.closing:
			cancel()
		case <-ctx.Done():
		}
	}()
	r := &Request{Method: hr.Method, Path: hr.URL.Path, At: time.Now(), Headers: map[string]string{}, ctx: ctx}
	for _, h := range keptHeaders {
		if v := hr.Header.Get(h); v != "" {
			r.Headers[h] = v
		}
	}
	body, err := io.ReadAll(io.LimitReader(hr.Body, s.opts.MaxBodyBytes+1))
	if int64(len(body)) > s.opts.MaxBodyBytes {
		s.record(r)
		s.fail(w, r, http.StatusRequestEntityTooLarge, "request_too_large", "fake: request body too large")
		return
	}
	if err != nil {
		s.record(r)
		s.finish(r, 0, ClientGone, nil)
		return
	}
	r.Body = body
	if hr.Method == http.MethodPost && (r.Path == "/v1/messages" || r.Path == "/v1/messages/count_tokens") {
		r.parse(body)
	}
	if hr.Method == http.MethodPost && r.Path == "/v1/messages" {
		r.Main = s.opts.IsMain(r)
	}
	s.record(r)
	if s.opts.Logf != nil {
		s.opts.Logf("fakemodel: #%d %s %s main=%v model=%s stream=%v messages=%d tools=%d safeguards=%v",
			r.Seq, r.Method, r.Path, r.Main, r.Model, r.Stream, len(r.Messages), len(r.Tools), r.Safeguards != nil)
	}
	switch {
	case hr.Method == http.MethodPost && r.Path == "/v1/messages":
		s.messages(w, r)
	case hr.Method == http.MethodPost && r.Path == "/v1/messages/count_tokens":
		s.json(w, r, http.StatusOK, map[string]any{"input_tokens": max(1, len(body)/4)})
	case hr.Method == http.MethodHead || (hr.Method == http.MethodGet && (r.Path == "/" || r.Path == "/v1")):
		s.json(w, r, http.StatusOK, map[string]any{})
	default:
		s.fail(w, r, http.StatusNotFound, "not_found_error", "fake: "+r.Path+" is not served")
	}
}

func (s *Server) json(w http.ResponseWriter, r *Request, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	err := json.NewEncoder(w).Encode(v)
	outcome := Completed
	if err != nil {
		outcome = ClientGone
	}
	s.finish(r, status, outcome, nil)
}

func (s *Server) fail(w http.ResponseWriter, r *Request, status int, kind, message string) {
	s.json(w, r, status, map[string]any{"type": "error", "error": map[string]any{"type": kind, "message": message}})
}

func (s *Server) messages(w http.ResponseWriter, r *Request) {
	if s.opts.RejectSafeguards && r.Safeguards != nil {
		s.fail(w, r, http.StatusBadRequest, "invalid_request_error", "Unknown field: safeguards (fake: the dangerous-tool-use check is not passed through)")
		return
	}
	handler := s.opts.Side
	if r.Main {
		handler = s.opts.Main
	}
	var res *Response
	if handler == nil {
		res = Text(SideText)
	} else {
		res = handler(r)
	}
	if res == nil {
		res = &Response{}
	}
	if r.Context().Err() != nil {
		s.finish(r, 0, s.gone(), nil)
		return
	}
	if res.Status != 0 {
		kind := res.ErrorType
		if kind == "" {
			kind = "api_error"
		}
		message := res.Error
		if message == "" {
			message = "fake error"
		}
		retry := res.Status == http.StatusRequestTimeout || res.Status == http.StatusConflict ||
			res.Status == http.StatusTooManyRequests || res.Status >= 500
		if res.ShouldRetry != nil {
			retry = *res.ShouldRetry
		}
		w.Header().Set("x-should-retry", fmt.Sprint(retry))
		if retry {
			w.Header().Set("retry-after", fmt.Sprint(res.RetryAfterSeconds))
		}
		s.fail(w, r, res.Status, kind, message)
		return
	}
	msg := s.build(r, res)
	ids := make([]string, 0, len(res.ToolUses))
	for _, b := range msg.Content {
		if b.Type == "tool_use" {
			ids = append(ids, b.ID)
		}
	}
	if !r.Stream {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		out := msg.final()
		if r.Safeguards != nil {
			out["safeguard_results"] = safeguardResults(ids, res.Flagged)
		}
		if err := json.NewEncoder(w).Encode(out); err != nil {
			s.finish(r, http.StatusOK, ClientGone, ids)
			return
		}
		s.finish(r, http.StatusOK, Completed, ids)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	sw := &sseWriter{w: w}
	if f, ok := w.(http.Flusher); ok {
		sw.flush = f.Flush
	}
	var verdicts any
	if r.Safeguards != nil {
		verdicts = safeguardResults(ids, res.Flagged)
	}
	err := writeStream(sw, msg, res, verdicts, func() error {
		if res.Hold == nil {
			return nil
		}
		res.Hold.reach()
		select {
		case <-res.Hold.released:
			return nil
		case <-r.Context().Done():
			return r.Context().Err()
		}
	})
	if err != nil && !errors.Is(err, errStreamFailed) {
		s.finish(r, http.StatusOK, s.gone(), ids)
		return
	}
	s.finish(r, http.StatusOK, Completed, ids)
}

func (s *Server) gone() Outcome {
	select {
	case <-s.closing:
		return ServerClosed
	default:
		return ClientGone
	}
}

func safeguardResults(ids []string, flagged map[string]string) []any {
	verdicts := map[string]any{}
	for _, id := range ids {
		if why, ok := flagged[id]; ok {
			verdicts[id] = map[string]any{"type": "evaluated", "outcome": "flagged", "explanation": why}
		} else {
			verdicts[id] = map[string]any{"type": "evaluated", "outcome": "not_flagged"}
		}
	}
	return []any{map[string]any{"type": "dangerous_tool_use", "status": map[string]any{"type": "available", "tool_uses": verdicts}}}
}

// message is an answer being built.
type message struct {
	ID         string
	Model      string
	Content    []outBlock
	StopReason string
	In, Out    int
}

type outBlock struct {
	Type  string
	Text  string
	ID    string
	Name  string
	Input json.RawMessage
}

func (s *Server) build(r *Request, res *Response) message {
	m := message{ID: fmt.Sprintf("msg_fake_%d", r.Seq), Model: r.Model, StopReason: res.StopReason, In: res.InputTokens, Out: res.OutputTokens}
	if m.Model == "" {
		m.Model = "claude-fake"
	}
	if m.In == 0 {
		m.In = 25
	}
	if m.Out == 0 {
		m.Out = 10
	}
	if res.Thinking != "" {
		m.Content = append(m.Content, outBlock{Type: "thinking", Text: res.Thinking})
	}
	if res.Text != "" {
		m.Content = append(m.Content, outBlock{Type: "text", Text: res.Text})
	}
	for i, u := range res.ToolUses {
		id := u.ID
		if id == "" {
			id = fmt.Sprintf("toolu_fake_%d_%d", r.Seq, i)
		}
		input := json.RawMessage("{}")
		if u.Input != nil {
			if raw, ok := u.Input.(json.RawMessage); ok {
				input = raw
			} else if b, err := json.Marshal(u.Input); err == nil {
				input = b
			}
		}
		m.Content = append(m.Content, outBlock{Type: "tool_use", ID: id, Name: u.Name, Input: input})
	}
	if m.StopReason == "" {
		m.StopReason = "end_turn"
		if len(res.ToolUses) > 0 {
			m.StopReason = "tool_use"
		}
	}
	return m
}

func (b outBlock) final() map[string]any {
	switch b.Type {
	case "thinking":
		return map[string]any{"type": "thinking", "thinking": b.Text, "signature": fakeSignature}
	case "tool_use":
		return map[string]any{"type": "tool_use", "id": b.ID, "name": b.Name, "input": b.Input}
	}
	return map[string]any{"type": "text", "text": b.Text}
}

func (m message) usage(out int) map[string]any {
	return map[string]any{"input_tokens": m.In, "output_tokens": out, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0}
}

func (m message) final() map[string]any {
	content := make([]any, 0, len(m.Content))
	for _, b := range m.Content {
		content = append(content, b.final())
	}
	return map[string]any{
		"id": m.ID, "type": "message", "role": "assistant", "model": m.Model, "content": content,
		"stop_reason": m.StopReason, "stop_sequence": nil, "usage": m.usage(m.Out),
	}
}
