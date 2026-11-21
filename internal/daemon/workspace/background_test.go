package workspace_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/daemon/jobs"
	"github.com/EricWvi/circus/internal/daemon/workspace"
)

// foregroundTimers is what a prepare call arms on the fake clock: the
// preparation fallback, then the execution deadline and its fallback.
const foregroundTimers = 3

// toBackground starts a gated clone of url and lets its 2-minute deadline
// pass; it returns the job ID.
func (f *fixture) toBackground(path, url string) string {
	f.t.Helper()
	before := f.clock.Waiters()
	id := f.submit(f.prepareCall(path, url, ""))
	f.armed(before, foregroundTimers)
	f.started(1)
	f.clock.Advance(2 * time.Minute)
	r := f.wait(id).GetPrepareWorkspace()
	if r.Outcome != circusv1.PrepareWorkspaceResult_OUTCOME_IN_BACKGROUND || r.JobId == "" {
		f.t.Fatalf("result = %v, want IN_BACKGROUND", r)
	}
	info, ok := f.jobs.Get(r.JobId)
	if !ok || info.State != circusv1.JobEvent_STATE_RUNNING || info.SessionID != session || info.CallID != id {
		f.t.Fatalf("job = %+v, %v", info, ok)
	}
	return r.JobId
}

// resume submits a resume call and returns its ID once its deadline is armed.
func (f *fixture) resume(path, url, jobID string) string {
	f.t.Helper()
	before := f.clock.Waiters()
	id := f.submit(f.prepareCall(path, url, jobID))
	f.armed(before, foregroundTimers)
	return id
}

func (f *fixture) jobEnded(id string) jobs.Info {
	f.t.Helper()
	var info jobs.Info
	eventually(f.t, "the job to end", func() bool {
		var ok bool
		info, ok = f.jobs.Get(id)
		return ok && info.State != circusv1.JobEvent_STATE_RUNNING
	})
	return info
}

func TestBackgroundResumeWhileRunning(t *testing.T) {
	f := newFixture(t)
	url := f.bareRepo("one")
	f.gate()
	job := f.toBackground(request, url)
	if info, _ := f.jobs.Get(job); info.Command != "git clone "+url {
		t.Fatalf("job command = %q", info.Command)
	}
	// Still running at the resume call's deadline: still in the background.
	id := f.resume(request, url, job)
	f.clock.Advance(2 * time.Minute)
	r := f.wait(id).GetPrepareWorkspace()
	if r.Outcome != circusv1.PrepareWorkspaceResult_OUTCOME_IN_BACKGROUND || r.JobId != job || r.Path != f.target() {
		t.Fatalf("resume = %v, want IN_BACKGROUND %s", r, job)
	}
	// Finishing while a resume call waits answers that call.
	id = f.resume(request, url, job)
	f.release()
	expectReady(t, f.wait(id).GetPrepareWorkspace(), cloned)
	if info := f.jobEnded(job); info.State != circusv1.JobEvent_STATE_EXITED || info.ExitCode != 0 {
		t.Fatalf("job ended %+v", info)
	}
	f.expectClone(f.target(), url)
}

func TestBackgroundResumeFinishedAndUnknown(t *testing.T) {
	f := newFixture(t)
	url := f.bareRepo("one")
	f.gate()
	job := f.toBackground(request, url)
	f.release()
	f.jobEnded(job)
	f.idle()
	// Finished: the retained outcome, repeatedly.
	for range 2 {
		expectReady(t, f.wait(f.submit(f.prepareCall(request, url, job))).GetPrepareWorkspace(), cloned)
	}
	// Another session's resume does not see it and prepares normally.
	other := f.prepareCall(request, url, job)
	other.SessionId = "workspace-other"
	expectReady(t, f.wait(f.submit(other)).GetPrepareWorkspace(), usedRepository)
	// Unknown job: prepares as if unset.
	expectReady(t, f.wait(f.submit(f.prepareCall(request, url, "0123456789ab"))).GetPrepareWorkspace(), usedRepository)
	// Retention expired: unknown as well.
	f.clock.Advance(workspace.ResultRetention)
	expectReady(t, f.wait(f.submit(f.prepareCall(request, url, job))).GetPrepareWorkspace(), usedRepository)
}

func TestBackgroundResumeOfRunningJobFromAnotherSessionIsBusy(t *testing.T) {
	f := newFixture(t)
	url := f.bareRepo("one")
	f.gate()
	job := f.toBackground(request, url)
	other := f.prepareCall(request, url, job)
	other.SessionId = "workspace-other"
	expectFailed(t, f.wait(f.submit(other)).GetPrepareWorkspace(), busy)
	f.release()
	f.jobEnded(job)
}

