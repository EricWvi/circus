// The request admission budget B is min(negotiated M, 32 MiB). It is charged
// before allocation and covers serialized session/resume requests plus their
// possible Unwatch frames. At most 64 grants (including flushing cancellations)
// and 63 open or waiting one-shot operations coexist with the Attach stream.
// Waiters reference caller-owned messages. Attach adds at most 64 KiB.
//
// The largest admitted encode needs less than 9*B + 512 bytes of Buf scratch
// (up to 3*B + 10 requested capacity, a 128-byte initial allocation, and
// geometric growth with old and new buffers). Decode can use up to M of
// normalized bytes and 2*M of strings,
// plus at most 8192 nodes, 65536 fields and depth 32. Encode and decode run
// serially. Reassembly grows in 64 KiB blocks up to min(M, in-flight bytes);
// final flattening needs one additional logical-message buffer temporarily.
// Thus payload storage is bounded by B + 64 KiB + max(M + 9*B + 512, 4*M)
// plus O(F + 128) for frame I/O. At default limits this is under 385 MiB
// before frame I/O and object/allocator/GC overhead. Frame bookkeeping is
// capped at 65,536 TCP chunks, 64 WebSocket fragments and 16 pending pongs.
//
// State also includes 64 held leases, 256 coalesced hints, 64 listeners and
// one pending hint read. Request objects and delivered replies are owned by
// callers. A transcript reply can be M on the wire (up to 2*M of strings),
// so the runner needs its own budget for concurrent reads and writes.
import { create } from "@bufbuild/protobuf";
import {
  AttachSchema, GetSessionState_Section, ReportAgentsSchema, ResumeLeasesSchema, SessionReply_Rejected_Code, SessionRequestSchema,
  UnwatchSchema, WorkerUplinkSchema,
  type Attach, type SessionReply, type SessionReply_Resumed_Outcome, type SessionRequest,
  type WorkerEvent, type WorkerUplink,
} from "@circus/proto/circus/v1/worker_link_pb";
import { readBoundedFile } from "../config.js";
import type { AgentStatus } from "@circus/proto/circus/v1/agent_pb";
import type { Logger } from "../log.js";
import { METADATA_BYTES, ProtocolError, TRANSCRIPT_BATCH_BYTES, encodeUplink } from "../wire/codec.js";
import { MAX_UINT64, TerminalLinkError, WireConnection } from "../wire/connection.js";
import { workerEndpoint } from "../wire/socket.js";
import { SendAdmission, type SendPermit } from "./admission.js";
import { HintQueue, validSessionId } from "./hints.js";

export { TerminalLinkError };
export type { SessionReply, WorkerEvent };
export type SessionOp = Exclude<SessionRequest["op"], { case: undefined }>;
export type ResumeOutcome = SessionReply_Resumed_Outcome;

export interface LinkOptions {
  url: string;
  token: string;
  principalId: string;
  caFile?: string;
  bootId: string;
  clientVersion: string;
  attach: {
    maxSessions: number;
    sdkVersion: string;
    account?: { email: string; organization: string; subscriptionType: string };
    localOnly: boolean;
    /** The agents' states, read for each connection's Attach. */
    agents?: () => AgentStatus[];
  };
  log: Logger;
}

export class LinkLostError extends Error {
  override name = "LinkLostError";
}

const MAX_OPERATIONS = 64;
const FIRST_BACKOFF_MS = 500;
const MAX_BACKOFF_MS = 10_000;
const STABLE_LINK_MS = 60_000;
const ATTACH_TIMEOUT_MS = 10_000;
const MAX_SEND_BYTES = 32 << 20;
// Unwatch has an empty payload and up to ten request_id varint bytes.
const UNWATCH_BYTES = 16;
const MAX_CA_BYTES = 1 << 20;

