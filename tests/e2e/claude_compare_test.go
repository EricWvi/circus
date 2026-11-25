//go:build e2e && linux

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/testutil/fakeworker"
)

// The Web UI's transcript reducer is tested against what the Go fake
// worker publishes (web/src/lib/session-events/fake-worker.fixture.json,
// recorded by internal/testutil/fakeworker). compareWithFakeWorker checks
// that the real worker publishes the same events as the fake for a
// scenario both have: the same durable kinds and streamed SDK events in the
// same order, each with the same key paths and JSON types as the fake's,
// and equal where the value is enumeration-like (see compareEvents).

// fixtureEvent is one event as the fixture records it.
type fixtureEvent struct {
	Seq       uint64 `json:"seq"`
	Kind      string `json:"kind"`
	Ephemeral bool   `json:"ephemeral"`
	Truncated bool   `json:"truncated"`
	Payload   any    `json:"payload"`
}

var fakeWorkerFixture struct {
	once sync.Once
	err  error
	data map[string]struct {
		Events []fixtureEvent `json:"events"`
	}
}

// The comparison's scope is the session's events from the first durable
// event the watch received to the scenario's last durable event (the last
// one received when the comparison is asked for), plus the ephemeral
// events after it up to a fence: a durable event that every event the
// scenario published comes before on the stream. compareBeforeNext uses
// the session's next durable event (the test goes on with the session);
// compareWithFakeWorker stops the session (the test is done with it) and
// uses its SESSION_STATE. A session already stopped needs no fence: its
// worker's lease ended before its last durable event. The fixture's scope
// is the same: its recorder reads until the session has settled.

// compareWithFakeWorker compares the scenario's events with the fake
// worker's recording of scenario now, stopping the session as the fence
// unless the watch already received a durable event after the scenario.
func compareWithFakeWorker(t *testing.T, scenario string, w *sessionWatch) {
	t.Helper()
	start, last := w.scenarioBounds()
	end := nextDurable(w.all, last)
	if end < 0 {
		end = w.fence(last)
	}
	compareScope(t, scenario, w.all[start:end])
}

// compareBeforeNext compares the scenario's events with the fake worker's
// recording of scenario once the watch receives the session's next
// durable event, which the test must make happen.
func compareBeforeNext(t *testing.T, scenario string, w *sessionWatch) {
	t.Helper()
	start, last := w.scenarioBounds()
	if end := nextDurable(w.all, last); end >= 0 {
		compareScope(t, scenario, w.all[start:end])
		return
	}
	done := false
	w.atNextDurable = append(w.atNextDurable, func(end int) {
		done = true
		compareScope(t, scenario, w.all[start:end])
	})
	t.Cleanup(func() {
		if !done && !t.Failed() {
			t.Errorf("scenario %s was not compared: no durable event followed it", scenario)
		}
	})
}

func compareScope(t *testing.T, scenario string, events []*pb.SessionEvent) {
	t.Helper()
	got := fixtureEvents(t, events)
	recordReal(t, scenario, got)
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
	diffs := compareEvents(got, rec.Events)
	const most = 40
	for i, d := range diffs {
		if i == most {
			t.Errorf("scenario %s: %d more differences", scenario, len(diffs)-most)
			break
		}
		t.Errorf("scenario %s: %s", scenario, d)
	}
}

// scenarioBounds are the indexes in w.all of the first durable event and
// of the last one received so far.
func (w *sessionWatch) scenarioBounds() (start, last int) {
	w.t.Helper()
	if len(w.durable) == 0 {
		w.t.Fatal("no durable event to compare")
	}
	return w.index(w.durable[0]), w.index(w.durable[len(w.durable)-1])
}

func (w *sessionWatch) index(e *pb.SessionEvent) int {
	for i, x := range w.all {
		if x == e {
			return i
		}
	}
	w.t.Fatal("a durable event of the watch is not among all its events")
	return -1
}

// nextDurable is the index of the first durable event after index last, or
// -1 when there is none.
func nextDurable(all []*pb.SessionEvent, last int) int {
	for i := last + 1; i < len(all); i++ {
		if !all[i].GetEphemeral() {
			return i
		}
	}
	return -1
}

