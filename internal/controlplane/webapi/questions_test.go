package webapi_test

import (
	"strings"
	"testing"

	"connectrpc.com/connect"
	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/store"
)

const twoQuestions = `{"questions":[` +
	`{"question":"Which color?","header":"Color","options":[{"label":"Red","description":"r"},{"label":"Blue","description":"b"}],"multiSelect":false},` +
	`{"question":"Which features?","header":"Features","options":[{"label":"Dark mode","description":"d"},{"label":"Search","description":"s"},` +
	`{"label":"Export","description":"e"}],"multiSelect":true}]}`

func (h *harness) requestQuestion(t *testing.T, f store.Fence, id string) {
	t.Helper()
	_, err := h.st.RequestApproval(t.Context(), f, store.Approval{
		ApprovalID: id, Kind: store.ApprovalQuestion, ToolUseID: "tu-" + id, Tool: "AskUserQuestion",
		Display: "Which color?", InputJSON: twoQuestions, CreatedAt: h.clock.Load(),
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestQuestionApprovals(t *testing.T) {
	h, super := newHarness(t)
	ctx := t.Context()
	_, sess := sessionSetup(t, h, super)
	id := sess.SessionId
	fence := h.lease(t, id)
	h.requestQuestion(t, fence, "q1")
	h.requestApproval(t, fence, "ap1", 600_000)
	// Questions do not expire.
	h.clock.Add(1_800_000) // past the tool approval's ten minutes
	list, err := super.sessions.ListApprovals(ctx, connect.NewRequest(&circusv1.ListApprovalsRequest{SessionId: id}))
	if err != nil || len(list.Msg.Approvals) != 1 {
		t.Fatalf("list = %v, %v", list, err)
	}
	if a := list.Msg.Approvals[0]; a.ApprovalId != "q1" || a.Kind != circusv1.ApprovalKind_APPROVAL_KIND_QUESTION || a.InputJson != twoQuestions ||
		a.ExpiresAtUnixMs != 0 || a.Device != "" || a.MatchKey != "" || a.Tool != "AskUserQuestion" || a.ToolUseId != "tu-q1" || a.Display != "Which color?" {
		t.Fatalf("approval = %v", a)
	}
	if got := super.getSession(t, id); got.PendingApprovalCount != 1 {
		t.Fatalf("pending = %d", got.PendingApprovalCount)
	}

	resolve := func(approvalID string, approve bool, answers ...*circusv1.QuestionAnswer) error {
		_, err := super.sessions.ResolveApproval(ctx, connect.NewRequest(&circusv1.ResolveApprovalRequest{ApprovalId: approvalID, Approve: approve, Answers: answers}))
		return err
	}
	answer := func(question string, other string, selected ...string) *circusv1.QuestionAnswer {
		return &circusv1.QuestionAnswer{Question: question, Selected: selected, Other: other}
	}
	color, features := answer("Which color?", "", "Blue"), answer("Which features?", "", "Dark mode", "Export")
	for name, answers := range map[string][]*circusv1.QuestionAnswer{
		"no answers":            nil,
		"one answer missing":    {color},
		"out of order":          {features, color},
		"another question":      {answer("Which colour?", "", "Blue"), features},
		"not an option":         {answer("Which color?", "", "Green"), features},
		"two on single-choice":  {answer("Which color?", "", "Red", "Blue"), features},
		"nothing chosen":        {answer("Which color?", ""), features},
		"an option twice":       {color, answer("Which features?", "", "Search", "Search")},
		"an answer too long":    {color, answer("Which features?", strings.Repeat("x", 4097))},
		"too many answers":      {color, features, color, features, color},
		"five selected options": {color, answer("Which features?", "", "a", "b", "c", "d", "e")},
	} {
		if err := resolve("q1", true, answers...); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Fatalf("%s: error = %v", name, err)
		}
	}
	wantCode(t, resolve("q1", false, color, features), connect.CodeInvalidArgument)
	// Answers belong to questions only.
	wantCode(t, resolve("ap1", true, color), connect.CodeInvalidArgument)
	wantCalls(t, "nothing woken", h.waker.take())

	// The user's own text is quoted so that it cannot break the format.
	tricky := "and \"keyboard\" shortcuts\\\nnew line\x01"
	if err := resolve("q1", true, color, answer("Which features?", tricky, "Dark mode", "Export")); err != nil {
		t.Fatal(err)
	}
	wantCalls(t, "answering wakes", h.waker.take(), id+":INPUT")
	// Resolving again changes nothing, whatever it asks.
	if err := resolve("q1", true, color, features); err != nil {
		t.Fatal(err)
	}
	if err := resolve("q1", false); err != nil {
		t.Fatal(err)
	}
	wantCalls(t, "repeats wake nothing", h.waker.take())
	inputs, err := h.st.TakeInputs(ctx, store.Fence{SessionID: id, Epoch: fence.Epoch, WorkerID: "w1", BootID: "boot", ConnID: "conn", Now: h.clock.Load()})
	if err != nil || len(inputs) != 1 {
		t.Fatalf("inputs = %v, %v", inputs, err)
	}
	const text = `The user answered your questions: "Which color?"="Blue", ` +
		`"Which features?"="Dark mode, Export, and \"keyboard\" shortcuts\\\nnew line\u0001". Continue with these answers in mind.`
	if in := inputs[0]; in.Kind != store.InputAnswer || in.Text != text || in.ClientMsgID != "approval:q1" || in.Priority != store.InputNormal {
		t.Fatalf("answer input = %+v\nwant text %s", in, text)
	}

	// Dismissing writes no input and wakes nothing.
	h.requestQuestion(t, fence, "q2")
	if err := resolve("q2", false); err != nil {
		t.Fatal(err)
	}
	wantCalls(t, "dismissing wakes nothing", h.waker.take())
	inputs, err = h.st.TakeInputs(ctx, store.Fence{SessionID: id, Epoch: fence.Epoch, WorkerID: "w1", BootID: "boot", ConnID: "conn", Now: h.clock.Load()})
	// Only the answers, taken and not yet persisted, are open.
	if err != nil || len(inputs) != 1 || inputs[0].ClientMsgID != "approval:q1" {
		t.Fatalf("inputs after dismissing = %v, %v", inputs, err)
	}
	// A question no longer pending and not answered takes no answers: they
	// would reach nobody. Dismissing it again, or dismissing a question
	// replaced by a newer one, is a no-op.
	wantCode(t, resolve("q2", true, color, features), connect.CodeFailedPrecondition)
	if err := resolve("q2", false); err != nil {
		t.Fatal(err)
	}
	h.requestQuestion(t, fence, "q4")
	h.requestQuestion(t, fence, "q5") // supersedes q4
	wantCode(t, resolve("q4", true, color, features), connect.CodeFailedPrecondition)
	if err := resolve("q4", false); err != nil {
		t.Fatal(err)
	}
	wantCalls(t, "refusals wake nothing", h.waker.take())
	var resolved []string
	for _, e := range h.events(t, id) {
		if e.Kind == "APPROVAL_RESOLVED" {
			resolved = append(resolved, string(e.Payload))
		}
	}
	// The answered event names the input it wrote.
	wantCalls(t, "resolution events", resolved,
		`{"approval_id":"q1","kind":"question","state":"answered","answers":[{"question":"Which color?","selected":["Blue"],"other":""},`+
			`{"question":"Which features?","selected":["Dark mode","Export"],"other":"and \"keyboard\" shortcuts\\\nnew line\u0001"}],`+
			`"input_id":"`+inputs[0].InputID+`"}`,
		`{"approval_id":"q2","kind":"question","state":"dismissed"}`)

	// A stop ends the pending question, recorded before the stop itself;
	// answering it then is refused. The tool approval that expired by time
	// (swept) gets no event from the stop.
	if exp, err := h.st.ExpireApprovals(ctx, h.clock.Load(), 10); err != nil || len(exp) != 1 || exp[0].ApprovalID != "ap1" {
		t.Fatalf("swept %v, %v", exp, err)
	}
	if _, err := super.sessions.StopSession(ctx, connect.NewRequest(&circusv1.StopSessionRequest{SessionId: id})); err != nil {
		t.Fatal(err)
	}
	wantCode(t, resolve("q5", true, color, features), connect.CodeFailedPrecondition)
	wantCode(t, resolve("q5", false), connect.CodeFailedPrecondition)
	var tail []string
	for _, e := range h.events(t, id) {
		if e.Kind == "APPROVAL_RESOLVED" || e.Kind == "SESSION_STATE" {
			tail = append(tail, e.Kind+" "+string(e.Payload))
		}
	}
	wantCalls(t, "stop events", tail[len(tail)-3:], `APPROVAL_RESOLVED {"approval_id":"q2","kind":"question","state":"dismissed"}`,
		`APPROVAL_RESOLVED {"approval_id":"q5","kind":"question","state":"expired"}`, `SESSION_STATE {"state":"stopped","reason":"user"}`)
	list, err = super.sessions.ListApprovals(ctx, connect.NewRequest(&circusv1.ListApprovalsRequest{SessionId: id}))
	if err != nil || len(list.Msg.Approvals) != 0 {
		t.Fatalf("list after the stop = %v, %v", list, err)
	}
}
