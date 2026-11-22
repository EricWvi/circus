//go:build e2e && linux

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	pb "github.com/EricWvi/circus/gen/go/circus/v1"
)

// The Web UI's transcript reducer is tested against what the Go fake
// worker publishes (web/src/lib/session-events/fake-worker.fixture.json,
// recorded by internal/testutil/fakeworker). compareWithFakeWorker checks
// that the real worker publishes the same durable event kinds in the same
// order (and so the same turn boundaries) for a scenario both have, with
// the payload keys the reducer reads present, of the same JSON type, and
// equal where the reducer branches on the value.

var fakeWorkerFixture struct {
	once sync.Once
	err  error
	data map[string]struct {
		Events []struct {
			Kind      string          `json:"kind"`
			Ephemeral bool            `json:"ephemeral"`
			Payload   json.RawMessage `json:"payload"`
		} `json:"events"`
	}
}

// deviation is a known difference in order from the fake worker: the
// durable events of kinds fake, in that order, come from the real worker
// in the order real (the same kinds, permuted).
type deviation struct{ fake, real []string }

// approvalBeforeDenial: the real worker publishes the approval card as soon
// as the permission denial arrives, before Claude Code's tool_result that
// carries the denial to the model; the fake worker publishes it after. The
// Web shows both the same way (the denial marks the tool call, so its error
// result is never shown).
var approvalBeforeDenial = deviation{
	fake: []string{"KIND_PERMISSION_DENIED", "KIND_USER", "KIND_APPROVAL_PENDING"},
	real: []string{"KIND_PERMISSION_DENIED", "KIND_APPROVAL_PENDING", "KIND_USER"},
}

// compareWithFakeWorker compares a session's durable events, from its first,
// with the fake worker's recording of scenario, after applying the known
// deviations.
func compareWithFakeWorker(t *testing.T, scenario string, got []*pb.SessionEvent, deviations ...deviation) {
	t.Helper()
	fakeWorkerFixture.once.Do(func() {
		var raw []byte
		raw, fakeWorkerFixture.err = os.ReadFile(filepath.Join(root, "web/src/lib/session-events/fake-worker.fixture.json"))
		if fakeWorkerFixture.err == nil {
			fakeWorkerFixture.err = json.Unmarshal(raw, &fakeWorkerFixture.data)
		}
	})
	must(t, fakeWorkerFixture.err)
	rec, ok := fakeWorkerFixture.data[scenario]
	if !ok {
		t.Fatalf("no fake worker scenario %q", scenario)
	}
	var wantKinds, gotKinds []string
	var wantPayloads []json.RawMessage
	for _, e := range rec.Events {
		if !e.Ephemeral {
			wantKinds = append(wantKinds, e.Kind)
			wantPayloads = append(wantPayloads, e.Payload)
		}
	}
	for _, d := range deviations {
		wantKinds, wantPayloads = permute(wantKinds, wantPayloads, d)
	}
	var durable []*pb.SessionEvent
	for _, e := range got {
		if !e.GetEphemeral() {
			durable = append(durable, e)
			gotKinds = append(gotKinds, e.GetKind().String())
		}
	}
	if strings.Join(gotKinds, " ") != strings.Join(wantKinds, " ") {
		t.Fatalf("scenario %s: event kinds differ from the fake worker's\nreal: %v\nfake: %v", scenario, gotKinds, wantKinds)
	}
	for i, e := range durable {
		var want, real map[string]any
		must(t, json.Unmarshal(wantPayloads[i], &want))
		must(t, json.Unmarshal([]byte(e.GetPayloadJson()), &real))
		ws, rs := webShape(e.GetKind(), want), webShape(e.GetKind(), real)
		if d := diffShapes(ws, rs); d != "" {
			t.Errorf("scenario %s: event %d (%s) differs from the fake worker's in keys the Web reads:\n%s\nreal payload: %.2000s", scenario, i+1, e.GetKind(), d, e.GetPayloadJson())
		}
	}
}