// fence stops the watch's session and returns the index of its
// SESSION_STATE; for a session stopped already, the index after last.
func (w *sessionWatch) fence(last int) int {
	w.t.Helper()
	s := w.session
	got, err := s.api.GetSession(s.ctx, connect.NewRequest(&pb.GetSessionRequest{SessionId: s.id}))
	must(w.t, err)
	if got.Msg.GetState() == pb.Session_STATE_STOPPED {
		return last + 1
	}
	_, err = s.api.StopSession(s.ctx, connect.NewRequest(&pb.StopSessionRequest{SessionId: s.id}))
	must(w.t, err)
	var state *pb.SessionEvent
	w.within(eventWait, func() {
		state = w.until(func(e *pb.SessionEvent) bool { return !e.GetEphemeral() })
	})
	if state.GetKind() != pb.SessionEvent_KIND_SESSION_STATE {
		w.t.Fatalf("expected the stop's SESSION_STATE, got %v", state)
	}
	return len(w.all) - 1
}

// fixtureEvents converts events to the fixture's format, leaving out
// keepalives.
func fixtureEvents(t *testing.T, events []*pb.SessionEvent) []fixtureEvent {
	t.Helper()
	var out []fixtureEvent
	for _, e := range events {
		if e.GetKind() == pb.SessionEvent_KIND_UNSPECIFIED && e.GetPayloadJson() == "" {
			continue
		}
		var payload any
		if raw := e.GetPayloadJson(); raw != "" {
			must(t, json.Unmarshal([]byte(raw), &payload))
		}
		out = append(out, fixtureEvent{
			Seq: e.GetSeq(), Kind: e.GetKind().String(), Ephemeral: e.GetEphemeral(), Truncated: e.GetTruncated(), Payload: payload,
		})
	}
	return out
}

// recordEnv names a file that collects, under one key per scenario, the
// real worker's events each compareWithFakeWorker call compared (the
// fixture's format, payloads as received, ephemeral events included). It
// is for re-capturing the real shapes after an SDK or Claude Code upgrade.
// The test process truncates the file at its first write, then adds each
// scenario it compares. A relative path is taken from the repository root.
// Unset, nothing is written.
const recordEnv = "CIRCUS_E2E_RECORD_REAL"

var record struct {
	sync.Mutex
	// started is set once this process has truncated the file.
	started bool
}

