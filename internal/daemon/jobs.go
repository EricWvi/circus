package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
)

// ErrJobNotFound indicates an unknown or expired job ID.
var ErrJobNotFound = errors.New("job not found")

// Job represents a background process owned by a tool.
type Job interface {
	Wait() (exitCode int, err error)
	Stop() error
}

// JobInfo is a snapshot of a job's execution state.
type JobInfo struct {
	ID, SessionID, OutputPath string
	Command                   string
	State                     circusv1.JobEvent_State
	ExitCode                  int32
	Started, Ended            time.Time
}

// JobOptions configures job retention, events, and ID generation.
type JobOptions struct {
	Clock        Clock
	RetainExited time.Duration
	OnEvent      func(*circusv1.JobEvent)
	NewID        func() string
}

type jobRecord struct {
	info          JobInfo
	job           Job
	stopRequested bool
	stopOnce      sync.Once
	stopErr       error
}

// JobManager can be passed to tools at construction without changing Tool.
// OnEvent runs outside the manager's lock; events for different jobs may overlap.
type JobManager struct {
	mu        sync.Mutex
	opts      JobOptions
	jobs      map[string]*jobRecord
	reserved  map[string]struct{}
	closed    bool
	stopSweep chan struct{}
	stopped   chan struct{}
	wg        sync.WaitGroup
}

func NewJobManager(opts JobOptions) *JobManager {
	if opts.Clock == nil {
		opts.Clock = realClock{}
	}
	if opts.RetainExited <= 0 {
		opts.RetainExited = 30 * time.Minute
	}
	if opts.NewID == nil {
		opts.NewID = randomJobID
	}
	m := &JobManager{
		opts:      opts,
		jobs:      make(map[string]*jobRecord),
		reserved:  make(map[string]struct{}),
		stopSweep: make(chan struct{}),
		stopped:   make(chan struct{}),
	}
	tick := opts.Clock.After(time.Minute)
	go m.sweep(tick)
	return m
}

func randomJobID() string {
	var bytes [6]byte
	// crypto/rand.Read is guaranteed to fill the buffer or terminate the process.
	_, _ = rand.Read(bytes[:])
	return hex.EncodeToString(bytes[:])
}

func (m *JobManager) expireLocked() {
	now := m.opts.Clock.Now()
	for id, r := range m.jobs {
		if r.info.State != circusv1.JobEvent_STATE_RUNNING && now.Sub(r.info.Ended) >= m.opts.RetainExited {
			delete(m.jobs, id)
		}
	}
}

