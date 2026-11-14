package calls

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/controlplane/gateway"
	"github.com/EricWvi/circus/internal/controlplane/timers"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/wire"
)

// pageSize bounds every store walk and every loss or grace batch.
const pageSize = 64

// healthProbeTimeout bounds the single-row store read used to recover readiness.
const healthProbeTimeout = time.Second

// resultRetention is the daemon's result lifetime: a dispatched call whose
// device stays offline this long can no longer be recovered, so it is lost.
const resultRetention = 30 * time.Minute

// listJobsKey marks the sync's outstanding ListJobs; it is not a valid call ID.
const listJobsKey = "\x00list-jobs"

var active = []store.CallState{store.CallDispatched, store.CallRunning, store.CallUnknown}

// router serializes all mutation and send admission under mu (DESIGN.md,
// deviation 1). mu is never held while waiting for a daemon response, a
// subscriber or sync capacity. Every map has an explicit Config count bound.
type router struct {
	lifecycle sync.Mutex
	stopping  bool
	joinOnce  sync.Once
	joined    chan struct{}

	mu      sync.Mutex
	config  Config
	started int64 // ms; offline time of devices a previous process left connected
	// syncLimit bounds unanswered sync requests per device: MaxSyncRequests,
	// further limited to MaxSyncReplyBytes / MaxMessageBytes worst-case replies.
	syncLimit int
	ctx       context.Context
	cancel    context.CancelFunc
	done      chan struct{}
	work      sync.WaitGroup
	// callbacks joins subscription context callbacks; callbackCount counts them.
	callbacks     sync.WaitGroup
	callbackCount int
	operations    chan struct{} // MaxOperations admission slots
	closed        bool
	health        atomic.Uint64
	pending       map[string]*pendingCall // <= Pending.MaxCalls
	pendingBytes  int64
	subs          map[*Subscription]struct{} // with callbacks/queries <= MaxSubscriptions
	bufferedBytes int64
	devices       map[string]*deviceState   // <= MaxDevices current generations
	queries       map[string][]*queryWaiter // total waiters <= MaxSubscriptions
	queryCount    int
	// unrecorded holds disconnects whose store write failed, one small entry
	// per device until a write succeeds. Its bound is the number of distinct
	// devices whose disconnect write failed (at most the device table), not
	// the registry: an entry outlives its route. The sweep retries them.
	unrecorded map[string]int64
}

// pendingCall is retained request content and its ownership cost.
type pendingCall struct {
	call *circusv1.DeviceCall
	cost int64
}

// deviceState is one current generation's sync state. outstanding holds at
// most syncLimit call IDs (and listJobsKey) awaiting a sync reply.
type deviceState struct {
	connection       gateway.Connection
	seq, snapshotSeq uint64
	snapshotPending  bool
	outstanding      map[string]struct{}
	changed          chan struct{}
}

func (d *deviceState) signal() { close(d.changed); d.changed = make(chan struct{}) }

// operation is one admitted public call, hook or timer handler. It holds an
// admission slot and, while locked, the router mutex.
type operation struct {
	r      *router
	ctx    context.Context
	cancel context.CancelFunc
	stop   func() bool
	locked bool
	slot   bool
	kind   string    // operation name reported to Config.lockObserver
	since  time.Time // start of the current locked segment, when observed
}

