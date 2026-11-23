// Package workspaces prepares project default directories through device calls.
package workspaces

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/clock"
	"github.com/EricWvi/circus/internal/controlplane/calls"
	"github.com/EricWvi/circus/internal/controlplane/sessions"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/wire"
	"github.com/google/uuid"
)

const (
	maxDevice = 2
	maxUser   = 4
	maxGlobal = 64
	scanLimit = 1000
	scanEvery = 30 * time.Second
	prepareMs = 120000
)

type Subscription interface {
	Next(context.Context) (*circusv1.CallUpdate, error)
	Close() error
}

// SubmitFunc is the unleased device call path.
type SubmitFunc func(context.Context, string, string, *circusv1.DeviceCall) (Subscription, error)

func SubmitThrough(r calls.Service) SubmitFunc {
	return func(ctx context.Context, userID, deviceName string, call *circusv1.DeviceCall) (Subscription, error) {
		return r.Submit(ctx, userID, deviceName, call)
	}
}

type Config struct {
	Store    store.Store
	Clock    clock.Clock
	Submit   SubmitFunc
	Supports func(deviceID, feature string) (online, supported bool)
	Cancel   func(ctx context.Context, userID, callID string) error
	StopJobs func(ctx context.Context, userID, sessionID string) error
	Bus      *sessions.Bus
	Waker    sessions.Waker
	Logger   *slog.Logger
}

type Manager struct {
	cfg Config
	// Hints never block the router hooks. A scan repairs missed hints.
	kicks      chan string
	online     chan deviceOnline
	slotsFreed chan struct{}
	wg         sync.WaitGroup

	mu        sync.Mutex
	runners   map[string]*runner
	paused    map[string]int
	admitting *admission
}

type deviceOnline struct{ userID, deviceID string }

type admission struct {
	projectID string
	cancel    context.CancelFunc
}

// The Run goroutine owns both bounded queues. A dropped hint is repaired by
// the periodic scan.
type workQueue struct {
	retries map[string]int64
	limited map[string]deviceOnline
}

type runner struct {
	workspace store.Workspace
	wake      chan struct{}
	cancel    context.CancelFunc
}

