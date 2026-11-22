import { randomBytes } from "node:crypto";
import { readFileSync } from "node:fs";
import { query } from "@anthropic-ai/claude-agent-sdk";
import { loadConfig } from "./config.js";
import { WorkerLink } from "./link/index.js";
import { createLogger } from "./log.js";
import { SessionPool } from "./pool.js";
import { CliProcesses, ensureStateDirectory, findExecGuard, stateDirectory } from "./process.js";
import { bundledClaudeVersion, sdkVersion } from "./versions.js";

/** Graceful shutdown: turns end, leases are released, the link closes. */
const SHUTDOWN_MS = 15_000;

function bootID(): string {
  const bytes = randomBytes(16);
  bytes.writeUIntBE(Date.now(), 0, 6);
  bytes[6] = (bytes[6] & 0x0f) | 0x70;
  bytes[8] = (bytes[8] & 0x3f) | 0x80;
  const hex = bytes.toString("hex");
  return [hex.slice(0, 8), hex.slice(8, 12), hex.slice(12, 16), hex.slice(16, 20), hex.slice(20)].join("-");
}

async function main(): Promise<void> {
  const config = loadConfig();
  const log = createLogger(config.logLevel);
  const { version } = JSON.parse(readFileSync(new URL("../package.json", import.meta.url), "utf8")) as { version: string };

  const dir = stateDirectory();
  ensureStateDirectory(dir);
  const guard = await findExecGuard();
  if (!guard) {
    log.warn("circus internal exec-guard is unavailable (set CIRCUS_BIN or put circus on PATH); " +
      "a Claude Code process may outlive the worker");
  }
  const processes = new CliProcesses(dir, guard, log);
  processes.cleanupAll();

  const link = new WorkerLink({
    ...config, bootId: bootID(), clientVersion: version, log,
    attach: { maxSessions: config.maxSessions, sdkVersion: sdkVersion(), claudeVersion: await bundledClaudeVersion(), localOnly: true },
  });
  const pool = new SessionPool({ link, maxSessions: config.maxSessions, log, processes, runner: { query, idleMs: config.idleMs } });
  link.onReady((outcomes) => log.info("worker attached", { resumed: outcomes.length }));

  let stopping: Promise<void> | undefined;
  const stop = (signal: string) => {
    stopping ??= (async () => {
      log.info("worker shutting down", { signal });
      await pool.shutdown(SHUTDOWN_MS);
      await link.close();
    })();
  };
  const onTerm = () => stop("SIGTERM");
  const onInt = () => stop("SIGINT");
  process.once("SIGTERM", onTerm);
  process.once("SIGINT", onInt);
  link.start();
  try {
    await pool.run();
    await stopping;
    await link.closed;
  } finally {
    process.removeListener("SIGTERM", onTerm);
    process.removeListener("SIGINT", onInt);
    if (!stopping) await pool.shutdown(SHUTDOWN_MS);
    await link.close();
  }
}

await main().catch((error: unknown) => {
  createLogger("error").error("worker stopped", { error: String(error).slice(0, 1024) });
  process.exitCode = 1;
});