// enter admits an operation and returns it locked. Public calls refuse at
// once when every slot is busy (wait false); hooks and timers wait for one.
func (r *router) enter(ctx context.Context, wait bool, kind string) (*operation, error) {
	if r.ctx.Err() != nil {
		return nil, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if wait {
		select {
		case r.operations <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-r.ctx.Done():
			return nil, ErrClosed
		}
	} else {
		select {
		case r.operations <- struct{}{}:
		default:
			return nil, ErrOverloaded
		}
	}
	r.lifecycle.Lock()
	if r.stopping {
		r.lifecycle.Unlock()
		<-r.operations
		return nil, ErrClosed
	}
	r.work.Add(1)
	r.lifecycle.Unlock()
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(r.ctx, cancel)
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		<-r.operations
		stop()
		cancel()
		r.work.Done()
		return nil, ErrClosed
	}
	op := &operation{r: r, ctx: ctx, cancel: cancel, stop: stop, locked: true, slot: true, kind: kind}
	if r.config.lockObserver != nil {
		op.since = time.Now()
	}
	return op, nil
}
func (o *operation) unlock() {
	observe := o.r.config.lockObserver
	var held time.Duration
	if observe != nil {
		held = time.Since(o.since)
	}
	o.locked = false
	o.r.mu.Unlock()
	if observe != nil {
		observe(o.kind, held)
	}
}
func (o *operation) lock() {
	o.r.mu.Lock()
	o.locked = true
	if o.r.config.lockObserver != nil {
		o.since = time.Now()
	}
}

// yield lets other operations run between items of a long walk.
func (o *operation) yield() { o.unlock(); o.lock() }

// releaseSlot returns the operation's admission slot while it waits.
func (o *operation) releaseSlot() {
	if o.slot {
		<-o.r.operations
		o.slot = false
	}
}

// acquireSlot waits for an admission slot again; call it unlocked.
func (o *operation) acquireSlot() error {
	select {
	case o.r.operations <- struct{}{}:
		o.slot = true
		return nil
	case <-o.ctx.Done():
		return o.ctx.Err()
	}
}
func (o *operation) finish() {
	if o.locked {
		o.unlock()
	}
	o.stop()
	o.cancel()
	if o.slot {
		<-o.r.operations
	}
	o.r.work.Done()
}

func newRouter(c Config) (Router, error) {
	if c.Store == nil || c.Registry == nil || c.Sender == nil || c.Clock == nil {
		return nil, fmt.Errorf("calls: missing dependency")
	}
	defaults := []struct {
		v *int
		d int
	}{{&c.Pending.MaxCalls, 4096}, {&c.MaxDevices, 256}, {&c.MaxOperations, 128}, {&c.MaxSubscriptions, 1024}, {&c.MaxUpdatesPerSubscription, 16}, {&c.MaxMessageBytes, wire.DefaultMaxMessageBytes}, {&c.MaxSyncRequests, 16}}
	for _, v := range defaults {
		if *v.v == 0 {
			*v.v = v.d
		}
	}
	for _, v := range []struct {
		v *int64
		d int64
	}{{&c.Pending.MaxBytes, 256 << 20}, {&c.MaxSubscriptionBytes, 144 << 20}, {&c.MaxBufferedUpdateBytes, 256 << 20}} {
		if *v.v == 0 {
			*v.v = v.d
		}
	}
	for _, v := range []struct {
		v *time.Duration
		d time.Duration
	}{{&c.GracePeriod, 30 * time.Second}, {&c.SubscriberStall, 15 * time.Second}, {&c.WatchdogSlack, 30 * time.Second}, {&c.DeliveryTimeout, 2 * time.Minute}, {&c.ResendHorizon, 10 * time.Minute}} {
		if *v.v == 0 {
			*v.v = v.d
		}
	}
	for _, v := range defaults {
		if *v.v < 1 {
			return nil, fmt.Errorf("calls: invalid limits")
		}
	}
	if c.MaxSyncReplyBytes == 0 {
		c.MaxSyncReplyBytes = 2 * int64(c.MaxMessageBytes)
	}
	if c.Pending.MaxBytes < 1 || c.MaxSubscriptionBytes < 1 || c.MaxBufferedUpdateBytes < 1 || c.MaxSyncRequests > 256 || c.MaxSyncReplyBytes < int64(c.MaxMessageBytes) {
		return nil, fmt.Errorf("calls: invalid limits")
	}
	if c.GracePeriod <= 0 || c.SubscriberStall <= 0 || c.WatchdogSlack <= 0 || c.DeliveryTimeout <= 0 || c.ResendHorizon <= 0 {
		return nil, fmt.Errorf("calls: invalid durations")
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &router{
		config: c, started: c.Clock.Now().UnixMilli(), ctx: ctx, cancel: cancel,
		syncLimit: int(min(int64(c.MaxSyncRequests), c.MaxSyncReplyBytes/int64(c.MaxMessageBytes))),
		done:      make(chan struct{}), joined: make(chan struct{}), operations: make(chan struct{}, c.MaxOperations),
		pending: make(map[string]*pendingCall), subs: make(map[*Subscription]struct{}),
		devices: make(map[string]*deviceState), queries: make(map[string][]*queryWaiter), unrecorded: make(map[string]int64),
	}
	r.health.Store(1)
	if err := r.reconcileStartup(ctx); err != nil {
		cancel()
		return nil, err
	}
	go r.sweep()
	return r, nil
}

// reconcileStartup loses accepted rows: their content died with the previous
// process. They were never dispatched, so an equal resubmission may retry.
// Dispatched rows keep their boot and watchdog and recover by query.
func (r *router) reconcileStartup(ctx context.Context) error {
	var after *store.CallCursor
	for {
		rows, err := r.config.Store.ListCalls(ctx, store.CallFilter{States: []store.CallState{store.CallAccepted}, Limit: pageSize, After: after})
		if err != nil {
			return err
		}
		for _, row := range rows {
			err = r.config.Store.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
				_, err := tx.TransitionCall(ctx, row.CallID, store.CallExpect{States: []store.CallState{store.CallAccepted}, Undispatched: true}, store.CallLost, store.CallPatch{FinishedAt: r.started, OutcomeReason: "CONTROL_PLANE_RESTARTED"})
				if err != nil {
					return err
				}
				return tx.CancelTimer(ctx, timers.CallWatchdog, row.CallID)
			})
			if err != nil {
				return err
			}
		}
		if len(rows) < pageSize {
			return nil
		}
		last := rows[len(rows)-1]
		after = &store.CallCursor{SubmittedAt: last.SubmittedAt, CallID: last.CallID}
	}
}

