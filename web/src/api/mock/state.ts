import type { Device, Project, SessionSummary } from "@/api/types"
import { settleEvents } from "@/lib/settle"
import type { Conversations } from "@/lib/types"
import { projects as seedProjects, sessions as seedSessions } from "@/mock/data"

/** The setup token of a fresh mock server, as printed in its "terminal". */
export const DEV_SETUP_TOKEN = "dev-setup-token"

export interface MockUser {
  username: string
  password: string
}

/** Everything the mock server knows. Plain JSON so it can be persisted. */
export interface MockState {
  /** Cleared once the first user exists. */
  setupToken: string | null
  users: MockUser[]
  /** The signed-in username; stands in for the session cookie. */
  cookie: string | null
  projects: (Project & { owner: string })[]
  sessions: SessionSummary[]
  devices: (Device & { owner: string })[]
  /** Transcripts of the simulated conversations, by session id. */
  conversations: Conversations
  /**
   * When a live tab last wrote a session's transcript or state, by session
   * id. Lets a reloading tab tell abandoned work from work still streaming.
   */
  writtenAtMs: Record<string, number>
  /** Which tab made that write, by session id (see `settleInFlight`). */
  writtenBy: Record<string, string>
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
    conversations: {},
    writtenAtMs: {},
    writtenBy: {},
  }
}

const MINUTE = 60_000
const DAY = 24 * 60 * MINUTE

/** A set-up server with user `eric` (password `circus`), signed out, and the demo data. */
export function seedState(now: number = Date.now()): MockState {
  const owner = "eric"
  const devices: MockState["devices"] = [
    { name: "devbox", online: true, lastSeenUnixMs: now, os: "linux", arch: "amd64" },
    { name: "eric-mbp", online: true, lastSeenUnixMs: now, os: "darwin", arch: "arm64" },
    { name: "eric-win", online: false, lastSeenUnixMs: now - 2 * DAY, os: "windows", arch: "amd64" },
    { name: "palace-builder", online: true, lastSeenUnixMs: now, os: "linux", arch: "arm64" },
  ].map((d) => ({ ...d, owner, version: "0.1.0" }))

  const sessions: SessionSummary[] = seedSessions.map((s) => {
    const project = seedProjects.find((p) => p.id === s.projectId)
    const location = project?.locations.find((l) => l.device === s.deviceName)
    if (!location) throw new Error(`seed session ${s.id} has no location on ${s.deviceName}`)
    const lastActivityAtUnixMs = Date.parse(s.lastActivityAt)
    return {
      sessionId: s.id,
      projectId: s.projectId,
      title: s.title,
      state: s.state,
      lastActivityAtUnixMs,
      pendingApprovalCount: s.pendingApprovalCount,
      device: location.device,
      cwd: location.path,
      createdAtUnixMs: lastActivityAtUnixMs - 45 * MINUTE,
    }
  })

  const projects: MockState["projects"] = seedProjects.map((p, i) => {
    const latest = sessions
      .filter((s) => s.projectId === p.id)
      .sort((a, b) => b.lastActivityAtUnixMs - a.lastActivityAtUnixMs)[0]
    const lastUsed = latest ? p.locations.findIndex((l) => l.device === latest.device) : 0
    return {
      owner,
      projectId: p.id,
      name: p.name,
      locations: p.locations.map((l) => ({ ...l })),
      lastUsedLocation: Math.max(0, lastUsed),
      createdAtUnixMs: now - (30 - i * 7) * DAY,
    }
  })

  return {
    setupToken: null,
    users: [{ username: owner, password: "circus" }],
    cookie: null,
    projects,
    sessions,
    devices,
    conversations: Object.fromEntries(seedSessions.map((s) => [s.id, s.events])),
    writtenAtMs: {},
    writtenBy: {},
  }
}

/**
 * Longer than any gap between writes of a live simulation (the slowest step,
 * a tool run after approval, takes 1.5 s; streaming saves every 0.5 s).
 */
export const STALE_AFTER_MS = 5_000

/** Ends what a session had in flight: nothing will finish it any more. */
export function settleSession(state: MockState, sessionId: string): void {
  const session = state.sessions.find((s) => s.sessionId === sessionId)
  if (session?.state === "running") session.state = "interrupted"
  const events = state.conversations[sessionId]
  if (events) state.conversations[sessionId] = settleEvents(events)
}

/**
 * Run on load: a session lost its stream if this tab wrote it last (the
 * reload ended the page that ran it) or if nobody has written it for a
 * while. Sessions another open tab is still streaming have recent writes
 * by that tab and are left alone.
 */
export function settleInFlight(state: MockState, tabId: string, now: number = Date.now()): void {
  const ids = new Set([...state.sessions.map((s) => s.sessionId), ...Object.keys(state.conversations)])
  for (const id of ids) {
    const ours = state.writtenBy[id] === tabId
    if (ours || now - (state.writtenAtMs[id] ?? 0) >= STALE_AFTER_MS) settleSession(state, id)
  }
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
    Array.isArray(v.sessions) &&
    Array.isArray(v.devices) &&
    isRecord(v.conversations) &&
    Object.values(v.conversations).every(Array.isArray) &&
    isRecord(v.writtenAtMs) &&
    isRecord(v.writtenBy)
  )
}
