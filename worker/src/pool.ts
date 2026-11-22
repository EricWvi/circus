import { create } from "@bufbuild/protobuf";
import {
  AcquireLeaseSchema, ReleaseLeaseSchema, ReleaseLease_Reason, SessionReply_Rejected_Code as Code,
  type SessionReply_Lease,
} from "@circus/proto/circus/v1/worker_link_pb";
import { LinkLostError, type ResumeOutcome, type WorkerEvent } from "./link/index.js";
import type { Logger } from "./log.js";
import type { CliProcessesPort } from "./process.js";
import { Semaphore, sleep, type LinkPort } from "./runner/ops.js";
import { SessionRunner, type RunnerDeps, type RunnerEnd } from "./runner/runner.js";

/** What the pool needs of a runner (tests substitute their own). */
export interface Runner {
  readonly epoch: bigint;
  readonly ending: boolean;
  /** ReleaseLease is in flight. */
  readonly releasing: boolean;
  start(): Promise<RunnerEnd>;
  nudge(reasons: WorkerEvent_Nudge["reasons"], interruptTurnNo: bigint): void;
  stop(): void;
  lose(reason: string): void;
  shutdown(): void;
}

type WorkerEvent_Nudge = Extract<WorkerEvent["event"], { case: "nudge" }>["value"];

export type RunnerFactory = (lease: SessionReply_Lease, sessionId: string, deps: RunnerDeps) => Runner;

export interface PoolOptions {
  link: LinkPort;
  maxSessions: number;
  log: Logger;
  processes: CliProcessesPort;
  runner: Omit<RunnerDeps, "link" | "ready" | "slots" | "loadSlots" | "log" | "processes">;
  createRunner?: RunnerFactory;
}

/**
 * Session operations open at once across all sessions. The link has 63
 * slots beside Attach; this leaves room for acquisitions and releases.
 */
const SESSION_OP_SLOTS = 48;
const ACQUIRE_TIMEOUT_MS = 30_000;
const ACQUIRE_ATTEMPTS = 5;
/** An Offer stays reserved for its connection this long (docs/wire.md). */
const OFFER_MS = 30_000;
const MAX_WAITING_OFFERS = 256;
const FAILURE_BACKOFF_MS = 30_000;
const MAX_FAILURE_BACKOFF_MS = 10 * 60_000;
const MAX_BACKOFFS = 1024;
const READY_POLL_MS = 1_000;
/** How long shutdown still waits for releases in flight after its deadline. */
const RELEASE_WAIT_MS = 5_000;

/**
 * SessionPool turns link hints into held sessions: it acquires offered and
 * nudged sessions up to max_sessions, at most one AcquireLease per session at
 * a time (hints for a session being acquired are ignored), runs one
 * SessionRunner per lease, routes later hints to it and drops runners whose
 * lease did not survive a reconnect.
 */
export class SessionPool {
  readonly #opts: PoolOptions;
  readonly #log: Logger;
  readonly #slots = new Semaphore(SESSION_OP_SLOTS);
  readonly #loadSlots = new Semaphore(1);
  readonly #runners = new Map<string, Runner>();
  readonly #acquiring = new Set<string>();
  readonly #offers = new Map<string, number>();
  readonly #reoffered = new Set<string>();
  readonly #backoff = new Map<string, { until: number; delay: number }>();
  #readyWaiters = new Set<() => void>();
  #stopping = false;
  readonly #ended = new Set<Promise<unknown>>();

