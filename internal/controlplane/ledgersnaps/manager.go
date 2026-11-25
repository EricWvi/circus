// Package ledgersnaps keeps sessions' ledger snapshots: a copy of the
// project's ledger on the session's device, which the session's agent
// edits with ordinary tools. It prepares a snapshot when the session is
// created (the first turn waits for it), collects its changes after every
// turn into the session's changeset, renames new decisions in it after a
// merge (rebase_ledger), writes it again after a discard, tells the agent
// what it should know (notes), and removes it when the session is deleted.
// Rebases and resets run only between turns, and the session's turns wait
// for them, so the agent never works on files that change under it.
//
// The device calls run like the workspace preparations of package
// workspaces: only on a device that is online, retried when it
// reconnects. The work is driven by the store: a row of
// session_ledgers with work (a preparation, a due collection, a rebase or
// a reset) is found by a scan when a hint was missed, and a removal row
// waits for its device. Memory is bounded by maxInFlight: at most that many
// snapshot operations, collections included, hold their device results at
// once; the queue holds only session ids.
package ledgersnaps

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/clock"
	"github.com/EricWvi/circus/internal/controlplane/ledgerslots"
	"github.com/EricWvi/circus/internal/controlplane/sessions"
	"github.com/EricWvi/circus/internal/controlplane/workspaces"
	"github.com/EricWvi/circus/internal/ledger"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/wire"
)

const (
	// maxInFlight bounds the snapshot operations (preparations,
	// collections, rebases, resets and removals) that run at once, the
	// RPCs' forced collections included.
	maxInFlight = 4
	// maxPerDevice matches the daemon, which runs at most two ledger calls
	// at once and refuses more as BUSY.
	maxPerDevice = 2
	// maxQueue bounds the session ids waiting for a worker; a hint beyond
	// it is dropped and found by the next scan.
	maxQueue = 4096
	// scanEvery is the period of the scan for work whose hint was missed,
	// of retries and of the pruning of calls and old removals.
	scanEvery = 30 * time.Second
	// scanLimit bounds the rows one scan reads.
	scanLimit = 1000
	// maxRetryDelay caps the backoff of a failed step.
	maxRetryDelay = 5 * time.Minute
	// retryEvery is how often the backoffs that passed are queued.
	retryEvery = time.Second
	// sweepPage bounds the orphaned changesets one pass of the scan
	// discards.
	sweepPage = 16
)

// Config wires a Manager.
type Config struct {
	Store  store.Store
	Ledger *ledger.Manager
	Clock  clock.Clock
	// Submit sends a device call without a lease (control-plane only).
	Submit workspaces.SubmitFunc
	// Online reports whether a device is connected.
	Online func(deviceID string) bool
	// Slots are the ledger write slots shared with the changeset RPCs.
	Slots  *ledgerslots.Slots
	Bus    *sessions.Bus
	Waker  sessions.Waker
	Logger *slog.Logger
	// MaxMessageBytes is the links' message limit; zero means the wire
	// default. A preparation's chunk holds at most half of it.
	MaxMessageBytes int
}

// Manager runs the snapshot work. Its methods are safe for concurrent use.
type Manager struct {
	cfg         Config
	hints       chan string
	online      chan string
	removalHint chan struct{}
	// flight holds a token per operation in flight.
	flight chan struct{}

	mu sync.Mutex
	// locks serializes the operations on one session's snapshot.
	locks map[string]*sessionLock
	// queue holds session ids with work, each once; running the ones a
	// worker took, with their devices.
	queue   []string
	queued  map[string]bool
	running map[string]string
	devices map[string]int
	// retries maps a session waiting for its backoff to its retry time.
	retries map[string]int64
	// pendingRemovals holds the queued removals, by session id.
	pendingRemovals map[string]store.LedgerSnapshotRemoval
	// blocked holds, by device, the queue keys whose work found the
	// device's operations at their bound: they are queued again when one
	// ends. It holds at most maxQueue keys in all; a key beyond them is
	// found by the next scan.
	blocked  map[string][]string
	nblocked int
	// sweepAfter is where the next sweep of orphaned changesets starts;
	// only Run's goroutine uses it.
	sweepAfter string
	ready      chan struct{}
}

type sessionLock struct {
	ch    chan struct{}
	users int
}

