//go:build e2e && linux

package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/gen/go/circus/v1/circusv1connect"
	"github.com/EricWvi/circus/internal/clock"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/testutil/fakeworker"
	testwait "github.com/EricWvi/circus/internal/testutil/wait"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
)

func TestFakeWorkerSessions(t *testing.T) {
	h := newHarness(t)
	api, project := browserSessionAPI(t, h)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	gate := interruptClock{entered: make(chan struct{}, 1), release: make(chan struct{})}
	go func() {
		done <- fakeworker.Run(ctx, fakeworker.Options{
			Credential: h.worker, MaxSessions: 1, StreamDelay: 40 * time.Millisecond, Clock: gate,
			Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		})
	}()
	t.Cleanup(func() {
		cancel()
		if err := testwait.Recv(t, done); !errors.Is(err, context.Canceled) {
			t.Errorf("fake worker exited: %v", err)
		}
	})

	open := func(t *testing.T) *sessionBrowser {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
		t.Cleanup(cancel)
		res, err := api.CreateSession(ctx, connect.NewRequest(&pb.CreateSessionRequest{ProjectId: project}))
		must(t, err)
		s := &sessionBrowser{t: t, ctx: ctx, api: api, id: res.Msg.GetSession().GetSessionId()}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), testwait.Timeout)
			defer cancel()
			_, err := api.StopSession(ctx, connect.NewRequest(&pb.StopSessionRequest{SessionId: s.id}))
			if connect.CodeOf(err) != connect.CodeNotFound {
				must(t, err)
			}
		})
		return s
	}

	t.Run("stream and replay", func(t *testing.T) {
		s := open(t)
		watch := s.watch(0)
		watch.expect(pb.SessionEvent_KIND_UNSPECIFIED)
		sent := s.send("Hello, **worker**", pb.InputPriority_INPUT_PRIORITY_NORMAL)
		accepted := watch.expect(pb.SessionEvent_KIND_INPUT_ACCEPTED)
		if accepted.GetSeq() != sent.GetEventSeq() {
			t.Fatalf("SendMessage seq %d, WatchSession %v", sent.GetEventSeq(), accepted)
		}
		started := watch.expect(pb.SessionEvent_KIND_TURN_STARTED)
		delivered := watch.expect(pb.SessionEvent_KIND_USER)
		if eventString(t, delivered, "uuid") != sent.GetInputId() {
			t.Fatalf("input UUID %q, delivered %v", sent.GetInputId(), delivered)
		}
		var chunks strings.Builder
		n := 0
		for e := watch.next(); ; e = watch.next() {
			if !e.GetEphemeral() {
				if e.GetKind() != pb.SessionEvent_KIND_ASSISTANT || !strings.Contains(e.GetPayloadJson(), "> Hello, **worker**") {
					t.Fatalf("assistant event %v", e)
				}
				break
			}
			var frame struct {
				Type  string
				Event struct {
					Delta struct{ Type, Text string }
				}
			}
			must(t, json.Unmarshal([]byte(e.GetPayloadJson()), &frame))
			if frame.Type == "stream_event" && frame.Event.Delta.Type == "text_delta" {
				n++
				chunks.WriteString(frame.Event.Delta.Text)
			}
		}
		if n < 2 || chunks.String() != "You said:\n\n> Hello, **worker**" {
			t.Fatalf("streamed %d deltas: %q", n, chunks.String())
		}
		watch.expect(pb.SessionEvent_KIND_RESULT)
		s.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_COMPLETED)
		must(t, watch.stream.Close())

		replay := s.watch(started.GetSeq())
		for _, want := range watch.durable[2:] {
			if got := replay.next(); !proto.Equal(got, want) {
				t.Fatalf("replayed %v, want %v", got, want)
			}
		}
		sent = s.send("new live input", pb.InputPriority_INPUT_PRIORITY_NORMAL)
		if got := replay.next(); got.GetKind() != pb.SessionEvent_KIND_INPUT_ACCEPTED || got.GetSeq() != sent.GetEventSeq() {
			t.Fatalf("first event after replay: %v", got)
		}
	})

	t.Run("device tools", func(t *testing.T) {
		s := open(t)
		watch := s.watch(0)
		watch.expect(pb.SessionEvent_KIND_UNSPECIFIED)
		s.send("/fake bash printf 'fake-bash\n'; pwd", pb.InputPriority_INPUT_PRIORITY_NORMAL)
		toolUse := watch.until(contains(pb.SessionEvent_KIND_ASSISTANT, `"type":"tool_use"`))
		result := watch.until(contains(pb.SessionEvent_KIND_USER, `"type":"tool_result"`))
		checkToolPair(t, toolUse, result, "Bash", false, "fake-bash", h.dir)
		watch.expect(pb.SessionEvent_KIND_ASSISTANT)
		watch.expect(pb.SessionEvent_KIND_RESULT)
		s.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_COMPLETED)

		must(t, os.WriteFile(filepath.Join(h.dir, "read.txt"), []byte("from a real file\n"), 0o600))
		s.send("/fake read read.txt", pb.InputPriority_INPUT_PRIORITY_NORMAL)
		toolUse = watch.until(contains(pb.SessionEvent_KIND_ASSISTANT, `"type":"tool_use"`))
		result = watch.until(contains(pb.SessionEvent_KIND_USER, `"type":"tool_result"`))
		checkToolPair(t, toolUse, result, "Read", false, "from a real file")
		watch.expect(pb.SessionEvent_KIND_ASSISTANT)
		watch.expect(pb.SessionEvent_KIND_RESULT)
		s.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_COMPLETED)
	})

	t.Run("approval", func(t *testing.T) {
		s := open(t)
		watch := s.watch(0)
		watch.expect(pb.SessionEvent_KIND_UNSPECIFIED)
		cmd := "/fake approve printf approved-bash > approval.txt; cat approval.txt"
		s.send(cmd, pb.InputPriority_INPUT_PRIORITY_NORMAL)
		toolUse := watch.until(contains(pb.SessionEvent_KIND_ASSISTANT, `"type":"tool_use"`))
		denial := watch.expect(pb.SessionEvent_KIND_PERMISSION_DENIED)
		denied := watch.expect(pb.SessionEvent_KIND_USER)
		checkToolPair(t, toolUse, denied, "Bash", true, "denied by the Claude Code auto mode classifier")
		if eventString(t, denial, "subtype") != "permission_denied" || eventString(t, denial, "tool_use_id") != toolUseID(t, toolUse) {
			t.Fatalf("permission denial: %v", denial)
		}
		pending := watch.expect(pb.SessionEvent_KIND_APPROVAL_PENDING)
		watch.expect(pb.SessionEvent_KIND_ASSISTANT)
		watch.until(contains(pb.SessionEvent_KIND_RESULT, "I need approval"))
		s.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_COMPLETED)
		list, err := api.ListApprovals(s.ctx, connect.NewRequest(&pb.ListApprovalsRequest{SessionId: s.id}))
		must(t, err)
		if len(list.Msg.GetApprovals()) != 1 {
			t.Fatalf("pending approvals: %v", list.Msg)
		}
		id := list.Msg.GetApprovals()[0].GetApprovalId()
		if id != eventString(t, pending, "approval_id") {
			t.Fatalf("approval %q, event %v", id, pending)
		}
		if _, err := os.Stat(filepath.Join(h.dir, "approval.txt")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("Bash ran before the grant: %v", err)
		}
		_, err = api.ResolveApproval(s.ctx, connect.NewRequest(&pb.ResolveApprovalRequest{ApprovalId: id, Approve: true}))
		must(t, err)
		if e := watch.expect(pb.SessionEvent_KIND_APPROVAL_RESOLVED); eventString(t, e, "state") != "approved" {
			t.Fatalf("resolution: %v", e)
		}
		started := watch.expect(pb.SessionEvent_KIND_TURN_STARTED)
		if eventString(t, started, "turn_no") != "2" {
			t.Fatalf("approval must start a retry turn: %v", started)
		}
		if delivered := watch.expect(pb.SessionEvent_KIND_USER); !strings.Contains(delivered.GetPayloadJson(), "The user approved") {
			t.Fatalf("approval input: %v", delivered)
		}
		retry := watch.expect(pb.SessionEvent_KIND_ASSISTANT)
		if toolUseID(t, retry) == toolUseID(t, toolUse) {
			t.Fatal("approval retried with the denied tool-use ID")
		}
		result := watch.expect(pb.SessionEvent_KIND_USER)
		checkToolPair(t, retry, result, "Bash", false, "approved-bash")
		watch.expect(pb.SessionEvent_KIND_ASSISTANT)
		watch.expect(pb.SessionEvent_KIND_RESULT)
		s.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_COMPLETED)
	})

	t.Run("approval joins a running turn", func(t *testing.T) {
		s := open(t)
		watch := s.watch(0)
		watch.expect(pb.SessionEvent_KIND_UNSPECIFIED)
		s.send("/fake approve printf merged-approval", pb.InputPriority_INPUT_PRIORITY_NORMAL)
		original := watch.until(contains(pb.SessionEvent_KIND_ASSISTANT, `"type":"tool_use"`))
		pending := watch.until(contains(pb.SessionEvent_KIND_APPROVAL_PENDING, "merged-approval"))
		watch.until(contains(pb.SessionEvent_KIND_RESULT, "I need approval"))
		s.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_COMPLETED)

		s.send("/fake slow 2", pb.InputPriority_INPUT_PRIORITY_NORMAL)
		watch.expect(pb.SessionEvent_KIND_INPUT_ACCEPTED)
		started := watch.expect(pb.SessionEvent_KIND_TURN_STARTED)
		if eventString(t, started, "turn_no") != "2" {
			t.Fatalf("unexpected active turn: %v", started)
		}
		watch.expect(pb.SessionEvent_KIND_USER)
		watch.until(contains(pb.SessionEvent_KIND_ASSISTANT, `"type":"text_delta"`))
		_, err := api.ResolveApproval(s.ctx, connect.NewRequest(&pb.ResolveApprovalRequest{
			ApprovalId: eventString(t, pending, "approval_id"), Approve: true,
		}))
		must(t, err)
		watch.expect(pb.SessionEvent_KIND_APPROVAL_RESOLVED)
		watch.expect(pb.SessionEvent_KIND_ASSISTANT) // Final block of the slow reply.
		if delivered := watch.expect(pb.SessionEvent_KIND_USER); !strings.Contains(delivered.GetPayloadJson(), "The user approved") {
			t.Fatalf("expected approval input within the active turn: %v", delivered)
		}
		retry := watch.expect(pb.SessionEvent_KIND_ASSISTANT)
		if toolUseID(t, retry) == toolUseID(t, original) {
			t.Fatal("approval retried with the denied tool-use ID")
		}
		checkToolPair(t, retry, watch.expect(pb.SessionEvent_KIND_USER), "Bash", false, "merged-approval")
		watch.expect(pb.SessionEvent_KIND_ASSISTANT)
		watch.expect(pb.SessionEvent_KIND_RESULT)
		if state := s.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_COMPLETED); state.GetTurnCount() != 2 {
			t.Fatalf("approval created a separate turn: %v", state)
		}
	})

	t.Run("denied approval stays idle", func(t *testing.T) {
		s := open(t)
		watch := s.watch(0)
		watch.expect(pb.SessionEvent_KIND_UNSPECIFIED)
		s.send("/fake approve touch denied.txt", pb.InputPriority_INPUT_PRIORITY_NORMAL)
		pending := watch.until(contains(pb.SessionEvent_KIND_APPROVAL_PENDING, "denied.txt"))
		watch.until(contains(pb.SessionEvent_KIND_RESULT, "I need approval"))
		s.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_COMPLETED)
		_, err := api.ResolveApproval(s.ctx, connect.NewRequest(&pb.ResolveApprovalRequest{
			ApprovalId: eventString(t, pending, "approval_id"), Approve: false,
		}))
		must(t, err)
		if e := watch.expect(pb.SessionEvent_KIND_APPROVAL_RESOLVED); eventString(t, e, "state") != "denied" {
			t.Fatalf("denial resolution: %v", e)
		}
		state := s.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_COMPLETED)
		if state.GetTurnCount() != 1 || state.GetPendingApprovalCount() != 0 {
			t.Fatalf("state after denial: %v", state)
		}
		if _, err := os.Stat(filepath.Join(h.dir, "denied.txt")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("denied Bash ran: %v", err)
		}
		next := s.send("still ready", pb.InputPriority_INPUT_PRIORITY_NORMAL)
		watch.expect(pb.SessionEvent_KIND_INPUT_ACCEPTED)
		if e := watch.expect(pb.SessionEvent_KIND_TURN_STARTED); eventString(t, e, "turn_no") != "2" {
			t.Fatalf("unexpected retry: %v", e)
		}
		if e := watch.expect(pb.SessionEvent_KIND_USER); eventString(t, e, "uuid") != next.GetInputId() {
			t.Fatalf("unexpected user input: %v", e)
		}
		watch.until(contains(pb.SessionEvent_KIND_RESULT, "still ready"))
	})

	t.Run("interrupt and now", func(t *testing.T) {
		s := open(t)
		watch := s.watch(0)
		watch.expect(pb.SessionEvent_KIND_UNSPECIFIED)
		s.send("/fake slow 12", pb.InputPriority_INPUT_PRIORITY_NORMAL)
		delta := watch.until(contains(pb.SessionEvent_KIND_ASSISTANT, `"type":"text_delta"`))
		_, err := api.Interrupt(s.ctx, connect.NewRequest(&pb.InterruptRequest{SessionId: s.id}))
		must(t, err)
		partial := watch.expect(pb.SessionEvent_KIND_ASSISTANT)
		checkAbortedText(t, delta, partial)
		if annotation := watch.expect(pb.SessionEvent_KIND_USER); !strings.Contains(annotation.GetPayloadJson(), "[Request interrupted by user]") {
			t.Fatalf("interrupt annotation: %v", annotation)
		}
		checkAbortedResult(t, watch.expect(pb.SessionEvent_KIND_RESULT), "error_during_execution", "aborted_streaming", true)
		boundary := watch.expect(pb.SessionEvent_KIND_TURN_STOPPED)
		if eventString(t, boundary, "turn_no") != "1" {
			t.Fatalf("interrupt boundary: %v", boundary)
		}
		s.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_STOPPED)

		s.send("/fake slow 12", pb.InputPriority_INPUT_PRIORITY_NORMAL)
		delta = watch.until(contains(pb.SessionEvent_KIND_ASSISTANT, `"type":"text_delta"`))
		next := s.send("right now", pb.InputPriority_INPUT_PRIORITY_NOW)
		watch.expect(pb.SessionEvent_KIND_INPUT_ACCEPTED)
		text := checkAbortedText(t, delta, watch.expect(pb.SessionEvent_KIND_ASSISTANT))
		result := watch.expect(pb.SessionEvent_KIND_RESULT)
		checkAbortedResult(t, result, "success", "aborted_streaming", false)
		if eventString(t, result, "result") != text {
			t.Fatalf("NOW result did not preserve %q: %v", text, result)
		}
		boundary = watch.expect(pb.SessionEvent_KIND_TURN_STOPPED)
		if eventString(t, boundary, "turn_no") != "2" {
			t.Fatalf("NOW boundary: %v", boundary)
		}
		watch.expect(pb.SessionEvent_KIND_TURN_STARTED)
		delivered := watch.expect(pb.SessionEvent_KIND_USER)
		if eventString(t, delivered, "uuid") != next.GetInputId() {
			t.Fatalf("replacement input: %v", delivered)
		}
		watch.until(contains(pb.SessionEvent_KIND_RESULT, "right now"))
		s.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_COMPLETED)
	})

	t.Run("interrupt during tool call", func(t *testing.T) {
		s := open(t)
		watch := s.watch(0)
		watch.expect(pb.SessionEvent_KIND_UNSPECIFIED)
		s.send("/fake bash sleep 12", pb.InputPriority_INPUT_PRIORITY_NORMAL)
		use := watch.until(contains(pb.SessionEvent_KIND_ASSISTANT, `"type":"tool_use"`))
		var id string
		testwait.For(t, func() bool {
			rows, err := h.st.ListCalls(s.ctx, store.CallFilter{UserID: h.user(), SessionID: s.id, States: []store.CallState{store.CallDispatched}, Limit: 10})
			must(t, err)
			if len(rows) == 0 {
				return false
			}
			id = rows[0].CallID
			return true
		})
		_, err := api.Interrupt(s.ctx, connect.NewRequest(&pb.InterruptRequest{SessionId: s.id}))
		must(t, err)
		checkToolPair(t, use, watch.expect(pb.SessionEvent_KIND_USER), "Bash", true, "The tool call was interrupted")
		watch.until(contains(pb.SessionEvent_KIND_USER, "[Request interrupted by user]"))
		checkAbortedResult(t, watch.expect(pb.SessionEvent_KIND_RESULT), "error_during_execution", "aborted_tools", true)
		watch.expect(pb.SessionEvent_KIND_TURN_STOPPED)
		row, err := h.st.GetCall(s.ctx, id)
		must(t, err)
		if row.ReturnedAt == 0 || row.PersistedAt == 0 {
			t.Fatalf("interrupted call was not returned and persisted: %+v", row)
		}
	})

	t.Run("crash and new boot", func(t *testing.T) {
		s := open(t)
		watch := s.watch(0)
		watch.expect(pb.SessionEvent_KIND_UNSPECIFIED)
		s.send(strings.Repeat("usage before the crash ", 64), pb.InputPriority_INPUT_PRIORITY_NORMAL)
		costBefore := resultCost(t, watch.until(contains(pb.SessionEvent_KIND_RESULT, "usage before the crash")))
		s.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_COMPLETED)
		s.send("/fake crash", pb.InputPriority_INPUT_PRIORITY_NORMAL)
		watch.until(contains(pb.SessionEvent_KIND_USER, "/fake crash"))
		next := s.send("after the crash", pb.InputPriority_INPUT_PRIORITY_NORMAL)
		watch.expect(pb.SessionEvent_KIND_INPUT_ACCEPTED)
		boundary := watch.expect(pb.SessionEvent_KIND_TURN_INTERRUPTED)
		if eventString(t, boundary, "turn_no") != "2" {
			t.Fatalf("crash boundary: %v", boundary)
		}
		watch.expect(pb.SessionEvent_KIND_TURN_STARTED)
		watch.expect(pb.SessionEvent_KIND_USER) // previous-turn notice
		if e := watch.expect(pb.SessionEvent_KIND_USER); eventString(t, e, "uuid") != next.GetInputId() {
			t.Fatalf("input after the crash: %v", e)
		}
		costAfter := resultCost(t, watch.until(contains(pb.SessionEvent_KIND_RESULT, "after the crash")))
		if costAfter <= costBefore {
			t.Fatalf("cumulative cost failed to advance across worker boots: before %g, after %g", costBefore, costAfter)
		}
		s.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_COMPLETED)
	})

	t.Run("network blip resumes", func(t *testing.T) {
		s := open(t)
		watch := s.watch(0)
		watch.expect(pb.SessionEvent_KIND_UNSPECIFIED)
		s.send("/fake blip", pb.InputPriority_INPUT_PRIORITY_NORMAL)
		watch.expect(pb.SessionEvent_KIND_INPUT_ACCEPTED)
		watch.expect(pb.SessionEvent_KIND_TURN_STARTED)
		watch.expect(pb.SessionEvent_KIND_USER)
		before, err := h.st.GetLease(s.ctx, s.id)
		must(t, err)
		watch.expect(pb.SessionEvent_KIND_ASSISTANT)
		watch.expect(pb.SessionEvent_KIND_RESULT)
		after, err := h.st.GetLease(s.ctx, s.id)
		must(t, err)
		if before.ConnID == "" || after.ConnID == "" || before.ConnID == after.ConnID ||
			before.BootID != after.BootID || before.Epoch != after.Epoch {
			t.Fatalf("blip lease before %+v, after %+v", before, after)
		}
		if state := s.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_COMPLETED); state.GetTurnCount() != 1 {
			t.Fatalf("blip created another turn: %v", state)
		}
	})

	t.Run("stop releases lease", func(t *testing.T) {
		s := open(t)
		watch := s.watch(0)
		watch.expect(pb.SessionEvent_KIND_UNSPECIFIED)
		s.send("/fake slow 12", pb.InputPriority_INPUT_PRIORITY_NORMAL)
		watch.until(contains(pb.SessionEvent_KIND_ASSISTANT, `"type":"text_delta"`))
		_, err := api.StopSession(s.ctx, connect.NewRequest(&pb.StopSessionRequest{SessionId: s.id}))
		must(t, err)
		watch.expect(pb.SessionEvent_KIND_SESSION_STATE)
		watch.expect(pb.SessionEvent_KIND_TURN_INTERRUPTED)
		s.state(pb.Session_STATE_STOPPED, pb.TurnOutcome_TURN_OUTCOME_INTERRUPTED)
		_, err = api.SendMessage(s.ctx, connect.NewRequest(&pb.SendMessageRequest{SessionId: s.id, ClientMsgId: "after-stop", Text: "refuse this"}))
		if connect.CodeOf(err) != connect.CodeFailedPrecondition {
			t.Fatalf("send after stop: %v", err)
		}
		_, err = api.DeleteSession(s.ctx, connect.NewRequest(&pb.DeleteSessionRequest{SessionId: s.id}))
		must(t, err)
	})

	t.Run("mirror failure and recovery", func(t *testing.T) {
		s := open(t)
		watch := s.watch(0)
		watch.expect(pb.SessionEvent_KIND_UNSPECIFIED)
		s.send("/fake break", pb.InputPriority_INPUT_PRIORITY_NORMAL)
		watch.until(contains(pb.SessionEvent_KIND_RESULT, "transcript mirror failure"))
		s.state(pb.Session_STATE_BROKEN, pb.TurnOutcome_TURN_OUTCOME_ERROR)
		next := s.send("/fake error", pb.InputPriority_INPUT_PRIORITY_NORMAL)
		_, err := api.RecoverSession(s.ctx, connect.NewRequest(&pb.RecoverSessionRequest{SessionId: s.id, AcceptTranscriptGap: true}))
		must(t, err)
		watch.expect(pb.SessionEvent_KIND_INPUT_ACCEPTED)
		watch.expect(pb.SessionEvent_KIND_TRANSCRIPT_GAP_ACCEPTED)
		watch.expect(pb.SessionEvent_KIND_TURN_STARTED)
		watch.until(contains(pb.SessionEvent_KIND_USER, "conversation history is missing"))
		if e := watch.expect(pb.SessionEvent_KIND_USER); eventString(t, e, "uuid") != next.GetInputId() {
			t.Fatalf("input after recovery: %v", e)
		}
		watch.until(contains(pb.SessionEvent_KIND_RESULT, "scripted worker error"))
		s.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_ERROR)
		row, err := h.st.GetSession(s.ctx, s.id, time.Now().UnixMilli())
		must(t, err)
		if row.TranscriptGapTurnNo != 0 {
			t.Fatalf("gap mark left after ERROR: %d", row.TranscriptGapTurnNo)
		}
		s.send("after accepting the gap", pb.InputPriority_INPUT_PRIORITY_NORMAL)
		watch.expect(pb.SessionEvent_KIND_INPUT_ACCEPTED)
		watch.expect(pb.SessionEvent_KIND_TURN_STARTED)
		if e := watch.expect(pb.SessionEvent_KIND_USER); strings.Contains(e.GetPayloadJson(), "conversation history is missing") {
			t.Fatalf("repeated gap reminder: %v", e)
		}
		watch.until(contains(pb.SessionEvent_KIND_RESULT, "after accepting the gap"))
		s.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_COMPLETED)
	})

	t.Run("background job", func(t *testing.T) {
		s := open(t)
		watch := s.watch(0)
		watch.expect(pb.SessionEvent_KIND_UNSPECIFIED)
		s.send("/fake job sleep 0.2; printf bg", pb.InputPriority_INPUT_PRIORITY_NORMAL)
		watch.expect(pb.SessionEvent_KIND_INPUT_ACCEPTED)
		watch.expect(pb.SessionEvent_KIND_TURN_STARTED)
		watch.expect(pb.SessionEvent_KIND_USER)
		use := watch.expect(pb.SessionEvent_KIND_ASSISTANT)
		watch.expect(pb.SessionEvent_KIND_USER) // Bash returned the background job.
		watch.expect(pb.SessionEvent_KIND_ASSISTANT)
		watch.expect(pb.SessionEvent_KIND_RESULT)
		watch.expect(pb.SessionEvent_KIND_TURN_STARTED)
		watch.expect(pb.SessionEvent_KIND_USER) // The job notice delivered to the model.
		notice := watch.expect(pb.SessionEvent_KIND_NOTIFICATION_DELIVERED)
		if eventString(t, notice, "job_id") == "" || eventString(t, notice, "call_id") == "" ||
			eventString(t, notice, "output_path") == "" || eventString(t, notice, "device") != "desk" ||
			eventString(t, notice, "tool_use_id") != toolUseID(t, use) || eventString(t, notice, "state") != "exited" {
			t.Fatalf("incomplete job notice: %v", notice)
		}
		var code struct {
			ExitCode *int `json:"exit_code"`
		}
		must(t, json.Unmarshal([]byte(notice.GetPayloadJson()), &code))
		if code.ExitCode == nil || *code.ExitCode != 0 {
			t.Fatalf("expected a successful job: %v", notice)
		}
		watch.expect(pb.SessionEvent_KIND_RESULT)
		if state := s.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_COMPLETED); state.GetTurnCount() != 2 {
			t.Fatalf("missing job notification turn: %v", state)
		}
	})

	t.Run("job nudge survives interrupt", func(t *testing.T) {
		defer close(gate.release)
		s := open(t)
		watch := s.watch(0)
		watch.expect(pb.SessionEvent_KIND_UNSPECIFIED)
		s.send("/fake job while [ ! -e job-go ]; do sleep 0.02; done", pb.InputPriority_INPUT_PRIORITY_NORMAL)
		watch.until(contains(pb.SessionEvent_KIND_RESULT, "started a background job"))
		s.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_COMPLETED)
		s.send("/fake slow 16", pb.InputPriority_INPUT_PRIORITY_NORMAL)
		watch.until(contains(pb.SessionEvent_KIND_ASSISTANT, `"type":"text_delta"`))
		testwait.Recv(t, gate.entered)
		must(t, os.WriteFile(filepath.Join(h.dir, "job-go"), nil, 0o600))
		testwait.For(t, func() bool {
			rows, err := h.st.ListJobs(s.ctx, store.JobFilter{SessionID: s.id, Limit: 1})
			must(t, err)
			return len(rows) == 1 && rows[0].State == store.JobExited
		})
		_, err := api.Interrupt(s.ctx, connect.NewRequest(&pb.InterruptRequest{SessionId: s.id}))
		must(t, err)
		// Keep the actor paused while the link reader receives both hints.
		time.Sleep(100 * time.Millisecond)
		gate.release <- struct{}{}
		watch.until(contains(pb.SessionEvent_KIND_TURN_STOPPED, `"turn_no":"2"`))
		watch.until(contains(pb.SessionEvent_KIND_NOTIFICATION_DELIVERED, `"state":"exited"`))
		watch.expect(pb.SessionEvent_KIND_RESULT)
		if state := s.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_COMPLETED); state.GetTurnCount() != 3 {
			t.Fatalf("missing notification turn after interrupt: %v", state)
		}
	})

	t.Run("thinking and error", func(t *testing.T) {
		s := open(t)
		watch := s.watch(0)
		watch.expect(pb.SessionEvent_KIND_UNSPECIFIED)
		s.send("/fake think", pb.InputPriority_INPUT_PRIORITY_NORMAL)
		thinking := watch.until(func(e *pb.SessionEvent) bool {
			return !e.GetEphemeral() && e.GetKind() == pb.SessionEvent_KIND_ASSISTANT
		})
		if !strings.Contains(thinking.GetPayloadJson(), `"type":"thinking"`) {
			t.Fatalf("thinking block: %v", thinking)
		}
		watch.until(contains(pb.SessionEvent_KIND_ASSISTANT, `"type":"message_delta"`))
		text := watch.expect(pb.SessionEvent_KIND_ASSISTANT)
		if assistantID(t, thinking) != assistantID(t, text) {
			t.Fatalf("thinking and text used different API message IDs")
		}
		watch.until(contains(pb.SessionEvent_KIND_RESULT, "/fake think"))
		s.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_COMPLETED)
		s.send("/fake error", pb.InputPriority_INPUT_PRIORITY_NORMAL)
		watch.until(contains(pb.SessionEvent_KIND_RESULT, `"subtype":"error_during_execution"`))
		s.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_ERROR)
	})
	t.Run("shutdown flushes and releases with capacity one", func(t *testing.T) {
		s := open(t)
		watch := s.watch(0)
		watch.expect(pb.SessionEvent_KIND_UNSPECIFIED)
		s.send("/fake slow 12", pb.InputPriority_INPUT_PRIORITY_NORMAL)
		watch.until(contains(pb.SessionEvent_KIND_ASSISTANT, `"type":"text_delta"`))
		queued := open(t)
		queued.send("waiting for capacity", pb.InputPriority_INPUT_PRIORITY_NORMAL)
		queued.state(pb.Session_STATE_QUEUED, pb.TurnOutcome_TURN_OUTCOME_UNSPECIFIED)
		cancel()
		watch.expect(pb.SessionEvent_KIND_TURN_INTERRUPTED)
		lease, err := h.st.GetLease(s.ctx, s.id)
		must(t, err)
		if lease.WorkerID != "" {
			t.Fatalf("worker left a lease on shutdown: %+v", lease)
		}
	})

}