func TestBackgroundLifetime(t *testing.T) {
	f := newFixture(t)
	url := f.bareRepo("one")
	f.gate()
	job := f.toBackground(request, url)
	f.clock.Advance(workspace.BackgroundLifetime)
	info := f.jobEnded(job)
	if info.State != circusv1.JobEvent_STATE_KILLED {
		t.Fatalf("job ended %+v, want KILLED", info)
	}
	expectFailed(t, f.wait(f.submit(f.prepareCall(request, url, job))).GetPrepareWorkspace(), timedOut)
	f.expectNoLeftovers(f.projects)
	if _, err := os.Lstat(f.target()); !os.IsNotExist(err) {
		t.Fatalf("expired clone created the target: %v", err)
	}
}

func TestCancelForeground(t *testing.T) {
	f := newFixture(t)
	url := f.bareRepo("one")
	f.gate()
	id := f.submit(f.prepareCall(request, url, ""))
	f.started(1)
	f.d.Cancel(id)
	expectFailed(t, f.wait(id).GetPrepareWorkspace(), cancelled)
	f.expectNoLeftovers(f.projects)
	if _, err := os.Lstat(f.target()); !os.IsNotExist(err) {
		t.Fatalf("cancelled clone created the target: %v", err)
	}
	// The slot and lock are free again.
	os.Remove(filepath.Join(f.ctl, "gate"))
	expectReady(t, f.prepareNow(request, url), cloned)
}

func TestCancelBackground(t *testing.T) {
	t.Run("stop session jobs", func(t *testing.T) {
		f := newFixture(t)
		url := f.bareRepo("one")
		f.gate()
		job := f.toBackground(request, url)
		f.jobs.StopSession(session)
		if info := f.jobEnded(job); info.State != circusv1.JobEvent_STATE_KILLED {
			t.Fatalf("job ended %+v, want KILLED", info)
		}
		expectFailed(t, f.wait(f.submit(f.prepareCall(request, url, job))).GetPrepareWorkspace(), cancelled)
		f.expectNoLeftovers(f.projects)
	})
	t.Run("cancel the resume call", func(t *testing.T) {
		f := newFixture(t)
		url := f.bareRepo("one")
		f.gate()
		job := f.toBackground(request, url)
		id := f.resume(request, url, job)
		f.d.Cancel(id)
		expectFailed(t, f.wait(id).GetPrepareWorkspace(), cancelled)
		if info := f.jobEnded(job); info.State != circusv1.JobEvent_STATE_KILLED {
			t.Fatalf("job ended %+v, want KILLED", info)
		}
		f.expectNoLeftovers(f.projects)
	})
	t.Run("daemon shutdown", func(t *testing.T) {
		f := newFixture(t)
		url := f.bareRepo("one")
		f.gate()
		job := f.toBackground(request, url)
		// Dispatcher shutdown stops the clone through the tool's Shutdown;
		// the job manager then reports it killed.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := f.d.Shutdown(ctx); err != nil {
			t.Fatal(err)
		}
		if info := f.jobEnded(job); info.State != circusv1.JobEvent_STATE_KILLED {
			t.Fatalf("job after shutdown = %+v", info)
		}
		if workspace.ActivePreparations(f.prepare) != 0 {
			t.Fatal("shutdown left a preparation holding its slot")
		}
	})
}

func TestConcurrencyLimit(t *testing.T) {
	f := newFixture(t)
	url := f.bareRepo("one")
	f.gate()
	before := f.clock.Waiters()
	first := f.submit(f.prepareCall(request, url, ""))
	second := f.submit(f.prepareCall(request+"-2", url, ""))
	f.armed(before, 2*foregroundTimers)
	f.started(2)
	expectFailed(t, f.prepareNow(request+"-3", url), busy)
	f.clock.Advance(2 * time.Minute)
	jobsRunning := []string{}
	for _, id := range []string{first, second} {
		r := f.wait(id).GetPrepareWorkspace()
		if r.Outcome != circusv1.PrepareWorkspaceResult_OUTCOME_IN_BACKGROUND {
			t.Fatalf("result = %v", r)
		}
		jobsRunning = append(jobsRunning, r.JobId)
	}
	// Background clones keep their slots.
	expectFailed(t, f.prepareNow(request+"-3", ""), busy)
	f.release()
	for _, job := range jobsRunning {
		f.jobEnded(job)
	}
	f.idle()
	expectReady(t, f.prepareNow(request+"-3", ""), created)
}

