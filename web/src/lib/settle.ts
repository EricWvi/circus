import type { SessionEvent } from "./types"

/**
 * The error a tool call is settled with when its turn ended without its
 * result reaching the view (for example a result too large to keep). It
 * marks the client's own note rather than agent text, so the tool view shows
 * it in the active language (`session.tool.noResult`).
 */
export const NO_RESULT_TOOL_ERROR = "The turn ended without this tool's result."

/**
 * Why a running tool call is settled: the turn was cut off under it (the
 * session stopped, the worker was lost, the turn was interrupted), or the
 * turn ended without its result.
 */
export type SettleReason = "interrupted" | "no_result"

/**
 * Ends work that nothing will finish any more: a streaming reply stops where
 * it is and a running tool call becomes interrupted (cut off) or, for
 * `no_result`, fails. Returns the same array when nothing was in flight.
 */
export function settleEvents(
  events: readonly SessionEvent[],
  reason: SettleReason = "interrupted",
): readonly SessionEvent[] {
  let changed = false
  const settled = events.map((event): SessionEvent => {
    if ((event.kind === "assistant_text" || event.kind === "reasoning") && event.streaming) {
      changed = true
      return { ...event, streaming: false }
    }
    if (event.kind === "tool_call" && event.status === "running") {
      changed = true
      return reason === "interrupted"
        ? { ...event, status: "interrupted", interruption: "cut_off", error: undefined }
        : { ...event, status: "error", error: NO_RESULT_TOOL_ERROR }
    }
    return event
  })
  return changed ? settled : events
}
