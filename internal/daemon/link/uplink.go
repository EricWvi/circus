package link

import (
	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"google.golang.org/protobuf/proto"
)

// resultTooLarge is the fixed fault message of an undeliverable result. The
// fault code reuses CODE_INTERNAL; the proto has no dedicated code for it.
const resultTooLarge = "RESULT_TOO_LARGE"

// fitUplink replaces a result that no idle link could accept with a small
// placeholder carrying a fixed fault. It changes only the transport view: the
// retained Dispatcher result stays intact, and Result, CallStatus and duplicate
// CallAck all get the same placeholder, so a resync by query cannot loop on
// reconnects. Other envelopes are bounded by construction and pass unchanged.
func fitUplink(m *circusv1.DaemonUplink, limit int) *circusv1.DaemonUplink {
	if proto.Size(m) <= limit {
		return m
	}
	placeholder := func(id string) *circusv1.DeviceResult {
		return &circusv1.DeviceResult{
			CallId: id,
			Fault:  &circusv1.DaemonFault{Code: circusv1.DaemonFault_CODE_INTERNAL, Message: resultTooLarge},
			Output: &circusv1.ToolOutput{IsError: true, Content: []*circusv1.ContentBlock{{Block: &circusv1.ContentBlock_Text{
				Text: "Error: the result exceeds the control plane's message limit and cannot be delivered.",
			}}}},
		}
	}
	status := func(s *circusv1.CallStatus) *circusv1.CallStatus {
		return &circusv1.CallStatus{CallId: s.CallId, State: s.State, Result: placeholder(s.Result.GetCallId())}
	}
	switch {
	case m.GetResult() != nil:
		return &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_Result{Result: placeholder(m.GetResult().CallId)}}
	case m.GetCallStatus().GetResult() != nil:
		return &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_CallStatus{CallStatus: status(m.GetCallStatus())}}
	case m.GetCallAck().GetStatus().GetResult() != nil:
		return &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_CallAck{CallAck: &circusv1.CallAck{CallId: m.GetCallAck().CallId, Outcome: &circusv1.CallAck_Status{Status: status(m.GetCallAck().GetStatus())}}}}
	default:
		return m
	}
}
