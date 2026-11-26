package sessions_test

import (
	"encoding/json"
	"strings"
	"testing"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/store"
	wc "github.com/EricWvi/circus/internal/testutil/workerclient"
)

const askInput = `{"questions":[{"question":"Which color?","header":"Color","options":[{"label":"Red","description":"r"},` +
	`{"label":"Blue","description":"b"}],"multiSelect":false}]}`

// TestQuestionRequests: a QUESTION request is checked for the shape of an
// AskUserQuestion input, merged by tool use, and supersedes the session's
// pending question with an APPROVAL_RESOLVED event in the same
// transaction; the answers come back as a KIND_ANSWER input. PLAN is
// refused.
func TestQuestionRequests(t *testing.T) {
	f := newFixture(t)
	c, _, epoch := held(t, f)
	call(t, c, wc.TakeInputs("s1", epoch))
	ask := func(toolUseID, input string) *circusv1.RequestApproval {
		return &circusv1.RequestApproval{Kind: circusv1.ApprovalKind_APPROVAL_KIND_QUESTION, ToolUseId: toolUseID, Tool: "AskUserQuestion",
			Display: "Which color?", InputJson: input}
	}
	question := func(q string) string {
		return `{"questions":[` + q + `]}`
	}
	const red, blue = `{"label":"Red","description":"r"}`, `{"label":"Blue","description":"b"}`
	var qs []string
	for i := range 5 {
		qs = append(qs, `{"question":"Q`+string(rune('0'+i))+`?","header":"H","options":[`+red+`,`+blue+`],"multiSelect":false}`)
	}
	five := strings.Join(qs, ",")
	invalid := map[string]*circusv1.RequestApproval{
		"plan":              {Kind: circusv1.ApprovalKind_APPROVAL_KIND_PLAN, ToolUseId: "t", Tool: "ExitPlanMode", InputJson: "{}"},
		"unknown kind":      {Kind: 9, ToolUseId: "t", Tool: "AskUserQuestion", InputJson: askInput},
		"another tool":      {Kind: circusv1.ApprovalKind_APPROVAL_KIND_QUESTION, ToolUseId: "t", Tool: "Bash", InputJson: askInput},
		"a device":          {Kind: circusv1.ApprovalKind_APPROVAL_KIND_QUESTION, ToolUseId: "t", Tool: "AskUserQuestion", Device: "desk", InputJson: askInput},
		"a match key":       {Kind: circusv1.ApprovalKind_APPROVAL_KIND_QUESTION, ToolUseId: "t", Tool: "AskUserQuestion", MatchKey: matchKey, InputJson: askInput},
		"a reason":          {Kind: circusv1.ApprovalKind_APPROVAL_KIND_QUESTION, ToolUseId: "t", Tool: "AskUserQuestion", Reason: "r", InputJson: askInput},
		"a subagent":        {Kind: circusv1.ApprovalKind_APPROVAL_KIND_QUESTION, ToolUseId: "t", Tool: "AskUserQuestion", AgentId: "a1", InputJson: askInput},
		"no tool use":       ask("", askInput),
		"no input":          ask("t", ""),
		"input on a tool":   {ToolUseId: "t", Tool: "bash", Device: "desk", MatchKey: matchKey, InputJson: askInput},
		"input too large":   ask("t", question(`{"question":"`+strings.Repeat("x", 32<<10)+`","header":"H","options":[`+red+`,`+blue+`],"multiSelect":false}`)),
		"not JSON":          ask("t", "{"),
		"trailing data":     ask("t", askInput+"{}"),
		"no questions":      ask("t", `{"questions":[]}`),
		"five questions":    ask("t", question(five)),
		"no question text":  ask("t", question(`{"question":"","header":"H","options":[`+red+`,`+blue+`],"multiSelect":false}`)),
		"no header":         ask("t", question(`{"question":"Q?","options":[`+red+`,`+blue+`],"multiSelect":false}`)),
		"one option":        ask("t", question(`{"question":"Q?","header":"H","options":[`+red+`],"multiSelect":false}`)),
		"five options":      ask("t", question(`{"question":"Q?","header":"H","options":[`+red+`,`+blue+`,{"label":"a"},{"label":"b"},{"label":"c"}],"multiSelect":false}`)),
		"no label":          ask("t", question(`{"question":"Q?","header":"H","options":[`+red+`,{"description":"b"}],"multiSelect":false}`)),
		"repeated label":    ask("t", question(`{"question":"Q?","header":"H","options":[`+red+`,`+red+`],"multiSelect":false}`)),
		"no multiSelect":    ask("t", question(`{"question":"Q?","header":"H","options":[`+red+`,`+blue+`]}`)),
		"string multi":      ask("t", question(`{"question":"Q?","header":"H","options":[`+red+`,`+blue+`],"multiSelect":"yes"}`)),
		"repeated question": ask("t", question(`{"question":"Q?","header":"H","options":[`+red+`,`+blue+`],"multiSelect":false},{"question":"Q?","header":"H","options":[`+red+`,`+blue+`],"multiSelect":true}`)),
		"long header":       ask("t", question(`{"question":"Q?","header":"`+strings.Repeat("h", 257)+`","options":[`+red+`,`+blue+`],"multiSelect":false}`)),
	}
	for name, req := range invalid {
		r := call(t, c, wc.Op("s1", epoch, req))
		if r.GetRejected().GetCode() != circusv1.SessionReply_Rejected_CODE_INVALID {
			t.Fatalf("%s: %v", name, r)
		}
	}

	r := call(t, c, wc.Op("s1", epoch, ask("tu1", askInput)))
	first := r.GetApproval().GetApprovalId()
	if first == "" || r.GetApproval().GetDeduplicated() {
		t.Fatalf("question %v", r)
	}
	if r := call(t, c, wc.Op("s1", epoch, ask("tu1", askInput))); r.GetApproval().GetApprovalId() != first || !r.GetApproval().GetDeduplicated() {
		t.Fatalf("repeated question %v", r)
	}
	a, err := f.st.GetApproval(t.Context(), first)
	must(t, err)
	if a.Kind != store.ApprovalQuestion || a.InputJSON != askInput || a.ExpiresAt != 0 || a.DeviceID != "" || a.Display != "Which color?" {
		t.Fatalf("stored %+v", a)
	}

	// A new question supersedes the first: the first's RESOLVED, then the
	// second's PENDING, recorded with the request and announced.
	before := f.bus.seq("s1")
	r = call(t, c, wc.Op("s1", epoch, ask("tu2", askInput)))
	second := r.GetApproval().GetApprovalId()
	if second == "" || second == first {
		t.Fatalf("second question %v", r)
	}
	// A repeat records nothing more.
	call(t, c, wc.Op("s1", epoch, ask("tu2", askInput)))
	// A tool approval's PENDING names the device and has no input.
	tool := call(t, c, wc.Op("s1", epoch, &circusv1.RequestApproval{ToolUseId: "tu3", Tool: "Bash", Device: "desk", MatchKey: matchKey,
		Display: "ls", Reason: "the mode asks"})).GetApproval().GetApprovalId()
	call(t, c, wc.Op("s1", epoch, &circusv1.RequestApproval{ToolUseId: "tu4", Tool: "Bash", Device: "desk", MatchKey: matchKey, Display: "ls"}))
	page, err := f.st.ReadEvents(t.Context(), store.EventFilter{SessionID: "s1", Limit: store.MaxPageSize, MaxBytes: store.MaxTranscriptPageBytes})
	must(t, err)
	var got []string
	var seq int64
	for _, e := range page.Events {
		if e.Kind == store.EventKindApprovalResolved || e.Kind == store.EventKindApprovalPending {
			got, seq = append(got, e.Kind+" "+string(e.Payload)), e.Seq
		}
	}
	pending := func(id, toolUseID string) string {
		return `APPROVAL_PENDING {"approval_id":"` + id + `","kind":"question","tool":"AskUserQuestion","device":"","tool_use_id":"` + toolUseID +
			`","display":"Which color?","reason":"","input_json":` + jsonQuote(askInput) + `}`
	}
	want := []string{
		pending(first, "tu1"),
		`APPROVAL_RESOLVED {"approval_id":"` + first + `","kind":"question","state":"superseded"}`,
		pending(second, "tu2"),
		`APPROVAL_PENDING {"approval_id":"` + tool + `","kind":"tool","tool":"Bash","device":"desk","tool_use_id":"tu3","display":"ls","reason":"the mode asks"}`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("events\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if f.bus.seq("s1") <= before || f.bus.seq("s1") != uint64(seq) {
		t.Fatalf("announced %d, event %d", f.bus.seq("s1"), seq)
	}
	if a, _ := f.st.GetApproval(t.Context(), first); a.State != store.ApprovalSuperseded {
		t.Fatalf("first question %+v", a)
	}

	// The answers arrive as a KIND_ANSWER input.
	res, err := f.st.ResolveApproval(t.Context(), store.ApprovalResolution{ApprovalID: second, Approve: true, At: ms(f),
		Input: store.Input{InputID: "answers", Text: `The user answered your questions: "Which color?"="Red". Continue with these answers in mind.`}})
	must(t, err)
	if !res.Changed {
		t.Fatal("not answered")
	}
	in := call(t, c, wc.TakeInputs("s1", epoch)).GetInputs().GetInputs()
	var answers []*circusv1.SessionReply_Inputs_Input
	for _, i := range in {
		if i.GetKind() == circusv1.SessionReply_Inputs_Input_KIND_ANSWER {
			answers = append(answers, i)
		}
	}
	if len(answers) != 1 || answers[0].GetInputId() != "answers" || !strings.Contains(answers[0].GetText(), `"Which color?"="Red"`) {
		t.Fatalf("inputs %v", in)
	}
}

// jsonQuote is s as a JSON string.
func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
