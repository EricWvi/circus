package calls

import (
	"context"
	"errors"
	"strings"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/controlplane/gateway"
	"github.com/EricWvi/circus/internal/controlplane/timers"
	"github.com/EricWvi/circus/internal/identifier"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/wire"
)

func protocol(reason string) error { return &wire.ProtocolError{Reason: reason} }

// current reports whether c is the device's registered generation.
func (r *router) current(ctx context.Context, c gateway.Connection) bool {
	v, ok, err := r.config.Registry.Lookup(ctx, c.Target.DeviceID)
	return err == nil && ok && sameConnection(v, c)
}

func (r *router) OnDisconnected(ctx context.Context, c gateway.Connection) error {
	op, err := r.enter(ctx, true, "disconnected")
	if err != nil {
		return err
	}
	defer op.finish()
	if d := r.devices[c.Target.DeviceID]; d != nil && sameConnection(d.connection, c) {
		delete(r.devices, c.Target.DeviceID)
		d.signal()
	}
	current := r.current(op.ctx, c)
	r.failQueries(c.Target.DeviceID, c.Target.ConnectionID, current)
	if !current {
		return nil
	}
	// Withdraw the route before recording the disconnect, under the admission
	// lock: no Submit can see this generation as online once the grace period
	// is recorded, and the gateway's later exact Unregister is a no-op.
	if _, err := r.config.Registry.Unregister(op.ctx, c); err != nil {
		return err
	}
	now := r.config.Clock.Now().UnixMilli()
	err = r.config.Store.Tx(op.ctx, func(ctx context.Context, tx store.Ops) error {
		if err := tx.SetDisconnected(ctx, store.KindDevice, c.Target.DeviceID, now); err != nil {
			return err
		}
		return tx.SetTimer(ctx, timers.DeviceGrace, c.Target.DeviceID, now+r.config.GracePeriod.Milliseconds())
	})
	if err != nil {
		// Keep a recovery path (L1): Submit and the sweep persist it later.
		r.storeFailed(err)
		r.unrecorded[strings.Clone(c.Target.DeviceID)] = now
		return err
	}
	delete(r.unrecorded, c.Target.DeviceID)
	return nil
}

// inbound loads the call a daemon reply names. Replies are fenced by user,
// device and dispatch boot, not by connection generation (M2): a superseded
// connection of the dispatch boot still delivers authoritative final results.
func (r *router) inbound(ctx context.Context, conn gateway.Connection, id string) (store.Call, bool, error) {
	c, err := r.config.Store.GetCall(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return c, false, nil
	}
	if err != nil {
		r.storeFailed(err)
		return c, false, err
	}
	return c, c.UserID == conn.UserID && c.DeviceID == conn.Target.DeviceID && c.DispatchedBootID == conn.Target.BootID, nil
}

func (r *router) OnCallAck(ctx context.Context, conn gateway.Connection, ack *circusv1.CallAck) (err error) {
	op, err := r.enter(ctx, true, "call-ack")
	if err != nil {
		return err
	}
	defer op.finish()
	if ack == nil || !identifier.Valid(ack.CallId, false) {
		return protocol("invalid ack")
	}
	r.replied(conn, ack.CallId)
	defer r.unapplied(conn, ack.CallId, &err)
	c, ok, err := r.inbound(op.ctx, conn, ack.CallId)
	if err != nil || !ok {
		return err
	}
	if status := ack.GetStatus(); status != nil {
		if status.CallId != ack.CallId {
			return protocol("ack call id mismatch")
		}
		return r.status(op.ctx, c, status)
	}
	rejection := ack.GetRejection()
	if rejection == nil {
		return protocol("missing ack outcome")
	}
	if c.State != store.CallDispatched {
		return nil
	}
	to, reason, code := store.CallRejected, "INVALID", circusv1.CallUpdate_Rejected_CODE_INVALID
	switch rejection.Code {
	case circusv1.Rejection_CODE_INVALID:
	case circusv1.Rejection_CODE_CALL_MISMATCH:
		reason, code = "CALL_MISMATCH", circusv1.CallUpdate_Rejected_CODE_CALL_MISMATCH
	case circusv1.Rejection_CODE_SHUTTING_DOWN:
		// The daemon deduplicates before its shutdown check and its shutdown is
		// permanent for this boot. The age check bounds two copies below the
		// tombstone lifetime, so a refusal of any copy proves none executed.
		reason, code = "DEVICE_SHUTTING_DOWN", circusv1.CallUpdate_Rejected_CODE_DEVICE_SHUTTING_DOWN
	case circusv1.Rejection_CODE_BOOT_MISMATCH:
		to, reason = store.CallLost, "BOOT_MISMATCH"
	case circusv1.Rejection_CODE_EXPIRED:
		to, reason = store.CallLost, "EXPIRED"
	default:
		return protocol("invalid rejection code")
	}
	u := rejected(c.CallID, code)
	if to == store.CallLost {
		u = lost(c.CallID, reason)
	}
	now := r.config.Clock.Now().UnixMilli()
	return r.settle(op.ctx, c, to, store.CallPatch{AckedAt: now, FinishedAt: now, OutcomeReason: reason}, u)
}

func (r *router) OnDeviceResult(ctx context.Context, conn gateway.Connection, result *circusv1.DeviceResult) error {
	op, err := r.enter(ctx, true, "device-result")
	if err != nil {
		return err
	}
	defer op.finish()
	if result == nil || !identifier.Valid(result.CallId, false) {
		return protocol("invalid result")
	}
	c, ok, err := r.inbound(op.ctx, conn, result.CallId)
	if err != nil || !ok {
		return err
	}
	return r.complete(op.ctx, c, result)
}

