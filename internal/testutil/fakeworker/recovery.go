package fakeworker

import (
	"fmt"
	"slices"
	"strings"

	pb "github.com/EricWvi/circus/gen/go/circus/v1"
	wc "github.com/EricWvi/circus/internal/testutil/workerclient"
	"github.com/EricWvi/circus/internal/wire"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
)

func (s *session) recoverCalls(t *turn) error {
	if s.recovery == nil {
		return nil
	}
	r := s.recovery
	if r.GetPreviousTurnInterrupted() {
		if err := s.record(sdkUser(s.sdkID, uuid.NewString(), "The previous turn was interrupted.", true)); err != nil {
			return err
		}
	}
	for _, call := range r.GetCalls() {
		if err := s.recoverCall(t, call); err != nil {
			return err
		}
	}
	for _, task := range r.GetLostBgtasks() {
		text := fmt.Sprintf("Background task %s was lost (tool use %s).", task.GetTaskId(), task.GetToolUseId())
		if err := s.record(sdkUser(s.sdkID, uuid.NewString(), text, true)); err != nil {
			return err
		}
		// Simulate the CLI's own task_notification for an orphaned SDK task.
		payload := object{
			"type": "system", "subtype": "task_notification", "uuid": uuid.NewString(),
			"session_id": s.sdkID, "task_id": task.GetTaskId(), "status": "stopped",
			"reason": "worker_restart", "summary": text, "output_file": "",
		}
		if id := task.GetToolUseId(); id != "" {
			payload["tool_use_id"] = id
		}
		s.queue(pb.SessionEvent_KIND_BGTASK_ENDED, payload)
		t.tasks = append(t.tasks, task.GetTaskId())
	}
	s.recovery = nil
	return nil
}

func (s *session) recoverCall(t *turn, call *pb.SessionState_Call) error {
	id := call.GetCallId()
	summary := callSummary(call)
	blocks := []object{{"type": "text", "text": summary}}
	owed := call.GetNoticeOwed()
	if !call.GetReturned() {
		u, err := s.callOnce(id, false)
		if err != nil {
			return err
		}
		patch := &pb.PatchSessionState{ReturnedCallIds: []string{id}}
		switch {
		case u.GetRejected().GetCode() == pb.CallUpdate_Rejected_CODE_NOT_FOUND:
			blocks = append(blocks, object{"type": "text", "text": "Call result unavailable."})
		case wire.Terminal(u):
			output, _ := callOutput(u)
			blocks = append(blocks, output...)
		default:
			patch.AwaitingCallIds = []string{id}
			owed = true
		}
		if _, err := s.request(wc.Patch(s.id, s.epoch, patch)); rejected(err, pb.SessionReply_Rejected_CODE_LIMIT) {
			blocks = append(blocks, object{"type": "text", "text": "Call result unavailable."})
			patch.AwaitingCallIds = nil
			owed = false
			if _, err := s.request(wc.Patch(s.id, s.epoch, patch)); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
	}
	if owed {
		if s.owed == nil {
			s.owed = make(map[string]*pb.SessionState_Call)
		}
		s.owed[id] = call
		blocks = append(blocks, object{"type": "text", "text": "A completion notice will follow."})
	}
	if err := s.record(sdkUser(s.sdkID, uuid.NewString(), blocks, true)); err != nil {
		return err
	}
	t.marks.CallIds = append(t.marks.CallIds, id)
	return s.flush()
}

func (s *session) pollOwed() error {
	for id := range s.owed {
		u, err := s.callOnce(id, false)
		if err != nil {
			return err
		}
		if wire.Terminal(u) || u.GetRejected().GetCode() == pb.CallUpdate_Rejected_CODE_NOT_FOUND {
			s.notices = append(s.notices, u)
		}
	}
	return nil
}

func (s *session) deliverNotices(t *turn) error {
	for _, u := range s.notices {
		call := s.owed[u.GetCallId()]
		output, _ := callOutput(u)
		blocks := append([]object{{"type": "text", "text": fmt.Sprintf("%s call %s on %s finished.", call.GetTool(), call.GetCallId(), call.GetDevice())}}, output...)
		if err := s.record(sdkUser(s.sdkID, uuid.NewString(), blocks, true)); err != nil {
			return err
		}
		t.marks.LateCallIds = append(t.marks.LateCallIds, u.GetCallId())
	}
	s.notices = nil
	for _, job := range s.jobs {
		if _, err := s.request(wc.Patch(s.id, s.epoch, &pb.PatchSessionState{HandedJobs: []*pb.JobKey{job.GetKey()}})); err != nil {
			return err
		}
		state := strings.ToLower(strings.TrimPrefix(job.GetState().String(), "STATE_"))
		text := fmt.Sprintf("Background job %s on %s %s (exit %d).", job.GetKey().GetJobId(), job.GetDevice(), state, job.GetExitCode())
		if job.GetOutputPath() != "" {
			text += " Output: " + job.GetOutputPath()
		}
		if err := s.record(sdkUser(s.sdkID, uuid.NewString(), text, true)); err != nil {
			return err
		}
		s.queue(pb.SessionEvent_KIND_NOTIFICATION_DELIVERED, object{
			"job_id": job.GetKey().GetJobId(), "call_id": job.GetCallId(), "tool_use_id": job.GetToolUseId(),
			"device": job.GetDevice(), "state": state, "exit_code": job.GetExitCode(), "output_path": job.GetOutputPath(),
		})
		t.marks.Jobs = append(t.marks.Jobs, job.GetKey())
		if err := s.flush(); err != nil {
			return err
		}
	}
	s.jobs = nil
	return nil
}

func (s *session) splitMarks(marks *pb.ReportTurn_Persisted) (*pb.ReportTurn_Persisted, error) {
	if len(marks.GetCallIds()) <= 1024 && proto.Size(marks) <= 60<<10 {
		return marks, nil
	}
	// The 64 KiB session-request limit binds before the per-field
	// cardinality limits for a large recovery. Every chunk fits even at
	// the maximum encoded sizes of a call ID or a three-part job key.
	for chunk := range slices.Chunk(marks.GetCallIds(), 64) {
		patch := &pb.PatchSessionState{Persisted: &pb.ReportTurn_Persisted{CallIds: chunk}}
		if _, err := s.request(wc.Patch(s.id, s.epoch, patch)); err != nil {
			return nil, err
		}
	}
	for chunk := range slices.Chunk(marks.GetJobs(), 64) {
		patch := &pb.PatchSessionState{Persisted: &pb.ReportTurn_Persisted{Jobs: chunk}}
		if _, err := s.request(wc.Patch(s.id, s.epoch, patch)); err != nil {
			return nil, err
		}
	}
	return &pb.ReportTurn_Persisted{InputIds: marks.GetInputIds(), LateCallIds: marks.GetLateCallIds(), GapNoticeTurnNo: marks.GetGapNoticeTurnNo()}, nil
}
