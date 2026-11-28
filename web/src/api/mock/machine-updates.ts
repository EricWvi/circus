// The machine updates of the mock: UpdateMachine and the update a machine
// would run, simulated with timers in this tab's memory. A machine is the
// devices and workers that share a `machineKey`. As on the server, an update is offered on the card of
// a role that is online and runs as the installed service (not
// `foreground`), and runs on such a role of the machine.

import { ApiError } from "@/api/errors"
import type { Device, MachineUpdate, MachineUpdateFailure } from "@/api/types"

import type { MockDevice, MockState, MockWorker } from "./state"

/** The latest circus release, as the mock's control plane read it. */
export const MOCK_LATEST_RELEASE = "v0.2.0"

/** How the simulated updates pace themselves. */
export interface MockUpdateTiming {
  /** How long an update runs before the machine comes back with the new version. */
  completeMs: number
  /**
   * How long the control plane waits for that: an update that would take
   * longer fails as timed out when this has passed.
   */
  deadlineMs: number
}

export const defaultUpdateTiming: MockUpdateTiming = {
  completeMs: 12_000,
  deadlineMs: 5 * 60_000,
}

/** The parts of a "vX.Y.Z" release, or null for anything else (such as "dev"). */
function release(version: string): [number, number, number] | null {
  const m = /^v(\d+)\.(\d+)\.(\d+)$/.exec(version)
  return m ? [Number(m[1]), Number(m[2]), Number(m[3])] : null
}

/** Whether `version` is a release older than `latest`. */
export function olderRelease(version: string, latest: string): boolean {
  const a = release(version)
  const b = release(latest)
  if (!a || !b) return false
  for (let i = 0; i < 3; i++) if (a[i] !== b[i]) return a[i] < b[i]
  return false
}

type Role = MockDevice | MockWorker

/** Whether a role could run `circus update` now: online, and running as the installed service. */
const executes = (r: Role) => r.online && !r.foreground