  constructor(opts: PoolOptions) {
    this.#opts = opts;
    this.#log = opts.log;
    opts.link.onReady((outcomes) => this.#ready(outcomes));
  }

  get held(): number {
    return this.#runners.size + this.#acquiring.size;
  }

  has(sessionId: string): boolean {
    return this.#runners.has(sessionId) || this.#acquiring.has(sessionId);
  }

  /** Consumes the link's hints until they end. */
  async run(): Promise<void> {
    for await (const event of this.#opts.link.hints()) this.hint(event);
  }

  /** Resolves at the next link readiness, or after a short poll interval. */
  waitReady = (signal: AbortSignal): Promise<void> => {
    const { promise, resolve } = Promise.withResolvers<void>();
    const wake = () => {
      this.#readyWaiters.delete(wake);
      resolve();
    };
    this.#readyWaiters.add(wake);
    return Promise.race([promise, sleep(READY_POLL_MS, signal)]).finally(() => this.#readyWaiters.delete(wake));
  };

  hint(message: WorkerEvent): void {
    const { event } = message;
    if (event.case === "leaseRevoked") {
      const runner = this.#runners.get(event.value.sessionId);
      if (runner && runner.epoch === event.value.epoch) runner.lose("lease revoked");
      return;
    }
    if (event.case !== "offer" && event.case !== "nudge") return;
    const id = event.value.sessionId;
    // An AcquireLease in flight: the session counts as held; ignore the hint.
    if (this.#acquiring.has(id)) return;
    const runner = this.#runners.get(id);
    if (runner) {
      if (runner.ending) this.#reoffered.add(id);
      else if (event.case === "nudge") runner.nudge(event.value.reasons, event.value.interruptTurnNo);
      return;
    }
    if (this.#stopping) return;
    const backoff = this.#backoff.get(id);
    if (backoff && backoff.until > performance.now()) return;
    if (this.held < this.#opts.maxSessions) {
      void this.#acquire(id);
    } else if (event.case === "offer") {
      this.#offers.delete(id);
      this.#offers.set(id, performance.now());
      if (this.#offers.size > MAX_WAITING_OFFERS) this.#offers.delete(this.#offers.keys().next().value!);
    }
  }

  /** Stops taking leases, releases every held one and waits, bounded in time. */
  async shutdown(timeoutMs: number): Promise<void> {
    this.#stopping = true;
    this.#offers.clear();
    for (const runner of this.#runners.values()) runner.shutdown();
    // #ended also holds acquisitions in flight: a lease they bring is released too.
    const all = () => Promise.allSettled([...this.#ended]);
    const timer = new AbortController();
    try {
      await Promise.race([all(), sleep(timeoutMs, timer.signal).catch(() => {})]);
      // Past the deadline, drop what is still running, but let a release in
      // flight finish: dropping it would leave its lease to expire.
      for (const runner of this.#runners.values()) if (!runner.releasing) runner.lose("shutdown deadline");
      await Promise.race([all(), sleep(RELEASE_WAIT_MS, timer.signal).catch(() => {})]);
    } finally {
      timer.abort();
    }
  }

  #ready(outcomes: ResumeOutcome[]): void {
    for (const outcome of outcomes) {
      const runner = this.#runners.get(outcome.sessionId);
      if (!runner) continue;
      // Any outcome that is not ok is a lost lease, whatever its epoch.
      if (!outcome.ok) runner.lose("lease not resumed");
      else if (outcome.stopped) runner.stop();
    }
    for (const wake of [...this.#readyWaiters]) wake();
  }

  #acquire(id: string): Promise<void> {
    const acquiring = this.#acquireRun(id);
    this.#ended.add(acquiring);
    void acquiring.finally(() => this.#ended.delete(acquiring));
    return acquiring;
  }

  async #acquireRun(id: string): Promise<void> {
    this.#acquiring.add(id);
    this.#offers.delete(id);
    let lease: SessionReply_Lease | undefined;
    try {
      lease = await this.#acquireLease(id);
    } catch (error) {
      this.#log.warn("session acquisition failed", { session_id: id, error: String(error).slice(0, 512) });
    } finally {
      this.#acquiring.delete(id);
    }
    if (!lease) {
      this.#admit();
      return;
    }
    const deps: RunnerDeps = {
      ...this.#opts.runner, link: this.#opts.link, ready: this.waitReady, slots: this.#slots, loadSlots: this.#loadSlots,
      log: this.#log, processes: this.#opts.processes,
    };
    const runner = (this.#opts.createRunner ?? ((l, s, d) => new SessionRunner(l, s, d)))(lease, id, deps);
    this.#runners.set(id, runner);
    this.#opts.link.setHeld(id, lease.epoch);
    this.#log.info("session lease acquired", { session_id: id, epoch: Number(lease.epoch), reattached: lease.reattached });
    const ended = runner.start().then(
      (end) => this.#end(id, runner, end),
      (error: unknown) => {
        this.#log.error("session runner crashed", { session_id: id, error: String(error).slice(0, 1024) });
        this.#end(id, runner, "lost");
      });
    this.#ended.add(ended);
    void ended.finally(() => this.#ended.delete(ended));
    if (this.#stopping) runner.shutdown();
  }

  /**
   * AcquireLease with fresh: the pool keeps no local state for a session it
   * does not hold, so every acquisition starts new epoch counters. A lost or
   * unwatched reply is retried the same way, which advances the epoch past
   * whatever the lost reply carried; giving up after an unwatched attempt
   * acquires once more and releases what that returns.
   */
  async #acquireLease(id: string): Promise<SessionReply_Lease | undefined> {
    const op = { case: "acquireLease" as const, value: create(AcquireLeaseSchema, { fresh: true }) };
    let unwatched = false;
    let backoff = 100;
    for (let attempt = 0; attempt < ACQUIRE_ATTEMPTS; attempt++) {
      if (this.#stopping && !unwatched) return undefined;
      const signal = AbortSignal.timeout(ACQUIRE_TIMEOUT_MS);
      let reply;
      try {
        reply = await this.#opts.link.session(id, 0n, op, signal);
      } catch (error) {
        if (error instanceof LinkLostError) {
          // The reply, if any, is lost; the retry after readiness is fresh.
          unwatched = true;
          await this.waitReady(AbortSignal.timeout(ACQUIRE_TIMEOUT_MS)).catch(() => {});
          continue;
        }
        if (signal.aborted) unwatched = true;
        else if (!(error instanceof RangeError)) throw error;
        await sleep(backoff, AbortSignal.timeout(MAX_FAILURE_BACKOFF_MS)).catch(() => {});
        backoff = Math.min(backoff * 2, 2_000);
        continue;
      }
      const result = reply.result;
      if (result.case === "lease") return result.value;
      if (result.case === "rejected") {
        const { code, message } = result.value;
        if (code === Code.STORE_UNAVAILABLE) {
          await sleep(backoff, AbortSignal.timeout(MAX_FAILURE_BACKOFF_MS)).catch(() => {});
          backoff = Math.min(backoff * 2, 2_000);
          continue;
        }
        if (code !== Code.NOT_OFFERED && code !== Code.STOPPED) {
          this.#log.warn("AcquireLease rejected", { session_id: id, code: Code[code] ?? code, error: message.slice(0, 512) });
        }
        return undefined;
      }
      throw new Error(`unexpected AcquireLease reply ${result.case}`);
    }
    if (unwatched) await this.#releaseUnknown(id);
    return undefined;
  }

  /** After giving up on unwatched acquisitions: acquire fresh once and release it. */
  async #releaseUnknown(id: string): Promise<void> {
    try {
      const signal = AbortSignal.timeout(ACQUIRE_TIMEOUT_MS);
      const reply = await this.#opts.link.session(id, 0n, { case: "acquireLease", value: create(AcquireLeaseSchema, { fresh: true }) }, signal);
      if (reply.result.case !== "lease") return;
      await this.#opts.link.session(id, reply.result.value.epoch, {
        case: "releaseLease", value: create(ReleaseLeaseSchema, { reason: ReleaseLease_Reason.ERROR }),
      }, AbortSignal.timeout(ACQUIRE_TIMEOUT_MS));
    } catch (error) {
      this.#log.warn("could not release an unconfirmed lease; it expires", { session_id: id, error: String(error).slice(0, 512) });
    }
  }

  #end(id: string, runner: Runner, end: RunnerEnd): void {
    if (this.#runners.get(id) !== runner) return;
    this.#runners.delete(id);
    this.#opts.link.setHeld(id, null);
    try {
      this.#opts.processes.cleanup(id);
    } catch (error) {
      this.#log.warn("CLI cleanup failed", { session_id: id, error: String(error).slice(0, 512) });
    }
    this.#log.info("session ended", { session_id: id, end });
    if (end === "failed") {
      // A session whose CLI keeps failing is not acquired again at once.
      const delay = Math.min((this.#backoff.get(id)?.delay ?? FAILURE_BACKOFF_MS / 2) * 2, MAX_FAILURE_BACKOFF_MS);
      this.#backoff.delete(id);
      this.#backoff.set(id, { until: performance.now() + delay, delay });
      if (this.#backoff.size > MAX_BACKOFFS) this.#backoff.delete(this.#backoff.keys().next().value!);
      this.#reoffered.delete(id);
    } else if (end === "released") {
      this.#backoff.delete(id);
    }
    if (this.#reoffered.delete(id) && !this.#stopping && this.held < this.#opts.maxSessions) {
      void this.#acquire(id);
    }
    this.#admit();
  }

  #admit(): void {
    if (this.#stopping) return;
    const now = performance.now();
    for (const [id, at] of this.#offers) {
      if (this.held >= this.#opts.maxSessions) return;
      this.#offers.delete(id);
      if (now - at > OFFER_MS || this.has(id)) continue;
      const backoff = this.#backoff.get(id);
      if (backoff && backoff.until > now) continue;
      void this.#acquire(id);
    }
  }
}