// interruptClock pauses one slow stream so JOB and INTERRUPT can both
// reach the actor's pending mailbox before it handles them.
type interruptClock struct {
	clock.Real
	entered chan struct{}
	release chan struct{}
}

func (c interruptClock) After(d time.Duration) <-chan time.Time {
	if d == 4*time.Second {
		select {
		case c.entered <- struct{}{}:
		default:
		}
		<-c.release
	}
	return c.Real.After(d)
}

func browserSessionAPI(t *testing.T, h *harness) (circusv1connect.SessionServiceClient, string) {
	t.Helper()
	jar, err := cookiejar.New(nil)
	must(t, err)
	httpClient := &http.Client{Jar: jar, Transport: http.DefaultTransport.(*http.Transport).Clone()}
	t.Cleanup(httpClient.CloseIdleConnections)
	ctx := testwait.Ctx(t)
	auth := circusv1connect.NewAuthServiceClient(httpClient, h.base)
	_, err = auth.Login(ctx, connect.NewRequest(&pb.LoginRequest{
		Email: enrollmentEmail, Password: enrollmentPassword,
	}))
	must(t, err)
	projects := circusv1connect.NewProjectServiceClient(httpClient, h.base)
	project, err := projects.CreateProject(ctx, connect.NewRequest(&pb.CreateProjectRequest{
		Name: "Fake worker", Locations: []*pb.Project_Location{{Device: "desk", Path: h.dir}},
	}))
	must(t, err)
	return circusv1connect.NewSessionServiceClient(httpClient, h.base), project.Msg.GetProject().GetProjectId()
}

