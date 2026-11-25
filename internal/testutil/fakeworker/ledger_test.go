package fakeworker

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	pb "github.com/EricWvi/circus/gen/go/circus/v1"
	"google.golang.org/protobuf/proto"
)

func userInput(id, text string) *input {
	return &input{InputId: id, Kind: pb.SessionReply_Inputs_Input_KIND_USER, Text: text, Priority: pb.InputPriority_INPUT_PRIORITY_NORMAL}
}

// A control-plane note is pushed as a user message whose view event names
// it in circus_note, synthetic unless it is a ledger note, starts an input
// turn on an idle session and is answered as text even when it reads like
// a script command.
func TestNoteInputStartsTurn(t *testing.T) {
	for _, tc := range []struct {
		name, note string
		synthetic  bool
	}{{"ledger_problems", "ledger_problems", false}, {"ledger_merged", "ledger_merged", false}, {"other", "other_note", true}, {"empty", "", true}} {
		t.Run(tc.name, func(t *testing.T) {
			const text = "/fake bash echo not a script"
			f := &fakeLink{inputs: []*input{{
				InputId: "note-1", Kind: pb.SessionReply_Inputs_Input_KIND_NOTE, Text: text, Note: tc.note,
				Priority: pb.InputPriority_INPUT_PRIORITY_NORMAL,
			}}}
			f.run(t)

			users := f.kinds(pb.SessionEvent_KIND_USER)
			if len(users) != 1 {
				t.Fatalf("user events: %v", users)
			}
			user := users[0]
			// An empty category is left out, as the real worker does.
			if note, ok := user["circus_note"]; user["uuid"] != "note-1" || (user["isSynthetic"] == true) != tc.synthetic || ok != (tc.note != "") || ok && note != tc.note {
				t.Errorf("user event: %v", user)
			}
			if content := user["message"].(map[string]any)["content"]; content != text {
				t.Errorf("content: %v", content)
			}
			started := f.kinds(pb.SessionEvent_KIND_TURN_STARTED)
			if len(started) != 1 || started[0]["origin"] != "input" {
				t.Errorf("turn started: %v", started)
			}
			f.mu.Lock()
			reports, entries := f.reports, f.entries[""]
			f.mu.Unlock()
			if len(reports) != 2 || reports[0].GetOrigin() != pb.TurnOrigin_TURN_ORIGIN_INPUT ||
				!reflect.DeepEqual(reports[1].GetPersisted().GetInputIds(), []string{"note-1"}) {
				t.Errorf("reports: %v", reports)
			}
			// The transcript keeps the SDK message, without circus_note.
			if len(entries) == 0 || entries[0].GetUuid() != "note-1" ||
				strings.Contains(entries[0].GetJson(), `"isSynthetic":true`) != tc.synthetic || strings.Contains(entries[0].GetJson(), "circus_note") {
				t.Errorf("transcript entries: %v", entries)
			}
			if len(f.submitted()) != 0 {
				t.Errorf("a note ran a script: %v", f.submitted())
			}
			if got := f.resultText(t); got != quote(text) {
				t.Errorf("answer %q", got)
			}
		})
	}
}

