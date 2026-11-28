import { randomBytes } from "node:crypto";
import { readFileSync } from "node:fs";
import { query } from "@anthropic-ai/claude-agent-sdk";
import { agentsDirectory, registryBase } from "./agents/install.js";
import { AgentManager, claudeCode } from "./agents/manager.js";
import { loadConfig } from "./config.js";
import { WorkerLink } from "./link/index.js";
import { exitStatus } from "./exit.js";
import { CREDENTIAL_GONE, removeRevoked, SERVICE_FLAG, type RunningCredential } from "./rejected.js";
import { clearStop, recordStop, stopReason } from "./stop.js";
import { createLogger } from "./log.js";
import { SessionPool } from "./pool.js";
import { CliProcesses, ensureStateDirectory, findExecGuard, stateDirectory } from "./process.js";
import { helloFeatures, Updater } from "./update.js";
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

async function main(running: { credential?: RunningCredential }, argv: readonly string[]): Promise<void> {
  clearStop();
  const config = loadConfig();
  running.credential = { principalId: config.principalId, token: config.token };
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

  // Only the installed service runs `circus update` (see helloFeatures).
  const features = helloFeatures(argv);
  const service = features.length > 0;
  const updater = new Updater({ log, report: (id, error, signal) => late.link!.updateFailed(id, error, signal) });
  const link = new WorkerLink({
    ...config, bootId: bootID(), clientVersion: version, log,
    features,
    // The account and auto-mode fields are not reported.
    attach: { maxSessions: config.maxSessions, sdkVersion: sdkVersion(), localOnly: true, agents: () => agents.forAttach() },
  });
  const pool = new SessionPool({
    link, maxSessions: config.maxSessions, log, processes,
    runner: { query, idleMs: config.idleMs, agentExecutable: (kind) => agents.executable(kind) },
    agentsWanted: (wanted) => agents.want(wanted),
    updateMachine: (update) => {
      if (service) updater.update(update);
      else log.warn("UpdateMachine ignored: the worker does not run as the installed service", { update_id: update.updateId });
    },
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

/**
 * Runs the worker until it stops and returns its exit status (see
 * exitStatus). The reason of an earlier terminal stop is cleared first; a
 * terminal stop records its reason (stop.ts) before returning. The control
 * plane's proof that it revoked the credential also removes the credential
 * file and, run as the installed service (argv holds SERVICE_FLAG), the
 * service (rejected.ts).
 */
export async function run(argv: readonly string[] = process.argv.slice(2)): Promise<number> {
  // The credential the worker runs with, as it read it at start.
  const running: { credential?: RunningCredential } = {};
  try {
    await main(running, argv);
    return 0;
  } catch (error: unknown) {
    const status = exitStatus(error);
    const reason = stopReason(error);
    const log = createLogger("error");
    if (reason) {
      // A stop file that cannot be written skips neither the cleanup below nor the exit status.
      try {
        recordStop(reason, error instanceof Error ? error.message : String(error));
      } catch (failure: unknown) {
        log.error("could not record the stop reason", { error: String(failure).slice(0, 1024) });
      }
    }
    log.error("worker stopped", { error: String(error).slice(0, 1024), terminal: reason !== undefined });
    const removal = removeRevoked(error, argv, running.credential);
    if (removal) {
      const outcome = removal.status === 0 ? "removed" : removal.status === 1 || removal.status === null ? "kept"
        : removal.status === CREDENTIAL_GONE ? "gone" : "failed";
      log.error("worker credential after the revocation", { service: argv.includes(SERVICE_FLAG), outcome, output: removal.output });
    }
    return status;
  }
}
