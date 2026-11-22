import type { Device, Location, LoginMethods, ProviderScheme, Role, Worker } from "@/api/types"
import { projects as seedProjects, queueDemoSessions, sessions as seedSessionList, type SeedSession } from "@/mock/data"

import { seedSession, type MockSession } from "./sessions"

/** The setup token of a fresh mock server, as printed in its "terminal". */
export const DEV_SETUP_TOKEN = "dev-setup-token"

/** How the mock server says users can sign in. */
export const MOCK_LOGIN_METHODS: LoginMethods = {
  password: true,
  oidc: { displayName: "Authelia" },
}

/** The demo accounts; both use the password `circus`. */
export const SEED_SUPER_EMAIL = "eric@example.com"
export const SEED_USER_EMAIL = "dev@example.com"
/** The user id of the seeded super user, who owns the demo data. */
export const SEED_OWNER_ID = "user-eric"
/** Who setup through the mock's OIDC provider creates. */
export const OIDC_SETUP_EMAIL = "oidc-admin@example.com"

export interface MockUser {
  userId: string
  email: string
  /** Null for accounts that only sign in through OIDC. */
  password: string | null
  role: Role
}

/**
 * A stored project. Locations keep only what a request sets; the default
 * directory and the revoked mark are worked out when it is returned.
 */
export interface MockProject {
  owner: string
  projectId: string
  name: string
  repoUrl: string
  /**
   * Bound to a device by its id: a device revoked and enrolled again under
   * the same name is another device, and the location stays revoked until
   * it is saved again.
   */
  locations: MockLocation[]
  lastUsedLocation: number
  createdAtUnixMs: number
  /** Moves forward whenever the name, the locations, the repository URL or the default provider change. */
  updatedAtUnixMs: number
  /** Empty or missing: none. May name a deleted provider. */
  defaultProviderId?: string
}

export type MockLocation = Location & { deviceId: string }

/** An enrolled device. `deviceId` is unique; a name can be enrolled again. */
export type MockDevice = Device & { owner: string; deviceId: string }

/** A stored model provider, secrets in the clear (the mock has nothing to protect). */
export interface MockProvider {
  owner: string
  providerId: string
  name: string
  baseUrl: string
  scheme: ProviderScheme
  key: string
  opusModel: string
  sonnetModel: string
  haikuModel: string
  env: { name: string; value: string }[]
  workerIds: string[]
  maxSessions: number
  isDefault: boolean
  /** Stands in for a lost encryption key: the secrets above count as unreadable. */
  secretsUnreadable: boolean
  createdAtUnixMs: number
  updatedAtUnixMs: number
}

/** An enrolled worker. */
export type MockWorker = Worker & { owner: string }

/**
 * Everything the mock server knows. Plain JSON so it can be persisted;
 * sessions are not (they live in this tab's memory, see `persist.ts`).
 */
export interface MockState {
  /** Accepted until a super user exists; null on the seeded server. */
  setupToken: string | null
  users: MockUser[]
  /** The signed-in user id; stands in for the session cookie. */
  cookie: string | null
  projects: MockProject[]
  sessions: MockSession[]
  devices: MockDevice[]
  /** In this tab's memory only, like the sessions. */
  providers: MockProvider[]
  /** In this tab's memory only, like the sessions. */
  workers: MockWorker[]
}

/** An uninitialized server: no users, no data. */
export function freshState(): MockState {
  return {
    setupToken: DEV_SETUP_TOKEN,
    users: [],
    cookie: null,
    projects: [],
    sessions: [],
    devices: [],
    providers: [],
    workers: [],
  }
}

const MINUTE = 60_000
const DAY = 24 * 60 * MINUTE

/**
 * A set-up server, signed out, with the super user `eric@example.com` (who
 * owns the demo data) and the user `dev@example.com`, both with password
 * `circus`.
 */
export function seedState(now: number = Date.now()): MockState {
  const owner = SEED_OWNER_ID
  const devices: MockState["devices"] = [
    { name: "devbox", online: true, lastSeenUnixMs: now, os: "linux", arch: "amd64" },
    { name: "eric-mbp", online: true, lastSeenUnixMs: now, os: "darwin", arch: "arm64" },
    { name: "eric-win", online: false, lastSeenUnixMs: now - 2 * DAY, os: "windows", arch: "amd64" },
    { name: "palace-builder", online: true, lastSeenUnixMs: now, os: "linux", arch: "arm64" },
  ].map((d) => ({ ...d, owner, version: "0.1.0", deviceId: `device-${d.name}` }))

  const projects: MockState["projects"] = seedProjects.map((p, i) => {
    const latest = seedSessionList
      .filter((s) => s.projectId === p.id)
      .sort((a, b) => a.minutesAgo - b.minutesAgo)[0]
    const lastUsed = latest ? p.locations.findIndex((l) => l.device === latest.device) : 0
    return {
      owner,
      projectId: p.id,
      name: p.name,
      locations: p.locations.map((l) => ({ ...l, deviceId: `device-${l.device}` })),
      lastUsedLocation: Math.max(0, lastUsed),
      repoUrl: "",
      createdAtUnixMs: now - (30 - i * 7) * DAY,
      updatedAtUnixMs: now - (30 - i * 7) * DAY,
    }
  })

  const state: MockState = {
    setupToken: null,
    users: [
      { userId: owner, email: SEED_SUPER_EMAIL, password: "circus", role: "super" },
      { userId: "user-dev", email: SEED_USER_EMAIL, password: "circus", role: "user" },
    ],
    cookie: null,
    projects,
    sessions: [],
    devices,
    providers: seedProviders(now),
    workers: seedWorkers(now),
  }
  state.sessions = seedSessions(state, now)
  return state
}

