package calls

import (
	"context"
	"errors"

	"github.com/EricWvi/circus/internal/controlplane/gateway"
	"github.com/EricWvi/circus/internal/controlplane/timers"
	"github.com/EricWvi/circus/internal/store"
)

// timerEffects are the post-commit effects of one handled timer.
type timerEffects struct {
	changes []store.Call // at most pageSize committed notifications
	query   *store.Call  // watchdog query to send on a ready route
	route   gateway.Connection
	offline string // device whose unrecorded disconnect was persisted
}

func (r *router) HandleTimer(ctx context.Context, claim store.Timer) error {
	op, err := r.enter(ctx, true)
	if err != nil {
		return err
	}
	defer op.finish()
	if claim.Kind != timers.DeviceGrace && claim.Kind != timers.CallWatchdog {
		return store.ErrInvalid
	}
	var fx timerEffects
	err = r.config.Store.Tx(op.ctx, func(ctx context.Context, tx store.Ops) error {
		fx = timerEffects{}
		ok, err := tx.CompleteTimer(ctx, claim)
		if err != nil || !ok {
			return err
		}
		if claim.Kind == timers.DeviceGrace {
			return r.graceExpired(ctx, tx, claim.SubjectID, &fx)
		}
		return r.watchdogDue(ctx, tx, claim.SubjectID, &fx)
	})
	if err != nil {
		r.storeFailed(err)
		return err
	}
	r.markHealthy()
	if fx.offline != "" {
		delete(r.unrecorded, fx.offline)
	}
	for _, c := range fx.changes {
		if c.State.Terminal() {
			r.release(c.CallID)
		}
		u := r.update(op.ctx, c)
		r.publish(u)
		r.answer(c.CallID, u, nil)
	}
	if fx.query != nil {
		// The follow-up watchdog is already durable, so a lost reply only
		// delays recovery.
		logSendFailure("query", fx.query.CallID, r.sendQuery(op.ctx, fx.route, fx.query.CallID))
		if fx.query.CancelRequestedAt != 0 {
			logSendFailure("cancel", fx.query.CallID, r.sendCancel(op.ctx, fx.route, fx.query.CallID))
		}
	}
	return nil
}

// graceExpired runs in the claim's Tx. No registry check: OnConnected cancels
// this timer in its first Tx, so a successful claim proves no reconnect has
// synchronized; a registered but unsynchronized route must not swallow it.
// Accepted calls go offline; dispatched/running go unknown and keep a recovery
// watchdog bounded by the result-retention cutoff. A full page re-arms the
// timer as immediately due, so large sweeps drain in successive claims.
func (r *router) graceExpired(ctx context.Context, tx store.Ops, deviceID string, fx *timerEffects) error {
	device, err := tx.GetIdentity(ctx, store.KindDevice, deviceID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	now := r.config.Clock.Now().UnixMilli()
	offlineAt := r.offlineSince(device)
	rows, err := tx.ListCalls(ctx, store.CallFilter{DeviceID: deviceID, States: []store.CallState{store.CallAccepted, store.CallDispatched, store.CallRunning}, Limit: pageSize})
	if err != nil {
		return err
	}
	for _, c := range rows {
		to, reason, patch := store.CallUnknown, "CONNECTION_LOST", store.CallPatch{OutcomeReason: "CONNECTION_LOST"}
		if c.State == store.CallAccepted {
			to, reason = store.CallOffline, "DEVICE_OFFLINE"
			patch = store.CallPatch{OutcomeReason: reason, FinishedAt: now}
		}
		ok, err := tx.TransitionCall(ctx, c.CallID, store.CallExpect{States: []store.CallState{c.State}, BootID: c.DispatchedBootID}, to, patch)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		c.State, c.OutcomeReason = to, reason
		fx.changes = append(fx.changes, c)
		if to == store.CallOffline {
			err = tx.CancelTimer(ctx, timers.CallWatchdog, c.CallID)
		} else {
			err = tx.SetTimer(ctx, timers.CallWatchdog, c.CallID, min(now+r.watchdog(nil).Milliseconds(), offlineAt+resultRetention.Milliseconds()))
		}
		if err != nil {
			return err
		}
	}
	if len(rows) == pageSize {
		return tx.SetTimer(ctx, timers.DeviceGrace, deviceID, now)
	}
	return nil
}

// watchdogDue runs in the claim's Tx. It never infers success from time. A new
// boot loses the call; an offline device loses it only after the
// result-retention cutoff measured from when it went offline (for a row left
// connected by a previous process, from this process's start: M3). Otherwise
// it re-arms and, on a ready route of the dispatch boot, queries after commit.
func (r *router) watchdogDue(ctx context.Context, tx store.Ops, callID string, fx *timerEffects) error {
	c, err := tx.GetCall(ctx, callID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if c.State.Terminal() || c.State == store.CallAccepted {
		return nil
	}
	device, err := tx.GetIdentity(ctx, store.KindDevice, c.DeviceID)
	if err != nil {
		return err
	}
	_, online, err := r.config.Registry.Lookup(ctx, c.DeviceID)
	if err != nil {
		return err
	}
	now := r.config.Clock.Now().UnixMilli()
	reason := ""
	due := now + r.watchdog(nil).Milliseconds()
	if device.CurrentBootID != "" && device.CurrentBootID != c.DispatchedBootID {
		reason = "DAEMON_RESTARTED"
	} else if !online {
		offlineAt := r.offlineSince(device)
		if err = r.recordOffline(ctx, tx, device, offlineAt); err != nil {
			return err
		}
		fx.offline = device.ID
		if now-offlineAt >= resultRetention.Milliseconds() {
			reason = "RESULT_RETENTION_EXPIRED"
		}
		due = min(due, offlineAt+resultRetention.Milliseconds())
	}
	if reason != "" {
		ok, err := tx.TransitionCall(ctx, c.CallID, store.CallExpect{States: []store.CallState{c.State}, BootID: c.DispatchedBootID}, store.CallLost, store.CallPatch{FinishedAt: now, OutcomeReason: reason})
		if ok {
			c.State, c.OutcomeReason = store.CallLost, reason
			fx.changes = append(fx.changes, c)
		}
		return err
	}
	if route, ok := r.readyRoute(ctx, c.DeviceID, c.DispatchedBootID); ok {
		fx.query, fx.route = &c, route
	}
	// Persist the follow-up before the outbound query.
	return tx.SetTimer(ctx, timers.CallWatchdog, c.CallID, due)
}
