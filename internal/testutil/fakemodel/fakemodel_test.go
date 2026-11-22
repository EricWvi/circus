package fakemodel

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type sseEvent struct {
	Name string
	Data map[string]any
	Raw  string
}

// parseSSE reads a whole stream, checking the framing: every event is an
// "event:" line, one "data:" line whose JSON type equals the event name,
// and a blank line.
func parseSSE(t *testing.T, r io.Reader) []sseEvent {
	t.Helper()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	var out []sseEvent
	for sc.Scan() {
		line := sc.Text()
		name, ok := strings.CutPrefix(line, "event: ")
		if !ok {
			t.Fatalf("expected an event line, got %q", line)
		}
		if !sc.Scan() {
			t.Fatalf("event %q without data", name)
		}
		raw, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			t.Fatalf("expected a data line after %q, got %q", name, sc.Text())
		}
		if !sc.Scan() || sc.Text() != "" {
			t.Fatalf("event %q not terminated by a blank line", name)
		}
		var data map[string]any
		if err := json.Unmarshal([]byte(raw), &data); err != nil {
			t.Fatalf("event %q data %q: %v", name, raw, err)
		}
		if data["type"] != name {
			t.Fatalf("event %q carries type %v", name, data["type"])
		}
		out = append(out, sseEvent{Name: name, Data: data, Raw: raw})
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func start(t *testing.T, opts Options) *Server {
	t.Helper()
	s, err := Start(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func post(t *testing.T, ctx context.Context, url string, body any) *http.Response {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("x-api-key", "secret-not-recorded")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("x-claude-code-session-id", "session-1")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = res.Body.Close() })
	return res
}

func mainBody(stream bool, extra map[string]any) map[string]any {
	body := map[string]any{
		"model": "claude-test", "max_tokens": 1000, "stream": stream,
		"system":   []any{map[string]any{"type": "text", "text": "You are Claude Code, Anthropic's official CLI for Claude."}},
		"tools":    []any{map[string]any{"name": "Bash"}, map[string]any{"name": "Read"}},
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}
	for k, v := range extra {
		body[k] = v
	}
	return body
}

// assembled rebuilds the content blocks from the deltas.
type assembled struct {
	blocks     []map[string]any
	text       []string
	stop       string
	safeguards any
}

func assemble(t *testing.T, events []sseEvent) assembled {
	t.Helper()
	var a assembled
	if len(events) < 3 || events[0].Name != "message_start" || events[len(events)-1].Name != "message_stop" {
		t.Fatalf("stream does not start with message_start and end with message_stop: %v", events)
	}
	open := -1
	for _, e := range events {
		switch e.Name {
		case "content_block_start":
			index := int(e.Data["index"].(float64))
			if open != -1 || index != len(a.blocks) {
				t.Fatalf("block %d started while %d open (%d done)", index, open, len(a.blocks))
			}
			open = index
			a.blocks = append(a.blocks, e.Data["content_block"].(map[string]any))
			a.text = append(a.text, "")
		case "content_block_delta":
			index := int(e.Data["index"].(float64))
			if index != open {
				t.Fatalf("delta for block %d while %d open", index, open)
			}
			d := e.Data["delta"].(map[string]any)
			switch d["type"] {
			case "text_delta":
				a.text[index] += d["text"].(string)
			case "thinking_delta":
				a.text[index] += d["thinking"].(string)
			case "input_json_delta":
				a.text[index] += d["partial_json"].(string)
			case "signature_delta":
				if d["signature"] == "" {
					t.Fatal("empty signature")
				}
			default:
				t.Fatalf("unknown delta %v", d)
			}
		case "content_block_stop":
			if int(e.Data["index"].(float64)) != open {
				t.Fatalf("stop of a block that is not open: %v", e.Raw)
			}
			open = -1
		case "message_delta":
			d := e.Data["delta"].(map[string]any)
			a.stop, _ = d["stop_reason"].(string)
			a.safeguards = d["safeguard_results"]
		}
	}
	if open != -1 {
		t.Fatalf("block %d never stopped", open)
	}
	return a
}

func TestStreamFraming(t *testing.T) {
	s := start(t, Options{Main: func(r *Request) *Response {
		return &Response{
			Thinking: "Let me think about ünïcode.",
			Text:     "I will run two commands: ✓",
			ToolUses: []ToolUse{Bash("toolu_a", "printf 'a\"b'"), Use("", "Read", map[string]any{"file_path": "/tmp/x"})},
			Chunks:   4,
		}
	}})
	res := post(t, t.Context(), s.URL()+"/v1/messages", mainBody(true, nil))
	if res.StatusCode != 200 || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status %d, content type %q", res.StatusCode, res.Header.Get("Content-Type"))
	}
	events := parseSSE(t, res.Body)
	a := assemble(t, events)
	if len(a.blocks) != 4 || a.blocks[0]["type"] != "thinking" || a.blocks[1]["type"] != "text" ||
		a.blocks[2]["type"] != "tool_use" || a.blocks[3]["type"] != "tool_use" {
		t.Fatalf("blocks %v", a.blocks)
	}
	if a.text[0] != "Let me think about ünïcode." || a.text[1] != "I will run two commands: ✓" {
		t.Fatalf("text %q", a.text)
	}
	textDeltas := 0
	for _, e := range events {
		if d, ok := e.Data["delta"].(map[string]any); ok && d["type"] == "text_delta" {
			textDeltas++
		}
	}
	if textDeltas != 4 {
		t.Fatalf("text split into %d deltas, want 4", textDeltas)
	}
	var input map[string]string
	if err := json.Unmarshal([]byte(a.text[2]), &input); err != nil || input["command"] != `printf 'a"b'` {
		t.Fatalf("tool input %q: %v", a.text[2], err)
	}
	if a.blocks[2]["id"] != "toolu_a" || a.blocks[3]["id"] != "toolu_fake_1_1" || a.blocks[3]["name"] != "Read" {
		t.Fatalf("tool blocks %v", a.blocks[2:])
	}
	if a.stop != "tool_use" || a.safeguards != nil {
		t.Fatalf("stop %q, safeguard results %v without safeguards", a.stop, a.safeguards)
	}
	start := events[0].Data["message"].(map[string]any)
	if start["id"] != "msg_fake_1" || start["model"] != "claude-test" || start["role"] != "assistant" || start["stop_reason"] != nil {
		t.Fatalf("message_start %v", start)
	}
	if events[1].Name != "ping" {
		t.Fatalf("second event %q, want ping", events[1].Name)
	}
	got := s.Requests()
	if len(got) != 1 || !got[0].Main || got[0].Outcome != Completed || got[0].Status != 200 ||
		strings.Join(got[0].ToolUseIDs, ",") != "toolu_a,toolu_fake_1_1" || got[0].Headers["anthropic-version"] != "2023-06-01" {
		t.Fatalf("record %+v", got)
	}
	if strings.Contains(fmt.Sprint(got[0].Headers), "secret") {
		t.Fatal("credential header recorded")
	}
}

