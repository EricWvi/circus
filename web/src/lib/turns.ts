// How a session changes when a turn starts, ends early, or the session
// loses history. Pure functions: each returns the new transcript and the
// session progress to report with it.

import { settleEvents } from "./settle"
import { titleFromMessage } from "./title"
import type {
  HistoryGapEvent,
  ImageAttachment,
  SessionActivity,
  SessionEvent,
  TurnInterruptedEvent,
  TurnStoppedEvent,
} from "./types"

export interface SessionChange {
  events: SessionEvent[]
  activity: SessionActivity
}

/** Identity of a marker event the caller creates. */
export interface MarkerBase {
  id: string
  /** ISO 8601 timestamp. */
  at: string
}

/**
 * The user sends a message: the session runs (an interrupted one too) and
 * the first message names it, as the server would.
 */
export function startTurn(
  events: readonly SessionEvent[],
  message: { text: string; attachments: ImageAttachment[] },
  marker: MarkerBase,
): SessionChange {
  const first = !events.some((e) => e.kind === "user_message")
  const title = first ? titleFromMessage(message.text) : null
  return {
    events: [...events, { kind: "user_message", ...marker, ...message }],
    activity: title ? { state: "running", title } : { state: "running" },
  }
}

/**
 * The user stopped the turn: the session is idle, text already shown stays
 * as it is, running tool calls settle, and a quiet marker says so.
 */
export function stopTurn(events: readonly SessionEvent[], marker: MarkerBase): SessionChange {
  const stopped: TurnStoppedEvent = { kind: "turn_stopped", ...marker }
  return { events: [...settleEvents([...events]), stopped], activity: { state: "idle" } }
}

/**
 * The worker was lost mid-turn: the session is interrupted, text that was
 * still streaming stays visible but is marked unsaved, running tool calls
 * settle, and a marker offers to retry or continue the turn.
 */
export function interruptTurn(events: readonly SessionEvent[], marker: MarkerBase): SessionChange {
  const marked = events.map((event) =>
    event.kind === "assistant_text" && event.streaming ? { ...event, unsaved: true } : event,
  )
  const interrupted: TurnInterruptedEvent = { kind: "turn_interrupted", ...marker }
  return {
    events: [...settleEvents(marked), interrupted],
    activity: { state: "interrupted", pendingApprovalCount: 0 },
  }
}

/** Part of the transcript couldn't be saved: the session is broken. */
export function breakTranscript(events: readonly SessionEvent[]): SessionChange {
  return { events: [...events], activity: { state: "broken" } }
}

/**
 * The user continued a broken session without the missing part: the
 * session is idle and a divider marks where the history has a gap.
 */
export function continueWithoutHistory(
  events: readonly SessionEvent[],
  marker: MarkerBase,
): SessionChange {
  const gap: HistoryGapEvent = { kind: "history_gap", ...marker }
  return { events: [...events, gap], activity: { state: "idle" } }
}

/** The user stopped the session for good; nothing is pending any more. */
export function stopSession(events: readonly SessionEvent[]): SessionChange {
  return { events: [...events], activity: { state: "stopped", pendingApprovalCount: 0 } }
}

/** The id of the latest interrupted-turn marker, if any. */
export function latestInterruptId(events: readonly SessionEvent[]): string | undefined {
  return events.findLast((e) => e.kind === "turn_interrupted")?.id
}

/**
 * What "Retry" sends again: the user message that started the latest
 * interrupted turn, with its images. Null when there is none, or when a
 * message was sent after the interruption (the turn was already redone).
 */
export function interruptedTurnInput(
  events: readonly SessionEvent[],
): { text: string; attachments: ImageAttachment[] } | null {
  const markerIndex = events.findLastIndex((e) => e.kind === "turn_interrupted")
  if (markerIndex < 0) return null
  if (events.slice(markerIndex + 1).some((e) => e.kind === "user_message")) return null
  const message = events.slice(0, markerIndex).findLast((e) => e.kind === "user_message")
  if (message?.kind !== "user_message") return null
  return { text: message.text, attachments: message.attachments ?? [] }
}

/**
 * What a viewer that just lost the stream keeps showing: partially streamed
 * text is ephemeral (it is not replayed after a reconnect), so it goes.
 */
export function withoutStreamingText(events: readonly SessionEvent[]): SessionEvent[] {
  return events.filter((e) => !(e.kind === "assistant_text" && e.streaming))
}
