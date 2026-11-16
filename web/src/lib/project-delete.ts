import type { SessionSummary } from "@/api/types"

/** Sessions that keep a project from being deleted: any not stopped. */
export const activeSessionCount = (sessions: readonly Pick<SessionSummary, "state">[]) =>
  sessions.filter((s) => s.state !== "stopped").length

/** Why the project can't be deleted yet, or null when it can. */
export function deleteBlockedReason(
  sessions: readonly Pick<SessionSummary, "state">[],
): string | null {
  const active = activeSessionCount(sessions)
  if (active === 0) return null
  return active === 1 ? "Stop its active session first" : `Stop its ${active} active sessions first`
}
