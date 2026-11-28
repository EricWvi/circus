package sqlite

import (
	"context"

	"github.com/EricWvi/circus/internal/store"
)

// Machine updates.

const machineUpdateCols = `user_id, machine_id, update_id, target_version, state, failure, error, started_at, deadline_at`

func scanMachineUpdate(s scanner) (store.MachineUpdate, error) {
	var u store.MachineUpdate
	var state, failure string
	err := s.Scan(&u.UserID, &u.MachineID, &u.UpdateID, &u.TargetVersion, &state, &failure, &u.Error, &u.StartedAt, &u.DeadlineAt)
	u.State, u.Failure = store.MachineUpdateState(state), store.MachineUpdateFailure(failure)
	return u, mapErr(err)
}

func (o *ops) GetMachineUpdate(ctx context.Context, userID, machineID string) (store.MachineUpdate, error) {
	if err := store.CheckID("user id", userID); err != nil {
		return store.MachineUpdate{}, err
	}
	if err := store.CheckText("machine id", machineID, store.MaxMachineIDBytes, false); err != nil {
		return store.MachineUpdate{}, err
	}
	return scanMachineUpdate(o.q.QueryRow(ctx, `SELECT `+machineUpdateCols+` FROM machine_updates
		WHERE user_id = ? AND machine_id = ?`, userID, machineID))
}

func (o *ops) GetMachineUpdateByID(ctx context.Context, updateID string) (store.MachineUpdate, error) {
	if err := store.CheckID("update id", updateID); err != nil {
		return store.MachineUpdate{}, err
	}
	return scanMachineUpdate(o.q.QueryRow(ctx, `SELECT `+machineUpdateCols+` FROM machine_updates WHERE update_id = ?`, updateID))
}

func (o *ops) ListMachineUpdates(ctx context.Context, userID string) ([]store.MachineUpdate, error) {
	if err := store.CheckID("user id", userID); err != nil {
		return nil, err
	}
	rows, err := o.q.QueryContext(ctx, `SELECT `+machineUpdateCols+` FROM machine_updates
		WHERE user_id = ? ORDER BY machine_id`, userID)
	if err != nil {
		return nil, mapErr(err)
	}
	return collect(rows, scanMachineUpdate)
}

func (o *ops) PutMachineUpdate(ctx context.Context, u store.MachineUpdate) error {
	if err := store.CheckMachineUpdate(u); err != nil {
		return err
	}
	_, err := o.exec(ctx, `INSERT INTO machine_updates (`+machineUpdateCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (user_id, machine_id) DO UPDATE SET update_id = excluded.update_id,
			target_version = excluded.target_version, state = excluded.state, failure = excluded.failure, error = excluded.error,
			started_at = excluded.started_at, deadline_at = excluded.deadline_at`,
		u.UserID, u.MachineID, u.UpdateID, u.TargetVersion, string(u.State), string(u.Failure), u.Error, u.StartedAt, u.DeadlineAt)
	return err
}

func (o *ops) FailMachineUpdate(ctx context.Context, updateID string, failure store.MachineUpdateFailure, errText string) (store.MachineUpdate, bool, error) {
	if err := store.CheckID("update id", updateID); err != nil {
		return store.MachineUpdate{}, false, err
	}
	if !failure.Valid() {
		return store.MachineUpdate{}, false, invalid("unknown machine update failure %q", failure)
	}
	if err := store.CheckText("machine update error", errText, store.MaxMachineUpdateErrorBytes, true); err != nil {
		return store.MachineUpdate{}, false, err
	}
	u, err := scanMachineUpdate(o.q.QueryRow(ctx, `UPDATE machine_updates SET state = 'failed', failure = ?, error = ?
		WHERE update_id = ? AND state = 'running' RETURNING `+machineUpdateCols, string(failure), errText, updateID))
	if err == store.ErrNotFound {
		return store.MachineUpdate{}, false, nil
	}
	if err != nil {
		return store.MachineUpdate{}, false, err
	}
	return u, true, nil
}

func (o *ops) DeleteMachineUpdate(ctx context.Context, userID, machineID, updateID string) (bool, error) {
	if err := store.CheckID("user id", userID); err != nil {
		return false, err
	}
	if err := store.CheckText("machine id", machineID, store.MaxMachineIDBytes, false); err != nil {
		return false, err
	}
	if err := store.CheckOptionalID("update id", updateID); err != nil {
		return false, err
	}
	n, err := o.exec(ctx, `DELETE FROM machine_updates WHERE user_id = ? AND machine_id = ? AND (?3 = '' OR update_id = ?3)`,
		userID, machineID, updateID)
	return n > 0, err
}
