//go:build e2e && linux

package e2e

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/testutil/fakemodel"
)

// askInput is the AskUserQuestion input the model sends: two questions,
// the second multiSelect.
var askInput = map[string]any{"questions": []any{
	map[string]any{"question": "Which color should the button be?", "header": "Color", "multiSelect": false, "options": []any{
		map[string]any{"label": "Red", "description": "A warm color"},
		map[string]any{"label": "Blue", "description": "A cool color"},
	}},
	map[string]any{"question": "Which features do you want?", "header": "Features", "multiSelect": true, "options": []any{
		map[string]any{"label": "Dark mode", "description": "A dark theme"},
		map[string]any{"label": "Search", "description": "Full-text search"},
		map[string]any{"label": "Export", "description": "CSV export"},
	}},
}}

const askAnswerText = `The user answered your questions: "Which color should the button be?"="Blue", ` +
	`"Which features do you want?"="Dark mode, Export, and \"keyboard\" shortcuts". Continue with these answers in mind.`

// TestClaudeAskUserQuestion: the model asks two questions; the worker
// turns the call into a question approval and tells the model to end its
// turn; the user's answers start the next turn, whose model request
// carries them. A dismissed question writes no input. Plan mode's tools
// are not offered.
func TestClaudeAskUserQuestion(t *testing.T) {
	var log scriptLog
	main := func(r *fakemodel.Request) *fakemodel.Response {
		if !r.HasTool("AskUserQuestion") || r.HasTool("EnterPlanMode") || r.HasTool("ExitPlanMode") {
			return log.fail("tools offered: %v", r.Tools)
		}
		switch {
		case r.Contains("still ready"):
			if r.Contains("The user answered") {
				return log.fail("a dismissed question was answered: %.2000s", r.Text())
			}
			return fakemodel.Text("Still ready.")
		case r.Contains("ask me") || r.Contains("ask and dismiss"):
		default:
			return log.fail("unexpected request: %.300s", r.Last().Text())
		}
		switch r.Assistants() {
		case 0:
			return fakemodel.Tools(fakemodel.Use("toolu_ask", "AskUserQuestion", askInput))
		case 1:
			if b, ok := r.ToolResult("toolu_ask"); !ok || !b.IsError || !strings.Contains(b.ResultText(), "shown to the user") {
				return log.fail("question result %+v", b)
			}
			return fakemodel.Text("I asked you two questions.")
		case 2:
			if !strings.Contains(r.Last().Text(), askAnswerText) {
				return log.fail("answers missing: %.2000s", r.Last().Text())
			}
			return fakemodel.Text("Blue with dark mode and export.")
		}
		return log.fail("unexpected step %d", r.Assistants())
	}
	s := newClaudeStack(t, fakemodel.Options{Main: main})
	s.startWorker()
	sess := s.open("")
	w := sess.watch(0)
	logOnFailure(t, w)
	w.expect(pb.SessionEvent_KIND_UNSPECIFIED)
	sess.send("ask me", pb.InputPriority_INPUT_PRIORITY_NORMAL)
	events := durableUntil(t, w, func(e *pb.SessionEvent) bool { return e.GetKind() == pb.SessionEvent_KIND_RESULT })
	sess.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_COMPLETED)
	pending := lastOfKind(events, pb.SessionEvent_KIND_APPROVAL_PENDING)
	if pending == nil || eventString(t, pending, "kind") != "question" || eventString(t, pending, "tool") != "AskUserQuestion" ||
		eventString(t, pending, "tool_use_id") != "toolu_ask" || eventString(t, pending, "display") != "Which color should the button be?" {
		t.Fatalf("question card %v", pending)
	}
	var sent, stored any
	raw, _ := json.Marshal(askInput)
	must(t, json.Unmarshal(raw, &sent))
	must(t, json.Unmarshal([]byte(eventString(t, pending, "input_json")), &stored))
	if !jsonEqual(sent, stored) {
		t.Fatalf("question input %s", eventString(t, pending, "input_json"))
	}
	list, err := sess.api.ListApprovals(sess.ctx, connect.NewRequest(&pb.ListApprovalsRequest{SessionId: sess.id}))
	must(t, err)
	if a := list.Msg.GetApprovals(); len(a) != 1 || a[0].GetApprovalId() != eventString(t, pending, "approval_id") ||
		a[0].GetKind() != pb.ApprovalKind_APPROVAL_KIND_QUESTION || a[0].GetInputJson() != eventString(t, pending, "input_json") ||
		a[0].GetExpiresAtUnixMs() != 0 {
		t.Fatalf("pending approvals %v", list.Msg)
	}
	if state := sess.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_COMPLETED); state.GetPendingApprovalCount() != 1 {
		t.Fatalf("while asking: %v", state)
	}
	_, err = sess.api.ResolveApproval(sess.ctx, connect.NewRequest(&pb.ResolveApprovalRequest{
		ApprovalId: eventString(t, pending, "approval_id"), Approve: true, Answers: []*pb.QuestionAnswer{
			{Question: "Which color should the button be?", Selected: []string{"Blue"}},
			{Question: "Which features do you want?", Selected: []string{"Dark mode", "Export"}, Other: `and "keyboard" shortcuts`},
		},
	}))
	must(t, err)
	events = durableUntil(t, w, func(e *pb.SessionEvent) bool { return e.GetKind() == pb.SessionEvent_KIND_RESULT })
	if state := sess.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_COMPLETED); state.GetTurnCount() != 2 || state.GetPendingApprovalCount() != 0 {
		t.Fatalf("after the answers: %v", state)
	}
	if got := strings.Join(kinds(events), " "); got != "APPROVAL_RESOLVED TURN_STARTED USER ASSISTANT RESULT" {
		t.Fatalf("answered turn: %s", got)
	}
	// The answered event names the input; the answer's message has its id.
	if eventString(t, events[0], "state") != "answered" || eventString(t, events[2], "circus_note") != "question_answer" ||
		eventString(t, events[0], "input_id") == "" || eventString(t, events[0], "input_id") != eventString(t, events[2], "uuid") {
		t.Fatalf("answered turn: %v %v", events[0], events[2])
	}
	checkStream(t, w, true)
	compareWithFakeWorker(t, "ask", w)

	// A dismissed question: no input, the session takes the next one.
	sess = s.open("")
	w = sess.watch(0)
	logOnFailure(t, w)
	w.expect(pb.SessionEvent_KIND_UNSPECIFIED)
	sess.send("ask and dismiss", pb.InputPriority_INPUT_PRIORITY_NORMAL)
	events = durableUntil(t, w, func(e *pb.SessionEvent) bool { return e.GetKind() == pb.SessionEvent_KIND_RESULT })
	sess.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_COMPLETED)
	pending = lastOfKind(events, pb.SessionEvent_KIND_APPROVAL_PENDING)
	if pending == nil {
		t.Fatal("no question card")
	}
	_, err = sess.api.ResolveApproval(sess.ctx, connect.NewRequest(&pb.ResolveApprovalRequest{ApprovalId: eventString(t, pending, "approval_id")}))
	must(t, err)
	resolved := durableUntil(t, w, func(e *pb.SessionEvent) bool { return e.GetKind() == pb.SessionEvent_KIND_APPROVAL_RESOLVED })
	if r := resolved[len(resolved)-1]; eventString(t, r, "state") != "dismissed" || eventString(t, r, "kind") != "question" {
		t.Fatalf("resolution %v", resolved)
	}
	compareBeforeNext(t, "ask_dismiss", w)
	checkStream(t, w, true)
	if state := sess.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_COMPLETED); state.GetTurnCount() != 1 || state.GetPendingApprovalCount() != 0 {
		t.Fatalf("after dismissing: %v", state)
	}
	sess.send("still ready", pb.InputPriority_INPUT_PRIORITY_NORMAL)
	events = durableUntil(t, w, func(e *pb.SessionEvent) bool { return e.GetKind() == pb.SessionEvent_KIND_RESULT })
	if got := strings.Join(kinds(events), " "); got != "INPUT_ACCEPTED TURN_STARTED USER ASSISTANT RESULT" {
		t.Fatalf("turn after dismissing: %s", got)
	}
	log.check(t)
}

