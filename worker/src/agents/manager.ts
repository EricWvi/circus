import { create } from "@bufbuild/protobuf";
import { join } from "node:path";
import type { Logger } from "../log.js";
import { CLAUDE_CODE_ARTIFACTS, CLAUDE_CODE_VERSION } from "./claude-code.generated.js";
import { LinkLostError } from "../link/index.js";
import { install, installedVersions, platformKey, registryBase, removeTemporary, uninstall, type InstallLimits } from "./install.js";
import { AgentsLock, type AgentsLockTiming } from "./lock.js";
import { AgentKind, type AgentStatus, AgentStatusSchema, AgentStatus_State } from "@circus/proto/circus/v1/agent_pb";
import type { WorkerEvent_AgentsWanted as AgentsWanted } from "@circus/proto/circus/v1/worker_link_pb";

// The agents this worker build supports, installed and uninstalled as the
// control plane's AgentsWanted says (proto/circus/v1/worker_link.proto):
//
// - wanted at this build's version and not INSTALLED: install it, replacing
//   an OUTDATED one; a FAILED install only when the wish's attempt is
//   greater than the attempt the failure belongs to (the user asked again);
// - wanted at another version (the worker was upgraded since the user
//   asked): change nothing;
// - not wanted and on disk: uninstall it once no session the worker holds
//   runs it.
//
// Each agent runs one operation at a time; a wish that arrives meanwhile is
// applied when it ends (a wish that no longer wants a running install
// aborts it). A FAILED install is never retried by the worker itself (not
// after a reconnect, not on a timer): only a wish whose attempt shows a new
// install request of the user. A failed uninstall is retried with backoff. Installs and uninstalls hold the
// agents directory's lock (lock.ts).
//
// Every change is reported (ReportAgents), one report at a time, always
// the latest state; while installing, the progress at most once a second.
// A report that fails while the link is up (timeout, refusal, store
// unavailable) is sent again with backoff; one that fails because the link
// is down is not: the next Attach carries the state, and ready() reports
// what changed after it was built.

export interface AgentDefinition {
  kind: AgentKind;
  /** The agent's directory name under the agents directory. */
  name: string;
  /** The executable inside the package. */
  binary: string;
  /** The only version this build runs. */
  version: string;
  /** The tarball for this platform; undefined where the agent does not ship. */
  artifact?: { path: string; integrity: string };
}

/** Claude Code at the version the pinned Agent SDK bundles, for this platform. */
export function claudeCode(platform = platformKey()): AgentDefinition {
  const artifact = platform ? CLAUDE_CODE_ARTIFACTS[platform] : undefined;
  return {
    kind: AgentKind.CLAUDE_CODE, name: "claude-code", binary: "claude", version: CLAUDE_CODE_VERSION,
    artifact: artifact && { path: artifact.path, integrity: artifact.integrity },
  };
}

export interface AgentManagerOptions {
  /** The agents directory (agentsDirectory()). */
  dir: string;
  agents: AgentDefinition[];
  log: Logger;
  /** Sends ReportAgents; rejects when the link is not ready (the next Attach reports instead). */
  report: (agents: AgentStatus[]) => Promise<void>;
  /** Whether a session the worker holds runs this agent. */
  busy: (kind: AgentKind) => boolean;
  /** The environment: CIRCUS_NPM_REGISTRY and proxy variables; process.env by default. */
  env?: NodeJS.ProcessEnv;
  /** The progress report interval; one second. */
  progressMs?: number;
  /** Download bounds (DEFAULT_LIMITS by default) and extra CA certificates (tests). */
  limits?: Partial<InstallLimits>;
  ca?: string | Buffer;
  /** The first and the longest wait before a failed report or uninstall is retried; 1 s and 30 s (5 min for uninstalls). */
  retryMs?: { first: number; max: number; maxUninstall?: number };
  /** How long an install or uninstall waits for another process's lock; 60 s (5 s at start). */
  lockWaitMs?: number;
  /** The lock's refresh and staleness times (AgentsLockTiming); 10 s and 60 s. */
  lockTiming?: AgentsLockTiming;
}

const MAX_ERROR_BYTES = 1024;
const PROGRESS_MS = 1_000;
const RETRY_FIRST_MS = 1_000;
const RETRY_MAX_MS = 30_000;
const UNINSTALL_RETRY_MAX_MS = 5 * 60_000;
const LOCK_WAIT_MS = 60_000;
const START_LOCK_WAIT_MS = 5_000;

/** Cuts text to at most max UTF-8 bytes, at a character boundary. */
export function clipError(text: string, max = MAX_ERROR_BYTES): string {
  const bytes = Buffer.from(text, "utf8");
  if (bytes.length <= max) return text;
  let end = max;
  while (end > 0 && (bytes[end] & 0xc0) === 0x80) end--;
  return bytes.subarray(0, end).toString("utf8");
}

