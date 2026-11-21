// A session's or location's workspace: the project's default directory on
// one device, which the control plane prepares before the session's first
// turn runs. The view reads its state from GetSession and from KIND_WORKSPACE
// events, whichever the server wrote last.

import type {
  WorkspaceAction,
  WorkspaceError,
  WorkspaceState,
  WorkspaceStatus,
  WorkspaceWaiting,
} from "@/api/types"
import { i18n } from "@/i18n/i18n"

const STATES = new Set<WorkspaceState>(["pending", "preparing", "ready", "failed"])
const WAITINGS = new Set<WorkspaceWaiting>(["device_offline", "daemon_outdated", "retry"])
const ACTIONS = new Set<WorkspaceAction>(["created", "used_empty", "cloned", "used_repository"])
const ERRORS = new Set<WorkspaceError>([
  "invalid_request",
  "home_unknown",
  "unsupported_os",
  "path_conflict",
  "not_empty",
  "remote_mismatch",
  "git_unavailable",
  "clone_failed",
  "no_space",
  "permission_denied",
  "timeout",
  "busy",
  "cancelled",
  "io",
  "refused",
  "device_revoked",
  "interrupted",
  "daemon_fault",
])

const pick = <T extends string>(known: ReadonlySet<T>, v: unknown): T | "unspecified" =>
  typeof v === "string" && known.has(v as T) ? (v as T) : "unspecified"
const str = (v: unknown) => (typeof v === "string" ? v : "")

/**
 * The status a KIND_WORKSPACE payload carries (lower-case value names), as
 * of `tsUnixMs`. The payload has no attempt start; it reads as 0. Null when
 * the payload names no state this client knows.
 */
export function workspaceFromPayload(p: Record<string, unknown>, tsUnixMs: number): WorkspaceStatus | null {
  const state = pick(STATES, p.state)
  if (state === "unspecified") return null
  return {
    state,
    waiting: pick(WAITINGS, p.waiting),
    device: str(p.device),
    path: str(p.path),
    repoUrl: str(p.repo_url),
    background: p.background === true,
    action: pick(ACTIONS, p.action),
    error: pick(ERRORS, p.error),
    errorDetail: str(p.error_detail),
    attemptStartedAtUnixMs: 0,
    updatedAtUnixMs: tsUnixMs,
  }
}

/** The latest workspace status the session's stream brought, durable or not. */
export interface LiveWorkspace {
  status: WorkspaceStatus
  /**
   * The event's time (the server's clock). The control plane gives every
   * KIND_WORKSPACE event the workspace's new `updated_at_unix_ms`, which
   * moves on every change, waiting reason included.
   */
  tsUnixMs: number
  /** Preparing: when the attempt started, as far as the stream showed it; 0 when it didn't. */
  attemptStartedAtUnixMs: number
}

/**
 * The workspace as the view shows it: whichever of GetSession and the
 * stream the server wrote last, by `updated_at_unix_ms` (which an event's
 * time equals). On a tie they describe the same change and GetSession,
 * which carries the attempt's start, is taken. An event doesn't carry the
 * start: the stream's own record of it, or else the fetched one of a
 * preparing workspace, or else the event's time.
 */
export function currentWorkspace(
  fetched: WorkspaceStatus | undefined,
  live: LiveWorkspace | null,
): WorkspaceStatus | undefined {
  if (!live) return fetched
  if (fetched && live.tsUnixMs <= fetched.updatedAtUnixMs) return fetched
  let attemptStartedAtUnixMs = fetched?.attemptStartedAtUnixMs ?? 0
  if (live.status.state === "preparing") {
    attemptStartedAtUnixMs =
      live.attemptStartedAtUnixMs ||
      (fetched?.state === "preparing" ? fetched.attemptStartedAtUnixMs : 0) ||
      live.tsUnixMs
  }
  return { ...live.status, attemptStartedAtUnixMs, updatedAtUnixMs: live.tsUnixMs }
}

