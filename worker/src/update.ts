import { spawn } from "node:child_process";
import type { UpdateMachine } from "@circus/proto/circus/v1/link_pb";
import type { Logger } from "./log.js";
import { SERVICE_FLAG } from "./rejected.js";

/** The Hello feature of a worker that runs UpdateMachine: only the installed service lists it. */
export const FEATURE_MACHINE_UPDATE = "machine_update";

/**
 * Hello.features for a worker started with argv: FEATURE_MACHINE_UPDATE
 * only for the installed service (SERVICE_FLAG), which the service manager
 * restarts with the new version after `circus update`; a foreground worker
 * would never come back updated.
 */
export function helloFeatures(argv: readonly string[]): string[] {
  return argv.includes(SERVICE_FLAG) ? [FEATURE_MACHINE_UPDATE] : [];
}

/** How long `circus update` may run: less than the control plane's 5-minute deadline. */
export const UPDATE_TIMEOUT_MS = 270_000;

/** UpdateFailed.error's bound, in UTF-8 bytes. */
export const MAX_UPDATE_ERROR_BYTES = 2048;

/** How much of the command's output is kept (its tail). */
const OUTPUT_TAIL_BYTES = 8 << 10;

/** How long the output may still arrive after the command exited. */
const EXIT_GRACE_MS = 1_000;

/** How long one UpdateFailed may wait for its reply. */
const REPORT_TIMEOUT_MS = 30_000;

export interface UpdaterOptions {
  log: Logger;
  /** Sends UpdateFailed; rejects when the link is not ready. */
  report: (updateId: string, error: string, signal: AbortSignal) => Promise<void>;
  env?: NodeJS.ProcessEnv;
  timeoutMs?: number;
}

/**
 * Runs `circus update` (CIRCUS_BIN, or circus on PATH) for UpdateMachine:
 * one child at a time, with the worker's environment, in its own process
 * group, stopped with that group after timeoutMs. A non-zero exit, a
 * failure to start or the deadline is reported with UpdateFailed; success
 * is not reported, since the service manager's restart and the reconnect
 * with the new version complete the update. Once the command has exited,
 * whatever it left in its process group is killed too, and the slot is
 * free for the next command at once: a failure report still under way
 * keeps its own attempt's id.
 *
 * The child never keeps the worker from exiting, and the worker does not
 * stop it at shutdown: the shutdown is normally the restart `circus
 * update` asked for as its last step, after the service manager already
 * took the request (systemctl --no-block, or launchd's helper in its own
 * session, outside the group), and the command only reports and exits
 * after it; the service manager's restart ends it with the service.
 */
export class Updater {
  #opts: UpdaterOptions;
  #running?: string;
  #done?: Promise<void>;

  constructor(opts: UpdaterOptions) {
    this.#opts = opts;
  }

  /** The update id of the running child, if any. */
  get running(): string | undefined {
    return this.#running;
  }

  /** Resolves once the current child (if any) has ended and its failure was reported. */
  settled(): Promise<void> {
    return this.#done ?? Promise.resolve();
  }