const success = {
  acquireLease: "lease",
  releaseLease: "ok",
  loadTranscript: "transcriptPage",
  listTranscriptKeys: "transcriptKeys",
  appendTranscript: "ok",
  takeInputs: "inputs",
  reportTurn: "turn",
  patchState: "ok",
  getState: "state",
  publishEvents: "ok",
  requestApproval: "approval",
  consumeGrant: "grant",
  transcriptRepaired: "ok",
  getLaunchEnv: "launchEnv",
} as const satisfies Record<SessionOp["case"], SessionReply["result"]["case"]>;

interface Operation {
  result: SessionReply["result"]["case"];
  acquire?: string;
  resolve: (reply: SessionReply) => void;
  reject: (error: unknown) => void;
  removeAbort?: () => void;
  sent?: boolean;
  retired?: () => void;
}

class Connection {
  #nextId = 0n;
  #attachId = 0n;
  #ops = new Map<bigint, Operation>();
  #acquiring = new Set<string>();
  #admission?: SendAdmission;
  readonly wire: WireConnection;

  constructor(opts: LinkOptions, endpoint: URL, hint: (event: WorkerEvent) => void) {
    this.wire = new WireConnection({
      endpoint,
      token: opts.token,
      ca: opts.caFile ? readBoundedFile(opts.caFile, MAX_CA_BYTES) : undefined,
      principalId: opts.principalId,
      bootId: opts.bootId,
      clientVersion: opts.clientVersion,
    }, ({ requestId, payload }) => {
      switch (payload.case) {
        case "workerEvent":
          if (requestId !== 0n && requestId === this.#attachId) hint(payload.value);
          else if (this.#ops.get(requestId)?.sent) throw new ProtocolError("worker event for a one-shot operation");
          break;
        case "sessionReply": {
          const op = this.#ops.get(requestId);
          if (!op?.sent) {
            if (requestId !== 0n && requestId === this.#attachId) {
              throw new ProtocolError("session reply for Attach");
            }
            break;
          }
          const reply = payload.value;
          const kind = reply.result.case;
          if (kind !== op.result && kind !== "fenced" && kind !== "rejected") {
            throw new ProtocolError("unexpected session reply");
          }
          this.#finish(requestId, op);
          op.resolve(reply);
          break;
        }
        case "callUpdate":
          if (this.#ops.get(requestId)?.sent || (requestId !== 0n && requestId === this.#attachId)) {
            throw new ProtocolError("call update for a worker operation");
          }
          break;
        default:
          throw new ProtocolError("unexpected worker downlink after Welcome");
      }
    });
    const ended = () => this.#lost(this.wire.ending.reason);
    this.wire.ending.addEventListener("abort", ended, { once: true });
  }

  #id(): bigint {
    if (this.#nextId === MAX_UINT64) {
      const error = new LinkLostError("worker request ID exhausted");
      this.wire.fail(error);
      throw error;
    }
    return ++this.#nextId;
  }

  attach(value: Attach): void {
    this.#attachId = this.#id();
    this.wire.send(create(WorkerUplinkSchema, {
      requestId: this.#attachId, payload: { case: "attach", value },
    }));
  }

  request(
    payload: WorkerUplink["payload"], result: SessionReply["result"]["case"],
    signal?: AbortSignal, acquire?: string,
  ): Promise<SessionReply> {
    signal?.throwIfAborted();
    if (!this.wire.active) throw new LinkLostError("worker link is disconnected");
    if (this.#ops.size >= MAX_OPERATIONS - 1) throw new RangeError("worker operation slots exhausted (63 plus Attach)");
    if (acquire && this.#acquiring.has(acquire)) throw new Error("AcquireLease already in flight for session");
    const requestId = this.#id();
    const message = create(WorkerUplinkSchema, { requestId, payload });
    const bytes = this.wire.measure(message);
    this.#admission ??= new SendAdmission(Math.min(this.wire.messageBytes, MAX_SEND_BYTES), MAX_OPERATIONS);
    const admission = this.#admission.take(bytes, UNWATCH_BYTES, signal);
    const pending = Promise.withResolvers<SessionReply>();
    const op: Operation = { result, acquire, resolve: pending.resolve, reject: pending.reject };
    this.#ops.set(requestId, op);
    if (acquire) this.#acquiring.add(acquire);
    if (signal) {
      const abort = () => {
        if (!this.#ops.has(requestId)) return;
        const unwatch = op.sent === true && this.wire.active;
        if (unwatch) {
          try {
            this.wire.send(create(WorkerUplinkSchema, {
              requestId, payload: { case: "unwatch", value: create(UnwatchSchema) },
            }), undefined, op.retired);
          } catch (error) {
            this.wire.fail(error);
          }
        }
        this.#finish(requestId, op, unwatch);
        op.reject(signal.reason);
      };
      signal.addEventListener("abort", abort, { once: true });
      op.removeAbort = () => signal.removeEventListener("abort", abort);
    }
    const send = (permit: SendPermit) => {
      if (!this.#ops.has(requestId)) {
        permit.release();
        return;
      }
      let written = false;
      let retired = false;
      op.retired = () => {
        retired = true;
        if (written) permit.release();
      };
      try {
        this.wire.send(message, bytes, () => {
          written = true;
          if (retired) permit.release();
          else permit.written();
        });
        op.sent = true;
      } catch (error) {
        written = true;
        this.#finish(requestId, op);
        op.reject(error);
      }
    };
    void admission.then(send, (error: unknown) => {
      if (!this.#ops.has(requestId)) return;
      this.#finish(requestId, op);
      op.reject(error);
    });
    return pending.promise;
  }

  #finish(id: bigint, op: Operation, unwatch = false): void {
    this.#ops.delete(id);
    if (op.acquire) this.#acquiring.delete(op.acquire);
    op.removeAbort?.();
    if (!unwatch) op.retired?.();
  }

  #lost(cause?: unknown): void {
    const error = new LinkLostError("worker link closed before the reply", { cause });
    this.#admission?.close(error);
    for (const [id, op] of this.#ops) {
      this.#finish(id, op);
      op.reject(error);
    }
  }
}

export class WorkerLink {
  #opts: LinkOptions;
  #endpoint: URL;
  #held = new Map<string, bigint>();
  #hints = new HintQueue();
  #listeners = new Set<(outcomes: ResumeOutcome[]) => void>();
  #current?: Connection;
  #active?: Connection;
  #stop = new AbortController();
  #closed = Promise.withResolvers<void>();
  #started = false;

  readonly closed = this.#closed.promise;

  constructor(opts: LinkOptions) {
    if (!/^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/.test(opts.bootId)) {
      throw new Error("worker bootId must be a canonical UUIDv7");
    }
    if (!/^[A-Za-z0-9_-]{43}$/.test(opts.token) || !opts.principalId) {
      throw new Error("worker bearer token and principal ID are required");
    }
    if (opts.clientVersion.length > METADATA_BYTES || Buffer.byteLength(opts.clientVersion) > METADATA_BYTES - 128 ||
      !opts.clientVersion.isWellFormed()) {
      throw new Error("invalid worker client version");
    }
    const capacity = opts.attach.maxSessions;
    if (!Number.isInteger(capacity) || capacity < 1 || capacity > 64) {
      throw new RangeError("worker maxSessions must be an integer from 1 to 64");
    }
    this.#opts = { ...opts };
    this.#endpoint = workerEndpoint(opts.url);
    encodeUplink(create(WorkerUplinkSchema, {
      requestId: 1n, payload: { case: "attach", value: this.#attachMessage() },
    }), METADATA_BYTES);
    // Allow a caller to finish consuming hints before observing terminal failure.
    void this.closed.catch(() => {});
  }

  start(): void {
    if (this.#started || this.#stop.signal.aborted) return;
    this.#started = true;
    void this.#run().then(
      () => this.#finish(),
      (error: unknown) => this.#finish(error),
    );
  }

  hints(): AsyncIterable<WorkerEvent> {
    return this.#hints.iterate();
  }

  onReady(listener: (resumed: ResumeOutcome[]) => void): () => void {
    if (this.#listeners.size >= 64 && !this.#listeners.has(listener)) {
      throw new RangeError("too many worker readiness listeners");
    }
    if (!this.#stop.signal.aborted) this.#listeners.add(listener);
    return () => { this.#listeners.delete(listener); };
  }

  // Callers must pass a signal that bounds admission and reply time.
  // The caller owns per-epoch idempotency keys and retries. After aborting
  // AcquireLease, it must reacquire with fresh and release before giving up
  // the session: Unwatch discards the reply but does not undo the acquisition.
  async session(sessionId: string, epoch: bigint, op: SessionOp, signal?: AbortSignal): Promise<SessionReply> {
    signal?.throwIfAborted();
    const connection = this.#active;
    if (!connection?.wire.active) throw new LinkLostError("worker link is disconnected or resuming");
    const acquire = op.case === "acquireLease";
    if (!validSessionId(sessionId) || (acquire ? epoch !== 0n : epoch <= 0n || epoch > MAX_UINT64)) {
      throw new RangeError("invalid session ID or epoch");
    }
    if (op.case === "loadTranscript" && (!Number.isInteger(op.value.maxBytes) ||
      op.value.maxBytes < 1 || op.value.maxBytes > TRANSCRIPT_BATCH_BYTES)) {
      throw new RangeError("LoadTranscript.maxBytes must be 1..8 MiB");
    }
    if (op.case === "getState") {
      const sections = op.value.sections;
      if (sections.length > 5 || sections.length !== new Set(sections).size ||
        sections.some((section) => !Number.isInteger(section) || section < GetSessionState_Section.CWDS ||
          section > GetSessionState_Section.NONE || (section === GetSessionState_Section.NONE && sections.length !== 1))) {
        throw new RangeError("invalid GetSessionState sections");
      }
    }
    const value = create(SessionRequestSchema, { sessionId, epoch, op });
    return connection.request({ case: "session", value }, success[op.case], signal,
      acquire ? sessionId : undefined);
  }

  /**
   * ReportAgents on the ready connection: resolves when the control plane
   * accepted it. Rejects with LinkLostError when the link is not ready
   * (the next Attach carries the states instead).
   */
  async reportAgents(agents: AgentStatus[], signal?: AbortSignal): Promise<void> {
    signal?.throwIfAborted();
    const connection = this.#active;
    if (!connection?.wire.active) throw new LinkLostError("worker link is disconnected or resuming");
    const reply = await connection.request({ case: "reportAgents", value: create(ReportAgentsSchema, { agents }) }, "ok", signal);
    if (reply.result.case === "rejected") {
      const { code, message } = reply.result.value;
      throw new Error(`ReportAgents rejected (code ${SessionReply_Rejected_Code[code] ?? code}): ${message.slice(0, 512)}`);
    }
  }

  setHeld(sessionId: string, epoch: bigint | null): void {
    if (!validSessionId(sessionId) || (epoch !== null && (epoch <= 0n || epoch > MAX_UINT64))) {
      throw new RangeError("invalid held session ID or epoch");
    }
    if (this.#stop.signal.aborted) return;
    if (epoch === null) {
      this.#held.delete(sessionId);
      return;
    }
    if (this.#held.size >= 64 && !this.#held.has(sessionId)) throw new RangeError("too many held leases");
    this.#held.set(sessionId, epoch);
  }

  async close(): Promise<void> {
    this.#stop.abort();
    this.#current?.wire.close();
    if (!this.#started) this.#finish();
    await this.closed.catch(() => {});
  }

  #attachMessage(): Attach {
    const { agents, ...attach } = this.#opts.attach;
    return create(AttachSchema, { ...attach, agents: agents?.() ?? [] });
  }

  #finish(error?: unknown): void {
    this.#stop.abort();
    this.#hints.close();
    this.#listeners.clear();
    this.#held.clear();
    if (error !== undefined) this.#closed.reject(error);
    else this.#closed.resolve();
  }

  async #run(): Promise<void> {
    let delay = FIRST_BACKOFF_MS;
    while (!this.#stop.signal.aborted) {
      let connection: Connection | undefined;
      let failure: unknown;
      let readyAt: number | undefined;
      try {
        connection = new Connection(this.#opts, this.#endpoint, (event) => this.#hints.push(event));
        this.#current = connection;
        await connection.wire.ready;
        if (!this.#stop.signal.aborted && connection.wire.active) {
          connection.attach(this.#attachMessage());
          const resumed = await this.#resume(connection);
          if (connection.wire.active && !this.#stop.signal.aborted) {
            this.#active = connection;
            readyAt = performance.now();
            for (const outcome of resumed) {
              if (!outcome.ok) this.#held.delete(outcome.sessionId);
            }
            for (const listener of [...this.#listeners]) {
              if (this.#stop.signal.aborted) break;
              try {
                listener(resumed);
              } catch (error) {
                this.#opts.log.error("worker readiness listener failed", { error: String(error).slice(0, 1024) });
              }
            }
            if (!this.#stop.signal.aborted) this.#hints.ready();
          }
        }
        failure = await connection.wire.done;
      } catch (error) {
        failure = error;
        if (connection) {
          if (error instanceof LinkLostError) failure = await connection.wire.done;
          else {
            connection.wire.fail(error);
            await connection.wire.done;
          }
        }
      } finally {
        this.#active = undefined;
        this.#current = undefined;
        this.#hints.reset();
      }
      if (failure instanceof TerminalLinkError) throw failure;
      if (this.#stop.signal.aborted) return;
      if (readyAt !== undefined && performance.now() - readyAt >= STABLE_LINK_MS) delay = FIRST_BACKOFF_MS;
      const retryIn = Math.floor(delay / 2 + Math.random() * (delay / 2 + 1));
      this.#opts.log.warn("worker link disconnected", {
        error: failure === undefined ? "peer closed the link" : String(failure).slice(0, 1024),
        retry_in_ms: retryIn,
      });
      await this.#wait(retryIn);
      delay = Math.min(delay * 2, MAX_BACKOFF_MS);
    }
  }

  async #resume(connection: Connection): Promise<ResumeOutcome[]> {
    const timeout = new AbortController();
    const timer = setTimeout(() => timeout.abort(new Error("worker lease resumption timeout")), ATTACH_TIMEOUT_MS);
    try {
      return await this.#resumeHeld(connection, timeout.signal);
    } finally {
      clearTimeout(timer);
    }
  }

  async #resumeHeld(connection: Connection, signal: AbortSignal): Promise<ResumeOutcome[]> {
    // An empty ResumeLeases is also an acknowledgment that the server
    // processed Attach, since both messages are barriers handled inline.
    for (;;) {
      const leases = [...this.#held].map(([sessionId, epoch]) => ({ sessionId, epoch }));
      const reply = await connection.request({
        case: "resumeLeases", value: create(ResumeLeasesSchema, { leases }),
      }, "resumed", signal);
      if (reply.result.case === "rejected") {
        const { code } = reply.result.value;
        throw new Error(`worker ResumeLeases rejected (code ${SessionReply_Rejected_Code[code] ?? code})`);
      }
      if (reply.result.case !== "resumed" || reply.result.value.leases.length !== leases.length) {
        throw new ProtocolError("worker lease resumption failed");
      }
      const expected = new Map(leases.map(({ sessionId, epoch }) => [sessionId, epoch]));
      for (const outcome of reply.result.value.leases) {
        const { sessionId, epoch, ok } = outcome;
        const requested = expected.get(sessionId);
        if (requested === undefined || (ok && epoch !== requested)) {
          throw new ProtocolError("invalid lease resumption outcome");
        }
        expected.delete(sessionId);
      }
      if (this.#held.size === leases.length &&
        leases.every(({ sessionId, epoch }) => this.#held.get(sessionId) === epoch)) {
        return reply.result.value.leases;
      }
    }
  }

  #wait(ms: number): Promise<void> {
    const signal = this.#stop.signal;
    return new Promise((resolve) => {
      if (signal.aborted) {
        resolve();
        return;
      }
      const done = () => {
        clearTimeout(timer);
        signal.removeEventListener("abort", done);
        resolve();
      };
      const timer = setTimeout(done, ms);
      signal.addEventListener("abort", done, { once: true });
    });
  }
}
