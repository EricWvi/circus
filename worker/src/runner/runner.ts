import { randomUUID } from "node:crypto";
import { statSync } from "node:fs";
import { isMainThread } from "node:worker_threads";
import { create } from "@bufbuild/protobuf";
import {
  type HookCallback, type HookJSONOutput, type Options, type PermissionMode, type Query, type SDKMessage, type SDKUserMessage,
} from "@anthropic-ai/claude-agent-sdk";
import { InputPriority, TurnOrigin, TurnOutcome, type SessionConfig } from "@circus/proto/circus/v1/session_pb";
import {
  ConsumeGrantSchema, GetLaunchEnvSchema, GetSessionStateSchema, GetSessionState_Section, PatchSessionStateSchema,
  PatchSessionState_BackgroundTaskSchema, ReleaseLeaseSchema, ReleaseLease_Reason, ReportTurnSchema,
  ReportTurn_PersistedSchema, ReportTurn_Phase, RequestApprovalSchema, SessionReply_Inputs_Input_Kind as InputKind,
  SessionReply_Rejected_Code as RejectCode,
  TakeInputsSchema, TranscriptRepairedSchema, WorkerEvent_Nudge_Reason as Reason,
  type PatchSessionState_BackgroundTask, type ReportTurn_Persisted, type SessionReply_Inputs_Input, type SessionReply_Lease,
} from "@circus/proto/circus/v1/worker_link_pb";
import type { Logger } from "../log.js";
import type { CliProcessesPort } from "../process.js";
import {
  ToolUses, approvalReason, approvalSubject, clipUtf8, toolUsesOf, type ApprovalSubject,
} from "./approvals.js";
import {
  EventPublisher, Kind, approvalPendingPayload, errorPayload, gapDetectedPayload, lostTaskPayload, sdkEvent,
  turnStartedPayload, userMessagePayload, type NoteKind, type TurnOriginName,
} from "./events.js";
import {
  historyMissingText, lostTaskSummary, lostTaskText, previousTurnInterruptedText, systemReminder, undeliveredTaskText,
} from "./notes.js";
import {
  BrokenError, FencedError, RejectedError, SessionOps, StoppedError, type LinkPort, type ReadyWaiter, type Semaphore,
} from "./ops.js";
import { PROVIDER_DEFAULT_MODEL, launchPlan } from "./launch.js";
import { ledgerPrompt } from "./ledger.js";
import { PushQueue } from "./push.js";
import { RetainedBudget, TranscriptLostError, TranscriptStore } from "./store.js";

export type QueryFactory = (params: { prompt: AsyncIterable<SDKUserMessage>; options: Options }) => Query;

export interface RunnerDeps {
  link: LinkPort;
  ready: ReadyWaiter;
  /** Shared by every session: bounds open link operations. */
  slots: Semaphore;
  /** Shared by every session: bounds concurrent transcript loads (memory). */
  loadSlots: Semaphore;
  log: Logger;
  query: QueryFactory;
  processes: CliProcessesPort;
  /** Idle time before an idle release; five minutes. */
  idleMs?: number;
  /** Longest wait for running background tasks; two hours. */
  taskHoldMs?: number;
  /** How long a worker-initiated interrupt may take before the query is closed. */
  abortGraceMs?: number;
  /** How long a shutdown waits for the running turn to end; ten seconds. */
  shutdownGraceMs?: number;
  /** Retry budget of one SessionStore append; 20 seconds. */
  appendBudgetMs?: number;
  /** Retry budget of ConsumeGrant in the hook; 20 seconds. */
  grantBudgetMs?: number;
  /** The worker-wide budget of retained transcript entries; the process's by default. */
  retained?: RetainedBudget;
  /** The worker's environment, the base of a provider's CLI environment; process.env by default. */
  env?: NodeJS.ProcessEnv;
}

/** released: the lease was released; failed: released with ERROR; lost: dropped without a release. */
export type RunnerEnd = "released" | "failed" | "lost";

const IDLE_MS = 5 * 60_000;
const TASK_HOLD_MS = 2 * 60 * 60_000;
const ABORT_GRACE_MS = 10_000;
const SHUTDOWN_GRACE_MS = 10_000;
const REPAIR_ATTEMPTS = 3;
/** A foreground task's task_started is kept (to publish if it is backgrounded) up to this size. */
const MAX_HELD_TASK_EVENT = 64 << 10;
const GRANT_BUDGET_MS = 20_000;
const MAX_TRACKED = 256;
const USAGE_BYTES = 4096;
const permissionModes = new Set<PermissionMode>(["default", "acceptEdits", "bypassPermissions", "plan", "dontAsk", "auto"]);

class LeaseLost extends Error {
  override name = "LeaseLost";
}

interface Note {
  kind: NoteKind;
  text: string;
  gap?: bigint;
  task?: PatchSessionState_BackgroundTask;
}

/** An input pushed to the SDK, until a turn takes it. */
interface Delivery {
  inputId: string;
  notes: Note[];
  events: Array<{ kind: Kind; payload: object }>;
  /** The number of turns started when it was pushed. */
  pushedAtSeq: bigint;
  /** A turn started after it was pushed ended without naming it: it no longer keeps the session busy. */
  unnamed?: boolean;
  /** Two turns have started since it was pushed without naming it: logged. */
  warned?: boolean;
}

class Turn {
  turnNo = 0n;
  readonly started: Promise<bigint>;
  resolveStarted!: (turnNo: bigint) => void;
  rejectStarted!: (error: unknown) => void;
  inputIds: string[] = [];
  gapNotice = 0n;
  lostTasks: string[] = [];
  /** A user Interrupt or NOW input was applied to this turn. */
  userAbort = false;
  interruptSent = false;
  /** The worker interrupted this turn itself (mirror_error): not the user's stop. */
  workerAbort = false;
  /** The model call failed authentication. */
  authFailed = false;
  /** mirror_error: the transcript of this turn is incomplete. */
  incomplete = false;

  constructor(readonly localSeq: bigint, readonly origin: TurnOriginName) {
    this.started = new Promise((resolve, reject) => {
      this.resolveStarted = resolve;
      this.rejectStarted = reject;
    });
    this.started.catch(() => {});
  }
}

interface Generation {
  query: Query;
  prompt: PushQueue;
  closing: boolean;
  done?: Promise<void>;
}

interface Task {
  startedAt: number;
  toolUseId: string;
  description: string;
  kind: string;
  backgrounded: boolean;
  patched: boolean;
  /** BGTASK_STARTED was published (the view shows background tasks only). */
  published: boolean;
  /** A foreground task's task_started, published if it moves to the background. */
  held?: string;
}

const origins: Record<TurnOriginName, TurnOrigin> = {
  input: TurnOrigin.INPUT, notification: TurnOrigin.NOTIFICATION, cli: TurnOrigin.CLI,
};