type sessionBrowser struct {
	t   *testing.T
	ctx context.Context
	api circusv1connect.SessionServiceClient
	id  string
}

func (s *sessionBrowser) send(text string, priority pb.InputPriority) *pb.SendMessageResponse {
	s.t.Helper()
	reply, err := s.api.SendMessage(s.ctx, connect.NewRequest(&pb.SendMessageRequest{
		SessionId: s.id, ClientMsgId: uuid.NewString(), Text: text, Priority: priority,
	}))
	must(s.t, err)
	return reply.Msg
}

func (s *sessionBrowser) state(want pb.Session_State, outcome pb.TurnOutcome) *pb.Session {
	s.t.Helper()
	var state *pb.Session
	testwait.For(s.t, func() bool {
		resp, err := s.api.GetSession(s.ctx, connect.NewRequest(&pb.GetSessionRequest{SessionId: s.id}))
		must(s.t, err)
		state = resp.Msg
		return state.GetState() == want && state.GetLastTurnOutcome() == outcome
	})
	return state
}

type sessionWatch struct {
	t       *testing.T
	stream  *connect.ServerStreamForClient[pb.SessionEvent]
	seq     uint64
	durable []*pb.SessionEvent
}

func (s *sessionBrowser) watch(after uint64) *sessionWatch {
	s.t.Helper()
	stream, err := s.api.WatchSession(s.ctx, connect.NewRequest(&pb.WatchSessionRequest{SessionId: s.id, AfterSeq: after}))
	must(s.t, err)
	s.t.Cleanup(func() { _ = stream.Close() })
	return &sessionWatch{t: s.t, stream: stream, seq: after}
}

