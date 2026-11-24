//go:build e2e && linux

package e2e

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/testutil/fakemodel"
	testwait "github.com/EricWvi/circus/internal/testutil/wait"
)

// scriptLog collects what went wrong in the model script (which runs on
// the fake's goroutines); the test checks it at the end.
type scriptLog struct {
	mu   sync.Mutex
	errs []string
}

func (l *scriptLog) fail(format string, args ...any) *fakemodel.Response {
	msg := fmt.Sprintf(format, args...)
	l.mu.Lock()
	l.errs = append(l.errs, msg)
	l.mu.Unlock()
	return fakemodel.Text("script error: " + msg)
}

func (l *scriptLog) check(t *testing.T) {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, e := range l.errs {
		t.Error("model script: " + e)
	}
}

// logOnFailure logs a watch's durable events if the test fails.
func logOnFailure(t *testing.T, w *sessionWatch) {
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		for _, e := range w.durable {
			t.Logf("event %d %s %.600s", e.GetSeq(), e.GetKind(), e.GetPayloadJson())
		}
	})
}

// eventWait bounds each wait for a session event, so a regression fails in
// seconds rather than at the stack's deadline.
const eventWait = 20 * time.Second

// durableUntil waits (bounded) for a durable event matching match and
// returns the durable events received meanwhile, that one included.
func durableUntil(t *testing.T, w *sessionWatch, match func(*pb.SessionEvent) bool) []*pb.SessionEvent {
	t.Helper()
	start := len(w.durable)
	w.within(eventWait, func() {
		w.until(func(e *pb.SessionEvent) bool { return !e.GetEphemeral() && match(e) })
	})
	return w.durable[start:]
}

func kinds(events []*pb.SessionEvent) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		if !e.GetEphemeral() {
			out = append(out, strings.TrimPrefix(e.GetKind().String(), "KIND_"))
		}
	}
	return out
}

// checkResult checks a result's subtype, terminal reason and error flag.
func checkResult(t *testing.T, event *pb.SessionEvent, subtype, reason string, isError bool) {
	t.Helper()
	var result struct {
		Subtype        string
		TerminalReason string `json:"terminal_reason"`
		IsError        bool   `json:"is_error"`
	}
	must(t, json.Unmarshal([]byte(event.GetPayloadJson()), &result))
	if result.Subtype != subtype || result.TerminalReason != reason || result.IsError != isError {
		t.Fatalf("result: %v", event)
	}
}

// resultFor finds the tool_result event of a tool use.
func resultFor(t *testing.T, events []*pb.SessionEvent, toolUseID string) *pb.SessionEvent {
	t.Helper()
	for _, e := range events {
		if e.GetKind() == pb.SessionEvent_KIND_USER && strings.Contains(e.GetPayloadJson(), `"tool_use_id":"`+toolUseID+`"`) {
			return e
		}
	}
	t.Fatalf("no tool result for %s", toolUseID)
	return nil
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	must(t, err)
	return string(data)
}

// TestClaudeTextAndTools: a text turn, a thinking turn, one Bash, two
// parallel Bash calls, and Write, Read, Edit and Bash that really change
// the project directory; each session's events match the fake worker's for
// the same scenario.
func TestClaudeTextAndTools(t *testing.T) {
	var log scriptLog
	notes := ""
	main := func(r *fakemodel.Request) *fakemodel.Response {
		step := r.Assistants()
		switch {
		case r.Contains("say hello"):
			return fakemodel.Text("Hello from the fake model.")
		case r.Contains("think first"):
			return &fakemodel.Response{Thinking: "The user wants me to think.", Text: "Thought about it."}
		case r.Contains("run one command"):
			if step == 0 {
				return fakemodel.Tools(fakemodel.Bash("toolu_one", "echo hi"))
			}
			if b, ok := r.ToolResult("toolu_one"); !ok || b.IsError || !strings.Contains(b.ResultText(), "hi") {
				return log.fail("one command: result %+v", b)
			}
			return fakemodel.Text("Bash finished.")
		case r.Contains("run two commands"):
			if step == 0 {
				return fakemodel.Tools(fakemodel.Bash("toolu_p1", "echo first"), fakemodel.Bash("toolu_p2", "echo second"))
			}
			for id, want := range map[string]string{"toolu_p1": "first", "toolu_p2": "second"} {
				if b, ok := r.ToolResult(id); !ok || b.IsError || !strings.Contains(b.ResultText(), want) {
					return log.fail("parallel %s: result %+v", id, b)
				}
			}
			return fakemodel.Text("Both finished.")
		case r.Contains("edit the notes"):
			uses := []fakemodel.ToolUse{
				fakemodel.Use("toolu_w", "Write", map[string]any{"file_path": notes, "content": "alpha\nkeep\n"}),
				fakemodel.Use("toolu_r", "Read", map[string]any{"file_path": notes}),
				fakemodel.Use("toolu_e", "Edit", map[string]any{"file_path": notes, "old_string": "alpha", "new_string": "beta"}),
				fakemodel.Bash("toolu_b", "cat notes.txt && pwd && printf bash-ran > bash.txt"),
			}
			if step > 0 {
				prev := uses[step-1].ID
				b, ok := r.ToolResult(prev)
				if !ok || b.IsError {
					return log.fail("%s: result %+v", prev, b)
				}
				if prev == "toolu_r" && !strings.Contains(b.ResultText(), "alpha") {
					return log.fail("Read returned %q", b.ResultText())
				}
			}
			if step < len(uses) {
				return fakemodel.Tools(uses[step])
			}
			return fakemodel.Text("Notes edited.")
		}
		return log.fail("unexpected request: %.300s", r.Last().Text())
	}
	s := newClaudeStack(t, fakemodel.Options{Main: main})
	notes = filepath.Join(s.dir, "notes.txt")
	s.startWorker()

	run := func(scenario, text string) []*pb.SessionEvent {
		t.Helper()
		sess := s.open("")
		w := sess.watch(0)
		logOnFailure(t, w)
		w.expect(pb.SessionEvent_KIND_UNSPECIFIED)
		sent := sess.send(text, pb.InputPriority_INPUT_PRIORITY_NORMAL)
		events := durableUntil(t, w, func(e *pb.SessionEvent) bool { return e.GetKind() == pb.SessionEvent_KIND_RESULT })
		sess.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_COMPLETED)
		if user := events[2]; eventString(t, user, "uuid") != sent.GetInputId() {
			t.Fatalf("delivered input %v, sent %s", user, sent.GetInputId())
		}
		if scenario != "" {
			compareWithFakeWorker(t, scenario, w)
		}
		checkStream(t, w, true)
		return events
	}

	events := run("text", "say hello")
	if got := eventString(t, events[len(events)-1], "result"); got != "Hello from the fake model." {
		t.Fatalf("result %q", got)
	}
	main0 := s.fake.MainRequests()[0]
	if main0.Safeguards == nil || !main0.Contains("Primary working directory: "+s.dir) || main0.Count("say hello") != 1 {
		t.Fatalf("first model request: safeguards %s, cwd or input missing: %.2000s", main0.Safeguards, main0.Text())
	}

	events = run("think", "think first")
	if assistantID(t, events[3]) != assistantID(t, events[4]) || !strings.Contains(events[3].GetPayloadJson(), `"thinking":"The user wants me to think."`) {
		t.Fatalf("thinking and text: %v / %v", events[3], events[4])
	}

	events = run("tool", "run one command")
	checkToolPair(t, events[3], events[4], "Bash", false, "hi")

	events = run("parallel_tools", "run two commands")
	checkToolPair(t, events[3], resultFor(t, events, "toolu_p1"), "Bash", false, "first")
	checkToolPair(t, events[4], resultFor(t, events, "toolu_p2"), "Bash", false, "second")

	events = run("", "edit the notes")
	if got := strings.Join(kinds(events), " "); got != "INPUT_ACCEPTED TURN_STARTED USER ASSISTANT USER ASSISTANT USER ASSISTANT USER ASSISTANT USER ASSISTANT RESULT" {
		t.Fatalf("edit turn events: %s", got)
	}
	for i, tool := range []string{"Write", "Read", "Edit", "Bash"} {
		checkToolPair(t, events[3+2*i], events[4+2*i], tool, false)
	}
	checkToolPair(t, events[9], events[10], "Bash", false, `beta\nkeep\n`+s.dir)
	if got := readFile(t, notes); got != "beta\nkeep\n" {
		t.Fatalf("notes.txt = %q", got)
	}
	if got := readFile(t, filepath.Join(s.dir, "bash.txt")); got != "bash-ran" {
		t.Fatalf("bash.txt = %q", got)
	}
	log.check(t)
}