/** The session waits for its folder: no turn runs until it is ready. */
export const workspaceBlocks = (ws: WorkspaceStatus | undefined): ws is WorkspaceStatus =>
  ws !== undefined && ws.state !== "ready" && ws.state !== "unspecified"

/** Why preparing (or removing) a folder failed, as a phrase for a sentence. */
export function workspaceReason(error: WorkspaceError, { device, detail }: { device: string; detail: string }): string {
  switch (error) {
    case "not_empty":
      return i18n.t("workspace.reason.notEmpty")
    case "remote_mismatch":
      return detail === ""
        ? i18n.t("workspace.reason.remoteMismatchUnknown")
        : i18n.t("workspace.reason.remoteMismatch", { detail })
    case "path_conflict":
      return i18n.t("workspace.reason.pathConflict")
    case "git_unavailable":
      return i18n.t("workspace.reason.gitUnavailable", { device })
    case "clone_failed":
      return i18n.t("workspace.reason.cloneFailed")
    case "no_space":
      return i18n.t("workspace.reason.noSpace")
    case "permission_denied":
      return i18n.t("workspace.reason.permissionDenied")
    case "home_unknown":
      return i18n.t("workspace.reason.homeUnknown")
    case "unsupported_os":
      return i18n.t("workspace.reason.unsupportedOs")
    case "timeout":
      return i18n.t("workspace.reason.timeout")
    case "interrupted":
      return i18n.t("workspace.reason.interrupted")
    case "device_revoked":
      return i18n.t("workspace.reason.deviceRevoked")
    case "refused":
      return i18n.t("workspace.reason.refused")
    case "busy":
      return i18n.t("workspace.reason.busy")
    default:
      return i18n.t("workspace.reason.other")
  }
}

/** The device's own words are worth showing apart from the reason (the remote is already in it). */
export const showsDetail = (ws: Pick<WorkspaceStatus, "error" | "errorDetail">) =>
  ws.errorDetail !== "" && ws.error !== "remote_mismatch"

/**
 * One line for a recorded state change, as the conversation keeps it: where
 * the folder came from once ready, what went wrong, or what it waited for.
 */
export function workspaceEventText(ws: WorkspaceStatus): string {
  const { path, repoUrl: repo, device } = ws
  switch (ws.state) {
    case "ready":
      switch (ws.action) {
        case "cloned":
          return i18n.t("workspace.event.cloned", { repo, path })
        case "used_repository":
          return i18n.t("workspace.event.usedRepository", { path })
        case "used_empty":
          return i18n.t("workspace.event.usedEmpty", { path })
        case "created":
          return i18n.t("workspace.event.created", { path })
        default:
          return i18n.t("workspace.event.ready", { path })
      }
    case "failed":
      return i18n.t("workspace.failed", { path, reason: workspaceReason(ws.error, { device, detail: ws.errorDetail }) })
    case "preparing":
      return repo === "" ? i18n.t("workspace.event.creating", { path }) : i18n.t("workspace.event.cloning", { repo, path })
    default:
      return waitingText(ws)
  }
}

/** What a pending workspace waits for. */
export function waitingText(ws: Pick<WorkspaceStatus, "waiting" | "device">): string {
  switch (ws.waiting) {
    case "device_offline":
      return i18n.t("workspace.waiting.deviceOffline", { device: ws.device })
    case "daemon_outdated":
      return i18n.t("workspace.waiting.daemonOutdated", { device: ws.device })
    default:
      return i18n.t("workspace.waiting.retry")
  }
}

/** How long since `fromMs`, ticking by the second: "42s", "3m 5s", "1h 12m". */
export function formatElapsed(fromMs: number, now: number): string {
  const total = Math.max(0, Math.floor((now - fromMs) / 1000))
  const h = Math.floor(total / 3600)
  const m = Math.floor((total % 3600) / 60)
  const s = total % 60
  if (h > 0) return i18n.t("time.elapsedHours", { h, m })
  if (m > 0) return i18n.t("time.elapsedMinutes", { m, s })
  return i18n.t("time.elapsedSeconds", { s })
}
