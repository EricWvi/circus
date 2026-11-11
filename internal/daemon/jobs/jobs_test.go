package jobs_test

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/clock"
	"github.com/EricWvi/circus/internal/daemon/jobs"
	"github.com/EricWvi/circus/internal/testutil/leakcheck"
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
		_, err := m.Start("session", "echo hi", func(string) (jobs.Job, string, error) {
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
	return m.Start(session, "", func(string) (jobs.Job, string, error) { return job, path, nil })
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
		_, err := m.Start("s", "command", func(string) (jobs.Job, string, error) {
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