func waitFile(t *testing.T, path string) {
	t.Helper()
	wait(t, 20*time.Second, func() bool { _, err := os.Stat(path); return err == nil })
}

// streamed waits for the first streamed text delta of a held answer.
func streamed(t *testing.T, w *sessionWatch, gate *fakemodel.Gate) {
	t.Helper()
	testwait.Recv(t, gate.Reached())
	w.within(eventWait, func() {
		w.until(func(e *pb.SessionEvent) bool {
			return e.GetEphemeral() && strings.Contains(e.GetPayloadJson(), `"text_delta"`)
		})
	})
}

// TestClaudeMergeAndNow: an input sent while a tool runs joins the running
// turn (the model sees it with the tool result; one turn, one result); an
// input with NOW priority stops the turn streaming and starts its own.
func TestClaudeMergeAndNow(t *testing.T) {
	var log scriptLog
	held := fakemodel.NewGate()
	main := func(r *fakemodel.Request) *fakemodel.Response {
		switch {
		case r.Contains("after the replacement"):
			return fakemodel.Text("After the replacement.")
		case r.Contains("right now"):
			return fakemodel.Text("Handled right now.")
		case r.Contains("stream slowly"):
			return &fakemodel.Response{Text: "This answer streams until it is replaced.", Hold: held}
		case r.Contains("start waiting"):
			if r.Assistants() == 0 {
				return fakemodel.Tools(fakemodel.Bash("toolu_wait", "touch started.txt; for i in $(seq 1200); do [ -e go.txt ] && break; sleep 0.05; done; echo waited"))
			}
			merged := r.MidTurnMessages()
			if b, ok := r.ToolResult("toolu_wait"); !ok || b.IsError || b.ResultText() != "waited" || len(merged) != 1 || merged[0] != "also this" {
				return log.fail("merge: result %+v, merged %q", b, merged)
			}
			return fakemodel.Text("Merged: " + merged[0])
		}
		return log.fail("unexpected request: %.300s", r.Last().Text())
	}
	s := newClaudeStack(t, fakemodel.Options{Main: main})
	s.startWorker()

	sess := s.open("")
	w := sess.watch(0)
	logOnFailure(t, w)
	w.expect(pb.SessionEvent_KIND_UNSPECIFIED)
	sess.send("start waiting", pb.InputPriority_INPUT_PRIORITY_NORMAL)
	// The worker publishes its events in batches (about every 100 ms): wait
	// for the tool use to be published so the second input's acceptance
	// (which the control plane records at once) comes after it.
	durableUntil(t, w, contains(pb.SessionEvent_KIND_ASSISTANT, `"id":"toolu_wait"`))
	waitFile(t, filepath.Join(s.dir, "started.txt"))
	second := sess.send("also this", pb.InputPriority_INPUT_PRIORITY_NORMAL)
	// Claude Code queued it (its transcript says so) while the tool runs.
	s.waitTranscript(sess.id, func(e transcriptEntry) bool {
		return strings.Contains(e.JSON, `"operation":"enqueue"`) && strings.Contains(e.JSON, `"content":"also this"`)
	})
	must(t, os.WriteFile(filepath.Join(s.dir, "go.txt"), nil, 0o600))
	durableUntil(t, w, func(e *pb.SessionEvent) bool { return e.GetKind() == pb.SessionEvent_KIND_RESULT })
	events := w.durable
	if got := strings.Join(kinds(events), " "); got != "INPUT_ACCEPTED TURN_STARTED USER ASSISTANT INPUT_ACCEPTED USER USER ASSISTANT RESULT" {
		t.Fatalf("merged turn events: %s", got)
	}
	checkToolPair(t, events[3], events[5], "Bash", false, "waited")
	if eventString(t, events[6], "uuid") != second.GetInputId() {
		t.Fatalf("merged input event %v", events[6])
	}
	if got := eventString(t, events[8], "result"); got != "Merged: also this" {
		t.Fatalf("result %q", got)
	}
	if state := sess.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_COMPLETED); state.GetTurnCount() != 1 {
		t.Fatalf("the merged input started a turn of its own: %v", state)
	}
	checkStream(t, w, true)

	// NOW: compared with the fake worker's "now" in a session of its own.
	sess = s.open("")
	w = sess.watch(0)
	logOnFailure(t, w)
	w.expect(pb.SessionEvent_KIND_UNSPECIFIED)
	sess.send("stream slowly", pb.InputPriority_INPUT_PRIORITY_NORMAL)
	streamed(t, w, held)
	now := sess.send("right now", pb.InputPriority_INPUT_PRIORITY_NOW)
	results := 0
	events = durableUntil(t, w, func(e *pb.SessionEvent) bool {
		if e.GetKind() == pb.SessionEvent_KIND_RESULT {
			results++
		}
		return results == 2
	})
	compareBeforeNext(t, "now", w)
	checkAbortedResult(t, w.durable[5], "success", "aborted_streaming", false)
	if eventString(t, w.durable[8], "uuid") != now.GetInputId() || eventString(t, w.durable[10], "result") != "Handled right now." {
		t.Fatalf("NOW turn: %v / %v", w.durable[8], w.durable[10])
	}
	if state := sess.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_COMPLETED); state.GetTurnCount() != 2 {
		t.Fatalf("after NOW: %v", state)
	}
	stopped := s.waitFake(func(r *fakemodel.Request) bool { return r.Contains("stream slowly") && r.Outcome != fakemodel.Pending })
	if stopped.Outcome != fakemodel.ClientGone {
		t.Fatalf("the replaced stream ended %v", stopped.Outcome)
	}
	// The turn after the replacement runs to completion.
	sess.send("after the replacement", pb.InputPriority_INPUT_PRIORITY_NORMAL)
	events = durableUntil(t, w, func(e *pb.SessionEvent) bool {
		return e.GetKind() == pb.SessionEvent_KIND_RESULT || e.GetKind() == pb.SessionEvent_KIND_TURN_STOPPED
	})
	if got := strings.Join(kinds(events), " "); got != "INPUT_ACCEPTED TURN_STARTED USER ASSISTANT RESULT" || eventString(t, events[4], "result") != "After the replacement." {
		t.Fatalf("turn after NOW: %s", got)
	}
	if state := sess.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_COMPLETED); state.GetTurnCount() != 3 {
		t.Fatalf("after the turn after NOW: %v", state)
	}
	checkStream(t, w, true)
	s.checkTranscript(sess.id)
	log.check(t)
}

