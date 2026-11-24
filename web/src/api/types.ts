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
  /**
   * The default directory on the device, for a default location (empty
   * `path`) once a session was created there; its path is absolute once
   * ready. Output only.
   */
  workspace?: WorkspaceStatus
}

/** Preparation state of a project's default directory on one device. */
export type WorkspaceState = "unspecified" | "pending" | "preparing" | "ready" | "failed"

/** Why a pending workspace hasn't started. */
export type WorkspaceWaiting = "unspecified" | "device_offline" | "daemon_outdated" | "retry"

/** What a ready workspace's preparation did. */
export type WorkspaceAction = "unspecified" | "created" | "used_empty" | "cloned" | "used_repository"

/** Why preparing or removing a workspace failed, by the proto names without `WORKSPACE_ERROR_`. */
export type WorkspaceError =
  | "unspecified"
  | "invalid_request"
  | "home_unknown"
  | "unsupported_os"
  | "path_conflict"
  | "not_empty"
  | "remote_mismatch"
  | "git_unavailable"
  | "clone_failed"
  | "no_space"
  | "permission_denied"
  | "timeout"
  | "busy"
  | "cancelled"
  | "io"
  | "refused"
  | "device_revoked"
  | "interrupted"
  | "daemon_fault"

/**
 * A project's default directory on one device, which the control plane
 * prepares (creates, or clones the repository into) before a session there
 * runs its first turn. Strings carry no credentials.
 */
export interface WorkspaceStatus {
  state: WorkspaceState
  /** Pending only. */
  waiting: WorkspaceWaiting
  /** The device's name. */
  device: string
  /** Absolute once ready; `~/circus/projects/<project id>` before. */
  path: string
  /** The repository cloned (or to clone); empty for an empty folder. */
  repoUrl: string
  /** Preparing only: the clone goes on as a background job on the device. */
  background: boolean
  /** Ready only. */
  action: WorkspaceAction
  /** Failed only. */
  error: WorkspaceError
  /** Failed only: at most 1024 bytes of what the device reported. */
  errorDetail: string
  /** When the current or last attempt started; 0 before any. */
  attemptStartedAtUnixMs: number
  updatedAtUnixMs: number
}

/** What deleting a project did to one device's default directory. */
export interface WorkspaceRemoval {
  device: string
  /** `kept`: the device was offline, too old, had no ready folder or didn't answer in time. */
  outcome: "unspecified" | "removed" | "kept" | "failed"
  /** The directory, absolute when known. */
  path: string
  /** Failed only. */
  error: WorkspaceError
  /** Failed only. */
  errorDetail: string
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
  /**
   * The model provider new sessions in this project use unless they name
   * one; empty for none. May name a deleted provider, which counts as none.
   */
  defaultProviderId: string
}

export interface ProjectService {
  createProject(req: {
    name: string
    repoUrl: string
    locations: Location[]
    /** Empty or missing: none. */
    defaultProviderId?: string
  }): Promise<Project>
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
    /** "" clears it; otherwise one of the caller's model providers. Versioned like the name. */
    defaultProviderId?: string
    lastUsedLocation?: number
    expectedUpdatedAt?: ProjectVersion
  }): Promise<Project>
  /**
   * Fails with `failed_precondition` while any session in it is not stopped.
   * `removeWorkspacesOn` names devices whose ready default directory of the
   * project is deleted too (at most 32); each is asked once, before the
   * project goes, and the outcome comes back per device in request order.
   */
  deleteProject(req: {
    projectId: string
    removeWorkspacesOn?: string[]
  }): Promise<{ workspaceRemovals: WorkspaceRemoval[] }>
}

export interface Device {
  name: string
  online: boolean
  lastSeenUnixMs: number
  os: string
  arch: string
  version: string
}

