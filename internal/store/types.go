package store

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Sentinel errors. Backends wrap them with context; match with errors.Is.
var (
	// ErrNotFound reports that the addressed row does not exist (or, for
	// identities looked up by name, has been deleted).
	ErrNotFound = errors.New("store: not found")
	// ErrExists reports a primary-key or unique-constraint conflict.
	ErrExists = errors.New("store: already exists")
	// ErrTokenUnusable reports an enroll token that is unknown, already used
	// or expired. The three cases are deliberately indistinguishable (F20).
	ErrTokenUnusable = errors.New("store: enroll token unusable")
	// ErrInvalidTransition reports a TransitionCall edge that the call state
	// machine does not have. It is a programming error, not a lost race: a
	// lost race returns false with a nil error.
	ErrInvalidTransition = errors.New("store: invalid call transition")
	// ErrInvalid reports an argument the store refuses before touching data,
	// such as an empty id or an unknown enum value.
	ErrInvalid = errors.New("store: invalid argument")
	// ErrBusy reports that the backend could not get the lock it needed in
	// time (SQLITE_BUSY, a Postgres lock or serialization timeout). Nothing
	// was written by the failed operation; the caller may retry.
	ErrBusy = errors.New("store: busy")
)

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// PrincipalKind names the role of an identity. Each kind has its own table
// with the same shape; credentials point at an identity by kind and id.
type PrincipalKind string

const (
	// KindDevice is a device daemon identity (the "daemon" role).
	KindDevice PrincipalKind = "device"
	// KindWorker is a Loop worker identity.
	KindWorker PrincipalKind = "worker"
)

// Valid reports whether k is a known kind.
func (k PrincipalKind) Valid() bool { return k == KindDevice || k == KindWorker }

// User owns every other resource (D14 item 3).
type User struct {
	ID        string
	Name      string // unique
	CreatedAt int64
}

// Identity is one machine × role: a device or a worker (D22, D43 item 8).
// The devices and workers tables have exactly these columns.
type Identity struct {
	Kind          PrincipalKind
	ID            string
	UserID        string
	Name          string // unique per user and kind among rows not deleted
	MachineID     string // links a machine's device and worker identities
	OS            string
	Arch          string
	Hostname      string
	ClientVersion string
	// CurrentBootID is the boot epoch of the identity's latest connection,
	// written by SetConnected. Empty until the first connection.
	CurrentBootID      string
	LastConnectedAt    int64
	LastDisconnectedAt int64
	CreatedAt          int64
	DeletedAt          int64
}

// Credential is a long-term token held by one identity. Only the token's
// SHA-256 is stored. Credentials are separate from identities so rotation
// and revocation (D14) replace this layer only.
type Credential struct {
	ID          string
	Kind        PrincipalKind
	PrincipalID string
	TokenHash   []byte // unique
	CreatedAt   int64
	LastUsedAt  int64
	RevokedAt   int64 // non-zero once revoked
}

// Revoked reports whether the credential has been revoked.
func (c Credential) Revoked() bool { return c.RevokedAt != 0 }

// EnrollToken is a one-time token an administrator hands to a machine. Kinds
// lists the identities one use of it may create: a single join can claim a
// device and a worker credential at once (D43 item 8).
type EnrollToken struct {
	ID        string
	UserID    string
	TokenHash []byte // unique
	Kinds     []PrincipalKind
	CreatedAt int64
	ExpiresAt int64
	UsedAt    int64
	// UsedBy is a free-form audit value recorded by UseEnrollToken, normally
	// the machine id of the enrolling machine.
	UsedBy string
}

// EncodeKinds returns the canonical text form of a kind set ("device",
// "worker" or "device,worker"). It rejects empty, duplicate and unknown kinds.
func EncodeKinds(kinds []PrincipalKind) (string, error) {
	if len(kinds) == 0 {
		return "", invalid("enroll token has no kinds")
	}
	s := make([]string, 0, len(kinds))
	for _, k := range kinds {
		if !k.Valid() {
			return "", invalid("unknown principal kind %q", k)
		}
		s = append(s, string(k))
	}
	slices.Sort(s)
	if len(slices.Compact(s)) != len(kinds) {
		return "", invalid("duplicate principal kind in %v", kinds)
	}
	return strings.Join(s, ","), nil
}

// DecodeKinds parses the text form written by EncodeKinds.
func DecodeKinds(s string) ([]PrincipalKind, error) {
	var kinds []PrincipalKind
	for part := range strings.SplitSeq(s, ",") {
		kinds = append(kinds, PrincipalKind(part))
	}
	if _, err := EncodeKinds(kinds); err != nil {
		return nil, err
	}
	return kinds, nil
}

