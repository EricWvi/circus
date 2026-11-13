package calls

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/controlplane/gateway"
	"github.com/EricWvi/circus/internal/controlplane/timers"
	"github.com/EricWvi/circus/internal/identifier"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/wire"
)

// errSyncStalled ends a sync whose daemon stopped answering sync requests.
var errSyncStalled = errors.New("calls: daemon did not answer synchronization requests")

func protocol(reason string) error { return &wire.ProtocolError{Reason: reason} }

// current reports whether c is the device's registered generation.
func (r *router) current(ctx context.Context, c gateway.Connection) bool {
	v, ok, err := r.config.Registry.Lookup(ctx, c.Target.DeviceID)
	return err == nil && ok && sameConnection(v, c)
}

// syncing returns the device state while d is still the current generation.
func (r *router) syncing(ctx context.Context, conn gateway.Connection, d *deviceState) bool {
	return r.devices[conn.Target.DeviceID] == d && r.current(ctx, conn)
}

// syncSend sends one sync request that the daemon answers (CallQuery, resent
// DispatchCall, ListJobs) once fewer than syncLimit are outstanding.
// The daemon closes its link when its send queue fills, so an unbounded
// resync batch would disconnect on every reconnect. Waiting releases the lock.
func (r *router) syncSend(op *operation, conn gateway.Connection, d *deviceState, key string, send func() error) error {
	for len(d.outstanding) >= r.syncLimit {
		changed := d.changed
		alarm := r.config.Clock.After(r.config.DeliveryTimeout)
		// Like a waiting Query, a waiting sync owns no operation slot: the
		// replies it waits for need one (review 2, M1).
		op.unlock()
		op.releaseSlot()
		var err error
		select {
		case <-changed:
		case <-alarm:
			err = errSyncStalled
		case <-op.ctx.Done():
			err = op.ctx.Err()
		}
		if err == nil {
			err = op.acquireSlot()
		}
		op.lock()
		if err != nil {
			return err
		}
		if !r.syncing(op.ctx, conn, d) {
			return errSuperseded
		}
	}
	d.outstanding[key] = struct{}{}
	return send()
}

// errSuperseded ends a sync whose generation was replaced; not a failure.
var errSuperseded = errors.New("calls: generation superseded")

// replied frees the sync slot of key once its answer arrives on conn.
func (r *router) replied(conn gateway.Connection, key string) {
	d := r.devices[conn.Target.DeviceID]
	if d == nil || d.connection.Target != conn.Target {
		return
	}
	if _, ok := d.outstanding[key]; ok {
		delete(d.outstanding, key)
		d.signal()
	}
}

func (r *router) OnConnected(ctx context.Context, conn gateway.Connection) error {
	op, err := r.enter(ctx, true, "connected")
	if err != nil {
		return err
	}
	defer op.finish()
	if !r.current(op.ctx, conn) {
		return nil
	}
	if len(r.devices) >= r.config.MaxDevices && r.devices[conn.Target.DeviceID] == nil {
		return ErrOverloaded
	}
	if old := r.devices[conn.Target.DeviceID]; old != nil {
		old.signal()
	}
	d := &deviceState{connection: cloneConnection(conn), snapshotPending: true, outstanding: make(map[string]struct{}, r.syncLimit), changed: make(chan struct{})}
	r.devices[d.connection.Target.DeviceID] = d
	err = r.synchronize(op, conn, d)
	if errors.Is(err, errSuperseded) {
		return nil
	}
	return err
}

