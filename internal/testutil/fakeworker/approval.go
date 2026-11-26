package fakeworker

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"

	pb "github.com/EricWvi/circus/gen/go/circus/v1"
	wc "github.com/EricWvi/circus/internal/testutil/workerclient"
	"github.com/google/uuid"
)

const approvalReason = "Scripted Bash approval required"

const denialText = "Permission for this action was denied by the Claude Code auto mode classifier. Reason: " +
	approvalReason + ". If you have other tasks that don't depend on this action, continue working on those. " +
	"IMPORTANT: You *may* attempt to accomplish this action using other tools that might naturally be used to accomplish this goal, e.g. using head instead of cat. " +
	"But you *should not* attempt to work around this denial in malicious ways, e.g. do not use your ability to run tests to execute non-test actions. " +
	"You should only try to work around this restriction in reasonable ways that do not attempt to bypass the intent behind this denial. " +
	"If you believe this capability is essential to complete the user's request, first try a safer method. " +
	"Get as much of the rest of the task done as you can, then STOP and explain to the user what you were trying to do and why you need this permission. " +
	"Let the user decide how to proceed. To allow this type of action in the future, the user can add a permission rule for Bash to their settings."

func approvalKey(command string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(command))) }

func approvalDisplay(command string) string {
	if len(command) <= 1024 {
		return command
	}
	suffix := " … [" + approvalKey(command) + "]"
	return clipUTF8(command, 1024-len(suffix)) + suffix
}

func (s *session) rememberApproval(command string) {
	if len(command) <= 1024 {
		return
	}
	if s.longApprovals == nil {
		s.longApprovals = make(map[string]string)
	}
	s.longApprovals[approvalDisplay(command)] = command
}

func sdkPermissionDenied(sessionID, id, toolID string) sdkMessage {
	return sdkMessage{pb.SessionEvent_KIND_PERMISSION_DENIED, object{
		"type": "system", "subtype": "permission_denied", "uuid": id, "session_id": sessionID,
		"tool_name": approvalTool, "tool_use_id": toolID, "decision_reason_type": "classifier",
		"decision_reason": approvalReason, "message": denialText,
	}}
}

// approvalTool is the tool name approvals carry: the SDK's, as the real
// worker sends it.
const approvalTool = "Bash"

// sdkDenialResult is the tool_result Claude Code records for a call the
// auto mode classifier blocked.
func sdkDenialResult(sessionID, id, toolID string) sdkMessage {
	return sdkToolResult(sessionID, id, toolID, denialText, true, "Error: "+denialText, object{
		"id": toolID, "non_execution_kind": "automode-blocked",
		"permission_decision": object{"decision": "reject", "reason_type": "classifier", "source": "config"},
	})
}

// denyApproval denies a tool use the way the real worker reports it: the
// permission denial, then the approval request (whose APPROVAL_PENDING the
// control plane records, before Claude Code's tool_result that carries the
// denial to the model), then that tool_result.
func (s *session) denyApproval(t *turn, toolID, command string, input object) error {
	s.events = append(s.events, sdkPermissionDenied(s.sdkID, uuid.NewString(), toolID).event())
	t.denials = append(t.denials, object{"tool_name": approvalTool, "tool_use_id": toolID, "tool_input": input})
	if err := s.flush(); err != nil {
		return err
	}

	s.rememberApproval(command)
	display := approvalDisplay(command)
	r, err := s.request(wc.Op(s.id, s.epoch, &pb.RequestApproval{
		Tool: approvalTool, Device: s.device, ToolUseId: toolID, MatchKey: approvalKey(command), Display: display, Reason: approvalReason,
	}))
	if err != nil {
		return err
	}
	if r.GetApproval().GetApprovalId() == "" {
		return fmt.Errorf("approval: %v", r)
	}
	if err := s.record(sdkDenialResult(s.sdkID, uuid.NewString(), toolID)); err != nil {
		return err
	}
	if err := s.flush(); err != nil {
		return err
	}
	return s.stream(t, "I need approval to run this Bash command.", s.w.opts.StreamDelay, plainText)
}

func (s *session) retryApproval(t *turn, text string) error {
	prefix := fmt.Sprintf("The user approved this %s call on device %q once: ", approvalTool, s.device)
	display, ok := strings.CutPrefix(text, prefix)
	if !ok {
		return nil
	}
	display, ok = strings.CutSuffix(display, "\nRetry it now.")
	if !ok {
		return nil
	}
	command := display
	if full := s.longApprovals[display]; full != "" {
		command = full
	}
	return s.tool(t, "approved-bash", command)
}

// questionTool is the tool a question approval names.
const questionTool = "AskUserQuestion"

// askInputJSON is the AskUserQuestion input of `/fake ask`, as the real
// worker sends it (in the order of Claude Code's own schema): two
// questions, the second multiSelect.
const askInputJSON = `{"questions":[{"question":"Which color should the button be?","header":"Color",` +
	`"options":[{"label":"Red","description":"A warm color"},{"label":"Blue","description":"A cool color"}],"multiSelect":false},` +
	`{"question":"Which features do you want?","header":"Features","options":[{"label":"Dark mode","description":"A dark theme"},` +
	`{"label":"Search","description":"Full-text search"},{"label":"Export","description":"CSV export"}],"multiSelect":true}]}`

// questionAskedText is what the real worker's canUseTool tells the model
// when it turns AskUserQuestion into a question approval
// (worker/src/runner/approvals.ts).
const questionAskedText = "Your questions were shown to the user. End your turn now, without guessing the answers: " +
	"the user's answers will arrive as the next user message."

// sdkQuestionDenied is the tool_result Claude Code records for a call
// canUseTool denied.
func sdkQuestionDenied(sessionID, id, toolID string) sdkMessage {
	return sdkToolResult(sessionID, id, toolID, questionAskedText, true, "Error: "+questionAskedText, object{
		"id": toolID, "non_execution_kind": "permission-rule",
		"permission_decision": object{"decision": "reject", "source": "user_reject"},
	})
}

// ask makes the model's AskUserQuestion call the way the real worker
// handles it: the tool use, the question approval (recorded by the control
// plane before
// Claude Code's tool_result that carries the denial to the model), that
// tool_result, and a closing text that ends the turn. The answers arrive
// as the next turn's input.
func (s *session) ask(t *turn) error {
	var input object
	if err := json.Unmarshal([]byte(askInputJSON), &input); err != nil {
		return err
	}
	u := toolCall{id: "toolu_" + uuid.NewString(), name: questionTool, input: input}
	if err := s.announce(t, u); err != nil {
		return err
	}
	const display = "Which color should the button be?"
	r, err := s.request(wc.Op(s.id, s.epoch, &pb.RequestApproval{
		Kind: pb.ApprovalKind_APPROVAL_KIND_QUESTION, Tool: questionTool, ToolUseId: u.id, Display: display, InputJson: askInputJSON,
	}))
	if err != nil {
		return err
	}
	if r.GetApproval().GetApprovalId() == "" {
		return fmt.Errorf("question approval: %v", r)
	}
	t.denials = append(t.denials, object{"tool_name": questionTool, "tool_use_id": u.id, "tool_input": input})
	if err := s.record(sdkQuestionDenied(s.sdkID, uuid.NewString(), u.id)); err != nil {
		return err
	}
	if err := s.flush(); err != nil {
		return err
	}
	return s.stream(t, "I asked you two questions.", s.w.opts.StreamDelay, plainText)
}
