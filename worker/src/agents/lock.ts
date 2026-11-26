import { execFileSync } from "node:child_process";
import { randomBytes } from "node:crypto";
import { readFileSync } from "node:fs";
import { mkdir, open, readFile, rename, stat, unlink, utimes } from "node:fs/promises";
import { hostname } from "node:os";
import { join } from "node:path";

// The agents directory's lock: held while an install or uninstall changes
// files there and while a starting worker removes interrupted installs, so
// two worker processes sharing a data directory never remove each other's
// temporary directories or installs.
//
// Node has no flock, so the lock is a file created with O_EXCL
// (<agents>/.lock) that records its holder: pid, host and the process's
// start time (processStart). The holder removes it when done and, while it
// holds it, sets the file's mtime to now every refreshMs (10 s).
//
// On this host, the holder process decides, never the mtime (a holder
// paused for a while, by SIGSTOP or a suspend, is still the holder). A lock
// naming this host is stale, and taken over, when:
// - it names this process's pid but this process does not hold it: a
//   previous run with the same pid (a container restarted) left it;
// - the process it names is not running;
// - a process with its pid runs but started at another time (the pid was
//   reused).
// A live process with the start time the lock records is never taken over,
// however old the mtime.
//
// For a lock naming another host (sharing the data directory), an empty or
// damaged file, or one without a start time, the mtime decides: older than
// staleMs (60 s), its holder stopped refreshing it (a holder killed in a
// container whose next run has another hostname). Hosts sharing a data
// directory need synchronised clocks for this.
//
// Taking over is serialized by a second O_EXCL file (<agents>/.lock.takeover):
// a taker holding it checks again that the lock is the same stale file
// (same inode) before it removes it, then competes for the lock like any
// other process. A takeover file is itself stale after staleMs.

/** A lock file as read: gone, or its inode, holder and staleness. */
type Inspected = "gone" | { ino: number; holder?: Holder; stale: boolean };

export class AgentsLockBusy extends Error {
  override name = "AgentsLockBusy";
}

interface Holder {
  pid: number;
  host: string;
  /** The process's start time (processStart); empty when unknown. */
  start: string;
}

export interface AgentsLockTiming {
  /** How often a holder refreshes the lock's mtime; 10 s. */
  refreshMs?: number;
  /** How old an mtime makes a lock stale; 60 s. */
  staleMs?: number;
  /** Tests: runs in a takeover after the takeover file is held, before the lock is checked again. */
  beforeTakeover?: () => Promise<void>;
  /** Tests: runs before a stale takeover file is moved aside. */
  beforeGuardClear?: () => Promise<void>;
}

const POLL_MS = 200;
const REFRESH_MS = 10_000;
const STALE_MS = 60_000;

/** The lock files this process holds, whatever AgentsLock holds them, and the holder each names. */
const held = new Map<string, Holder>();

function alive(pid: number): boolean {
  try {
    process.kill(pid, 0);
    return true;
  } catch (error) {
    return (error as NodeJS.ErrnoException).code === "EPERM";
  }
}

const sleep = (ms: number, signal?: AbortSignal) => new Promise<void>((done) => {
  const timer = setTimeout(done, ms);
  signal?.addEventListener("abort", () => {
    clearTimeout(timer);
    done();
  }, { once: true });
});

/**
 * When process pid started, comparable between reads and unique for the
 * pid over time; undefined when the process is not running or the time
 * cannot be read. Linux: the boot id and the start time in clock ticks
 * since boot (/proc/<pid>/stat, field 22), exact. macOS: `ps -o lstart=`,
 * to the second (a pid is not reused within a second). Elsewhere unknown.
 */
export function processStart(pid: number): string | undefined {
  try {
    if (process.platform === "linux") {
      const stat = readFileSync(`/proc/${pid}/stat`, "utf8");
      const ticks = stat.slice(stat.lastIndexOf(")") + 2).split(" ")[19];
      const boot = readFileSync("/proc/sys/kernel/random/boot_id", "utf8").trim();
      return ticks ? `${boot}:${ticks}` : undefined;
    }
    if (process.platform === "darwin") {
      const out = execFileSync("ps", ["-o", "lstart=", "-p", String(pid)], { encoding: "utf8", timeout: 5_000 }).trim();
      return out || undefined;
    }
  } catch {
    // Not running, or unreadable.
  }
  return undefined;
}

function parseHolder(text: string): Holder | undefined {
  try {
    const value = JSON.parse(text) as Partial<Holder>;
    if (typeof value?.pid === "number" && typeof value.host === "string") {
      return { pid: value.pid, host: value.host, start: typeof value.start === "string" ? value.start : "" };
    }
  } catch {
    // Being written by its holder, or damaged.
  }
  return undefined;
}

// No parameter properties: tests run this file in child processes with
// Node's type stripping.
export class AgentsLock {
  readonly dir: string;
  readonly pid: number;
  readonly host: string;
  readonly path: string;
  readonly #takeover: string;
  readonly #refreshMs: number;
  readonly #staleMs: number;
  readonly #beforeTakeover?: () => Promise<void>;
  readonly #beforeGuardClear?: () => Promise<void>;
  /** Serializes this instance's holders. */
  #local: Promise<void> = Promise.resolve();

