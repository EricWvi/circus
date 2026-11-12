package jobs

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
	"github.com/EricWvi/circus/internal/clock"
	"github.com/EricWvi/circus/internal/identifier"
	"github.com/EricWvi/circus/internal/wire"
	"google.golang.org/protobuf/proto"
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
	ID, SessionID, CallID, OutputPath string
	Seq                               uint64
	Command                           string
	State                             circusv1.JobEvent_State
	ExitCode                          int32
	Started, Ended                    time.Time
}

// Options configures job retention, events, and ID generation.
type Options struct {
	Clock        Clock
	RetainExited time.Duration
	// OnEvent runs serially in transition order and must return promptly.
	// Shutdown drains all queued callbacks.
	OnEvent func(*circusv1.JobEvent)
	// CallbackByteLimit bounds serialized callback backlog; default 8 << 20.
	// At most 4096 callbacks are retained. Overflow drops the new notification
	// and calls OnOverflow under the state lock; OnOverflow must not block.
	CallbackByteLimit int
	OnOverflow        func()
	NewID             func() string
}

type jobRecord struct {
	info     Info
	job      Job
	done     chan struct{}
	stopOnce sync.Once
	stopErr  error
	// stopRequested is set under the state lock by the first stop caller; it
	// keeps repeated requests from starting more than one stop goroutine.
	stopRequested bool
}

// Manager can be passed to tools at construction without changing Tool.
// OnEvent runs on a single goroutine, in state transition order. Callback storage
// owns at most CallbackByteLimit serialized bytes (default 8 MiB), 4096 entries
// (at most about 256 KiB of slice overhead), and one additional <=64 KiB event
// plus its decoded protobuf during delivery. Overflow drops notifications and
// invokes OnOverflow; the link client disconnects for snapshot recovery. Jobs
// themselves have no count cap; Snapshot bounds allocations independently.
type Manager struct {
	mu             sync.Mutex
	opts           Options
	jobs           map[string]*jobRecord
	reserved       map[string]struct{}
	seq            uint64
	closed         bool
	stopSweep      chan struct{}
	stopped        chan struct{}
	wg             sync.WaitGroup
	callbackQueue  [][]byte
	callbackBytes  int
	callbackWake   chan struct{}
	callbackDone   chan struct{}
	callbackClosed bool
}

// NewManager starts retention sweeping; nonpositive RetainExited defaults
// to thirty minutes.
func NewManager(opts Options) *Manager {
	if opts.CallbackByteLimit <= 0 {
		opts.CallbackByteLimit = 8 << 20
	}
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
func (m *Manager) emit(info *Info) {
	m.seq++
	info.Seq = m.seq
	if m.opts.OnEvent == nil {
		return
	}
	n := proto.Size(jobEvent(*info))
	if n > m.opts.CallbackByteLimit-m.callbackBytes || len(m.callbackQueue) >= 4096 {
		if m.opts.OnOverflow != nil {
			m.opts.OnOverflow()
		}
		return
	}
	data, err := proto.Marshal(jobEvent(*info))
	if err != nil {
		if m.opts.OnOverflow != nil {
			m.opts.OnOverflow()
		}
		return
	}
	m.callbackQueue = append(m.callbackQueue, data)
	m.callbackBytes += len(data)
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
		data := m.callbackQueue[0]
		m.callbackBytes -= len(data)
		m.callbackQueue[0] = nil
		m.callbackQueue = m.callbackQueue[1:]
		m.mu.Unlock()
		event := new(circusv1.JobEvent)
		if proto.Unmarshal(data, event) == nil {
			m.opts.OnEvent(event)
		}
	}
}

