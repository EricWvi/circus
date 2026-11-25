package fakeworker

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"path"
	"strings"

	pb "github.com/EricWvi/circus/gen/go/circus/v1"
	wc "github.com/EricWvi/circus/internal/testutil/workerclient"
	"github.com/EricWvi/circus/internal/wire"
	"github.com/google/uuid"
)

// toolCall is one device tool use the script makes.
type toolCall struct {
	call  *pb.DeviceCall
	id    string
	name  string
	input object
}

func (s *session) newToolCall(command, arg string) toolCall {
	call := &pb.DeviceCall{CallId: uuid.NewString(), SessionId: s.id}
	name := "Bash"
	input := object{"command": arg}
	if command == "read" {
		name = "Read"
		input = object{"file_path": arg}
		call.Tool = &pb.DeviceCall_Read{Read: &pb.ReadRequest{Path: arg, Cwd: s.cwd}}
	} else {
		background := command == "job"
		if background {
			input["run_in_background"] = true
		}
		call.Tool = &pb.DeviceCall_Bash{Bash: &pb.BashRequest{
			Command: arg, Cwd: s.cwd, WorkingDirs: []string{s.root},
			RunInBackground: background, Description: clipUTF8(arg, 1024),
		}}
	}
	return toolCall{call: call, id: "toolu_" + uuid.NewString(), name: name, input: input}
}

// useResult is the call's tool_use_result (toolUseResult). A Write or
// Edit that succeeded gets Claude Code's shape for it; the fake knows
// neither the file's previous content nor the diff.
func (u toolCall) useResult(arg, output string, result *pb.DeviceResult, failed bool) any {
	if !failed {
		switch u.name {
		case "Write":
			kind := "update"
			if result.GetWrite().GetCreated() {
				kind = "create"
			}
			return object{
				"type": kind, "filePath": u.input["file_path"], "content": u.input["content"],
				"structuredPatch": []any{}, "originalFile": nil,
			}
		case "Edit":
			return object{
				"filePath": u.input["file_path"], "oldString": u.input["old_string"], "newString": u.input["new_string"],
				"originalFile": nil, "structuredPatch": []any{}, "userModified": false, "replaceAll": false,
			}
		}
	}
	return toolUseResult(u.name, arg, output, failed)
}

// announce streams the tool uses as the blocks of one API message.
func (s *session) announce(t *turn, uses ...toolCall) error {
	r := s.begin(t)
	output := 0
	for i, u := range uses {
		input := []rune(jsonString(u.input))
		outputTokens := tokens(string(input))
		output += outputTokens
		s.partial(r, object{"type": "content_block_start", "index": i, "content_block": object{"type": "tool_use", "id": u.id, "name": u.name, "input": object{}}}, false)
		s.partial(r, inputDelta(i, string(input[:len(input)/2])), false)
		s.partial(r, inputDelta(i, string(input[len(input)/2:])), false)
		if err := s.assistant(t, r, toolUse(u.id, u.name, u.input), outputTokens, false); err != nil {
			return err
		}
		s.partial(r, blockStop(i), false)
	}
	t.outputTokens += output
	s.partial(r, messageDelta("tool_use", r.input, output, 0), false)
	s.partial(r, object{"type": "message_stop"}, false)
	return s.flush()
}

func (s *session) tool(t *turn, command, arg string) error {
	u := s.newToolCall(command, arg)
	if err := s.announce(t, u); err != nil {
		return err
	}
	return s.runTool(t, command, arg, u, true)
}

// parallel makes two Bash calls in one API message, then runs them in turn.
func (s *session) parallel(t *turn, arg string) error {
	uses := []toolCall{s.newToolCall("bash", arg), s.newToolCall("bash", arg)}
	if err := s.announce(t, uses...); err != nil {
		return err
	}
	for _, u := range uses {
		if err := s.runTool(t, "bash", arg, u, false); err != nil {
			return err
		}
	}
	return s.stream(t, "Both Bash calls finished.", s.w.opts.StreamDelay, plainText)
}