// TestClaudeAskTwoQuestions: the model asks twice in one message. The
// second question supersedes the first; the view has each approval's
// APPROVAL_PENDING before its APPROVAL_RESOLVED (both recorded by the
// control plane with the requests), and one question stays pending.
func TestClaudeAskTwoQuestions(t *testing.T) {
	var log scriptLog
	main := func(r *fakemodel.Request) *fakemodel.Response {
		switch r.Assistants() {
		case 0:
			return fakemodel.Tools(fakemodel.Use("toolu_a1", "AskUserQuestion", askInput), fakemodel.Use("toolu_a2", "AskUserQuestion", askInput))
		case 1:
			for _, id := range []string{"toolu_a1", "toolu_a2"} {
				if b, ok := r.ToolResult(id); !ok || !b.IsError || !strings.Contains(b.ResultText(), "shown to the user") {
					return log.fail("%s result %+v", id, b)
				}
			}
			return fakemodel.Text("I asked twice.")
		}
		return log.fail("unexpected step %d", r.Assistants())
	}
	s := newClaudeStack(t, fakemodel.Options{Main: main})
	s.startWorker()
	sess := s.open("")
	w := sess.watch(0)
	logOnFailure(t, w)
	w.expect(pb.SessionEvent_KIND_UNSPECIFIED)
	sess.send("ask me twice", pb.InputPriority_INPUT_PRIORITY_NORMAL)
	events := durableUntil(t, w, func(e *pb.SessionEvent) bool { return e.GetKind() == pb.SessionEvent_KIND_RESULT })
	sess.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_COMPLETED)
	pendingAt, resolvedAt, toolUseAt := map[string]int{}, map[string]int{}, map[string]int{}
	var order []string
	for i, e := range events {
		switch e.GetKind() {
		case pb.SessionEvent_KIND_APPROVAL_PENDING:
			id := eventString(t, e, "approval_id")
			pendingAt[id] = i
			order = append(order, eventString(t, e, "tool_use_id"))
		case pb.SessionEvent_KIND_APPROVAL_RESOLVED:
			if eventString(t, e, "state") != "superseded" {
				t.Fatalf("resolution %v", e)
			}
			resolvedAt[eventString(t, e, "approval_id")] = i
		case pb.SessionEvent_KIND_ASSISTANT:
			for _, id := range []string{"toolu_a1", "toolu_a2"} {
				if strings.Contains(e.GetPayloadJson(), `"id":"`+id+`"`) {
					toolUseAt[id] = i
				}
			}
		}
	}
	if len(pendingAt) != 2 || len(resolvedAt) != 1 || len(toolUseAt) != 2 {
		t.Fatalf("pending %v resolved %v tool uses %v", pendingAt, resolvedAt, toolUseAt)
	}
	for id, at := range resolvedAt {
		if p, ok := pendingAt[id]; !ok || p > at {
			t.Fatalf("approval %s resolved at %d, pending at %d", id, at, p)
		}
	}
	for _, id := range order {
		if toolUseAt[id] > pendingAt[approvalOf(t, events, id)] {
			t.Fatalf("the card of %s precedes its tool use", id)
		}
	}
	list, err := sess.api.ListApprovals(sess.ctx, connect.NewRequest(&pb.ListApprovalsRequest{SessionId: sess.id}))
	must(t, err)
	if a := list.Msg.GetApprovals(); len(a) != 1 || a[0].GetToolUseId() != order[1] {
		t.Fatalf("pending approvals %v, asked %v", list.Msg, order)
	}
	// Answering the superseded question is refused: the answers would reach nobody.
	var superseded string
	for id := range resolvedAt {
		superseded = id
	}
	_, err = sess.api.ResolveApproval(sess.ctx, connect.NewRequest(&pb.ResolveApprovalRequest{ApprovalId: superseded, Approve: true, Answers: []*pb.QuestionAnswer{
		{Question: "Which color should the button be?", Selected: []string{"Blue"}},
		{Question: "Which features do you want?", Selected: []string{"Search"}},
	}}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("answering a superseded question: %v", err)
	}
	log.check(t)
}

