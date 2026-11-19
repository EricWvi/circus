package sessions

import (
	"context"
	"errors"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/store"
)

// reasonSet is a set of Nudge reasons, one bit per reason value.
type reasonSet uint8

const allReasons = reasonSet(1<<circusv1.WorkerEvent_Nudge_REASON_INPUT | 1<<circusv1.WorkerEvent_Nudge_REASON_JOB |
	1<<circusv1.WorkerEvent_Nudge_REASON_INTERRUPT | 1<<circusv1.WorkerEvent_Nudge_REASON_STOP |
	1<<circusv1.WorkerEvent_Nudge_REASON_APPROVAL)

func reasonsOf(rs ...circusv1.WorkerEvent_Nudge_Reason) reasonSet {
	var out reasonSet
	for _, r := range rs {
		if r > 0 && r < 8 {
			out |= 1 << r
		}
	}
	return out & allReasons
}

func (r reasonSet) list() []circusv1.WorkerEvent_Nudge_Reason {
	var out []circusv1.WorkerEvent_Nudge_Reason
	for v := circusv1.WorkerEvent_Nudge_REASON_INPUT; v <= circusv1.WorkerEvent_Nudge_REASON_APPROVAL; v++ {
		if r&(1<<v) != 0 {
			out = append(out, v)
		}
	}
	return out
}

const (
	reasonInput     = reasonSet(1 << circusv1.WorkerEvent_Nudge_REASON_INPUT)
	reasonJob       = reasonSet(1 << circusv1.WorkerEvent_Nudge_REASON_JOB)
	reasonInterrupt = reasonSet(1 << circusv1.WorkerEvent_Nudge_REASON_INTERRUPT)
	reasonStop      = reasonSet(1 << circusv1.WorkerEvent_Nudge_REASON_STOP)
	reasonApproval  = reasonSet(1 << circusv1.WorkerEvent_Nudge_REASON_APPROVAL)
)

// holder is a lease as this process last saw it in the store.
type holder struct {
	conn, worker, boot string
	epoch              int64
}

// entry is the in-memory state of one tracked session: the lease binding
// this process knows of, an outstanding Offer, and the pending Nudge
// reasons (level-triggered: each stays pending until the holder has acted
// on it, and every pending reason is re-sent after Attach and ResumeLeases).
type entry struct {
	user    string
	holder  holder // zero when no live lease is known
	bound   *Conn  // the live connection named by holder.conn, if any
	offered *Conn
	// offeredAt is when the Offer was made (Unix ms).
	offeredAt int64
	reasons   reasonSet
	// gen is, per reason, the Wake generation that last set it. An
	// operation clears a reason only if it was set no later than the
	// generation the operation observed before reading the store, so a
	// reason raised by a commit the operation did not see stays pending.
	gen       [8]uint64
	interrupt int64
	// pending: the lease was acquired and its Lease reply has not reached
	// the link yet. It is extended while its AcquireLease operation is
	// open; abandoned: that operation ended without delivering the reply
	// (unwatched, send failed, link closed), so the worker never learned
	// the lease and it is no longer extended (it expires).
	pending, abandoned bool
	// token identifies the acquisition pending and abandoned belong to;
	// 0 after a resumption.
	token uint64
	// known: this process has bound the lease itself (AcquireLease,
	// ResumeLeases, or a store lease found bound to one of its live
	// connections). A resumption of a lease not known is the first one
	// after a restart, whoever created the entry.
	known bool
	// reserved counts AcquireLease and ResumeLeases operations that hold
	// the entry while their transaction runs; a reserved entry is never
	// forgotten.
	reserved int
}

// bit returns the reason value of a one-reason set.
func bit(r reasonSet) int {
	for v := 1; v < 8; v++ {
		if r == 1<<v {
			return v
		}
	}
	return 0
}

// Wake implements Waker.
func (s *Service) Wake(sessionID string, reasons ...circusv1.WorkerEvent_Nudge_Reason) {
	rs := reasonsOf(reasons...)
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[sessionID]
	if e != nil {
		s.addReasons(e, rs)
	}
	if !s.queueWake(sessionID, rs) && e != nil {
		// The Wake queue is full: nudge a bound holder at once with the
		// level it last saw, rather than hold its hint back.
		s.nudge(sessionID, e)
	}
}