// storeFailed records persistence health. Context endings (a link closing
// during sync) are not store failures and leave readiness untouched.
func (r *router) storeFailed(err error) {
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		r.markFailed()
	}
}

// markFailed advances the packed failure generation and clears health in one
// atomic operation. markHealthy sets the bit only when no failure raced it.
func (r *router) markFailed() {
	for {
		v := r.health.Load()
		if r.health.CompareAndSwap(v, (v+2)&^1) {
			return
		}
	}
}

func (r *router) markHealthy() {
	for {
		v := r.health.Load()
		if v&1 == 1 || r.health.CompareAndSwap(v, v|1) {
			return
		}
	}
}

// offlineSince returns when an offline device (no live route) went offline.
// A row that still says connected was left by a previous process (or its
// disconnect write failed): the device counts as offline since this process
// started, or since the failed write.
func (r *router) offlineSince(d store.Identity) int64 {
	if at, ok := r.unrecorded[d.ID]; ok {
		return at
	}
	if rowConnected(d) {
		return max(d.LastDisconnectedAt, r.started)
	}
	return d.LastDisconnectedAt
}

// rowConnected reports whether the device row records a connection without a
// later disconnect. Equal milliseconds are ambiguous and count as connected:
// offlineSince then yields LastDisconnectedAt for a disconnect recorded by
// this process, which is never before its start.
func rowConnected(d store.Identity) bool {
	return d.CurrentBootID != "" && d.LastConnectedAt >= d.LastDisconnectedAt
}

// recordOffline persists offlineSince for a device whose row still says
// connected, and creates its grace timer unless one exists. The caller has
// established there is no live route and removes r.unrecorded after commit.
func (r *router) recordOffline(ctx context.Context, tx store.Ops, d store.Identity, at int64) error {
	_, unrecorded := r.unrecorded[d.ID]
	if !unrecorded && !rowConnected(d) {
		return nil
	}
	if err := tx.SetDisconnected(ctx, store.KindDevice, d.ID, at); err != nil {
		return err
	}
	rows, err := tx.ListTimers(ctx, store.TimerFilter{Kind: timers.DeviceGrace, SubjectID: d.ID, Limit: 1})
	if err != nil || len(rows) > 0 {
		return err
	}
	return tx.SetTimer(ctx, timers.DeviceGrace, d.ID, at+r.config.GracePeriod.Milliseconds())
}

