package storetest

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/EricWvi/circus/internal/store"
)

func machineUpdate(user, machine, id string) store.MachineUpdate {
	return store.MachineUpdate{UserID: user, MachineID: machine, UpdateID: id, TargetVersion: "v1.2.3",
		State: store.MachineUpdateRunning, StartedAt: t0, DeadlineAt: t0 + 300_000}
}

// testMachineUpdates: one update per machine, replaced by a new attempt;
// only the running attempt named fails; deletes match the attempt when
// one is named; reads are per user.
func testMachineUpdates(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	seed(t, s)
	must(t, s.CreateUser(ctx, store.User{ID: "u2", Name: "other", CreatedAt: t0}))

	mustIs(t, errOf(s.GetMachineUpdate(ctx, "u1", "m1")), store.ErrNotFound)
	list, err := s.ListMachineUpdates(ctx, "u1")
	must(t, err)
	mustEqual(t, "no updates", len(list), 0)

	first := machineUpdate("u1", "m1", "a1")
	must(t, s.PutMachineUpdate(ctx, first))
	got, err := s.GetMachineUpdate(ctx, "u1", "m1")
	must(t, err)
	mustEqual(t, "stored", got, first)
	got, err = s.GetMachineUpdateByID(ctx, "a1")
	must(t, err)
	mustEqual(t, "by attempt", got, first)
	mustIs(t, errOf(s.GetMachineUpdateByID(ctx, "nope")), store.ErrNotFound)
	// A machine id as long as enrollment allows.
	long := machineUpdate("u1", strings.Repeat("m", store.MaxMachineIDBytes), "long")
	must(t, s.PutMachineUpdate(ctx, long))
	deleted, err := s.DeleteMachineUpdate(ctx, "u1", long.MachineID, "")
	must(t, err)
	mustEqual(t, "long machine id deleted", deleted, true)
	// Another user's machine of the same id is separate.
	other := machineUpdate("u2", "m1", "b1")
	must(t, s.PutMachineUpdate(ctx, other))
	// An attempt id is unique across machines.
	mustIs(t, s.PutMachineUpdate(ctx, machineUpdate("u1", "m2", "a1")), store.ErrExists)
	// An unknown user is refused.
	mustIs(t, s.PutMachineUpdate(ctx, machineUpdate("nope", "m1", "c1")), store.ErrNotFound)

	// The executor's failure marks the running attempt failed, once.
	failed, ok, err := s.FailMachineUpdate(ctx, "a1", store.UpdateFailedCommand, "boom")
	must(t, err)
	mustEqual(t, "failed", ok, true)
	want := first
	want.State, want.Failure, want.Error = store.MachineUpdateFailed, store.UpdateFailedCommand, "boom"
	mustEqual(t, "failed update", failed, want)
	_, ok, err = s.FailMachineUpdate(ctx, "a1", store.UpdateTimedOut, "")
	must(t, err)
	mustEqual(t, "already failed", ok, false)
	got, err = s.GetMachineUpdate(ctx, "u1", "m1")
	must(t, err)
	mustEqual(t, "first failure kept", got.Error, "boom")

	// A retry replaces the record; the old attempt's outcome then matches nothing.
	retry := machineUpdate("u1", "m1", "a2")
	retry.StartedAt, retry.DeadlineAt = t0+1000, t0+301_000
	must(t, s.PutMachineUpdate(ctx, retry))
	_, ok, err = s.FailMachineUpdate(ctx, "a1", store.UpdateFailedCommand, "late")
	must(t, err)
	mustEqual(t, "stale attempt ignored", ok, false)
	got, err = s.GetMachineUpdate(ctx, "u1", "m1")
	must(t, err)
	mustEqual(t, "retry stored", got, retry)

	m2 := machineUpdate("u1", "m2", "a3")
	must(t, s.PutMachineUpdate(ctx, m2))
	list, err = s.ListMachineUpdates(ctx, "u1")
	must(t, err)
	mustEqual(t, "listed by machine", list, []store.MachineUpdate{retry, m2})

	// Deleting with an old attempt id changes nothing; with the current one, or none, it deletes.
	deleted, err = s.DeleteMachineUpdate(ctx, "u1", "m1", "a1")
	must(t, err)
	mustEqual(t, "stale delete", deleted, false)
	deleted, err = s.DeleteMachineUpdate(ctx, "u1", "m1", "a2")
	must(t, err)
	mustEqual(t, "delete", deleted, true)
	deleted, err = s.DeleteMachineUpdate(ctx, "u1", "m2", "")
	must(t, err)
	mustEqual(t, "delete any", deleted, true)
	deleted, err = s.DeleteMachineUpdate(ctx, "u1", "m2", "")
	must(t, err)
	mustEqual(t, "delete again", deleted, false)
	list, err = s.ListMachineUpdates(ctx, "u2")
	must(t, err)
	mustEqual(t, "other user untouched", list, []store.MachineUpdate{other})

	// Writes and the timer that guards them share a transaction.
	rollback := errors.New("roll back")
	mustIs(t, s.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		if err := tx.PutMachineUpdate(ctx, machineUpdate("u1", "m3", "a4")); err != nil {
			return err
		}
		if err := tx.SetTimer(ctx, "machine_update", "a4", t0+300_000); err != nil {
			return err
		}
		return rollback
	}), rollback)
	mustIs(t, errOf(s.GetMachineUpdate(ctx, "u1", "m3")), store.ErrNotFound)
	timers, err := s.ListTimers(ctx, store.TimerFilter{Kind: "machine_update", Limit: 10})
	must(t, err)
	mustEqual(t, "timer rolled back", len(timers), 0)
}