func recordReal(t *testing.T, scenario string, events []fixtureEvent) {
	t.Helper()
	path := os.Getenv(recordEnv)
	if path == "" {
		return
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	record.Lock()
	defer record.Unlock()
	// A process starts the file afresh, so no scenario of an earlier run
	// (another Claude Code, a renamed scenario) stays in it unnoticed.
	data := map[string]json.RawMessage{}
	if record.started {
		raw, err := os.ReadFile(path)
		must(t, err)
		must(t, json.Unmarshal(raw, &data))
	}
	record.started = true
	encode := func(v any) []byte {
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", " ")
		must(t, enc.Encode(v))
		return b.Bytes()
	}
	data[scenario] = encode(struct {
		Events []fixtureEvent `json:"events"`
	}{events})
	must(t, os.WriteFile(path, encode(data), 0o644))
}

// enumFields hold enumeration-like values: compared by value. Other
// strings and numbers (ids, uuids, paths, model ids, texts the scripts
// write, counts, timings) are compared by type only.
var enumFields = map[string]bool{
	"type": true, "subtype": true, "kind": true, "state": true, "origin": true, "outcome": true,
	"terminal_reason": true, "stop_reason": true, "is_error": true, "isSynthetic": true, "circus_note": true,
	"non_execution_kind": true, "decision_reason_type": true, "role": true, "aborted": true, "index": true,
	"thinking_display": true, "priority": true,
	// Tool names.
	"name": true, "tool": true, "tool_name": true,
}

// opaqueFields are compared by presence and JSON type only: metric
// objects the fake worker fills with placeholders (usage and modelUsage
// are type markers on both sides already), and tool inputs (the fake
// model's scripts and the fake worker's commands differ).
var opaqueFields = map[string]bool{
	"usage": true, "modelUsage": true, "model_usage_since_last_result": true, "subagent_stats": true,
	"safeguard_results": true, "input": true, "tool_input": true,
}

// compareEvents compares the real worker's events with the fake worker's
// recording of the same scenario and describes each difference.
//
//   - The sequence of durable kinds and ephemeral events must be equal.
//     Streamed SDK events are named by their event, delta and block type;
//     a run of content_block_delta events of one delta type counts as one
//     (the fake model and the fake worker chunk texts differently).
//     KIND_QUEUE and ephemeral KIND_LEDGER events are left out of the real
//     side: the control plane publishes them on its own timing, while a
//     session waits for a worker and as its ledger snapshot is prepared and
//     collected (the fixture records none, its stack has the worker
//     attached first and no snapshot).
//   - Each event's payload must have the same key paths, with the same
//     JSON types, as the fake's matching event, its n-th event of the same
//     kind (arrays element by element); values are equal for enumFields
//     and for the fields of a permission_decision; opaqueFields only need
//     the same type. The fields the fixture keeps as type markers
//     (fakeworker.UnstableFields) are masked on the real side the same way.
//   - The fixed texts the Web shows are equal (compareShownTexts).
//   - On each side, the ids the Web reducer joins on point where they
//     should (checkLinks).
func compareEvents(real, fake []fixtureEvent) []string {
	var kept []fixtureEvent
	for _, e := range real {
		if e.Ephemeral && (e.Kind == pb.SessionEvent_KIND_QUEUE.String() || e.Kind == pb.SessionEvent_KIND_LEDGER.String()) {
			continue
		}
		e.Payload = fakeworker.MaskUnstable(clone(e.Payload))
		kept = append(kept, e)
	}
	rg, fg := groupEvents(kept), groupEvents(fake)
	var diffs []string
	for _, d := range checkLinks(kept) {
		diffs = append(diffs, "real: "+d)
	}
	for _, d := range checkLinks(fake) {
		diffs = append(diffs, "fake: "+d)
	}
	rt, ft := approvalTerms(kept), approvalTerms(fake)
	if !equalLabels(rg, fg) {
		diffs = append(diffs, fmt.Sprintf("event sequence differs from the fake worker's\n%s", sideBySide(rg, fg)))
	}
	// The n-th event of a kind is compared with the fake's n-th of that
	// kind, so the payloads are compared even when the order differs.
	fakeByLabel := map[string][]eventGroup{}
	for _, g := range fg {
		fakeByLabel[g.label] = append(fakeByLabel[g.label], g)
	}
	seen := map[string]int{}
	for i, g := range rg {
		n := seen[g.label]
		seen[g.label]++
		if n >= len(fakeByLabel[g.label]) {
			continue
		}
		fakeGroup := fakeByLabel[g.label][n]
		for j, r := range g.events {
			f := fakeGroup.events[min(j, len(fakeGroup.events)-1)]
			where := fmt.Sprintf("event %d (%s)", i+1, g.label)
			if r.Truncated != f.Truncated {
				diffs = append(diffs, fmt.Sprintf("%s: truncated real %v, fake %v", where, r.Truncated, f.Truncated))
			}
			for _, d := range compareValue("", "", "", r.Payload, f.Payload) {
				diffs = append(diffs, where+": "+d)
			}
			for _, d := range compareShownTexts(r.Payload, f.Payload, rt, ft) {
				diffs = append(diffs, where+": "+d)
			}
		}
	}
	return diffs
}

type eventGroup struct {
	label  string
	events []fixtureEvent
}

func groupEvents(events []fixtureEvent) []eventGroup {
	var out []eventGroup
	for _, e := range events {
		label := eventLabel(e)
		if n := len(out); n > 0 && out[n-1].label == label && strings.Contains(label, "content_block_delta") {
			out[n-1].events = append(out[n-1].events, e)
			continue
		}
		out = append(out, eventGroup{label: label, events: []fixtureEvent{e}})
	}
	return out
}

func eventLabel(e fixtureEvent) string {
	label := strings.TrimPrefix(e.Kind, "KIND_")
	p, _ := e.Payload.(map[string]any)
	if !e.Ephemeral {
		return label
	}
	if p["type"] != "stream_event" {
		return label + " (ephemeral)"
	}
	ev, _ := p["event"].(map[string]any)
	label = fmt.Sprintf("stream_event %v", ev["type"])
	for _, key := range []string{"delta", "content_block"} {
		if sub, ok := ev[key].(map[string]any); ok && sub["type"] != nil {
			label += fmt.Sprintf(" %v", sub["type"])
		}
	}
	return label
}

func equalLabels(a, b []eventGroup) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].label != b[i].label {
			return false
		}
	}
	return true
}

func sideBySide(real, fake []eventGroup) string {
	var b strings.Builder
	fmt.Fprintf(&b, "  %-3s %-52s %s\n", "#", "real", "fake")
	for i := range max(len(real), len(fake)) {
		var r, f string
		if i < len(real) {
			r = real[i].label
		}
		if i < len(fake) {
			f = fake[i].label
		}
		mark := " "
		if r != f {
			mark = "*"
		}
		fmt.Fprintf(&b, "%s %-3d %-52s %s\n", mark, i+1, r, f)
	}
	return b.String()
}

