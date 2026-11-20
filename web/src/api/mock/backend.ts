import { nanoid } from "nanoid"

import { ApiError } from "@/api/errors"
import type {
  AuthState,
  CircusClient,
  EnrollRole,
  Location,
  LoginMethods,
  Project,
  ProjectService,
  SessionSummary,
  User,
} from "@/api/types"
import {
  checkPath,
  checkProjectName,
  checkRepoUrl,
  defaultPath,
  MAX_LOCATIONS,
  normalizeEmail,
  sameLocation,
  sameProjectName,
  validateEmail,
  validatePassword,
} from "@/api/validation"
import type { ConversationStore, SessionEvent } from "@/lib/types"

import { listMockDirectory } from "./directories"
import {
  MOCK_LOGIN_METHODS,
  OIDC_SETUP_EMAIL,
  SEED_SUPER_EMAIL,
  settleSession,
  type MockLocation,
  type MockProject,
  type MockState,
  type MockUser,
} from "./state"

export interface MockBackend {
  client: CircusClient
  /**
   * Transcripts and session progress of the conversations the UI simulates
   * for now; the server would observe both from the agent itself.
   */
  conversations: ConversationStore
  /** Replaces the state with one another tab wrote, notifying subscribers. */
  adopt(next: MockState): void
  /**
   * Called when another tab changed projects, sessions or devices, the way
   * a server would push updates; the app refetches.
   */
  onExternalChange(listener: () => void): () => void
  /** Switches for simulating device trouble; in memory, reset on reload. */
  simulation: DeviceSimulation
}

export interface DeviceSimulation {
  /** Devices listed as offline whatever their own state. */
  offlineDevices(): ReadonlySet<string>
  setDeviceOffline(name: string, offline: boolean): void
  subscribe(listener: () => void): () => void
}

/** Pasted images larger than this are not persisted (localStorage is small). */
export const MAX_KEPT_ATTACHMENT_URL = 8 * 1024

/** Large data URLs don't fit the store and blob URLs die with the page. */
const keepsUrl = (url: string) => url.length <= MAX_KEPT_ATTACHMENT_URL && !url.startsWith("blob:")

/** Drops attachment data that isn't kept; the UI shows a placeholder for an empty url. */
function withoutAttachmentData(events: SessionEvent[]): SessionEvent[] {
  return events.map((event) =>
    event.kind === "user_message" && event.attachments?.some((a) => !keepsUrl(a.url))
      ? {
          ...event,
          attachments: event.attachments.map((a) => (keepsUrl(a.url) ? a : { ...a, url: "" })),
        }
      : event,
  )
}

export interface MockBackendOptions {
  /** Mutated in place. */
  state: MockState
  /** Called after every change, e.g. to persist the state. */
  onChange?: (state: MockState) => void
  /** Delay in milliseconds before each call settles. */
  latency?: () => number
  now?: () => number
  /** Identifies the tab writing transcripts and session progress. */
  tabId?: string
  /** Receives notes about what the mock does in place of the real server. */
  note?: (message: string) => void
  /** Whether an OIDC provider is configured (default true). */
  oidc?: boolean
  /**
   * When set, auth is served elsewhere (a real control plane): every
   * project, device and session call is made as this user id, without
   * checking the mock's own sign-in. The app's auth guard still keeps
   * signed-out people away from the pages that make them.
   */
  actAs?: string
  /**
   * The projects the mock's sessions belong to (by default the mock's own).
   * Sessions look projects up and record the last used location only
   * through this service, so they also work with a real server's projects.
   */
  projects?: ProjectService
}

/** Failed sign-ins per email within the window before further tries are refused. */
export const MAX_LOGIN_FAILURES = 5
export const LOGIN_FAILURE_WINDOW_MS = 60_000

const sleep = (ms: number) => new Promise<void>((resolve) => setTimeout(resolve, ms))

const invalid = (message: string, field?: string) =>
  new ApiError("invalid_argument", message, field)