// TestClaudeInterruptAndStop: the user interrupts a turn while the model
// streams and while Bash runs (the command is killed), and stops a session
// mid-turn (the lease is released and its Claude Code exits).
func TestClaudeInterruptAndStop(t *testing.T) {
	var log scriptLog
	streaming := fakemodel.NewGate()
	main := func(r *fakemodel.Request) *fakemodel.Response {
		switch {
		case r.Contains("next after the interrupt"):
			if r.Count("next after the interrupt") != 1 {
				return log.fail("next input twice: %.2000s", r.Text())
			}
			return fakemodel.Text("Next turn done.")
		case r.Contains("stream then stop"), r.Contains("stream then interrupt"):
			if r.Assistants() == 0 {
				return &fakemodel.Response{Text: "This answer streams until the user acts.", Hold: streaming}
			}
		case r.Contains("run forever"):
			if r.Assistants() == 0 {
				return fakemodel.Tools(fakemodel.Bash("toolu_forever", "echo $$ > pid.txt; touch started.txt; for i in $(seq 1200); do sleep 0.05; done"))
			}
		}
		return log.fail("unexpected request: %.300s", r.Last().Text())
	}
	s := newClaudeStack(t, fakemodel.Options{Main: main})
	s.startWorker()
	interrupt := func(sess *sessionBrowser) {
		_, err := sess.api.Interrupt(sess.ctx, connect.NewRequest(&pb.InterruptRequest{SessionId: sess.id}))
		must(t, err)
	}
	// The turn after an interrupted one runs to completion.
	next := func(sess *sessionBrowser, w *sessionWatch, turns uint64) {
		t.Helper()
		sess.send("next after the interrupt", pb.InputPriority_INPUT_PRIORITY_NORMAL)
		events := durableUntil(t, w, func(e *pb.SessionEvent) bool {
			return e.GetKind() == pb.SessionEvent_KIND_RESULT || e.GetKind() == pb.SessionEvent_KIND_TURN_STOPPED
		})
		if got := strings.Join(kinds(events), " "); got != "INPUT_ACCEPTED TURN_STARTED USER ASSISTANT RESULT" || eventString(t, events[4], "result") != "Next turn done." {
			t.Fatalf("turn after the interrupt: %s", got)
		}
		if state := sess.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_COMPLETED); state.GetTurnCount() != turns {
			t.Fatalf("after the interrupt: %v", state)
		}
		checkStream(t, w, true)
	}
	until := func(w *sessionWatch, kind pb.SessionEvent_Kind) []*pb.SessionEvent {
		t.Helper()
		return durableUntil(t, w, func(e *pb.SessionEvent) bool { return e.GetKind() == kind })
	}

	sess := s.open("")
	w := sess.watch(0)
	logOnFailure(t, w)
	w.expect(pb.SessionEvent_KIND_UNSPECIFIED)
	sess.send("stream then interrupt", pb.InputPriority_INPUT_PRIORITY_NORMAL)
	streamed(t, w, streaming)
	interrupt(sess)
	until(w, pb.SessionEvent_KIND_TURN_STOPPED)
	events := w.durable
	compareBeforeNext(t, "interrupt", w)
	checkAbortedResult(t, events[5], "error_during_execution", "aborted_streaming", true)
	if !strings.Contains(events[4].GetPayloadJson(), "[Request interrupted by user]") {
		t.Fatalf("interrupt note %v", events[4])
	}
	sess.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_STOPPED)
	if r := s.waitFake(func(r *fakemodel.Request) bool {
		return r.Contains("stream then interrupt") && r.Outcome != fakemodel.Pending
	}); r.Outcome != fakemodel.ClientGone {
		t.Fatalf("interrupted stream ended %v", r.Outcome)
	}
	next(sess, w, 2)

	sess = s.open("")
	w = sess.watch(0)
	logOnFailure(t, w)
	w.expect(pb.SessionEvent_KIND_UNSPECIFIED)
	sess.send("run forever", pb.InputPriority_INPUT_PRIORITY_NORMAL)
	waitFile(t, filepath.Join(s.dir, "started.txt"))
	pid, err := strconv.Atoi(strings.TrimSpace(readFile(t, filepath.Join(s.dir, "pid.txt"))))
	must(t, err)
	if !processAlive(pid, "") {
		t.Fatalf("Bash %d is not running", pid)
	}
	interrupt(sess)
	until(w, pb.SessionEvent_KIND_TURN_STOPPED)
	events = w.durable
	compareBeforeNext(t, "interrupt_tool", w)
	checkToolPair(t, events[3], events[4], "Bash", true)
	checkResult(t, events[6], "error_during_execution", "aborted_tools", true)
	sess.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_STOPPED)
	wait(t, 10*time.Second, func() bool { return !processAlive(pid, "") })
	next(sess, w, 2)

	sess = s.open("")
	w = sess.watch(0)
	logOnFailure(t, w)
	w.expect(pb.SessionEvent_KIND_UNSPECIFIED)
	sess.send("stream then stop", pb.InputPriority_INPUT_PRIORITY_NORMAL)
	streamed(t, w, streaming)
	cli := s.liveCLI(sess.id)
	_, err = sess.api.StopSession(sess.ctx, connect.NewRequest(&pb.StopSessionRequest{SessionId: sess.id}))
	must(t, err)
	until(w, pb.SessionEvent_KIND_TURN_INTERRUPTED)
	compareWithFakeWorker(t, "stop", w)
	checkStream(t, w, false)
	sess.state(pb.Session_STATE_STOPPED, pb.TurnOutcome_TURN_OUTCOME_INTERRUPTED)
	wait(t, 10*time.Second, func() bool {
		lease, err := s.h.st.GetLease(sess.ctx, sess.id)
		must(t, err)
		return lease.WorkerID == "" && !processAlive(cli.PID, cli.Start)
	})
	log.check(t)
}