/** The demo owner's workers: one beside each of three devices, and one on a machine of its own. */
export function seedWorkers(now: number = Date.now()): MockWorker[] {
  return [
    { name: "cloud-runner", online: true, lastSeenUnixMs: now, os: "linux", arch: "amd64", localDevices: [] },
    { name: "devbox", online: true, lastSeenUnixMs: now, os: "linux", arch: "amd64", localDevices: ["devbox"] },
    { name: "eric-mbp", online: true, lastSeenUnixMs: now, os: "darwin", arch: "arm64", localDevices: ["eric-mbp"] },
    { name: "eric-win", online: false, lastSeenUnixMs: now - 2 * DAY, os: "windows", arch: "amd64", localDevices: ["eric-win"] },
  ].map((w) => ({ ...w, owner: SEED_OWNER_ID, workerId: `worker-${w.name}`, hostname: w.name, version: "0.1.0" }))
}

/**
 * The demo owner's model providers, with fake keys: the default one on
 * every online worker, a proxy limited to two sessions on devbox, and one
 * whose secrets can't be read any more.
 */
export function seedProviders(now: number = Date.now()): MockProvider[] {
  const base = {
    owner: SEED_OWNER_ID,
    baseUrl: "",
    scheme: "x_api_key" as const,
    opusModel: "",
    sonnetModel: "",
    haikuModel: "",
    env: [],
    maxSessions: 0,
    isDefault: false,
    secretsUnreadable: false,
    createdAtUnixMs: now - 20 * DAY,
    updatedAtUnixMs: now - 20 * DAY,
  }
  return [
    {
      ...base,
      providerId: "provider-anthropic",
      name: "Anthropic",
      key: "sk-ant-mock-not-a-real-key-7Qa2",
      workerIds: ["worker-cloud-runner", "worker-devbox", "worker-eric-mbp"],
      isDefault: true,
    },
    {
      ...base,
      providerId: "provider-proxy",
      name: "Team proxy",
      baseUrl: "https://llm-proxy.example.com/anthropic",
      scheme: "bearer",
      key: "mock-proxy-token-k9Qz",
      opusModel: "proxy-opus-large",
      sonnetModel: "proxy-sonnet-medium",
      haikuModel: "proxy-haiku-small",
      env: [
        { name: "DISABLE_TELEMETRY", value: "1" },
        { name: "PROXY_TEAM", value: "platform-web" },
      ],
      workerIds: ["worker-devbox"],
      maxSessions: 2,
    },
    {
      ...base,
      providerId: "provider-legacy",
      name: "Legacy key",
      key: "sk-ant-mock-lost-key-0000-u1Rw",
      env: [{ name: "LEGACY_REGION", value: "eu-west" }],
      workerIds: ["worker-devbox"],
      secretsUnreadable: true,
    },
  ]
}

/**
 * The demo sessions, for the seeded projects the demo data's owner still
 * has at a location on the session's device.
 */
export function seedSessions(
  state: Pick<MockState, "projects">,
  now: number = Date.now(),
  seeds: readonly SeedSession[] = seedSessionList,
): MockSession[] {
  return seeds.flatMap((seed) => {
    const project = state.projects.find((p) => p.projectId === seed.projectId && p.owner === SEED_OWNER_ID)
    const location = project?.locations.find((l) => l.device === seed.device)
    if (!location) return []
    return [seedSession(seed, location.path, now)]
  })
}

/**
 * Adds the queue demo: sessions of the demo projects that wait, one for
 * each reason a session can't be leased (`?mock=queue`).
 */
export function addQueueDemo(state: MockState, now: number = Date.now()) {
  state.sessions.push(...seedSessions(state, now, queueDemoSessions))
  const proxy = state.providers.find((p) => p.providerId === "provider-proxy")
  if (proxy) proxy.maxSessions = 1
}

const isRecord = (v: unknown): v is Record<string, unknown> =>
  typeof v === "object" && v !== null && !Array.isArray(v)
const isNullableString = (v: unknown) => v === null || typeof v === "string"

/** A minimal shape check, so a stale or corrupt blob is dropped, not run. */
export function isMockState(v: unknown): v is MockState {
  return (
    isRecord(v) &&
    isNullableString(v.setupToken) &&
    isNullableString(v.cookie) &&
    Array.isArray(v.users) &&
    Array.isArray(v.projects) &&
    Array.isArray(v.devices)
  )
}