interface Status {
  state: AgentStatus_State;
  installedVersion: string;
  error: string;
  downloaded: number;
  total: number;
}

interface Agent {
  def: AgentDefinition;
  dir: string;
  status: Status;
  /** The latest wish: a version, null when not wanted, undefined before any AgentsWanted. */
  wish?: string | null;
  /** The latest wish's attempt (AgentsWanted.Wanted.attempt). */
  wishAttempt: bigint;
  /** The attempt of the wish under which the last install failed; undefined unless FAILED by an install. */
  failedAttempt?: bigint;
  /** The operation running, if any. */
  running?: Promise<void>;
  /** Aborts the running install or uninstall wait. */
  abort?: AbortController;
  /** A wish arrived while the operation ran. */
  pending?: boolean;
  /** The last uninstall failed: retried with backoff while not wanted. */
  uninstallFailed?: boolean;
  uninstallDelay?: number;
  uninstallTimer?: ReturnType<typeof setTimeout>;
}

export class AgentManager {
  readonly #opts: AgentManagerOptions;
  readonly #agents = new Map<AgentKind, Agent>();
  #sessionsChanged = Promise.withResolvers<void>();
  #reporting = false;
  #dirty = false;
  #attached = "";
  #lastProgress = 0;
  #closed = false;
  #reportDelay = 0;
  #reportTimer?: ReturnType<typeof setTimeout>;
  readonly #lock: AgentsLock;

