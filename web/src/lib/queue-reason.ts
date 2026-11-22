// Why a queued session waits (Session.queue_reason), from GetSession and
// from the ephemeral KIND_QUEUE events of its stream.

import type { ModelProvider, QueueReason } from "@/api/types"
import { i18n } from "@/i18n/i18n"

const REASONS: ReadonlySet<QueueReason> = new Set<QueueReason>([
  "unspecified",
  "provider_full",
  "no_assigned_worker",
  "provider_deleted",
  "provider_secrets_unreadable",
  "worker_offline",
])

/** The reason a KIND_QUEUE payload carries; one this client doesn't know reads as none. */
export function queueReasonFromPayload(p: Record<string, unknown>): QueueReason {
  const reason = p.queue_reason
  return typeof reason === "string" && REASONS.has(reason as QueueReason) ? (reason as QueueReason) : "unspecified"
}

/** The latest reason the stream announced, and when it arrived (this browser's clock). */
export interface LiveQueueReason {
  reason: QueueReason
  receivedAtMs: number
}

/**
 * The reason as the view shows it: the stream's while it has one, else the
 * read's. The watch keeps a live reason until a read whose request was sent
 * after it arrived has landed (`SessionWatch.sessionRead`), and drops it
 * when the stream opens again. Only this browser's clock is involved.
 */
export function currentQueueReason(fetched: QueueReason, live: LiveQueueReason | null): QueueReason {
  return live ? live.reason : fetched
}

/** Reasons the Providers page can do something about. */
export const fixedOnProvidersPage = (reason: QueueReason) =>
  reason === "provider_full" || reason === "no_assigned_worker" || reason === "provider_secrets_unreadable"

/**
 * What the session waits for, as one sentence. `provider` is the session's
 * provider, undefined while the provider list loads (or the provider is
 * gone): a reason about it then reads as plain waiting until its name is
 * known.
 */
export function queueReasonText(reason: QueueReason, provider: Pick<ModelProvider, "name"> | undefined): string {
  const named = reason === "provider_full" || reason === "no_assigned_worker" || reason === "provider_secrets_unreadable"
  if (named && !provider) return i18n.t("session.queued")
  const name = provider?.name ?? ""
  switch (reason) {
    case "provider_full":
      return i18n.t("session.queue.provider_full", { provider: name })
    case "no_assigned_worker":
      return i18n.t("session.queue.no_assigned_worker", { provider: name })
    case "provider_deleted":
      return i18n.t("session.queue.provider_deleted")
    case "provider_secrets_unreadable":
      return i18n.t("session.queue.provider_secrets_unreadable", { provider: name })
    case "worker_offline":
      return i18n.t("session.queue.worker_offline")
    default:
      return i18n.t("session.queued")
  }
}