// synchronize is the reconnect sequence (DESIGN.md "Connection ownership").
func (r *router) synchronize(op *operation, conn gateway.Connection, d *deviceState) error {
	device, boot := conn.Target.DeviceID, conn.Target.BootID
	if err := r.connectAndLose(op, conn, d); err != nil {
		return err
	}
	delete(r.unrecorded, device)
	// Same boot: resend unacked retained requests inside the horizon, else query.
	err := r.walkCalls(op, conn, d, store.CallFilter{DeviceID: device, DispatchedBootID: boot, States: active}, func(c store.Call) error {
		r.adoptDeferred(c.CallID, device, conn.Target.ConnectionID)
		p, retained := r.pending[c.CallID]
		elapsed := r.config.Clock.Now().UnixMilli() - c.DispatchedAt
		if retained && c.AckedAt == 0 && elapsed >= 0 && elapsed < r.config.ResendHorizon.Milliseconds() {
			return r.syncSend(op, conn, d, c.CallID, func() error {
				if r.pending[c.CallID] != p {
					delete(d.outstanding, c.CallID) // settled while waiting
					return nil
				}
				return r.writeDispatch(op.ctx, c, conn, true)
			})
		}
		if elapsed >= r.config.ResendHorizon.Milliseconds() {
			r.release(c.CallID)
		}
		return r.syncSend(op, conn, d, c.CallID, func() error { return r.sendQuery(op.ctx, conn, c.CallID) })
	})
	if err != nil {
		return err
	}
	// Reserve the ListJobs slot first: from here to Activate the lock is held
	// throughout, so a cancel intent recorded meanwhile cannot fall between
	// replay and readiness.
	if err = r.syncSend(op, conn, d, listJobsKey, func() error { return nil }); err != nil {
		return err
	}
	if err = r.replayCancels(op, conn); err != nil {
		return err
	}
	if err = r.config.Bus.Send(op.ctx, conn.Target, &circusv1.DaemonDownlink{Payload: &circusv1.DaemonDownlink_ListJobs{ListJobs: &circusv1.ListJobs{}}}); err != nil {
		return err
	}
	if !r.syncing(op.ctx, conn, d) {
		return errSuperseded
	}
	if ok, err := r.config.Registry.Activate(op.ctx, conn); err != nil || !ok {
		return err
	}
	conn.Ready = true
	// Accepted calls never dispatched: by submission time, then call ID.
	err = r.walkCalls(op, conn, d, store.CallFilter{DeviceID: device, States: []store.CallState{store.CallAccepted}}, func(c store.Call) error {
		if _, ok := r.pending[c.CallID]; ok {
			return r.dispatch(op.ctx, c, conn)
		}
		// Content this process does not hold (an ambiguous failed submission)
		// can never dispatch; never dispatched, it may be resubmitted.
		return r.settle(op.ctx, c, store.CallLost, store.CallPatch{FinishedAt: r.config.Clock.Now().UnixMilli(), OutcomeReason: "REQUEST_UNAVAILABLE"}, lost(c.CallID, "REQUEST_UNAVAILABLE"))
	})
	if err != nil {
		return err
	}
	// Queries deferred while the route was unready and not answered by the
	// replay (for example completed calls) get one bounded CallQuery each.
	// The route is ready, so a new Query sends directly or coalesces onto a
	// deferred leader that this loop then sends (review 2, L2).
	return r.flushDeferred(op, conn, d)
}