func TestSafeguards(t *testing.T) {
	s := start(t, Options{Main: func(r *Request) *Response {
		return &Response{ToolUses: []ToolUse{Bash("toolu_ok", "ls"), Bash("toolu_bad", "rm -rf /")}, Flagged: map[string]string{"toolu_bad": "destructive"}}
	}})
	res := post(t, t.Context(), s.URL()+"/v1/messages", mainBody(true, map[string]any{"safeguards": []any{map[string]any{"type": "dangerous_tool_use"}}}))
	a := assemble(t, parseSSE(t, res.Body))
	b, _ := json.Marshal(a.safeguards)
	want := `[{"status":{"tool_uses":{"toolu_bad":{"explanation":"destructive","outcome":"flagged","type":"evaluated"},"toolu_ok":{"outcome":"not_flagged","type":"evaluated"}},"type":"available"},"type":"dangerous_tool_use"}]`
	if string(b) != want {
		t.Fatalf("safeguard_results\n%s\nwant\n%s", b, want)
	}
	if r := s.Requests()[0]; r.Safeguards == nil {
		t.Fatal("safeguards not recorded")
	}

	reject := start(t, Options{RejectSafeguards: true})
	res = post(t, t.Context(), reject.URL()+"/v1/messages", mainBody(true, map[string]any{"safeguards": []any{}}))
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != 400 || !strings.Contains(string(body), "safeguards") || !strings.Contains(string(body), "invalid_request_error") {
		t.Fatalf("reject: %d %s", res.StatusCode, body)
	}
	res = post(t, t.Context(), reject.URL()+"/v1/messages", mainBody(true, nil))
	if res.StatusCode != 200 {
		t.Fatalf("request without safeguards: %d", res.StatusCode)
	}
}

