import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import { ApiError, type ErrorCode } from "@/api/errors"
import type { CircusClient, Worker } from "@/api/types"

import { createMockBackend } from "./backend"
import { addAgentsDemo, addQueueDemo, MOCK_AGENT_BYTES, removeAgents, SEED_OWNER_ID, seedState, type MockState } from "./state"

const timing = { tickMs: 100, chunkBytes: 40_000_000, uninstallMs: 100, unknownSizeTicks: 3 }

function client(state: MockState = seedState()): CircusClient {
  state.cookie = SEED_OWNER_ID
  return createMockBackend({ state, latency: () => 0, agentTiming: timing, sessionTiming: { stepMs: 10, chunkMs: 5 } })
    .client
}

async function settle(ms = 0) {
  await vi.advanceTimersByTimeAsync(ms)
}

async function rejection(promise: Promise<unknown>, code: ErrorCode) {
  const error = await promise.then(
    () => undefined,
    (e: unknown) => e,
  )
  expect(error).toBeInstanceOf(ApiError)
  expect((error as ApiError).code).toBe(code)
}

/** The worker's Claude Code as the list shows it now. */
async function claude(c: CircusClient, name: string) {
  const workers = await c.machines.listWorkers()
  return workers.find((w: Worker) => w.name === name)?.agents.find((a) => a.agent === "claude_code")
}

beforeEach(() => {
  vi.useFakeTimers()
})
afterEach(() => {
  vi.useRealTimers()
})

