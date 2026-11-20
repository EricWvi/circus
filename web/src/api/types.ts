// Client-side view of the control plane API, shaped after its protobuf
// services. Method names follow the RPC names and requests are plain
// objects. Responses are UI-friendly: int64 timestamps become `number`
// milliseconds (exact up to the year 287396) and enums become string
// unions. A project's version (`updatedAtUnixMs`) is the exception: it is
// only ever compared and sent back, so it stays the exact int64 as a decimal
// string. A Connect-backed client converts from the generated messages; the
// in-memory mock implements the same interface.

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

/** A location as a request names it. */
export interface Location {
  device: string
  /**
   * An absolute path on that device, or empty for the device's default
   * directory for the project (at most one such location per device).
   */
  path: string
}

/** A location as the server returns it. */
export interface ProjectLocation extends Location {
  /**
   * The directory a default location (empty `path`) resolves to on its
   * device, `~/circus/projects/<project id>`; empty when `path` is set.
   */
  effectivePath: string
  /**
   * The device was revoked after the location was saved. Sessions can't
   * start here, and saving the locations again needs another device.
   */
  deviceRevoked: boolean
}

/**
 * A project's version: the exact `updated_at_unix_ms` int64 as a decimal
 * string. It moves only when the name, the locations or the repository URL
 * change, and goes back unchanged as `expectedUpdatedAt`.
 */
export type ProjectVersion = string

export interface Project {
  projectId: string
  /** Unique per user ignoring case, trimmed, 1 to 64 characters. */
  name: string
  /** Git remote of the project; empty means none. */
  repoUrl: string
  /** Between 0 and 16; explicit (device, path) pairs are unique. */
  locations: ProjectLocation[]
  /** Index into `locations`; the default for new sessions. 0 while there are none. */
  lastUsedLocation: number
  createdAtUnixMs: number
  updatedAtUnixMs: ProjectVersion
}

export interface ProjectService {
  createProject(req: { name: string; repoUrl: string; locations: Location[] }): Promise<Project>
  /** Ordered by name. */
  listProjects(): Promise<Project[]>
  getProject(req: { projectId: string }): Promise<Project>
  /**
   * Changes the fields that are set. With `expectedUpdatedAt`, a change of
   * the name, the locations or the repository URL applies only if the
   * project is still at that version, and fails with `aborted` otherwise.
   * Setting only `lastUsedLocation` is never refused that way.
   */
  updateProject(req: {
    projectId: string
    name?: string
    /** Replaces all locations. */
    locations?: Location[]
    /** "" clears it. */
    repoUrl?: string
    lastUsedLocation?: number
    expectedUpdatedAt?: ProjectVersion
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

/** Which identities an enrollment link enrolls: a device (daemon), a worker, or both. */
export type EnrollRole = "daemon" | "worker" | "both"

export interface EnrollLink {
  /** `<public_url>/join#<token>[.<token>]`; works once and is shown only here. */
  link: string
  expiresUnixMs: number
  /** The control plane's build version, such as "v0.1.0" or "dev". */
  serverVersion: string
}

/** Why a directory was not listed; the path itself is the problem, not the device. */
export type DirectoryError =
  /** Relative, too long, or contains NUL. */
  | "invalid_path"
  | "not_found"
  | "not_directory"
  | "permission_denied"
  /** Reading took longer than the device allows (such as a hung network mount). */
  | "timeout"
  /** Any other I/O error. */
  | "io"

export interface DirectoryEntry {
  /** Base name, without separators. */
  name: string
  /** The directory contains a ".git" entry. */
  gitRepository: boolean
  /** A symbolic link to a directory. */
  symlink: boolean
}

/** One directory on a device, as `listDirectory` found it. */
export interface DirectoryListing {
  /**
   * The listed directory: absolute and lexically cleaned, symbolic links
   * not resolved. Empty when the request's path couldn't be resolved.
   */
  path: string
  /** Parent of `path` (lexically); empty at the root. */
  parent: string
  /** The home directory of the user the device's circus runs as. */
  home: string
  /** Subdirectories, sorted by name in byte order. Empty when `error` is set. */
  entries: DirectoryEntry[]
  /** Only the first entries (by name) were kept. */
  truncated: boolean
  /** Set when the directory was not listed. */
  error?: DirectoryError
}

export interface MachineService {
  listDevices(): Promise<Device[]>
  /**
   * Lists the subdirectories of a directory on one of the caller's
   * devices, asking the device live. `path` is empty or "~" for the home
   * directory, "~/x" under it, or absolute. Fails with `not_found` for an
   * unknown device, `failed_precondition` when it is offline, `unavailable`
   * when its link drops or its circus is too old to answer,
   * `deadline_exceeded` when it doesn't answer in time and
   * `resource_exhausted` when too many requests are in flight. Problems
   * with the path come back in the listing's `error`.
   */
  listDirectory(req: { device: string; path: string; includeHidden: boolean }): Promise<DirectoryListing>
  /**
   * Issues a one-time link that enrolls machines for the caller. Fails with
   * `resource_exhausted` while the caller has too many unused links, and
   * with `failed_precondition` when the server has no public URL.
   */
  createEnrollLink(req: { role: EnrollRole }): Promise<EnrollLink>
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