// compareValue compares the real value r with the fake value f at path;
// key is the value's own key and parent its object's.
func compareValue(path, parent, key string, r, f any) []string {
	rt, ft := jsonType(r), jsonType(f)
	if rt != ft {
		return []string{fmt.Sprintf("%s: real %s, fake %s", pathOrRoot(path), rt, ft)}
	}
	if opaqueFields[key] {
		return nil
	}
	switch r := r.(type) {
	case map[string]any:
		f := f.(map[string]any)
		keys := map[string]bool{}
		for k := range r {
			keys[k] = true
		}
		for k := range f {
			keys[k] = true
		}
		sorted := make([]string, 0, len(keys))
		for k := range keys {
			sorted = append(sorted, k)
		}
		sort.Strings(sorted)
		var diffs []string
		for _, k := range sorted {
			rv, rok := r[k]
			fv, fok := f[k]
			child := joinPath(path, k)
			switch {
			case !fok:
				diffs = append(diffs, fmt.Sprintf("%s: only in the real event (%s)", child, jsonType(rv)))
			case !rok:
				diffs = append(diffs, fmt.Sprintf("%s: only in the fake event (%s)", child, jsonType(fv)))
			default:
				diffs = append(diffs, compareValue(child, key, k, rv, fv)...)
			}
		}
		return diffs
	case []any:
		f := f.([]any)
		if len(r) != len(f) {
			return []string{fmt.Sprintf("%s: real has %d elements, fake %d", pathOrRoot(path), len(r), len(f))}
		}
		var diffs []string
		for i := range r {
			diffs = append(diffs, compareValue(fmt.Sprintf("%s[%d]", path, i), parent, key, r[i], f[i])...)
		}
		return diffs
	}
	if (enumFields[key] || parent == "permission_decision") && r != f {
		return []string{fmt.Sprintf("%s: real %s, fake %s", pathOrRoot(path), compact(r), compact(f))}
	}
	return nil
}

func joinPath(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

func pathOrRoot(path string) string {
	if path == "" {
		return "payload"
	}
	return path
}

func compact(v any) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return strings.TrimSuffix(b.String(), "\n")
}

// jsonType names a decoded JSON value's type.
func jsonType(v any) string {
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
	return fmt.Sprintf("%T", v)
}

func clone(v any) any {
	switch v := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, child := range v {
			out[k] = clone(child)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, child := range v {
			out[i] = clone(child)
		}
		return out
	}
	return v
}

// approvalTerms are the run-specific strings of a scenario's approvals
// (device, command display, reason), each with the placeholder that stands
// for it in a text compared by value. The command comes first: it can
// contain the others.
func approvalTerms(events []fixtureEvent) [][2]string {
	var terms [][2]string
	for _, e := range events {
		p, _ := e.Payload.(map[string]any)
		if e.Kind != pb.SessionEvent_KIND_APPROVAL_PENDING.String() || p == nil {
			continue
		}
		for _, f := range [][2]string{{"display", "<display>"}, {"reason", "<reason>"}} {
			if v, _ := p[f[0]].(string); v != "" {
				terms = append(terms, [2]string{v, f[1]})
			}
		}
		if v, _ := p["device"].(string); v != "" {
			terms = append(terms, [2]string{`"` + v + `"`, `"<device>"`})
		}
	}
	return terms
}

