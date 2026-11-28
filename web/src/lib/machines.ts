import type { Device, Worker } from "@/api/types"

/** One of the user's machines: its devices and workers (each usually one, or none). */
export interface Machine {
  /** The roles' shared `machineKey`. */
  key: string
  /**
   * The machine's name: the hostname the roles reported, the most current
   * role's first (see `byCurrency`), else that role's name.
   */
  name: string
  /** "linux/amd64", or whichever half is known, from the most current role that reported either; may be empty. */
  platform: string
  /** In the order given (by name). */
  devices: Device[]
  workers: Worker[]
}

type Role = Device | Worker

/**
 * A machine's roles, the most current first: the online ones (devices
 * before workers), then the offline ones by when they were last seen,
 * latest first (devices before workers when equal). An old identity left
 * on the machine (enrolled again instead of rejoined) so names it only
 * when nothing newer reported.
 */
function byCurrency(devices: Device[], workers: Worker[]): Role[] {
  const roles: Role[] = [...devices, ...workers]
  return roles
    .map((role, order) => ({ role, order }))
    .sort((a, b) => {
      if (a.role.online !== b.role.online) return a.role.online ? -1 : 1
      if (!a.role.online && a.role.lastSeenUnixMs !== b.role.lastSeenUnixMs) return b.role.lastSeenUnixMs - a.role.lastSeenUnixMs
      return a.order - b.order
    })
    .map(({ role }) => role)
}

/**
 * Groups devices and workers by machine (their `machineKey`), ordered by
 * the machines' names, ignoring case.
 */
export function groupMachines(devices: Device[], workers: Worker[]): Machine[] {
  const byKey = new Map<string, { devices: Device[]; workers: Worker[] }>()
  const of = (key: string) => {
    let m = byKey.get(key)
    if (!m) byKey.set(key, (m = { devices: [], workers: [] }))
    return m
  }
  for (const d of devices) of(d.machineKey).devices.push(d)
  for (const w of workers) of(w.machineKey).workers.push(w)
  const machines = [...byKey].map(([key, m]): Machine => {
    const roles = byCurrency(m.devices, m.workers)
    const reported = roles.find((r) => r.os || r.arch)
    return {
      key,
      name: roles.find((r) => r.hostname)?.hostname ?? roles[0].name,
      platform: reported ? [reported.os, reported.arch].filter(Boolean).join("/") : "",
      ...m,
    }
  })
  return machines.sort(
    (a, b) => a.name.localeCompare(b.name, undefined, { sensitivity: "base" }) || (a.key < b.key ? -1 : a.key > b.key ? 1 : 0),
  )
}