func New(cfg Config) *Manager {
	if cfg.Clock == nil {
		cfg.Clock = clock.Real{}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	return &Manager{
		cfg: cfg, kicks: make(chan string, scanLimit), online: make(chan deviceOnline, maxGlobal),
		slotsFreed: make(chan struct{}, 1), runners: make(map[string]*runner), paused: make(map[string]int),
	}
}

func (m *Manager) Kick(workspaceID string) {
	select {
	case m.kicks <- workspaceID:
	default:
	}
}

func (m *Manager) DeviceOnline(userID, deviceID string) {
	select {
	case m.online <- deviceOnline{userID, deviceID}:
	default:
	}
}

func (m *Manager) WorkspaceJobEnded(_, sessionID, _ string) {
	if id, ok := strings.CutPrefix(sessionID, store.WorkspaceSessionPrefix); ok {
		m.Kick(id)
	}
}

// Publish delivers notices after the transaction commits.
func (m *Manager) Publish(change store.WorkspaceChange) {
	Publish(m.cfg.Bus, m.cfg.Waker, change)
}

// Publish is shared with API writes, which commit before notifying watchers.
func Publish(bus *sessions.Bus, waker sessions.Waker, change store.WorkspaceChange) {
	w := change.Workspace
	var event *circusv1.SessionEvent
	for _, n := range change.Notices {
		if bus != nil && n.Seq != 0 {
			bus.Durable(n.SessionID, uint64(n.Seq))
		} else if bus != nil {
			if event == nil {
				event = &circusv1.SessionEvent{
					Kind: circusv1.SessionEvent_KIND_WORKSPACE, TsUnixMs: w.UpdatedAt,
					PayloadJson: string(w.EventPayload()), Ephemeral: true,
				}
			}
			bus.Ephemeral(n.SessionID, event)
		}
		if w.State == store.WorkspaceReady && waker != nil {
			waker.Wake(n.SessionID, circusv1.WorkerEvent_Nudge_REASON_INPUT)
		}
	}
}

// Run recovers persisted attempts and owns at most one runner per preparing
// workspace. The caller starts it once and joins it on shutdown.
func (m *Manager) Run(ctx context.Context) {
	defer m.wg.Wait()
	queue := &workQueue{retries: make(map[string]int64), limited: make(map[string]deviceOnline)}
	var cursor *store.WorkspaceCursor
	nextScan := m.cfg.Clock.Now().Add(scanEvery)
	var armedAt time.Time
	var timer <-chan time.Time
	cursor = m.scan(ctx, cursor, queue)
	for {
		next := nextScan
		for _, at := range queue.retries {
			if d := time.UnixMilli(at); d.Before(next) {
				next = d
			}
		}
		if timer == nil || next.Before(armedAt) {
			armedAt = next
			timer = clock.Until(m.cfg.Clock, next)
		}
		select {
		case <-ctx.Done():
			return
		case id := <-m.kicks:
			m.considerID(ctx, id, queue)
		case device := <-m.online:
			m.wakeDevice(device)
			m.scanDevice(ctx, device, queue)
		case <-m.slotsFreed:
			m.startLimited(ctx, queue)
		case <-timer:
			timer = nil
			now := m.cfg.Clock.Now()
			if !now.Before(nextScan) {
				nextScan = now.Add(scanEvery)
				m.scanRunners(ctx)
				cursor = m.scan(ctx, cursor, queue)
			}
			for id, at := range queue.retries {
				if at <= now.UnixMilli() {
					m.considerID(ctx, id, queue)
				}
			}
		}
	}
}

func (m *Manager) scan(ctx context.Context, after *store.WorkspaceCursor, queue *workQueue) *store.WorkspaceCursor {
	defer func() { _, _ = m.cfg.Store.PruneWorkspaceCalls(ctx, "", store.MaxPageSize) }()
	for read := 0; read < scanLimit && ctx.Err() == nil; {
		limit := min(store.MaxPageSize, scanLimit-read)
		rows, err := m.cfg.Store.ListActiveWorkspaces(ctx, after, limit)
		if err != nil {
			m.logError(store.Workspace{}, err)
			break
		}
		for _, w := range rows {
			m.consider(ctx, w, queue)
		}
		if len(rows) < limit {
			return nil
		}
		last := rows[len(rows)-1]
		after = &store.WorkspaceCursor{State: last.State, UpdatedAt: last.UpdatedAt, ID: last.ID}
		read += len(rows)
	}
	return after
}

func (m *Manager) scanDevice(ctx context.Context, device deviceOnline, queue *workQueue) {
	rows, err := m.cfg.Store.ListDeviceWorkspaces(ctx, device.userID, device.deviceID, scanLimit)
	if err != nil {
		m.logError(store.Workspace{}, err)
		return
	}
	for _, w := range rows {
		m.consider(ctx, w, queue)
	}
}

func (m *Manager) considerID(ctx context.Context, id string, queue *workQueue) {
	delete(queue.retries, id)
	delete(queue.limited, id)
	w, err := m.cfg.Store.GetWorkspaceByID(ctx, id)
	if err != nil {
		m.logError(w, err)
		return
	}
	m.consider(ctx, w, queue)
}

// startLimited gives work blocked by any of the three limits another chance
// as soon as a runner leaves. Candidates still blocked by other runners
// need no database read.
func (m *Manager) startLimited(ctx context.Context, queue *workQueue) {
	for id, device := range queue.limited {
		m.mu.Lock()
		allowed := m.hasSlot(device)
		m.mu.Unlock()
		if allowed && ctx.Err() == nil {
			m.considerID(ctx, id, queue)
		}
	}
}

func (m *Manager) consider(ctx context.Context, w store.Workspace, queue *workQueue) {
	if w.State != store.WorkspacePreparing && w.State != store.WorkspacePending {
		return
	}
	key := deviceOnline{w.UserID, w.DeviceID}
	m.mu.Lock()
	if m.paused[w.ProjectID] != 0 || ctx.Err() != nil {
		m.mu.Unlock()
		return
	}
	if r := m.runners[w.ID]; r != nil {
		poke(r.wake)
		m.mu.Unlock()
		return
	}
	allowed := m.hasSlot(key)
	if w.State == store.WorkspacePending {
		admitCtx, cancel := context.WithCancel(ctx)
		m.admitting = &admission{projectID: w.ProjectID, cancel: cancel}
		m.mu.Unlock()

		// Admission decides on its own, later clock reading. Comparing
		// against one taken before it queues every backoff it found not
		// yet due, even one that falls due before the check below. Only a
		// backoff already due here, left by a failed admission, waits for
		// the scan instead of retrying in a tight loop.
		before := m.now()
		var err error
		w, err = m.admit(admitCtx, w, allowed)
		m.logError(w, err)

		m.mu.Lock()
		m.admitting = nil
		interrupted := admitCtx.Err() != nil
		cancel()
		if interrupted || m.paused[w.ProjectID] != 0 {
			m.mu.Unlock()
			return
		}
		if w.Waiting == store.WorkspaceBackoff && w.RetryAt > before && len(queue.retries) < scanLimit {
			queue.retries[w.ID] = w.RetryAt
		}
	}
	if !allowed && (w.State == store.WorkspacePreparing || w.Waiting == store.WorkspaceLimit) {
		if len(queue.limited) < scanLimit {
			queue.limited[w.ID] = key
		}
		m.mu.Unlock()
		return
	}
	delete(queue.limited, w.ID)
	if w.State != store.WorkspacePreparing {
		m.mu.Unlock()
		return
	}
	runCtx, cancel := context.WithCancel(ctx)
	r := &runner{workspace: w, wake: make(chan struct{}, 1), cancel: cancel}
	m.runners[w.ID] = r
	m.mu.Unlock()

	m.wg.Go(func() {
		defer r.cancel()
		m.runAttempt(runCtx, w, r.wake)
		m.mu.Lock()
		delete(m.runners, w.ID)
		m.mu.Unlock()
		poke(m.slotsFreed)
		if runCtx.Err() == nil {
			m.Kick(w.ID)
		}
	})
}

func (m *Manager) hasSlot(key deviceOnline) bool {
	if len(m.runners) >= maxGlobal {
		return false
	}
	var device, user int
	for _, r := range m.runners {
		if r.workspace.DeviceID == key.deviceID {
			device++
		}
		if r.workspace.UserID == key.userID {
			user++
		}
	}
	return device < maxDevice && user < maxUser
}

func poke(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}

func (m *Manager) wakeDevice(device deviceOnline) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.runners {
		if w := r.workspace; w.UserID == device.userID && w.DeviceID == device.deviceID {
			poke(r.wake)
		}
	}
}

