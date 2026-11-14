package calls

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/controlplane/bus"
	"github.com/EricWvi/circus/internal/controlplane/gateway"
	"github.com/EricWvi/circus/internal/controlplane/timers"
	"github.com/EricWvi/circus/internal/identifier"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/wire"
	"google.golang.org/protobuf/proto"
)

type queryReply struct {
	update *circusv1.CallUpdate
	err    error
	cost   int64
}

// queryWaiter is one Query (or duplicate Submit of a completed call) awaiting
// the daemon. conn is the connection its CallQuery was sent on; empty means
// deferred: the device's route is synchronizing and its sync sends or answers
// the query (M1). A disconnect of conn, or of the syncing route, fails it.
type queryWaiter struct {
	reply    chan queryReply
	device   string
	boot     string // the call's dispatch boot
	conn     string
	answered bool
}

func boundedName(s string) bool { return len(s) > 0 && len(s) <= 128 }

func isActive(s store.CallState) bool {
	return s == store.CallDispatched || s == store.CallRunning || s == store.CallUnknown
}

func (r *router) Submit(ctx context.Context, user, name string, call *circusv1.DeviceCall) (*Subscription, error) {
	op, err := r.enter(ctx, false, "submit")
	if err != nil {
		return nil, err
	}
	defer op.finish()
	id := call.GetCallId()
	if !identifier.Valid(id, false) {
		return r.once(ctx, rejected("", circusv1.CallUpdate_Rejected_CODE_INVALID))
	}
	if !identifier.Valid(call.GetSessionId(), false) || !identifier.Valid(call.GetAgentId(), true) || toolName(call) == "" || !boundedName(user) || !boundedName(name) {
		return r.once(ctx, rejected(id, circusv1.CallUpdate_Rejected_CODE_INVALID))
	}
	envelope := &circusv1.WorkerUplink{Payload: &circusv1.WorkerUplink_SubmitCall{SubmitCall: &circusv1.SubmitCall{Device: name, Call: call}}}
	cost, err := bus.Charge(envelope, r.config.MaxMessageBytes)
	if err != nil {
		if errors.Is(err, wire.ErrTooLarge) {
			// Inferred: D43 bounds input; large Write must be split by the caller.
			return r.once(ctx, resultUpdate(&circusv1.DeviceResult{CallId: id, Output: &circusv1.ToolOutput{IsError: true, Content: []*circusv1.ContentBlock{{Block: &circusv1.ContentBlock_Text{Text: "Error: content too large; write in chunks"}}}}}))
		}
		return r.once(ctx, rejected(id, circusv1.CallUpdate_Rejected_CODE_INVALID))
	}
	device, err := r.config.Store.GetIdentityByName(op.ctx, store.KindDevice, user, name)
	if errors.Is(err, store.ErrNotFound) {
		return r.once(ctx, rejected(id, circusv1.CallUpdate_Rejected_CODE_UNKNOWN_DEVICE))
	}
	if err != nil {
		r.storeFailed(err)
		return nil, errors.Join(ErrStoreUnavailable, err)
	}
	// Look up existing calls before scratch admission so equal duplicates can
	// attach without new pending memory. New calls still budget scratch before
	// Marshal; existing calls must marshal to verify their deterministic hash.
	scratch := int64(2 * proto.Size(call))
	old, lookupErr := r.config.Store.GetCall(op.ctx, id)
	if lookupErr != nil && !errors.Is(lookupErr, store.ErrNotFound) {
		r.storeFailed(lookupErr)
		return nil, errors.Join(ErrStoreUnavailable, lookupErr)
	}
	if errors.Is(lookupErr, store.ErrNotFound) && scratch > r.config.Pending.MaxBytes-r.pendingBytes {
		return r.once(ctx, rejected(id, circusv1.CallUpdate_Rejected_CODE_OVERLOADED))
	}
	data, err := proto.MarshalOptions{Deterministic: true}.Marshal(call)
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256(data)
	if lookupErr == nil {
		if old.UserID != user {
			return nil, store.ErrNotFound // never disclose another user's call
		}
		if old.DeviceID != device.ID || !bytes.Equal(old.RequestHash, hash[:]) {
			return r.once(ctx, rejected(id, circusv1.CallUpdate_Rejected_CODE_CALL_MISMATCH))
		}
		if old.State != store.CallLost || old.DispatchedBootID != "" {
			return r.attach(op, ctx, old)
		}
		// An undispatched lost call starts a new attempt and needs scratch.
		if scratch > r.config.Pending.MaxBytes-r.pendingBytes {
			return r.once(ctx, rejected(id, circusv1.CallUpdate_Rejected_CODE_OVERLOADED))
		}
	}
	route, online, err := r.config.Registry.Lookup(op.ctx, device.ID)
	if err != nil {
		return nil, err
	}
	now := r.config.Clock.Now().UnixMilli()
	offlineAt := int64(0)
	if !online && device.CurrentBootID != "" {
		offlineAt = r.offlineSince(device)
	}
	waiting := online || (device.CurrentBootID != "" && now < offlineAt+r.config.GracePeriod.Milliseconds())
	if waiting && (len(r.pending) >= r.config.Pending.MaxCalls || cost+scratch > r.config.Pending.MaxBytes-r.pendingBytes) {
		return r.once(ctx, rejected(id, circusv1.CallUpdate_Rejected_CODE_OVERLOADED))
	}
	s, err := r.newSub(ctx, id)
	if err != nil {
		return nil, err
	}
	row := store.Call{CallID: id, UserID: user, DeviceID: device.ID, SessionID: call.SessionId, AgentID: call.AgentId, Tool: toolName(call), RequestHash: hash[:], RequestBytes: int64(len(data)), State: store.CallAccepted, SubmittedAt: now}
	if waiting {
		r.pending[strings.Clone(id)] = &pendingCall{call: bus.CloneOwned(call).(*circusv1.DeviceCall), cost: cost}
		r.pendingBytes += cost
	}
	initialDispatch := false
	err = r.config.Store.Tx(op.ctx, func(ctx context.Context, tx store.Ops) error {
		if lookupErr == nil {
			ok, err := tx.TransitionCall(ctx, id, store.CallExpect{States: []store.CallState{store.CallLost}, Undispatched: true}, store.CallAccepted, store.CallPatch{SubmittedAt: now})
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("calls: resubmit lost its precondition")
			}
			if !waiting {
				_, err := tx.TransitionCall(ctx, id, store.CallExpect{States: []store.CallState{store.CallAccepted}}, store.CallOffline, store.CallPatch{FinishedAt: now, OutcomeReason: "DEVICE_OFFLINE"})
				if err != nil {
					return err
				}
			}
		} else {
			if !waiting {
				row.State = store.CallOffline
				row.FinishedAt = now
				row.OutcomeReason = "DEVICE_OFFLINE"
			}
			if err := tx.CreateCall(ctx, row); err != nil {
				return err
			}
		}
		if !online {
			// A row left connected by a previous process starts its grace now.
			return r.recordOffline(ctx, tx, device, offlineAt)
		}
		if waiting && route.Ready {
			var err error
			row, initialDispatch, err = r.commitDispatch(ctx, tx, row, route)
			return err
		}
		return nil
	})
	if err != nil {
		r.storeFailed(err)
		r.release(id)
		r.detach(s, ErrStoreUnavailable)
		// Prove that no attempt committed before advertising a safe same-ID retry.
		persisted, e := r.config.Store.GetCall(op.ctx, id)
		if errors.Is(e, store.ErrNotFound) || (e == nil && lookupErr == nil && persisted.State == store.CallLost && persisted.DispatchedBootID == "") {
			return r.once(ctx, rejected(id, circusv1.CallUpdate_Rejected_CODE_STORE_UNAVAILABLE))
		}
		return nil, errors.Join(ErrStoreUnavailable, err)
	}
	r.markHealthy()
	if !online {
		delete(r.unrecorded, device.ID)
	}
	if !waiting {
		owned, c, e := r.own(r.offline(op.ctx, id, user))
		r.enqueue(s, owned, c, e)
		return s, nil
	}
	owned, c, e := r.own(accepted(id))
	r.enqueue(s, owned, c, e)
	if initialDispatch {
		r.publish(dispatched(id))
		r.writeDispatch(op.ctx, row, route, false)
	}
	return s, nil
}