// queueWake schedules session id for evaluation and reports whether it is
// queued; s.mu held. Past MaxWakeQueue it is not: the fallback scan finds
// queued sessions, and Wake nudges tracked ones directly.
func (s *Service) queueWake(id string, rs reasonSet) bool {
	if _, ok := s.wakes[id]; !ok && len(s.wakes) >= MaxWakeQueue {
		return false
	}
	s.wakes[id] |= rs
	s.signal()
	return true
}

func (s *Service) signal() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// addReasons raises reasons on e at a new generation; s.mu held.
func (s *Service) addReasons(e *entry, rs reasonSet) {
	if rs == 0 {
		return
	}
	s.gen++
	for v := 1; v < 8; v++ {
		if rs&(1<<v) != 0 {
			e.gen[v] = s.gen
		}
	}
	e.reasons |= rs
}

// generation returns the current Wake generation, for clearReasons.
func (s *Service) generation() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gen
}

// clearReasons clears rs on session id's entry where they were set no
// later than generation g and the entry's lease is still epoch.
func (s *Service) clearReasons(id string, epoch int64, rs reasonSet, g uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[id]
	if e == nil || e.holder.epoch != epoch {
		return
	}
	for v := 1; v < 8; v++ {
		if rs&(1<<v) != 0 && e.gen[v] <= g {
			e.reasons &^= 1 << v
		}
	}
}

// track returns session id's entry, creating it unless the table is full;
// s.mu held.
func (s *Service) track(id, user string) *entry {
	e := s.entries[id]
	if e == nil {
		if len(s.entries) >= s.maxTracked {
			s.log.Warn("session table full; session not tracked", "session_id", id)
			return nil
		}
		e = &entry{}
		s.entries[id] = e
	}
	if user != "" {
		e.user = user
	}
	return e
}

// forget drops e when it holds nothing; s.mu held.
func (s *Service) forget(id string, e *entry) {
	if e.holder == (holder{}) && e.offered == nil && e.bound == nil && e.reserved == 0 {
		delete(s.entries, id)
	}
}

func (s *Service) setBound(e *entry, c *Conn) {
	if e.bound == c {
		return
	}
	if e.bound != nil {
		e.bound.leases--
		s.freed(e.bound, 1)
	}
	e.bound = c
	if c != nil {
		c.leases++
	}
}

func (s *Service) setOffer(e *entry, c *Conn) {
	if e.offered == c {
		return
	}
	if e.offered != nil {
		e.offered.offers--
		s.freed(e.offered, 1)
	}
	e.offered = c
	if c != nil {
		c.offers++
		e.offeredAt = s.nowMs()
	}
}

// freed notes that c's load fell by n: when that took it from full to not
// full, its user's queued sessions are offered again; s.mu held.
func (s *Service) freed(c *Conn, n int) {
	load := c.leases + c.offers
	if c.attached && !c.closed && load < c.max && load+n >= c.max {
		s.rescan[c.userID] = struct{}{}
		s.signal()
	}
}

// bind records that session id's lease l is held through connection c (nil
// when that connection is not live here); s.mu held. A different lease
// than the one recorded before means the old holder lost it: the old
// connection gets LeaseRevoked unless revokeOld is false.
func (s *Service) bind(id string, e *entry, l store.Lease, c *Conn, revokeOld bool) {
	h := holder{conn: l.ConnID, worker: l.WorkerID, boot: l.BootID, epoch: l.Epoch}
	old := e.holder
	if old != (holder{}) && (old.epoch != h.epoch || old.worker != h.worker || old.boot != h.boot) {
		s.unbind(id, e, revokeOld)
	}
	e.holder = h
	// pending and abandoned belong to an acquisition (its token): only
	// AcquireLease and ResumeLeases, the two ways a lease is bound to a
	// connection of this process, set or reset them.
	s.setBound(e, c)
	s.setOffer(e, nil)
}

// unbind forgets e's lease; s.mu held. With revoke the connection it was
// bound to gets LeaseRevoked for its epoch.
func (s *Service) unbind(id string, e *entry, revoke bool) {
	if revoke && e.holder.epoch > 0 {
		c := e.bound
		if c == nil {
			c = s.conns[e.holder.conn]
		}
		if c != nil {
			epoch := e.holder.epoch
			c.queue(id, func(h *hint) { h.revoke = epoch })
		}
	}
	s.setBound(e, nil)
	e.holder = holder{}
	e.reasons, e.interrupt = 0, 0
	e.pending, e.abandoned, e.known, e.token = false, false, false, 0
}