// TestClaudeApproval: auto mode's check flags a Bash command, the worker
// asks for approval, and after the user approves the model's retry runs
// once although the check flags it again (the PreToolUse hook allows the
// approved call).
func TestClaudeApproval(t *testing.T) {
	var log scriptLog
	const command = "printf 'ran\\n' >> approved.txt; echo approved"
	const why = "Scripted approval required"
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
				return &fakemodel.Response{ToolUses: []fakemodel.ToolUse{fakemodel.Bash("toolu_deny", "touch denied.txt")}, Flagged: map[string]string{"toolu_deny": why}}
			case 1:
				return fakemodel.Text("I need approval to run this Bash command.")
			}
			return log.fail("unexpected step %d: %.500s", r.Assistants(), r.Last().Text())
		case !r.Contains("needs approval"):
			return log.fail("unexpected request: %.300s", r.Last().Text())
		}
		switch r.Assistants() {
		case 0:
			return &fakemodel.Response{ToolUses: []fakemodel.ToolUse{fakemodel.Bash("toolu_denied", command)}, Flagged: map[string]string{"toolu_denied": why}}
		case 1:
			if b, ok := r.ToolResult("toolu_denied"); !ok || !b.IsError || !strings.Contains(b.ResultText(), why) {
				return log.fail("denial result %+v", b)
			}
			return fakemodel.Text("I need approval to run this Bash command.")
		case 2:
			if !r.Contains("The user approved") {
				return log.fail("approval input missing: %.500s", r.Last().Text())
			}
			return &fakemodel.Response{ToolUses: []fakemodel.ToolUse{fakemodel.Bash("toolu_retry", command)}, Flagged: map[string]string{"toolu_retry": why}}
		case 3:
			if b, ok := r.ToolResult("toolu_retry"); !ok || b.IsError || !strings.Contains(b.ResultText(), "approved") {
				return log.fail("retry result %+v", b)
			}
			return fakemodel.Text("Bash finished.")
		}
		return log.fail("unexpected step %d", r.Assistants())
	}
	s := newClaudeStack(t, fakemodel.Options{Main: main})
	s.startWorker()
	sess := s.open("")
	w := sess.watch(0)
	logOnFailure(t, w)
	w.expect(pb.SessionEvent_KIND_UNSPECIFIED)
	sess.send("needs approval", pb.InputPriority_INPUT_PRIORITY_NORMAL)
	events := durableUntil(t, w, func(e *pb.SessionEvent) bool { return e.GetKind() == pb.SessionEvent_KIND_RESULT })
	sess.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_COMPLETED)
	var pending *pb.SessionEvent
	for _, e := range events {
		if e.GetKind() == pb.SessionEvent_KIND_APPROVAL_PENDING {
			pending = e
		}
	}
	if pending == nil || eventString(t, pending, "display") != command || eventString(t, pending, "tool_use_id") != "toolu_denied" {
		t.Fatalf("approval card %v", pending)
	}
	if _, err := os.Stat(filepath.Join(s.dir, "approved.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the flagged command ran: %v", err)
	}
	list, err := sess.api.ListApprovals(sess.ctx, connect.NewRequest(&pb.ListApprovalsRequest{SessionId: sess.id}))
	must(t, err)
	if len(list.Msg.GetApprovals()) != 1 || list.Msg.GetApprovals()[0].GetApprovalId() != eventString(t, pending, "approval_id") {
		t.Fatalf("pending approvals %v", list.Msg)
	}
	_, err = sess.api.ResolveApproval(sess.ctx, connect.NewRequest(&pb.ResolveApprovalRequest{ApprovalId: eventString(t, pending, "approval_id"), Approve: true}))
	must(t, err)
	durableUntil(t, w, func(e *pb.SessionEvent) bool { return e.GetKind() == pb.SessionEvent_KIND_RESULT })
	if state := sess.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_COMPLETED); state.GetTurnCount() != 2 || state.GetPendingApprovalCount() != 0 {
		t.Fatalf("after approval: %v", state)
	}
	if got := readFile(t, filepath.Join(s.dir, "approved.txt")); got != "ran\n" {
		t.Fatalf("the approved command ran %q", got)
	}
	checkStream(t, w, true)
	compareWithFakeWorker(t, "approve", w)

	// A denied approval: no retry, the session takes the next input.
	sess = s.open("")
	w = sess.watch(0)
	logOnFailure(t, w)
	w.expect(pb.SessionEvent_KIND_UNSPECIFIED)
	sess.send("deny me", pb.InputPriority_INPUT_PRIORITY_NORMAL)
	durableUntil(t, w, func(e *pb.SessionEvent) bool { return e.GetKind() == pb.SessionEvent_KIND_RESULT })
	sess.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_COMPLETED)
	pending = nil
	for _, e := range w.durable {
		if e.GetKind() == pb.SessionEvent_KIND_APPROVAL_PENDING {
			pending = e
		}
	}
	if pending == nil {
		t.Fatal("no approval card for the denied command")
	}
	_, err = sess.api.ResolveApproval(sess.ctx, connect.NewRequest(&pb.ResolveApprovalRequest{ApprovalId: eventString(t, pending, "approval_id"), Approve: false}))
	must(t, err)
	resolved := durableUntil(t, w, func(e *pb.SessionEvent) bool { return e.GetKind() == pb.SessionEvent_KIND_APPROVAL_RESOLVED })
	if eventString(t, resolved[len(resolved)-1], "state") != "denied" {
		t.Fatalf("resolution %v", resolved)
	}
	compareBeforeNext(t, "deny", w)
	checkStream(t, w, true)
	if state := sess.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_COMPLETED); state.GetTurnCount() != 1 || state.GetPendingApprovalCount() != 0 {
		t.Fatalf("after the denial: %v", state)
	}
	sess.send("still ready", pb.InputPriority_INPUT_PRIORITY_NORMAL)
	events = durableUntil(t, w, func(e *pb.SessionEvent) bool { return e.GetKind() == pb.SessionEvent_KIND_RESULT })
	if got := strings.Join(kinds(events), " "); got != "INPUT_ACCEPTED TURN_STARTED USER ASSISTANT RESULT" || eventString(t, events[4], "result") != "Still ready." {
		t.Fatalf("turn after the denial: %s", got)
	}
	if _, err := os.Stat(filepath.Join(s.dir, "denied.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the denied command ran: %v", err)
	}
	log.check(t)
}