// CallState is the control plane's view of one call (outline 4.2).
//
//	accepted ─▶ dispatched ─▶ running ─▶ completed
//	   │            │            │
//	   │            └─▶ unknown ◀┘   (grace period expired)
//	   │                  └─▶ running / completed / lost (after reconnect)
//	   ├─▶ offline   (grace period expired before dispatch)
//	   ├─▶ cancelled (cancel before dispatch)
//	   └─▶ lost      (control plane restarted before dispatch)
//	dispatched, running, unknown ─▶ lost  (boot changed, daemon reports lost)
//	accepted, dispatched ─▶ rejected      (validation or daemon rejection)
//	lost ─▶ accepted  (resubmission of a call that was never dispatched)
//
// A result may arrive before its ack (F22), so dispatched ─▶ completed is
// allowed. A cancel on a dispatched call only records CancelRequestedAt; the
// daemon's result decides the final state.
//
// The transitions into dispatched and into accepted apply only to a call
// that was never dispatched (DispatchedBootID unset), whatever the caller
// expects: a call sent to one boot is never sent again, so a lost call that
// had reached a device stays lost for good (outline 4.4 items 2 and 3, D43
// item 5). Backends enforce this in their storage layer too, not only in
// CheckTransition.
//
// lost ─▶ accepted starts a fresh attempt of a call the control plane lost
// before dispatching it (D43 item 5): it records the resubmission time as
// SubmittedAt and clears every per-attempt column, so nothing of the lost
// attempt survives; from accepted the call follows the normal edges (grace
// period, cancel, offline, dispatch).
type CallState string

const (
	CallAccepted   CallState = "accepted"
	CallDispatched CallState = "dispatched"
	CallRunning    CallState = "running"
	CallUnknown    CallState = "unknown"
	CallCompleted  CallState = "completed"
	CallOffline    CallState = "offline"
	CallCancelled  CallState = "cancelled"
	CallRejected   CallState = "rejected"
	CallLost       CallState = "lost"
)

var callEdges = map[CallState][]CallState{
	CallAccepted:   {CallDispatched, CallOffline, CallCancelled, CallRejected, CallLost},
	CallDispatched: {CallRunning, CallCompleted, CallUnknown, CallRejected, CallLost},
	CallRunning:    {CallCompleted, CallUnknown, CallLost},
	CallUnknown:    {CallRunning, CallCompleted, CallLost},
	CallLost:       {CallAccepted},
}

// Valid reports whether s is a known state.
func (s CallState) Valid() bool {
	switch s {
	case CallAccepted, CallDispatched, CallRunning, CallUnknown,
		CallCompleted, CallOffline, CallCancelled, CallRejected, CallLost:
		return true
	}
	return false
}

// Terminal reports whether s is an outcome reported to the submitter:
// completed, offline, cancelled, rejected or lost. Only lost has an edge
// out, back to accepted for a resubmitted call that never reached a device.
func (s CallState) Terminal() bool {
	switch s {
	case CallCompleted, CallOffline, CallCancelled, CallRejected, CallLost:
		return true
	}
	return false
}

// CanTransition reports whether the state machine has the edge from → to.
// Self-edges are not transitions; use PatchCall to update a call in place.
func CanTransition(from, to CallState) bool { return slices.Contains(callEdges[from], to) }

// Call is the metadata row of one device call. It never holds the request
// or the result.
type Call struct {
	CallID       string // primary key, chosen by the submitter
	UserID       string
	DeviceID     string
	SessionID    string
	AgentID      string
	Tool         string
	RequestHash  []byte // SHA-256 of the canonical request, for de-dup
	RequestBytes int64
	State        CallState
	// DispatchedBootID is the device boot the call was sent to. It is set in
	// the same write that moves the call to dispatched and never changes
	// (backends reject any change of a recorded boot).
	DispatchedBootID  string
	SubmittedAt       int64
	DispatchedAt      int64
	AckedAt           int64
	FinishedAt        int64
	CancelRequestedAt int64
	ExitCode          *int32
	IsError           bool
	FaultCode         string
	JobID             string
	OutcomeReason     string
}

// MaxCodeBytes bounds Call.OutcomeReason and Call.FaultCode, and the reason
// of LoseCalls. Both hold codes or fixed short phrases chosen by the control
// plane ("daemon restarted", "CALL_MISMATCH"), never tool output, daemon
// error text or anything derived from the request (D9 item 3). Longer values
// are refused with ErrInvalid.
const MaxCodeBytes = 256

