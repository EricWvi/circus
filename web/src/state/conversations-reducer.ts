import { settleEvents } from "@/lib/settle"
import type { Conversations, SessionEvent } from "@/lib/types"

export type ConversationsAction =
  | { type: "append"; sessionId: string; event: SessionEvent }
  | {
      type: "patch_event"
      sessionId: string
      eventId: string
      patch: Partial<SessionEvent>
    }
  /** Take over a transcript as stored, e.g. after another tab changed it. */
  | { type: "replace"; sessionId: string; events: SessionEvent[] }
  /** End what the session had in flight. */
  | { type: "settle"; sessionId: string }

export function conversationsReducer(
  state: Conversations,
  action: ConversationsAction,
): Conversations {
  const events = state[action.sessionId] ?? []
  switch (action.type) {
    case "append":
      return { ...state, [action.sessionId]: [...events, action.event] }
    case "patch_event":
      return {
        ...state,
        [action.sessionId]: events.map((e) =>
          e.id === action.eventId ? ({ ...e, ...action.patch } as SessionEvent) : e,
        ),
      }
    case "replace":
      return { ...state, [action.sessionId]: action.events }
    case "settle": {
      const settled = settleEvents(events)
      return settled === events ? state : { ...state, [action.sessionId]: settled }
    }
  }
}