// liveConn returns the registered connection with id, if it belongs to
// worker; s.mu held.
func (s *Service) liveConn(id, worker string) *Conn {
	if c := s.conns[id]; c != nil && c.workerID == worker && !c.closed {
		return c
	}
	return nil
}

// nudge queues a Nudge for e's pending reasons on its bound connection;
// s.mu held. Before Attach it waits in the connection's outbox.
func (s *Service) nudge(id string, e *entry) {
	if e.bound != nil && e.reasons != 0 {
		e.bound.queue(id, func(h *hint) { h.nudge = true })
	}
}

// lost applies a committed lease loss: the boundary event is announced,
// the binding forgotten, the holder's connection told (unless it caused the
// loss itself: initiator), and the session offered again when it has work
// (mayOffer; false where the same transaction gave the lease to someone).
func (s *Service) lost(l store.LeaseLoss, initiator *Conn, mayOffer bool) {
	if l.Repeated {
		return
	}
	id := l.Released.SessionID
	if l.Interrupted && l.Session.LastEventSeq > 0 {
		s.cfg.Bus.Durable(id, uint64(l.Session.LastEventSeq))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	revoke := initiator == nil || initiator.id != l.Released.ConnID
	e := s.entries[id]
	switch {
	case e != nil && e.holder.epoch == l.Released.Epoch && e.holder.worker == l.Released.WorkerID:
		s.unbind(id, e, revoke)
	case revoke:
		if c := s.liveConn(l.Released.ConnID, l.Released.WorkerID); c != nil {
			epoch := l.Released.Epoch
			c.queue(id, func(h *hint) { h.revoke = epoch })
		}
	}
	if mayOffer && l.Session.State == store.SessionQueued {
		s.queueWake(id, 0)
	} else if e != nil {
		s.forget(id, e)
	}
}

// schedule runs the scheduler until ctx ends: it evaluates queued Wakes
// and rescans users. It never runs the lease upkeep, so a long backlog of
// scheduling work cannot delay lease extension or expiry.
func (s *Service) schedule(ctx context.Context) {
	defer s.loops.Done()
	for {
		s.processWakes(ctx)
		select {
		case <-ctx.Done():
			return
		case <-s.kick:
		}
	}
}

// upkeep runs the lease upkeep on its own timer until ctx ends: expiry
// every SweepInterval, extension every ExtendInterval and, every
// ScheduleInterval, the session_schedule fallback (handed to the
// scheduler) and the approval expiry. It starts with alarm, armed by Start
// for SweepInterval after now.
func (s *Service) upkeep(ctx context.Context, now time.Time, alarm <-chan time.Time) {
	defer s.loops.Done()
	next := [3]time.Time{now.Add(SweepInterval), now.Add(ExtendInterval), now.Add(ScheduleInterval)}
	for {
		select {
		case <-ctx.Done():
			return
		case <-alarm:
		}
		now = s.clk.Now()
		s.mu.Lock()
		off := s.noUpkeep
		s.mu.Unlock()
		if off {
			next = [3]time.Time{now.Add(SweepInterval), now.Add(ExtendInterval), now.Add(ScheduleInterval)}
		}
		if !now.Before(next[0]) {
			s.expire(ctx)
			next[0] = now.Add(SweepInterval)
		}
		if !now.Before(next[1]) {
			s.extend(ctx)
			next[1] = now.Add(ExtendInterval)
		}
		if !now.Before(next[2]) {
			s.fallback(ctx)
			next[2] = now.Add(ScheduleInterval)
		}
		soonest := next[0]
		for _, t := range next[1:] {
			if t.Before(soonest) {
				soonest = t
			}
		}
		alarm = s.clk.After(soonest.Sub(now))
		s.rounds.Add(1)
	}
}

// processWakes evaluates every queued Wake and rescans users whose
// connections attached or went from full to not full.
func (s *Service) processWakes(ctx context.Context) {
	for ctx.Err() == nil {
		s.mu.Lock()
		wakes, users := s.wakes, s.rescan
		if len(wakes) == 0 && len(users) == 0 {
			s.mu.Unlock()
			return
		}
		s.wakes, s.rescan = make(map[string]reasonSet), make(map[string]struct{})
		s.busy++
		s.mu.Unlock()
		for id, rs := range wakes {
			s.evaluate(ctx, id, rs)
		}
		for u := range users {
			s.rescanUser(ctx, u)
		}
		s.mu.Lock()
		s.busy--
		s.mu.Unlock()
	}
}

// evaluate re-derives what session id needs from the store: a Nudge to
// its holder, an Offer to a worker, a LeaseRevoked to a holder that lost it,
// or nothing.
func (s *Service) evaluate(ctx context.Context, id string, rs reasonSet) {
	now := s.nowMs()
	tctx, cancel := storeCtx(ctx)
	st, err := s.cfg.Store.SettleSession(tctx, id, now)
	cancel()
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrInvalid) {
		s.mu.Lock()
		if e := s.entries[id]; e != nil {
			s.unbind(id, e, true)
			s.setOffer(e, nil)
			s.forget(id, e)
		}
		s.mu.Unlock()
		return
	}
	if err != nil {
		if ctx.Err() == nil {
			s.log.Warn("session scheduling read failed; the fallback retries", "session_id", id, "error", err)
		}
		return
	}
	if st.Loss != nil {
		s.lost(*st.Loss, nil, false)
	}
	// Re-raising rs here gives it a newer generation than the Wake's, so
	// an operation that started between the Wake and now no longer clears
	// it: at worst one spurious Nudge, never a lost one.
	s.mu.Lock()
	defer s.mu.Unlock()
	if st.Lease.Live(now) {
		e := s.track(id, st.Session.UserID)
		if e == nil {
			return
		}
		live := s.liveConn(st.Lease.ConnID, st.Lease.WorkerID)
		s.bind(id, e, st.Lease, live, true)
		if live != nil {
			e.known = true
		}
		e.interrupt = st.Session.InterruptTurnNo
		if st.Session.State == store.SessionStopped {
			rs |= reasonStop
		}
		s.addReasons(e, rs)
		s.nudge(id, e)
		return
	}
	e := s.entries[id]
	if e != nil && e.holder != (holder{}) {
		s.unbind(id, e, true)
	}
	if st.Session.State == store.SessionQueued {
		s.offer(id, st.Session)
		return
	}
	if e != nil {
		s.setOffer(e, nil)
		s.forget(id, e)
	}
}