// New returns a manager; Run starts its work.
func New(cfg Config) *Manager {
	if cfg.Clock == nil {
		cfg.Clock = clock.Real{}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.MaxMessageBytes <= 0 {
		cfg.MaxMessageBytes = wire.DefaultMaxMessageBytes
	}
	return &Manager{cfg: cfg, hints: make(chan string, 1024), online: make(chan string, 256), flight: make(chan struct{}, maxInFlight),
		locks: make(map[string]*sessionLock), queued: make(map[string]bool), running: make(map[string]string),
		devices: make(map[string]int), retries: make(map[string]int64), pendingRemovals: make(map[string]store.LedgerSnapshotRemoval),
		blocked: make(map[string][]string), ready: make(chan struct{}, 1), removalHint: make(chan struct{}, 1)}
}

// Kick asks for the session's snapshot work to be looked at. It never
// blocks; a dropped hint is repaired by the scan.
func (m *Manager) Kick(sessionID string) {
	select {
	case m.hints <- sessionID:
	default:
	}
}

// Due tells the manager that a turn's end made the session's snapshot due
// for a collection: its watchers hear of it, and the collection (or the
// rebase or reset the turn held back) runs. A rebase or reset that
// problems stopped is tried again at once, without its backoff: the
// agent may have fixed them in that turn.
func (m *Manager) Due(ctx context.Context, sessionID string) {
	l, err := m.cfg.Store.GetSessionLedger(ctx, sessionID)
	if err != nil {
		m.Kick(sessionID)
		return
	}
	if l.Pending != "" && l.ProblemCount > 0 && l.RetryAt > 0 {
		_, _, err = m.update(ctx, sessionID, eventCollected, func(x *store.SessionLedger) bool {
			if x.Pending == "" || x.ProblemCount == 0 || x.RetryAt == 0 {
				return false
			}
			x.RetryAt = 0
			return true
		})
		m.logError("retry after a turn", sessionID, err)
	} else {
		m.publish(l, eventCollected)
	}
	m.Kick(sessionID)
}

// DeviceOnline tells the manager that a device connected: its pending
// preparations, due collections, rebases, resets and removals run.
func (m *Manager) DeviceOnline(_, deviceID string) {
	select {
	case m.online <- deviceID:
	default:
	}
}

func (m *Manager) now() int64 { return m.cfg.Clock.Now().UnixMilli() }

// lock takes the session's lock, waiting while ctx allows.
func (m *Manager) lock(ctx context.Context, sessionID string) (func(), error) {
	m.mu.Lock()
	l := m.locks[sessionID]
	if l == nil {
		l = &sessionLock{ch: make(chan struct{}, 1)}
		m.locks[sessionID] = l
	}
	l.users++
	m.mu.Unlock()
	drop := func() {
		m.mu.Lock()
		if l.users--; l.users == 0 {
			delete(m.locks, sessionID)
		}
		m.mu.Unlock()
	}
	select {
	case l.ch <- struct{}{}:
		return func() { <-l.ch; drop() }, nil
	case <-ctx.Done():
		drop()
		return nil, ctx.Err()
	}
}

// tryLock takes the session's lock if it is free.
func (m *Manager) tryLock(sessionID string) (func(), bool) {
	m.mu.Lock()
	l := m.locks[sessionID]
	if l == nil {
		l = &sessionLock{ch: make(chan struct{}, 1)}
		m.locks[sessionID] = l
	}
	select {
	case l.ch <- struct{}{}:
		l.users++
		m.mu.Unlock()
		return func() {
			<-l.ch
			m.mu.Lock()
			if l.users--; l.users == 0 {
				delete(m.locks, sessionID)
			}
			m.mu.Unlock()
		}, true
	default:
		if l.users == 0 {
			delete(m.locks, sessionID)
		}
		m.mu.Unlock()
		return nil, false
	}
}

// takeFlight takes an in-flight token, waiting while ctx allows.
func (m *Manager) takeFlight(ctx context.Context) (func(), error) {
	select {
	case m.flight <- struct{}{}:
		return func() { <-m.flight }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// enqueue adds a session (or a removal's key) to the queue once.
func (m *Manager) enqueue(sessionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.enqueueLocked(sessionID)
}

func (m *Manager) enqueueLocked(sessionID string) {
	delete(m.retries, sessionID)
	if m.queued[sessionID] || len(m.queue) >= maxQueue {
		return
	}
	m.queued[sessionID] = true
	m.queue = append(m.queue, sessionID)
	poke(m.ready)
}

// retryAt schedules the session again at the given time.
func (m *Manager) retryAt(sessionID string, at int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.retries) < maxQueue {
		m.retries[sessionID] = at
	}
}

func poke(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}

// Run scans for work, owns the queue and runs up to maxInFlight workers
// until ctx ends; it returns once they have stopped.
func (m *Manager) Run(ctx context.Context) {
	var wg sync.WaitGroup
	defer wg.Wait()
	for range maxInFlight {
		wg.Go(func() { m.worker(ctx) })
	}
	after := m.scan(ctx, "", "")
	m.removals(ctx, "")
	ticker := m.cfg.Clock.After(scanEvery)
	retries := m.cfg.Clock.After(retryEvery)
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-m.hints:
			m.enqueue(id)
		case device := <-m.online:
			m.scan(ctx, device, "")
			m.removals(ctx, device)
		case <-m.removalHint:
			m.removals(ctx, "")
		case <-retries:
			retries = m.cfg.Clock.After(retryEvery)
			m.dueRetries()
		case <-ticker:
			ticker = m.cfg.Clock.After(scanEvery)
			after = m.scan(ctx, "", after)
			m.removals(ctx, "")
			m.prune(ctx)
			m.sweep(ctx)
		}
	}
}

