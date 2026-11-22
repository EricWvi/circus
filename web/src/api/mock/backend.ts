import { nanoid } from "nanoid"

import { ApiError } from "@/api/errors"
import type {
  AuthState,
  CircusClient,
  EnrollRole,
  CreateSessionRequest,
  Location,
  LoginMethods,
  Project,
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

import { seedLedgers } from "@/mock/ledger"

import { listMockDirectory, readMockGitRemote } from "./directories"
import { decisionOf, historyOf, ledgerOf, type MockLedgers } from "./ledger"
import {
  createMockSessionRuntime,
  defaultSessionTiming,
  detailsOf,
  summaryOf,
  type MockSession,
  type MockSessionTiming,
} from "./sessions"
import {
  createMockWorkspaces,
  defaultWorkspaceTiming,
  workspacePayload,
  workspaceStatus,
  type MockWorkspaceTiming,
} from "./workspaces"
import {
  MOCK_LOGIN_METHODS,
  OIDC_SETUP_EMAIL,
  SEED_SUPER_EMAIL,
  type MockDevice,
  type MockLocation,
  type MockProject,
  type MockState,
  type MockUser,
} from "./state"

export interface MockBackend {
  client: CircusClient
  /** Replaces the state with one another tab wrote (keeping this tab's sessions), notifying subscribers. */
  adopt(next: MockState): void
  /**
   * Called when another tab changed projects or devices, the way a server
   * would push updates; the app refetches.
   */
  onExternalChange(listener: () => void): () => void
}

export interface MockBackendOptions {
  /** Mutated in place. */
  state: MockState
  /** Called after every change, e.g. to persist the state. */
  onChange?: (state: MockState) => void
  /** Delay in milliseconds before each call settles. */
  latency?: () => number
  now?: () => number
  /** How the scripted session replies pace themselves. */
  sessionTiming?: MockSessionTiming
  /** How the scripted preparation of default folders paces itself. */
  workspaceTiming?: MockWorkspaceTiming
  /** Receives notes about what the mock does in place of the real server. */
  note?: (message: string) => void
  /** Whether an OIDC provider is configured (default true). */
  oidc?: boolean
  /**
   * Every project's ledger, by project id (the demo ledgers by default).
   * In memory only, like the sessions; a project without one has an empty
   * ledger.
   */
  ledgers?: MockLedgers
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
  sessionTiming = defaultSessionTiming,
  workspaceTiming = defaultWorkspaceTiming,
  note = () => {},
  oidc = true,
  ledgers = seedLedgers(now()),
}: MockBackendOptions): MockBackend {
  const methods = (): LoginMethods =>
    oidc ? structuredClone(MOCK_LOGIN_METHODS) : { password: MOCK_LOGIN_METHODS.password }
  const checkOidc = () => {
    if (!oidc) throw new ApiError("failed_precondition", "OIDC is not configured.")
  }
  const changed = () => onChange?.(state)
  const externalListeners = new Set<() => void>()

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
    const user = state.users.find((u) => u.userId === state.cookie)
    if (!user) throw new ApiError("unauthenticated", "Sign in to continue.")
    return user.userId
  }

  /** One of the signed-in user's devices that is online, for a live device query. */
  function onlineDevice(name: string): MockDevice {
    const owner = currentUser()
    const device = state.devices.find((d) => d.owner === owner && d.name === name)
    if (!device) throw new ApiError("not_found", "No such device.")
    if (!device.online) {
      throw new ApiError("failed_precondition", "The device is offline.")
    }
    return device
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

  /** The main line of one of the signed-in user's projects; empty for a ledger without decisions. */
  function mainLine(projectId: string) {
    const project = ownedProject(currentUser(), projectId)
    return ledgers.get(project.projectId) ?? []
  }

  /** A session of the signed-in user's; not_found otherwise. */
  function ownSession(sessionId: string): MockSession {
    const owner = currentUser()
    const session = state.sessions.find((s) => s.sessionId === sessionId)
    const owned = session && state.projects.some((p) => p.projectId === session.projectId && p.owner === owner)
    if (!session || !owned) throw new ApiError("not_found", "Session not found.")
    return session
  }

  const runtime = createMockSessionRuntime({
    sessions: () => state.sessions,
    own: ownSession,
    delay: () => sleep(latency()),
    now,
    timing: sessionTiming,
  })

  const workspaces = createMockWorkspaces({
    now,
    timing: workspaceTiming,
    device: (deviceId) => state.devices.find((d) => d.deviceId === deviceId),
    repoUrl: (projectId) => state.projects.find((p) => p.projectId === projectId)?.repoUrl ?? "",
    changed(ws, durable) {
      for (const s of state.sessions) {
        if (s.workspace === ws && s.state !== "stopped") runtime.record(s, "workspace", workspacePayload(ws), durable, ws.updatedAtUnixMs)
      }
    },
    ready(ws) {
      for (const s of state.sessions) {
        if (s.workspace !== ws) continue
        s.cwd = ws.path
        runtime.wake(s)
      }
    },
  })

  const publicProject = ({ owner, updatedAtUnixMs, ...project }: MockProject): Project => ({
    ...project,
    locations: project.locations.map(({ deviceId, ...l }) => {
      const workspace = l.path === "" ? workspaces.get(`${project.projectId}/${deviceId}`) : undefined
      return {
        ...l,
        effectivePath: l.path === "" ? defaultPath(project.projectId) : "",
        deviceRevoked: !state.devices.some((d) => d.owner === owner && d.deviceId === deviceId),
        ...(workspace ? { workspace: workspaceStatus(workspace) } : {}),
      }
    }),
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

      deleteProject: call((req: { projectId: string; removeWorkspacesOn?: string[] }) => {
        const owner = currentUser()
        const project = ownedProject(owner, req.projectId)
        const names = req.removeWorkspacesOn ?? []
        if (names.length > 32) throw invalid("At most 32 devices.", "remove_workspaces_on")
        const sessions = state.sessions.filter((s) => s.projectId === project.projectId)
        if (sessions.some((s) => s.state !== "stopped")) {
          throw new ApiError(
            "failed_precondition",
            "Stop every session in this project before deleting it.",
          )
        }
        // Each named device is asked once, before the project goes.
        const workspaceRemovals = names.map((name) =>
          workspaces.remove(
            project.projectId,
            state.devices.find((d) => d.owner === owner && d.name === name),
            name,
          ),
        )
        state.projects = state.projects.filter((p) => p !== project)
        state.sessions = state.sessions.filter((s) => s.projectId !== project.projectId)
        workspaces.forget(project.projectId)
        ledgers.delete(project.projectId)
        changed()
        return { workspaceRemovals }
      }),
    },

    machines: {
      listDevices: call(() => {
        const owner = currentUser()
        return state.devices
          .filter((d) => d.owner === owner)
          .sort((a, b) => a.name.localeCompare(b.name))
          .map(({ owner: _, deviceId: __, ...device }) => device)
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
        const device = onlineDevice(req.device)
        const { delayMs, outcome } = listMockDirectory(device.os, req)
        if (delayMs > 0) await sleep(delayMs)
        if (outcome instanceof ApiError) throw outcome
        return outcome
      }),
      // The same fake tree's repositories (see `directories.ts`).
      getGitRemote: callAsync(async (req: { device: string; path: string }) => {
        await sleep(latency())
        const device = onlineDevice(req.device)
        const { delayMs, remote } = readMockGitRemote(device.os, req)
        if (delayMs > 0) await sleep(delayMs)
        return remote
      }),
    },

    sessions: {
      listSessions: call((req: { projectId?: string }) => {
        const owner = currentUser()
        // Unknown projects are not_found.
        if (req.projectId !== undefined) ownedProject(owner, req.projectId)
        const owned = new Set(state.projects.filter((p) => p.owner === owner).map((p) => p.projectId))
        return state.sessions
          .filter((s) => owned.has(s.projectId) && (req.projectId === undefined || s.projectId === req.projectId))
          .map(summaryOf)
          .sort((a, b) => b.lastActivityAtUnixMs - a.lastActivityAtUnixMs)
      }),

      createSession: callAsync(async (req: CreateSessionRequest) => {
        const owner = currentUser()
        const key = req.clientRequestId
        if (typeof key !== "string" || key === "" || new TextEncoder().encode(key).length > 128) {
          throw invalid("client_request_id must be 1 to 128 bytes.", "client_request_id")
        }
        // A retry returns the session the first request created, without
        // checking the location again; the same key for another request is
        // refused.
        ownedProject(owner, req.projectId)
        const retried = () => {
          const earlier = state.sessions.find((s) => s.projectId === req.projectId && s.created?.clientRequestId === key)
          if (earlier && earlier.created!.locationIndex !== req.locationIndex) {
            throw new ApiError("already_exists", "client_request_id was used for another request.", "client_request_id")
          }
          return earlier && detailsOf(earlier)
        }
        const first = retried()
        if (first) {
          await sleep(latency())
          return first
        }
        const usable = (p: Project) => {
          const location = Number.isInteger(req.locationIndex) ? p.locations[req.locationIndex] : undefined
          return location && !location.deviceRevoked ? location : undefined
        }
        const chosen = usable(await client.projects.getProject({ projectId: req.projectId }))
        if (!chosen) throw invalid("Choose one of the project's locations.", "location_index")
        // Exempt from the version check, like starting a session on the server.
        const project = await client.projects.updateProject({
          projectId: req.projectId,
          lastUsedLocation: req.locationIndex,
        })
        const location = usable(project)
        if (!location || !sameLocation(location, chosen)) {
          throw invalid("Choose one of the project's locations.", "location_index")
        }
        // A retry that overtook this request while it waited.
        const overtaken = retried()
        if (overtaken) return overtaken
        // A default location's folder is prepared first; the session waits for it.
        const stored = state.projects.find((p) => p.projectId === project.projectId)?.locations[req.locationIndex]
        const device = state.devices.find((d) => d.deviceId === stored?.deviceId)
        const workspace = location.path === "" && device ? workspaces.ensure(project.projectId, device) : undefined
        const at = now()
        const session: MockSession = {
          sessionId: nanoid(12),
          projectId: project.projectId,
          title: "",
          device: location.device,
          cwd: location.path || (workspace?.state === "ready" ? workspace.path : location.effectivePath),
          createdAtUnixMs: at,
          lastActivityAtUnixMs: at,
          state: "idle",
          lastTurnOutcome: "unspecified",
          events: [],
          approvals: [],
          queue: [],
          turns: 0,
          created: { clientRequestId: key, locationIndex: req.locationIndex },
          ...(workspace ? { workspace } : {}),
        }
        state.sessions.push(session)
        return detailsOf(session)
      }),

      retryWorkspace: callAsync(async ({ sessionId }: { sessionId: string }) => {
        await sleep(latency())
        const s = ownSession(sessionId)
        if (!s.workspace) throw new ApiError("failed_precondition", "The session has no workspace.")
        workspaces.retry(s.workspace)
        return workspaceStatus(s.workspace)
      }),

      ...runtime.service,
    },

    ledger: {
      getLedger: call(({ projectId }: { projectId: string }) =>
        ledgerOf(mainLine(projectId)),
      ),
      getDecision: call(({ projectId, number, commit }: { projectId: string; number: number; commit?: string }) =>
        decisionOf(mainLine(projectId), number, commit),
      ),
      listDecisionHistory: call(
        (req: { projectId: string; number: number; pageSize?: number; pageToken?: string }) =>
          historyOf(mainLine(req.projectId), req.number, req.pageSize, req.pageToken),
      ),
    },
  }

  return {
    client,
    adopt(next) {
      const data = (m: MockState) => JSON.stringify([m.projects, m.devices])
      const dataChanged = data(state) !== data(next)
      Object.assign(state, { ...next, sessions: state.sessions })
      if (dataChanged) for (const listener of externalListeners) listener()
    },
    onExternalChange(listener) {
      externalListeners.add(listener)
      return () => externalListeners.delete(listener)
    },
  }
}
