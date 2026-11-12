package calls

import (
	"context"
	"io"
	"strings"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/controlplane/bus"
	"github.com/EricWvi/circus/internal/store"
)

type queuedUpdate struct {
	update *circusv1.CallUpdate
	cost   int64
}

func rejected(id string, code circusv1.CallUpdate_Rejected_Code) *circusv1.CallUpdate {
	return &circusv1.CallUpdate{CallId: id, Update: &circusv1.CallUpdate_Rejected_{Rejected: &circusv1.CallUpdate_Rejected{Code: code}}}
}
func accepted(id string) *circusv1.CallUpdate {
	return &circusv1.CallUpdate{CallId: id, Update: &circusv1.CallUpdate_Accepted_{Accepted: &circusv1.CallUpdate_Accepted{}}}
}
func dispatched(id string) *circusv1.CallUpdate {
	return &circusv1.CallUpdate{CallId: id, Update: &circusv1.CallUpdate_Dispatched_{Dispatched: &circusv1.CallUpdate_Dispatched{}}}
}
func cancelled(id string) *circusv1.CallUpdate {
	return &circusv1.CallUpdate{CallId: id, Update: &circusv1.CallUpdate_Cancelled_{Cancelled: &circusv1.CallUpdate_Cancelled{}}}
}
func resultUpdate(v *circusv1.DeviceResult) *circusv1.CallUpdate {
	return &circusv1.CallUpdate{CallId: v.CallId, Update: &circusv1.CallUpdate_Result{Result: v}}
}
func lost(id, reason string) *circusv1.CallUpdate {
	return &circusv1.CallUpdate{CallId: id, Update: &circusv1.CallUpdate_Lost_{Lost: &circusv1.CallUpdate_Lost{Reason: reason}}}
}
func terminal(u *circusv1.CallUpdate) bool {
	return u.GetResult() != nil || u.GetOffline() != nil || u.GetLost() != nil || u.GetRejected() != nil || u.GetCancelled() != nil
}
func toolName(c *circusv1.DeviceCall) string {
	switch c.Tool.(type) {
	case *circusv1.DeviceCall_Bash:
		return "bash"
	case *circusv1.DeviceCall_Read:
		return "read"
	case *circusv1.DeviceCall_Write:
		return "write"
	case *circusv1.DeviceCall_Edit:
		return "edit"
	case *circusv1.DeviceCall_TaskStop:
		return "task_stop"
	}
	return ""
}

// offline lists the user's online device names (bounded by the registry).
func (r *router) offline(ctx context.Context, id, user string) *circusv1.CallUpdate {
	routes, _ := r.config.Registry.ListOnline(ctx, user)
	names := make([]string, 0, len(routes))
	for _, v := range routes {
		names = append(names, v.DeviceName)
	}
	return &circusv1.CallUpdate{CallId: id, Update: &circusv1.CallUpdate_Offline{Offline: &circusv1.CallUpdate_DeviceOffline{OnlineDevices: names}}}
}

// update maps durable state to the worker update; running maps to Dispatched
// because CallUpdate has no Running variant.
func (r *router) update(ctx context.Context, c store.Call) *circusv1.CallUpdate {
	switch c.State {
	case store.CallAccepted:
		return accepted(c.CallID)
	case store.CallDispatched, store.CallRunning:
		return dispatched(c.CallID)
	case store.CallUnknown:
		return &circusv1.CallUpdate{CallId: c.CallID, Update: &circusv1.CallUpdate_Unknown_{Unknown: &circusv1.CallUpdate_Unknown{Reason: c.OutcomeReason}}}
	case store.CallOffline:
		return r.offline(ctx, c.CallID, c.UserID)
	case store.CallCancelled:
		return cancelled(c.CallID)
	case store.CallLost:
		return lost(c.CallID, c.OutcomeReason)
	case store.CallRejected:
		code := circusv1.CallUpdate_Rejected_Code(circusv1.CallUpdate_Rejected_Code_value["CODE_"+c.OutcomeReason])
		if code == 0 {
			code = circusv1.CallUpdate_Rejected_CODE_INVALID
		}
		return rejected(c.CallID, code)
	default:
		return nil
	}
}

