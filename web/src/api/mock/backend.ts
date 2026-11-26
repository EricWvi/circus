import { nanoid } from "nanoid"

import { ApiError } from "@/api/errors"
import type {
  AgentKind,
  AuthState,
  CircusClient,
  EnrollRole,
  CreateSessionRequest,
  Location,
  LoginMethods,
  ModelProviderInput,
  Project,
  ProviderVersion,
  QueueReason,
  User,
  Worker,
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

import { sessionPermissionMode } from "@/lib/permission-mode"
import { seedLedgers } from "@/mock/ledger"

import { listMockDirectory, readMockGitRemote } from "./directories"
import { decisionOf, historyOf, ledgerOf, type MockLedgers } from "./ledger"
import { createMockAgents, defaultAgentTiming, type MockAgentTiming } from "./agents"
import { createMockModelProviders, runsSessions } from "./model-providers"
import {
  collectSessionLedger,
  deleteSessionLedger,
  addLargeDecision,
  discardSessionLedger,
  mainLineMoved,
  mergeSessionLedger,
  readyLedger,
  seedSessionLedger,
  sessionLedgerDiffOf,
  sessionLedgerOf,
} from "./session-ledger"
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
  type MockWorker,
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
  /** How the simulated agent installs and uninstalls pace themselves. */
  agentTiming?: MockAgentTiming
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
  /** `web-vite`'s snapshot also adds a decision at the size limit (256 KiB), for large diffs. */
  largeSnapshotChange?: boolean
}