func (w *sessionWatch) next() *pb.SessionEvent {
	w.t.Helper()
	if !w.stream.Receive() {
		w.t.Fatalf("watch ended after seq %d: %v", w.seq, w.stream.Err())
	}
	e := w.stream.Msg()
	if e.GetEphemeral() {
		if e.GetSeq() != 0 {
			w.t.Fatalf("ephemeral event with durable sequence: %v", e)
		}
		return e
	}
	if e.GetSeq() != w.seq+1 {
		w.t.Fatalf("watch gap after seq %d: %v", w.seq, e)
	}
	w.seq = e.GetSeq()
	w.durable = append(w.durable, e)
	return e
}

func (w *sessionWatch) expect(kind pb.SessionEvent_Kind) *pb.SessionEvent {
	w.t.Helper()
	for {
		e := w.next()
		if e.GetKind() == kind && (!e.GetEphemeral() || kind == pb.SessionEvent_KIND_UNSPECIFIED) {
			return e
		}
		if !e.GetEphemeral() {
			w.t.Fatalf("watch expected %s, got %v", kind, e)
		}
	}
}

func (w *sessionWatch) until(match func(*pb.SessionEvent) bool) *pb.SessionEvent {
	w.t.Helper()
	for {
		if e := w.next(); match(e) {
			return e
		}
	}
}

