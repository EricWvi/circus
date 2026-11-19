package sessions

import circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"

// Waker is what the user-facing API needs from the scheduler. Wake is
// called after a transaction that changed something a worker must act on
// has committed. It never blocks on the network and never fails: hints are
// level-triggered, and the scheduler re-derives what to send from the
// store (and its own pending-reason table), so a lost or duplicate Wake is
// harmless. Reasons map to WorkerEvent.Nudge.Reason.
type Waker interface {
	Wake(sessionID string, reasons ...circusv1.WorkerEvent_Nudge_Reason)
}
