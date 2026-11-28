# circus link wire protocol

This is the wire protocol between the control plane and its two kinds of
link clients: the device daemon (`/v1/link/daemon`) and workers
(`/v1/link/worker`). It is written so that a peer can be implemented without
reading Go. Transport behaviour is implemented in Go; numeric transport
limits name their Go symbols (package `wire` unless qualified). Session
processing rules define the contract the control plane's session service
(`internal/controlplane/sessions`) implements.

Protocol version: **1** (`ProtocolVersion`). Messages are the proto3 messages
in `proto/circus/v1/` (`link.proto`, `daemon_link.proto`,
`worker_link.proto`, `device.proto`, `session.proto`); TypeScript bindings are generated into
`gen/ts` (`@circus/proto`). Run `npm run build --prefix gen/ts` to produce
the exported JavaScript and declarations in ignored `gen/ts/dist`;
`npx tsc --noEmit -p gen/ts` remains the type-check gate. "MUST" marks what the Go peer enforces (breaking
it closes the link) or what Go peers rely on; "SHOULD" marks the reference
behaviour of the Go clients.

Notation: F = negotiated `max_fragment_bytes`, M = negotiated
`max_message_bytes`. KiB/MiB are powers of two.

## 1. Transport

| | Daemon link | Worker link |
| --- | --- | --- |
| Path | `/v1/link/daemon` | `/v1/link/worker` |
| Credential role | device (daemon) | worker |
| Client sends | `DaemonUplink` | `WorkerUplink` |
| Server sends | `DaemonDownlink` | `WorkerDownlink` |
| Server connection cap (HTTP 503 when full) | 100 (`gateway.Config.MaxConnections`, max 256) | 8 by default (`worker_connections` in the server config, `workerlink.Config.MaxConnections`), and at most 4 per credential (`workerlink.maxPerCredential`) |

**URL.** The credential file holds a base `control_plane_url` (no userinfo,
query or fragment). The link URL is the base with its path's trailing `/`
removed and the link path appended; `https` becomes `wss`, `http` becomes
`ws` (`loopback.linkURL`). Example: `https://cp.example/base` →
`wss://cp.example/base/v1/link/worker`.

**TLS vs plaintext (client rule, `loopback.CheckScheme`, `loopback.Client`).**
- `https`/`wss`: TLS 1.2 or later. Server certificates are verified against
  the system roots, or only against the certificates in the credential's CA
  file when one is set.
- `http`/`ws`: allowed only when the URL host is literally `localhost` or a
  loopback IP literal (`loopback.Host`), **and** the address actually dialed
  is a loopback address (`loopback.Addr`); otherwise the connection is closed
  before anything is written. No HTTP proxy is used for plaintext.
- Any other scheme is refused. Redirects are never followed.

**Upgrade request.** `GET` on the exact path (other methods: 405, other
paths: 404). Plain RFC 6455 upgrade:
- No WebSocket subprotocol is defined. The server selects none, so a client
  MUST NOT offer one (many client libraries fail the handshake when an
  offered subprotocol is not selected).
- No extensions: the server does not negotiate `permessage-deflate`.
- If an `Origin` header is sent, it must match the request `Host`, otherwise
  the WebSocket library answers 403. Non-browser clients should omit it.

**Authentication** happens before the upgrade (`auth.Admit`):
`Authorization: Bearer <token>` (scheme case-insensitive). A token is 43
characters of unpadded base64url encoding 32 bytes (`auth.TokenLen`,
`auth.TokenBytes`).

| HTTP status | Meaning |
| --- | --- |
| 401 (`WWW-Authenticate: Bearer`) | Missing, malformed or unknown token. Also on the daemon link for an unusable principal. |
| 403 | Credential revoked or identity deleted; or a credential of the other role (e.g. a daemon token on the worker link). |
| 503 | Authentication store failure; connection cap full (worker link: the total or the per-credential cap); server shutting down. |

The worker link authenticates first and then checks its cap; the daemon link
checks its cap first. After the upgrade the server re-validates the
credential about once per second (`auth.RecheckPolicy`: interval 1 s, check
timeout 5 s, max stale 30 s). A definitive revocation aborts the connection
within about one interval plus one timeout; if checks keep failing (without
a definitive answer) for 30 s, the connection is aborted too. There is no
in-band notice; the reconnect then gets 403.

**Frames.** Every WebSocket message is **binary** and carries exactly one
serialized envelope (one of the four messages above). A text message closes
the link (`WebSocket.ReadMessage`). The receiver's per-message size limit is
64 KiB during the handshake (`MaxMetadataBytes`, set in `newWebSocket`) and
F + 128 bytes after Welcome (`FrameOverhead`, set in `New`); a larger
WebSocket message fails the read and closes the link. WebSocket ping/pong
are handled by the WebSocket layer and are invisible to this protocol.

## 2. Handshake

```
client                                   server
  | -- HTTP GET + Bearer, upgrade -------> |   401/403/503 here, no upgrade
  | -- Uplink{hello} --------------------> |   first and only frame before Welcome
  | <------------- Downlink{welcome} ----- |   or Downlink{reject}, then close
  |  ... link (sections 3-6) ...           |
```

The client sends exactly one Hello and nothing else until it has read the
answer. The server reads exactly one frame as Hello. Neither side sends a
business message before Welcome. Both sides start the link (heartbeats,
fragmentation, the limits from Welcome) immediately after Welcome is
written/read.

**Timeouts.** The server bounds reading Hello and writing its answer by
10 s from the upgrade (`HelloTimeout`); on expiry it closes without a
Reject. The Go clients bound dial + upgrade + Hello/Welcome by 10 s
(`HelloTimeout` in `workerclient.Dial`; `link.Options.HandshakeTimeout`,
default 10 s, on the daemon).

### Hello (client → server)

| Field | Daemon link | Worker link |
| --- | --- | --- |
| `protocol_version` | MUST be 1 | MUST be 1 |
| `boot_id` | MUST be 1-128 bytes; a fresh UUIDv7 per process start, kept across reconnects | A worker process MUST send a fresh UUIDv7 (canonical lowercase form) per process, kept across reconnects; it is required to Attach or drive sessions. A client that only submits calls without a session (the call CLI) sends none. Anything else, or a UUIDv7 older than the worker's current boot while that boot is alive (below), is `CODE_BAD_HELLO` |
| `client_version` | informational | informational |
| `os`, `arch`, `hostname`, `machine_id` | sent, informational | not checked |
| `features` | optional capabilities for this connection | optional capabilities for this connection |