// Start allocates a job ID before launch and attributes events to callID.
// Malformed ownership IDs are rejected before launch. OutputPath is bounded
// to 4096 bytes in wire events; the full path remains available in Info.
func (m *Manager) Start(sessionID, callID, command string, launch func(id string) (Job, string, error)) (Info, error) {
	if !identifier.Valid(sessionID, false) || !identifier.Valid(callID, false) {
		return Info{}, ErrInvalidMetadata
	}
	m.mu.Lock()
	m.expireLocked()
	if m.closed {
		m.mu.Unlock()
		return Info{}, ErrShutdown
	}
	id := m.opts.NewID()
	if !identifier.Valid(id, false) {
		m.mu.Unlock()
		return Info{}, ErrInvalidMetadata
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
	info := Info{ID: id, SessionID: sessionID, CallID: callID, Command: command, OutputPath: outputPath, State: circusv1.JobEvent_STATE_RUNNING, Started: m.opts.Clock.Now()}
	r := &jobRecord{info: info, job: job, done: make(chan struct{})}
	m.jobs[id] = r
	m.wg.Add(1)
	m.emit(&r.info)
	info = r.info
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
	m.emit(&r.info)
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
	m.mu.Lock()
	r.stopRequested = true
	m.mu.Unlock()
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
			m.requestStopLocked(r)
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

// ErrInvalidMetadata rejects job ownership IDs outside the wire grammar.
var ErrInvalidMetadata = errors.New("invalid job ownership IDs")

// ErrSnapshotLimit reports omitted jobs exceeding the wire snapshot caps.
var ErrSnapshotLimit = errors.New("job snapshot exceeds wire limits")

func jobEvent(info Info) *circusv1.JobEvent {
	return &circusv1.JobEvent{JobId: info.ID, SessionId: info.SessionID, CallId: info.CallID, Seq: info.Seq, State: info.State, ExitCode: info.ExitCode, OutputPath: boundedPath(info.OutputPath)}
}

// Snapshot atomically captures retained jobs and their sequence high-water mark.
// At most 4096 entries and 8 MiB INCLUDING the uplink envelope are returned.
// On overflow it sets Truncated and returns a prefix and ErrSnapshotLimit.
// RUNNING jobs come first, then ended jobs; each group is ordered by job ID;
// callers must log the incomplete snapshot. Individual entries fit 64 KiB.
// Allocation is bounded by 8 MiB of encoded entries plus 4096 protobuf objects
// and at most 4096 candidate pointers; it never copies the unbounded job map.
func (m *Manager) Snapshot() (*circusv1.JobSnapshot, error) {
	return m.SnapshotSession("", 0)
}

// SnapshotSession applies the optional session filter before the Snapshot caps.
// Empty sessionID returns all jobs. Seq is always the boot-wide high-water mark.
// maxBytes further lowers the 8 MiB envelope cap, for example to a link's
// negotiated limit; nonpositive means 8 MiB. Exceeding it truncates as above.
func (m *Manager) SnapshotSession(sessionID string, maxBytes int) (*circusv1.JobSnapshot, error) {
	limit := wire.MaxSnapshotBytes
	if maxBytes > 0 {
		limit = min(limit, maxBytes)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expireLocked()
	snapshot := &circusv1.JobSnapshot{Seq: m.seq}
	// Retain only 4096 candidates, ordered by running state then job ID.
	ids := make([]string, 0, min(len(m.jobs), 4096))
	before := func(a, b string) bool {
		ar, br := m.jobs[a].info.State == circusv1.JobEvent_STATE_RUNNING, m.jobs[b].info.State == circusv1.JobEvent_STATE_RUNNING
		if ar != br {
			return ar
		}
		return a < b
	}
	count := 0
	for id, record := range m.jobs {
		if sessionID != "" && record.info.SessionID != sessionID {
			continue
		}
		count++
		at := sort.Search(len(ids), func(i int) bool { return !before(ids[i], id) })
		if at >= 4096 {
			continue
		}
		if len(ids) < 4096 {
			ids = append(ids, "")
		}
		copy(ids[at+1:], ids[at:len(ids)-1])
		ids[at] = id
	}
	var err error
	if count > len(ids) {
		err = ErrSnapshotLimit
		snapshot.Truncated = true
	}
	size := 18 // sequence, truncated flag and uplink envelope framing
	for _, id := range ids {
		event := jobEvent(m.jobs[id].info)
		n := proto.Size(event)
		if n > wire.MaxMetadataBytes || size+n+4 > limit {
			err = ErrSnapshotLimit
			snapshot.Truncated = true
			break
		}
		snapshot.Jobs = append(snapshot.Jobs, event)
		size += n + 4
	}
	return snapshot, err
}

// boundedPath keeps event metadata below 64 KiB without stopping an already
// launched process. Real OS paths fit 4096 bytes; malformed longer paths use a
// fixed diagnostic rather than a misleading truncated filesystem path.
func boundedPath(path string) string {
	if len(path) > 4096 {
		return "[output path exceeds 4096 bytes]"
	}
	return path
}

// RequestStopSession requests stop for matching running jobs and returns
// without waiting for Job.Stop or Wait, so the link reader never blocks on a
// stubborn process. Each running job gets at most one stop goroutine for its
// lifetime, however often stop is requested, so these goroutines are bounded
// by the running jobs (each of which already has a watcher goroutine) and stop
// in parallel. KILLED/EXITED is emitted by the job watcher.
//
// A job whose Stop never returns keeps its stop goroutine (and its watcher)
// for the life of the process. Every later stop of that job, including
// StopOwned from a TaskStop call, waits in the same sync.Once for that Stop.
// Shutdown is not held by it: it returns when its ctx expires.
func (m *Manager) RequestStopSession(sessionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.jobs {
		if r.info.SessionID == sessionID && r.info.State == circusv1.JobEvent_STATE_RUNNING {
			m.requestStopLocked(r)
		}
	}
}

// requestStopLocked must be called with r RUNNING: its watcher then still holds
// a wg count, so this Add never races Shutdown's Wait from zero.
func (m *Manager) requestStopLocked(r *jobRecord) {
	if r.stopRequested {
		return
	}
	r.stopRequested = true
	m.wg.Add(1)
	go func() { defer m.wg.Done(); _, _ = m.stop(r) }()
}
