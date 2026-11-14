# Call routing

Durable state uses `store.CallState`; notifications use
`circusv1.CallUpdate`. There is no durable request, result, output path, arbitrary
daemon error text, or event outbox. OutcomeReason and FaultCode contain only
allowlisted codes or fixed phrases of at most 256 bytes.

## Named invariants

Comments in this package refer to these rules by name.

- **Boot fencing.** Device replies (ack, result, status) are fenced by user,
  device and dispatch boot, not by connection generation; a superseded
  connection of the dispatch boot still settles calls like the live one.
- **Sync-first rule.** Per-call commands (CallQuery, CancelCall, first
  DispatchCall) are never sent on an unready route by anything but that
  route's sync. A Query for such a route is deferred to the sync and answered
  by its resend or query reply, never sent ahead of the resend.
- **Disconnect-record retry.** A disconnect whose store write failed stays in
  memory and is persisted, with its grace timer, by the subscription sweep,
  any Submit or any watchdog for the device, so accepted calls always get a
  grace timer.
- **Restart offline accounting.** A device whose row still says connected but
  has no live route was left so by a previous process; it counts as offline
  since max(LastDisconnectedAt, this process's start). See "Restart offline
  accounting" under Deviations.

## State × event transition table

`A/D/R/U/C/O/X/J/L` mean accepted/dispatched/running/unknown/completed/offline/
cancelled/rejected/lost. `active` means D/R/U. A same-state action uses PatchCall,
not TransitionCall. Device replies (ack, result, status) are fenced by user,
device and dispatch boot, NOT by connection generation (boot fencing): a superseded
connection of the dispatch boot is still authoritative (a graceful close still
delivers its final results).
A mismatching reply changes nothing. Job events follow the current generation. Each row below is a public-interface test obligation;
set-valued rows expand into one case per state/event. Every unlisted pair is a
no-op, except invalid input returns an error or rejection without mutation.

| State | Event / guard | Next | Notification and effects |
| --- | --- | --- | --- |
| absent | valid submit, online or synchronizing/in grace | A | Accepted; reserve pending content and subscription |
| absent | submit, never connected or grace expired | O | Offline with bounded user-owned online device names; no dispatch |
| absent | invalid/mismatching/over-budget submit | absent or J | Rejected INVALID/CALL_MISMATCH/OVERLOADED; no dispatch; invalid identities cannot create a row |
| any existing | duplicate submit, same owner/device/hash | same | Subscribe/query existing outcome; no new execution |
| any existing | duplicate submit, different owner | same | No information disclosure; store.ErrNotFound |
| any existing | duplicate submit, different device/hash | same | Rejected CALL_MISMATCH for this submission only |
| L | equal resubmit, DispatchedBootID empty | A | Guarded lost → accepted; reset SubmittedAt and every previous attempt patch column; reserve content anew |
| L | equal resubmit, DispatchedBootID set | L | Lost; never send DispatchCall |
| A | dispatch, ready current route | D | In one Tx read CurrentBootID, commit state/boot/watchdog; only then write DispatchCall with that boot |
| A | cancel before dispatch commit | X | Cancelled; release pending; never send dispatch |
| A | grace expires | O | Offline; release pending |
| A | control-plane startup, content absent | L | Lost (control plane restarted before dispatch); safe equal resubmit |
| D | first Send returns ErrBusy, proven never enqueued | J | OVERLOADED; keep dispatch boot evidence, release pending, cancel watchdog |
| D | first Send returns ErrTooLarge, proven never enqueued | J | INVALID; same effects as ErrBusy. ErrNoRoute or a closed link keep D: sync resends or the watchdog/boot change settles |
| D | ack/status RUNNING | R | Release pending and record first ack; external current state is Dispatched |
| R | duplicate ack/status RUNNING | R | No regression; preserve first ack |
| U | same-boot ack/status RUNNING | R | Dispatched; rearm watchdog |
| active | result or ack/status COMPLETED with result | C | Result implies ack; release pending and cancel watchdog; no output persisted |
| D | ack rejected INVALID/CALL_MISMATCH | J | Matching rejection; release pending and cancel watchdog |
| D | ack rejected BOOT_MISMATCH/EXPIRED | L | Lost; never retry on a new boot |
| D | ack rejected SHUTTING_DOWN | J | DEVICE_SHUTTING_DOWN; release pending and cancel watchdog; deduplication before permanent shutdown plus the dispatch-age bound proves no copy executed |
| active | same-boot query LOST | L | Lost; release pending and cancel watchdog |
| active | cancel | same | Commit first cancel intent; send CancelCall only on a ready route, else the sync replays it after the resend/query; daemon result wins |
| active/C | Query, route registered but unsynchronized | same | Waiter deferred to that route's sync: answered by its resend/query reply, never sent ahead of the resend (sync-first rule) |
| A/active | disconnect | same | SetDisconnected and device_grace timer atomically; no tool cancellation |
| D/R | grace expires | U | Unknown with call ID; preserve recovery/watchdog; discard retained request once retry horizon expires |
| U | grace expires again | U | No regression or duplicate terminal update |
| active | reconnect, same boot | same | Resend only unacked retained requests within ResendHorizon, rechecked after every sync-capacity wait; otherwise query; replay recorded cancel afterward |
| active | reconnect, different boot | L | LoseCalls; notify returned IDs and cancel their watchdogs in the same Tx; LoseJobs old boots |
| A | reconnect, either boot | A then D | After recovery commands and ListJobs admission, activate route and dispatch by SubmittedAt then call ID |
| active | watchdog due | same | Claim completion first; query only on a ready route of the dispatch boot; persist next recovery watchdog; never infer success from timeout |
| active | disconnect exceeds result-retention window | L | The daemon no longer keeps the result (30-minute retention), measured from the recorded disconnect (or this process's start: restart offline accounting); never resend |
| C/O/X/J | cancel, disconnect, reconnect, late ack/result | same | Preserve terminal metadata; no new dispatch |
| L | any event except permitted resubmit | L | Preserve loss and dispatch evidence |
| C | Query, result not locally retained | C | Query daemon; expired/LOST returns ErrResultUnavailable, unreachable returns transient ErrDeviceOffline; preserve metadata |
| any | reply for another user, device or dispatch boot, or duplicate terminal event | same | No mutation, no extra pending entry or timer |
| active | result/ack/status on a superseded connection of the dispatch boot | as live | Settles like the live connection (boot fencing) |
| any | timer claim cancelled/reset/reclaimed | same | CompleteTimer false: no handler effects |

Store write failure on submission prevents dispatch. CODE_STORE_UNAVAILABLE is
returned only after establishing that no new attempt persisted; an ambiguous
crash/commit returns a Go error instead. If result metadata
cannot commit, publish the actual result anyway, release pending content and mark
readiness unhealthy. Keep the existing durable watchdog for later query repair;
do not pretend completion was persisted. Transient delivery is not a new durable
state transition. Duplicate terminal replies may supply content to explicit query
waiters without regressing metadata. A daemon reply (ack or status) that cannot
be applied still answers the waiting queries of that call whose CallQuery went
out on the reply's connection, instead of leaving them waiting for a reply that
already came (found by TestRandomSequences, pinned by
TestQueryAnsweredWhenReplyCannotPersist): after a store write or read failure
an active call falls back to its durable state and a completed one reports
ErrStoreUnavailable; after an invalid reply (the link closes) they are
answered as when the daemon cannot be asked. Waiters on other connections
keep waiting for their own reply.

## Connection ownership and reconnect

The gateway authenticates `/v1/link/daemon` before `wire.AcceptWebSocket`, reads
raw Hello using the bounded wire decoder, writes raw Welcome/Reject, uses
`wire.FillWelcome`, then creates `wire.New` with `DaemonControlSide`. Hello has
a deadline; unsupported versions reject before any business traffic. All daemon
links share one budget; worker links have their own (package workerlink). Only daemon principals pass the auth seam. Recheck
polling closes a revoked connection within interval plus check timeout; future
upgrades receive 403. No bearer token survives authentication in gateway state.

Register atomically installs an unready generation. The gateway sends SUPERSEDED
and gracefully shuts down the previous local socket. It keeps Receive draining
through graceful EOF/Shutdown, including final results; only after drain does it
notify disconnect. A new generation's OnConnected starts only after EVERY
older, not yet released local peer of the same device finished draining and
disconnecting, not just its direct predecessor: a predecessor that is itself
superseded stops waiting for its own, so a one-level wait is not transitive.
The gateway registers under its own mutex, numbering
registrations, so "older" matches registry order, and waits for them in that
order. Each drain is bounded by its shutdown deadline and MaxConsumerStall and
the chain by MaxConnections. So a restarted daemon's new boot cannot lose a
call whose final result is still queued on an old link. Operational cost: when
the old socket is half-open, the reconnect's sync starts only after its
shutdown deadline (HeartbeatTimeout, 15 s) plus its receive drain
(MaxConsumerStall, by default also 15 s): about 30 s at defaults during which
the device is connected but its route is not ready. Only the same device's
older connections are waited for; other devices are unaffected. Old generations cannot unregister or change durable connection state;
their call replies settle calls of their own boot like any other. Their
disconnect fails the queries sent on them. For the current generation,
OnDisconnected withdraws the exact route itself (under the admission lock)
before committing SetDisconnected and the grace timer, so no Submit dispatches
to a closed link inside the grace period; the gateway's later Unregister is a
no-op. It also fails that route's outstanding and deferred query waiters. If
the disconnect write fails, the router keeps it in memory (one small entry per
device until a write succeeds, so bounded by the number of distinct devices
whose write failed, at most the device table) and the subscription sweep, any
Submit and any watchdog for the device persist it with its grace timer (the
disconnect-record retry). The registry holds one entry per device and
overwrites it on replacement; the gateway handler's MaxConnections peer set
counts handshaking and superseded sockets too, so replacements cannot
accumulate closing sockets.

OnConnected takes the admission lock (released between items), and owns SetConnected, comparison of
the old boot, cancellation of device_grace, and reconciliation. New boot: the first
Tx performs SetConnected + LoseJobs + a bounded LoseCalls batch and cancels the
returned IDs' watchdogs; later transactions drain successive batches. Notify each
batch only after commit. Every reconnect drains old-boot calls, even when an
earlier interrupted sync already recorded the new boot. Same boot: enumerate D/R/U; an unacked request
still in pending can be resent only to the recorded boot. Without content, or
when now - DispatchedAt >= ResendHorizon (default 10 minutes), use CallQuery. Never move a dispatched lost call back
to accepted. Queries deferred while the route was unready are adopted by the
replay of their call; the rest (for example completed calls) get one CallQuery
each after Activate. Every resend, query and the ListJobs is a sync request:
at most syncLimit = min(MaxSyncRequests (default 16), MaxSyncReplyBytes /
MaxMessageBytes) are unanswered at once, because the daemon closes its link
when its send queue (256 messages or 2M bytes, M the message limit) cannot
take a reply (internal/daemon/link). Any reply may carry a result of up to M
bytes, so the byte budget (default 2M) charges each request its worst case:
with default limits two sync replies are outstanding, a resync of N calls takes
about N/2 round trips, each reply also costing one store transaction (for
example 1000 active calls at 50 ms RTT take tens of seconds). Until the sync
activates the route, new submissions to that device stay accepted, queries
are deferred to the sync and cancels are only recorded; this is a latency
cost, not a correctness one. Results and job events the daemon pushes outside the
sync share its queue; they are bounded by the daemon's own callback queues,
not by this budget, which is the remaining overflow risk (resolved by close
and resync). A sync waiting for capacity releases the lock AND its operation
slot, like a waiting Query, so the replies it waits for can always enter;
a daemon answering none within DeliveryTimeout ends the sync.
From the ListJobs reservation to Activate the lock is held: recorded cancels
are replayed after their call's resend/query, then ListJobs is sent and the
exact route activated, so a cancel recorded during the sync cannot fall
between replay and readiness. Then queued accepted calls dispatch in durable
submission order (each produces an ack like any submission; this is ordinary
traffic, not sync), and finally the remaining deferred queries are flushed
within the sync bound; a new Query on the now-ready route sends directly or
coalesces onto a deferred leader that the flush sends. Per-call
commands (CallQuery, CancelCall, first DispatchCall) are never sent on an
unready route by anything but its sync (sync-first rule). Inbound hooks remain runnable
throughout. A failed sync leaves the route unready
and ends the connection so reconnect can retry. Accepted calls survive boot
changes because they have never been dispatched.

Job tracking keeps one boot cursor per device, not an event log. Apply snapshots
with per-job LastSeq guards, discard stale events at/below the snapshot watermark,
and never regress newer live events or terminal jobs. A live sequence gap requests
one coalesced ListJobs. Events can create job metadata before a call result; never
persist JobEvent.output_path. Unfiltered, untruncated snapshots of the current
boot settle omissions (implemented decision 4 below); a snapshot with `truncated` set never
does. The router currently issues no filtered ListJobs.

## Commit, send, cancel and time

One router-wide admission lock orders initial dispatch, resend, cancel and
barriers (see Deviations). Nothing waits for a daemon response or a subscriber
while holding it; Link.Send only admits to the wire queue.
Dispatch reads the device's current boot in the SAME Tx as the guarded A → D,
sets its watchdog there, commits, then calls Sender.Deliver. The gateway validates
the exact generation/boot; Link.Send classifies every message itself.
Send success means queue admission. ListJobs and StopSessionJobs are barriers;
there is no custom priority table and Cancel is not Control.

Cancel racing before dispatch commit wins A → X. After commit it records intent,
even if the first dispatch has not yet reached Send; the lock enqueues dispatch
first. A known first-send ErrBusy or ErrTooLarge can produce a rejection
without any execution; an uncertain send failure cannot clear the boot or
permit cross-boot retry. Every DispatchCall carries the original committed
DispatchedAt, including resends. A valid SHUTTING_DOWN refusal of any copy maps
to DEVICE_SHUTTING_DOWN: the daemon deduplicates before its permanent shutdown
check, and its dispatch-age bound keeps every earlier execution's record within
the tombstone lifetime, so the refusal proves no copy executed.
On a failed resend retain the recovery path. Wire queue failure, budget exhaustion
and consumer stall close the link as appropriate and recover through reconnect.

Use injected clock time exclusively. Default grace is 30 seconds from observed
connection close, not the last heartbeat; default half-open detection is 15–20
seconds, so total observation is normally 45–50 seconds, plus any bounded local
consumer pause. Persist all grace/watchdog deadlines. A timer handler starts its
own Tx with CompleteTimer; false means abandon. True permits domain effects and
replacement timers in that Tx; notifications/network writes occur after commit.
Enrollment expiry is routed by server composition, outside calls.

Watchdog due time includes effective daemon timeout (default 2 minutes, capped
at 10 minutes for foreground execution), 5 seconds abandonment grace, configurable
WatchdogSlack (normally 30 seconds), AND DeliveryTimeout allowance for queued
transport work. Explicit-background invocation returns a job result promptly;
its lifetime is not a foreground timeout. Query RUNNING rearms; LOST settles loss.
Persist a follow-up watchdog before an outbound query, so a lost response cannot
remove the recovery path. Disconnected calls use the same watchdog for the
30-minute result-retention cutoff; grace expiration alone does not lose them.

A large Dispatch followed by a barrier can delay even an unrelated Cancel or
Query. Size DeliveryTimeout from maximum queued bytes / minimum supported effective
business throughput, plus scheduling and framing margin; 30 seconds alone is not
enough at the largest sizes. A cancel watchdog must include this allowance plus
5 seconds and slack. No finite transport bound exists without a throughput
assumption: an expired watchdog queries or disconnects/reconciles, never declares
execution cancelled or sends to another boot. Resend is allowed only with
retained pending content and elapsed dispatch time below ResendHorizon, both
when replay is selected and immediately before its write after any sync-capacity
wait. A late replay releases the content and sends CallQuery instead.
ResendHorizon is a policy and request-retention knob, not a safety bound;
configuration rejects only nonpositive values. The daemon's dispatch-age check
provides at-most-once safety without a throughput or live-queue-residence
assumption.

## Backpressure and memory

Pending has byte AND count bounds; ack/result/terminal outcome releases it.
For a new call, reserve decoded request and serialization scratch before cloning
or marshaling. An existing call, including an undispatched-Lost resubmit, instead
performs one transient deterministic marshal while serialized under the router
lock (at most 2x message size, outside the pending budget) to prove equality.
An equal existing call attaches without new pending ownership; a Lost resubmit
then passes the new-attempt scratch check before allocation. Direct Submit
enforces wire structural limits. Per-device sync cursors, public operations, query waiters, subscriptions and
subscription buffers have explicit limits. No terminal-result cache exists.
Next hands out an immutable update (one clone shared by every subscriber and
query waiter of the publish, each charged separately) and decrements charges;
exposing a buffered channel
would hide consumption from accounting. Overflow/stall detaches only that
subscriber, discards its queue, and reports ErrOverflow. Its execution continues;
it can Query again. Context cancellation and Close release subscription state. Context callbacks
are separately counted against MaxSubscriptions and joined by Close; waiting
queries release execution slots so inbound replies can acquire one.
SubscriberStall (normally 15 seconds) bounds undrained final updates as well.
The stall age starts when an empty queue receives its first unread update;
idle time with an empty queue never causes immediate overflow on a later result.
Configure a subscription byte limit large enough for a maximum decoded result,
not just its encoded size; otherwise that result intentionally forces Query.

Delivery has no retry queue: Sender.Deliver runs under the router lock and only
admits to the wire queue. Timers keep at most BatchSize claims and one wake hint; no in-memory heap
of all durable deadlines. Stats reports actual ownership, never fabricated zeroes.
Calls.Close releases volatile state but must preserve legitimate durable recovery
timers. Outcomes cancel watchdogs transactionally. Teardown assertions distinguish
legitimate nonterminal recovery timers from orphan timers on terminal calls.

Store enumeration uses required Limits (maximum 256) and exclusive keyset
cursors. Router walks use 64-row pages, including startup and diagnostics. Loss
notifications are processed in 64-ID transactions. Large grace-expiry sweeps
rearm an immediately due durable continuation; the runner drains due batches
without waiting for another clock tick. SQLite keyset indexes avoid full-table
sorts. Returned caller-owned updates and bounded operation scratch are additional
to retained pending/subscription charges. Owned clones copy string backing storage.

## At-most-once invariants and dispatch-age proof

| Case | Code point / invariant |
| --- | --- |
| duplicate submission | Service.Submit: global primary key + owner/device + deterministic request hash; no mutation on mismatch |
| first execution | dispatch transaction: read current boot + guarded A → D + watchdog, commit before Sender.Deliver |
| same-boot retransmission | OnConnected: original target_boot_id, content/hash and DispatchedAt; daemon deduplicates first, then rejects an unseen stale dispatch before shutdown or execution |
| new daemon boot | OnConnected: LoseCalls/LoseJobs and per-call timer cancellation; never dispatch old attempt to new boot; daemon independently rejects mismatched target |
| control-plane crash | New startup reconciliation: A → L; guarded L → A only with no recorded dispatch; dispatched attempts query after reconnect |
| crash between commit and write | durable boot remains; query may report lost although execution count is zero; never clear dispatch evidence |
| cancel race | Service.Cancel and the admission lock: A → X or durable intent after D; final authoritative result wins |
| tombstone expiry | retained store row blocks ID reuse; a copy old enough to find an expired tombstone is itself beyond MaxDispatchAge and is refused without a new record |
| power failure | existing SQLite FULL durability preserves committed dispatch evidence |
| shutdown refusal | OnCallAck: DEVICE_SHUTTING_DOWN for any valid dispatched refusal; deduplication precedes permanent shutdown, and the age bound proves no earlier copy executed |

The dispatch-age proof uses a per-connection time base rather than either peer's
wall clock. The control plane stamps `DispatchedAt` and Welcome's
`server_time_unix_ms` from the same injected clock. When the daemon reads
Welcome it records `welcomeMono` from its own injectable monotonic clock. For a
DispatchCall with original timestamp `D`, it computes
`age = since(welcomeMono) + (welcomeServerMs - D)`, and passes that age to the
dispatcher. A resend always carries the original `D`.

Let δ ≥ 0 be Welcome's transit time. The handshake deadline starts before the
control plane receives Hello, so δ < HandshakeTimeout (10 seconds by default).
On the daemon's monotonic clock, which is also the clock that ages tombstones,
at most `age + δ` passes between the earliest moment any copy of the dispatch
could arrive (control-plane time `D`) and this copy's arrival: Welcome left
`welcomeServerMs - D` after the dispatch, arrived δ later, and
`since(welcomeMono)` has passed on that clock since. `D` and Welcome's time are
whole milliseconds, so truncation can make `age` underestimate by less than
1 ms; the strict inequality below leaves a gap only when Welcome's transit is
within that millisecond of the full handshake timeout.
Any earlier copy that executed was received at control-plane time at or after
`DispatchedAt`, because every send follows the dispatch commit. Its record is
retained until at least TombstoneTTL after completion. The dispatcher first
deduplicates an existing record (including mismatch and retained-result expiry),
then accepts an unseen ID only when `age <= MaxDispatchAge`; a stale refusal
creates no record. Consequently two executions of one call on one boot would be
at most `MaxDispatchAge + δ` apart, and
`MaxDispatchAge + HandshakeTimeout < TombstoneTTL`. The second copy therefore
finds the first copy's record instead of executing. The age check precedes the
permanent shutdown check so a stale copy cannot turn a shutdown refusal into a
false proof of non-execution after a tombstone has expired.

This leaves two clock assumptions: the control-plane wall clock does not step
backwards between dispatch and Welcome (a forward step can cause an honest
EXPIRED/Lost outcome), and clock-rate drift remains at ppm level. A daemon
monotonic clock that pauses during suspend only makes `since(welcomeMono)`
smaller; tombstone ages use the same clock, so the inequality retains its
direction. No link-throughput or queue-residence assumption remains.

The control plane and its daemons must run the same protocol revision: an
older daemon ignores `dispatched_at_unix_ms` and enforces no age bound, so this
proof does not hold for it, and an older control plane sends no timestamp, so
a current daemon refuses every dispatch as INVALID.

## Capacity planning

For normalized wire options calculate `(Smin, Rmin) = wire.MinBudget(opts)` and
require send >= 2*Smin and receive >= 2*Rmin. With 64 MiB messages that is send
>= 256 MiB and receive >= 524 MiB + 8 KiB; 512 MiB receive is insufficient for
this task's rule. A practical small deployment starts at 256 MiB send / 640 MiB
receive. All daemon links share that ONE NewBudget, with independent pools.

Plan live wire ownership as S + R + N*(5 MiB + 64 KiB), then add pending,
subscription buffers, at most one borrowed inbound message per admitted socket,
bounded operation scratch, store/SQLite, TLS/kernel buffers and GC
headroom. For 100 sockets the wire portion at 256/640 MiB is approximately
1.37 GiB before router memory and GC. N counts handshaking/closing sockets too.
MaxConnections is not configurable: the server fixes it at 100 daemon sockets
(the gateway default; the registry also holds at most 100 devices) and 2
worker-link sockets. Operators configure the daemon-link pools
(`--send-pool-bytes`, `--receive-pool-bytes`), `--max-message-bytes` and
GOMEMLIMIT; size GOMEMLIMIT against the deployment's full budget, including
the worker link's own pools and the router budgets (README, Memory).

A live slow consumer can retain default MaxReceiveBytes = 136 MiB in wire's
unread queue. After Receive, a hook's borrowed value is outside that pool and
is included separately in the process memory plan. Several such consumers can
exhaust a small receive pool. One link's peak receive phase can reach about
462 MiB, so size for concurrent large transfers as well as device count. For K
simultaneous worst-case large receivers conservatively provision at least
max(2*Rmin, K*462 MiB) receive, with additional allowance for slow retained queues;
small heartbeats use per-link reserves, but large results can still starve.

Keep Receive moving: no subscriber waits in a hook, and store work must have
bounded latency. ErrBudgetExhausted/ErrConsumerStalled mean close and resync;
graceful close still drains within MaxConsumerStall before reservations disappear.
The measurements (gateway/measure_test.go, TestMeasureReceivePoolShare)
found no wire change necessary at the current scale. Pools sized by max(2*Rmin, K*462 MiB)
showed no starvation, budget exhaustion or stalls at 4, 16 and 64 devices;
unconstrained large links peaked at about 250–300 MiB each, close to Rmin
(262 MiB at 64 MiB messages). At the current scale (4 devices, K = 1) the 524 and
640 MiB pools are fine as well. When K is not bounded (64 devices, 8 large
senders, 524/640 MiB) ordinary 1 KiB results waited up to the 15 s stall
bound and 25–82% were lost to forced reconnects: large links' partial
reassemblies compete for the pool, and small results queue behind a large
message waiting on the same link. Operational rule: size receive at
max(2*Rmin, K*Rmin) plus headroom (K*462 MiB is the measured-safe value), and
read ErrBudgetExhausted or forced reconnects on large-result links as K
exceeding the provisioning. Before multi-user (M4), add a per-link cap of
Rmin on shared receive holdings plus a 32–64 MiB reserve usable only by
admissions up to 4 MiB; a cap alone does not protect ordinary links once
K*Rmin reaches the pool. The measurement is opt-in
(CIRCUS_MEASURE=1), a few minutes per segment:

    CIRCUS_MEASURE=1 CIRCUS_MEASURE_SCENARIOS=n16-k4-2min,n16-k4-640,n16-k4-k462 \
      go test -timeout 580s -count=1 -v -run TestMeasureReceivePoolShare ./internal/controlplane/gateway/

Scenarios run for N = 4, 16 and 64 devices with K = min(max(1, N/4), 8)
large senders: n<N>-none, n<N>-k<K>-2min and n<N>-k<K>-640, plus
n<N>-k<K>-k462 when K > 1 (at K = 1 that pool equals 2*Rmin, which -2min
already covers), so n4-k1-k462 does not exist;
CIRCUS_MEASURE_SECONDS and CIRCUS_MEASURE_SLOW_MIBPS tune duration and the
large consumer's rate.

## Implemented decisions

1. **Rejected codes implemented.** The worker proto gained three rejection
   codes, with generated Go/TS: CODE_STORE_UNAVAILABLE means nothing
   persisted/dispatched and permits a same-ID retry; CODE_DEVICE_SHUTTING_DOWN
   is a terminal daemon refusal and requires a new ID to retry;
   CODE_NOT_FOUND answers only a Query for an ID this user cannot see and is
   not a call outcome.
2. **Enumeration implemented.** CallFilter gains UserID/Limit/After; JobFilter
   and TimerFilter provide bounded keyset pages. LoseCalls takes a capped limit
   and drains in successive transactions. Store contract tests cover ordering,
   invalid limits, concurrent inserts on a second handle, and bounded loss batches.
3. **Unavailable completed result implemented.** ErrResultUnavailable is permanent;
   ErrDeviceOffline is transient. Neither fabricates content or changes C metadata.
4. **Snapshot omission implemented with existing PutJob.** For an unfiltered,
   untruncated snapshot of the current boot, a stored running job omitted from
   it with LastSeq <= snapshot.Seq becomes lost through PutJob(state lost).
   PutJob applies only strictly newer sequences, so the loss is written at
   snapshot.Seq, or at snapshot.Seq+1 when the job's last event is the
   watermark itself: lost is terminal and job writes are serialized, so no
   real event can collide (only Seq == MaxInt64 is left running and logged).
   Jobs have no reason column, so the fixed reason `missing from daemon
   snapshot` is logged, not stored. A snapshot with `truncated` set never
   settles omissions. Values above math.MaxInt64 close the link.
5. **Attempt and transport corrections implemented.** Guarded lost -> accepted resets all attempt
   columns; current boot is read in the dispatch transaction; results follow
   negotiated limits; pools are doubled; graceful Receive drains before disconnect.
6. **Resend decision and receiver-side age bound implemented.** ResendHorizon
   defaults to ten minutes and requires retained, unacknowledged content; it is
   rechecked after sync waits and otherwise recovery uses CallQuery. The daemon
   rejects unseen dispatches older than twelve hours using the Welcome time
   base, making the horizon a policy knob rather than a safety assumption.

## Deviations from the initial design

- **One admission lock, not per-device gates.** All router mutations, store
  transactions and send admissions run under one mutex; long walks (sync
  replay, accepted dispatch, loss batches) release it between items so inbound
  hooks and other devices progress. It is never held while waiting for a
  daemon response, a subscriber or sync capacity. Costs that remain under the
  lock: marshalling a DispatchCall of up to MaxMessageBytes inside
  Link.Send, charging (a tree walk) and cloning each update once per publish
  (subscribers and query waiters share that immutable clone, each charged),
  the final cancel replay of one device's active calls, and every store
  transaction. A slow store or a JobEvent storm from one device therefore
  delays every link's Receive and can trip MaxConsumerStall and resync on
  other devices. Acceptable at the current scale (single user, few devices,
  SQLite's single writer). Per-device locks with marshalling and cloning
  outside them are the follow-up. Lock hold times are measured by
  measure_lock_test.go, TestMeasureLockHold (opt-in, about a minute; put
  the SQLite file on a real disk to include fsync):

      CIRCUS_MEASURE=1 CIRCUS_MEASURE_DIR=$PWD/.tmp \
        go test -timeout 580s -count=1 -v -run TestMeasureLockHold ./internal/controlplane/calls/
- **Grace expiry does not consult the registry.** OnConnected cancels the grace
  timer in its first Tx, so a successful CompleteTimer already proves no sync
  has started. A registered-but-unsynchronized route must not swallow the timer.
- **Query of a nonterminal call never blocks on an absent daemon.** When the
  daemon cannot be asked, or its connection ends before answering, Query
  returns the durable state (Dispatched/Unknown); only a completed call returns
  ErrDeviceOffline. While the route synchronizes, Query waits for the sync. A
  duplicate Submit of a completed call queries the daemon like Query and
  returns its error the same way.
- **Accepted rows without retained content** (an ambiguous failed submission
  whose row committed) become lost `REQUEST_UNAVAILABLE` when a route syncs;
  never dispatched, so an equal resubmission may start a new attempt.
- **Restart offline accounting.** The router records its start time. A device
  whose row still says connected (a recorded boot and LastConnectedAt >=
  LastDisconnectedAt) but
  has no live route was left so by a previous process (crash, or a reversed
  shutdown order); it counts as offline since max(LastDisconnectedAt, start).
  Submit and the watchdog persist that disconnect lazily in their Tx and create
  the device_grace timer if none exists, so no device table scan is needed. A
  submission within the grace period after the restart stays accepted (workers often
  reconnect before daemons), dispatched and
  running calls go unknown at grace expiry, and the 30-minute
  result-retention cutoff counts from that time, so watchdogs firing before
  the daemon reconnects only re-arm. Composition must call gateway Shutdown
  BEFORE Router.Close (package doc), so a normal shutdown records every
  disconnect itself. Known display-only inaccuracy: a device
  that never reconnects after a crash, and has no submission or watchdog that
  would record its disconnect, keeps a row that says connected. Routing never
  reads that flag alone (offlineSince applies whenever no route exists);
  readers such as `circus doctor` or a UI must treat "no live route" as
  offline.
- **Per-user subscription share is not implemented.** MaxSubscriptions is
  global; one leaking worker can exhaust it. Acceptable with a single user;
  planned for M3/M4.
