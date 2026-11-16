# circus link wire protocol

This is the wire protocol between the control plane and its two kinds of
link clients: the device daemon (`/v1/link/daemon`) and workers
(`/v1/link/worker`). It is written so that a peer can be implemented without
reading Go. Transport behaviour is implemented in Go; numeric transport
limits name their Go symbols (package `wire` unless qualified). Session
processing rules define the contract for the session implementation; the
currently unimplemented processing is identified below.

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
| `boot_id` | MUST be 1-128 bytes; a fresh UUIDv7 per process start, kept across reconnects | MUST be 1-128 bytes; a fresh UUIDv7 per worker process, kept across reconnects |
| `client_version` | informational | informational |
| `os`, `arch`, `hostname`, `machine_id` | sent, informational | not checked |

The control plane uses a changed worker `boot_id` to release all leases of
that worker's previous process. Workers MUST ensure their old CLI processes
have exited before a new process resumes sessions. Worker boot validation,
previous-boot lease release, and setting boot_id in the Go worker client are
**to be implemented**; the current Go server still accepts an empty worker
boot_id. The whole Hello envelope must decode within 64 KiB (`MaxMetadataBytes`).

**Server decision (`AcceptHello`)**, in this order:
1. Frame does not decode, or its payload is not `hello` → Reject `CODE_BAD_HELLO`.
2. Link-specific check fails (daemon: `boot_id` empty or longer than 128 bytes) → `CODE_BAD_HELLO`.
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
| `DaemonUplink` | hello 1, heartbeat 2, fragment 3, call_ack 10, result 11, call_status 12, job_event 13, job_snapshot 14 |
| `DaemonDownlink` | welcome 1, reject 2, heartbeat 3, fragment 4, go_away 5, dispatch_call 10, cancel_call 11, call_query 12, list_jobs 13, stop_session_jobs 14 |
| `WorkerUplink` | hello 1, heartbeat 2, fragment 3, submit_call 10, cancel_call 11, call_query 12, unwatch 13, attach 14, session 15, resume_leases 16, list_devices 17 |
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
| job_event (daemon uplink) | Data | one key for the whole boot |
| session (worker uplink) | Data | `session/<session_id>` |
| list_devices, session_reply, worker_event | Data | none |
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
| fragment | F + 128 (`FrameOverhead`) |
| everything else (hello, welcome, reject, heartbeat, go_away, cancel_call, call_query, unwatch, list_jobs, stop_session_jobs, job_event, attach, resume_leases, list_devices, worker_event) | 64 KiB (`MaxMetadataBytes`) |

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
- Unfragmented frames ≤ F; reassembled envelopes ≤ M (section 3.3).

**Structure limits** (per envelope, counted over the whole tree):

| Limit | Value |
| --- | --- |
| Message nesting depth, envelope included | 32 (`scanMessage`, `RecursionLimit`) |
| Message nodes (envelope counts as one) | 8192 (`MaxDecodeNodes`) |
| Field occurrences: every tag at every level, unknown fields included; a packed repeated field counts once | 65536 (`MaxDecodeFields`) |
| `JobSnapshot.jobs` elements | 4096 (`MaxSnapshotJobs`) |
| `ToolOutput.content` elements | 128 |
| `BashRequest.working_dirs` elements | 256 |
| `CallUpdate.DeviceOffline.online_devices` elements | 256 |
| `WorkerEvent.Nudge.reasons`, `GetSessionState.sections` | 5 each, unpacked enums |
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

Go sender detail, for reference only (`Link.next`): each scheduling round
writes at most one eligible control frame, then one small business frame,
then up to `min(2, max_in_flight_messages)` fragments, rotating between
started large messages. A started fragmented control message rotates like
any large message. A sender is conforming as long as it keeps the rules of
sections 3.2 and 3.3; this round structure is not required.

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
| `Attach` | Advertise 1..64 session capacity and SDK/CLI/account/check metadata; at most once per connection, even after Unwatch. | WorkerEvent stream: Offer, Nudge, LeaseRevoked, starting with every Offer still pending for this worker. Hints are level-triggered, may repeat or coalesce, and carry no authoritative state. Account is recorded only. | Unwatch or disconnect. |
| `SessionRequest{session_id, epoch, op}` | One of the session operations below; Data with key `session/<session_id>`. | Exactly one SessionReply, success, Fenced or Rejected. | At that reply (or Unwatch). |
| `ResumeLeases{leases}` | Reattach up to 64 (session_id, epoch) leases on the same worker boot before expiry, rebinding each to this connection. | One SessionReply.resumed, one outcome per requested lease, with retained epoch (and `stopped`) on success or current epoch when fenced. | At that reply (or Unwatch). |
| `ListDevices{}` | List the user's devices. | One SessionReply.devices, at most 256 devices with name, online, os, arch, hostname, last_seen_unix_ms. | At that reply (or Unwatch). |
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

