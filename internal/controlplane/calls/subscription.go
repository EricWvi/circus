package calls

import (
	"context"
	"io"
	"strings"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/wire"
)

type queuedUpdate struct {
	update *circusv1.CallUpdate
	charge *charge
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

func (r *router) newSub(ctx context.Context, user, id string) (*Subscription, error) {
	if r.subscriptionFull(user) {
		return nil, ErrOverloaded
	}
	s := &Subscription{owner: r, user: strings.Clone(user), callID: strings.Clone(id), changed: make(chan struct{}), lastRead: r.config.Clock.Now()}
	r.subs[s] = struct{}{}
	r.callbacks.Add(1)
	r.callbackCount++
	r.changeSlots(s.user, 1)
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
	for _, q := range s.queue {
		r.releaseCharge(q.charge)
	}
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
	r.changeSlots(s.user, -1)
	r.callbacks.Done()
}

// enqueue charges and queues one owned, immutable update. An update that does
// not fit detaches only this subscriber with ErrOverflow.
func (r *router) enqueue(s *Subscription, owned *circusv1.CallUpdate, c *charge, chargeErr error) {
	if _, ok := r.subs[s]; !ok || s.terminal {
		return
	}
	if chargeErr != nil || len(s.queue) >= r.config.MaxUpdatesPerSubscription || c.cost > r.config.MaxSubscriptionBytes-s.bytes || !r.acquireCharge(s.user, c) {
		r.detach(s, ErrOverflow)
		return
	}
	if len(s.queue) == 0 {
		s.lastRead = r.config.Clock.Now()
	}
	s.queue = append(s.queue, queuedUpdate{update: owned, charge: c})
	s.bytes += c.cost
	s.terminal = wire.Terminal(owned) || s.single
	s.signal()
}

// charge is the global buffer charge of one owned, immutable update,
// shared by queued subscription entries and unconsumed query replies.
// Its refs and the router's shared total are protected by r.mu.
type charge struct {
	user string
	cost int64
	refs int
}

func (r *router) acquireCharge(user string, c *charge) bool {
	if c.refs == 0 {
		usage := r.users[user]
		if c.cost > min(r.config.MaxBufferedUpdateBytes-r.bufferedBytes, r.config.MaxBufferedUpdateBytes/2-usage.bufferedBytes) {
			return false
		}
		c.user = user
		r.bufferedBytes += c.cost
		usage.bufferedBytes += c.cost
		r.setUsage(user, usage)
	} else if c.user != user {
		return false
	}
	c.refs++
	return true
}

func (r *router) releaseCharge(c *charge) {
	if c == nil {
		return
	}
	c.refs--
	if c.refs == 0 {
		r.bufferedBytes -= c.cost
		usage := r.users[c.user]
		usage.bufferedBytes -= c.cost
		r.setUsage(c.user, usage)
	}
}

// own computes u's charge and clones it once. Router-side holders share the
// immutable clone and one global charge; subscription limits count each holder.
func (r *router) own(u *circusv1.CallUpdate) (*circusv1.CallUpdate, *charge, error) {
	cost, err := wire.Charge(u, r.config.MaxMessageBytes)
	if err != nil {
		return nil, nil, err
	}
	return wire.CloneOwned(u).(*circusv1.CallUpdate), &charge{cost: cost}, nil
}
func (r *router) publish(u *circusv1.CallUpdate) {
	for s := range r.subs {
		if s.callID == u.CallId {
			owned, c, err := r.own(u)
			r.publishOwned(u.CallId, owned, c, err)
			return
		}
	}
}

func (r *router) publishOwned(id string, owned *circusv1.CallUpdate, c *charge, err error) {
	for s := range r.subs {
		if s.callID == id {
			r.enqueue(s, owned, c, err)
		}
	}
}

// publishAndAnswer shares one clone and global charge across subscriptions and
// query waiters of this update. With no holders, it avoids cloning entirely.
func (r *router) publishAndAnswer(u *circusv1.CallUpdate) {
	if r.leader(u.CallId) == nil {
		r.publish(u)
		return
	}
	owned, c, err := r.own(u)
	r.publishOwned(u.CallId, owned, c, err)
	if err != nil {
		err = ErrOverflow
	}
	r.answerOwned(u.CallId, owned, c, err)
}

func (r *router) once(ctx context.Context, user string, u *circusv1.CallUpdate) (*Subscription, error) {
	s, err := r.newSub(ctx, user, u.CallId)
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
			s.bytes -= q.charge.cost
			r.releaseCharge(q.charge)
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
func (s *Subscription) wait(ctx context.Context) (int64, error) {
	if s == nil || s.owner == nil {
		return 0, nil // Next reports ErrClosed at once
	}
	r := s.owner
	for {
		r.mu.Lock()
		var cost int64
		if len(s.queue) > 0 {
			cost = s.queue[0].charge.cost
		}
		ready := len(s.queue) > 0 || s.err != nil || s.terminal
		changed := s.changed
		r.mu.Unlock()
		if ready {
			return cost, nil
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
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
