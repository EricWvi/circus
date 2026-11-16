import { describe, expect, it } from "vitest"

import { ApiError, type ErrorCode } from "@/api/errors"
import type { CircusClient } from "@/api/types"

import {
  createMockBackend,
  LOGIN_FAILURE_WINDOW_MS,
  MAX_KEPT_ATTACHMENT_URL,
  MAX_LOGIN_FAILURES,
} from "./backend"
import { parsePersisted, serialize, VERSION } from "./persist"
import {
  DEV_SETUP_TOKEN,
  freshState,
  MOCK_LOGIN_METHODS,
  OIDC_SETUP_EMAIL,
  seedState,
  settleInFlight,
  STALE_AFTER_MS,
  type MockState,
} from "./state"

function backend(state: MockState = seedState(), now?: () => number) {
  return createMockBackend({ state, latency: () => 0, now })
}

const eric = { email: "eric@example.com", password: "circus" }
const dev = { email: "dev@example.com", password: "circus" }
const methods = MOCK_LOGIN_METHODS

async function signedIn(): Promise<CircusClient> {
  const { client } = backend()
  await client.auth.login(eric)
  return client
}

async function expectCode(promise: Promise<unknown>, code: ErrorCode) {
  const error = await promise.then(
    () => undefined,
    (e: unknown) => e,
  )
  expect(error).toBeInstanceOf(ApiError)
  expect((error as ApiError).code).toBe(code)
}

/** The field a rejected call's error names. */
async function fieldOf(promise: Promise<unknown>) {
  const error = await promise.then(
    () => undefined,
    (e: unknown) => e,
  )
  expect(error).toBeInstanceOf(ApiError)
  return (error as ApiError).field
}

const mbp = (path: string) => ({ device: "eric-mbp", path })

