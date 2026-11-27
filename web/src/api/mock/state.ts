import type { Device, Location, LoginMethods, ProviderScheme, Role, Worker, WorkerAgent } from "@/api/types"
import {
  projects as seedProjects,
  queueDemoSessions,
  sessions as seedSessionList,
  workerDemoSessions,
  type SeedSession,
} from "@/mock/data"

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
export type MockDevice = Device & { owner: string }

/** A stored model provider, secrets in the clear (the mock has nothing to protect). */
export interface MockProvider {
  owner: string
  providerId: string
  name: string
  baseUrl: string
  scheme: ProviderScheme
  key: string
  fableModel: string
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
const HOUR = 60 * MINUTE
const DAY = 24 * HOUR

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

/** The Claude Code version the demo workers' builds run. */
export const MOCK_AGENT_VERSION = "2.1.3"

/** The size of the demo Claude Code download, for install progress. */
export const MOCK_AGENT_BYTES = 74_500_000

/** A demo worker's Claude Code entry; installed at the target version, reported at `reportedAtUnixMs`, unless overridden. */
export function mockAgent(reportedAtUnixMs: number, fields: Partial<WorkerAgent> = {}): WorkerAgent {
  return {
    agent: "claude_code",
    state: "installed",
    installedVersion: MOCK_AGENT_VERSION,
    targetVersion: MOCK_AGENT_VERSION,
    error: "",
    downloadedBytes: 0,
    totalBytes: 0,
    wanted: true,
    wantedVersion: MOCK_AGENT_VERSION,
    reportedAtUnixMs,
    ...fields,
  }
}

type SeedWorker = Omit<MockWorker, "owner" | "workerId" | "hostname" | "version" | "integrator" | "agents"> &
  Partial<Pick<MockWorker, "integrator" | "agents">>

const seedWorker = (w: SeedWorker): MockWorker => ({
  integrator: false,
  agents: [],
  ...w,
  owner: SEED_OWNER_ID,
  workerId: `worker-${w.name}`,
  hostname: w.name,
  version: "0.1.0",
})

/**
 * The demo owner's workers: one beside each of three devices, and one on a
 * machine of its own, the only integrator. All have Claude Code installed
 * (the offline one as it last reported).
 */
export function seedWorkers(now: number = Date.now()): MockWorker[] {
  return [
    { name: "cloud-runner", online: true, lastSeenUnixMs: now, os: "linux", arch: "amd64", localDevices: [], integrator: true },
    { name: "devbox", online: true, lastSeenUnixMs: now, os: "linux", arch: "amd64", localDevices: ["devbox"] },
    { name: "eric-mbp", online: true, lastSeenUnixMs: now, os: "darwin", arch: "arm64", localDevices: ["eric-mbp"] },
    { name: "eric-win", online: false, lastSeenUnixMs: now - 2 * DAY, os: "windows", arch: "amd64", localDevices: ["eric-win"] },
  ].map((w) => seedWorker({ ...w, agents: [mockAgent(w.lastSeenUnixMs)] }))
}

/**
 * Takes Claude Code off every worker, as reported (`?mock=noagents`): new
 * sessions wait for a worker to have it, and the project page says so.
 */
export function removeAgents(state: MockState) {
  for (const w of state.workers) {
    w.agents = w.agents.map((a) => ({ ...a, state: "not_installed", installedVersion: "", wanted: false, wantedVersion: "" }))
  }
}

/**
 * Adds the agents demo (`?mock=agents`): workers on machines of their own
 * (so no session is pinned to them) with Claude Code in every state: not
 * installed, downloading (with and without a known size), outdated,
 * failed, uninstalling, never reported, not supported, and offline: with a
 * wish waiting for the worker to connect, in the middle of a download, and
 * failed (installed again when it connects).
 */
export function addAgentsDemo(state: MockState, now: number = Date.now()) {
  const online = { online: true, lastSeenUnixMs: now, os: "linux", arch: "amd64", localDevices: [] }
  const offline = (ago: number) => ({ ...online, online: false, lastSeenUnixMs: now - ago })
  const notInstalled = { state: "not_installed" as const, installedVersion: "", wanted: false, wantedVersion: "" }
  state.workers.push(
    ...[
      { ...online, name: "spare-box", agents: [mockAgent(now, notInstalled)] },
      {
        ...online,
        name: "ci-runner-1",
        agents: [
          mockAgent(now, { state: "installing", installedVersion: "", downloadedBytes: 31_200_000, totalBytes: MOCK_AGENT_BYTES }),
        ],
      },
      {
        ...online,
        name: "ci-runner-2",
        arch: "arm64",
        agents: [mockAgent(now, { state: "installing", installedVersion: "", downloadedBytes: 12_800_000 })],
      },
      { ...online, name: "build-mac", os: "darwin", arch: "arm64", agents: [mockAgent(now, { state: "outdated", installedVersion: "2.0.9", wantedVersion: "2.0.9" })] },
      {
        ...online,
        name: "lab-pi",
        arch: "arm64",
        agents: [
          mockAgent(now, {
            state: "failed",
            installedVersion: "",
            error: "download failed: GET https://downloads.claude.ai/claude-code/2.1.3/linux-arm64/claude: connection reset by peer",
          }),
        ],
      },
      { ...online, name: "old-laptop", agents: [mockAgent(now, { state: "uninstalling", wanted: false, wantedVersion: "" })] },
      { ...offline(0), lastSeenUnixMs: 0, os: "", arch: "", name: "fresh-worker" },
      // Online, and its build doesn't support Claude Code: it reported no entry.
      { ...online, name: "legacy-runner" },
      // Went offline in the middle of a download.
      {
        ...offline(DAY),
        name: "field-laptop",
        agents: [
          mockAgent(now - DAY, { state: "installing", installedVersion: "", downloadedBytes: 20_100_000, totalBytes: MOCK_AGENT_BYTES }),
        ],
      },
      // Failed, still wanted: it installs again when it connects.
      {
        ...offline(2 * HOUR),
        name: "kiosk",
        agents: [mockAgent(now - 2 * HOUR, { state: "failed", installedVersion: "", error: "no space left on device" })],
      },
      { ...offline(3 * HOUR), name: "travel-mbp", os: "darwin", arch: "arm64", agents: [mockAgent(now - 3 * HOUR, { wanted: false, wantedVersion: "" })] },
      { ...offline(5 * DAY), name: "nas", agents: [mockAgent(now - 5 * DAY, { ...notInstalled, wanted: true, wantedVersion: MOCK_AGENT_VERSION })] },
    ].map(seedWorker),
  )
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
    fableModel: "",
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
      fableModel: "proxy-fable-max",
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
  // A worker online without Claude Code, the only one of a provider: that
  // provider's session waits for the agent.
  state.workers.push(
    seedWorker({
      name: "gpu-box",
      online: true,
      lastSeenUnixMs: now,
      os: "linux",
      arch: "amd64",
      localDevices: [],
      agents: [mockAgent(now, { state: "not_installed", installedVersion: "", wanted: false, wantedVersion: "" })],
    }),
  )
  const anthropic = state.providers.find((p) => p.providerId === "provider-anthropic")
  if (anthropic) {
    state.providers.push({
      ...anthropic,
      providerId: "provider-gpu",
      name: "GPU pool",
      key: "sk-ant-mock-gpu-pool-key-Hc4t",
      workerIds: ["worker-gpu-box"],
      isDefault: false,
    })
  }
  state.sessions.push(...seedSessions(state, now, queueDemoSessions))
  const proxy = state.providers.find((p) => p.providerId === "provider-proxy")
  if (proxy) proxy.maxSessions = 1
}

/**
 * Adds the worker demo: sessions of the demo projects whose worker lost its
 * connection, one running a turn and one idle (`?mock=worker`).
 */
export function addWorkerDemo(state: MockState, now: number = Date.now()) {
  state.sessions.push(...seedSessions(state, now, workerDemoSessions))
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
