// The mock's workspaces: a project's default folder on a device, prepared
// the first time a session starts in that location. In this tab's memory
// only, like the sessions. A scripted progression stands in for the
// control plane and the daemon: pending, preparing after a second, ready a
// couple of seconds later. The repository URL picks other outcomes (see
// the Web README's mock section):
//
// - "offline": waits for the device to come online for a few seconds first.
// - "outdated": waits for circus to be updated on the device, for good.
// - "slow": clones for half a minute, in the background after a few seconds.
// - "not-empty", "mismatch", "clone-fails", "no-space": the first attempt
//   fails that way; Retry succeeds.
// - "remove-fails": deleting the folder with the project fails.
//
// An offline device waits until it is online; a Windows device fails as
// unsupported.

import type {
  WorkspaceAction,
  WorkspaceError,
  WorkspaceRemoval,
  WorkspaceState,
  WorkspaceStatus,
  WorkspaceWaiting,
} from "@/api/types"
import { defaultPath } from "@/api/validation"

import type { MockDevice } from "./state"

export interface MockWorkspace {
  /** `<project id>/<device id>`. */
  key: string
  projectId: string
  deviceId: string
  state: WorkspaceState
  waiting: WorkspaceWaiting
  device: string
  path: string
  repoUrl: string
  background: boolean
  action: WorkspaceAction
  error: WorkspaceError
  errorDetail: string
  attemptStartedAtUnixMs: number
  updatedAtUnixMs: number
  /** Attempts started; scripted failures hit the first one only. */
  attempts: number
  /** The scripted wait for the device ("offline") has been shown. */
  waitedOffline: boolean
}

/** How the scripted preparation paces itself; tests shorten it. */
export interface MockWorkspaceTiming {
  /** From pending to preparing. */
  startMs: number
  /** From preparing to ready (or failed). */
  prepareMs: number
  /** How long the "offline" script waits for the device. */
  offlineMs: number
  /** A "slow" clone: how long it takes, and when it moves to the background. */
  slowMs: number
  backgroundMs: number
  /** How often a workspace on an offline device looks again. */
  recheckMs: number
}

export const defaultWorkspaceTiming: MockWorkspaceTiming = {
  startMs: 1_000,
  prepareMs: 2_500,
  offlineMs: 6_000,
  slowMs: 30_000,
  backgroundMs: 8_000,
  recheckMs: 2_000,
}

export interface MockWorkspaceHost {
  now(): number
  timing: MockWorkspaceTiming
  /** The device by id; undefined once revoked. */
  device(deviceId: string): MockDevice | undefined
  /** The project's repository URL now. */
  repoUrl(projectId: string): string
  /**
   * Tells the workspace's sessions: a durable KIND_WORKSPACE event, or an
   * ephemeral one, at the workspace's new `updatedAtUnixMs`.
   */
  changed(ws: MockWorkspace, durable: boolean): void
  /** The workspace is ready: its sessions take its path and may start. */
  ready(ws: MockWorkspace): void
}

const homeOf = (os: string) => (os === "darwin" ? "/Users/eric" : "/home/eric")
const has = (url: string, word: string) => url.toLowerCase().includes(word)

/** The failure a repository URL scripts for a first attempt, if any. */
function scriptedFailure(url: string): { error: WorkspaceError; detail: string } | null {
  if (has(url, "not-empty")) return { error: "not_empty", detail: "" }
  if (has(url, "mismatch")) return { error: "remote_mismatch", detail: "git@github.com:someone-else/fork.git" }
  if (has(url, "no-space")) {
    return { error: "no_space", detail: "fatal: write error: No space left on device\nfatal: fetch-pack: invalid index-pack output" }
  }
  if (has(url, "clone-fails")) {
    return {
      error: "clone_failed",
      detail: `Cloning into '.circus-partial-x7k2'...\nremote: Repository not found.\nfatal: repository '${url}' not found`,
    }
  }
  return null
}

/** The WorkspaceStatus a session or location carries. */
export function workspaceStatus(ws: MockWorkspace): WorkspaceStatus {
  return {
    state: ws.state,
    waiting: ws.waiting,
    device: ws.device,
    path: ws.path,
    repoUrl: ws.repoUrl,
    background: ws.background,
    action: ws.action,
    error: ws.error,
    errorDetail: ws.errorDetail,
    attemptStartedAtUnixMs: ws.attemptStartedAtUnixMs,
    updatedAtUnixMs: ws.updatedAtUnixMs,
  }
}

/** The KIND_WORKSPACE payload: lower-case value names, "" for unset. */
export function workspacePayload(ws: MockWorkspace) {
  const name = (v: string) => (v === "unspecified" ? "" : v)
  return {
    state: ws.state,
    waiting: name(ws.waiting),
    device: ws.device,
    path: ws.path,
    repo_url: ws.repoUrl,
    background: ws.background,
    action: name(ws.action),
    error: name(ws.error),
    error_detail: ws.errorDetail,
  }
}

