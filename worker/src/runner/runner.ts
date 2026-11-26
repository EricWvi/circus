import { randomUUID } from "node:crypto";
import { statSync } from "node:fs";
import { isMainThread } from "node:worker_threads";
import { create } from "@bufbuild/protobuf";
import {
  type CanUseTool, type HookCallback, type HookJSONOutput, type Options, type PermissionMode, type PermissionResult, type Query,
  type SDKMessage, type SDKUserMessage,
} from "@anthropic-ai/claude-agent-sdk";
import { ApprovalKind, InputPriority, TurnOrigin, TurnOutcome, type SessionConfig } from "@circus/proto/circus/v1/session_pb";
import {
  ConsumeGrantSchema, GetLaunchEnvSchema, GetSessionStateSchema, GetSessionState_Section, PatchSessionStateSchema,
  PatchSessionState_BackgroundTaskSchema, ReleaseLeaseSchema, ReleaseLease_Reason, ReportTurnSchema,
  ReportTurn_PersistedSchema, ReportTurn_Phase, RequestApprovalSchema, SessionReply_Inputs_Input_Kind as InputKind,
  SessionReply_Rejected_Code as RejectCode,
  TakeInputsSchema, TranscriptRepairedSchema, WorkerEvent_Nudge_Reason as Reason,
  type PatchSessionState_BackgroundTask, type ReportTurn_Persisted, type SessionReply_Inputs_Input, type SessionReply_Lease,
} from "@circus/proto/circus/v1/worker_link_pb";
import { AgentKind } from "@circus/proto/circus/v1/agent_pb";
import type { Logger } from "../log.js";
import type { CliProcessesPort } from "../process.js";
import {
  DISALLOWED_TOOLS, PublishedToolUses, QUESTION_INPUT_BYTES, QUESTION_TOOL, ToolUses, approvalReason, approvalSubject, clipUtf8, denials,
  questionDisplay, toolUsesOf, type ApprovalSubject,
} from "./approvals.js";
import {
  EventPublisher, Kind, QUESTION_ANSWER_NOTE, errorPayload, gapDetectedPayload, lostTaskPayload, sdkEvent, turnStartedPayload,
  userMessagePayload, type NoteKind, type TurnOriginName,
} from "./events.js";
import {
  historyMissingText, lostTaskSummary, lostTaskText, previousTurnInterruptedText, systemReminder, undeliveredTaskText,
} from "./notes.js";
import {
  BrokenError, FencedError, RejectedError, SessionOps, StoppedError, type LinkPort, type ReadyWaiter, type Semaphore,
} from "./ops.js";
import { PROVIDER_DEFAULT_MODEL, launchPlan } from "./launch.js";
import { ledgerNoteKinds, ledgerPrompt } from "./ledger.js";
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
  /**
   * The agent's executable when it is installed at this worker's version
   * (AgentManager.executable); undefined otherwise. Never the SDK's own.
   */
  agentExecutable: (agent: AgentKind) => string | undefined;
  /** Idle time before an idle release; five minutes. */
  idleMs?: number;
  /** Longest wait for running background tasks; two hours. */
  taskHoldMs?: number;
  /** How long a worker-initiated interrupt may take before the query is closed; also bounds a graceful end. */
  abortGraceMs?: number;
  /** How long a deferred ReleaseIdle stays wanted; the control plane's 30 s by default. */
  releaseWantedMs?: number;
  /** How long a shutdown waits for the running turn to end; ten seconds. */
  shutdownGraceMs?: number;
  /** Retry budget of one SessionStore append; 20 seconds. */
  appendBudgetMs?: number;
  /** Retry budget of ConsumeGrant in the hook; 20 seconds. */
  grantBudgetMs?: number;
  /** How long an approval waits for its call's message (PUBLISH_WAIT_MS). */
  publishWaitMs?: number;
  /** The worker-wide budget of retained transcript entries; the process's by default. */
  retained?: RetainedBudget;
  /** The worker's environment, the base of a provider's CLI environment; process.env by default. */
  env?: NodeJS.ProcessEnv;
}

/** released: the lease was released; failed: released with ERROR; lost: dropped without a release. */
export type RunnerEnd = "released" | "failed" | "lost";

const IDLE_MS = 5 * 60_000;
/**
 * How long a ReleaseIdle refused while the session was busy stays
 * wanted: the control plane's ReleaseIdleTimeout, after which it counts
 * the request as declined and may ask another session instead.
 */
const RELEASE_WANTED_MS = 30_000;
const TASK_HOLD_MS = 2 * 60 * 60_000;
const ABORT_GRACE_MS = 10_000;
const SHUTDOWN_GRACE_MS = 10_000;
const REPAIR_ATTEMPTS = 3;
/** A foreground task's task_started is kept (to publish if it is backgrounded) up to this size. */
const MAX_HELD_TASK_EVENT = 64 << 10;
const GRANT_BUDGET_MS = 20_000;
const MAX_TRACKED = 256;
const USAGE_BYTES = 4096;
// "plan" is not offered: ExitPlanMode is disallowed until plan approval exists.
const permissionModes = new Set<PermissionMode>(["default", "acceptEdits", "bypassPermissions", "dontAsk", "auto"]);