const previousTurnNote = "The previous turn was interrupted before it finished"

// TestClaudeWorkerKilled: the worker is killed with SIGKILL while one
// session's model call streams and another session sits idle. Claude Code
// dies with it (exec-guard) and makes no further model call; a new worker
// process takes the sessions in a new epoch, resuming each from the stored
// transcript. The interrupted session's next turn tells the model about the
// interruption; nothing of either transcript is duplicated.
func TestClaudeWorkerKilled(t *testing.T) {
	var log scriptLog
	held := fakemodel.NewGate()
	var heldOnce atomic.Bool
	main := func(r *fakemodel.Request) *fakemodel.Response {
		switch {
		case r.Contains("after the crash"):
			if !r.Contains(previousTurnNote) || r.Count("held when killed") != 1 || r.Count("after the crash") != 1 {
				return log.fail("resumed request after the crash: %.3000s", r.Text())
			}
			return fakemodel.Text("Resumed after the crash.")
		case r.Contains("held when killed"):
			if heldOnce.CompareAndSwap(false, true) {
				return &fakemodel.Response{Text: "This answer is cut off by the kill.", Hold: held}
			}
			return log.fail("held input again: %.3000s", r.Text())
		case r.Contains("idle session, again"):
			if r.Contains(previousTurnNote) || r.Count("idle session, first") != 1 || r.Count("First answer.") != 1 {
				return log.fail("resumed idle session: %.3000s", r.Text())
			}
			return fakemodel.Text("Second answer.")
		case r.Contains("idle session, first"):
			return fakemodel.Text("First answer.")
		}
		return log.fail("unexpected request: %.300s", r.Last().Text())
	}
	s := newClaudeStack(t, fakemodel.Options{Main: main})
	first := s.startWorker()

	idle := s.open("")
	idleWatch := idle.watch(0)
	logOnFailure(t, idleWatch)
	idleWatch.expect(pb.SessionEvent_KIND_UNSPECIFIED)
	idle.send("idle session, first", pb.InputPriority_INPUT_PRIORITY_NORMAL)
	durableUntil(t, idleWatch, func(e *pb.SessionEvent) bool { return e.GetKind() == pb.SessionEvent_KIND_RESULT })
	idle.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_COMPLETED)

	sess := s.open("")
	w := sess.watch(0)
	logOnFailure(t, w)
	w.expect(pb.SessionEvent_KIND_UNSPECIFIED)
	heldInput := sess.send("held when killed", pb.InputPriority_INPUT_PRIORITY_NORMAL)
	// Mid-turn: the turn's events are published (the model's answer is
	// streaming) and its input is in the stored transcript, so the lost
	// lease counts it as persisted rather than handing it out again.
	streamed(t, w, held)
	s.waitTranscript(sess.id, func(e transcriptEntry) bool { return e.UUID == heldInput.GetInputId() })
	guard := s.liveCLI(sess.id)
	idleGuard := s.liveCLI(idle.id)
	cli, cliStart := s.guardCommand(guard)
	idleCLI, idleStart := s.guardCommand(idleGuard)
	before, err := s.h.st.GetLease(s.ctx, sess.id)
	must(t, err)

	if strings.Contains(first.stderr.String(), "exec-guard is unavailable") {
		t.Fatalf("the worker runs Claude Code without the exec guard: %s", first.stderr.String())
	}
	first.signal(t, syscall.SIGKILL)
	if err := first.wait(t, 5*time.Second); err == nil {
		t.Fatal("killed worker exited cleanly")
	}
	reapExited(t, guard.PID, guard.Start, 10*time.Second)
	reapExited(t, idleGuard.PID, idleGuard.Start, 10*time.Second)
	requireReaped(t, cli, cliStart)
	requireReaped(t, idleCLI, idleStart)
	cut := s.waitFake(func(r *fakemodel.Request) bool {
		return r.Contains("held when killed") && r.Outcome != fakemodel.Pending
	})
	if cut.Outcome != fakemodel.ClientGone {
		t.Fatalf("the held stream ended %v", cut.Outcome)
	}
	// Both Claude Code processes are gone: nothing calls the model again
	// until the next input (checked below, after the new worker took over).
	requests := len(s.fake.Requests())
	s.startWorker()
	durableUntil(t, w, func(e *pb.SessionEvent) bool { return e.GetKind() == pb.SessionEvent_KIND_TURN_INTERRUPTED })
	sess.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_INTERRUPTED)
	// The new worker removed the dead worker's records of its processes.
	wait(t, 10*time.Second, func() bool {
		for _, r := range s.cliRecords() {
			if r.Worker.PID == first.cmd.Process.Pid {
				return false
			}
		}
		return true
	})

	if n := len(s.fake.Requests()); n != requests {
		t.Fatalf("%d model requests after the kill, before any input", n-requests)
	}
	sess.send("after the crash", pb.InputPriority_INPUT_PRIORITY_NORMAL)
	durableUntil(t, w, func(e *pb.SessionEvent) bool { return e.GetKind() == pb.SessionEvent_KIND_RESULT })
	if note := w.durable[6]; eventString(t, note, "circus_note") != "previous_turn_interrupted" {
		t.Fatalf("previous-turn note %v", note)
	}
	sess.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_COMPLETED)
	after, err := s.h.st.GetLease(s.ctx, sess.id)
	must(t, err)
	if after.Epoch <= before.Epoch || after.BootID == before.BootID {
		t.Fatalf("lease before the kill %+v, after %+v", before, after)
	}
	s.checkTranscript(sess.id)
	compareWithFakeWorker(t, "crash_recover", w)

	idle.send("idle session, again", pb.InputPriority_INPUT_PRIORITY_NORMAL)
	events := durableUntil(t, idleWatch, func(e *pb.SessionEvent) bool { return e.GetKind() == pb.SessionEvent_KIND_RESULT })
	if got := strings.Join(kinds(events), " "); got != "INPUT_ACCEPTED TURN_STARTED USER ASSISTANT RESULT" {
		t.Fatalf("idle session after the kill: %s", got)
	}
	idle.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_COMPLETED)
	s.checkTranscript(idle.id)
	log.check(t)
}