function isLeaseAbort(error: unknown): boolean {
  return error instanceof LeaseLost || (error instanceof Error && error.name === "AbortError");
}

/**
 * SessionRunner drives one held session in local mode: one Claude Agent SDK
 * query with Claude Code's own tools in the session's working directory,
 * its transcript mirrored to the control plane, its messages published as
 * view events, its turns reported, inputs pushed, approvals requested and
 * consumed, and the lease released or dropped at the end (docs/wire.md,
 * "Session processing contract").
 */
export class SessionRunner {
  readonly sessionId: string;
  readonly epoch: bigint;
  readonly ops: SessionOps;
  readonly #abort = new AbortController();
  readonly #log: Logger;
  readonly #publisher: EventPublisher;
  readonly #store: TranscriptStore;
  readonly #claudeSessionId: string;
  readonly #idleMs: number;
  readonly #taskHoldMs: number;
  readonly #abortGraceMs: number;
  readonly #shutdownGraceMs: number;
  readonly #config?: SessionConfig;
  /** The device approvals and grants name: the session's default device, where its tools run. */
  readonly #device: string;
  readonly #gapAtLease: bigint;
  /** The session's ledger snapshot, empty when none; fixed for the lease. */
  readonly #ledgerPath: string;
  /** The Lease's state and recovery, until #begin has used them. */
  #initial?: Pick<SessionReply_Lease, "state" | "recovery">;

  #cwd = "";
  #gen?: Generation;
  /** The closed query's CLI is confirming its exit: no new CLI starts before. */
  #exited?: Promise<void>;
  #hasTranscript: boolean;
  #turn?: Turn;
  #localSeq = 0n;
  #interruptLevel = 0n;
  #gapMark = 0n;
  #pushed = new Set<string>();
  #deliveries = new Map<string, Delivery>();
  #nextTurn: Delivery[] = [];
  #notes: Note[] = [];
  #broken = false;
  #repair = false;
  #brokenItems?: ReportTurn_Persisted;
  #brokenLostTasks: string[] = [];
  #tasks = new Map<string, Task>();
  /** Background tasks that ended between turns: the CLI has yet to start the turn telling the model. */
  #undelivered = new Map<string, { toolUseId: string; status: string; summary: string }>();
  #stopping = false;
  /** The CLI ended on its own: the session ends with ERROR and no other CLI starts. */
  #cliFailed = false;
  /** The query's login failed: replace the query when nothing of it is still needed. */
  #closeAfterAuth = false;
  #releasing = false;
  #toolUses = new ToolUses();
  #handledDenials = new Set<string>();
  #grants = new Map<string, true>();
  #inputsBusy = false;
  #inputsAgain = false;
  #idleTimer?: ReturnType<typeof setTimeout>;
  #ending?: Promise<void>;
  #end = Promise.withResolvers<RunnerEnd>();

  /** Resolves when the runner has released or dropped the lease. */
  readonly done = this.#end.promise;

  constructor(lease: SessionReply_Lease, sessionId: string, readonly deps: RunnerDeps) {
    this.sessionId = sessionId;
    this.#config = lease.config;
    this.#device = lease.config?.defaultDevice ?? "";
    this.#gapAtLease = lease.transcriptGapTurnNo;
    this.#ledgerPath = lease.ledgerPath;
    this.#initial = { state: lease.state, recovery: lease.recovery };
    this.#shutdownGraceMs = deps.shutdownGraceMs ?? SHUTDOWN_GRACE_MS;
    this.epoch = lease.epoch;
    this.#log = deps.log;
    this.#claudeSessionId = lease.claudeSessionId;
    this.#hasTranscript = lease.hasTranscript;
    this.#idleMs = deps.idleMs ?? IDLE_MS;
    this.#taskHoldMs = deps.taskHoldMs ?? TASK_HOLD_MS;
    this.#abortGraceMs = deps.abortGraceMs ?? ABORT_GRACE_MS;
    this.ops = new SessionOps(deps.link, sessionId, lease.epoch, this.#abort.signal, deps.ready, deps.slots);
    this.#publisher = new EventPublisher(this.ops, deps.log, (error) => this.#fatal(error));
    this.#store = new TranscriptStore(this.ops, lease.claudeSessionId, {
      fenced: () => this.lose("fenced on append"),
      stopped: () => this.stop(),
      gap: (subpath, unknown) => {
        if (this.#gapAtLease !== 0n) this.#publisher.add(Kind.TRANSCRIPT_GAP_DETECTED, gapDetectedPayload(subpath, unknown));
      },
    }, deps.loadSlots, deps.appendBudgetMs, undefined, deps.retained);
  }

  /** Starts serving the lease; resolves like done. */
  start(): Promise<RunnerEnd> {
    void this.#begin().catch((error: unknown) => this.#fatal(error));
    return this.done;
  }

  get ending(): boolean {
    return this.#ending !== undefined;
  }

  /** ReleaseLease has been sent: the lease is being given back. */
  get releasing(): boolean {
    return this.#releasing;
  }

