import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import { ApiError, type ErrorCode } from "@/api/errors"
import type { CircusClient } from "@/api/types"

import { createMockBackend } from "./backend"
import { MOCK_LATEST_RELEASE, olderRelease } from "./machine-updates"
import { parsePersisted, serialize } from "./persist"
import { SEED_OWNER_ID, seedState, type MockState } from "./state"

const timing = { completeMs: 1_000, deadlineMs: 300_000 }

function client(state: MockState = seedState()): CircusClient {
  state.cookie = SEED_OWNER_ID
  return createMockBackend({ state, latency: () => 0, updateTiming: timing }).client
}

async function rejection(promise: Promise<unknown>, code: ErrorCode) {
  const error = await promise.then(
    () => undefined,
    (e: unknown) => e,
  )
  expect(error).toBeInstanceOf(ApiError)
  expect((error as ApiError).code).toBe(code)
}

const deviceOf = async (c: CircusClient, name: string) => (await c.machines.listDevices()).find((d) => d.name === name)!
const workerOf = async (c: CircusClient, name: string) => (await c.machines.listWorkers()).find((w) => w.name === name)!

beforeEach(() => {
  // Calls settle after a zero-latency timer: let it fire on its own.
  vi.useFakeTimers({ shouldAdvanceTime: true })
})
afterEach(() => {
  vi.useRealTimers()
})