func approvalOf(t *testing.T, events []*pb.SessionEvent, toolUseID string) string {
	t.Helper()
	for _, e := range events {
		if e.GetKind() == pb.SessionEvent_KIND_APPROVAL_PENDING && eventString(t, e, "tool_use_id") == toolUseID {
			return eventString(t, e, "approval_id")
		}
	}
	t.Fatalf("no approval of %s", toolUseID)
	return ""
}

// TestClaudeAskModes: in each permission mode, a Bash call the mode asks
// about reaches the worker's canUseTool, which asks the user for approval
// and denies the call; once approved, the model's retry runs (the
// PreToolUse hook allows it, and canUseTool too where an ask rule asks
// again after the hook). A denied approval is not retried. Auto mode asks
// nothing by itself here (the fake's check flags or allows a call, never
// asks), so in auto mode an ask rule of the project's settings asks.
func TestClaudeAskModes(t *testing.T) {
	for _, mode := range []string{"default", "acceptEdits", "auto"} {
		t.Run(mode, func(t *testing.T) { testAskMode(t, mode) })
	}
}

func testAskMode(t *testing.T, mode string) {
	var log scriptLog
	// Node writes the file: no permission mode allows it without asking.
	const command = `node -e "require('fs').appendFileSync('approved.txt', 'ran\n')"; echo approved`
	const deniedCommand = `node -e "require('fs').writeFileSync('denied.txt', '')"`
	main := func(r *fakemodel.Request) *fakemodel.Response {
		switch {
		case r.Contains("still ready"):
			if r.Contains("The user approved") {
				return log.fail("a denied approval was retried: %.2000s", r.Text())
			}
			return fakemodel.Text("Still ready.")
		case r.Contains("deny me"):
			switch r.Assistants() {
			case 0:
				return fakemodel.Tools(fakemodel.Bash("toolu_deny", deniedCommand))
			case 1:
				return fakemodel.Text("I need approval to run this Bash command.")
			}
			return log.fail("unexpected step %d: %.500s", r.Assistants(), r.Last().Text())
		case !r.Contains("needs approval"):
			return log.fail("unexpected request: %.300s", r.Last().Text())
		}
		switch r.Assistants() {
		case 0:
			return fakemodel.Tools(fakemodel.Bash("toolu_asked", command))
		case 1:
			if b, ok := r.ToolResult("toolu_asked"); !ok || !b.IsError || !strings.Contains(b.ResultText(), "sent to the user for approval") {
				return log.fail("asked result %+v", b)
			}
			return fakemodel.Text("I need approval to run this Bash command.")
		case 2:
			if !r.Contains("The user approved") {
				return log.fail("approval input missing: %.500s", r.Last().Text())
			}
			return fakemodel.Tools(fakemodel.Bash("toolu_retry", command))
		case 3:
			if b, ok := r.ToolResult("toolu_retry"); !ok || b.IsError || !strings.Contains(b.ResultText(), "approved") {
				return log.fail("retry result %+v", b)
			}
			return fakemodel.Text("Bash finished.")
		}
		return log.fail("unexpected step %d", r.Assistants())
	}
	s := newClaudeStack(t, fakemodel.Options{Main: main})
	if mode == "auto" {
		// Auto mode's check allows the call (the fake flags nothing); an
		// ask rule of the project's settings makes the mode ask.
		must(t, os.MkdirAll(filepath.Join(s.dir, ".claude"), 0o755))
		must(t, os.WriteFile(filepath.Join(s.dir, ".claude", "settings.json"), []byte(`{"permissions":{"ask":["Bash(node:*)"]}}`), 0o644))
	}
	s.startWorker()
	sess := s.open(mode)
	w := sess.watch(0)
	logOnFailure(t, w)
	w.expect(pb.SessionEvent_KIND_UNSPECIFIED)
	sess.send("needs approval", pb.InputPriority_INPUT_PRIORITY_NORMAL)
	events := durableUntil(t, w, func(e *pb.SessionEvent) bool { return e.GetKind() == pb.SessionEvent_KIND_RESULT })
	sess.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_COMPLETED)
	pending := lastOfKind(events, pb.SessionEvent_KIND_APPROVAL_PENDING)
	if pending == nil || eventString(t, pending, "kind") != "tool" || eventString(t, pending, "display") != command ||
		eventString(t, pending, "tool_use_id") != "toolu_asked" {
		t.Fatalf("approval card %v", pending)
	}
	if n := countKind(w.durable, pb.SessionEvent_KIND_APPROVAL_PENDING); n != 1 {
		t.Fatalf("%d approval cards for one call", n)
	}
	if _, err := os.Stat(filepath.Join(s.dir, "approved.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the asked command ran: %v", err)
	}
	_, err := sess.api.ResolveApproval(sess.ctx, connect.NewRequest(&pb.ResolveApprovalRequest{ApprovalId: eventString(t, pending, "approval_id"), Approve: true}))
	must(t, err)
	durableUntil(t, w, func(e *pb.SessionEvent) bool { return e.GetKind() == pb.SessionEvent_KIND_RESULT })
	if state := sess.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_COMPLETED); state.GetTurnCount() != 2 || state.GetPendingApprovalCount() != 0 {
		t.Fatalf("after approval: %v", state)
	}
	if got := readFile(t, filepath.Join(s.dir, "approved.txt")); got != "ran\n" {
		t.Fatalf("the approved command ran %q", got)
	}
	checkStream(t, w, true)

	sess = s.open(mode)
	w = sess.watch(0)
	logOnFailure(t, w)
	w.expect(pb.SessionEvent_KIND_UNSPECIFIED)
	sess.send("deny me", pb.InputPriority_INPUT_PRIORITY_NORMAL)
	events = durableUntil(t, w, func(e *pb.SessionEvent) bool { return e.GetKind() == pb.SessionEvent_KIND_RESULT })
	sess.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_COMPLETED)
	pending = lastOfKind(events, pb.SessionEvent_KIND_APPROVAL_PENDING)
	if pending == nil {
		t.Fatal("no approval card for the denied command")
	}
	_, err = sess.api.ResolveApproval(sess.ctx, connect.NewRequest(&pb.ResolveApprovalRequest{ApprovalId: eventString(t, pending, "approval_id"), Approve: false}))
	must(t, err)
	resolved := durableUntil(t, w, func(e *pb.SessionEvent) bool { return e.GetKind() == pb.SessionEvent_KIND_APPROVAL_RESOLVED })
	if eventString(t, resolved[len(resolved)-1], "state") != "denied" {
		t.Fatalf("resolution %v", resolved)
	}
	sess.send("still ready", pb.InputPriority_INPUT_PRIORITY_NORMAL)
	events = durableUntil(t, w, func(e *pb.SessionEvent) bool { return e.GetKind() == pb.SessionEvent_KIND_RESULT })
	if got := strings.Join(kinds(events), " "); got != "INPUT_ACCEPTED TURN_STARTED USER ASSISTANT RESULT" || eventString(t, events[4], "result") != "Still ready." {
		t.Fatalf("turn after the denial: %s", got)
	}
	if state := sess.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_COMPLETED); state.GetTurnCount() != 2 {
		t.Fatalf("after the denial: %v", state)
	}
	if _, err := os.Stat(filepath.Join(s.dir, "denied.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the denied command ran: %v", err)
	}
	log.check(t)
}

func lastOfKind(events []*pb.SessionEvent, kind pb.SessionEvent_Kind) *pb.SessionEvent {
	var out *pb.SessionEvent
	for _, e := range events {
		if e.GetKind() == kind && !e.GetEphemeral() {
			out = e
		}
	}
	return out
}

func countKind(events []*pb.SessionEvent, kind pb.SessionEvent_Kind) int {
	n := 0
	for _, e := range events {
		if e.GetKind() == kind && !e.GetEphemeral() {
			n++
		}
	}
	return n
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