// TestClaudeWorkerKilledDuringBash covers a tool shell in a different
// session from the CLI. Both must disappear before a new epoch takes over.
func TestClaudeWorkerKilledDuringBash(t *testing.T) {
	var log scriptLog
	var issued atomic.Bool
	main := func(r *fakemodel.Request) *fakemodel.Response {
		switch {
		case r.Contains("after the Bash crash"):
			if !r.Contains(previousTurnNote) || r.Count("run Bash before crashing") != 1 {
				return log.fail("resumed request after the Bash crash: %.3000s", r.Text())
			}
			return fakemodel.Text("Resumed after the Bash crash.")
		case r.Contains("run Bash before crashing") && issued.CompareAndSwap(false, true):
			return fakemodel.Tools(fakemodel.Bash("toolu_crash", `
				printf '%s\n' "$$" > bash.pid.tmp
				mv bash.pid.tmp bash.pid
				for i in $(seq 1200); do sleep 0.05; done
			`))
		}
		return log.fail("unexpected request: %.300s", r.Last().Text())
	}
	s := newClaudeStack(t, fakemodel.Options{Main: main})
	first := s.startWorker()
	sess := s.open("")
	w := sess.watch(0)
	logOnFailure(t, w)
	w.expect(pb.SessionEvent_KIND_UNSPECIFIED)
	input := sess.send("run Bash before crashing", pb.InputPriority_INPUT_PRIORITY_NORMAL)
	s.waitTranscript(sess.id, func(e transcriptEntry) bool { return e.UUID == input.GetInputId() })
	guard := s.liveCLI(sess.id)
	before, err := s.h.st.GetLease(s.ctx, sess.id)
	must(t, err)

	path := filepath.Join(s.dir, "bash.pid")
	waitFile(t, path)
	bash, err := strconv.Atoi(strings.TrimSpace(readFile(t, path)))
	must(t, err)
	bashFields := processFields(bash)
	if len(bashFields) <= 19 || bashFields[0] == "Z" || bashFields[3] != strconv.Itoa(bash) || bash == guard.PID {
		t.Fatalf("Bash %d does not lead a separate live session: %v", bash, bashFields)
	}
	cli, cliStart := s.guardCommand(guard)
	first.signal(t, syscall.SIGKILL)
	if err := first.wait(t, 5*time.Second); err == nil {
		t.Fatal("killed worker exited cleanly")
	}
	reapExited(t, guard.PID, guard.Start, 5*time.Second)
	// The guard must reap the CLI and the Bash shell before it exits.
	requireReaped(t, cli, cliStart)
	requireReaped(t, bash, bashFields[19])

	s.startWorker()
	durableUntil(t, w, func(e *pb.SessionEvent) bool { return e.GetKind() == pb.SessionEvent_KIND_TURN_INTERRUPTED })
	sess.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_INTERRUPTED)
	sess.send("after the Bash crash", pb.InputPriority_INPUT_PRIORITY_NORMAL)
	durableUntil(t, w, func(e *pb.SessionEvent) bool { return e.GetKind() == pb.SessionEvent_KIND_RESULT })
	sess.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_COMPLETED)
	after, err := s.h.st.GetLease(s.ctx, sess.id)
	must(t, err)
	if after.Epoch <= before.Epoch || after.BootID == before.BootID {
		t.Fatalf("lease before the kill %+v, after %+v", before, after)
	}
	s.checkTranscript(sess.id)
	log.check(t)
}