func TestHoldAndClientGone(t *testing.T) {
	gate := NewGate()
	s := start(t, Options{Main: func(r *Request) *Response { return &Response{Text: "one two three four", Hold: gate} }})
	ctx, cancel := context.WithCancel(t.Context())
	res := post(t, ctx, s.URL()+"/v1/messages", mainBody(true, nil))
	select {
	case <-gate.Reached():
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not reach the gate")
	}
	// The first delta is already delivered while the stream is held.
	br := bufio.NewReader(res.Body)
	var seen strings.Builder
	for !strings.Contains(seen.String(), "text_delta") {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		seen.WriteString(line)
	}
	if strings.Contains(seen.String(), "message_stop") {
		t.Fatal("stream ended while held")
	}
	cancel()
	wctx, wcancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer wcancel()
	if _, err := s.Wait(wctx, func(r *Request) bool { return r.Outcome == ClientGone }); err != nil {
		t.Fatalf("client going away not recorded: %v %+v", err, s.Requests())
	}

	// A released gate lets the next stream through to the end.
	gate.Release()
	res = post(t, t.Context(), s.URL()+"/v1/messages", mainBody(true, nil))
	if a := assemble(t, parseSSE(t, res.Body)); a.text[0] != "one two three four" || a.stop != "end_turn" {
		t.Fatalf("released stream %+v", a)
	}
}

func TestCloseEndsHeldStream(t *testing.T) {
	gate := NewGate()
	s, err := Start(Options{Main: func(r *Request) *Response { return &Response{Text: "held", Hold: gate} }})
	if err != nil {
		t.Fatal(err)
	}
	res := post(t, t.Context(), s.URL()+"/v1/messages", mainBody(true, nil))
	<-gate.Reached()
	done := make(chan error, 1)
	go func() { done <- s.Close() }()
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		t.Fatal("Close blocked on a held stream")
	}
	_, _ = io.Copy(io.Discard, res.Body)
	if r := s.Requests()[0]; r.Outcome != ServerClosed {
		t.Fatalf("outcome %v", r.Outcome)
	}
}