// compareShownTexts compares by value the fixed texts the Web shows: the
// content of a note the worker wrote for the model (a USER event with a
// circus_note other than user_interrupt, which the Web hides), and the
// content of a tool_result for a call that did not run to completion (its
// tool_result_meta has a non_execution_kind). A scenario's approval
// device, command and reason are replaced by placeholders first.
func compareShownTexts(r, f any, rTerms, fTerms [][2]string) []string {
	rp, _ := r.(map[string]any)
	fp, _ := f.(map[string]any)
	if rp == nil || fp == nil || rp["type"] != "user" {
		return nil
	}
	norm := func(v any, terms [][2]string) string {
		text := compact(v)
		for _, term := range terms {
			text = strings.ReplaceAll(text, strings.Trim(compact(term[0]), `"`), strings.Trim(compact(term[1]), `"`))
		}
		return text
	}
	content := func(p map[string]any) any {
		m, _ := p["message"].(map[string]any)
		return m["content"]
	}
	var diffs []string
	if note, _ := rp["circus_note"].(string); note != "" && note != "user_interrupt" {
		if rv, fv := norm(content(rp), rTerms), norm(content(fp), fTerms); rv != fv {
			diffs = append(diffs, fmt.Sprintf("%s note text: real %s, fake %s", note, rv, fv))
		}
	}
	if notRun(rp) || notRun(fp) {
		rb, _ := content(rp).([]any)
		fb, _ := content(fp).([]any)
		for i := range min(len(rb), len(fb)) {
			rv, _ := rb[i].(map[string]any)
			fv, _ := fb[i].(map[string]any)
			if rv["type"] != "tool_result" {
				continue
			}
			if rt, ft := norm(rv["content"], rTerms), norm(fv["content"], fTerms); rt != ft {
				diffs = append(diffs, fmt.Sprintf("message.content[%d].content: real %s, fake %s", i, rt, ft))
			}
		}
	}
	return diffs
}

// notRun reports whether a USER event's tool_result_meta says a call did
// not run to completion.
func notRun(p map[string]any) bool {
	meta, _ := p["tool_result_meta"].([]any)
	for _, m := range meta {
		if m, _ := m.(map[string]any); m["non_execution_kind"] != nil {
			return true
		}
	}
	return false
}

// checkLinks checks, within one side's events, the ids the Web reducer
// joins on: a permission denial, an approval request and a tool_result
// (and its tool_result_meta) name a tool_use streamed or recorded before
// them; a stream event's api_message_id is the id of its message_start's
// message, and so are a durable assistant message's message.id and an
// abandoned_blocks.api_message_id.
func checkLinks(events []fixtureEvent) []string {
	var diffs []string
	toolUses := map[string]bool{}
	current := ""
	str := func(m map[string]any, key string) string { v, _ := m[key].(string); return v }
	obj := func(m map[string]any, key string) map[string]any { v, _ := m[key].(map[string]any); return v }
	for i, e := range events {
		p, _ := e.Payload.(map[string]any)
		if p == nil {
			continue
		}
		where := fmt.Sprintf("event %d (%s)", i+1, eventLabel(e))
		needTool := func(field, id string) {
			if !toolUses[id] {
				diffs = append(diffs, fmt.Sprintf("%s: %s %q names no earlier tool_use", where, field, id))
			}
		}
		switch e.Kind {
		case pb.SessionEvent_KIND_PERMISSION_DENIED.String(), pb.SessionEvent_KIND_APPROVAL_PENDING.String():
			needTool("tool_use_id", str(p, "tool_use_id"))
		case pb.SessionEvent_KIND_ASSISTANT.String():
			if p["type"] == "stream_event" {
				ev := obj(p, "event")
				if ev["type"] == "message_start" {
					current = str(obj(ev, "message"), "id")
				}
				if id := str(p, "api_message_id"); id != current {
					diffs = append(diffs, fmt.Sprintf("%s: api_message_id %q, streaming message %q", where, id, current))
				}
				if ab := obj(p, "abandoned_blocks"); ab != nil && str(ab, "api_message_id") != current {
					diffs = append(diffs, fmt.Sprintf("%s: abandoned_blocks.api_message_id %q, streaming message %q", where, str(ab, "api_message_id"), current))
				}
				if block := obj(ev, "content_block"); block["type"] == "tool_use" {
					toolUses[str(block, "id")] = true
				}
				continue
			}
			msg := obj(p, "message")
			if id := str(msg, "id"); id != current {
				diffs = append(diffs, fmt.Sprintf("%s: message.id %q, streaming message %q", where, id, current))
			}
			blocks, _ := msg["content"].([]any)
			for _, b := range blocks {
				if b, _ := b.(map[string]any); b["type"] == "tool_use" {
					toolUses[str(b, "id")] = true
				}
			}
		case pb.SessionEvent_KIND_USER.String():
			blocks, _ := obj(p, "message")["content"].([]any)
			for _, b := range blocks {
				if b, _ := b.(map[string]any); b["type"] == "tool_result" {
					needTool("tool_result.tool_use_id", str(b, "tool_use_id"))
				}
			}
			meta, _ := p["tool_result_meta"].([]any)
			for _, m := range meta {
				if m, _ := m.(map[string]any); m != nil {
					needTool("tool_result_meta.id", str(m, "id"))
				}
			}
		}
	}
	return diffs
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