func TestCredentialsNeverLeave(t *testing.T) {
	const token = "s3cr3t-t0ken"
	url := "https://alice:" + token + "@127.0.0.1:9/o/r.git"
	check := func(t *testing.T, f *fixture, where, text string) {
		t.Helper()
		if strings.Contains(text, token) {
			t.Fatalf("%s contains the token: %q", where, text)
		}
	}
	t.Run("stderr detail and logs", func(t *testing.T) {
		f := newFixture(t)
		mustWrite(t, filepath.Join(f.ctl, "echo"), "")
		result := f.wait(f.submit(f.prepareCall(request, url, "")))
		r := result.GetPrepareWorkspace()
		expectFailed(t, r, cloneFailed)
		check(t, f, "detail", r.ErrorDetail)
		check(t, f, "output", text(result))
		check(t, f, "logs", f.logs.String())
		want := "fatal: unable to access 'https://127.0.0.1:9/o/r.git': The requested URL returned error: 403"
		if !strings.Contains(r.ErrorDetail, want) || !strings.Contains(r.ErrorDetail, "Receiving objects: 50%") || strings.Contains(r.ErrorDetail, "10%") {
			t.Fatalf("detail = %q", r.ErrorDetail)
		}
	})
	t.Run("job command and background outcome", func(t *testing.T) {
		f := newFixture(t)
		f.gate()
		job := f.toBackground(request, url)
		info, _ := f.jobs.Get(job)
		check(t, f, "job command", info.Command)
		if info.Command != "git clone https://127.0.0.1:9/o/r.git" {
			t.Fatalf("job command = %q", info.Command)
		}
		f.release()
		f.jobEnded(job)
		r := f.wait(f.submit(f.prepareCall(request, url, job))).GetPrepareWorkspace()
		expectFailed(t, r, cloneFailed)
		check(t, f, "detail", r.ErrorDetail)
		check(t, f, "logs", f.logs.String())
	})
	t.Run("query string tokens", func(t *testing.T) {
		f := newFixture(t)
		const query = "https://127.0.0.1:9/o/r.git?access_token=" + token
		mustWrite(t, filepath.Join(f.ctl, "echo"), "")
		r := f.prepareNow(request, query)
		expectFailed(t, r, cloneFailed)
		check(t, f, "detail", r.ErrorDetail)
		if !strings.Contains(r.ErrorDetail, "https://127.0.0.1:9/o/r.git?[redacted]") {
			t.Fatalf("detail = %q", r.ErrorDetail)
		}
		_ = os.Remove(filepath.Join(f.ctl, "echo"))
		f.gate()
		job := f.toBackground(request+"-2", query)
		info, _ := f.jobs.Get(job)
		if info.Command != "git clone https://127.0.0.1:9/o/r.git?[redacted]" {
			t.Fatalf("job command = %q", info.Command)
		}
		f.release()
		f.jobEnded(job)
		check(t, f, "logs", f.logs.String())
	})
	t.Run("existing origin", func(t *testing.T) {
		f := newFixture(t)
		mustWrite(t, filepath.Join(f.target(), "file"), "x")
		f.git(f.target(), "init", "-q")
		f.git(f.target(), "remote", "add", "origin", "https://bob:"+token+"@example.com/other.git")
		r := f.prepareNow(request, url)
		expectFailed(t, r, remoteMismatch)
		if r.ErrorDetail != "https://example.com/other.git" {
			t.Fatalf("detail = %q", r.ErrorDetail)
		}
		check(t, f, "logs", f.logs.String())
	})
}

// TestTargetFilledDuringClone: the clone is renamed onto the target only
// if the target is still absent or empty; the user's files win.
func TestTargetFilledDuringClone(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent", true: "empty directory"}[existing], func(t *testing.T) {
			f := newFixture(t)
			url := f.bareRepo("one")
			if existing {
				mustMkdir(t, f.target())
			}
			f.gate()
			id := f.submit(f.prepareCall(request, url, ""))
			f.started(1)
			mustWrite(t, filepath.Join(f.target(), "mine.txt"), "mine")
			f.release()
			expectFailed(t, f.wait(id).GetPrepareWorkspace(), notEmpty)
			if names := entries(t, f.target()); len(names) != 1 || names[0] != "mine.txt" {
				t.Fatalf("target now contains %v", names)
			}
			f.expectNoLeftovers(f.projects)
		})
	}
}