  constructor(dir: string, pid = process.pid, host = hostname(), timing: AgentsLockTiming = {}) {
    this.dir = dir;
    this.pid = pid;
    this.host = host;
    this.path = join(dir, ".lock");
    this.#takeover = join(dir, ".lock.takeover");
    this.#refreshMs = timing.refreshMs ?? REFRESH_MS;
    this.#staleMs = timing.staleMs ?? STALE_MS;
    this.#beforeTakeover = timing.beforeTakeover;
    this.#beforeGuardClear = timing.beforeGuardClear;
  }

  /**
   * Runs fn holding the lock, waiting for another holder at most waitMs
   * (else AgentsLockBusy naming it). signal aborts the wait.
   */
  async run<T>(fn: () => Promise<T>, waitMs: number, signal?: AbortSignal): Promise<T> {
    const previous = this.#local;
    const mine = Promise.withResolvers<void>();
    this.#local = previous.then(() => mine.promise);
    try {
      await previous;
      const ino = await this.#acquire(waitMs, signal);
      const refresh = setInterval(() => {
        const now = new Date();
        void utimes(this.path, now, now).catch(() => {});
      }, this.#refreshMs);
      refresh.unref();
      try {
        return await fn();
      } finally {
        clearInterval(refresh);
        await this.#release(ino);
      }
    } finally {
      mine.resolve();
    }
  }

  /** Takes the lock; returns the lock file's inode. */
  async #acquire(waitMs: number, signal?: AbortSignal): Promise<number> {
    await mkdir(this.dir, { recursive: true, mode: 0o755 });
    const deadline = performance.now() + waitMs;
    const me: Holder = { pid: this.pid, host: this.host, start: processStart(this.pid) ?? "" };
    const record = JSON.stringify(me);
    for (;;) {
      signal?.throwIfAborted();
      let found: Inspected | undefined;
      if (held.has(this.path)) {
        // Another holder in this process (another instance): wait for it,
        // whatever the file shows meanwhile.
      } else {
        try {
          const file = await open(this.path, "wx", 0o600);
          try {
            await file.writeFile(record);
            await file.sync();
            const { ino } = await file.stat();
            held.set(this.path, me);
            return ino;
          } finally {
            await file.close();
          }
        } catch (error) {
          if ((error as NodeJS.ErrnoException).code !== "EEXIST") throw error;
        }
        found = await this.#inspect();
        if (found === "gone") continue;
        if (found.stale) {
          // A takeover blocked by another taker's guard (one left by a
          // crashed taker included) must not keep this acquirer past its
          // own deadline.
          if (await this.#takeOver(found.ino) || performance.now() < deadline) continue;
        }
      }
      if (performance.now() >= deadline) {
        const holder = found === undefined ? held.get(this.path) : found === "gone" ? undefined : found.holder;
        const who = holder ? `process ${holder.pid} on ${holder.host}` : "another process";
        throw new AgentsLockBusy(`the agents directory is locked by ${who} (${this.path}); is another worker running with the same data directory?`);
      }
      await sleep(POLL_MS, signal);
    }
  }

  /** The lock file as it is now, and whether it is stale. */
  async #inspect(): Promise<Inspected> {
    let info;
    let text;
    try {
      info = await stat(this.path);
      text = await readFile(this.path, "utf8");
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code === "ENOENT") return "gone";
      throw error;
    }
    const holder = parseHolder(text);
    const old = Date.now() - info.mtimeMs > this.#staleMs;
    if (!holder || holder.host !== this.host) return { ino: info.ino, holder, stale: old };
    // On this host the holder process decides. A lock naming this process
    // that it does not hold was left by an earlier run with the same pid
    // (held is checked by the caller).
    if (holder.pid === this.pid || !alive(holder.pid)) return { ino: info.ino, holder, stale: true };
    // Without a start time the process cannot be told from a reused pid.
    if (!holder.start) return { ino: info.ino, holder, stale: old };
    return { ino: info.ino, holder, stale: processStart(holder.pid) !== holder.start };
  }

  /**
   * Removes the stale lock with inode ino, serialized by the takeover file;
   * false when another taker's guard blocked it (it then waited a little).
   */
  async #takeOver(ino: number): Promise<boolean> {
    let guard;
    try {
      guard = await open(this.#takeover, "wx", 0o600);
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code !== "EEXIST") throw error;
      // Another taker; a guard left by a crashed one goes after staleMs.
      const info = await stat(this.#takeover).catch(() => undefined);
      if (info && Date.now() - info.mtimeMs > this.#staleMs) {
        await this.#beforeGuardClear?.();
        const aside = `${this.#takeover}.${randomBytes(6).toString("hex")}`;
        if (await rename(this.#takeover, aside).then(() => true, () => false)) {
          const moved = await stat(aside).catch(() => undefined);
          if (moved?.ino === info.ino) await unlink(aside).catch(() => {});
          else {
            await rename(aside, this.#takeover).catch(() => {}); // Someone's fresh guard: put back.
            return false;
          }
          return true;
        }
      }
      await sleep(POLL_MS / 4);
      return false;
    }
    try {
      await this.#beforeTakeover?.();
      // Still the same lock file, and still stale: remove it.
      const found = await this.#inspect();
      if (found !== "gone" && found.ino === ino && found.stale) await unlink(this.path).catch(() => {});
    } finally {
      await guard.close();
      await unlink(this.#takeover).catch(() => {});
    }
    return true;
  }

  async #release(ino: number): Promise<void> {
    held.delete(this.path);
    // Only the file this holder created: a holder that stopped refreshing
    // for longer than staleMs may have been taken over.
    const info = await stat(this.path).catch(() => undefined);
    if (info?.ino === ino) await unlink(this.path).catch(() => {});
  }
}
