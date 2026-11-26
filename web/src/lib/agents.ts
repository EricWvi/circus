// The coding agents of a worker as the Devices page shows them: the
// worker's last report and the user's wish (Worker.agents), and what the
// user can do about them.

import type { AgentKind, AgentState, Worker, WorkerAgent } from "@/api/types"
import { currentLanguage } from "@/i18n/i18n"

/** The agents this client knows, in the order the UI lists them. */
export const AGENT_KINDS: readonly AgentKind[] = ["claude_code"]

/** The agent a session runs when it names none: the only one so far. */
export const DEFAULT_AGENT: AgentKind = "claude_code"

/**
 * The worker's entry for an agent; none when the worker never reported it
 * and the user never asked for it.
 */
export const agentOf = (worker: Pick<Worker, "agents">, agent: AgentKind): WorkerAgent | undefined =>
  worker.agents.find((a) => a.agent === agent)

/**
 * What the row knows besides the agent's entry: whether the worker is
 * online, and the report the latest successful SetWorkerAgent answered
 * with (its `reportedAtUnixMs`; undefined when there was none). While the
 * entry still carries that same report, the worker hasn't reported since
 * the request: it hasn't acted on it yet.
 */
export interface AgentContext {
  online: boolean
  answeredReportAt?: number
}

/** The worker hasn't reported the agent since the latest request was answered. */
const unanswered = (a: WorkerAgent, ctx: AgentContext) =>
  ctx.answeredReportAt !== undefined && ctx.answeredReportAt === a.reportedAtUnixMs

/**
 * What the user asked for that the worker hasn't started on yet: an
 * install (or update) the worker will do once it has the wish, or an
 * uninstall. A wish for a version the worker no longer runs (it was
 * upgraded since) is no wish: the worker does nothing about it, and the
 * user is offered the install (or update) again. A failed install that is
 * still wanted is installed again when the worker connects, or once it
 * acts on a retry; a state this client doesn't know carries no wish.
 */
export function pendingWish(a: WorkerAgent, ctx: AgentContext): "install" | "uninstall" | null {
  if (!a.wanted) return a.state === "installed" || a.state === "outdated" ? "uninstall" : null
  const current = a.wantedVersion !== "" && a.wantedVersion === a.targetVersion
  if (!current) return null
  if (a.state === "not_installed" || a.state === "outdated") return "install"
  if (a.state === "failed" && (!ctx.online || unanswered(a, ctx))) return "install"
  return null
}

/**
 * The state the row shows: a failure nobody wants installed any more (the
 * user gave up on it, or an uninstall failed) left nothing on disk, so it
 * shows as not installed.
 */
export const shownState = (a: WorkerAgent): AgentState =>
  a.state === "failed" && !a.wanted ? "not_installed" : a.state

/** The failure the row shows: that of a failed install still wanted. */
export const shownError = (a: WorkerAgent): string => (shownState(a) === "failed" ? a.error : "")

/** A request the agent's row can make. */
export type AgentAction =
  /** Install it (also over an outdated or failed one): SetWorkerAgent(install). */
  | "install"
  /** Install the worker's target version over an outdated one; the same request. */
  | "update"
  /** Install again after a failure; the same request. */
  | "retry"
  /** Take back an uninstall the worker hasn't done; the same request (a no-op on the worker). */
  | "keep"
  /** Uninstall it, after the user confirms. */
  | "uninstall"
  /** Withdraw an install the worker hasn't started: SetWorkerAgent(not install), nothing to confirm. */
  | "cancel"
  /** Give up on a failed install (nothing is on disk): SetWorkerAgent(not install), nothing to confirm. */
  | "dismiss"

/** Whether an action asks for the agent to be installed (else not to be). */
export const installs = (action: AgentAction) =>
  action === "install" || action === "update" || action === "retry" || action === "keep"

/** The actions the row offers, the main one first; none while the worker is busy with the agent. */
export function agentActions(a: WorkerAgent, ctx: AgentContext): AgentAction[] {
  const wish = pendingWish(a, ctx)
  switch (a.state) {
    case "unknown":
      return ["install"]
    case "not_installed":
      return wish === "install" ? ["cancel"] : ["install"]
    case "installed":
      return wish === "uninstall" ? ["keep"] : ["uninstall"]
    case "outdated":
      // Keeping an outdated install means installing the target version over it.
      if (wish === "uninstall") return ["update"]
      return wish === "install" ? ["uninstall"] : ["update", "uninstall"]
    case "failed":
      // Not wanted, it shows as not installed (shownState).
      if (!a.wanted) return ["install"]
      return wish === "install" ? ["dismiss"] : ["retry", "dismiss"]
    case "installing":
    case "uninstalling":
      return []
  }
}

/**
 * An online worker is working on one of its agents, or is about to: it
 * installs or uninstalls one, or hasn't acted on a wish yet. An offline
 * worker's report is old news, whatever it says. `answeredReportAt` is
 * AgentContext's, per agent.
 */
export function agentBusy(
  worker: Pick<Worker, "online" | "agents">,
  answeredReportAt: (agent: AgentKind) => number | undefined = () => undefined,
): boolean {
  if (!worker.online) return false
  return worker.agents.some(
    (a) =>
      a.state === "installing" ||
      a.state === "uninstalling" ||
      pendingWish(a, { online: true, answeredReportAt: answeredReportAt(a.agent) }) !== null,
  )
}

/** When the worker last reported its agents (it reports them all at once); 0 when never. */
export const lastReportedAt = (worker: Pick<Worker, "agents">) =>
  Math.max(0, ...worker.agents.map((a) => a.reportedAtUnixMs))

/** Download progress in whole percent; null when the total is unknown. */
export function installProgress(a: Pick<WorkerAgent, "downloadedBytes" | "totalBytes">): number | null {
  if (a.totalBytes <= 0) return null
  return Math.min(100, Math.floor((a.downloadedBytes / a.totalBytes) * 100))
}

const UNITS = ["kilobyte", "megabyte", "gigabyte"] as const

/**
 * A byte count for people, such as "12.5 MB", in the active language; in
 * kilobytes at the least ("0 kB"), as Intl has no short unit for bytes.
 */
export function formatBytes(bytes: number, language: string = currentLanguage()): string {
  let value = Math.max(0, bytes) / 1000
  let unit = 0
  while (value >= 1000 && unit < UNITS.length - 1) {
    value /= 1000
    unit++
  }
  return new Intl.NumberFormat(language, {
    style: "unit",
    unit: UNITS[unit],
    unitDisplay: "short",
    maximumFractionDigits: value >= 100 ? 0 : 1,
  }).format(value)
}

/** The worker has the agent installed at its target version, so sessions can run on it. */
export const runsAgent = (worker: Pick<Worker, "agents">, agent: AgentKind = DEFAULT_AGENT) =>
  agentOf(worker, agent)?.state === "installed"

/**
 * The worker a session in a location on `device` is pinned to: the one on
 * the device's machine (the session runs in local mode); none when no
 * worker shares that machine, and any worker may take the session.
 */
export const localWorker = <W extends Pick<Worker, "localDevices">>(workers: W[], device: string): W | undefined =>
  workers.find((w) => w.localDevices.includes(device))
