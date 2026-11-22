import { SessionReply_Rejected_Code as Code } from "@circus/proto/circus/v1/worker_link_pb";
import type { ResumeOutcome, SessionOp, SessionReply, WorkerEvent } from "../link/index.js";
import { LinkLostError } from "../link/index.js";

// The part of WorkerLink the pool and the runners use. Tests replace it with
// an in-memory link.
export interface LinkPort {
  session(sessionId: string, epoch: bigint, op: SessionOp, signal?: AbortSignal): Promise<SessionReply>;
  setHeld(sessionId: string, epoch: bigint | null): void;
  onReady(listener: (resumed: ResumeOutcome[]) => void): () => void;
  hints(): AsyncIterable<WorkerEvent>;
}

type Result = Exclude<SessionReply["result"], { case: undefined }>;
export type ReplyValues = { [R in Result as R["case"]]: R["value"] };
type Expected = { [K in SessionOp["case"]]: keyof ReplyValues };

const expected = {
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
} as const satisfies Expected;

export type ReplyOf<C extends SessionOp["case"]> = ReplyValues[(typeof expected)[C]];

/** The lease is gone: Fenced on the current connection. */
export class FencedError extends Error {
  override name = "FencedError";
  constructor(readonly epoch: bigint) {
    super(`session lease fenced (current epoch ${epoch})`);
  }
}

/** STOPPED or TRANSCRIPT_FULL: the session is stopped; release with STOPPED. */
export class StoppedError extends Error {
  override name = "StoppedError";
}

/** BROKEN: RequestApproval and ConsumeGrant are refused until the repair. */
export class BrokenError extends Error {
  override name = "BrokenError";
}

/** Any other rejection; not retryable with the same content. */
export class RejectedError extends Error {
  override name = "RejectedError";
  constructor(readonly code: Code, message: string) {
    super(`${Code[code] ?? code}: ${message}`);
  }
}

/** The retry budget of one call ran out on transient failures. */
export class BudgetError extends Error {
  override name = "BudgetError";
}

export const ATTEMPT_TIMEOUT_MS = 30_000;
const MIN_BACKOFF_MS = 100;
const MAX_BACKOFF_MS = 2_000;

/** A counting semaphore bounding concurrent link operations. */
export class Semaphore {
  #free: number;
  #waiters: Array<() => void> = [];

  constructor(readonly size: number) {
    this.#free = size;
  }

  async acquire(signal: AbortSignal): Promise<() => void> {
    signal.throwIfAborted();
    if (this.#free > 0 && this.#waiters.length === 0) {
      this.#free--;
      return this.#releaser();
    }
    const { promise, resolve, reject } = Promise.withResolvers<void>();
    const wake = () => {
      signal.removeEventListener("abort", abort);
      resolve();
    };
    const abort = () => {
      const index = this.#waiters.indexOf(wake);
      if (index >= 0) this.#waiters.splice(index, 1);
      reject(signal.reason);
    };
    this.#waiters.push(wake);
    signal.addEventListener("abort", abort, { once: true });
    await promise;
    return this.#releaser();
  }

  #releaser(): () => void {
    let released = false;
    return () => {
      if (released) return;
      released = true;
      const next = this.#waiters.shift();
      if (next) next();
      else this.#free++;
    };
  }
}

/** Resolves when the link is next ready (after Attach and ResumeLeases). */
export type ReadyWaiter = (signal: AbortSignal) => Promise<void>;

export function sleep(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal.aborted) {
      reject(signal.reason);
      return;
    }
    const done = () => {
      signal.removeEventListener("abort", abort);
      resolve();
    };
    const abort = () => {
      clearTimeout(timer);
      reject(signal.reason);
    };
    const timer = setTimeout(done, ms);
    signal.addEventListener("abort", abort, { once: true });
  });
}

export interface CallOptions {
  /** Total time for transient failures; unlimited (until the lease ends) when absent. */
  budgetMs?: number;
}

/**
 * SessionOps sends one session's operations at one epoch. Transient failures
 * (a lost link, STORE_UNAVAILABLE, an attempt timeout or a full operation
 * table) are retried with the same operation, which carries its own
 * idempotency keys; replies map to typed errors otherwise. Every call ends
 * when the runner's signal aborts (the lease is gone or being dropped).
 */
export class SessionOps {
  constructor(
    readonly link: LinkPort,
    readonly sessionId: string,
    readonly epoch: bigint,
    readonly signal: AbortSignal,
    readonly ready: ReadyWaiter,
    readonly slots: Semaphore,
  ) {}

  async call<C extends SessionOp["case"]>(
    op: Extract<SessionOp, { case: C }>, options: CallOptions = {},
  ): Promise<ReplyOf<C>> {
    const deadline = options.budgetMs === undefined ? Infinity : performance.now() + options.budgetMs;
    let backoff = MIN_BACKOFF_MS;
    const transient = async (waitReady: boolean) => {
      const left = deadline - performance.now();
      if (left <= 0) throw new BudgetError(`session operation ${op.case} retry budget exhausted`);
      const bounded = AbortSignal.any([this.signal, AbortSignal.timeout(Math.ceil(Math.min(left, 2 ** 31 - 1)))]);
      try {
        if (waitReady) await this.ready(bounded);
        else await sleep(backoff, bounded);
      } catch (error) {
        this.signal.throwIfAborted();
        if (bounded.aborted) throw new BudgetError(`session operation ${op.case} retry budget exhausted`);
        throw error;
      }
      backoff = Math.min(backoff * 2, MAX_BACKOFF_MS);
    };
    for (;;) {
      this.signal.throwIfAborted();
      const left = deadline - performance.now();
      if (left <= 0) throw new BudgetError(`session operation ${op.case} retry budget exhausted`);
      // Waiting for an operation slot counts against the budget too.
      let release: () => void;
      const slotWait = Number.isFinite(left)
        ? AbortSignal.any([this.signal, AbortSignal.timeout(Math.ceil(Math.min(left, 2 ** 31 - 1)))]) : this.signal;
      try {
        release = await this.slots.acquire(slotWait);
      } catch (error) {
        this.signal.throwIfAborted();
        if (slotWait.aborted) throw new BudgetError(`session operation ${op.case} waited past its budget for a slot`);
        throw error;
      }
      const attempt = AbortSignal.any([this.signal, AbortSignal.timeout(Math.max(1, Math.ceil(Math.min(ATTEMPT_TIMEOUT_MS, deadline - performance.now()))))]);
      let reply: SessionReply;
      try {
        reply = await this.link.session(this.sessionId, this.epoch, op, attempt);
      } catch (error) {
        this.signal.throwIfAborted();
        if (error instanceof LinkLostError) {
          await transient(true);
          continue;
        }
        if (attempt.aborted || (error instanceof RangeError && /slots exhausted/.test(error.message))) {
          await transient(false);
          continue;
        }
        throw error;
      } finally {
        release();
      }
      const result = reply.result;
      switch (result.case) {
        case "fenced":
          throw new FencedError(result.value.epoch);
        case "rejected": {
          const { code, message } = result.value;
          if (code === Code.STORE_UNAVAILABLE) {
            await transient(false);
            continue;
          }
          if (code === Code.STOPPED || code === Code.TRANSCRIPT_FULL) throw new StoppedError(`${Code[code]}: ${message}`);
          if (code === Code.BROKEN) throw new BrokenError(message);
          throw new RejectedError(code, message);
        }
        case expected[op.case]:
          return result.value as ReplyOf<C>;
        default:
          throw new Error(`unexpected reply ${result.case} to ${op.case}`);
      }
    }
  }
}