// connectAndLose records the connection and, for a new boot, loses the old
// boots' jobs and calls. SetConnected and the first loss batch are atomic;
// later batches drain the same set idempotently. Every reconnect drains, so a
// crash between batches is repaired even when the boot was already recorded.
func (r *router) connectAndLose(op *operation, conn gateway.Connection, d *deviceState) error {
	first := true
	for {
		if !r.syncing(op.ctx, conn, d) {
			return errSuperseded
		}
		now := r.config.Clock.Now().UnixMilli()
		var losses []store.LostCall
		err := r.config.Store.Tx(op.ctx, func(ctx context.Context, tx store.Ops) error {
			if first {
				if _, err := tx.SetConnected(ctx, store.KindDevice, conn.Target.DeviceID, conn.Target.BootID, now); err != nil {
					return err
				}
				if err := tx.CancelTimer(ctx, timers.DeviceGrace, conn.Target.DeviceID); err != nil {
					return err
				}
				if _, err := tx.LoseJobs(ctx, conn.Target.DeviceID, conn.Target.BootID, now); err != nil {
					return err
				}
			}
			var err error
			losses, err = tx.LoseCalls(ctx, conn.Target.DeviceID, conn.Target.BootID, now, "DAEMON_RESTARTED", pageSize)
			if err != nil {
				return err
			}
			for _, l := range losses {
				if err := tx.CancelTimer(ctx, timers.CallWatchdog, l.CallID); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			r.storeFailed(err)
			return err
		}
		r.markHealthy()
		for _, l := range losses {
			r.release(l.CallID)
			u := lost(l.CallID, "DAEMON_RESTARTED")
			r.publish(u)
			r.answer(l.CallID, u, nil)
		}
		first = false
		if len(losses) < pageSize {
			return nil
		}
		op.yield()
	}
}

// walkCalls visits filter's calls in keyset pages, yielding between items.
func (r *router) walkCalls(op *operation, conn gateway.Connection, d *deviceState, filter store.CallFilter, visit func(store.Call) error) error {
	filter.Limit = pageSize
	for {
		if !r.syncing(op.ctx, conn, d) {
			return errSuperseded
		}
		rows, err := r.config.Store.ListCalls(op.ctx, filter)
		if err != nil {
			r.storeFailed(err)
			return err
		}
		for _, c := range rows {
			if !r.syncing(op.ctx, conn, d) {
				return errSuperseded
			}
			if err = visit(c); err != nil {
				return err
			}
			op.yield()
		}
		if len(rows) < pageSize {
			return nil
		}
		c := rows[len(rows)-1]
		filter.After = &store.CallCursor{SubmittedAt: c.SubmittedAt, CallID: c.CallID}
	}
}

// adoptDeferred assigns the call's deferred waiters to conn: the sync's own
// resend or query for that call answers them.
func (r *router) adoptDeferred(id, device, conn string) {
	for _, q := range r.queries[id] {
		if !q.answered && q.conn == "" && q.device == device {
			q.conn = conn
		}
	}
}

// flushDeferred sends one CallQuery per call with waiters deferred on this
// device that the replay did not cover (for example completed calls), each
// within the sync bound. It runs after Activate; waiting releases the lock.
func (r *router) flushDeferred(op *operation, conn gateway.Connection, d *deviceState) error {
	for {
		id := ""
		for callID, list := range r.queries {
			for _, q := range list {
				if !q.answered && q.conn == "" && q.device == conn.Target.DeviceID {
					id = callID
					break
				}
			}
			if id != "" {
				break
			}
		}
		if id == "" {
			return nil
		}
		if mismatched := r.failDeferredBoot(id, conn); mismatched {
			continue
		}
		r.adoptDeferred(id, conn.Target.DeviceID, conn.Target.ConnectionID)
		if err := r.syncSend(op, conn, d, id, func() error { return r.sendQuery(op.ctx, conn, id) }); err != nil {
			return err
		}
	}
}

// failDeferredBoot fails deferred waiters of id that wait for another boot
// than conn serves (their route was replaced before it synchronized); the
// daemon of conn cannot answer for them.
func (r *router) failDeferredBoot(id string, conn gateway.Connection) bool {
	failed := false
	for _, q := range r.queries[id] {
		if !q.answered && q.conn == "" && q.device == conn.Target.DeviceID && q.boot != conn.Target.BootID {
			q.answered = true
			q.reply <- queryReply{err: ErrDeviceOffline}
			failed = true
		}
	}
	return failed
}

// replayCancels resends every recorded cancel intent of this boot after its
// call's resend or query; CancelCall is idempotent. It holds the lock.
func (r *router) replayCancels(op *operation, conn gateway.Connection) error {
	filter := store.CallFilter{DeviceID: conn.Target.DeviceID, DispatchedBootID: conn.Target.BootID, States: active, Limit: pageSize}
	for {
		rows, err := r.config.Store.ListCalls(op.ctx, filter)
		if err != nil {
			r.storeFailed(err)
			return err
		}
		for _, c := range rows {
			if c.CancelRequestedAt != 0 {
				if err = r.sendCancel(op.ctx, conn, c.CallID); err != nil {
					return err
				}
			}
		}
		if len(rows) < pageSize {
			return nil
		}
		c := rows[len(rows)-1]
		filter.After = &store.CallCursor{SubmittedAt: c.SubmittedAt, CallID: c.CallID}
	}
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
		// H1: only a DispatchCall sent exactly once, by this process, on this
		// connection, proves the daemon never saw the call. Any other refusal
		// may follow an execution: keep D and recover by query or boot change.
		if p := r.pending[c.CallID]; p == nil || p.resent || p.sentOn != conn.Target.ConnectionID {
			slog.Warn("daemon shutting down refused a possibly delivered dispatch; recovering", "call_id", c.CallID, "device_id", c.DeviceID)
			return nil
		}
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
