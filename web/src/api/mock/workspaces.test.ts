import { describe, expect, it } from "vitest"

import type { CircusClient, RawSessionEvent } from "@/api/types"

import { createMockBackend } from "./backend"
import { SEED_OWNER_ID, seedState, type MockState } from "./state"

const timing = { startMs: 5, prepareMs: 10, offlineMs: 20, slowMs: 40, backgroundMs: 15, recheckMs: 5 }
const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms))

/** The demo seed with a project "fresh" whose locations are the defaults on these devices. */
function mock(repoUrl: string, devices: string[] = ["devbox"], edit: (state: MockState) => void = () => {}) {
  const state = seedState()
  state.cookie = SEED_OWNER_ID
  state.projects.push({
    owner: SEED_OWNER_ID,
    projectId: "fresh",
    name: "fresh",
    repoUrl,
    locations: devices.map((device) => ({ device, path: "", deviceId: `device-${device}` })),
    lastUsedLocation: 0,
    createdAtUnixMs: 0,
    updatedAtUnixMs: 0,
  })
  edit(state)
  const backend = createMockBackend({
    state,
    latency: () => 0,
    sessionTiming: { stepMs: 1, chunkMs: 1 },
    workspaceTiming: timing,
  })
  return { state, ...backend }
}

const start = (client: CircusClient, locationIndex = 0) =>
  client.sessions.createSession({ projectId: "fresh", locationIndex, clientRequestId: crypto.randomUUID() })

/** Watches a session until `done` holds for the events received, or a second passes. */
async function watchUntil(client: CircusClient, sessionId: string, done: (events: RawSessionEvent[]) => boolean) {
  const abort = new AbortController()
  const timer = setTimeout(() => abort.abort(), 1_000)
  const events: RawSessionEvent[] = []
  try {
    for await (const event of client.sessions.watchSession({ sessionId, afterSeq: 0, signal: abort.signal })) {
      events.push(event)
      if (done(events)) break
    }
  } finally {
    clearTimeout(timer)
    abort.abort()
  }
  return events
}

const workspaceEvents = (events: RawSessionEvent[]) =>
  events
    .filter((e) => e.kind === "workspace")
    .map((e): Record<string, unknown> => ({ durable: !e.ephemeral, ...(JSON.parse(e.payloadJson) as Record<string, unknown>) }))
const reached = (state: string) => (events: RawSessionEvent[]) =>
  workspaceEvents(events).some((e) => e.state === state)

