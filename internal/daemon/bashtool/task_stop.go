package bashtool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
)

// TaskStop stops a background Bash job owned by the calling session.
type TaskStop struct{ jobs Jobs }

// NewTaskStop returns a session-scoped background job stop tool.
func NewTaskStop(jobs Jobs) *TaskStop { return &TaskStop{jobs: jobs} }

// Name returns the device call tool name.
func (*TaskStop) Name() string { return "task_stop" }

// Run stops only jobs owned by the call's session and returns native JSON text.
func (t *TaskStop) Run(_ context.Context, call *circusv1.DeviceCall) (*circusv1.DeviceResult, error) {
	id := call.GetTaskStop().GetJobId()
	var command string
	var found bool
	var err error
	if t.jobs != nil {
		command, found, err = t.jobs.StopBash(call.SessionId, id)
	}
	if err != nil {
		return nil, err
	}
	r := result("", !found, nil)
	r.Detail = &circusv1.DeviceResult_TaskStop{TaskStop: &circusv1.TaskStopResult{}}
	if !found {
		r.Output.Content[0].Block = &circusv1.ContentBlock_Text{Text: fmt.Sprintf("<tool_use_error>No task found with ID: %s</tool_use_error>", id)}
		return r, nil
	}
	var body bytes.Buffer
	encoder := json.NewEncoder(&body)
	encoder.SetEscapeHTML(false)
	err = encoder.Encode(struct {
		Message  string `json:"message"`
		TaskID   string `json:"task_id"`
		TaskType string `json:"task_type"`
		Command  string `json:"command"`
	}{fmt.Sprintf("Successfully stopped task: %s (%s)", id, command), id, "local_bash", command})
	if err != nil {
		return nil, err
	}
	r.Output.Content[0].Block = &circusv1.ContentBlock_Text{Text: strings.TrimSuffix(body.String(), "\n")}
	return r, nil
}
