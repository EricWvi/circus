import { randomBytes } from "node:crypto";
import { readFileSync } from "node:fs";
import { query } from "@anthropic-ai/claude-agent-sdk";
import { agentsDirectory, registryBase } from "./agents/install.js";
import { AgentManager, claudeCode } from "./agents/manager.js";
import { loadConfig } from "./config.js";
import { WorkerLink } from "./link/index.js";
import { createLogger } from "./log.js";
import { SessionPool } from "./pool.js";
import { CliProcesses, ensureStateDirectory, findExecGuard, stateDirectory } from "./process.js";
import { sdkVersion } from "./versions.js";

/** Graceful shutdown: turns end, leases are released, the link closes. */
const SHUTDOWN_MS = 15_000;
/** How long one ReportAgents may wait for its reply. */
const REPORT_TIMEOUT_MS = 30_000;

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
  // A crashed CLI's stderr tail is logged when its stderr ends: not later than the worker's exit.
  const flushExits = () => processes.flush();
  process.once("exit", flushExits);

  registryBase(); // Refuses an invalid CIRCUS_NPM_REGISTRY at start.
  // The manager reports through the link and asks the pool, both made below.
  const late: { link?: WorkerLink; pool?: SessionPool } = {};
  const agents = new AgentManager({
    dir: agentsDirectory(), agents: [claudeCode()], log,
    report: (statuses) => late.link!.reportAgents(statuses, AbortSignal.timeout(REPORT_TIMEOUT_MS)),
    busy: (kind) => late.pool?.runs(kind) ?? false,
  });
  await agents.start();

  const link = new WorkerLink({
    ...config, bootId: bootID(), clientVersion: version, log,
    // The account and auto-mode fields are not reported.
    attach: { maxSessions: config.maxSessions, sdkVersion: sdkVersion(), localOnly: true, agents: () => agents.forAttach() },
  });
  const pool = new SessionPool({
    link, maxSessions: config.maxSessions, log, processes,
    runner: { query, idleMs: config.idleMs, agentExecutable: (kind) => agents.executable(kind) },
    agentsWanted: (wanted) => agents.want(wanted),
    sessionsChanged: () => agents.sessionsChanged(),
  });
  late.link = link;
  late.pool = pool;
  link.onReady((outcomes) => {
    log.info("worker attached", { resumed: outcomes.length });
    agents.ready();
  });

  let stopping: Promise<void> | undefined;
  const stop = (signal: string) => {
    stopping ??= (async () => {
      log.info("worker shutting down", { signal });
      await agents.close();
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
    if (!stopping) {
      await agents.close();
      await pool.shutdown(SHUTDOWN_MS);
    }
    await link.close();
    processes.flush();
  }
}

await main().catch((error: unknown) => {
  createLogger("error").error("worker stopped", { error: String(error).slice(0, 1024) });
  process.exitCode = 1;
});
