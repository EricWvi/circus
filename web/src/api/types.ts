// Client-side view of the control plane API, shaped after its protobuf
// services. Method names follow the RPC names and requests are plain
// objects. Responses are UI-friendly: int64 timestamps become `number`
// milliseconds (exact up to the year 287396) and enums become string
// unions. A project's version (`updatedAtUnixMs`) is the exception: it is
// only ever compared and sent back, so it stays the exact int64 as a decimal
// string. A Connect-backed client converts from the generated messages; the
// in-memory mock implements the same interface.

import type { ServerSessionState } from "@/lib/session-state"
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

/** Why a folder's git origin URL was not read; the folder is the problem, not the device. */
export type GitRemoteError =
  /** Relative, too long, or contains NUL. */
  | "invalid_path"
  /** The path doesn't exist or is not a directory. */
  | "not_found"
  /** The folder is not inside a git work tree. */
  | "not_repository"
  /** The repository has no "origin" remote, or its URL is empty. */
  | "no_origin"
  /** git is not installed on the device. */
  | "git_unavailable"
  /** git took longer than the device allows. */
  | "timeout"
  /** Anything else. */
  | "failed"

/** The origin remote of the git repository containing a folder, as `getGitRemote` read it. */
export interface GitRemote {
  /** The origin's fetch URL, trimmed; empty when `error` is set. */
  url: string
  /** Set when the URL was not read. */
  error?: GitRemoteError
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
   * Reads the origin remote URL of the git repository containing a
   * directory on one of the caller's devices, asking the device live. The
   * path is as for `listDirectory`, and so are the failures (`unavailable`
   * also when the device's circus is too old to read remotes). Problems
   * with the path or the repository come back in the result's `error`.
   */
  getGitRemote(req: { device: string; path: string }): Promise<GitRemote>
  /**
   * Issues a one-time link that enrolls machines for the caller. Fails with
   * `resource_exhausted` while the caller has too many unused links, and
   * with `failed_precondition` when the server has no public URL.
   */
  createEnrollLink(req: { role: EnrollRole }): Promise<EnrollLink>
}

/** A session as lists show it. */
export interface SessionSummary {
  sessionId: string
  projectId: string
  /** Set from the first message unless renamed; empty until then. */
  title: string
  /** What the UI shows, worked out from the server's state (see `viewState`). */
  state: SessionState
  /** The control plane's own state, behind `state`. */
  serverState: ServerSessionState
  /** Turns started so far; the latest turn's number. */
  turnCount: number
  /** The view filled (64 MiB): only turn boundaries are kept from then on. */
  viewTruncated: boolean
  lastActivityAtUnixMs: number
  /** Pending, unexpired approvals. */
  pendingApprovalCount: number
  createdAtUnixMs: number
}

/** A session with where it runs, as GetSession returns it. */
export interface SessionDetails extends SessionSummary {
  /** Default device the session's tools execute on. */
  device: string
  /** The main thread's working directory on that device. */
  cwd: string
}

/** The control plane's kinds of session events, by their proto names without `KIND_`. */
export type SessionEventKind =
  | "unspecified"
  | "turn_started"
  | "assistant"
  | "user"
  | "tool_progress"
  | "result"
  | "permission_denied"
  | "approval_pending"
  | "approval_resolved"
  | "turn_interrupted"
  | "session_state"
  | "bgtask_started"
  | "bgtask_ended"
  | "notification_delivered"
  | "error"
  | "transcript_gap_accepted"
  | "transcript_gap_detected"
  | "turn_stopped"
  | "input_accepted"

/**
 * A session event as the control plane streams it. Durable events have
 * increasing `seq`; ephemeral ones (`seq` 0) are live only. A keepalive is
 * an ephemeral `unspecified` event without payload.
 */
export interface RawSessionEvent {
  seq: number
  tsUnixMs: number
  kind: SessionEventKind
  /** Opaque JSON; mostly Claude Agent SDK messages. */
  payloadJson: string
  ephemeral: boolean
  /** The payload was too large for the view and was dropped. */
  truncated: boolean
}

export type InputPriority = "normal" | "now"

/** A pending, unexpired approval. */
export interface SessionApproval {
  approvalId: string
  sessionId: string
  toolUseId: string
  tool: string
  device: string
  display: string
  reason: string
  expiresAtUnixMs: number
}

export interface SessionService {
  /** Newest activity first; includes stopped sessions. */
  listSessions(req: { projectId?: string }): Promise<SessionSummary[]>
  /** Also makes `locationIndex` the project's `lastUsedLocation`. */
  createSession(req: { projectId: string; locationIndex: number }): Promise<SessionDetails>
  getSession(req: { sessionId: string }): Promise<SessionDetails>
  /**
   * Sends a message. Repeating `clientMsgId` with the same text returns the
   * first answer without sending again; with other text it fails with
   * `already_exists`. `resource_exhausted` while too many messages wait for
   * delivery, `failed_precondition` once the session is stopped.
   */
  sendMessage(req: {
    sessionId: string
    clientMsgId: string
    text: string
    priority?: InputPriority
  }): Promise<{ inputId: string; eventSeq: number }>
  /**
   * The session's durable events after `afterSeq`, then live events. Ends
   * (or throws an `ApiError`) like the server's stream: `unavailable` when
   * it should be reopened, `not_found` once the session is gone.
   */
  watchSession(req: { sessionId: string; afterSeq: number; signal: AbortSignal }): AsyncIterable<RawSessionEvent>
  /** Stops the running turn; the session goes on. */
  interrupt(req: { sessionId: string }): Promise<void>
  /** Ends the session for good. */
  stopSession(req: { sessionId: string }): Promise<void>
  listApprovals(req: { sessionId: string }): Promise<SessionApproval[]>
  resolveApproval(req: { approvalId: string; approve: boolean }): Promise<void>
  /** Lets a broken session go on without the part of its history that was lost. */
  recoverSession(req: { sessionId: string }): Promise<void>
  updateSession(req: { sessionId: string; title: string }): Promise<SessionDetails>
}

export interface CircusClient {
  auth: AuthService
  oidc: OidcFlow
  projects: ProjectService
  machines: MachineService
  sessions: SessionService
}
