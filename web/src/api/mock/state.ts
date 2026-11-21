import type { Device, Location, LoginMethods, Role } from "@/api/types"
import { projects as seedProjects, sessions as seedSessionList } from "@/mock/data"

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
  /** Moves forward whenever the name, the locations or the repository URL change. */
  updatedAtUnixMs: number
}

export type MockLocation = Location & { deviceId: string }

/** An enrolled device. `deviceId` is unique; a name can be enrolled again. */
export type MockDevice = Device & { owner: string; deviceId: string }

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
  }
  state.sessions = seedSessions(state, now)
  return state
}

/**
 * The demo sessions, for the seeded projects the demo data's owner still
 * has at a location on the session's device.
 */
export function seedSessions(state: Pick<MockState, "projects">, now: number = Date.now()): MockSession[] {
  return seedSessionList.flatMap((seed) => {
    const project = state.projects.find((p) => p.projectId === seed.projectId && p.owner === SEED_OWNER_ID)
    const location = project?.locations.find((l) => l.device === seed.device)
    if (!location) return []
    return [seedSession(seed, location.path, now)]
  })
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