func (r *router) OnCallStatus(ctx context.Context, conn gateway.Connection, status *circusv1.CallStatus) (err error) {
	op, err := r.enter(ctx, true, "call-status")
	if err != nil {
		return err
	}
	defer op.finish()
	if status == nil || !identifier.Valid(status.CallId, false) {
		return protocol("invalid status")
	}
	r.replied(conn, status.CallId)
	defer r.unapplied(conn, status.CallId, &err)
	c, ok, err := r.inbound(op.ctx, conn, status.CallId)
	if err != nil || !ok {
		return err
	}
	return r.status(op.ctx, c, status)
}

// unapplied answers the waiting queries of call id whose CallQuery went out
// on conn when the daemon's reply could not be applied: a store write or
// read failed (ErrStoreUnavailable: an active call falls back to its durable
// state, a completed one reports the store failure), or the reply was
// invalid (ErrDeviceOffline: the link closes, the daemon cannot answer).
// Waiters on other connections keep waiting for their own reply. A result
// already answered them (F19), so this reaches only unanswered waiters.
func (r *router) unapplied(conn gateway.Connection, id string, err *error) {
	if *err == nil {
		return
	}
	code := ErrStoreUnavailable
	if protocolErr := (*wire.ProtocolError)(nil); errors.As(*err, &protocolErr) {
		code = ErrDeviceOffline
	}
	for _, q := range r.queries[id] {
		if !q.answered && q.conn == conn.Target.ConnectionID {
			q.answered = true
			q.reply <- queryReply{err: code}
		}
	}
}

func (r *router) status(ctx context.Context, c store.Call, s *circusv1.CallStatus) error {
	switch s.State {
	case circusv1.CallStatus_STATE_COMPLETED:
		if s.Result == nil || s.Result.CallId != s.CallId {
			return protocol("completed status lacks matching result")
		}
		return r.complete(ctx, c, s.Result)
	case circusv1.CallStatus_STATE_LOST:
		if c.State == store.CallCompleted {
			r.answer(c.CallID, nil, ErrResultUnavailable)
			return nil
		}
		if c.State.Terminal() {
			return nil
		}
		return r.settle(ctx, c, store.CallLost, store.CallPatch{FinishedAt: r.config.Clock.Now().UnixMilli(), OutcomeReason: "RESULT_UNAVAILABLE"}, lost(c.CallID, "RESULT_UNAVAILABLE"))
	case circusv1.CallStatus_STATE_RUNNING:
		if c.State.Terminal() {
			if c.State == store.CallCompleted {
				r.answer(c.CallID, nil, ErrResultUnavailable)
			}
			return nil
		}
		now := r.config.Clock.Now().UnixMilli()
		err := r.config.Store.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
			if c.State == store.CallRunning {
				// A duplicate RUNNING keeps the first ack and only rearms the
				// watchdog of the still-running attempt.
				current, err := tx.GetCall(ctx, c.CallID)
				if err != nil || current.State != store.CallRunning || current.DispatchedBootID != c.DispatchedBootID {
					return err
				}
			} else {
				patch := store.CallPatch{}
				if c.AckedAt == 0 {
					patch.AckedAt = now
				}
				changed, err := tx.TransitionCall(ctx, c.CallID, store.CallExpect{States: []store.CallState{c.State}, BootID: c.DispatchedBootID}, store.CallRunning, patch)
				if err != nil || !changed {
					return err
				}
			}
			return tx.SetTimer(ctx, timers.CallWatchdog, c.CallID, now+r.watchdog(nil).Milliseconds())
		})
		if err != nil {
			r.storeFailed(err)
			return err
		}
		r.markHealthy()
		r.release(c.CallID)
		if c.State != store.CallRunning {
			r.publish(dispatched(c.CallID))
		}
		r.answer(c.CallID, dispatched(c.CallID), nil)
		return nil
	default:
		return protocol("invalid call status")
	}
}

func (r *router) complete(ctx context.Context, c store.Call, v *circusv1.DeviceResult) error {
	if v.Output == nil {
		return protocol("result lacks output")
	}
	if v.Fault != nil {
		if _, ok := circusv1.DaemonFault_Code_name[int32(v.Fault.Code)]; !ok || v.Fault.Code == circusv1.DaemonFault_CODE_UNSPECIFIED {
			return protocol("invalid fault code")
		}
	}
	if c.State.Terminal() {
		if c.State == store.CallCompleted {
			r.answer(c.CallID, resultUpdate(v), nil)
		}
		return nil
	}
	now := r.config.Clock.Now().UnixMilli()
	patch := store.CallPatch{AckedAt: now, FinishedAt: now, IsError: v.Output.IsError, OutcomeReason: "COMPLETED"}
	if v.Fault != nil {
		patch.FaultCode = v.Fault.Code.String()
	}
	if b := v.GetBash(); b != nil {
		exit := b.ExitCode
		patch.ExitCode = &exit
		patch.JobID = b.JobId
	}
	err := r.settle(ctx, c, store.CallCompleted, patch, resultUpdate(v))
	if err != nil {
		// F19: the content is authoritative even while metadata storage is
		// broken. The durable watchdog keeps a query repair path.
		r.release(c.CallID)
		u := resultUpdate(v)
		r.publish(u)
		r.answer(c.CallID, u, nil)
	}
	return err
}