/** Failed sign-ins per email within the window before further tries are refused. */
export const MAX_LOGIN_FAILURES = 5
export const LOGIN_FAILURE_WINDOW_MS = 60_000
/** How long a lease outlives its worker's connection, as on the server. */
export const MOCK_LEASE_MS = 60_000

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
  agentTiming = defaultAgentTiming,
  note = () => {},
  oidc = true,
  ledgers = seedLedgers(now()),
  largeSnapshotChange = false,
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

  const getSessionLedger = call(({ sessionId }: { sessionId: string }) => sessionLedgerOf(ownSession(sessionId).ledger))
  const getSessionLedgerDiff = call(({ sessionId, path }: { sessionId: string; path: string }) =>
    sessionLedgerDiffOf(ownSession(sessionId).ledger, path),
  )

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

  /** A worker as its owner sees it: only their own devices share its machine. */
  function publicWorker(owner: string, { owner: _, ...worker }: MockWorker): Worker {
    const devices = new Set(state.devices.filter((d) => d.owner === owner).map((d) => d.name))
    return { ...worker, localDevices: worker.localDevices.filter((d) => devices.has(d)) }
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

  const ownerOf = (s: MockSession) => state.projects.find((p) => p.projectId === s.projectId)?.owner

  /** Looks at every waiting session again, as the scheduler does after a change. */
  const reschedule = () => {
    for (const s of state.sessions) if (s.queue.length > 0 && !s.leased) runtime.wake(s)
  }
  const providers = createMockModelProviders({ state, currentUser, now, ownerOf, reschedule })

  /** Records why the session waits, announcing a change live as the server does. */
  function setQueueReason(s: MockSession, reason: QueueReason) {
    if ((s.queueReason ?? "unspecified") === reason) return
    s.queueReason = reason
    runtime.record(s, "queue", { queue_reason: reason }, false, now())
  }

  /**
   * The worker holding the session's lease is back or the lease ended:
   * announced live as the server does, only when it was disconnected.
   */
  function clearWorkerDisconnect(s: MockSession) {
    if (!s.workerDisconnectedAtUnixMs) return
    s.workerDisconnectedAtUnixMs = 0
    runtime.record(s, "worker", { disconnected_at_unix_ms: "0" }, false, now())
  }

  /**
   * The disconnected worker doesn't come back (the mock has no worker to
   * reconnect): the lease expires a minute after the disconnect. As on the
   * server, the mark clears before the open turn ends interrupted, and
   * messages that waited for the worker go to a new lease.
   */
  function expireLeaseOf(s: MockSession) {
    const at = s.workerDisconnectedAtUnixMs
    if (!at) return
    setTimeout(
      () => {
        if (s.workerDisconnectedAtUnixMs !== at) return
        clearWorkerDisconnect(s)
        s.leased = false
        runtime.leaseLost(s)
        reschedule()
      },
      Math.max(0, at + MOCK_LEASE_MS - now()),
    )
  }

  /** Announces a change of the session's snapshot live, or recorded for a merge or a discard. */
  function ledgerChanged(s: MockSession, payload: object, durable: boolean) {
    if (!s.ledger) return
    runtime.record(s, "ledger", payload, durable, s.ledger.status.updatedAtUnixMs)
  }

  /** The seeded sessions' snapshots, from their projects' main lines. */
  function seedSnapshots() {
    for (const s of state.sessions) {
      if (s.ledger) continue
      const ledger = seedSessionLedger(s.sessionId, ledgers.get(s.projectId) ?? [], now())
      if (ledger && largeSnapshotChange && s.sessionId === "web-vite") addLargeDecision(ledger)
      if (ledger) s.ledger = ledger
    }
  }

  /**
   * The worker that holds a leased session: the one on its device's
   * machine, else (as the mock has no record of it) the first online one,
   * by name, that runs sessions.
   */
  function hostOf(s: MockSession): MockWorker | undefined {
    const owner = ownerOf(s)
    const workers = state.workers.filter((w) => w.owner === owner)
    return (
      workers.find((w) => w.localDevices.includes(s.device)) ??
      workers
        .filter((w) => w.online && runsSessions(w))
        .sort((a, b) => a.name.localeCompare(b.name))[0]
    )
  }

  const agents = createMockAgents({
    state,
    now,
    timing: agentTiming,
    // Every session runs the one agent there is.
    holdsSession: (worker) => state.sessions.some((s) => s.leased && s.state !== "stopped" && hostOf(s) === worker),
    installed: () => reschedule(),
  })

  const runtime = createMockSessionRuntime({
    sessions: () => state.sessions,
    own: ownSession,
    delay: () => sleep(latency()),
    now,
    timing: sessionTiming,
    admit(s) {
      // The lease's holder is away: nothing runs until it is back or the lease ends.
      if (s.workerDisconnectedAtUnixMs) return false
      if (s.leased) return true
      const reason = providers.queueReasonOf(s)
      setQueueReason(s, reason)
      if (reason !== "unspecified") return false
      s.leased = true
      return true
    },
    setProvider(s, providerId) {
      providers.checkSessionProvider(ownerOf(s) ?? "", providerId)
      if ((s.providerId ?? "") === providerId) return
      s.providerId = providerId
      // A new provider means a new lease, under its workers and limit.
      if (s.leased) {
        clearWorkerDisconnect(s)
        s.leased = false
        reschedule()
      }
    },
    turnEnded(s, input) {
      // A message that asks for a discard plays an integrator discarding the changes.
      if (/\bdiscard\b/i.test(input)) {
        const discarded = discardSessionLedger(s.ledger, now())
        if (discarded !== null) {
          ledgerChanged(s, { event: "discarded", changeset_id: discarded, reason: "discarded by an integrator" }, true)
          return
        }
      }
      // A message that asks for a decision gets one in the snapshot.
      const asked = /\b(ledger|decision)\b/i.test(input)
      const head = ledgerOf(ledgers.get(s.projectId) ?? []).headCommit
      const newDecision = asked
        ? { title: `Record: ${input.trim().slice(0, 60)}`, body: `Recorded from the session's message:\n\n> ${input.trim()}\n` }
        : undefined
      if (collectSessionLedger(s.ledger, { sessionId: s.sessionId, head, newDecision }, now())) {
        ledgerChanged(s, { event: "collected", changeset_id: s.ledger?.open?.changeset.changesetId ?? "" }, false)
      }
    },
    stopped(s) {
      setQueueReason(s, "unspecified")
      clearWorkerDisconnect(s)
      if (!s.leased) return
      s.leased = false
      reschedule()
    },
  })

  for (const s of state.sessions) expireLeaseOf(s)
  seedSnapshots()

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

  const publicProject = ({ owner, updatedAtUnixMs, defaultProviderId, ...project }: MockProject): Project => ({
    ...project,
    defaultProviderId: defaultProviderId ?? "",
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
      createProject: call((req: { name: string; repoUrl: string; locations: Location[]; defaultProviderId?: string }) => {
        const owner = currentUser()
        const name = checkName(owner, req.name)
        const repoUrl = checkRepo(req.repoUrl)
        const locations = checkLocations(owner, req.locations)
        const defaultProviderId = providers.checkProjectDefault(owner, req.defaultProviderId ?? "")
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
          defaultProviderId,
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
          defaultProviderId?: string
          lastUsedLocation?: number
          expectedUpdatedAt?: string
        }) => {
          const owner = currentUser()
          const project = ownedProject(owner, req.projectId)
          const name = req.name === undefined ? project.name : checkName(owner, req.name, project.projectId)
          const repoUrl = req.repoUrl === undefined ? project.repoUrl : checkRepo(req.repoUrl)
          const locations =
            req.locations === undefined ? project.locations : checkLocations(owner, req.locations)
          const defaultProviderId =
            req.defaultProviderId === undefined
              ? (project.defaultProviderId ?? "")
              : providers.checkProjectDefault(owner, req.defaultProviderId)
          const versioned =
            req.name !== undefined ||
            req.repoUrl !== undefined ||
            req.locations !== undefined ||
            req.defaultProviderId !== undefined
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
            defaultProviderId !== (project.defaultProviderId ?? "") ||
            JSON.stringify(locations) !== JSON.stringify(project.locations)
          if (moved) project.updatedAtUnixMs = nextVersion(project.updatedAtUnixMs)
          Object.assign(project, { name, repoUrl, locations, lastUsedLocation: lastUsed, defaultProviderId })
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
      listWorkers: call(() => {
        const owner = currentUser()
        return state.workers
          .filter((w) => w.owner === owner)
          .sort((a, b) => a.name.localeCompare(b.name))
          .map((w) => publicWorker(owner, w))
      }),
      setWorkerGrant: call(({ workerId, integrator }: { workerId: string; integrator: boolean }) => {
        const owner = currentUser()
        const worker = state.workers.find((w) => w.owner === owner && w.workerId === workerId)
        if (!worker) throw new ApiError("not_found", "Worker not found.")
        // Workers live in this tab's memory: nothing to save.
        worker.integrator = integrator
        return publicWorker(owner, worker)
      }),
      setWorkerAgent: call(({ workerId, agent, install }: { workerId: string; agent: AgentKind; install: boolean }) => {
        const owner = currentUser()
        // Workers live in this tab's memory: nothing to save.
        const worker = agents.set(owner, { workerId, agent, install })
        if (!install) reschedule()
        return publicWorker(owner, worker)
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
        // Kept as given, as the server keeps it; the worker refuses a mode it doesn't know.
        const permissionMode = req.permissionMode ?? ""
        if (new TextEncoder().encode(permissionMode).length > 128) {
          throw invalid("permission mode is longer than 128 bytes", "config.permission_mode")
        }
        // A retry returns the session the first request created, without
        // checking the location again; the same key for another request is
        // refused.
        const owned = ownedProject(owner, req.projectId)
        const retried = () => {
          const earlier = state.sessions.find((s) => s.projectId === req.projectId && s.created?.clientRequestId === key)
          if (
            earlier &&
            (earlier.created!.locationIndex !== req.locationIndex ||
              earlier.created!.providerId !== req.providerId ||
              sessionPermissionMode(earlier.created!.permissionMode) !== sessionPermissionMode(permissionMode))
          ) {
            throw new ApiError("already_exists", "client_request_id was used for another request.", "client_request_id")
          }
          return earlier && detailsOf(earlier)
        }
        const first = retried()
        if (first) {
          await sleep(latency())
          return first
        }
        const providerId = providers.resolve(owner, req.providerId, owned.defaultProviderId)
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
        const sessionId = nanoid(12)
        const session: MockSession = {
          sessionId,
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
          created: { clientRequestId: key, locationIndex: req.locationIndex, providerId: req.providerId, permissionMode },
          ...(workspace ? { workspace } : {}),
          providerId,
          permissionMode,
          ledger: readyLedger(sessionId, at),
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

      deleteSession: callAsync(async ({ sessionId, discardLedgerChanges }: { sessionId: string; discardLedgerChanges?: boolean }) => {
        await sleep(latency())
        const s = ownSession(sessionId)
        if (s.state !== "stopped" || s.leased) {
          throw new ApiError("failed_precondition", "The session is not stopped.")
        }
        deleteSessionLedger(s.ledger, discardLedgerChanges === true, now())
        state.sessions = state.sessions.filter((x) => x !== s)
      }),

      ...runtime.service,
    },

    modelProviders: {
      createProvider: call((req: ModelProviderInput) => providers.service.createProvider(req)),
      listProviders: call(() => providers.service.listProviders()),
      getProvider: call((req: { providerId: string }) => providers.service.getProvider(req)),
      updateProvider: call(
        (req: ModelProviderInput & { providerId: string; expectedUpdatedAt?: ProviderVersion }) =>
          providers.service.updateProvider(req),
      ),
      deleteProvider: call((req: { providerId: string }) => providers.service.deleteProvider(req)),
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
      // The mock answers in a moment, so it ignores the abort signal (which can't be copied like the request).
      getSessionLedger: (req: { sessionId: string }) => getSessionLedger(req),
      getSessionLedgerDiff: (req: { sessionId: string; path: string }) => getSessionLedgerDiff(req),
      mergeSessionLedger: call(({ sessionId, expectedChangesetId }: { sessionId: string; expectedChangesetId: string }) => {
        const s = ownSession(sessionId)
        const commits = ledgers.get(s.projectId) ?? []
        const device = state.devices.find((d) => d.owner === ownerOf(s) && d.name === s.device)
        const { commit, merged } = mergeSessionLedger(
          s.ledger,
          { sessionId, expectedChangesetId, turnRunning: s.state === "running", deviceOnline: device?.online === true },
          commits,
          now(),
        )
        ledgers.set(s.projectId, [...commits, commit])
        ledgerChanged(s, { event: "merged", changeset_id: merged.changeset.changesetId, numbers: merged.numbers }, true)
        // The other sessions' open changesets are behind the main line now.
        for (const other of state.sessions) {
          if (other !== s && other.projectId === s.projectId && mainLineMoved(other.ledger, commit.commit, now())) {
            ledgerChanged(other, { event: "collected", changeset_id: other.ledger?.open?.changeset.changesetId ?? "" }, false)
          }
        }
        return { changeset: merged.changeset, numbers: merged.numbers }
      }),
    },
  }

  // Seeded sessions that wait learn why.
  reschedule()

  return {
    client,
    adopt(next) {
      const data = (m: MockState) => JSON.stringify([m.projects, m.devices])
      const dataChanged = data(state) !== data(next)
      Object.assign(state, { ...next, sessions: state.sessions, providers: state.providers, workers: state.workers })
      if (dataChanged) for (const listener of externalListeners) listener()
    },
    onExternalChange(listener) {
      externalListeners.add(listener)
      return () => externalListeners.delete(listener)
    },
  }
}
