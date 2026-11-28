import { describe, expect, it } from "vitest"

import type { Device, Worker } from "@/api/types"

import { groupMachines } from "./machines"

const device = (name: string, machineKey: string, hostname = ""): Device => ({
  deviceId: `d-${name}`,
  name,
  online: true,
  lastSeenUnixMs: 0,
  os: "linux",
  arch: "amd64",
  hostname,
  version: "v0.1.0",
  machineKey,
  updateVersion: "",
})

const worker = (name: string, machineKey: string, hostname = ""): Worker => ({
  workerId: `w-${name}`,
  name,
  online: true,
  lastSeenUnixMs: 0,
  os: "linux",
  arch: "amd64",
  hostname,
  version: "v0.1.0",
  localDevices: [],
  machineKey,
  integrator: false,
  agents: [],
  updateVersion: "",
})

const summary = (devices: Device[], workers: Worker[]) =>
  groupMachines(devices, workers).map((m) => [m.name, m.devices.map((d) => d.name), m.workers.map((w) => w.name)])

describe("groupMachines", () => {
  it("puts a device and a worker with the same key on one machine, and others on their own", () => {
    expect(summary([device("box", "m1"), device("solo", "m2")], [worker("box", "m1"), worker("runner", "m3")])).toEqual([
      ["box", ["box"], ["box"]],
      ["runner", [], ["runner"]],
      ["solo", ["solo"], []],
    ])
  })

  it("groups by key, never by name", () => {
    expect(summary([device("box", "m1")], [worker("box", "m2"), worker("other", "m1")])).toEqual([
      ["box", ["box"], ["other"]],
      ["box", [], ["box"]],
    ])
  })

  it("names a machine by its device's hostname, else its worker's, else its device's name, else its worker's", () => {
    expect(summary([device("d", "m1", "host-d")], [worker("w", "m1", "host-w")])[0][0]).toBe("host-d")
    expect(summary([device("d", "m1")], [worker("w", "m1", "host-w")])[0][0]).toBe("host-w")
    expect(summary([device("d", "m1")], [worker("w", "m1")])[0][0]).toBe("d")
    expect(summary([], [worker("w", "m1")])[0][0]).toBe("w")
  })

  it("prefers an online role for the name and the platform, else the one seen last", () => {
    const at = <R extends Device | Worker>(r: R, online: boolean, lastSeenUnixMs: number, os = r.os): R => ({ ...r, online, lastSeenUnixMs, os })
    // An old device left on the machine (offline, seen long ago) and the new, online one.
    const old = at(device("aaa-old", "m1", "oldname"), false, 1_000, "windows")
    const box = at(device("box", "m1", "newname"), true, 9_000, "linux")
    let [m] = groupMachines([old, box], [])
    expect([m.name, m.platform]).toEqual(["newname", "linux/amd64"])
    // An online worker beats an offline device.
    ;[m] = groupMachines([old], [at(worker("w", "m1", "workerhost"), true, 9_000, "darwin")])
    expect([m.name, m.platform]).toEqual(["workerhost", "darwin/amd64"])
    // All offline: the one seen last, whichever role it is.
    ;[m] = groupMachines([old], [at(worker("w", "m1", "workerhost"), false, 2_000, "darwin")])
    expect(m.name).toBe("workerhost")
    ;[m] = groupMachines([at(old, false, 3_000)], [at(worker("w", "m1", "workerhost"), false, 2_000, "darwin")])
    expect([m.name, m.platform]).toEqual(["oldname", "windows/amd64"])
    // Ties (online, or offline seen at the same time): the device first.
    ;[m] = groupMachines([at(old, true, 1)], [at(worker("w", "m1", "workerhost"), true, 2, "darwin")])
    expect(m.name).toBe("oldname")
    ;[m] = groupMachines([at(old, false, 5)], [at(worker("w", "m1", "workerhost"), false, 5)])
    expect(m.name).toBe("oldname")
    // The current role without a hostname: the next one that has one.
    ;[m] = groupMachines([at(device("box", "m1"), true, 9)], [at(worker("w", "m1", "workerhost"), false, 1)])
    expect(m.name).toBe("workerhost")
    // Nothing reported: no platform, and the current role's name.
    ;[m] = groupMachines([{ ...at(device("box", "m1"), false, 1), os: "", arch: "" }], [at(worker("w", "m1"), false, 5, "")])
    expect([m.name, m.platform]).toEqual(["w", "amd64"])
  })

  it("orders machines by name ignoring case, then by key", () => {
    const machines = groupMachines(
      [device("b", "m1", "Beta"), device("a", "m2", "alpha"), device("c", "m4", "beta")],
      [worker("z", "m3", "ALPHA2")],
    )
    expect(machines.map((m) => m.name)).toEqual(["alpha", "ALPHA2", "Beta", "beta"])
    expect(groupMachines([], [])).toEqual([])
  })
})