  /** A Nudge for this session. */
  nudge(reasons: readonly Reason[], interruptTurnNo: bigint): void {
    if (this.#ending) return;
    if (interruptTurnNo > this.#interruptLevel) this.#interruptLevel = interruptTurnNo;
    if (reasons.includes(Reason.STOP)) {
      this.stop();
      return;
    }
    void this.#checkInterrupt();
    if (reasons.includes(Reason.INPUT) || reasons.includes(Reason.APPROVAL)) this.#kickInputs();
  }

  /** Stopped: close the query and release with STOPPED. */
  stop(): void {
    this.#finish({ release: ReleaseLease_Reason.STOPPED });
  }

  /** The lease is gone (Fenced, LeaseRevoked, resume not ok): drop everything. */
  lose(reason: string): void {
    if (!this.#abort.signal.aborted) this.#log.warn("session lease lost", { session_id: this.sessionId, reason });
    this.#finish({ lost: true });
  }

  /**
   * Worker shutdown: take no more input, let the running turn end on its
   * own (its ENDED keeps its real outcome and marks) for a bounded time,
   * then close the query and release. A turn still running then is closed
   * by the release as INTERRUPTED, which is what the contract prescribes
   * for a shutdown: interrupting it from the worker would not be a user
   * stop and could only be reported ERROR.
   */
  shutdown(): void {
    if (this.#stopping) return;
    this.#stopping = true;
    if (!this.#turn) {
      this.#finish({ release: ReleaseLease_Reason.IDLE });
      return;
    }
    setTimeout(() => this.#finish({ release: ReleaseLease_Reason.IDLE }), this.#shutdownGraceMs).unref?.();
  }

  async #begin(): Promise<void> {
    const config = this.#config;
    const { state, recovery } = this.#initial ?? {};
    this.#initial = undefined;
    if (!config?.local) {
      this.#log.error("lease of a session that is not local; releasing it", { session_id: this.sessionId });
      this.#publisher.add(Kind.ERROR, errorPayload("This worker runs only local sessions, and this session is not local."));
      this.#finish({ release: ReleaseLease_Reason.ERROR });
      return;
    }
    const cwd = config.cwds.find((entry) => entry.device === config.defaultDevice)?.cwd ?? "";
    let isDirectory = false;
    try {
      isDirectory = cwd !== "" && statSync(cwd).isDirectory();
    } catch {
      // Missing.
    }
    if (!isDirectory) {
      this.#log.error("session working directory is missing", { session_id: this.sessionId, cwd: cwd.slice(0, 1024) });
      this.#publisher.add(Kind.ERROR, errorPayload(`The session's working directory ${JSON.stringify(cwd)} is not a directory on this machine.`));
      this.#finish({ release: ReleaseLease_Reason.ERROR });
      return;
    }
    const mode = config.permissionMode || "auto";
    if (!permissionModes.has(mode as PermissionMode)) {
      this.#publisher.add(Kind.ERROR, errorPayload(`Unknown permission mode ${JSON.stringify(mode)}.`));
      this.#finish({ release: ReleaseLease_Reason.ERROR });
      return;
    }
    this.#cwd = cwd;
    if (state?.stopped) {
      this.stop();
      return;
    }
    this.#interruptLevel = state?.interruptTurnNo ?? 0n;
    for (const grant of state?.grants ?? []) {
      if (grant.device === this.#device) this.#rememberGrant(grant.tool, grant.matchKey);
    }
    if (recovery?.calls.length) {
      this.#log.error("recovery lists device calls for a local session; ignoring them", {
        session_id: this.sessionId, calls: recovery.calls.length,
      });
    }
    this.#gapMark = this.#gapAtLease;
    if (this.#gapMark) this.#notes.push({ kind: "history_missing", text: historyMissingText, gap: this.#gapMark });
    if (recovery?.previousTurnInterrupted) this.#notes.push({ kind: "previous_turn_interrupted", text: previousTurnInterruptedText });
    for (const task of recovery?.lostBgtasks ?? []) {
      this.#notes.push({ kind: "lost_bgtask", text: lostTaskText(task.taskId, task.toolUseId, task.description), task });
    }
    this.#kickInputs();
  }

  // ---- Errors and endings ----

  #fatal(error: unknown): void {
    if (this.#abort.signal.aborted && isLeaseAbort(error)) return;
    if (error instanceof FencedError) this.lose(error.message);
    else if (error instanceof StoppedError) this.stop();
    else if (isLeaseAbort(error)) return;
    else {
      this.#log.error("session runner failed", { session_id: this.sessionId, error: String(error).slice(0, 1024) });
      this.#publisher.add(Kind.ERROR, errorPayload(`The worker failed: ${String(error).slice(0, 1024)}`));
      this.#finish({ release: ReleaseLease_Reason.ERROR });
    }
  }

  #finish(how: { lost: true } | { release: ReleaseLease_Reason }): void {
    if ("lost" in how) this.#abort.abort(new LeaseLost("session lease lost"));
    if (this.#ending) return;
    this.#ending = this.#close(how).then(
      (end) => this.#end.resolve(end),
      (error: unknown) => {
        this.#log.error("session ending failed", { session_id: this.sessionId, error: String(error).slice(0, 1024) });
        this.#end.resolve("lost");
      });
  }

  async #close(how: { lost: true } | { release: ReleaseLease_Reason }): Promise<RunnerEnd> {
    this.#clearIdle();
    await this.#closeQuery(true);
    // What is still unacknowledged can no longer be repaired by this worker.
    this.#store.dispose();
    if ("lost" in how || this.#abort.signal.aborted) {
      this.#publisher.close();
      return "lost";
    }
    try {
      await this.#publisher.flush();
    } catch (error) {
      if (!isLeaseAbort(error)) this.#log.warn("session events not flushed before release", { session_id: this.sessionId, error: String(error).slice(0, 512) });
    }
    this.#publisher.close();
    this.#releasing = true;
    try {
      await this.ops.call({ case: "releaseLease", value: create(ReleaseLeaseSchema, { reason: how.release }) });
      this.#log.info("session lease released", { session_id: this.sessionId, reason: ReleaseLease_Reason[how.release] });
    } catch (error) {
      if (error instanceof FencedError) return "lost";
      if (isLeaseAbort(error)) return "lost";
      throw error;
    } finally {
      this.#abort.abort(new LeaseLost("session released"));
    }
    return how.release === ReleaseLease_Reason.ERROR ? "failed" : "released";
  }

  // ---- The query ----

  /**
   * The open query, or a new one. Every CLI start asks GetLaunchEnv first
   * (never reused across starts, so provider edits reach the next CLI).
   * Undefined when the session is ending (PROVIDER_UNAVAILABLE included:
   * then no CLI starts and the lease is released with PROVIDER).
   */
  async #ensureQuery(): Promise<Generation | undefined> {
    if (this.#gen) return this.#gen;
    if (!this.#mayStart()) return undefined;
    // The exec guard's parent-death signal follows the spawning thread.
    if (!isMainThread) throw new Error("query() must run on the main thread");
    // A query closed by another path (a replaced login) may still be waiting for its CLI.
    await this.#exited;
    if (!this.#mayStart()) return undefined;
    let reply;
    try {
      reply = await this.ops.call({ case: "getLaunchEnv", value: create(GetLaunchEnvSchema) });
    } catch (error) {
      if (!(error instanceof RejectedError) || error.code !== RejectCode.PROVIDER_UNAVAILABLE) throw error;
      // The diagnostic comes from the control plane and holds no secret.
      this.#log.warn("the session's provider is unavailable to this worker; releasing", {
        session_id: this.sessionId, reason: error.message.slice(0, 512),
      });
      this.#finish({ release: ReleaseLease_Reason.PROVIDER });
      return undefined;
    }
    // Stopped, lost or failed while asking: no CLI.
    if (!this.#mayStart()) return undefined;
    // The reply holds secrets: only the provider id and counts are logged.
    const plan = launchPlan(reply, this.deps.env ?? process.env);
    if (plan.skipped.length) {
      this.#log.warn("launch environment variables not applied", {
        session_id: this.sessionId, provider_id: plan.providerId.slice(0, 128), names: plan.skipped.slice(0, 16).join(","),
      });
    }
    this.#log.info("starting Claude Code", plan.env
      ? { session_id: this.sessionId, provider_id: plan.providerId.slice(0, 128), variables: reply.env.length - plan.skipped.length }
      : { session_id: this.sessionId, local_login: true });
    const prompt = new PushQueue();
    const mode = (this.#config?.permissionMode || "auto") as PermissionMode;
    const hook: HookCallback = (input, _toolUseId, { signal }) => this.#preToolUse(input, signal);
    const options: Options = {
      cwd: this.#cwd,
      settingSources: ["user", "project", "local"],
      systemPrompt: { type: "preset", preset: "claude_code", ...(this.#ledgerPath ? { append: ledgerPrompt(this.#ledgerPath) } : {}) },
      permissionMode: mode,
      ...(mode === "bypassPermissions" ? { allowDangerouslySkipPermissions: true } : {}),
      // An empty model: local login leaves it to the CLI; a provider session
      // names the CLI's own default alias, so that no settings file picks it
      // and the provider's alias variables map it.
      ...(this.#config?.model ? { model: this.#config.model } : plan.env ? { model: PROVIDER_DEFAULT_MODEL } : {}),
      includePartialMessages: true,
      ...(this.#hasTranscript ? { resume: this.#claudeSessionId } : { sessionId: this.#claudeSessionId }),
      sessionStore: this.#store,
      sessionStoreFlush: "eager",
      hooks: { PreToolUse: [{ hooks: [hook] }] },
      spawnClaudeCodeProcess: this.deps.processes.spawner(this.sessionId),
      // Local login: the worker's own environment, unchanged.
      ...(plan.env ? { env: plan.env } : {}),
    };
    const query = this.deps.query({ prompt, options });
    const gen: Generation = { query, prompt, closing: false };
    this.#gen = gen;
    // Later queries of this epoch resume what this one writes.
    this.#hasTranscript = true;
    gen.done = this.#consume(gen);
    return gen;
  }

  /**
   * Whether a new CLI may start. Not during a transcript repair: an input
   * taken before it began (its grants refresh still in flight) would start
   * one on the transcript being repaired; the repair takes the inputs again.
   */
  #mayStart(): boolean {
    return !this.#ending && !this.#stopping && !this.#cliFailed && !this.#repair;
  }

  /** Closes the query and confirms its CLI exited (also one closed by another path). */
  async #closeQuery(waitLoop: boolean): Promise<void> {
    const gen = this.#gen;
    if (!gen) {
      await this.#exited;
      return;
    }
    this.#gen = undefined;
    gen.closing = true;
    gen.prompt.end();
    try {
      gen.query.close();
    } catch (error) {
      this.#log.warn("closing the query failed", { session_id: this.sessionId, error: String(error).slice(0, 512) });
    }
    const exited = this.deps.processes.confirmExit(this.sessionId);
    this.#exited = exited;
    await exited;
    if (waitLoop) await gen.done;
    // Inputs pushed and never taken by a turn go to the next query.
    for (const delivery of [...this.#nextTurn, ...this.#deliveries.values()]) {
      this.#pushed.delete(delivery.inputId);
      this.#notes.unshift(...delivery.notes);
    }
    this.#nextTurn = [];
    this.#deliveries.clear();
  }

  async #consume(gen: Generation): Promise<void> {
    let failure: unknown;
    let handlerError: unknown;
    try {
      for await (const message of gen.query) {
        if (gen.closing) break;
        try {
          await this.#onMessage(message as SDKMessage & Record<string, unknown>);
          // The broken turn has ended: repair while the query is still open
          // (leaving the loop would end it), then the repair closes it.
          if (this.#repair && !this.#turn && !gen.closing) {
            await this.#repairTranscript();
            break;
          }
        } catch (error) {
          handlerError = error ?? new Error("message handler failed");
          break;
        }
      }
    } catch (error) {
      failure = error;
    }
    if (handlerError !== undefined) {
      this.#fatal(handlerError);
      return;
    }
    if (gen.closing || this.#ending) return;
    try {
      // The CLI ended before the broken turn did: report and repair now.
      if (this.#repair) await this.#repairTranscript();
      else await this.#cliEnded(failure);
    } catch (error) {
      this.#fatal(error);
    }
  }

  async #onMessage(message: SDKMessage & Record<string, unknown>): Promise<void> {
    const subtype = message.type === "system" ? message.subtype as string : "";
    if (message.type === "system" && subtype === "init") {
      if (!this.#turn) await this.#startTurn();
      return;
    }
    if ((message.type as string) === "command_lifecycle") {
      const uuid = (message.command_uuid ?? message.uuid) as string | undefined;
      if (message.state === "started" && uuid) this.#attribute(uuid);
      return;
    }
    if (!this.#turn && ["assistant", "stream_event", "user", "result"].includes(message.type)) await this.#startTurn();
    // Task events are published by #task, for background tasks only.
    const event = subtype.startsWith("task_") ? undefined : sdkEvent(message, { interruptNote: !this.#turn?.workerAbort });
    if (event) {
      this.#publisher.add(event.kind, event.json, event.ephemeral);
      await this.#publisher.drained();
    }
    switch (message.type) {
      case "assistant":
        // The query's login no longer works (an expired copied token): the
        // next input starts a new query, which copies the current login.
        if (message.error === "authentication_failed" && this.#turn) this.#turn.authFailed = true;
        for (const use of toolUsesOf(message)) {
          if (!this.#toolUses.get(use.id)) this.#toolUses.remember(use.id, { tool: use.name, input: use.input, agentId: "" });
        }
        break;
      case "result":
        await this.#endTurn(message);
        break;
      case "system":
        switch (subtype) {
          case "permission_denied":
            await this.#denied(message);
            break;
          case "task_started":
          case "task_updated":
          case "task_notification":
            await this.#task(subtype, message);
            break;
          case "mirror_error":
            this.#mirrorError(message);
            break;
          case "informational":
            if (message.tag === "auto_mode_no_server_check") {
              this.#publisher.add(Kind.ERROR, errorPayload(clipUtf8(String(message.content ?? ""), 4096)));
            }
            break;
        }
    }
  }

  // ---- Turns ----

  async #startTurn(): Promise<void> {
    // The SDK announces each input it takes (command_lifecycle "started")
    // before the system/init of the turn that takes it.
    const origin: TurnOriginName = this.#nextTurn.length ? "input" : "cli";
    // A CLI turn delivers the notices of tasks that ended while idle.
    this.#undelivered.clear();
    const turn = new Turn(++this.#localSeq, origin);
    this.#turn = turn;
    this.#clearIdle();
    try {
      // The preceding turn's events go first (docs/wire.md, "Inputs and turns").
      await this.#publisher.flush();
      const reply = await this.ops.call({
        case: "reportTurn",
        value: create(ReportTurnSchema, { localSeq: turn.localSeq, phase: ReportTurn_Phase.STARTED, origin: origins[origin] }),
      });
      turn.turnNo = reply.turnNo;
      turn.resolveStarted(reply.turnNo);
    } catch (error) {
      turn.rejectStarted(error);
      throw error;
    }
    this.#publisher.add(Kind.TURN_STARTED, turnStartedPayload(turn.turnNo, origin));
    // Inputs the SDK announced for this turn. One it has not announced is
    // not this turn's (it may be racing a turn the CLI started by itself);
    // it is attributed when announced or when a result names it.
    for (const delivery of this.#nextTurn) this.#deliver(turn, delivery);
    this.#nextTurn = [];
    void this.#checkInterrupt();
    // Publish the turn's start and its inputs now rather than with the next
    // batch: a worker that dies early in the turn then leaves the boundary
    // and the inputs' delivery in the view (it still can die before the
    // acknowledgement).
    await this.#publisher.flush();
  }

  #attribute(uuid: string): void {
    const delivery = this.#deliveries.get(uuid);
    if (!delivery) return;
    this.#deliveries.delete(uuid);
    if (this.#turn) this.#deliver(this.#turn, delivery);
    else this.#nextTurn.push(delivery);
  }

  #deliver(turn: Turn, delivery: Delivery): void {
    turn.inputIds.push(delivery.inputId);
    for (const note of delivery.notes) {
      if (note.gap) turn.gapNotice = note.gap;
      if (note.task) turn.lostTasks.push(note.task.taskId);
    }
    for (const event of delivery.events) this.#publisher.add(event.kind, event.payload);
  }

  async #endTurn(result: Record<string, unknown>): Promise<void> {
    const turn = this.#turn;
    if (!turn) return;
    // The result names the inputs the turn answered.
    const answered = [result.user_message_uuid, ...(Array.isArray(result.user_message_uuids) ? result.user_message_uuids : [])];
    for (const uuid of answered) if (typeof uuid === "string") this.#attribute(uuid);
    // An input pushed before this turn started and not named by it no longer
    // keeps the session busy: if it is ever named, its own turn starts at once
    // (it may have raced a turn the CLI started itself). It stays unmarked
    // (the next epoch requeues it). When a second turn has started since it
    // was pushed and still nothing named it, it is not going to be: say so.
    for (const delivery of this.#deliveries.values()) {
      if (delivery.pushedAtSeq >= turn.localSeq) continue;
      delivery.unnamed = true;
      if (turn.localSeq - delivery.pushedAtSeq >= 2n && !delivery.warned) {
        delivery.warned = true;
        this.#log.warn("an input was never attributed to a turn", { session_id: this.sessionId, input_id: delivery.inputId });
      }
    }
    for (const denial of Array.isArray(result.permission_denials) ? result.permission_denials as Array<Record<string, unknown>> : []) {
      const toolUseId = typeof denial.tool_use_id === "string" ? denial.tool_use_id : "";
      const tool = typeof denial.tool_name === "string" ? denial.tool_name : "";
      if (!toolUseId || !tool || this.#handledDenials.has(toolUseId)) continue;
      this.#trackDenial(toolUseId);
      this.#toolUses.remember(toolUseId, { tool, input: denial.tool_input, agentId: this.#toolUses.get(toolUseId)?.agentId ?? "" });
      await this.#requestApproval(toolUseId, tool, undefined);
    }
    const aborted = typeof result.terminal_reason === "string" && result.terminal_reason.startsWith("aborted");
    let outcome: TurnOutcome;
    if (aborted) outcome = turn.userAbort ? TurnOutcome.STOPPED : TurnOutcome.ERROR;
    else outcome = result.subtype === "success" && result.is_error !== true ? TurnOutcome.COMPLETED : TurnOutcome.ERROR;
    await this.#reportEnded(turn, outcome, usageJson(result));
  }

  /** Flushes the turn's events, reports ENDED and applies its persistence marks. */
  async #reportEnded(turn: Turn, outcome: TurnOutcome, usage: string): Promise<void> {
    this.#turn = undefined;
    await this.#publisher.flush();
    const marks = this.#marks(turn);
    // Never mark the items of a turn whose transcript is incomplete.
    if (turn.incomplete && marks) {
      this.#brokenItems ??= create(ReportTurn_PersistedSchema);
      this.#brokenItems.inputIds.push(...marks.inputIds);
      if (marks.gapNoticeTurnNo) this.#brokenItems.gapNoticeTurnNo = marks.gapNoticeTurnNo;
      this.#brokenLostTasks.push(...turn.lostTasks);
    }
    const persisted = turn.incomplete ? undefined : marks;
    await this.ops.call({
      case: "reportTurn",
      value: create(ReportTurnSchema, {
        localSeq: turn.localSeq, phase: ReportTurn_Phase.ENDED, outcome, usageJson: usage,
        persisted, transcriptIncomplete: turn.incomplete,
      }),
    });
    if (persisted) await this.#persisted(persisted, turn.lostTasks);
    if (turn.authFailed && !this.#repair && !this.#stopping) {
      this.#log.warn("Claude Code failed authentication; the next input starts a new query", { session_id: this.sessionId });
      this.#closeAfterAuth = true;
    }
    // Background tasks keep running (and leave their output) until they
    // finish or the next input arrives: only then is the query replaced.
    if (this.#closeAfterAuth && !this.#backgroundRunning()) await this.#replaceAfterAuth();
    // Foreground tasks end with their turn.
    for (const [id, task] of this.#tasks) if (!task.backgrounded) this.#tasks.delete(id);
    if (this.#stopping) {
      this.#finish({ release: ReleaseLease_Reason.IDLE });
      return;
    }
    this.#armIdle();
  }

  #marks(turn: Turn): ReportTurn_Persisted | undefined {
    if (!turn.inputIds.length && !turn.gapNotice) return undefined;
    return create(ReportTurn_PersistedSchema, { inputIds: turn.inputIds, gapNoticeTurnNo: turn.gapNotice });
  }

  /** After the control plane recorded the marks: forget them locally. */
  async #persisted(marks: ReportTurn_Persisted, lostTasks: string[]): Promise<void> {
    for (const id of marks.inputIds) this.#pushed.delete(id);
    if (marks.gapNoticeTurnNo && marks.gapNoticeTurnNo === this.#gapMark) this.#gapMark = 0n;
    if (lostTasks.length) {
      await this.ops.call({ case: "patchState", value: create(PatchSessionStateSchema, { bgtasksEnded: lostTasks }) });
    }
  }

  async #checkInterrupt(): Promise<void> {
    const turn = this.#turn;
    if (!turn) return; // An idle interrupt changes nothing.
    let turnNo: bigint;
    try {
      turnNo = await turn.started;
    } catch {
      return;
    }
    if (turn !== this.#turn || turn.interruptSent || turnNo > this.#interruptLevel) return;
    turn.interruptSent = true;
    turn.userAbort = true;
    this.#log.info("interrupting turn", { session_id: this.sessionId, turn_no: Number(turnNo) });
    await this.#interrupt();
  }

  async #interrupt(): Promise<void> {
    const gen = this.#gen;
    if (!gen) return;
    try {
      await withTimeout(gen.query.interrupt(), this.#abortGraceMs);
    } catch (error) {
      this.#log.warn("interrupt failed", { session_id: this.sessionId, error: String(error).slice(0, 512) });
    }
  }

  // ---- The CLI ended or the transcript mirror failed ----

  async #cliEnded(failure: unknown): Promise<void> {
    // Before any await: an input taken meanwhile must not start another CLI.
    this.#cliFailed = true;
    const message = failure === undefined ? "Claude Code exited unexpectedly." : `Claude Code failed: ${String(failure).slice(0, 2048)}`;
    this.#log.error("Claude Code ended", { session_id: this.sessionId, error: message });
    await this.#closeQuery(false);
    this.#publisher.add(Kind.ERROR, errorPayload(message));
    const turn = this.#turn;
    if (turn?.turnNo) {
      // A failure before the result is no evidence of persistence.
      turn.inputIds = [];
      turn.gapNotice = 0n;
      turn.lostTasks = [];
      await this.#reportEnded(turn, TurnOutcome.ERROR, "");
    }
    this.#turn = undefined;
    this.#finish({ release: ReleaseLease_Reason.ERROR });
  }

  #mirrorError(message: Record<string, unknown>): void {
    const key = message.key as { subpath?: string } | undefined;
    this.#log.error("transcript mirror failed", {
      session_id: this.sessionId, subpath: key?.subpath ?? "", error: String(message.error ?? "").slice(0, 1024),
    });
    this.#broken = true;
    this.#publisher.add(Kind.ERROR, errorPayload(`Saving the transcript failed: ${String(message.error ?? "").slice(0, 1024)}`));
    if (this.#repair) return;
    this.#repair = true;
    const turn = this.#turn;
    if (!turn) return;
    turn.incomplete = true;
    turn.workerAbort = true;
    // Stop the turn: its result ends the loop; a CLI that does not answer is closed.
    const gen = this.#gen;
    void this.#interrupt();
    setTimeout(() => {
      if (this.#gen === gen && this.#turn === turn && gen) {
        gen.closing = true;
        void this.#repairAfterTimeout(gen);
      }
    }, this.#abortGraceMs).unref?.();
  }

  async #repairAfterTimeout(gen: Generation): Promise<void> {
    if (this.#ending) return;
    try {
      gen.query.close();
    } catch {
      // Closed already.
    }
    try {
      await gen.done;
      await this.#repairTranscript();
    } catch (error) {
      this.#fatal(error);
    }
  }

  /**
   * After mirror_error: report the broken turn ENDED with
   * transcript_incomplete and no marks (the session becomes BROKEN), close
   * the query, resend exactly the batches the control plane has not
   * acknowledged, then mark the turn's items persisted and send
   * TranscriptRepaired. When the missing batches are gone (refused as
   * INVALID or LIMIT, or beyond the bound), the repair fails: release with
   * ERROR and the session stays BROKEN for the user to recover.
   */
  async #repairTranscript(): Promise<void> {
    const turn = this.#turn;
    if (turn?.turnNo) {
      turn.incomplete = true;
      await this.#reportEnded(turn, TurnOutcome.ERROR, "");
    }
    this.#turn = undefined;
    if (this.#ending) return;
    await this.#closeQuery(false);
    this.#tasksLost();
    let failure: unknown;
    for (let attempt = 0; attempt < REPAIR_ATTEMPTS; attempt++) {
      try {
        await this.#store.repair();
        failure = undefined;
        break;
      } catch (error) {
        if (error instanceof FencedError || error instanceof StoppedError || isLeaseAbort(error)) throw error;
        failure = error;
        if (error instanceof TranscriptLostError) break;
      }
    }
    if (failure !== undefined) {
      this.#log.error("transcript repair failed", { session_id: this.sessionId, error: String(failure).slice(0, 1024) });
      this.#publisher.add(Kind.ERROR, errorPayload(`Repairing the transcript failed: ${String(failure).slice(0, 1024)}`));
      this.#finish({ release: ReleaseLease_Reason.ERROR });
      return;
    }
    const items = this.#brokenItems;
    const lostTasks = this.#brokenLostTasks;
    if (items) await this.ops.call({ case: "patchState", value: create(PatchSessionStateSchema, { persisted: items }) });
    await this.ops.call({ case: "transcriptRepaired", value: create(TranscriptRepairedSchema) });
    this.#brokenItems = undefined;
    this.#brokenLostTasks = [];
    if (items) await this.#persisted(items, lostTasks);
    this.#broken = false;
    this.#repair = false;
    this.#log.info("transcript repaired", { session_id: this.sessionId });
    this.#kickInputs();
  }

  /**
   * The closed CLI took its background tasks with it: tell the model with
   * the next input (and send bgtasks_ended once that turn is persisted),
   * as for tasks lost with a worker, and stop holding the lease for them.
   * Tasks that ended between turns, whose notice the CLI had yet to
   * deliver, are told too.
   */
  #tasksLost(): void {
    for (const [taskId, task] of this.#tasks) {
      if (!task.backgrounded) continue;
      this.#notes.push({
        kind: "lost_bgtask", text: lostTaskText(taskId, task.toolUseId, task.description),
        task: create(PatchSessionState_BackgroundTaskSchema, { taskId, toolUseId: task.toolUseId, kind: task.kind, description: task.description }),
      });
    }
    this.#tasks.clear();
    for (const [taskId, notice] of this.#undelivered) {
      this.#notes.push({ kind: "lost_bgtask", text: undeliveredTaskText(taskId, notice.toolUseId, notice.status, notice.summary) });
    }
    this.#undelivered.clear();
  }

  // ---- Inputs ----

  #kickInputs(): void {
    if (this.#ending || this.#stopping) return;
    if (this.#inputsBusy) {
      this.#inputsAgain = true;
      return;
    }
    this.#inputsBusy = true;
    this.#clearIdle();
    void (async () => {
      try {
        do {
          this.#inputsAgain = false;
          await this.#takeInputs();
        } while (this.#inputsAgain && !this.#ending);
      } catch (error) {
        this.#fatal(error);
      } finally {
        this.#inputsBusy = false;
        this.#armIdle();
      }
    })();
  }

  #backgroundRunning(): boolean {
    return [...this.#tasks.values()].some((task) => task.backgrounded);
  }

  async #replaceAfterAuth(kick = true): Promise<void> {
    if (!this.#closeAfterAuth || this.#turn || this.#repair) return;
    this.#closeAfterAuth = false;
    await this.#closeQuery(false);
    this.#tasksLost();
    // Inputs the closed query had not taken go to the next one.
    if (kick) this.#kickInputs();
  }

  async #takeInputs(): Promise<void> {
    if (this.#repair) return;
    const reply = await this.ops.call({ case: "takeInputs", value: create(TakeInputsSchema) });
    const inputs = reply.inputs.filter((input) => !this.#pushed.has(input.inputId));
    if (!inputs.length || this.#ending || this.#repair) return;
    if (inputs.some((input) => input.kind === InputKind.APPROVAL)) await this.#refreshGrants();
    // The next input replaces a query whose login failed (its background tasks are told lost).
    await this.#replaceAfterAuth(false);
    if (this.#ending) return;
    const gen = await this.#ensureQuery();
    if (!gen || this.#ending) return;
    for (const input of inputs) this.#push(gen, input);
  }

  #push(gen: Generation, input: SessionReply_Inputs_Input): void {
    const timestamp = new Date().toISOString();
    const notes = this.#notes.splice(0);
    const events: Delivery["events"] = [];
    for (const note of notes) {
      events.push({ kind: Kind.USER, payload: userMessagePayload(this.#claudeSessionId, randomUUID(), note.text, note.kind, timestamp) });
      if (note.task) {
        events.push({
          kind: Kind.BGTASK_ENDED,
          payload: lostTaskPayload(this.#claudeSessionId, randomUUID(), note.task, lostTaskSummary(note.task.taskId)),
        });
      }
    }
    // A control-plane note keeps its category as given, empty included.
    const synthetic = input.kind === InputKind.NOTE;
    const note: string | undefined = input.kind === InputKind.APPROVAL ? "approval_retry" : synthetic ? input.note : undefined;
    events.push({ kind: Kind.USER, payload: userMessagePayload(this.#claudeSessionId, input.inputId, input.text, note, timestamp) });
    const content = notes.length
      ? [...notes.map((n) => ({ type: "text" as const, text: systemReminder(n.text) })), { type: "text" as const, text: input.text }]
      : input.text;
    const now = input.priority === InputPriority.NOW;
    const message: SDKUserMessage = {
      type: "user", message: { role: "user", content }, parent_tool_use_id: null,
      uuid: input.inputId as SDKUserMessage["uuid"],
      ...(synthetic ? { isSynthetic: true } : {}),
      ...(now ? { priority: "now" as const } : {}),
    };
    this.#pushed.add(input.inputId);
    this.#deliveries.set(input.inputId, { inputId: input.inputId, notes, events, pushedAtSeq: this.#localSeq });
    // A NOW input aborts the turn running at its delivery, not its replacement.
    if (now && this.#turn) this.#turn.userAbort = true;
    gen.prompt.push(message);
  }

  // ---- Approvals ----

  #rememberGrant(tool: string, matchKey: string): void {
    const key = `${tool}\n${matchKey}`;
    this.#grants.delete(key);
    this.#grants.set(key, true);
    if (this.#grants.size > MAX_TRACKED) this.#grants.delete(this.#grants.keys().next().value!);
  }

  #trackDenial(toolUseId: string): void {
    this.#handledDenials.add(toolUseId);
    if (this.#handledDenials.size > MAX_TRACKED) this.#handledDenials.delete(this.#handledDenials.values().next().value!);
  }

  async #refreshGrants(): Promise<void> {
    if (this.#broken) return;
    const reply = await this.ops.call({
      case: "getState", value: create(GetSessionStateSchema, { sections: [GetSessionState_Section.GRANTS] }),
    });
    const state = reply.state;
    if (state?.stopped) {
      this.stop();
      return;
    }
    for (const grant of state?.grants ?? []) {
      if (grant.device === this.#device) this.#rememberGrant(grant.tool, grant.matchKey);
    }
  }

  async #preToolUse(input: Parameters<HookCallback>[0], signal: AbortSignal): Promise<HookJSONOutput> {
    if (input.hook_event_name !== "PreToolUse") return {};
    const agentId = input.agent_id ?? "";
    this.#toolUses.remember(input.tool_use_id, { tool: input.tool_name, input: input.tool_input, agentId });
    if (this.#broken || this.#ending) return {};
    const subject = approvalSubject(input.tool_name, input.tool_input, this.#cwd);
    if (!subject || !this.#grants.has(`${subject.tool}\n${subject.matchKey}`)) return {};
    try {
      const reply = await this.ops.call({
        case: "consumeGrant",
        value: create(ConsumeGrantSchema, {
          tool: subject.tool, device: this.#device, matchKey: subject.matchKey, toolUseId: input.tool_use_id,
        }),
      }, { budgetMs: this.deps.grantBudgetMs ?? GRANT_BUDGET_MS });
      if (!reply.allowed || signal.aborted) return {};
      this.#grants.delete(`${subject.tool}\n${subject.matchKey}`);
      return {
        hookSpecificOutput: {
          hookEventName: "PreToolUse", permissionDecision: "allow", permissionDecisionReason: "The user approved this call once.",
        },
      };
    } catch (error) {
      if (error instanceof BrokenError) this.#broken = true;
      else if (error instanceof FencedError || error instanceof StoppedError) this.#fatal(error);
      else if (!isLeaseAbort(error)) this.#log.warn("ConsumeGrant failed", { session_id: this.sessionId, error: String(error).slice(0, 512) });
      return {};
    }
  }

  async #denied(message: Record<string, unknown>): Promise<void> {
    const toolUseId = typeof message.tool_use_id === "string" ? message.tool_use_id : "";
    const tool = typeof message.tool_name === "string" ? message.tool_name : "";
    if (!toolUseId || !tool || this.#handledDenials.has(toolUseId)) return;
    this.#trackDenial(toolUseId);
    // Explicit deny rules and hooks decided on purpose; only the permission
    // mode (auto's classifier, dontAsk, ...) is asked about.
    if (message.decision_reason_type === "rule" || message.decision_reason_type === "hook") return;
    const reason = typeof message.decision_reason === "string" ? message.decision_reason : undefined;
    await this.#requestApproval(toolUseId, tool, reason, typeof message.agent_id === "string" ? message.agent_id : undefined);
  }

  async #requestApproval(toolUseId: string, tool: string, reason: string | undefined, agentId?: string): Promise<void> {
    if (this.#broken) return;
    const use = this.#toolUses.get(toolUseId);
    const subject: ApprovalSubject | undefined = use ? approvalSubject(tool, use.input, this.#cwd) : undefined;
    if (!subject) {
      this.#log.warn("denied tool use without a known input; no approval requested", { session_id: this.sessionId, tool: tool.slice(0, 256) });
      return;
    }
    const why = approvalReason(reason);
    try {
      const reply = await this.ops.call({
        case: "requestApproval",
        value: create(RequestApprovalSchema, {
          toolUseId, agentId: agentId ?? use?.agentId ?? "", tool: subject.tool, device: this.#device,
          matchKey: subject.matchKey, display: subject.display, reason: why,
        }),
      });
      // A grant exists only once the user approves (Lease.state.grants or a refresh).
      this.#publisher.add(Kind.APPROVAL_PENDING, approvalPendingPayload({
        approvalId: reply.approvalId, tool: subject.tool, device: this.#device, toolUseId, display: subject.display, reason: why,
      }));
    } catch (error) {
      if (error instanceof BrokenError) {
        this.#broken = true;
        return;
      }
      if (!(error instanceof RejectedError)) throw error;
      this.#log.warn("RequestApproval rejected", { session_id: this.sessionId, error: error.message.slice(0, 512) });
      this.#publisher.add(Kind.ERROR, errorPayload(`Asking for approval of ${tool} failed: ${error.message.slice(0, 512)}`));
    }
  }

  // ---- Background tasks ----

  /**
   * The CLI's task_* messages. Background tasks are published (BGTASK_*)
   * and recorded with the control plane; a foreground task is kept only
   * until its turn ends, and published if it moves to the background.
   */
  async #task(subtype: string, message: Record<string, unknown>): Promise<void> {
    const taskId = typeof message.task_id === "string" ? message.task_id : "";
    if (!taskId) return;
    let task = this.#tasks.get(taskId);
    if (subtype === "task_started") {
      if (message.ambient === true || task || this.#tasks.size >= MAX_TRACKED) return;
      const json = JSON.stringify(message);
      task = {
        startedAt: performance.now(), toolUseId: typeof message.tool_use_id === "string" ? message.tool_use_id : "",
        description: clipUtf8(typeof message.description === "string" ? message.description : "", 1024),
        kind: clipUtf8(typeof message.task_type === "string" ? message.task_type : "", 128),
        backgrounded: message.is_backgrounded === true, patched: false, published: false,
        held: message.is_backgrounded === true || json.length > MAX_HELD_TASK_EVENT ? undefined : json,
      };
      this.#tasks.set(taskId, task);
      if (task.backgrounded) {
        this.#publisher.add(Kind.BGTASK_STARTED, json);
        task.published = true;
      }
    } else if (subtype === "task_updated") {
      const patch = message.patch as { is_backgrounded?: unknown } | undefined;
      if (!task || task.backgrounded || patch?.is_backgrounded !== true) return;
      task.backgrounded = true;
      if (task.held) {
        this.#publisher.add(Kind.BGTASK_STARTED, task.held);
        task.published = true;
      }
      task.held = undefined;
    } else {
      this.#tasks.delete(taskId);
      if (task?.published) this.#publisher.add(Kind.BGTASK_ENDED, JSON.stringify(message));
      if (task?.backgrounded && !this.#turn) {
        // The CLI starts a turn to tell the model; until then the notice is only in the CLI.
        this.#undelivered.set(taskId, {
          toolUseId: task.toolUseId, status: String(message.status ?? ""), summary: clipUtf8(String(message.summary ?? ""), 1024),
        });
        if (this.#undelivered.size > MAX_TRACKED) this.#undelivered.delete(this.#undelivered.keys().next().value!);
      }
      if (task?.patched) await this.#patchTasks({ bgtasksEnded: [taskId] });
      this.#armIdle();
      return;
    }
    if (task.backgrounded && !task.patched) {
      task.patched = true;
      await this.#patchTasks({
        bgtasksStarted: [create(PatchSessionState_BackgroundTaskSchema, {
          taskId: clipUtf8(taskId, 128), toolUseId: clipUtf8(task.toolUseId, 128), kind: task.kind, description: task.description,
        })],
      });
    }
  }

  async #patchTasks(patch: { bgtasksStarted?: PatchSessionState_BackgroundTask[]; bgtasksEnded?: string[] }): Promise<void> {
    try {
      await this.ops.call({ case: "patchState", value: create(PatchSessionStateSchema, patch) });
    } catch (error) {
      if (!(error instanceof RejectedError)) throw error;
      this.#log.warn("background task not recorded", { session_id: this.sessionId, error: error.message.slice(0, 512) });
    }
  }

  // ---- Idle release ----

  #clearIdle(): void {
    if (this.#idleTimer) clearTimeout(this.#idleTimer);
    this.#idleTimer = undefined;
  }

  #busy(): boolean {
    return this.#turn !== undefined || [...this.#deliveries.values()].some((d) => !d.unnamed) || this.#nextTurn.length > 0 ||
      this.#inputsBusy || this.#repair;
  }

  #armIdle(): void {
    this.#clearIdle();
    if (this.#ending || this.#busy()) return;
    const running = [...this.#tasks.values()].filter((task) => task.backgrounded);
    const delay = running.length
      ? Math.max(0, Math.min(...running.map((task) => task.startedAt)) + this.#taskHoldMs - performance.now())
      : this.#idleMs;
    this.#idleTimer = setTimeout(() => {
      this.#idleTimer = undefined;
      if (this.#ending || this.#busy()) return;
      const left = [...this.#tasks.values()].filter((task) => task.backgrounded);
      if (left.length && Math.min(...left.map((task) => task.startedAt)) + this.#taskHoldMs > performance.now()) {
        this.#armIdle();
        return;
      }
      if (left.length) this.#log.warn("releasing with background tasks still running; they will count as lost", { session_id: this.sessionId, tasks: left.length });
      this.#log.info("session idle; releasing", { session_id: this.sessionId });
      this.#finish({ release: ReleaseLease_Reason.IDLE });
    }, delay);
  }
}

function usageJson(result: Record<string, unknown>): string {
  const usage = (result.usage ?? {}) as Record<string, unknown>;
  const json = JSON.stringify({
    input_tokens: usage.input_tokens, output_tokens: usage.output_tokens,
    cache_creation_input_tokens: usage.cache_creation_input_tokens, cache_read_input_tokens: usage.cache_read_input_tokens,
    total_cost_usd: result.total_cost_usd, duration_ms: result.duration_ms, num_turns: result.num_turns,
  });
  return Buffer.byteLength(json) <= USAGE_BYTES ? json : "{}";
}

function withTimeout<T>(promise: Promise<T>, ms: number): Promise<T> {
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error(`timed out after ${ms} ms`)), ms);
    promise.then((value) => {
      clearTimeout(timer);
      resolve(value);
    }, (error: unknown) => {
      clearTimeout(timer);
      reject(error);
    });
  });
}