// largeResult answers a Bash use with n bytes of output without running it,
// for a view that stores oversized payloads truncated.
func (s *session) largeResult(t *turn, n int) error {
	u := s.newToolCall("bash", fmt.Sprintf("yes | head -c %d", n))
	if err := s.announce(t, u); err != nil {
		return err
	}
	output := strings.Repeat("y\n", n/2)
	if err := s.record(sdkToolResult(s.sdkID, uuid.NewString(), u.id, output, false,
		toolUseResult(u.name, "", output, false), allowedMeta(u.id, false))); err != nil {
		return err
	}
	if err := s.flush(); err != nil {
		return err
	}
	return s.stream(t, "Bash finished.", s.w.opts.StreamDelay, plainText)
}

// runTool runs an announced tool use and records its result; closing streams
// the closing text.
func (s *session) runTool(t *turn, command, arg string, u toolCall, closing bool) error {
	if command == "approve" {
		return s.denyApproval(t, u.id, arg, u.input)
	}
	if command == "approved-bash" {
		reply, err := s.request(wc.Op(s.id, s.epoch, &pb.ConsumeGrant{
			Tool: approvalTool, Device: s.device, MatchKey: approvalKey(arg), ToolUseId: u.id,
		}))
		if err != nil {
			return err
		}
		if !reply.GetGrant().GetAllowed() {
			return s.denyApproval(t, u.id, arg, u.input)
		}
	}

	update, failed, err := s.callTool(t, u, arg, command == "approved-bash")
	if err != nil || !closing {
		return err
	}
	text := u.name + " finished."
	if failed {
		text = u.name + " returned an error."
	} else if update.GetResult().GetBash().GetJobId() != "" {
		text = "Bash started a background job."
	}
	return s.stream(t, text, s.w.opts.StreamDelay, plainText)
}

// callTool runs an announced tool use on the session's device and records
// its tool_result; failed reports an error result. hook marks a call the
// worker's PreToolUse hook let run (an approved retry). arg is the script's
// argument: a Read's path as given.
func (s *session) callTool(t *turn, u toolCall, arg string, hook bool) (*pb.CallUpdate, bool, error) {
	call, toolID := u.call, u.id
	req := &pb.SubmitCall{Device: s.device, Call: call, SessionEpoch: s.epoch, ToolUseId: toolID}
	update, submitted, err := s.submit(t, req)
	if err != nil {
		if isAbort(err) {
			t.abortReason = abortedTools
			if submitted {
				if _, patchErr := s.request(wc.Patch(s.id, s.epoch, &pb.PatchSessionState{ReturnedCallIds: []string{call.GetCallId()}})); patchErr != nil {
					return nil, false, errors.Join(err, patchErr)
				}
				t.marks.CallIds = append(t.marks.CallIds, call.GetCallId())
			}
			if saveErr := s.record(sdkInterruptedTool(s.sdkID, uuid.NewString(), toolID)); saveErr != nil {
				return nil, false, errors.Join(err, saveErr)
			}
		}
		return nil, false, err
	}
	switch update.GetRejected().GetCode() {
	case pb.CallUpdate_Rejected_CODE_SESSION_FENCED:
		return nil, false, errFenced
	case pb.CallUpdate_Rejected_CODE_SESSION_STOPPED:
		reply, err := s.request(wc.State(s.id, s.epoch, pb.GetSessionState_SECTION_NONE))
		if err != nil {
			return nil, false, err
		}
		if reply.GetState().GetState().GetStopped() {
			return nil, false, errStopped
		}
		return nil, false, errBroken
	}

	patch := &pb.PatchSessionState{ReturnedCallIds: []string{call.GetCallId()}}
	result := update.GetResult()
	if cwd := result.GetBash().GetCwd(); cwd != "" && cwd != s.cwd {
		patch.Cwds = []*pb.PatchSessionState_Cwd{{Device: s.device, Cwd: cwd}}
		s.cwd = cwd
	}
	if read := result.GetRead(); read.GetSha256() != "" {
		file := arg
		if !path.IsAbs(file) {
			file = path.Join(s.cwd, file)
		}
		if len(file) <= 4096 {
			patch.Files = []*pb.PatchSessionState_FileRecord{{
				Device: s.device, Path: file, Sha256: read.GetSha256(), MtimeUnixMs: read.GetMtimeUnixMs(),
			}}
		}
	}
	if _, err := s.request(wc.Patch(s.id, s.epoch, patch)); err != nil {
		return nil, false, err
	}
	blocks, failed := callOutput(update)
	content := resultContent(blocks)
	output, _ := content.(string)
	message := sdkToolResult(s.sdkID, uuid.NewString(), toolID, content, failed,
		u.useResult(arg, output, result, failed), allowedMeta(toolID, hook))
	if err := s.record(message); err != nil {
		return nil, false, err
	}
	t.marks.CallIds = append(t.marks.CallIds, call.GetCallId())
	return update, failed, nil
}