// attach answers an equal duplicate with the existing call's state; it never
// starts a new execution. A completed call fetches its result like Query.
func (r *router) attach(op *operation, ctx context.Context, old store.Call) (*Subscription, error) {
	if old.State == store.CallCompleted {
		u, err := r.query(op, old)
		if err != nil {
			return nil, err
		}
		return r.once(ctx, u)
	}
	s, err := r.once(ctx, r.update(op.ctx, old))
	if err == nil && isActive(old.State) {
		if route, ok := r.readyRoute(op.ctx, old.DeviceID, old.DispatchedBootID); ok {
			logSendFailure("query", old.CallID, r.sendQuery(op.ctx, route, old.CallID))
		}
	}
	return s, err
}

func (r *router) watchdog(call *circusv1.DeviceCall) time.Duration {
	timeout := 2 * time.Minute
	if call != nil && call.TimeoutMs > 0 {
		timeout = min(time.Duration(call.TimeoutMs)*time.Millisecond, 10*time.Minute)
	}
	return timeout + 5*time.Second + r.config.WatchdogSlack + r.config.DeliveryTimeout
}

// commitDispatch is the guarded A → D in the caller's Tx. It reads the
// device's current boot in that Tx and refuses a route serving another boot.
func (r *router) commitDispatch(ctx context.Context, tx store.Ops, c store.Call, route gateway.Connection) (store.Call, bool, error) {
	device, err := tx.GetIdentity(ctx, store.KindDevice, c.DeviceID)
	if err != nil {
		return c, false, err
	}
	if device.CurrentBootID != route.Target.BootID {
		return c, false, nil
	}
	now := r.config.Clock.Now().UnixMilli()
	ok, err := tx.TransitionCall(ctx, c.CallID, store.CallExpect{States: []store.CallState{store.CallAccepted}, Undispatched: true}, store.CallDispatched, store.CallPatch{DispatchedBootID: device.CurrentBootID, DispatchedAt: now})
	if err != nil || !ok {
		return c, false, err
	}
	var call *circusv1.DeviceCall
	if p := r.pending[c.CallID]; p != nil {
		call = p.call
	}
	if err = tx.SetTimer(ctx, timers.CallWatchdog, c.CallID, now+r.watchdog(call).Milliseconds()); err != nil {
		return c, false, err
	}
	c.State = store.CallDispatched
	c.DispatchedBootID = device.CurrentBootID
	c.DispatchedAt = now
	return c, true, nil
}

