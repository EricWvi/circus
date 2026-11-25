package ledgersnaps

import (
	"context"
	"errors"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/controlplane/calls"
	"github.com/EricWvi/circus/internal/store"
	"github.com/google/uuid"
)

// Deadlines of the device calls, per call (a chunk of a preparation).
const (
	prepareTimeout = 2 * time.Minute
	collectTimeout = time.Minute
	rebaseTimeout  = time.Minute
	removeTimeout  = 30 * time.Second
)

// outcome classifies how a device call ended.
type outcome int

const (
	// gotResult: the device answered; see the result.
	gotResult outcome = iota
	// offline: the device is not connected (or went away during the call).
	offline
	// transient: the call was lost, cancelled or refused for now; try
	// again later.
	transient
	// unusable: the device is unknown or revoked, refused the call as
	// invalid, or its daemon does not run ledger calls; the error says
	// which.
	unusable
)

var (
	errOffline     = errors.New("ledgersnaps: the device is offline")
	errRevoked     = errors.New("the device is unknown or revoked")
	errUnsupported = errors.New("the device's daemon does not keep ledger snapshots")
)

// deviceName resolves a live device's name; deleted is true for a deleted
// or unknown device.
func (m *Manager) deviceName(ctx context.Context, deviceID string) (name string, deleted bool, err error) {
	d, err := m.cfg.Store.GetIdentity(ctx, store.KindDevice, deviceID)
	if errors.Is(err, store.ErrNotFound) {
		return "", true, nil
	}
	if err != nil {
		return "", false, err
	}
	return d.Name, d.DeletedAt != 0, nil
}

// reachable checks, before any work on the snapshot of l, that its device
// can take a call now: it fails the snapshot of a revoked or unknown
// device and reports false for it and for an offline device, which
// queues the work again when it connects.
func (m *Manager) reachable(ctx context.Context, l store.SessionLedger) bool {
	_, deleted, err := m.deviceName(ctx, l.DeviceID)
	switch {
	case err != nil:
		m.logError("read device", l.SessionID, err)
		return false
	case deleted:
		m.fail(ctx, l.SessionID, circusv1.LedgerSnapshotError_LEDGER_SNAPSHOT_ERROR_UNSPECIFIED, errRevoked.Error())
		return false
	}
	return m.cfg.Online(l.DeviceID)
}

// call runs one control-plane device call on the snapshot of sessionID to
// its end. The result is set only for gotResult; a fault TOOL_UNAVAILABLE
// is unusable with errUnsupported.
func (m *Manager) call(ctx context.Context, userID, deviceID, sessionID string, timeout time.Duration, call *circusv1.DeviceCall) (*circusv1.DeviceResult, outcome, error) {
	if !m.cfg.Online(deviceID) {
		return nil, offline, nil
	}
	name, deleted, err := m.deviceName(ctx, deviceID)
	switch {
	case err != nil:
		return nil, transient, err
	case deleted:
		return nil, unusable, errRevoked
	}
	id, err := uuid.NewV7()
	if err != nil {
		return nil, transient, err
	}
	call.CallId = store.LedgerSnapshotCallPrefix + id.String()
	call.SessionId = store.LedgerSnapshotSessionPrefix + sessionID
	call.TimeoutMs = uint32(timeout.Milliseconds())
	// The router answers within the call's deadline; the margin covers
	// the trip and the queue.
	ctx, cancel := context.WithTimeout(ctx, timeout+30*time.Second)
	defer cancel()
	sub, err := m.cfg.Submit(ctx, userID, name, call)
	if err != nil {
		return nil, subscriptionOutcome(err), err
	}
	defer sub.Close()
	for {
		update, err := sub.Next(ctx)
		if err != nil {
			return nil, subscriptionOutcome(err), err
		}
		switch u := update.GetUpdate().(type) {
		case *circusv1.CallUpdate_Result:
			if fault := u.Result.GetFault(); fault != nil {
				if fault.GetCode() == circusv1.DaemonFault_CODE_TOOL_UNAVAILABLE {
					return nil, unusable, errUnsupported
				}
				return nil, transient, errors.New("ledgersnaps: daemon fault " + fault.GetCode().String())
			}
			return u.Result, gotResult, nil
		case *circusv1.CallUpdate_Offline:
			return nil, offline, nil
		case *circusv1.CallUpdate_Lost_, *circusv1.CallUpdate_Cancelled_:
			return nil, transient, nil
		case *circusv1.CallUpdate_Rejected_:
			switch u.Rejected.GetCode() {
			case circusv1.CallUpdate_Rejected_CODE_UNKNOWN_DEVICE:
				return nil, unusable, errRevoked
			case circusv1.CallUpdate_Rejected_CODE_INVALID:
				return nil, unusable, errors.New("the device refused the call as invalid")
			}
			return nil, transient, nil
		}
		// Accepted, dispatched and unknown are not terminal.
	}
}

func subscriptionOutcome(err error) outcome {
	switch {
	case errors.Is(err, calls.ErrDeviceOffline):
		return offline
	default:
		return transient
	}
}