describe("auth", () => {
  const setupReq = { token: DEV_SETUP_TOKEN, email: "Admin@Example.com ", password: "password1" }

  it("reports needs_setup until a super user exists", async () => {
    const { client } = backend(freshState())
    expect(await client.auth.whoAmI()).toEqual({ kind: "needs_setup", methods })
  })

  it("checks the setup token before anything else", async () => {
    const { client } = backend(freshState())
    await expectCode(
      client.auth.setup({ token: "nope", email: "not an email", password: "x" }),
      "permission_denied",
    )
    expect(await fieldOf(client.auth.setup({ ...setupReq, token: "" }))).toBe("token")
    expect(await client.auth.whoAmI()).toEqual({ kind: "needs_setup", methods })
  })

  it("creates the super user with a normalized email and signs them in", async () => {
    const { client } = backend(freshState())
    const user = await client.auth.setup(setupReq)
    expect(user).toMatchObject({ email: "admin@example.com", role: "super" })
    expect(await client.auth.whoAmI()).toEqual({ kind: "signed_in", user })
  })

  it("allows a single super user", async () => {
    const { client } = backend(freshState())
    await client.auth.setup(setupReq)
    await expectCode(client.auth.setup({ ...setupReq, email: "other@example.com" }), "already_exists")
    // A wrong token still learns nothing.
    await expectCode(client.auth.setup({ ...setupReq, token: "nope" }), "permission_denied")
  })

  it("names the invalid field", async () => {
    const { client } = backend(freshState())
    await expectCode(client.auth.setup({ ...setupReq, email: "" }), "invalid_argument")
    for (const email of ["", "no-at", "a@@b", "@b", "a@", "a b@c", `${"a".repeat(250)}@b.cd`]) {
      expect(await fieldOf(client.auth.setup({ ...setupReq, email }))).toBe("email")
    }
    for (const password of ["short", "x".repeat(73)]) {
      expect(await fieldOf(client.auth.setup({ ...setupReq, password }))).toBe("password")
    }
  })

  it("signs in by email with the user's role", async () => {
    const { client } = backend()
    expect(await client.auth.whoAmI()).toEqual({ kind: "signed_out", methods })
    expect(await client.auth.login({ ...dev, email: " DEV@example.com" })).toMatchObject({
      email: "dev@example.com",
      role: "user",
    })
    await client.auth.logout()
    expect(await client.auth.login(eric)).toMatchObject({ role: "super" })
  })

  it("rejects wrong credentials", async () => {
    const { client } = backend()
    await expectCode(client.auth.login({ ...eric, password: "wrong" }), "unauthenticated")
    await expectCode(client.auth.login({ ...eric, email: "nobody@example.com" }), "unauthenticated")
    expect(await client.auth.whoAmI()).toEqual({ kind: "signed_out", methods })
  })

  it("limits failed sign-ins per email", async () => {
    let at = 0
    const { client } = backend(seedState(), () => at)
    for (let i = 0; i < MAX_LOGIN_FAILURES; i++) {
      await expectCode(client.auth.login({ ...eric, password: "wrong" }), "unauthenticated")
    }
    await expectCode(client.auth.login(eric), "resource_exhausted")
    expect(await client.auth.login(dev)).toMatchObject({ email: dev.email })
    at += LOGIN_FAILURE_WINDOW_MS
    expect(await client.auth.login(eric)).toMatchObject({ email: eric.email })
  })

  it("keeps each user's data to themselves", async () => {
    const { client } = backend()
    await client.auth.login(dev)
    expect(await client.projects.listProjects()).toEqual([])
    await expectCode(client.projects.getProject({ projectId: "circus" }), "not_found")
  })

  it("signs in through the mock OIDC provider", async () => {
    const notes: string[] = []
    const { client } = createMockBackend({ state: seedState(), latency: () => 0, note: (n) => notes.push(n) })
    await client.oidc.startLogin("/p/circus")
    expect(await client.auth.whoAmI()).toMatchObject({ user: { email: eric.email, role: "super" } })
    expect(notes).toHaveLength(1)
  })

  it("can run without an OIDC provider", async () => {
    const { client } = createMockBackend({ state: seedState(), latency: () => 0, oidc: false })
    expect(await client.auth.whoAmI()).toEqual({ kind: "signed_out", methods: { password: true } })
    await expectCode(client.oidc.startLogin(), "failed_precondition")
  })

  it("sets up through the mock OIDC provider with the same token rules", async () => {
    const { client } = backend(freshState())
    await expectCode(client.oidc.startSetup("nope"), "permission_denied")
    await client.oidc.startSetup(DEV_SETUP_TOKEN)
    expect(await client.auth.whoAmI()).toMatchObject({
      user: { email: OIDC_SETUP_EMAIL, role: "super" },
    })
    await expectCode(client.oidc.startSetup(DEV_SETUP_TOKEN), "already_exists")
  })

  it("rejects every non-auth call when signed out", async () => {
    const { client } = backend()
    await expectCode(client.projects.listProjects(), "unauthenticated")
    await expectCode(client.projects.getProject({ projectId: "circus" }), "unauthenticated")
    await expectCode(client.projects.createProject({ name: "x", locations: [mbp("/x")] }), "unauthenticated")
    await expectCode(client.machines.listDevices(), "unauthenticated")
    await expectCode(client.sessions.listSessions({}), "unauthenticated")
  })

  it("signs out", async () => {
    const client = await signedIn()
    await client.auth.logout()
    expect(await client.auth.whoAmI()).toEqual({ kind: "signed_out", methods })
    await expectCode(client.projects.listProjects(), "unauthenticated")
  })
})