The control plane considers the first 32 feature names and drops entries
outside 1–64 bytes of `[a-z0-9_.]`. Unknown names have no effect. The
daemon advertises `device_query.list_directory` when it accepts directory
queries, `device_query.git_remote` when it accepts git remote queries,
and `device_call.prepare_workspace` and `device_call.remove_workspace`
when it runs the `prepare_workspace` and `remove_workspace` device calls
(see "Workspace preparation").
Either role advertises `machine_update` when it runs as the installed user
service (the daemon started with `circus daemon --service`, the worker by
its launcher with `--service`) and accepts UpdateMachine (see "Machine
updates"). Both links read features the same way.
The control plane sends a feature-dependent message only when the peer
advertised the feature in its current Hello.

The control plane records each worker connection's nonempty `boot_id` as
the worker's current boot, and every connection of a boot that is still
current releases every lease the worker holds under another boot before it
is served (an
open turn becomes interrupted, the old connections get `LeaseRevoked`);
connections of any boot but the current one get no Offers. Boots are
ordered by their UUIDv7 timestamp, then by their remaining bytes. A Hello
with a boot older than the current one (a replaced process still
reconnecting) is refused with `CODE_BAD_HELLO`, releasing nothing, while
the current boot is alive: it has a connection to this control plane, it
connected or its last connection closed within the last 60 seconds, or it
holds a lease that has not expired. Otherwise the older-looking boot is accepted and becomes current
(the timestamps come from the workers' clocks, so a boot minted on a clock
that was ahead cannot lock later boots out once it is gone). Reject is
terminal for the refused process. Workers MUST ensure their old
CLI processes have exited before a new process connects. The Go call client (`internal/workerclient`, used by `circus
call`) sends no boot_id. The whole Hello envelope must decode within 64 KiB
(`MaxMetadataBytes`).

**Server decision (`AcceptHello`)**, in this order:
1. Frame does not decode, or its payload is not `hello` → Reject `CODE_BAD_HELLO`.
2. Link-specific check fails (daemon: `boot_id` empty or longer than 128 bytes; worker: `boot_id` nonempty and not a canonical UUIDv7, or older than the worker's current boot while that boot is alive) → `CODE_BAD_HELLO`.
3. `protocol_version != 1` → `CODE_VERSION_UNSUPPORTED`.
4. Otherwise → Welcome.

A Reject always carries `min_version = max_version = 1` and an empty
`message`; the server then performs a normal WebSocket close (1000).
`CODE_UNAUTHENTICATED` and `CODE_REVOKED` exist in the schema but are never
sent: authentication failures are HTTP statuses (section 1).

### Welcome (server → client)

`FillWelcome` always writes explicit nonzero limits. Defaults below are those
of `Options.defaults`; the control plane uses the same options for both links
except `max_in_flight_messages` (`controlplane/links.go`; M and the
heartbeat values are configurable, F is not).

| Field | Default | Meaning |
| --- | --- | --- |
| `principal_id` | | Device or worker id of the authenticated credential. |
| `instance_id` | | Control-plane instance; informational. |
| `heartbeat_interval_ms` | 5000 | Send period for Heartbeat, both directions. |
| `heartbeat_timeout_ms` | 15000 | Inactivity limit, both directions. Must exceed the interval. |
| `max_message_bytes` (M) | 67108864 = 64 MiB (`DefaultMaxMessageBytes`) | Largest serialized logical envelope, both directions. |
| `max_fragment_bytes` (F) | 262144 = 256 KiB (`DefaultMaxFragmentBytes`) | Largest unfragmented envelope and largest `Fragment.data`. F ≤ M. |
| `negotiated_version` | 1 | |
| `server_time_unix_ms` | server clock | See below. |
| `max_in_flight_messages` | 2 on the daemon link, 1 on the worker link (`workerlink.New`); allowed 1..2 | Max partially transferred fragmented messages per direction. |
| `max_in_flight_bytes` | 2 × M | Max sum of `total_size` of partially transferred fragmented messages per direction. |

A zero limit field is read as its default (`OptionsFromWelcome`), for
compatibility; the Go server never sends zero.

`server_time_unix_ms` is **normative on the daemon link**: it is the
control plane's clock when it wrote Welcome, on the same clock as
`DispatchCall.dispatched_at_unix_ms`. The daemon anchors it to its monotonic
clock at Welcome receipt and computes a dispatch's age as
`(now_mono − welcome_mono) + (server_time_unix_ms − dispatched_at_unix_ms)`
(`link.dispatchAge`); a Welcome with `server_time_unix_ms <= 0` is unusable
(the daemon drops the connection and retries). On the worker link the field
is set but **informational**; clients must not depend on it.

**Client checks** (`DialHello`, `OptionsFromWelcome`):
- The answer is a Welcome or a Reject; anything else is an error.
- `negotiated_version == 1`.
- `principal_id` equals the principal id stored with the credential.
- `max_message_bytes` ≤ the client's own message cap (default 64 MiB).
- The limits form a valid set: F > 0, F ≤ M, `max_in_flight_bytes` > 0,
  `max_in_flight_messages` in 1..2, interval > 0, timeout > interval.
- Daemon only: `server_time_unix_ms > 0`.

There is no version range negotiation in practice: the only version is 1 on
both sides.

**Failure classes** (reference behaviour of the Go daemon client,
`link.Client.connect`/`Run`; the Go worker client never reconnects and
returns every failure to its caller):

| Failure | Class |
| --- | --- |
| HTTP 401 or 403 | terminal: fix or replace the credential |
| Any Reject | terminal |
| Version, principal or message-limit mismatch in Welcome (`ErrIncompatible`) | terminal |
| HTTP 503, other HTTP status, network/TLS error, timeout, invalid limit set, missing server time, any later link error | retryable |

Retry backoff (daemon): equal jitter in [base/2, base], base doubling from
500 ms to 30 s, reset after a connection that lasted 60 s. A GoAway's
`reconnect_after_ms`, when nonzero, replaces the jittered delay but is raised
to at least base/2.

## 3. Framing

### 3.1 Envelopes

Each envelope has a single `oneof payload`.

| Envelope | Payload fields (number) |
| --- | --- |
| `DaemonUplink` | hello 1, heartbeat 2, fragment 3, call_ack 10, result 11, call_status 12, job_event 13, job_snapshot 14, device_query_result 15, update_failed 16 |
| `DaemonDownlink` | welcome 1, reject 2, heartbeat 3, fragment 4, go_away 5, dispatch_call 10, cancel_call 11, call_query 12, list_jobs 13, stop_session_jobs 14, device_query 15, update_machine 16 |
| `WorkerUplink` | hello 1, heartbeat 2, fragment 3, submit_call 10, cancel_call 11, call_query 12, unwatch 13, attach 14, session 15, resume_leases 16, list_devices 17, report_agents 18, update_failed 19 |
| `WorkerDownlink` | welcome 1, reject 2, heartbeat 3, fragment 4, call_update 10, session_reply 12, worker_event 13 (11 and `job_event` reserved) |

Both worker envelopes also have `uint64 request_id = 20` beside the oneof
(section 7). It is 0 (absent) on hello, welcome, reject, heartbeat and
fragment frames; receivers ignore it there.

A received envelope MUST contain exactly one field of its `payload` oneof,
encoded as a length-delimited field (`scan`). Other known fields
(`request_id`) and unknown fields next to it are ignored by this check, but
an envelope whose only payload is a field number the receiver does not know
is rejected ("missing envelope payload"), so adding a payload type is a
breaking change for older peers.

`hello`/`welcome`/`reject` are only valid in the handshake. After Welcome
they are unexpected business messages: the worker-link server and the Go
daemon client close the link; the daemon-link server ignores them.
`heartbeat` and `fragment` belong to the transport and are never delivered
to the application.

### 3.2 Classes and ordering keys

The sender classifies every message (`classify`). Class and key are local
scheduling metadata; nothing about them is on the wire.

| Payload | Class | Ordering key |
| --- | --- | --- |
| heartbeat, go_away, call_ack | Control | none |
| list_jobs, stop_session_jobs, unwatch, attach, resume_leases | Barrier | none |
| dispatch_call, submit_call, cancel_call, call_query, call_update | Data | `call/<call_id>` |
| device_query, device_query_result | Data | `device_query/<request_id>` for valid IDs |
| job_event (daemon uplink) | Data | one key for the whole boot |
| session (worker uplink) | Data | `session/<session_id>` |
| list_devices, report_agents, session_reply, worker_event, update_machine, update_failed | Data | none |
| result, call_status, job_snapshot (and hello, welcome, reject) | Data | none |

Independently of the class, a message is **small** if its serialized
envelope is ≤ F bytes (sent as one frame) and **large** otherwise (sent as
fragments). So there are three service categories: control, small business
(Data/Barrier ≤ F), large (anything > F, including a large control such as
an oversized `call_ack`).

Rules the sender enforces (`blocked`):
- Messages with the same nonempty key are written in enqueue order, and a
  later one is not started until the earlier one is completely written.
- A Barrier is not started until every earlier Data/Barrier message is
  completely written, and no later Data/Barrier message starts before the
  barrier is completely written.
- Control messages are never held back by keys or barriers.
- Messages without a key have no order relative to anything else.

### 3.3 Fragmentation

A logical envelope larger than F is serialized once and cut into chunks of
exactly F bytes (the last one shorter). Each chunk travels as its own frame:
an envelope of the same direction whose payload is `fragment`:

| `Fragment` field | Rule |
| --- | --- |
| `message_id` (1) | Nonzero. Assigned when the message's first fragment is written; strictly greater than every earlier `message_id` in this direction. The Go sender uses 1, 2, 3, …; receivers only require "greater than all previous". |
| `index` (2) | 0 for the first fragment, then +1 without gaps. |
| `last` (3) | True exactly on the fragment that completes `total_size`. |
| `total_size` (4) | On index 0: the full envelope size, 1..M. On every later fragment: 0. |
| `data` (5) | 1..F bytes, and never past `total_size`. |

The reassembled bytes are decoded as an envelope of the same type; it MUST
NOT itself be a `fragment`. proto3 omits zero values, so `index` 0,
`last` false and `total_size` 0 are absent on the wire. Observed Go frames
for a 614426-byte `WorkerUplink` with F = 256 KiB (`1a` = field 3, LEN):

```
index 0: 1a 8a8010 | 08 01 | 20 9ac025 | 2a 808010 <262144 bytes>   (no index, no last)
index 1: 1a 888010 | 08 01 | 10 01     | 2a 808010 <262144 bytes>
index 2: 1a a4c005 | 08 01 | 10 02 | 18 01 | 2a 9ac005 <90138 bytes>
```

`WorkerDownlink`/`DaemonDownlink` use field 4 (`22`). Fragment framing costs
at most 38 bytes with maximal varints, well inside the 128-byte allowance
(`FrameOverhead`).

**Sender rules** (`Link.send`, `canStart`):
- Envelope ≤ F → one frame; > F → fragments; > M → not sendable.
- A fragmented message larger than `max_in_flight_bytes` is not sendable.
- A new fragmented message may start only while fewer than
  `min(2, max_in_flight_messages)` fragmented messages are partially written
  and the sum of their `total_size` plus the new one is ≤
  `max_in_flight_bytes`. A message stops counting once its `last` fragment
  is written.

**Receiver rules** (`reassembler.add`; every violation closes the link):
- `message_id` 0, empty `data` or `data` > F.
- A fragment for an unknown `message_id` must have index 0, a `message_id`
  greater than every earlier one, and `total_size` in 1..M.
- Starting a message when `max_in_flight_messages` messages are already
  partial, or when the sum of partial `total_size` values would exceed
  `max_in_flight_bytes`.
- `total_size` on a continuation; wrong `index`; data beyond `total_size`;
  `last` not set exactly on the completing fragment.
- An unfragmented (non-`fragment`) frame larger than F.
- A reassembled envelope that is a `fragment`, or that fails section 5.
- Reassembly idle timeout: a partial message that receives no fragment for
  the receiver's fragment timeout. This is a local setting, not negotiated
  (`Options.FragmentTimeout`, default = heartbeat timeout, 15 s). The timer
  restarts with each fragment of that message, so other traffic does not
  keep a stalled message alive.
- End of stream (even a clean close) while any message is partial.

A message smaller than F that arrives fragmented is accepted. Frames of
different messages, unfragmented messages and heartbeats may arrive between
the fragments of a message.

### 3.4 GoAway and orderly close

**GoAway** exists only in `DaemonDownlink`. The daemon-link server sends it
with `REASON_SUPERSEDED` (a newer connection of the same device registered)
or `REASON_SHUTTING_DOWN`, always with `reconnect_after_ms = 0` today
(`gateway.requestClose`). It is a Control message, so it can arrive before
business messages queued earlier. The server then stops accepting new
sends, flushes what is already queued (including the rest of any partial
fragmented message), and closes the WebSocket with 1000, all bounded by the
heartbeat timeout; past that it aborts. The Go daemon, on GoAway, stops
processing received messages, flushes its own queue, closes with 1000 and
reconnects after the delay from section 2. The worker link has no GoAway:
worker-link shutdown and revocation abort the connection.

**Orderly close** (`Link.Shutdown`): stop accepting sends, stop sending
heartbeats, write every queued frame, then a WebSocket close with status
1000. The receiver treats a 1000 close (or EOF) that arrives after a
complete frame with no partial message as graceful and still delivers
everything already received. Any other close status, a TCP reset, or a
close with a partial message is an error. On every error the Go side aborts
the TCP connection without a close frame, so the peer sees an abnormal
closure (1006).

## 4. Liveness

- **Heartbeats.** Each side sends `Heartbeat{seq, sent_unix_ms}` every
  `heartbeat_interval_ms`, whether or not other traffic flows. `seq` starts
  at 1 and increases by 1; both fields are diagnostic and ignored by the
  receiver. Heartbeats are Control; the Go sender keeps at most one unsent
  heartbeat queued and sends none once shutdown has started.
- **Activity.** Any received frame counts: heartbeat, fragment or other
  envelope. WebSocket pings do not.
- **Timeout.** No frame for `heartbeat_timeout_ms` closes the link. The Go
  timer runs every `min(heartbeat interval, fragment timeout)` (5 s by
  default), so detection takes 15-20 s at the defaults.
- **Throughput.** Liveness is per complete frame: one fragment frame must
  arrive within the heartbeat timeout even when two large messages share the
  link. Plan for more than 2 × F / timeout of usable bandwidth (about
  35 KiB/s at the defaults), plus control and small traffic; use a smaller F
  on slower links.
- **Consumer stall (local).** When a Go reader has a frame but no room to
  accept it (its consumer is not draining, or a shared memory pool is
  full), it stops reading. The inactivity and fragment deadlines pause while
  it waits and resume (shifted by the pause) afterwards; the wait is capped
  at `MaxConsumerStall` (default = heartbeat timeout, 15 s), after which the
  link closes (`ErrConsumerStalled`/`ErrBudgetExhausted`). From the peer's
  side the Go endpoint may stop reading for up to that long while its
  writer keeps sending heartbeats. A peer must therefore judge liveness only
  by frames it receives, not by its own writes making progress. A peer MAY
  apply the same pause rule to its own reader.

The link closes on: heartbeat timeout, reassembly timeout, any protocol
violation (sections 3 and 5), consumer stall, the sender's own queue
overflowing (`ErrQueueFull`; Go defaults 256 messages or 2 × M bytes,
local), credential revalidation failure, GoAway/shutdown, or
application-level failures listed in section 7.

## 5. Decode limits

A receiver checks the protobuf wire structure before decoding (`scan`,
`scanMessage`). Every violation is a protocol error that **closes the link**;
no frame is dropped silently. The Go sender runs the same scan on its own
message before queueing it and refuses it locally (`ErrTooLarge`) without
closing; other senders MUST NOT send what the scan would reject.

**Size limits by payload** (`payloadLimit`; measured on the whole envelope):

| Payload | Max envelope bytes |
| --- | --- |
| dispatch_call, submit_call, result, call_status, call_ack, call_update, session, session_reply | M |
| job_snapshot | 8 MiB (`MaxSnapshotBytes`) |
| device_query_result | 4 MiB (`MaxDirectoryResultBytes`) |
| fragment | F + 128 (`FrameOverhead`) |
| everything else (hello, welcome, reject, heartbeat, go_away, cancel_call, call_query, device_query, unwatch, list_jobs, stop_session_jobs, job_event, attach, resume_leases, list_devices, report_agents, worker_event, update_machine, update_failed) | 64 KiB (`MaxMetadataBytes`) |

Further size rules:
- call_status, call_ack, call_update: envelope size minus the encoded size of
  all embedded `DeviceResult` messages ≤ 64 KiB; only result output may be
  large.
- Every `JobEvent` message anywhere in the tree (including
  `JobSnapshot.jobs`) ≤ 64 KiB encoded.
- `session` envelopes other than AppendTranscript and PublishEvents ≤ 64 KiB
  (`MaxMetadataBytes`). This limit binds before the repeated-field caps
  below: those are upper bounds that are never all reachable together in one
  request, so a worker splits large mark sets across several requests
  (section 7).
- AppendTranscript batches contain ≤ 8 MiB of encoded `entries` fields
  (`MaxTranscriptBatchBytes`), or exactly one larger entry fitting M together
  with its envelope.
- The TypeScript worker applies a local send budget of `min(M, 32 MiB)` to
  a single uplink request, including 16 bytes reserved for a possible
  Unwatch. Requests exceeding this budget, including oversized individual
  records, fail locally.
- Unfragmented frames ≤ F; reassembled envelopes ≤ M (section 3.3).

**Structure limits** (per envelope, counted over the whole tree):

| Limit | Value |
| --- | --- |
| Message nesting depth, envelope included | 32 (`scanMessage`, `RecursionLimit`) |
| Message nodes (envelope counts as one) | 8192 (`MaxDecodeNodes`) |
| Field occurrences: every tag at every level, unknown fields included; a packed repeated field counts once | 65536 (`MaxDecodeFields`) |
| `JobSnapshot.jobs` elements | 4096 (`MaxSnapshotJobs`) |
| `ListDirectoryResult.entries` elements | 1000 (`MaxDirectoryEntries`) |
| `ToolOutput.content` elements | 128 |
| `BashRequest.working_dirs` elements | 256 |
| `CallUpdate.DeviceOffline.online_devices` elements | 256 |
| `WorkerEvent.Nudge.reasons`, `GetSessionState.sections` | 5 each, unpacked enums |
| `Attach.agents`, `ReportAgents.agents`, `WorkerEvent.AgentsWanted.agents` | 16 each |
| `SessionConfig.cwds`, `PatchSessionState.cwds`, `SessionState.cwds` | 256 each |
| `PatchSessionState.files`, `SessionState.files` | 4096 each |
| `AppendTranscript.entries`, `SessionReply.TranscriptPage.entries` | 8189 each (8192 nodes minus the three containing messages); primarily byte-bounded |
| `ReportTurn.Persisted.input_ids`, `SessionReply.Inputs.inputs` | 32 each |
| `ReportTurn.Persisted.call_ids`, `ReportTurn.Persisted.jobs` | 1024 each |
| `ReportTurn.Persisted.late_call_ids`, `PatchSessionState.awaiting_call_ids` | 16 each |
| `PatchSessionState.returned_call_ids`, `PatchSessionState.handed_jobs` | 1024 each |
| `PatchSessionState.bgtasks_started`, `PatchSessionState.bgtasks_ended` | 64 each |
| `SessionState.jobs` | 1024 |
| `SessionState.calls`, `Recovery.calls` | 1040 each (1024 unpersisted plus 16 with a notice owed) |
| `SessionState.grants` | 32 |
| `Recovery.lost_bgtasks` | 64 |
| `PublishEvents.events` | 128 |
| `ResumeLeases.leases`, `SessionReply.Resumed.leases` | 64 each |
| `SessionReply.TranscriptKeys.subpaths` | 1024 |
| `SessionReply.Devices.devices` | 256 |
| `SessionReply.LaunchEnv.env` | 72 |
| `PrepareLedgerRequest.files`, `CollectLedgerRequest.known`, `CollectLedgerResult.changes`, `CollectLedgerResult.reverted`, `RebaseLedgerRequest.edits`, `RebaseLedgerRequest.known` | 4096 each (`MaxLedgerSnapshotFiles`) |
| `CollectLedgerResult.problems` | 1024 |

User-side RPC response caps are registered in the same table:
`ListSessionsResponse.sessions` ≤ 1000, `ListApprovalsResponse.approvals` ≤ 32.
These RPCs do not run through the link decoder. The structural limits above
still apply to any combination of fields in one link envelope, even when
individual repeated fields remain below their caps.

The repeated-field caps (`repeatedLimits`) count all elements of that field
in the envelope, not per parent. A field reused as a message type keeps its
cap wherever it appears: `PatchSessionState.persisted` uses the
`ReportTurn.Persisted.*` caps. Also rejected: groups (wire types 3/4)
anywhere, packed encoding of the new repeated enums, field number 0,
truncated or malformed fields, a known message-typed field not encoded as length-delimited, more than one payload
in the envelope, more than one `op` field in a `SessionRequest` (even the same op repeated), and anything the protobuf decoder refuses, notably
**invalid UTF-8 in a `string` field**.

**Decoded cost** (`decodedCost`):
`2 × encoded_bytes + 256 × nodes + 64 × fields + 4096`. The Go receiver
charges this estimate against its local receive memory before decoding; on
a link with a shared budget, an unfragmented frame whose cost plus its
length fits 64 KiB (`LinkReserveBytes`) uses a per-link reserve instead of
the shared pool. The cost never makes a message invalid; it only decides
when the Go reader pauses (section 4). It is local accounting, and a
non-Go peer may bound its memory differently.

## 6. Ordering as observed by the peer

The receiver delivers a message when its last frame arrives, so the
delivery order is the order in which messages *complete* on the wire.
WebSocket keeps frames FIFO per direction.

A peer can rely on:
- **Per-key FIFO:** messages with the same ordering key arrive in send
  order (e.g. all `CallUpdate`s of one call; `SubmitCall` before a later
  `CancelCall`/`CallQuery` of the same call; job events of one boot, which
  also carry increasing `seq`).
- **Barriers:** `ListJobs`/`StopSessionJobs`/`Unwatch`/`Attach`/`ResumeLeases` arrive after every
  Data/Barrier message sent before them, and before every one sent after
  them.
- **Interleaving:** small messages and other messages' fragments can arrive
  between the fragments of a large message.

A peer must not assume:
- any order between messages with different keys or without a key (a
  `DeviceResult` may arrive before its `CallAck`; a small `CallUpdate` for
  call B may arrive before a large one for call A that was sent first);
- that Control messages (heartbeat, GoAway, `CallAck`) keep their position
  relative to business messages: they are sent ahead of queued business;
- that heartbeats arrive exactly every interval (one can wait behind a
  fragment being written);
- contiguous `message_id`s, or that only messages larger than F are
  fragmented.

### Device queries on daemon links

`DaemonDownlink.device_query` asks the daemon for read-only information
outside a session. The control plane gives each query a fresh
`request_id` and tracks at most 16 on the current connection. It drops
unmatched results, removes requests on cancellation or after 10 seconds,
and fails pending requests as soon as the link closes or is superseded.
Queries have no persistent state, boot ID, resend or deduplication.

`DaemonUplink.device_query_result` echoes the ID and carries either a
query error or the result for the requested query kind. An ID outside
1–128 bytes of `[A-Za-z0-9_-]` gets `CODE_INVALID` with an empty ID. A
missing or unknown query type gets `CODE_UNSUPPORTED`. The daemon runs
up to four device queries concurrently across both kinds and answers
additional queries with `CODE_BUSY`. During shutdown it uses `CODE_SHUTTING_DOWN`
while the link can still send. An empty or mismatched outcome fails the
pending request. The Web API limits `MachineService.ListDirectory` and
`MachineService.GetGitRemote` to eight combined in-flight requests per user.

Directory listings resolve empty paths, `~`, and `~/...` using the
daemon user's home. Other paths must be absolute. Paths must be valid
UTF-8, with up to 4096 bytes and no NUL. Results carry the lexically
cleaned path and parent, preserving symlinks in the path. At the root,
the parent is empty. The home is also lexically cleaned. An unknown, relative or
non-UTF-8 home is empty, and paths that need it return
`DIRECTORY_ERROR_INVALID_PATH`.

Listings include subdirectories and symbolic links whose targets are
directories or could not be checked within the probe budget. Dot-prefixed
names are included on request. Names that are not valid UTF-8 count toward
the 20000 raw entries but are omitted from the result. The daemon sorts
directories by name in byte order and returns up to 1000. It uses a
one-entry lookahead to set `truncated` only if more raw entries exist or
it omits entries at the result cap.

Path and filesystem failures are carried in `ListDirectoryResult.error`.
If reading the listed directory has not returned after five seconds, the
daemon replies with `DIRECTORY_ERROR_TIMEOUT`. The blocked directory read
holds its query slot until it returns, including across reconnects.

After reading the directory, the daemon checks symlink targets and
`.git` markers on a best-effort basis using a separate pool of 16
probe slots. Each query can probe until two seconds from its start or
one second before its query deadline, whichever is earlier. If time
runs out or no probe slot is free, it returns the listing with the
markers it has. Unchecked entries get `git_repository = false` and
unchecked symlinks keep `symlink = true`, so a listed symlink may point
to a file and listing it yields `DIRECTORY_ERROR_NOT_DIRECTORY`. Blocked
probes hold only their probe slots until the syscall returns; the daemon
logs a warning when a listing finds every probe slot busy. On
filesystems that do not report entry types (some FUSE filesystems, XFS
without ftype), reading the directory itself stats each entry, so a hung
mount below the listed directory can still make the listing time out.

If a listing exceeds the link's size limits, the daemon reports
`DIRECTORY_ERROR_IO`. Listings larger than a frame use regular fragmentation.

Git remote queries use the same path resolution as directory listings,
with `GIT_REMOTE_ERROR_INVALID_PATH` for an invalid path. The daemon
stats the path and reports `GIT_REMOTE_ERROR_NOT_FOUND` if the path is
missing or points to a non-directory. It runs
`git -C <path> rev-parse --git-dir` to locate a repository, including a
bare repository, then `git -C <path> remote get-url origin` to obtain the
fetch URL with Git's `url.*.insteadOf` rules applied. A missing repository
or origin reports `GIT_REMOTE_ERROR_NOT_REPOSITORY` or
`GIT_REMOTE_ERROR_NO_ORIGIN`, respectively. A missing executable reports
`GIT_REMOTE_ERROR_GIT_UNAVAILABLE`.

Both commands run without a shell or prompts. They share a five-second
budget with path resolution and stat, with independent 64 KiB caps on
stdout and stderr. The daemon reports `GIT_REMOTE_ERROR_TIMEOUT` if the
budget expires and kills the subprocess group on timeout or cancellation.
The query holds its slot until stat returns and the daemon reaps its
subprocesses. The daemon waits up to one more second for an external
adopter to reap any remaining killed group members. The result carries
the trimmed URL, up to 2048 UTF-8 bytes. Scheme URLs have passwords
removed, and HTTP(S) and FTP(S) URLs have all userinfo removed.
SCP-style URLs keep their user. An empty trimmed URL or the literal `origin` is
`GIT_REMOTE_ERROR_NO_ORIGIN`. A dubious-ownership refusal, invalid URL
output or another unexpected failure is `GIT_REMOTE_ERROR_FAILED`, with
details in the daemon log.

Go sender detail, for reference only (`Link.next`): each scheduling round
writes at most one eligible control frame, then one small business frame,
then up to `min(2, max_in_flight_messages)` fragments, rotating between
started large messages. A started fragmented control message rotates like
any large message. A sender is conforming as long as it keeps the rules of
sections 3.2 and 3.3; this round structure is not required.

### Machine updates

UpdateMachine{update_id = 1, version = 2} asks a role that advertised
`machine_update` to run `circus update` for its whole machine: the daemon
its own executable, the worker `CIRCUS_BIN` (or `circus` on PATH), as a
child process in its own process group with the service's environment.
`update_id` is 1-128 bytes of `[A-Za-z0-9_-]` and names the attempt;
`version` is the expected release, informational. It is
`DaemonDownlink.update_machine` (16) on the daemon link and
`WorkerEvent.update_machine` (6) on the worker link's Attach stream; the
other WorkerEvent fields are offer 1, nudge 2, lease_revoked 3,
release_idle 4 and agents_wanted 5. It is a one-shot command, unlike the
WorkerEvent hints: not persisted, acknowledged, coalesced, repeated or
resent after a reconnect. A role runs at most
one such child: a command that arrives while one runs is ignored, and the
next one is taken as soon as the child has ended. The role kills the
child's process group after 4 minutes 30 seconds, and once the child has
exited, whatever it left in its group; it does not stop the child when it
shuts down itself (that shutdown is normally the restart the command asked
for). On success the role reports nothing: the service manager restarts
the services, and a role of the machine connecting with a version at least
the target completes the update. On a non-zero exit, a failure to start or
the deadline, it sends UpdateFailed{update_id, error}
(`DaemonUplink.update_failed`, or `WorkerUplink.update_failed`, answered
as in section 7) with the tail of the command's output, at most 2048
bytes. On both links the control plane keeps the last 2048 bytes of a
longer error and removes control characters other than newlines and tabs;
it ignores a report that does not name the reporter's machine's running
attempt (UpdateFailed{update_id = 1, error = 2}). A failure report, or the
deadline 5 minutes after the start, finds the update done instead when
every role of the machine already reports a version at least the target;
otherwise the report fails it, and the deadline fails it as timed out.

### Workspace preparation

A project location without a path uses the device's default directory
`~/circus/projects/<project_id>`, the project's **workspace**. Two device
calls manage it: `PrepareWorkspaceRequest` (`DeviceCall.prepare_workspace`)
creates it, clones the project's repository into it, or accepts what is
there; `RemoveWorkspaceRequest` (`DeviceCall.remove_workspace`) deletes it.
Only the control plane sends them, with a `session_id` of the form
`workspace-<workspace_id>` that no session has, and only to daemons that
advertised `device_call.prepare_workspace` or `device_call.remove_workspace`
in their current Hello. Results carry `DeviceResult.prepare_workspace` or
`DeviceResult.remove_workspace`; `output` holds one human-readable line and
`is_error` matches `OUTCOME_FAILED`. Daemon failures remain `DaemonFault`s.

#### Daemon

**Paths.** Both calls resolve `path` like `ListDirectoryQuery.path`
(`~/` and absolute paths, valid UTF-8, at most 4096 bytes, no NUL);
`""`, `~` alone, the root and anything else are
`WORKSPACE_ERROR_INVALID_REQUEST`. A `~/` path while the home is unset,
relative or not UTF-8 is `WORKSPACE_ERROR_HOME_UNKNOWN`. Results carry the
lexically cleaned absolute path with symlinks unresolved, once it could be
resolved. A Windows daemon answers `WORKSPACE_ERROR_UNSUPPORTED_OS`.

**Preparation.** `repo_url` is at most 2048 bytes of UTF-8 without control
characters, not starting with `-`, and must be a URL whose credentials can
be located (otherwise `INVALID_REQUEST`). The daemon:

1. creates the parent directories (mode 0755). A non-directory in the way
   is `PATH_CONFLICT`; `EACCES`/`EPERM` is `PERMISSION_DENIED`;
2. takes a non-blocking `flock` on `<parent>/.<name>.circus-lock`. If it is
   held (another preparation or removal, in this daemon or another one on
   the device) the answer is `BUSY`. The kernel releases the lock when the
   daemon dies; releasing it normally also deletes the file;
3. while holding the lock, deletes `<parent>/.<name>.circus-partial-<hex>`
   (exactly 16 lowercase hex digits, the suffix clones get), the hidden
   clones of attempts that died, with the removal walk described under
   **Removal**. No other name matches, so neither lock files nor the
   clones of a workspace whose name starts with `<name>.circus-partial-`.
   If that does not finish before the call's deadline the answer is
   `BUSY`; the next attempt continues;
4. acts on the target. A symlink to a directory is resolved first; the
   clone and rename then happen beside the real directory, under its lock
   as well, and the symlink stays:

| Target | `repo_url` empty | `repo_url` set |
| --- | --- | --- |
| absent | create it → `READY`/`CREATED` | clone → `READY`/`CLONED` |
| file, special file, dangling symlink | `PATH_CONFLICT` | `PATH_CONFLICT` |
| empty directory | `READY`/`USED_EMPTY` | clone, replacing the empty directory → `READY`/`CLONED` |
| non-empty; `git rev-parse --show-toplevel` is the target itself; origin is the same remote | `NOT_EMPTY` | `READY`/`USED_REPOSITORY` |
| non-empty repository root with another origin or none | `NOT_EMPTY` | `REMOTE_MISMATCH`, `error_detail` = the existing origin (sanitized; empty without one) |
| anything else non-empty, including a subdirectory of an enclosing repository | `NOT_EMPTY` | `NOT_EMPTY` |

The daemon never modifies the target's existing contents. A clone runs
`git clone --progress -- <repo_url> <parent>/.<name>.circus-partial-<hex>`
(a full clone) and renames the result onto the target only once complete,
so a repository of the same remote at the target is always a finished
clone. If the rename finds the target no longer empty the answer is
`NOT_EMPTY`; a non-directory there is `PATH_CONFLICT`. Repeating a
successful preparation reports `READY` without changing anything.

**Same remote.** Both the origin and `repo_url` pass
`git -C <target> ls-remote --get-url`, which applies `url.*.insteadOf`
without network access. The results are compared after trimming
whitespace, trailing `/` and one `.git` suffix, lower-casing scheme and
host, dropping passwords (all userinfo for HTTP(S) and FTP(S)) and
default ports, reading `[user@]host:path` as `ssh://[user@]host/path` and
`file:///path` as `/path`.

**git.** git is found on the `PATH` of the environment Bash commands get
(the captured login-shell exports, so credential helpers and
`SSH_AUTH_SOCK` work as in Bash); none, or a file that is not executable,
is `GIT_UNAVAILABLE` (other start failures, such as a parent directory
removed meanwhile, are `IO`). It runs without a shell, in its own session
and process group, with stdin on the null device, without the
repository-locating variables of `git rev-parse --local-env-vars`, without
tracing (`GIT_TRACE*`, `GIT_CURL_VERBOSE`, `GIT_REDACT_COOKIES`), without
anything that could open a graphical prompt (`SSH_ASKPASS`, `DISPLAY`,
`WAYLAND_DISPLAY`), and with `GIT_TERMINAL_PROMPT=0`, `GIT_EDITOR=true`,
`LC_ALL=C`, `GCM_INTERACTIVE=never`, an empty `GIT_ASKPASS` (which also
stops git from using `core.askPass` or `SSH_ASKPASS`) and
`SSH_ASKPASS_REQUIRE=never`. A prompt for credentials therefore fails the
clone (`CLONE_FAILED`) instead of waiting. On Linux git gets `SIGKILL`
when the daemon dies (`PR_SET_PDEATHSIG`); on macOS, which has no such
signal, git outlives a crashed daemon until it ends on its own, and the
next attempt deletes its hidden directory under the lock. Collecting that
environment counts as tool preparation (the Bash preparation budget), not
against `timeout_ms`. When git exits, its group is killed and reaped. A
non-zero clone exit is `CLONE_FAILED`, or `NO_SPACE` if git reported
`No space left on device` or `Disk quota exceeded` (`ENOSPC`/`EDQUOT`
from the daemon's own file operations is `NO_SPACE` too); `error_detail`
carries the end of git's stderr: progress lines folded to their last
state, the last 16 lines, at most 1024 bytes of valid UTF-8. Other
filesystem failures are `IO`, with details only in the daemon log.

**Background.** The control plane sends `timeout_ms` = 120000. If the
clone is still running at the deadline, the daemon turns it into a job of
the call's session (`JobEvent`, command `git clone <sanitized URL>`; its
`output_path` is empty, since git's stderr is not kept in a file) and
answers `OUTCOME_IN_BACKGROUND` with `job_id`. The job
may run for two hours more; then its group is killed (TERM, KILL two
seconds later) and its outcome is `TIMEOUT`. It ends `EXITED` with exit
code 0 when the workspace is ready and 1 when it failed, and `KILLED`
when stopped or expired. `StopSessionJobs` of the session (and daemon
shutdown) stops it with outcome `CANCELLED`.

The daemon keeps a finished job's structured outcome for 30 minutes. A
call with `resume_job_id` naming a job of the same session and path:

- running: waits for it until this call's deadline, then answers its
  outcome, or `IN_BACKGROUND` with the same `job_id` if it still runs;
  cancelling this call stops the job (`CANCELLED`);
- finished and retained: answers the retained outcome at once;
- unknown (another session or path, restart, retention expired): prepares
  as if `resume_job_id` were empty.

`CancelCall` of a foreground preparation kills the clone's group and
answers `CANCELLED`. A stopped clone's hidden directory is deleted after
the answer, still under the lock, so a quick retry may see `BUSY`.

**Limits.** At most two preparations run per daemon, foreground and
background together; more are `BUSY` (retryable, like a held lock). Each
git output stream is captured up to 64 KiB (stderr keeps its end).

**Credentials.** `repo_url` is passed to git as given, since it may carry
the credentials the clone needs; as with a manual clone it is stored in
the clone's `.git/config` and visible in the device's process list. It
never leaves the device otherwise: results, job commands and daemon logs
carry it only with passwords removed (all userinfo for HTTP(S) and
FTP(S)) and its query string replaced by `?[redacted]`. git's stderr has
every occurrence of `repo_url` replaced by the sanitized form, then every
remaining `scheme://user:password@` (quotes in the password included)
and every scheme URL's query string redacted the same way, before it is
cut to size; if the capture lost its beginning, the first, possibly
partial, line is dropped first. The same applies to the existing origin
in a `REMOTE_MISMATCH` detail.

**Removal.** `RemoveWorkspaceRequest` is foreground only and stops at the
call's deadline (the control plane sends 30 seconds). The daemon refuses
(`REFUSED`) anything but a real directory, not a symlink, whose path is
exactly `<home>/circus/projects/<name>` for a single, non-hidden path
element `<name>`, where neither `circus` nor `projects` is a symlink and
the directory is not a mount point. A symlinked home is resolved first. It
takes the preparation's lock (`BUSY` if held), answers `OUTCOME_ABSENT`
when nothing is there, and refuses before deleting anything when
`/proc/self/mountinfo` lists a mount point below the directory (Linux).
It then removes leftover hidden clones of that workspace and deletes the
tree through directory descriptors: each directory is opened relative to
its open parent, and the descriptor that is read is the one that was
checked, so no check can be raced. On Linux the open is
`openat2(RESOLVE_NO_SYMLINKS | RESOLVE_NO_XDEV)`, which rejects a symlink
and stepping onto any mount, same-filesystem bind mounts included; a
kernel without `openat2` (before 5.6) gets `REFUSED` rather than a weaker
check (hidden clones, which the daemon made itself, fall back to the
macOS rule there). On macOS the open uses `O_NOFOLLOW` and the directory
must have its parent's `st_dev`; that covers macOS's mounts (volumes, disk
images, network and FUSE filesystems), as it has no bind mounts. Symlinks
and other non-directories inside are unlinked, never followed. A mount
found by the walk itself stops it: `REFUSED` if nothing was removed yet,
otherwise `IO`. Read-only directories the daemon user owns are made
writable before being emptied; if the removal stops, the directories it
leaves keep their original modes. A directory that writers refill while
it is being emptied is emptied again up to three times, then the removal
fails with `IO`. `EACCES`/`EPERM` is `PERMISSION_DENIED`, anything else
`IO`; if the deadline or a cancellation stops it, the outcome is
`FAILED`/`IO`. Removal fails safe: it stops at the first error, and
`error_detail` (at most 1024 bytes) says what failed, how many entries
were removed before and that the rest remains in the directory.

#### Control plane

Each project and device has at most one default workspace. A session at a
default location creates that row if needed. Sessions at explicit paths use
the existing scheduling path. Removing a location from a project leaves its
workspace and sessions in place.

| State | Control plane behavior |
| --- | --- |
| PENDING | Waits for a device connection, the preparation feature, a free preparation slot, or the end of retry backoff. |
| PREPARING | Runs a device call or follows a background job on that device. |
| READY | Holds the absolute directory and the action taken. The row is final, including after a project repository URL change. |
| FAILED | Holds a structured error. RetryWorkspace or another session created in that default location starts preparation again. |

A workspace session can accept input immediately. It stays IDLE or QUEUED
according to the ordinary session rules. Until its workspace is READY, a
queued session receives no Offer and AcquireLease refuses it. The READY
transaction stores the absolute main-thread cwd for existing sessions and
appends their workspace events. The daemon's READY path must be absolute,
lexically clean, and end in `/circus/projects/<project_id>`. An invalid
path fails preparation with DAEMON_FAULT. After commit, the scheduler
wakes each non-stopped session. New sessions use the stored absolute path directly.

The project owner makes the unleased device call, under a synthetic
`session_id` of `workspace-<workspace_id>`. Workspace operations are
control-plane-only. Preparation uses `PrepareWorkspaceRequest` with the
default path and `timeout_ms = 120000`. The control plane checks
`device_call.prepare_workspace` on the current daemon connection.
Windows devices fail with UNSUPPORTED_OS without a call. Revoked devices
fail with DEVICE_REVOKED.

The control plane commits each call ID and its request snapshot before
submitting. Recovery can resubmit the identical request to attach to the
call or fetch its retained result. After a foreground call transfers to a
job, the control plane keeps the job ID, dispatch boot and registration
time. The job's terminal notification or a scan starts a follow-up call
with a new call ID and `resume_job_id`. A missing job record is given 2 hours and 32 minutes
from when its ID was recorded, covering execution and result retention.
Each attempt sends at most one follow-up due to an expired missing record.
A second missing record gets its own full window, then fails with TIMEOUT.
This bound survives control-plane restarts.

A lost call, missing retained result or lost job starts a fresh attempt.
The third loss in the streak fails with INTERRUPTED. Consecutive retryable
failures back off for 5 seconds times two for each preceding failure in the
streak, capped at 5 minutes. READY, manual retry, and successful preparation
admission after a wait other than backoff reset the counter. Moving into a
background job also resets it. TOOL_UNAVAILABLE waits for a fresh daemon
connection. Background work survives a session stop or deletion, because
the real session and the synthetic call session have different IDs.

At most two workspaces per device, four per user, and 64 globally prepare
at once, including preparations in background jobs. Each preparing
workspace has one runner and at most one subscription. Releasing a slot
prompts another attempt for workspaces queued on the concurrency limits.
Device connections and job-end notifications trigger recovery. A scan runs every 30 seconds
and reads at most 1,000 active rows. Terminal calls are pruned in chunks
once their current write-ahead reference is gone, including after project
deletion.

KIND_WORKSPACE records state changes for each non-stopped workspace session.
When the state stays the same, changes to the waiting reason or
`WorkspaceStatus.background` are ephemeral with seq 0. Each workspace
change advances `updated_at` in Unix milliseconds.
`WorkspaceStatus.updated_at_unix_ms` reports that value. If the clock has not advanced past it, the value
increases by 1 ms. Every durable or ephemeral KIND_WORKSPACE event's
`ts_unix_ms` equals the value for the change it reports. Workspace events
do not move session activity. Payloads are JSON of at most 4 KiB and use
lower-case workspace enum names. Workspace status and events redact
repository credentials. RetryWorkspace resets a failed workspace and both
retry counters. It returns the current state for PENDING,
PREPARING and READY, and also accepts stopped workspace sessions.

Project deletion asks the router to cancel active preparations and stop
the synthetic session's jobs. For the requested devices with READY
workspaces, the control plane sends one unleased `RemoveWorkspaceRequest`
per online device that advertises `device_call.remove_workspace`. Calls
run in parallel with a shared 30-second budget and
`timeout_ms = 30000` on each call. The project cascade proceeds after the
responses or the deadline. A timed-out removal is reported as KEPT,
which means the control plane received no confirmation. No removal is
retried. Workspace records cascade with the project.

### Session ledger snapshots

A session's **ledger snapshot** is a plain directory on the session's
device holding a copy of the project's ledger, which the session's agent
reads and edits with ordinary tools. Four device calls manage it:
`PrepareLedgerRequest` (`DeviceCall.prepare_ledger`) writes it from a base
commit's files, `CollectLedgerRequest` (`collect_ledger`) reports its
changes relative to that base, `RebaseLedgerRequest` (`rebase_ledger`)
applies edits and makes the result the new base, and
`RemoveLedgerRequest` (`remove_ledger`) deletes it. Only the control plane
sends them, with a `session_id` of `ledger-<snapshot_id>`, where
`snapshot_id` is the circus session id, to any connected daemon: a daemon
without these calls answers with a `DaemonFault`, which fails the
snapshot's preparation. Results carry the
matching `DeviceResult` detail; `output` holds one human-readable line and
`is_error` matches `OUTCOME_FAILED`. `error_detail` is at most 1024 bytes
of valid UTF-8. Daemon failures remain `DaemonFault`s.

#### Daemon

**Layout.** Below the ledger directory `$XDG_STATE_HOME/circus/ledger`
(`$CIRCUS_HOME/state/ledger` when that is set; `~/.local/state` when
`XDG_STATE_HOME` is unset or relative):

| Path | Holds |
| --- | --- |
| `<id>/` | the snapshot |
| `.<id>.circus-lock` | the lock serializing calls on `<id>` |
| `.manifests/<id>.json` | its manifest (and `<id>.json.tmp` while it is replaced) |
| `.staging/<id>/`, `.staging/<id>.state`, `.staging/<id>.paths.<n>` | the chunks of a preparation, the last chunk accepted, and each chunk's paths |
| `.staging/<id>.old`, `.staging/<id>.tmp` | a replaced snapshot being removed; a file a rebase is writing |
| `.staging/<id>.aside`, `.staging/<id>.aside.path` | a file a rebase moved aside to check it, and its path and expected hash |
| `.staging/<id>.clock` | the probe file whose timestamp a manifest records |

`cli/` beside them belongs to `circus ledger`; the id `cli` (in any case)
is `INVALID_REQUEST`, as is any id outside 1–128 bytes of
`[A-Za-z0-9_-]`. The directories above the ledger directory are created
as needed and may be symlinks; the ledger directory and everything below
it are opened relative to their open parent with `O_NOFOLLOW`, so a symlink
there is `PATH_CONFLICT`, never followed. A state directory that cannot
be resolved is `STATE_DIR_UNKNOWN`; a Windows daemon answers
`UNSUPPORTED_OS`.

**Manifest.** JSON: for each base file its path, size, modification time
and inode change time in nanoseconds, inode and lowercase hex SHA-256, in
path order, plus the base commit and `written_at_ns`, a timestamp of the
snapshot's own file system taken from a probe file in `.staging/`, outside
the snapshot, so a daemon killed while the probe exists never leaves it
where a collection would see it. It is replaced atomically (temporary
file, fsync, rename, fsync of the directory). A snapshot exists exactly
when its manifest does: a directory without one is `ABSENT` to collect and
rebase, and a manifest without its directory (removed from outside) is
removed by the next ledger call that can take its id's lock. Every
directory the daemon creates is followed by a sync of its parent; syncs
use `F_FULLFSYNC` on macOS.

**Paths and content.** A path is relative, `/`-separated, valid UTF-8
without control characters, at most 4096 bytes, with no empty, `.` or `..`
segment and no segment over 255 bytes. Content is valid UTF-8 of at most
256 KiB, except the ledger's generated index, `INDEX.md` at the top (every
snapshot holds it; that of the empty ledger, base commit Git's empty tree,
is an empty table saying there are no decisions yet), which
may hold up to `MaxIndexBytes` (64 + 4096 × 2848 bytes, about 11 MiB: a
row per decision whose title's `<`, `>` and `&` take five bytes each) and
does not count toward a chunk's or a rebase's 8 MiB of content. Anything else, duplicate paths in one request, or a base commit
that is not 40 or 64 lowercase hex digits is `INVALID_REQUEST`. On macOS,
whose file systems usually take names differing in case or Unicode
normalization for one, two requested paths (or their directories) that
would be one name (canonical caseless matching) are `INVALID_REQUEST`
naming them, within a request, across a preparation's chunks, and for a
rebase's written paths against those already in the snapshot (a file an
edit deletes does not count, so a case-only rename works). Elsewhere such
names are prepared like any others, and the last chunk's check that the
file system holds exactly the requested paths catches a file system that
folds them anyway. The ledger's validation refuses such names in
changesets (a problem naming both paths), so a ledger written through
changesets holds none.

**Preparation.** Chunks hold at most 4096 files and 8 MiB of content.
Chunk 0 discards any staging of the id and starts a new one; chunk *n*
must follow chunk *n*−1 of the same base, or repeat the last chunk
accepted (its files are written again); any other index is
`INVALID_REQUEST` and changes nothing. Files are written and fsynced into
`.staging/<id>/`, never through a symlink, and each chunk's paths are
recorded. The last chunk checks all the chunks' paths together: they must
be `total_files` (at most 8192) distinct paths, free of the collisions
above, which with their directories make at most 8192 entries; anything
else is `INVALID_REQUEST`. It then reads the staged files back into the
manifest, which must hold exactly the requested paths (another file
system's rules may still merge names), else `INVALID_REQUEST`. It removes
the old manifest (whether or not a directory is there) and the chunk
state, moves an existing snapshot aside, renames the staging directory
onto `<id>`, writes the new manifest last and deletes the old directory
without following symlinks. A last chunk sent again after a failure past
the chunk state's removal is `INVALID_REQUEST` "start again from chunk 0". Before staging anything every chunk checks the
existing snapshot: the same `base_commit` without `reset` answers `READY`
at once and changes nothing; another base without `reset` is
`INVALID_REQUEST`; a symlink or non-directory at `<id>` is
`PATH_CONFLICT` unless `reset` (it is then unlinked, never followed); a
directory without a manifest is replaced without `reset`. Staging of any
id untouched for an hour is removed by the next ledger call that can take
that id's lock.

**Collection.** The daemon compares the directory with the manifest as
git compares a work tree with its index (with `core.trustctime`, git's
default): a regular file whose size, modification time, inode change time
and inode equal its entry is unchanged and is not opened; any other file
is read and hashed, and so is every file whose modification time is not
earlier than `written_at_ns` (racy). The inode change time, which no one
can set, catches a same-size rewrite whose modification time was set back. Manifests
are taken once the file system clock has moved past the files written
(waiting at most 50 ms), so a fresh snapshot has no racy file. Dot files
are ordinary files. Directories are opened with `O_NOFOLLOW`; other
entries are examined without following them and regular files are opened
with `O_NOFOLLOW|O_NONBLOCK` and checked again on the descriptor, so a
symlink, FIFO, socket or device is a problem and is never opened. Bounds:

| Bound | Beyond it |
| --- | --- |
| 8192 directory entries scanned | a problem of the whole snapshot (empty path), `too_large`, no changes, nothing reverted |
| 128 directory levels, 4096-byte paths | a problem at that path |
| 256 KiB per file, `MaxIndexBytes` for `INDEX.md` (reads stop one byte beyond) | a problem with the file's size |
| 4096 changes, deletions included | a problem of the whole snapshot, `too_large`, no changes |
| 8 MiB of changed content | `too_large`; every change has path, hash and size, `known` set, no content |
| 1024 problems | the first 1024 in path order |

Invalid UTF-8 content or names, and names with control characters, are
problems too. A base file at or below a problem path is not reported as
deleted. A change equal to its `known` entry (same SHA-256, or both
deleted) comes with `known` set and no content; `reverted` lists the known
paths that are no longer changed and are not problems. Every result made
while a manifest exists names its base commit in `current_base_commit`,
so the control plane reads the changes against the base they are relative
to.

**Rebase.** A rebase whose `base_commit` the manifest already records was
done by an earlier attempt whose answer was lost: it is `REBASED` when the
snapshot shows no change, and otherwise `STALE`; its edits are never
applied again, since any change since then (even one back to a file's
collected content) is the agent's. The result's `current_base_commit`
names the base the manifest records when the call ends, on every outcome
where a manifest exists: the request's `base_commit` when the snapshot is
on it, the kept base otherwise.

Otherwise `known` is the complete set of changes against the current
base that the control plane collected and merged. Before anything changes,
the daemon compares the snapshot with its manifest as a collection does
(hashes only) and requires, for every path, its collected state (its
`known` change, or the base's content for a path without one) or, on a
path with an edit, the edit's target (its content, or nothing for a
deletion); and every edit must find `expected_sha256` (empty: nothing at
the path) or its target. Any other state, and any problem a collection
would report (a symlink or other non-regular file, a file over 256 KiB,
invalid UTF-8, too many entries), is `STALE` and nothing changes: the
agent changed the snapshot after the collection the merge used, and
**a STALE rebase means "collect again"**, merge what is new and send the
rebase with the new `known`. Accepting an edit's target is what lets the
same request finish a rebase that stopped after some edits.

The edits are then applied in order, each only if its file still holds
what the edit expects, checked again right before and enforced by the
file operations themselves, since the agent may be writing meanwhile. A
file to replace or delete is first renamed to `.staging/<id>.aside` (its
path and expected hash recorded in `.staging/<id>.aside.path` beforehand,
and never while an earlier one is there) and hashed there; anything but
the expected content is renamed back. A new file, written
and fsynced as `.staging/<id>.tmp`, is installed only where nothing
exists: `renameat2(RENAME_NOREPLACE)` on Linux, `renameatx_np(RENAME_EXCL)`
on macOS, else a hard link (which fails on an existing name) and an
unlink, else a check followed by a plain rename. So a file the agent
rewrote, deleted or created on an edit's path is never replaced or
removed: the rebase stops there with `STALE`, the edits before it stay
applied and the old manifest stays. The next collection reports the
agent's write (and the applied edits, as changes against the old base);
the same request sent again is `STALE`, as that path holds neither its
collected state nor its target; a rebase built from the new collection
finishes. What remains is a write through a descriptor the agent opened
before the file was renamed aside, landing in the moved file: before the
moved file is hashed, it is hashed with it and renamed back; after (within
microseconds of the moved file being discarded or replaced), it is lost,
as with any rename of a file someone holds open. Directories are synced
once, after all renames.

A daemon killed with a file aside leaves the journal, and the next
collection or rebase recovers it before anything else, never recreating
a directory the agent removed:

- the path is free: the file goes back;
- the path is taken (by the edit's target, or by the agent's newer file
  or directory): a file holding the expected content, which the control
  plane already has, is dropped; anything else is renamed beside it to
  `<name>.conflict-<first 12 hex digits of its SHA-256>`;
- a directory on the way is gone or is no longer a directory: the same,
  but the conflict file goes into the nearest directory that is still
  there;
- the journal cannot be read or parsed: the file goes to the snapshot's
  top as `recovered.conflict-<…>`.

A conflict file is an ordinary file of the snapshot: the agent sees it,
and the next collection reports it as an added file, which ledger
validation reports as a file that is not a decision, for the agent to
resolve. Nothing of the agent's is dropped, and no recovery fails a call.

Finally the daemon reads the whole directory into a new manifest with the
request's `base_commit`. That manifest must be exactly the merged view
(the base, then `known`, then the edits), else the answer is `STALE` with
the edits applied and the old manifest kept, so a file the agent wrote
while the rebase ran is collected next rather than made part of the base.
The manifest is written last: a crash before it leaves the old one, the
next collection reports the applied edits as changes against the old
base, and the same rebase sent again completes it.

**Removal.** The daemon refuses (`PATH_CONFLICT`) a symlink or
non-directory at `<id>` and removes nothing. Otherwise it removes the
manifest first, then the directory (with the removal walk of "Workspace
preparation", which never follows a symlink or enters another mount), then
any staging of the id: `OUTCOME_REMOVED`, or `OUTCOME_ABSENT` when none of
them existed.

**Concurrency and resources.** A non-blocking `flock` on
`.<id>.circus-lock` serializes the calls on one snapshot (`BUSY` when
held); at most two ledger calls run at once per daemon (`BUSY` beyond).
`EACCES`/`EPERM` is `PERMISSION_DENIED`, `ENOSPC`/`EDQUOT` `NO_SPACE`, a
symlink or file in the way `PATH_CONFLICT`, anything else `IO`. One
collection holds at most 8 MiB of changed content plus one read buffer
(256 KiB, or about 11 MiB while the generated index is read), the manifest and the change and problem lists (a few megabytes for
a real ledger; with every path at 4096 bytes the bound is about 100 MiB).
A chunk holds its request (at most 8 MiB of content, plus the generated
index of up to about 11 MiB) and the last chunk
reads the staged files back one at a time. Disk holds one copy of the
ledger per session, its manifest and, while a preparation or reset runs,
a staged copy.

## 7. Worker link operations

Every `SubmitCall`, `CallQuery`, `CancelCall`, `Attach`, `SessionRequest`,
`ResumeLeases` and `ListDevices` opens one **operation** named by the
worker envelope's `request_id`; `Unwatch` names an open operation.
Among the new session operations, only Attach is a stream; each of the
others gets exactly one SessionReply. SubmitCall retains its existing stream.
`CancelCall` and `CallQuery` are the messages the daemon link uses too, so
the ID lives only on the worker envelope.

**Request IDs** (`workerlink.operations`):
- Chosen by the client: nonzero, and a client MUST NOT reuse a request id
  on a connection. A per-connection counter starting at 1 satisfies this;
  the Go client does exactly that.
- Why never reuse: after `Unwatch`, an update of the old operation may
  already be on its way (the server may have released the slot and be
  sending the last reply, or have a Submit update past its last check).
  Under a reused id that update would be routed to the new operation: the
  client would treat it as the new operation's reply or terminal update and
  end it locally while the server still holds its slot, so the client's
  count of open operations would fall below the server's and a later
  operation would trip the cap and close the link.
- On any business uplink payload, `request_id = 0` closes the link;
  so does an opening payload whose ID names an operation that is
  still open (the server can only detect reuse of an open id).
- Every `CallUpdate`, `SessionReply` and `WorkerEvent` carries the `request_id` of the
  operation it answers (two operations on the same call get separate
  updates). WorkerEvent uses the Attach request_id. Hello, Welcome, Reject,
  Heartbeat and Fragment frames carry 0.
- A client drops any business downlink whose `request_id` is not an operation it
  has open (unwatched, already answered, or never opened).

| Uplink payload | Effect | Replies (each with the operation's `request_id`) | Operation ends |
| --- | --- | --- | --- |
| `SubmitCall{device, call, session_epoch, tool_use_id}` | Submit a `DeviceCall` to the user's device named `device` (1-128 bytes). `call.call_id` and `call.session_id`: 1-128 bytes of `[A-Za-z0-9_-]`; `agent_id`: empty or the same grammar; exactly one tool. | A stream of `CallUpdate`s: zero or more nonterminal updates, then one terminal update (unless the link closes or the operation is unwatched first). | At its terminal update, or at Unwatch. |
| `CallQuery{call_id}` | Read the call's current state. | Exactly one `CallUpdate` (may be nonterminal, e.g. `unknown`, or `rejected NOT_FOUND`). | At that reply (or Unwatch). |
| `CancelCall{call_id}` | Cancel before dispatch; after dispatch record the intent (the daemon's result wins). The intent is recorded in receive order, so it cannot overtake an earlier Submit of the same call. | Exactly one `CallUpdate`: the call's state read after the intent was recorded (the call may have finished in between). `cancelled`: cancelled before dispatch. `dispatched` or `unknown`: intent recorded, the daemon's result decides. A terminal outcome (`result`, `lost`, ...): the call had already finished; `unknown` with reason `DEVICE_OFFLINE` when it finished but its result cannot be read now. `rejected NOT_FOUND`: no such call for this user; `rejected INVALID`: invalid id. | At that reply (or Unwatch). The intent stays recorded either way. |
| `Attach` | Advertise 1..64 session capacity, SDK/account/check metadata, `local_only` and the state of every agent the worker supports (`agents`, at most 16, one per kind); at most once per connection, even after Unwatch. `local_only`: this connection runs only local-mode sessions pinned to its worker (Scheduling below). | WorkerEvent stream: first AgentsWanted (the user's wishes for the worker's agents, complete, each with the count of the user's install requests, `attempt`; sent again whenever they change; a worker retries a FAILED install only for a greater attempt), then Offer, Nudge, LeaseRevoked, ReleaseIdle and UpdateMachine (only on a connection whose Hello listed `machine_update`; see "Machine updates"), starting with every Offer still pending for this worker. Hints (every event except UpdateMachine) are level-triggered, may repeat or coalesce, and carry no authoritative state; UpdateMachine is a one-shot command, sent once and never repeated. Account is recorded only. | Unwatch or disconnect. |
| `SessionRequest{session_id, epoch, op}` | One of the session operations below; Data with key `session/<session_id>`. | Exactly one SessionReply, success, Fenced or Rejected. | At that reply (or Unwatch). |
| `ResumeLeases{leases}` | Reattach up to 64 (session_id, epoch) leases on the same worker boot before expiry, rebinding each to this connection. | One SessionReply.resumed, one outcome per requested lease, with retained epoch (and `stopped`) on success or current epoch when fenced. | At that reply (or Unwatch). |
| `ListDevices{}` | List the user's devices. | One SessionReply.devices, at most 256 devices with name, online, os, arch, hostname, last_seen_unix_ms. | At that reply (or Unwatch). |
| `ReportAgents{agents}` | Replace the worker's reported agent states (same rules as `Attach.agents`); sent on every change, at most once a second for install progress. A session is offered to a worker only while it reports the session's agent INSTALLED at its target version and the user still wants it. | One SessionReply: ok, or `rejected INVALID` for a field rule, a replaced boot, or a connection older than the worker's latest attached one (a late report there is older than that Attach's). An Attach of a replaced boot closes the link. | At that reply (or Unwatch). |
| `UpdateFailed{update_id, error}` | Report that `circus update` run for the UpdateMachine `update_id` failed (see "Machine updates"). | One SessionReply: ok once recorded, or ignored as not the machine's running attempt; `rejected INVALID` for an `update_id` that is not 1-128 bytes of `[A-Za-z0-9_-]`; `rejected STORE_UNAVAILABLE` when it could not be recorded. | At that reply (or Unwatch). |
| `Unwatch{}` | Stop delivery for the open operation named by `request_id` (normally a Submit; on a Query or Cancel it abandons the reply). Never cancels the call. Unknown or finished ids: no-op. | **None.** Updates already in flight may still arrive and are dropped. | Immediately. |

`CallUpdate` variants:

| Variant | Terminal (`wire.Terminal`) |
| --- | --- |
| `accepted`, `dispatched` (also used for "running") | no |
| `unknown` (dispatched, outcome open; e.g. reason `DEVICE_OFFLINE` from Query) | **no**: a Submit stream continues; a result can still arrive |
| `result` (a `DeviceResult`) | yes |
| `offline` (device offline, with the user's online device names) | yes |
| `lost` (e.g. reason `RESULT_UNAVAILABLE`) | yes |
| `rejected` with `INVALID`, `OVERLOADED`, `CALL_MISMATCH`, `UNKNOWN_DEVICE`, `STORE_UNAVAILABLE`, `DEVICE_SHUTTING_DOWN`, `SESSION_FENCED`, `SESSION_STOPPED` | yes |
| `rejected` with `NOT_FOUND` | **no**: it ends only the Query or Cancel it answers (no call with this id is visible to this user) and is not a call outcome |
| `cancelled` | yes |

"Terminal" is a property of the call. An operation ends at its last reply:
for a Submit the terminal update, for a Query or Cancel its one reply,
whatever its variant.

Semantics:
- Re-submitting the same `call_id` with identical content for the same
  device attaches to the existing call; different content or device yields
  `CALL_MISMATCH`. On the worker link `tool_use_id` is part of that
  identity; `session_epoch` is not, because every submission passes the
  lease check on its own (session contract below).
- **Per-session call caps:** a session has at most 16 non-terminal calls and
  at most 1024 calls not yet persisted. A submission beyond either gets
  `rejected OVERLOADED`: terminal for this submission, nothing is recorded,
  and the same `call_id` may be submitted again later.
- **Per-connection operation cap: 64** (`workerlink.maxOperations`). Every
  open Submit, Query, Cancel, Attach, SessionRequest, ResumeLeases and ListDevices
  holds a slot. Attach occupies one slot for its entire stream; even with
  `max_sessions = 64`, a worker has only 63 other operation slots while
  attached. The server releases a slot
  *before* sending the operation's last reply, so a client may start a new
  operation as soon as that reply arrives. `Unwatch` releases the slot while
  the server reads it, before it reads the next message, so an operation
  sent after an `Unwatch` always finds that slot free. An operation beyond
  the cap **closes the link** (it is not rejected); a client must count its
  open operations and stay within the cap. Because the receiver processes
  messages in arrival order, the client must make sure `Unwatch` arrives
  before the operation that reuses its slot: it is a Barrier (section 3.2),
  and a single FIFO send queue satisfies this.
- **Slot reservation (worker rule):** a Submit holds its slot until its
  terminal update, which can take minutes. A worker keeps at most **48**
  call operations (Submit, Query and Cancel together, including the
  CallQuery polls for owed notices) open per connection and queues further
  ones locally (it MAY also Unwatch a long-running Submit and poll it with
  `CallQuery`). The remaining 16 slots stay free for Attach and 15 session
  operations (SessionRequest, ResumeLeases, ListDevices); otherwise
  long tool calls can hold every slot, transcript appends wait past the SDK's
  retry budget, and the session becomes BROKEN.
- **Connections:** at most 8 authenticated worker-link connections per
  control plane by default (`worker_connections` in the server config), of any kind (worker or
  CLI), and at most 4 per credential; the next gets HTTP 503. CLIs that use
  the worker's credential count toward its 4. A worker that restarts while
  its old connection is still half-open (until the server's heartbeat
  timeout) therefore still gets in, and one busy credential cannot lock
  out another.
- **Server-side flow control (informational):** updates (including Query
  and Cancel answers) wait in the server until they can be sent. Updates
  whose decoded cost (section 5) is at most 256 KiB go out at once; larger
  ones wait for one of 4 send permits shared by all worker connections
  (`workerlink.pushPermits`). Waiting holds the operation's slot, and
  `Unwatch` still frees it at once. Normally this is only latency, but if
  the server's shared send pool stays saturated (slow readers holding it),
  a large update can wait long enough to exceed the 15 s update-buffer
  stall below, and its connection is closed even though this worker reads
  promptly. Every router-side holder of one settled or published update,
  including subscribers and query waiters, shares one immutable clone and
  one charge toward the router's global 256 MiB buffered-update budget.
  Each subscription still has its own limits.
  Recovery is the usual reconnect and `CallQuery`.
- The server **closes the link** (abort, no notice, reason logged) when: a
  request id is zero or names an open operation; the operation cap is
  exceeded; a rejection would carry an empty `call_id` (a Submit whose
  `call_id` is invalid, or a Query or Cancel with an empty one; a nonempty
  invalid id gets `rejected INVALID`); the router reports overload or an
  ambiguous store failure; a Submit names a `call_id` owned by another
  user; the server's send queue overflows because the worker reads too
  slowly (256 messages or 2 × M bytes, `Options.defaults`); a call's update
  buffer overflows (16 updates, 144 MiB, or a 15 s stall: `calls.Config`
  defaults), because the worker reads too slowly or because a large update
  waited too long for a send permit (above); a send stays refused by the shared memory pool for 10 s
  (`workerlink.sendBusyLimit`); an unexpected payload arrives; or the
  credential is revoked. Recovery is always: reconnect, then `CallQuery`.
  Closing a connection never cancels a call.
- WorkerDownlink field 11 and name `job_event` are reserved; WorkerJobEvent
  is removed. The server sends no GoAway on this link.

### Session processing contract

The control plane implements this contract in `internal/controlplane/sessions`
(scheduling, leases, every session operation) with `workerlink` dispatching
to it and the call router admitting session calls. Writes, Attach and
ResumeLeases run inline in the connection's receive loop, in receive order,
each in one store transaction bounded by 5 s (`STORE_UNAVAILABLE` past it);
reads (LoadTranscript, ListTranscriptKeys, GetSessionState, ListDevices and
AcquireLease's Lease reply) run off the loop, at most two at once across all
connections.
The user-facing Connect SessionService (the user-side API below) is served
by `internal/controlplane/webapi`.

#### Local mode

A session is local (`SessionConfig.local`) when it is pinned to a worker
on the same machine as its default device: the worker runs Claude Code's
built-in tools there, directly, in the `cwds` entry of `default_device`,
instead of submitting device calls. CreateSession decides it (user-side API
below); local mode and the pin are stored with the session and never
change, and `Lease.config` carries `local`. The control plane expects no
SubmitCall from a local session but does not refuse one: it passes the
same fence and admission as any session call. Everything else in this
contract (leases, fencing, transcript, events, inputs, turns, approvals,
stop) is the same for local sessions. The daemon still prepares the
working directory (the session is leased only once it is ready) and
serves the path picker.

#### Providers and the launch environment

A session names a provider (`Session.provider_id`) or none, which means
local login: the worker's own Claude Code login and environment. A
provider belongs to a Web user and carries an API endpoint
(`ANTHROPIC_BASE_URL`, `ANTHROPIC_API_KEY` or `ANTHROPIC_AUTH_TOKEN`, and
the `ANTHROPIC_DEFAULT_{FABLE,OPUS,SONNET,HAIKU}_MODEL` aliases) plus custom
variables. The control plane stores its secrets encrypted and never returns
them on the user-side API.

- **Scheduling.** A session with a provider is offered only to workers the
  provider is assigned to, and only while fewer than its `max_sessions`
  sessions hold leases across the control plane. AcquireLease checks both
  again in its transaction (`PROVIDER_UNAVAILABLE`, `PROVIDER_FULL`); the
  lease records the provider and counts until it ends. A session that
  cannot be leased stays QUEUED with a `queue_reason`, announced by
  ephemeral `KIND_QUEUE` events (`{"queue_reason":"<name>"}`). While
  sessions wait as `PROVIDER_FULL`, the control plane asks holders of the
  provider's idle leases to give them back (`WorkerEvent.ReleaseIdle`),
  one per waiting session that a live connection of a worker it may run
  on (its pin and local mode honoured, the worker assigned) could take,
  beyond the requests still outstanding (30 seconds; a request queued on
  a connection that closes or detaches no longer counts), least recent activity first, only for a lease bound to the
  holder's live connection with no Nudge reason pending. Idle there means
  state IDLE, no input pending or taken, no pending TOOL approval (a pending question does not count), no ended
  job whose notice is not yet persisted, no background task recorded at
  the lease's epoch and no ledger merge or discard still to apply. It asks
  when a session is found waiting, when a held session's turn or
  background task ends, and on every fallback pass while one waits; a
  turn's start voids the request outstanding for its lease. Like
  `worker_disconnected_at_unix_ms` and its `KIND_WORKER` events
  (**Disconnected holder** under "Fencing and leases"), it is kept in
  memory and filled on every
  Session that GetSession, ListSessions and the writes return.
- **Launch.** Before every CLI start (new session, resume, replacement
  query) the holder calls `GetLaunchEnv`. With a provider it removes the
  inherited authentication and endpoint variables, then sets the returned
  ones; with `local_login` it keeps its environment. The reply holds
  secrets and is never logged or published. `PROVIDER_UNAVAILABLE` means
  start no CLI and release with `REASON_PROVIDER`.
- **Model.** An empty `SessionConfig.model` starts the CLI without a model,
  so its default alias applies through the provider's alias variables.
- **Changes.** Provider edits reach the next CLI start; running CLIs keep
  their environment, and lowering `max_sessions` or unassigning a worker
  revokes no lease. Changing a session's provider (UpdateSession) is
  refused while a turn runs; with an idle lease it revokes the lease and
  the session is scheduled again under the new provider.

#### Fencing and leases

Session ids are 1-128 bytes of `[A-Za-z0-9_-]`. Every SessionRequest has
exactly one op; a second `op` field closes the link (section 5).
AcquireLease uses epoch 0; every other op requires a nonzero epoch.

The **fence** is (session, epoch, worker, boot, connection): an unexpired
lease at that epoch, held by this connection's worker and boot, and bound to
this connection. The check and any mutation MUST occur atomically. A
mismatch returns `Fenced{epoch}` with the current epoch and no mutation.
A SubmitCall MUST carry a nonzero `session_epoch` and the model's opaque
`tool_use_id` (1-128 UTF-8 bytes), and passes the same fence for
`call.session_id`. Otherwise `rejected SESSION_FENCED` is terminal: this
submission was neither recorded nor executed (a call recorded by an earlier
submission of the same `call_id` is unaffected). A stopped (or broken)
session gives `rejected SESSION_STOPPED`, also terminal and unrecorded. A
`tool_use_id` over 128 bytes is `rejected INVALID`. A SubmitCall with
neither field (the call CLI's) is not a session call: it is routed as
before, unless `call.session_id` names an existing session, which gives
`SESSION_FENCED`.

**Connection binding.** A lease is bound to the connection that acquired or
last resumed it. ResumeLeases and an idempotent AcquireLease rebind it to the
calling connection; from then on, session operations and SubmitCalls for it
on any other connection of the same boot are fenced. This orders a reconnect
against a half-open old connection whose handler may still be applying an
older write: once the new connection has resumed, nothing more from the old
one commits. A worker therefore sends session operations only on its newest
connection and ignores replies on a connection it has abandoned. Connection
ids are internal to the control plane and never appear on the wire.

**Reconnect order.** Attach and ResumeLeases are barriers (section 3.2), and
the control plane processes both inline, in receive order. After a
reconnect the worker sends Attach, then ResumeLeases, and only then queues
the session operations and SubmitCalls it resends; the barriers keep them
behind the rebinding, so none of them is fenced. The control plane defers
re-sent Nudges until the connection has an Attach stream.

**Lifetime.** A lease lives 60 seconds. The control plane extends every lease
bound to a live connection every 20 seconds, without a renewal message; a
lease bound to a closed connection expires naturally (expired leases are
released within 5 seconds). After a control-plane
start, no lease expires sooner than one lifetime later, so holders can
reconnect and ResumeLeases even after a longer outage. Workers reconnect with
backoff capped at 10 seconds. A new worker process uses a new `boot_id`; the
control plane then releases every lease of the previous boot.

**Disconnected holder.** The control plane cannot tell a dead worker process
from a broken network, so a lease outlives its holder's connection for the
rest of its lifetime: it is no longer extended, so it expires 40 to 60
seconds after the disconnect (its last extension was at most 20 seconds
before), and is released within 5 seconds more. A worker that reconnects
and resumes it within that window keeps its turn. Users are told instead:
when the last connection of a
worker boot closes, every session whose lease that boot holds without a
live connection gets `Session.worker_disconnected_at_unix_ms` set to the
close time, announced by an ephemeral `KIND_WORKER` event
(`{"disconnected_at_unix_ms":"<int64>"}`). It returns to 0, announced with
`"0"`, when the lease is bound to a live connection again (ResumeLeases, an
idempotent AcquireLease; a boot that reconnected but has not resumed the
lease yet keeps the mark) and when the lease ends for any reason (expiry,
release, revocation, a new boot); a turn still running at expiry ends
interrupted. The value lives in the control plane's memory only: after a
control-plane start it reads 0 until a holder's connection closes again.

**AcquireLease is idempotent for the holder.**
- If this worker and boot already hold the unexpired lease and `fresh` is
  false, it skips the Offer check, keeps the epoch, rebinds the lease to
  this connection and returns the same lease with `reattached = true`, even
  for a stopped session (with `state.stopped` set). Nothing is requeued.
- Otherwise it requires a current Offer to this connection, unless this
  worker and boot hold the lease (else `NOT_OFFERED`), and refuses a stopped
  session (`STOPPED`). A `fresh` acquisition by the holder of a stopped
  session also releases that lease atomically, so a worker without local
  state never keeps a stopped session's lease it cannot release. It atomically increments the epoch, binds worker,
  boot and connection, and applies the recovery requeueing below.

A worker sets `fresh` whenever it holds no local state for the session (no
epoch, `local_seq` or `batch_seq` counters, no pushed-input set), for
example after it discarded the session or never learned the epoch. Its
counters then start at 1 in a new epoch instead of colliding with the old
epoch's deduplication (a reattached `batch_seq` 1 would be acknowledged
without being written). A lost AcquireLease reply is therefore retried with
AcquireLease, also after a reconnect: the retry finds the lease held by this
boot, and with `fresh` advances the epoch once more.

After a successful AcquireLease (fresh or reattached) the worker calls
TakeInputs: the Lease reply carries the state, stopped flag and interrupt
level, but not the inputs, and a pending INPUT or APPROVAL reason is not
cleared by the acquisition. Reasons still pending when the Lease reply has
been handed to the link are nudged right after it. A lease is extended
while its AcquireLease operation is open, however long its reply waits;
once the operation has ended without the reply reaching the link (the send
failed, the link closed) or was unwatched, the lease is no longer extended
and expires, unless the worker resumes or acquires it again.

A worker that unwatched an AcquireLease MUST treat the session as possibly
held by it (the reply may have been on its way when the Unwatch was sent;
the client drops it): before it gives the session up, it retries
AcquireLease with `fresh` and then releases what it got. The control
plane's marking of unwatched acquisitions is best effort (it remembers
only each connection's last 16 AcquireLease operations), so a lease the
worker never learned of may stay held until it expires or the connection
closes.

A worker sends at most one AcquireLease per session at a time. A session
with an AcquireLease in flight counts as held: hints for it are ignored
until the reply arrives. A Lease reply can be large and fragmented, so a
Nudge sent after it often arrives first; answering that Nudge with a second
`fresh` acquisition would advance the epoch again and fence the first.

**ResumeLeases** takes up to 64 (session_id, epoch) pairs held by this boot,
before expiry. Each outcome is independent: `ok` with the retained epoch
(and `stopped` if the session is stopped; it still resumes so the holder can
flush), or not ok with the current epoch. Each resumed lease is rebound to
this connection.

**ReleaseLease**: close the SDK query and confirm its CLI has exited first.
Repeating it at the released epoch returns `ok.deduplicated = true` until
another epoch is acquired, `Fenced` afterwards; both mean the lease is gone.

On `Fenced` (on the current connection), a failed resume, or `LeaseRevoked`
for the held epoch, the worker MUST close the SDK query, confirm the CLI
process has exited, Unwatch outstanding call operations without Cancel, and
discard local session state.

**CancelCall is not fenced by a session lease.** Closing an SDK query fires
the abort signal of every in-flight tool handler. A handler MUST NOT turn
that into a CancelCall when the query is closing because of fencing, lease
loss, stop or release: the next holder recovers those calls. It Unwatches
them instead. CancelCall is only for a cancellation the user caused (an
interrupt) while the lease is held.

#### Hints

WorkerEvent hints (every WorkerEvent except UpdateMachine, a one-shot
command; see "Machine updates") are level-triggered: they may be coalesced
or repeated, carry no authoritative state, and never replace the fence. The control plane
keeps each session's pending reasons until the holder acts on them
(TakeInputs clears INPUT and APPROVAL; GetSessionState including jobs clears
JOB; ReportTurn ENDED clears INTERRUPT; a delivered Lease reply clears JOB,
INTERRUPT and STOP, which it carries; otherwise STOP stays until the lease
ends). A lease's first resumption after a control-plane restart adds INPUT
and JOB, and INTERRUPT when the running turn is at or below the stored
interrupt level, since earlier hints may be lost. Every Nudge carries the
session's stored interrupt level (also after a resumption). The control
plane re-sends:
- after `Attach`, every Offer still pending for this worker on the
  attaching connection: an Offer held by another connection of the same
  worker and boot moves to it;
- after each successful ResumeLeases outcome, that session's pending Nudge
  reasons.

**Scheduling.** A session with pending work and no live lease is offered
to its pinned worker, or else to the user's attached connection (of the
worker's newest boot) that is not full (leases plus Offers below
`max_sessions`) and has the fewest leases and Offers; with none it stays
queued until a connection attaches or frees capacity. A pinned session,
local or not, waits while its worker has no such connection; it is never
offered to another worker. A connection attached with `local_only` is
offered only local sessions pinned to its worker (`SessionConfig.local`,
below); every other session goes to the worker's other connections or
waits. The same rules hold for every way to a lease: AcquireLease (with an
Offer, or for a lease this boot already holds, fresh or reattaching) and
ResumeLeases on a `local_only` connection refuse a session that is not
local (`NOT_OFFERED`, or a resume outcome not ok with the current epoch;
the lease stays bound where it was), and an Offer moved at Attach from
another connection of the same boot moves only to a connection that may
run the session. The store refuses a lease of a pinned session to any
other worker. `local_only` takes effect at Attach: session operations a
connection sends before its Attach, ResumeLeases and AcquireLease
included, are not filtered by it (such a connection can still resume or
reattach a lease its boot holds of a session that is not local). A
local-only worker therefore sends Attach first on every connection, as
the reconnect order above requires. It treats every resume outcome that
is not ok as a lost lease, whatever the epoch it carries: a refused
non-local session reports the worker's own epoch while the lease stays
bound to the connection that held it. An Offer stays
reserved for its connection for 30 seconds; then the fallback scan (every 30
seconds) offers it again, possibly elsewhere. An AcquireLease refused for any
reason withdraws an Offer the session no longer needs.

| Hint | Worker action |
| --- | --- |
| Offer, session not held | AcquireLease, with `fresh` when the worker has no local state for the session. If the session is being released, remember the Offer and act on it after the ReleaseLease reply. |
| Nudge, session not held | AcquireLease with `fresh` (NOT_OFFERED unless this boot still holds the lease). |
| Any hint, AcquireLease in flight | Ignore it until the reply arrives; the session counts as held. |
| Nudge INPUT or APPROVAL | TakeInputs (an approved request arrives as a `KIND_APPROVAL` input, an answered question as a `KIND_ANSWER` input after a Nudge INPUT). |
| Nudge JOB | GetSessionState with `SECTION_JOBS`. The control plane registers a session call's background job with the session (`SessionState.jobs`) when the call's result reports it (`BashResult.job_id`), or, for a call settled lost without its result, when it is settled lost (from the job already stored for the call's device and boot), when a late result arrives, or when a JobEvent or snapshot entry of the job from that device and boot names the call; only calls admitted under the session's lease (`tool_use_id`) of the session's own user register. Its end, by event or snapshot, sends Nudge JOB (only when the session is one of the reporting daemon's user) to the holder, or offers the session when no lease is held (an ended job whose notice was not handed is pending work). The job's description is filled only when the call's request is still held at the result. |
| Nudge INTERRUPT | Interrupt the current turn only if its `turn_no` ≤ `Nudge.interrupt_turn_no`. If STARTED has not been answered yet, wait for its `turn_no`, then compare. |
| Nudge STOP | Stopping is permanent, so the hint suffices: flush, close the query, ReleaseLease with `REASON_STOPPED`. |
| LeaseRevoked for the held epoch | The fencing procedure above. |
| ReleaseIdle for the held epoch | If the session is idle (no turn running or starting, no turn end being reported, no input being taken or pushed and not yet answered, no background task running or ended and not yet told to the model, in the CLI or only in the worker's notes, no transcript repair, no transcript append unacknowledged), release exactly as the idle timer does: end the CLI's input and let it exit by itself (closed after a grace; meanwhile no turn starts and every tool use is denied), confirm its exit, store what it wrote as it exited (within the same grace), flush, ReleaseLease with `REASON_IDLE`. A session busy only for a moment (a turn end being reported, an append in flight) is released once that is over, unless a turn starts first or 30 seconds pass. Otherwise keep it: no answer. A Nudge for the session queued before the request is consumed drops it. |
| ReleaseIdle for another epoch, a session being acquired or released, or one not held | Ignore it. |

**Interrupt is a level keyed to a turn.** `Interrupt` raises the session's
`interrupt_turn_no` to the running turn's `turn_no`; with no running turn it
changes nothing. The value is in every Nudge and in every SessionState,
whatever sections were requested; `SECTION_NONE` returns only those.
A repeated or late hint never interrupts a later turn, and a lost one is
re-sent after ResumeLeases.

#### Stop

StopSession marks the session stopped; the holder keeps its lease until it
has flushed and released. The holder observes it through
`SessionState.stopped` (every SessionState, including `Lease.state`),
`Resumed.Outcome.stopped`, Nudge STOP and the rejections below.

| After stop | Operations |
| --- | --- |
| Refused | TakeInputs, RequestApproval, ConsumeGrant, GetLaunchEnv: `rejected STOPPED`. SubmitCall: `rejected SESSION_STOPPED` (terminal; nothing recorded or executed). A non-reattaching AcquireLease: `STOPPED` (a `fresh` one by the holder also releases the lease). |
| Accepted | AppendTranscript, ReportTurn, PatchSessionState, PublishEvents, TranscriptRepaired, LoadTranscript, ListTranscriptKeys, GetSessionState, ReleaseLease, ResumeLeases, a reattaching AcquireLease. |

On any STOPPED the worker closes the query and sends ReleaseLease with
`REASON_STOPPED`. `TRANSCRIPT_FULL` stops the session atomically with that
rejection (recording the stop event `KIND_SESSION_STATE`
`{"state":"stopped","reason":"transcript_full"}` in the same transaction);
the worker then releases with `REASON_STOPPED` rather than dropping entries.

#### Operations

| Session op | Successful SessionReply | Rules |
| --- | --- | --- |
| AcquireLease | lease | `fresh` as above. epoch, stable claude_session_id, has_transcript, `state` (every section except calls), `recovery`, shared SessionConfig, `reattached` and `transcript_gap_turn_no` (broken transcripts below). NOT_OFFERED and STOPPED as above. A new holder of a session with a provider: PROVIDER_UNAVAILABLE or PROVIDER_FULL, nothing written (Providers and the launch environment); a reattaching one is not gated. |
| ReleaseLease | ok | Reason IDLE, STOPPED, ERROR, FENCED or PROVIDER; relinquishes ownership; repeats as above. |
| GetLaunchEnv | launch_env | Before every CLI start. `local_login` without a provider; otherwise the provider's variables, at most 72. PROVIDER_UNAVAILABLE when the provider is deleted, not assigned to this worker or its secrets cannot be decrypted. Never logged. |
| LoadTranscript | transcript_page | Opaque subpath (empty = main), exclusive after_seq, max_bytes 1..8 MiB. Return at least one entry if available, even if it alone exceeds max_bytes; it must fit M with its envelope. Continue at next_seq until done. |
| ListTranscriptKeys | transcript_keys | At most 1024 subpaths. |
| AppendTranscript | ok | Atomic append; at most 8 MiB encoded entries, or one larger entry fitting M. JSON is opaque and never parsed. Retry rules below. |
| TakeInputs | inputs | Atomically take pending input; return all taken in this epoch and not yet persisted, at most 32, each text at most 256 KiB. |
| ReportTurn | turn | STARTED and ENDED return the same assigned turn_no. Rules below. |
| PatchSessionState | ok | Idempotent upserts/marks. Cwd by (agent_id, device), files by (device, path), CLI tasks by task_id. Empty sha256 forgets a file. Mark returned calls as soon as MCP returns, handed jobs when delivered to the SDK; `persisted` and `awaiting_call_ids` below. |
| GetSessionState | state | Empty sections = all; otherwise at most five distinct sections (cwds, files, jobs, calls, grants), or `SECTION_NONE` alone for no data. `stopped` and `interrupt_turn_no` always. Metadata only; use CallQuery for call output. |
| PublishEvents | ok | At most 128 per batch, normally every 100 ms; shared SessionEvent.Kind with json and ephemeral. `batch_seq` per epoch, retry rules as AppendTranscript. |
| RequestApproval | approval | TOOL: merge pending requests by (session, tool, device, match_key); reply includes approval_id and deduplicated. `match_key` is the SHA-256, in 64 lowercase hex digits, of the bash command verbatim or of the write/edit path; display, reason and tool_use_id do not change it. QUESTION: merge by tool_use_id; a new one supersedes the session's pending question (an `APPROVAL_RESOLVED` `superseded` event each). |
| ConsumeGrant | grant | Atomically find and consume one matching unexpired grant for `tool_use_id`. Repeating with the same tool_use_id and the same (tool, device, match_key) while that grant would still be unexpired replays `allowed = true` without consuming another; the same tool_use_id with another key gets `allowed = false`. Only allowed=true permits a hook to allow. |
| TranscriptRepaired | ok | BROKEN → IDLE, or QUEUED with pending work; never touches the gap mark `transcript_gap_turn_no`. Idempotent; ok without change in any other state. |

#### Transcript append and event retries

- `batch_seq` starts at 1 per (epoch, subpath) and increments by one. Only
  an `ok` reply (deduplicated or not) consumes it; Fenced, Rejected or a
  lost reply leave it unconsumed.
- A sequence at or below the last committed one returns
  `ok.deduplicated = true` without writing, including entries without uuid.
  A sequence beyond the last committed one plus one returns
  `rejected INVALID` and writes nothing.
- The adapter keeps the unacknowledged batch. Every later append call on
  that subpath, whether the SDK's retry of the same append or the next
  append, first resends that batch with the same `batch_seq` and identical
  content until it is acknowledged, and only then sends new entries under
  the next `batch_seq`. Never give entries already sent a fresh
  `batch_seq`: entries without uuid would be written twice.
- An SDK append that does not fit one wire batch (for example one entry over
  8 MiB beside small ones) is split into consecutive `batch_seq` values.
  Atomicity then holds per wire batch only; after a partial failure the
  retry resumes at the first unacknowledged `batch_seq`.
- A nonempty entry uuid independently deduplicates by (session, subpath,
  uuid), including across epochs.
- `STORE_UNAVAILABLE`: retry the same batch. `TRANSCRIPT_FULL`: not
  retryable; the session is stopped. `INVALID` (for example an over-cap
  uuid or subpath) and `LIMIT` (a subpath beyond 1024 per session) are not
  retryable: the adapter reports a failed append to the SDK, drops that
  batch, and gives the next content (for example the repair appends) its
  unconsumed `batch_seq`; the turn follows the `mirror_error` path to
  BROKEN. The resend-first rule above therefore never resends a rejected
  batch.
- Quotas default to 256 MiB per session and 20 GiB per user. Each entry
  counts as `len(json) + len(uuid) + 64` bytes in both totals.

PublishEvents `batch_seq` starts at 1 per epoch: a repeat is
`ok.deduplicated` without writing, a gap is `INVALID` and consumes nothing,
and a retry carries identical content. Unlike appends, a batch at the next
sequence that fails validation (an over-cap field, or a kind only the
control plane records: `APPROVAL_RESOLVED` (8), `TURN_INTERRUPTED` (9),
`SESSION_STATE` (10), `TRANSCRIPT_GAP_ACCEPTED` (15), `TURN_STOPPED` (17) or
`INPUT_ACCEPTED` (18)) consumes its
`batch_seq`: the control plane records it as dropped and answers `INVALID`,
and the worker continues with the next sequence, so one bad event cannot
freeze the view. A worker treats every PublishEvents `INVALID` as consumed:
the two cases share the code, and a gap only follows from a worker bug. `STORE_UNAVAILABLE` and Fenced consume nothing.

#### Inputs and turns

TakeInputs retries return everything taken in this epoch and not yet
persisted. While a session's turns are held (its workspace or ledger
snapshot is not ready, or the snapshot owes a merge's rebase or a discard's
reset) TakeInputs takes and returns nothing, and Nudges leave out
`REASON_INPUT`, `REASON_JOB` and `REASON_APPROVAL`; they stay pending and
are sent when the hold ends. A turn the CLI starts on its own still runs. Push to the SDK with uuid=input_id, avoiding duplicate pushes
within the epoch. NORMAL (and unspecified priority) merges into a running
turn or starts one when idle; NOW interrupts the current turn first.
Approval inputs use NORMAL.

A note (`KIND_NOTE`) followed by another input of the same TakeInputs reply
(which lists notes first) is pushed with `shouldQuery: false`, and with
NOW priority when an input after it in the reply has it, so it keeps its
place. Claude Code records such a message as a transcript entry of its own
(its uuid, `queueTranscriptOnly`) without a model call and merges it into
the next message that queries, the note first: the note and that input
run one turn. While idle the CLI still wraps each such note in a
`system/init` and a `result` (no model call, `num_turns` 0) that name only
it: the worker reports no turn for them and publishes nothing of them; the
note is credited to the next turn, which publishes its view event before
the input's; a turn's `system/init` also claims the inputs its
`user_message_uuids` name. A model call between such an init and its
result starts a real turn after all; another system message does not. A
failed result of such a note credits nothing: the note is pushed again,
silent and at normal priority (never NOW, which would stop the turn of the
input it goes with if that turn already started): a running turn absorbs
it, else it is recorded before or after that input, whichever the CLI
reaches first. When recorded notes are not followed by a turn within the
worker's abort grace (10 s) and no input pushed with them has been
announced by the CLI (`command_lifecycle` queued or started, and not
cancelled since: a turn for it is coming, however long its
UserPromptSubmit hooks take; a cancel starts the bound again), or when the query closes
first, they are marked persisted (PatchSessionState) and their view events
published by themselves, and the inputs pushed with them no longer keep
the session busy. A `mirror_error` between turns keeps the recorded notes
as a broken turn's items, marked after the repair and never pushed again;
notes announced but not yet recorded are pushed again.
Mid-turn the note is absorbed like any message (a `queued_command`
attachment naming it). A lost lease finds each note by its own id, like
any input. The last input of a reply always queries.

ReportTurn is idempotent by (epoch, local_seq, phase). Repeats retain the
turn_no, do not increment turn_count twice and do not duplicate persistence
marks. STARTED is the first SDK message belonging to a new turn and must use
the next local_seq (1 for the epoch's first turn); a STARTED whose
predecessor never ENDED closes that turn as INTERRUPTED, and a late ENDED
for it still applies its persisted marks and `transcript_incomplete`
without changing its outcome. ENDED for a local_seq never STARTED is
`INVALID`. ENDED follows the SDK result, including an interrupted empty
result, and carries outcome COMPLETED, INTERRUPTED, ERROR or STOPPED, opaque
usage_json, optional `persisted` and `transcript_incomplete`. Origin
describes what started the turn: input pushed while idle, a notification
pushed while idle, or the CLI itself (for example subagent completion).
Report turns of all origins.

Before sending a turn's ReportTurn ENDED, and before sending its successor's
STARTED, the worker MUST flush every queued PublishEvents batch belonging
to the preceding turn and wait for each batch to be acknowledged (or consumed
as INVALID under the event retry rules). This includes its final SDK RESULT
and assistant events. The control plane assigns boundary event sequences
when the turn closes, so flushing first ensures TURN_STOPPED or
TURN_INTERRUPTED follows that turn's worker events in durable seq order.

STOPPED requires the worker to observe an abort attributable to a user
Interrupt or NOW input applied to that turn. Track attribution by epoch
and local sequence / assigned turn number, not by a session-wide flag.
For Interrupt, wait for STARTED's assigned turn number before comparing
the interrupt level; duplicate or stale hints must not affect a later
turn. For NOW, attribute the abort to the turn active at actual delivery,
deduplicate pushes by input_id, and do not tag the replacement turn.
An idle interrupt changes nothing, and an idle NOW input starts normal work.
Natural completion or error is not STOPPED merely because a stop request
was pending. Ambiguous termination is reported ERROR. STOPPED does not
stop the session or establish which partial assistant text was saved.

INTERRUPTED means an unfinished turn was closed without a confirmed
user-stop report, including lease loss or a successor starting before
ENDED arrived; it is not proof of worker death or transcript loss. The
first committed closure wins. If STOPPED ENDED commits first, later lease
loss preserves it. If loss or forced closure commits first, late or fenced
ENDED cannot relabel it even when an interrupt request preceded the loss.
Reported mirror failure still makes the session BROKEN, including with a
confirmed STOPPED outcome; an already STOPPED session keeps that state.

The store records exactly one durable `TURN_STOPPED` (kind 17) or
`TURN_INTERRUPTED` (kind 9) boundary per turn first closed with that
outcome, in the same transaction as the outcome; fan-out follows commit.
Their payloads are respectively
`{"turn_no":"<decimal uint64>","outcome":"stopped"}` and
`{"turn_no":"<decimal uint64>","outcome":"interrupted"}`.
The decimal string preserves JavaScript uint64 precision. Workers publish
SDK results and assistant messages, but cannot publish either boundary.
Both kinds survive view truncation without their payloads; replay then
identifies the category without asserting a turn correlation.

Every lease-loss entry point closes an open turn as INTERRUPTED:
ReleaseLease (including shutdown or permanent stop cleanup),
ReleaseWorkerLeases on a new boot, ExpireLeases, SettleSession of an expired
lease, AcquireLease takeover or fresh replacement (including a fresh
holder's release from STOPPED/BROKEN), and RecoverSession revocation.
A successor STARTED without ENDED uses the same closure rule. StopSession
and transcript quota stop retain the lease and do not themselves close a
turn; accepted final ENDED retains its actual outcome, otherwise release
closes it as INTERRUPTED. Shutdown or control-plane restart alone is not a
user stop-turn action; restart grace and successful resume preserve an open
turn. Permanent session stop must not synthesize TURN_STOPPED.

#### Persistence marks

Track the items delivered in a turn and not yet persisted: inputs pushed,
calls returned to the SDK, job notices handed over and a delivered
missing-history reminder (`gap_notice_turn_no`, broken transcripts below). Once the SDK result
has arrived (it follows the turn's last append), mark them persisted, in
`ReportTurn.persisted` of ENDED or, when they do not fit the 64 KiB limit
together, partly in `PatchSessionState.persisted`. That field is allowed
only for items delivered before the SDK result of a turn already reported
ENDED or about to be; marks are idempotent and ENDED carries the remainder.
Never mark items of a turn whose transcript is incomplete before it has been
repaired. An SDK result with an ERROR outcome still acknowledges the delivered
items. A CLI failure before any result has arrived supplies no such evidence.

**Owed call notices.** When MCP returns a call to the model with a
nonterminal outcome (unknown, or "still running, you will be notified"), add
it to `PatchSessionState.awaiting_call_ids` (at most 16 per session, else
`LIMIT`) before returning it. After `LIMIT`, return the call with a final
"result unavailable" text instead of promising a notice, so no promise
exists without its mark. Such a call stays in `SessionState.calls` and `Recovery.calls`
with `notice_owed`, also after it is persisted. The holder polls it with
CallQuery, delivers the completion notice and, once that notice is
persisted, lists the call in `Persisted.late_call_ids`, which clears the
mark.

Caps of the stored state: 256 cwds, 4096 file records (the oldest evicted),
1024 job notices, 1024 calls not yet persisted plus 16 with a notice owed,
64 live CLI background tasks, 32 grants. Full call results remain available
through CallQuery; SessionState never embeds their output.

#### Worker event payloads

The following fields are in `payload_json`. Fields are required unless
marked optional. The worker publishes `TURN_STARTED` with
`{"turn_no":"<decimal uint64>","origin":"input"|"notification"|"cli"}`.
`ASSISTANT`, `USER` and `RESULT` carry Claude Agent SDK message JSON.
An ephemeral `ASSISTANT` carries an SDK `stream_event`. A completed block
is published as a durable SDK assistant message, one per block, after the
block's last `content_block_delta` and before its `content_block_stop`.
Blocks from the same response share the API `message.id`, which every
`stream_event` of the response also carries as `api_message_id`. Text,
thinking and `tool_use` blocks all stream: a `tool_use` block starts with
`input: {}` and its input arrives as `input_json_delta` deltas
(`partial_json`); the response then ends with `message_delta`
(`stop_reason: "tool_use"`) and `message_stop`.

A `USER` event for a message the worker wrote for the model, rather than
the user's input or a tool result, adds a top-level `circus_note` string
naming its kind. The field is part of the view event only and is not
written to the transcript. Kinds: `approval_retry` (the input an approval
creates), `question_answer` (the user's answers to a question approval, a
`KIND_ANSWER` input; pushed as an ordinary user message, so its view event
carries `circus_note` without `isSynthetic`), `user_interrupt` (the annotation of a user's Interrupt),
`previous_turn_interrupted`, `recovered_call` (a call result returned on
recovery), `call_notice` (a late call's completion), `job_notice` (a device
background job's completion), `lost_bgtask` (an SDK background task lost
with its worker) and `history_missing` (the missing-history reminder).
A note input the control plane writes (`Inputs.Input` of `KIND_NOTE`) is
published with its `note` as the kind: `ledger_problems` (the problems of
a collection of the session's ledger snapshot), `ledger_merged` (the
session's ledger changes were merged and its new decisions renamed) and
`ledger_discarded` (they were discarded). The control plane records no view
event for a note itself: the view shows it when the worker delivers it.
These three ledger notes, whose text starts with `[circus ledger]`, are
pushed to the CLI as ordinary user messages, not synthetic ones, and their
view events carry `circus_note` without `isSynthetic`; any other note is
synthetic. The control plane holds `ledger_merged` and `ledger_discarded`
back (deferred inputs): `TakeInputs` returns them only with an input that
starts a turn, before it, so they never start one themselves (a turn the
CLI starts by itself takes no input and does not carry them); they reach
the agent in that input's turn (see "Inputs and turns"). Held notes do
not count towards the 32 open inputs; at most 8 are held, and one more
replaces them with a single `ledger_merged` note that tells the agent to
re-read `INDEX.md`.
Clients treat an unknown kind, or a synthetic user message without one, as
a generic note to the model.

For a text-stream abort, the measured SDK emits the partial assistant block
with `aborted: true` before RESULT. Interrupt produces
`error_during_execution`. NOW produces `success` with the partial text as
its result. Both have `terminal_reason: "aborted_streaming"`. Interrupt
also writes `[Request interrupted by user]` to the transcript, published as
the `user_interrupt` note before the aborted response's stream ends
(`content_block_stop`, `message_delta`, `message_stop`).

Interrupt during a tool call produces the tool's `tool_result` (Claude
Code's rejection text with a correction that the call was stopped while
running, `tool_result_meta` with `non_execution_kind: "user-rejected"`),
then the `[Request interrupted by user for tool use]` note, and a RESULT
with `terminal_reason: "aborted_tools"` and `stop_reason: "tool_use"`. The
Go fake worker publishes these shapes as the e2e tests captured them from
the real worker; for a tool interrupted by NOW it infers the same
`tool_result` and `subtype: "success"` with `terminal_reason:
"aborted_tools"`.

**APPROVAL_PENDING**

Recorded by the control plane only, in the RequestApproval transaction that
creates the approval (a deduplicated request records nothing), so it comes
before every `APPROVAL_RESOLVED` of the same approval and is never lost when
the worker dies after the request. The worker does not publish it.

Its place relative to the call's own events is best effort. The worker
publishes what it holds of the call before it requests the approval, so the
card normally follows the `ASSISTANT` event with its `tool_use`; but the CLI
asks `canUseTool` independently of the message stream, and when the worker's
message loop lags (back-pressure, an earlier message's RPC) the card can come
first. Clients key cards by `tool_use_id` and accept either order.

A TOOL request merged into a pending approval of the same (tool, device,
match_key) records no new `APPROVAL_PENDING`: a later call of the same
command denied while the first approval is pending (in the same message or a
later turn) has no card of its own, only its error result ("sent to the user
for approval"); the earlier card's approval covers it (one grant, one
retry). A pending approval past its expiry does not merge: the request
expires it and creates a new one with its own card.

| Field | Type and meaning |
| --- | --- |
| `approval_id` | String from RequestApproval |
| `kind` | `tool` (a denied tool call) or `question` (the model's AskUserQuestion) |
| `tool`, `device`, `tool_use_id` | Strings identifying the denied device tool use; `tool` is the SDK tool name. A question's `tool` is `AskUserQuestion` and its `device` is empty |
| `display`, `reason` | Strings shown in the approval prompt; `reason` is empty when the permission mode gave none, and clients then say that the permission mode denied the tool. A question's `display` is its first question and its `reason` is empty |
| `input_json` | Question only: the AskUserQuestion input, as `SessionApproval.input_json` |

**PERMISSION_DENIED**

| Field | Type and meaning |
| --- | --- |
| `type`, `subtype` | Strings `system` and `permission_denied` |
| `uuid`, `session_id` | SDK message UUID and Claude session ID |
| `tool_name`, `tool_use_id` | SDK tool name and the denied tool-use ID |
| `agent_id` | Optional string for a subagent |
| `decision_reason_type`, `decision_reason` | Optional decision source and explanation strings |
| `message` | Rejection text carried in the error `tool_result` |

After a permission denial the worker surfaces the rejection and requests
approval. The current turn can finish while approval is pending. An
approved input joins an active turn or starts one while idle. It asks the
model to retry with a new tool-use ID. The worker only permits that retry
when ConsumeGrant returns `allowed`.

The worker registers `canUseTool` and disallows `EnterPlanMode` and
`ExitPlanMode` (and refuses permission mode `plan`). A call the permission
mode or an ask rule asks about reaches `canUseTool` instead of producing
`permission_denied`: the worker publishes what it holds of the call,
requests a TOOL approval as above (the control plane records
`APPROVAL_PENDING`), and denies the call,
telling the model it was sent to the user and must not be retried until
the user approves it. Classifier, `dontAsk` and rule denials still arrive
as `permission_denied`. The model's `AskUserQuestion` on the main thread
becomes a QUESTION approval: the worker denies the call, telling the model
the questions were shown to the user and to end its turn without guessing
the answers, which arrive as the next user message (`question_answer`). The
control plane writes that input when the user answers: `The user answered
your questions: "<question>"="<answer>", .... Continue with these answers
in mind.`, each question and answer a JSON string, an answer being its
selected labels and then the user's own text joined with `, `. A
subagent's `AskUserQuestion` is denied without an approval; an input over
32 KiB is denied without asking, and one the control plane refuses as
INVALID is denied with the reason (the user sees an `ERROR` event); the
model is told it may ask again with a corrected call. In `dontAsk` mode the
CLI denies `AskUserQuestion` itself, before `canUseTool`: no question is
asked, and that denial (a `permission_denied`, then a result denial) never
becomes an approval.

**BGTASK_STARTED**

The worker forwards the CLI's own SDK `task_started` messages under this kind.

| Field | Type and meaning |
| --- | --- |
| `type`, `subtype` | Strings `system` and `task_started` |
| `uuid`, `session_id` | SDK message UUID and Claude session ID |
| `task_id`, `description` | SDK task ID and display description strings |
| `tool_use_id`, `task_type` | Optional originating tool-use ID and task type strings |
| `is_backgrounded` | Optional boolean indicating a background task |

**BGTASK_ENDED**

The worker forwards the CLI's own SDK `task_notification` messages under this kind.

| Field | Type and meaning |
| --- | --- |
| `type`, `subtype` | Strings `system` and `task_notification` |
| `uuid`, `session_id` | SDK message UUID and Claude session ID |
| `task_id` | Task ID from BGTASK_STARTED |
| `tool_use_id` | Optional originating tool-use ID string |
| `status` | `completed`, `failed`, or `stopped` |
| `summary`, `output_file` | Display summary and output path strings. The path may be empty |
| `reason` | Optional string `worker_restart` for an orphaned SDK task |

**NOTIFICATION_DELIVERED**

Device background jobs use this kind when the worker delivers their completion
notice. Their lifecycle emits no BGTASK events.

| Field | Type and meaning |
| --- | --- |
| `job_id`, `call_id`, `tool_use_id`, `device` | String IDs and device name of the delivered device job notice |
| `state` | `exited`, `killed`, or `lost` |
| `exit_code` | JSON integer from the daemon |
| `output_path` | Device output path string, possibly empty |

**TRANSCRIPT_GAP_DETECTED**

| Field | Type and meaning |
| --- | --- |
| `subpath` | Transcript subpath string, empty for the main thread |
| `unknown_parents` | Array of missing parent UUID strings |

**ERROR**

| Field | Type and meaning |
| --- | --- |
| `message` | Worker error description string |

#### Broken transcripts

On `mirror_error`, close the query and send ENDED with
`transcript_incomplete = true` and without `persisted`: the session becomes
BROKEN. A CLI failure without mirror_error is ENDED with ERROR and without
the flag (then release); it does not make the session BROKEN. While BROKEN:
appends are accepted (for the repair), inputs already taken stay taken and
unpersisted, and new inputs are accepted but not taken: TakeInputs returns
nothing while BROKEN, and after the repair everything taken in the epoch and
not yet persisted. RequestApproval and ConsumeGrant answer `rejected
BROKEN`; the worker stops calling them until TranscriptRepaired, or until a
RecoverSession ends its lease. The holder
re-imports the missing batches with ordinary appends (uuid deduplication
drops entries already stored), marks the repaired turn's items with
`PatchSessionState.persisted`, then sends TranscriptRepaired. A BROKEN
session is not offered to other workers; if its holder releases or loses the
lease before repairing, the session stays BROKEN: repair by another worker
is outside this version of the protocol. The user can then either stop the
session or call `RecoverSession{accept_transcript_gap: true}`, which is
allowed only while BROKEN. It first revokes any held lease (`LeaseRevoked`;
that epoch is fenced, so a holder still running a CLI with the old context
cannot continue), then moves the session to IDLE (or QUEUED with pending
work) and records a `TRANSCRIPT_GAP_ACCEPTED` event saying the transcript
has a gap. The next holder resumes from the transcript the control plane
holds, which lacks at least the failed batches; later batches may have
landed after them, so the hole can sit in the middle of the entry chain.

Measured resume behavior of a transcript with a hole, for holes that remove
whole turns, single entries or the tail: the CLI does not fail, does not
signal the hole, and repairs nothing in the store. Not measured yet: a hole
that splits a `tool_use` from its `tool_result` (the likely shape when a
batch boundary falls inside a tool turn, and one the Messages API may
reject), holes in subagent subpaths, and holes across a compaction.
- A hole in the middle of the `parentUuid` chain is bridged: the model sees
  every surviving entry in file order and misses only the lost entries;
  where the loss leaves two user turns adjacent, they are merged into one
  user message. The next resume bridges the same hole again.
- A hole at the tail is indistinguishable from a shorter conversation: the
  leaf becomes the last surviving conversation entry and the lost turn is
  silently absent.
- Cutting the loaded list at the first unknown parent would also discard
  every later turn that did persist, so it is not done.

Worker obligations when it loads a transcript (LoadTranscript before
starting the SDK query):
- Pass every loaded entry to the SDK unchanged; never cut or reorder.
- Detect a hole inside the chain: an entry whose nonempty `parentUuid` is
  not the uuid of any loaded entry of that subpath. This rule is untested on
  subagent subpaths, sidechain entries and compaction boundaries; a false
  positive there only produces a spurious user-view event. On finding one
  while the mark below is nonzero, publish a `KIND_TRANSCRIPT_GAP_DETECTED`
  event naming the subpath and the unknown parents, for the user's view; with
  the mark at 0 publish nothing. The hole stays in the store and is found
  again on every load, so this limits the event to the first resume after
  acceptance. It adds no reminder of its own: a store can contain holes only
  after a broken session's gap was accepted, which the mark covers.
- Two notions are kept apart. An unaccepted break is the BROKEN state
  itself; TranscriptRepaired ends it and leaves the gap mark alone. The gap
  mark `Lease.transcript_gap_turn_no` means only "a gap the user accepted
  that the model has not been told": RecoverSession sets it to the turn_no
  of the turn that broke, and only an acknowledgement of the equal value
  clears it; 0 when none. It also covers a hole at the tail, which the
  entries cannot show.
- When the mark is nonzero, inject a reminder telling the model that part of
  the conversation history is missing in the next turn the worker starts
  with a pushed message (input or notification); a CLI-originated turn
  cannot carry it, so the reminder waits. Acknowledge it in that turn's
  persisted set with `Persisted.gap_notice_turn_no` set to the value
  received; the control plane clears the mark only if it still equals the
  stored one. The reminder is thus delivered once: a crash before the
  acknowledgement repeats it, nothing else does.

Workers are
unchanged: a worker never clears BROKEN except by TranscriptRepaired, and
learns the recovered state through the usual lease and state replies;
inputs taken earlier and absent from the transcript return to pending at the
next epoch as usual. `TRANSCRIPT_GAP_ACCEPTED` is written by the control
plane only; a worker batch containing it is `INVALID` (above).

#### Recovery

Recovery lists every item not yet acknowledged, whichever epoch produced it.
It is never computed relative to `epoch - 1`, so a reattachment, or an
acquisition after an intermediate epoch that did nothing, returns the same
items.
- `calls`: every call not yet persisted and every call with `notice_owed`
  (at most 1040), with `returned`, `outcome`, `rejected_code` and
  `exit_code`. For a call not returned, query it and inject its full
  terminal result (or a running-task notice); for a returned call inject
  only a tool/device/status summary, because its output may already be in
  the transcript; for a call with a notice owed, poll it and deliver the
  completion notice. At most 16 calls may be in flight. Never reexecute.
- `lost_bgtasks`: CLI background tasks started by an earlier epoch and never
  ended (at most 64). Inject a failed notification with the original
  tool_use_id; send `bgtasks_ended` for the task once the turn that
  delivered the notice has had its items marked persisted.
- `previous_turn_interrupted`: the most recent turn was closed without ENDED
  (for example by lease loss) and no turn has started since.

A new epoch first requeues: inputs taken by any earlier epoch are marked
persisted if their uuid is already in the transcript and otherwise return to
pending; job notices handed by any earlier epoch and not persisted become
unhanded again (they appear in `state.jobs` with `handed = false`). A
reattachment requeues nothing. The control plane may instead requeue
already when the earlier lease is lost (expiry, revocation or release);
either moment gives the same result, because the uuid check runs at the
moment of requeueing. Place recovery before the next user message;
never automatically resume a crashed turn. Recovery items alone are not
pending work and do not get the session scheduled.

#### States

State is independent of lease ownership: IDLE means no running turn;
QUEUED means pending input or a completed unnotified device job with no valid
lease; RUNNING means a CLI turn is active; STOPPED is user-stopped; BROKEN
means transcript mirroring is incomplete. STARTED increments turn_count;
ENDED returns to IDLE (or QUEUED when work remains without a lease). Lease
loss during a turn records INTERRUPTED, emits TURN_INTERRUPTED and returns
to IDLE/QUEUED. Idle CLI subagents prevent idle release; otherwise release
after five idle minutes with no pending pushes (subagents may hold at most
two hours), or at once on a ReleaseIdle when idle (running subagents keep
the lease then). Either idle release, when the CLI holds nothing to run
(no input pushed and unanswered, no background task or undelivered task
notice), ends the CLI's input and lets it exit by itself, bounded by a
grace, so what it writes after its last result (`last-prompt`,
`cost-state`) is stored before the lease is given back; no turn starts
and every tool use is denied meanwhile. A stop, an error or a worker
shutdown closes the CLI at once.

#### Rejections and field caps

SessionReply.rejected codes are INVALID, NOT_FOUND, STORE_UNAVAILABLE,
TRANSCRIPT_FULL, LIMIT, STOPPED, NOT_OFFERED and BROKEN (RequestApproval
and ConsumeGrant on a broken session; SubmitCall on a broken session is
`SESSION_STOPPED`). STORE_UNAVAILABLE is
retryable and means nothing was written; retry with the same idempotency
keys. A transport request_id is never an idempotency key and changes after
reconnect. Pending approvals and unused grants each have a cap of 32 per
session and a ten-minute expiry. Resolving approval creates one grant and
one approval input atomically, is idempotent and is refused after StopSession.

**Field caps.** The control plane checks these byte lengths on every session
operation and SubmitCall, and answers a violation with `rejected INVALID`
(SubmitCall: `CODE_INVALID`) without writing anything. An Attach that
violates them, or has `max_sessions` outside 1..64, closes the link. They
are not part of the decoder (section 5).

| Cap | Fields |
| --- | --- |
| 128 B | every id: session, call, input, approval, client message, worker and Claude session ids, `tool_use_id`, `agent_id`, `TranscriptEntry.uuid`, `JobKey` parts, background task ids and `kind`; every device name; `SessionConfig.model` and `permission_mode` |
| 64 hex digits | `match_key`; `FileRecord.sha256` (or empty) |
| 256 B | `tool`; every Attach string; `Devices.Device` `os`, `arch` and `hostname` |
| 1 KiB | `subpath`, job and background task `description`, `display`, `reason`, `Rejected.message` |
| 4 KiB | `Cwd.cwd`, `FileRecord.path`, `Job.output_path`, `usage_json` |
| 256 KiB | input text |

**Worst-case replies.** With every cap reached, measured by
`TestWorstCaseSessionReplies`:

| Reply | Encoded bytes |
| --- | --- |
| Lease | 26,737,645 (≤ 25.5 MiB): files 16.8 MiB, jobs 5.8 MiB, cwds 1.1 MiB, config cwds 1.0 MiB, recovery calls 0.7 MiB, the rest under 0.2 MiB |
| SessionStateReply, all sections | 25,562,376 (≤ 24.5 MiB) |
| TranscriptKeys | 1,051,668 (≤ 1028 KiB) |

All fit M and the structure limits of section 5; `Lease.state` leaves calls
to `Lease.recovery` so that the Lease stays within 8192 message nodes. The
session-op request side is bounded by the 64 KiB rule, except appends and
event batches.

#### User-side API

The user-side Connect API authenticates a browser session cookie, with the
same Origin rule as ProjectService. A Bearer credential for the CLI comes
later. CreateSession requires a project of the caller and optionally a
location_index (otherwise the project's last_used_location) and pinned
worker.
config.default_device and config.cwds must be empty; the control plane fills
them from the project location. config.local must be false
(INVALID_ARGUMENT with a `config.local` field violation): the control
plane decides it. model and permission_mode may be supplied.

Local mode is decided once the location's device is resolved. With a
pinned_worker_id, the session is local exactly when that worker's machine
is the device's machine. Without one, if the caller has a live worker
(not deleted, with an unrevoked credential) on the device's machine, the
session is local and pinned to it; with several, the one connected most
recently (a worker never connected counts as oldest; ties go to the
smallest id). Otherwise the session is neither local nor pinned. Local
mode and the pin are stored with the session and never change; responses
with config (GetSession, CreateSession and the writes) return
config.local, and pinned_worker_id is the pin, derived or requested.

Authorization: a session belongs to the Web user who owns its project.
Every call resolves the session (for ResolveApproval, the approval's
session) through a project of the caller, in the transaction of the
write it authorizes, and answers NOT_FOUND for anything else, including
the sessions of other users' projects. ListSessions with project_id
first requires that project of the caller (NOT_FOUND otherwise); without
it, the store lists the sessions of every project the caller owns. Store
errors map to fixed messages: NOT_FOUND, FAILED_PRECONDITION for a
stopped session (and DeleteSession's and RecoverSession's preconditions),
RESOURCE_EXHAUSTED for the session, input and stream caps,
ALREADY_EXISTS for a client_msg_id or client_request_id reused with
different data, UNAVAILABLE when the store is busy, CANCELED or
DEADLINE_EXCEEDED when the request ended, INTERNAL otherwise. Argument
errors are INVALID_ARGUMENT with a
google.rpc.BadRequest field violation naming the request field.

A new session requires a project location on a non-revoked device of the caller.
A missing location or unavailable device returns FAILED_PRECONDITION with
a `location_index` field violation.
An out-of-range location_index is INVALID_ARGUMENT. The server fills
default_device and one main-thread working directory. A default location
ensures its workspace in the session creation transaction. Until READY, the
cwd is the placeholder `~/circus/projects/<project_id>` and the scheduler
holds the session. READY supplies its absolute cwd at creation.
`Session.workspace` is populated in create, get, list and update responses
for a default-location session. `Project.Location.workspace` is populated
for a default location with a workspace row.
pinned_worker_id must name a worker
of the caller. The location becomes the project's
last_used_location in the same transaction, without moving the project's
updated_at.

CreateSession requires a client_request_id of 1 to 128 bytes, unique within
the project. A retry with the same key returns the current session without
checking the device or session cap again or changing last_used_location.
An explicit location_index must match the index resolved for the first
request. After the project's locations are reordered, an explicit retry
must send the original index. An omitted index reuses that result even
if last_used_location has changed. The pinned_worker_id the client sent
(omitted and empty are equivalent; a pin the control plane derived is not
compared), config.model, and config.permission_mode (empty selects
auto) must also match. A retry returns the first session, local or not,
even if the caller's workers changed since. Different settings return ALREADY_EXISTS with a
client_request_id field violation.
Concurrent requests with the same key create at most one session.
Deleting the session releases the key for a new create.

The control plane records these durable view events itself, in the
transaction of the change, with JSON payloads:

| RPC | Kind | Payload |
| --- | --- | --- |
| SendMessage, new input | `KIND_INPUT_ACCEPTED` | `{"input_id","client_msg_id","priority":"normal"\|"now","text"}`, encoded without HTML escaping; not a boundary, so a filled view drops it and event_seq is 0 |
| RequestApproval that creates an approval (worker link) | `KIND_APPROVAL_PENDING` (boundary) | `{"approval_id","kind":"tool"\|"question","tool","device","tool_use_id","display","reason"}`, plus `"input_json"` for a question (see APPROVAL_PENDING above); a deduplicated request records nothing |
| ResolveApproval of a pending approval | `KIND_APPROVAL_RESOLVED` (boundary) | `{"approval_id","state":"approved"\|"denied"\|"expired"}`; expired when it expired before the resolution (the call then answers FAILED_PRECONDITION). A QUESTION approval records `{"approval_id","kind":"question","state":"answered","answers":[{"question","selected","other"}],"input_id"}`, where `input_id` is the `KIND_ANSWER` input written in the same transaction (the worker pushes it with that uuid, so the answer's `USER` view event has `uuid` = `input_id`), or `{"approval_id","kind":"question","state":"dismissed"}`. Answering a question that is no longer pending and was not answered (dismissed, superseded, or ended by a stop) answers FAILED_PRECONDITION and records nothing; repeating an answer or a dismissal, or dismissing an answered question, is an idempotent no-op |
| RequestApproval of a QUESTION that supersedes pending ones | `KIND_APPROVAL_RESOLVED` (boundary), one per superseded approval, before the new one's `APPROVAL_PENDING` | `{"approval_id","kind":"question","state":"superseded"}` |
| Workspace preparation changed state | `KIND_WORKSPACE` (boundary) | `{"state","waiting","device","path","repo_url","background","action","error","error_detail"}`. Changes limited to the waiting reason or background flag are ephemeral. Each event timestamp equals the workspace version reported by `WorkspaceStatus.updated_at_unix_ms`. |
| StopSession that stopped the session | `KIND_SESSION_STATE` (boundary) | `{"state":"stopped","reason":"user"}`; a stop by the transcript quota (worker link) records `{"state":"stopped","reason":"transcript_full"}`. Either stop first records `KIND_APPROVAL_RESOLVED` `{"approval_id","kind":"question","state":"expired"}` for the pending question it ends |

These kinds (with `TURN_INTERRUPTED`, `TURN_STOPPED` and
`TRANSCRIPT_GAP_ACCEPTED`, and the live-only `KIND_QUEUE` and `KIND_WORKER`
announcements) are recorded by the control plane only: a worker event batch
with any of them, durable or ephemeral, is dropped as INVALID. A worker
publishes `KIND_USER` for the SDK's user messages.

A repeated SendMessage returns the seq stored with its input. After
commit the server announces new durable events to watchers and hints the
scheduler: SendMessage with a new input INPUT, plus INTERRUPT for
INPUT_PRIORITY_NOW while a turn runs (NOW does not raise the interrupt
level); an approval APPROVAL; an Interrupt that raised the level
INTERRUPT; a StopSession that stopped the session STOP; RecoverSession
INPUT. Every StopSession also asks the session's devices to stop its
background jobs (best effort; a device offline is skipped and a later
StopSession retries). The approval input reads "The user approved this
<tool> call on device "<name>" once: <display>" followed by "Retry it
now."; its grant lasts ten minutes. SendMessage refuses empty or blank
text, and a message whose INPUT_ACCEPTED event payload would exceed the 256 KiB
payload cap (the stored form of an over-cap payload keeps no bytes at
all, `truncated` set): INVALID_ARGUMENT with a `text` field violation. The
check is on the payload as encoded above, so the largest accepted text is
256 KiB less the envelope for plain text, and less for text with quotes,
backslashes or control characters, which JSON escapes. client_msg_id may
not start with `approval:`, the prefix of approval inputs
(INVALID_ARGUMENT, `client_msg_id`). Resolving an approval that has expired,
at the call or before, answers FAILED_PRECONDITION: it had no effect (an
expiry the call itself detected is recorded). Resolving one already
approved or denied succeeds without effect, whatever `approve` says.

UpdateSession trims with the ECMAScript rule of project names and refuses
an empty title, more than 200 characters, more than 1024 bytes and the
characters forbidden in project names. ListSessions page tokens are
opaque and carry the keyset of the last session returned. ListApprovals
lists the pending approvals unexpired at the request time, with device
names.

Session.project_id is required in the store and references projects. The
session and its project have the same Web user as owner. That user's
devices and workers serve the session. The service authorizes through the
project.

SendMessage deduplicates by (session_id,
client_msg_id), retaining identical content, and returns input_id/event_seq.
A stopped session refuses SendMessage; a broken one accepts but does not
deliver until repaired. ListSessions is paged (default 100, maximum 1000),
optionally filtered by project_id, and ordered by last_activity_at descending,
then session_id descending, with an exclusive keyset cursor. It leaves
`config` unset; GetSession returns it. A Web user has at most 1000
sessions. Both reads return project_id, title, last_activity_at_unix_ms and
pending_approval_count. Last activity starts at creation and never moves
backwards: every stored durable event except KIND_WORKSPACE and every newly
accepted user-kind input sets it to max(current, its timestamp). This includes control-plane
boundaries and retained events with dropped payloads; ephemeral or dropped
events, approval inputs and duplicate input submissions do not move it.
A ListSessions traversal is not a snapshot. Sessions remaining in the filter
with unchanged activity are returned exactly once, and an already-returned
session never repeats. A not-yet-returned session that becomes active and
moves above the cursor is skipped in this traversal and appears on refresh.
Deletions disappear; new sessions above the cursor also appear on refresh.
Store expiry reads take an explicit Unix-millisecond now
(GetSession's argument, SessionFilter.Now); pending approvals count only
when their state is pending and expires_at > now, even before an expiry
sweep runs, at most 32.

A new user-kind SubmitInput sets an empty title atomically from the first
nonempty text line: collapse whitespace, trim, and cut at 60 Unicode code
points, preferring the last whitespace in the second half of the cut,
including whitespace immediately after the first 60 characters. Whitespace
is normalized before removing the control, zero-width and bidirectional
characters forbidden in project names; whitespace exposed by removal is
collapsed again. The 60-character limit excludes the appended `…`, so a
hard-cut title can contain 61 Unicode code points including the ellipsis.
Empty text derives no title; approval inputs and
duplicate submissions do not set it. SubmitInput currently records no view
event of its own. UpdateSession sets a trimmed title of 1–200 characters
and at most 1024 bytes; the store's SetSessionTitle enforces nonempty and
the byte cap, with character validation left to the service. Explicit title
updates do not move activity and prevent subsequent automatic naming.

The DeleteProject RPC first checks every session's DeleteSession
preconditions in one snapshot. If any is not deletable it returns
FAILED_PRECONDITION and deletes nothing. It then deletes the checked
sessions one at a time through chunked DeleteSession, followed by the empty
project. The store's DeleteProject always refuses with ErrHasSessions while
any session remains, including a deletable one. For a deletion without
removals, a session created during the cascade makes the final step
return FAILED_PRECONDITION and the client can retry. A failed or interrupted cascade may have removed some
sessions already; retry finishes the remaining work. This avoids holding
the database writer for all transcripts in one transaction. Like
DeleteSession, job rows survive while their jobs are tracked.

DeleteProject accepts up to 32 device names in `remove_workspaces_on`.
During a deletion with removals, new sessions for that project are refused
with FAILED_PRECONDITION. After preflight, bounded session deletion, device
operations, and the final project write continue if the client disconnects.
After session deletion, requested READY workspaces can be removed on devices
with a current compatible connection. The response returns a result per
name in request order, including duplicates. REMOVED includes an already
absent directory. KEPT covers offline or outdated devices, devices with
no READY workspace and calls that did not answer in time. FAILED carries
the device's removal error. The shared 30-second budget and the outcome
of device operations do not change the project's deletion preconditions.

DeleteUser cascades its empty projects but refuses with ErrHasSessions if
any owned project has sessions, deleting nothing; remove those sessions
through the chunked deletion flow first.

The development migration to project-backed sessions
deletes all old session data, their calls and device jobs and resets user
transcript totals, because old sessions cannot be assigned a project;
users, devices, workers and projects survive. The outcome checks accept
stopped alongside the historical completed, interrupted and error values.

WatchSession replays durable events strictly after after_seq, then
streams live events. The stream subscribes to the process's event bus
before its last replay read and sends an event only when its seq is above
the last one sent, so replay and live meet without a gap or duplicate;
evicted events are skipped. On a durable announcement the stream reads
the store only up to the announced seq before the items queued after it.
Ephemeral events are sent only live, in publication order relative to the
announcements; one queued before an announcement the stream has already
passed is skipped. This ordering is best effort: a durable event is
announced after its commit, and a stream that reads the store between the
two sends it before an ephemeral event published in that window. A stream that falls
behind loses ephemeral events (each stream queues at most 256 of them and
256 KiB of payload; a larger one never fits), never durable ones. A
stream whose replay is empty starts with a keepalive, and a keepalive
follows every 25 s without another message: an ephemeral event with kind
KIND_UNSPECIFIED, seq 0 and no payload, which clients ignore. At most 32
streams per Web user and 256 in all (RESOURCE_EXHAUSTED). The stream ends
NOT_FOUND when the session is deleted (also by DeleteProject), UNAVAILABLE
when the server shuts down or after about an hour (each stream draws its
lifetime within ±10 %; the client reconnects with after_seq), and UNAUTHENTICATED within 30 s of its browser session being
signed out, expiring or losing its user (the stream rechecks the cookie's
session; a recheck counts as use of the session, like any request). Writes
that make no progress for 30 s end the stream (a response is written in
chunks of at most 32 KiB, each under its own deadline, so a slow link
that keeps reading is never cut). Deletion and shutdown cut a write in
progress at once. When no write is in progress, the stream's final status
and the end of the response are written under the write timeout, so a
client that does not read can keep the stream for up to 30 s more. A
stream cut in the middle of a write leaves no status: the client sees a
transport error (Connect `unknown`, "unexpected EOF") instead of
NOT_FOUND or UNAVAILABLE. Clients treat it like UNAVAILABLE and reconnect
with after_seq; a deleted session then answers NOT_FOUND. SessionService responses are not
compressed by the server. Repeated replays read the store, which is a single
SQLite connection; their cost is bounded only by the stream caps. SessionEvent carries seq, ts_unix_ms, shared kind,
opaque payload_json, ephemeral and truncated. TURN_STARTED payload includes
origin. Ephemeral events have seq=0 and are not stored or replayed. Durable
view payloads over 256 KiB are truncated; after the 64 MiB view fills only
turn boundaries are stored and Session.view_truncated is set. Interrupt
raises the interrupt level as above; StopSession is idempotent, prevents new
turns and stops session jobs. RecoverSession accepts a broken session's
transcript gap (section 7, broken transcripts). DeleteSession deletes a stopped session with no
unexpired lease and no non-terminal call (else FAILED_PRECONDITION); it is
NOT_FOUND once deleted and finishes an interrupted delete when called again.

## 8. Minimal implementation subset

The worker-link server sends **`max_in_flight_messages = 1`** in Welcome
(`workerlink.New`; the daemon link keeps 2). A worker that relies on this
subset MUST treat a Welcome with `max_in_flight_messages` other than 1 as
incompatible (terminal), exactly like a message-limit mismatch. (The Go
worker client accepts any valid value.)

**Why this is conforming.** Welcome's limits bind both directions (section
2), and the Go link applies them to its sender (`canStart`: at most
`min(2, max_in_flight_messages)` partial messages) and to its receiver
(`reassembler.add`: a second partial is a protocol error). With the value 1:
- the server never starts a fragmented message before the previous one's
  `last` fragment is written, and WebSocket keeps frames in order, so the
  worker never sees two partial messages at once; one reassembly buffer
  suffices;
- the worker must not start a second fragmented message before finishing
  the first, which a single FIFO queue guarantees; sending fewer messages in
  flight than allowed is always conforming, because the limits are maxima.

**What a conforming minimal worker needs:**
1. **Transport and handshake** (sections 1-2): TLS/loopback rule, Bearer
   header, no subprotocol, binary frames only, Hello with
   `protocol_version = 1` and a fresh UUIDv7 process boot_id (kept across reconnects),
   the Welcome checks plus the in-flight check above.
2. **Receive:** for each frame, decode the envelope and check the frame
   limits; if it is a `fragment`, feed the **single reassembly buffer**
   (all checks of section 3.3 with an in-flight count of 1: a first fragment
   while a message is partial closes the link); otherwise deliver it at
   once, even while a message is partial. Ignore heartbeats after recording
   activity. Run a per-message idle timer on the partial message (use
   `heartbeat_timeout_ms` as the fragment timeout).
3. **Send:** one FIFO queue of serialized envelopes. Envelopes ≤ F go out
   as one frame. An envelope > F (and ≤ M, ≤ `max_in_flight_bytes`) is
   fragmented with the next `message_id`; only the head of the queue may be
   in fragments, so at most one fragmented message is in flight. Between
   two fragments the worker MAY write heartbeats, and MAY write a small
   business message from further back in the queue only if no earlier
   unsent message has the same ordering key and neither it nor an earlier
   unsent message is a barrier (section 3.2; worker uplink keys are
   `call/<call_id>` and `session/<session_id>`, and the barriers are `Unwatch`, `Attach` and `ResumeLeases`). Never letting
   business messages overtake each other is the simplest correct choice.
4. **Heartbeats** (section 4): send every `heartbeat_interval_ms`; close
   when no frame arrives for `heartbeat_timeout_ms`.
5. **Limits:** do not send anything section 5 would reject (payload sizes,
   repeated-field caps, node/field/depth limits, valid UTF-8); enforce at
   least the frame size, M and the fragment rules on receive.
6. **Close:** orderly close with WebSocket status 1000 after flushing; any
   error closes the connection; treat 1000 with no partial message as
   graceful.
7. **GoAway:** the worker link has none today. If a revision adds it,
   handle it as in section 3.4: stop new sends, flush, close with 1000,
   reconnect after the given delay.
8. **Operations** (section 7): a per-connection request-id counter from 1;
   a table of open operations, at most 64, routing each CallUpdate, SessionReply and WorkerEvent by
   request_id and dropping unknown ids; an operation ends at its last
   reply (terminal update for Submit, one reply for Query/Cancel/SessionRequest/
   ResumeLeases/ListDevices; Attach remains open for WorkerEvent), or
   when the worker sends Unwatch for it, which frees its slot at once. Attach
   uses one of the same 64 slots and is allowed at most once per connection.
   Keep at most 48 call operations (Submit, Query, Cancel) open and queue
   further ones locally, so session operations always find a slot.
9. **Sessions:** apply the lease/epoch, transcript, input, turn, recovery and
   approval rules of section 7. A hint triggers state fetches; it is not a
   counted notification. Close the SDK/CLI on fencing and release; reconnect
   by sending Attach, then ResumeLeases, then the resent session operations
   and calls behind them (both are barriers), only on the newest connection;
   acquire with `fresh` whenever no local state exists.

**What it can skip:** multiple reassembly buffers; the rotating large-message
scheduler (round structure, up to two concurrent fragmented messages);
barrier handling beyond FIFO order (one queue keeps `Unwatch` in place);
the per-key multi-queue scheduler; control priority beyond inserting heartbeats between frames; and
all Go memory accounting (decoded cost, shared budgets, link reserve,
consumer-stall pauses). Its own memory bound is one partial message
(≤ M) plus its receive queue.

## 9. Conformance checklist

- [ ] Uses `wss` (TLS ≥ 1.2, CA file if configured); plaintext only to a loopback name *and* a loopback dialed address; follows no redirects.
- [ ] Sends `Authorization: Bearer <43-char token>`, no subprotocol, no compression, no foreign `Origin`.
- [ ] Treats 401/403 as terminal, 503 as retry-later.
- [ ] Sends exactly one Hello (`protocol_version = 1`; daemon and worker process: `boot_id` a fresh UUIDv7 per process, retained across reconnects; a call-only client sends none) and nothing else before Welcome; completes the handshake within 10 s.
- [ ] Treats Reject as terminal; checks `negotiated_version == 1`, `principal_id`, `max_message_bytes` against its cap, a valid limit set, and (if relying on section 8) `max_in_flight_messages == 1`. Daemon: requires `server_time_unix_ms > 0` and uses it as the dispatch-age anchor.
- [ ] Sends only binary frames, each one envelope with exactly one known payload.
- [ ] Sends envelopes > F as fragments: chunks ≤ F, `message_id` nonzero and strictly increasing, `index` from 0 without gaps, `total_size` only on index 0, `last` exactly on the final chunk, no nested fragments.
- [ ] Respects `max_in_flight_messages` and `max_in_flight_bytes` when sending; enforces them when receiving.
- [ ] Keeps per-key FIFO (same call id or same session id) and barrier order; assumes no order between different keys.
- [ ] Accepts small messages and heartbeats between fragments; delivers messages in completion order.
- [ ] Closes on any malformed, out-of-order, oversize or timed-out fragment, and on a close with a partial message.
- [ ] Sends heartbeats every `heartbeat_interval_ms` regardless of traffic; closes after `heartbeat_timeout_ms` with no received frame.
- [ ] Never sends an envelope that violates section 5 (sizes, repeated caps, nodes, fields, depth, groups, UTF-8).
- [ ] Shuts down by flushing and closing with 1000; treats other closes as errors.
- [ ] Worker link: sends a nonzero `request_id` on every business payload and never reuses one on a connection (a counter from 1); routes CallUpdate/SessionReply/WorkerEvent by `request_id` and drops unknown ids; keeps at most 64 operations open including Attach and all session operations; ends an operation at its last reply (Submit: terminal update; Query, Cancel, SessionRequest, ResumeLeases and ListDevices: their one reply; Attach: stream until Unwatch/disconnect) or by sending `Unwatch`, which frees the slot at once and gets no reply; keeps `Unwatch` before any later operation; treats `unknown` and `NOT_FOUND` as nonterminal call states; recovers by reconnecting and querying.
- [ ] Worker sessions: Attach at most once per connection; advertise capacity 1..64; treat WorkerEvent hints (all but UpdateMachine) as repeatable/coalescible and fetch authoritative state, and act on each UpdateMachine it receives, since it is sent once; answer an Offer or Nudge for a session it does not hold with AcquireLease; interrupt only a turn whose `turn_no` ≤ `interrupt_turn_no`; release with STOPPED on Nudge STOP or any STOPPED.
- [ ] Keeps at most 48 call operations (Submit, Query, Cancel) open per connection, so session operations always find a slot.
- [ ] Uses epoch 0 only for AcquireLease, sets `fresh` whenever it holds no local state, and retries a lost AcquireLease reply with AcquireLease; after a reconnect sends Attach, then ResumeLeases, then resent operations; fences every other session read/write and SubmitCall; sends session operations only on its newest connection; carries opaque tool_use_id; treats SESSION_FENCED and SESSION_STOPPED as terminal and unrecorded.
- [ ] Reuses an unacknowledged append or event `batch_seq` with identical content until it is acknowledged (an event batch answered INVALID is consumed; an append answered INVALID or LIMIT is reported to the SDK as failed), splitting oversized SDK appends into consecutive batches; preserves ReportTurn local_seq/phase across retries; reuses taken input ids as SDK uuids; marks persisted items only after the SDK result, splitting them across PatchSessionState.persisted when needed; records owed call notices.
- [ ] Flushes every queued PublishEvents batch for a turn and waits for acknowledgment (or INVALID consumption) before sending its ENDED or a successor's STARTED, so control-plane turn boundaries follow the turn's worker events in seq order.
- [ ] Reports mirror_error as ENDED with `transcript_incomplete`, repairs, then sends TranscriptRepaired.
- [ ] Reconnects with the same boot and ResumeLeases within TTL; closes SDK and confirms CLI exit on lease loss/release; Unwatches calls without Cancel and never cancels calls of a query closed for fencing, stop or release; injects Recovery before the next input without automatically resuming.
- [ ] Respects section 5 caps, the section 7 field caps, 8 MiB transcript batches/pages (single-entry exception), the 64 KiB limit for every other session op, 128-event batches, input limits and one-use grants.