// CallPatch lists the columns a transition or patch may fill in. A zero
// field leaves its column unchanged; a patch never clears a column (only
// the resubmission, lost → accepted, does). Within one attempt:
//
//   - Write-once: DispatchedBootID, DispatchedAt, AckedAt,
//     CancelRequestedAt, JobID. The first value written stays; later
//     patches of the column are ignored, so the audit record shows the
//     first ack and the first cancel request.
//   - Outcome: FinishedAt, ExitCode, FaultCode, OutcomeReason take the
//     latest value written; IsError, once true, stays true. Earlier values
//     stay when a later patch leaves the column zero, so a transition into a
//     terminal state writes every outcome column it means to report (for
//     example unknown → completed sets FinishedAt and OutcomeReason, or the
//     row keeps those of the grace-period expiry).
//
// The transition lost → accepted takes only SubmittedAt and clears every
// other column of the list for the new attempt (DispatchedBootID is already
// unset). DispatchedBootID is never cleared.
type CallPatch struct {
	// SubmittedAt is written by the transition lost → accepted, which
	// requires it and nothing else, and by nothing else.
	SubmittedAt int64
	// DispatchedBootID is written by the transition to dispatched, which
	// requires it, and by nothing else: any other transition, and PatchCall,
	// fail with ErrInvalid when it is set. To require a boot, use
	// CallExpect.BootID.
	DispatchedBootID  string
	DispatchedAt      int64
	AckedAt           int64
	FinishedAt        int64
	CancelRequestedAt int64
	ExitCode          *int32
	IsError           bool
	FaultCode         string // at most MaxCodeBytes
	JobID             string
	OutcomeReason     string // at most MaxCodeBytes
}

// CheckCodes rejects an OutcomeReason or FaultCode longer than MaxCodeBytes.
func CheckCodes(outcomeReason, faultCode string) error {
	if len(outcomeReason) > MaxCodeBytes || len(faultCode) > MaxCodeBytes {
		return invalid("outcome reason and fault code hold short codes, at most %d bytes", MaxCodeBytes)
	}
	return nil
}

// CheckNewCall validates a call before CreateCall inserts it. A new call
// starts accepted, or directly in one of the outcomes a submission can have
// without dispatch: rejected or offline.
func CheckNewCall(c Call) error {
	if c.CallID == "" || c.UserID == "" || c.DeviceID == "" || c.SessionID == "" || c.Tool == "" {
		return invalid("call needs call id, user id, device id, session id and tool")
	}
	if len(c.RequestHash) == 0 {
		return invalid("call %q has no request hash", c.CallID)
	}
	switch c.State {
	case CallAccepted, CallRejected, CallOffline:
	default:
		return invalid("call %q cannot start in state %q", c.CallID, c.State)
	}
	if c.DispatchedBootID != "" || c.DispatchedAt != 0 {
		return invalid("new call %q is already dispatched", c.CallID)
	}
	return CheckCodes(c.OutcomeReason, c.FaultCode)
}

// CallExpect is the precondition of TransitionCall and PatchCall: the write
// applies only to a call that matches every field set. It is never written.
type CallExpect struct {
	// States lists the states the call may be in. Required.
	States []CallState
	// BootID, when set, also requires the call's DispatchedBootID to equal
	// it, so an answer from one boot never settles a call sent to another.
	// A call that was never dispatched matches no BootID.
	BootID string
	// Undispatched, when set, also requires the call never to have been
	// dispatched (DispatchedBootID unset). The transitions into dispatched
	// and into accepted always require it, set or not. It excludes BootID.
	Undispatched bool
}

// CheckTransition validates TransitionCall arguments: expect.States must be
// non-empty and every state → to an edge of the state machine
// (ErrInvalidTransition otherwise). The transition to dispatched must write
// the boot in the same step (outline 3.3: record the dispatch before
// sending it) and cannot expect one, since it leaves accepted, where none is
// recorded; no other transition may write the boot.
func CheckTransition(expect CallExpect, to CallState, patch CallPatch) error {
	if len(expect.States) == 0 {
		return invalid("transition to %q has no expected states", to)
	}
	if err := checkExpect(expect); err != nil {
		return err
	}
	for _, f := range expect.States {
		if !CanTransition(f, to) {
			return fmt.Errorf("%w: %s → %s", ErrInvalidTransition, f, to)
		}
	}
	switch {
	case to == CallAccepted && (patch.SubmittedAt == 0 || patch != CallPatch{SubmittedAt: patch.SubmittedAt}):
		return invalid("transition to accepted (resubmission) takes the submission time and nothing else")
	case to == CallAccepted && expect.BootID != "":
		return invalid("transition to accepted cannot expect a boot: only an undispatched call may be resubmitted")
	case to != CallAccepted && patch.SubmittedAt != 0:
		return invalid("transition to %q writes a submission time; only the resubmission (to accepted) does", to)
	case to == CallDispatched && patch.DispatchedBootID == "":
		return invalid("transition to dispatched without a boot id to record")
	case to == CallDispatched && expect.BootID != "":
		return invalid("transition to dispatched cannot expect a boot: the call has none yet")
	case to != CallDispatched && patch.DispatchedBootID != "":
		return invalid("transition to %q writes a boot id; only the transition to dispatched does (use CallExpect.BootID to require one)", to)
	}
	return CheckCodes(patch.OutcomeReason, patch.FaultCode)
}