export function createMockMachineUpdates({
  state,
  now,
  timing,
  changed,
}: {
  state: MockState
  now: () => number
  timing: MockUpdateTiming
  /** The persisted state changed (a device's version). */
  changed: () => void
}) {
  /** Each machine's update, by `<owner>/<machine>`. */
  const updates = new Map<string, MachineUpdate>()
  const timers = new Map<string, ReturnType<typeof setTimeout>>()

  const machineOf = (r: Role) => r.machineKey
  const key = (owner: string, machine: string) => `${owner}/${machine}`

  function roles(owner: string, machine: string) {
    return {
      devices: state.devices.filter((d) => d.owner === owner && machineOf(d) === machine),
      workers: state.workers.filter((w) => w.owner === owner && machineOf(w) === machine),
    }
  }

  /** The release an update would install from the card of `role` on `machine`; empty when none is offered. */
  function offer(owner: string, machine: string, role: Role): string {
    if (updates.get(key(owner, machine))?.state === "running") return ""
    if (!executes(role) || !olderRelease(role.version, MOCK_LATEST_RELEASE)) return ""
    return MOCK_LATEST_RELEASE
  }

  function fields(owner: string, machine: string, role: Role): Pick<Device, "updateVersion" | "update"> {
    const update = updates.get(key(owner, machine))
    return { updateVersion: offer(owner, machine, role), ...(update ? { update: { ...update } } : {}) }
  }

  /** Records `update` on the machine and schedules its end: done after `completeMs`, or timed out at the deadline. */
  function run(owner: string, machine: string, update: MachineUpdate, completeMs: number) {
    const k = key(owner, machine)
    updates.set(k, update)
    clearTimeout(timers.get(k))
    const left = update.deadlineUnixMs - now()
    timers.set(
      k,
      completeMs <= left
        ? setTimeout(() => complete(owner, machine, update), completeMs)
        : setTimeout(() => timeOut(owner, machine, update), Math.max(left, 0)),
    )
  }

  /** The machine comes back with the target version on every role, unless `update` was replaced since. */
  function complete(owner: string, machine: string, update: MachineUpdate) {
    const k = key(owner, machine)
    timers.delete(k)
    if (updates.get(k) !== update) return
    updates.delete(k)
    const { devices, workers } = roles(owner, machine)
    for (const d of devices) d.version = update.targetVersion
    for (const w of workers) w.version = update.targetVersion
    changed()
  }

  /** The deadline passed before the machine came back: the update fails, unless it was replaced since. */
  function timeOut(owner: string, machine: string, update: MachineUpdate) {
    const k = key(owner, machine)
    timers.delete(k)
    if (updates.get(k) !== update) return
    updates.set(k, { ...update, state: "failed", error: "", failure: "timed_out" })
  }

  /** UpdateMachine for the identity whose card was pressed; the update takes `timing.completeMs`. */
  function start(owner: string, req: { deviceId: string } | { workerId: string }): MachineUpdate {
    let machine: string
    let role: Role
    if ("deviceId" in req) {
      const d = state.devices.find((x) => x.owner === owner && x.deviceId === req.deviceId)
      if (!d) throw new ApiError("not_found", "Device not found.")
      machine = machineOf(d)
      role = d
    } else {
      const w = state.workers.find((x) => x.owner === owner && x.workerId === req.workerId)
      if (!w) throw new ApiError("not_found", "Worker not found.")
      machine = machineOf(w)
      role = w
    }
    const current = updates.get(key(owner, machine))
    if (current?.state === "running") return { ...current }
    if (!olderRelease(role.version, MOCK_LATEST_RELEASE)) {
      throw new ApiError("failed_precondition", `Already at the latest release ${MOCK_LATEST_RELEASE}.`)
    }
    if (!executes(role)) {
      throw new ApiError("failed_precondition", "It is not online as the installed service.")
    }
    const at = now()
    const update: MachineUpdate = {
      targetVersion: MOCK_LATEST_RELEASE,
      state: "running",
      error: "",
      startedUnixMs: at,
      deadlineUnixMs: at + timing.deadlineMs,
    }
    run(owner, machine, update, timing.completeMs)
    return { ...update }
  }

  /**
   * Whether `machine` is older than the latest release, by its device (the
   * mock persists devices, not workers, which are seeded again at their old
   * version on every load), or its worker without a device: only then do
   * the demo's seeds apply.
   */
  function behind(owner: string, machine: string): boolean {
    const { devices, workers } = roles(owner, machine)
    return (devices.length ? devices : workers).some((r) => olderRelease(r.version, MOCK_LATEST_RELEASE))
  }

  return {
    device: (owner: string, d: MockDevice) => fields(owner, machineOf(d), d),
    worker: (owner: string, w: MockWorker) => fields(owner, machineOf(w), w),
    start: (owner: string, req: { deviceId: string } | { workerId: string }) => start(owner, req),

    /**
     * Seeds a failed update on `machine` (the demo), written directly: it
     * never fails, whatever the persisted state says. Skipped once the
     * machine is at the latest release.
     */
    seedFailed(owner: string, machine: string, failure: MachineUpdateFailure, error: string) {
      if (!behind(owner, machine)) return
      const at = now() - 6 * 60_000
      updates.set(key(owner, machine), {
        targetVersion: MOCK_LATEST_RELEASE,
        state: "failed",
        error,
        failure,
        startedUnixMs: at,
        deadlineUnixMs: at + timing.deadlineMs,
      })
    },

    /**
     * Seeds a running update on `machine` that takes `completeMs` (the
     * demo), written directly as seedFailed's. Each page load starts it
     * again while the machine is behind; once it completed (the new
     * version is persisted), it is skipped.
     */
    seedRunning(owner: string, machine: string, completeMs: number) {
      if (!behind(owner, machine)) return
      const at = now()
      run(owner, machine, {
        targetVersion: MOCK_LATEST_RELEASE,
        state: "running",
        error: "",
        startedUnixMs: at,
        deadlineUnixMs: at + timing.deadlineMs,
      }, completeMs)
    },

    /** The machine's last identity is gone: so is its update. */
    forget(owner: string) {
      for (const k of [...updates.keys()]) {
        if (!k.startsWith(`${owner}/`)) continue
        const machine = k.slice(owner.length + 1)
        const { devices, workers } = roles(owner, machine)
        if (devices.length + workers.length > 0) continue
        updates.delete(k)
        clearTimeout(timers.get(k))
        timers.delete(k)
      }
    },
  }
}