func contains(kind pb.SessionEvent_Kind, text string) func(*pb.SessionEvent) bool {
	return func(e *pb.SessionEvent) bool {
		return e.GetKind() == kind && strings.Contains(e.GetPayloadJson(), text)
	}
}

func eventString(t *testing.T, e *pb.SessionEvent, field string) string {
	t.Helper()
	var payload map[string]json.RawMessage
	must(t, json.Unmarshal([]byte(e.GetPayloadJson()), &payload))
	var value string
	must(t, json.Unmarshal(payload[field], &value))
	return value
}

func checkToolPair(t *testing.T, use, result *pb.SessionEvent, tool string, isError bool, output ...string) {
	t.Helper()
	var used, returned struct {
		Message struct {
			Content []struct {
				Type, ID, Name string
				ToolUseID      string `json:"tool_use_id"`
				IsError        bool   `json:"is_error"`
			}
		}
	}
	must(t, json.Unmarshal([]byte(use.GetPayloadJson()), &used))
	must(t, json.Unmarshal([]byte(result.GetPayloadJson()), &returned))
	a, b := used.Message.Content, returned.Message.Content
	if len(a) != 1 || len(b) != 1 || a[0].Type != "tool_use" || a[0].ID == "" || a[0].Name != tool ||
		b[0].Type != "tool_result" || a[0].ID != b[0].ToolUseID || b[0].IsError != isError {
		t.Fatalf("invalid tool event pair\nuse %v\nresult %v", use, result)
	}
	for _, want := range output {
		if !strings.Contains(result.GetPayloadJson(), want) {
			t.Fatalf("tool result expected %q, got %s", want, result.GetPayloadJson())
		}
	}
}