// TestClaudeControlPlaneRestart: the control plane restarts while the model
// streams; the worker resumes its lease (same epoch, new connection) and the
// turn completes once, with nothing of the transcript duplicated.
func TestClaudeControlPlaneRestart(t *testing.T) {
	var log scriptLog
	held := fakemodel.NewGate()
	main := func(r *fakemodel.Request) *fakemodel.Response {
		switch {
		case r.Contains("after the restart"):
			if r.Count("held across the restart") != 1 || r.Count("Finished across the restart.") != 1 {
				return log.fail("request after the restart: %.3000s", r.Text())
			}
			return fakemodel.Text("Still here.")
		case r.Contains("held across the restart"):
			return &fakemodel.Response{Text: "Finished across the restart.", Hold: held}
		}
		return log.fail("unexpected request: %.300s", r.Last().Text())
	}
	s := newClaudeStack(t, fakemodel.Options{Main: main})
	wk := s.startWorker()
	sess := s.open("")
	w := sess.watch(0)
	logOnFailure(t, w)
	w.expect(pb.SessionEvent_KIND_UNSPECIFIED)
	sess.send("held across the restart", pb.InputPriority_INPUT_PRIORITY_NORMAL)
	streamed(t, w, held)
	before, err := s.h.st.GetLease(s.ctx, sess.id)
	must(t, err)
	must(t, w.stream.Close())

	s.h.server.signal(t, syscall.SIGTERM)
	must(t, s.h.server.wait(t, 10*time.Second))
	s.h.startServer(false)
	wait(t, 20*time.Second, func() bool {
		lease, err := s.h.st.GetLease(s.ctx, sess.id)
		must(t, err)
		return lease.ConnID != "" && lease.ConnID != before.ConnID
	})
	resumed, err := s.h.st.GetLease(s.ctx, sess.id)
	must(t, err)
	if resumed.Epoch != before.Epoch || resumed.BootID != before.BootID {
		t.Fatalf("lease before the restart %+v, after %+v", before, resumed)
	}
	held.Release()
	w = sess.watch(w.seq)
	logOnFailure(t, w)
	events := durableUntil(t, w, func(e *pb.SessionEvent) bool { return e.GetKind() == pb.SessionEvent_KIND_RESULT })
	if got := strings.Join(kinds(events), " "); got != "ASSISTANT RESULT" || eventString(t, events[1], "result") != "Finished across the restart." {
		t.Fatalf("events after the restart: %s %v", got, events)
	}
	if state := sess.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_COMPLETED); state.GetTurnCount() != 1 {
		t.Fatalf("after the restart: %v", state)
	}
	sess.send("after the restart", pb.InputPriority_INPUT_PRIORITY_NORMAL)
	events = durableUntil(t, w, func(e *pb.SessionEvent) bool { return e.GetKind() == pb.SessionEvent_KIND_RESULT })
	if eventString(t, events[len(events)-1], "result") != "Still here." {
		t.Fatalf("turn after the restart: %v", events)
	}
	s.checkTranscript(sess.id)
	if n := len(s.fake.MainRequests()); n != 2 {
		t.Fatalf("%d main model requests, want 2", n)
	}
	// Stop the worker while the restarted control plane still runs, so it
	// releases its lease instead of being killed at cleanup.
	wk.signal(t, syscall.SIGTERM)
	must(t, wk.wait(t, 20*time.Second))
	if lease, err := s.h.st.GetLease(s.ctx, sess.id); err != nil || lease.WorkerID != "" {
		t.Fatalf("lease after the worker's shutdown: %+v, %v", lease, err)
	}
	log.check(t)
}

// TestClaudeIdleRelease: an idle session's lease is released (here after a
// second), and the next input acquires it again in a new epoch: Claude Code
// resumes the conversation from the stored transcript.
func TestClaudeIdleRelease(t *testing.T) {
	var log scriptLog
	answering := fakemodel.NewGate()
	main := func(r *fakemodel.Request) *fakemodel.Response {
		switch {
		case r.Contains("after the release"):
			if r.Count("first input, kept") != 1 || r.Count("Answer before the release.") != 1 {
				return log.fail("request after the release: %.3000s", r.Text())
			}
			return fakemodel.Text("Answer after the release.")
		case r.Contains("first input, kept"):
			return &fakemodel.Response{Text: "Answer before the release.", Hold: answering}
		}
		return log.fail("unexpected request: %.300s", r.Last().Text())
	}
	s := newClaudeStack(t, fakemodel.Options{Main: main}, "CIRCUS_WORKER_IDLE_SECONDS=1")
	s.startWorker()
	sess := s.open("")
	w := sess.watch(0)
	logOnFailure(t, w)
	w.expect(pb.SessionEvent_KIND_UNSPECIFIED)
	sess.send("first input, kept", pb.InputPriority_INPUT_PRIORITY_NORMAL)
	// Observed while the answer is held: the idle timer starts only after it.
	streamed(t, w, answering)
	cli := s.liveCLI(sess.id)
	held, err := s.h.st.GetLease(s.ctx, sess.id)
	must(t, err)
	answering.Release()
	durableUntil(t, w, func(e *pb.SessionEvent) bool { return e.GetKind() == pb.SessionEvent_KIND_RESULT })
	sess.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_COMPLETED)
	var released store.Lease
	wait(t, 20*time.Second, func() bool {
		released, err = s.h.st.GetLease(s.ctx, sess.id)
		must(t, err)
		return released.WorkerID == "" && !processAlive(cli.PID, cli.Start)
	})
	if released.Epoch != held.Epoch {
		t.Fatalf("lease held %+v, released %+v", held, released)
	}
	sess.send("after the release", pb.InputPriority_INPUT_PRIORITY_NORMAL)
	events := durableUntil(t, w, func(e *pb.SessionEvent) bool { return e.GetKind() == pb.SessionEvent_KIND_RESULT })
	if got := strings.Join(kinds(events), " "); got != "INPUT_ACCEPTED TURN_STARTED USER ASSISTANT RESULT" || eventString(t, events[4], "result") != "Answer after the release." {
		t.Fatalf("turn after the release: %s %v", got, events)
	}
	if state := sess.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_COMPLETED); state.GetTurnCount() != 2 {
		t.Fatalf("after the release: %v", state)
	}
	again, err := s.h.st.GetLease(s.ctx, sess.id)
	must(t, err)
	if again.Epoch <= held.Epoch || again.BootID != held.BootID {
		t.Fatalf("lease before the release %+v, after %+v", held, again)
	}
	s.checkTranscript(sess.id)
	log.check(t)
}