func callOutput(u *pb.CallUpdate) ([]object, bool) {
	if result := u.GetResult(); result != nil {
		out := result.GetOutput()
		blocks := make([]object, 0, len(out.GetContent()))
		for _, block := range out.GetContent() {
			if img := block.GetImage(); img != nil {
				blocks = append(blocks, object{"type": "image", "source": object{
					"type": "base64", "media_type": img.GetMediaType(), "data": base64.StdEncoding.EncodeToString(img.GetData()),
				}})
			} else {
				blocks = append(blocks, object{"type": "text", "text": block.GetText()})
			}
		}
		return blocks, out.GetIsError()
	}
	var text string
	switch {
	case u.GetOffline() != nil:
		text = "Device is offline."
	case u.GetLost() != nil:
		text = "Call result unavailable: " + u.GetLost().GetReason()
	case u.GetCancelled() != nil:
		text = "Call cancelled."
	case u.GetRejected() != nil:
		text = "Call rejected: " + u.GetRejected().GetCode().String()
		if message := u.GetRejected().GetMessage(); message != "" {
			text += ": " + message
		}
	default:
		text = "Call is still running."
	}
	return []object{{"type": "text", "text": text}}, true
}

func (s *session) submit(t *turn, req *pb.SubmitCall) (*pb.CallUpdate, bool, error) {
	attempted := false
	id := req.GetCall().GetCallId()
	abort := func(err error) error {
		if attempted && isAbort(err) {
			return s.cancelCall(id, err)
		}
		return err
	}
	for {
		l, err := s.w.connection(s.ctx)
		if err != nil {
			return nil, attempted, err
		}
		if err := s.serviceHints(t); err != nil {
			return nil, attempted, abort(err)
		}
		if attempted {
			update, err := s.callOnce(id, false)
			if err != nil {
				return nil, attempted, err
			}
			if wire.Terminal(update) {
				return update, true, nil
			}
			// An identical Submit joins an existing call stream after a link loss.
			l, err = s.w.connection(s.ctx)
			if err != nil {
				return nil, attempted, err
			}
		}
		update, submitted, err := s.watchCall(t, l, req)
		attempted = attempted || submitted
		switch {
		case err == nil:
			return update, attempted, nil
		case errors.Is(err, wire.ErrBusy):
			if err := s.wait(t, retryDelay); err != nil {
				return nil, attempted, abort(err)
			}
		case !errors.Is(err, errLink):
			return nil, attempted, abort(err)
		}
	}
}

var errLink = errors.New("worker link changed")