// A held note before a NOW input is read in the input's turn, before it,
// as with the real worker: the note is pushed silently and takes the
// input's priority, and the CLI runs one turn for the input. Idle, that
// is the only turn; mid-turn, the running turn is stopped first.
func TestSilentNoteBeforeNowInput(t *testing.T) {
	const merged = "[circus ledger] Your ledger changes were merged."
	note := func() *input {
		return &input{InputId: "note-1", Kind: pb.SessionReply_Inputs_Input_KIND_NOTE, Text: merged, Note: "ledger_merged",
			Priority: pb.InputPriority_INPUT_PRIORITY_NORMAL}
	}
	now := func() *input {
		in := userInput("in-1", "/fake ledger-path")
		in.Priority = pb.InputPriority_INPUT_PRIORITY_NOW
		return in
	}
	ended := func(f *fakeLink) []*pb.ReportTurn {
		f.mu.Lock()
		defer f.mu.Unlock()
		var out []*pb.ReportTurn
		for _, r := range f.reports {
			if r.GetPhase() == pb.ReportTurn_PHASE_ENDED {
				out = append(out, r)
			}
		}
		return out
	}
	users := func(f *fakeLink) []any {
		var ids []any
		for _, u := range f.kinds(pb.SessionEvent_KIND_USER) {
			if u["uuid"] == "note-1" || strings.HasPrefix(fmt.Sprint(u["uuid"]), "in-") {
				ids = append(ids, u["uuid"])
			}
		}
		return ids
	}

	t.Run("idle", func(t *testing.T) {
		f := &fakeLink{inputs: []*input{note(), now()}}
		f.run(t)
		reports := ended(f)
		if len(reports) != 1 || reports[0].GetOutcome() != pb.TurnOutcome_TURN_OUTCOME_COMPLETED ||
			!reflect.DeepEqual(reports[0].GetPersisted().GetInputIds(), []string{"note-1", "in-1"}) {
			t.Fatalf("turns: %v", reports)
		}
		if got := users(f); !reflect.DeepEqual(got, []any{"note-1", "in-1"}) {
			t.Errorf("user events %v", got)
		}
		// The note gets no answer of its own: the turn answers the input.
		if got := f.resultText(t); got != "ledger path: none" {
			t.Errorf("answer %q", got)
		}
		for _, a := range f.kinds(pb.SessionEvent_KIND_ASSISTANT) {
			if strings.Contains(fmt.Sprint(a["message"]), "ledger changes were merged") {
				t.Errorf("the note was answered: %v", a)
			}
		}
	})

	t.Run("mid-turn", func(t *testing.T) {
		// A slow answer streams while the note and the NOW input arrive.
		f := &fakeLink{inputs: []*input{userInput("in-0", "/fake slow 20")}, hints: make(chan *pb.WorkerEvent, 1), ended: 2}
		go func() {
			// The turn has started and taken its input (polled here: not
			// the test goroutine).
			for {
				f.mu.Lock()
				started := len(f.reports) == 1 && len(f.inputs) == 0
				f.mu.Unlock()
				if started {
					break
				}
				time.Sleep(time.Millisecond)
			}
			f.mu.Lock()
			f.inputs = []*input{note(), now()}
			f.mu.Unlock()
			f.hints <- inputNudge()
		}()
		f.run(t)
		reports := ended(f)
		if len(reports) != 2 || reports[0].GetOutcome() != pb.TurnOutcome_TURN_OUTCOME_STOPPED ||
			reports[1].GetOutcome() != pb.TurnOutcome_TURN_OUTCOME_COMPLETED ||
			!reflect.DeepEqual(reports[1].GetPersisted().GetInputIds(), []string{"note-1", "in-1"}) {
			t.Fatalf("turns: %v", reports)
		}
		if got := users(f); !reflect.DeepEqual(got, []any{"in-0", "note-1", "in-1"}) {
			t.Errorf("user events %v", got)
		}
	})
}

func TestLedgerPathScript(t *testing.T) {
	for _, tc := range []struct{ path, want string }{{"/srv/ledger/snap 1", "ledger path: /srv/ledger/snap 1"}, {"", "ledger path: none"}} {
		f := &fakeLink{ledgerPath: tc.path, inputs: []*input{userInput("in-1", "/fake ledger-path")}}
		f.run(t)
		if got := f.resultText(t); got != tc.want {
			t.Errorf("ledger path %q: answer %q, want %q", tc.path, got, tc.want)
		}
	}
}

const ledgerRoot = "/srv/ledger/snap"