func (m *Manager) scanRunners(ctx context.Context) {
	m.mu.Lock()
	active := make([]*runner, 0, len(m.runners))
	for _, r := range m.runners {
		active = append(active, r)
	}
	m.mu.Unlock()
	for _, r := range active {
		poke(r.wake)
		d, err := m.cfg.Store.GetIdentity(ctx, store.KindDevice, r.workspace.DeviceID)
		if err == nil && d.DeletedAt != 0 {
			w, err := m.cfg.Store.GetWorkspaceByID(ctx, r.workspace.ID)
			if err == nil && w.State == store.WorkspacePreparing {
				_, err = m.apply(ctx, w, failed(w, circusv1.WorkspaceError_WORKSPACE_ERROR_DEVICE_REVOKED, "", m.now()))
			}
			m.logError(w, err)
			if err == nil || errors.Is(err, store.ErrNotFound) {
				r.cancel()
			}
		}
	}
}

func (m *Manager) now() int64 { return m.cfg.Clock.Now().UnixMilli() }

func (m *Manager) logError(w store.Workspace, err error) {
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, store.ErrNotFound) {
		// Store and router errors may contain request data.
		m.cfg.Logger.Warn("workspace operation failed", "workspace_id", w.ID, "state", w.State, "code", w.ErrorCode)
	}
}

// admit decides without calling the router. The project URL is read in the
// same transaction that writes the call ID and request snapshot.
func (m *Manager) admit(ctx context.Context, w store.Workspace, slot bool) (store.Workspace, error) {
	var change store.WorkspaceChange
	// Read the clock before the device's connection. A clock read after it,
	// for example once BEGIN IMMEDIATE has waited for the write lock, could
	// find a backoff due and start an attempt on a connection that was gone
	// before the retry fell due. That attempt only waits for the device and
	// never records that it is offline.
	now := m.now()
	online, supported := m.cfg.Supports(w.DeviceID, wire.FeaturePrepareWorkspace)
	err := m.cfg.Store.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		device, err := tx.GetIdentity(ctx, store.KindDevice, w.DeviceID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		p := w.WorkspacePatch
		p.UpdatedAt = now
		switch {
		case err != nil || device.DeletedAt != 0:
			p = failed(w, circusv1.WorkspaceError_WORKSPACE_ERROR_DEVICE_REVOKED, "", p.UpdatedAt)
		case strings.EqualFold(device.OS, "windows"):
			p = failed(w, circusv1.WorkspaceError_WORKSPACE_ERROR_UNSUPPORTED_OS, "", p.UpdatedAt)
		case w.Waiting == store.WorkspaceBackoff && w.RetryAt > p.UpdatedAt:
			return nil
		case !online:
			p.Waiting = store.WorkspaceOffline
			if w.Waiting == store.WorkspaceBackoff {
				p.RetryAt = 0
			}
		case !supported || (w.Waiting != store.WorkspaceBackoff && w.RetryAt != 0 && w.RetryAt >= device.LastConnectedAt):
			// TOOL_UNAVAILABLE from an advertised implementation. Wait
			// for a fresh connection before trusting Hello again.
			p.Waiting = store.WorkspaceOutdated
			if w.Waiting == store.WorkspaceBackoff {
				p.RetryAt = 0
			}
		case !slot:
			p.Waiting, p.RetryAt = store.WorkspaceLimit, 0
		default:
			project, err := tx.GetProject(ctx, w.UserID, w.ProjectID)
			if err != nil {
				return err
			}
			id, err := newCallID()
			if err != nil {
				return err
			}
			p.State, p.Waiting, p.RetryAt = store.WorkspacePreparing, "", 0
			p.Attempt++
			if w.Waiting != store.WorkspaceBackoff {
				p.BackoffStreak = 0
			}
			p.CallID, p.TimeoutMs, p.RepoURL = id, prepareMs, project.RepoURL
			p.AttemptStartedAt = p.UpdatedAt
		}
		if p.State == w.State && p.Waiting == w.Waiting && p.RetryAt == w.RetryAt {
			return nil
		}
		change, err = tx.TransitionWorkspace(ctx, w.ID, w.Expect(), p)
		return err
	})
	if err == nil && change.Applied {
		m.Publish(change)
		return change.Workspace, nil
	}
	return w, err
}

func newCallID() (string, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return "", err
	}
	return "ws-" + id.String(), nil
}