describe("projects", () => {
  it("lists projects by name", async () => {
    const client = await signedIn()
    await client.projects.createProject({ name: "aardvark", locations: [mbp("/src/a")] })
    const names = (await client.projects.listProjects()).map((p) => p.name)
    expect(names).toEqual(["aardvark", "circus", "palace", "toolbox"])
  })

  it("trims names and keeps them unique regardless of case", async () => {
    const client = await signedIn()
    const created = await client.projects.createProject({ name: "  web  ", locations: [mbp("/src/web")] })
    expect(created.name).toBe("web")
    expect(created.lastUsedLocation).toBe(0)
    await expectCode(
      client.projects.createProject({ name: "Circus", locations: [mbp("/src/c")] }),
      "already_exists",
    )
    await expectCode(
      client.projects.updateProject({ projectId: created.projectId, name: "palace" }),
      "already_exists",
    )
    // Renaming to its own name is not a conflict.
    await client.projects.updateProject({ projectId: created.projectId, name: "WEB" })
  })

  it("validates names", async () => {
    const client = await signedIn()
    for (const name of ["", "   ", "x".repeat(65), "a\tb"]) {
      await expectCode(client.projects.createProject({ name, locations: [mbp("/src")] }), "invalid_argument")
    }
    await client.projects.createProject({ name: "x".repeat(64), locations: [mbp("/src")] })
  })

  it("validates locations", async () => {
    const client = await signedIn()
    const create = (locations: { device: string; path: string }[]) =>
      client.projects.createProject({ name: "new", locations })
    await expectCode(create([]), "invalid_argument")
    await expectCode(create([{ device: "ghost", path: "/src" }]), "invalid_argument")
    await expectCode(create([mbp("src/relative")]), "invalid_argument")
    await expectCode(create([{ device: "eric-win", path: "/not/windows" }]), "invalid_argument")
    await expectCode(create([mbp("/src/a"), mbp("/src/a/")]), "invalid_argument")
    const ok = await create([mbp("/src/a/"), { device: "eric-win", path: "D:\\work" }])
    expect(ok.locations).toEqual([mbp("/src/a"), { device: "eric-win", path: "D:\\work" }])
  })

  it("normalizes Windows paths before checking for duplicates", async () => {
    const client = await signedIn()
    const win = (path: string) => ({ device: "eric-win", path })
    await expectCode(
      client.projects.createProject({ name: "w", locations: [win("D:\\work"), win("d:/work/")] }),
      "invalid_argument",
    )
    const p = await client.projects.createProject({ name: "w", locations: [win("c:/src/app")] })
    expect(p.locations).toEqual([win("C:\\src\\app")])
  })

  it("names the field each validation error is about", async () => {
    const client = await signedIn()
    expect(await fieldOf(client.projects.createProject({ name: "", locations: [mbp("/a")] }))).toBe("name")
    expect(await fieldOf(client.projects.createProject({ name: "circus", locations: [mbp("/a")] }))).toBe("name")
    expect(await fieldOf(client.projects.createProject({ name: "n", locations: [] }))).toBe("locations")
    expect(
      await fieldOf(client.projects.createProject({ name: "n", locations: [mbp("/a"), { device: "x", path: "/a" }] })),
    ).toBe("locations[1].device")
    expect(await fieldOf(client.projects.createProject({ name: "n", locations: [mbp("rel")] }))).toBe(
      "locations[0].path",
    )
    expect(await fieldOf(client.projects.createProject({ name: "n", locations: [mbp("/a"), mbp("/a")] }))).toBe(
      "locations[1].path",
    )
  })

  it("clamps lastUsedLocation when locations shrink", async () => {
    const client = await signedIn()
    const p = await client.projects.createProject({
      name: "multi",
      locations: [mbp("/a"), mbp("/b"), mbp("/c")],
    })
    await client.sessions.createSession({ projectId: p.projectId, locationIndex: 2 })
    const shrunk = await client.projects.updateProject({
      projectId: p.projectId,
      locations: [mbp("/x"), mbp("/y")],
    })
    expect(shrunk.lastUsedLocation).toBe(1)
    // A surviving location keeps being the default even if it moved.
    const moved = await client.projects.updateProject({
      projectId: p.projectId,
      locations: [mbp("/y"), mbp("/z"), mbp("/w")],
    })
    expect(moved.lastUsedLocation).toBe(0)
  })

  it("returns not_found for unknown projects", async () => {
    const client = await signedIn()
    await expectCode(client.projects.getProject({ projectId: "missing" }), "not_found")
    await expectCode(client.sessions.listSessions({ projectId: "missing" }), "not_found")
  })

  it("refuses to delete a project with sessions that are not stopped", async () => {
    const client = await signedIn()
    await expectCode(client.projects.deleteProject({ projectId: "circus" }), "failed_precondition")
  })

  it("deletes a project whose sessions are all stopped, with its sessions", async () => {
    const state = seedState()
    for (const s of state.sessions) if (s.projectId === "toolbox") s.state = "stopped"
    const { client } = backend(state)
    await client.auth.login(eric)
    await client.projects.deleteProject({ projectId: "toolbox" })
    await expectCode(client.projects.getProject({ projectId: "toolbox" }), "not_found")
    const sessions = await client.sessions.listSessions({})
    expect(sessions.some((s) => s.projectId === "toolbox")).toBe(false)
  })
})