export function createMockWorkspaces(host: MockWorkspaceHost) {
  const all = new Map<string, MockWorkspace>()
  const timers = new Map<string, ReturnType<typeof setTimeout>[]>()

  const clear = (key: string) => {
    for (const timer of timers.get(key) ?? []) clearTimeout(timer)
    timers.delete(key)
  }
  const later = (ws: MockWorkspace, ms: number, run: () => void) => {
    const timer = setTimeout(() => {
      if (all.get(ws.key) === ws) run()
    }, ms)
    timers.set(ws.key, [...(timers.get(ws.key) ?? []), timer])
  }

  /**
   * Applies a change; a new state is durable, anything else ephemeral.
   * Every change moves `updatedAtUnixMs` forward, and its event carries it.
   */
  function set(ws: MockWorkspace, patch: Partial<MockWorkspace>) {
    const before = JSON.stringify(workspacePayload(ws))
    const stateChanged = patch.state !== undefined && patch.state !== ws.state
    Object.assign(ws, patch)
    if (!stateChanged && JSON.stringify(workspacePayload(ws)) === before) return
    ws.updatedAtUnixMs = Math.max(host.now(), ws.updatedAtUnixMs + 1)
    host.changed(ws, stateChanged)
  }

  /** Moves a pending workspace on, or makes it wait. */
  function advance(ws: MockWorkspace) {
    if (ws.state !== "pending") return
    const device = host.device(ws.deviceId)
    if (!device) return set(ws, { state: "failed", waiting: "unspecified", error: "device_revoked" })
    if (!device.online) {
      set(ws, { waiting: "device_offline" })
      return later(ws, host.timing.recheckMs, () => advance(ws))
    }
    if (device.os === "windows") return set(ws, { state: "failed", waiting: "unspecified", error: "unsupported_os" })
    const url = host.repoUrl(ws.projectId)
    const first = ws.attempts === 0
    if (first && has(url, "outdated")) return set(ws, { waiting: "daemon_outdated" })
    if (first && has(url, "offline") && !ws.waitedOffline) {
      ws.waitedOffline = true
      set(ws, { waiting: "device_offline" })
      return later(ws, host.timing.offlineMs, () => {
        set(ws, { waiting: "retry" })
        later(ws, host.timing.startMs, () => advance(ws))
      })
    }
    prepare(ws, url)
  }

  function prepare(ws: MockWorkspace, url: string) {
    const slow = has(url, "slow")
    const failure = ws.attempts === 0 ? scriptedFailure(url) : null
    ws.attempts++
    set(ws, {
      state: "preparing",
      waiting: "unspecified",
      repoUrl: url,
      background: false,
      error: "unspecified",
      errorDetail: "",
      attemptStartedAtUnixMs: host.now(),
    })
    if (slow) later(ws, host.timing.backgroundMs, () => set(ws, { background: true }))
    later(ws, slow ? host.timing.slowMs : host.timing.prepareMs, () => {
      if (failure) return set(ws, { state: "failed", background: false, error: failure.error, errorDetail: failure.detail })
      const device = host.device(ws.deviceId)
      set(ws, {
        state: "ready",
        background: false,
        path: `${homeOf(device?.os ?? "")}/circus/projects/${ws.projectId}`,
        action: url === "" ? "created" : "cloned",
      })
      host.ready(ws)
    })
  }

  /** Starts a pending or failed workspace over; leaves one under way or ready as it is. */
  function restart(ws: MockWorkspace) {
    if (ws.state !== "failed") return
    clear(ws.key)
    set(ws, { state: "pending", waiting: "retry", error: "unspecified", errorDetail: "", background: false })
    later(ws, host.timing.startMs, () => advance(ws))
  }

  return {
    get: (key: string) => all.get(key),
    forProject: (projectId: string) => [...all.values()].filter((ws) => ws.projectId === projectId),

    /**
     * The workspace of a session created in a default location: a new one
     * starts pending, a failed one starts over.
     */
    ensure(projectId: string, device: MockDevice): MockWorkspace {
      const key = `${projectId}/${device.deviceId}`
      const existing = all.get(key)
      if (existing) {
        restart(existing)
        return existing
      }
      const ws: MockWorkspace = {
        key,
        projectId,
        deviceId: device.deviceId,
        state: "pending",
        waiting: "unspecified",
        device: device.name,
        path: defaultPath(projectId),
        repoUrl: host.repoUrl(projectId),
        background: false,
        action: "unspecified",
        error: "unspecified",
        errorDetail: "",
        attemptStartedAtUnixMs: 0,
        updatedAtUnixMs: host.now(),
        attempts: 0,
        waitedOffline: false,
      }
      all.set(key, ws)
      later(ws, host.timing.startMs, () => advance(ws))
      return ws
    },

    retry: restart,

    /** Deletes a project's folder on a device, as DeleteProject asks it to. */
    remove(projectId: string, device: MockDevice | undefined, name: string): WorkspaceRemoval {
      const ws = device && all.get(`${projectId}/${device.deviceId}`)
      const path = ws?.path ?? defaultPath(projectId)
      const kept: WorkspaceRemoval = { device: name, outcome: "kept", path, error: "unspecified", errorDetail: "" }
      if (!device || !ws || !device.online || ws.state !== "ready") return kept
      if (has(host.repoUrl(projectId), "remove-fails")) {
        return { ...kept, outcome: "failed", error: "permission_denied", errorDetail: `rm: cannot remove '${path}/.git/objects/pack': Permission denied` }
      }
      return { ...kept, outcome: "removed" }
    },

    /** Forgets a deleted project's workspaces. */
    forget(projectId: string) {
      for (const ws of [...all.values()]) {
        if (ws.projectId !== projectId) continue
        clear(ws.key)
        all.delete(ws.key)
      }
    },
  }
}

export type MockWorkspaces = ReturnType<typeof createMockWorkspaces>
