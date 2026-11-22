import { execFile, spawn, type ChildProcess } from "node:child_process";
import {
  closeSync, constants, fsyncSync, lstatSync, mkdirSync, openSync, readFileSync, readdirSync, renameSync, rmSync, unlinkSync, writeSync,
} from "node:fs";
import { homedir, tmpdir } from "node:os";
import { basename, dirname, isAbsolute, join, relative, resolve } from "node:path";
import type { SpawnOptions, SpawnedProcess } from "@anthropic-ai/claude-agent-sdk";
import type { Logger } from "./log.js";

// Claude Code CLI processes of the worker's sessions.
//
// The worker launches each CLI through `circus internal exec-guard` when
// available. The guard supervises the CLI and its descendants and tears them
// down when the worker or CLI exits, or when a release requests it. On macOS
// the guard can clean up only the CLI's process group.
//
// The spawned process (guard, or CLI when launched directly) leads its own
// session and process group. Its pid and start time, the boot id and the
// owning worker's pid and start time are recorded outside HOME. On Linux the
// record lets a worker clean up leftovers in that session or process group
// once the guard is gone. It also identifies the SDK's temporary config
// directory of a resumed query, which holds a copy of the credentials.

const STDERR_TAIL_BYTES = 8 << 10;
/** How long an exited CLI's stderr may stay open before its tail is logged. */
const STDERR_DRAIN_MS = 1_000;
const GUARD_PROBE_MS = 5_000;
const EXIT_GRACE_MS = 5_000;
const KILL_WAIT_MS = 2_000;
// TestWorkerTeardownBudget checks this against Linux exec-guard's teardownLimit.
const GUARD_TEARDOWN_MS = 5_000;
const GUARD_EXIT_WAIT_MS = GUARD_TEARDOWN_MS + KILL_WAIT_MS;

/** The state directory, `${XDG_RUNTIME_DIR:-$TMPDIR}/circus-worker-<uid>`, never under HOME. */
export function stateDirectory(env: NodeJS.ProcessEnv = process.env, home = homedir()): string {
  const uid = process.getuid?.() ?? 0;
  const inside = (dir: string) => {
    const rel = relative(resolve(home), resolve(dir));
    return rel === "" || (!rel.startsWith("..") && !isAbsolute(rel));
  };
  const base = [env.XDG_RUNTIME_DIR, env.TMPDIR, tmpdir(), "/tmp"]
    .find((dir) => dir && isAbsolute(dir) && !inside(dir));
  if (!base) throw new Error("no runtime directory outside HOME for the worker state");
  return join(base, `circus-worker-${uid}`);
}

/** Creates the state directory 0700, refusing one that is not ours. */
export function ensureStateDirectory(dir: string): void {
  try {
    mkdirSync(dir, { mode: 0o700 });
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code !== "EEXIST") throw error;
  }
  const info = lstatSync(dir);
  const uid = process.getuid?.();
  if (!info.isDirectory() || (uid !== undefined && info.uid !== uid) || (info.mode & 0o077) !== 0) {
    throw new Error(`worker state directory must be a 0700 directory owned by this user: ${dir}`);
  }
}

/**
 * Finds the exec guard: CIRCUS_BIN, else `circus` on PATH, if it runs
 * `internal exec-guard` successfully (with a trivial command).
 */
export function findExecGuard(env: NodeJS.ProcessEnv = process.env): Promise<string | undefined> {
  const bin = env.CIRCUS_BIN || "circus";
  return new Promise((done) => {
    execFile(bin, ["internal", "exec-guard", "--parent", String(process.pid), "--", process.execPath, "-e", ""], {
      timeout: GUARD_PROBE_MS, env, maxBuffer: 64 << 10,
    }, (error) => done(error ? undefined : bin));
  });
}

interface Stat {
  pgrp: number;
  session: number;
  start: string;
}

/** Linux /proc/<pid>/stat fields: process group, session and start time. */
function readStat(pid: number): Stat | undefined {
  let text: string;
  try {
    text = readFileSync(`/proc/${pid}/stat`, "utf8");
  } catch {
    return undefined;
  }
  const fields = text.slice(text.lastIndexOf(")") + 2).split(" ");
  // fields[0] is field 3 (state): pgrp is field 5, session 6, starttime 22.
  return { pgrp: Number(fields[2]), session: Number(fields[3]), start: fields[19] };
}