describe("the mock's agents", () => {
  it("are installed on every seeded worker, the offline one as it last reported", async () => {
    const workers = await settled(client().machines.listWorkers())
    expect(workers.map((w) => [w.name, w.agents.map((a) => [a.state, a.installedVersion, a.wanted])])).toEqual([
      ["cloud-runner", [["installed", "2.1.3", true]]],
      ["devbox", [["installed", "2.1.3", true]]],
      ["eric-mbp", [["installed", "2.1.3", true]]],
      ["eric-win", [["installed", "2.1.3", true]]],
    ])
  })

  it("download with progress when asked to install, then are installed", async () => {
    const state = seedState()
    addAgentsDemo(state)
    const c = client(state)
    const worker = await settled(c.machines.setWorkerAgent({ workerId: "worker-spare-box", agent: "claude_code", install: true }))
    expect(worker.agents[0]).toMatchObject({ state: "installing", wanted: true, wantedVersion: "2.1.3", downloadedBytes: 0, totalBytes: MOCK_AGENT_BYTES })
    await settle(100)
    expect(await settled(claude(c, "spare-box"))).toMatchObject({ state: "installing", downloadedBytes: 40_000_000 })
    await settle(100)
    expect(await settled(claude(c, "spare-box"))).toMatchObject({ state: "installed", installedVersion: "2.1.3", downloadedBytes: 0 })
  })

  it("go on with the seeded downloads, of known size and not", async () => {
    const state = seedState()
    addAgentsDemo(state)
    const c = client(state)
    await settle(100)
    expect(await settled(claude(c, "ci-runner-1"))).toMatchObject({ state: "installing", downloadedBytes: 71_200_000 })
    expect(await settled(claude(c, "ci-runner-2"))).toMatchObject({ state: "installing", downloadedBytes: 52_800_000, totalBytes: 0 })
    expect(await settled(claude(c, "old-laptop"))).toMatchObject({ state: "not_installed", installedVersion: "" })
    await settle(200)
    expect(await settled(claude(c, "ci-runner-1"))).toMatchObject({ state: "installed" })
    expect(await settled(claude(c, "ci-runner-2"))).toMatchObject({ state: "installed" })
  })

  it("update an outdated install and retry a failed one", async () => {
    const state = seedState()
    addAgentsDemo(state)
    const c = client(state)
    await settled(c.machines.setWorkerAgent({ workerId: "worker-build-mac", agent: "claude_code", install: true }))
    await settled(c.machines.setWorkerAgent({ workerId: "worker-lab-pi", agent: "claude_code", install: true }))
    expect(await settled(claude(c, "lab-pi"))).toMatchObject({ state: "installing", error: "" })
    await settle(200)
    expect(await settled(claude(c, "build-mac"))).toMatchObject({ state: "installed", installedVersion: "2.1.3" })
    expect(await settled(claude(c, "lab-pi"))).toMatchObject({ state: "installed" })
  })

  it("uninstall, and stop a download when the install is called off", async () => {
    const state = seedState()
    addAgentsDemo(state)
    const c = client(state)
    const worker = await settled(c.machines.setWorkerAgent({ workerId: "worker-build-mac", agent: "claude_code", install: false }))
    expect(worker.agents[0]).toMatchObject({ state: "uninstalling", installedVersion: "2.0.9", wanted: false })
    await settled(c.machines.setWorkerAgent({ workerId: "worker-ci-runner-1", agent: "claude_code", install: false }))
    expect(await settled(claude(c, "ci-runner-1"))).toMatchObject({ state: "not_installed", downloadedBytes: 0 })
    await settle(500)
    expect(await settled(claude(c, "build-mac"))).toMatchObject({ state: "not_installed", installedVersion: "" })
    expect(await settled(claude(c, "ci-runner-1"))).toMatchObject({ state: "not_installed" })
  })

  it("keep an offline worker's wish without changing its report", async () => {
    const state = seedState()
    addAgentsDemo(state)
    const c = client(state)
    const worker = await settled(c.machines.setWorkerAgent({ workerId: "worker-travel-mbp", agent: "claude_code", install: true }))
    expect(worker.agents[0]).toMatchObject({ state: "installed", wanted: true, wantedVersion: "2.1.3" })
    await settled(c.machines.setWorkerAgent({ workerId: "worker-nas", agent: "claude_code", install: false }))
    await settle(1_000)
    expect(await settled(claude(c, "nas"))).toMatchObject({ state: "not_installed", wanted: false, wantedVersion: "" })
  })

  it("refuse what the server refuses", async () => {
    const state = seedState()
    addAgentsDemo(state)
    // Its proxy-holder session holds a lease on devbox.
    addQueueDemo(state)
    state.workers.push({ ...state.workers[0], owner: "user-dev", workerId: "worker-other", name: "other" })
    const c = client(state)
    const set = (workerId: string, install: boolean, agent = "claude_code") =>
      c.machines.setWorkerAgent({ workerId, agent: agent as "claude_code", install })
    // Never reported: no target version to install.
    const fresh = rejection(set("worker-fresh-worker", true), "failed_precondition")
    // devbox holds a session's lease.
    const held = rejection(set("worker-devbox", false), "failed_precondition")
    const other = rejection(set("worker-other", true), "not_found")
    const unknown = rejection(set("worker-devbox", true, "codex"), "invalid_argument")
    // Connected and reported without it: its build doesn't support it.
    const unsupported = rejection(set("worker-legacy-runner", true), "invalid_argument")
    await settle()
    await Promise.all([fresh, held, other, unknown, unsupported])
    expect(await settled(claude(c, "devbox"))).toMatchObject({ state: "installed", wanted: true })
    // Uninstalling a worker that never reported only records the wish.
    const worker = await settled(set("worker-fresh-worker", false))
    expect(worker.agents[0]).toMatchObject({ state: "unknown", wanted: false })
  })

  it("hold sessions back until a worker that may run them has it installed", async () => {
    const state = seedState()
    addQueueDemo(state)
    const c = client(state)
    const reason = async () => (await settled(c.sessions.getSession({ sessionId: "queue-agent" }))).queueReason
    expect(await reason()).toBe("agent_not_installed")
    await settled(c.machines.setWorkerAgent({ workerId: "worker-gpu-box", agent: "claude_code", install: true }))
    expect(await reason()).toBe("agent_not_installed")
    await settle(200)
    expect(await reason()).toBe("unspecified")
  })
})

/** The promise's value, once the mock's zero-latency timers ran. */
async function settled<T>(promise: Promise<T>): Promise<T> {
  await settle()
  return promise
}

describe("the mock without agents", () => {
  it("has no worker with Claude Code", async () => {
    const state = seedState()
    removeAgents(state)
    const c = client(state)
    const workers = await settled(c.machines.listWorkers())
    expect(workers.flatMap((w) => w.agents.map((a) => [a.state, a.wanted]))).toEqual(
      Array(4).fill(["not_installed", false]),
    )
  })
})
