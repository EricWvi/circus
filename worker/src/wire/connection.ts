import { create } from "@bufbuild/protobuf";
import type { Welcome } from "@circus/proto/circus/v1/link_pb";
import {
  WorkerDownlinkSchema, WorkerUplinkSchema, type WorkerDownlink, type WorkerUplink,
} from "@circus/proto/circus/v1/worker_link_pb";
import {
  decodeEnvelope, encodeUplink, FRAME_OVERHEAD, FRAGMENT_BYTES, MESSAGE_BYTES,
  METADATA_BYTES, ProtocolError, measureUplink,
} from "./codec.js";
import { fragmentFrame, Receiver, type Limits } from "./framing.js";
import { LinkSocket } from "./socket.js";

const HELLO_TIMEOUT_MS = 10_000;
const CLOSE_TIMEOUT_MS = 5_000;
const MAX_QUEUED_MESSAGES = 256;
export const MAX_UINT64 = (1n << 64n) - 1n;

/** The header the control plane sets on its own link authentication refusals (`internal/wire.LinkAuthHeader`). */
export const LINK_AUTH_HEADER = "circus-auth";
export const LINK_AUTH_REJECTED = "rejected";

/**
 * A link failure retrying cannot fix: the control plane refused the
 * credential (`refused`), or it does not accept this worker build
 * (`incompatible`).
 */
export class TerminalLinkError extends Error {
  override name = "TerminalLinkError";
  constructor(message: string, readonly reason: "refused" | "incompatible" = "refused") {
    super(message);
  }
}

interface WireOptions {
  endpoint: URL;
  token: string;
  ca?: Buffer;
  principalId: string;
  bootId: string;
  clientVersion: string;
}

export function welcomeLimits(welcome: Welcome, principalId: string): Limits {
  const message = welcome.maxMessageBytes || BigInt(MESSAGE_BYTES);
  const count = welcome.maxInFlightMessages || 2;
  if (welcome.negotiatedVersion !== 1 || welcome.principalId !== principalId ||
      message > BigInt(MESSAGE_BYTES) || count !== 1) {
    throw new TerminalLinkError("incompatible worker Welcome (version, principal, message limit or in-flight count)", "incompatible");
  }
  const messageBytes = Number(message);
  const fragmentBytes = welcome.maxFragmentBytes || FRAGMENT_BYTES;
  const inFlight = welcome.maxInFlightBytes || 2n * message;
  const intervalMs = welcome.heartbeatIntervalMs || 5000;
  const timeoutMs = welcome.heartbeatTimeoutMs || 15000;
  if (fragmentBytes > messageBytes || timeoutMs <= intervalMs || inFlight > (1n << 63n) - 1n) {
    throw new ProtocolError("invalid Welcome limits");
  }
  // With a single partial message, larger in-flight budgets are equivalent to M.
  const inFlightBytes = inFlight < message ? Number(inFlight) : messageBytes;
  return { messageBytes, fragmentBytes, inFlightBytes, intervalMs, timeoutMs };
}

// Node's native timeout range is smaller than uint32 milliseconds in Welcome.
function deadline(ms: number, fire: () => void): () => void {
  const end = performance.now() + ms;
  let timer: NodeJS.Timeout;
  const schedule = () => {
    const left = end - performance.now();
    timer = setTimeout(() => {
      if (performance.now() >= end) fire();
      else schedule();
    }, Math.min(left, 0x7fffffff));
  };
  schedule();
  return () => clearTimeout(timer);
}

interface Outbound {
  bytes: Uint8Array;
  id: bigint;
  index: number;
  written?: () => void;
}

export class WireConnection {
  #socket: LinkSocket;
  #dial = new AbortController();
  #ending = new AbortController();
  #ready = Promise.withResolvers<void>();
  #done = Promise.withResolvers<Error | undefined>();
  #limits?: Limits;
  #receiver?: Receiver;
  #error?: Error;
  #ended = false;
  #closing = false;
  #cancelHello: () => void;
  #cancelClose?: () => void;
  #cancelHeartbeat?: () => void;
  #cancelIdle?: () => void;
  #cancelFragment?: () => void;
  #queue: Outbound[] = [];
  #queuedBytes = 0;
  #writing = false;
  #heartbeat?: Outbound;
  #seq = 0n;
  #messageId = 0n;

  readonly ending: AbortSignal = this.#ending.signal;
  readonly ready = this.#ready.promise;
  readonly done = this.#done.promise;