// writeDispatch sends DispatchCall after its commit. Every send carries the
// original committed dispatch time, including a resend. A first send that wire
// or bus proves was never queued settles the attempt: busy → OVERLOADED, too
// large → INVALID (L3). Other failures (no route, closed link) keep D; sync or
// the watchdog recovers.
func (r *router) writeDispatch(ctx context.Context, c store.Call, route gateway.Connection, resend bool) error {
	p, ok := r.pending[c.CallID]
	if !ok {
		return nil
	}
	err := r.config.Bus.Send(ctx, route.Target, &circusv1.DaemonDownlink{Payload: &circusv1.DaemonDownlink_DispatchCall{DispatchCall: &circusv1.DispatchCall{Call: p.call, TargetBootId: c.DispatchedBootID, DispatchedAtUnixMs: c.DispatchedAt}}})
	if resend {
		return err
	}
	now := r.config.Clock.Now().UnixMilli()
	switch {
	case errors.Is(err, wire.ErrBusy) || errors.Is(err, bus.ErrBusy):
		return r.settle(ctx, c, store.CallRejected, store.CallPatch{FinishedAt: now, OutcomeReason: "OVERLOADED"}, rejected(c.CallID, circusv1.CallUpdate_Rejected_CODE_OVERLOADED))
	case errors.Is(err, wire.ErrTooLarge):
		return r.settle(ctx, c, store.CallRejected, store.CallPatch{FinishedAt: now, OutcomeReason: "INVALID"}, rejected(c.CallID, circusv1.CallUpdate_Rejected_CODE_INVALID))
	}
	logSendFailure("dispatch", c.CallID, err)
	return nil
}

// dispatch commits A → D in its own Tx, then writes the DispatchCall.
func (r *router) dispatch(ctx context.Context, c store.Call, route gateway.Connection) error {
	var changed bool
	err := r.config.Store.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		var err error
		c, changed, err = r.commitDispatch(ctx, tx, c, route)
		return err
	})
	if err != nil {
		r.storeFailed(err)
		return err
	}
	r.markHealthy()
	if changed {
		r.publish(dispatched(c.CallID))
		return r.writeDispatch(ctx, c, route, false)
	}
	return nil
}

