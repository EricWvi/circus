package calls

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
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
	charge *charge
}

// queryWaiter is one Query (or duplicate Submit of a completed call) awaiting
// the daemon. conn is the connection its CallQuery was sent on; empty means
// deferred: the device's route is synchronizing and its sync sends or answers
// the query (the sync-first rule). A disconnect of conn, or of the syncing route, fails it.
type queryWaiter struct {
	user     string
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

// refusal is a session admission refusal: the terminal rejection a
// SubmitCall gets instead of a call.
type refusal struct {
	code circusv1.CallUpdate_Rejected_Code
}

func (e refusal) Error() string { return "calls: session admission refused: " + e.code.String() }

// admissionCode maps an AdmitSessionCall refusal; ok is false for a store
// failure.
func admissionCode(err error) (circusv1.CallUpdate_Rejected_Code, bool) {
	switch {
	case errors.Is(err, store.ErrFenced), errors.Is(err, store.ErrNotFound):
		return circusv1.CallUpdate_Rejected_CODE_SESSION_FENCED, true
	case errors.Is(err, store.ErrStopped), errors.Is(err, store.ErrBroken):
		return circusv1.CallUpdate_Rejected_CODE_SESSION_STOPPED, true
	case errors.Is(err, store.ErrTooManyCalls):
		return circusv1.CallUpdate_Rejected_CODE_OVERLOADED, true
	case errors.Is(err, store.ErrInvalid):
		return circusv1.CallUpdate_Rejected_CODE_INVALID, true
	}
	return 0, false
}

func (r *router) Submit(ctx context.Context, user, name string, call *circusv1.DeviceCall) (*Subscription, error) {
	return r.submit(ctx, user, name, call, Admission{}, true)
}

func (r *router) SubmitSession(ctx context.Context, user, name string, call *circusv1.DeviceCall, a Admission) (*Subscription, error) {
	return r.submit(ctx, user, name, call, a, false)
}

func (r *router) submit(ctx context.Context, user, name string, call *circusv1.DeviceCall, a Admission, internal bool) (*Subscription, error) {
	op, err := r.enter(ctx, false, "submit")
	if err != nil {
		return nil, err
	}
	defer op.finish()
	id := call.GetCallId()
	if !identifier.Valid(id, false) {
		return r.once(ctx, user, rejected("", circusv1.CallUpdate_Rejected_CODE_INVALID))
	}
	tool := toolName(call)
	if !identifier.Valid(call.GetSessionId(), false) || !identifier.Valid(call.GetAgentId(), true) || tool == "" || !boundedName(user) || !boundedName(name) {
		return r.once(ctx, user, rejected(id, circusv1.CallUpdate_Rejected_CODE_INVALID))
	}
	prepare, remove := tool == "prepare_workspace", tool == "remove_workspace"
	workspaceID, synthetic := strings.CutPrefix(call.SessionId, store.WorkspaceSessionPrefix)
	if (prepare || remove) && (!internal || !synthetic || workspaceID == "") {
		return r.once(ctx, user, rejected(id, circusv1.CallUpdate_Rejected_CODE_INVALID))
	}
	// A ledger snapshot call comes from the control plane only, under the
	// session id "ledger-<snapshot id>" with a call id of its own prefix.
	snapshotID, snapshotCall := ledgerSnapshotID(call)
	if snapshotCall && (!internal || call.SessionId != store.LedgerSnapshotSessionPrefix+snapshotID || snapshotID == "" ||
		!strings.HasPrefix(id, store.LedgerSnapshotCallPrefix)) {
		return r.once(ctx, user, rejected(id, circusv1.CallUpdate_Rejected_CODE_INVALID))
	}
	// admit runs the session admission in the caller's Tx (or on its own
	// for an attach, which writes nothing).
	var admit func(ctx context.Context, tx store.Ops) error
	if a != (Admission{}) {
		if len(a.ToolUseID) > store.MaxIDBytes {
			return r.once(ctx, user, rejected(id, circusv1.CallUpdate_Rejected_CODE_INVALID))
		}
		if a.Epoch == 0 || a.Epoch > math.MaxInt64 || a.ToolUseID == "" || a.BootID == "" || a.ConnID == "" {
			return r.once(ctx, user, rejected(id, circusv1.CallUpdate_Rejected_CODE_SESSION_FENCED))
		}
		admit = func(ctx context.Context, tx store.Ops) error {
			f := store.Fence{SessionID: call.SessionId, Epoch: int64(a.Epoch), WorkerID: a.WorkerID, BootID: a.BootID, ConnID: a.ConnID, Now: r.config.Clock.Now().UnixMilli()}
			err := tx.AdmitSessionCall(ctx, f, id, MaxActiveSessionCalls)
			if code, ok := admissionCode(err); ok {
				return refusal{code}
			}
			return err
		}
	} else {
		// A call naming any existing session requires a lease. Use the
		// same refusal regardless of who owns it.
		_, err := r.config.Store.GetSession(op.ctx, call.SessionId, r.config.Clock.Now().UnixMilli())
		switch {
		case err == nil:
			return r.once(ctx, user, rejected(id, circusv1.CallUpdate_Rejected_CODE_SESSION_FENCED))
		case err != nil && !errors.Is(err, store.ErrNotFound):
			r.storeFailed(err)
			return nil, errors.Join(ErrStoreUnavailable, err)
		}
	}
	if snapshotCall {
		// The snapshot is of a session of the user (or of a deleted one
		// whose removal waits) on the named device.
		admit = func(ctx context.Context, tx store.Ops) error {
			owner, deviceID := "", ""
			if l, err := tx.GetSessionLedger(ctx, snapshotID); err == nil {
				owner, deviceID = l.UserID, l.DeviceID
			} else if !errors.Is(err, store.ErrNotFound) {
				return err
			} else if r, err := tx.GetLedgerSnapshotRemoval(ctx, snapshotID); err == nil {
				owner, deviceID = r.UserID, r.DeviceID
			} else if !errors.Is(err, store.ErrNotFound) {
				return err
			}
			if owner != user {
				return refusal{circusv1.CallUpdate_Rejected_CODE_INVALID}
			}
			d, err := tx.GetIdentityByName(ctx, store.KindDevice, user, name)
			if errors.Is(err, store.ErrNotFound) || (err == nil && d.ID != deviceID) {
				return refusal{circusv1.CallUpdate_Rejected_CODE_UNKNOWN_DEVICE}
			}
			return err
		}
	}
	if prepare || remove {
		admit = func(ctx context.Context, tx store.Ops) error {
			w, err := tx.GetWorkspaceByID(ctx, workspaceID)
			if errors.Is(err, store.ErrNotFound) || (err == nil && w.UserID != user) {
				return refusal{circusv1.CallUpdate_Rejected_CODE_INVALID}
			}
			if err != nil {
				return err
			}
			if (prepare && (w.State != store.WorkspacePreparing || w.CallID != id)) ||
				(remove && w.State != store.WorkspaceReady) {
				return refusal{circusv1.CallUpdate_Rejected_CODE_INVALID}
			}
			d, err := tx.GetIdentityByName(ctx, store.KindDevice, user, name)
			if errors.Is(err, store.ErrNotFound) || (err == nil && d.ID != w.DeviceID) {
				return refusal{circusv1.CallUpdate_Rejected_CODE_UNKNOWN_DEVICE}
			}
			return err
		}
	}
	envelope := &circusv1.WorkerUplink{Payload: &circusv1.WorkerUplink_SubmitCall{SubmitCall: &circusv1.SubmitCall{Device: name, Call: call}}}
	cost, err := wire.Charge(envelope, r.config.MaxMessageBytes)
	if err != nil {
		if errors.Is(err, wire.ErrTooLarge) {
			// Input is bounded by the message limit; a large Write must be split by the caller.
			return r.once(ctx, user, resultUpdate(&circusv1.DeviceResult{CallId: id, Output: &circusv1.ToolOutput{IsError: true, Content: []*circusv1.ContentBlock{{Block: &circusv1.ContentBlock_Text{Text: "Error: content too large; write in chunks"}}}}}))
		}
		return r.once(ctx, user, rejected(id, circusv1.CallUpdate_Rejected_CODE_INVALID))
	}
	device, err := r.config.Store.GetIdentityByName(op.ctx, store.KindDevice, user, name)
	if errors.Is(err, store.ErrNotFound) {
		return r.once(ctx, user, rejected(id, circusv1.CallUpdate_Rejected_CODE_UNKNOWN_DEVICE))
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
	if errors.Is(lookupErr, store.ErrNotFound) && scratch > r.pendingBytesAvailable(user) {
		return r.once(ctx, user, rejected(id, circusv1.CallUpdate_Rejected_CODE_OVERLOADED))
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
		if old.DeviceID != device.ID || !bytes.Equal(old.RequestHash, hash[:]) || old.ToolUseID != a.ToolUseID {
			return r.once(ctx, user, rejected(id, circusv1.CallUpdate_Rejected_CODE_CALL_MISMATCH))
		}
		if old.State != store.CallLost || old.DispatchedBootID != "" {
			if admit != nil {
				var ref refusal
				if err := admit(op.ctx, r.config.Store); errors.As(err, &ref) {
					return r.once(ctx, user, rejected(id, ref.code))
				} else if err != nil {
					r.storeFailed(err)
					return nil, errors.Join(ErrStoreUnavailable, err)
				}
			}
			return r.attach(op, ctx, old)
		}
		// An undispatched lost call starts a new attempt and needs scratch.
		if scratch > r.pendingBytesAvailable(user) {
			return r.once(ctx, user, rejected(id, circusv1.CallUpdate_Rejected_CODE_OVERLOADED))
		}
	}
	route, online, err := r.config.Registry.Lookup(op.ctx, device.ID)
	if err != nil {
		return nil, err
	}
	if remove && (!online || !route.Ready) {
		return r.once(ctx, user, r.offline(op.ctx, id, user))
	}
	now := r.config.Clock.Now().UnixMilli()
	offlineAt := int64(0)
	if !online && device.CurrentBootID != "" {
		offlineAt = r.offlineSince(device)
	}
	waiting := online || (device.CurrentBootID != "" && now < offlineAt+r.config.GracePeriod.Milliseconds())
	if waiting && (len(r.pending) >= r.config.Pending.MaxCalls || r.users[user].pendingCalls >= r.config.Pending.MaxCalls/2 || cost+scratch > r.pendingBytesAvailable(user)) {
		return r.once(ctx, user, rejected(id, circusv1.CallUpdate_Rejected_CODE_OVERLOADED))
	}
	s, err := r.newSub(ctx, user, id)
	if err != nil {
		return nil, err
	}
	row := store.Call{CallID: id, UserID: user, DeviceID: device.ID, SessionID: call.SessionId, AgentID: call.AgentId, Tool: tool, RequestHash: hash[:], RequestBytes: int64(len(data)), State: store.CallAccepted, SubmittedAt: now, ToolUseID: a.ToolUseID}
	if waiting {
		r.retain(user, id, call, cost)
	}
	initialDispatch := false
	err = r.config.Store.Tx(op.ctx, func(ctx context.Context, tx store.Ops) error {
		if admit != nil {
			if err := admit(ctx, tx); err != nil {
				return err
			}
		}
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
	if ref := (refusal{}); errors.As(err, &ref) {
		// Nothing was recorded; the store is healthy.
		r.release(id)
		r.detach(s, ErrClosed)
		return r.once(ctx, user, rejected(id, ref.code))
	}
	if err != nil {
		r.storeFailed(err)
		r.release(id)
		r.detach(s, ErrStoreUnavailable)
		// Prove that no attempt committed before advertising a safe same-ID retry.
		persisted, e := r.config.Store.GetCall(op.ctx, id)
		if errors.Is(e, store.ErrNotFound) || (e == nil && lookupErr == nil && persisted.State == store.CallLost && persisted.DispatchedBootID == "") {
			return r.once(ctx, user, rejected(id, circusv1.CallUpdate_Rejected_CODE_STORE_UNAVAILABLE))
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
		u, c, err := r.query(op, old)
		if err != nil {
			return nil, err
		}
		return r.onceAnswer(ctx, old.UserID, old.CallID, u, c)
	}
	s, err := r.once(ctx, old.UserID, r.update(op.ctx, old))
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
// proves was never queued settles the attempt: busy → OVERLOADED, too
// large → INVALID. Other failures (no route, closed link) keep D; sync or
// the watchdog recovers.
func (r *router) writeDispatch(ctx context.Context, c store.Call, route gateway.Connection, resend bool) error {
	p, ok := r.pending[c.CallID]
	if !ok {
		return nil
	}
	err := r.config.Sender.Deliver(ctx, route.Target, &circusv1.DaemonDownlink{Payload: &circusv1.DaemonDownlink_DispatchCall{DispatchCall: &circusv1.DispatchCall{Call: p.call, TargetBootId: c.DispatchedBootID, DispatchedAtUnixMs: c.DispatchedAt}}})
	if resend {
		return err
	}
	now := r.config.Clock.Now().UnixMilli()
	switch {
	case errors.Is(err, gateway.ErrFeatureUnsupported):
		return r.complete(ctx, c, &circusv1.DeviceResult{
			CallId: c.CallID, Output: &circusv1.ToolOutput{IsError: true},
			Fault: &circusv1.DaemonFault{Code: circusv1.DaemonFault_CODE_TOOL_UNAVAILABLE},
		})
	case errors.Is(err, wire.ErrBusy):
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
	// The intent is durable now: a delivery failure (no route, busy pool,
	// full queue) is recovered by that sync or the call watchdog, so it is
	// not the caller's error.
	if route, ok := r.readyRoute(op.ctx, c.DeviceID, c.DispatchedBootID); ok {
		logSendFailure("cancel", id, r.sendCancel(op.ctx, route, id))
	}
	return nil
}

func (r *router) Query(ctx context.Context, user, id string) (*circusv1.CallUpdate, error) {
	op, err := r.enter(ctx, false, "query")
	if err != nil {
		return nil, err
	}
	defer op.finish()
	u, c, err := r.queryLocked(op, user, id)
	r.releaseCharge(c)
	return u, err
}

func (r *router) QueryOnce(ctx context.Context, user, id string) (*Subscription, error) {
	op, err := r.enter(ctx, false, "query")
	if err != nil {
		return nil, err
	}
	defer op.finish()
	u, c, err := r.queryLocked(op, user, id)
	if err != nil {
		return nil, err
	}
	return r.onceAnswer(ctx, user, id, u, c)
}

// onceAnswer transfers a query reply to a single-update subscription while
// keeping its shared charge held continuously under r.mu.
func (r *router) onceAnswer(ctx context.Context, user, id string, u *circusv1.CallUpdate, c *charge) (*Subscription, error) {
	defer r.releaseCharge(c)
	if u == nil {
		return nil, fmt.Errorf("calls: missing query answer for %q", id)
	}
	s, err := r.newSub(ctx, user, id)
	if err != nil {
		return nil, err
	}
	s.single = true
	if c == nil {
		u, c, err = r.own(u)
	}
	r.enqueue(s, u, c, err)
	return s, nil
}

func (r *router) queryLocked(op *operation, user, id string) (*circusv1.CallUpdate, *charge, error) {
	if !identifier.Valid(id, false) {
		return nil, nil, store.ErrInvalid
	}
	c, err := r.config.Store.GetCall(op.ctx, id)
	if err != nil {
		return nil, nil, err
	}
	if c.UserID != user {
		return nil, nil, store.ErrNotFound
	}
	if c.State == store.CallAccepted || (c.State.Terminal() && c.State != store.CallCompleted) {
		return r.update(op.ctx, c), nil, nil
	}
	return r.query(op, c)
}

// unreachable answers when the daemon cannot be asked now: a completed call
// reports the transient ErrDeviceOffline, others their durable state.
func (r *router) unreachable(ctx context.Context, c store.Call, cause error) (*circusv1.CallUpdate, *charge, error) {
	if c.State == store.CallCompleted {
		return nil, nil, errors.Join(ErrDeviceOffline, cause)
	}
	if current, err := r.config.Store.GetCall(ctx, c.CallID); err == nil {
		c = current
	}
	if c.State == store.CallCompleted {
		return nil, nil, errors.Join(ErrDeviceOffline, cause)
	}
	return r.update(ctx, c), nil, nil
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
// unready route the waiter is deferred to that route's sync (the sync-first rule).
func (r *router) query(op *operation, c store.Call) (*circusv1.CallUpdate, *charge, error) {
	route, online, err := r.config.Registry.Lookup(op.ctx, c.DeviceID)
	if err != nil {
		return nil, nil, err
	}
	if !online || route.Target.BootID != c.DispatchedBootID {
		return r.unreachable(op.ctx, c, nil)
	}
	if r.subscriptionFull(c.UserID) {
		return nil, nil, ErrOverloaded
	}
	q := &queryWaiter{user: strings.Clone(c.UserID), reply: make(chan queryReply, 1), device: strings.Clone(c.DeviceID), boot: strings.Clone(c.DispatchedBootID)}
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
	r.changeSlots(q.user, 1)
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
	if response.charge == nil {
		select {
		case queued := <-q.reply:
			r.releaseCharge(queued.charge)
		default:
		}
	}
	r.removeWaiter(c.CallID, q)
	if r.closed {
		r.releaseCharge(response.charge)
		return nil, nil, ErrClosed
	}
	if response.err != nil {
		r.releaseCharge(response.charge)
		response.charge = nil
	}
	if errors.Is(response.err, ErrDeviceOffline) || (errors.Is(response.err, ErrStoreUnavailable) && c.State != store.CallCompleted) {
		return r.unreachable(op.ctx, c, nil)
	}
	return response.update, response.charge, response.err
}
func (r *router) removeWaiter(id string, q *queryWaiter) {
	list := r.queries[id]
	for i, v := range list {
		if v == q {
			copy(list[i:], list[i+1:])
			list[len(list)-1] = nil
			list = list[:len(list)-1]
			r.queryCount--
			r.changeSlots(q.user, -1)
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
	if r.leader(id) == nil {
		return
	}
	var c *charge
	if u != nil {
		var ownErr error
		u, c, ownErr = r.own(u)
		if ownErr != nil {
			err = ErrOverflow
		}
	}
	r.answerOwned(id, u, c, err)
}

// answerOwned gives each unanswered waiter a reference to the same owned update.
func (r *router) answerOwned(id string, owned *circusv1.CallUpdate, c *charge, err error) {
	for _, q := range r.queries[id] {
		if q.answered {
			continue
		}
		q.answered = true
		response := queryReply{err: err}
		if owned != nil {
			if !r.acquireCharge(q.user, c) {
				response.err = ErrOverflow
			} else {
				response.update = owned
				response.charge = c
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
		rows, err := r.config.Store.ListJobs(op.ctx, store.JobFilter{UserID: user, SessionID: session, States: []store.JobState{store.JobRunning}, Limit: pageSize, After: after})
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
			err = r.config.Sender.Deliver(op.ctx, route.Target, &circusv1.DaemonDownlink{Payload: &circusv1.DaemonDownlink_StopSessionJobs{StopSessionJobs: &circusv1.StopSessionJobs{SessionId: session}}})
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
