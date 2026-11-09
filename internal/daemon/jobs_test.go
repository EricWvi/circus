package daemon_test

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/daemon"
	"github.com/EricWvi/circus/internal/daemon/clocktest"
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
	j.stopOnce.Do(func() { j.exit <- jobExit{code: -1} })
	return nil
}

func newJobs(t *testing.T, opts daemon.JobOptions) *daemon.JobManager {
	t.Helper()
	m := daemon.NewJobManager(opts)
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
	clock := clocktest.New()
	events := make(chan *circusv1.JobEvent, 16)
	m := newJobs(t, daemon.JobOptions{Clock: clock, RetainExited: time.Minute, OnEvent: func(e *circusv1.JobEvent) { events <- e }})
	job := newFakeJob()
	info, err := m.Add("session", "/output", job)
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
	if infos := m.List("session"); len(infos) != 1 || infos[0] != info {
		t.Fatalf("list: %+v", infos)
	}
	if len(m.List("other")) != 0 {
		t.Fatal("list leaked another session")
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
	if _, err := m.Stop(info.ID); err != nil || job.stops.Load() != 0 {
		t.Fatalf("stop ended: %v, stops %d", err, job.stops.Load())
	}
	clock.Advance(time.Minute)
	if _, ok := m.Get(info.ID); ok || len(m.List("session")) != 0 {
		t.Fatal("expired job retained")
	}
	if _, err := m.Stop(info.ID); !errors.Is(err, daemon.ErrJobNotFound) {
		t.Fatalf("expired stop: %v", err)
	}
}

func TestJobManagerSessionStopAndShutdown(t *testing.T) {
	events := make(chan *circusv1.JobEvent, 32)
	var ids int
	m := newJobs(t, daemon.JobOptions{OnEvent: func(e *circusv1.JobEvent) { events <- e }, NewID: func() string { ids++; return fmt.Sprintf("job%d", ids) }})
	jobs := []*fakeJob{newFakeJob(), newFakeJob(), newFakeJob()}
	infos := make([]daemon.JobInfo, len(jobs))
	for i, job := range jobs {
		session := "a"
		if i == 2 {
			session = "b"
		}
		info, err := m.Add(session, "", job)
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
			if _, err := m.Stop(infos[0].ID); err != nil {
				t.Error(err)
			}
		})
	}
	stoppers.Wait()
	if e := receive(t, events); e.State != circusv1.JobEvent_STATE_KILLED || e.JobId != infos[0].ID {
		t.Fatalf("stop event: %v", e)
	}
	if jobs[0].stops.Load() != 1 {
		t.Fatalf("stop ran %d times", jobs[0].stops.Load())
	}
	if stopped := m.StopSession("a"); len(stopped) != 2 {
		t.Fatalf("session snapshots: %+v", stopped)
	}
	if e := receive(t, events); e.State != circusv1.JobEvent_STATE_KILLED || e.JobId != infos[1].ID {
		t.Fatalf("session stop: %v", e)
	}
	if info, _ := m.Get(infos[2].ID); info.State != circusv1.JobEvent_STATE_RUNNING || jobs[2].stops.Load() != 0 {
		t.Fatalf("other session stopped: %+v", info)
	}
	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if e := receive(t, events); e.State != circusv1.JobEvent_STATE_KILLED || e.JobId != infos[2].ID {
		t.Fatalf("shutdown event: %v", e)
	}
	if _, err := m.Add("a", "", newFakeJob()); !errors.Is(err, daemon.ErrShutdown) {
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
	m := newJobs(t, daemon.JobOptions{OnEvent: func(e *circusv1.JobEvent) { events <- e }})
	for range 8 {
		job := newFakeJob()
		job.exit <- jobExit{err: errors.New("wait failed")}
		info, err := m.Add("s", "", job)
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
	m := newJobs(t, daemon.JobOptions{NewID: func() string { return "job" }})
	entered := make(chan struct{})
	release := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	started := make(chan error, 1)
	go func() {
		_, err := m.Start("session", "echo hi", func(string) (daemon.Job, string, error) {
			close(entered)
			<-release
			return newFakeJob(), "/output", nil
		})
		started <- err
	}()
	receive(t, entered)

	type lookupResult struct {
		found bool
		infos []daemon.JobInfo
	}
	lookups := make(chan lookupResult, 1)
	go func() {
		_, found := m.Get("job")
		lookups <- lookupResult{found: found, infos: m.List("session")}
	}()
	select {
	case result := <-lookups:
		if result.found || len(result.infos) != 0 {
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
	m := newJobs(t, daemon.JobOptions{})
	job := slowStopJob{stopStarted: make(chan struct{}), release: make(chan struct{})}
	if _, err := m.Add("s", "", job); err != nil {
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