// dueRetries queues the sessions whose backoff has passed.
func (m *Manager) dueRetries() {
	now := m.now()
	var due []string
	m.mu.Lock()
	for id, at := range m.retries {
		if at <= now {
			due = append(due, id)
		}
	}
	m.mu.Unlock()
	for _, id := range due {
		m.enqueue(id)
	}
}

// scan queues sessions with work, of one device or of all from after on,
// and returns where the next full scan continues.
func (m *Manager) scan(ctx context.Context, deviceID, after string) string {
	for read := 0; read < scanLimit && ctx.Err() == nil; {
		limit := min(store.MaxPageSize, scanLimit-read)
		rows, err := m.cfg.Store.ListSessionLedgers(ctx, store.SessionLedgerFilter{DeviceID: deviceID, After: after, Limit: limit})
		if err != nil {
			m.logError("scan", "", err)
			return after
		}
		now := m.now()
		for _, l := range rows {
			if l.RetryAt > now && deviceID == "" {
				m.retryAt(l.SessionID, l.RetryAt)
				continue
			}
			m.enqueue(l.SessionID)
		}
		if len(rows) < limit {
			return ""
		}
		after = rows[len(rows)-1].SessionID
		read += len(rows)
	}
	return after
}

// prune drops terminal snapshot calls and removals older than the
// retention.
func (m *Manager) prune(ctx context.Context) {
	if _, err := m.cfg.Store.PruneLedgerCalls(ctx, store.MaxPageSize); err != nil {
		m.logError("prune calls", "", err)
	}
	if _, err := m.cfg.Store.PruneLedgerSnapshotRemovals(ctx, m.now()-store.LedgerRemovalRetention, store.MaxPageSize); err != nil {
		m.logError("prune removals", "", err)
	}
}

// next takes the first queued session whose device has room, waiting
// until there is one or ctx ends.
func (m *Manager) next(ctx context.Context) (string, bool) {
	for {
		m.mu.Lock()
		for i, id := range m.queue {
			if _, busy := m.running[id]; busy {
				continue
			}
			m.queue = append(m.queue[:i:i], m.queue[i+1:]...)
			delete(m.queued, id)
			m.running[id] = ""
			m.mu.Unlock()
			return id, true
		}
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			return "", false
		case <-m.ready:
		}
	}
}

func (m *Manager) worker(ctx context.Context) {
	for {
		id, ok := m.next(ctx)
		if !ok {
			return
		}
		m.process(ctx, id)
		m.mu.Lock()
		delete(m.running, id)
		more := len(m.queue) > 0
		m.mu.Unlock()
		if more {
			poke(m.ready)
		}
	}
}

// process does the session's next step of work. A session whose lock an
// RPC holds is queued again when the RPC ends; work whose device has
// maxPerDevice operations running is queued again when one of them ends.
func (m *Manager) process(ctx context.Context, sessionID string) {
	release, err := m.takeFlight(ctx)
	if err != nil {
		return
	}
	defer release()
	if id, ok := strings.CutPrefix(sessionID, removalKey); ok {
		m.mu.Lock()
		r, ok := m.pendingRemovals[id]
		m.mu.Unlock()
		if !ok {
			return
		}
		device, ok := m.takeDevice(r.DeviceID, sessionID)
		if !ok {
			return
		}
		defer device()
		m.mu.Lock()
		delete(m.pendingRemovals, id)
		m.mu.Unlock()
		m.remove(ctx, r)
		return
	}
	unlock, ok := m.tryLock(sessionID)
	if !ok {
		return
	}
	defer unlock()
	l, err := m.cfg.Store.GetSessionLedger(ctx, sessionID)
	if err != nil {
		m.logError("read snapshot", sessionID, err)
		return
	}
	if l.RetryAt > m.now() {
		m.retryAt(sessionID, l.RetryAt)
		return
	}
	if l.State.Unprepared() {
		device, ok := m.takeDevice(l.DeviceID, sessionID)
		if !ok {
			return
		}
		defer device()
		m.prepare(ctx, l)
		return
	}
	if l.State != store.SessionLedgerReady || l.Pending == "" && !l.CollectionDue() || !m.reachable(ctx, l) {
		// An offline device's work is queued when it connects.
		return
	}
	sess, err := m.cfg.Store.GetSession(ctx, sessionID, m.now())
	if err != nil {
		m.logError("read session", sessionID, err)
		return
	}
	if sess.State == store.SessionRunning {
		// Nothing changes the snapshot during a turn; the turn's end
		// queues the work again.
		return
	}
	device, ok := m.takeDevice(l.DeviceID, sessionID)
	if !ok {
		return
	}
	defer device()
	switch {
	case l.Pending == store.SessionLedgerReset:
		m.reset(ctx, l)
	case l.Pending == store.SessionLedgerRebase:
		m.rebase(ctx, l)
	default:
		if _, err := m.collect(ctx, l); err != nil && !errors.Is(err, errOffline) {
			m.logError("collect", sessionID, err)
		}
	}
}

