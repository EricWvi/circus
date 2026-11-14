# circus link wire protocol

This is the wire protocol between the control plane and its two kinds of
link clients: the device daemon (`/v1/link/daemon`) and workers
(`/v1/link/worker`). It is written so that a peer can be implemented without
reading Go. Where it states behaviour, the Go code is the source; each number
names the Go symbol it comes from (package `wire` unless qualified) so it can
be re-checked.

Protocol version: **1** (`ProtocolVersion`). Messages are the proto3 messages
in `proto/circus/v1/` (`link.proto`, `daemon_link.proto`,
`worker_link.proto`, `device.proto`); TypeScript bindings are generated into
`gen/ts` (`@circus/proto`). "MUST" marks what the Go peer enforces (breaking
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
| Server connection cap (HTTP 503 when full) | 100 (`gateway.Config.MaxConnections`, max 256) | 2 (`workerlink.maxConnections`) |

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
| 503 | Authentication store failure; connection cap full; server shutting down. |

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
| `boot_id` | MUST be 1-128 bytes; a fresh UUIDv7 per process start, kept across reconnects | not checked (Go worker client leaves it empty) |
| `client_version` | informational | informational |
| `os`, `arch`, `hostname`, `machine_id` | sent, informational | not checked |

The whole Hello envelope must decode within 64 KiB (`MaxMetadataBytes`).

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
(`controlplane/links.go`; M and the heartbeat values are configurable,
F is not).

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
| `max_in_flight_messages` | 2; allowed 1..2 | Max partially transferred fragmented messages per direction. |
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
| `WorkerUplink` | hello 1, heartbeat 2, fragment 3, submit_call 10, cancel_call 11, call_query 12 |
| `WorkerDownlink` | welcome 1, reject 2, heartbeat 3, fragment 4, call_update 10, job_event 11 |

A received envelope MUST contain exactly one known payload field, encoded
as a length-delimited field (`scan`). Unknown fields next to it are ignored,
but an envelope whose only payload is a field number the receiver does not
know is rejected ("missing envelope payload"), so adding a payload type is a
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
| list_jobs, stop_session_jobs | Barrier | none |
| dispatch_call, submit_call, cancel_call, call_query, call_update | Data | `call/<call_id>` |
| job_event (daemon uplink) | Data | one key for the whole boot |
| job_event (worker downlink) | Data | one key per (device, boot_id) |
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
| dispatch_call, submit_call, result, call_status, call_ack, call_update | M |
| job_snapshot | 8 MiB (`MaxSnapshotBytes`) |
| fragment | F + 128 (`FrameOverhead`) |
| everything else (hello, welcome, reject, heartbeat, go_away, cancel_call, call_query, list_jobs, stop_session_jobs, job_event) | 64 KiB (`MaxMetadataBytes`) |

Further size rules:
- call_status, call_ack, call_update: envelope size minus the encoded size of
  all embedded `DeviceResult` messages ≤ 64 KiB; only result output may be
  large.
- Every `JobEvent` message anywhere in the tree (including
  `JobSnapshot.jobs` and `WorkerJobEvent.event`) ≤ 64 KiB encoded.
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

The repeated-field caps (`repeatedLimits`) count all elements of that field
in the envelope, not per parent. Also rejected: groups (wire types 3/4)
anywhere, field number 0, truncated or malformed fields, a known
message-typed field not encoded as length-delimited, more than one payload
in the envelope, and anything the protobuf decoder refuses, notably
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
- **Barriers:** `ListJobs`/`StopSessionJobs` arrive after every Data/Barrier
  message sent before them, and before every one sent after them.
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

## 7. Worker link today

> **This section will change.** A worker-protocol revision is planned before
> the TypeScript worker is written: request IDs on the envelope, an
> `Unwatch` message, exactly one reply to Cancel, and configurable
> connection counts. This section describes the current code only; the wire
> format of those changes is not defined yet.

| Uplink payload | Effect | Reply |
| --- | --- | --- |
| `SubmitCall{device, call}` | Submit a `DeviceCall` to the user's device named `device` (1-128 bytes). `call.call_id` and `call.session_id`: 1-128 bytes of `[A-Za-z0-9_-]`; `agent_id`: empty or the same grammar; exactly one tool. | A stream of `CallUpdate`s with this `call_id`: zero or more nonterminal updates, then one terminal update (unless the link closes first). |
| `CallQuery{call_id}` | Read the call's current state. | Exactly one `CallUpdate` (may be nonterminal). |
| `CancelCall{call_id}` | Cancel before dispatch; after dispatch the intent is recorded and the daemon's result wins. Unknown, invalid or finished ids are no-ops. | **Never a reply.** The outcome arrives on a watched Submit stream or through a later Query. |

`CallUpdate` variants:

| Variant | Terminal (`wire.Terminal`) |
| --- | --- |
| `accepted`, `dispatched` (also used for "running") | no |
| `unknown` (dispatched, outcome open; e.g. reason `DEVICE_OFFLINE` from Query) | **no**: the stream continues; a result can still arrive |
| `result` (a `DeviceResult`) | yes |
| `offline` (device offline, with the user's online device names) | yes |
| `lost` (e.g. reason `RESULT_UNAVAILABLE`) | yes |
| `rejected` with `INVALID`, `OVERLOADED`, `CALL_MISMATCH`, `UNKNOWN_DEVICE`, `STORE_UNAVAILABLE`, `DEVICE_SHUTTING_DOWN` | yes |
| `rejected` with `NOT_FOUND` | **no**: it answers only that Query (no call with this id is visible to this user) and is not a call outcome |
| `cancelled` | yes |

Semantics today:
- **Correlation is by `call_id` only**; there is no request id. A client
  must not have two operations on the same `call_id` open on one connection
  (the Go client enforces this), because their updates cannot be told
  apart. Updates for a `call_id` the client is not waiting for are dropped
  by the Go client.
- Re-submitting the same `call_id` with identical content for the same
  device attaches to the existing call; different content or device yields
  `CALL_MISMATCH`.
- **Per-connection operation cap: 4** (`workerlink.maxOperations`). Each
  Submit holds a slot until its terminal update; each Query until its
  answer; Cancel holds none. The slot is released *before* the
  terminal/answer is sent, so a new operation may follow immediately. A
  fifth concurrent operation **closes the link** (it is not rejected).
- **Connections:** at most 2 authenticated worker-link connections per
  control plane, of any kind (`workerlink.maxConnections`); the next gets
  HTTP 503.
- The server **closes the link** (abort, no notice) when: the operation cap
  is exceeded; a rejection would carry an empty `call_id` (a Submit whose
  `call_id` is invalid, or a Query with an empty one; a Query with a
  nonempty invalid id gets `rejected INVALID`); the router reports overload
  or an ambiguous store failure; a Submit names a `call_id` owned by another
  user; the worker reads too slowly, so the server's send queue overflows
  (256 messages or 2 × M bytes, `Options.defaults`) or a call's update
  buffer overflows (16 updates, 144 MiB or a 15 s stall: `calls.Config`
  defaults); a send stays refused by the shared memory pool for 10 s
  (`workerlink.sendBusyLimit`); an unexpected payload arrives; or the
  credential is revoked. Recovery is always: reconnect, then `CallQuery`.
  Closing a connection never cancels a call.
- `WorkerJobEvent` (`job_event`) is defined but not sent yet. The server
  sends no GoAway on this link.

## 8. Minimal implementation subset

Decided rule: the worker-link server will send **`max_in_flight_messages =
1`** in Welcome. This is not in the code yet (today it sends the default 2);
it lands with the protocol revision of section 7. A worker that relies on
this subset MUST treat a Welcome with `max_in_flight_messages` other than 1
as incompatible (terminal), exactly like a message-limit mismatch.

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
   `protocol_version = 1`, the Welcome checks plus the in-flight check above.
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
   unsent message has the same ordering key (section 3.2; worker uplink
   keys are `call/<call_id>`, and there are no barriers). Never letting
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

**What it can skip:** multiple reassembly buffers; the rotating large-message
scheduler (round structure, up to two concurrent fragmented messages);
barrier handling (the worker uplink has none); the per-key multi-queue
scheduler; control priority beyond inserting heartbeats between frames; and
all Go memory accounting (decoded cost, shared budgets, link reserve,
consumer-stall pauses). Its own memory bound is one partial message
(≤ M) plus its receive queue.

## 9. Conformance checklist

- [ ] Uses `wss` (TLS ≥ 1.2, CA file if configured); plaintext only to a loopback name *and* a loopback dialed address; follows no redirects.
- [ ] Sends `Authorization: Bearer <43-char token>`, no subprotocol, no compression, no foreign `Origin`.
- [ ] Treats 401/403 as terminal, 503 as retry-later.
- [ ] Sends exactly one Hello (`protocol_version = 1`; daemon: `boot_id` 1-128 bytes) and nothing else before Welcome; completes the handshake within 10 s.
- [ ] Treats Reject as terminal; checks `negotiated_version == 1`, `principal_id`, `max_message_bytes` against its cap, a valid limit set, and (if relying on section 8) `max_in_flight_messages == 1`. Daemon: requires `server_time_unix_ms > 0` and uses it as the dispatch-age anchor.
- [ ] Sends only binary frames, each one envelope with exactly one known payload.
- [ ] Sends envelopes > F as fragments: chunks ≤ F, `message_id` nonzero and strictly increasing, `index` from 0 without gaps, `total_size` only on index 0, `last` exactly on the final chunk, no nested fragments.
- [ ] Respects `max_in_flight_messages` and `max_in_flight_bytes` when sending; enforces them when receiving.
- [ ] Keeps per-key FIFO (same call id) and barrier order; assumes no order between different keys.
- [ ] Accepts small messages and heartbeats between fragments; delivers messages in completion order.
- [ ] Closes on any malformed, out-of-order, oversize or timed-out fragment, and on a close with a partial message.
- [ ] Sends heartbeats every `heartbeat_interval_ms` regardless of traffic; closes after `heartbeat_timeout_ms` with no received frame.
- [ ] Never sends an envelope that violates section 5 (sizes, repeated caps, nodes, fields, depth, groups, UTF-8).
- [ ] Shuts down by flushing and closing with 1000; treats other closes as errors.
- [ ] Worker link: correlates by `call_id` with at most one open operation per id; keeps at most 4 Submit/Query operations open; expects no reply to Cancel; treats `unknown` and `NOT_FOUND` as nonterminal; recovers by reconnecting and querying.