// permute applies a deviation to every occurrence of its fake sequence.
func permute(kinds []string, payloads []json.RawMessage, d deviation) ([]string, []json.RawMessage) {
	kinds = append([]string(nil), kinds...)
	payloads = append([]json.RawMessage(nil), payloads...)
	for i := 0; i+len(d.fake) <= len(kinds); i++ {
		if strings.Join(kinds[i:i+len(d.fake)], " ") != strings.Join(d.fake, " ") {
			continue
		}
		used := make([]bool, len(d.fake))
		moved := make([]json.RawMessage, 0, len(d.fake))
		for _, k := range d.real {
			for j, f := range d.fake {
				if !used[j] && f == k {
					used[j] = true
					moved = append(moved, payloads[i+j])
					break
				}
			}
		}
		copy(kinds[i:], d.real)
		copy(payloads[i:], moved)
		i += len(d.fake) - 1
	}
	return kinds, payloads
}

// checkStream checks a watch's streamed assistant messages against their
// durable events the way the Web reducer joins them: a stream_event's
// message_start names the API message (the durable message.id), its
// content_block_start indexes count 0, 1, ... and the n-th durable
// assistant event of that message (Claude Code sends one per block)
// carries block n, of the same type, which replaces the streamed copy
// "<message id>:<n>". With complete, every durable block was streamed and
// every streamed block became durable (no turn was cut off).
func checkStream(t *testing.T, w *sessionWatch, complete bool) {
	t.Helper()
	streamedTypes := map[string][]string{}
	durable := map[string]int{}
	var order []string
	current := ""
	for _, e := range w.all {
		if e.GetKind() != pb.SessionEvent_KIND_ASSISTANT {
			continue
		}
		var p struct {
			Type   string
			Parent *string `json:"parent_tool_use_id"`
			Event  struct {
				Type    string
				Index   *int
				Message struct{ ID string }
				Block   struct{ Type string } `json:"content_block"`
			}
			Message struct {
				ID      string
				Content []struct{ Type string }
			}
		}
		must(t, json.Unmarshal([]byte(e.GetPayloadJson()), &p))
		if p.Parent != nil && *p.Parent != "" {
			continue
		}
		if e.GetEphemeral() {
			if p.Type != "stream_event" {
				continue
			}
			switch p.Event.Type {
			case "message_start":
				current = p.Event.Message.ID
				if current == "" {
					t.Fatalf("message_start without an id: %s", e.GetPayloadJson())
				}
				order = append(order, current)
			case "content_block_start":
				if current == "" || p.Event.Index == nil || *p.Event.Index != len(streamedTypes[current]) {
					t.Fatalf("content_block_start out of order for message %q (%d started): %s", current, len(streamedTypes[current]), e.GetPayloadJson())
				}
				streamedTypes[current] = append(streamedTypes[current], p.Event.Block.Type)
			}
			continue
		}
		id := p.Message.ID
		for _, b := range p.Message.Content {
			n := durable[id]
			durable[id]++
			streamed := streamedTypes[id]
			if n >= len(streamed) {
				if complete {
					t.Fatalf("durable block %d of message %q was not streamed: %s", n, id, e.GetPayloadJson())
				}
				continue
			}
			if streamed[n] != b.Type {
				t.Fatalf("durable block %d of message %q is %s, streamed as %s", n, id, b.Type, streamed[n])
			}
		}
	}
	if !complete {
		return
	}
	if len(order) == 0 {
		t.Fatal("no streamed assistant message")
	}
	for _, id := range order {
		if durable[id] != len(streamedTypes[id]) {
			t.Fatalf("message %q: %d blocks streamed, %d durable", id, len(streamedTypes[id]), durable[id])
		}
	}
}

