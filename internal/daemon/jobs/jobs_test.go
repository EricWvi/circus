package jobs_test

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/clock"
	"github.com/EricWvi/circus/internal/daemon/jobs"
	"github.com/EricWvi/circus/internal/testutil/leakcheck"
	"github.com/EricWvi/circus/internal/wire"
	"google.golang.org/protobuf/proto"
)

type jobExit struct {
	code int
	err  error
}
type fakeJob struct {
	exit     chan jobExit
	stops    atomic.Int32
	stopOnce sync.Once
}

func newFakeJob() *fakeJob            { return &fakeJob{exit: make(chan jobExit, 1)} }
func (j *fakeJob) Wait() (int, error) { result := <-j.exit; return result.code, result.err }
func (j *fakeJob) Stop() error {
	j.stops.Add(1)
	j.stopOnce.Do(func() { j.exit <- jobExit{code: 143, err: jobs.ErrKilled} })
	return nil
}

func newJobs(t *testing.T, opts jobs.Options) *jobs.Manager {
	t.Helper()
	leakcheck.Track(t)
	m := jobs.NewManager(opts)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := m.Shutdown(ctx); err != nil {
			t.Errorf("jobs shutdown: %v", err)
		}
	})
	return m
}

func TestJobManagerLifecycleAndRetention(t *testing.T) {
	clock := clock.New()
	events := make(chan *circusv1.JobEvent, 16)
	m := newJobs(t, jobs.Options{Clock: clock, RetainExited: time.Minute, OnEvent: func(e *circusv1.JobEvent) { events <- e }})
	job := newFakeJob()
	info, err := addJob(m, "session", "/output", job)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[a-z0-9]{1,16}$`).MatchString(info.ID) || info.State != circusv1.JobEvent_STATE_RUNNING || !info.Started.Equal(clock.Now()) {
		t.Fatalf("added: %+v", info)
	}
	event := receive(t, events)
	if event.State != circusv1.JobEvent_STATE_RUNNING || event.SessionId != "session" || event.OutputPath != "/output" || event.JobId != info.ID {
		t.Fatalf("running event: %v", event)
	}
	if got, ok := m.Get(info.ID); !ok || got != info {
		t.Fatalf("get: %+v, %v", got, ok)
	}
	clock.Advance(time.Second)
	job.exit <- jobExit{code: 7}
	if e := receive(t, events); e.State != circusv1.JobEvent_STATE_EXITED || e.ExitCode != 7 {
		t.Fatalf("exit event: %v", e)
	}
	ended, ok := m.Get(info.ID)
	if !ok || ended.ExitCode != 7 || !ended.Ended.Equal(clock.Now()) {
		t.Fatalf("ended: %+v", ended)
	}
	if _, err := m.StopOwned("session", info.ID); !errors.Is(err, jobs.ErrNotFound) || job.stops.Load() != 0 {
		t.Fatalf("stop ended: %v, stops %d", err, job.stops.Load())
	}
	clock.Advance(time.Minute)
	if _, ok := m.Get(info.ID); ok {
		t.Fatal("expired job retained")
	}
	if _, err := m.StopOwned("session", info.ID); !errors.Is(err, jobs.ErrNotFound) {
		t.Fatalf("expired stop: %v", err)
	}
}

func TestJobManagerSessionStopAndShutdown(t *testing.T) {
	events := make(chan *circusv1.JobEvent, 32)
	var ids int
	m := newJobs(t, jobs.Options{OnEvent: func(e *circusv1.JobEvent) { events <- e }, NewID: func() string { ids++; return fmt.Sprintf("job%d", ids) }})
	processes := []*fakeJob{newFakeJob(), newFakeJob(), newFakeJob()}
	infos := make([]jobs.Info, len(processes))
	for i, job := range processes {
		session := "a"
		if i == 2 {
			session = "b"
		}
		info, err := addJob(m, session, "", job)
		if err != nil {
			t.Fatal(err)
		}
		infos[i] = info
		if e := receive(t, events); e.State != circusv1.JobEvent_STATE_RUNNING {
			t.Fatalf("first event: %v", e)
		}
	}
	var stoppers sync.WaitGroup
	for range 16 {
		stoppers.Go(func() {
			if _, err := m.StopOwned("a", infos[0].ID); err != nil && !errors.Is(err, jobs.ErrNotFound) {
				t.Error(err)
			}
		})
	}
	stoppers.Wait()
	if e := receive(t, events); e.State != circusv1.JobEvent_STATE_KILLED || e.JobId != infos[0].ID {
		t.Fatalf("stop event: %v", e)
	}
	if processes[0].stops.Load() != 1 {
		t.Fatalf("stop ran %d times", processes[0].stops.Load())
	}
	if stopped := m.StopSession("a"); len(stopped) != 2 {
		t.Fatalf("session snapshots: %+v", stopped)
	}
	if e := receive(t, events); e.State != circusv1.JobEvent_STATE_KILLED || e.JobId != infos[1].ID {
		t.Fatalf("session stop: %v", e)
	}
	if info, _ := m.Get(infos[2].ID); info.State != circusv1.JobEvent_STATE_RUNNING || processes[2].stops.Load() != 0 {
		t.Fatalf("other session stopped: %+v", info)
	}
	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if e := receive(t, events); e.State != circusv1.JobEvent_STATE_KILLED || e.JobId != infos[2].ID {
		t.Fatalf("shutdown event: %v", e)
	}
	if _, err := addJob(m, "a", "", newFakeJob()); !errors.Is(err, jobs.ErrShutdown) {
		t.Fatalf("add after shutdown: %v", err)
	}
	select {
	case e := <-events:
		t.Fatalf("extra event: %v", e)
	default:
	}
}

func TestJobManagerImmediateExitAndWaitError(t *testing.T) {
	events := make(chan *circusv1.JobEvent, 32)
	m := newJobs(t, jobs.Options{OnEvent: func(e *circusv1.JobEvent) { events <- e }})
	for range 8 {
		job := newFakeJob()
		job.exit <- jobExit{err: errors.New("wait failed")}
		info, err := addJob(m, "s", "", job)
		if err != nil {
			t.Fatal(err)
		}
		for _, state := range []circusv1.JobEvent_State{circusv1.JobEvent_STATE_RUNNING, circusv1.JobEvent_STATE_EXITED} {
			e := receive(t, events)
			if e.JobId != info.ID || e.State != state {
				t.Fatalf("event order: %v", e)
			}
			if state == circusv1.JobEvent_STATE_EXITED && e.ExitCode != -1 {
				t.Fatalf("wait error exit code: %v", e)
			}
		}
	}
}

func TestJobManagerLaunchDoesNotBlockLookups(t *testing.T) {
	m := newJobs(t, jobs.Options{NewID: func() string { return "job" }})
	entered := make(chan struct{})
	release := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	started := make(chan error, 1)
	go func() {
		_, err := m.Start("session", "call", "echo hi", func(string) (jobs.Job, string, error) {
			close(entered)
			<-release
			return newFakeJob(), "/output", nil
		})
		started <- err
	}()
	receive(t, entered)

	type lookupResult struct {
		found bool
	}
	lookups := make(chan lookupResult, 1)
	go func() {
		_, found := m.Get("job")
		lookups <- lookupResult{found: found}
	}()
	select {
	case result := <-lookups:
		if result.found {
			t.Fatalf("reserved job was visible: %+v", result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("lookups blocked while launch was running")
	}

	unblock()
	if err := receive(t, started); err != nil {
		t.Fatal(err)
	}
	if info, ok := m.Get("job"); !ok || info.Command != "echo hi" {
		t.Fatalf("started job: %+v, %v", info, ok)
	}
}

type slowStopJob struct {
	stopStarted chan struct{}
	release     chan struct{}
}

func (j slowStopJob) Wait() (int, error) { <-j.release; return 0, nil }
func (j slowStopJob) Stop() error        { close(j.stopStarted); <-j.release; return nil }

func TestJobManagerShutdownHonorsContext(t *testing.T) {
	m := newJobs(t, jobs.Options{})
	job := slowStopJob{stopStarted: make(chan struct{}), release: make(chan struct{})}
	if _, err := addJob(m, "s", "", job); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.Shutdown(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("shutdown: %v", err)
	}
	receive(t, job.stopStarted)
	close(job.release)
	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func addJob(m *jobs.Manager, session, path string, job jobs.Job) (jobs.Info, error) {
	return m.Start(session, "call", "", func(string) (jobs.Job, string, error) { return job, path, nil })
}
func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for event")
		var zero T
		return zero
	}
}

func TestNaturalExitWhileStopIsInFlight(t *testing.T) {
	events := make(chan *circusv1.JobEvent, 4)
	m := newJobs(t, jobs.Options{OnEvent: func(e *circusv1.JobEvent) { events <- e }})
	job := slowStopJob{stopStarted: make(chan struct{}), release: make(chan struct{})}
	info, err := addJob(m, "session", "", job)
	if err != nil {
		t.Fatal(err)
	}
	receive(t, events)
	stopped := make(chan error, 1)
	go func() { _, err := m.StopOwned("session", info.ID); stopped <- err }()
	receive(t, job.stopStarted)
	close(job.release)
	if err := receive(t, stopped); err != nil {
		t.Fatal(err)
	}
	if e := receive(t, events); e.State != circusv1.JobEvent_STATE_EXITED || e.ExitCode != 0 {
		t.Fatal(e)
	}
}

func TestEventsSerializedWithoutBlockingStateChanges(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	events := make(chan *circusv1.JobEvent, 8)
	m := newJobs(t, jobs.Options{OnEvent: func(e *circusv1.JobEvent) {
		once.Do(func() { close(entered); <-release })
		events <- e
	}})
	first := newFakeJob()
	firstInfo, err := addJob(m, "s", "", first)
	if err != nil {
		t.Fatal(err)
	}
	receive(t, entered)
	second := newFakeJob()
	secondInfo, err := addJob(m, "s", "", second)
	if err != nil {
		t.Fatal(err)
	}
	first.exit <- jobExit{code: 4}
	// Wait for the state transition without waiting for the blocked callback.
	deadline := time.After(2 * time.Second)
	for {
		if info, ok := m.Get(firstInfo.ID); ok && info.State == circusv1.JobEvent_STATE_EXITED {
			break
		}
		select {
		case <-deadline:
			t.Fatal("callback blocked state transitions")
		default:
			runtime.Gosched()
		}
	}
	select {
	case e := <-events:
		t.Fatalf("callback executed concurrently: %v", e)
	default:
	}
	unblock()
	for _, expected := range []struct {
		id    string
		state circusv1.JobEvent_State
	}{
		{firstInfo.ID, circusv1.JobEvent_STATE_RUNNING},
		{secondInfo.ID, circusv1.JobEvent_STATE_RUNNING},
		{firstInfo.ID, circusv1.JobEvent_STATE_EXITED},
	} {
		if e := receive(t, events); e.JobId != expected.id || e.State != expected.state {
			t.Fatalf("event order: %v", e)
		}
	}
}

func TestShutdownWaitsForLaunchingJob(t *testing.T) {
	m := newJobs(t, jobs.Options{})
	entered, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	process := newFakeJob()
	started := make(chan error, 1)
	go func() {
		_, err := m.Start("s", "call", "command", func(string) (jobs.Job, string, error) {
			close(entered)
			<-release
			return process, "", nil
		})
		started <- err
	}()
	receive(t, entered)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.Shutdown(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("shutdown with launch in flight: %v", err)
	}
	timeoutCtx, timeoutCancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer timeoutCancel()
	if err := m.Shutdown(timeoutCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown returned before launch cleanup: %v", err)
	}
	unblock()
	if err := receive(t, started); !errors.Is(err, jobs.ErrShutdown) {
		t.Fatal(err)
	}
	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if process.stops.Load() != 1 {
		t.Fatal("shutdown did not stop rejected launch")
	}
}

func TestSnapshotAtomicityAndStrictSequence(t *testing.T) {
	const count = 64
	events := make(chan *circusv1.JobEvent, 2*count)
	m := newJobs(t, jobs.Options{OnEvent: func(e *circusv1.JobEvent) { events <- e }})
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			job := newFakeJob()
			job.exit <- jobExit{code: i}
			_, err := m.Start("session", fmt.Sprintf("call-%d", i), "cmd", func(string) (jobs.Job, string, error) { return job, "/output", nil })
			if err != nil {
				t.Error(err)
			}
		}(i)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	snapshots := make([]*circusv1.JobSnapshot, 0, 128)
	for i := 0; i < 128; i++ {
		snap, err := m.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		snapshots = append(snapshots, snap)
		runtime.Gosched()
	}
	<-done
	history := make(map[string][]*circusv1.JobEvent)
	for seq := uint64(1); seq <= 2*count; seq++ {
		event := receive(t, events)
		if event.Seq != seq || event.CallId == "" {
			t.Fatalf("sequence %d: %v", seq, event)
		}
		history[event.JobId] = append(history[event.JobId], event)
	}
	final, err := m.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	snapshots = append(snapshots, final)
	if final.Seq != 2*count || len(final.Jobs) != count {
		t.Fatal(final)
	}
	for _, snap := range snapshots {
		got := make(map[string]*circusv1.JobEvent)
		for _, e := range snap.Jobs {
			if e.Seq > snap.Seq {
				t.Fatalf("event beyond snapshot: %v / %d", e, snap.Seq)
			}
			got[e.JobId] = e
		}
		for id, events := range history {
			var want *circusv1.JobEvent
			for _, e := range events {
				if e.Seq <= snap.Seq {
					want = e
				}
			}
			if !proto.Equal(want, got[id]) {
				t.Fatalf("non-atomic snapshot at %d for %s: got %v want %v", snap.Seq, id, got[id], want)
			}
		}
	}
	// Callers own snapshots; mutation cannot affect retained state.
	final.Jobs[0].CallId = "mutated"
	fresh, _ := m.Snapshot()
	for _, e := range fresh.Jobs {
		if e.CallId == "mutated" {
			t.Fatal("snapshot aliases state")
		}
	}
}

func TestSnapshotWireCaps(t *testing.T) {
	for _, tc := range []struct {
		name             string
		count, pathBytes int
	}{{"entry-count", 4097, 10}, {"aggregate-bytes", 2200, 4096}} {
		t.Run(tc.name, func(t *testing.T) {
			var counter int
			m := newJobs(t, jobs.Options{NewID: func() string { counter++; return fmt.Sprintf("%012x", counter) }})
			startExitedJobs(t, m, tc.count, strings.Repeat("x", tc.pathBytes))
			snapshot, err := m.Snapshot()
			if !snapshot.Truncated || !errors.Is(err, jobs.ErrSnapshotLimit) || len(snapshot.Jobs) > 4096 || len(snapshot.Jobs) >= tc.count {
				t.Fatalf("limit: count=%d err=%v", len(snapshot.Jobs), err)
			}
			envelope := &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_JobSnapshot{JobSnapshot: snapshot}}
			data, err := proto.Marshal(envelope)
			if err != nil {
				t.Fatal(err)
			}
			if len(data) > 8<<20 {
				t.Fatalf("oversize snapshot: %d", len(data))
			}
			if _, err = wire.Decode(wire.DaemonUplink, data, 64<<20); err != nil {
				t.Fatalf("not sendable: %v", err)
			}
			for _, e := range snapshot.Jobs {
				if proto.Size(e) > 64<<10 {
					t.Fatal("oversize event")
				}
			}
		})
	}
	t.Run("oversize-event", func(t *testing.T) {
		m := newJobs(t, jobs.Options{})
		job := newFakeJob()
		_, err := m.Start("s", "c", "cmd", func(string) (jobs.Job, string, error) { return job, strings.Repeat("x", 64<<10), nil })
		if err != nil || job.stops.Load() != 0 {
			t.Fatalf("err=%v stops=%d", err, job.stops.Load())
		}
		snapshot, _ := m.Snapshot()
		if snapshot.Seq != 1 || len(snapshot.Jobs) != 1 || len(snapshot.Jobs[0].OutputPath) > 4096 {
			t.Fatal(snapshot)
		}
		if snapshot.Jobs[0].OutputPath != "[output path exceeds 4096 bytes]" {
			t.Fatal(snapshot)
		}

	})
}

func TestJobCallbackOverflowAndFilteredSnapshot(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var overflows atomic.Int32
	m := newJobs(t, jobs.Options{CallbackByteLimit: 128, OnOverflow: func() { overflows.Add(1) }, OnEvent: func(*circusv1.JobEvent) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
	}})
	job := newFakeJob()
	if _, err := m.Start("first", "first-call", "cmd", func(string) (jobs.Job, string, error) { return job, "/output", nil }); err != nil {
		t.Fatal(err)
	}
	receive(t, entered)
	for i := 0; i < 20; i++ {
		job := newFakeJob()
		job.exit <- jobExit{}
		if _, err := m.Start("other", fmt.Sprintf("call-%d", i), "cmd", func(string) (jobs.Job, string, error) { return job, "/output", nil }); err != nil {
			t.Fatal(err)
		}
	}
	if overflows.Load() == 0 {
		t.Fatal("event callback backlog was not bounded")
	}
	snapshot, err := m.SnapshotSession("first", 0)
	if err != nil || len(snapshot.Jobs) != 1 || snapshot.Jobs[0].CallId != "first-call" {
		t.Fatalf("filtered snapshot: %v %v", snapshot, err)
	}
	close(release)
	var launched bool
	if _, err = m.Start("s", "", "cmd", func(string) (jobs.Job, string, error) { launched = true; return nil, "", nil }); !errors.Is(err, jobs.ErrInvalidMetadata) || launched {
		t.Fatalf("invalid ownership: %v launched=%v", err, launched)
	}
}

// startExitedJobs starts n jobs that exit immediately and waits until all have
// exited. It waits after every 256 starts: each live watcher goroutine is
// costly under -race, and thousands at once exceed leakcheck's RSS limit.
func startExitedJobs(t *testing.T, m *jobs.Manager, n int, outputPath string) {
	t.Helper()
	base, _ := m.Snapshot()
	for i := 0; i < n; i++ {
		job := newFakeJob()
		job.exit <- jobExit{}
		if _, err := m.Start("s", fmt.Sprintf("c%d", i), "cmd", func(string) (jobs.Job, string, error) { return job, outputPath, nil }); err != nil {
			t.Fatal(err)
		}
		if i%256 == 255 || i == n-1 {
			// Each job emits RUNNING then EXITED.
			want := base.Seq + uint64(2*(i+1))
			eventually(t, func() bool { snap, _ := m.Snapshot(); return snap.Seq == want })
		}
	}
}

func TestTruncatedSnapshotKeepsRunningJobs(t *testing.T) {
	var counter int
	m := newJobs(t, jobs.Options{NewID: func() string { counter++; return fmt.Sprintf("%012x", counter) }})
	startExitedJobs(t, m, 4096, "/o")
	for i := 0; i < 4; i++ {
		job := newFakeJob()
		if _, err := m.Start("s", fmt.Sprintf("running-%d", i), "cmd", func(string) (jobs.Job, string, error) { return job, "/o", nil }); err != nil {
			t.Fatal(err)
		}
	}
	snap, err := m.Snapshot()
	if !errors.Is(err, jobs.ErrSnapshotLimit) || !snap.Truncated || len(snap.Jobs) != 4096 {
		t.Fatalf("count=%d truncated=%v err=%v", len(snap.Jobs), snap.Truncated, err)
	}
	for i, e := range snap.Jobs {
		if i < 4 && e.State != circusv1.JobEvent_STATE_RUNNING || i >= 4 && e.State == circusv1.JobEvent_STATE_RUNNING {
			t.Fatalf("running priority at %d: %v", i, e)
		}
	}
}

func TestSnapshotSequenceSharesStateLock(t *testing.T) {
	// With no expiry or truncation, the high-water mark must equal the newest
	// retained entry. Writers contend with snapshots throughout each round;
	// capturing seq before or after the state lock violates this equality.
	for round := 0; round < 4; round++ {
		m := newJobs(t, jobs.Options{})
		start := make(chan struct{})
		var writers, readers sync.WaitGroup
		for w := 0; w < 4; w++ {
			writers.Add(1)
			go func(w int) {
				defer writers.Done()
				<-start
				for i := 0; i < 8; i++ {
					job := newFakeJob()
					job.exit <- jobExit{}
					_, err := m.Start("s", fmt.Sprintf("call-%d-%d", w, i), "cmd", func(string) (jobs.Job, string, error) { return job, "/o", nil })
					if err != nil {
						t.Error(err)
						return
					}
				}
			}(w)
		}
		for range 2 {
			readers.Add(1)
			go func() {
				defer readers.Done()
				<-start
				for range 64 {
					snap, err := m.Snapshot()
					if err != nil {
						t.Error(err)
						return
					}
					var newest uint64
					for _, e := range snap.Jobs {
						newest = max(newest, e.Seq)
					}
					if newest != snap.Seq {
						t.Errorf("seq outside state lock: watermark=%d newest=%d", snap.Seq, newest)
						return
					}
					runtime.Gosched()
				}
			}()
		}
		close(start)
		writers.Wait()
		readers.Wait()
		eventually(t, func() bool { snap, _ := m.Snapshot(); return snap.Seq == 64 })
	}
}

type heldStopJob struct {
	permit, done chan struct{}
	entered      chan<- struct{}
	stops        atomic.Int32
}

func (j *heldStopJob) Wait() (int, error) { <-j.done; return 143, jobs.ErrKilled }
func (j *heldStopJob) Stop() error {
	j.stops.Add(1)
	j.entered <- struct{}{}
	<-j.permit
	close(j.done)
	return nil
}

// A stubborn Stop must neither block RequestStopSession nor serialize other
// jobs' stops; repeated requests start no second Stop for the same job.
func TestRequestStopSessionIsAsyncParallelAndOncePerJob(t *testing.T) {
	permit := make(chan struct{})
	defer func() {
		select {
		case <-permit:
		default:
			close(permit)
		}
	}()
	const count = 32
	entered := make(chan struct{}, 2*count)
	m := newJobs(t, jobs.Options{})
	held := make([]*heldStopJob, count)
	ids := make([]string, count)
	for i := range held {
		held[i] = &heldStopJob{permit: permit, done: make(chan struct{}), entered: entered}
		info, err := m.Start("s", fmt.Sprintf("c-%d", i), "cmd", func(string) (jobs.Job, string, error) { return held[i], "/o", nil })
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = info.ID
	}
	other := newFakeJob()
	if _, err := m.Start("other", "c-other", "cmd", func(string) (jobs.Job, string, error) { return other, "/o", nil }); err != nil {
		t.Fatal(err)
	}
	for range 100 {
		m.RequestStopSession("s") // returns although every Stop is still held
	}
	for range count {
		receive(t, entered) // all stops run concurrently, none waits for another
	}
	m.RequestStopSession("s")
	if other.stops.Load() != 0 {
		t.Fatal("session stop reached another session's job")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.Shutdown(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	close(permit)
	for i, job := range held {
		eventually(t, func() bool { info, _ := m.Get(ids[i]); return info.State == circusv1.JobEvent_STATE_KILLED })
		if n := job.stops.Load(); n != 1 {
			t.Fatalf("job %d stopped %d times", i, n)
		}
	}
	if len(entered) != 0 || other.stops.Load() != 1 {
		t.Fatalf("extra stops: entered=%d other=%d", len(entered), other.stops.Load())
	}
}

func eventually(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !check() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		runtime.Gosched()
	}
}