  constructor(opts: AgentManagerOptions) {
    this.#opts = opts;
    this.#lock = new AgentsLock(opts.dir, undefined, undefined, opts.lockTiming);
    for (const def of opts.agents) {
      if (!def.artifact) continue; // Not shipped for this platform: not supported.
      this.#agents.set(def.kind, {
        def, dir: join(opts.dir, def.name),
        status: { state: AgentStatus_State.NOT_INSTALLED, installedVersion: "", error: "", downloaded: 0, total: 0 }, wishAttempt: 0n,
      });
    }
  }

  /** Removes interrupted installs and reads what is on disk. Call once before the link starts. */
  async start(): Promise<void> {
    // Another worker process holding the lock is installing: its temporary
    // directory is its own, so nothing is removed then.
    await this.#lock.run(async () => {
      for (const agent of this.#agents.values()) await removeTemporary(agent.dir);
    }, Math.min(this.#opts.lockWaitMs ?? START_LOCK_WAIT_MS, START_LOCK_WAIT_MS)).catch((error: unknown) => {
      this.#opts.log.warn("interrupted installs not removed", { error: String(error).slice(0, 512) });
    });
    for (const agent of this.#agents.values()) {
      const versions = installedVersions(agent.dir, agent.def.binary);
      const installed = versions.includes(agent.def.version) ? agent.def.version : versions.at(-1) ?? "";
      agent.status = {
        state: installed === "" ? AgentStatus_State.NOT_INSTALLED
          : installed === agent.def.version ? AgentStatus_State.INSTALLED : AgentStatus_State.OUTDATED,
        installedVersion: installed, error: "", downloaded: 0, total: 0,
      };
      this.#opts.log.info("agent state", { agent: agent.def.name, state: AgentStatus_State[agent.status.state], version: installed });
    }
  }

  /** The supported agents' states, as reported. */
  statuses(): AgentStatus[] {
    return [...this.#agents.values()].map(({ def, status }) => create(AgentStatusSchema, {
      agent: def.kind, state: status.state, installedVersion: status.installedVersion, targetVersion: def.version,
      error: status.error, downloadedBytes: BigInt(status.downloaded), totalBytes: BigInt(status.total),
    }));
  }

  /** The statuses for an Attach: remembered, so a change before the link is ready is reported then (ready). */
  forAttach(): AgentStatus[] {
    const statuses = this.statuses();
    this.#attached = this.#key();
    return statuses;
  }

  /** The link is ready after an Attach: reports what changed since the Attach was built. */
  ready(): void {
    if (this.#key() !== this.#attached) this.#report();
  }

  /** The agent's executable when it is INSTALLED at this build's version; undefined otherwise. */
  executable(kind: AgentKind): string | undefined {
    const agent = this.#agents.get(kind);
    if (!agent || agent.status.state !== AgentStatus_State.INSTALLED || agent.status.installedVersion !== agent.def.version) return undefined;
    return join(agent.dir, agent.def.version, agent.def.binary);
  }

  /** The sessions the worker holds changed: an uninstall waiting for them may go on. */
  sessionsChanged(): void {
    this.#sessionsChanged.resolve();
    this.#sessionsChanged = Promise.withResolvers<void>();
  }

  /**
   * Applies the control plane's wishes (sent after every Attach and when a
   * wish changes): an agent wanted at this build's version and not
   * INSTALLED is installed; a FAILED install only for a newer attempt.
   */
  want(wanted: AgentsWanted): void {
    if (this.#closed) return;
    const wishes = new Map<AgentKind, { version: string; attempt: bigint }>();
    for (const { agent, version, attempt } of wanted.agents) wishes.set(agent, { version, attempt });
    for (const agent of this.#agents.values()) {
      const entry = wishes.get(agent.def.kind);
      const wish = entry?.version ?? null;
      agent.wish = wish;
      agent.wishAttempt = entry?.attempt ?? 0n;
      if (agent.running) {
        // A running install the wish no longer asks for, or an uninstall
        // still waiting for sessions that is wanted again, stops; the wish
        // applies once the operation has ended.
        if (agent.status.state === AgentStatus_State.INSTALLING ? wish !== agent.def.version : wish !== null) {
          agent.abort?.abort(new Error("superseded"));
        }
        agent.pending = true;
        continue;
      }
      this.#reconcile(agent);
    }
  }

  /** Stops every operation and waits for it. */
  async close(): Promise<void> {
    this.#closed = true;
    clearTimeout(this.#reportTimer);
    for (const agent of this.#agents.values()) clearTimeout(agent.uninstallTimer);
    for (const agent of this.#agents.values()) agent.abort?.abort(new Error("worker stopping"));
    this.sessionsChanged();
    await Promise.allSettled([...this.#agents.values()].map((agent) => agent.running));
  }

  #reconcile(agent: Agent): void {
    if (agent.running || this.#closed || agent.wish === undefined) return;
    const { state } = agent.status;
    let operation: ((signal: AbortSignal) => Promise<void>) | undefined;
    if (agent.wish === agent.def.version) {
      // A failed install waits for the user's next request (a newer attempt).
      const failed = state === AgentStatus_State.FAILED && agent.failedAttempt !== undefined &&
        agent.wishAttempt <= agent.failedAttempt;
      if (state !== AgentStatus_State.INSTALLED && !failed) operation = (signal) => this.#install(agent, signal);
    } else if (agent.wish === null && (state === AgentStatus_State.INSTALLED || state === AgentStatus_State.OUTDATED ||
      state === AgentStatus_State.UNINSTALLING || (state === AgentStatus_State.FAILED && agent.uninstallFailed))) {
      operation = (signal) => this.#uninstall(agent, signal);
    }
    if (!operation) return;
    const abort = new AbortController();
    agent.abort = abort;
    agent.running = operation(abort.signal).catch((error: unknown) => {
      this.#opts.log.error("agent operation failed", { agent: agent.def.name, error: String(error).slice(0, 1024) });
    }).finally(() => {
      agent.running = undefined;
      agent.abort = undefined;
      if (agent.pending) {
        agent.pending = false;
        this.#reconcile(agent);
      }
    });
  }

  #set(agent: Agent, status: Partial<Status>): void {
    agent.status = { ...agent.status, ...status };
    this.#report();
  }

  async #install(agent: Agent, signal: AbortSignal): Promise<void> {
    const { def } = agent;
    const url = registryBase(this.#opts.env) + def.artifact!.path;
    this.#opts.log.info("installing agent", { agent: def.name, version: def.version, url });
    agent.uninstallFailed = false;
    agent.uninstallDelay = undefined;
    clearTimeout(agent.uninstallTimer);
    agent.failedAttempt = undefined;
    const attempt = agent.wishAttempt;
    this.#set(agent, { state: AgentStatus_State.INSTALLING, error: "", downloaded: 0, total: 0 });
    this.#lastProgress = performance.now();
    const progressMs = this.#opts.progressMs ?? PROGRESS_MS;
    try {
      await this.#lock.run(async () => {
        // Another worker process on this data directory may have installed
        // it since this one last looked: under the lock, its files are
        // complete, and an install here (or its failure) must not touch them.
        if (installedVersions(agent.dir, def.binary).includes(def.version)) {
          this.#opts.log.info("agent already installed by another process", { agent: def.name, version: def.version });
          return;
        }
        await install({
        dir: agent.dir, version: def.version, binary: def.binary, url, integrity: def.artifact!.integrity, signal,
        env: this.#opts.env, limits: this.#opts.limits, ca: this.#opts.ca,
        progress: (downloaded, total) => {
          agent.status = { ...agent.status, downloaded, total };
          const now = performance.now();
          if (now - this.#lastProgress >= progressMs) {
            this.#lastProgress = now;
            this.#report();
          }
        },
        }).catch(async (error: unknown) => {
          // FAILED: the previous files, if any, are removed too, under the same lock.
          if (!signal.aborted) await uninstall(agent.dir).catch(() => {});
          throw error;
        });
      }, this.#opts.lockWaitMs ?? LOCK_WAIT_MS, signal);
    } catch (error) {
      if (signal.aborted) {
        // Superseded or stopping: what is on disk stays as it was.
        const versions = installedVersions(agent.dir, def.binary);
        const installed = versions.includes(def.version) ? def.version : versions.at(-1) ?? "";
        this.#opts.log.info("agent install stopped", { agent: def.name, reason: String(signal.reason).slice(0, 256) });
        this.#set(agent, {
          state: installed === "" ? AgentStatus_State.NOT_INSTALLED : installed === def.version ? AgentStatus_State.INSTALLED : AgentStatus_State.OUTDATED,
          installedVersion: installed, downloaded: 0, total: 0,
        });
        return;
      }
      const message = clipError(error instanceof Error ? error.message : String(error));
      this.#opts.log.error("agent install failed", { agent: def.name, version: def.version, error: message });
      agent.failedAttempt = attempt;
      this.#set(agent, { state: AgentStatus_State.FAILED, installedVersion: "", error: message, downloaded: 0, total: 0 });
      return;
    }
    this.#opts.log.info("agent installed", { agent: def.name, version: def.version });
    this.#set(agent, { state: AgentStatus_State.INSTALLED, installedVersion: def.version, error: "", downloaded: 0, total: 0 });
  }

  async #uninstall(agent: Agent, signal: AbortSignal): Promise<void> {
    const { def } = agent;
    let logged = false;
    while (this.#opts.busy(def.kind)) {
      if (signal.aborted) return;
      if (!logged) this.#opts.log.info("agent uninstall waits for its sessions", { agent: def.name });
      logged = true;
      const aborted = new Promise<void>((done) => signal.addEventListener("abort", () => done(), { once: true }));
      await Promise.race([this.#sessionsChanged.promise, aborted]);
    }
    if (signal.aborted) return;
    // From here on no new session starts it (executable() is undefined).
    this.#set(agent, { state: AgentStatus_State.UNINSTALLING, error: "", downloaded: 0, total: 0 });
    this.#opts.log.info("uninstalling agent", { agent: def.name, version: agent.status.installedVersion });
    try {
      await this.#lock.run(() => uninstall(agent.dir), this.#opts.lockWaitMs ?? LOCK_WAIT_MS);
    } catch (error) {
      const message = clipError(error instanceof Error ? error.message : String(error));
      const retry = this.#opts.retryMs;
      const delay = Math.min(agent.uninstallDelay ?? retry?.first ?? RETRY_FIRST_MS,
        retry?.maxUninstall ?? UNINSTALL_RETRY_MAX_MS);
      agent.uninstallFailed = true;
      agent.uninstallDelay = delay * 2;
      this.#opts.log.error("agent uninstall failed; retrying", { agent: def.name, error: message, retry_in_ms: delay });
      this.#set(agent, { state: AgentStatus_State.FAILED, installedVersion: "", error: message });
      clearTimeout(agent.uninstallTimer);
      agent.uninstallTimer = setTimeout(() => {
        agent.uninstallTimer = undefined;
        if (agent.running) agent.pending = true;
        else this.#reconcile(agent);
      }, delay);
      return;
    }
    agent.uninstallFailed = false;
    agent.uninstallDelay = undefined;
    this.#opts.log.info("agent uninstalled", { agent: def.name });
    this.#set(agent, { state: AgentStatus_State.NOT_INSTALLED, installedVersion: "" });
  }

  #key(): string {
    return JSON.stringify([...this.#agents.values()].map(({ status }) => status));
  }

  /** One ReportAgents in flight; a change meanwhile sends the latest state after it. */
  #report(): void {
    if (this.#closed && this.#reporting) return;
    if (this.#reporting) {
      this.#dirty = true;
      return;
    }
    clearTimeout(this.#reportTimer);
    this.#reportTimer = undefined;
    this.#reporting = true;
    this.#dirty = false;
    void this.#opts.report(this.statuses()).then(() => {
      this.#reportDelay = 0;
    }, (error: unknown) => {
      if (error instanceof LinkLostError) {
        this.#opts.log.debug("agent report not sent; the next attach carries it", { error: String(error).slice(0, 256) });
        return;
      }
      // The link is up: the latest state is sent again after a backoff,
      // unless a change sends it sooner.
      const retry = this.#opts.retryMs;
      const delay = Math.min(this.#reportDelay || (retry?.first ?? RETRY_FIRST_MS), retry?.max ?? RETRY_MAX_MS);
      this.#reportDelay = delay * 2;
      this.#opts.log.warn("agent report failed; retrying", { error: String(error).slice(0, 512), retry_in_ms: delay });
      if (!this.#closed) {
        this.#reportTimer = setTimeout(() => {
          this.#reportTimer = undefined;
          this.#report();
        }, delay);
      }
    }).finally(() => {
      this.#reporting = false;
      if (this.#dirty) this.#report();
    });
  }
}