**Server-side session processing is to be implemented.** The generated
messages, scheduling classes/keys and structural limits (including the
one-op rule) exist; today sending Attach, SessionRequest, ResumeLeases or
ListDevices takes the existing `unexpected payload` path and closes the
link. The Connect SessionService also has generated bindings only.
SubmitCall's lease admission, its per-session caps and validation of its new
session fields, and the Attach field caps (the one field-cap violation
that closes the link), are **to be implemented**; existing call routing continues
without them until that integration is added. Everything below is the
contract that implementation follows.

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
submission of the same `call_id` is unaffected). A stopped session gives
`rejected SESSION_STOPPED`, also terminal and unrecorded.

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
lease bound to a closed connection expires naturally. After a control-plane
start, no lease expires sooner than one lifetime later, so holders can
reconnect and ResumeLeases even after a longer outage. Workers reconnect with
backoff capped at 10 seconds. A new worker process uses a new `boot_id`; the
control plane then releases every lease of the previous boot.

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

WorkerEvent hints are level-triggered: they may be coalesced or repeated,
carry no authoritative state, and never replace the fence. The control plane
keeps each session's pending reasons and re-sends them:
- after `Attach`, every Offer still pending for this worker, on the
  attaching connection;
- after each successful ResumeLeases outcome, that session's pending Nudge
  reasons.

| Hint | Worker action |
| --- | --- |
| Offer, session not held | AcquireLease, with `fresh` when the worker has no local state for the session. If the session is being released, remember the Offer and act on it after the ReleaseLease reply. |
| Nudge, session not held | AcquireLease with `fresh` (NOT_OFFERED unless this boot still holds the lease). |
| Any hint, AcquireLease in flight | Ignore it until the reply arrives; the session counts as held. |
| Nudge INPUT or APPROVAL | TakeInputs (an approved request arrives as a `KIND_APPROVAL` input). |
| Nudge JOB | GetSessionState with `SECTION_JOBS`. |
| Nudge INTERRUPT | Interrupt the current turn only if its `turn_no` ≤ `Nudge.interrupt_turn_no`. If STARTED has not been answered yet, wait for its `turn_no`, then compare. |
| Nudge STOP | Stopping is permanent, so the hint suffices: flush, close the query, ReleaseLease with `REASON_STOPPED`. |
| LeaseRevoked for the held epoch | The fencing procedure above. |

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
| Refused | TakeInputs, RequestApproval, ConsumeGrant: `rejected STOPPED`. SubmitCall: `rejected SESSION_STOPPED` (terminal; nothing recorded or executed). A non-reattaching AcquireLease: `STOPPED` (a `fresh` one by the holder also releases the lease). |
| Accepted | AppendTranscript, ReportTurn, PatchSessionState, PublishEvents, TranscriptRepaired, LoadTranscript, ListTranscriptKeys, GetSessionState, ReleaseLease, ResumeLeases, a reattaching AcquireLease. |

On any STOPPED the worker closes the query and sends ReleaseLease with
`REASON_STOPPED`. `TRANSCRIPT_FULL` stops the session atomically with that
rejection; the worker then releases with `REASON_STOPPED` rather than
dropping entries.

#### Operations

| Session op | Successful SessionReply | Rules |
| --- | --- | --- |
| AcquireLease | lease | `fresh` as above. epoch, stable claude_session_id, has_transcript, `state` (every section except calls), `recovery`, shared SessionConfig, `reattached` and `transcript_gap_turn_no` (broken transcripts below). NOT_OFFERED and STOPPED as above. |
| ReleaseLease | ok | Reason IDLE, STOPPED, ERROR or FENCED; relinquishes ownership; repeats as above. |
| LoadTranscript | transcript_page | Opaque subpath (empty = main), exclusive after_seq, max_bytes 1..8 MiB. Return at least one entry if available, even if it alone exceeds max_bytes; it must fit M with its envelope. Continue at next_seq until done. |
| ListTranscriptKeys | transcript_keys | At most 1024 subpaths. |
| AppendTranscript | ok | Atomic append; at most 8 MiB encoded entries, or one larger entry fitting M. JSON is opaque and never parsed. Retry rules below. |
| TakeInputs | inputs | Atomically take pending input; return all taken in this epoch and not yet persisted, at most 32, each text at most 256 KiB. |
| ReportTurn | turn | STARTED and ENDED return the same assigned turn_no. Rules below. |
| PatchSessionState | ok | Idempotent upserts/marks. Cwd by (agent_id, device), files by (device, path), CLI tasks by task_id. Empty sha256 forgets a file. Mark returned calls as soon as MCP returns, handed jobs when delivered to the SDK; `persisted` and `awaiting_call_ids` below. |
| GetSessionState | state | Empty sections = all; otherwise at most five distinct sections (cwds, files, jobs, calls, grants), or `SECTION_NONE` alone for no data. `stopped` and `interrupt_turn_no` always. Metadata only; use CallQuery for call output. |
| PublishEvents | ok | At most 128 per batch, normally every 100 ms; shared SessionEvent.Kind with json and ephemeral. `batch_seq` per epoch, retry rules as AppendTranscript. |
| RequestApproval | approval | Merge pending requests by (session, tool, device, match_key); reply includes approval_id and deduplicated. `match_key` is the SHA-256, in 64 lowercase hex digits, of the bash command verbatim or of the write/edit path; display, reason and tool_use_id do not change it. |
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
control plane records, such as `TRANSCRIPT_GAP_ACCEPTED`) consumes its
`batch_seq`: the control plane records it as dropped and answers `INVALID`,
and the worker continues with the next sequence, so one bad event cannot
freeze the view. A worker treats every PublishEvents `INVALID` as consumed:
the two cases share the code, and a gap only follows from a worker bug. `STORE_UNAVAILABLE` and Fenced consume nothing.

