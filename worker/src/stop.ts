import { mkdirSync, renameSync, rmSync, writeFileSync } from "node:fs";
import { dirname, isAbsolute, join } from "node:path";
import { dataDirectory } from "./agents/install.js";
import { ConfigError } from "./config.js";
import { TerminalLinkError } from "./wire/connection.js";

/**
 * Why the worker stopped for good, for `circus worker status`: the file
 * CIRCUS_WORKER_STOP_FILE names (`circus worker install` records it in the
 * service), else <data>/worker-state/stop.json. The worker removes it when
 * it starts and writes it before a terminal exit, and the package's
 * launcher writes it (reason "node") when Node.js is missing or too old.
 */
export function stopFile(env: NodeJS.ProcessEnv = process.env): string {
  const recorded = env.CIRCUS_WORKER_STOP_FILE;
  if (recorded && isAbsolute(recorded)) return recorded;
  return join(dataDirectory(env), "worker-state", "stop.json");
}

export type StopReason = "credential-refused" | "incompatible" | "credential" | "config";

/** The reason of a terminal stop, or undefined for an error that is retried. */
export function stopReason(error: unknown): StopReason | undefined {
  if (error instanceof TerminalLinkError) return error.reason === "incompatible" ? "incompatible" : "credential-refused";
  if (error instanceof ConfigError) return error.kind;
  return undefined;
}

/**
 * Writes the reason of a terminal stop, atomically and readable by this
 * user only. It creates the file's own directory (0700) when missing, never
 * one above it, such as $CIRCUS_HOME. A failure to write is ignored.
 */
export function recordStop(reason: StopReason, message: string, env: NodeJS.ProcessEnv = process.env): void {
  const file = stopFile(env);
  const temporary = `${file}.${process.pid}`;
  try {
    try {
      mkdirSync(dirname(file), { mode: 0o700 });
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code !== "EEXIST") throw error;
    }
    writeFileSync(temporary, JSON.stringify({ reason, message: message.slice(0, 1024) }) + "\n", { mode: 0o600, flag: "w" });
    renameSync(temporary, file);
  } catch {
    // The exit status and the log still say it.
    rmSync(temporary, { force: true });
  }
}

/** Removes the reason of an earlier terminal stop. */
export function clearStop(env: NodeJS.ProcessEnv = process.env): void {
  try {
    rmSync(stopFile(env), { force: true });
  } catch {
    // Nothing to remove.
  }
}
