import { create } from "@bufbuild/protobuf";
import type { SessionKey, SessionStore, SessionStoreEntry } from "@anthropic-ai/claude-agent-sdk";
import {
  AppendTranscriptSchema, ListTranscriptKeysSchema, LoadTranscriptSchema, TranscriptEntrySchema,
  type AppendTranscript,
} from "@circus/proto/circus/v1/worker_link_pb";
import { FencedError, RejectedError, StoppedError, type Semaphore, type SessionOps } from "./ops.js";

/** Per-call retry budget for transient append failures (docs: plan, SessionStore adapter). */
export const APPEND_BUDGET_MS = 20_000;
/**
 * Bounds of what is kept unacknowledged, in memory cost: twice the UTF-8
 * bytes, since V8 keeps a string with any non-Latin-1 character two bytes
 * per character. Per session, and for all sessions of the worker together.
 */
export const MAX_QUEUED_BYTES = 32 << 20;
export const WORKER_RETAINED_BYTES = 512 << 20;
/**
 * Wire batches stay small so each fits a slow uplink within its 20 s
 * budget (about 52 KiB/s); a single larger entry goes alone.
 */
export const BATCH_BYTES = 1 << 20;
const ENTRY_OVERHEAD = 16;
const PAGE_BYTES = 8 << 20;
const MAX_SUBPATHS = 1024;
/** At most this many unknown parents are named in one gap event. */
export const MAX_GAP_PARENTS = 64;

export interface StoreEvents {
  /** The lease is gone (Fenced). */
  fenced(error: FencedError): void;
  /** The session was stopped (TRANSCRIPT_FULL). */
  stopped(error: StoppedError): void;
  /** A loaded transcript has entries whose parentUuid is unknown. */
  gap(subpath: string, unknownParents: string[]): void;
  /** Every append handed to the store so far has settled. */
  settled?(): void;
}

interface Entry {
  json: string;
  uuid: string;
  /** Encoded size on the wire. */
  bytes: number;
  /** Memory cost counted against the bounds. */
  cost: number;
}

/** The worker-wide budget of retained transcript entries, shared by every session. */
export class RetainedBudget {
  #used = 0;

  constructor(readonly limit = WORKER_RETAINED_BYTES) {}

  get used(): number {
    return this.#used;
  }

  take(cost: number): boolean {
    if (this.#used + cost > this.limit) return false;
    this.#used += cost;
    return true;
  }

  give(cost: number): void {
    this.#used = Math.max(0, this.#used - cost);
  }
}

/** The budget of a worker process (one per process; the pool and runners share it). */
export const workerRetained = new RetainedBudget();

interface Subpath {
  /** The last acknowledged batch_seq of this epoch. */
  seq: bigint;
  /** Entries handed to append() and not acknowledged, in order. */
  queue: Entry[];
  /** The batch sent at least once: the first `count` entries of the queue, under a fixed batch_seq. */
  head?: { value: AppendTranscript; count: number };
  /** The entries of the latest call still (partly) queued, to recognize the SDK's retry of it. */
  lastCall?: string[];
  /** The entries of the latest call refused for good, so the SDK's retry of it fails again. */
  refused?: { jsons: string[]; error: Error };
}

const same = (a: string[], b: string[]) => a.length === b.length && a.every((value, i) => value === b[i]);

/** Thrown by repair() when entries are gone for good. */
export class TranscriptLostError extends Error {
  override name = "TranscriptLostError";
}

/**
 * TranscriptStore is the SDK SessionStore of one session, mirrored to the
 * control plane through LoadTranscript / AppendTranscript /
 * ListTranscriptKeys. It ignores projectKey and refuses another sessionId.
 *
 * Every entry handed to append() stays queued, per subpath and in order,
 * until the control plane acknowledges it, including entries of calls the
 * SDK gave up on (the ones behind a mirror_error); the next append, or
 * repair(), sends them first. A batch once sent keeps its batch_seq and
 * content until acknowledged, and the SDK's retry of a call is not queued
 * twice, so no entry is ever sent under two batch_seq values. Each call
 * retries link loss and STORE_UNAVAILABLE for at most 20 s. INVALID and
 * LIMIT drop the batch (its batch_seq stays unconsumed) and make the
 * transcript unrepairable in this epoch; FENCED and TRANSCRIPT_FULL also
 * tell the runner. The queue is bounded by the session's transcript quota.
 */
export class TranscriptStore implements SessionStore {
  #subpaths = new Map<string, Subpath>();
  #chain: Promise<unknown> = Promise.resolve();
  #queuedBytes = 0;
  /** append() calls not yet settled. */
  #active = 0;
  #disposed = false;
  /** Why some entries can no longer reach the control plane, if they cannot. */
  #lost?: string;