// takeDevice counts an operation on the device, at most maxPerDevice. When
// the device is at its bound, key (if any) is queued again once one of its
// operations ends.
func (m *Manager) takeDevice(deviceID, key string) (func(), bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.devices[deviceID] >= maxPerDevice {
		if key != "" && m.nblocked < maxQueue && !slices.Contains(m.blocked[deviceID], key) {
			m.blocked[deviceID] = append(m.blocked[deviceID], key)
			m.nblocked++
		}
		return nil, false
	}
	m.devices[deviceID]++
	return func() {
		m.mu.Lock()
		if m.devices[deviceID]--; m.devices[deviceID] <= 0 {
			delete(m.devices, deviceID)
		}
		keys := m.blocked[deviceID]
		delete(m.blocked, deviceID)
		m.nblocked -= len(keys)
		for _, k := range keys {
			m.enqueueLocked(k)
		}
		m.mu.Unlock()
		poke(m.ready)
	}, true
}

// waitDevice counts an operation on the device, waiting for room.
func (m *Manager) waitDevice(ctx context.Context, deviceID string) (func(), error) {
	for {
		if release, ok := m.takeDevice(deviceID, ""); ok {
			return release, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-m.cfg.Clock.After(50 * time.Millisecond):
		}
	}
}

func (m *Manager) logError(op, sessionID string, err error) {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, store.ErrNotFound) {
		return
	}
	// Errors may carry ledger content; log only the operation's kind.
	m.cfg.Logger.Warn("ledger snapshot operation failed", "op", op, "session_id", sessionID, "error_type", errorType(err))
}

func errorType(err error) string {
	var request *ledger.RequestError
	var limit *ledger.LimitError
	var precondition *ledger.PreconditionError
	switch {
	case errors.As(err, &request):
		return "request"
	case errors.As(err, &limit):
		return "limit"
	case errors.As(err, &precondition):
		return "precondition"
	case errors.Is(err, store.ErrBusy):
		return "busy"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	}
	return "internal"
}

// backoff is the delay before attempt n+1 of a failed step.
func backoff(attempts int64) time.Duration {
	return min(5*time.Second<<uint(min(attempts, 6)), maxRetryDelay)
}

// Status is a snapshot's state as the API shows it.
func Status(l store.SessionLedger) *circusv1.SessionLedgerStatus {
	out := &circusv1.SessionLedgerStatus{Path: l.Path, Error: circusv1.LedgerSnapshotError(l.ErrorCode), ErrorDetail: l.ErrorDetail,
		CollectedAtUnixMs: l.CollectedAt, CollectionDue: l.CollectionDue(), ChangesetId: l.ChangesetID,
		ChangeCount: l.ChangeCount, ProblemCount: l.ProblemCount, UpdatedAtUnixMs: l.UpdatedAt}
	switch {
	case l.State != store.SessionLedgerReady:
	case l.Pending == store.SessionLedgerRebase:
		out.Applying = circusv1.SessionLedgerStatus_APPLYING_MERGE
	case l.Pending == store.SessionLedgerReset:
		out.Applying = circusv1.SessionLedgerStatus_APPLYING_DISCARD
	}
	switch l.State {
	case store.SessionLedgerPending:
		out.State = circusv1.SessionLedgerStatus_STATE_PENDING
	case store.SessionLedgerPreparing:
		out.State = circusv1.SessionLedgerStatus_STATE_PREPARING
	case store.SessionLedgerReady:
		out.State = circusv1.SessionLedgerStatus_STATE_READY
	case store.SessionLedgerFailed:
		out.State = circusv1.SessionLedgerStatus_STATE_FAILED
	}
	return out
}
