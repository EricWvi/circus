// Whether the worker holding the session's lease is disconnected
// (Session.worker_disconnected_at_unix_ms), from GetSession and from the
// ephemeral KIND_WORKER events of its stream. The lease outlives the
// connection by 40 to 60 seconds, so the worker can reconnect and keep its
// turn; if it doesn't, the lease expires and a running turn is interrupted.

import { i18n } from "@/i18n/i18n"
import { formatTimeAgo } from "@/lib/time"

/**
 * When the worker's last connection closed (the server's clock), as a
 * KIND_WORKER payload says; 0 when it reconnected or the lease ended, and
 * for a payload this client can't read.
 */
export function workerDisconnectedAtFromPayload(p: Record<string, unknown>): number {
  const v = p.disconnected_at_unix_ms
  const n = typeof v === "string" && /^\d+$/.test(v) ? Number(v) : typeof v === "number" ? v : 0
  return Number.isSafeInteger(n) && n > 0 ? n : 0
}

/** The latest disconnect time the stream announced, and when it arrived (this browser's clock). */
export interface LiveWorkerLink {
  disconnectedAtUnixMs: number
  receivedAtMs: number
}

/**
 * The disconnect time as the view shows it (0: connected, or nothing is
 * known): the stream's when it arrived no earlier than the read's request
 * was sent (`readSentAtMs`, unset: the stream's whenever it has one), else
 * the read's. A stream that opens again keeps its last value until a later
 * read lands, so the view never falls back to an older read.
 */
export function currentWorkerDisconnectedAt(
  fetched: number,
  readSentAtMs: number | undefined,
  live: LiveWorkerLink | null,
): number {
  return live && live.receivedAtMs >= (readSentAtMs ?? 0) ? live.disconnectedAtUnixMs : fetched
}

/**
 * What the disconnect means for the session, as one sentence: with a turn
 * running, that it is interrupted unless the worker is back soon. `now` is
 * the server's clock; null while it isn't known, which leaves the time out.
 */
export function workerDisconnectedText(disconnectedAtUnixMs: number, turnRunning: boolean, now: number | null): string {
  if (now === null) {
    return i18n.t(turnRunning ? "session.worker.disconnectedRunningUntimed" : "session.worker.disconnectedUntimed")
  }
  const ago = formatTimeAgo(disconnectedAtUnixMs, now)
  return i18n.t(turnRunning ? "session.worker.disconnectedRunning" : "session.worker.disconnected", { ago })
}
