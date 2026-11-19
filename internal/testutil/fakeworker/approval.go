package fakeworker

import (
	"crypto/sha256"
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
		"tool_name": "Bash", "tool_use_id": toolID, "decision_reason_type": "classifier",
		"decision_reason": approvalReason, "message": denialText,
	}}
}

func (s *session) denyApproval(t *turn, toolID, command string, input object) error {
	s.events = append(s.events, sdkPermissionDenied(s.sdkID, uuid.NewString(), toolID).event())
	t.denials = append(t.denials, object{"tool_name": "Bash", "tool_use_id": toolID, "tool_input": input})
	if err := s.recordToolResult(toolID, denialText, true); err != nil {
		return err
	}
	if err := s.flush(); err != nil {
		return err
	}

	s.rememberApproval(command)
	display := approvalDisplay(command)
	r, err := s.request(wc.Op(s.id, s.epoch, &pb.RequestApproval{
		Tool: "bash", Device: s.device, ToolUseId: toolID, MatchKey: approvalKey(command), Display: display, Reason: approvalReason,
	}))
	if err != nil {
		return err
	}
	if r.GetApproval().GetApprovalId() == "" {
		return fmt.Errorf("approval: %v", r)
	}
	s.queue(pb.SessionEvent_KIND_APPROVAL_PENDING, object{
		"approval_id": r.GetApproval().GetApprovalId(), "tool": "bash", "device": s.device,
		"tool_use_id": toolID, "display": display, "reason": approvalReason,
	})
	return s.stream(t, "I need approval to run this Bash command.", s.w.opts.StreamDelay, false)
}

func (s *session) retryApproval(t *turn, text string) error {
	prefix := fmt.Sprintf("The user approved this bash call on device %q once: ", s.device)
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
