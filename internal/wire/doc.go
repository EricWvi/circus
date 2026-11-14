// Package wire transports bounded protobuf envelopes over authenticated binary
// message connections. Authentication, Hello/Welcome/Reject and limit negotiation
// run on raw Conn with Decode; construct a side-typed Link only AFTER Welcome.
// AcceptWebSocket/DialWebSocket retain an abort-capable transport; a bare coder
// Conn cannot interrupt an already-started Close with CloseNow. For a raw Reject,
// write it synchronously and use WebSocket.Shutdown(ctx) to close gracefully.
//
// Send(ctx, message) classifies each message itself and preserves FIFO for each
// nonempty ordering key: a call key for Dispatch/Submit/Query/Cancel/updates,
// one boot-wide event key for JobEvent, Barrier for ListJobs/StopSessionJobs, and
// Control for heartbeat/GoAway/ack. Ordering keys are cloned and capped
// at 1024 bytes so a short substring cannot retain an unbounded backing string.
// Barriers fence earlier business messages; later business cannot overtake them. Different keys may interleave. At most
// two fragmented messages start, respecting negotiated in-flight count/bytes.
// Each round serves one eligible control, one small business message and up to K
// rotating large fragments. Started controls join rotation; neither a small
// business burst nor one large ack can starve an already-started partial.
//
// Inputs and tool-output envelopes follow negotiated MaxMessageBytes (default
// 64 MiB), including DeviceResult inside CallStatus, a duplicate CallAck or
// a worker CallUpdate. Ranged Read can format 6.4 MB, and Edit failures echo
// old_string near the input limit; wire does not impose a smaller result cap.
// Status-only envelopes and all metadata outside embedded results stay within
// 64 KiB, enough for IDs, state, diagnostics and bounded device lists. Other
// metadata envelopes and each JobEvent also have a 64 KiB cap.
// JobSnapshot has independent aggregate 8 MiB and 4096-entry limits: even 4096
// maximum-size events (4096*64 KiB plus framing) must fit the tighter aggregate
// cap. These are transport budgets, NOT a claimed jobs.Manager capacity: the
// existing manager has no fixed count cap. Callers must handle oversize snapshots;
// pagination/daemon retention changes are outside this package. Fragment framing
// is capped by the negotiated fragment size plus 128 bytes.
//
// A protowire scan precedes Unmarshal: at most 8192 message nodes, 65536 fields,
// 4096 snapshot jobs, 128 content blocks and 256 directories/online device names.
// Duplicate envelope payloads and groups are rejected. Decoded admission charges
// 2*encoded_bytes + 256*nodes + 64*fields + 4096, not proto.Size. This covers
// copied strings/unknown bytes, generated objects, slice capacity and size-class
// overhead conservatively on the supported 64-bit Go targets. Receive blocks on
// its byte budget. A reader pause suspends peer/fragment deadlines; upon
// resumption the peer clock resets and partial deadlines shift by the pause.
// Each wait lasts at most MaxConsumerStall, default HeartbeatTimeout. Expiry
// while waiting for this link's own consumer (local receive space or the link
// reserve) closes with ErrConsumerStalled; expiry while waiting for the shared
// receive pool closes with ErrBudgetExhausted. Neither is a peer protocol error,
// and no pause can keep a connection alive indefinitely.
//
// Memory model with default options (MiB per endpoint, live ownership, not RSS):
// Send owns at most Q=128 encoded queue bytes plus 2B=128 while one Send holds
// a marshaled B=64 buffer and its fragment copies (fragmented messages are
// copied into fragment-sized chunks so each written fragment is freed at once).
// One reader owns R=136 of received admission. Its phases are mutually exclusive:
//
//   - filling partials: at most 2B of actual blocks, plus R;
//   - flattening: at most 2B of blocks plus B of new flat buffer, plus R;
//   - decoding: at most B of another partial, B of flat input, 3B+6 MiB+4 KiB
//     of conservative decoder growth/objects, plus R;
//   - waiting for local receive space: at most B of another partial, one
//     decoded cost <=2B+6 MiB+4 KiB, plus R. Flat input is no longer live.
//
// Take the MAXIMUM phase, not their sum: receive <=462 MiB+4 KiB. Add send
// 256 MiB and 5 MiB of fixed overhead (below): <=723 MiB+4 KiB, rounded up to
// 736 MiB per endpoint without a Budget. This bound is deliberately
// conservative; it does not claim every maximum can occur together. The opt-in
// TestReceiveMemoryPhases reproduces partial reassemblies plus a decoded
// message waiting for Receive, and logs post-GC live heap separately from
// peak HeapAlloc (which has garbage).
// Two tiny first fragments still allocate <=128 KiB, not their 128 MiB declaration.
//
// Fixed per-link overhead, never charged to a pool: one frame being read and
// one being written (2*(F+128), 512 KiB), queued ordering keys (256*1 KiB),
// queue, chunk, block and received-entry metadata (received entries cost 32 B
// each against >=4 KiB of charged admission, about 2.1 MiB at the default
// R with slice growth) and goroutine stacks: about 3 MiB, planned as 5 MiB.
// A budgeted link adds its LinkReserveBytes (64 KiB) reserve.
//
// For a process cap, create ONE NewBudget(sendLimit, receiveLimit) and pass its
// pointer to every Link. The pools are independent: sending never consumes
// receive capacity or the reverse. Reserve before allocation:
//
//   - send pool: serialized sends (2n during the chunk copy of a fragmented
//     message, then n). Each written fragment releases its bytes at once, so a
//     slow link holds only its unsent bytes. Exhaustion returns recoverable
//     ErrBusy before Marshal; Send never blocks on the pool.
//   - receive pool: actual assembly block capacity (not declared total_size),
//     flat buffers, and decoded cost plus one encoded length of temporary
//     decoder credit for every reassembled message and every frame whose
//     admission exceeds LinkReserveBytes. Retained costs move to received
//     entries until Receive.
//   - link reserve: frames whose admission fits LinkReserveBytes (heartbeats,
//     acks, GoAway, small control/business, about 5 KiB each) decode from the
//     link's own reserve and never wait for the receive pool, so liveness of a
//     link with a live peer and a healthy consumer never depends on other links.
//     Capacity: accounting cost, not size, fills it. A ~40-byte JobEvent costs
//     about 5 KiB, so a budgeted link buffers only about 12 unread small
//     messages before its reader pauses (backpressure; after MaxConsumerStall,
//     ErrConsumerStalled). Without a Budget the bound is MaxReceiveBytes
//     (136 MiB). Consumers must keep Receive running to sustain throughput.
//
// New rejects pools that cannot hold one maximum-size message (MinBudget):
// send >= 2M, receive >= 2M + 2M + 6 MiB + 4 KiB = 4M + 6 MiB + 4 KiB, which is
// 128 MiB and 262 MiB + 4 KiB with defaults. Size the receive pool for the
// concurrency you need: one link with two interleaved maximum messages can need
// its full 462 MiB receive phase bound, so the minimum alone can starve it.
//
// A link that ends for any reason returns every shared reservation without
// Receive or Close. Partials and queued sends return as its goroutines exit (I/O
// is aborted). Received-but-undelivered messages depend on how it ended:
//
//   - graceful end (the peer's EOF after a complete frame, a WebSocket normal
//     close, or a successful local Shutdown): every message stays deliverable
//     in order, then Receive returns the close error. Messages not drained
//     within MaxConsumerStall of the end are dropped and their budget returned;
//     a GoAway behind a pool-holding message is therefore still delivered.
//   - error, stall, budget exhaustion or local Close/abort: they are dropped at
//     once from the first one holding shared budget onward, so Receive yields
//     an in-order prefix of reserve-only messages and then the link error.
//
// After the end the reader accepts no further message. With no Budget, nothing
// is dropped before Receive or Close.
// Releases broadcast to every waiting link. Stats.SharedSendBytes,
// Stats.SharedReceiveBytes, Stats.LinkReserveBytes and Budget.Stats expose real
// ownership and peak admission. A healthy link whose consumer is merely slow
// keeps up to MaxReceiveBytes of received charges; there is no per-link share
// of the pool, so a few such links can make others wait (and, past
// MaxConsumerStall, fail with ErrBudgetExhausted).
//
// Thus charged process ownership never exceeds sendLimit + receiveLimit, and
// conservative live wire planning is
//
//	sendLimit + receiveLimit + N * (5 MiB + 64 KiB)
//
// for N budgeted links with defaults, e.g. NewBudget(256 MiB, 512 MiB) with 100
// links is about 1.25 GiB instead of 100*736 MiB. Caller-owned protos and
// received values, kernel/TLS buffers, Go runtime, race instrumentation and GC
// garbage (such as a marshaled buffer after its chunk copy) are excluded. With
// GOGC=100, allow roughly a further live-heap-sized GC margin; this is an
// estimate, not an RSS guarantee. Use GOMEMLIMIT and higher-layer connection
// limits as well. Options changes require recomputing the overhead and phases.
//
// Complete-frame liveness assumes throughput greater than
// 2*max_fragment_bytes/min(heartbeat_timeout, fragment_timeout) of effective
// fragment service capacity, AFTER priority traffic (~35 KiB/s without competing
// traffic by default); use smaller negotiated fragments on
// slower links. With both control and small traffic each round can contain four
// frames, requiring roughly 70 KiB/s at default fragment size/timeout.
// Absent local consumer pauses, timer sampling detects half-open
// peers in timeout..timeout+min(interval,fragment_timeout), default 15..20 s.
// A subsequent 30 s gateway grace therefore gives 45..50 s, not exactly 45 s.
//
// Barriers also delay unrelated cancels behind earlier large dispatches;
// cancellation watchdogs must account for that delay. Send serializes under a
// per-link mutex; another Send waiting for it does not observe ctx until it
// acquires the mutex (shared exhaustion still returns ErrBusy before that wait).
// Shutdown should use a caller deadline; deadline/cancellation wraps both
// ErrShutdownTimeout and the context error, preserving errors.Is for each.
// Shutdown stops Send, drains the accepted queue within ctx, performs a graceful
// transport close, and joins I/O. EOF preserves received messages until Receive
// drains them, except those a budgeted link drops on failure (above). Close is
// an explicit abort/discard. Done signifies I/O completion; resource gauges
// reach zero through real release, not resets.
package wire
