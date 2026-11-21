import type { SessionEvent } from "./types"

/**
 * The error a settled tool call is stored with. It marks the client's own
 * note rather than agent text, so the tool view shows it in the active
 * language (`session.tool.interrupted`).
 */
export const INTERRUPTED_TOOL_ERROR = "Interrupted before the tool finished."

/**
 * The error a tool call is settled with when its turn ended without its
 * result reaching the view (for example a result too large to keep). Like
 * INTERRUPTED_TOOL_ERROR, a client note the tool view translates.
 */
export const NO_RESULT_TOOL_ERROR = "The turn ended without this tool's result."

/**
 * Ends work that nothing will finish any more: a streaming reply stops where
 * it is and a running tool call fails. Returns the same array when nothing
 * was in flight.
 */
export function settleEvents(
  events: readonly SessionEvent[],
  toolError: string = INTERRUPTED_TOOL_ERROR,
): readonly SessionEvent[] {
  let changed = false
  const settled = events.map((event): SessionEvent => {
    if ((event.kind === "assistant_text" || event.kind === "reasoning") && event.streaming) {
      changed = true
      return { ...event, streaming: false }
    }
    if (event.kind === "tool_call" && event.status === "running") {
      changed = true
      return { ...event, status: "error", error: toolError }
    }
    return event
  })
  return changed ? settled : events
}