  constructor(
    readonly ops: SessionOps,
    readonly claudeSessionId: string,
    readonly events: StoreEvents,
    readonly loadSlots: Semaphore,
    readonly budgetMs = APPEND_BUDGET_MS,
    readonly maxQueuedBytes = MAX_QUEUED_BYTES,
    readonly retained: RetainedBudget = workerRetained,
  ) {}

  /**
   * The session is over: returns its retained entries to the worker budget.
   * Appends after this (the SDK flushes while it closes) are refused at once
   * and take nothing.
   */
  dispose(): void {
    this.#disposed = true;
    this.retained.give(this.#queuedBytes);
    this.#queuedBytes = 0;
    for (const state of this.#subpaths.values()) {
      state.queue = [];
      state.head = undefined;
    }
  }

  /** Memory cost (2 × UTF-8 bytes) of the entries not yet acknowledged. */
  get queuedBytes(): number {
    return this.#queuedBytes;
  }

  /** No append is in progress and every entry handed to the store is acknowledged. */
  get idle(): boolean {
    return this.#active === 0 && this.#queuedBytes === 0;
  }

  /**
   * Resolves once every append handed to the store so far has settled
   * (acknowledged, or failed after its retry budget), so entries the CLI
   * wrote while it exited reach the control plane before dispose().
   */
  drained(): Promise<void> {
    return this.#chain.then(() => {});
  }

  #check(key: { sessionId: string }): void {
    if (key.sessionId !== this.claudeSessionId) {
      throw new Error(`session store bound to ${this.claudeSessionId} got ${String(key.sessionId).slice(0, 128)}`);
    }
  }

  append(key: SessionKey, entries: SessionStoreEntry[]): Promise<void> {
    try {
      this.#check(key);
    } catch (error) {
      return Promise.reject(error);
    }
    const subpath = key.subpath ?? "";
    // Encode now: the SDK may reuse its objects after the call returns.
    const jsons = entries.map((entry) => JSON.stringify(entry));
    const uuids = entries.map((entry) => typeof entry.uuid === "string" ? entry.uuid : "");
    this.#active++;
    const run = this.#serialize(() => this.#append(subpath, jsons, uuids));
    void run.catch(() => {}).finally(() => {
      if (--this.#active === 0 && this.#queuedBytes === 0) this.events.settled?.();
    });
    return run;
  }

  #serialize(fn: () => Promise<void>): Promise<void> {
    const run = this.#chain.then(fn);
    this.#chain = run.catch(() => {});
    return run;
  }

  #state(subpath: string): Subpath {
    let state = this.#subpaths.get(subpath);
    if (!state) {
      if (this.#subpaths.size >= MAX_SUBPATHS) throw new Error("too many transcript subpaths");
      state = { seq: 0n, queue: [] };
      this.#subpaths.set(subpath, state);
    }
    return state;
  }

