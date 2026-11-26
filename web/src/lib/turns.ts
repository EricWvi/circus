// What the session view offers after a turn was interrupted.

import type { SessionEvent } from "./types"

/** The id of the latest interrupted-turn marker, if any. */
export function latestInterruptId(events: readonly SessionEvent[]): string | undefined {
  return events.findLast((e) => e.kind === "turn_interrupted")?.id
}

const isInput = (e: SessionEvent) => e.kind === "user_message" || e.kind === "question_answer"

/**
 * What "Retry" sends again: the text of the user message that started the
 * latest interrupted turn (for answers to a question, what Claude was sent).
 * Null when there is none, or when a message was sent after the
 * interruption (the turn was already redone).
 */
export function interruptedTurnInput(events: readonly SessionEvent[]): string | null {
  const markerIndex = events.findLastIndex((e) => e.kind === "turn_interrupted")
  if (markerIndex < 0) return null
  if (events.slice(markerIndex + 1).some(isInput)) return null
  const message = events.slice(0, markerIndex).findLast(isInput)
  return message?.kind === "user_message" || message?.kind === "question_answer" ? message.text : null
}