#### Inputs and turns

TakeInputs retries return everything taken in this epoch and not yet
persisted. Push to the SDK with uuid=input_id, avoiding duplicate pushes
within the epoch. NORMAL (and unspecified priority) merges into a running
turn or starts one when idle; NOW interrupts the current turn first.
Approval inputs use NORMAL.

ReportTurn is idempotent by (epoch, local_seq, phase). Repeats retain the
turn_no, do not increment turn_count twice and do not duplicate persistence
marks. STARTED is the first SDK message belonging to a new turn and must use
the next local_seq (1 for the epoch's first turn); a STARTED whose
predecessor never ENDED closes that turn as INTERRUPTED, and a late ENDED
for it still applies its persisted marks and `transcript_incomplete`
without changing its outcome. ENDED for a local_seq never STARTED is
`INVALID`. ENDED follows the SDK result, including an interrupted empty
result, and carries outcome COMPLETED, INTERRUPTED or ERROR, opaque
usage_json, optional `persisted` and `transcript_incomplete`. Origin
describes what started the turn: input pushed while idle, a notification
pushed while idle, or the CLI itself (for example subagent completion).
Report turns of all origins.

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
repaired.

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

#### Broken transcripts

On `mirror_error`, close the query and send ENDED with
`transcript_incomplete = true` and without `persisted`: the session becomes
BROKEN. A CLI failure without mirror_error is ENDED with ERROR and without
the flag (then release); it does not make the session BROKEN. While BROKEN:
appends are accepted (for the repair), inputs already taken stay taken and
unpersisted, and new inputs are accepted but not taken. The holder
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
two hours).

#### Rejections and field caps

SessionReply.rejected codes are INVALID, NOT_FOUND, STORE_UNAVAILABLE,
TRANSCRIPT_FULL, LIMIT, STOPPED and NOT_OFFERED. STORE_UNAVAILABLE is
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

The user-side Connect API creates sessions with SessionConfig and an
optional pinned worker; SendMessage deduplicates by (session_id,
client_msg_id), retaining identical content, and returns input_id/event_seq.
A stopped session refuses SendMessage; a broken one accepts but does not
deliver until repaired. ListSessions is paged (default 100, maximum 1000)
and leaves `config` unset; GetSession returns it. A user has at most 1000
sessions. WatchSession replays durable events strictly after after_seq, then
streams live events. SessionEvent carries seq, ts_unix_ms, shared kind,
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
   `protocol_version = 1` and a fresh process boot_id (kept across reconnects),
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
- [ ] Sends exactly one Hello (`protocol_version = 1`; both roles: `boot_id` 1-128 bytes, fresh UUIDv7 per process, retained across reconnects) and nothing else before Welcome; completes the handshake within 10 s.
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
- [ ] Worker sessions: Attach at most once per connection; advertise capacity 1..64; treat WorkerEvent as repeatable/coalescible hints and fetch authoritative state; answer an Offer or Nudge for a session it does not hold with AcquireLease; interrupt only a turn whose `turn_no` ≤ `interrupt_turn_no`; release with STOPPED on Nudge STOP or any STOPPED.
- [ ] Keeps at most 48 call operations (Submit, Query, Cancel) open per connection, so session operations always find a slot.
- [ ] Uses epoch 0 only for AcquireLease, sets `fresh` whenever it holds no local state, and retries a lost AcquireLease reply with AcquireLease; after a reconnect sends Attach, then ResumeLeases, then resent operations; fences every other session read/write and SubmitCall; sends session operations only on its newest connection; carries opaque tool_use_id; treats SESSION_FENCED and SESSION_STOPPED as terminal and unrecorded.
- [ ] Reuses an unacknowledged append or event `batch_seq` with identical content until it is acknowledged (an event batch answered INVALID is consumed; an append answered INVALID or LIMIT is reported to the SDK as failed), splitting oversized SDK appends into consecutive batches; preserves ReportTurn local_seq/phase across retries; reuses taken input ids as SDK uuids; marks persisted items only after the SDK result, splitting them across PatchSessionState.persisted when needed; records owed call notices.
- [ ] Reports mirror_error as ENDED with `transcript_incomplete`, repairs, then sends TranscriptRepaired.
- [ ] Reconnects with the same boot and ResumeLeases within TTL; closes SDK and confirms CLI exit on lease loss/release; Unwatches calls without Cancel and never cancels calls of a query closed for fencing, stop or release; injects Recovery before the next input without automatically resuming.
- [ ] Respects section 5 caps, the section 7 field caps, 8 MiB transcript batches/pages (single-entry exception), the 64 KiB limit for every other session op, 128-event batches, input limits and one-use grants.
