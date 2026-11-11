package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/clock"
)

// ErrNotFound indicates an unknown or expired job ID.
var ErrNotFound = errors.New("job not found")

// ErrShutdown rejects jobs after shutdown begins.
var ErrShutdown = errors.New("job manager is shut down")

// ErrKilled identifies a process actually terminated by a stop or lifetime limit.
// A process that exits on its own during Stop must retain its real exit status.
var ErrKilled = errors.New("job killed")

// Clock supplies retention time and timers.
type Clock = clock.Clock

type realClock = clock.Real

// Job represents a background process owned by a tool. Wait reports the actual
// exit code and wraps ErrKilled only when termination caused the exit; Stop
// racing with natural completion must not turn that completion into a kill.
type Job interface {
	Wait() (exitCode int, err error)
	Stop() error
}

// Info is a snapshot of a job's execution state.
type Info struct {
	ID, SessionID, OutputPath string
	Command                   string
	State                     circusv1.JobEvent_State
	ExitCode                  int32
	Started, Ended            time.Time
}

// Options configures job retention, events, and ID generation.
type Options struct {
	Clock        Clock
	RetainExited time.Duration
	// OnEvent runs serially in transition order and must return promptly.
	// Shutdown drains all queued callbacks.
	OnEvent func(*circusv1.JobEvent)
	NewID   func() string
}

type jobRecord struct {
	info     Info
	job      Job
	done     chan struct{}
	stopOnce sync.Once
	stopErr  error
}

// Manager can be passed to tools at construction without changing Tool.
// OnEvent runs on a single goroutine, in state transition order.
type Manager struct {
	mu             sync.Mutex
	opts           Options
	jobs           map[string]*jobRecord
	reserved       map[string]struct{}
	closed         bool
	stopSweep      chan struct{}
	stopped        chan struct{}
	wg             sync.WaitGroup
	callbackQueue  []*circusv1.JobEvent
	callbackWake   chan struct{}
	callbackDone   chan struct{}
	callbackClosed bool
}

// NewManager starts retention sweeping; nonpositive RetainExited defaults
// to thirty minutes.
func NewManager(opts Options) *Manager {
	if opts.Clock == nil {
		opts.Clock = realClock{}
	}
	if opts.RetainExited <= 0 {
		opts.RetainExited = 30 * time.Minute
	}
	if opts.NewID == nil {
		opts.NewID = randomJobID
	}
	m := &Manager{
		opts:         opts,
		jobs:         make(map[string]*jobRecord),
		reserved:     make(map[string]struct{}),
		stopSweep:    make(chan struct{}),
		stopped:      make(chan struct{}),
		callbackWake: make(chan struct{}, 1),
		callbackDone: make(chan struct{}),
	}
	tick := opts.Clock.After(time.Minute)
	go m.sweep(tick)
	go m.deliverEvents()
	return m
}

func randomJobID() string {
	var bytes [6]byte
	// crypto/rand.Read is guaranteed to fill the buffer or terminate the process.
	_, _ = rand.Read(bytes[:])
	return hex.EncodeToString(bytes[:])
}

func (m *Manager) expireLocked() {
	now := m.opts.Clock.Now()
	for id, r := range m.jobs {
		if r.info.State != circusv1.JobEvent_STATE_RUNNING && now.Sub(r.info.Ended) >= m.opts.RetainExited {
			delete(m.jobs, id)
		}
	}
}

func (m *Manager) sweep(tick <-chan time.Time) {
	for {
		select {
		case <-m.stopSweep:
			return
		case <-tick:
			m.mu.Lock()
			m.expireLocked()
			m.mu.Unlock()
			tick = m.opts.Clock.After(time.Minute)
		}
	}
}

// Queue events under the state lock to preserve transition order.
func (m *Manager) emit(info Info) {
	if m.opts.OnEvent == nil {
		return
	}
	m.callbackQueue = append(m.callbackQueue, &circusv1.JobEvent{JobId: info.ID, SessionId: info.SessionID, State: info.State, ExitCode: info.ExitCode, OutputPath: info.OutputPath})
	select {
	case m.callbackWake <- struct{}{}:
	default:
	}
}

func (m *Manager) deliverEvents() {
	defer close(m.callbackDone)
	for {
		m.mu.Lock()
		if len(m.callbackQueue) == 0 {
			closed := m.callbackClosed
			m.mu.Unlock()
			if closed {
				return
			}
			<-m.callbackWake
			continue
		}
		event := m.callbackQueue[0]
		m.callbackQueue[0] = nil
		m.callbackQueue = m.callbackQueue[1:]
		m.mu.Unlock()
		m.opts.OnEvent(event)
	}
}