func checkAbortedText(t *testing.T, delta, assistant *pb.SessionEvent) string {
	t.Helper()
	var partial struct {
		Event struct {
			Delta struct{ Text string }
		}
	}
	var saved struct {
		Aborted bool
		Message struct{ Content []struct{ Text string } }
	}
	must(t, json.Unmarshal([]byte(delta.GetPayloadJson()), &partial))
	must(t, json.Unmarshal([]byte(assistant.GetPayloadJson()), &saved))
	if !saved.Aborted || len(saved.Message.Content) != 1 || partial.Event.Delta.Text == "" ||
		!strings.HasPrefix(saved.Message.Content[0].Text, partial.Event.Delta.Text) {
		t.Fatalf("partial text was not retained: delta %v, assistant %v", delta, assistant)
	}
	return saved.Message.Content[0].Text
}

func checkAbortedResult(t *testing.T, event *pb.SessionEvent, subtype, reason string, isError bool) {
	t.Helper()
	var result struct {
		Subtype        string
		TerminalReason string  `json:"terminal_reason"`
		StopReason     *string `json:"stop_reason"`
		IsError        bool    `json:"is_error"`
	}
	must(t, json.Unmarshal([]byte(event.GetPayloadJson()), &result))
	if result.Subtype != subtype || result.TerminalReason != reason || result.IsError != isError || result.StopReason != nil {
		t.Fatalf("aborted result: %v", event)
	}
}