  constructor(opts: WireOptions, onBusiness: (message: WorkerDownlink) => void) {
    const hello = encodeUplink(create(WorkerUplinkSchema, {
      payload: { case: "hello", value: {
        protocolVersion: 1, bootId: opts.bootId, clientVersion: opts.clientVersion,
      } },
    }), METADATA_BYTES);

    this.#socket = new LinkSocket(opts.endpoint, opts.token, opts.ca, METADATA_BYTES, this.#dial.signal);
    this.#cancelHello = deadline(HELLO_TIMEOUT_MS, () => this.fail(new Error("worker handshake timeout")));
    this.#socket.rejectClose = () => {
      if (!this.#receiver?.partial) return false;
      this.fail(new ProtocolError("closed with a partial message"));
      return true;
    };
    this.#socket.on("error", (error) => this.fail(error));
    this.#socket.once("close", (code) => this.#finish(code));
    this.#socket.on("unexpected-response", (_request, response) => {
      const status = response.statusCode;
      // Only the control plane's own refusal of the credential is terminal:
      // it marks it (LINK_AUTH_HEADER). A 401 or 403 from a proxy or WAF in
      // front of it is retried like any other failure.
      const refused = (status === 401 || status === 403) &&
        response.headers[LINK_AUTH_HEADER] === LINK_AUTH_REJECTED;
      this.fail(refused
        ? new TerminalLinkError(`worker upgrade HTTP ${status}: the control plane refused the credential`)
        : new Error(`worker upgrade HTTP ${status}`));
    });
    this.#socket.once("open", () => {
      if (this.#ended) return;
      this.#write(hello, () => this.#pump());
    });
    this.#socket.on("message", (frame, binary) => {
      if (this.#ended) return;
      try {
        if (!binary || !Buffer.isBuffer(frame)) throw new ProtocolError("expected binary WebSocket message");
        if (!this.#receiver) {
          const { payload } = decodeEnvelope(WorkerDownlinkSchema, frame, METADATA_BYTES);
          if (payload.case === "reject") {
            throw new TerminalLinkError(`worker Hello rejected (code ${payload.value.code})`);
          }
          if (payload.case !== "welcome") throw new ProtocolError("expected Welcome or Reject");
          const limits = welcomeLimits(payload.value, opts.principalId);
          this.#socket.setReadLimit(limits.fragmentBytes + FRAME_OVERHEAD);
          this.#limits = limits;
          this.#receiver = new Receiver(limits);
          this.#cancelHello();
          this.#scheduleHeartbeat();
          this.#recordActivity();
          this.#ready.resolve();
          return;
        }
        this.#recordActivity();
        const message = this.#receiver.receive(frame, () => this.#fragmentReceived());
        if (message && message.payload.case !== "heartbeat") onBusiness(message);
        if (this.#closing) this.#pump();
      } catch (error) {
        this.fail(error);
      }
    });
  }

  get active(): boolean {
    return this.#limits !== undefined && !this.#ended && !this.#closing &&
      this.#socket.readyState === LinkSocket.OPEN;
  }

  get messageBytes(): number {
    return this.#limits!.messageBytes;
  }

  measure(message: WorkerUplink): number {
    const { messageBytes, fragmentBytes, inFlightBytes } = this.#limits!;
    const size = measureUplink(message, messageBytes, fragmentBytes);
    if (size > fragmentBytes && size > inFlightBytes) {
      throw new ProtocolError("message exceeds in-flight byte limit");
    }
    return size;
  }

  send(message: WorkerUplink, measured?: number, written?: () => void): void {
    if (!this.active) throw new Error("worker connection is closed");
    const { messageBytes, fragmentBytes } = this.#limits!;
    const bytes = encodeUplink(message, measured ?? this.measure(message), fragmentBytes);
    if (this.#queue.length >= MAX_QUEUED_MESSAGES || this.#queuedBytes + bytes.length > 2 * messageBytes) {
      throw new RangeError("worker send queue full");
    }
    // All business frames use one FIFO: session ordering keys and the
    // Attach, ResumeLeases and Unwatch barriers hold even across fragments.
    this.#queue.push({ bytes, id: 0n, index: 0, written });
    this.#queuedBytes += bytes.length;
    this.#pump();
  }

  #write(bytes: Uint8Array, complete: () => void): void {
    this.#writing = true;
    try {
      this.#socket.send(bytes, { binary: true }, (error) => {
        this.#writing = false;
        if (error) this.fail(error);
        else if (!this.#ended) complete();
      });
    } catch (error) {
      this.fail(error);
    }
  }

  #pump(): void {
    if (this.#ended || this.#writing || !this.#limits || this.#socket.readyState !== LinkSocket.OPEN) return;
    const f = this.#limits.fragmentBytes;
    const head = this.#queue[0];
    const heartbeat = this.#heartbeat;
    const useHeartbeat = heartbeat !== undefined &&
      (heartbeat.id !== 0n || heartbeat.bytes.length <= f || !head?.id);
    const item = useHeartbeat ? heartbeat : head;
    if (!item) {
      if (this.#closing && !this.#receiver?.partial) this.#socket.close(1000);
      return;
    }
    let frame = item.bytes;
    let last = true;
    if (item.bytes.length > f) {
      if (item.id === 0n) {
        if (this.#messageId === MAX_UINT64) {
          this.fail(new ProtocolError("fragment message ID exhausted"));
          return;
        }
        item.id = ++this.#messageId;
      }
      frame = fragmentFrame(item.bytes, f, item.id, item.index++);
      last = item.index * f >= item.bytes.length;
    }
    this.#write(frame, () => {
      if (last) {
        if (useHeartbeat) this.#heartbeat = undefined;
        else {
          this.#queue.shift();
          this.#queuedBytes -= item.bytes.length;
          item.written?.();
        }
      }
      this.#pump();
    });
  }

  #scheduleHeartbeat(): void {
    const limits = this.#limits!;
    this.#cancelHeartbeat = deadline(limits.intervalMs, () => {
      if (this.#closing || this.#ended) return;
      try {
        if (!this.#heartbeat) {
          const bytes = encodeUplink(create(WorkerUplinkSchema, {
            payload: { case: "heartbeat", value: { seq: ++this.#seq, sentUnixMs: BigInt(Date.now()) } },
          }), limits.messageBytes, limits.fragmentBytes);
          if (bytes.length > limits.fragmentBytes && bytes.length > limits.inFlightBytes) {
            throw new ProtocolError("heartbeat exceeds in-flight byte limit");
          }
          this.#heartbeat = { bytes, id: 0n, index: 0 };
          this.#pump();
        }
        if (!this.#ended) this.#scheduleHeartbeat();
      } catch (error) {
        this.fail(error);
      }
    });
  }

  #recordActivity(): void {
    if (this.#closing) return;
    this.#cancelIdle?.();
    this.#cancelIdle = deadline(this.#limits!.timeoutMs, () => this.fail(new ProtocolError("heartbeat timeout")));
  }

  #fragmentReceived(): void {
    this.#cancelFragment?.();
    this.#cancelFragment = this.#receiver!.partial
      ? deadline(this.#limits!.timeoutMs, () => this.fail(new ProtocolError("fragment timeout")))
      : undefined;
  }

  close(): void {
    if (this.#closing || this.#ended) return;
    this.#closing = true;
    this.#ending.abort();
    if (!this.#limits) {
      this.fail(new Error("worker closed during handshake"));
      return;
    }
    this.#cancelHeartbeat?.();
    this.#cancelIdle?.();
    // A started fragmented heartbeat must be completed before close.
    if (!this.#heartbeat?.id) this.#heartbeat = undefined;
    this.#cancelClose = deadline(CLOSE_TIMEOUT_MS, () => this.fail(new Error("worker close timeout")));
    this.#pump();
  }

  fail(error: unknown): void {
    if (this.#ended) return;
    this.#ended = true;
    this.#error = error instanceof Error ? error : new Error(String(error));
    this.#ending.abort(this.#error);
    this.#dial.abort();
    this.#socket.terminate();
  }

  #finish(code: number): void {
    this.#ended = true;
    if (!this.#error) {
      if (this.#receiver?.partial) this.#error = new ProtocolError("closed with a partial message");
      else if (code !== 1000 && !(code === 1006 && this.#socket.cleanEOF)) this.#error = new Error(`worker WebSocket closed (${code})`);
    }
    this.#ending.abort(this.#error);
    this.#cancelHello();
    this.#cancelClose?.();
    this.#cancelHeartbeat?.();
    this.#cancelIdle?.();
    this.#cancelFragment?.();
    this.#receiver?.clear();
    this.#queue = [];
    this.#heartbeat = undefined;
    this.#ready.reject(this.#error ?? new Error("worker closed before Welcome"));
    this.#done.resolve(this.#error);
  }
}