func TestRoutingAndOtherEndpoints(t *testing.T) {
	s := start(t, Options{
		Main: func(r *Request) *Response { return Text("main") },
		Side: func(r *Request) *Response { return Text("side " + r.Model) },
	})
	// A side request without tools, not streamed.
	res := post(t, t.Context(), s.URL()+"/v1/messages", map[string]any{
		"model": "claude-small", "max_tokens": 50, "system": "Generate a title",
		"messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "hi"}}}},
	})
	var msg struct {
		Content []struct{ Type, Text string }
		Stop    string `json:"stop_reason"`
	}
	if err := json.NewDecoder(res.Body).Decode(&msg); err != nil || len(msg.Content) != 1 || msg.Content[0].Text != "side claude-small" || msg.Stop != "end_turn" {
		t.Fatalf("side answer %+v: %v", msg, err)
	}
	// A subagent: tools and the agent prompt.
	res = post(t, t.Context(), s.URL()+"/v1/messages", mainBody(false, map[string]any{"system": "You are an agent for Claude Code, Anthropic's official CLI for Claude."}))
	_ = json.NewDecoder(res.Body).Decode(&msg)
	if msg.Content[0].Text != "side claude-test" {
		t.Fatalf("subagent routed to %q", msg.Content[0].Text)
	}
	res = post(t, t.Context(), s.URL()+"/v1/messages", mainBody(false, nil))
	_ = json.NewDecoder(res.Body).Decode(&msg)
	if msg.Content[0].Text != "main" {
		t.Fatalf("main routed to %q", msg.Content[0].Text)
	}
	// The Agent SDK's claude_code preset.
	res = post(t, t.Context(), s.URL()+"/v1/messages", mainBody(false, map[string]any{"system": []any{
		map[string]any{"type": "text", "text": "x-anthropic-billing-header: cc_version=2.1.330;"},
		map[string]any{"type": "text", "text": "You are a Claude agent, built on Anthropic's Claude Agent SDK."},
	}}))
	_ = json.NewDecoder(res.Body).Decode(&msg)
	if msg.Content[0].Text != "main" {
		t.Fatalf("SDK preset routed to %q", msg.Content[0].Text)
	}
	res = post(t, t.Context(), s.URL()+"/v1/messages/count_tokens", mainBody(false, nil))
	var count struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.NewDecoder(res.Body).Decode(&count); err != nil || res.StatusCode != 200 || count.InputTokens <= 0 {
		t.Fatalf("count_tokens %d %+v: %v", res.StatusCode, count, err)
	}
	res = post(t, t.Context(), s.URL()+"/api/unknown", map[string]any{})
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != 404 || !strings.Contains(string(body), "not_found_error") {
		t.Fatalf("unknown path %d %s", res.StatusCode, body)
	}
	main := s.MainRequests()
	if len(main) != 2 || main[0].Seq != 3 || main[1].Seq != 4 || len(s.Requests()) != 6 {
		t.Fatalf("records %+v", s.Requests())
	}
}

func TestStreamError(t *testing.T) {
	s := start(t, Options{Main: func(r *Request) *Response {
		return &Response{Text: "partial answer here", StreamError: "overloaded_error", StreamErrorMessage: "Overloaded"}
	}})
	res := post(t, t.Context(), s.URL()+"/v1/messages", mainBody(true, nil))
	events := parseSSE(t, res.Body)
	last := events[len(events)-1]
	if last.Name != "error" || last.Raw != `{"error":{"message":"Overloaded","type":"overloaded_error"},"type":"error"}` {
		t.Fatalf("last event %+v", last)
	}
	if events[len(events)-2].Name != "content_block_delta" {
		t.Fatalf("error not after the first delta: %v", events)
	}
	for _, e := range events {
		if e.Name == "message_stop" || e.Name == "message_delta" {
			t.Fatalf("stream went on after the error: %v", events)
		}
	}
	if r := s.Requests()[0]; r.Outcome != Completed {
		t.Fatalf("outcome %v", r.Outcome)
	}
}

func TestRetryHeaders(t *testing.T) {
	no := false
	for _, tc := range []struct {
		res          Response
		retry, after string
	}{
		{Response{Status: 529, ErrorType: "overloaded_error"}, "true", "0"},
		{Response{Status: 429, ErrorType: "rate_limit_error", RetryAfterSeconds: 2}, "true", "2"},
		{Response{Status: 400, ErrorType: "invalid_request_error"}, "false", ""},
		{Response{Status: 500, ShouldRetry: &no}, "false", ""},
	} {
		s := start(t, Options{Main: func(r *Request) *Response { res := tc.res; return &res }})
		res := post(t, t.Context(), s.URL()+"/v1/messages", mainBody(true, nil))
		if res.StatusCode != tc.res.Status || res.Header.Get("x-should-retry") != tc.retry || res.Header.Get("retry-after") != tc.after {
			t.Errorf("%d: x-should-retry %q retry-after %q", tc.res.Status, res.Header.Get("x-should-retry"), res.Header.Get("retry-after"))
		}
	}
}