describe("sessions", () => {
  it("lists newest activity first, including stopped sessions", async () => {
    const client = await signedIn()
    const sessions = await client.sessions.listSessions({ projectId: "circus" })
    const times = sessions.map((s) => s.lastActivityAtUnixMs)
    expect(times).toEqual([...times].sort((a, b) => b - a))
    expect(sessions.some((s) => s.state === "stopped")).toBe(true)
  })

  it("creates a session at a location and makes it the project's default", async () => {
    const client = await signedIn()
    const before = await client.projects.getProject({ projectId: "circus" })
    expect(before.lastUsedLocation).toBe(0)
    const session = await client.sessions.createSession({ projectId: "circus", locationIndex: 1 })
    expect(session).toMatchObject({
      projectId: "circus",
      state: "idle",
      device: before.locations[1].device,
      cwd: before.locations[1].path,
    })
    const after = await client.projects.getProject({ projectId: "circus" })
    expect(after.lastUsedLocation).toBe(1)
    expect((await client.sessions.listSessions({}))[0].sessionId).toBe(session.sessionId)
  })

  it("rejects a location index out of range", async () => {
    const client = await signedIn()
    await expectCode(client.sessions.createSession({ projectId: "circus", locationIndex: 5 }), "invalid_argument")
    await expectCode(client.sessions.createSession({ projectId: "circus", locationIndex: -1 }), "invalid_argument")
  })

  it("records simulated activity", async () => {
    const mock = backend()
    await mock.client.auth.login(eric)
    mock.conversations.reportActivity("web-vite", { state: "running" })
    const [latest] = await mock.client.sessions.listSessions({})
    expect(latest).toMatchObject({ sessionId: "web-vite", state: "running" })
  })
})