interface CliRecord {
  sessionId: string;
  pid: number;
  /** Whether the recorded process is an exec guard. */
  guarded?: boolean;
  /** /proc start time in clock ticks; empty where unknown. */
  start: string;
  /** The kernel's boot id: start times count from that boot. */
  boot: string;
  /** The worker that launched the CLI. */
  worker: { pid: number; start: string };
  /** The SDK's temporary config directory of a resumed query. */
  resumeDir?: ResumeDir;
  /** Uptime in clock ticks when the recorded process exited, once it has. */
  exited?: string;
}

const RECORD_SUFFIX = ".cli.json";
/** Linux reports times in USER_HZ, 100 on every architecture the CLI ships for. */
const TICKS_PER_SECOND = 100;

function bootId(): string {
  try {
    return readFileSync("/proc/sys/kernel/random/boot_id", "utf8").trim();
  } catch {
    return "";
  }
}

function uptimeTicks(): string {
  try {
    return String(Math.ceil(Number(readFileSync("/proc/uptime", "utf8").split(" ")[0]) * TICKS_PER_SECOND));
  } catch {
    return "";
  }
}

const RESUME_NAME = /^claude-resume-[A-Za-z0-9-]+$/;

export interface ResumeDir {
  dir: string;
  /** Device and inode at spawn: removal requires the same directory. */
  dev: string;
  ino: string;
}

/**
 * The SDK's resume directory for this CLI, if it made one: a
 * `claude-resume-*` directory in the worker's own tmpdir, not the worker's
 * own CLAUDE_CONFIG_DIR (a worker started inside a resumed Claude Code
 * session inherits that session's), and created after the worker started.
 * Where the file system reports no birth time (Node gives 0), that last
 * check is skipped, not replaced by ctime (which moves whenever the CLI
 * rewrites a file in it); the two others and the inode recorded here, which
 * removal compares, remain.
 */
export function resumeDirectory(env: SpawnOptions["env"], own = process.env.CLAUDE_CONFIG_DIR, startedMs = performance.timeOrigin): ResumeDir | undefined {
  const dir = env.CLAUDE_CONFIG_DIR;
  if (!dir || !isAbsolute(dir) || !RESUME_NAME.test(basename(dir))) return undefined;
  if (own && resolve(own) === resolve(dir)) return undefined;
  if (resolve(dirname(dir)) !== resolve(tmpdir())) return undefined;
  try {
    const info = lstatSync(dir, { bigint: true });
    if (!info.isDirectory()) return undefined;
    const born = Number(info.birthtimeMs);
    if (born > 0 && born < startedMs) return undefined;
    return { dir, dev: String(info.dev), ino: String(info.ino) };
  } catch {
    return undefined;
  }
}

function pidAlive(pid: number): boolean {
  try {
    process.kill(pid, 0);
    return true;
  } catch (error) {
    return (error as NodeJS.ErrnoException).code === "EPERM";
  }
}

function killQuietly(pid: number, signal: NodeJS.Signals): void {
  try {
    process.kill(pid, signal);
  } catch {
    // Gone already.
  }
}

export interface CliProcessesPort {
  /** The spawnClaudeCodeProcess function for one session. */
  spawner(sessionId: string): (options: SpawnOptions) => SpawnedProcess;
  /** Waits for exit, requesting teardown after a grace period. */
  confirmExit(sessionId: string): Promise<void>;
  /** Kills what is left of the session's recorded CLI and forgets it. */
  cleanup(sessionId: string): void;
}

export class CliProcesses implements CliProcessesPort {
  #live = new Map<string, { child: ChildProcess; exited: Promise<void> }>();
  /** Exits whose stderr tail waits for stderr to end; flush() logs them at once. */
  #pendingReports = new Set<() => void>();
  readonly #boot: string;
  readonly #self: { pid: number; start: string };

  constructor(
    readonly dir: string,
    readonly guard: string | undefined,
    readonly log: Logger,
    readonly linux = process.platform === "linux",
    readonly graceMs = EXIT_GRACE_MS,
    /** The owning worker; tests stand in for another process. */
    self?: { pid: number; start: string },
  ) {
    this.#boot = linux ? bootId() : "";
    this.#self = self ?? { pid: process.pid, start: linux ? readStat(process.pid)?.start ?? "" : "" };
  }

  spawner(sessionId: string): (options: SpawnOptions) => SpawnedProcess {
    return (options) => {
      const child = this.spawn(sessionId, options);
      if (!child.stdin || !child.stdout) throw new Error("Claude Code process has no stdio pipes");
      return child as ChildProcess & SpawnedProcess;
    };
  }