func (m *JobManager) sweep(tick <-chan time.Time) {
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

func (m *JobManager) emit(info JobInfo) {
	if m.opts.OnEvent != nil {
		m.opts.OnEvent(&circusv1.JobEvent{JobId: info.ID, SessionId: info.SessionID, State: info.State, ExitCode: info.ExitCode, OutputPath: info.OutputPath})
	}
}

func (m *JobManager) Add(sessionID, outputPath string, job Job) (JobInfo, error) {
	return m.Start(sessionID, "", func(string) (Job, string, error) { return job, outputPath, nil })
}

// Start allocates a job ID before launching, so output can use that ID.
func (m *JobManager) Start(sessionID, command string, launch func(id string) (Job, string, error)) (JobInfo, error) {
	m.mu.Lock()
	m.expireLocked()
	if m.closed {
		m.mu.Unlock()
		return JobInfo{}, ErrShutdown
	}
	id := m.opts.NewID()
	if id == "" {
		m.mu.Unlock()
		return JobInfo{}, errors.New("empty job ID")
	}
	if _, exists := m.jobs[id]; exists {
		m.mu.Unlock()
		return JobInfo{}, fmt.Errorf("duplicate job ID %q", id)
	}
	if _, exists := m.reserved[id]; exists {
		m.mu.Unlock()
		return JobInfo{}, fmt.Errorf("duplicate job ID %q", id)
	}
	m.reserved[id] = struct{}{}
	m.mu.Unlock()

	job, outputPath, err := launch(id)

	m.mu.Lock()
	delete(m.reserved, id)
	if err != nil {
		m.mu.Unlock()
		return JobInfo{}, err
	}
	if job == nil {
		m.mu.Unlock()
		return JobInfo{}, errors.New("nil job")
	}
	if m.closed {
		m.mu.Unlock()
		_ = job.Stop()
		return JobInfo{}, ErrShutdown
	}
	info := JobInfo{ID: id, SessionID: sessionID, Command: command, OutputPath: outputPath, State: circusv1.JobEvent_STATE_RUNNING, Started: m.opts.Clock.Now()}
	r := &jobRecord{info: info, job: job}
	m.jobs[id] = r
	m.wg.Add(1)
	m.mu.Unlock()
	// Publish RUNNING before starting the watcher, even if Wait returns immediately.
	m.emit(info)
	go m.watch(r)
	return info, nil
}

// StopOwned checks ownership while holding the same lock as lookup and expiry.
func (m *JobManager) StopOwned(sessionID, id string) (JobInfo, error) {
	m.mu.Lock()
	m.expireLocked()
	r := m.jobs[id]
	if r == nil || r.info.SessionID != sessionID {
		m.mu.Unlock()
		return JobInfo{}, ErrJobNotFound
	}
	if r.info.State != circusv1.JobEvent_STATE_RUNNING {
		info := r.info
		m.mu.Unlock()
		return info, nil
	}
	r.stopRequested = true
	m.mu.Unlock()
	return m.stop(r)
}

func (m *JobManager) watch(r *jobRecord) {
	defer m.wg.Done()
	code, err := r.job.Wait()
	if err != nil && code == 0 {
		code = -1
	}
	m.mu.Lock()
	r.info.ExitCode = int32(code)
	r.info.Ended = m.opts.Clock.Now()
	r.info.State = circusv1.JobEvent_STATE_EXITED
	if r.stopRequested {
		r.info.State = circusv1.JobEvent_STATE_KILLED
	}
	info := r.info
	m.mu.Unlock()
	m.emit(info)
}

func (m *JobManager) Get(id string) (JobInfo, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expireLocked()
	if r := m.jobs[id]; r != nil {
		return r.info, true
	}
	return JobInfo{}, false
}

func (m *JobManager) List(sessionID string) []JobInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expireLocked()
	infos := make([]JobInfo, 0)
	for _, r := range m.jobs {
		if r.info.SessionID == sessionID {
			infos = append(infos, r.info)
		}
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].ID < infos[j].ID })
	return infos
}

func (m *JobManager) Stop(id string) (JobInfo, error) {
	m.mu.Lock()
	m.expireLocked()
	r := m.jobs[id]
	if r == nil {
		m.mu.Unlock()
		return JobInfo{}, ErrJobNotFound
	}
	if r.info.State != circusv1.JobEvent_STATE_RUNNING {
		info := r.info
		m.mu.Unlock()
		return info, nil
	}
	r.stopRequested = true
	m.mu.Unlock()
	return m.stop(r)
}

func (m *JobManager) stop(r *jobRecord) (JobInfo, error) {
	r.stopOnce.Do(func() { r.stopErr = r.job.Stop() })
	m.mu.Lock()
	defer m.mu.Unlock()
	return r.info, r.stopErr
}

// StopSession requests stop for every running job owned by sessionID.
// Returned snapshots include retained jobs that had already ended.
func (m *JobManager) StopSession(sessionID string) []JobInfo {
	infos := m.List(sessionID)
	for i, info := range infos {
		if stopped, err := m.Stop(info.ID); err == nil {
			infos[i] = stopped
		}
	}
	return infos
}

func (m *JobManager) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	m.expireLocked()
	if !m.closed {
		m.closed = true
		close(m.stopSweep)
		for _, r := range m.jobs {
			if r.info.State != circusv1.JobEvent_STATE_RUNNING {
				continue
			}
			r.stopRequested = true
			m.wg.Add(1)
			go func() { defer m.wg.Done(); _, _ = m.stop(r) }()
		}
		go func() { m.wg.Wait(); close(m.stopped) }()
	}
	m.mu.Unlock()
	select {
	case <-m.stopped:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