  async #append(subpath: string, jsons: string[], uuids: string[]): Promise<void> {
    if (this.#disposed) throw new Error("the session has ended; the transcript is no longer mirrored by this worker");
    const state = this.#state(subpath);
    if (state.refused && same(state.refused.jsons, jsons)) throw state.refused.error;
    state.refused = undefined;
    if (!state.lastCall || !same(state.lastCall, jsons)) {
      const entries = jsons.map((json, i) => {
        const bytes = Buffer.byteLength(json) + uuids[i].length + ENTRY_OVERHEAD;
        return { json, uuid: uuids[i], bytes, cost: 2 * bytes };
      });
      const cost = entries.reduce((sum, e) => sum + e.cost, 0);
      if (this.#queuedBytes + cost > this.maxQueuedBytes) {
        this.#lost ??= "more unacknowledged transcript than the session may hold";
        throw new Error(this.#lost);
      }
      if (!this.retained.take(cost)) {
        this.#lost ??= "more unacknowledged transcript than the worker may hold";
        throw new Error(this.#lost);
      }
      state.queue.push(...entries);
      this.#queuedBytes += cost;
      state.lastCall = jsons;
    }
    // Each batch has its own budget, so a slow link drains a backlog batch by batch.
    await this.#drain(subpath, state, performance.now() + this.budgetMs, this.budgetMs);
    state.lastCall = undefined;
  }

  /** Sends the queue; with perBatchMs each batch gets its own budget, else all share the deadline. */
  async #drain(subpath: string, state: Subpath, deadline: number, perBatchMs?: number): Promise<void> {
    while (state.queue.length) {
      if (perBatchMs !== undefined) deadline = performance.now() + perBatchMs;
      if (!state.head) {
        let count = 0;
        let bytes = 0;
        while (count < state.queue.length) {
          const size = state.queue[count].bytes;
          if (count > 0 && bytes + size > BATCH_BYTES) break;
          bytes += size;
          count++;
        }
        state.head = {
          count,
          value: create(AppendTranscriptSchema, {
            subpath, batchSeq: state.seq + 1n,
            entries: state.queue.slice(0, count).map((e) => create(TranscriptEntrySchema, { uuid: e.uuid, json: e.json })),
          }),
        };
      }
      const head = state.head;
      try {
        await this.ops.call({ case: "appendTranscript", value: head.value }, { budgetMs: Math.max(0, deadline - performance.now()) });
      } catch (error) {
        if (error instanceof RejectedError || error instanceof RangeError) {
          // Not retryable: drop the batch; its batch_seq goes to the next content.
          this.#forget(state, head.count);
          this.#lost ??= `a transcript batch was refused: ${error.message.slice(0, 512)}`;
          if (state.lastCall) state.refused = { jsons: state.lastCall, error };
          state.lastCall = undefined;
        } else if (error instanceof FencedError) {
          this.events.fenced(error);
        } else if (error instanceof StoppedError) {
          this.events.stopped(error);
        }
        throw error;
      }
      state.seq = head.value.batchSeq;
      this.#forget(state, head.count);
    }
  }

  #forget(state: Subpath, count: number): void {
    for (const entry of state.queue.splice(0, count)) {
      // dispose() has returned (and stopped counting) everything.
      if (this.#disposed) continue;
      this.#queuedBytes -= entry.cost;
      this.retained.give(entry.cost);
    }
    state.head = undefined;
  }

  /**
   * The repair after mirror_error: sends every entry still unacknowledged,
   * in order and with its original content. Throws TranscriptLostError when
   * some entries can never be stored (a refused batch, or the queue bound).
   */
  repair(): Promise<void> {
    return this.#serialize(async () => {
      for (const [subpath, state] of this.#subpaths) {
        await this.#drain(subpath, state, performance.now() + this.budgetMs, this.budgetMs);
        state.lastCall = undefined;
      }
      if (this.#lost) throw new TranscriptLostError(this.#lost);
    });
  }

  async load(key: SessionKey): Promise<SessionStoreEntry[] | null> {
    this.#check(key);
    const subpath = key.subpath ?? "";
    const release = await this.loadSlots.acquire(this.ops.signal);
    try {
      const entries: SessionStoreEntry[] = [];
      const ids = new Set<string>();
      const parents = new Set<string>();
      let afterSeq = 0n;
      for (;;) {
        const page = await this.ops.call({
          case: "loadTranscript", value: create(LoadTranscriptSchema, { subpath, afterSeq, maxBytes: PAGE_BYTES }),
        });
        for (const entry of page.entries) {
          const value = JSON.parse(entry.json) as SessionStoreEntry;
          entries.push(value);
          if (entry.uuid) ids.add(entry.uuid);
          if (typeof value.uuid === "string" && value.uuid) ids.add(value.uuid);
          const parent = (value as { parentUuid?: unknown }).parentUuid;
          if (typeof parent === "string" && parent) parents.add(parent);
        }
        if (page.done) break;
        if (page.nextSeq <= afterSeq) throw new Error(`transcript ${JSON.stringify(subpath)} did not advance`);
        afterSeq = page.nextSeq;
      }
      const unknown = [...parents].filter((parent) => !ids.has(parent));
      if (unknown.length) this.events.gap(subpath, unknown.slice(0, MAX_GAP_PARENTS));
      return entries.length ? entries : null;
    } finally {
      release();
    }
  }

  async listSubkeys(key: { projectKey: string; sessionId: string }): Promise<string[]> {
    this.#check(key);
    const reply = await this.ops.call({ case: "listTranscriptKeys", value: create(ListTranscriptKeysSchema) });
    return reply.subpaths.filter((subpath) => subpath !== "");
  }
}