func TestErrorResponse(t *testing.T) {
	s := start(t, Options{Main: func(r *Request) *Response {
		return &Response{Status: 529, ErrorType: "overloaded_error", Error: "busy"}
	}})
	res := post(t, t.Context(), s.URL()+"/v1/messages", mainBody(true, nil))
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != 529 || string(body) != `{"error":{"message":"busy","type":"overloaded_error"},"type":"error"}`+"\n" {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
}

func TestRecordingBounds(t *testing.T) {
	s := start(t, Options{MaxRecords: 3, MaxRecordBytes: 2000})
	big := strings.Repeat("x", 600)
	for i := range 5 {
		res := post(t, t.Context(), s.URL()+"/v1/messages", map[string]any{"model": fmt.Sprint(i), "messages": []any{map[string]any{"role": "user", "content": big}}})
		_, _ = io.Copy(io.Discard, res.Body)
	}
	got := s.Requests()
	if len(got) != 3 || s.Dropped() != 2 || got[0].Seq != 3 || got[2].Seq != 5 {
		t.Fatalf("kept %d (dropped %d)", len(got), s.Dropped())
	}
	if !got[0].Stripped || got[0].Body != nil || got[0].Messages != nil || !got[1].Stripped || got[2].Stripped ||
		got[2].Body == nil || got[2].Messages[0].Content[0].Text != big || got[0].Model != "2" {
		t.Fatal("content beyond the byte budget was kept, the newest was stripped, or metadata was lost")
	}

	small := start(t, Options{MaxBodyBytes: 100})
	res := post(t, t.Context(), small.URL()+"/v1/messages", map[string]any{"pad": big})
	if res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body: %d", res.StatusCode)
	}
}

func TestRequestHelpers(t *testing.T) {
	var r Request
	r.parse([]byte(`{"model":"m","messages":[
		{"role":"user","content":"first"},
		{"role":"assistant","content":[{"type":"text","text":"ok"},{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls"}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"a.txt"}]},
			{"type":"text","text":"<system-reminder>\nThe user sent a new message while you were working:\nsecond\n\nThis is how Claude Code surfaces messages the user sends mid-turn — within the running turn.\n</system-reminder>\n"}]},
		{"role":"assistant","content":[{"type":"tool_use","id":"t2","name":"Bash","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t2","is_error":true,"content":"boom"},
			{"type":"text","text":"<task-notification>\n<task-id>b1</task-id>\n</task-notification>"}]},
		{"role":"system","content":"The user sent a new message while you were working:\nthird\n\nThis is how Claude Code surfaces messages the user sends mid-turn — within the running turn."}]}`))
	if r.Assistants() != 2 || r.Last().Role != "system" {
		t.Fatalf("parsed %+v", r.Messages)
	}
	if b, ok := r.ToolResult("t1"); !ok || b.ResultText() != "a.txt" || b.IsError {
		t.Fatalf("t1 %+v", b)
	}
	if b, ok := r.ToolResult("t2"); !ok || b.ResultText() != "boom" || !b.IsError {
		t.Fatalf("t2 %+v", b)
	}
	if u, ok := r.ToolUse("t1"); !ok || string(u.Input) != `{"command":"ls"}` {
		t.Fatalf("tool use %+v", u)
	}
	if m := r.MidTurnMessages(); len(m) != 2 || m[0] != "second" || m[1] != "third" {
		t.Fatalf("mid-turn messages %q", m)
	}
	if n := r.Notifications(); len(n) != 1 || !strings.Contains(n[0], "<task-id>b1</task-id>") {
		t.Fatalf("notifications %q", n)
	}
	if !r.Contains("first") || r.Count("Bash") != 0 || r.Count("a.txt") != 1 {
		t.Fatal("Contains/Count")
	}
}

func TestSplit(t *testing.T) {
	for _, tc := range []struct {
		in   string
		n    int
		want []string
	}{
		{"", 3, []string{""}},
		{"ab", 3, []string{"a", "b"}},
		{"abcdef", 3, []string{"ab", "cd", "ef"}},
		{"abcdefg", 3, []string{"abc", "def", "g"}},
		{"äöü✓", 2, []string{"äö", "ü✓"}},
	} {
		if got := split(tc.in, tc.n); strings.Join(got, "|") != strings.Join(tc.want, "|") {
			t.Errorf("split(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
}
