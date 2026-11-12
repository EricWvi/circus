package storetest

import (
	"fmt"
	"testing"

	"github.com/EricWvi/circus/internal/store"
)

func testPages(t *testing.T, b Backend) {
	s, dir := open(t, b)
	other := reopen(t, b, dir)
	f := seed(t, s)
	ctx := t.Context()
	for _, id := range []string{"b", "d", "f"} {
		must(t, s.CreateCall(ctx, newCall(id, f)))
		_, err := s.PutJob(ctx, store.Job{DeviceID: "d1", BootID: "b1", JobID: id, UserID: "u1", State: store.JobRunning, LastSeq: 1})
		must(t, err)
		must(t, s.SetTimer(ctx, "k", id, t0))
	}
	cs, err := s.ListCalls(ctx, store.CallFilter{UserID: "u1", Limit: 1})
	must(t, err)
	mustEqual(t, "first call", cs[0].CallID, "b")
	js, err := s.ListJobs(ctx, store.JobFilter{Limit: 1})
	must(t, err)
	mustEqual(t, "first job", js[0].JobID, "b")
	ts, err := s.ListTimers(ctx, store.TimerFilter{Limit: 1})
	must(t, err)
	mustEqual(t, "first timer", ts[0].SubjectID, "b")
	// Inserts on another handle straddling the cursor cannot shift or repeat rows.
	for _, id := range []string{"a", "c"} {
		must(t, other.CreateCall(ctx, newCall(id, f)))
		_, err := other.PutJob(ctx, store.Job{DeviceID: "d1", BootID: "b1", JobID: id, UserID: "u1", State: store.JobRunning, LastSeq: 1})
		must(t, err)
		must(t, other.SetTimer(ctx, "k", id, t0))
	}
	cs, err = s.ListCalls(ctx, store.CallFilter{Limit: 2, After: &store.CallCursor{SubmittedAt: t0, CallID: "b"}})
	must(t, err)
	mustEqual(t, "calls after", []string{cs[0].CallID, cs[1].CallID}, []string{"c", "d"})
	js, err = s.ListJobs(ctx, store.JobFilter{Limit: 2, After: &store.JobCursor{DeviceID: "d1", BootID: "b1", JobID: "b"}})
	must(t, err)
	mustEqual(t, "jobs after", []string{js[0].JobID, js[1].JobID}, []string{"c", "d"})
	ts, err = s.ListTimers(ctx, store.TimerFilter{Limit: 2, After: &store.TimerCursor{DueAt: t0, Kind: "k", SubjectID: "b"}})
	must(t, err)
	mustEqual(t, "timers after", []string{ts[0].SubjectID, ts[1].SubjectID}, []string{"c", "d"})
	cs, err = s.ListCalls(ctx, store.CallFilter{UserID: "absent", Limit: 1})
	must(t, err)
	mustEqual(t, "owner filter", len(cs), 0)

	// A writer runs concurrently with three independent keyset walks. Every new
	// key is behind the saved cursor; none may shift or repeat the remaining rows.
	writes := make(chan error, 1)
	go func() {
		for i := range 16 {
			id := fmt.Sprintf("a%02d", i)
			if err := other.CreateCall(ctx, newCall(id, f)); err != nil {
				writes <- err
				return
			}
			if _, err := other.PutJob(ctx, store.Job{DeviceID: "d1", BootID: "b1", JobID: id, UserID: "u1", State: store.JobRunning, LastSeq: 1}); err != nil {
				writes <- err
				return
			}
			if err := other.SetTimer(ctx, "k", id, t0); err != nil {
				writes <- err
				return
			}
		}
		writes <- nil
	}()
	ca := &store.CallCursor{SubmittedAt: t0, CallID: "b"}
	ja := &store.JobCursor{DeviceID: "d1", BootID: "b1", JobID: "b"}
	ta := &store.TimerCursor{DueAt: t0, Kind: "k", SubjectID: "b"}
	for _, want := range []string{"c", "d", "f"} {
		cs, err := s.ListCalls(ctx, store.CallFilter{Limit: 1, After: ca})
		must(t, err)
		mustEqual(t, "concurrent call cursor", cs[0].CallID, want)
		ca = &store.CallCursor{SubmittedAt: cs[0].SubmittedAt, CallID: cs[0].CallID}
		js, err := s.ListJobs(ctx, store.JobFilter{Limit: 1, After: ja})
		must(t, err)
		mustEqual(t, "concurrent job cursor", js[0].JobID, want)
		ja = &store.JobCursor{DeviceID: js[0].DeviceID, BootID: js[0].BootID, JobID: js[0].JobID}
		ts, err := s.ListTimers(ctx, store.TimerFilter{Limit: 1, After: ta})
		must(t, err)
		mustEqual(t, "concurrent timer cursor", ts[0].SubjectID, want)
		ta = &store.TimerCursor{DueAt: ts[0].DueAt, Kind: ts[0].Kind, SubjectID: ts[0].SubjectID}
	}
	must(t, <-writes)
	for _, limit := range []int{0, -1, store.MaxPageSize + 1} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			_, err := s.ListCalls(ctx, store.CallFilter{Limit: limit})
			mustIs(t, err, store.ErrInvalid)
			_, err = s.ListJobs(ctx, store.JobFilter{Limit: limit})
			mustIs(t, err, store.ErrInvalid)
			_, err = s.ListTimers(ctx, store.TimerFilter{Limit: limit})
			mustIs(t, err, store.ErrInvalid)
			_, err = s.LoseCalls(ctx, "d1", "b2", t0, "restart", limit)
			mustIs(t, err, store.ErrInvalid)
		})
	}
	for _, id := range []string{"a", "b", "c", "d", "f"} {
		_, err := s.TransitionCall(ctx, id, store.CallExpect{States: []store.CallState{store.CallAccepted}}, store.CallDispatched, store.CallPatch{DispatchedBootID: "b1"})
		must(t, err)
	}
	for _, want := range []int{2, 2, 1, 0} {
		lost, err := s.LoseCalls(ctx, "d1", "b2", t0, "restart", 2)
		must(t, err)
		mustEqual(t, "bounded loss", len(lost), want)
	}
}
