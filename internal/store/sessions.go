package store

import "context"

// SessionOps is every session operation. It is embedded in Ops, so the
// Store and the tx given to Store.Tx both implement it, and a session write
// can share one transaction with call writes (the call router's admission
// hook, the cascade of DeleteSession, the persistence markers of
// ReportTurn).
//
// # Leases and fencing
//
// A worker drives a session only while it holds the session's lease: one
// row per session holding (epoch, holder worker, holder boot, holder
// connection, expiry). The epoch starts at 0 and only grows: AcquireLease
// adds 1 every time it hands the lease to a new holder; re-acquiring,
// extending, resuming and releasing keep it. A released or expired lease
// has no holder; the next AcquireLease takes epoch + 1.
//
// The lease is bound to one connection of the holder's boot: the one that
// acquired it, or the one that last re-acquired or resumed it. Connection
// ids are opaque strings chosen by the control plane, unique across its
// starts (for example a per-start nonce plus a counter), so an id stored by
// an earlier process never names a new connection; after a control-plane
// restart every lease is rebound by ResumeLeases. A lease is extended per connection (ExtendLeases), so a
// lease bound to a dead connection expires.
//
// Every operation a worker drives, read or write, carries a Fence:
// (session, epoch, worker, boot, connection, now), and checks it in its own
// transaction before touching anything: the lease must name that epoch,
// worker, boot and connection, and expire after now. Otherwise it returns
// *FencedError (matching ErrFenced) with the current epoch and writes
// nothing. Control-plane methods take no fence. AppendEvents takes an
// optional one, for events a worker publishes. The call router's admission
// hook uses AdmitSessionCall, so a session call is created, resubmitted or
// attached to only in a transaction that proves its session's lease.
//
// Losing a lease (ReleaseLease, ReleaseWorkerLeases, ExpireLeases,
// SettleSession, RecoverSession, or an AcquireLease that replaces a holder) applies
// one rule in the same transaction, and reports it as a LeaseLoss: a
// running turn ends as interrupted; taken inputs not yet persisted become
// persisted when their input id is in the transcript and pending
// otherwise; job notifications handed but not persisted go back to not
// handed; and the session becomes queued when it has pending work (a
// pending input, or an ended job whose notification is not handed), idle
// otherwise. A stopped or broken session keeps its state.
// Each open turn closed this way gets one EventKindTurnInterrupted boundary
// in the same transaction. A stop request alone closes no turn.
//
// # Recovery
//
// GetRecovery lists everything not yet acknowledged, whichever epoch
// produced it: inputs taken and not persisted, job notifications handed and
// not persisted, background tasks started under an older epoch (their CLI
// is dead), calls not persisted or owing a completion notice, and whether
// the last turn was interrupted. It is never computed from "the previous
// epoch", so an AcquireLease whose reply was lost and is retried (same
// lease, see AcquireLease) gets the same recovery.
//
// # Persistence markers
//
// The direction is always "repeat rather than lose":
//
//   - Inputs: pending → taken (TakeInputs, by epoch) → persisted (a
//     persisted set, or the lease-loss rule when the input id is in the
//     transcript). The input id is the transcript uuid of the user message
//     the worker pushes, so the transcript itself proves delivery.
//   - Calls: the calls row is the record of an in-flight call. returned_at
//     is set when the worker's tool handler returned (PatchSessionRecords);
//     persisted_at when a persisted set lists it. notice_owed marks a call
//     returned to the model with a non-terminal outcome whose completion
//     notice is still owed (PatchSessionRecords.AwaitingCallIDs); a
//     persisted set's LateCallIDs clear it once the notice is delivered.
//   - Job notifications: no → handed (PatchSessionRecords) → persisted (a
//     persisted set); handed goes back to no on lease loss.
//   - Background tasks of the CLI: a row exists from its start until it
//     ends or a persisted set lists it as notified.
//
// A persisted set (TurnPersisted) comes with ReportTurn ENDED or, split,
// with PatchSessionRecords. The worker may list only items delivered
// before the SDK result of a turn it has reported or is reporting ENDED;
// the store cannot check that. Marks are idempotent.
//
// # Session states
//
//	idle ──▶ queued     pending work arrives (or remains at lease loss) and no live lease
//	idle, queued ──▶ running   ReportTurn STARTED
//	queued ──▶ idle     AcquireLease
//	running ──▶ idle    ReportTurn ENDED, or lease loss without pending work
//	running ──▶ queued  lease loss with pending work
//	idle, running ──▶ broken   ReportTurn ENDED with TranscriptIncomplete
//	broken ──▶ idle     TranscriptRepaired by the holder
//	any but stopped ──▶ stopped   StopSession
//
// Holding the lease is independent of the state: an idle session may have a
// holder whose CLI waits for input. A stopped session keeps its lease until
// the holder releases it; a broken one accepts appends (for the repair) and
// new inputs, but no new lease, no taking of inputs, no approvals and no
// calls.
//
// What a stopped session refuses with ErrStopped: SubmitInput,
// TakeInputs, RequestApproval, ResolveApproval, ConsumeGrant,
// AdmitSessionCall and AcquireLease. What it still accepts:
// AppendTranscript, ReportTurn, PatchSessionRecords, AppendEvents, every
// read, ExtendLeases, ResumeLeases (reporting Stopped) and ReleaseLease.
// Worker-driven methods that return a session report its state, so the
// holder learns of the stop from any of them.
//
// # Bounds
//
// Each bound is enforced by the store in the transaction that would exceed
// it. Configurable ones are arguments supplied by the caller (Default*
// constants hold the defaults); the others are constants. Every string has
// a byte cap (Max*Bytes) checked before anything is read; an over-long one
// is ErrInvalid.
//
//   - sessions: maxSessions per user (DefaultMaxSessionsPerUser), counting
//     stopped sessions until deleted. ErrLimit.
//   - session_leases: one row per session.
//   - transcript_entries: each entry's charge (TranscriptCharge: JSON, uuid
//     and TranscriptEntryOverhead) ≤ quota.MaxEntryBytes; the session's
//     charges ≤ quota.MaxSessionBytes (DefaultMaxTranscriptBytes); the
//     user's ≤ quota.MaxUserBytes (DefaultMaxUserTranscriptBytes).
//     ErrTranscriptFull, and the whole batch is refused. The overhead bounds
//     the row count: a full default session holds at most 4 Mi entries.
//     Subpaths per session ≤ MaxTranscriptSubpaths (ErrLimit).
//   - session_inputs: inputs not yet persisted ≤ MaxOpenInputs per session
//     (ErrLimit); text ≤ MaxInputTextBytes. A persisted input keeps its
//     metadata but not its text; only the newest KeptInputs persisted
//     inputs are kept.
//   - session_turns: the newest KeptTurns turns; usage totals live on the
//     session, so deleting a turn loses no aggregate.
//   - session_events: a payload over MaxEventPayloadBytes is stored without
//     it (Truncated). Once a session's stored payload bytes would pass
//     maxViewBytes (DefaultMaxViewBytes), only boundary events are stored,
//     without payload, and the session is marked ViewTruncated. At most
//     MaxSessionEvents events are kept, the oldest evicted first.
//   - session_cwds ≤ MaxSessionCwds (ErrLimit); session_files ≤
//     MaxSessionFiles, evicting the least recently touched; session_jobs ≤
//     MaxSessionJobs (ErrLimit), ended and persisted ones purged by
//     PurgeSessionJobs; session_bgtasks ≤ MaxSessionBgTasks (ErrLimit).
//   - session_approvals: pending ≤ MaxPendingApprovals per session, the
//     oldest pending one expiring to make room; resolved ones: the newest
//     KeptResolvedApprovals. With 64-hex-digit match keys and 1 KiB display
//     and reason, an approval is at most about 2.6 KiB, so a session's
//     approvals stay under about 250 KiB. session_grants: unconsumed ones ≤
//     MaxSessionGrants per session, the one expiring first dropped to make
//     room; consumed ones (kept until they expire, for the replay of
//     ConsumeGrant) ≤ MaxSessionGrants, the oldest consumption dropped. A
//     grant row is at most about 0.7 KiB, so 64 rows stay under 45 KiB.
//   - calls: a session's calls not in a terminal state ≤ the maxActive of
//     AdmitSessionCall (DefaultMaxActiveSessionCalls), ErrTooManyCalls;
//     calls owing a notice ≤ MaxOwedCalls (ErrLimit). A session's calls
//     rows are not otherwise capped; they are deleted with the session.
//
// Every list is a bounded page: a keyset page (Limit 1..MaxPageSize) with
// a byte budget where rows are large, or bounded by one of the caps above.
//
// # Protocol operations
//
// The worker link's session operations and the SessionService RPCs map to
// these methods (worker operations take the connection's Fence):
//
//	AcquireLease (fresh)        AcquireLease (LeaseRequest.Fresh) in one Tx; the lease
//	                            reply is read in a second, fenced Tx (GetRecovery,
//	                            GetSessionRecords, GetSession), so a write acquires and
//	                            a read builds the reply; a lease lost in between is
//	                            answered Fenced. Lease.transcript_gap_turn_no is
//	                            Session.TranscriptGapTurnNo
//	Persisted.gap_notice_turn_no  TurnPersisted.GapNoticeTurnNo (ReportTurn, PatchSessionRecords)
//	ResumeLeases                ResumeLeases
//	ReleaseLease                ReleaseLease (LeaseLoss.Repeated → ok.deduplicated)
//
// Some refusals are results with a nil error, because they commit a
// change the caller must keep (it must not roll its Tx back):
// LeaseAcquired.Released with Refusal ErrStopped (→ STOPPED) or ErrBroken
// (→ NOT_OFFERED: a broken session is not offered); EventsAppended.Dropped
// (→ INVALID); TranscriptAppended.Full (→ TRANSCRIPT_FULL, the session is
// stopped; the control plane then runs its usual stop follow-ups, as after
// StopSession: the stop event, Nudge STOP to the holder and stopping the
// session's device jobs). Other errors map as: *FencedError → Fenced, ErrStopped →
// STOPPED, ErrBroken on AcquireLease → NOT_OFFERED, ErrLimit → LIMIT,
// ErrInvalid → INVALID, ErrBusy → STORE_UNAVAILABLE.
//
//	LoadTranscript              LoadTranscript
//	ListTranscriptKeys          ListTranscriptSubpaths
//	AppendTranscript            AppendTranscript
//	TakeInputs                  TakeInputs
//	ReportTurn                  ReportTurn; the control plane parses usage_json into
//	                            TurnUsage, and stores zero usage when it does not parse
//	PatchSessionState           PatchSessionRecords
//	GetSessionState             GetSessionRecords (and GetRecovery for recovery)
//	TranscriptRepaired          TranscriptRepaired
//	PublishEvents               AppendEvents with an EventPublish
//	RequestApproval             RequestApproval
//	ConsumeGrant                ConsumeGrant
//	SubmitCall                  AdmitSessionCall, then CreateCall or TransitionCall, in one Tx
//	(lease upkeep, no message)  ExtendLeases; ExtendLeasesAfterRestart once at start;
//	                            ExpireLeases; ReleaseWorkerLeases on a new boot
//	CreateSession               CreateSession
//	SendMessage                 SubmitInput, then for a new input AppendEvents (the
//	                            INPUT_ACCEPTED view event) and SetInputEventSeq in the
//	                            same Tx
//	WatchSession                ReadEvents
//	ListSessions / GetSession   ListSessions (SessionFilter.WebUserID) / GetSession and
//	                            ListSessionCwds
//	ListApprovals               ListApprovals
//	ResolveApproval             ResolveApproval
//	Interrupt                   InterruptSession
//	StopSession                 StopSession
//	RecoverSession              RecoverSession
//	DeleteSession               DeleteSession
//
// Event kinds are the SessionEvent.Kind names without the KIND_ prefix,
// such as "TURN_STARTED" and EventKindTranscriptGapAccepted.
// Every stored durable event except WORKSPACE moves LastActivityAt to max(current, its
// timestamp), including a retained boundary with a truncated payload. New
// user inputs also move it; dropped events and live-only fragments do not. Internal turn boundaries use DefaultMaxViewBytes
// and respect a view already truncated by a caller's smaller cap.
type SessionOps interface {
	// Sessions.

	// CreateSession inserts s in state idle with turn count, usage and byte
	// counters zero, creates its lease row (epoch 0, no holder) and its
	// initial working directories cwds (AgentID "" is the main thread), in
	// one transaction. The project must belong to the user (ErrNotFound
	// otherwise). A session with the same (ProjectID, ClientRequestID)
	// returns its current row with Created false when the resolved
	// LocationIndex, the requested worker (RequestedWorkerID), Model and
	// PermissionMode match. Local requires PinnedWorkerID, and PinDerived
	// requires Local (ErrInvalid).
	// ErrExists reports different creation settings or a unique-key conflict.
	// Only a fresh session checks the cap (ErrLimit) and ownership of the
	// default device and any pinned worker (ErrNotFound). Title starts empty
	// and LastActivityAt starts at CreatedAt. len(cwds) ≤ MaxSessionCwds.
	//
	// The epoch starts over for a new session, so session ids must never
	// be reused: a deleted session's id given to a new one would let a
	// message still in flight for the old one pass the new one's fence at
	// epoch 1. The control plane mints UUIDv7 ids and never reuses them.
	CreateSession(ctx context.Context, s Session, cwds []SessionCwd, maxSessions int) (SessionCreated, error)
	// GetSessionByClientRequestID finds the session for the given project and
	// nonempty client request key, or returns ErrNotFound. Callers authorize
	// through the project. now has the same semantics as in GetSession.
	GetSessionByClientRequestID(ctx context.Context, projectID, clientRequestID string, now int64) (Session, error)
	// GetSession returns the session, or ErrNotFound. now is required, in
	// Unix milliseconds; pending approvals count only when ExpiresAt > now,
	// even before the expiry sweep runs, as with other expiry reads.
	GetSession(ctx context.Context, sessionID string, now int64) (Session, error)
	// ListSessions returns at most f.Limit sessions ordered by LastActivityAt
	// descending, then ID descending. f.Now has GetSession's expiry semantics.
	// A traversal is not a snapshot: sessions remaining in the filter with
	// unchanged activity are returned exactly once. Activity only increases,
	// so already returned sessions never repeat. A not-yet-returned session
	// moving above the cursor is skipped until refresh; deleted sessions
	// disappear, and new sessions above the cursor appear on refresh.
	ListSessions(ctx context.Context, f SessionFilter) ([]Session, error)
	// ListSessionCwds returns the session's main-thread working directories
	// (AgentID ""), ordered by device id, at most MaxSessionCwds. It takes no
	// fence: the user-facing API shows them. ErrNotFound if the session is
	// absent.
	ListSessionCwds(ctx context.Context, sessionID string) ([]SessionCwd, error)
	// SetSessionTitle unconditionally sets a nonempty title of at most
	// MaxSessionTitleBytes. Character validation is the service's job.
	// ErrNotFound if absent. It does not move LastActivityAt.
	SetSessionTitle(ctx context.Context, sessionID, title string) error
	// StopSession moves the session to stopped at time at with reason (a
	// short code, at most MaxCodeBytes) and returns it with its lease as it
	// stands. The lease is not released: the caller tells the holder to
	// stop, and the holder's final transcript appends and turn reports
	// still pass the fence; the holder then calls ReleaseLease. Idempotent:
	// a stopped session is returned unchanged with changed false.
	// ErrNotFound if absent.
	StopSession(ctx context.Context, sessionID string, at int64, reason string) (s Session, lease Lease, changed bool, err error)
	// InterruptSession records an interrupt of the running turn: it sets
	// the session's InterruptTurnNo to the number of the open turn (the
	// highest one, when several are open) and returns the session with its
	// lease, so the caller nudges the holder. With no turn open it changes
	// nothing and returns changed false. The holder interrupts its current
	// turn only if that turn's number is at most InterruptTurnNo, so a
	// repeated or late hint never interrupts a later turn. ErrNotFound if
	// absent.
	InterruptSession(ctx context.Context, sessionID string, at int64) (s Session, lease Lease, changed bool, err error)
	// DeleteSession deletes the session and everything that belongs to it:
	// lease, transcript, inputs, turns, events, working directories, file
	// records, jobs, background tasks, approvals, grants, its calls rows and
	// the ended rows of the device jobs table that name it, and subtracts
	// its transcript bytes from the user's total. It refuses with
	// ErrSessionActive, deleting nothing, unless the session is stopped, has
	// no live lease at now, and has no call in a non-terminal state (a
	// deleted calls row would let the same call id be dispatched again).
	//
	// It runs as a series of short transactions on the store handle, each
	// deleting at most DeleteChunkRows transcript entries or calls rows and
	// rechecking the preconditions, then one last transaction for the
	// bounded rest; called inside a Tx it returns ErrInvalid. Between chunks
	// other operations run; none can reach the transcript, since a stopped
	// session without a live lease cannot be leased. A delete interrupted by
	// a crash leaves a stopped session with part of its transcript; calling
	// DeleteSession again finishes it. Device jobs still running keep their
	// rows until they end. Timers naming the session or its approvals are
	// left to their handlers, which find nothing. ErrNotFound if absent,
	// including after a delete that already succeeded.
	DeleteSession(ctx context.Context, sessionID string, now int64) error
	// CheckSessionDeletable checks DeleteSession's preconditions without
	// deleting anything: ErrSessionActive if not stopped, with a live lease,
	// or with a non-terminal call; ErrNotFound if absent. now is required.
	// Use inside one Tx to preflight all sessions before a project cascade.
	// It reserves nothing; DeleteSession rechecks during its chunked writes.
	CheckSessionDeletable(ctx context.Context, sessionID string, now int64) error
	// TranscriptRepaired moves a broken session back to idle after the
	// holder re-imported the missing transcript batches. It never touches
	// TranscriptGapTurnNo. Fenced.
	// Idempotent: any other state is returned unchanged. (The holder's live
	// lease rules out queued: pending work goes to the holder.)
	TranscriptRepaired(ctx context.Context, f Fence) (Session, error)
	// RecoverSession accepts the gap in a broken session's transcript, the
	// user's way out when the holder cannot repair it. accept must be true
	// (ErrInvalid otherwise); the session must be broken (ErrNotBroken
	// otherwise, nothing written). In one transaction it releases any held
	// lease, live or not, with the lease-loss rule (that epoch is then
	// fenced; Loss reports it so the caller revokes it), moves the session
	// to idle, or queued with pending work, and records a boundary event of
	// kind EventKindTranscriptGapAccepted at time at. It sets
	// TranscriptGapTurnNo to the number of the turn that broke the session
	// (the first incomplete turn since it last became broken): the gap now
	// exists and the model has not been told; the next holder delivers the
	// reminder and acknowledges it. ErrNotFound if absent.
	RecoverSession(ctx context.Context, sessionID string, accept bool, at int64, maxViewBytes int64) (SessionRecovered, error)
	// SettleSession re-applies the no-lease rule at now: if the lease has
	// expired it is released (the lease-loss rule, reported as Loss); then
	// an idle session without a live lease and with pending work becomes
	// queued. The control plane calls it when work arrives outside
	// SubmitInput, such as a device job of the session ending. ErrNotFound
	// if absent.
	SettleSession(ctx context.Context, sessionID string, now int64) (SessionSettled, error)

	// Leases.

	// AcquireLease gives the lease to r's worker, boot and connection when
	// it has no holder or has expired at r.Now: epoch + 1, ExpiresAt =
	// r.ExpiresAt. Taking over an expired lease first applies the
	// lease-loss rule to the old holder in the same transaction and reports
	// it as Loss. A queued session becomes idle.
	//
	// Idempotent: a lease already held, unexpired, by the same worker and
	// boot is returned with the same epoch (Reattached), rebound to
	// r.ConnID and extended to r.ExpiresAt; this succeeds on a stopped or
	// broken session too, so a holder whose reply was lost can learn the
	// epoch and release. With r.Fresh (the worker holds no local state for
	// the session) the holder is treated as a new holder instead: its own
	// lease goes through the lease-loss rule (reported as Loss) and the
	// epoch advances, so the worker's counters starting at 1 never collide
	// with the old epoch's. A fresh acquisition by the holder of a stopped
	// or broken session releases its lease instead (the lease-loss rule,
	// Loss set, remembered for a resent ReleaseLease; a broken session stays
	// broken) and reports Released with Refusal ErrStopped or ErrBroken and a
	// nil error, so the release commits even inside the caller's Tx: a
	// holder that lost its state cannot leave the lease unreleasable. Other
	// refusals are errors and write nothing: ErrLeaseHeld (another
	// live holder, or the session is pinned to another worker), ErrStopped
	// or ErrBroken (a new holder for a stopped or broken session),
	// ErrNotFound (no such session, or the worker is not the session
	// user's).
	AcquireLease(ctx context.Context, r LeaseRequest) (LeaseAcquired, error)
	// HasLiveLease reports whether workerID holds, under bootID, any lease
	// that has not expired at now. The worker link uses it to tell whether
	// the worker's current boot is still alive.
	HasLiveLease(ctx context.Context, workerID, bootID string, now int64) (bool, error)
	// GetLease returns the session's lease row, held or not, or
	// ErrNotFound.
	GetLease(ctx context.Context, sessionID string) (Lease, error)
	// GetRecovery returns, fenced, what the holder must recover (see the
	// type): every unacknowledged item, whichever epoch produced it. Calls
	// are listed oldest first, at most MaxPageSize, with CallsTruncated set
	// when more exist (list them with ListCalls and CallFilter.Recovery).
	GetRecovery(ctx context.Context, f Fence) (Recovery, error)
	// ExtendLeases moves ExpiresAt to expiresAt, in one statement, for each
	// key whose lease still has that epoch, worker, boot and connection and
	// has not expired at now; stopped sessions included. It returns the
	// keys it did not extend (lease lost, taken over, rebound or expired),
	// in input order; the caller revokes them on their connections.
	// len(keys) ≤ MaxPageSize; the caller chunks.
	ExtendLeases(ctx context.Context, keys []LeaseKey, now, expiresAt int64) (lost []LeaseKey, err error)
	// ReleaseLease clears the holder (epoch unchanged) and applies the
	// lease-loss rule. Fenced, except for a resent release: when the lease
	// has no holder, is still at f.Epoch, and was last held by f's worker
	// and boot (whichever way it was lost), it succeeds with no change and
	// Repeated set, on any connection. Once another epoch is acquired the
	// resend is fenced.
	ReleaseLease(ctx context.Context, f Fence) (LeaseLoss, error)
	// ExtendLeasesAfterRestart extends every held lease to at least
	// expiresAt (now + the lease lifetime), in one statement, and returns
	// how many. The control plane calls it once at start, before any
	// ExpireLeases, SettleSession or AcquireLease: a lease's expiry is the
	// last extension of the previous process plus the lifetime, so after
	// an outage longer than the lifetime the first sweep would otherwise
	// reclaim every lease and interrupt running turns whose workers are
	// about to resume. Leases already past their expiry are extended too;
	// a released lease (no holder) is not touched, and no expiry is moved
	// earlier.
	ExtendLeasesAfterRestart(ctx context.Context, expiresAt int64) (int, error)
	// ReleaseWorkerLeases releases at most limit (1..MaxPageSize) leases
	// held by workerID under a boot other than keepBootID, applying the
	// lease-loss rule to each, oldest acquisition first. It runs when the
	// worker connects with a new boot: the old process and its CLIs are
	// dead. The caller repeats it until it returns fewer than limit.
	ReleaseWorkerLeases(ctx context.Context, workerID, keepBootID string, now int64, limit int) ([]LeaseLoss, error)
	// ResumeLeases re-attaches leases after the same boot reconnects on
	// connection connID: for each key, the lease must have that epoch,
	// workerID and bootID (any connection) and must not have expired at
	// now; it is then rebound to connID and extended to expiresAt, stopped
	// sessions included (Stopped reports it). Results are per key, in input
	// order: Err nil, *FencedError (another epoch, holder or boot, or
	// expired), or ErrNotFound. len(keys) ≤ MaxResumeLeases. Idempotent.
	ResumeLeases(ctx context.Context, workerID, bootID, connID string, keys []SessionEpoch, now, expiresAt int64) ([]LeaseResumed, error)
	// ExpireLeases releases at most limit (1..MaxPageSize) leases that
	// expired at or before now, earliest expiry first, applying the
	// lease-loss rule to each. The control plane runs it from its lease
	// sweep; a lease is never extended by a timer write, so extension
	// stays one statement.
	ExpireLeases(ctx context.Context, now int64, limit int) ([]LeaseLoss, error)
	// CheckFence returns the lease when f matches it, else *FencedError
	// with the current epoch. ErrNotFound if the session does not exist. It
	// writes nothing.
	CheckFence(ctx context.Context, f Fence) (Lease, error)
	// AdmitSessionCall is the admission check of a session's device call.
	// The call router runs it, in the same Tx, on every path that lets a
	// session call run or be observed by a worker: before CreateCall of a
	// new call, before the lost → accepted resubmission of an existing
	// call, and before attaching a subscriber to an existing call. callID
	// is the call's id; for an existing call it must be a call of
	// f.SessionID (ErrNotFound otherwise) and is not counted against
	// maxActive. Checks: f (*FencedError); the session is neither stopped
	// (ErrStopped) nor broken (ErrBroken); the session has fewer than
	// maxActive other calls in a non-terminal state and fewer than
	// MaxUnpersistedSessionCalls other calls not yet persisted
	// (ErrTooManyCalls), so recovery lists at most MaxRecoveryCalls. It
	// writes nothing. The router then creates or transitions the call with
	// SessionID = f.SessionID and the tool_use_id.
	//
	// Every error but a store failure is a refusal, not a store fault: the
	// router answers it as a terminal rejection, records nothing, and keeps
	// its store health: *FencedError and ErrNotFound → SESSION_FENCED;
	// ErrStopped → SESSION_STOPPED; ErrBroken → SESSION_STOPPED (the
	// holder of a broken session runs no CLI); ErrTooManyCalls →
	// OVERLOADED. The cap is a backstop: the worker queues device calls
	// locally at DefaultMaxActiveSessionCalls per session.
	AdmitSessionCall(ctx context.Context, f Fence, callID string, maxActive int) error

	// Transcript.

	// AppendTranscript appends b's entries to the subpath, fenced. For each
	// (session, subpath) the store keeps the epoch and the highest batch
	// sequence applied in it; a new epoch starts again from 0. A batch
	// whose BatchSeq is not above it returns Deduplicated and writes
	// nothing; one exactly 1 above it is applied; a gap is ErrInvalid. A
	// batch sequence is consumed only by a successful reply: the worker
	// resends an unacknowledged batch with the same sequence and content,
	// so a duplicate is told from a skipped batch. An entry whose uuid is
	// already in the subpath, or repeats one earlier in the batch, is
	// skipped. Inserted entries get the next seqs of the subpath. Bounds,
	// on the charge (TranscriptCharge) of the entries that would be
	// inserted, all or nothing: each ≤ q.MaxEntryBytes (itself ≤
	// MaxTranscriptEntryBytes, the link's message limit, which a single
	// entry may fill), the session's total ≤ q.MaxSessionBytes, the user's
	// total ≤ q.MaxUserBytes. Past a cap nothing of the batch is written,
	// the session is stopped (reason TRANSCRIPT_FULL) in the same
	// transaction, and the result reports Full with a FullReason matching
	// ErrTranscriptFull; the error is nil, so the stop commits. A batch of
	// more than one entry holds at most MaxTranscriptBatchBytes of JSON and
	// uuids (ErrInvalid). A new subpath beyond MaxTranscriptSubpaths is
	// ErrLimit. Allowed on a stopped or broken session (final flush,
	// repair).
	AppendTranscript(ctx context.Context, f Fence, b TranscriptBatch, q TranscriptQuota) (TranscriptAppended, error)
	// LoadTranscript returns, fenced, the subpath's entries with seq >
	// r.AfterSeq in seq order, at most r.Limit (1..MaxTranscriptPageEntries)
	// and stopping before the entry that would bring the page's charge over
	// r.MaxBytes (1..MaxTranscriptPageBytes), but always at least one entry
	// when one exists. Done reports that no entry follows the page.
	LoadTranscript(ctx context.Context, f Fence, r TranscriptPageRequest) (TranscriptPage, error)
	// ListTranscriptSubpaths returns, fenced, every subpath of the session
	// that has entries, ordered by subpath (at most MaxTranscriptSubpaths).
	ListTranscriptSubpaths(ctx context.Context, f Fence) ([]TranscriptSubpath, error)
	// HasTranscriptUUID reports, fenced, whether any subpath of the session
	// holds an entry with this uuid.
	HasTranscriptUUID(ctx context.Context, f Fence, uuid string) (bool, error)

	// Inputs.

	// SubmitInput inserts in as pending at in.CreatedAt. Idempotent by
	// (session, ClientMsgID): a repeat with the same kind, priority and text
	// returns the stored input with Created false; one that differs is
	// ErrExists. Checked in the same transaction: the session is not
	// stopped (ErrStopped), and it has fewer than MaxOpenInputs inputs not
	// yet persisted (ErrLimit). A broken session accepts inputs; they wait
	// for the repair. An idle session without a live lease becomes queued.
	// The result carries the session and its lease, so the caller nudges
	// the holder or offers the session. A new user-kind input also sets an
	// empty title to DeriveSessionTitle(in.Text), when nonempty, in this
	// transaction. Approval inputs and retries do not set it. A new user
	// input also moves LastActivityAt to max(current, in.CreatedAt) in this
	// transaction, even before a worker publishes. Approval inputs and
	// retries do not move activity. SubmitInput appends no view event itself.
	SubmitInput(ctx context.Context, in Input) (InputSubmitted, error)
	// SetInputEventSeq records seq as the view event of the session's input
	// inputID, so a repeated SubmitInput returns it (Input.EventSeq). The
	// control plane calls it in the transaction that submitted the input and
	// appended the event. seq ≥ 0; ErrNotFound if the input is absent.
	SetInputEventSeq(ctx context.Context, sessionID, inputID string, seq int64) error
	// TakeInputs marks every pending input taken by f.Epoch and returns all
	// inputs taken by f.Epoch and not yet persisted, oldest first (at most
	// MaxOpenInputs). Fenced. Idempotent: a retry returns the same inputs
	// plus any that arrived since. ErrStopped on a stopped session; on a
	// broken one it takes nothing and returns none.
	TakeInputs(ctx context.Context, f Fence) ([]Input, error)

	// Turns.

	// ReportTurn records a turn's start or end, fenced, idempotent by
	// (epoch, LocalSeq, Phase).
	//
	// STARTED must use the epoch's next LocalSeq (1 for its first turn),
	// else ErrInvalid. It closes a predecessor that never ENDED as
	// interrupted (ended at r.At), creates the turn with the next turn
	// number, adds 1 to the session's turn count, clears
	// PreviousTurnInterrupted and moves idle or queued to running.
	//
	// ENDED for a LocalSeq never STARTED in the epoch is ErrInvalid. The
	// first ENDED of a turn still open sets its end time, outcome and
	// usage, adds the usage to the session's totals, records the outcome as
	// the session's last and moves running to idle once no turn is open.
	// STOPPED means the holder observed a user abort from Interrupt or NOW;
	// the store does not infer it from an interrupt level or input priority.
	// The first closure wins: STOPPED and INTERRUPTED each record exactly
	// one control-plane-only durable boundary, atomically with the outcome.
	// A successor without ENDED records INTERRUPTED for its predecessor;
	// a late or fenced report never relabels it or adds another boundary.
	// The first, late, ENDED of a turn its successor closed keeps that
	// turn's outcome and end time but records its usage, on the turn and in
	// the session's totals. Either first ENDED applies r.Persisted (see
	// TurnPersisted) and, with TranscriptIncomplete (the SDK dropped a
	// transcript batch), moves the session to broken, remembering the turn
	// that broke it for RecoverSession; TranscriptIncomplete with a
	// persisted set is ErrInvalid. A repeated ENDED changes nothing but
	// applies its persisted set again. Stopped and broken sessions keep
	// their state. Only the newest KeptTurns turns, and KeptInputs
	// persisted inputs, are kept.
	ReportTurn(ctx context.Context, f Fence, r TurnReport) (TurnReported, error)
	// ListTurns returns at most f.Limit turns of the session ordered by
	// turn number.
	ListTurns(ctx context.Context, f TurnFilter) ([]Turn, error)

	// Session records.

	// PatchSessionRecords applies p, fenced, in one transaction: upserts
	// working directories (ErrLimit past MaxSessionCwds), upserts or
	// deletes file records (evicting the least recently touched past
	// MaxSessionFiles), registers jobs by the call that started them (the
	// call must be the session's and dispatched, ErrNotFound otherwise; its
	// device and boot come from the calls row; ErrLimit past
	// MaxSessionJobs), marks returned calls (returned_at = f.Now where
	// unset), marks calls owing a completion notice (AwaitingCallIDs,
	// ErrLimit past MaxOwedCalls per session), marks job notifications
	// handed, records started background tasks with f.Epoch (ErrLimit past
	// MaxSessionBgTasks), deletes ended ones, and applies a persisted set
	// (see TurnPersisted). Ids of other sessions are ignored. Repeats are
	// harmless. At most MaxPatchItems items in all.
	PatchSessionRecords(ctx context.Context, f Fence, p RecordsPatch) error
	// RegisterSessionJob registers, without a fence, the device job j a
	// session call started (the control plane calls it when the call's
	// result reports a background job, in the transaction that settles the
	// call). j's CallID, JobID, ToolUseID, Description, OutputPath and
	// CreatedAt are read; device and boot come from the calls row. It returns false, with
	// nothing written, when the call is not a dispatched call of an
	// existing session of the call's own user (a call outside any session,
	// or one naming another user's session); ErrLimit, with nothing
	// written, past MaxSessionJobs; ErrExists, with nothing written, when
	// the job (device, boot, job id) is already registered for another
	// call. A repeat updates the row, but an empty Description or
	// OutputPath keeps the stored one. The caller registers only calls that
	// passed session admission.
	RegisterSessionJob(ctx context.Context, j SessionJob) (bool, error)
	// GetSessionRecords returns, fenced, a page of the session's records:
	// the sections r selects, in the order cwds, files, jobs, background
	// tasks, grants, each in its key order, starting after r.After, and
	// stopping before the row that would bring the page's size
	// (RecordsSize) over r.MaxBytes (1..MaxTranscriptPageBytes), but with
	// at least one row when one exists. Next is the cursor of the next
	// page, nil when the page holds the rest. Jobs carry the state of the
	// matching jobs row; grants are the unexpired ones at f.Now.
	GetSessionRecords(ctx context.Context, f Fence, r RecordsRequest) (SessionRecords, error)
	// PurgeSessionJobs deletes at most limit (1..MaxPageSize) session jobs
	// whose job has ended, whose notification is persisted, and whose
	// notification was persisted at or before before. It returns how many.
	PurgeSessionJobs(ctx context.Context, before int64, limit int) (int, error)

	// Events.

	// AppendEvents stores events with the next sequence numbers of the
	// session and returns, per event, its seq, or 0 when it was dropped.
	// The control plane's own events pass pub nil. Events a worker
	// publishes pass an EventPublish: its fence is checked first, in the
	// same transaction, and its batch sequence follows the PublishEvents
	// rule. Per (session, epoch) the store keeps the last batch sequence
	// consumed: a batch at or below it is Deduplicated (nothing written); one
	// beyond it plus one is ErrInvalid and consumes nothing; one exactly
	// next is consumed, and if any of its events fails validation (an
	// over-cap field, or a kind only the control plane records,
	// ControlPlaneOnlyKind) the whole batch is recorded as
	// dropped (Session.DroppedEventBatches + 1) and the result reports
	// Dropped with a DropReason matching ErrInvalid; the error is nil, so
	// the consumption commits. A worker batch with no events (its events
	// were all live-only, which never enter the store) consumes its batch
	// sequence and stores nothing. Fenced and store errors consume nothing. A
	// payload over MaxEventPayloadBytes is stored without its bytes
	// (Truncated, PayloadBytes keeps the size). When the session's stored
	// payload bytes would pass maxViewBytes, the session is marked
	// ViewTruncated, non-boundary events are dropped and boundary events are
	// stored without payload. Past MaxSessionEvents stored events the
	// oldest, of any kind, are deleted (their payload bytes leave the view
	// count; ViewTruncated stays). At most MaxEventBatch events per call.
	AppendEvents(ctx context.Context, sessionID string, pub *EventPublish, events []EventInput, maxViewBytes int64) (EventsAppended, error)
	// ReadEvents returns the session's events with seq > f.AfterSeq in seq
	// order, at most f.Limit and stopping before the event that would bring
	// the payload bytes over f.MaxBytes, but at least one when one exists.
	// Evicted events are skipped: the page then starts at the oldest kept
	// event and reports Missed. ErrNotFound if the session is absent.
	ReadEvents(ctx context.Context, f EventFilter) (EventPage, error)

	// Approvals and grants.

	// RequestApproval records a pending approval, fenced. Dedup: a pending
	// approval with the same (tool, device, match key) is returned with
	// Created false. ErrStopped on a stopped session, ErrBroken on a
	// broken one. With MaxPendingApprovals pending already, the oldest
	// pending one is set to expired in the same transaction and returned as
	// Expired. Resolved approvals past the newest KeptResolvedApprovals are
	// deleted.
	RequestApproval(ctx context.Context, f Fence, a Approval) (ApprovalRequested, error)
	// ResolveApproval approves or denies a pending approval at r.At.
	// Approving, in one transaction: the approval becomes approved, a grant
	// with the approval's key and r.GrantExpiresAt is written (the grant
	// expiring first is dropped past MaxSessionGrants), and r.Input is
	// submitted as kind approval under client message id
	// "approval:<approval id>" with SubmitInput's checks and state change.
	// Denying: the approval becomes denied. A pending approval whose
	// ExpiresAt ≤ r.At is set to expired instead and returned with Changed
	// false. An approval no longer pending is returned unchanged with
	// Changed false, whatever r asks. ErrStopped on a stopped session,
	// ErrLimit when the input queue is full; neither writes anything.
	// ErrNotFound if absent.
	ResolveApproval(ctx context.Context, r ApprovalResolution) (ApprovalResolved, error)
	// GetApproval returns the approval, or ErrNotFound.
	GetApproval(ctx context.Context, approvalID string) (Approval, error)
	// ListApprovals returns at most f.Limit approvals ordered by
	// (CreatedAt, ApprovalID), stopping before the one that would bring the
	// page's size (ApprovalSize) over f.MaxBytes, but with at least one.
	ListApprovals(ctx context.Context, f ApprovalFilter) ([]Approval, error)
	// ConsumeGrant consumes, for the tool call toolUseID, one unconsumed
	// grant of the session matching k that has not expired at f.Now, the
	// one expiring first, and returns true; false when there is none.
	// Replay: when toolUseID already consumed a grant of the session, the
	// answer is true if that grant's key is k and it has not expired at
	// f.Now, false otherwise, and nothing more is consumed. Replay records
	// are kept for MaxSessionGrants consumptions, expired ones dropped
	// first; a resend of a call older than that within the grant lifetime
	// (33 approvals of one session in its 10 minutes) may consume another
	// matching grant. Fenced.
	// ErrStopped on a stopped session, ErrBroken on a broken one. Of
	// concurrent consumers of one grant exactly one gets it.
	ConsumeGrant(ctx context.Context, f Fence, k GrantKey, toolUseID string) (bool, error)
	// ExpireApprovals sets at most limit (1..MaxPageSize) pending
	// approvals with ExpiresAt ≤ now to expired, earliest first, and
	// returns them; it also deletes every grant, consumed or not, with
	// ExpiresAt ≤ now (at most 2 × MaxSessionGrants per session).
	ExpireApprovals(ctx context.Context, now int64, limit int) ([]Approval, error)
}
