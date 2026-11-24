import { describe, expect, it } from "vitest"

import { ApiError, type ErrorCode } from "@/api/errors"
import type { CircusClient } from "@/api/types"

import { createMockBackend, LOGIN_FAILURE_WINDOW_MS, MAX_LOGIN_FAILURES } from "./backend"
import { parsePersisted, serialize, VERSION } from "./persist"
import {
  DEV_SETUP_TOKEN,
  freshState,
  MOCK_LOGIN_METHODS,
  OIDC_SETUP_EMAIL,
  SEED_OWNER_ID,
  seedState,
  type MockState,
} from "./state"

/** The state, signed in as the demo data's owner. */
function asOwner(state: MockState): MockState {
  state.cookie = SEED_OWNER_ID
  return state
}

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
    // The server names the field, as it does for a missing token.
    expect(await fieldOf(client.auth.setup({ ...setupReq, token: "nope" }))).toBe("token")
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
    await expectCode(client.projects.createProject({ repoUrl: "", name: "x", locations: [mbp("/x")] }), "unauthenticated")
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
    await client.projects.createProject({ repoUrl: "", name: "aardvark", locations: [mbp("/src/a")] })
    const names = (await client.projects.listProjects()).map((p) => p.name)
    expect(names).toEqual(["aardvark", "circus", "palace", "toolbox"])
  })

  it("trims names and keeps them unique regardless of case", async () => {
    const client = await signedIn()
    const created = await client.projects.createProject({ repoUrl: "", name: "  web  ", locations: [mbp("/src/web")] })
    expect(created.name).toBe("web")
    expect(created.lastUsedLocation).toBe(0)
    await expectCode(
      client.projects.createProject({ repoUrl: "", name: "Circus", locations: [mbp("/src/c")] }),
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
      await expectCode(client.projects.createProject({ name, repoUrl: "", locations: [mbp("/src")] }), "invalid_argument")
    }
    await client.projects.createProject({ repoUrl: "", name: "x".repeat(64), locations: [mbp("/src")] })
  })

  it("validates locations", async () => {
    const client = await signedIn()
    const create = (locations: { device: string; path: string }[]) =>
      client.projects.createProject({ repoUrl: "", name: "new", locations })
    await expectCode(create(Array.from({ length: 17 }, (_, i) => mbp(`/src/${i}`))), "invalid_argument")
    await expectCode(create([{ device: "ghost", path: "/src" }]), "invalid_argument")
    await expectCode(create([mbp("src/relative")]), "invalid_argument")
    await expectCode(create([{ device: "eric-win", path: "/not/windows" }]), "invalid_argument")
    await expectCode(create([mbp("/src/a"), mbp("/src/a/")]), "invalid_argument")
    const ok = await create([mbp("/src/a/"), { device: "eric-win", path: "D:\\work" }])
    expect(ok.locations).toMatchObject([mbp("/src/a"), { device: "eric-win", path: "D:\\work" }])
  })

  it("normalizes Windows paths before checking for duplicates", async () => {
    const client = await signedIn()
    const win = (path: string) => ({ device: "eric-win", path })
    await expectCode(
      client.projects.createProject({ repoUrl: "", name: "w", locations: [win("D:\\work"), win("d:/work/")] }),
      "invalid_argument",
    )
    const p = await client.projects.createProject({ repoUrl: "", name: "w", locations: [win("c:/src/app")] })
    expect(p.locations).toMatchObject([win("C:\\src\\app")])
  })

  it("names the field each validation error is about", async () => {
    const client = await signedIn()
    expect(await fieldOf(client.projects.createProject({ repoUrl: "", name: "", locations: [mbp("/a")] }))).toBe("name")
    expect(await fieldOf(client.projects.createProject({ repoUrl: "", name: "circus", locations: [mbp("/a")] }))).toBe("name")
    const many = Array.from({ length: 17 }, (_, i) => mbp(`/${i}`))
    expect(await fieldOf(client.projects.createProject({ repoUrl: "", name: "n", locations: many }))).toBe("locations")
    expect(await fieldOf(client.projects.createProject({ repoUrl: "x", name: "n", locations: [] }))).toBe("repo_url")
    expect(
      await fieldOf(client.projects.createProject({ repoUrl: "", name: "n", locations: [mbp("/a"), { device: "x", path: "/a" }] })),
    ).toBe("locations[1].device")
    expect(await fieldOf(client.projects.createProject({ repoUrl: "", name: "n", locations: [mbp("rel")] }))).toBe(
      "locations[0].path",
    )
    expect(await fieldOf(client.projects.createProject({ repoUrl: "", name: "n", locations: [mbp("/a"), mbp("/a")] }))).toBe(
      "locations[1].path",
    )
  })

  it("clamps lastUsedLocation when locations shrink", async () => {
    const client = await signedIn()
    const p = await client.projects.createProject({
      repoUrl: "",
      name: "multi",
      locations: [mbp("/a"), mbp("/b"), mbp("/c")],
    })
    await client.sessions.createSession({ projectId: p.projectId, locationIndex: 2, clientRequestId: crypto.randomUUID() })
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

describe("the project model", () => {
  const create = (client: CircusClient, locations: { device: string; path: string }[], repoUrl = "") =>
    client.projects.createProject({ name: "model", repoUrl, locations })

  it("takes an optional repository URL, trimmed, and projects without locations", async () => {
    const client = await signedIn()
    const p = await create(client, [], " git@github.com:owner/repo.git ")
    expect(p).toMatchObject({ repoUrl: "git@github.com:owner/repo.git", locations: [], lastUsedLocation: 0 })
    const cleared = await client.projects.updateProject({ projectId: p.projectId, repoUrl: "" })
    expect(cleared.repoUrl).toBe("")
    await expectCode(
      client.projects.updateProject({ projectId: p.projectId, repoUrl: "github.com/owner/repo" }),
      "invalid_argument",
    )
  })

  it("resolves a location without a path to the default directory, one per device", async () => {
    const client = await signedIn()
    const p = await create(client, [mbp(""), mbp("/src"), { device: "devbox", path: "  " }])
    expect(p.locations).toEqual([
      { device: "eric-mbp", path: "", effectivePath: `~/circus/projects/${p.projectId}`, deviceRevoked: false },
      { device: "eric-mbp", path: "/src", effectivePath: "", deviceRevoked: false },
      { device: "devbox", path: "", effectivePath: `~/circus/projects/${p.projectId}`, deviceRevoked: false },
    ])
    const twice = client.projects.createProject({ name: "twice", repoUrl: "", locations: [mbp(""), mbp(" ")] })
    expect(await fieldOf(twice)).toBe("locations[1].path")
  })

  it("marks locations whose device is gone", async () => {
    const state = seedState()
    const { client } = createMockBackend({ state: asOwner(state), latency: () => 0 })
    const p = await create(client, [mbp("/a"), { device: "devbox", path: "/b" }])
    state.devices = state.devices.filter((d) => d.name !== "devbox")
    const again = await client.projects.getProject({ projectId: p.projectId })
    expect(again.locations.map((l) => l.deviceRevoked)).toEqual([false, true])
    // Starting a session there is refused; saving it again needs another device.
    await expectCode(client.sessions.createSession({ projectId: p.projectId, locationIndex: 1, clientRequestId: crypto.randomUUID() }), "invalid_argument")
    expect(
      await fieldOf(client.projects.updateProject({ projectId: p.projectId, locations: again.locations })),
    ).toBe("locations[1].device")
  })

  it("refuses a change made on an older version, but not a new last used location", async () => {
    let clock = 1_000
    const { client } = createMockBackend({
      state: asOwner(seedState()),
      latency: () => 0,
      now: () => clock,
    })
    const p = await create(client, [mbp("/a"), mbp("/b")])
    expect(p.updatedAtUnixMs).toBe("1000")
    // Two changes in the same millisecond still get two versions.
    const renamed = await client.projects.updateProject({
      projectId: p.projectId,
      name: "renamed",
      expectedUpdatedAt: p.updatedAtUnixMs,
    })
    expect(renamed.updatedAtUnixMs).toBe("1001")
    await expectCode(
      client.projects.updateProject({ projectId: p.projectId, name: "again", expectedUpdatedAt: p.updatedAtUnixMs }),
      "aborted",
    )
    expect((await client.projects.getProject({ projectId: p.projectId })).name).toBe("renamed")
    // Setting only the last used location is never refused, and is no new version.
    clock = 5_000
    const used = await client.projects.updateProject({
      projectId: p.projectId,
      lastUsedLocation: 1,
      expectedUpdatedAt: p.updatedAtUnixMs,
    })
    expect(used).toMatchObject({ lastUsedLocation: 1, updatedAtUnixMs: "1001" })
    // Saving the same values is no change either.
    const same = await client.projects.updateProject({
      projectId: p.projectId,
      name: "renamed",
      repoUrl: "",
      expectedUpdatedAt: "1001",
    })
    expect(same.updatedAtUnixMs).toBe("1001")
    expect(
      await fieldOf(client.projects.updateProject({ projectId: p.projectId, lastUsedLocation: 2 })),
    ).toBe("last_used_location")
  })
})

describe("a device enrolled again under a removed device's name", () => {
  it("leaves the location revoked until it is saved again, which binds the new device", async () => {
    const state = seedState()
    const { client } = createMockBackend({ state: asOwner(state), latency: () => 0 })
    const p = await client.projects.createProject({ name: "rebind", repoUrl: "", locations: [{ device: "devbox", path: "/b" }] })
    // devbox is revoked, and a new device joins with the same name.
    state.devices.find((d) => d.name === "devbox")!.deviceId = "device-devbox-2"
    const revoked = await client.projects.getProject({ projectId: p.projectId })
    expect(revoked.locations[0].deviceRevoked).toBe(true)
    await expectCode(client.sessions.createSession({ projectId: p.projectId, locationIndex: 0, clientRequestId: crypto.randomUUID() }), "invalid_argument")
    const rebound = await client.projects.updateProject({
      projectId: p.projectId,
      locations: revoked.locations,
      expectedUpdatedAt: revoked.updatedAtUnixMs,
    })
    expect(rebound.locations[0].deviceRevoked).toBe(false)
    expect(await client.sessions.createSession({ projectId: p.projectId, locationIndex: 0, clientRequestId: crypto.randomUUID() })).toMatchObject({
      device: "devbox",
      cwd: "/b",
    })
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
    const session = await client.sessions.createSession({ projectId: "circus", locationIndex: 1, clientRequestId: crypto.randomUUID() })
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
    await expectCode(client.sessions.createSession({ projectId: "circus", locationIndex: 5, clientRequestId: crypto.randomUUID() }), "invalid_argument")
    await expectCode(client.sessions.createSession({ projectId: "circus", locationIndex: -1, clientRequestId: crypto.randomUUID() }), "invalid_argument")
  })

  it("returns the first session to a retry with the same key, and refuses the key for another request", async () => {
    const client = await signedIn()
    const req = { projectId: "circus", locationIndex: 1, clientRequestId: crypto.randomUUID() }
    const [first, concurrent] = await Promise.all([client.sessions.createSession(req), client.sessions.createSession(req)])
    expect(concurrent.sessionId).toBe(first.sessionId)
    expect((await client.sessions.createSession(req)).sessionId).toBe(first.sessionId)
    const sessions = await client.sessions.listSessions({ projectId: "circus" })
    expect(sessions.filter((s) => s.sessionId === first.sessionId)).toHaveLength(1)

    await expectCode(client.sessions.createSession({ ...req, locationIndex: 0 }), "already_exists")
    // The key belongs to its project; another one starts a session of its own.
    const other = await client.sessions.createSession({ ...req, clientRequestId: crypto.randomUUID() })
    expect(other.sessionId).not.toBe(first.sessionId)
    await expectCode(client.sessions.createSession({ ...req, clientRequestId: "" }), "invalid_argument")
  })

})

it("does not share objects with the store", async () => {
  const client = await signedIn()
  const [project] = await client.projects.listProjects()
  project.name = "mutated"
  project.locations.push({ ...mbp("/elsewhere"), effectivePath: "", deviceRevoked: false })
  const again = await client.projects.getProject({ projectId: project.projectId })
  expect(again.name).not.toBe("mutated")
  expect(again.locations).toHaveLength(2)
})

describe("persistence", () => {
  it("round-trips the state without sessions, providers and workers, which start again from the demo seed", () => {
    const state = seedState()
    const persisted = JSON.parse(serialize(state)).state
    expect([persisted.sessions, persisted.providers, persisted.workers]).toEqual([undefined, undefined, undefined])
    const loaded = parsePersisted(serialize(state))!
    const { sessions: loadedSessions, providers: loadedProviders, workers: loadedWorkers, ...rest } = loaded
    const { sessions, providers, workers, ...expected } = state
    expect(rest).toEqual(JSON.parse(JSON.stringify(expected)))
    expect(loadedSessions.map((s) => s.sessionId)).toEqual(sessions.map((s) => s.sessionId))
    expect(loadedProviders.map((p) => p.providerId)).toEqual(providers.map((p) => p.providerId))
    expect(loadedWorkers.map((w) => w.workerId)).toEqual(workers.map((w) => w.workerId))
  })

  it("starts no demo sessions for demo projects that are gone", () => {
    const state = seedState()
    state.projects = state.projects.filter((p) => p.projectId !== "toolbox")
    const loaded = parsePersisted(serialize(state))!
    expect(loaded.sessions.some((s) => s.projectId === "toolbox")).toBe(false)
    expect(loaded.sessions.length).toBeGreaterThan(0)
  })

  it("drops a blob from the previous version instead of crashing", () => {
    const v1 = structuredClone(seedState()) as Partial<MockState>
    delete v1.devices
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

describe("model providers", () => {
  const base = {
    name: "Staging",
    baseUrl: "",
    scheme: "x_api_key" as const,
    key: "sk-staging-1234",
    opusModel: "",
    sonnetModel: "",
    haikuModel: "",
    env: [{ name: "REGION", value: "eu" }],
    workerIds: ["worker-devbox"],
    maxSessions: 0,
    isDefault: false,
  }

  it("never return secrets, only their last four characters", async () => {
    const { client } = backend(asOwner(seedState()))
    const created = await client.modelProviders.createProvider(base)
    expect(created.keyLast4).toBe("1234")
    expect(created.env).toEqual([{ name: "REGION", valueLast4: "eu" }])
    expect(JSON.stringify(await client.modelProviders.listProviders())).not.toContain("sk-staging")
  })

  it("keep the stored key and values a write leaves out, and check the version", async () => {
    const state = asOwner(seedState())
    const { client } = backend(state)
    const p = await client.modelProviders.createProvider(base)
    const update = { ...base, providerId: p.providerId, key: "", env: [{ name: "REGION" }] }
    await client.modelProviders.updateProvider({ ...update, name: "Staging 2", expectedUpdatedAt: p.updatedAtUnixMs })
    const stored = state.providers.find((x) => x.providerId === p.providerId)!
    expect([stored.key, stored.env]).toEqual(["sk-staging-1234", [{ name: "REGION", value: "eu" }]])
    await expectCode(client.modelProviders.updateProvider({ ...update, expectedUpdatedAt: p.updatedAtUnixMs }), "aborted")
    expect(await fieldOf(client.modelProviders.updateProvider({ ...update, env: [{ name: "OTHER" }] }))).toBe(
      "env[0].value",
    )
  })

  it("can't keep secrets that can't be read", async () => {
    const { client } = backend(asOwner(seedState()))
    const legacy = await client.modelProviders.getProvider({ providerId: "provider-legacy" })
    const update = { ...base, providerId: legacy.providerId, name: legacy.name, env: [{ name: "LEGACY_REGION" }] }
    expect(await fieldOf(client.modelProviders.updateProvider({ ...update, key: "" }))).toBe("api.key")
    expect(await fieldOf(client.modelProviders.updateProvider(update))).toBe("env[0].value")
    const fixed = await client.modelProviders.updateProvider({ ...update, env: [{ name: "LEGACY_REGION", value: "eu" }] })
    expect(fixed.secretsUnreadable).toBe(false)
  })

  it("resolve a new session's provider: asked for, else the project's default, else the user's", async () => {
    const state = asOwner(seedState())
    const { client } = backend(state)
    const create = (key: string, providerId?: string) =>
      client.sessions.createSession({ projectId: "circus", locationIndex: 0, clientRequestId: key, providerId })
    expect((await create("a")).providerId).toBe("provider-anthropic")
    expect((await create("b", "")).providerId).toBe("")
    await client.projects.updateProject({ projectId: "circus", defaultProviderId: "provider-proxy" })
    expect((await create("c")).providerId).toBe("provider-proxy")
    // A default naming a deleted provider counts as unset.
    state.providers = state.providers.filter((p) => p.providerId !== "provider-proxy")
    expect((await create("d")).providerId).toBe("provider-anthropic")
    expect(await fieldOf(create("e", "provider-proxy"))).toBe("provider_id")
  })

  it("refuse deleting a provider while a session using it holds a lease", async () => {
    const state = asOwner(seedState())
    const { client } = backend(state)
    const session = state.sessions.find((s) => s.sessionId === "web-vite")!
    Object.assign(session, { providerId: "provider-proxy", leased: true })
    await expectCode(client.modelProviders.deleteProvider({ providerId: "provider-proxy" }), "failed_precondition")
    await client.sessions.updateSession({ sessionId: "web-vite", providerId: "provider-anthropic" })
    expect(session.leased).toBe(false)
    await client.modelProviders.deleteProvider({ providerId: "provider-proxy" })
    expect(state.providers.some((p) => p.providerId === "provider-proxy")).toBe(false)
  })
})

describe("worker grants", () => {
  it("seeds one integrator", async () => {
    const client = await signedIn()
    const workers = await client.machines.listWorkers()
    expect(workers.filter((w) => w.integrator).map((w) => w.name)).toEqual(["cloud-runner"])
    expect(workers.length).toBeGreaterThan(1)
  })

  it("turns the grant on and off and returns the worker as listed", async () => {
    const client = await signedIn()
    const on = await client.machines.setWorkerGrant({ workerId: "worker-devbox", integrator: true })
    const listed = await client.machines.listWorkers()
    expect(on).toEqual(listed.find((w) => w.workerId === "worker-devbox"))
    expect(on.integrator).toBe(true)
    const off = await client.machines.setWorkerGrant({ workerId: "worker-devbox", integrator: false })
    expect(off.integrator).toBe(false)
    expect((await client.machines.listWorkers()).find((w) => w.workerId === "worker-devbox")?.integrator).toBe(false)
  })

  it("rejects an unknown worker and another user's", async () => {
    const { client } = backend()
    await client.auth.login(dev)
    await expectCode(client.machines.setWorkerGrant({ workerId: "worker-devbox", integrator: true }), "not_found")
    await expectCode(client.machines.setWorkerGrant({ workerId: "nope", integrator: true }), "not_found")
    await client.auth.logout()
    await client.auth.login(eric)
    const devbox = (await client.machines.listWorkers()).find((w) => w.workerId === "worker-devbox")
    expect(devbox?.integrator).toBe(false)
  })

  it("needs a signed-in user", async () => {
    const { client } = backend()
    await expectCode(client.machines.setWorkerGrant({ workerId: "worker-devbox", integrator: true }), "unauthenticated")
  })
})