/** An in-memory control plane with the server's validation and error semantics. */
export function createMockBackend({
  state,
  onChange,
  latency = () => 100 + Math.random() * 200,
  now = Date.now,
  tabId = "",
  note = () => {},
  oidc = true,
  actAs,
  projects: sessionProjects,
}: MockBackendOptions): MockBackend {
  const methods = (): LoginMethods =>
    oidc ? structuredClone(MOCK_LOGIN_METHODS) : { password: MOCK_LOGIN_METHODS.password }
  const checkOidc = () => {
    if (!oidc) throw new ApiError("failed_precondition", "OIDC is not configured.")
  }
  const changed = () => onChange?.(state)
  const stamp = (sessionId: string) => {
    state.writtenAtMs[sessionId] = now()
    state.writtenBy[sessionId] = tabId
  }
  const listeners = new Set<(sessionIds: string[]) => void>()
  const externalListeners = new Set<() => void>()
  // Simulated device outages: device name → when it went offline.
  const forcedOffline = new Map<string, number>()
  let offlineNames: ReadonlySet<string> = new Set()
  const simulationListeners = new Set<() => void>()

  // Runs a handler after the simulated network delay. Requests and responses
  // are copied so callers never share objects with the store.
  function call<Args extends unknown[], Res>(handler: (...args: Args) => Res) {
    return (...args: Args): Promise<Res> => {
      const copy = structuredClone(args)
      return new Promise((resolve, reject) => {
        setTimeout(() => {
          try {
            const res = handler(...copy)
            resolve(res === undefined ? res : structuredClone(res))
          } catch (error) {
            reject(error)
          }
        }, latency())
      })
    }
  }

  // Like `call`, for handlers that call other services. Those calls take
  // the time a request takes, so there is no delay of its own.
  function callAsync<Args extends unknown[], Res>(handler: (...args: Args) => Promise<Res>) {
    return async (...args: Args): Promise<Res> => {
      const res = await handler(...structuredClone(args))
      return res === undefined ? res : structuredClone(res)
    }
  }

  /** The signed-in user's id. */
  function currentUser(): string {
    if (actAs !== undefined) return actAs
    const user = state.users.find((u) => u.userId === state.cookie)
    if (!user) throw new ApiError("unauthenticated", "Sign in to continue.")
    return user.userId
  }

  const hasSuper = () => state.users.some((u) => u.role === "super")
  const publicUser = ({ userId, email, role }: MockUser): User => ({ userId, email, role })

  function signIn(user: MockUser): User {
    state.cookie = user.userId
    changed()
    return publicUser(user)
  }

  // The token is checked before anything else, so a stale link learns nothing.
  function checkSetup(token: unknown) {
    if (typeof token !== "string" || token === "") {
      throw invalid("Open the setup link printed in the server log.", "token")
    }
    if (state.setupToken === null || token !== state.setupToken) {
      throw new ApiError("permission_denied", "This setup link is not valid.", "token")
    }
    if (hasSuper()) throw new ApiError("already_exists", "Circus is already set up.")
  }

  function createSuper(email: string, password: string | null): User {
    const user: MockUser = { userId: `user-${nanoid(8)}`, email, password, role: "super" }
    state.users.push(user)
    return signIn(user)
  }

  // Recent failed sign-ins by email. Kept in memory only: a reload resets it.
  const loginFailures = new Map<string, number[]>()

  /** Throws once the email has too many recent failures; returns them otherwise. */
  function recentFailures(email: string): number[] {
    const recent = (loginFailures.get(email) ?? []).filter(
      (at) => now() - at < LOGIN_FAILURE_WINDOW_MS,
    )
    loginFailures.set(email, recent)
    if (recent.length >= MAX_LOGIN_FAILURES) {
      throw new ApiError("resource_exhausted", "Too many attempts. Try again in a minute.")
    }
    return recent
  }

  function ownedProject(owner: string, projectId: string) {
    const project = state.projects.find((p) => p.projectId === projectId && p.owner === owner)
    if (!project) throw new ApiError("not_found", "Project not found.")
    return project
  }

  function checkName(owner: string, name: unknown, exceptId?: string): string {
    const checked = checkProjectName(typeof name === "string" ? name : "")
    if (!checked.ok) throw invalid(checked.problem, "name")
    const taken = state.projects.some(
      (p) => p.owner === owner && p.projectId !== exceptId && sameProjectName(p.name, checked.value),
    )
    if (taken) {
      throw new ApiError("already_exists", `A project named "${checked.value}" already exists.`, "name")
    }
    return checked.value
  }

  function checkRepo(repoUrl: unknown): string {
    const checked = checkRepoUrl(typeof repoUrl === "string" ? repoUrl : "")
    if (!checked.ok) throw invalid(checked.problem, "repo_url")
    return checked.value
  }

  /** Normalized locations; an empty path is the device's default directory. */
  function checkLocations(owner: string, locations: unknown): MockLocation[] {
    const list = Array.isArray(locations) ? (locations as Location[]) : []
    if (list.length > MAX_LOCATIONS) {
      throw invalid(`At most ${MAX_LOCATIONS} locations.`, "locations")
    }
    const out: MockLocation[] = []
    for (const [i, loc] of list.entries()) {
      const device = state.devices.find((d) => d.owner === owner && d.name === loc?.device)
      if (!device) {
        throw invalid(`Unknown device "${loc?.device ?? ""}".`, `locations[${i}].device`)
      }
      const checked = checkPath(String(loc.path ?? ""), device.os)
      if (!checked.ok) throw invalid(checked.problem, `locations[${i}].path`)
      // Binds the device that has this name now.
      const location = { device: device.name, path: checked.value, deviceId: device.deviceId }
      if (out.some((l) => sameLocation(l, location))) {
        throw invalid(
          checked.value === ""
            ? `${device.name} already has a default location.`
            : `${device.name}:${checked.value} is listed more than once.`,
          `locations[${i}].path`,
        )
      }
      out.push(location)
    }
    return out
  }

  const publicProject = ({ owner, updatedAtUnixMs, ...project }: MockProject): Project => ({
    ...project,
    locations: project.locations.map(({ deviceId, ...l }) => ({
      ...l,
      effectivePath: l.path === "" ? defaultPath(project.projectId) : "",
      deviceRevoked: !state.devices.some((d) => d.owner === owner && d.deviceId === deviceId),
    })),
    updatedAtUnixMs: String(updatedAtUnixMs),
  })

  /** The next version: now, but always past the previous one. */
  const nextVersion = (previous: number) => Math.max(now(), previous + 1)

  const client: CircusClient = {
    auth: {
      whoAmI: call((): AuthState => {
        if (!hasSuper()) return { kind: "needs_setup", methods: methods() }
        const user = state.users.find((u) => u.userId === state.cookie)
        return user
          ? { kind: "signed_in", user: publicUser(user) }
          : { kind: "signed_out", methods: methods() }
      }),

      setup: call((req: { token: string; email: string; password: string }): User => {
        checkSetup(req.token)
        const problem = validateEmail(req.email)
        if (problem) throw invalid(problem, "email")
        const passwordProblem = validatePassword(req.password)
        if (passwordProblem) throw invalid(passwordProblem, "password")
        return createSuper(normalizeEmail(req.email), req.password)
      }),

      login: call((req: { email: string; password: string }): User => {
        if (!hasSuper()) throw new ApiError("failed_precondition", "Circus is not set up yet.")
        const email = normalizeEmail(req.email)
        const failures = recentFailures(email)
        const user = state.users.find((u) => u.email === email)
        if (!user || user.password === null || user.password !== req.password) {
          failures.push(now())
          throw new ApiError("unauthenticated", "Wrong email or password.")
        }
        loginFailures.delete(email)
        return signIn(user)
      }),

      logout: call(() => {
        state.cookie = null
        changed()
      }),
    },

    // Without a provider to redirect to, the mock signs in on the spot.
    oidc: {
      startLogin: call((next?: string) => {
        checkOidc()
        if (!hasSuper()) throw new ApiError("failed_precondition", "Circus is not set up yet.")
        const user =
          state.users.find((u) => u.email === SEED_SUPER_EMAIL) ??
          state.users.find((u) => u.role === "super")!
        note(`Mock OIDC: signed in as ${user.email} instead of redirecting to the provider${next ? ` (next: ${next})` : ""}.`)
        signIn(user)
      }),

      startSetup: call((token: string) => {
        checkOidc()
        checkSetup(token)
        note(`Mock OIDC: created the super user ${OIDC_SETUP_EMAIL} instead of redirecting to the provider.`)
        createSuper(OIDC_SETUP_EMAIL, null)
      }),
    },

    projects: {
      createProject: call((req: { name: string; repoUrl: string; locations: Location[] }) => {
        const owner = currentUser()
        const name = checkName(owner, req.name)
        const repoUrl = checkRepo(req.repoUrl)
        const locations = checkLocations(owner, req.locations)
        const at = now()
        const project: MockProject = {
          owner,
          projectId: nanoid(12),
          name,
          repoUrl,
          locations,
          lastUsedLocation: 0,
          createdAtUnixMs: at,
          updatedAtUnixMs: at,
        }
        state.projects.push(project)
        changed()
        return publicProject(project)
      }),

      listProjects: call(() => {
        const owner = currentUser()
        return state.projects
          .filter((p) => p.owner === owner)
          .sort((a, b) => a.name.localeCompare(b.name))
          .map(publicProject)
      }),

      getProject: call((req: { projectId: string }) =>
        publicProject(ownedProject(currentUser(), req.projectId)),
      ),

      updateProject: call(
        (req: {
          projectId: string
          name?: string
          locations?: Location[]
          repoUrl?: string
          lastUsedLocation?: number
          expectedUpdatedAt?: string
        }) => {
          const owner = currentUser()
          const project = ownedProject(owner, req.projectId)
          const name = req.name === undefined ? project.name : checkName(owner, req.name, project.projectId)
          const repoUrl = req.repoUrl === undefined ? project.repoUrl : checkRepo(req.repoUrl)
          const locations =
            req.locations === undefined ? project.locations : checkLocations(owner, req.locations)
          const versioned =
            req.name !== undefined || req.repoUrl !== undefined || req.locations !== undefined
          if (
            versioned &&
            req.expectedUpdatedAt !== undefined &&
            req.expectedUpdatedAt !== String(project.updatedAtUnixMs)
          ) {
            throw new ApiError("aborted", "The project changed since it was read.")
          }
          // Keep pointing at the same location if it survived the edit.
          let lastUsed = project.lastUsedLocation
          if (req.locations !== undefined) {
            const previous = project.locations[project.lastUsedLocation]
            const kept = previous ? locations.findIndex((l) => sameLocation(l, previous)) : -1
            lastUsed = kept >= 0 ? kept : Math.max(0, Math.min(lastUsed, locations.length - 1))
          }
          if (req.lastUsedLocation !== undefined) {
            const i = req.lastUsedLocation
            const inRange = locations.length === 0 ? i === 0 : i >= 0 && i < locations.length
            if (!Number.isInteger(i) || !inRange) {
              throw invalid("The last used location is out of range.", "last_used_location")
            }
            lastUsed = i
          }
          const moved =
            name !== project.name ||
            repoUrl !== project.repoUrl ||
            JSON.stringify(locations) !== JSON.stringify(project.locations)
          if (moved) project.updatedAtUnixMs = nextVersion(project.updatedAtUnixMs)
          Object.assign(project, { name, repoUrl, locations, lastUsedLocation: lastUsed })
          changed()
          return publicProject(project)
        },
      ),

      deleteProject: call((req: { projectId: string }) => {
        const project = ownedProject(currentUser(), req.projectId)
        const sessions = state.sessions.filter((s) => s.projectId === project.projectId)
        if (sessions.some((s) => s.state !== "stopped")) {
          throw new ApiError(
            "failed_precondition",
            "Stop every session in this project before deleting it.",
          )
        }
        state.projects = state.projects.filter((p) => p !== project)
        state.sessions = state.sessions.filter((s) => s.projectId !== project.projectId)
        for (const s of sessions) {
          delete state.conversations[s.sessionId]
          delete state.writtenAtMs[s.sessionId]
          delete state.writtenBy[s.sessionId]
        }
        changed()
      }),
    },

    machines: {
      listDevices: call(() => {
        const owner = currentUser()
        return state.devices
          .filter((d) => d.owner === owner)
          .sort((a, b) => a.name.localeCompare(b.name))
          .map(({ owner: _, deviceId: __, ...device }) => {
            const since = forcedOffline.get(device.name)
            if (since === undefined || !device.online) return device
            return { ...device, online: false, lastSeenUnixMs: since }
          })
      }),
      // A link that enrolls nothing: the mock has no enrollment endpoint.
      createEnrollLink: call(({ role }: { role: EnrollRole }) => {
        currentUser()
        const tokens = { daemon: 1, worker: 1, both: 2 }[role]
        if (!tokens) throw invalid("role must be daemon, worker or both", "role")
        const origin = globalThis.location?.origin ?? "http://localhost:5173"
        const fragment = Array.from({ length: tokens }, () => nanoid(43)).join(".")
        return { link: `${origin}/join#${fragment}`, expiresUnixMs: now() + 3_600_000, serverVersion: "dev" }
      }),
      // A fake tree, the same on every device (see `directories.ts`).
      listDirectory: callAsync(async (req: { device: string; path: string; includeHidden: boolean }) => {
        await sleep(latency())
        const owner = currentUser()
        const device = state.devices.find((d) => d.owner === owner && d.name === req.device)
        if (!device) throw new ApiError("not_found", "No such device.")
        if (!device.online || forcedOffline.has(device.name)) {
          throw new ApiError("failed_precondition", "The device is offline.")
        }
        const { delayMs, outcome } = listMockDirectory(device.os, req)
        if (delayMs > 0) await sleep(delayMs)
        if (outcome instanceof ApiError) throw outcome
        return outcome
      }),
    },

    sessions: {
      listSessions: callAsync(async (req: { projectId?: string }) => {
        currentUser()
        const projects = sessionProjects ?? client.projects
        // Unknown projects are not_found; without a project, sessions of
        // projects that are gone are left out.
        const owned =
          req.projectId !== undefined
            ? new Set([(await projects.getProject({ projectId: req.projectId })).projectId])
            : new Set((await projects.listProjects()).map((p) => p.projectId))
        return state.sessions
          .filter((s) => owned.has(s.projectId))
          .sort((a, b) => b.lastActivityAtUnixMs - a.lastActivityAtUnixMs)
      }),

      createSession: callAsync(async (req: { projectId: string; locationIndex: number }) => {
        currentUser()
        const projects = sessionProjects ?? client.projects
        const usable = (p: Project) => {
          const location = Number.isInteger(req.locationIndex) ? p.locations[req.locationIndex] : undefined
          return location && !location.deviceRevoked ? location : undefined
        }
        const chosen = usable(await projects.getProject({ projectId: req.projectId }))
        if (!chosen) throw invalid("Choose one of the project's locations.", "locationIndex")
        // Exempt from the version check, like starting a session on the server.
        const project = await projects.updateProject({
          projectId: req.projectId,
          lastUsedLocation: req.locationIndex,
        })
        const location = usable(project)
        if (!location || !sameLocation(location, chosen)) {
          throw invalid("Choose one of the project's locations.", "locationIndex")
        }
        const at = now()
        const session: SessionSummary = {
          sessionId: nanoid(12),
          projectId: project.projectId,
          title: "New session",
          state: "idle",
          lastActivityAtUnixMs: at,
          pendingApprovalCount: 0,
          device: location.device,
          cwd: location.path || location.effectivePath,
          createdAtUnixMs: at,
        }
        state.sessions.push(session)
        changed()
        return session
      }),
    },
  }

  return {
    client,
    conversations: {
      load: () => structuredClone(state.conversations),
      save(sessionId, events) {
        state.conversations[sessionId] = withoutAttachmentData(structuredClone(events))
        stamp(sessionId)
        changed()
      },
      reportActivity(sessionId, activity) {
        const session = state.sessions.find((s) => s.sessionId === sessionId)
        if (!session) return
        Object.assign(session, activity, { lastActivityAtUnixMs: now() })
        stamp(sessionId)
        changed()
      },
      settle(sessionId) {
        settleSession(state, sessionId)
        stamp(sessionId)
        changed()
      },
      subscribe(listener) {
        listeners.add(listener)
        return () => listeners.delete(listener)
      },
    },
    adopt(next) {
      const changedIds = Object.keys({ ...state.conversations, ...next.conversations }).filter(
        (id) => JSON.stringify(state.conversations[id]) !== JSON.stringify(next.conversations[id]),
      )
      const data = (m: MockState) => JSON.stringify([m.projects, m.sessions, m.devices])
      const dataChanged = data(state) !== data(next)
      Object.assign(state, next)
      if (changedIds.length > 0) for (const listener of listeners) listener(changedIds)
      if (dataChanged) for (const listener of externalListeners) listener()
    },
    onExternalChange(listener) {
      externalListeners.add(listener)
      return () => externalListeners.delete(listener)
    },
    simulation: {
      offlineDevices: () => offlineNames,
      setDeviceOffline(name, offline) {
        if (offline === forcedOffline.has(name)) return
        if (offline) forcedOffline.set(name, now())
        else forcedOffline.delete(name)
        offlineNames = new Set(forcedOffline.keys())
        for (const listener of simulationListeners) listener()
      },
      subscribe(listener) {
        simulationListeners.add(listener)
        return () => simulationListeners.delete(listener)
      },
    },
  }
}
