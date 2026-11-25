package store

import (
	"bytes"
	"cmp"
	"encoding/json"
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
	// or expired. The three cases are deliberately indistinguishable.
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
	// ErrFenced reports a session write whose fence does not match the
	// session's lease (another epoch, holder or boot, or an expired lease).
	// Backends return it as *FencedError, which carries the current epoch;
	// match with errors.Is(err, ErrFenced) or errors.As.
	ErrFenced = errors.New("store: session lease fenced")
	// ErrLeaseHeld reports an AcquireLease refused because another holder's
	// lease has not expired, or because the session is pinned to another
	// worker.
	ErrLeaseHeld = errors.New("store: session lease held")
	// ErrTranscriptFull reports a transcript batch refused by a size bound:
	// an entry over the per-entry limit, or the session's or the user's
	// transcript total over its cap. Nothing of the batch was written.
	// Retrying the same batch fails the same way.
	ErrTranscriptFull = errors.New("store: transcript full")
	// ErrLimit reports a count bound reached (sessions per user, open
	// inputs, working directories, jobs, background tasks, subpaths, calls
	// owing a notice). Nothing was written.
	ErrLimit = errors.New("store: limit reached")
	// ErrStopped reports work a stopped session no longer accepts: new
	// input, a new lease, taking inputs, approvals, grants, device calls.
	ErrStopped = errors.New("store: session stopped")
	// ErrBroken reports work a broken session (its transcript misses a
	// batch) does not accept until the holder repairs it: a new lease,
	// approvals, grants, device calls.
	ErrBroken = errors.New("store: session broken")
	// ErrNotBroken reports a RecoverSession refused because the session is
	// not broken. Nothing was written.
	ErrNotBroken = errors.New("store: session not broken")
	// ErrSessionActive reports a DeleteSession refused because the session
	// is not stopped, a worker still holds its lease, or one of its calls
	// has not reached a terminal state. Nothing was deleted.
	ErrSessionActive = errors.New("store: session still active")
	// ErrTooManyCalls reports a device call refused by AdmitSessionCall
	// because the session already has its maximum of calls in a
	// non-terminal state. The call was not recorded, so it was never
	// executed; the worker may retry it under a new call id.
	ErrTooManyCalls = errors.New("store: too many active session calls")
)

// FencedError is the error of a fenced session write. Epoch is the
// session's current lease epoch, the only detail a fenced caller learns.
type FencedError struct{ Epoch int64 }

func (e *FencedError) Error() string {
	return fmt.Sprintf("store: session lease fenced (current epoch %d)", e.Epoch)
}

// Is makes errors.Is(err, ErrFenced) match a *FencedError.
func (e *FencedError) Is(target error) bool { return target == ErrFenced }

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

// User owns every other resource; a user's calls may name only that user's
// devices.
type User struct {
	Email        string
	Role         UserRole
	PasswordHash string
	ID           string
	Name         string // unique; defaults to ID when omitted
	CreatedAt    int64
	// TranscriptBytes is the sum of the transcript bytes of the user's
	// sessions, kept by AppendTranscript and DeleteSession for the per-user
	// cap. CreateUser ignores it.
	TranscriptBytes int64
	// DefaultProviderID is the user's default provider, set by
	// CreateProvider and UpdateProvider (Provider.IsDefault); it may name a
	// deleted provider. CreateUser ignores it.
	DefaultProviderID string
}

// Identity is one machine × role: a device or a worker.
// The devices and workers tables have exactly these columns, except
// Integrator, which only workers have.
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
	// Integrator is a worker's integrator grant (SetWorkerIntegrator);
	// always false for a device. CreateIdentity ignores it.
	Integrator bool
}

// HelloMetadata is the mutable identity metadata reported during link handshakes.
type HelloMetadata struct {
	ClientVersion string
	OS            string
	Arch          string
	Hostname      string
}

// IdentitySummary is a live device or worker with its owner's email for
// administrative listings.
type IdentitySummary struct {
	ID, Name, MachineID, OwnerEmail string
	CreatedAt, LastConnectedAt      int64
}

// Credential is a long-term token held by one identity. Only the token's
// SHA-256 is stored. Credentials are separate from identities so rotation,
// revocation and a later key-pair scheme replace this layer only.
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
// device and a worker credential at once.
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

// CallState is the control plane's view of one call.
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
// A result may arrive before its ack, so dispatched ─▶ completed is
// allowed. A cancel on a dispatched call only records CancelRequestedAt; the
// daemon's result decides the final state.
//
// The transitions into dispatched and into accepted apply only to a call
// that was never dispatched (DispatchedBootID unset), whatever the caller
// expects: a call sent to one boot is never sent again, so a lost call that
// had reached a device stays lost for good. Backends enforce this in their storage layer too, not only in
// CheckTransition.
//
// lost ─▶ accepted starts a fresh attempt of a call the control plane lost
// before dispatching it: it records the resubmission time as
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
	// ToolUseID is the model's tool_use id the call answers, recorded at
	// creation for session recovery. Empty for calls made outside a
	// session turn.
	ToolUseID string
	// ReturnedAt is when the worker's tool handler returned the call's
	// result to the CLI (PatchSessionRecords); zero until then.
	ReturnedAt int64
	// PersistedAt is when a persisted set proved the result is in the
	// transcript (ReportTurn, PatchSessionRecords); zero until then.
	PersistedAt int64
	// NoticeOwed marks a call returned to the model with a non-terminal
	// outcome whose completion notice is still owed. A session's calls
	// with PersistedAt zero or NoticeOwed set are its recovery candidates.
	NoticeOwed bool
}

// CallOutcome is what a worker can tell the model about a call, mapped
// from its state by CallState.Outcome.
type CallOutcome string

const (
	OutcomeAccepted   CallOutcome = "accepted"
	OutcomeDispatched CallOutcome = "dispatched" // dispatched or running
	OutcomeUnknown    CallOutcome = "unknown"
	OutcomeResult     CallOutcome = "result" // completed: the device has the result
	OutcomeOffline    CallOutcome = "offline"
	OutcomeLost       CallOutcome = "lost"
	OutcomeRejected   CallOutcome = "rejected" // Call.OutcomeReason holds the code
	OutcomeCancelled  CallOutcome = "cancelled"
)

// Outcome maps a call state to its outcome; "" for an unknown state.
func (s CallState) Outcome() CallOutcome {
	switch s {
	case CallAccepted:
		return OutcomeAccepted
	case CallDispatched, CallRunning:
		return OutcomeDispatched
	case CallUnknown:
		return OutcomeUnknown
	case CallCompleted:
		return OutcomeResult
	case CallOffline:
		return OutcomeOffline
	case CallLost:
		return OutcomeLost
	case CallRejected:
		return OutcomeRejected
	case CallCancelled:
		return OutcomeCancelled
	}
	return ""
}