describe("mock workspaces", () => {
  it("clones into the default folder and holds the first turn until it is ready", async () => {
    const { client } = mock("https://example.com/fresh.git")
    const session = await start(client)
    expect(session.workspace).toMatchObject({ state: "pending", device: "devbox", path: "~/circus/projects/fresh" })
    expect(session.cwd).toBe("~/circus/projects/fresh")
    await client.sessions.sendMessage({ sessionId: session.sessionId, clientMsgId: "c1", text: "hello" })
    expect((await client.sessions.getSession({ sessionId: session.sessionId })).serverState).toBe("queued")

    const events = await watchUntil(client, session.sessionId, (e) => e.some((x) => x.kind === "result"))
    const states = workspaceEvents(events).map((e) => [e.state, e.durable])
    expect(states).toEqual([
      ["preparing", true],
      ["ready", true],
    ])
    expect(workspaceEvents(events).at(-1)).toMatchObject({
      action: "cloned",
      path: "/home/eric/circus/projects/fresh",
      repo_url: "https://example.com/fresh.git",
    })
    // The turn starts only after the folder is ready.
    const readyAt = events.findIndex((e) => e.kind === "workspace" && e.payloadJson.includes('"ready"'))
    expect(events.findIndex((e) => e.kind === "turn_started")).toBeGreaterThan(readyAt)
    const details = await client.sessions.getSession({ sessionId: session.sessionId })
    expect(details.cwd).toBe("/home/eric/circus/projects/fresh")
    expect(details.workspace).toMatchObject({ state: "ready", action: "cloned" })

    // The next session there uses the folder at once.
    const second = await start(client)
    expect(second.workspace?.state).toBe("ready")
    expect(second.cwd).toBe("/home/eric/circus/projects/fresh")
    const project = await client.projects.getProject({ projectId: "fresh" })
    expect(project.locations[0].workspace).toMatchObject({ state: "ready", path: "/home/eric/circus/projects/fresh" })
  })

  it("creates an empty folder without a repository", async () => {
    const { client } = mock("")
    const session = await start(client)
    const events = await watchUntil(client, session.sessionId, reached("ready"))
    expect(workspaceEvents(events).at(-1)).toMatchObject({ state: "ready", action: "created", repo_url: "" })
  })

  it("tells the wait for the device live, then prepares", async () => {
    const { client } = mock("https://example.com/offline.git")
    const session = await start(client)
    const events = await watchUntil(client, session.sessionId, reached("ready"))
    expect(workspaceEvents(events).map((e) => [e.state, e.waiting, e.durable])).toEqual([
      ["pending", "device_offline", false],
      ["pending", "retry", false],
      ["preparing", "", true],
      ["ready", "", true],
    ])
  })

  it("waits while the device is offline, and fails on Windows", async () => {
    const { client, state } = mock("", ["devbox", "eric-win"], (s) => {
      s.devices.find((d) => d.name === "devbox")!.online = false
      s.devices.find((d) => d.name === "eric-win")!.online = true
    })
    const offline = await start(client, 0)
    await sleep(20)
    expect((await client.sessions.getSession({ sessionId: offline.sessionId })).workspace).toMatchObject({
      state: "pending",
      waiting: "device_offline",
    })
    state.devices.find((d) => d.name === "devbox")!.online = true
    await watchUntil(client, offline.sessionId, reached("ready"))

    const windows = await start(client, 1)
    await watchUntil(client, windows.sessionId, reached("failed"))
    expect((await client.sessions.getSession({ sessionId: windows.sessionId })).workspace).toMatchObject({
      state: "failed",
      error: "unsupported_os",
    })
  })

  it("fails as the repository URL says, and succeeds when retried", async () => {
    const { client } = mock("https://example.com/clone-fails.git")
    const session = await start(client)
    await watchUntil(client, session.sessionId, reached("failed"))
    const failed = (await client.sessions.getSession({ sessionId: session.sessionId })).workspace
    expect(failed).toMatchObject({ state: "failed", error: "clone_failed" })
    expect(failed?.errorDetail).toContain("Repository not found")

    expect(await client.sessions.retryWorkspace({ sessionId: session.sessionId })).toMatchObject({
      state: "pending",
      waiting: "retry",
      error: "unspecified",
    })
    await watchUntil(client, session.sessionId, reached("ready"))
    // Retrying a ready folder changes nothing.
    expect((await client.sessions.retryWorkspace({ sessionId: session.sessionId })).state).toBe("ready")
  })

  it("starts a failed folder over when another session starts there", async () => {
    const { client } = mock("https://example.com/not-empty.git")
    const first = await start(client)
    await watchUntil(client, first.sessionId, reached("failed"))
    const second = await start(client)
    expect(second.workspace).toMatchObject({ state: "pending", error: "unspecified" })
  })

  it("refuses a retry for a session without a folder", async () => {
    const { client } = mock("")
    await expect(client.sessions.retryWorkspace({ sessionId: "web-vite" })).rejects.toMatchObject({
      code: "failed_precondition",
    })
  })

  it("deletes ready folders with the project, and says which it kept or couldn't delete", async () => {
    const { client, state } = mock("https://example.com/remove-fails.git", ["devbox", "eric-mbp", "palace-builder"])
    const sessions = [await start(client, 0), await start(client, 1)]
    await Promise.all(sessions.map((s) => watchUntil(client, s.sessionId, reached("ready"))))
    for (const s of sessions) await client.sessions.stopSession({ sessionId: s.sessionId })
    state.devices.find((d) => d.name === "eric-mbp")!.online = false

    const { workspaceRemovals } = await client.projects.deleteProject({
      projectId: "fresh",
      removeWorkspacesOn: ["devbox", "eric-mbp", "palace-builder"],
    })
    expect(workspaceRemovals).toEqual([
      expect.objectContaining({ device: "devbox", outcome: "failed", error: "permission_denied", path: "/home/eric/circus/projects/fresh" }),
      expect.objectContaining({ device: "eric-mbp", outcome: "kept", path: "/Users/eric/circus/projects/fresh" }),
      expect.objectContaining({ device: "palace-builder", outcome: "kept", path: "~/circus/projects/fresh" }),
    ])
  })

  it("removes a ready folder", async () => {
    const { client } = mock("")
    const s = await start(client)
    await watchUntil(client, s.sessionId, reached("ready"))
    await client.sessions.stopSession({ sessionId: s.sessionId })
    expect(await client.projects.deleteProject({ projectId: "fresh", removeWorkspacesOn: ["devbox"] })).toEqual({
      workspaceRemovals: [
        { device: "devbox", outcome: "removed", path: "/home/eric/circus/projects/fresh", error: "unspecified", errorDetail: "" },
      ],
    })
  })
})