func (r *router) Cancel(ctx context.Context, user, id string) error {
	op, err := r.enter(ctx, false, "cancel")
	if err != nil {
		return err
	}
	defer op.finish()
	if !identifier.Valid(id, false) {
		return store.ErrInvalid
	}
	c, err := r.config.Store.GetCall(op.ctx, id)
	if err != nil {
		return err
	}
	if c.UserID != user {
		return store.ErrNotFound
	}
	if c.State.Terminal() {
		return nil
	}
	now := r.config.Clock.Now().UnixMilli()
	if c.State == store.CallAccepted {
		return r.settle(op.ctx, c, store.CallCancelled, store.CallPatch{CancelRequestedAt: now, FinishedAt: now, OutcomeReason: "CANCELLED"}, cancelled(id))
	}
	err = r.config.Store.Tx(op.ctx, func(ctx context.Context, tx store.Ops) error {
		ok, err := tx.PatchCall(ctx, id, store.CallExpect{States: active, BootID: c.DispatchedBootID}, store.CallPatch{CancelRequestedAt: now})
		if err != nil || !ok {
			return err
		}
		// A barrier ahead of the cancel can delay it; see DESIGN.md timing.
		return tx.SetTimer(ctx, timers.CallWatchdog, id, now+(r.config.DeliveryTimeout+5*time.Second+r.config.WatchdogSlack).Milliseconds())
	})
	if err != nil {
		r.storeFailed(err)
		return err
	}
	// An unready route's sync replays recorded intent after its resend/query.
	if route, ok := r.readyRoute(op.ctx, c.DeviceID, c.DispatchedBootID); ok {
		return r.sendCancel(op.ctx, route, id)
	}
	return nil
}

func (r *router) Query(ctx context.Context, user, id string) (*circusv1.CallUpdate, error) {
	op, err := r.enter(ctx, false, "query")
	if err != nil {
		return nil, err
	}
	defer op.finish()
	if !identifier.Valid(id, false) {
		return nil, store.ErrInvalid
	}
	c, err := r.config.Store.GetCall(op.ctx, id)
	if err != nil {
		return nil, err
	}
	if c.UserID != user {
		return nil, store.ErrNotFound
	}
	if c.State == store.CallAccepted || (c.State.Terminal() && c.State != store.CallCompleted) {
		return r.update(op.ctx, c), nil
	}
	return r.query(op, c)
}

// unreachable answers when the daemon cannot be asked now: a completed call
// reports the transient ErrDeviceOffline, others their durable state.
func (r *router) unreachable(ctx context.Context, c store.Call, cause error) (*circusv1.CallUpdate, error) {
	if c.State == store.CallCompleted {
		return nil, errors.Join(ErrDeviceOffline, cause)
	}
	if current, err := r.config.Store.GetCall(ctx, c.CallID); err == nil {
		c = current
	}
	return r.update(ctx, c), nil
}

// leader returns the unanswered waiter whose query later waiters share.
func (r *router) leader(id string) *queryWaiter {
	for _, w := range r.queries[id] {
		if !w.answered {
			return w
		}
	}
	return nil
}