// retryDisconnects persists disconnects whose write failed (the
// disconnect-record retry, DESIGN.md), so accepted calls get their grace timer. Called by the sweep with mu held.
func (r *router) retryDisconnects() {
	for id, at := range r.unrecorded {
		if _, online, err := r.config.Registry.Lookup(r.ctx, id); err != nil || online {
			if online {
				delete(r.unrecorded, id)
			}
			continue
		}
		err := r.config.Store.Tx(r.ctx, func(ctx context.Context, tx store.Ops) error {
			d, err := tx.GetIdentity(ctx, store.KindDevice, id)
			if errors.Is(err, store.ErrNotFound) {
				return nil
			}
			if err != nil {
				return err
			}
			return r.recordOffline(ctx, tx, d, at)
		})
		if err != nil {
			r.storeFailed(err)
			return
		}
		r.markHealthy()
		delete(r.unrecorded, id)
	}
}

// probeStore recovers readiness without requiring new call traffic. The read is
// limited to one row and one second, runs only while the health bit is false,
// and is always called outside r.mu. A successful read can make readiness flap
// when writes keep failing (for example disk full); the next failed write clears
// the bit again.
func (r *router) probeStore() {
	generation := r.health.Load()
	ctx, cancel := context.WithTimeout(r.ctx, healthProbeTimeout)
	defer cancel()
	if _, err := r.config.Store.ListTimers(ctx, store.TimerFilter{Limit: 1}); err == nil {
		// Restore only if no newer failed write advanced the packed generation.
		r.health.CompareAndSwap(generation, generation|1)
	}
}

func (r *router) sweep() {
	defer close(r.done)
	interval := min(r.config.SubscriberStall, time.Second)
	alarm := r.config.Clock.After(interval)
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-alarm:
		}
		if !r.Healthy() {
			r.probeStore()
		}
		r.mu.Lock()
		observe, since := r.config.lockObserver, time.Time{}
		if observe != nil {
			since = time.Now()
		}
		if !r.closed {
			now := r.config.Clock.Now()
			for s := range r.subs {
				if len(s.queue) > 0 && now.Sub(s.lastRead) >= r.config.SubscriberStall {
					r.detach(s, ErrOverflow)
				}
			}
			if len(r.unrecorded) > 0 {
				r.retryDisconnects()
			}
		}
		r.mu.Unlock()
		if observe != nil {
			observe("sweep", time.Since(since))
		}
		alarm = r.config.Clock.After(interval)
	}
}

