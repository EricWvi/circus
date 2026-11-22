import { describe, expect, it } from "vitest"

import type { Project, WorkspaceState, WorkspaceWaiting } from "@/api/types"

import { PROJECTS_WAITING_INTERVAL_MS, projectsRefetchInterval, SESSIONS_LIVE_INTERVAL_MS } from "./hooks"

const project = (...states: (WorkspaceState | `pending/${WorkspaceWaiting}` | undefined)[]): Project => ({
  projectId: "p",
  name: "p",
  repoUrl: "",
  lastUsedLocation: 0,
  createdAtUnixMs: 0,
  updatedAtUnixMs: "0",
  defaultProviderId: "",
  locations: states.map((spec) => {
    const [state, waiting = "unspecified"] = (spec ?? "").split("/") as [WorkspaceState, WorkspaceWaiting?]
    return {
    device: "d",
    path: "",
    effectivePath: "",
    deviceRevoked: false,
    ...(spec
      ? {
          workspace: {
            state,
            waiting,
            device: "d",
            path: "",
            repoUrl: "",
            background: false,
            action: "unspecified" as const,
            error: "unspecified" as const,
            errorDetail: "",
            attemptStartedAtUnixMs: 0,
            updatedAtUnixMs: 0,
          },
        }
      : {}),
    }
  }),
})

describe("how often the projects are read for their folders", () => {
  it("is often while one is prepared or about to be, seldom while all wait for a device or an update, never otherwise", () => {
    expect(projectsRefetchInterval([project("pending/device_offline"), project("preparing")])).toBe(SESSIONS_LIVE_INTERVAL_MS)
    expect(projectsRefetchInterval([project("pending/retry", "pending/device_offline")])).toBe(SESSIONS_LIVE_INTERVAL_MS)
    expect(projectsRefetchInterval([project("pending")])).toBe(SESSIONS_LIVE_INTERVAL_MS)
    expect(projectsRefetchInterval([project("pending/device_offline", "ready"), project("pending/daemon_outdated")])).toBe(
      PROJECTS_WAITING_INTERVAL_MS,
    )
    expect(PROJECTS_WAITING_INTERVAL_MS).toBe(60_000)
    expect(projectsRefetchInterval([project("ready", "failed", undefined)])).toBe(false)
    expect(projectsRefetchInterval([])).toBe(false)
    expect(projectsRefetchInterval(undefined)).toBe(false)
  })
})