// testMachineUpdateValidation: malformed arguments are ErrInvalid.
func testMachineUpdateValidation(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	seed(t, s)
	for name, edit := range map[string]func(*store.MachineUpdate){
		"no user":            func(u *store.MachineUpdate) { u.UserID = "" },
		"no machine":         func(u *store.MachineUpdate) { u.MachineID = "" },
		"no id":              func(u *store.MachineUpdate) { u.UpdateID = "" },
		"no version":         func(u *store.MachineUpdate) { u.TargetVersion = "" },
		"long version":       func(u *store.MachineUpdate) { u.TargetVersion = strings.Repeat("v", 65) },
		"unknown state":      func(u *store.MachineUpdate) { u.State = "done" },
		"running w/ error":   func(u *store.MachineUpdate) { u.Error = "x" },
		"running w/ failure": func(u *store.MachineUpdate) { u.Failure = store.UpdateTimedOut },
		"failed w/o failure": func(u *store.MachineUpdate) { u.State = store.MachineUpdateFailed },
		"unknown failure":    func(u *store.MachineUpdate) { u.State, u.Failure = store.MachineUpdateFailed, "other" },
		"long error": func(u *store.MachineUpdate) {
			u.State, u.Failure, u.Error = store.MachineUpdateFailed, store.UpdateFailedCommand, strings.Repeat("e", 2049)
		},
		"long machine id":   func(u *store.MachineUpdate) { u.MachineID = strings.Repeat("m", 256) },
		"no start":          func(u *store.MachineUpdate) { u.StartedAt = 0 },
		"deadline too soon": func(u *store.MachineUpdate) { u.DeadlineAt = u.StartedAt - 1 },
	} {
		t.Run(name, func(t *testing.T) {
			u := machineUpdate("u1", "m1", "a1")
			edit(&u)
			mustIs(t, s.PutMachineUpdate(ctx, u), store.ErrInvalid)
		})
	}
	mustIs(t, errOf(s.GetMachineUpdate(ctx, "", "m1")), store.ErrInvalid)
	mustIs(t, errOf(s.ListMachineUpdates(ctx, "")), store.ErrInvalid)
	_, _, err := s.FailMachineUpdate(ctx, "a1", "", "x")
	mustIs(t, err, store.ErrInvalid)
	_, _, err = s.FailMachineUpdate(ctx, "", store.UpdateTimedOut, "")
	mustIs(t, err, store.ErrInvalid)
	_, _, err = s.FailMachineUpdate(ctx, "a1", store.UpdateFailedCommand, strings.Repeat("e", 2049))
	mustIs(t, err, store.ErrInvalid)
	mustIs(t, errOf(s.DeleteMachineUpdate(ctx, "u1", "", "")), store.ErrInvalid)
}
