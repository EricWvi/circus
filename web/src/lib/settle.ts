import type { SessionEvent } from "./types"

/**
 * The error a settled tool call is stored with. It marks the client's own
 * note rather than agent text, so the tool view shows it in the active
 * language (`session.tool.interrupted`).
 */
export const INTERRUPTED_TOOL_ERROR = "Interrupted before the tool finished."

/**
 * Ends work that nothing will finish any more: a streaming reply stops where
 * it is and a running tool call fails. Returns the same array when nothing
 * was in flight.
 */
export function settleEvents(events: SessionEvent[]): SessionEvent[] {
  let changed = false
  const settled = events.map((event): SessionEvent => {
    if (event.kind === "assistant_text" && event.streaming) {
      changed = true
      return { ...event, streaming: false }
    }
    if (event.kind === "tool_call" && event.status === "running") {
      changed = true
      return { ...event, status: "error", error: INTERRUPTED_TOOL_ERROR }
    }
    return event
  })
  return changed ? settled : events
}
