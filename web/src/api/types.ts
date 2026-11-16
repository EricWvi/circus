// Client-side view of the control plane API, shaped after its protobuf
// services. Method names follow the RPC names and requests are plain
// objects. Responses are UI-friendly: int64 timestamps become `number`
// milliseconds and enums become string unions. A Connect-backed client
// converts from the generated messages; the in-memory mock implements the
// same interface.

import type { SessionState } from "@/lib/types"

export type Role = "user" | "super"

export interface User {
  userId: string
  /** Normalized: trimmed and lower case. The identity users sign in with. */
  email: string
  role: Role
}

/** How users can sign in on this server. */
export interface LoginMethods {
  password: boolean
  /** Present when an OpenID Connect provider is configured. */
  oidc?: { displayName: string }
}

export type AuthState =
  /** No super user exists yet; the first one is created through /setup. */
  | { kind: "needs_setup"; methods: LoginMethods }
  | { kind: "signed_out"; methods: LoginMethods }
  | { kind: "signed_in"; user: User }

export interface AuthService {
  whoAmI(): Promise<AuthState>
  /** Creates the super user with the setup token and signs them in. */
  setup(req: { token: string; email: string; password: string }): Promise<User>
  login(req: { email: string; password: string }): Promise<User>
  logout(): Promise<void>
}

/**
 * Sign-in through the OpenID Connect provider. Not RPCs: the browser leaves
 * the app and the server redirects back (see `oidc.ts`), so in a real client
 * the returned promise never settles. The mock signs in on the spot and
 * resolves once it has, or rejects the way `setup` would.
 */
export interface OidcFlow {
  /** `next` is the page to return to after signing in. */
  startLogin(next?: string): Promise<void>
  /** Creates the super user from the provider's account. */
  startSetup(token: string): Promise<void>
}

export interface Location {
  device: string
  /** Absolute path on that device. */
  path: string
}

export interface Project {
  projectId: string
  /** Unique per user, trimmed, 1 to 64 characters. */
  name: string
  /** At least one; (device, path) pairs are unique. */
  locations: Location[]
  /** Index into `locations`; the default for new sessions. */
  lastUsedLocation: number
  createdAtUnixMs: number
}

export interface ProjectService {
  createProject(req: { name: string; locations: Location[] }): Promise<Project>
  /** Ordered by name. */
  listProjects(): Promise<Project[]>
  getProject(req: { projectId: string }): Promise<Project>
  updateProject(req: {
    projectId: string
    name?: string
    locations?: Location[]
  }): Promise<Project>
  /** Fails with `failed_precondition` while any session in it is not stopped. */
  deleteProject(req: { projectId: string }): Promise<void>
}

export interface Device {
  name: string
  online: boolean
  lastSeenUnixMs: number
  os: string
  arch: string
  version: string
}

export interface MachineService {
  listDevices(): Promise<Device[]>
}

export interface SessionSummary {
  sessionId: string
  projectId: string
  title: string
  state: SessionState
  lastActivityAtUnixMs: number
  pendingApprovalCount: number
  /** Default device the session's tools execute on. */
  device: string
  cwd: string
  createdAtUnixMs: number
}

export interface SessionService {
  /** Newest activity first; includes stopped sessions. */
  listSessions(req: { projectId?: string }): Promise<SessionSummary[]>
  /** Also makes `locationIndex` the project's `lastUsedLocation`. */
  createSession(req: { projectId: string; locationIndex: number }): Promise<SessionSummary>
}

export interface CircusClient {
  auth: AuthService
  oidc: OidcFlow
  projects: ProjectService
  machines: MachineService
  sessions: SessionService
}