func TestLedgerScript(t *testing.T) {
	ops := `[{"op":"write","path":"dir/D?a-New title.md","content":"# New\n"},` +
		`{"op":"edit","path":"dir/D1-Old.md","old":"draft","new":"final"},` +
		`{"op":"move","from":"dir/D2-It's.md","to":"other dir/D2-It's.md"},` +
		`{"op":"delete","path":"dir/D3-Gone.md"}]`
	f := &fakeLink{ledgerPath: ledgerRoot, inputs: []*input{userInput("in-1", "/fake ledger "+ops)}}
	f.result = func(call *pb.DeviceCall) *pb.DeviceResult {
		r := &pb.DeviceResult{Output: textOutput("done", false)}
		if call.GetWrite() != nil {
			r.Detail = &pb.DeviceResult_Write{Write: &pb.WriteResult{Sha256: strings.Repeat("a", 64), Created: true}}
		}
		return r
	}
	f.run(t)

	want := []*pb.DeviceCall{
		{Tool: &pb.DeviceCall_Write{Write: &pb.WriteRequest{Path: ledgerRoot + "/dir/D?a-New title.md", Content: "# New\n", Cwd: linkCwd}}},
		{Tool: &pb.DeviceCall_Edit{Edit: &pb.EditRequest{Path: ledgerRoot + "/dir/D1-Old.md", OldString: "draft", NewString: "final", Cwd: linkCwd}}},
		{Tool: &pb.DeviceCall_Bash{Bash: &pb.BashRequest{
			Command: `mkdir -p '/srv/ledger/snap/other dir' && mv '/srv/ledger/snap/dir/D2-It'\''s.md' '/srv/ledger/snap/other dir/D2-It'\''s.md'`,
			Cwd:     linkCwd, WorkingDirs: []string{linkCwd},
		}}},
		{Tool: &pb.DeviceCall_Bash{Bash: &pb.BashRequest{Command: `rm -f '/srv/ledger/snap/dir/D3-Gone.md'`, Cwd: linkCwd, WorkingDirs: []string{linkCwd}}}},
	}
	calls := f.submitted()
	if len(calls) != len(want) {
		t.Fatalf("%d calls, want %d: %v", len(calls), len(want), calls)
	}
	for i, c := range calls {
		got := proto.Clone(c.GetCall()).(*pb.DeviceCall)
		if c.GetDevice() != linkDevice || got.GetSessionId() != linkSessionID || got.GetCallId() == "" {
			t.Errorf("call %d: %v", i+1, c)
		}
		got.CallId, got.SessionId = "", ""
		if bash := got.GetBash(); bash != nil {
			bash.Description = ""
		}
		if !proto.Equal(got, want[i]) {
			t.Errorf("call %d:\n got  %v\n want %v", i+1, got, want[i])
		}
	}

	// Each call is announced in an API message of its own and answered with
	// its tool_result.
	var uses []map[string]any
	for _, a := range f.kinds(pb.SessionEvent_KIND_ASSISTANT) {
		block := a["message"].(map[string]any)["content"].([]any)[0].(map[string]any)
		if block["type"] == "tool_use" {
			uses = append(uses, block)
		}
	}
	names := []string{"Write", "Edit", "Bash", "Bash"}
	if len(uses) != len(names) {
		t.Fatalf("tool uses: %v", uses)
	}
	for i, u := range uses {
		if u["name"] != names[i] || u["id"] != calls[i].GetToolUseId() {
			t.Errorf("tool use %d: %v", i+1, u)
		}
	}
	if input := uses[1]["input"]; !reflect.DeepEqual(input, map[string]any{
		"file_path": ledgerRoot + "/dir/D1-Old.md", "old_string": "draft", "new_string": "final",
	}) {
		t.Errorf("Edit input: %v", input)
	}
	var results []map[string]any
	for _, u := range f.kinds(pb.SessionEvent_KIND_USER) {
		if u["tool_use_result"] != nil {
			results = append(results, u)
		}
	}
	if len(results) != len(names) {
		t.Fatalf("tool results: %v", results)
	}
	write := results[0]["tool_use_result"].(map[string]any)
	if write["type"] != "create" || write["filePath"] != ledgerRoot+"/dir/D?a-New title.md" {
		t.Errorf("Write tool_use_result: %v", write)
	}
	if got := f.resultText(t); got != "Applied 4 ledger operations." {
		t.Errorf("answer %q", got)
	}
}

