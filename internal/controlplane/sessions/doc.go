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
// A lease stays valid while its holder is disconnected: the reconnect
// window, in which the same process resumes it. The user is told at once
// instead (Session.worker_disconnected_at_unix_ms). When the last
// connection of a worker's boot closes (no other connection of that worker
// and boot is registered), every tracked lease of that boot not bound to a
// live connection (those bound to the closing connection, and those left
// unbound by an earlier connection of the boot) is marked with the close
// time on its entry, announced by an ephemeral KIND_WORKER event. The mark
// clears, announced the same way ("0"), when the lease is bound to a live
// connection again (ResumeLeases, AcquireLease, an evaluation that finds it
// bound to one) and when it ends for any reason (unbind: release, expiry,
// revocation, a boot change, a takeover, a stopped session's release); a
// later close of the boot's last connection moves it. A lease this process
// has seen no connection of its holder's boot close for (after a restart,
// before the boot reconnects) is not marked: unknown reads 0, and
// a restarted control plane cannot tell a worker about to reconnect from
// one that is gone. A marked lease is unbound, and unbound leases were
// never extended, so it expires 40 to 60 seconds (LeaseLifetime minus up
// to one ExtendInterval) after the mark and the sweep ends it within one
// SweepInterval more; the fallback evaluates a lease still marked
// LeaseLifetime + ScheduleInterval after its mark (at most
// MaxReasonRechecks per pass), so one that ended unseen clears too.
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
// another connection of the same worker and boot move to the attaching one,
// if it may run the session. AcquireLease needs a current Offer to the
// connection unless this worker and boot hold the lease.
//
// A local session (Session.Local) is always pinned. A connection attached
// with local_only runs only local sessions pinned to its worker: Offers
// (offer, Attach's move) skip it for any other session, and its
// AcquireLease and ResumeLeases refuse one in the transaction that would
// bind the lease. The store refuses a pinned session's lease to another
// worker.
//
// # Providers and queue reasons
//
// A session with a provider (Session.ProviderID) is offered only to the
// provider's workers, and only while fewer than its max_sessions sessions
// hold live leases; every evaluation reads the provider in its own store
// read (gate), and AcquireLease checks deletion, assignment, the cap and
// whether the secrets open again in its transaction (PROVIDER_UNAVAILABLE,
// PROVIDER_FULL; an Offer the provider refused is withdrawn). Local login
// is not gated.
//
// An evaluation that finds a queued session it cannot offer records why
// on the session's entry (QueueReason): a deleted provider, unreadable
// secrets, a pinned worker the provider is not assigned to, a full
// provider, or no worker that may run it online (UNSPECIFIED when one is
// online but busy). An Offer, a lease or leaving the queue clears it, and
// every change is announced with an ephemeral KIND_QUEUE event. An entry
// holding only a reason stays until the reason clears; the fallback
// re-evaluates a reason no event re-derived for ReasonRecheck (at most
// MaxReasonRechecks per pass), so one whose session left the queue unseen
// clears too; when the table is full such entries give way to leases,
// silently (the reason did not clear).
//
// An evaluation reads the store without s.mu (SettleSession, then the
// provider). Before acting it checks, under s.mu, that nothing changed the
// lease in memory meanwhile (the entry's epoch is above the one read; the
// entry was bound to a live connection or unbound after the read began,
// since a resumption and a release keep the epoch; the entry was
// forgotten, replaced or created (an AcquireLease or ResumeLeases reserves
// one); or, with no entry before or after, any lease loss was
// applied: the session is evaluated again, at once up to MaxStaleRetries
// times in a row and then at the next fallback pass, so a read of a lease
// that ended meanwhile never tracks a holder for it) and
// that none is in flight (the entry is reserved: the evaluation is
// deferred to the end of the reservation), so a stale read never revokes
// or re-offers a lease just taken, never binds again (and later revokes on
// its holder) one just released, and never unbinds one just resumed on a
// new connection.
//
// Wake-ups: a lease acquired under a provider that ends (release, revoke,
// expiry, takeover) evaluates the sessions waiting as PROVIDER_FULL for
// that provider; ProviderChanged (assignment, cap, secrets, deletion)
// evaluates every queued session naming it, read from the store, and
// rescans the user. A provider counter (provEpoch) moves with both, so an
// evaluation whose provider read raced one is repeated. A provider change
// never revokes a lease; changing a session's provider releases its lease
// in the store, and the Wake that follows revokes it on the holder.
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
// # Logging
//
// The service logs through Config.Logger with log/slog: short fixed
// English messages, snake_case keys (session_id, worker_id, boot_id,
// conn_id, epoch, ...). At info, the low-frequency events an operator
// follows a session by: "lease acquired" (with fresh, reattached and the
// lease's provider_id); "lease ended"; "leases resumed" (one line per
// ResumeLeases call, refused and failed ones included: result ok, rejected
// or failed, and counts per outcome); "turn started" and "turn ended" (a
// duplicate report logs nothing); "transcript repaired" (only when the
// session was broken); "session stopped" (reason transcript_full); "queue
// reason changed" (the KIND_QUEUE name); "worker disconnect marked" and
// "worker disconnect cleared" (only on a change); "worker attached",
// "worker connection closed" (last_of_boot, marked) and "old boot leases
// released". "session broken" is a warning: an ENDED report with an
// incomplete transcript that moved the session to broken. A queue reason
// is logged on every change of its entry, so an entry dropped from a full
// table (room) logs its reason again when an evaluation tracks it anew,
// although the reason itself never went away.
//
// "lease ended" is written once per committed end of a lease, from the
// loss the store returned (holder, epoch, interrupted), by lost (which
// returns it for its caller to write) and by the web API's own
// lease-ending transactions (LogLeaseEnded). Its reason comes from the operation that committed the
// end: released (the holder's ReleaseLease; a repeated release ended
// nothing and logs nothing), expired (the sweep, an evaluation's settle, or
// an acquisition that replaced an expired lease), taken_over (the live
// holder's own fresh acquisition took a new epoch), stopped and broken (a
// fresh acquisition by the holder of a stopped or broken session released
// it), boot_changed (a newer boot of the worker released it),
// provider_changed (UpdateSession) and recovered (RecoverSession). A
// binding the scheduler only infers is gone (an evaluation that finds no
// live lease or another holder, a deleted session, an acquisition or
// resumption replacing a different tracked holder) is a debug "lease
// binding dropped" with a cause, never a second "lease ended". A lease row
// deleted with its session (an expired, unswept lease of a stopped
// session) is reported by no store call and logs no "lease ended".
//
// At debug, the high-frequency events: "offer queued" and "offer
// withdrawn" (the scheduler's decision, with the connection's load and a
// cause; an Offer queued may still never reach the worker, when its
// connection closes or the Offer moves first), "lease binding dropped",
// "lease extension round", "nudge sent" (after the send), "stale read
// retried" and "stale read parked", "inputs taken", "transcript appended",
// "events published", and the worker's account email ("worker account").
// Whether the debug records collected under s.mu (Offers, dropped
// bindings, stale reads) are built is decided once, by asking the logger
// for debug with context.Background() in New (s.dbg; the server's level is
// fixed at start), so no handler method is asked under s.mu. With a logger
// whose level changes later, a debug level off at New builds none of them
// ever, and one on at New builds their attributes even after debug was
// turned off; the handler's own check when they are written, with the
// context they are written with, still filters them. The debug sites
// outside s.mu check the level, for the context they log with, before
// building attributes.
//
// Records carry only ids (session, user, project, device, worker, boot,
// connection, provider, input, approval), epochs, turn and batch numbers,
// enum names (reasons, causes, outcomes, origins, priorities, decisions,
// results), counts, byte sizes, flags, Unix millisecond times, the worker's
// SDK and Claude versions and max_sessions, and, at debug only, the
// worker's account email. Never logged, at any level: message and input
// text, transcript entries or their JSON, tool inputs and outputs, approval
// text, event payloads, usage JSON, provider secrets (key, OAuth token),
// the provider's base URL, custom environment variables and their names,
// the launch environment, and free-form session settings (model,
// permission mode).
//
// No logger method runs while s.mu is held: code under s.mu collects its
// records in s.pend (later), and unlock, which every critical section ends
// with, detaches them, releases s.mu and then writes them, in the order
// they were collected (unlockKeep hands them to the caller instead, which
// writes them later). Records of concurrent critical sections may
// interleave in another order than the sections ran.
//
// A committed state change is applied and announced before its record is
// written: the binding forgotten, revocations queued, the Bus told, the
// session woken or nudged, the operation's reservation ended. Every loss
// of a committed page (the expiry sweep, a boot's release of other boots'
// leases) is applied before any of the page's records is written, and a
// successful Connect writes its boot's release only once it has registered
// the connection and its boot and withdrawn the Offers of stale
// connections. A Connect that returns an error (a later page's release
// failed, or the service is closed) registers nothing and writes the
// records of the losses it committed anyway. The web API writes its
// records after its Bus calls, Wakes and job stops.
//
// That, and that no record is written under s.mu, is all the service
// guarantees about a slow sink. A goroutine that writes a record waits for
// the handler, so a handler that blocks delays that goroutine's own next
// steps: a link's next request and its reply, the scheduler's next
// evaluation, a hint sender's next hint, and whatever waits for that
// goroutine (Close waits for its connection's sender before it records the
// disconnect). A handler that panics loses the rest of the batch being
// written and unwinds the writing goroutine; s.mu is released already, and
// lost releases it in a deferred call should a callback panic. A panic in
// a store call or a callback during an expiry sweep or a boot's release
// also drops the records of the losses that sweep or release had already
// applied and not yet written (the losses themselves stand: committed and
// applied). A Connect whose release records panic after it registered the
// connection closes that connection, as its link would, and panics on
// with the same value.
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
//   - the session table: at most MaxTracked (8192) entries, queue reasons
//     and disconnect marks included (a mark is a field of its lease's
//     entry, never an entry of its own). An entry is a 288-byte struct
//     (its own size class), about
//     32 bytes of map slot, and up to eight ids of at most 128 bytes each
//     (the session id key, user, pin, the holder's worker, boot,
//     connection and provider, the reason's provider): 288 + 32 + 8·128 =
//     1,344 bytes, so 8192 · 1,344 B = 10.5 MiB at every id's cap (about
//     5.5 MiB with 36-byte UUIDs: 288 + 32 + 8·48 = 704 B). The Wake queue
//     holds at most MaxWakeQueue (4096) ids (1 MiB); the provider scans at
//     most MaxWakeQueue provider and user ids, each pair about 2·16 bytes
//     of string headers, 8 of slot and 2·128 of ids: 4096 · 296 B =
//     1.2 MiB; the stale-read counts and the sessions parked for the
//     fallback at most MaxWakeQueue ids each (about 0.6 MiB each at
//     128-byte ids); a
//     connection's hint queue at most one hint per session of its user (the
//     store's per-user cap, 1000), so all queues together at most C·1000
//     hints (about 2 MiB); one lease extension round copies at most
//     MaxTracked keys (about 4 MiB); each connection remembers 16 recent
//     AcquireLeases (about 4 KiB); the current-boot map holds one entry per
//     worker identity that connected since start;
//   - log records: one critical section's batch (s.pend) holds at most one
//     record per tracked entry for a pass over the table (a connection's
//     close withdraws an Offer or marks a lease per entry; Connect and the
//     fallback withdraw Offers) plus a few per session it acts on (at most
//     64 in a ResumeLeases), about 8,700 records. A record is 48 bytes in
//     the batch (up to about 1.15 times that with the slice's spare
//     capacity) and its own attribute array of at most five 40-byte
//     attributes (208 bytes allocated): about 264 bytes, 2.2 MiB a batch,
//     plus the ids it keeps alive once their entry is gone (at most three
//     of 128 bytes a record, 3.2 MiB). unlock detaches the batch and holds
//     it while the handler writes it, and unlockKeep's caller holds it
//     until its follow-ups are done (Close's sender stop and disconnect
//     write). A goroutine can hold more than one meanwhile, the extra ones
//     small: an evaluation holds its loss's records while it writes
//     its own section's, an acquisition the loss's while it binds, the
//     expiry sweep a page's (at most 256 losses) until all are applied,
//     and Connect a boot release's, every page of it, until it has
//     registered. A loss is at most two records (the lease end, a cleared
//     disconnect mark), about 1.3 KiB with the ids they keep alive, so a
//     page is about 0.33 MiB. The release goes on while pages come back
//     full, and the code bounds their number only indirectly: the first
//     pass releases what the old boots held, at most one lease per session
//     of the worker's user (the store's per-user cap, 1000: four pages,
//     1.3 MiB); a further full page needs 256 leases the old boot acquired
//     again between two page transactions, each through an Offer, which
//     an old connection gets only while its leases and Offers are below its
//     max_sessions (at most 64), so only an old process with at least four
//     full connections still taking Offers between pages, which a
//     replaced worker does not do for long. The
//     large batches come from the upkeep loop (fallback) and the
//     connections' link goroutines (Connect, Close), others hold a few
//     records (a sender's detach at most 64): (C + 1) · 5.4 MiB = 49 MiB;
//   - goroutines: the scheduler and the upkeep loop, one hint sender per
//     attached connection, at most 4 job-stop calls after a
//     transcript-quota stop, and one per open read operation (at most 64
//     per connection, all but ReadConcurrency waiting for a permit or a
//     send).
//
// At the defaults that is about 1,093 MiB. Live-only events handed to the
// Bus (at most 128 per batch, each payload at most 256 KiB; KIND_QUEUE and
// KIND_WORKER announcements under 64 bytes each) are charged to
// each watcher's queue (bus.go) and counted with the watch streams; a
// payload nobody watches is dropped at once. These are ownership bounds,
// not RSS.
package sessions