// offer offers a queued session without a live lease to its pinned worker,
// else to the user's attached, non-full connection with the fewest leases;
// with none it stays queued until a connection attaches or frees capacity.
// An Offer stays with its connection for ScheduleInterval. s.mu held.
func (s *Service) offer(id string, sess store.Session) {
	e := s.entries[id]
	now := s.nowMs()
	if e != nil && e.offered != nil && !e.offered.closed && !s.stale(e.offered) && e.offered.attached && now < e.offeredAt+ScheduleInterval.Milliseconds() {
		return
	}
	var best *Conn
	for _, c := range s.conns {
		if !s.candidate(c, sess, e) {
			continue
		}
		if best == nil || c.leases+c.offers < best.leases+best.offers ||
			(c.leases+c.offers == best.leases+best.offers && c.seq > best.seq) {
			best = c
		}
	}
	if best == nil {
		if e != nil {
			s.setOffer(e, nil)
			s.forget(id, e)
		}
		return
	}
	if e == nil {
		if e = s.track(id, sess.UserID); e == nil {
			return
		}
	}
	s.setOffer(e, best)
	e.offeredAt = now
	best.queue(id, func(h *hint) { h.offer = true })
}

// candidate reports whether connection c may get an Offer of sess (whose
// entry, if tracked, is e); s.mu held.
func (s *Service) candidate(c *Conn, sess store.Session, e *entry) bool {
	if c.closed || s.stale(c) || !c.attached || c.userID != sess.UserID || (sess.PinnedWorkerID != "" && c.workerID != sess.PinnedWorkerID) {
		return false
	}
	load := c.leases + c.offers
	if e != nil && e.offered == c {
		load--
	}
	return load < c.max
}

// offerable reports whether session sess, listed as queued, needs and can
// get an Offer now: it has none still reserved, and some connection may
// take it; s.mu held.
func (s *Service) offerable(sess store.Session) bool {
	e := s.entries[sess.ID]
	if e != nil && e.offered != nil && !e.offered.closed && !s.stale(e.offered) && e.offered.attached &&
		s.nowMs() < e.offeredAt+ScheduleInterval.Milliseconds() {
		return false
	}
	for _, c := range s.conns {
		if s.candidate(c, sess, e) {
			return true
		}
	}
	return false
}

// capacity reports whether any connection of user can take an Offer;
// s.mu held.
func (s *Service) capacity(user string) bool {
	for _, c := range s.conns {
		if !c.closed && !s.stale(c) && c.attached && c.userID == user && c.leases+c.offers < c.max {
			return true
		}
	}
	return false
}