// The first error result ends the operations.
func TestLedgerScriptStopsAtError(t *testing.T) {
	ops := `[{"op":"delete","path":"a.md"},{"op":"edit","path":"b.md","old":"x","new":"y"},{"op":"delete","path":"c.md"}]`
	f := &fakeLink{ledgerPath: ledgerRoot, inputs: []*input{userInput("in-1", "/fake ledger "+ops)}}
	f.result = func(call *pb.DeviceCall) *pb.DeviceResult {
		if call.GetEdit() != nil {
			return &pb.DeviceResult{Output: textOutput("String to replace not found in file.", true)}
		}
		return &pb.DeviceResult{Output: textOutput("", false)}
	}
	f.run(t)
	if n := len(f.submitted()); n != 2 {
		t.Errorf("%d calls, want 2", n)
	}
	if got := f.resultText(t); got != "Ledger operation 2 of 3 (edit b.md) failed." {
		t.Errorf("answer %q", got)
	}
}

func TestLedgerScriptRefusals(t *testing.T) {
	for _, tc := range []struct{ name, path, ops, want string }{
		{"no snapshot", "", `[{"op":"delete","path":"a.md"}]`, "no ledger snapshot"},
		{"invalid JSON", ledgerRoot, `[{"op":"delete",`, "/fake ledger refused: "},
		{"unknown op", ledgerRoot, `[{"op":"copy","path":"a.md"}]`, `/fake ledger refused: operation 1: unknown op "copy"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeLink{ledgerPath: tc.path, inputs: []*input{userInput("in-1", "/fake ledger "+tc.ops)}}
			f.run(t)
			if n := len(f.submitted()); n != 0 {
				t.Errorf("%d calls", n)
			}
			if got := f.resultText(t); !strings.HasPrefix(got, tc.want) {
				t.Errorf("answer %q, want prefix %q", got, tc.want)
			}
		})
	}
}

func TestParseLedgerOps(t *testing.T) {
	valid := `[{"op":"write","path":"./dir/D?a-T.md","content":""},{"op":"edit","path":"x.md","old":"","new":"y"},` +
		`{"op":"move","from":"a b.md","to":"c/d.md"},{"op":"delete","path":"e.md"}]`
	ops, err := parseLedgerOps(valid)
	if err != nil || len(ops) != 4 {
		t.Fatalf("valid operations: %v, %v", ops, err)
	}
	for _, bad := range []string{
		``, `null`, `[]`, `{}`, `[{"op":"write","path":"a.md"}] x`, `[{"op":"write","path":"a.md","extra":1}]`,
		`[{"op":"delete"}]`, `[{"op":"delete","path":"/etc/passwd"}]`, `[{"op":"delete","path":"a/../../b.md"}]`,
		`[{"op":"delete","path":".."}]`, `[{"op":"delete","path":"."}]`, `[{"op":"move","from":"a.md"}]`,
		`[{"op":"move","from":"a.md","to":"../b.md"}]`, `[{"op":"edit","path":"a.md","old":"x","new":"x"}]`,
		`[{"op":"rename","path":"a.md"}]`,
	} {
		if ops, err := parseLedgerOps(bad); err == nil {
			t.Errorf("%s accepted: %v", bad, ops)
		}
	}
}

func TestShellQuote(t *testing.T) {
	for in, want := range map[string]string{
		"/a/D?x y.md": `'/a/D?x y.md'`, "it's": `'it'\''s'`, "$(rm -rf /)": `'$(rm -rf /)'`,
	} {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %s, want %s", in, got, want)
		}
	}
}