describe("the mock's machine updates", () => {
  it("compares releases", () => {
    expect(olderRelease("v0.1.0", "v0.2.0")).toBe(true)
    expect(olderRelease("v0.2.0", "v0.2.0")).toBe(false)
    expect(olderRelease("v0.10.0", "v0.2.0")).toBe(false)
    expect(olderRelease("dev", "v0.2.0")).toBe(false)
    expect(olderRelease("0.0.0", "v0.2.0")).toBe(false)
  })

  it("offer an update only on the card of a role that runs as the service, and run it on the whole machine", async () => {
    const c = client()
    expect((await deviceOf(c, "devbox")).updateVersion).toBe(MOCK_LATEST_RELEASE)
    // devbox's worker runs in the foreground.
    expect((await workerOf(c, "devbox")).updateVersion).toBe("")
    expect("foreground" in (await workerOf(c, "devbox"))).toBe(false)
    // Both roles offline: no update.
    expect((await deviceOf(c, "eric-win")).updateVersion).toBe("")
    await rejection(c.machines.updateMachine({ workerId: "worker-devbox" }), "failed_precondition")

    const update = await c.machines.updateMachine({ deviceId: "device-devbox" })
    expect(update).toMatchObject({ targetVersion: MOCK_LATEST_RELEASE, state: "running", error: "" })
    // Pressing again, on either card, returns the update under way.
    expect(await c.machines.updateMachine({ workerId: "worker-devbox" })).toEqual(update)
    for (const m of [await deviceOf(c, "devbox"), await workerOf(c, "devbox")]) {
      expect(m.updateVersion).toBe("")
      expect(m.update).toEqual(update)
    }

    await vi.advanceTimersByTimeAsync(timing.completeMs)
    for (const m of [await deviceOf(c, "devbox"), await workerOf(c, "devbox")]) {
      expect(m.version).toBe(MOCK_LATEST_RELEASE)
      expect(m.updateVersion).toBe("")
      expect(m.update).toBeUndefined()
    }
    await rejection(c.machines.updateMachine({ deviceId: "device-devbox" }), "failed_precondition")
  })

  it("fail an update that outlasts its deadline as timed out", async () => {
    const state = seedState()
    state.cookie = SEED_OWNER_ID
    const c = createMockBackend({ state, latency: () => 0, updateTiming: { completeMs: 10_000, deadlineMs: 4_000 } }).client
    await c.machines.updateMachine({ deviceId: "device-eric-mbp" })
    await vi.advanceTimersByTimeAsync(3_000)
    expect((await workerOf(c, "eric-mbp")).update?.state).toBe("running")
    await vi.advanceTimersByTimeAsync(1_000)
    for (const m of [await deviceOf(c, "eric-mbp"), await workerOf(c, "eric-mbp")]) {
      expect(m.update).toMatchObject({ state: "failed", failure: "timed_out", error: "" })
      expect(m.updateVersion).toBe(MOCK_LATEST_RELEASE)
      expect(m.version).toBe("v0.1.0")
    }
    // It never completes afterwards; a retry is a new attempt.
    await vi.advanceTimersByTimeAsync(10_000)
    expect((await deviceOf(c, "eric-mbp")).version).toBe("v0.1.0")
    expect((await c.machines.updateMachine({ workerId: "worker-eric-mbp" })).state).toBe("running")
  })

  it("refuse unknown identities and machines without an update", async () => {
    const c = client()
    await rejection(c.machines.updateMachine({ deviceId: "nope" }), "not_found")
    await rejection(c.machines.updateMachine({ workerId: "nope" }), "not_found")
    await rejection(c.machines.updateMachine({ deviceId: "device-eric-win" }), "failed_precondition")
  })

  it("seed each failure and a running update for the demo, and retry a failed one", async () => {
    const state = seedState()
    state.cookie = SEED_OWNER_ID
    const c = createMockBackend({ state, latency: () => 0, updateTiming: timing, updatesDemo: true }).client
    const mbp = await deviceOf(c, "eric-mbp")
    expect(mbp.update).toMatchObject({ state: "failed", failure: "command", targetVersion: MOCK_LATEST_RELEASE })
    expect(mbp.updateVersion).toBe(MOCK_LATEST_RELEASE)
    expect((await workerOf(c, "eric-mbp")).update?.state).toBe("failed")
    expect((await workerOf(c, "cloud-runner")).update).toMatchObject({ state: "failed", failure: "timed_out", error: "" })
    expect((await deviceOf(c, "eric-win")).update).toMatchObject({ state: "failed", failure: "unreachable" })
    expect((await deviceOf(c, "eric-win")).updateVersion).toBe("")
    expect((await deviceOf(c, "palace-builder")).update?.state).toBe("running")

    expect((await c.machines.updateMachine({ deviceId: "device-eric-mbp" })).state).toBe("running")
    await vi.advanceTimersByTimeAsync(timing.completeMs)
    expect((await workerOf(c, "eric-mbp")).version).toBe(MOCK_LATEST_RELEASE)
  })

  it("start again from the saved state after the demo's and the user's updates completed", async () => {
    const state = seedState()
    state.cookie = SEED_OWNER_ID
    const c = createMockBackend({ state, latency: () => 0, updateTiming: timing, updatesDemo: true }).client
    await c.machines.updateMachine({ deviceId: "device-devbox" })
    // palace-builder's demo update takes 90 s.
    await vi.advanceTimersByTimeAsync(90_000)
    expect((await deviceOf(c, "palace-builder")).version).toBe(MOCK_LATEST_RELEASE)
    expect((await deviceOf(c, "devbox")).version).toBe(MOCK_LATEST_RELEASE)

    // A reload: the saved state, with those versions, seeds the demo again.
    const loaded = parsePersisted(serialize(state))!
    expect(loaded).not.toBeNull()
    loaded.cookie = SEED_OWNER_ID
    const again = createMockBackend({ state: loaded, latency: () => 0, updateTiming: timing, updatesDemo: true }).client
    const palace = await deviceOf(again, "palace-builder")
    expect(palace.update).toBeUndefined()
    expect(palace.updateVersion).toBe("")
    expect((await deviceOf(again, "devbox")).update).toBeUndefined()
    // The machines still behind keep their demo states.
    expect((await deviceOf(again, "eric-mbp")).update?.failure).toBe("command")
  })

  it("keep a machine updated after a reload although its worker is seeded again at the old version", async () => {
    const state = seedState()
    state.cookie = SEED_OWNER_ID
    const c = createMockBackend({ state, latency: () => 0, updateTiming: timing, updatesDemo: true }).client
    await c.machines.updateMachine({ deviceId: "device-eric-mbp" })
    await vi.advanceTimersByTimeAsync(timing.completeMs)
    const loaded = parsePersisted(serialize(state))!
    loaded.cookie = SEED_OWNER_ID
    const again = createMockBackend({ state: loaded, latency: () => 0, updateTiming: timing, updatesDemo: true }).client
    // Its demo failure is not seeded again on either card.
    expect((await deviceOf(again, "eric-mbp")).update).toBeUndefined()
    expect((await workerOf(again, "eric-mbp")).update).toBeUndefined()
  })

  it("forget a machine's update once its last identity is deleted", async () => {
    const state = seedState()
    state.cookie = SEED_OWNER_ID
    const c = createMockBackend({ state, latency: () => 0, updateTiming: timing, updatesDemo: true }).client
    await c.machines.deleteDevice({ deviceId: "device-eric-mbp" })
    // The worker is still there, and so is the update.
    expect((await workerOf(c, "eric-mbp")).update?.state).toBe("failed")
    await c.machines.deleteWorker({ workerId: "worker-eric-mbp" })
    expect(state.workers.some((w) => w.name === "eric-mbp")).toBe(false)
  })
})