// rescanUser offers the user's queued sessions, read in pages (at most the
// store's per-user session cap). It is incremental: it stops as soon as no
// connection of the user has capacity, and it evaluates (a store
// transaction) only sessions that need and can get an Offer.
func (s *Service) rescanUser(ctx context.Context, user string) {
	var after *store.SessionCursor
	for ctx.Err() == nil {
		tctx, cancel := storeCtx(ctx)
		page, err := s.cfg.Store.ListSessions(tctx, store.SessionFilter{UserID: user, States: []store.SessionState{store.SessionQueued},
			Now: s.nowMs(), Limit: store.MaxPageSize, After: after})
		cancel()
		if err != nil {
			if ctx.Err() == nil {
				s.log.Warn("queued session scan failed; the fallback retries", "error", err)
			}
			return
		}
		for _, sess := range page {
			s.mu.Lock()
			more, offerable := s.capacity(user), s.offerable(sess)
			s.mu.Unlock()
			if !more {
				return
			}
			if offerable {
				s.evaluate(ctx, sess.ID, 0)
			}
		}
		if len(page) < store.MaxPageSize {
			return
		}
		last := page[len(page)-1]
		after = &store.SessionCursor{LastActivityAt: last.LastActivityAt, ID: last.ID}
	}
}

// fallback is the session_schedule timer: it asks the scheduler to
// re-offer the queued sessions of every user with an attached connection,
// including Offers not taken within ScheduleInterval, and expires
// approvals and grants.
func (s *Service) fallback(ctx context.Context) {
	s.mu.Lock()
	if !s.noFallback {
		// Offers not taken within ScheduleInterval are withdrawn, so the
		// rescan sees their capacity and offers the sessions again.
		now := s.nowMs()
		for id, e := range s.entries {
			if e.offered != nil && now >= e.offeredAt+ScheduleInterval.Milliseconds() {
				s.setOffer(e, nil)
				s.forget(id, e)
			}
		}
		for _, c := range s.conns {
			if c.attached {
				s.rescan[c.userID] = struct{}{}
			}
		}
		s.signal()
	}
	s.mu.Unlock()
	for ctx.Err() == nil {
		tctx, cancel := storeCtx(ctx)
		expired, err := s.cfg.Store.ExpireApprovals(tctx, s.nowMs(), store.MaxPageSize)
		cancel()
		if err != nil || len(expired) < store.MaxPageSize {
			return
		}
	}
}

// expire releases leases that expired (their connection is gone and the
// worker did not resume them).
func (s *Service) expire(ctx context.Context) {
	for ctx.Err() == nil {
		tctx, cancel := storeCtx(ctx)
		losses, err := s.cfg.Store.ExpireLeases(tctx, s.nowMs(), store.MaxPageSize)
		cancel()
		if err != nil {
			if ctx.Err() == nil {
				s.log.Warn("lease expiry sweep failed", "error", err)
			}
			return
		}
		for _, l := range losses {
			s.lost(l, nil, true)
		}
		if len(losses) < store.MaxPageSize {
			return
		}
	}
}

// extend moves the expiry of every lease bound to a live connection to now
// + LeaseLifetime. A lease the store no longer has as recorded (lost,
// rebound or expired) is revoked on its connection.
func (s *Service) extend(ctx context.Context) {
	s.mu.Lock()
	var keys []store.LeaseKey
	for id, e := range s.entries {
		if e.bound != nil && !e.abandoned && !s.stale(e.bound) {
			keys = append(keys, store.LeaseKey{SessionID: id, Epoch: e.holder.epoch, WorkerID: e.holder.worker, BootID: e.holder.boot, ConnID: e.holder.conn})
		}
	}
	s.mu.Unlock()
	for len(keys) > 0 && ctx.Err() == nil {
		n := min(len(keys), store.MaxPageSize)
		chunk := keys[:n]
		keys = keys[n:]
		now := s.nowMs()
		tctx, cancel := storeCtx(ctx)
		lost, err := s.cfg.Store.ExtendLeases(tctx, chunk, now, now+LeaseLifetime.Milliseconds())
		cancel()
		if err != nil {
			if ctx.Err() == nil {
				s.log.Warn("lease extension failed", "error", err)
			}
			continue
		}
		for _, k := range lost {
			// Re-derive from the store: the lease was released, taken over,
			// rebound or expired, and the session may need an Offer.
			s.mu.Lock()
			s.queueWake(k.SessionID, 0)
			s.mu.Unlock()
		}
	}
}
