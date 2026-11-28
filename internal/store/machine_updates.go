package store

import "context"

// MachineUpdateState is the state of a machine's update.
type MachineUpdateState string

const (
	// MachineUpdateRunning: the update was sent to the machine and its
	// deadline has not passed.
	MachineUpdateRunning MachineUpdateState = "running"
	// MachineUpdateFailed: the executor reported an error or the deadline
	// passed; Error says which.
	MachineUpdateFailed MachineUpdateState = "failed"
)

// Valid reports whether s is a known state.
func (s MachineUpdateState) Valid() bool {
	return s == MachineUpdateRunning || s == MachineUpdateFailed
}

// MachineUpdateFailure is why a machine update failed.
type MachineUpdateFailure string

const (
	// UpdateFailedCommand: the role ran `circus update` and it failed;
	// Error holds its output's tail.
	UpdateFailedCommand MachineUpdateFailure = "command"
	// UpdateTimedOut: no role of the machine was at the target version
	// when the deadline passed.
	UpdateTimedOut MachineUpdateFailure = "timed_out"
	// UpdateUnreachable: the command could not be sent; Error says why.
	UpdateUnreachable MachineUpdateFailure = "unreachable"
)

// Valid reports whether f is a known failure.
func (f MachineUpdateFailure) Valid() bool {
	return f == UpdateFailedCommand || f == UpdateTimedOut || f == UpdateUnreachable
}

const (
	// MaxMachineUpdateVersionBytes bounds MachineUpdate.TargetVersion.
	MaxMachineUpdateVersionBytes = 64
	// MaxMachineUpdateErrorBytes bounds MachineUpdate.Error.
	MaxMachineUpdateErrorBytes = 2048
	// MaxMachineIDBytes bounds a machine id, as enrollment does.
	MaxMachineIDBytes = 255
)

// MachineUpdate is the update of one machine of a user (its device and
// worker identities share MachineID) to TargetVersion: at most one per
// machine. A completed update is deleted.
type MachineUpdate struct {
	UserID, MachineID string
	// UpdateID names this attempt; a retry replaces the record with a new
	// one, so a late outcome of an older attempt matches nothing.
	UpdateID      string
	TargetVersion string
	State         MachineUpdateState
	// Failure is set exactly when State is failed; Error may say more.
	Failure MachineUpdateFailure
	Error   string
	// StartedAt and DeadlineAt are Unix milliseconds.
	StartedAt, DeadlineAt int64
}

// MachineUpdateOps stores machine updates.
type MachineUpdateOps interface {
	// GetMachineUpdate returns the machine's update, or ErrNotFound.
	GetMachineUpdate(ctx context.Context, userID, machineID string) (MachineUpdate, error)
	// GetMachineUpdateByID returns the update whose attempt is updateID,
	// or ErrNotFound.
	GetMachineUpdateByID(ctx context.Context, updateID string) (MachineUpdate, error)
	// ListMachineUpdates returns the user's machine updates, by machine id.
	ListMachineUpdates(ctx context.Context, userID string) ([]MachineUpdate, error)
	// PutMachineUpdate records u as its machine's update, replacing the one
	// recorded before. ErrNotFound if the user does not exist; ErrExists
	// if another machine's update has the same UpdateID.
	PutMachineUpdate(ctx context.Context, u MachineUpdate) error
	// FailMachineUpdate marks the running update updateID failed because
	// of failure, with errText (optional), and returns it. It returns false
	// with no change when no running update has that id.
	FailMachineUpdate(ctx context.Context, updateID string, failure MachineUpdateFailure, errText string) (MachineUpdate, bool, error)
	// DeleteMachineUpdate deletes the machine's update, only when its id is
	// updateID unless updateID is empty, and reports whether it did.
	DeleteMachineUpdate(ctx context.Context, userID, machineID, updateID string) (bool, error)
}

// CheckMachineUpdate validates u for PutMachineUpdate.
func CheckMachineUpdate(u MachineUpdate) error {
	if err := CheckID("user id", u.UserID); err != nil {
		return err
	}
	if err := CheckText("machine id", u.MachineID, MaxMachineIDBytes, false); err != nil {
		return err
	}
	if err := CheckID("update id", u.UpdateID); err != nil {
		return err
	}
	if err := CheckText("target version", u.TargetVersion, MaxMachineUpdateVersionBytes, false); err != nil {
		return err
	}
	if !u.State.Valid() {
		return invalid("unknown machine update state %q", u.State)
	}
	if u.State == MachineUpdateFailed && !u.Failure.Valid() {
		return invalid("a failed machine update needs a known failure, not %q", u.Failure)
	}
	if u.State == MachineUpdateRunning && (u.Failure != "" || u.Error != "") {
		return invalid("a running machine update has no failure or error")
	}
	if err := CheckText("machine update error", u.Error, MaxMachineUpdateErrorBytes, true); err != nil {
		return err
	}
	if u.StartedAt <= 0 || u.DeadlineAt < u.StartedAt {
		return invalid("a machine update needs a start and a deadline not before it")
	}
	return nil
}