// query asks the dispatch boot's daemon about c and waits for the answer. At
// most one CallQuery per call is outstanding; later waiters coalesce. On an
// unready route the waiter is deferred to that route's sync (M1).
func (r *router) query(op *operation, c store.Call) (*circusv1.CallUpdate, error) {
	route, online, err := r.config.Registry.Lookup(op.ctx, c.DeviceID)
	if err != nil {
		return nil, err
	}
	if !online || route.Target.BootID != c.DispatchedBootID {
		return r.unreachable(op.ctx, c, nil)
	}
	if r.queryCount+r.callbackCount >= r.config.MaxSubscriptions {
		return nil, ErrOverloaded
	}
	q := &queryWaiter{reply: make(chan queryReply, 1), device: strings.Clone(c.DeviceID), boot: strings.Clone(c.DispatchedBootID)}
	if lead := r.leader(c.CallID); lead != nil {
		q.conn = lead.conn
	} else if route.Ready {
		if err := r.sendQuery(op.ctx, route, c.CallID); err != nil {
			return r.unreachable(op.ctx, c, err)
		}
		q.conn = strings.Clone(route.Target.ConnectionID)
	}
	r.queries[c.CallID] = append(r.queries[c.CallID], q)
	r.queryCount++
	// A waiting query owns a bounded waiter, not an execution slot. Keep one
	// slot available for the inbound reply even with MaxOperations == 1.
	<-r.operations
	op.slot = false
	op.unlock()
	var response queryReply
	select {
	case response = <-q.reply:
	case <-op.ctx.Done():
		response.err = op.ctx.Err()
	}
	op.lock()
	// If cancellation raced a delivered reply, release the queued ownership too.
	if response.cost == 0 {
		select {
		case queued := <-q.reply:
			r.bufferedBytes -= queued.cost
		default:
		}
	} else {
		r.bufferedBytes -= response.cost
	}
	r.removeWaiter(c.CallID, q)
	if r.closed {
		return nil, ErrClosed
	}
	if errors.Is(response.err, ErrDeviceOffline) || (errors.Is(response.err, ErrStoreUnavailable) && c.State != store.CallCompleted) {
		return r.unreachable(op.ctx, c, nil)
	}
	return response.update, response.err
}
func (r *router) removeWaiter(id string, q *queryWaiter) {
	list := r.queries[id]
	for i, v := range list {
		if v == q {
			copy(list[i:], list[i+1:])
			list[len(list)-1] = nil
			list = list[:len(list)-1]
			r.queryCount--
			break
		}
	}
	if len(list) == 0 {
		delete(r.queries, id)
	} else {
		r.queries[id] = list
	}
}

// failQueries answers waiters whose CallQuery went out on connection conn,
// and, when the device's current route ends, its deferred waiters too.
func (r *router) failQueries(device, conn string, current bool) {
	for _, list := range r.queries {
		for _, q := range list {
			if !q.answered && q.device == device && (q.conn == conn || (current && q.conn == "")) {
				q.answered = true
				q.reply <- queryReply{err: ErrDeviceOffline}
			}
		}
	}
}

// answer replies to every waiter of id with one shared, immutable update.
func (r *router) answer(id string, u *circusv1.CallUpdate, err error) {
	var owned *circusv1.CallUpdate
	var cost int64
	var chargeErr error
	charged := false
	for _, q := range r.queries[id] {
		if q.answered {
			continue
		}
		q.answered = true
		response := queryReply{err: err}
		if u != nil {
			if !charged {
				owned, cost, chargeErr = r.own(u)
				charged = true
			}
			if chargeErr != nil || cost > r.config.MaxBufferedUpdateBytes-r.bufferedBytes {
				response.err = ErrOverflow
			} else {
				response.update = owned
				response.cost = cost
				r.bufferedBytes += cost
			}
		}
		q.reply <- response
	}
}

func (r *router) StopSessionJobs(ctx context.Context, user, session string) error {
	op, err := r.enter(ctx, false, "stop-session-jobs")
	if err != nil {
		return err
	}
	defer op.finish()
	if !identifier.Valid(session, false) {
		return store.ErrInvalid
	}
	// Immutable key order keeps one previous device ID instead of an unbounded set.
	var after *store.JobCursor
	previous := ""
	var failures error
	for {
		rows, err := r.config.Store.ListJobs(op.ctx, store.JobFilter{SessionID: session, States: []store.JobState{store.JobRunning}, Limit: pageSize, After: after})
		if err != nil {
			return err
		}
		for _, j := range rows {
			if j.UserID != user || j.DeviceID == previous {
				continue
			}
			previous = j.DeviceID
			route, ok := r.readyRoute(op.ctx, j.DeviceID, j.BootID)
			if !ok {
				failures = ErrDeviceOffline
				continue
			}
			err = r.config.Bus.Send(op.ctx, route.Target, &circusv1.DaemonDownlink{Payload: &circusv1.DaemonDownlink_StopSessionJobs{StopSessionJobs: &circusv1.StopSessionJobs{SessionId: session}}})
			if err != nil {
				failures = err
			}
		}
		if len(rows) < pageSize {
			return failures
		}
		j := rows[len(rows)-1]
		after = &store.JobCursor{DeviceID: j.DeviceID, BootID: j.BootID, JobID: j.JobID}
	}
}