/** A worker: runs the Claude Code CLI for sessions. */
export interface Worker {
  workerId: string
  /** Unique among the user's workers. */
  name: string
  /** A worker link is connected now. */
  online: boolean
  /** As `Device.lastSeenUnixMs`; 0 when it never connected. */
  lastSeenUnixMs: number
  os: string
  arch: string
  hostname: string
  /** The worker's client version. */
  version: string
  /**
   * Names of the user's devices on the same machine: sessions in their
   * locations run in local mode on this worker.
   */
  localDevices: string[]
  /**
   * The integrator grant: the worker may merge ledger changesets that do
   * not fast-forward and discard changesets it did not open, for the
   * user's projects. Off by default.
   */
  integrator: boolean
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
   * The caller's workers that are not deleted, ordered by name.
   */
  listWorkers(): Promise<Worker[]>
  /**
   * Turns the integrator grant of one of the caller's workers on or off and
   * returns the worker. Fails with `not_found` for an unknown worker or
   * another user's.
   */
  setWorkerGrant(req: { workerId: string; integrator: boolean }): Promise<Worker>
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
  /**
   * Only for a session created in a project's default location: its
   * folder on the device. Until it is ready no turn runs; messages wait.
   */
  workspace?: WorkspaceStatus
  /**
   * The model provider the CLI starts with; empty for local login (the
   * worker machine's own Claude Code login). May name a deleted provider.
   */
  providerId: string
  /** Why a queued session is not running yet; `unspecified` otherwise. */
  queueReason: QueueReason
  /**
   * When the connection of the worker holding the session's lease closed
   * (the server's clock), while it hasn't reconnected; 0 when it is
   * connected, no lease is held, or nothing is known.
   */
  workerDisconnectedAtUnixMs: number
}

/**
 * Why a queued session can't be taken by a worker now, by the proto names
 * without `QUEUE_REASON_`.
 */
export type QueueReason =
  | "unspecified"
  /** The provider's session limit is reached. */
  | "provider_full"
  /** None of the provider's workers that may run the session is online. */
  | "no_assigned_worker"
  | "provider_deleted"
  /** The provider's key and variable values have to be entered again. */
  | "provider_secrets_unreadable"
  /** The session's worker (or every worker) is offline. */
  | "worker_offline"

