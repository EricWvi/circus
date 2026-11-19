// Package sessions is the control plane's session service on the worker
// link: it serves the session operations of docs/wire.md section 7 against
// the store, keeps leases bound to live connections, and schedules sessions
// onto workers. The user-facing half (SessionService, WatchSession) lives in
// webapi and publishes through the event Bus of this package.
//
// # Operations
//
// workerlink dispatches every Attach, SessionRequest, ResumeLeases and
// ListDevices of a connection to its Conn. Writes run inline in the
// connection's receive loop, in receive order, each in one store
// transaction bounded by StoreTimeout (STORE_UNAVAILABLE past it):
// ReleaseLease, AppendTranscript, TakeInputs, ReportTurn, PatchSessionState,
// PublishEvents, RequestApproval, ConsumeGrant, TranscriptRepaired,
// ResumeLeases, Attach and the lease write of AcquireLease. Their reply is
// sent from the receive loop too, so a connection holds at most one inline
// reply. Reads (LoadTranscript, ListTranscriptKeys, GetSessionState,
// ListDevices and the Lease reply of AcquireLease) run in the operation's
// goroutine under one of ReadConcurrency global permits, held while the
// reply is built and sent. Every fenced operation checks (session, epoch,
// worker, boot, connection) in its own transaction; a mismatch is Fenced
// with the current epoch and writes nothing.
//
// After a commit that appended durable events (PublishEvents, a turn
// boundary of ReportTurn, a lease loss that interrupted a turn) the
// service calls Bus.Durable with the highest sequence written; live-only
// events of PublishEvents go to Bus.Ephemeral and never reach the store.
// Workers may not publish the kinds only the control plane records
// (store.ControlPlaneOnlyKind). A stop by the transcript quota records its
// view event with AppendStopEvent, the helper StopSession uses too, in the
// transaction of the stop. The call router registers a session call's
// background job with its session when the result reports one (or, for a
// call settled lost without its result, at the loss, at a late result, or
// from the job's events), only for calls admitted under the session's
// lease, and wakes the scheduler (JOB) when it is registered ended or ends
// (a job's end only for a session of the reporting daemon's user).
//
// # Leases
//
// A lease is bound to the connection that acquired or last resumed it. The
// lease upkeep runs on its own goroutine and timer, never behind the
// scheduler: every ExtendInterval the leases bound to live connections are
// extended to now + LeaseLifetime; every SweepInterval expired ones are
// released (an open turn becomes interrupted). A lease is extended while
// its AcquireLease operation is open, and at once when its Lease reply
// reaches the link; once the operation ended without that (unwatched, send
// failed, link closed), the lease is no longer extended and expires, unless
// it is resumed. Leases bound to a connection of a boot that is no longer
// current are not extended. Start extends every held
// lease once, so none expires sooner than one lifetime after a
// control-plane start.
//
// Worker boot ids are UUIDv7s, ordered by timestamp, then by their other
// bytes. Each connection's boot becomes the worker's current one in the
// store; in memory, the current boot follows the order of those boot
// transactions (a sequence taken inside each), not the order in which
// connections register, so a connection of the store's current boot is
// never stale. Each connection whose boot is still current releases every
// lease of the worker's other boots (so a release that failed is completed
// by the next connect; a superseded boot releases nothing). A boot older
// than the current one is refused at Hello (CheckBoot, again atomically in
// Connect), releasing nothing, while the current boot is alive: connected
// here, connected or disconnected within LeaseLifetime (the last connection
// of the current boot records its disconnect), or holding an unexpired
// lease (store.HasLiveLease). Otherwise it is accepted, since boot
// timestamps come from the workers' clocks. Connections of any boot but the
// worker's current one are stale and get no Offers.
//
// Each AcquireLease takes a token; its pending and abandoned marks belong to
// that token, so the end of an earlier operation never touches a later
// acquisition. Each connection remembers its last 16 AcquireLeases
// (request id, session, token), so an Unwatch that arrives after the slot
// was released still abandons the lease (best effort, wire.md).
//
// # Scheduling
//
// Service implements Waker. A session with pending work (the store's
// queued state) and no live lease is offered to its pinned worker, or else
// to the user's attached, non-stale connection that is not full (leases
// plus Offers below Attach.max_sessions) and has the fewest; with none it
// stays queued until a connection attaches or goes from full to not full,
// which rescans the user's queued sessions. A rescan is incremental: it
// stops when no connection of the user has room, and runs a store
// transaction only for sessions that need and can get an Offer. An Offer
// stays reserved for ScheduleInterval; the session_schedule fallback then
// withdraws it and rescans every attached user. On Attach, Offers held by
// another connection of the same worker and boot move to the attaching one.
// AcquireLease needs a current Offer to the connection unless this worker
// and boot hold the lease.
//
// For a held session, Wake adds Nudge reasons to the session's pending set
// and the scheduler nudges the holder's connection (with the stored
// interrupt level); reasons stay pending until the holder acts (TakeInputs
// clears INPUT and APPROVAL, GetSessionState with jobs clears JOB,
// ReportTurn ENDED clears INTERRUPT, a delivered Lease reply clears what it
// carries: JOB, INTERRUPT and STOP; otherwise STOP stays until the lease
// ends) and are re-sent after Attach, after each successful ResumeLeases
// outcome and after a delivered Lease reply. The first resumption after a
// restart (a resumption of a lease this process has not bound itself,
// whatever Wakes created its entry before) adds INPUT, JOB and, for a
// running turn at or below the stored level, INTERRUPT. Hints for a connection without an Attach stream wait in
// its outbox for its Attach. A reason raised by a Wake the operation did
// not see survives the operation's clearing (each reason carries the Wake
// generation that set it).
//
// # Bounds and worst-case memory
//
// With C worker connections (workerlink.Config.MaxConnections, default 8)
// and M the link's message limit (default 64 MiB), beyond what workerlink
// already counts (its pools, sockets, push permits and one borrowed inbound
// message per connection):
//
//   - reads: ReadConcurrency (2) replies being built at once, each at most
//     about 2·M (a transcript page with one entry of up to M − 64 KiB, held
//     once as stored bytes and once as the reply's string; a Lease or full
//     SessionState at every cap is about 25.5 MiB encoded, about twice that
//     while built): 4·M = 256 MiB. A read reply the send pool refuses gives
//     its read permit back: one of at most 256 KiB is retried without a
//     permit (at most one per open operation, counted with workerlink's
//     small updates), a larger one under one of workerlink's four push
//     permits (counted there), or, with none free, dropped and rebuilt
//     under a push permit taken once one has freed (that wait holds no
//     permit; the push permit is then kept through the wait for a read
//     permit, the rebuild and its send, all within workerlink's 10-second
//     send deadline started at the drop, and no busy retry holds a read
//     permit);
//   - inline operations: one in progress per connection, at most the
//     converted copy of its request (an AppendTranscript or PublishEvents
//     of up to M), plus the connection's queue of inline replies not yet
//     handed to the link, at most 16 MiB encoded (one larger reply alone),
//     about twice that decoded: C·(M + 32 MiB) = 768 MiB;
//   - the session table: at most MaxTracked (8192) entries of about 0.5 KiB
//     (4 MiB); the Wake queue at most MaxWakeQueue (4096) ids (1 MiB); a
//     connection's hint queue at most one hint per session of its user (the
//     store's per-user cap, 1000), so all queues together at most C·1000
//     hints (about 2 MiB); one lease extension round copies at most
//     MaxTracked keys (about 4 MiB); each connection remembers 16 recent
//     AcquireLeases (about 4 KiB); the current-boot map holds one entry per
//     worker identity that connected since start;
//   - goroutines: the scheduler and the upkeep loop, one hint sender per
//     attached connection, at most 4 job-stop calls after a
//     transcript-quota stop, and one per open read operation (at most 64
//     per connection, all but ReadConcurrency waiting for a permit or a
//     send).
//
// At the defaults that is about 1,030 MiB. Live-only events handed to the
// Bus (at most 128 per batch, each payload at most 256 KiB) are charged to
// each watcher's queue (bus.go) and counted with the watch streams; a
// payload nobody watches is dropped at once. These are ownership bounds,
// not RSS.
package sessions
