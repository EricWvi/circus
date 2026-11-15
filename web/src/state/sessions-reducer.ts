import type { Session, SessionEvent, SessionState } from "@/lib/types"

export type SessionsAction =
  | { type: "append"; sessionId: string; event: SessionEvent }
  | {
      type: "patch_event"
      sessionId: string
      eventId: string
      patch: Partial<SessionEvent>
    }
  | { type: "set_state"; sessionId: string; state: SessionState }
  | { type: "approval_resolved"; sessionId: string }

function updateSession(
  sessions: Session[],
  sessionId: string,
  fn: (s: Session) => Session,
): Session[] {
  return sessions.map((s) =>
    s.id === sessionId ? { ...fn(s), lastActivityAt: new Date().toISOString() } : s,
  )
}

export function sessionsReducer(sessions: Session[], action: SessionsAction): Session[] {
  switch (action.type) {
    case "append":
      return updateSession(sessions, action.sessionId, (s) => ({
        ...s,
        events: [...s.events, action.event],
      }))
    case "patch_event":
      return updateSession(sessions, action.sessionId, (s) => ({
        ...s,
        events: s.events.map((e) =>
          e.id === action.eventId ? ({ ...e, ...action.patch } as SessionEvent) : e,
        ),
      }))
    case "set_state":
      return updateSession(sessions, action.sessionId, (s) => ({
        ...s,
        state: action.state,
      }))
    case "approval_resolved":
      return updateSession(sessions, action.sessionId, (s) => {
        const pendingApprovalCount = Math.max(0, s.pendingApprovalCount - 1)
        return { ...s, pendingApprovalCount }
      })
  }
}