// TestClaudeBackgroundTask: a background Bash task ends while the session
// is idle; Claude Code starts a turn by itself to tell the model (origin
// cli), after the task's end is published.
func TestClaudeBackgroundTask(t *testing.T) {
	var log scriptLog
	main := func(r *fakemodel.Request) *fakemodel.Response {
		if !r.Contains("start a background job") {
			return log.fail("unexpected request: %.300s", r.Last().Text())
		}
		if n := r.Notifications(); len(n) > 0 {
			if len(n) != 1 || !strings.Contains(n[0], "toolu_bg") {
				return log.fail("notifications %q", n)
			}
			return fakemodel.Text("Background finished.")
		}
		switch r.Assistants() {
		case 0:
			return fakemodel.Tools(fakemodel.Use("toolu_bg", "Bash", map[string]any{
				"command":           "for i in $(seq 1200); do [ -e bg-go.txt ] && break; sleep 0.05; done; echo bg-done",
				"description":       "Wait for bg-go.txt",
				"run_in_background": true,
			}))
		case 1:
			return fakemodel.Text("Started it.")
		}
		return log.fail("unexpected step %d: %.1000s", r.Assistants(), r.Last().Text())
	}
	s := newClaudeStack(t, fakemodel.Options{Main: main})
	s.startWorker()
	sess := s.open("")
	w := sess.watch(0)
	logOnFailure(t, w)
	w.expect(pb.SessionEvent_KIND_UNSPECIFIED)
	sess.send("start a background job", pb.InputPriority_INPUT_PRIORITY_NORMAL)
	first := durableUntil(t, w, func(e *pb.SessionEvent) bool { return e.GetKind() == pb.SessionEvent_KIND_RESULT })
	sess.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_COMPLETED)
	must(t, os.WriteFile(filepath.Join(s.dir, "bg-go.txt"), nil, 0o600))
	second := durableUntil(t, w, func(e *pb.SessionEvent) bool { return e.GetKind() == pb.SessionEvent_KIND_RESULT })
	if got := strings.Join(kinds(first), " "); got != "INPUT_ACCEPTED TURN_STARTED USER ASSISTANT BGTASK_STARTED USER ASSISTANT RESULT" {
		t.Fatalf("turn starting the task: %s", got)
	}
	if got := strings.Join(kinds(second), " "); got != "BGTASK_ENDED TURN_STARTED ASSISTANT RESULT" {
		t.Fatalf("turn after the task: %s", got)
	}
	taskID := eventString(t, first[4], "task_id")
	if eventString(t, second[0], "task_id") != taskID || eventString(t, second[0], "status") != "completed" || eventString(t, second[0], "tool_use_id") != "toolu_bg" {
		t.Fatalf("task end %v, start %v", second[0], first[4])
	}
	if eventString(t, second[1], "origin") != "cli" || eventString(t, second[1], "turn_no") != "2" || eventString(t, second[3], "result") != "Background finished." {
		t.Fatalf("CLI turn %v / %v", second[1], second[3])
	}
	if state := sess.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_COMPLETED); state.GetTurnCount() != 2 {
		t.Fatalf("after the CLI turn: %v", state)
	}
	checkStream(t, w, true)
	s.checkTranscript(sess.id)
	log.check(t)
}

// TestClaudeMissingDirectory: a session whose working directory is gone
// when the worker takes it gets an ERROR event, published before the lease
// is released with ERROR; no Claude Code starts.
func TestClaudeMissingDirectory(t *testing.T) {
	s := newClaudeStack(t, fakemodel.Options{Main: func(r *fakemodel.Request) *fakemodel.Response {
		return &fakemodel.Response{Status: 400, ErrorType: "invalid_request_error", Error: "no model call expected"}
	}})
	sess := s.open("")
	w := sess.watch(0)
	logOnFailure(t, w)
	w.expect(pb.SessionEvent_KIND_UNSPECIFIED)
	must(t, os.Remove(s.dir))
	s.startWorker()
	sess.send("nowhere to run", pb.InputPriority_INPUT_PRIORITY_NORMAL)
	events := durableUntil(t, w, func(e *pb.SessionEvent) bool { return e.GetKind() == pb.SessionEvent_KIND_ERROR })
	if got := strings.Join(kinds(events), " "); got != "INPUT_ACCEPTED ERROR" ||
		!strings.Contains(eventString(t, events[1], "message"), "is not a directory on this machine") {
		t.Fatalf("events: %s %v", got, events)
	}
	wait(t, 10*time.Second, func() bool {
		lease, err := s.h.st.GetLease(s.ctx, sess.id)
		must(t, err)
		return lease.Epoch > 0 && lease.WorkerID == ""
	})
	if n := len(s.fake.Requests()); n != 0 || len(s.cliRecords()) != 0 {
		t.Fatalf("%d model requests, CLI records %v", n, s.cliRecords())
	}
}