describe("conversations", () => {
  it("keeps transcripts with the store", () => {
    const state = seedState()
    const mock = backend(state)
    const events = mock.conversations.load()["web-vite"]
    const added = [...events, { kind: "assistant_text" as const, id: "x", at: "", text: "hi" }]
    mock.conversations.save("web-vite", added)
    expect(backend(structuredClone(state)).conversations.load()["web-vite"]).toEqual(added)
  })

  const inFlight = (mock: ReturnType<typeof backend>) => {
    mock.conversations.reportActivity("web-vite", { state: "running" })
    mock.conversations.save("web-vite", [
      { kind: "assistant_text", id: "a", at: "", text: "par", streaming: true },
      { kind: "tool_call", id: "t", at: "", tool: "Bash", input: {}, status: "running" },
    ])
  }

  it("settles abandoned in-flight work on load", () => {
    const state = seedState()
    inFlight(backend(state))
    settleInFlight(state, "other-tab", Date.now() + STALE_AFTER_MS)
    expect(state.sessions.find((s) => s.sessionId === "web-vite")?.state).toBe("interrupted")
    expect(state.sessions.some((s) => s.state === "running")).toBe(false)
    const [text, tool] = state.conversations["web-vite"]
    expect(text).toMatchObject({ streaming: false, text: "par" })
    expect(tool).toMatchObject({ status: "error" })
  })

  it("leaves work another tab is still streaming alone", () => {
    const state = seedState()
    inFlight(backend(state))
    settleInFlight(state, "other-tab", Date.now() + 1000)
    expect(state.sessions.find((s) => s.sessionId === "web-vite")?.state).toBe("running")
    expect(state.conversations["web-vite"][0]).toMatchObject({ streaming: true })
    // The seed's running session has never been written by a live tab.
    expect(state.sessions.find((s) => s.sessionId === "store-fencing")?.state).toBe("interrupted")
  })

  it("settles this tab's own in-flight work on reload, however recent", () => {
    const state = seedState()
    inFlight(createMockBackend({ state, latency: () => 0, tabId: "this-tab" }))
    settleInFlight(state, "this-tab", Date.now() + 1000)
    expect(state.sessions.find((s) => s.sessionId === "web-vite")?.state).toBe("interrupted")
    expect(state.conversations["web-vite"][0]).toMatchObject({ streaming: false })
  })

  it("settles one session on request", () => {
    const state = seedState()
    const mock = backend(state)
    inFlight(mock)
    mock.conversations.settle("web-vite")
    expect(state.sessions.find((s) => s.sessionId === "web-vite")?.state).toBe("interrupted")
    expect(mock.conversations.load()["web-vite"][1]).toMatchObject({ status: "error" })
  })

  it("does not keep large attachment data", () => {
    const state = seedState()
    const mock = backend(state)
    const big = `data:image/png;base64,${"A".repeat(MAX_KEPT_ATTACHMENT_URL + 1)}`
    const small = "data:image/svg+xml,<svg/>"
    const attachment = (id: string, url: string) => ({ id, filename: `${id}.png`, mediaType: "image/png", url })
    mock.conversations.save("web-vite", [
      { kind: "user_message", id: "u", at: "", text: "look", attachments: [attachment("big", big), attachment("small", small)] },
    ])
    const [message] = mock.conversations.load()["web-vite"]
    expect(message.kind === "user_message" && message.attachments?.map((a) => a.url)).toEqual(["", small])
  })

  it("tells the app when another tab changed its data, not for transcripts alone", () => {
    const state = seedState()
    const mock = backend(state)
    let calls = 0
    mock.onExternalChange(() => calls++)
    const transcriptOnly = structuredClone(state)
    transcriptOnly.conversations["web-vite"] = []
    mock.adopt(transcriptOnly)
    expect(calls).toBe(0)
    const fewerLocations = structuredClone(state)
    fewerLocations.projects[0].locations.pop()
    mock.adopt(fewerLocations)
    expect(calls).toBe(1)
  })

  it("tells subscribers which transcripts another tab changed", () => {
    const state = seedState()
    const mock = backend(state)
    const seen: string[][] = []
    mock.conversations.subscribe((ids) => seen.push(ids))
    const next = structuredClone(state)
    next.conversations["web-vite"] = []
    mock.adopt(next)
    expect(seen).toEqual([["web-vite"]])
    expect(mock.conversations.load()["web-vite"]).toEqual([])
  })

  it("drops a deleted project's transcripts", async () => {
    const state = seedState()
    for (const s of state.sessions) if (s.projectId === "toolbox") s.state = "stopped"
    const { client } = backend(state)
    await client.auth.login(eric)
    await client.projects.deleteProject({ projectId: "toolbox" })
    expect(state.conversations["dotfiles-go"]).toBeUndefined()
  })
})

it("does not share objects with the store", async () => {
  const client = await signedIn()
  const [project] = await client.projects.listProjects()
  project.name = "mutated"
  project.locations.push(mbp("/elsewhere"))
  const again = await client.projects.getProject({ projectId: project.projectId })
  expect(again.name).not.toBe("mutated")
  expect(again.locations).toHaveLength(2)
})

describe("persistence", () => {
  it("round-trips the state", () => {
    const state = seedState()
    expect(parsePersisted(serialize(state))).toEqual(JSON.parse(JSON.stringify(state)))
  })

  it("drops a blob from the previous version instead of crashing", () => {
    const v1 = structuredClone(seedState()) as Partial<MockState>
    delete v1.conversations
    delete v1.writtenAtMs
    delete v1.writtenBy
    expect(parsePersisted(JSON.stringify({ version: 1, state: v1 }))).toBeNull()
    // Same shape problem even if a blob claims the current version.
    expect(parsePersisted(JSON.stringify({ version: VERSION, state: v1 }))).toBeNull()
  })

  it("drops malformed blobs", () => {
    for (const raw of [null, "", "{", "null", "42", "[]", '{"version":2}', '{"version":2,"state":{"users":"x"}}']) {
      expect(parsePersisted(raw)).toBeNull()
    }
  })
})