func (r *router) newSub(ctx context.Context, id string) (*Subscription, error) {
	if r.callbackCount+r.queryCount >= r.config.MaxSubscriptions {
		return nil, ErrOverloaded
	}
	s := &Subscription{owner: r, callID: strings.Clone(id), changed: make(chan struct{}), lastRead: r.config.Clock.Now()}
	r.subs[s] = struct{}{}
	r.callbacks.Add(1)
	r.callbackCount++
	s.stopContext = context.AfterFunc(ctx, func() { r.mu.Lock(); defer r.mu.Unlock(); r.detach(s, ErrClosed); r.finishContext(s) })
	return s, nil
}
func (s *Subscription) signal() { close(s.changed); s.changed = make(chan struct{}) }

// detach ends a subscription, dropping its queue and buffer charges.
func (r *router) detach(s *Subscription, err error) {
	if _, ok := r.subs[s]; !ok {
		return
	}
	delete(r.subs, s)
	r.bufferedBytes -= s.bytes
	s.bytes = 0
	s.queue = nil
	s.err = err
	s.terminal = true
	s.signal()
	if s.stopContext != nil && s.stopContext() {
		r.finishContext(s)
	}
}
func (r *router) finishContext(s *Subscription) {
	if s.contextDone {
		return
	}
	s.contextDone = true
	r.callbackCount--
	r.callbacks.Done()
}

// enqueue charges and queues one owned, immutable update. An update that does
// not fit detaches only this subscriber with ErrOverflow.
func (r *router) enqueue(s *Subscription, owned *circusv1.CallUpdate, cost int64, chargeErr error) {
	if _, ok := r.subs[s]; !ok || s.terminal {
		return
	}
	if chargeErr != nil || len(s.queue) >= r.config.MaxUpdatesPerSubscription || cost > r.config.MaxSubscriptionBytes-s.bytes || cost > r.config.MaxBufferedUpdateBytes-r.bufferedBytes {
		r.detach(s, ErrOverflow)
		return
	}
	if len(s.queue) == 0 {
		s.lastRead = r.config.Clock.Now()
	}
	s.queue = append(s.queue, queuedUpdate{update: owned, cost: cost})
	s.bytes += cost
	r.bufferedBytes += cost
	s.terminal = terminal(owned)
	s.signal()
}

// own charges u and clones it once; subscribers share the immutable clone
// while each is charged for it separately (L7).
func (r *router) own(u *circusv1.CallUpdate) (*circusv1.CallUpdate, int64, error) {
	cost, err := bus.Charge(u, r.config.MaxMessageBytes)
	if err != nil {
		return nil, 0, err
	}
	return bus.CloneOwned(u).(*circusv1.CallUpdate), cost, nil
}
func (r *router) publish(u *circusv1.CallUpdate) {
	var owned *circusv1.CallUpdate
	var cost int64
	var err error
	charged := false
	for s := range r.subs {
		if s.callID != u.CallId {
			continue
		}
		if !charged {
			owned, cost, err = r.own(u)
			charged = true
		}
		r.enqueue(s, owned, cost, err)
	}
}
func (r *router) once(ctx context.Context, u *circusv1.CallUpdate) (*Subscription, error) {
	s, err := r.newSub(ctx, u.CallId)
	if err != nil {
		return nil, err
	}
	owned, cost, err := r.own(u)
	r.enqueue(s, owned, cost, err)
	return s, nil
}
func (s *Subscription) next(ctx context.Context) (*circusv1.CallUpdate, error) {
	if s == nil || s.owner == nil {
		return nil, ErrClosed
	}
	r := s.owner
	for {
		r.mu.Lock()
		if len(s.queue) > 0 {
			q := s.queue[0]
			s.queue[0] = queuedUpdate{}
			s.queue = s.queue[1:]
			s.bytes -= q.cost
			r.bufferedBytes -= q.cost
			s.lastRead = r.config.Clock.Now()
			if len(s.queue) == 0 && s.terminal {
				r.detach(s, nil)
			}
			r.mu.Unlock()
			return q.update, nil
		}
		err, done, changed := s.err, s.terminal, s.changed
		r.mu.Unlock()
		if err != nil {
			return nil, err
		}
		if done {
			return nil, io.EOF
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-changed:
		}
	}
}
func (s *Subscription) close() error {
	if s == nil || s.owner == nil {
		return nil
	}
	r := s.owner
	r.mu.Lock()
	defer r.mu.Unlock()
	r.detach(s, ErrClosed)
	return nil
}
