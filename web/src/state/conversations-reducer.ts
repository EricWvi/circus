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
  }
}