  update(command: UpdateMachine): void {
    const { log } = this.#opts;
    if (this.#running !== undefined) {
      log.warn("circus update already running; command ignored", { running: this.#running, update_id: command.updateId });
      return;
    }
    this.#running = command.updateId;
    log.info("running circus update", { update_id: command.updateId, version: command.version });
    this.#done = this.#run(command.updateId);
  }

  async #run(updateId: string): Promise<void> {
    let failure: string | undefined;
    try {
      failure = await this.#execute();
    } finally {
      // The child has settled: a new command may run while this report is sent.
      this.#running = undefined;
    }
    if (failure === undefined) {
      this.#opts.log.info("circus update finished; waiting for the service restart", { update_id: updateId });
      return;
    }
    this.#opts.log.error("circus update failed", { update_id: updateId, error: failure.slice(0, 1024) });
    try {
      await this.#opts.report(updateId, failure, AbortSignal.timeout(REPORT_TIMEOUT_MS));
    } catch (error) {
      this.#opts.log.warn("could not report the failed circus update", { update_id: updateId, error: String(error).slice(0, 512) });
    }
  }

  /** Runs the command; resolves with the failure to report, or undefined on success. */
  #execute(): Promise<string | undefined> {
    const env = this.#opts.env ?? process.env;
    const timeoutMs = this.#opts.timeoutMs ?? UPDATE_TIMEOUT_MS;
    const bin = env.CIRCUS_BIN || "circus";
    return new Promise((resolve) => {
      let output = Buffer.alloc(0);
      const keep = (chunk: Buffer) => {
        output = Buffer.concat([output, chunk]);
        if (output.length > OUTPUT_TAIL_BYTES) output = output.subarray(output.length - OUTPUT_TAIL_BYTES);
      };
      let child: ReturnType<typeof spawn>;
      try {
        child = spawn(bin, ["update"], { env, detached: true, stdio: ["ignore", "pipe", "pipe"] });
      } catch (error) {
        resolve(updateError(`could not run ${bin} update: ${String(error)}`, ""));
        return;
      }
      let timedOut = false;
      let settled = false;
      const timer = setTimeout(() => {
        timedOut = true;
        killGroup(child.pid);
      }, timeoutMs);
      timer.unref();
      child.unref();
      for (const stream of [child.stdout, child.stderr]) {
        stream?.on("data", keep);
        // Pipes keep the event loop alive: the child must not hold the worker.
        (stream as unknown as { unref?: () => void } | null)?.unref?.();
      }
      const finish = (failure: string | undefined) => {
        if (settled) return;
        settled = true;
        clearTimeout(timer);
        resolve(failure);
      };
      child.once("error", (error) => {
        killGroup(child.pid);
        finish(updateError(`could not run ${bin} update: ${error.message}`, output.toString("utf8")));
      });
      // The status comes with exit; the output once the pipes close, or
      // shortly after exit when something the command started holds them.
      const outcome = (code: number | null, signal: NodeJS.Signals | null) => {
        if (timedOut) {
          finish(updateError(`circus update was stopped after ${formatDuration(timeoutMs)}`, output.toString("utf8")));
        } else if (code !== 0) {
          finish(updateError(code === null ? `circus update was killed by ${signal}` : `circus update exited with status ${code}`,
            output.toString("utf8")));
        } else {
          finish(undefined);
        }
      };
      child.once("exit", (code, signal) => {
        // What the command left in its group goes with it, which also
        // closes the pipes such a process held.
        killGroup(child.pid);
        const grace = setTimeout(() => outcome(code, signal), EXIT_GRACE_MS);
        grace.unref();
        child.once("close", () => { clearTimeout(grace); outcome(code, signal); });
      });
    });
  }
}

/** Kills the process group led by pid (the child was started detached). */
function killGroup(pid: number | undefined): void {
  if (pid === undefined) return;
  try {
    process.kill(-pid, "SIGKILL");
  } catch {
    // Already gone.
  }
}

function formatDuration(ms: number): string {
  const seconds = Math.round(ms / 1000);
  const m = Math.floor(seconds / 60);
  const s = seconds % 60;
  if (m === 0) return `${s}s`;
  return s === 0 ? `${m}m` : `${m}m${s}s`;
}

/**
 * The text of UpdateFailed.error: summary, then the tail of the output
 * without terminal escapes and control characters (newlines and tabs
 * kept), at most MAX_UPDATE_ERROR_BYTES of UTF-8.
 */
export function updateError(summary: string, output: string): string {
  const head = cleanText(summary).slice(0, 512);
  const tail = cleanText(output).trim();
  if (tail === "") return head;
  const room = MAX_UPDATE_ERROR_BYTES - Buffer.byteLength(head) - 1;
  return `${head}\n${utf8Tail(tail, room)}`;
}

/** Strips ANSI escape sequences and control characters other than newline and tab; CR becomes a newline. */
export function cleanText(text: string): string {
  return text
    // CSI, OSC and other ESC sequences.
    // eslint-disable-next-line no-control-regex
    .replace(/\x1b\[[0-?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)?|\x1b[@-_]/g, "")
    .replace(/\r\n?/g, "\n")
    // eslint-disable-next-line no-control-regex
    .replace(/[\x00-\x08\x0b-\x1f\x7f-\x9f]/g, "")
    .toWellFormed();
}

/** The longest suffix of text within bytes of UTF-8, starting at a character boundary. */
function utf8Tail(text: string, bytes: number): string {
  if (bytes <= 0) return "";
  const encoded = Buffer.from(text, "utf8");
  if (encoded.length <= bytes) return text;
  let start = encoded.length - bytes;
  // Skip continuation bytes so the tail starts at a character.
  while (start < encoded.length && (encoded[start] & 0xc0) === 0x80) start++;
  return encoded.subarray(start).toString("utf8");
}