func toolUseID(t *testing.T, event *pb.SessionEvent) string {
	t.Helper()
	var payload struct {
		Message struct {
			Content []struct{ ID string }
		}
	}
	must(t, json.Unmarshal([]byte(event.GetPayloadJson()), &payload))
	if len(payload.Message.Content) != 1 || payload.Message.Content[0].ID == "" {
		t.Fatalf("missing tool-use ID: %v", event)
	}
	return payload.Message.Content[0].ID
}

func assistantID(t *testing.T, event *pb.SessionEvent) string {
	t.Helper()
	var payload struct {
		Message struct{ ID string }
	}
	must(t, json.Unmarshal([]byte(event.GetPayloadJson()), &payload))
	if payload.Message.ID == "" {
		t.Fatalf("missing assistant message ID: %v", event)
	}
	return payload.Message.ID
}

func resultCost(t *testing.T, event *pb.SessionEvent) float64 {
	t.Helper()
	var result struct {
		Cost   float64 `json:"total_cost_usd"`
		Models map[string]struct {
			Cost float64 `json:"costUSD"`
		} `json:"modelUsage"`
	}
	must(t, json.Unmarshal([]byte(event.GetPayloadJson()), &result))
	var cost float64
	for _, model := range result.Models {
		cost += model.Cost
	}
	if result.Cost <= 0 || result.Cost != cost {
		t.Fatalf("inconsistent result usage: %v", event)
	}
	return result.Cost
}