func checkExpect(expect CallExpect) error {
	if expect.Undispatched && expect.BootID != "" {
		return invalid("a call cannot be expected both undispatched and dispatched to boot %q", expect.BootID)
	}
	return nil
}

// CheckPatch validates PatchCall arguments: expect.States must be non-empty
// and valid, and patch must set at least one column other than
// DispatchedBootID, which PatchCall never writes.
func CheckPatch(expect CallExpect, patch CallPatch) error {
	if err := CheckStates(expect.States); err != nil {
		return err
	}
	if err := checkExpect(expect); err != nil {
		return err
	}
	if patch.DispatchedBootID != "" {
		return invalid("PatchCall cannot write a boot id; only the transition to dispatched does (use CallExpect.BootID to require one)")
	}
	if patch.SubmittedAt != 0 {
		return invalid("PatchCall cannot write a submission time; only the resubmission (lost → accepted) does")
	}
	if patch == (CallPatch{}) {
		return invalid("empty call patch")
	}
	return CheckCodes(patch.OutcomeReason, patch.FaultCode)
}

// CheckStates validates a non-empty set of known call states.
func CheckStates(states []CallState) error {
	if len(states) == 0 {
		return invalid("no call states given")
	}
	for _, s := range states {
		if !s.Valid() {
			return invalid("unknown call state %q", s)
		}
	}
	return nil
}

// LostCall is one call LoseCalls moved to lost, with the state it left.
type LostCall struct {
	CallID string
	From   CallState
}

// CallFilter selects calls for ListCalls. Empty fields do not filter.
type CallFilter struct {
	DeviceID         string
	DispatchedBootID string
	States           []CallState
}

// JobState mirrors the daemon's background job states.
type JobState string

const (
	JobRunning JobState = "running"
	JobExited  JobState = "exited"
	JobKilled  JobState = "killed"
	// JobLost is set by the control plane when the job's boot is gone.
	JobLost JobState = "lost"
)

// Valid reports whether s is a known state.
func (s JobState) Valid() bool {
	return s == JobRunning || s == JobExited || s == JobKilled || s == JobLost
}

// Terminal reports whether the job has ended. A terminal job row is final:
// PutJob never changes it again, so a late event of a dead boot cannot
// bring a lost job back to running.
func (s JobState) Terminal() bool { return s.Valid() && s != JobRunning }

// Job is one background job, keyed by (DeviceID, BootID, JobID): the daemon's
// short job id is unique only within one boot (D43 item 6).
type Job struct {
	DeviceID  string
	BootID    string
	JobID     string
	UserID    string
	SessionID string
	CallID    string
	State     JobState
	ExitCode  *int32
	// LastSeq is the daemon's sequence number of the event or snapshot this
	// row reflects. PutJob applies only strictly newer data.
	LastSeq   int64
	StartedAt int64
	EndedAt   int64
	UpdatedAt int64
}

// JobFilter selects jobs for ListJobs. Empty fields do not filter.
type JobFilter struct {
	DeviceID  string
	BootID    string
	SessionID string
	States    []JobState
}

// Timer is one durable timer (D11 item 4), unique by (Kind, SubjectID), for
// example ("device_grace", deviceID) or ("call_watchdog", callID).
type Timer struct {
	Kind      string
	SubjectID string
	DueAt     int64
	// The claim fields are set by ClaimDueTimers and cleared by SetTimer.
	ClaimedBy  string // instance id of the current claimant
	ClaimUntil int64  // lease end; the timer is claimable again from then on
	Attempts   int    // claims since the timer was last set
	// Token identifies one claim. CompleteTimer deletes the row only while
	// the token still matches, so a timer re-set or re-claimed after this
	// claim survives the stale handler's completion.
	Token string
}
