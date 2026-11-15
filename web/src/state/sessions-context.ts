import { createContext, useContext } from "react"

import type { ImageAttachment, Project, Session } from "@/lib/types"

export interface SessionsContextValue {
  projects: Project[]
  sessions: Session[]
  /** Append a user message and simulate a streamed assistant reply. */
  send: (sessionId: string, text: string, attachments: ImageAttachment[]) => void
  /** Interrupt the current turn. */
  stop: (sessionId: string) => void
  decideApproval: (
    sessionId: string,
    eventId: string,
    decision: "approved" | "denied",
  ) => void
}

export const SessionsContext = createContext<SessionsContextValue | null>(null)

export function useSessions(): SessionsContextValue {
  const ctx = useContext(SessionsContext)
  if (!ctx) throw new Error("useSessions must be used within SessionsProvider")
  return ctx
}
