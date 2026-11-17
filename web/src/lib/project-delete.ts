import type { SessionSummary } from "@/api/types"
import { i18n } from "@/i18n/i18n"

/** Sessions that keep a project from being deleted: any not stopped. */
export const activeSessionCount = (sessions: readonly Pick<SessionSummary, "state">[]) =>
  sessions.filter((s) => s.state !== "stopped").length

/** Why the project can't be deleted yet, or null when it can. */
export function deleteBlockedReason(
  sessions: readonly Pick<SessionSummary, "state">[],
): string | null {
  const active = activeSessionCount(sessions)
  if (active === 0) return null
  return i18n.t("projects.deleteBlocked", { count: active })
}
