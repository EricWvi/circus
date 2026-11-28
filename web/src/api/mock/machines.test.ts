import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import type { CircusClient } from "@/api/types"

import { createMockBackend } from "./backend"
import { SEED_OWNER_ID, seedState, type MockState } from "./state"

function client(state: MockState = seedState()): CircusClient {
  state.cookie = SEED_OWNER_ID
  return createMockBackend({ state, latency: () => 0 }).client
}

beforeEach(() => {
  // Calls settle after a zero-latency timer: let it fire on its own.
  vi.useFakeTimers({ shouldAdvanceTime: true })
})
afterEach(() => {
  vi.useRealTimers()
})

describe("the mock's machines", () => {
  it("give the demo machines consistent keys and hostnames: both roles, a device alone, a worker alone", async () => {
    const c = client()
    const devices = await c.machines.listDevices()
    const workers = await c.machines.listWorkers()
    const key = (name: string, list: { name: string; machineKey: string }[]) => list.find((r) => r.name === name)!.machineKey
    for (const name of ["devbox", "eric-mbp", "eric-win"]) expect(key(name, devices)).toBe(key(name, workers))
    const workerKeys = new Set(workers.map((w) => w.machineKey))
    expect(workerKeys.has(key("palace-builder", devices))).toBe(false)
    expect(new Set(devices.map((d) => d.machineKey)).has(key("cloud-runner", workers))).toBe(false)
    // The roles of a machine report the same hostname; palace-builder's daemon none.
    expect(Object.fromEntries(devices.map((d) => [d.name, d.hostname]))).toEqual({
      devbox: "devbox",
      "eric-mbp": "erics-mbp.local",
      "eric-win": "ERIC-WIN",
      "palace-builder": "",
    })
    expect(workers.find((w) => w.name === "eric-mbp")!.hostname).toBe("erics-mbp.local")
  })

  it("list the devices on a worker's machine by its key", async () => {
    const state = seedState()
    state.devices.find((d) => d.name === "palace-builder")!.machineKey = state.workers.find((w) => w.name === "devbox")!.machineKey
    const workers = await client(state).machines.listWorkers()
    expect(Object.fromEntries(workers.map((w) => [w.name, w.localDevices]))).toEqual({
      "cloud-runner": [],
      devbox: ["devbox", "palace-builder"],
      "eric-mbp": ["eric-mbp"],
      "eric-win": ["eric-win"],
    })
  })
})