class LeaseLost extends Error {
  override name = "LeaseLost";
}

interface Note {
  kind: NoteKind;
  text: string;
  gap?: bigint;
  task?: PatchSessionState_BackgroundTask;
  /**
   * Only this worker knows of it (a task notice the closed CLI never
   * delivered): the control plane would not tell the next holder, so the
   * session is not released on request while it is owed.
   */
  workerOnly?: boolean;
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
  /** A silent note the CLI has recorded (its own result came): in the transcript, waiting for the next turn. */
  recorded?: boolean;
  /** What was pushed (a silent note is pushed again when the CLI failed to record it). */
  message: SDKUserMessage;
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
  /** The tool uses its message loop has published, for its approvals to wait on. */
  published: PublishedToolUses;
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
  /**
   * Notes pushed with shouldQuery false, until persisted: idle, the CLI
   * records each with a system/init and a result of its own but no model
   * call (no turn); the input after them runs the turn that reads them.
   */
  #silent = new Set<string>();
  /** The CLI is recording silent notes (their system/init came): no turn runs. */
  #quiet = false;
  /** Bounds the wait for the turn after recorded silent notes (#settleRecorded). */
  #silentTimer?: ReturnType<typeof setTimeout>;
  /** Pushed inputs the CLI has announced (command_lifecycle queued or started), until persisted. */
  #announced = new Set<string>();
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
  /**
   * Tool uses a grant allowed. An ask rule still sends such a call to
   * canUseTool after the hook allowed it; canUseTool then allows it too.
   */
  #granted = new Map<string, string>();
  #inputsBusy = false;
  #inputsAgain = false;
  /** A turn's ENDED is being reported and its marks applied. */
  #settling = false;
  /** When a ReleaseIdle refused because the session was busy arrived (0: none wanted). */
  #releaseWantedAt = 0;
  readonly #releaseWantedMs: number;
  /** The CLI's input has ended for a graceful end: no turn may start and every tool use is denied. */
  #exiting = false;
  /** Cuts a graceful end short: a stop, a shutdown or a lost lease arrived during it. */
  readonly #cutShort = Promise.withResolvers<void>();
  /** The release reason of a stop or error that cut a graceful end short. */
  #lateReason?: ReleaseLease_Reason;
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
    this.#releaseWantedMs = deps.releaseWantedMs ?? RELEASE_WANTED_MS;
    this.ops = new SessionOps(deps.link, sessionId, lease.epoch, this.#abort.signal, deps.ready, deps.slots);
    this.#publisher = new EventPublisher(this.ops, deps.log, (error) => this.#fatal(error));
    this.#store = new TranscriptStore(this.ops, lease.claudeSessionId, {
      fenced: () => this.lose("fenced on append"),
      stopped: () => this.stop(),
      gap: (subpath, unknown) => {
        if (this.#gapAtLease !== 0n) this.#publisher.add(Kind.TRANSCRIPT_GAP_DETECTED, gapDetectedPayload(subpath, unknown));
      },
      settled: () => this.#retryRelease(),
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
    // The control plane keeps JOB pending until the holder reads the jobs
    // section (docs/wire.md, the hint table), and raises it whenever
    // earlier hints may be lost (a resumption after its restart, a
    // transcript repair).
    if (reasons.includes(Reason.JOB)) void this.#readJobs().catch((error: unknown) => this.#fatal(error));
  }

  /**
   * Nudge JOB: GetSessionState with SECTION_JOBS, which acknowledges the
   * reason. A local session submits no device calls, so it has no device
   * jobs to deliver; one listed anyway is logged and left to the control
   * plane.
   */
  async #readJobs(): Promise<void> {
    if (this.#ending) return;
    const reply = await this.ops.call({
      case: "getState", value: create(GetSessionStateSchema, { sections: [GetSessionState_Section.JOBS] }),
    });
    if (reply.state?.stopped) {
      this.stop();
      return;
    }
    const jobs = reply.state?.jobs.length ?? 0;
    if (jobs) this.#log.error("device jobs listed for a local session; ignoring them", { session_id: this.sessionId, jobs });
  }

  /**
   * ReleaseIdle: the control plane asks for the lease back because a
   * session of the same provider waits for its cap. An idle session is
   * released exactly as by the idle timer (IDLE, the CLI closed first); a
   * busy one is kept and the request is a no-op. Busy here is more than
   * the idle timer's notion: a background task still running or ended and
   * not yet told to the model, a turn's end still being reported, or a
   * broken transcript also keep it. Reports whether the release started.
   */
  releaseIdle(): boolean {
    if (this.#ending) return false;
    if (!this.#releasable()) {
      // Kept for a while: a session that turns idle without starting a
      // turn (its ENDED reported, the CLI's last writes stored, no inputs
      // after all) is released then; a turn's start drops it, since the
      // control plane asks again after that turn.
      this.#releaseWantedAt = performance.now();
      this.#log.info("release request deferred; the session is busy", { session_id: this.sessionId, kept_by: this.#keeps() });
      return false;
    }
    this.#idleRelease(true);
    return true;
  }

  /**
   * Whether a requested release may start now. Beyond the idle timer's
   * notion of busy (#busy): a turn's end still being reported, the CLI's
   * writes after its last result not yet stored, a broken transcript, a
   * background task running or ended and not yet told to the model, and
   * a task notice only this worker holds. (#stopping needs no check: a
   * shutdown either finishes at once or waits for the running turn, which
   * #busy and #settling cover.)
   */
  #releasable(): boolean {
    return this.#keeps() === "";
  }

  /** What keeps the session from a requested release, "" when nothing does (for #releasable and the log). */
  #keeps(): string {
    if (this.#busy()) return "busy";
    if (this.#settling) return "turn_ending";
    if (this.#broken) return "broken";
    if (this.#backgroundRunning()) return "background_task";
    if (this.#undelivered.size > 0) return "task_notice";
    if (this.#notes.some((note) => note.workerOnly)) return "worker_note";
    if (!this.#store.idle) return "transcript_unacknowledged";
    return "";
  }

  /** A deferred ReleaseIdle, at a moment the session may have turned idle. */
  #retryRelease(): void {
    if (!this.#releaseWantedAt || this.#ending) return;
    if (performance.now() - this.#releaseWantedAt > this.#releaseWantedMs) {
      this.#releaseWantedAt = 0;
      return;
    }
    if (!this.#releasable()) return;
    this.#releaseWantedAt = 0;
    this.#idleRelease(true);
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
    // A graceful end in progress closes the CLI at once (the pool's deadline).
    this.#cutShort.resolve();
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
    if (config.agent !== AgentKind.CLAUDE_CODE) {
      this.#log.error("lease of a session for an agent this worker does not run; releasing it", {
        session_id: this.sessionId, agent: config.agent,
      });
      this.#publisher.add(Kind.ERROR, errorPayload("This worker runs only Claude Code sessions."));
      this.#finish({ release: ReleaseLease_Reason.ERROR });
      return;
    }
    if (!this.#claudeExecutable()) return;
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

  /**
   * The installed Claude Code; when it is not installed at this worker's
   * version, the session's lease is released with ERROR instead.
   */
  #claudeExecutable(): string | undefined {
    const executable = this.deps.agentExecutable(AgentKind.CLAUDE_CODE);
    if (executable) return executable;
    this.#log.error("Claude Code is not installed on this worker at its version; releasing the session", {
      session_id: this.sessionId,
    });
    this.#publisher.add(Kind.ERROR, errorPayload("Claude Code is not installed on this worker. Install it from the worker's page in the Web UI."));
    this.#finish({ release: ReleaseLease_Reason.ERROR });
    return undefined;
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
    if (this.#ending) {
      // Already ending (an idle release's graceful end): a stop or an
      // error closes the CLI at once and gives its reason to the release.
      if ("lost" in how || how.release !== ReleaseLease_Reason.IDLE) {
        if ("release" in how) this.#lateReason ??= how.release;
        this.#cutShort.resolve();
      }
      return;
    }
    this.#ending = this.#close(how).then(
      (end) => this.#end.resolve(end),
      (error: unknown) => {
        this.#log.error("session ending failed", { session_id: this.sessionId, error: String(error).slice(0, 1024) });
        this.#end.resolve("lost");
      });
  }

  async #close(how: { lost: true } | { release: ReleaseLease_Reason }): Promise<RunnerEnd> {
    this.#clearIdle();
    // An idle release lets the CLI finish on its own: it still writes
    // after its last result (last-prompt, cost-state), as it exits. Only
    // when the CLI holds nothing to run (no input pushed and unanswered,
    // no background task, no task notice to deliver), and never for a
    // stop, an error or a shutdown, which close it at once (a shutdown has
    // the pool's deadline to keep). The end and the drain share one grace.
    const graceful = "release" in how && how.release === ReleaseLease_Reason.IDLE && !this.#stopping && !this.#abort.signal.aborted &&
      this.#cliIdle();
    const deadline = performance.now() + this.#abortGraceMs;
    await this.#closeQuery(true, graceful ? deadline : undefined);
    // Its last writes reach the control plane before the store goes.
    if (graceful) await within(Promise.race([this.#store.drained(), this.#cutShort.promise]), deadline);
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
    // A stop or an error that cut the graceful end short names the release.
    const reason = this.#lateReason ?? how.release;
    try {
      await this.ops.call({ case: "releaseLease", value: create(ReleaseLeaseSchema, { reason }) });
      this.#log.info("session lease released", { session_id: this.sessionId, reason: ReleaseLease_Reason[reason] });
    } catch (error) {
      if (error instanceof FencedError) return "lost";
      if (isLeaseAbort(error)) return "lost";
      throw error;
    } finally {
      this.#abort.abort(new LeaseLost("session released"));
    }
    return reason === ReleaseLease_Reason.ERROR ? "failed" : "released";
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
    const executable = this.#claudeExecutable();
    if (!executable) return undefined;
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
    const published = new PublishedToolUses(undefined, this.deps.publishWaitMs);
    const canUseTool: CanUseTool = (tool, input, { toolUseID, agentID, decisionReason, signal }) =>
      this.#canUseTool(published, tool, input, toolUseID, agentID ?? "", decisionReason, signal);
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
      // With canUseTool, a call the permission mode asks about reaches the
      // worker instead of being denied, and AskUserQuestion is available.
      canUseTool,
      disallowedTools: DISALLOWED_TOOLS,
      // The installed Claude Code: the SDK never looks for its own.
      pathToClaudeCodeExecutable: executable,
      spawnClaudeCodeProcess: this.deps.processes.spawner(this.sessionId),
      // Local login: the worker's own environment, unchanged.
      ...(plan.env ? { env: plan.env } : {}),
    };
    const query = this.deps.query({ prompt, options });
    const gen: Generation = { query, prompt, closing: false, published };
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
  /**
   * Nothing is pushed to or running in the CLI: no turn, no input it holds,
   * no background task or notice. (#nextTurn is implied by the callers'
   * #busy check, which neither the idle timer nor a requested release
   * passes with an announced turn waiting; it is kept so this check stands
   * on its own.)
   */
  #cliIdle(): boolean {
    return !this.#turn && this.#deliveries.size === 0 && this.#nextTurn.length === 0 && !this.#backgroundRunning() &&
      this.#undelivered.size === 0;
  }

  /**
   * Closes the open query and confirms its CLI's exit. With a graceful
   * deadline, the CLI's input ends first and the query is closed once it
   * has ended by itself (its exit-time writes done) or at the deadline;
   * messages it still yields meanwhile are handled, but no turn starts
   * (the query is closed at once instead) and every tool use is denied.
   */
  async #closeQuery(waitLoop: boolean, graceful?: number): Promise<void> {
    const gen = this.#gen;
    if (!gen) {
      await this.#exited;
      return;
    }
    this.#gen = undefined;
    if (graceful !== undefined && gen.done) {
      this.#exiting = true;
      gen.prompt.end();
      await within(Promise.race([gen.done, this.#cutShort.promise]), graceful);
    }
    gen.prompt.end();
    gen.closing = true;
    try {
      gen.query.close();
    } catch (error) {
      this.#log.warn("closing the query failed", { session_id: this.sessionId, error: String(error).slice(0, 512) });
    }
    const exited = this.deps.processes.confirmExit(this.sessionId);
    this.#exited = exited;
    await exited;
    if (waitLoop) await gen.done;
    // Silent notes the CLI recorded are in its transcript: never pushed again.
    await this.#settleRecorded();
    // Inputs pushed and never taken by a turn go to the next query.
    for (const delivery of [...this.#nextTurn, ...this.#deliveries.values()]) {
      this.#pushed.delete(delivery.inputId);
      this.#notes.unshift(...delivery.notes);
    }
    this.#nextTurn = [];
    this.#deliveries.clear();
    this.#silent.clear();
    this.#announced.clear();
    this.#quiet = false;
  }

  async #consume(gen: Generation): Promise<void> {
    let failure: unknown;
    let handlerError: unknown;
    try {
      for await (const message of gen.query) {
        if (gen.closing) break;
        try {
          await this.#onMessage(message as SDKMessage & Record<string, unknown>);
          // Its tool uses' approvals may be requested now.
          gen.published.published(message as Record<string, unknown>);
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
    gen.published.end();
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
      if (!this.#turn && !this.#silentInit(message)) await this.#startTurn(resultUuids(message));
      return;
    }
    if ((message.type as string) === "command_lifecycle") {
      const uuid = (message.command_uuid ?? message.uuid) as string | undefined;
      if (uuid && this.#pushed.has(uuid) && (message.state === "queued" || message.state === "started")) this.#announced.add(uuid);
      // A cancelled input runs no turn: it no longer holds off the bound,
      // which starts again for recorded notes still waiting.
      if (uuid && message.state === "cancelled" && this.#announced.delete(uuid) && !this.#silent.has(uuid) &&
        !this.#turn && this.#nextTurn.some((d) => d.recorded)) this.#boundSilent();
      if (message.state === "started" && uuid) this.#attribute(uuid);
      return;
    }
    if (this.#quiet) {
      // The silent notes' own result: no model call, no turn, nothing to show.
      if (message.type === "result") {
        this.#quiet = false;
        this.#silentResult(message);
        return;
      }
      // A model call after all: a real turn starts. Other system messages
      // (status, task events) leave the notes silent.
      if (["assistant", "stream_event", "user"].includes(message.type)) this.#quiet = false;
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

  /**
   * The silent notes' result. A success records them: they are in the
   * transcript and wait for the next turn, for at most the abort grace;
   * then (the input after them never started a turn) they are marked
   * persisted and shown by themselves, and the inputs pushed with them no
   * longer keep the session busy (as an input a turn did not name). A
   * failed one proves nothing: its notes are taken again and pushed anew.
   */
  #silentResult(result: Record<string, unknown>): void {
    const ids = resultUuids(result);
    for (const uuid of ids) this.#attribute(uuid);
    const named = this.#nextTurn.filter((d) => ids.includes(d.inputId) && this.#silent.has(d.inputId));
    if (result.subtype === "success" && result.is_error !== true) {
      for (const delivery of named) delivery.recorded = true;
      this.#boundSilent();
      return;
    }
    this.#log.warn("Claude Code failed to record silent notes; pushing them again", {
      session_id: this.sessionId, error: String(result.subtype ?? "").slice(0, 128),
    });
    // Again, silent and at the priority they had: never NOW, which would
    // stop the turn of the input they go with if it already started. A
    // running turn absorbs them; otherwise they are recorded after it.
    this.#nextTurn = this.#nextTurn.filter((d) => !named.includes(d));
    const gen = this.#gen;
    for (const delivery of named) {
      this.#announced.delete(delivery.inputId);
      if (gen) {
        this.#deliveries.set(delivery.inputId, delivery);
        const { priority: _promoted, ...again } = delivery.message;
        gen.prompt.push(again);
      } else {
        this.#pushed.delete(delivery.inputId);
        this.#silent.delete(delivery.inputId);
        this.#notes.unshift(...delivery.notes);
      }
    }
  }

  /**
   * Bounds the wait for the turn that is to read recorded silent notes.
   * Not while an input pushed with them is announced by the CLI: its turn
   * is coming, however long its UserPromptSubmit hooks take. Otherwise
   * (the input never reached the CLI), after the abort grace the notes are
   * marked persisted and shown by themselves, and the inputs pushed with
   * them no longer keep the session busy (as an input a turn did not name).
   */
  #boundSilent(): void {
    if (this.#silentTimer) clearTimeout(this.#silentTimer);
    this.#silentTimer = undefined;
    // Checked when the bound runs out, so an input announced meanwhile counts too.
    const coming = () => [...this.#nextTurn, ...this.#deliveries.values()]
      .some((d) => !this.#silent.has(d.inputId) && this.#announced.has(d.inputId));
    this.#silentTimer = setTimeout(() => {
      this.#silentTimer = undefined;
      if (this.#turn || this.#ending || coming() || !this.#nextTurn.some((d) => d.recorded)) return;
      this.#log.warn("no turn followed silent notes; marking them delivered", { session_id: this.sessionId });
      for (const delivery of this.#nextTurn.filter((d) => !d.recorded)) this.#deliveries.set(delivery.inputId, delivery);
      this.#nextTurn = this.#nextTurn.filter((d) => d.recorded);
      for (const delivery of this.#deliveries.values()) delivery.unnamed = true;
      void this.#settleRecorded().then(() => this.#armIdle(), (error: unknown) => this.#fatal(error));
    }, this.#abortGraceMs);
    this.#silentTimer.unref?.();
  }

  /**
   * Marks the silent notes the CLI recorded persisted and publishes their
   * view events, outside a turn: no turn will take them (the one after
   * them never came, or the query is closing). Not while the transcript
   * is broken: a repair marks them (#mirrorError).
   */
  async #settleRecorded(): Promise<void> {
    if (this.#silentTimer) clearTimeout(this.#silentTimer);
    this.#silentTimer = undefined;
    const recorded = this.#nextTurn.filter((d) => d.recorded);
    if (!recorded.length || this.#broken || this.#abort.signal.aborted) return;
    this.#nextTurn = this.#nextTurn.filter((d) => !d.recorded);
    const marks = create(ReportTurn_PersistedSchema);
    const lostTasks: string[] = [];
    for (const delivery of recorded) {
      this.#markDelivered(delivery, marks, lostTasks);
      for (const event of delivery.events) this.#publisher.add(event.kind, event.payload);
    }
    try {
      await this.ops.call({ case: "patchState", value: create(PatchSessionStateSchema, { persisted: marks }) });
      await this.#persisted(marks, lostTasks);
    } catch (error) {
      // A lost lease: the transcript holds them, which the lease-loss rule reads.
      if (!isLeaseAbort(error) && !(error instanceof FencedError)) throw error;
    }
  }

  /** Adds what delivery carried to a persisted set. */
  #markDelivered(delivery: Delivery, marks: ReportTurn_Persisted, lostTasks: string[]): void {
    marks.inputIds.push(delivery.inputId);
    for (const note of delivery.notes) {
      if (note.gap) marks.gapNoticeTurnNo = note.gap;
      if (note.task) lostTasks.push(note.task.taskId);
    }
  }

  /**
   * A system/init that only records silent notes (every input it names was
   * pushed with shouldQuery false): no turn starts; its result is dropped.
   * The notes wait in #nextTurn for the turn of the input pushed after
   * them, which credits them and publishes their view events first.
   */
  #silentInit(message: Record<string, unknown>): boolean {
    const uuids = Array.isArray(message.user_message_uuids) ? message.user_message_uuids : [];
    if (!uuids.length || !uuids.every((uuid) => typeof uuid === "string" && this.#silent.has(uuid))) return false;
    this.#quiet = true;
    return true;
  }

  async #startTurn(claimed: readonly string[] = []): Promise<void> {
    // A graceful end: the CLI started a turn after all; leaving the loop
    // ends the wait, and the query is closed at once.
    if (this.#exiting) throw new LeaseLost("the CLI is ending; no turn starts");
    // The SDK announces each input it takes (command_lifecycle "started")
    // before the system/init of the turn that takes it.
    // The init names the inputs it starts with: they are this turn's even
    // when their announcement was missed.
    for (const uuid of claimed) this.#attribute(uuid);
    const origin: TurnOriginName = this.#nextTurn.length ? "input" : "cli";
    // A CLI turn delivers the notices of tasks that ended while idle.
    this.#undelivered.clear();
    const turn = new Turn(++this.#localSeq, origin);
    this.#turn = turn;
    this.#quiet = false;
    if (this.#silentTimer) clearTimeout(this.#silentTimer);
    this.#silentTimer = undefined;
    this.#releaseWantedAt = 0;
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
    for (const uuid of resultUuids(result)) this.#attribute(uuid);
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
      if (!toolUseId || !tool || tool === QUESTION_TOOL || this.#handledDenials.has(toolUseId)) continue;
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
    this.#settling = true;
    try {
      await this.#settle(turn, outcome, usage);
    } finally {
      this.#settling = false;
    }
    if (this.#stopping) {
      this.#finish({ release: ReleaseLease_Reason.IDLE });
      return;
    }
    this.#armIdle();
  }

  async #settle(turn: Turn, outcome: TurnOutcome, usage: string): Promise<void> {
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
  }

  #marks(turn: Turn): ReportTurn_Persisted | undefined {
    if (!turn.inputIds.length && !turn.gapNotice) return undefined;
    return create(ReportTurn_PersistedSchema, { inputIds: turn.inputIds, gapNoticeTurnNo: turn.gapNotice });
  }

  /** After the control plane recorded the marks: forget them locally. */
  async #persisted(marks: ReportTurn_Persisted, lostTasks: string[]): Promise<void> {
    for (const id of marks.inputIds) {
      this.#pushed.delete(id);
      this.#silent.delete(id);
      this.#announced.delete(id);
    }
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
    if (!turn) {
      this.#keepSilentForRepair();
      return;
    }
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

  /**
   * mirror_error between turns: silent notes the CLI recorded (their
   * silent result came) were written to the transcript, whose batches the
   * repair resends. They are not pushed again: like a broken turn's items,
   * they are marked persisted once the repair succeeded, and shown now.
   */
  #keepSilentForRepair(): void {
    // Only those whose silent result confirmed them: an announced one may
    // not have been written, so it is pushed again (a duplicate uuid is
    // skipped by the store; a lost note would not be).
    const taken = this.#nextTurn.filter((d) => d.recorded);
    if (!taken.length) return;
    this.#nextTurn = this.#nextTurn.filter((d) => !taken.includes(d));
    this.#brokenItems ??= create(ReportTurn_PersistedSchema);
    for (const delivery of taken) {
      this.#markDelivered(delivery, this.#brokenItems, this.#brokenLostTasks);
      for (const event of delivery.events) this.#publisher.add(event.kind, event.payload);
    }
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
      this.#notes.push({
        kind: "lost_bgtask", text: undeliveredTaskText(taskId, notice.toolUseId, notice.status, notice.summary), workerOnly: true,
      });
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
    // A note followed by another input of the batch goes to the CLI with
    // shouldQuery false: it is recorded in the conversation, under its own
    // id, and read with the next input that queries (the CLI merges them
    // into one user message for the model, the note first), so the note
    // and that input run one turn. The control plane returns notes first.
    // A note keeps its place before a NOW input by taking its priority.
    const now = inputs.map((input) => input.priority === InputPriority.NOW);
    inputs.forEach((input, i) => {
      const silent = input.kind === InputKind.NOTE && i < inputs.length - 1;
      this.#push(gen, input, silent, silent && now.slice(i + 1).some(Boolean));
    });
  }

  #push(gen: Generation, input: SessionReply_Inputs_Input, silent = false, nowAfter = false): void {
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
    // A control-plane note keeps its category as given, empty included. A
    // ledger note goes to the CLI as an ordinary user message; its view
    // event still names its category, so the view shows it as a note.
    // The user's answers to a question are the user's message too: an
    // ordinary user message, its view event naming it.
    const isNote = input.kind === InputKind.NOTE;
    const answer = input.kind === InputKind.ANSWER;
    const ledger = isNote && ledgerNoteKinds.has(input.note);
    const synthetic = isNote && !ledger;
    const note: string | undefined = input.kind === InputKind.APPROVAL ? "approval_retry" : isNote ? input.note
      : answer ? QUESTION_ANSWER_NOTE : undefined;
    events.push({
      kind: Kind.USER,
      payload: userMessagePayload(this.#claudeSessionId, input.inputId, input.text, note, timestamp, note !== undefined && !ledger && !answer),
    });
    const content = notes.length
      ? [...notes.map((n) => ({ type: "text" as const, text: systemReminder(n.text) })), { type: "text" as const, text: input.text }]
      : input.text;
    const now = input.priority === InputPriority.NOW || nowAfter;
    const message: SDKUserMessage = {
      type: "user", message: { role: "user", content }, parent_tool_use_id: null,
      uuid: input.inputId as SDKUserMessage["uuid"],
      ...(synthetic ? { isSynthetic: true } : {}),
      ...(now ? { priority: "now" as const } : {}),
      ...(silent ? { shouldQuery: false } : {}),
    };
    this.#pushed.add(input.inputId);
    if (silent) this.#silent.add(input.inputId);
    this.#deliveries.set(input.inputId, { inputId: input.inputId, notes, events, pushedAtSeq: this.#localSeq, message });
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
    // The session is ending (its CLI may still run while it closes, or
    // during a graceful end, which only an ending session starts): no tool
    // runs any more.
    if (this.#ending) {
      return { hookSpecificOutput: { hookEventName: "PreToolUse", permissionDecision: "deny", permissionDecisionReason: "The session is ending." } };
    }
    if (!(await this.#consumeGrant(input.tool_name, input.tool_input, input.tool_use_id, signal))) return {};
    return {
      hookSpecificOutput: {
        hookEventName: "PreToolUse", permissionDecision: "allow", permissionDecisionReason: "The user approved this call once.",
      },
    };
  }

  /**
   * Consumes a grant for the tool use if the worker knows of one matching
   * it; true when the call may run. A tool use that consumed one already
   * stays allowed.
   */
  async #consumeGrant(tool: string, toolInput: unknown, toolUseId: string, signal: AbortSignal): Promise<boolean> {
    const subject = approvalSubject(tool, toolInput, this.#cwd);
    if (!subject) return false;
    const key = `${subject.tool}\n${subject.matchKey}`;
    // Allowed already, for the same tool and input (another hook may have
    // rewritten the input since).
    if (this.#granted.has(toolUseId)) return this.#granted.get(toolUseId) === key;
    if (this.#broken || !this.#grants.has(key)) return false;
    try {
      const reply = await this.ops.call({
        case: "consumeGrant",
        value: create(ConsumeGrantSchema, {
          tool: subject.tool, device: this.#device, matchKey: subject.matchKey, toolUseId,
        }),
      }, { budgetMs: this.deps.grantBudgetMs ?? GRANT_BUDGET_MS });
      if (!reply.allowed || signal.aborted) return false;
      this.#grants.delete(key);
      this.#granted.set(toolUseId, key);
      if (this.#granted.size > MAX_TRACKED) this.#granted.delete(this.#granted.keys().next().value!);
      return true;
    } catch (error) {
      if (error instanceof BrokenError) this.#broken = true;
      else if (error instanceof FencedError || error instanceof StoppedError) this.#fatal(error);
      else if (!isLeaseAbort(error)) this.#log.warn("ConsumeGrant failed", { session_id: this.sessionId, error: String(error).slice(0, 512) });
      return false;
    }
  }

  async #denied(message: Record<string, unknown>): Promise<void> {
    const toolUseId = typeof message.tool_use_id === "string" ? message.tool_use_id : "";
    const tool = typeof message.tool_name === "string" ? message.tool_name : "";
    // A question is never a tool approval: canUseTool asks it, or it was refused.
    if (!toolUseId || !tool || tool === QUESTION_TOOL || this.#handledDenials.has(toolUseId)) return;
    this.#trackDenial(toolUseId);
    // Explicit deny rules and hooks decided on purpose; only the permission
    // mode (auto's classifier, dontAsk, ...) is asked about.
    if (message.decision_reason_type === "rule" || message.decision_reason_type === "hook") return;
    const reason = typeof message.decision_reason === "string" ? message.decision_reason : undefined;
    await this.#requestApproval(toolUseId, tool, reason, typeof message.agent_id === "string" ? message.agent_id : undefined);
  }

  /**
   * The SDK's permission callback. Every call reaching it is denied, the
   * worker never waits for the user: AskUserQuestion on the main thread
   * becomes a question approval whose answers start the next turn; a
   * subagent's is refused; any other call (one the permission mode or an
   * ask rule asks about) gets the approval request #denied makes, tracked
   * so that a permission_denied for the same call does not ask again. A
   * call a grant allows (the user approved it) runs: an ask rule asks
   * even after the hook allowed it.
   *
   * A main-thread call is asked about only once the message loop has
   * published the assistant message carrying its tool_use (see
   * PublishedToolUses). A subagent's call does not wait: the CLI may never
   * hand its messages to the SDK (a background agent's between turns).
   */
  async #canUseTool(published: PublishedToolUses, tool: string, input: Record<string, unknown>, toolUseId: string, agentId: string,
    decisionReason: string | undefined, signal: AbortSignal): Promise<PermissionResult> {
    if (this.#ending) return { behavior: "deny", message: denials.ending };
    if (tool !== QUESTION_TOOL && await this.#consumeGrant(tool, input, toolUseId, signal)) {
      return { behavior: "allow", updatedInput: input };
    }
    // The turn's result lists this denial again (permission_denials): it
    // is handled here, once.
    const handled = this.#handledDenials.has(toolUseId);
    this.#trackDenial(toolUseId);
    const question = tool === QUESTION_TOOL;
    if (question && agentId) return { behavior: "deny", message: denials.subagentQuestion };
    try {
      if (!question) {
        this.#toolUses.remember(toolUseId, { tool, input, agentId });
        if (handled) return { behavior: "deny", message: denials.toolSent };
      }
      const waited = agentId ? "published" : await published.wait(toolUseId, signal);
      // Interrupted, or the query closed, before the call's message was published: nothing is asked.
      if (waited === "cancelled") return { behavior: "deny", message: question ? denials.questionFailed : denials.toolFailed };
      // Asked as before: the card may come before its call in the view.
      if (waited === "timed_out") {
        this.#log.warn("the assistant message of a call asked about was not published in time; asking anyway", {
          session_id: this.sessionId, tool: tool.slice(0, 256), tool_use_id: toolUseId.slice(0, 256),
        });
      }
      if (this.#ending) return { behavior: "deny", message: denials.ending };
      if (question) return { behavior: "deny", message: await this.#askQuestion(toolUseId, input) };
      const asked = await this.#requestApproval(toolUseId, tool, decisionReason, agentId);
      return { behavior: "deny", message: asked ? denials.toolSent : denials.toolFailed };
    } catch (error) {
      // As for a failure in the message loop: fenced, stopped or a worker failure.
      this.#fatal(error);
      return { behavior: "deny", message: question ? denials.questionFailed : denials.toolFailed };
    }
  }

  /** Asks the user the model's questions; returns what the model is told. */
  async #askQuestion(toolUseId: string, input: Record<string, unknown>): Promise<string> {
    const inputJson = JSON.stringify(input);
    if (Buffer.byteLength(inputJson) > QUESTION_INPUT_BYTES) {
      this.#log.warn("AskUserQuestion input too large; not asked", { session_id: this.sessionId, bytes: Buffer.byteLength(inputJson) });
      return denials.questionTooLarge;
    }
    if (this.#broken) return denials.questionFailed;
    const display = questionDisplay(input);
    try {
      // The control plane records APPROVAL_PENDING with the approval: the
      // call's tool_use goes to the view first.
      await this.#publisher.flush();
      await this.ops.call({
        case: "requestApproval",
        value: create(RequestApprovalSchema, { kind: ApprovalKind.QUESTION, toolUseId, tool: QUESTION_TOOL, display, inputJson }),
      });
      return denials.questionAsked;
    } catch (error) {
      if (error instanceof BrokenError) {
        this.#broken = true;
        return denials.questionFailed;
      }
      if (!(error instanceof RejectedError)) throw error;
      this.#log.warn("RequestApproval rejected", { session_id: this.sessionId, error: error.message.slice(0, 512) });
      this.#publisher.add(Kind.ERROR, errorPayload(`Asking the user the model's questions failed: ${error.message.slice(0, 512)}`));
      // A question the control plane found malformed may be asked again, corrected.
      return error.code === RejectCode.INVALID ? denials.questionInvalid(error.message.replace(/^INVALID: /, "")) : denials.questionFailed;
    }
  }

  /** Requests a tool approval; false when none was requested. */
  async #requestApproval(toolUseId: string, tool: string, reason: string | undefined, agentId?: string): Promise<boolean> {
    if (this.#broken) return false;
    const use = this.#toolUses.get(toolUseId);
    const subject: ApprovalSubject | undefined = use ? approvalSubject(tool, use.input, this.#cwd) : undefined;
    if (!subject) {
      this.#log.warn("denied tool use without a known input; no approval requested", { session_id: this.sessionId, tool: tool.slice(0, 256) });
      return false;
    }
    const why = approvalReason(reason);
    try {
      // The control plane records APPROVAL_PENDING with the approval (a
      // grant exists only once the user approves): what the CLI reported of
      // the call goes to the view first.
      await this.#publisher.flush();
      await this.ops.call({
        case: "requestApproval",
        value: create(RequestApprovalSchema, {
          toolUseId, agentId: agentId ?? use?.agentId ?? "", tool: subject.tool, device: this.#device,
          matchKey: subject.matchKey, display: subject.display, reason: why,
        }),
      });
      return true;
    } catch (error) {
      if (error instanceof BrokenError) {
        this.#broken = true;
        return false;
      }
      if (!(error instanceof RejectedError)) throw error;
      this.#log.warn("RequestApproval rejected", { session_id: this.sessionId, error: error.message.slice(0, 512) });
      this.#publisher.add(Kind.ERROR, errorPayload(`Asking for approval of ${tool} failed: ${error.message.slice(0, 512)}`));
      return false;
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
        // The SDK's task_type (local_agent, local_bash, ...); the CLI's own name for a task without one.
        kind: clipUtf8(typeof message.task_type === "string" && message.task_type ? message.task_type : "unknown", 128),
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
    this.#retryRelease();
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
      this.#idleRelease(false);
    }, delay);
  }

  /** The idle release, by the idle timer or on the control plane's request. */
  #idleRelease(requested: boolean): void {
    this.#log.info("session idle; releasing", { session_id: this.sessionId, requested });
    this.#finish({ release: ReleaseLease_Reason.IDLE });
  }
}

/** The inputs a result names. */
function resultUuids(result: Record<string, unknown>): string[] {
  const all = [result.user_message_uuid, ...(Array.isArray(result.user_message_uuids) ? result.user_message_uuids : [])];
  return all.filter((uuid): uuid is string => typeof uuid === "string");
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

/** Resolves when promise settles or at deadline (performance.now() time), whichever comes first. */
async function within(promise: Promise<unknown>, deadline: number): Promise<void> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  const late = new Promise<void>((resolve) => { timer = setTimeout(resolve, Math.max(0, deadline - performance.now())); });
  try {
    await Promise.race([promise.then(() => {}, () => {}), late]);
  } finally {
    clearTimeout(timer);
  }
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