// Start allocates a job ID before launching, so output can use that ID.
func (m *Manager) Start(sessionID, command string, launch func(id string) (Job, string, error)) (Info, error) {
	m.mu.Lock()
	m.expireLocked()
	if m.closed {
		m.mu.Unlock()
		return Info{}, ErrShutdown
	}
	id := m.opts.NewID()
	if id == "" {
		m.mu.Unlock()
		return Info{}, errors.New("empty job ID")
	}
	if _, exists := m.jobs[id]; exists {
		m.mu.Unlock()
		return Info{}, fmt.Errorf("duplicate job ID %q", id)
	}
	if _, exists := m.reserved[id]; exists {
		m.mu.Unlock()
		return Info{}, fmt.Errorf("duplicate job ID %q", id)
	}
	m.reserved[id] = struct{}{}
	// Keep shutdown pending until a launch outside the lock is registered or stopped.
	m.wg.Add(1)
	defer m.wg.Done()
	m.mu.Unlock()

	job, outputPath, err := launch(id)

	m.mu.Lock()
	delete(m.reserved, id)
	if err != nil {
		m.mu.Unlock()
		return Info{}, err
	}
	if job == nil {
		m.mu.Unlock()
		return Info{}, errors.New("nil job")
	}
	if m.closed {
		m.mu.Unlock()
		_ = job.Stop()
		return Info{}, ErrShutdown
	}
	info := Info{ID: id, SessionID: sessionID, Command: command, OutputPath: outputPath, State: circusv1.JobEvent_STATE_RUNNING, Started: m.opts.Clock.Now()}
	r := &jobRecord{info: info, job: job, done: make(chan struct{})}
	m.jobs[id] = r
	m.wg.Add(1)
	m.emit(info)
	m.mu.Unlock()
	go m.watch(r)
	return info, nil
}

// StopOwned checks ownership while holding the same lock as lookup and expiry.
func (m *Manager) StopOwned(sessionID, id string) (Info, error) {
	m.mu.Lock()
	m.expireLocked()
	r := m.jobs[id]
	if r == nil || r.info.SessionID != sessionID || r.info.State != circusv1.JobEvent_STATE_RUNNING {
		m.mu.Unlock()
		return Info{}, ErrNotFound
	}
	m.mu.Unlock()
	return m.stop(r)
}

func (m *Manager) watch(r *jobRecord) {
	defer m.wg.Done()
	code, err := r.job.Wait()
	if err != nil && code == 0 && !errors.Is(err, ErrKilled) {
		code = -1
	}
	m.mu.Lock()
	r.info.ExitCode = int32(code)
	r.info.Ended = m.opts.Clock.Now()
	r.info.State = circusv1.JobEvent_STATE_EXITED
	if errors.Is(err, ErrKilled) {
		r.info.State = circusv1.JobEvent_STATE_KILLED
	}
	m.emit(r.info)
	close(r.done)
	m.mu.Unlock()
}

// Get returns a job's current snapshot; false means unknown or expired.
func (m *Manager) Get(id string) (Info, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expireLocked()
	if r := m.jobs[id]; r != nil {
		return r.info, true
	}
	return Info{}, false
}

func (m *Manager) stop(r *jobRecord) (Info, error) {
	r.stopOnce.Do(func() { r.stopErr = r.job.Stop() })
	if r.stopErr == nil {
		<-r.done
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return r.info, r.stopErr
}

// StopSession requests stop for every running job owned by sessionID.
// Returned snapshots include retained jobs that had already ended.
func (m *Manager) StopSession(sessionID string) []Info {
	m.mu.Lock()
	infos := make([]Info, 0)
	for _, r := range m.jobs {
		if r.info.SessionID == sessionID {
			infos = append(infos, r.info)
		}
	}
	m.mu.Unlock()
	for i, info := range infos {
		if stopped, err := m.StopOwned(sessionID, info.ID); err == nil {
			infos[i] = stopped
		}
	}
	return infos
}

// Shutdown requests termination of every running job and waits for their
// watchers to finish or ctx to expire.
func (m *Manager) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	m.expireLocked()
	if !m.closed {
		m.closed = true
		close(m.stopSweep)
		for _, r := range m.jobs {
			if r.info.State != circusv1.JobEvent_STATE_RUNNING {
				continue
			}
			m.wg.Add(1)
			go func() { defer m.wg.Done(); _, _ = m.stop(r) }()
		}
		go func() {
			m.wg.Wait()
			m.mu.Lock()
			m.callbackClosed = true
			m.mu.Unlock()
			select {
			case m.callbackWake <- struct{}{}:
			default:
			}
			<-m.callbackDone
			close(m.stopped)
		}()
	}
	m.mu.Unlock()
	select {
	case <-m.stopped:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