func (r *router) Close(ctx context.Context) error {
	r.lifecycle.Lock()
	r.stopping = true
	r.cancel()
	r.lifecycle.Unlock()
	r.mu.Lock()
	r.closed = true
	for s := range r.subs {
		r.detach(s, ErrClosed)
	}
	for id := range r.pending {
		r.release(id)
	}
	for _, d := range r.devices {
		d.signal()
	}
	clear(r.devices)
	clear(r.unrecorded)
	r.mu.Unlock()
	r.joinOnce.Do(func() { go func() { <-r.done; r.work.Wait(); r.callbacks.Wait(); close(r.joined) }() })
	select {
	case <-r.joined:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Healthy reports the in-memory store health bit without locking or store I/O,
// so readiness probes cannot queue behind router work or a stalled store.
func (r *router) Healthy() bool {
	return r.health.Load()&1 == 1
}

func (r *router) Stats() Stats {
	r.mu.Lock()
	s := Stats{Callbacks: r.callbackCount, PendingCalls: len(r.pending), PendingBytes: r.pendingBytes, Subscribers: len(r.subs), BufferedBytes: r.bufferedBytes, QueryWaiters: r.queryCount, Devices: len(r.devices), Operations: len(r.operations), UnrecordedDisconnects: len(r.unrecorded), StoreHealthy: r.Healthy()}
	for _, d := range r.devices {
		s.SyncRequests += len(d.outstanding)
	}
	r.mu.Unlock()
	// The durable walk runs outside the admission lock in bounded pages.
	var after *store.TimerCursor
	for {
		rows, err := r.config.Store.ListTimers(context.Background(), store.TimerFilter{Limit: pageSize, After: after})
		if err != nil {
			s.StoreHealthy = false
			break
		}
		for _, tm := range rows {
			if tm.Kind == timers.CallWatchdog || tm.Kind == timers.DeviceGrace {
				s.DurableTimers++
			}
		}
		if len(rows) < pageSize {
			break
		}
		tm := rows[len(rows)-1]
		after = &store.TimerCursor{DueAt: tm.DueAt, Kind: tm.Kind, SubjectID: tm.SubjectID}
	}
	return s
}

// release drops retained request content.
func (r *router) release(id string) {
	if p, ok := r.pending[id]; ok {
		r.pendingBytes -= p.cost
		delete(r.pending, id)
	}
}

// settle moves c to a terminal state, cancels its watchdog in the same Tx and,
// after commit, releases content and notifies subscribers and query waiters.
func (r *router) settle(ctx context.Context, c store.Call, to store.CallState, patch store.CallPatch, u *circusv1.CallUpdate) error {
	changed := false
	err := r.config.Store.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		var err error
		changed, err = tx.TransitionCall(ctx, c.CallID, store.CallExpect{States: []store.CallState{c.State}, BootID: c.DispatchedBootID}, to, patch)
		if err != nil || !changed {
			return err
		}
		return tx.CancelTimer(ctx, timers.CallWatchdog, c.CallID)
	})
	if err != nil {
		r.storeFailed(err)
		return err
	}
	r.markHealthy()
	if changed {
		r.release(c.CallID)
		r.publish(u)
		r.answer(c.CallID, u, nil)
	}
	return nil
}

func cloneConnection(c gateway.Connection) gateway.Connection {
	c.Target.InstanceID = strings.Clone(c.Target.InstanceID)
	c.Target.ConnectionID = strings.Clone(c.Target.ConnectionID)
	c.Target.DeviceID = strings.Clone(c.Target.DeviceID)
	c.Target.BootID = strings.Clone(c.Target.BootID)
	c.UserID = strings.Clone(c.UserID)
	c.DeviceName = strings.Clone(c.DeviceName)
	return c
}

func sameConnection(a, b gateway.Connection) bool {
	return a.Target == b.Target && a.UserID == b.UserID && a.DeviceName == b.DeviceName
}

// readyRoute returns the device's route when it is synchronized and serves
// boot. Per-call commands (CallQuery, CancelCall, first DispatchCall) are sent
// only on such a route; an unready route's sync owns them (the sync-first rule, DESIGN.md).
func (r *router) readyRoute(ctx context.Context, device, boot string) (gateway.Connection, bool) {
	route, ok, err := r.config.Registry.Lookup(ctx, device)
	if err != nil || !ok || !route.Ready || route.Target.BootID != boot {
		return gateway.Connection{}, false
	}
	return route, true
}

func (r *router) sendQuery(ctx context.Context, route gateway.Connection, id string) error {
	return r.config.Sender.Deliver(ctx, route.Target, &circusv1.DaemonDownlink{Payload: &circusv1.DaemonDownlink_CallQuery{CallQuery: &circusv1.CallQuery{CallId: id}}})
}

func (r *router) sendCancel(ctx context.Context, route gateway.Connection, id string) error {
	return r.config.Sender.Deliver(ctx, route.Target, &circusv1.DaemonDownlink{Payload: &circusv1.DaemonDownlink_CancelCall{CancelCall: &circusv1.CancelCall{CallId: id}}})
}

// logSendFailure records a per-call command that could not be queued; the
// durable watchdog or the next sync recovers it.
func logSendFailure(kind, id string, err error) {
	if err != nil {
		slog.Debug("daemon command not queued", "kind", kind, "call_id", id, "error", err)
	}
}