// MaxCodeBytes bounds Call.OutcomeReason and Call.FaultCode, and the reason
// of LoseCalls. Both hold codes or fixed short phrases chosen by the control
// plane ("daemon restarted", "CALL_MISMATCH"), never tool output, daemon
// error text or anything derived from the request. Longer values
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
	if c.ReturnedAt != 0 || c.PersistedAt != 0 || c.NoticeOwed {
		return invalid("new call %q is already returned or persisted", c.CallID)
	}
	if len(c.ToolUseID) > MaxIDBytes {
		return invalid("tool use id longer than %d bytes", MaxIDBytes)
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
// the boot in the same step (the dispatch is recorded before it is sent)
// and cannot expect one, since it leaves accepted, where none is
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

// CallFilter selects calls for ListCalls. Limit is required (1..MaxPageSize).
// Other empty fields do not filter; After is an exclusive keyset cursor.
type CallFilter struct {
	UserID           string
	Limit            int
	After            *CallCursor
	DeviceID         string
	DispatchedBootID string
	States           []CallState
	// SessionID selects the calls of one session.
	SessionID string
	// NotPersisted selects calls whose PersistedAt is zero; use it with
	// SessionID.
	NotPersisted bool
	// Recovery selects a session's recovery candidates: PersistedAt zero or
	// NoticeOwed set; use it with SessionID.
	Recovery bool
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
// short job id is unique only within one boot.
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

// JobFilter selects jobs for ListJobs. Limit is required (1..MaxPageSize).
// Other empty fields do not filter; After is an exclusive keyset cursor.
type JobFilter struct {
	UserID    string
	Limit     int
	After     *JobCursor
	DeviceID  string
	BootID    string
	SessionID string
	// CallID selects the jobs a call started.
	CallID string
	States []JobState
}

// Timer is one durable timer, unique by (Kind, SubjectID), for
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

// MaxPageSize bounds every enumerated page and bulk loss batch.
const MaxPageSize = 256

// CallCursor is the exclusive (SubmittedAt, CallID) key of the last call read.
// Pages are live reads: new rows behind the cursor are not revisited. Changing
// SubmittedAt (resubmission) may move a row; use a Tx when a snapshot is needed.
type CallCursor struct {
	SubmittedAt int64
	CallID      string
}

// JobCursor is the exclusive immutable (DeviceID, BootID, JobID) ordering key.
type JobCursor struct{ DeviceID, BootID, JobID string }

// TimerCursor is the exclusive (DueAt, Kind, SubjectID) key. Resetting a timer
// may move it; use a Tx when a consistent snapshot is needed.
type TimerCursor struct {
	DueAt           int64
	Kind, SubjectID string
}

// TimerFilter bounds a live keyset page. Limit is required, 1..MaxPageSize.
// Kind and SubjectID optionally select a timer family or one subject.
type TimerFilter struct {
	Limit           int
	After           *TimerCursor
	Kind, SubjectID string
}

// CheckLimit rejects unbounded or excessively large result sets.
func CheckLimit(limit int) error {
	if limit < 1 || limit > MaxPageSize {
		return invalid("limit must be 1..%d", MaxPageSize)
	}
	return nil
}

// Session bounds. Default* values are the defaults of bounds the caller
// passes (configurable); the rest are fixed. See SessionOps for where each
// is enforced.
const (
	// DefaultMaxSessionsPerUser bounds a user's sessions, stopped ones
	// included until deleted (CreateSession's maxSessions).
	DefaultMaxSessionsPerUser = 1000
	// DefaultMaxTranscriptBytes bounds one session's transcript charge
	// (TranscriptQuota.MaxSessionBytes).
	DefaultMaxTranscriptBytes int64 = 256 << 20
	// DefaultMaxUserTranscriptBytes bounds the transcript charge of all of
	// one user's sessions (TranscriptQuota.MaxUserBytes).
	DefaultMaxUserTranscriptBytes int64 = 20 << 30
	// DefaultMaxViewBytes bounds one session's stored event payloads before
	// only boundary events are kept (AppendEvents' maxViewBytes).
	DefaultMaxViewBytes int64 = 64 << 20
	// DefaultMaxActiveSessionCalls bounds a session's calls not in a
	// terminal state (AdmitSessionCall's maxActive).
	DefaultMaxActiveSessionCalls = 16

	// TranscriptEntryOverhead is charged per transcript entry on top of its
	// JSON and uuid: the row's keys and index entries. It bounds a full
	// default session to 4 Mi entries.
	TranscriptEntryOverhead = 64

	// MaxIDBytes bounds every id: session, input, client message, client request, call,
	// tool use, approval, agent, device, worker, boot, connection, job,
	// task and transcript uuid, and short labels (model, permission mode,
	// event format, background task kind).
	MaxIDBytes = 128
	// MaxToolBytes bounds a tool name.
	MaxToolBytes = 256
	// MaxSubpathBytes bounds a transcript subpath.
	MaxSubpathBytes = 1024
	// MaxDescriptionBytes bounds a job or background task description and
	// an approval's display and reason.
	MaxDescriptionBytes = 1024
	// MaxPathBytes bounds a working directory, a file record path and a
	// job's output path.
	MaxPathBytes = 4096
	// MatchKeyHexDigits is the length of an approval's or grant's match
	// key: the SHA-256, in lowercase hexadecimal, of the bash command or
	// the write/edit path.
	MatchKeyHexDigits = 64

	// MaxOpenInputs bounds a session's inputs not yet persisted, deferred
	// notes apart.
	MaxOpenInputs = 32
	// MaxNoteOverflow is how many note inputs may exceed MaxOpenInputs:
	// the user's inputs filling the bound do not keep the control plane's
	// notes out.
	MaxNoteOverflow = 8
	// MaxDeferredNotes bounds a session's pending deferred notes, which
	// count towards neither bound above: at the bound, FoldDeferredNotes
	// replaces them with one.
	MaxDeferredNotes = 8
	// MaxInputTextBytes bounds one input's text.
	MaxInputTextBytes = 256 << 10
	// KeptInputs is how many persisted inputs a session keeps (metadata
	// only), so a late SubmitInput retry still finds its input.
	KeptInputs = 1000
	// KeptTurns is how many turns a session keeps.
	KeptTurns = 1000
	// MaxPersistedIDs bounds the ids in one TurnPersisted.
	MaxPersistedIDs = 1024
	// MaxOwedCalls bounds a session's calls owing a completion notice, and
	// the AwaitingCallIDs and LateCallIDs of one call.
	MaxOwedCalls = 16
	// MaxEventPayloadBytes bounds one stored event payload; a larger one is
	// stored without its bytes.
	MaxEventPayloadBytes = 256 << 10
	// MaxSessionEvents bounds a session's stored events by count; the
	// oldest are evicted past it.
	MaxSessionEvents = 10000
	// MaxSessionCwds bounds a session's working-directory rows (agent ×
	// device).
	MaxSessionCwds = 256
	// MaxSessionFiles bounds a session's file records; the least recently
	// touched are evicted past it, and an evicted path counts as never read.
	MaxSessionFiles = 4096
	// MaxSessionJobs bounds a session's registered device jobs.
	MaxSessionJobs = 1024
	// MaxSessionBgTasks bounds a session's CLI background tasks.
	MaxSessionBgTasks = 64
	// MaxPatchItems bounds the items of one RecordsPatch.
	MaxPatchItems = 1024
	// MaxPendingApprovals bounds a session's pending approvals.
	MaxPendingApprovals = 32
	// KeptResolvedApprovals is how many approved, denied or expired
	// approvals a session keeps.
	KeptResolvedApprovals = 64
	// MaxSessionGrants bounds a session's unconsumed grants.
	MaxSessionGrants = 32
	// MaxTranscriptSubpaths bounds a session's transcript subpaths.
	MaxTranscriptSubpaths = 1024
	// MaxTranscriptPageBytes bounds every byte budget of a page (transcript,
	// events, records, approvals).
	MaxTranscriptPageBytes = 8 << 20
	// MaxTranscriptBatchBytes bounds the entries (JSON and uuid) of one
	// AppendTranscript batch, unless the batch is exactly one entry.
	MaxTranscriptBatchBytes = 8 << 20
	// MaxTranscriptEntryBytes bounds TranscriptQuota.MaxEntryBytes: the
	// link's message limit M, which one entry may fill.
	MaxTranscriptEntryBytes = 64 << 20
	// MaxUnpersistedSessionCalls bounds a session's calls not yet
	// persisted (AdmitSessionCall, ErrTooManyCalls).
	MaxUnpersistedSessionCalls = 1024
	// MaxRecoveryCalls bounds Recovery.Calls: every unpersisted call plus
	// the owed notices.
	MaxRecoveryCalls = MaxUnpersistedSessionCalls + MaxOwedCalls
	// MaxEventBatch bounds the events of one AppendEvents call.
	MaxEventBatch = 128
	// MaxTranscriptPageEntries bounds the entries of one LoadTranscript page.
	MaxTranscriptPageEntries = 4096
	// MaxResumeLeases bounds the keys of one ResumeLeases.
	MaxResumeLeases = 64
	// DeleteChunkRows bounds the transcript entries or calls rows one
	// DeleteSession transaction deletes.
	DeleteChunkRows = 5000
	// rowOverhead is added to the strings of a record or approval for its
	// page size.
	rowOverhead = 64
)

// checkLen returns ErrInvalid when s is empty (unless optional) or longer
// than max bytes.
func checkLen(what, s string, max int, optional bool) error {
	if s == "" && !optional {
		return invalid("%s is required", what)
	}
	if len(s) > max {
		return invalid("%s longer than %d bytes", what, max)
	}
	return nil
}

// CheckID validates a required id of at most MaxIDBytes.
func CheckID(what, s string) error { return checkLen(what, s, MaxIDBytes, false) }

// CheckOptionalID validates an optional id of at most MaxIDBytes.
func CheckOptionalID(what, s string) error { return checkLen(what, s, MaxIDBytes, true) }

// CheckText validates a string of at most max bytes, required unless
// optional.
func CheckText(what, s string, max int, optional bool) error {
	return checkLen(what, s, max, optional)
}

// CheckIDs validates a list of required ids.
func CheckIDs(what string, ids []string) error {
	for _, id := range ids {
		if err := CheckID(what, id); err != nil {
			return err
		}
	}
	return nil
}

// SessionState is the session's place in the state machine (see
// SessionOps).
type SessionState string

const (
	// SessionIdle: no turn running. A worker may hold the lease and wait.
	SessionIdle SessionState = "idle"
	// SessionQueued: pending work and no live lease; waits for a worker.
	SessionQueued SessionState = "queued"
	// SessionRunning: the CLI is running a turn, of any origin.
	SessionRunning SessionState = "running"
	// SessionStopped: stopped by the user. Final.
	SessionStopped SessionState = "stopped"
	// SessionBroken: the transcript misses a batch; only the holder can
	// repair it and return the session to idle.
	SessionBroken SessionState = "broken"
)

// Valid reports whether s is a known state.
func (s SessionState) Valid() bool {
	switch s {
	case SessionIdle, SessionQueued, SessionRunning, SessionStopped, SessionBroken:
		return true
	}
	return false
}

// Refusal returns the error a session in state s gives work it does not
// accept (a new lease, approvals, grants, device calls): ErrStopped,
// ErrBroken, or nil.
func (s SessionState) Refusal() error {
	switch s {
	case SessionStopped:
		return ErrStopped
	case SessionBroken:
		return ErrBroken
	}
	return nil
}

// TurnOutcome is how a turn ended.
type TurnOutcome string

const (
	TurnCompleted   TurnOutcome = "completed"
	TurnInterrupted TurnOutcome = "interrupted"
	TurnError       TurnOutcome = "error"
	// TurnStopped is a confirmed user abort accepted from the fenced holder.
	TurnStopped TurnOutcome = "stopped"
)

// Valid reports whether o is a known outcome.
func (o TurnOutcome) Valid() bool {
	return o == TurnCompleted || o == TurnInterrupted || o == TurnError || o == TurnStopped
}

// TurnOrigin is what started a turn: a user input the worker pushed, a
// notification-only message the worker pushed, or the CLI on its own (an
// asynchronous subagent finishing).
type TurnOrigin string

const (
	TurnFromInput        TurnOrigin = "input"
	TurnFromNotification TurnOrigin = "notification"
	TurnFromCLI          TurnOrigin = "cli"
)

// Valid reports whether o is a known origin.
func (o TurnOrigin) Valid() bool {
	return o == TurnFromInput || o == TurnFromNotification || o == TurnFromCLI
}

// TurnPhase is the phase a ReportTurn records.
type TurnPhase string

const (
	TurnStarted TurnPhase = "started"
	TurnEnded   TurnPhase = "ended"
)

// Session is one remote Claude Code session.
type Session struct {
	Workspace        bool   // the session uses a project default directory
	WorkspaceBlocked bool   // computed from the workspace and ledger snapshot rows on reads
	ID               string // UUIDv7, chosen by the control plane; never reused
	// UserID owns the project, session, and the devices and workers that serve it.
	UserID          string
	ProjectID       string
	ClientRequestID string // unique within the project, empty on legacy sessions
	LocationIndex   uint32 // resolved project location at creation
	Title           string
	// LastActivityAt is the maximum of creation, stored durable event times
	// (excluding workspace and ledger events), and accepted user input times. It never decreases.
	LastActivityAt int64
	// PendingApprovalCount excludes approvals expired at the read's supplied now.
	PendingApprovalCount uint32
	// ClaudeSessionID is the CLI's session id, generated at creation.
	// Unique.
	ClaudeSessionID string
	State           SessionState
	DefaultDeviceID string // a device of UserID
	Model           string // at most MaxIDBytes
	PermissionMode  string // at most MaxIDBytes
	// PinnedWorkerID, when set, is the only worker that may hold the lease.
	PinnedWorkerID string
	// PinDerived: the control plane chose PinnedWorkerID (a local session
	// whose client asked for no worker), so the client's request named
	// none. It requires Local.
	PinDerived bool
	// Local: the session runs in local mode on its pinned worker, which
	// shares the default device's machine. It requires PinnedWorkerID.
	// Local, PinnedWorkerID and PinDerived are set at creation and never
	// change.
	Local bool
	// ProviderID is the provider the session's CLI starts with; empty means
	// local login. It may name a deleted provider.
	ProviderID      string
	TurnCount       int64
	LastTurnOutcome TurnOutcome // empty before the first turn ends
	// InterruptTurnNo is the highest turn number the user asked to
	// interrupt (InterruptSession); 0 when never asked.
	InterruptTurnNo int64
	// PreviousTurnInterrupted: the most recent turn was closed without its
	// ENDED report (lease loss) and no turn has started since.
	PreviousTurnInterrupted bool
	// TranscriptGapTurnNo marks a transcript gap the user accepted and the
	// model has not been told of: RecoverSession sets it to the number of
	// the turn that broke the session; only a persisted set acknowledging
	// this exact value (GapNoticeTurnNo) clears it; 0 when none. An
	// unaccepted break is the broken state, not this mark, and
	// TranscriptRepaired never touches it.
	TranscriptGapTurnNo int64
	// Usage is the sum over every ended turn, kept when old turns are
	// deleted.
	Usage           TurnUsage
	TranscriptBytes int64 // the transcript's charge (TranscriptCharge)
	ViewBytes       int64 // stored event payload bytes
	ViewTruncated   bool  // events past the view cap were dropped
	LastEventSeq    int64
	// DroppedEventBatches counts worker event batches refused for an
	// invalid event (AppendEvents with an EventPublish).
	DroppedEventBatches int64
	// Reason is the short code recorded by StopSession, at most
	// MaxCodeBytes.
	Reason    string
	CreatedAt int64
	UpdatedAt int64
	StoppedAt int64
}

// RequestedWorkerID is the worker the creating request pinned the session
// to: PinnedWorkerID unless the control plane derived it (PinDerived).
func (s Session) RequestedWorkerID() string {
	if s.PinDerived {
		return ""
	}
	return s.PinnedWorkerID
}

// MatchesCreate reports whether the creation settings match. A nil index
// accepts the location resolved for the original request, and a nil
// providerID the provider resolved for it. pinnedWorkerID is the worker the
// request named (RequestedWorkerID), not a derived pin. An empty permission
// mode selects auto.
func (s Session) MatchesCreate(index *uint32, pinnedWorkerID, model, permissionMode string, providerID *string) bool {
	return (index == nil || *index == s.LocationIndex) && pinnedWorkerID == s.RequestedWorkerID() &&
		model == s.Model && cmp.Or(permissionMode, "auto") == cmp.Or(s.PermissionMode, "auto") &&
		(providerID == nil || *providerID == s.ProviderID)
}

// SessionCreated is the current session and whether the call created it.
type SessionCreated struct {
	Session Session
	Created bool
}

// SessionCursor is the exclusive (LastActivityAt, ID) key of the last session
// read.
type SessionCursor struct {
	LastActivityAt int64
	ID             string
}

// SessionFilter selects sessions for ListSessions. Limit is required
// (1..MaxPageSize); other empty fields do not filter.
type SessionFilter struct {
	Runnable  bool // skip sessions blocked on workspace or ledger snapshot preparation
	UserID    string
	ProjectID string
	// WebUserID selects the sessions of the projects this Web user owns
	// (projects.user_id).
	WebUserID string
	// ProviderID selects the sessions naming this provider.
	ProviderID string
	// Now is the read time in Unix milliseconds, required for approval expiry.
	Now    int64
	States []SessionState
	Limit  int
	After  *SessionCursor
}

// SessionProviderSet is SetSessionProvider's result. Loss is the lease the
// change released, if any.
type SessionProviderSet struct {
	Session Session
	Changed bool
	Loss    *LeaseLoss
}

// SessionSettled is the result of SettleSession. Loss is set when the
// lease had expired and was released.
type SessionSettled struct {
	Session Session
	Lease   Lease
	Loss    *LeaseLoss
}

// Lease is a session's lease row. WorkerID, BootID, ConnID, AcquiredAt and
// ExpiresAt are empty when nobody holds it; Epoch stays.
type Lease struct {
	SessionID  string
	Epoch      int64
	WorkerID   string
	BootID     string
	ConnID     string // the connection the lease is bound to
	AcquiredAt int64
	ExpiresAt  int64
	// ProviderID is the provider the lease was acquired under, empty for
	// local login or when the lease has no holder.
	ProviderID string
}

// IdleLease is a lease ListIdleLeases found idle, with its session's
// LastActivityAt for the next page's cursor.
type IdleLease struct {
	Lease
	LastActivityAt int64
}

// Live reports whether the lease has a holder and has not expired at now.
func (l Lease) Live(now int64) bool { return l.WorkerID != "" && l.ExpiresAt > now }

// Fence is what every worker operation carries: the epoch the worker
// believes it holds, the worker, boot and connection it arrives on, and
// the time of the check.
type Fence struct {
	SessionID string
	Epoch     int64
	WorkerID  string
	BootID    string
	ConnID    string
	Now       int64
}

// RestartCursor is where the next page of ExtendLeasesAfterRestart
// resumes. Until Providers is set, the unexpired leases and the expired
// ones without a provider resume after SessionID; then the expired leases
// under providers resume in provider order: within ProviderID after
// (AcquiredAt, SessionID) when Within, else with the next provider after
// ProviderID.
type RestartCursor struct {
	Providers  bool
	SessionID  string
	ProviderID string
	Within     bool
	AcquiredAt int64
}

// LeaseRequest asks AcquireLease for a session's lease.
type LeaseRequest struct {
	SessionID string
	WorkerID  string
	BootID    string
	ConnID    string
	Now       int64
	ExpiresAt int64 // > Now
	// Fresh: the worker holds no local state for the session, so even its
	// own lease is taken anew with the next epoch.
	Fresh bool
}

// LeaseAcquired is AcquireLease's result. Reattached reports a lease this
// worker and boot already held (same epoch). Loss is the expired lease
// taken over, if any.
type LeaseAcquired struct {
	Lease      Lease
	Session    Session
	Reattached bool
	Loss       *LeaseLoss
	// Released reports a refused fresh acquisition by the holder: its own
	// lease was released (Loss) because the session takes no new holder,
	// and Refusal (ErrStopped or ErrBroken) is the answer to give. It
	// commits; the method's error is nil.
	Released bool
	Refusal  error
}

// LeaseKey names one held lease for ExtendLeases.
type LeaseKey struct {
	SessionID string
	Epoch     int64
	WorkerID  string
	BootID    string
	ConnID    string
}

// SessionEpoch is one (session, epoch) a reconnecting boot claims.
type SessionEpoch struct {
	SessionID string
	Epoch     int64
}

// LeaseResumed is ResumeLeases' result for one key. Err is nil (Lease is
// the rebound, extended lease; Stopped reports a stopped session, which
// the holder should release), a *FencedError, or ErrNotFound.
type LeaseResumed struct {
	SessionID string
	Lease     Lease
	Stopped   bool
	Err       error
}

// LeaseLoss reports one lease released by ReleaseLease,
// ReleaseWorkerLeases, ExpireLeases, SettleSession or a takeover in
// AcquireLease.
type LeaseLoss struct {
	// Released is the lease as it was: the epoch and holder that lost it.
	Released Lease
	// Session is the session after the lease-loss rule.
	Session Session
	// Interrupted reports that a running turn ended as interrupted.
	Interrupted bool
	// Repeated reports a resent ReleaseLease of a lease already gone: nothing
	// was applied.
	Repeated bool
}

// SessionRecovered is RecoverSession's result. Loss is the held lease it
// released, if any; EventSeq is the seq of the gap event.
type SessionRecovered struct {
	Session  Session
	Loss     *LeaseLoss
	EventSeq int64
}

// Recovery is everything the holder of a session must recover, whichever
// epoch produced it.
type Recovery struct {
	// LastTurnInterrupted reports that the most recent turn was closed
	// without its ENDED report (lease loss) and no turn has started since
	// (Session.PreviousTurnInterrupted). A turn the worker reported ENDED
	// with outcome interrupted does not count.
	LastTurnInterrupted bool
	// Inputs taken and not yet persisted (at most MaxOpenInputs).
	Inputs []Input
	// Jobs whose notification was handed and not persisted.
	Jobs []SessionJob
	// LostBgTasks are background tasks started under an older epoch than
	// the fence's: their CLI is dead.
	LostBgTasks []BgTask
	// Calls not persisted or owing a notice, oldest first, at most
	// MaxRecoveryCalls; CallsTruncated when more exist (only calls created
	// without admission can exceed it).
	Calls          []Call
	CallsTruncated bool
}

// TranscriptQuota is the caller-configured size bounds of AppendTranscript.
// All three are required and positive; MaxEntryBytes ≤
// MaxTranscriptPageBytes, so one entry always fits in a page.
type TranscriptQuota struct {
	MaxEntryBytes   int64
	MaxSessionBytes int64 // DefaultMaxTranscriptBytes
	MaxUserBytes    int64 // DefaultMaxUserTranscriptBytes
}

// TranscriptEntry is one stored transcript entry. JSON is the entry as the
// SDK wrote it; the store never parses it.
type TranscriptEntry struct {
	Subpath string // "" for the main thread
	Seq     int64  // 1, 2, … per (session, subpath)
	UUID    string // optional, at most MaxIDBytes; unique per (session, subpath)
	JSON    []byte
	Epoch   int64 // the epoch that appended it
}

// QueuedCommandSource returns the input id an entry stands for when it
// is the record Claude Code writes of a message it absorbed into a
// running turn: an attachment of type queued_command, whose source_uuid
// is the pushed message's uuid (the input's id). Otherwise, or when the
// id is not a valid one, it returns "".
func QueuedCommandSource(entry []byte) string {
	if !bytes.Contains(entry, []byte(`"queued_command"`)) {
		return ""
	}
	var e struct {
		Type       string `json:"type"`
		Attachment struct {
			Type       string `json:"type"`
			SourceUUID string `json:"source_uuid"`
		} `json:"attachment"`
	}
	if json.Unmarshal(entry, &e) != nil || e.Type != "attachment" || e.Attachment.Type != "queued_command" ||
		e.Attachment.SourceUUID == "" || CheckID("source uuid", e.Attachment.SourceUUID) != nil {
		return ""
	}
	return e.Attachment.SourceUUID
}

// TranscriptCharge is what an entry counts against the transcript caps:
// its JSON, its uuid and TranscriptEntryOverhead.
func TranscriptCharge(e TranscriptEntry) int64 {
	return int64(len(e.JSON) + len(e.UUID) + TranscriptEntryOverhead)
}

// TranscriptBatch is one AppendTranscript call. BatchSeq starts at 1 per
// (epoch, subpath) and grows by exactly 1 per new batch. In Entries only
// UUID and JSON are read.
type TranscriptBatch struct {
	Subpath  string
	BatchSeq int64
	Entries  []TranscriptEntry
}

// TranscriptAppended is AppendTranscript's result.
type TranscriptAppended struct {
	// Deduplicated reports a batch already applied: nothing was written.
	Deduplicated bool
	Inserted     int
	Skipped      int   // entries whose uuid was already present
	LastSeq      int64 // the subpath's highest seq after the call
	SessionBytes int64 // the session's transcript charge after the call
	// Full reports a batch refused by a charge cap: nothing of it was
	// written and the session was stopped in the same transaction (the
	// caller then runs the usual stop follow-ups: the stop event, Nudge
	// STOP, stopping the session's jobs). FullReason (matching
	// ErrTranscriptFull) says which cap. It commits; the method's error is
	// nil.
	Full       bool
	FullReason error
	// Stopped reports that this call stopped the session (it was not
	// stopped before), so the caller records the stop once.
	Stopped bool
}

// TranscriptPageRequest selects one LoadTranscript page of the fenced
// session.
type TranscriptPageRequest struct {
	Subpath  string
	AfterSeq int64
	Limit    int   // 1..MaxTranscriptPageEntries
	MaxBytes int64 // 1..MaxTranscriptPageBytes
}

// TranscriptPage is one LoadTranscript page.
type TranscriptPage struct {
	Entries []TranscriptEntry
	Done    bool
}

// TranscriptSubpath summarizes one subpath of a session's transcript.
type TranscriptSubpath struct {
	Subpath string
	LastSeq int64
	Bytes   int64 // the subpath's charge
}

// InputKind is what produced an input.
type InputKind string

const (
	InputUser     InputKind = "user"
	InputApproval InputKind = "approval" // the "approved, retry" message
	// InputNote is a note the control plane writes for the model, such as
	// the problems of a ledger collection; Input.Note names its category.
	InputNote InputKind = "note"
)

// InputPriority is how the worker pushes an input to the CLI.
type InputPriority string

const (
	// InputNormal merges into a running turn, as native input does.
	InputNormal InputPriority = "normal"
	// InputNow interrupts the running turn first.
	InputNow InputPriority = "now"
)

// InputState tracks an input's delivery.
type InputState string

const (
	InputPending   InputState = "pending"
	InputTaken     InputState = "taken"
	InputPersisted InputState = "persisted"
)

// Input is one message for the session's CLI.
type Input struct {
	// InputID (UUID, chosen by the control plane) is also the uuid of the
	// user message the worker pushes, so the transcript proves delivery.
	InputID     string
	SessionID   string
	ClientMsgID string // unique per session; the idempotency key
	Kind        InputKind
	Text        string // at most MaxInputTextBytes; empty once persisted
	Priority    InputPriority
	State       InputState
	TakenEpoch  int64 // set while taken, kept once persisted
	CreatedAt   int64
	TakenAt     int64
	PersistedAt int64
	// EventSeq is the seq of the view event the control plane recorded for
	// the input (SetInputEventSeq); 0 when none was recorded or stored.
	EventSeq int64
	// Note is the category of an InputNote (at most MaxInputNoteBytes of
	// [a-z0-9_]); empty for other kinds.
	Note string
	// Deferred (InputNote only) is a note that starts no turn: it stays
	// pending, does not queue the session and is not work for a worker,
	// until an input that is not deferred is pending too; TakeInputs then
	// takes it with that input, so it reaches the model in the same turn,
	// before it. Deferred notes do not count towards MaxOpenInputs, so
	// they never keep the user's input out; MaxDeferredNotes bounds them.
	Deferred bool
}

// InputSubmitted is SubmitInput's result: the stored input, whether this
// call created it, and the session and lease after the call.
type InputSubmitted struct {
	Input   Input
	Created bool
	Session Session
	Lease   Lease
}

// TurnUsage is a turn's usage as the SDK's result reports it. CostMicroUSD
// is the cost in millionths of a dollar.
type TurnUsage struct {
	InputTokens              int64
	OutputTokens             int64
	CacheReadInputTokens     int64
	CacheCreationInputTokens int64
	CostMicroUSD             int64
	DurationMs               int64
}

// TurnPersisted lists what the SDK's result of a turn proved to be in the
// transcript. Applying it, for the fenced session only (other sessions'
// ids are ignored):
//   - InputIDs: inputs taken by the fence's epoch become persisted;
//   - CallIDs: calls get PersistedAt (and ReturnedAt if unset);
//   - JobCallIDs: job notifications (by the id of the call that started
//     the job) become persisted;
//   - BgTaskIDs: background tasks whose loss notification was delivered
//     are deleted;
//   - LateCallIDs (at most MaxOwedCalls): calls whose owed completion
//     notice was delivered lose NoticeOwed;
//   - GapNoticeTurnNo: the Session.TranscriptGapTurnNo whose missing-history
//     reminder was delivered; the mark is cleared only if it still equals
//     this value (0 acknowledges nothing).
//
// At most MaxPersistedIDs ids in all.
type TurnPersisted struct {
	InputIDs    []string
	CallIDs     []string
	JobCallIDs  []string
	BgTaskIDs   []string
	LateCallIDs []string
	// GapNoticeTurnNo acknowledges the transcript gap reminder; 0 for none.
	GapNoticeTurnNo int64
}

func (p TurnPersisted) count() int {
	return len(p.InputIDs) + len(p.CallIDs) + len(p.JobCallIDs) + len(p.BgTaskIDs) + len(p.LateCallIDs)
}

// Check validates the persisted set's ids and counts.
func (p TurnPersisted) Check() error {
	if n := p.count(); n > MaxPersistedIDs {
		return invalid("%d persisted ids, at most %d", n, MaxPersistedIDs)
	}
	if p.GapNoticeTurnNo < 0 {
		return invalid("negative gap notice turn number")
	}
	if len(p.LateCallIDs) > MaxOwedCalls {
		return invalid("%d late call ids, at most %d", len(p.LateCallIDs), MaxOwedCalls)
	}
	for _, ids := range [][]string{p.InputIDs, p.CallIDs, p.JobCallIDs, p.BgTaskIDs, p.LateCallIDs} {
		if err := CheckIDs("persisted id", ids); err != nil {
			return err
		}
	}
	return nil
}

// Empty reports whether the set lists nothing.
func (p TurnPersisted) Empty() bool { return p.count() == 0 && p.GapNoticeTurnNo == 0 }

// CountIDs is the number of ids the set lists.
func (p TurnPersisted) CountIDs() int { return p.count() }

// TurnReport is one ReportTurn. Origin is read on STARTED (and on an ENDED
// without STARTED); Outcome, Usage, Persisted and TranscriptIncomplete on
// ENDED.
type TurnReport struct {
	// LocalSeq is the worker's turn counter within the epoch, from 1.
	LocalSeq  int64
	Phase     TurnPhase
	Origin    TurnOrigin
	Outcome   TurnOutcome
	Usage     TurnUsage
	At        int64
	Persisted TurnPersisted
	// TranscriptIncomplete reports that the SDK dropped a transcript batch
	// of this turn (mirror_error): the session becomes broken.
	TranscriptIncomplete bool
}

// Turn is one recorded turn.
type Turn struct {
	SessionID string
	TurnNo    int64 // 1, 2, … per session
	Epoch     int64
	LocalSeq  int64
	Origin    TurnOrigin
	StartedAt int64 // zero for a turn first seen at its end
	EndedAt   int64
	Outcome   TurnOutcome
	Usage     TurnUsage
}

// TurnReported is ReportTurn's result. Duplicate reports a phase already
// recorded.
type TurnReported struct {
	Turn      Turn
	Duplicate bool
	Session   Session
	// LedgerDue reports that the first ENDED made the session's ready
	// ledger snapshot due for a collection (SessionLedger.DueAt).
	LedgerDue bool
}

// TurnFilter selects turns for ListTurns. Limit is required
// (1..MaxPageSize); AfterTurnNo is an exclusive cursor.
type TurnFilter struct {
	SessionID   string
	AfterTurnNo int64
	Limit       int
}

// SessionCwd is the working directory of one agent on one device.
type SessionCwd struct {
	AgentID   string // "" for the main thread
	DeviceID  string
	Cwd       string // at most MaxPathBytes
	UpdatedAt int64
}

// Size is the row's page size.
func (c SessionCwd) Size() int64 {
	return int64(len(c.AgentID) + len(c.DeviceID) + len(c.Cwd) + rowOverhead)
}

// SessionFile is the read record of one file: what the session last saw.
type SessionFile struct {
	DeviceID  string
	Path      string // at most MaxPathBytes
	SHA256    []byte // empty in a patch deletes the record
	MtimeMs   int64
	TouchedAt int64
}

// Size is the row's page size.
func (f SessionFile) Size() int64 {
	return int64(len(f.DeviceID) + len(f.Path) + len(f.SHA256) + rowOverhead)
}

// JobNotice tracks the completion notification of a session job.
type JobNotice string

const (
	JobNoticeNo        JobNotice = "no"
	JobNoticeHanded    JobNotice = "handed"
	JobNoticePersisted JobNotice = "persisted"
)

// SessionJob is a device background job the session started, keyed by the
// call that started it. DeviceID and BootID come from the calls row.
type SessionJob struct {
	CallID      string
	DeviceID    string
	BootID      string
	JobID       string
	ToolUseID   string
	Description string // at most MaxDescriptionBytes
	OutputPath  string // at most MaxPathBytes
	// State, ExitCode and EndedAt come from the jobs row; State is empty
	// while no such row exists yet.
	State     JobState
	ExitCode  *int32
	EndedAt   int64
	Notice    JobNotice
	NoticeAt  int64
	CreatedAt int64
}

// Size is the row's page size.
func (j SessionJob) Size() int64 {
	return int64(len(j.CallID) + len(j.DeviceID) + len(j.BootID) + len(j.JobID) + len(j.ToolUseID) +
		len(j.Description) + len(j.OutputPath) + rowOverhead)
}

// BgTask is one background task of the CLI itself: a subagent, a shell
// command, a workflow or another task the SDK reports. Epoch is the lease epoch it started under: a task of an
// older epoch than the current one died with its CLI.
type BgTask struct {
	TaskID      string
	ToolUseID   string
	Kind        string // the SDK's task_type, such as "local_agent" or "local_bash"; at most MaxIDBytes
	Description string // at most MaxDescriptionBytes
	Epoch       int64
	StartedAt   int64
}

// Size is the row's page size.
func (b BgTask) Size() int64 {
	return int64(len(b.TaskID) + len(b.ToolUseID) + len(b.Kind) + len(b.Description) + rowOverhead)
}

// RecordsPatch is one PatchSessionRecords. Jobs are registered with CallID,
// JobID, ToolUseID, Description and OutputPath; other SessionJob fields are
// ignored. BgTasksStarted's Epoch and StartedAt are set by the store.
// AwaitingCallIDs (at most MaxOwedCalls) mark calls owing a completion
// notice. Persisted is a split part of a turn's persisted set.
type RecordsPatch struct {
	Cwds            []SessionCwd
	Files           []SessionFile
	Jobs            []SessionJob
	ReturnedCallIDs []string
	AwaitingCallIDs []string
	HandedJobCalls  []string // call ids of jobs whose notification was handed
	BgTasksStarted  []BgTask
	BgTasksEnded    []string // task ids
	Persisted       TurnPersisted
}

// RecordSections selects the sections GetSessionRecords returns.
type RecordSections uint8

const (
	RecordCwds RecordSections = 1 << iota
	RecordFiles
	RecordJobs
	RecordBgTasks
	RecordGrants
	RecordAll = RecordCwds | RecordFiles | RecordJobs | RecordBgTasks | RecordGrants
)

// RecordsCursor is the exclusive position after the last row of a
// GetSessionRecords page: the row's section and its key. Keys are
// (AgentID, DeviceID) for cwds, (DeviceID, Path) for files, (CallID, "")
// for jobs, (TaskID, "") for background tasks and (GrantID, "") for grants.
type RecordsCursor struct {
	Section    RecordSections // exactly one section
	Key1, Key2 string
}

// RecordsRequest selects one GetSessionRecords page.
type RecordsRequest struct {
	Sections RecordSections
	MaxBytes int64 // 1..MaxTranscriptPageBytes
	After    *RecordsCursor
}

// SessionRecords is one page of the session's runtime records, by section.
// Next is where the next page starts; nil when this page holds the rest.
type SessionRecords struct {
	Cwds    []SessionCwd
	Files   []SessionFile
	Jobs    []SessionJob
	BgTasks []BgTask
	Grants  []Grant
	Next    *RecordsCursor
}

// EventInput is one durable event to append; ephemeral events never enter
// the store. Boundary marks the events kept after the view cap (turn start
// and end, interruption, result summary). TURN_STOPPED and TURN_INTERRUPTED
// are always boundaries, regardless of this flag.
type EventInput struct {
	Kind     string // at most MaxCodeBytes
	Format   string // "claude-sdk" for SDK messages; at most MaxIDBytes
	Payload  []byte // opaque JSON
	At       int64
	Boundary bool
}

// EventKindTranscriptGapAccepted is the kind of the event RecoverSession
// records. Only the control plane records it: a worker batch with it is
// dropped.
const EventKindTranscriptGapAccepted = "TRANSCRIPT_GAP_ACCEPTED"

// Turn boundaries are durable and recorded only by the control plane in the
// transaction that first closes the turn. Their payload is
// {"turn_no":"<decimal uint64>","outcome":"stopped"|"interrupted"}.
const (
	EventKindTurnStopped     = "TURN_STOPPED"
	EventKindTurnInterrupted = "TURN_INTERRUPTED"
)

// Other kinds only the control plane records: a new user input
// (SendMessage), a resolved approval, and a session state change such as a
// stop.
const (
	EventKindInputAccepted    = "INPUT_ACCEPTED"
	EventKindApprovalResolved = "APPROVAL_RESOLVED"
	EventKindSessionState     = "SESSION_STATE"
	EventKindWorkspace        = "WORKSPACE"
	// EventKindLedger: the session's ledger snapshot changed. Like
	// workspace events, it does not count as activity.
	EventKindLedger = "LEDGER"
)

// Live-only kinds the control plane announces and never stores: a queued
// session's queue reason and its lease holder's disconnect. A worker batch
// with either is dropped as invalid like any other control-plane kind.
const (
	EventKindQueue  = "QUEUE"
	EventKindWorker = "WORKER"
)

// ControlPlaneOnlyKind reports whether only the control plane records events
// of kind: a worker event batch containing one is dropped as invalid.
func ControlPlaneOnlyKind(kind string) bool {
	switch kind {
	case EventKindTranscriptGapAccepted, EventKindTurnInterrupted, EventKindTurnStopped,
		EventKindInputAccepted, EventKindApprovalResolved, EventKindSessionState, EventKindWorkspace, EventKindLedger,
		EventKindQueue, EventKindWorker:
		return true
	}
	return false
}

// EventPublish marks events a worker publishes: the fence of its
// connection and the batch sequence, from 1 per epoch.
type EventPublish struct {
	Fence    Fence
	BatchSeq int64
}

// EventsAppended is AppendEvents' result. Seqs has, per event, its seq or
// 0 when it was not stored. Deduplicated reports a worker batch already
// consumed, Dropped one consumed but refused as invalid; neither stores an
// event.
type EventsAppended struct {
	Seqs         []int64
	Deduplicated bool
	// Dropped reports a batch consumed and recorded as dropped because an
	// event failed validation; DropReason (matching ErrInvalid) says why.
	// It commits; the method's error is nil.
	Dropped    bool
	DropReason error
}

// SessionEvent is one stored event of the conversation view.
type SessionEvent struct {
	SessionID string
	Seq       int64
	At        int64
	Kind      string
	Format    string
	Payload   []byte // nil when Truncated
	// PayloadBytes is the size of the payload given, stored or not.
	PayloadBytes int64
	Truncated    bool
}

// EventPage is one ReadEvents page. OldestSeq is the oldest event the
// session still keeps (0 when it has none). Missed reports that events
// after the requested seq were evicted: the page starts later than asked.
type EventPage struct {
	Events    []SessionEvent
	OldestSeq int64
	Missed    bool
}

// EventFilter selects events for ReadEvents. Limit is required
// (1..MaxPageSize); MaxBytes is required (1..MaxTranscriptPageBytes).
type EventFilter struct {
	SessionID string
	AfterSeq  int64
	Limit     int
	MaxBytes  int64
}

// ApprovalState is an approval's resolution.
type ApprovalState string

const (
	ApprovalPending  ApprovalState = "pending"
	ApprovalApproved ApprovalState = "approved"
	ApprovalDenied   ApprovalState = "denied"
	ApprovalExpired  ApprovalState = "expired"
)

// Valid reports whether s is a known state.
func (s ApprovalState) Valid() bool {
	switch s {
	case ApprovalPending, ApprovalApproved, ApprovalDenied, ApprovalExpired:
		return true
	}
	return false
}

// Approval is one request for a one-time approval of a denied device tool
// call. (Tool, DeviceID, MatchKey) is the grant key: the command for bash,
// the path for write and edit.
type Approval struct {
	ApprovalID string
	SessionID  string
	UserID     string // the session's user, set by the store
	ToolUseID  string
	AgentID    string
	Tool       string // at most MaxToolBytes
	DeviceID   string
	MatchKey   string // MatchKeyHexDigits lowercase hex digits
	Display    string // at most MaxDescriptionBytes
	Reason     string // at most MaxDescriptionBytes
	State      ApprovalState
	CreatedAt  int64
	ExpiresAt  int64
	ResolvedAt int64
}

// ApprovalSize is an approval's page size in ListApprovals.
func ApprovalSize(a Approval) int64 {
	return int64(len(a.ApprovalID) + len(a.SessionID) + len(a.UserID) + len(a.ToolUseID) + len(a.AgentID) +
		len(a.Tool) + len(a.DeviceID) + len(a.MatchKey) + len(a.Display) + len(a.Reason) + rowOverhead)
}

// ApprovalRequested is RequestApproval's result. Expired is the oldest
// pending approval expired to make room, if any.
type ApprovalRequested struct {
	Approval Approval
	Created  bool
	Expired  *Approval
}

// ApprovalResolution is one ResolveApproval. On approval, Input supplies
// InputID, Text and CreatedAt (= At when zero) of the retry message; its
// other fields are set by the store.
type ApprovalResolution struct {
	ApprovalID     string
	Approve        bool
	At             int64
	GrantExpiresAt int64 // required when Approve
	Input          Input
}

// ApprovalResolved is ResolveApproval's result. Input is the submitted
// retry message on approval. Session and Lease are as after the call.
type ApprovalResolved struct {
	Approval Approval
	Changed  bool
	Input    *Input
	Session  Session
	Lease    Lease
}

// ApprovalCursor is the exclusive (CreatedAt, ApprovalID) key of the last
// approval read.
type ApprovalCursor struct {
	CreatedAt  int64
	ApprovalID string
}

// ApprovalFilter selects approvals for ListApprovals. Limit
// (1..MaxPageSize) and MaxBytes (1..MaxTranscriptPageBytes) are required;
// one of UserID and SessionID is required; States empty does not filter.
type ApprovalFilter struct {
	UserID    string
	SessionID string
	States    []ApprovalState
	Limit     int
	MaxBytes  int64
	After     *ApprovalCursor
}

// GrantKey matches a grant to a tool call.
type GrantKey struct {
	Tool     string
	DeviceID string
	MatchKey string
}

// Check validates the key's lengths.
func (k GrantKey) Check() error {
	if err := CheckText("tool", k.Tool, MaxToolBytes, false); err != nil {
		return err
	}
	if err := CheckID("device id", k.DeviceID); err != nil {
		return err
	}
	return CheckMatchKey(k.MatchKey)
}

// CheckMatchKey validates a match key: exactly MatchKeyHexDigits lowercase
// hexadecimal digits.
func CheckMatchKey(k string) error {
	if len(k) != MatchKeyHexDigits {
		return invalid("match key must be %d lowercase hex digits", MatchKeyHexDigits)
	}
	for i := 0; i < len(k); i++ {
		if c := k[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return invalid("match key must be %d lowercase hex digits", MatchKeyHexDigits)
		}
	}
	return nil
}

// Grant is a one-time permission written by an approval; consuming it
// deletes it. GrantID is the approval's id.
type Grant struct {
	GrantID   string
	SessionID string
	Key       GrantKey
	CreatedAt int64
	ExpiresAt int64
}

// Size is the row's page size.
func (g Grant) Size() int64 {
	return int64(len(g.GrantID) + len(g.SessionID) + len(g.Key.Tool) + len(g.Key.DeviceID) + len(g.Key.MatchKey) + rowOverhead)
}