/** A session with where it runs, as GetSession returns it. */
export interface SessionDetails extends SessionSummary {
  /** Default device the session's tools execute on. */
  device: string
  /** The main thread's working directory on that device. */
  cwd: string
  /**
   * Client only: when the request that read this session was sent (this
   * browser's clock). A live value the stream announced at or after it is
   * newer than the read's; one announced before it is in the read already
   * or outdated by it. Unset for a session not read by this client.
   */
  readSentAtMs?: number
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
  | "workspace"
  | "queue"
  | "worker"

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
  /**
   * Client only: when this browser received a live event, by its clock
   * (set by the session watch for KIND_QUEUE and KIND_WORKER events).
   */
  receivedAtMs?: number
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

export interface CreateSessionRequest {
  projectId: string
  locationIndex: number
  /** A UUID, 1 to 128 bytes; see `SessionService.createSession`. */
  clientRequestId: string
  /**
   * Missing: the project's default provider, else the user's, else local
   * login. "" chooses local login; otherwise one of the user's providers.
   */
  providerId?: string
}

export interface SessionService {
  /** Newest activity first; includes stopped sessions. */
  listSessions(req: { projectId?: string }): Promise<SessionSummary[]>
  /**
   * Also makes `locationIndex` the project's `lastUsedLocation`.
   * `clientRequestId` makes retries safe: repeating it in the same project
   * with the same `locationIndex` returns the session the first request
   * created; with another `locationIndex` it fails with `already_exists`.
   * Generate it once per intent to start a session and reuse it for every
   * retry of that intent.
   */
  createSession(req: CreateSessionRequest): Promise<SessionDetails>
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
  /**
   * Changes the fields that are set (at least one). `providerId` ("" for
   * local login) is refused with `failed_precondition` while a turn runs
   * and once the session is stopped; it applies from the next CLI start.
   */
  updateSession(req: { sessionId: string; title?: string; providerId?: string }): Promise<SessionDetails>
  /**
   * Starts a failed workspace of the session over (it becomes pending);
   * any other state comes back unchanged. `failed_precondition` for a
   * session without a workspace.
   */
  retryWorkspace(req: { sessionId: string }): Promise<WorkspaceStatus>
}

export interface CircusClient {
  auth: AuthService
  oidc: OidcFlow
  projects: ProjectService
  machines: MachineService
  sessions: SessionService
  ledger: LedgerService
  modelProviders: ModelProviderService
}

/** How a provider's key is sent: as x-api-key, or as Authorization: Bearer. */
export type ProviderScheme = "x_api_key" | "bearer"

/**
 * A model provider's version: the exact `updated_at_unix_ms` int64 as a
 * decimal string, sent back as `expectedUpdatedAt`.
 */
export type ProviderVersion = string

/** A custom environment variable, as the server returns it: the value is secret. */
export interface ProviderEnvVar {
  name: string
  /** The value's last four characters (fewer when shorter). */
  valueLast4: string
}

/**
 * A model provider: the credentials and environment a worker gives Claude
 * Code when it starts the CLI for a session. Secrets (the key and the
 * variable values) are write-only; only their last four characters come back.
 */
export interface ModelProvider {
  providerId: string
  name: string
  /** Empty: Claude Code's default endpoint. */
  baseUrl: string
  /** "unspecified" for a scheme this client doesn't know (from a newer server). */
  scheme: ProviderScheme | "unspecified"
  keyLast4: string
  opusModel: string
  sonnetModel: string
  haikuModel: string
  env: ProviderEnvVar[]
  /** Workers that may run sessions with this provider. */
  workerIds: string[]
  /** 0: unlimited. */
  maxSessions: number
  /** The user's default provider. */
  isDefault: boolean
  /** Sessions with this provider holding a lease now. */
  leasedSessions: number
  /** The stored secrets can't be decrypted; the key and every value must be entered again. */
  secretsUnreadable: boolean
  createdAtUnixMs: number
  updatedAtUnixMs: ProviderVersion
}

/** A provider as a create or update request carries it. */
export interface ModelProviderInput {
  name: string
  baseUrl: string
  scheme: ProviderScheme
  /** Required on create; on update "" keeps the stored key. */
  key: string
  opusModel: string
  sonnetModel: string
  haikuModel: string
  /**
   * Replaces the list. A variable without `value` keeps the value stored
   * under the same name (update only).
   */
  env: { name: string; value?: string }[]
  workerIds: string[]
  maxSessions: number
  isDefault: boolean
}

/**
 * The signed-in user's model providers. Invalid input is `invalid_argument`
 * with the field ("name", "api.base_url", "api.key", "env[1].name", ...);
 * a name in use is `already_exists` on "name"; more than 32 providers is
 * `resource_exhausted`; another user's provider is `not_found`.
 */
export interface ModelProviderService {
  createProvider(req: ModelProviderInput): Promise<ModelProvider>
  /** Ordered by name, ignoring case. */
  listProviders(): Promise<ModelProvider[]>
  getProvider(req: { providerId: string }): Promise<ModelProvider>
  /**
   * Replaces the provider's fields. With `expectedUpdatedAt`, applies only
   * if the provider is still at that version (`aborted` otherwise).
   */
  updateProvider(
    req: ModelProviderInput & { providerId: string; expectedUpdatedAt?: ProviderVersion },
  ): Promise<ModelProvider>
  /** `failed_precondition` while a session using it holds a lease. */
  deleteProvider(req: { providerId: string }): Promise<void>
}

/** One decision of a ledger, as the ledger's listing shows it. */
export interface LedgerEntry {
  /** Referred to as D<number>; unique within the project, never reused. 0 is valid. */
  number: number
  title: string
  /** Relative to the ledger root, without leading or trailing "/"; empty for the root. */
  dir: string
  /** "<dir>/D<number>-<name>.md", relative to the ledger root. */
  path: string
  /** When the last main-line commit that changed this decision was made. */
  updatedAtUnixMs: number
}

/** A project's ledger: its decisions on the main line. */
export interface Ledger {
  /** Full hex id of the main line's head commit; empty for an empty ledger. */
  headCommit: string
  /** Ordered by path (directory, then file name). */
  entries: LedgerEntry[]
  /** The number the next merged decision will get. */
  nextNumber: number
}

/** One decision as it is now, or as it was at a commit. */
export interface Decision {
  number: number
  title: string
  dir: string
  path: string
  /** Markdown, exactly as stored. Written by agents: untrusted input. */
  body: string
  /** The main-line commit this version was read at, and its time. */
  commit: string
  committedAtUnixMs: number
}

/** A main-line commit that changed a decision. */
export interface DecisionVersion {
  commit: string
  committedAtUnixMs: number
  /** A session, an orchestrator, or "import". */
  author: string
  /** First line of the commit message. */
  summary: string
  /** The decision's path and title in this version. */
  path: string
  title: string
  /** Only the version that created the decision; the other flags are false then. */
  added: boolean
  contentChanged: boolean
  moved: boolean
  retitled: boolean
  /** The code commit merged together with this version; empty when none. */
  codeCommit: string
}

/** Read-only access to projects' ledgers. A project of another user is `not_found`. */
export interface LedgerService {
  getLedger(req: { projectId: string }): Promise<Ledger>
  /**
   * The decision as it is now, or with `commit` as it was at that
   * main-line commit (`not_found` if it didn't exist there, or the commit
   * isn't on the main line).
   */
  getDecision(req: { projectId: string; number: number; commit?: string }): Promise<Decision>
  /** Newest first. `pageSize` 1 to 100 (0 or missing: 50); `nextPageToken` is empty after the last page. */
  listDecisionHistory(req: {
    projectId: string
    number: number
    pageSize?: number
    pageToken?: string
  }): Promise<{ versions: DecisionVersion[]; nextPageToken: string }>
}
