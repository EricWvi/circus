// The agents of the mock's workers: SetWorkerAgent and the installs and
// uninstalls the workers would do, simulated with timers in this tab's
// memory (like the workers themselves).

import { ApiError } from "@/api/errors"
import type { AgentKind, WorkerAgent } from "@/api/types"

import { MOCK_AGENT_BYTES, type MockState, type MockWorker } from "./state"

/** How the simulated installs pace themselves. */
export interface MockAgentTiming {
  /** Between two progress reports of a download. */
  tickMs: number
  /** Bytes downloaded per report. */
  chunkBytes: number
  /** How long an uninstall takes. */
  uninstallMs: number
  /** A download of unknown size ends after this many reports. */
  unknownSizeTicks: number
}

export const defaultAgentTiming: MockAgentTiming = {
  tickMs: 1_000,
  chunkBytes: 9_000_000,
  uninstallMs: 2_500,
  unknownSizeTicks: 8,
}

export function createMockAgents({
  state,
  now,
  timing,
  holdsSession,
  installed,
}: {
  state: MockState
  now: () => number
  timing: MockAgentTiming
  /** Whether the worker holds the lease of a session that runs the agent. */
  holdsSession: (worker: MockWorker, agent: AgentKind) => boolean
  /** An agent became installed somewhere: sessions waiting for it may run. */
  installed: () => void
}) {
  /** The running simulation of each worker's agent, by `<workerId>/<agent>`. */
  const timers = new Map<string, ReturnType<typeof setTimeout>>()
  const key = (w: MockWorker, agent: AgentKind) => `${w.workerId}/${agent}`

  function stop(w: MockWorker, agent: AgentKind) {
    clearTimeout(timers.get(key(w, agent)))
    timers.delete(key(w, agent))
  }

  function later(w: MockWorker, agent: AgentKind, ms: number, step: () => void) {
    stop(w, agent)
    timers.set(
      key(w, agent),
      setTimeout(() => {
        timers.delete(key(w, agent))
        step()
      }, ms),
    )
  }

  /** The worker's entry for the agent, added (as never reported) when it has none. */
  function entry(w: MockWorker, agent: AgentKind): WorkerAgent {
    let a = w.agents.find((x) => x.agent === agent)
    if (!a) {
      a = {
        agent,
        state: "unknown",
        installedVersion: "",
        targetVersion: "",
        error: "",
        downloadedBytes: 0,
        totalBytes: 0,
        wanted: false,
        wantedVersion: "",
        reportedAtUnixMs: 0,
      }
      w.agents.push(a)
    }
    return a
  }

  /** The worker reports a change of the agent. */
  function report(a: WorkerAgent, fields: Partial<WorkerAgent>) {
    Object.assign(a, { error: "", downloadedBytes: 0, totalBytes: 0, ...fields, reportedAtUnixMs: now() })
  }

  /** One step of a download; the install ends once it is complete. */
  function download(w: MockWorker, a: WorkerAgent, ticks: number) {
    later(w, a.agent, timing.tickMs, () => {
      const known = a.totalBytes > 0
      const downloaded = a.downloadedBytes + timing.chunkBytes
      if (known ? downloaded >= a.totalBytes : ticks + 1 >= timing.unknownSizeTicks) {
        report(a, { state: "installed", installedVersion: a.targetVersion })
        installed()
        return
      }
      report(a, { state: "installing", installedVersion: "", downloadedBytes: downloaded, totalBytes: a.totalBytes })
      download(w, a, ticks + 1)
    })
  }

  function uninstall(w: MockWorker, a: WorkerAgent) {
    report(a, { state: "uninstalling", installedVersion: a.installedVersion })
    later(w, a.agent, timing.uninstallMs, () => report(a, { state: "not_installed", installedVersion: "" }))
  }

  /** What the online worker does with its wish for the agent, as AgentsWanted makes it. */
  function reconcile(w: MockWorker, a: WorkerAgent) {
    if (!w.online) return
    if (a.wanted) {
      if (a.state === "installed" || a.state === "installing" || a.wantedVersion !== a.targetVersion) return
      stop(w, a.agent)
      report(a, { state: "installing", installedVersion: "", totalBytes: MOCK_AGENT_BYTES })
      download(w, a, 0)
    } else if (a.state === "installing") {
      stop(w, a.agent)
      report(a, { state: "not_installed", installedVersion: "" })
    } else if (a.state === "installed" || a.state === "outdated") {
      uninstall(w, a)
    }
  }

  // Seeded work under way goes on.
  for (const w of state.workers) {
    if (!w.online) continue
    for (const a of w.agents) {
      if (a.state === "installing") download(w, a, 0)
      if (a.state === "uninstalling") uninstall(w, a)
    }
  }

  return {
    /** SetWorkerAgent on one of the owner's workers, with the server's refusals. */
    set(owner: string, { workerId, agent, install }: { workerId: string; agent: AgentKind; install: boolean }) {
      const w = state.workers.find((x) => x.owner === owner && x.workerId === workerId)
      if (!w) throw new ApiError("not_found", "Worker not found.")
      if ((agent as string) !== "claude_code") throw new ApiError("invalid_argument", "Unknown agent.", "agent")
      const reported = w.agents.find((x) => x.agent === agent)
      // A worker that connected has reported every agent it supports.
      if (install && !reported && w.lastSeenUnixMs > 0) {
        throw new ApiError("invalid_argument", "The worker does not support this agent.", "agent")
      }
      if (install && (!reported || reported.targetVersion === "")) {
        throw new ApiError("failed_precondition", "The worker has not reported its agents yet.")
      }
      if (!install && holdsSession(w, agent)) {
        throw new ApiError("failed_precondition", "The worker holds a session that runs this agent.")
      }
      const a = entry(w, agent)
      a.wanted = install
      a.wantedVersion = install ? a.targetVersion : ""
      reconcile(w, a)
      return w
    },
  }
}
