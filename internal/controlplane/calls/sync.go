package calls

import (
	"context"
	"errors"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/controlplane/gateway"
	"github.com/EricWvi/circus/internal/controlplane/timers"
	"github.com/EricWvi/circus/internal/store"
)

// errSyncStalled ends a sync whose daemon stopped answering sync requests.
var errSyncStalled = errors.New("calls: daemon did not answer synchronization requests")

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
		// replies it waits for need one.
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
				// Capacity waits release the admission lock and can cross the
				// horizon. Recheck now; late replay releases its content and
				// recovers by query instead of sending another DispatchCall.
				elapsed = r.config.Clock.Now().UnixMilli() - c.DispatchedAt
				if elapsed >= 0 && elapsed < r.config.ResendHorizon.Milliseconds() {
					return r.writeDispatch(op.ctx, c, conn, true)
				}
				r.release(c.CallID)
				return r.sendQuery(op.ctx, conn, c.CallID)
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
	if err = r.config.Sender.Deliver(op.ctx, conn.Target, &circusv1.DaemonDownlink{Payload: &circusv1.DaemonDownlink_ListJobs{ListJobs: &circusv1.ListJobs{}}}); err != nil {
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
	// deferred leader that this loop then sends.
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