func diffShapes(want, got map[string]string) string {
	keys := map[string]bool{}
	for k := range want {
		keys[k] = true
	}
	for k := range got {
		keys[k] = true
	}
	var lines []string
	for k := range keys {
		if want[k] != got[k] {
			lines = append(lines, fmt.Sprintf("  %s: fake %q, real %q", k, want[k], got[k]))
		}
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// jsonType names a JSON value's type ("absent" for a missing key).
func jsonType(v any, present bool) string {
	if !present {
		return "absent"
	}
	switch v.(type) {
	case nil:
		return "null"
	case string:
		return "string"
	case bool:
		return "bool"
	case float64:
		return "number"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return "unknown"
}

// webShape maps the payload keys web/src/lib/session-events/transcript.ts
// reads to their JSON type, or to their value where it branches on it.
func webShape(kind pb.SessionEvent_Kind, p map[string]any) map[string]string {
	s := map[string]string{}
	typ := func(path string, m map[string]any, key string) {
		v, ok := m[key]
		s[path] = jsonType(v, ok)
	}
	val := func(path string, m map[string]any, key string) {
		v, ok := m[key]
		if !ok {
			s[path] = "absent"
			return
		}
		s[path] = fmt.Sprintf("%v", v)
	}
	obj := func(v any) map[string]any {
		m, _ := v.(map[string]any)
		return m
	}
	// The reducer reads a content's text whether it is a string or blocks.
	text := func(v any) string {
		switch c := v.(type) {
		case string:
			return "text"
		case []any:
			for _, b := range c {
				if m := obj(b); m == nil || m["type"] != "text" {
					return "blocks"
				}
			}
			return "text"
		}
		return jsonType(v, v != nil)
	}
	switch kind {
	case pb.SessionEvent_KIND_INPUT_ACCEPTED:
		typ("input_id", p, "input_id")
		typ("client_msg_id", p, "client_msg_id")
		typ("text", p, "text")
	case pb.SessionEvent_KIND_TURN_STARTED, pb.SessionEvent_KIND_TURN_STOPPED, pb.SessionEvent_KIND_TURN_INTERRUPTED:
		val("turn_no", p, "turn_no")
		val("origin", p, "origin")
		val("outcome", p, "outcome")
	case pb.SessionEvent_KIND_USER:
		typ("uuid", p, "uuid")
		val("circus_note", p, "circus_note")
		val("isSynthetic", p, "isSynthetic")
		typ("parent_tool_use_id", p, "parent_tool_use_id")
		msg := obj(p["message"])
		s["message.content"] = text(msg["content"])
		if blocks, ok := msg["content"].([]any); ok {
			for i, b := range blocks {
				m := obj(b)
				prefix := fmt.Sprintf("message.content[%d].", i)
				val(prefix+"type", m, "type")
				if m["type"] == "tool_result" {
					typ(prefix+"tool_use_id", m, "tool_use_id")
					s[prefix+"content"] = text(m["content"])
					val(prefix+"is_error", m, "is_error")
				}
			}
		}
	case pb.SessionEvent_KIND_ASSISTANT:
		typ("parent_tool_use_id", p, "parent_tool_use_id")
		val("aborted", p, "aborted")
		msg := obj(p["message"])
		typ("message.id", msg, "id")
		blocks, _ := msg["content"].([]any)
		s["message.content.length"] = fmt.Sprint(len(blocks))
		for i, b := range blocks {
			m := obj(b)
			prefix := fmt.Sprintf("message.content[%d].", i)
			val(prefix+"type", m, "type")
			switch m["type"] {
			case "text":
				typ(prefix+"text", m, "text")
			case "thinking":
				typ(prefix+"thinking", m, "thinking")
			case "tool_use":
				typ(prefix+"id", m, "id")
				val(prefix+"name", m, "name")
				typ(prefix+"input", m, "input")
			}
		}
	case pb.SessionEvent_KIND_PERMISSION_DENIED:
		val("subtype", p, "subtype")
		typ("tool_use_id", p, "tool_use_id")
	case pb.SessionEvent_KIND_APPROVAL_PENDING:
		typ("approval_id", p, "approval_id")
		typ("tool_use_id", p, "tool_use_id")
		typ("reason", p, "reason")
		typ("display", p, "display")
		typ("tool", p, "tool")
	case pb.SessionEvent_KIND_APPROVAL_RESOLVED:
		typ("approval_id", p, "approval_id")
		val("state", p, "state")
	case pb.SessionEvent_KIND_SESSION_STATE:
		val("state", p, "state")
	case pb.SessionEvent_KIND_ERROR:
		typ("message", p, "message")
	case pb.SessionEvent_KIND_RESULT:
		// The reducer reads nothing of a result; the turn outcome comes from these.
		val("subtype", p, "subtype")
		val("is_error", p, "is_error")
		val("terminal_reason", p, "terminal_reason")
		// Claude Code 2.1.330 says "completed" where the fake worker's
		// recorded SDK messages (older) leave the key out.
		if s["terminal_reason"] == "completed" {
			s["terminal_reason"] = "absent"
		}
	}
	return s
}