func (s *session) watchCall(t *turn, l *connection, req *pb.SubmitCall) (*pb.CallUpdate, bool, error) {
	for {
		select {
		case l.calls <- struct{}{}:
			defer func() { <-l.calls }()
		case <-l.c.Done():
			return nil, false, errLink
		case <-s.ctx.Done():
			return nil, false, context.Cause(s.ctx)
		case <-s.wake:
			if err := s.serviceHints(t); err != nil {
				return nil, false, err
			}
			continue
		}
		break
	}
	call, err := l.c.Submit(s.ctx, req)
	if err != nil {
		if errors.Is(err, wire.ErrBusy) || errors.Is(err, wire.ErrTooLarge) || s.ctx.Err() != nil {
			return nil, false, err
		}
		_ = l.c.Close()
		return nil, true, errLink
	}

	var result struct {
		update *pb.CallUpdate
		err    error
	}
	ctx, cancel := context.WithCancel(s.ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			result.update, result.err = call.Next(ctx)
			if result.err != nil || wire.Terminal(result.update) {
				return
			}
		}
	}()
	finished := false
	defer func() {
		cancel()
		<-done
		if !finished {
			call.Abandon()
		}
	}()

	for {
		if err := s.serviceHints(t); err != nil {
			return nil, true, err
		}
		select {
		case <-s.wake:
		case <-ctx.Done():
			return nil, true, context.Cause(s.ctx)
		case <-done:
			if s.ctx.Err() != nil {
				return nil, true, context.Cause(s.ctx)
			}
			if result.err != nil || !s.isCurrent(l) {
				return nil, true, errLink
			}
			finished = true
			return result.update, true, nil
		}
	}
}

func (s *session) isCurrent(l *connection) bool {
	s.w.mu.Lock()
	defer s.w.mu.Unlock()
	return s.w.link == l && s.ctx.Err() == nil
}

func (s *session) callOnce(id string, cancelCall bool) (*pb.CallUpdate, error) {
	for {
		l, err := s.w.connection(s.ctx)
		if err != nil {
			return nil, err
		}
		if cancelCall {
			// CancelCall itself is unfenced. Confirm the lease on this same
			// connection, and abandon the attempt on stop or fence.
			ctx, cancel := context.WithTimeout(s.ctx, opTimeout)
			r, err := sessionOp(ctx, l, wc.State(s.id, s.epoch, pb.GetSessionState_SECTION_NONE))
			cancel()
			if err != nil {
				return nil, err
			}
			if err := replyError(r); err != nil {
				return nil, err
			}
			if r.GetState().GetState().GetStopped() {
				return nil, errStopped
			}
			if !s.isCurrent(l) {
				continue
			}
		}

		u, err := callOp(s.ctx, l, id, cancelCall)
		if err == nil && s.isCurrent(l) {
			return u, nil
		}
		if s.ctx.Err() != nil {
			return nil, context.Cause(s.ctx)
		}
		if cancelCall {
			return nil, err
		}
		if err != nil && !errors.Is(err, wire.ErrBusy) && !errors.Is(err, context.DeadlineExceeded) {
			_ = l.c.Close()
		}
		if !s.w.sleep(s.ctx, retryDelay) {
			return nil, context.Cause(s.ctx)
		}
	}
}

func callOp(ctx context.Context, l *connection, id string, cancelCall bool) (*pb.CallUpdate, error) {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	select {
	case l.calls <- struct{}{}:
		defer func() { <-l.calls }()
	case <-l.c.Done():
		return nil, l.c.Err()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if cancelCall {
		return l.c.Cancel(ctx, id)
	}
	return l.c.Query(ctx, id)
}

func (s *session) cancelCall(id string, cause error) error {
	if _, err := s.callOnce(id, true); errors.Is(err, errStopped) || errors.Is(err, errFenced) {
		return err
	} else if err != nil && s.ctx.Err() == nil {
		s.w.log.Warn("call cancel failed", "call_id", id, "error", err)
	}
	return cause
}

func callSummary(call *pb.SessionState_Call) string {
	outcome := strings.ToLower(strings.TrimPrefix(call.GetOutcome().String(), "CALL_OUTCOME_"))
	if call.GetRejectedCode() != pb.CallUpdate_Rejected_CODE_UNSPECIFIED {
		outcome += " (" + call.GetRejectedCode().String() + ")"
	}
	if call.ExitCode != nil {
		outcome += fmt.Sprintf(" (exit %d)", call.GetExitCode())
	}
	return fmt.Sprintf("Recovered %s call %s on %s: %s.", call.GetTool(), call.GetCallId(), call.GetDevice(), outcome)
}