  spawn(sessionId: string, options: SpawnOptions): ChildProcess {
    if (this.#live.has(sessionId)) throw new Error(`session ${sessionId} already has a CLI process`);
    const [command, args] = this.guard
      ? [this.guard, ["internal", "exec-guard", "--parent", String(process.pid), "--", options.command, ...options.args]]
      : [options.command, options.args];
    const child = spawn(command, args, {
      cwd: options.cwd, env: options.env, stdio: ["pipe", "pipe", "pipe"], detached: true,
      signal: options.signal, killSignal: "SIGTERM",
    });
    let tail = "";
    child.stderr?.setEncoding("utf8");
    child.stderr?.on("data", (chunk: string) => {
      tail = (tail + chunk).slice(-STDERR_TAIL_BYTES);
    });
    child.on("error", () => {});
    // "exit" can come before the last stderr chunk is read: the tail is
    // complete once stderr ends, which a descendant holding it open can delay.
    const stderrEnded = new Promise<void>((done) => {
      if (!child.stderr) return done();
      child.stderr.once("end", done);
      child.stderr.once("close", done);
      child.stderr.once("error", () => done());
    });
    const exited = new Promise<void>((done) => child.once("exit", (code, signal) => {
      done();
      let logged = false;
      const report = () => {
        if (logged) return;
        logged = true;
        clearTimeout(timer);
        this.#pendingReports.delete(report);
        if (code !== 0 && code !== null) {
          this.log.warn("Claude Code exited", { session_id: sessionId, code, stderr: tail.slice(-2048) });
        } else {
          this.log.debug("Claude Code exited", { session_id: sessionId, signal: signal ?? "" });
        }
      };
      const timer = setTimeout(report, STDERR_DRAIN_MS);
      timer.unref?.();
      this.#pendingReports.add(report);
      void stderrEnded.then(report);
    }));
    if (child.pid !== undefined) {
      const record: CliRecord = {
        sessionId, pid: child.pid, start: this.linux ? readStat(child.pid)?.start ?? "" : "", boot: this.#boot,
        ...(this.guard ? { guarded: true } : {}),
        worker: this.#self, resumeDir: resumeDirectory(options.env),
      };
      this.#record(record);
      void exited.then(() => {
        if (!this.linux) return;
        try {
          this.#record({ ...record, exited: uptimeTicks() });
        } catch {
          // The record stays without its exit time.
        }
      });
      this.#live.set(sessionId, { child, exited });
      void exited.then(() => {
        if (this.#live.get(sessionId)?.child === child) this.#live.delete(sessionId);
      });
    }
    return child;
  }

  /**
   * Logs every exited CLI whose stderr tail is still waiting for stderr to
   * end, with what has been read so far: the worker is exiting.
   */
  flush(): void {
    for (const report of [...this.#pendingReports]) report();
  }

  async confirmExit(sessionId: string): Promise<void> {
    const live = this.#live.get(sessionId);
    if (!live) return;
    const wait = (ms: number) => new Promise<boolean>((done) => {
      const timer = setTimeout(() => done(false), ms);
      void live.exited.then(() => {
        clearTimeout(timer);
        done(true);
      });
    });
    if (await wait(this.graceMs)) return;
    const pid = live.child.pid;
    if (pid === undefined) return;
    const fields = { session_id: sessionId, pid };
    if (this.guard) {
      this.log.warn("exec-guard did not exit; requesting process tree teardown", fields);
      killQuietly(pid, "SIGUSR1");
      if (await wait(GUARD_EXIT_WAIT_MS)) {
        this.log.info("exec-guard exited after SIGUSR1", fields);
        return;
      }
    }
    this.log.warn(this.guard ? "exec-guard did not exit; killing its process group"
      : "Claude Code did not exit; killing its process group", fields);
    killQuietly(-pid, "SIGKILL");
    if (!(await wait(KILL_WAIT_MS))) {
      this.log.error(this.guard ? "exec-guard survived SIGKILL" : "Claude Code survived SIGKILL", { session_id: sessionId });
    } else if (this.guard) {
      this.log.info("exec-guard exited after process-group SIGKILL", fields);
    }
  }

  /**
   * Cleans up this worker's records of CLIs that are not live: the
   * session's, a restarted query's predecessors, and any kept for a retry.
   */
  cleanup(_sessionId: string): void {
    for (const [name, record] of this.#records()) {
      const mine = record.worker.pid === this.#self.pid && record.worker.start === this.#self.start;
      if (mine && this.#live.get(record.sessionId)?.child.pid !== record.pid) this.#cleanupFile(join(this.dir, name), record);
    }
  }

  *#records(): Generator<[string, CliRecord]> {
    let names: string[];
    try {
      names = readdirSync(this.dir);
    } catch {
      return;
    }
    for (const name of names.slice(0, 4096)) {
      if (!name.endsWith(RECORD_SUFFIX)) continue;
      const record = this.#read(join(this.dir, name));
      if (record) yield [name, record];
    }
  }

  /** Kills leftovers of the recorded CLIs of workers no longer running (on worker start). */
  cleanupAll(): void {
    for (const [name, record] of this.#records()) {
      const { worker } = record;
      const mine = worker.pid === this.#self.pid && worker.start === this.#self.start;
      // Without start times (no /proc) a live pid counts as that worker running.
      const alive = worker.start === "" ? pidAlive(worker.pid)
        : record.boot === this.#boot && readStat(worker.pid)?.start === worker.start;
      if (mine || alive) continue; // Another live worker's CLI, or one of ours.
      this.#cleanupFile(join(this.dir, name), record);
    }
  }

  /** One record per CLI: a restarted query's CLI does not replace its predecessor's. */
  #path(sessionId: string, pid: number): string {
    return join(this.dir, `${sessionId}.${this.#self.pid}.${pid}${RECORD_SUFFIX}`);
  }

  #read(path: string): CliRecord | undefined {
    try {
      const record = JSON.parse(readFileSync(path, "utf8")) as CliRecord;
      return record && typeof record.pid === "number" && record.worker ? record : undefined;
    } catch {
      return undefined;
    }
  }

  #cleanupFile(path: string, record = this.#read(path)): void {
    if (record) {
      // After a reboot nothing of it runs any more, and its start times mean nothing.
      if (record.boot === this.#boot) {
        // A surviving guard is already watching its worker. Killing its group
        // here would interrupt teardown and could strand detached descendants.
        if (record.guarded && this.#running(record)) return;
        this.#killLeftovers(record);
        // Until the leader is gone (reaped), keep the record: the next cleanup retries.
        if (this.#running(record)) return;
      }
      this.#removeResumeDir(record);
    }
    try {
      unlinkSync(path);
    } catch {
      // Already gone.
    }
  }

  #removeResumeDir(record: CliRecord): void {
    const resume = record.resumeDir;
    const dir = typeof resume === "object" && resume ? resume.dir : undefined;
    if (!dir || !isAbsolute(dir) || !RESUME_NAME.test(basename(dir))) return;
    if (record.boot === this.#boot && this.#running(record)) return;
    try {
      const info = lstatSync(dir, { bigint: true });
      if (!info.isDirectory() || Number(info.uid) !== process.getuid?.()) return;
      // Only the directory recorded at spawn, not another one now at that path.
      if (String(info.dev) !== resume!.dev || String(info.ino) !== resume!.ino) return;
      rmSync(dir, { recursive: true, force: true, maxRetries: 2 });
      this.log.info("removed a leftover Claude Code resume directory", { session_id: record.sessionId });
    } catch {
      // Gone already.
    }
  }

  #running(record: CliRecord): boolean {
    if (!this.linux) return record.start === "" && pidAlive(record.pid);
    return record.start !== "" && readStat(record.pid)?.start === record.start;
  }

  #record(record: CliRecord): void {
    const path = this.#path(record.sessionId, record.pid);
    const temp = `${path}.${process.pid}.tmp`;
    const fd = openSync(temp, constants.O_WRONLY | constants.O_CREAT | constants.O_TRUNC, 0o600);
    try {
      writeSync(fd, JSON.stringify(record));
      fsyncSync(fd);
    } finally {
      closeSync(fd);
    }
    renameSync(temp, path);
  }

  #killLeftovers(record: CliRecord): void {
    if (!this.linux || !record.start || !record.boot || !Number.isInteger(record.pid) || record.pid <= 1) return;
    const leader = readStat(record.pid);
    if (leader && leader.start !== record.start) return; // The pid was reused: not ours.
    const victims: number[] = [];
    if (leader) {
      killQuietly(-record.pid, "SIGKILL");
      victims.push(record.pid);
    }
    // Processes still in the recorded leader's session (other groups included), started
    // after it and, once it has exited, before its exit (later ones belong to
    // a session that reused its pid).
    let pids: string[] = [];
    try {
      pids = readdirSync("/proc").filter((name) => /^\d+$/.test(name));
    } catch {
      // No /proc.
    }
    for (const name of pids) {
      const pid = Number(name);
      const stat = readStat(pid);
      if (stat && (stat.session === record.pid || stat.pgrp === record.pid) && BigInt(stat.start) >= BigInt(record.start) &&
        (leader !== undefined || !record.exited || BigInt(stat.start) <= BigInt(record.exited))) {
        killQuietly(pid, "SIGKILL");
        victims.push(pid);
      }
    }
    if (victims.length) this.log.warn("killed leftover Claude Code processes", { session_id: record.sessionId, count: victims.length });
  }
}
